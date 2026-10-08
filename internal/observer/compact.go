package observer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Stages are never recovery candidates. Open already holds the primary lock.
func (s *Store) removeOrphanCompactions() {
	entries, err := os.ReadDir(filepath.Dir(s.primaryPath))
	if err != nil {
		s.compactionErrors.Add(1)
		return
	}
	prefix := filepath.Base(s.primaryPath) + ".observer-compact-"
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			s.compactionErrors.Add(1)
			continue
		}
		if info.Mode().IsRegular() {
			if err := s.compactRemove(filepath.Join(filepath.Dir(s.primaryPath), entry.Name())); err != nil && !os.IsNotExist(err) {
				s.compactionErrors.Add(1)
			}
		}
	}
}

// Measurements are writer-owned; Status never performs disk I/O.
func (s *Store) measureDatabase() (int64, int64, error) {
	info, err := os.Stat(s.primaryPath)
	if err != nil {
		return 0, 0, err
	}
	stats := s.db.Stats()
	free := int64(stats.FreePageN+stats.PendingPageN) * int64(s.db.Info().PageSize)
	s.databaseBytes.Store(info.Size())
	s.reclaimableBytes.Store(free)
	return info.Size(), free, nil
}

func (s *Store) maybeCompact(now time.Time) {
	size, free, err := s.measureDatabase()
	if !s.lastCompactAttempt.IsZero() && now.Sub(s.lastCompactAttempt) < s.cfg.CompactInterval {
		return
	}
	if err == nil && (free < s.cfg.CompactMinBytes || free < size/4) {
		return
	}
	s.lastCompactAttempt = now
	if err != nil {
		s.compactionErrors.Add(1)
		return
	}
	if err = s.compactDatabase(); err != nil {
		s.compactionErrors.Add(1)
	}
	_, _, _ = s.measureDatabase()
}

func (s *Store) compactDatabase() (resultErr error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("atomic database replacement unsupported")
	}
	file, err := os.CreateTemp(filepath.Dir(s.primaryPath), filepath.Base(s.primaryPath)+".observer-compact-")
	if err != nil {
		return err
	}
	stage := file.Name()
	defer func() {
		if err := s.compactRemove(stage); err != nil && !os.IsNotExist(err) && resultErr == nil {
			resultErr = err
		}
	}()
	if err := file.Close(); err != nil {
		return err
	}
	dst, err := bolt.Open(stage, 0o600, nil)
	if err != nil {
		return err
	}
	// Bound allocation slack instead of bbolt's default 16MiB growth quantum.
	dst.AllocSize = 1 << 20
	swapped := false
	defer func() {
		if !swapped {
			if err := dst.Close(); err != nil && resultErr == nil {
				resultErr = err
			}
		}
	}()
	// Readers may keep using source during the copy; only writer is paused.
	if err := s.compactCopy(dst, s.db, 1<<20); err != nil {
		return err
	}
	if err := s.compactSync(dst); err != nil {
		return err
	}
	before := s.databaseBytes.Load()
	info, err := os.Stat(stage)
	if err != nil {
		return err
	}
	// Allocation slack can defeat free-page estimates. A no-op is still an
	// interval-limited attempt, never a successful swap.
	if info.Size() >= before {
		return nil
	}
	s.mu.Lock()
	if err := s.compactRename(stage, s.primaryPath); err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.db
	s.db = dst // dst.Path() is stale after rename: always use primaryPath.
	swapped = true
	s.mu.Unlock()
	s.compactions.Add(1)
	s.lastCompactionUnix.Store(s.now().Unix())
	reclaimed := before - info.Size()
	s.lastCompactionReclaimedBytes.Store(reclaimed)
	// Post-swap errors must never discard the working new database.
	closeErr := old.Close()
	dir, err := os.Open(filepath.Dir(s.primaryPath))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	dirErr := dir.Close()
	for _, err := range []error{closeErr, syncErr, dirErr} {
		if err != nil {
			return err
		}
	}
	return nil
}
