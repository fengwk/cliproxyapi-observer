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

func random() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
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
func (s *Store) decorate(k Key) Key {
	s.observeInterruption(k.ID)
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
	if s.unknown[k.ID] {
		k.BlockedReason = unknownReason
	} else if s.blocked[k.ID] {
		k.BlockedReason = "pending accounting"
	}
	return k
}
func (s *Store) CreateKey(v KeySpec) (Key, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Key{}, "", e
	}
	if !validSpec(v) {
		return Key{}, "", ErrInvalid
	}
	t := s.now().UTC()
	secret := random()
	k := storedKey{Key: Key{ID: random(), KeySpec: cleanSpec(v), CreatedAt: t, Starts: starts(t), Resets: Windows{FiveHour: t.Add(5 * time.Hour), Week: t.Add(7 * 24 * time.Hour), Month: month(t)}}, Hash: hash(secret)}
	e := s.db.Update(func(tx *bolt.Tx) error {
		if e := put(tx.Bucket([]byte("keys")), k.ID, k); e != nil {
			return e
		}
		return tx.Bucket([]byte("hashes")).Put([]byte(k.Hash), []byte(k.ID))
	})
	if e != nil {
		return Key{}, "", e
	}
	return s.decorate(k.Key), secret, nil
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
		old := k.Hash
		if e := f(&k); e != nil {
			return e
		}
		if old != k.Hash {
			h := tx.Bucket([]byte("hashes"))
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
	return s.decorate(k.Key), e
}
func (s *Store) UpdateKey(id string, v KeySpec) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validSpec(v) {
		return Key{}, ErrInvalid
	}
	return s.mutate(id, func(k *storedKey) error { k.KeySpec = cleanSpec(v); return nil })
}
func (s *Store) RotateKey(id string) (Key, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret := random()
	k, e := s.mutate(id, func(k *storedKey) error { k.Hash = hash(secret); return nil })
	if e != nil {
		return Key{}, "", e
	}
	return k, secret, e
}
func (s *Store) RevokeKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := s.mutate(id, func(k *storedKey) error { k.Hash = ""; k.Enabled = false; return nil })
	return e
}
func (s *Store) ResetKey(id string, windows []string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(id, func(k *storedKey) error {
		t := s.now().UTC()
		for _, w := range windows {
			switch w {
			case "five_hour":
				n, e := add(k.Epoch.FiveHour, 1)
				if e != nil {
					return e
				}
				k.Epoch.FiveHour = n
				k.Used.FiveHour = 0
				k.Starts.FiveHour = t
				k.Resets.FiveHour = t.Add(5 * time.Hour)
			case "week":
				n, e := add(k.Epoch.Week, 1)
				if e != nil {
					return e
				}
				k.Epoch.Week = n
				k.Used.Week = 0
				k.Starts.Week = t
				k.Resets.Week = t.Add(7 * 24 * time.Hour)
			case "month":
				n, e := add(k.Epoch.Month, 1)
				if e != nil {
					return e
				}
				k.Epoch.Month = n
				k.Used.Month = 0
				k.Starts.Month = t
				k.Resets.Month = month(t)
			case "total":
				n, e := add(k.Epoch.Total, 1)
				if e != nil {
					return e
				}
				k.Epoch.Total = n
				k.Used.Total = 0
				k.Starts.Total = t
			default:
				return ErrInvalid
			}
		}
		return nil
	})
}
func (s *Store) Keys() ([]Key, error) {
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
			out = append(out, s.decorate(k.Key))
			return nil
		})
	})
	return out, e
}
func authorized(k Key, t time.Time, model *string) error {
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
		e = authorized(k.Key, s.now(), nil)
	}
	return s.decorate(k.Key), e
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	if a.RequestID == "" {
		return ErrInvalid
	}
	if s.observeInterruption(a.KeyID) {
		return ErrAccounting
	}
	if old, ok := s.active[a.RequestID]; ok {
		if old.KeyID != a.KeyID || old.Model != a.Model || old.TraceID != a.TraceID {
			return ErrInvalid
		}
		return nil
	}
	s.pruneCompletedLocked()
	if old, ok := s.completed[a.RequestID]; ok {
		if old.KeyID != a.KeyID || old.Model != a.Model || old.TraceID != a.TraceID {
			return ErrInvalid
		}
		return nil
	}
	if s.unknown[a.KeyID] || s.blocked[a.KeyID] {
		return ErrAccounting
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
	if e == nil {
		s.active[a.RequestID] = a
	}
	return e
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
	}
	return nil
}
