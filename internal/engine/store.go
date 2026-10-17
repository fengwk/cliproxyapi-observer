package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	bolt "go.etcd.io/bbolt"
)

var buckets = []string{"meta", "keys", "hashes", "records", "dedup", "daily", "captures", "timeline", "correlations"}

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
	Epoch          Limits
}
type recordData struct {
	Record
	Quote          pricing.Quote
	CaptureEnabled bool
}
type Store struct {
	cutoff     time.Time
	usageFault func(pending) error
	writeFault func() error // fault injection for transaction failure tests

	mu        sync.Mutex
	db        *bolt.DB
	path      string
	now       func() time.Time
	pricer    pricing.Pricer
	settings  Settings
	closed    bool
	active    map[string]Admission
	completed map[string]Admission
	pending   map[string]pending
	blocked   map[string]bool
	unknown   map[string]bool
	lastError string
	dropped   atomic.Uint64
	captures  chan CaptureInput
	worker    sync.WaitGroup
	closeDone chan struct{}
	closeErr  error
}

func DefaultSettings() Settings {
	return Settings{MaxBodyBytes: 1 << 20, MaxBodyStorageBytes: 256 << 20, RawRetentionHours: 24, DailyRetentionDays: 365, Pricing: pricing.DefaultConfig()}
}
func validSettings(v Settings) bool {
	return v.MaxBodyBytes >= 1 && v.MaxBodyBytes <= maxCaptureBody && v.MaxBodyStorageBytes >= 1 && v.MaxBodyStorageBytes <= maxCaptureStorage && v.RawRetentionHours >= 1 && v.RawRetentionHours <= 168 && v.DailyRetentionDays >= 1 && v.DailyRetentionDays <= 3650
}
func Open(o Options) (*Store, error) {
	if o.Pricer == nil || o.DataPath == "" {
		return nil, ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// Inspect existing files read-only first: even bbolt's freelist initialization
	// must not touch a foreign database.
	if st, e := os.Stat(o.DataPath); e == nil && st.Size() > 0 {
		db, e := bolt.Open(o.DataPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
		if e != nil {
			return nil, e
		}
		e = db.View(func(tx *bolt.Tx) error {
			m := tx.Bucket([]byte("meta"))
			if m != nil && string(m.Get([]byte("schema"))) == "2" {
				return nil
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
	s := &Store{db: db, path: o.DataPath, now: o.Now, pricer: o.Pricer, settings: DefaultSettings(), active: map[string]Admission{}, completed: map[string]Admission{}, pending: map[string]pending{}, blocked: map[string]bool{}, unknown: map[string]bool{}, captures: make(chan CaptureInput, 64)}
	s.closeDone = make(chan struct{})
	e = db.Update(func(tx *bolt.Tx) error {
		for _, n := range buckets {
			if _, e := tx.CreateBucketIfNotExists([]byte(n)); e != nil {
				return e
			}
		}
		m := tx.Bucket([]byte("meta"))
		if v := m.Get([]byte("settings")); v != nil {
			if e := json.Unmarshal(v, &s.settings); e != nil {
				return e
			}
		}
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
			if unclean {
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
		if e := m.Put([]byte("schema"), []byte("2")); e != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Settings{}, e
	}
	if !validSettings(v) {
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
	if e := s.db.Update(func(tx *bolt.Tx) error {
		if e := trimCaptures(tx.Bucket([]byte("captures")), v.MaxBodyStorageBytes); e != nil {
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
func (s *Store) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Health{DroppedCaptures: s.dropped.Load(), PendingUsage: len(s.pending), ActiveRequests: len(s.active), LastError: s.lastError}
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
