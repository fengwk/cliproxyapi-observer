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
		return
	}
	prefix := filepath.Base(s.primaryPath) + ".observer-compact-"
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() {
			_ = os.Remove(filepath.Join(filepath.Dir(s.primaryPath), entry.Name()))
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
	if err == nil && (size < s.cfg.CompactMinBytes || free < size/4) {
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

func (s *Store) compactDatabase() error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("atomic database replacement unsupported")
	}
	file, err := os.CreateTemp(filepath.Dir(s.primaryPath), filepath.Base(s.primaryPath)+".observer-compact-")
	if err != nil {
		return err
	}
	stage := file.Name()
	defer os.Remove(stage)
	if err := file.Close(); err != nil {
		return err
	}
	dst, err := bolt.Open(stage, 0o600, nil)
	if err != nil {
		return err
	}
	swapped := false
	defer func() {
		if !swapped {
			_ = dst.Close()
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
	if reclaimed < 0 {
		reclaimed = 0
	}
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
