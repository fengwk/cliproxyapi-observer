package observer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func compactConfig(t *testing.T) Config {
	return testConfig(t, func(c *Config) {
		c.CaptureBodies = true
		c.RequestRetention = time.Minute
		c.BodyRetention = time.Minute
		c.CompactInterval = time.Minute
		c.CompactMinBytes = 64 << 10
		c.MaxBodyStorageBytes = 32 << 20
		c.FlushInterval = time.Hour
	})
}

func inflateStore(t *testing.T, s *Store, clk *clock) {
	t.Helper()
	s.setNow(clk.now)
	body := `{"text":"` + strings.Repeat("x", 512<<10) + `"}`
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("old-%d", i)
		if !captureBody(s, id, body) || !s.SubmitUsage(usageRecord(id, "p", "m", clk.now(), simpleUsage(1, 2))) {
			t.Fatal("unexpected queue rejection")
		}
	}
	flushAll(t, s)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// Measure the canonical file, not bbolt's stale post-rename Path. Check retained
// data, aggregates and sequence across two replacements and reopening.
func TestAutoCompactionPhysicalReclamation(t *testing.T) {
	cfg := compactConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clk := &clock{t: time.Now()}
	inflateStore(t, s, clk)
	before := fileSize(t, cfg.DataPath)
	clk.advance(2 * time.Minute)
	captureBody(s, "keep", `{"text":"retained"}`)
	s.SubmitUsage(usageRecord("keep", "p", "m", clk.now(), simpleUsage(3, 4)))
	clk.advance(time.Second)
	flushAll(t, s)
	if after := fileSize(t, cfg.DataPath); after >= before {
		t.Fatalf("not reclaimed: %d >= %d", after, before)
	}
	if st := s.Status(); st.Compactions != 1 || st.LastCompactionReclaimedBytes <= 0 || st.DatabaseBytes != fileSize(t, cfg.DataPath) {
		t.Fatalf("status: %+v", st)
	}
	body, err := s.Body("keep")
	if err != nil || body.Content != `{"text":"retained"}` {
		t.Fatalf("retained body: %+v %v", body, err)
	}
	reqs := allRequests(t, s)
	if len(reqs) != 1 || reqs[0].Sequence != 25 {
		t.Fatalf("requests: %+v", reqs)
	}
	sum, err := s.Summary(Query{})
	if err != nil || sum.Totals.Requests != 25 {
		t.Fatalf("stats: %+v %v", sum, err)
	}
	// Inflate again after swap to catch accidental use of dst.Path().
	inflateStore(t, s, clk)
	clk.advance(2 * time.Minute)
	s.SubmitUsage(usageRecord("new", "p", "m", clk.now(), simpleUsage(1, 1)))
	flushAll(t, s)
	if s.Status().Compactions != 2 {
		t.Fatal(s.Status())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.setNow(clk.now)
	s.SubmitUsage(usageRecord("reopen", "p", "m", clk.now(), simpleUsage(1, 1)))
	clk.advance(time.Second)
	flushAll(t, s)
	reqs = allRequests(t, s)
	if len(reqs) != 2 || reqs[0].Sequence != 51 {
		t.Fatalf("reopened sequences: %+v", reqs)
	}
}

// Reopen must perform cleanup and reclaim an already-inflated file without an API.
func TestStartupCompaction(t *testing.T) {
	cfg := compactConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().Add(-10 * time.Minute)}
	inflateStore(t, s, clk)
	before := fileSize(t, cfg.DataPath)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fileSize(t, cfg.DataPath) >= before || s.Status().Compactions != 1 {
		t.Fatal("startup did not reclaim", s.Status())
	}
}

// A database file symlink must keep pointing at a reclaimed target, rather
// than being replaced with a new file that leaves the large original behind.
func TestCompactionPreservesDatabaseSymlink(t *testing.T) {
	cfg := compactConfig(t)
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.Symlink(target, cfg.DataPath); err != nil {
		t.Fatal(err)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now()}
	inflateStore(t, s, clk)
	before := fileSize(t, target)
	clk.advance(2 * time.Minute)
	if !captureBody(s, "keep-linked", `{"text":"retained"}`) ||
		!s.SubmitUsage(usageRecord("keep-linked", "p", "m", clk.now(), simpleUsage(3, 4))) {
		t.Fatal("retained observation rejected")
	}
	clk.advance(time.Second)
	flushAll(t, s)
	if after := fileSize(t, target); after >= before {
		t.Fatalf("symlink target not reclaimed: %d >= %d", after, before)
	}
	if link, err := os.Lstat(cfg.DataPath); err != nil || link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("database symlink replaced: %v %v", link, err)
	}
	if s.primaryPath != canonicalTarget || s.Status().DatabaseBytes != fileSize(t, target) {
		t.Fatalf("wrong canonical path or physical size: %s %+v", s.primaryPath, s.Status())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.setNow(clk.now)
	if body, err := s.Body("keep-linked"); err != nil || body.Content != `{"text":"retained"}` {
		t.Fatalf("reopened body through symlink: %+v %v", body, err)
	}
}

// Partial copy, sync and rename failures leave the primary inode bytes intact.
// Retrying maintenance does not poison Flush or subsequent writes.
func TestCompactionFailuresAreIsolated(t *testing.T) {
	for _, phase := range []string{"copy", "sync", "rename"} {
		t.Run(phase, func(t *testing.T) {
			cfg := compactConfig(t)
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			clk := &clock{t: time.Now()}
			inflateStore(t, s, clk)
			var original []byte
			fail := func() error {
				var err error
				original, err = os.ReadFile(cfg.DataPath)
				if err != nil {
					t.Fatal(err)
				}
				return syscall.ENOSPC
			}
			switch phase {
			case "copy":
				s.compactCopy = func(dst, src *bolt.DB, n int64) error {
					if err := dst.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte("partial-sensitive")); return err }); err != nil {
						return err
					}
					return fail()
				}
			case "sync":
				s.compactSync = func(*bolt.DB) error { return fail() }
			case "rename":
				s.compactRename = func(string, string) error { return fail() }
			}
			clk.advance(2 * time.Minute)
			flushAll(t, s)
			current, err := os.ReadFile(cfg.DataPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, current) {
				t.Fatal("primary changed before replacement")
			}
			if s.Status().CompactionErrors != 1 {
				t.Fatal(s.Status())
			}
			files, _ := filepath.Glob(cfg.DataPath + ".observer-compact-*")
			if len(files) != 0 {
				t.Fatal(files)
			}
			for i := 0; i < 3; i++ {
				flushAll(t, s)
			}
			if s.Status().CompactionErrors != 1 {
				t.Fatal("attempt not rate limited")
			}
			// Restore hooks only after Flush has synchronized writer completion.
			s.compactCopy = bolt.Compact
			s.compactSync = (*bolt.DB).Sync
			s.compactRename = os.Rename
			s.SubmitUsage(usageRecord("after-failure", "p", "m", clk.now(), simpleUsage(1, 1)))
			clk.advance(time.Minute)
			flushAll(t, s)
			if s.Status().Compactions != 1 || len(allRequests(t, s)) != 1 {
				t.Fatal(s.Status())
			}
		})
	}
}

// The automatic cleanup ticker triggers compaction. While copy is paused,
// readers and enqueue remain usable; Close drains all accepted observations.
func TestTickerCompactionConcurrentClose(t *testing.T) {
	cfg := compactConfig(t)
	s, err := openStore(cfg, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now()}
	inflateStore(t, s, clk)
	started, release := make(chan struct{}), make(chan struct{})
	s.compactCopy = func(dst, src *bolt.DB, n int64) error { close(started); <-release; return bolt.Compact(dst, src, n) }
	clk.advance(2 * time.Minute)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("ticker did not compact")
	}
	var wg sync.WaitGroup
	accepted := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Requests(Query{})
			if err != nil {
				t.Error(err)
			}
			id := fmt.Sprintf("concurrent-%d", i)
			captured := captureBody(s, id, `{"text":"concurrent"}`)
			accepted <- s.SubmitUsage(usageRecord(id, "p", "m", clk.now(), simpleUsage(1, 1))) && captured
		}(i)
	}
	producersDone := make(chan struct{})
	go func() { wg.Wait(); close(producersDone) }()
	select {
	case <-producersDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("compaction blocked bounded enqueue/read")
	}
	close(accepted)
	clk.advance(time.Second)
	for ok := range accepted {
		if !ok {
			t.Fatal("enqueue rejected below capacity")
		}
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- s.Flush(context.Background()) }()
	readDone := make(chan struct{})
	go func(store *Store) {
		defer close(readDone)
		for {
			if _, err := store.Requests(Query{}); err != nil {
				if !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
				return
			}
		}
	}(s)
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-flushDone // Flush may return ErrClosed if Close wins.
	<-readDone
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.setNow(clk.now)
	if got := len(allRequests(t, s)); got != 100 {
		t.Fatalf("accepted lost: %d", got)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.Body(fmt.Sprintf("concurrent-%d", i)); err != nil {
			t.Fatalf("accepted body lost: %v", err)
		}
	}
}

// Maintenance errors are isolated, but malformed persisted counters still
// fail real writes and retain the existing finalErr/Close contract.
func TestCompactionDoesNotSuppressWriteCorruption(t *testing.T) {
	cfg := compactConfig(t)
	now := time.Now()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(cfg.DataPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(statsKey(statsMinute(now), "p", "m"), []byte("corrupt"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.SubmitUsage(usageRecord("corrupt", "p", "m", now, simpleUsage(1, 1)))
	if err := s.Flush(context.Background()); err == nil {
		t.Error("write corruption suppressed")
	}
	if s.Status().WriteErrors == 0 {
		t.Error("write error not observable")
	}
	if err := s.Close(); err == nil {
		t.Error("Close lost finalErr")
	}
}

func TestStartupOrphanCleanupDoesNotFollowSymlinks(t *testing.T) {
	cfg := compactConfig(t)
	victim := filepath.Join(filepath.Dir(cfg.DataPath), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	orphan := cfg.DataPath + ".observer-compact-1234"
	if err := os.WriteFile(orphan, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	link := cfg.DataPath + ".observer-compact-5678"
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan retained")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink removed")
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Fatal("victim changed")
	}
}

// Build a real expired database, then open it without a writer so eligibility
// boundaries can be checked without cleanup transactions changing page counts.
func maintenanceFixture(t *testing.T) *Store {
	t.Helper()
	cfg := compactConfig(t)
	cfg.CompactMinBytes = MaxCompactMinBytes
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now()}
	inflateStore(t, s, clk)
	clk.advance(2 * time.Minute)
	flushAll(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(cfg.DataPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	s = &Store{
		cfg: cfg, db: db, primaryPath: cfg.DataPath,
		compactCopy: bolt.Compact, compactSync: (*bolt.DB).Sync,
		compactRename: os.Rename, compactRemove: os.Remove,
	}
	t.Cleanup(func() { _ = s.db.Close() })
	return s
}

// The minimum is reclaimable bytes, not primary file size. Equality is
// eligible, but even a lowered threshold must respect the attempt interval.
func TestCompactionReclaimableMinimumBoundary(t *testing.T) {
	s := maintenanceFixture(t)
	size, free, err := s.measureDatabase()
	if err != nil {
		t.Fatal(err)
	}
	if free < size/4 || free < MinCompactMinBytes || free+1 >= size {
		t.Fatalf("fixture does not distinguish thresholds: size=%d free=%d", size, free)
	}
	original, err := os.ReadFile(s.primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.cfg.CompactMinBytes = free + 1
	s.maybeCompact(now)
	if s.Status().Compactions != 0 || !s.lastCompactAttempt.IsZero() {
		t.Fatal("compacted below reclaimable minimum", s.Status())
	}
	s.cfg.CompactMinBytes = free
	s.lastCompactAttempt = now
	s.maybeCompact(now.Add(s.cfg.CompactInterval - time.Nanosecond))
	current, err := os.ReadFile(s.primaryPath)
	if err != nil || !bytes.Equal(original, current) || s.Status().Compactions != 0 {
		t.Fatal("source changed before interval became due", err, s.Status())
	}
	s.maybeCompact(now.Add(s.cfg.CompactInterval))
	if s.Status().Compactions != 1 || fileSize(t, s.primaryPath) >= size {
		t.Fatal("exact reclaimable/interval boundary did not compact", s.Status())
	}
}

// Even a valid compact copy must not replace the source when allocation slack
// leaves an equal/larger file. No-op attempts are rate limited and retried.
func TestCompactionNonShrinkingCopyPreservesSource(t *testing.T) {
	for _, extra := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprintf("extra-%d", extra), func(t *testing.T) {
			s := maintenanceFixture(t)
			s.cfg.CompactMinBytes = MinCompactMinBytes
			size, _, err := s.measureDatabase()
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(s.primaryPath)
			if err != nil {
				t.Fatal(err)
			}
			source := s.db
			attempts := 0
			s.compactCopy = func(dst, src *bolt.DB, n int64) error {
				attempts++
				if dst.AllocSize != 1<<20 {
					return fmt.Errorf("unexpected destination AllocSize: %d", dst.AllocSize)
				}
				if err := bolt.Compact(dst, src, n); err != nil {
					return err
				}
				return os.Truncate(dst.Path(), size+extra)
			}
			now := time.Now()
			s.maybeCompact(now)
			current, err := os.ReadFile(s.primaryPath)
			status := s.Status()
			if err != nil || !bytes.Equal(original, current) || s.db != source {
				t.Fatal("non-shrinking copy replaced/modified source", err)
			}
			if status.Compactions != 0 || status.CompactionErrors != 0 || status.LastCompactionUnix != 0 || status.LastCompactionReclaimedBytes != 0 {
				t.Fatal("no-op reported success/error", status)
			}
			stages, err := filepath.Glob(s.primaryPath + ".observer-compact-*")
			if err != nil || len(stages) != 0 {
				t.Fatal("no-op leaked stage", stages, err)
			}
			if err := source.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketRequests).Put([]byte("still-live"), []byte("value"))
			}); err != nil {
				t.Fatal("no-op closed source", err)
			}
			s.maybeCompact(now.Add(s.cfg.CompactInterval - time.Nanosecond))
			if attempts != 1 {
				t.Fatal("no-op attempt was not rate limited")
			}
			s.compactCopy = bolt.Compact
			s.maybeCompact(now.Add(s.cfg.CompactInterval))
			if s.Status().Compactions != 1 || s.Status().LastCompactionReclaimedBytes <= 0 {
				t.Fatal("no-op did not retry later", s.Status())
			}
			if err := s.view(func(tx *bolt.Tx) error {
				if string(tx.Bucket(bucketRequests).Get([]byte("still-live"))) != "value" {
					return fmt.Errorf("write after no-op lost on retry")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Inject permission failure instead of relying on UID-dependent chmod behavior.
// Startup cleanup reports it without removing symlinks or damaging the primary.
func TestOrphanCompactionRemovalFailureIsObservable(t *testing.T) {
	s := maintenanceFixture(t)
	orphan := s.primaryPath + ".observer-compact-1234"
	if err := os.WriteFile(orphan, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	link := s.primaryPath + ".observer-compact-5678"
	if err := os.Symlink(orphan, link); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(s.primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	s.compactRemove = func(path string) error {
		if path != orphan {
			t.Errorf("cleanup tried removing a symlink: %s", path)
		}
		return syscall.EACCES
	}
	s.removeOrphanCompactions()
	if s.Status().CompactionErrors != 1 {
		t.Fatal("orphan removal error not observable", s.Status())
	}
	current, err := os.ReadFile(s.primaryPath)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatal("orphan removal failure modified primary", err)
	}
	s.compactRemove = os.Remove
	s.removeOrphanCompactions()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan not removed on retry")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink removed", err)
	}
}
