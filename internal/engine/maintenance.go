package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) Maintenance() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	if e := s.retryLocked(); e != nil {
		return e
	}
	now := s.now().UTC()
	cut := now.Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
	dc := day(now).AddDate(0, 0, -s.settings.DailyRetentionDays)
	e := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"records", "captures", "daily"} {
			b := tx.Bucket([]byte(name))
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				expired := false
				switch name {
				case "records":
					var r Record
					if e := decode(v, &r); e != nil {
						return e
					}
					expired = r.Time.Before(cut)
					if expired {
						if e := tx.Bucket([]byte("timeline")).Delete([]byte(timeID(r))); e != nil {
							return e
						}
					}
				case "captures":
					var r capture
					if e := decode(v, &r); e != nil {
						return e
					}
					expired = r.At.Before(cut)
				case "daily":
					var r daily
					if e := decode(v, &r); e != nil {
						return e
					}
					expired = r.Bucket.Time.Before(dc)
				}
				if expired {
					if e := c.Delete(); e != nil {
						return e
					}
				}
			}
		}
		return nil
	})
	if e != nil {
		return e
	}
	st, e := os.Stat(s.path)
	if e != nil {
		return e
	}
	free := int64(s.db.Stats().FreePageN) * int64(s.db.Info().PageSize)
	if free > 16<<20 && free > st.Size()/4 {
		return s.compactLocked()
	}
	return nil
}
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	return s.compactLocked()
}
func (s *Store) compactLocked() error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ErrInvalid
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".compact-")
	if e != nil {
		return e
	}
	stage := f.Name()
	defer os.Remove(stage)
	if e = f.Close(); e != nil {
		return e
	}
	dst, e := bolt.Open(stage, 0600, nil)
	if e != nil {
		return e
	}
	swapped := false
	defer func() {
		if !swapped {
			dst.Close()
		}
	}()
	dst.AllocSize = 1 << 20
	if e = bolt.Compact(dst, s.db, 1<<20); e != nil {
		return e
	}
	if e = dst.Sync(); e != nil {
		return e
	}
	if e = os.Rename(stage, s.path); e != nil {
		return e
	}
	old := s.db
	s.db = dst
	swapped = true
	// The new locked handle remains valid even if closing/fsync subsequently
	// fails. Never reopen or revert to the invalid old handle.
	ce := old.Close()
	dir, e := os.Open(filepath.Dir(s.path))
	if e != nil {
		return e
	}
	se := dir.Sync()
	de := dir.Close()
	if ce != nil {
		return ce
	}
	if se != nil {
		return se
	}
	return de
}
