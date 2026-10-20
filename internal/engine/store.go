package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	bolt "go.etcd.io/bbolt"
)

var buckets = []string{"meta", "keys", "hashes", "records", "dedup", "daily", "captures", "capture_state", "capture_fifo", "timeline", "correlations"}

type storedKey struct {
	Key
	Hash  string
	Epoch Limits
}
type pending struct {
	Quote          pricing.Quote
	Record         Record
	At             time.Time
	CaptureEnabled bool
	CaptureMode    string
	Epoch          Limits
}
type recordData struct {
	Record
	Quote          pricing.Quote
	CaptureEnabled bool
	CaptureMode    string
}
type Store struct {
	cutoff       time.Time
	usageFault   func(pending) error
	writeFault   func() error // fault injection for transaction failure tests
	cleanFault   func() error // fault injection for maintenance visibility tests
	captureFault func() error // fault injection for capture transaction rollback tests

	mu                sync.Mutex
	db                *bolt.DB
	path              string
	now               func() time.Time
	pricer            pricing.Pricer
	settings          Settings
	closed            bool
	active            map[string]Admission
	completed         map[string]Admission
	pending           map[string]pending
	blocked           map[string]bool
	unknown           map[string]bool
	lastError         string
	maintenanceError  string
	captures          chan CaptureInput
	capturePending    map[string]int
	worker            sync.WaitGroup
	closeDone         chan struct{}
	closeErr          error
	openedAt          time.Time
	captureGeneration uint64
	captureQueued     atomic.Int64
	maintenanceActive atomic.Bool
	lastCleanupAt     time.Time
	lastCleanupRecord int64
	lastCleanupBytes  int64
	lastCompactAt     time.Time
	lastCompactTry    time.Time
	lastCompactBefore int64
	lastCompactAfter  int64
	dropMu            sync.Mutex
	drops             map[string]int64
}

func DefaultSettings() Settings {
	return Settings{CaptureMode: CaptureModeOff, CaptureRetentionSeconds: 24 * 60 * 60, MaxBodyBytes: 1 << 20, MaxBodyStorageBytes: 256 << 20, RawRetentionHours: 24, DailyRetentionDays: 365, Pricing: pricing.DefaultConfig()}
}
func validMode(mode string) bool {
	return mode == CaptureModeOff || mode == CaptureModeHeaders || mode == CaptureModeHeadersAndBody
}
func validSettings(v Settings) bool {
	return validMode(v.CaptureMode) && v.CaptureRetentionSeconds >= 60 && v.CaptureRetentionSeconds <= v.RawRetentionHours*3600 && v.MaxBodyBytes >= 1 && v.MaxBodyBytes <= maxCaptureBody && v.MaxBodyStorageBytes >= 1 && v.MaxBodyStorageBytes <= maxCaptureStorage && v.RawRetentionHours >= 1 && v.RawRetentionHours <= 168 && v.DailyRetentionDays >= 1 && v.DailyRetentionDays <= 3650
}

// storedSettings decodes old schema2 values without confusing an omitted mode
// with the explicit "off" value. Old enabled values inherit headers_and_body;
// omitted retention inherits the metadata retention.
func storedSettings(raw []byte) (Settings, error) {
	v := DefaultSettings()
	if raw == nil {
		return v, nil
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return v, e
	}
	if e := json.Unmarshal(raw, &v); e != nil {
		return v, e
	}
	if _, ok := fields["capture_mode"]; !ok {
		v.CaptureMode = ""
	}
	if _, ok := fields["capture_retention_seconds"]; !ok {
		v.CaptureRetentionSeconds = 0
	}
	if v.CaptureMode == "" {
		if v.CaptureEnabled {
			v.CaptureMode = CaptureModeHeadersAndBody
		} else {
			v.CaptureMode = CaptureModeOff
		}
	}
	if v.CaptureRetentionSeconds <= 0 {
		v.CaptureRetentionSeconds = v.RawRetentionHours * 3600
	}
	v.CaptureEnabled = v.CaptureMode != CaptureModeOff
	return v, nil
}

// effectiveSettings projects legacy capture_enabled into the explicit mode and
// applies omitted-field preservation for callers that still send partial data.
func (s *Store) effectiveSettings(v Settings) (Settings, bool) {
	if v.CaptureMode == "" {
		switch {
		case !v.CaptureEnabled && s.settings.CaptureEnabled:
			v.CaptureMode = CaptureModeOff
		case v.CaptureEnabled && s.settings.CaptureMode == CaptureModeOff:
			v.CaptureMode = CaptureModeHeadersAndBody
		default:
			v.CaptureMode = s.settings.CaptureMode
		}
	} else if !validMode(v.CaptureMode) {
		return Settings{}, false
	} else if v.CaptureMode == CaptureModeOff && v.CaptureEnabled {
		// Current management wiring forwards the older boolean verbatim. Keep
		// that legacy input useful; the management layer still rejects a truly
		// contradictory new caller before reaching the store.
		v.CaptureMode = CaptureModeHeadersAndBody
	} else if !v.CaptureEnabled && s.settings.CaptureEnabled && v.CaptureMode == s.settings.CaptureMode {
		// The old UI round-trips the whole Settings object and only flips the
		// boolean. Its unchanged mode is therefore a legacy disable request.
		v.CaptureMode = CaptureModeOff
	}
	if !validMode(v.CaptureMode) {
		return Settings{}, false
	}
	v.CaptureEnabled = v.CaptureMode != CaptureModeOff
	if v.CaptureRetentionSeconds == 0 {
		v.CaptureRetentionSeconds = s.settings.CaptureRetentionSeconds
	}
	return v, true
}
func checkFeatureVersion(m *bolt.Bucket) error {
	if m == nil {
		return nil
	}
	raw := m.Get([]byte("observer_features"))
	if raw == nil {
		return nil
	}
	n, e := strconv.Atoi(string(raw))
	if e != nil || n < 1 {
		return ErrInvalid
	}
	if n > supportedObserverFeature {
		return ErrUnsupported
	}
	return nil
}
func Open(o Options) (*Store, error) {
	if o.Pricer == nil || o.DataPath == "" {
		return nil, ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// Inspect existing files read-only first: even bbolt's freelist initialization
	// must not touch a foreign database. A feature version this binary does not
	// understand is rejected before any writable handle is opened.
	if st, e := os.Stat(o.DataPath); e == nil && st.Size() > 0 {
		db, e := bolt.Open(o.DataPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
		if e != nil {
			return nil, e
		}
		e = db.View(func(tx *bolt.Tx) error {
			m := tx.Bucket([]byte("meta"))
			if m != nil && string(m.Get([]byte("schema"))) == "2" {
				return checkFeatureVersion(m)
			}
			empty := true
			tx.ForEach(func(_ []byte, _ *bolt.Bucket) error { empty = false; return nil })
			if !empty {
				return ErrInvalid
			}
			return nil
		})
		db.Close()
		if e != nil {
			return nil, e
		}
	} else if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Dir(o.DataPath), 0700); e != nil {
		return nil, e
	}
	db, e := bolt.Open(o.DataPath, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	s := &Store{db: db, path: o.DataPath, now: o.Now, pricer: o.Pricer, settings: DefaultSettings(), active: map[string]Admission{}, completed: map[string]Admission{}, pending: map[string]pending{}, blocked: map[string]bool{}, unknown: map[string]bool{}, captures: make(chan CaptureInput, 64), capturePending: map[string]int{}, openedAt: o.Now().UTC(), drops: map[string]int64{}}
	s.closeDone = make(chan struct{})
	e = db.Update(func(tx *bolt.Tx) error {
		for _, n := range buckets {
			if _, e := tx.CreateBucketIfNotExists([]byte(n)); e != nil {
				return e
			}
		}
		m := tx.Bucket([]byte("meta"))
		if e := checkFeatureVersion(m); e != nil {
			return e
		}
		loaded, e := storedSettings(m.Get([]byte("settings")))
		if e != nil {
			return e
		}
		s.settings = loaded
		unclean := string(m.Get([]byte("running"))) == "1"
		var keys []storedKey
		kb := tx.Bucket([]byte("keys"))
		if e := kb.ForEach(func(_, v []byte) error {
			var k storedKey
			if e := decode(v, &k); e != nil {
				return e
			}
			keys = append(keys, k)
			return nil
		}); e != nil {
			return e
		}
		for _, k := range keys {
			if s.observeInterruption(k.ID) || unclean {
				k.BlockedReason = unknownReason
				if e := put(kb, k.ID, k); e != nil {
					return e
				}
			}
			if k.BlockedReason != "" {
				s.unknown[k.ID] = true
			}
		}
		if !validSettings(s.settings) {
			return ErrInvalid
		}
		if e := migrateCaptureIndex(tx, s.settings.CaptureRetentionSeconds); e != nil {
			return e
		}
		if v := m.Get([]byte("capture_generation")); v != nil {
			n, e := strconv.ParseUint(string(v), 10, 64)
			if e != nil {
				return ErrInvalid
			}
			s.captureGeneration = n
		}
		if e := put(m, "settings", s.settings); e != nil {
			return e
		}
		if e := m.Put([]byte("schema"), []byte("2")); e != nil {
			return e
		}
		if e := m.Put([]byte("observer_features"), []byte("1")); e != nil {
			return e
		}
		return m.Put([]byte("running"), []byte("1"))
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	if e = s.pricer.Configure(s.settings.Pricing); e != nil {
		db.Close()
		return nil, e
	}
	if len(s.unknown) > 0 {
		s.lastError = unknownReason
	}
	s.worker.Add(1)
	go s.captureWorker()
	return s, nil
}
func put(b *bolt.Bucket, k string, v any) error {
	p, e := encode(v)
	if e != nil {
		return e
	}
	return b.Put([]byte(k), p)
}
func get(b *bolt.Bucket, k string, v any) error {
	p := b.Get([]byte(k))
	if p == nil {
		return ErrNotFound
	}
	return decode(p, v)
}
func (s *Store) check() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}
func (s *Store) Settings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Settings{}, e
	}
	p, _ := json.Marshal(s.settings)
	var v Settings
	json.Unmarshal(p, &v)
	return v, nil
}
func (s *Store) UpdateSettings(v Settings) (Settings, error) {
	if s.maintenanceActive.Load() {
		return Settings{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Settings{}, e
	}
	var ok bool
	v, ok = s.effectiveSettings(v)
	if !ok || !validSettings(v) {
		return Settings{}, ErrInvalid
	}
	if e := pricing.Validate(v.Pricing); e != nil {
		return Settings{}, ErrInvalid
	}
	if v.Pricing.Prices == nil {
		v.Pricing.Prices = []pricing.Price{}
	}
	if v.Pricing.Rules == nil {
		v.Pricing.Rules = []pricing.Rule{}
	}
	if e := s.pricer.Configure(v.Pricing); e != nil {
		return Settings{}, e
	}
	oldRetention := s.settings.CaptureRetentionSeconds
	if e := s.db.Update(func(tx *bolt.Tx) error {
		if e := reconfigureCaptures(tx, s.now().UTC(), oldRetention, v.CaptureRetentionSeconds, v.MaxBodyStorageBytes); e != nil {
			return e
		}
		return put(tx.Bucket([]byte("meta")), "settings", v)
	}); e != nil {
		if rollback := s.pricer.Configure(s.settings.Pricing); rollback != nil {
			s.fenceAllLocked()
		}
		return Settings{}, e
	}
	raw, _ := json.Marshal(v)
	json.Unmarshal(raw, &s.settings)
	return v, nil
}
func (s *Store) drop(reason string) { s.dropN(reason, 1) }
func (s *Store) dropN(reason string, n int64) {
	if n <= 0 {
		return
	}
	s.dropMu.Lock()
	s.drops[reason] += n
	s.dropMu.Unlock()
}
func (s *Store) dropSnapshot() (map[string]int64, uint64) {
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	out := make(map[string]int64, len(s.drops))
	var total uint64
	for k, v := range s.drops {
		out[k] = v
		if v > 0 {
			total += uint64(v)
		}
	}
	return out, total
}
func (s *Store) Health() Health {
	// Capture the flag before waiting for a long compaction: Health may block
	// on the store mutex, but the caller still learns that maintenance was
	// active when the snapshot started.
	maintenanceActive := s.maintenanceActive.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.db.View(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("keys")).ForEach(func(id, _ []byte) error { s.observeInterruption(string(id)); return nil })
		})
	}
	drops, total := s.dropSnapshot()
	h := Health{DroppedCaptures: total, CaptureDrops: drops, CountersSince: s.openedAt, PendingUsage: len(s.pending), ActiveRequests: len(s.active), LastError: s.lastError, MaintenanceError: s.maintenanceError, LastCleanupRecords: s.lastCleanupRecord, LastCleanupBytes: s.lastCleanupBytes, LastCompactBeforeBytes: s.lastCompactBefore, LastCompactAfterBytes: s.lastCompactAfter, MaintenanceActive: maintenanceActive || s.maintenanceActive.Load(), CaptureLimitBytes: s.settings.MaxBodyStorageBytes}
	if !s.lastCleanupAt.IsZero() {
		t := s.lastCleanupAt
		h.LastCleanupAt = &t
	}
	if !s.lastCompactAt.IsZero() {
		t := s.lastCompactAt
		h.LastCompactAt = &t
	}
	if !s.closed {
		_ = s.db.View(func(tx *bolt.Tx) error {
			h.CaptureBytes = captureUsedBytes(tx)
			return nil
		})
		h.ReclaimableBytes = int64(s.db.Stats().FreePageN) * int64(s.db.Info().PageSize)
	}
	if st, e := os.Stat(s.path); e == nil {
		h.DatabaseBytes = st.Size()
	}
	return h
}
func (s *Store) Close() (result error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.closeDone
		return s.closeErr
	}
	s.closed = true
	close(s.captures)
	s.mu.Unlock()
	s.worker.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { s.closeErr = result; close(s.closeDone) }()
	e := s.retryLocked()
	for _, a := range s.active {
		s.unknown[a.KeyID] = true
	}
	for _, p := range s.pending {
		s.unknown[p.Record.KeyID] = true
	}
	durable := s.db.Update(func(tx *bolt.Tx) error {
		for id := range s.unknown {
			var k storedKey
			b := tx.Bucket([]byte("keys"))
			if err := get(b, id, &k); err != nil {
				return err
			}
			k.BlockedReason = unknownReason
			if err := put(b, id, k); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte("meta")).Put([]byte("running"), []byte("0"))
	})
	if durable != nil {
		e = durable
	} else if len(s.active) > 0 || len(s.pending) > 0 {
		e = ErrAccounting
	}
	ce := s.db.Close()
	if e != nil {
		return e
	}
	return ce
}
