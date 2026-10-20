package engine

import (
	"time"

	bolt "go.etcd.io/bbolt"
)

const unknownReason = "unknown accounting: explicit acknowledgement required"
const replayHorizon = 7 * 24 * time.Hour
const completedTTL = 24 * time.Hour
const maxCompleted = 4096
const maxCaptureBody = 8 << 20
const maxCaptureStorage = 1 << 30

type dedupEntry struct {
	Time  time.Time
	KeyID string
}

func (s *Store) loadCutoff(tx *bolt.Tx) error {
	m := tx.Bucket([]byte("meta"))
	var cutoff time.Time
	if b := m.Get([]byte("replay_cutoff")); b != nil {
		if e := decode(b, &cutoff); e != nil {
			return e
		}
	}
	candidate := s.now().UTC().Add(-replayHorizon)
	if candidate.After(cutoff) {
		cutoff = candidate
	}
	if s.cutoff.After(cutoff) {
		cutoff = s.cutoff
	}
	s.cutoff = cutoff
	return nil
}
func (s *Store) advanceCutoff(tx *bolt.Tx) error {
	if e := s.loadCutoff(tx); e != nil {
		return e
	}
	return put(tx.Bucket([]byte("meta")), "replay_cutoff", s.cutoff)
}
func (s *Store) fenceLocked(id string) {
	s.unknown[id] = true
	s.lastError = unknownReason
	// A failed fence write is still safe across restart: running remains set.
	s.db.Update(func(tx *bolt.Tx) error {
		var k storedKey
		b := tx.Bucket([]byte("keys"))
		if e := get(b, id, &k); e != nil {
			return e
		}
		k.BlockedReason = unknownReason
		return put(b, id, k)
	})
}
func (s *Store) fenceAllLocked() {
	s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("keys")).ForEach(func(id, _ []byte) error { s.unknown[string(id)] = true; return nil })
	})
	for id := range s.unknown {
		s.fenceLocked(id)
	}
}
func (s *Store) RecoverKey(id string, acknowledge bool) (Key, error) {
	if s.maintenanceActive.Load() {
		return Key{}, ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return Key{}, e
	}
	// Retry every retained quote; failure for another Key must not prevent this
	// Key's recovery. The target's remaining pending events are never forgiven.
	s.retryLocked()
	if s.blocked[id] {
		return Key{}, ErrAccounting
	}
	path, e := interruptionPath(s.path, id)
	if e != nil {
		return Key{}, e
	}
	var recovered Key
	e = withInterruptionLock(path, func() error {
		present, err := markerPresent(s.path, id)
		if err != nil {
			s.unknown[id] = true
			s.lastError = interruptionIOReason
			return ErrAccounting
		}
		if present {
			s.unknown[id] = true
			s.lastError = interruptionReason
		}
		if s.writeFault != nil {
			if err = s.writeFault(); err != nil {
				return ErrAccounting
			}
		}
		// Persist a DB fence BEFORE unlink. If unlink or directory fsync fails, the
		// fence survives both this process and restart even if the file is gone.
		if present {
			if !acknowledge {
				return ErrAccounting
			}
			if _, err = s.mutate(id, func(k *storedKey) error { k.BlockedReason = unknownReason; return nil }); err != nil {
				return err
			}
			if err = removeInterruption(path); err != nil {
				s.lastError = interruptionIOReason
				return ErrAccounting
			}
		}
		recovered, err = s.mutate(id, func(k *storedKey) error {
			if (s.unknown[id] || k.BlockedReason != "") && !acknowledge {
				return ErrAccounting
			}
			k.BlockedReason = ""
			return nil
		})
		if err != nil {
			return err
		}
		delete(s.unknown, id)
		if len(s.unknown) == 0 && len(s.pending) == 0 {
			s.lastError = ""
		}
		recovered.BlockedReason = ""
		return nil
	})
	if e != nil {
		return Key{}, e
	}
	return s.decorate(recovered), nil
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
