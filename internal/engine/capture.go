package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	bolt "go.etcd.io/bbolt"
)

// capture is the versioned content envelope. New rows are gzip-compressed once
// as a whole and carry the JSON body directly. Gzip/Status remain only so old
// schema2 rows (which compressed the body separately) stay readable.
type capture struct {
	// Field names are intentionally untagged: schema2 stored these exact Go
	// names, and keeping them preserves old double-compressed rows.
	Version       int
	RequestID     string
	At            time.Time
	ExpiresAt     time.Time
	Headers       http.Header
	HeadersStatus string
	BodyStatus    string
	Status        string
	Reason        string
	OriginalBytes int64
	Body          json.RawMessage
	Gzip          []byte // legacy double envelope
}

type correlation struct {
	RequestID string
	At        time.Time
}

// captureState survives content eviction/purge without retaining headers or
// body. It is deliberately outside the content budget and carries no payload.
type captureState struct {
	RequestID       string    `json:"request_id,omitempty"`
	At              time.Time `json:"at,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	HeadersStatus   string    `json:"headers_status,omitempty"`
	BodyStatus      string    `json:"body_status,omitempty"`
	Status          string    `json:"status,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	OriginalBytes   int64     `json:"original_bytes,omitempty"`
	CompressedBytes int64     `json:"compressed_bytes,omitempty"`
}

func captureID(key, trace string) string {
	v, _ := json.Marshal([]string{key, trace})
	return string(v)
}
func captureFIFOKey(at time.Time, id string) string { return timePrefix(at) + id }
func captureIndexID(k, v []byte) string {
	if len(v) > 0 {
		return string(v)
	}
	// Older index implementations may have stored an empty value; the fixed
	// time prefix still lets us recover the capture key.
	if len(k) > len(timePrefix(time.Time{})) {
		return string(k[len(timePrefix(time.Time{})):])
	}
	return ""
}

func captureUsedBytes(tx *bolt.Tx) int64 {
	m := tx.Bucket([]byte("meta"))
	if m == nil {
		return 0
	}
	if raw := m.Get([]byte("capture_used_bytes")); raw != nil {
		if n, e := strconv.ParseInt(string(raw), 10, 64); e == nil && n >= 0 {
			return n
		}
	}
	return sumCaptureBytes(tx.Bucket([]byte("captures")))
}
func sumCaptureBytes(b *bolt.Bucket) int64 {
	var size int64
	if b == nil {
		return 0
	}
	b.ForEach(func(_, v []byte) error { size += int64(len(v)); return nil })
	return size
}
func setCaptureUsed(tx *bolt.Tx, n int64) error {
	if n < 0 {
		n = 0
	}
	return tx.Bucket([]byte("meta")).Put([]byte("capture_used_bytes"), []byte(strconv.FormatInt(n, 10)))
}
func addCaptureUsed(tx *bolt.Tx, delta int64) error {
	return setCaptureUsed(tx, captureUsedBytes(tx)+delta)
}

// migrateCaptureIndex is idempotent. The one-time index rebuild is guarded by a
// marker; the byte counter is reconciled on every open so an interrupted old
// writer cannot make the budget drift.
func migrateCaptureIndex(tx *bolt.Tx, retention int) error {
	m := tx.Bucket([]byte("meta"))
	fifo := tx.Bucket([]byte("capture_fifo"))
	captures := tx.Bucket([]byte("captures"))
	if m.Get([]byte("capture_index_ready")) == nil {
		if e := tx.DeleteBucket([]byte("capture_fifo")); e != nil && e != bolt.ErrBucketNotFound {
			return e
		}
		var e error
		if fifo, e = tx.CreateBucket([]byte("capture_fifo")); e != nil {
			return e
		}
		states := tx.Bucket([]byte("capture_state"))
		if e = captures.ForEach(func(k, v []byte) error {
			var c capture
			if e := decode(v, &c); e != nil {
				return e
			}
			at := c.At
			if at.IsZero() {
				at = time.Unix(0, 0).UTC()
			}
			// Legacy rows have no saved expiry. Record the inherited expiry in
			// lightweight state without rewriting their compressed content.
			if c.Version < captureEnvelopeVersion && states.Get(k) == nil && !c.At.IsZero() {
				hs, bs, status, reason := legacyCaptureStatuses(c)
				st := captureState{RequestID: c.RequestID, At: c.At, ExpiresAt: c.At.Add(time.Duration(retention) * time.Second), HeadersStatus: hs, BodyStatus: bs, Status: status, Reason: reason, OriginalBytes: c.OriginalBytes, CompressedBytes: int64(len(v))}
				if e := putCaptureState(tx, string(k), st); e != nil {
					return e
				}
			}
			return fifo.Put([]byte(captureFIFOKey(at, string(k))), k)
		}); e != nil {
			return e
		}
		if e = m.Put([]byte("capture_index_ready"), []byte("1")); e != nil {
			return e
		}
	}
	return setCaptureUsed(tx, sumCaptureBytes(captures))
}

// Collisions remain fenced for a complete retention interval after last use.
// A different admission sharing the trace immediately invalidates the earlier
// payload so content can never leak across requests.
func correlate(tx *bolt.Tx, key, trace, request string, at time.Time) error {
	if trace == "" || request == "" {
		return nil
	}
	b := tx.Bucket([]byte("correlations"))
	id := captureID(key, trace)
	c := correlation{RequestID: request, At: at}
	if old := b.Get([]byte(id)); old != nil {
		var prior correlation
		if e := decode(old, &prior); e != nil {
			return e
		}
		if prior.RequestID != request {
			if e := removeCaptureContent(tx, id, captureStatusMissing, captureReasonCorrelation); e != nil {
				return e
			}
			c.RequestID = ""
		}
	}
	return put(b, id, c)
}

// secretField is intentionally narrower than a substring match on "token":
// token accounting fields such as max_tokens are normal request parameters,
// while actual credential-bearing names remain redacted.
func secretField(k string) bool {
	n := strings.ToLower(k)
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(n)
	switch compact {
	case "maxtokens", "maxtoken", "mintokens", "mintoken",
		"prompttokens", "completiontokens", "inputtokens", "outputtokens",
		"totaltokens", "reasoningtokens", "cachedtokens", "cachereadtokens",
		"cachewritetokens", "cachecreationtokens", "maxoutputtokens",
		"maxcompletiontokens", "maxinputtokens":
		return false
	}
	return strings.Contains(compact, "authorization") ||
		strings.Contains(compact, "apikey") ||
		strings.Contains(compact, "secret") ||
		strings.Contains(compact, "password") ||
		strings.Contains(compact, "passwd") ||
		strings.Contains(compact, "cookie") ||
		strings.Contains(compact, "credential") ||
		strings.Contains(compact, "privatekey") ||
		strings.HasSuffix(compact, "token") ||
		compact == "key"
}
func redactURL(text string) (string, bool) {
	u, e := url.Parse(text)
	if e != nil || u.Scheme == "" || u.Host == "" {
		return text, false
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	q := u.Query()
	for k := range q {
		if secretField(k) {
			q.Set(k, "[REDACTED]")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
		return u.String(), true
	}
	return text, false
}
func redactValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return redactURL(x)
	case map[string]any:
		changed := false
		for k, a := range x {
			if secretField(k) {
				x[k] = "[REDACTED]"
				changed = true
			} else if b, ok := redactValue(a); ok {
				x[k] = b
				changed = true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, a := range x {
			if b, ok := redactValue(a); ok {
				x[i] = b
				changed = true
			}
		}
		return x, changed
	}
	return v, false
}

// Capture performs only bounded, nonblocking enqueue work. Content processing,
// redaction, compression and storage happen in the single worker.
func (s *Store) Capture(v CaptureInput) bool {
	if !s.mu.TryLock() {
		s.drop("lock_contention")
		return false
	}
	defer s.mu.Unlock()
	if s.closed || v.KeyID == "" || v.TraceID == "" || v.RequestID == "" {
		return false
	}
	mode := s.settings.CaptureMode
	if mode == CaptureModeOff {
		return false
	}
	if v.At.IsZero() {
		v.At = s.now().UTC()
	} else {
		v.At = v.At.UTC()
	}
	v.mode = mode
	v.generation = s.captureGeneration
	v.originalBytes = int64(len(v.Body))

	headers := http.Header{}
	size := 0
	for _, k := range []string{"Content-Type", "Accept", "User-Agent"} {
		for _, value := range v.Headers.Values(k) {
			size += len(value)
			if size > captureHeaderLimit {
				v.headersTooWide = true
				break
			}
			redacted, changed := redactURL(value)
			if changed {
				v.headersRedact = true
			}
			headers.Add(k, redacted)
		}
		if v.headersTooWide {
			headers = http.Header{}
			break
		}
	}
	v.Headers = headers
	if mode == CaptureModeHeaders {
		v.Body = nil
	} else if len(v.Body) > s.settings.MaxBodyBytes {
		v.bodyTooLarge = true
		v.Body = nil
	} else {
		v.Body = bytes.Clone(v.Body)
	}

	id := captureID(v.KeyID, v.TraceID)
	s.capturePending[id]++
	s.captureQueued.Add(1)
	select {
	case s.captures <- v:
		return true
	default:
		s.captureQueued.Add(-1)
		s.capturePending[id]--
		if s.capturePending[id] <= 0 {
			delete(s.capturePending, id)
		}
		s.drop("queue_full")
		return false
	}
}

// buildCapture turns one accepted request into an envelope without touching the
// caller's buffers. Content is only redacted JSON, never a response or upstream
// request.
func buildCapture(v CaptureInput) (capture, string) {
	c := capture{Version: captureEnvelopeVersion, RequestID: v.RequestID, At: v.At.UTC(), Headers: v.Headers, OriginalBytes: v.originalBytes}
	drop := ""
	if v.headersTooWide {
		c.Headers = http.Header{}
		c.HeadersStatus = captureStatusTooLarge
		c.Reason = captureReasonHeadersLarge
		drop = "headers_too_large"
	} else if v.headersRedact {
		c.HeadersStatus = captureStatusRedacted
		c.Reason = captureReasonRedacted
	} else {
		c.HeadersStatus = captureStatusAvailable
	}
	if v.mode == CaptureModeHeaders {
		c.BodyStatus = captureStatusDisabled
		return c, drop
	}
	if v.bodyTooLarge {
		c.BodyStatus = captureStatusTooLarge
		c.Reason = captureReasonBodyTooLarge
		return c, "too_large"
	}
	var body any
	dec := json.NewDecoder(bytes.NewReader(v.Body))
	dec.UseNumber()
	if e := dec.Decode(&body); e != nil {
		c.BodyStatus = captureStatusUnsupported
		c.Reason = captureReasonUnsupported
		return c, "unsupported"
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		c.BodyStatus = captureStatusUnsupported
		c.Reason = captureReasonUnsupported
		return c, "unsupported"
	}
	if redacted, changed := redactValue(body); changed {
		body = redacted
		c.BodyStatus = captureStatusRedacted
		c.Reason = captureReasonRedacted
	} else {
		c.BodyStatus = captureStatusAvailable
	}
	raw, e := json.Marshal(body)
	if e != nil {
		c.BodyStatus = captureStatusUnsupported
		c.Reason = captureReasonUnsupported
		return c, "unsupported"
	}
	c.Body = raw
	return c, drop
}

func captureAggregate(c capture) string {
	if c.BodyStatus != "" && c.BodyStatus != captureStatusDisabled {
		return c.BodyStatus
	}
	if c.HeadersStatus != "" {
		return c.HeadersStatus
	}
	return captureStatusMissing
}

func (s *Store) captureWorker() {
	defer s.worker.Done()
	for v := range s.captures {
		c, drop := buildCapture(v)
		if drop != "" {
			s.drop(drop)
		}
		s.writeCapture(v, c)
		if s.captureQueued.Load() > 0 {
			s.captureQueued.Add(-1)
		}
	}
}

func (s *Store) writeCapture(v CaptureInput, c capture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := captureID(v.KeyID, v.TraceID)
	defer func() {
		s.capturePending[id]--
		if s.capturePending[id] <= 0 {
			delete(s.capturePending, id)
		}
	}()
	// Re-check the accepted policy and purge generation after any queue delay.
	if s.settings.CaptureMode == CaptureModeOff || v.generation != s.captureGeneration {
		return
	}
	if s.settings.CaptureMode == CaptureModeHeaders && c.BodyStatus != captureStatusDisabled {
		c.Body = nil
		c.BodyStatus = captureStatusDisabled
		c.Reason = captureReasonHeadersOnly
	}
	retention := s.settings.CaptureRetentionSeconds
	c.ExpiresAt = c.At.Add(time.Duration(retention) * time.Second)
	c.Status = captureAggregate(c)
	now := s.now().UTC()
	var evicted, storageDropped int64
	e := s.db.Update(func(tx *bolt.Tx) error {
		var key storedKey
		if e := get(tx.Bucket([]byte("keys")), v.KeyID, &key); e != nil {
			return e
		}
		if e := correlate(tx, v.KeyID, v.TraceID, v.RequestID, now); e != nil {
			return e
		}
		var correlation correlation
		if e := get(tx.Bucket([]byte("correlations")), id, &correlation); e != nil {
			return e
		}
		if correlation.RequestID == "" || correlation.RequestID != v.RequestID {
			return nil
		}
		b := tx.Bucket([]byte("captures"))
		if old := b.Get([]byte(id)); old != nil {
			var previous capture
			if e := decode(old, &previous); e != nil {
				return e
			}
			if previous.RequestID == v.RequestID {
				return nil
			}
			if e := removeCaptureContent(tx, id, captureStatusMissing, captureReasonCorrelation); e != nil {
				return e
			}
		}
		// Expired rows are reclaimed before FIFO eviction so a write never
		// mistakes stale content for live pressure.
		if _, e := cleanupExpiredLocked(tx, now, retention); e != nil {
			return e
		}
		payload, e := encode(c)
		if e != nil {
			return e
		}
		limit := s.settings.MaxBodyStorageBytes
		if int64(len(payload)) > limit {
			storageDropped = 1
			return putCaptureState(tx, id, captureState{RequestID: v.RequestID, At: c.At, ExpiresAt: c.ExpiresAt, HeadersStatus: captureStatusTooLarge, BodyStatus: captureStatusTooLarge, Status: captureStatusTooLarge, Reason: captureReasonStorageLimit, OriginalBytes: c.OriginalBytes})
		}
		evicted, e = evictOldestLocked(tx, int64(len(payload)), limit, retention)
		if e != nil {
			return e
		}
		if s.captureFault != nil {
			if e := s.captureFault(); e != nil {
				return e
			}
		}
		if e := b.Put([]byte(id), payload); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("capture_fifo")).Put([]byte(captureFIFOKey(c.At, id)), []byte(id)); e != nil {
			return e
		}
		if e := addCaptureUsed(tx, int64(len(payload))); e != nil {
			return e
		}
		return putCaptureState(tx, id, stateFromCapture(c, int64(len(payload))))
	})
	if e != nil {
		s.drop("write_failed")
		return
	}
	if storageDropped > 0 {
		s.dropN("storage_limit", storageDropped)
	}
	if evicted > 0 {
		s.dropN("evicted", evicted)
	}
}

func stateFromCapture(c capture, compressed int64) captureState {
	return captureState{RequestID: c.RequestID, At: c.At, ExpiresAt: c.ExpiresAt, HeadersStatus: c.HeadersStatus, BodyStatus: c.BodyStatus, Status: c.Status, Reason: c.Reason, OriginalBytes: c.OriginalBytes, CompressedBytes: compressed}
}
func loadCaptureState(tx *bolt.Tx, id string) (captureState, error) {
	var st captureState
	raw := tx.Bucket([]byte("capture_state")).Get([]byte(id))
	if raw == nil {
		return st, nil
	}
	return st, decode(raw, &st)
}

// removedCaptureState keeps the inherited expiry and original size of legacy
// rows when content is expired, evicted or purged. The state marker is what
// remains after removal, so it must not silently lengthen the TTL.
func removedCaptureState(c capture, prior captureState, compressed int64, status, reason string) captureState {
	st := stateFromCapture(c, compressed)
	if prior.RequestID != "" {
		st.RequestID = prior.RequestID
	}
	if !prior.At.IsZero() {
		st.At = prior.At
	}
	if !prior.ExpiresAt.IsZero() && (st.ExpiresAt.IsZero() || prior.ExpiresAt.Before(st.ExpiresAt)) {
		st.ExpiresAt = prior.ExpiresAt
	}
	if prior.OriginalBytes != 0 {
		st.OriginalBytes = prior.OriginalBytes
	}
	st.HeadersStatus, st.BodyStatus, st.Status, st.Reason = status, status, status, reason
	return st
}
func putCaptureState(tx *bolt.Tx, id string, st captureState) error {
	return put(tx.Bucket([]byte("capture_state")), id, st)
}
func deleteCaptureIndex(tx *bolt.Tx, id string, at time.Time) error {
	return tx.Bucket([]byte("capture_fifo")).Delete([]byte(captureFIFOKey(at, id)))
}
func removeCaptureContent(tx *bolt.Tx, id, status, reason string) error {
	b := tx.Bucket([]byte("captures"))
	raw := b.Get([]byte(id))
	if raw == nil {
		// A prior removal may only have left a state marker.
		if sb := tx.Bucket([]byte("capture_state")); sb.Get([]byte(id)) != nil {
			var st captureState
			if e := decode(sb.Get([]byte(id)), &st); e != nil {
				return e
			}
			st.HeadersStatus, st.BodyStatus, st.Status, st.Reason = status, status, status, reason
			st.CompressedBytes = 0
			return putCaptureState(tx, id, st)
		}
		return nil
	}
	var c capture
	if e := decode(raw, &c); e != nil {
		return e
	}
	prior, e := loadCaptureState(tx, id)
	if e != nil {
		return e
	}
	used := captureUsedBytes(tx)
	if e := b.Delete([]byte(id)); e != nil {
		return e
	}
	if e := deleteCaptureIndex(tx, id, c.At); e != nil {
		return e
	}
	if e := setCaptureUsed(tx, used-int64(len(raw))); e != nil {
		return e
	}
	return putCaptureState(tx, id, removedCaptureState(c, prior, int64(len(raw)), status, reason))
}

func captureExpired(c capture, st captureState, now time.Time, retention int) bool {
	if c.At.IsZero() {
		return !st.ExpiresAt.IsZero() && !now.Before(st.ExpiresAt)
	}
	expiry := c.ExpiresAt
	if expiry.IsZero() {
		expiry = c.At.Add(time.Duration(retention) * time.Second)
	}
	policy := c.At.Add(time.Duration(retention) * time.Second)
	if policy.Before(expiry) {
		expiry = policy
	}
	// Legacy rows kept their inherited expiry in state without rewriting the
	// compressed content; a later policy extension must not revive them.
	if !st.ExpiresAt.IsZero() && st.ExpiresAt.Before(expiry) {
		expiry = st.ExpiresAt
	}
	return !now.Before(expiry)
}

// cleanupExpiredLocked removes only capture content and leaves records, daily
// rollups, counters and quotas untouched.
func cleanupExpiredLocked(tx *bolt.Tx, now time.Time, retention int) (CleanupResult, error) {
	out := CleanupResult{}
	b := tx.Bucket([]byte("captures"))
	f := tx.Bucket([]byte("capture_fifo"))
	if b == nil || f == nil {
		return out, nil
	}
	used := captureUsedBytes(tx)
	c := f.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		id := captureIndexID(k, v)
		raw := b.Get([]byte(id))
		if raw == nil {
			if e := c.Delete(); e != nil {
				return out, e
			}
			continue
		}
		var cap capture
		if e := decode(raw, &cap); e != nil {
			return out, e
		}
		var st captureState
		if rawState := tx.Bucket([]byte("capture_state")).Get([]byte(id)); rawState != nil {
			if e := decode(rawState, &st); e != nil {
				return out, e
			}
		}
		if !captureExpired(cap, st, now, retention) {
			continue
		}
		if e := b.Delete([]byte(id)); e != nil {
			return out, e
		}
		if e := c.Delete(); e != nil {
			return out, e
		}
		out.RemovedRecords++
		out.RemovedBytes += int64(len(raw))
		st = removedCaptureState(cap, st, int64(len(raw)), captureStatusExpired, captureReasonExpired)
		if e := putCaptureState(tx, id, st); e != nil {
			return out, e
		}
	}
	if e := setCaptureUsed(tx, used-out.RemovedBytes); e != nil {
		return out, e
	}
	return out, nil
}

func evictOldestLocked(tx *bolt.Tx, needed, limit int64, retention int) (int64, error) {
	used := captureUsedBytes(tx)
	if needed > limit {
		return 0, nil
	}
	var evicted int64
	b := tx.Bucket([]byte("captures"))
	f := tx.Bucket([]byte("capture_fifo"))
	for used+needed > limit {
		c := f.Cursor()
		k, v := c.First()
		if k == nil {
			break
		}
		id := captureIndexID(k, v)
		raw := b.Get([]byte(id))
		if raw == nil {
			if e := c.Delete(); e != nil {
				return evicted, e
			}
			continue
		}
		var cap capture
		if e := decode(raw, &cap); e != nil {
			return evicted, e
		}
		prior, e := loadCaptureState(tx, id)
		if e != nil {
			return evicted, e
		}
		if e := b.Delete([]byte(id)); e != nil {
			return evicted, e
		}
		if e := c.Delete(); e != nil {
			return evicted, e
		}
		used -= int64(len(raw))
		evicted++
		st := removedCaptureState(cap, prior, int64(len(raw)), captureStatusEvicted, captureReasonCapacity)
		if e := putCaptureState(tx, id, st); e != nil {
			return evicted, e
		}
	}
	if e := setCaptureUsed(tx, used); e != nil {
		return evicted, e
	}
	return evicted, nil
}

// reconfigureCaptures applies a shorter policy immediately and trims content by
// capture time when the compressed budget is lowered.
func reconfigureCaptures(tx *bolt.Tx, now time.Time, oldRetention, retention int, limit int64) error {
	if retention < oldRetention {
		b := tx.Bucket([]byte("captures"))
		oldUsed := captureUsedBytes(tx)
		type update struct {
			key, payload []byte
			delta        int64
		}
		var updates []update
		if e := b.ForEach(func(k, v []byte) error {
			var c capture
			if e := decode(v, &c); e != nil {
				return e
			}
			if c.At.IsZero() {
				return nil
			}
			tight := c.At.Add(time.Duration(retention) * time.Second)
			if c.ExpiresAt.IsZero() || tight.Before(c.ExpiresAt) {
				c.ExpiresAt = tight
				payload, e := encode(c)
				if e != nil {
					return e
				}
				updates = append(updates, update{key: append([]byte(nil), k...), payload: payload, delta: int64(len(payload) - len(v))})
			}
			return nil
		}); e != nil {
			return e
		}
		var delta int64
		for _, u := range updates {
			if e := b.Put(u.key, u.payload); e != nil {
				return e
			}
			delta += u.delta
		}
		if e := setCaptureUsed(tx, oldUsed+delta); e != nil {
			return e
		}
	}
	if _, e := cleanupExpiredLocked(tx, now, retention); e != nil {
		return e
	}
	used := captureUsedBytes(tx)
	if used > limit {
		// evictOldestLocked expects the prospective payload; zero means trim the
		// existing set down to the new budget.
		if _, e := evictOldestLocked(tx, 0, limit, retention); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) CleanupCaptures() (CleanupResult, error) {
	if s.maintenanceActive.Load() {
		return CleanupResult{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return CleanupResult{}, e
	}
	var out CleanupResult
	e := s.db.Update(func(tx *bolt.Tx) error {
		var e error
		out, e = cleanupExpiredLocked(tx, s.now().UTC(), s.settings.CaptureRetentionSeconds)
		return e
	})
	if e != nil {
		s.maintenanceError = "capture cleanup failed"
		return CleanupResult{}, e
	}
	s.lastCleanupAt = s.now().UTC()
	s.lastCleanupRecord = out.RemovedRecords
	s.lastCleanupBytes = out.RemovedBytes
	if s.maintenanceError == "capture cleanup failed" {
		s.maintenanceError = ""
	}
	return out, nil
}

// PurgeCaptures clears content and advances the generation in the same
// transaction. Jobs queued before this call can never write their payload back.
func (s *Store) PurgeCaptures() (CleanupResult, error) {
	if s.maintenanceActive.Load() {
		return CleanupResult{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return CleanupResult{}, e
	}
	var out CleanupResult
	oldGeneration := s.captureGeneration
	s.captureGeneration++
	e := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("captures"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			out.RemovedRecords++
			out.RemovedBytes += int64(len(v))
			var cap capture
			if e := decode(v, &cap); e == nil {
				prior, _ := loadCaptureState(tx, string(k))
				st := removedCaptureState(cap, prior, int64(len(v)), captureStatusPurged, captureReasonPurged)
				if e := putCaptureState(tx, string(k), st); e != nil {
					return e
				}
			}
			if e := c.Delete(); e != nil {
				return e
			}
		}
		if e := tx.DeleteBucket([]byte("capture_fifo")); e != nil && e != bolt.ErrBucketNotFound {
			return e
		}
		if _, e := tx.CreateBucketIfNotExists([]byte("capture_fifo")); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("meta")).Put([]byte("capture_generation"), []byte(strconv.FormatUint(s.captureGeneration, 10))); e != nil {
			return e
		}
		return setCaptureUsed(tx, 0)
	})
	if e != nil {
		s.captureGeneration = oldGeneration
		return CleanupResult{}, e
	}
	s.lastCleanupAt = s.now().UTC()
	s.lastCleanupRecord = out.RemovedRecords
	s.lastCleanupBytes = out.RemovedBytes
	return out, nil
}

func decodeCapture(raw []byte) (capture, error) {
	var c capture
	if e := decode(raw, &c); e != nil {
		return c, e
	}
	if c.Version < captureEnvelopeVersion && len(c.Gzip) > 0 {
		z, e := gzip.NewReader(bytes.NewReader(c.Gzip))
		if e != nil {
			return c, e
		}
		defer z.Close()
		body, e := io.ReadAll(io.LimitReader(z, maxCaptureBody+1))
		if e != nil {
			return c, e
		}
		if len(body) <= maxCaptureBody {
			c.Body = body
		}
	}
	return c, nil
}

func legacyCaptureStatuses(c capture) (headers, body, aggregate, reason string) {
	headers, body = captureStatusMissing, captureStatusMissing
	if len(c.Headers) > 0 {
		headers = captureStatusAvailable
	}
	switch c.Status {
	case captureStatusMissing:
		return captureStatusMissing, captureStatusMissing, captureStatusMissing, captureReasonMissing
	case captureStatusTruncated:
		return headers, captureStatusTooLarge, captureStatusTruncated, captureReasonBodyTooLarge
	case captureStatusRedacted:
		return headers, captureStatusRedacted, captureStatusRedacted, captureReasonRedacted
	case captureStatusAvailable, "":
		if len(c.Body) > 0 || len(c.Gzip) > 0 {
			body = captureStatusAvailable
		}
		return headers, body, captureAggregate(capture{HeadersStatus: headers, BodyStatus: body}), ""
	default:
		return headers, body, c.Status, c.Reason
	}
}

func statuses(c capture, st captureState) (headers, body, aggregate, reason string) {
	if c.Version >= captureEnvelopeVersion {
		headers, body, aggregate, reason = c.HeadersStatus, c.BodyStatus, c.Status, c.Reason
	} else {
		headers, body, aggregate, reason = legacyCaptureStatuses(c)
	}
	if st.RequestID != "" {
		if st.HeadersStatus != "" {
			headers = st.HeadersStatus
		}
		if st.BodyStatus != "" {
			body = st.BodyStatus
		}
		if st.Status != "" {
			aggregate = st.Status
		}
		if st.Reason != "" {
			reason = st.Reason
		}
	}
	return
}

func cloneRates(v pricing.Rates) pricing.Rates {
	if v.Reasoning != nil {
		r := *v.Reasoning
		v.Reasoning = &r
	}
	return v
}

// clonePriceQuote deep-copies every slice and pointer so callers can never
// mutate the immutable quote retained in bbolt.
func clonePriceQuote(q pricing.Quote) pricing.Quote {
	q.Rates = cloneRates(q.Rates)
	if q.CatalogTime != nil {
		t := *q.CatalogTime
		q.CatalogTime = &t
	}
	q.MissingRates = append([]string(nil), q.MissingRates...)
	if q.Candidates != nil {
		candidates := make([]pricing.Candidate, len(q.Candidates))
		for i, candidate := range q.Candidates {
			candidates[i] = candidate
			candidates[i].Rates = cloneRates(candidate.Rates)
			candidates[i].MissingRates = append([]string(nil), candidate.MissingRates...)
		}
		q.Candidates = candidates
	}
	return q
}

func (s *Store) Detail(id string) (Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := Detail{Headers: http.Header{}, Body: json.RawMessage("null"), CaptureStatus: captureStatusMissing, HeadersStatus: captureStatusMissing, BodyStatus: captureStatusMissing}
	if e := s.check(); e != nil {
		return d, e
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		var stored recordData
		if e := get(tx.Bucket([]byte("records")), id, &stored); e != nil {
			return e
		}
		d.Record = stored.Record
		d.PriceQuote = clonePriceQuote(stored.Quote)
		cut := s.now().Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
		if d.Record.Time.Before(cut) {
			return ErrNotFound
		}
		recordMode := stored.CaptureMode
		if recordMode == "" {
			if stored.CaptureEnabled {
				recordMode = CaptureModeHeadersAndBody
			} else {
				recordMode = CaptureModeOff
			}
		}
		if s.settings.CaptureMode == CaptureModeOff || recordMode == CaptureModeOff {
			d.CaptureStatus, d.HeadersStatus, d.BodyStatus, d.CaptureReason = captureStatusDisabled, captureStatusDisabled, captureStatusDisabled, captureReasonOff
			return nil
		}

		cid := captureID(d.Record.KeyID, d.Record.TraceID)
		corrRequest := ""
		if raw := tx.Bucket([]byte("correlations")).Get([]byte(cid)); raw != nil {
			var corr correlation
			if e := decode(raw, &corr); e != nil {
				return e
			}
			corrRequest = corr.RequestID
		}
		var c capture
		hasContent := false
		if raw := tx.Bucket([]byte("captures")).Get([]byte(cid)); raw != nil {
			var e error
			if c, e = decodeCapture(raw); e != nil {
				return e
			}
			// Captures are keyed by principal+trace and shared by multiple
			// executions. A non-empty correlation names the admission; no
			// correlation is the legacy/direct-capture case.
			if c.RequestID == "" || corrRequest == "" || c.RequestID == corrRequest {
				hasContent = true
			}
		}
		var st captureState
		hasState := false
		if raw := tx.Bucket([]byte("capture_state")).Get([]byte(cid)); raw != nil {
			if e := decode(raw, &st); e != nil {
				return e
			}
			hasState = st.RequestID == "" || corrRequest == "" || st.RequestID == corrRequest ||
				(st.Status == captureStatusMissing && st.Reason == captureReasonCorrelation)
		}
		if !hasContent && !hasState {
			if s.capturePending[cid] > 0 {
				d.CaptureStatus, d.HeadersStatus, d.BodyStatus = captureStatusPending, captureStatusPending, captureStatusPending
			} else {
				d.CaptureStatus, d.HeadersStatus, d.BodyStatus, d.CaptureReason = captureStatusMissing, captureStatusMissing, captureStatusMissing, captureReasonMissing
			}
			return nil
		}
		if hasContent {
			d.OriginalBytes = c.OriginalBytes
			d.CompressedBytes = int64(len(tx.Bucket([]byte("captures")).Get([]byte(cid))))
			if !c.At.IsZero() {
				at := c.At
				d.CapturedAt = &at
			}
			d.HeadersStatus, d.BodyStatus, d.CaptureStatus, d.CaptureReason = statuses(c, captureState{})
			expiry := c.ExpiresAt
			if expiry.IsZero() && !c.At.IsZero() {
				expiry = c.At.Add(time.Duration(s.settings.CaptureRetentionSeconds) * time.Second)
			}
			if !expiry.IsZero() {
				policy := c.At.Add(time.Duration(s.settings.CaptureRetentionSeconds) * time.Second)
				if policy.Before(expiry) {
					expiry = policy
				}
				ex := expiry
				d.ExpiresAt = &ex
			}
		}
		if hasContent && hasState {
			d.HeadersStatus, d.BodyStatus, d.CaptureStatus, d.CaptureReason = statuses(c, st)
		}
		if hasState {
			if !st.At.IsZero() && d.CapturedAt == nil {
				at := st.At
				d.CapturedAt = &at
			}
			if !st.ExpiresAt.IsZero() && (d.ExpiresAt == nil || st.ExpiresAt.Before(*d.ExpiresAt)) {
				ex := st.ExpiresAt
				d.ExpiresAt = &ex
			}
			if d.OriginalBytes == 0 {
				d.OriginalBytes = st.OriginalBytes
			}
			if d.CompressedBytes == 0 {
				d.CompressedBytes = st.CompressedBytes
			}
			if !hasContent {
				d.HeadersStatus, d.BodyStatus, d.CaptureStatus, d.CaptureReason = st.HeadersStatus, st.BodyStatus, st.Status, st.Reason
				if d.HeadersStatus == "" {
					d.HeadersStatus = captureStatusMissing
				}
				if d.BodyStatus == "" {
					d.BodyStatus = captureStatusMissing
				}
				if d.CaptureStatus == "" {
					d.CaptureStatus = captureAggregate(capture{HeadersStatus: d.HeadersStatus, BodyStatus: d.BodyStatus})
				}
				if !st.ExpiresAt.IsZero() && !s.now().Before(st.ExpiresAt) {
					d.CaptureStatus, d.HeadersStatus, d.BodyStatus, d.CaptureReason = captureStatusExpired, captureStatusExpired, captureStatusExpired, captureReasonExpired
				} else if s.settings.CaptureMode == CaptureModeHeaders {
					d.BodyStatus = captureStatusDisabled
					if d.HeadersStatus != "" {
						d.CaptureStatus = d.HeadersStatus
					}
				}
				return nil
			}
		}
		if d.ExpiresAt != nil && !s.now().Before(*d.ExpiresAt) {
			d.CaptureStatus, d.HeadersStatus, d.BodyStatus, d.CaptureReason = captureStatusExpired, captureStatusExpired, captureStatusExpired, captureReasonExpired
			return nil
		}
		if s.settings.CaptureMode == CaptureModeHeaders {
			d.BodyStatus = captureStatusDisabled
			d.Body = json.RawMessage("null")
			if d.HeadersStatus != "" {
				d.CaptureStatus = d.HeadersStatus
			}
		}
		if d.HeadersStatus == captureStatusAvailable || d.HeadersStatus == captureStatusRedacted {
			d.Headers = c.Headers
		}
		if s.settings.CaptureMode == CaptureModeHeadersAndBody && (d.BodyStatus == captureStatusAvailable || d.BodyStatus == captureStatusRedacted) && len(c.Body) > 0 {
			d.Body = append(json.RawMessage(nil), c.Body...)
		}
		return nil
	})
	return d, e
}
