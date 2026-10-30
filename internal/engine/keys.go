package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
)

const maxSafe int64 = 9_000_000_000_000_000
const completedTTL = 24 * time.Hour
const maxCompleted = 4096

func random() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}

// randomSecret returns sk- plus 48 unbiased [A-Za-z0-9] characters. Rejection
// sampling keeps modulo arithmetic from favouring the first eight symbols.
func randomSecret() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 48)
	buf := make([]byte, 64)
	for i := 0; i < len(out); {
		if _, e := rand.Read(buf); e != nil {
			panic(e)
		}
		for _, b := range buf {
			if int(b) >= 256-256%len(alphabet) {
				continue
			}
			out[i] = alphabet[int(b)%len(alphabet)]
			i++
			if i == len(out) {
				break
			}
		}
	}
	return "sk-" + string(out)
}
func hash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func validSpec(v KeySpec) bool {
	if v.MaxConcurrent < 0 || int64(v.MaxConcurrent) > maxSafe || len(v.Label) > 256 {
		return false
	}
	for _, n := range []int64{v.Limits.FiveHour, v.Limits.Week, v.Limits.Month, v.Limits.Total} {
		if n < 0 || n > maxSafe {
			return false
		}
	}
	for _, m := range v.AllowedModels {
		if m == "" || len(m) > 1024 {
			return false
		}
	}
	return true
}
func cleanSpec(v KeySpec) KeySpec {
	v.AllowedModels = append([]string{}, v.AllowedModels...)
	if v.ExpiresAt != nil {
		t := v.ExpiresAt.UTC()
		v.ExpiresAt = &t
	}
	return v
}
func month(t time.Time) time.Time {
	t = t.UTC()
	y, m, d := t.Date()
	first := time.Date(y, m+1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	if d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}
func starts(t time.Time) Windows { return Windows{t, t, t, t} }
func roll(k *Key, t time.Time) {
	if !t.Before(k.Resets.FiveHour) {
		k.Starts.FiveHour = t
		k.Resets.FiveHour = t.Add(5 * time.Hour)
		k.Used.FiveHour = 0
	}
	if !t.Before(k.Resets.Week) {
		k.Starts.Week = t
		k.Resets.Week = t.Add(7 * 24 * time.Hour)
		k.Used.Week = 0
	}
	if !t.Before(k.Resets.Month) {
		k.Starts.Month = t
		k.Resets.Month = month(t)
		k.Used.Month = 0
	}
}
func (s *Store) decorateStored(k storedKey) Key {
	view := s.decorate(k.Key)
	view.Revoked = view.DeletedAt == nil && k.Hash == ""
	return view
}
func (s *Store) decorate(k Key) Key {
	// Effective display only: leave anchors untouched until actual admission.
	now := s.now()
	if !now.Before(k.Resets.FiveHour) {
		k.Used.FiveHour = 0
	}
	if !now.Before(k.Resets.Week) {
		k.Used.Week = 0
	}
	if !now.Before(k.Resets.Month) {
		k.Used.Month = 0
	}
	k.Active = 0
	for _, a := range s.active {
		if a.KeyID == k.ID {
			k.Active++
		}
	}
	return k
}
func (s *Store) CreateKey(v KeySpec) (Key, string, error) {
	return s.CreateKeyWithSecret(v, randomSecret())
}

// Use supplied credentials exactly as entered; never silently trim secrets.
func validSecret(secret string) bool {
	if len(secret) < 8 || len(secret) > 256 {
		return false
	}
	for _, c := range []byte(secret) {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

func (s *Store) CreateKeyWithSecret(v KeySpec, secret string) (Key, string, error) {
	if s.maintenanceActive.Load() {
		return Key{}, "", ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Key{}, "", e
	}
	if !validSpec(v) || !validSecret(secret) {
		return Key{}, "", ErrInvalid
	}
	t := s.now().UTC()
	k := storedKey{Key: Key{ID: random(), Secret: secret, KeySpec: cleanSpec(v), CreatedAt: t, Starts: starts(t), Resets: Windows{FiveHour: t.Add(5 * time.Hour), Week: t.Add(7 * 24 * time.Hour), Month: month(t)}}, Hash: hash(secret)}
	e := s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("hashes")).Get([]byte(k.Hash)) != nil {
			return ErrConflict
		}
		if e := put(tx.Bucket([]byte("keys")), k.ID, k); e != nil {
			return e
		}
		return tx.Bucket([]byte("hashes")).Put([]byte(k.Hash), []byte(k.ID))
	})
	if e != nil {
		return Key{}, "", e
	}
	return s.decorateStored(k), secret, nil
}
func (s *Store) mutate(id string, f func(*storedKey) error) (Key, error) {
	if e := s.check(); e != nil {
		return Key{}, e
	}
	var k storedKey
	e := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("keys"))
		if e := get(b, id, &k); e != nil {
			return e
		}
		if k.DeletedAt != nil {
			return ErrDeleted
		}
		old := k.Hash
		if e := f(&k); e != nil {
			return e
		}
		if old != k.Hash {
			h := tx.Bucket([]byte("hashes"))
			if k.Hash != "" && h.Get([]byte(k.Hash)) != nil {
				return ErrConflict
			}
			if e := h.Delete([]byte(old)); e != nil {
				return e
			}
			if k.Hash != "" {
				if e := h.Put([]byte(k.Hash), []byte(id)); e != nil {
					return e
				}
			}
		}
		return put(b, id, k)
	})
	return s.decorateStored(k), e
}
func (s *Store) UpdateKey(id string, v KeySpec) (Key, error) {
	if s.maintenanceActive.Load() {
		return Key{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validSpec(v) {
		return Key{}, ErrInvalid
	}
	return s.mutate(id, func(k *storedKey) error {
		// A revoked key has no credential. Enabling it directly would create a
		// non-authenticatable "active" principal; rotation must come first.
		if v.Enabled && k.Hash == "" {
			return ErrConflict
		}
		k.KeySpec = cleanSpec(v)
		return nil
	})
}
func (s *Store) RotateKey(id string) (Key, string, error) {
	return s.RotateKeyWithSecret(id, randomSecret())
}

func (s *Store) RotateKeyWithSecret(id, secret string) (Key, string, error) {
	if s.maintenanceActive.Load() {
		return Key{}, "", ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validSecret(secret) {
		return Key{}, "", ErrInvalid
	}
	k, e := s.mutate(id, func(k *storedKey) error {
		next := hash(secret)
		if next == k.Hash {
			return ErrInvalid
		}
		k.Hash = next
		k.Secret = secret
		return nil
	})
	if e != nil {
		return Key{}, "", e
	}
	return k, secret, e
}
func (s *Store) RevokeKey(id string) error {
	if s.maintenanceActive.Load() {
		return ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := s.mutate(id, func(k *storedKey) error { k.Hash = ""; k.Secret = ""; k.Enabled = false; return nil })
	return e
}
func (s *Store) ResetKey(id string, windows []string) (Key, error) {
	if s.maintenanceActive.Load() {
		return Key{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(id, func(k *storedKey) error {
		t := s.now().UTC()
		for _, w := range windows {
			switch w {
			case "five_hour":
				k.Used.FiveHour = 0
				k.Starts.FiveHour = t
				k.Resets.FiveHour = t.Add(5 * time.Hour)
			case "week":
				k.Used.Week = 0
				k.Starts.Week = t
				k.Resets.Week = t.Add(7 * 24 * time.Hour)
			case "month":
				k.Used.Month = 0
				k.Starts.Month = t
				k.Resets.Month = month(t)
			case "total":
				k.Used.Total = 0
				k.Starts.Total = t
			default:
				return ErrInvalid
			}
		}
		return nil
	})
}

// Keys returns the default management list, which omits terminal tombstones.
// History views that must locate a deleted Key use KeysIncludingDeleted.
func (s *Store) Keys() ([]Key, error) { return s.keys(false) }

// KeysIncludingDeleted exposes tombstones for historical lookup. Callers still
// see deleted_at and the retained counters/records.
func (s *Store) KeysIncludingDeleted() ([]Key, error) { return s.keys(true) }

func (s *Store) keys(includeDeleted bool) ([]Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Key{}
	if e := s.check(); e != nil {
		return out, e
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("keys")).ForEach(func(_, v []byte) error {
			var k storedKey
			if e := decode(v, &k); e != nil {
				return e
			}
			if k.DeletedAt != nil && !includeDeleted {
				return nil
			}
			out = append(out, s.decorateStored(k))
			return nil
		})
	})
	return out, e
}

// DeleteKey writes a terminal tombstone, clears the authentication hash and
// disables the key in one transaction. It is idempotent, but never discards a
// live request.
func (s *Store) DeleteKey(id string) error {
	if s.maintenanceActive.Load() {
		return ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	for _, a := range s.active {
		if a.KeyID == id {
			return ErrBusy
		}
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("keys"))
		var k storedKey
		if e := get(b, id, &k); e != nil {
			return e
		}
		if k.DeletedAt != nil {
			return nil
		}
		old := k.Hash
		t := s.now().UTC()
		k.DeletedAt = &t
		k.Hash = ""
		k.Secret = ""
		k.Enabled = false
		k.Revoked = false
		if old != "" {
			if e := tx.Bucket([]byte("hashes")).Delete([]byte(old)); e != nil {
				return e
			}
		}
		return put(b, id, k)
	})
}
func authorized(k Key, t time.Time, model *string) error {
	if k.DeletedAt != nil {
		return ErrDeleted
	}
	if !k.Enabled {
		return ErrUnauthorized
	}
	if k.ExpiresAt != nil && !t.Before(*k.ExpiresAt) {
		return ErrExpired
	}
	if model != nil && len(k.AllowedModels) > 0 && !slices.Contains(k.AllowedModels, *model) {
		return ErrForbidden
	}
	return nil
}
func (s *Store) Authenticate(secret string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Key{}, e
	}
	var k storedKey
	e := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("hashes")).Get([]byte(hash(secret)))
		if id == nil {
			return ErrUnauthorized
		}
		return get(tx.Bucket([]byte("keys")), string(id), &k)
	})
	if e == nil {
		if k.DeletedAt != nil {
			return Key{}, ErrUnauthorized
		}
		e = authorized(k.Key, s.now(), nil)
	}
	return s.decorateStored(k), e
}
func (s *Store) Authorize(id, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	return s.db.View(func(tx *bolt.Tx) error {
		var k storedKey
		if e := get(tx.Bucket([]byte("keys")), id, &k); e != nil {
			return e
		}
		return authorized(k.Key, s.now(), &model)
	})
}
func (s *Store) Admit(a Admission) error {
	return s.admit(a, nil)
}

// AdmitWithBody admits one owned generation and snapshots the request-body
// condition results needed by later usage. body is transient and is never
// retained by the store.
func (s *Store) AdmitWithBody(a Admission, body []byte) error {
	return s.admit(a, body)
}

func (s *Store) admit(a Admission, body []byte) error {
	if s.maintenanceActive.Load() {
		return ErrMaintenance
	}
	s.mu.Lock()
	if e := s.check(); e != nil {
		s.mu.Unlock()
		return e
	}
	if a.RequestID == "" {
		s.mu.Unlock()
		return ErrInvalid
	}
	if old, ok := s.active[a.RequestID]; ok {
		s.mu.Unlock()
		if old.KeyID != a.KeyID || old.Model != a.Model || old.TraceID != a.TraceID {
			return ErrInvalid
		}
		return nil
	}
	s.pruneCompletedLocked()
	if old, ok := s.completed[a.RequestID]; ok {
		s.mu.Unlock()
		if old.KeyID != a.KeyID || old.Model != a.Model || old.TraceID != a.TraceID {
			return ErrInvalid
		}
		return nil
	}
	e := s.db.Update(func(tx *bolt.Tx) error {
		var k storedKey
		b := tx.Bucket([]byte("keys"))
		if e := get(b, a.KeyID, &k); e != nil {
			return e
		}
		t := s.now().UTC()
		if e := authorized(k.Key, t, &a.Model); e != nil {
			return e
		}
		roll(&k.Key, t)
		for _, p := range [][2]int64{{k.Used.FiveHour, k.Limits.FiveHour}, {k.Used.Week, k.Limits.Week}, {k.Used.Month, k.Limits.Month}, {k.Used.Total, k.Limits.Total}} {
			if p[1] > 0 && p[0] >= p[1] {
				return ErrQuota
			}
		}
		if k.MaxConcurrent > 0 && s.decorate(k.Key).Active >= k.MaxConcurrent {
			return ErrConcurrency
		}
		if e := correlate(tx, a.KeyID, a.TraceID, a.RequestID, s.now().UTC()); e != nil {
			return e
		}
		return put(b, k.ID, k)
	})
	if e != nil {
		s.mu.Unlock()
		return e
	}
	s.active[a.RequestID] = a
	s.mu.Unlock()
	if a.TraceID == "" {
		return nil
	}
	// Parsing the body is deliberately outside the store lock. The admission is
	// already active, so completion cannot remove its correlation slot first.
	conditions := s.pricer.SnapshotConditions(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.recordAdmissionFactsLocked(a, conditions)
	return nil
}
func (s *Store) Complete(id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	if a, ok := s.active[id]; ok {
		delete(s.active, id)
		a.At = s.now().UTC()
		s.completed[id] = a
		s.pruneCompletedLocked()
		s.completeAdmissionFactsLocked(a)
	}
	return nil
}

func (s *Store) pruneCompletedLocked() {
	cutoff := s.now().Add(-completedTTL)
	for id, a := range s.completed {
		if a.At.Before(cutoff) {
			delete(s.completed, id)
		}
	}
	for len(s.completed) > maxCompleted {
		var oldest string
		var at time.Time
		for id, a := range s.completed {
			if oldest == "" || a.At.Before(at) {
				oldest = id
				at = a.At
			}
		}
		delete(s.completed, oldest)
	}
}
