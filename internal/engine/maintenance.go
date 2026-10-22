package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	bolt "go.etcd.io/bbolt"
)

const compactRetryGate = 15 * time.Minute

func (s *Store) Compact() error {
	if s.maintenanceActive.Load() {
		return ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	if s.maintenanceBusyLocked() {
		return ErrBusy
	}
	if e := s.compactLocked(true); e != nil {
		s.maintenanceError = "compaction failed"
		return e
	}
	s.maintenanceError = ""
	return nil
}

func (s *Store) maintenanceBusyLocked() bool {
	return len(s.active) > 0 || len(s.pending) > 0 || s.captureQueued.Load() > 0
}

func (s *Store) Maintenance() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(); e != nil {
		return e
	}
	retryErr := s.retryLocked()
	s.pruneCompletedLocked()
	now := s.now().UTC()
	cut := now.Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
	dc := day(now).AddDate(0, 0, -s.settings.DailyRetentionDays)
	var cleaned CleanupResult
	maintenanceErr := ""
	e := s.db.Update(func(tx *bolt.Tx) error {
		if e := s.advanceCutoff(tx); e != nil {
			return e
		}
		if s.cleanFault != nil {
			if e := s.cleanFault(); e != nil {
				return e
			}
		}
		var e error
		cleaned, e = cleanupExpiredLocked(tx, now, s.settings.CaptureRetentionSeconds)
		if e != nil {
			return e
		}
		for _, name := range []string{"records", "hourly", "minutes", "daily", "dedup", "correlations"} {
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
						// Content and its lightweight state are unreachable once
						// the metadata row is gone.
						cid := captureID(r.KeyID, r.TraceID)
						if e := removeCaptureContent(tx, cid, captureStatusExpired, captureReasonExpired); e != nil {
							return e
						}
						if e := tx.Bucket([]byte("capture_state")).Delete([]byte(cid)); e != nil {
							return e
						}
					}
				case "hourly":
					var d daily
					if e := decode(v, &d); e != nil {
						return e
					}
					expired = d.Bucket.Time.Before(cut)
				case "minutes":
					var d daily
					if e := decode(v, &d); e != nil {
						return e
					}
					// Minute aggregates are a long-lived historical source and
					// therefore follow daily retention, never raw retention.
					expired = d.Bucket.Time.Before(dc)
				case "dedup":
					var entry dedupEntry
					if e := decode(v, &entry); e != nil {
						return e
					}
					expired = entry.Time.Before(s.cutoff)
				case "correlations":
					var entry correlation
					if e := decode(v, &entry); e != nil {
						return e
					}
					expired = entry.At.Before(cut)
					if expired {
						if e := tx.Bucket([]byte("capture_state")).Delete(k); e != nil {
							return e
						}
					}
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
		maintenanceErr = "maintenance cleanup failed"
		s.maintenanceError = maintenanceErr
		return e
	}
	s.lastCleanupAt = now
	s.lastCleanupRecord = cleaned.RemovedRecords
	s.lastCleanupBytes = cleaned.RemovedBytes
	if s.maintenanceError == "maintenance cleanup failed" {
		s.maintenanceError = ""
	}

	// The 15-minute gate is independent of whether the previous attempt
	// succeeded; a failed compaction is retried later, not on every minute.
	if !s.maintenanceBusyLocked() && (s.lastCompactTry.IsZero() || now.Sub(s.lastCompactTry) >= compactRetryGate) {
		st, e := os.Stat(s.path)
		if e != nil {
			s.lastCompactTry = now
			s.maintenanceError = "compaction check failed"
			maintenanceErr = "compaction check failed"
		} else {
			if s.maintenanceError == "compaction check failed" {
				s.maintenanceError = ""
			}
			free := int64(s.db.Stats().FreePageN) * int64(s.db.Info().PageSize)
			if free > 16<<20 && free > st.Size()/4 {
				s.lastCompactTry = now
				if e := s.compactLocked(false); e != nil {
					s.maintenanceError = "compaction failed"
					maintenanceErr = "compaction failed"
				} else if s.maintenanceError == "compaction failed" {
					s.maintenanceError = ""
				}
			}
		}
	}
	if retryErr != nil {
		return retryErr
	}
	if maintenanceErr != "" {
		return ErrMaintenance
	}
	return nil
}

func (s *Store) compactLocked(manual bool) error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return ErrInvalid
	}
	before, e := os.Stat(s.path)
	if e != nil {
		return e
	}
	s.maintenanceActive.Store(true)
	// No deferred unlock exists here: the caller holds s.mu, and the flag only
	// tells new admissions to fail fast instead of queueing behind compaction.
	defer s.maintenanceActive.Store(false)
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
	// Wait for readers using the old handle, then swap. New query readers cannot
	// enter because physical maintenance also holds s.mu while this runs.
	s.dbMu.Lock()
	if e = os.Rename(stage, s.path); e != nil {
		s.dbMu.Unlock()
		return e
	}
	old := s.db
	s.db = dst
	swapped = true
	// The new locked handle remains valid even if closing/fsync subsequently
	// fails. Never reopen or revert to the invalid old handle.
	ce := old.Close()
	s.dbMu.Unlock()
	dir, e := os.Open(filepath.Dir(s.path))
	if e != nil {
		return e
	}
	se := dir.Sync()
	de := dir.Close()
	after, statErr := os.Stat(s.path)
	if statErr != nil {
		return statErr
	}
	s.lastCompactAt = s.now().UTC()
	s.lastCompactTry = s.lastCompactAt
	s.lastCompactBefore = before.Size()
	s.lastCompactAfter = after.Size()
	if manual {
		s.maintenanceError = ""
	}
	if ce != nil {
		return ce
	}
	if se != nil {
		return se
	}
	return de
}
