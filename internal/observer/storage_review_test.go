package observer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Reopen before the shortened deadline, then cross it without any sweeper.
// Both direct and trace-resolved reads/listings must use the effective expiry.
func TestReopenBodyRetentionReadBoundary(t *testing.T) {
	for _, tt := range []struct {
		name     string
		prior    time.Duration
		current  time.Duration
		expected time.Duration
	}{
		{"shorten", 24 * time.Hour, time.Hour, time.Hour},
		{"never-extend", time.Hour, 24 * time.Hour, time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t, func(c *Config) {
				c.CaptureBodies = true
				c.BodyRetention = tt.prior
				c.FlushInterval = time.Hour
			})
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(-30 * time.Minute)
			clk := &clock{t: at}
			s.setNow(clk.now)
			if !captureTrace(s, "direct", "shared", `{"a":1}`) {
				t.Fatal("capture rejected")
			}
			for _, id := range []string{"direct", "execution"} {
				if !s.SubmitUsage(usageTrace(id, "shared", "openai", "gpt-5", at, simpleUsage(1, 1))) {
					t.Fatal("usage rejected")
				}
			}
			flushAll(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.BodyRetention = tt.current
			s, err = Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.setNow(clk.now)
			deadline := at.Add(tt.expected)
			clk.set(deadline.Add(-time.Nanosecond))
			for _, id := range []string{"direct", "execution"} {
				detail, err := s.Body(id)
				if err != nil {
					t.Errorf("Body(%s) before boundary: %v", id, err)
				} else if !detail.ExpiresAt.Equal(deadline) {
					t.Errorf("Body(%s) expiry = %s, want %s", id, detail.ExpiresAt, deadline)
				}
				assertBodyAvailable(t, s, id, true)
			}
			clk.set(deadline)
			for _, id := range []string{"direct", "execution"} {
				if _, err := s.Body(id); !errors.Is(err, ErrNotFound) {
					t.Errorf("Body(%s) at boundary = %v, want ErrNotFound", id, err)
				}
				assertBodyAvailable(t, s, id, false)
			}
			if got := bucketCount(t, s, bucketBodies); got != 1 {
				t.Fatalf("test requires unswept body, got %d", got)
			}
		})
	}
}

// Startup must enforce a reduced byte cap, with no new captures or Flush.
func TestReopenLowerBodyStorageCap(t *testing.T) {
	cfg := testConfig(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 100
		c.MaxBodyStorageBytes = 400
		c.FlushInterval = time.Hour
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().Add(-time.Minute)}
	s.setNow(clk.now)
	body := `{"p":"` + strings.Repeat("x", 92) + `"}`
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("cap-%d", i)
		if !captureBody(s, id, body) {
			t.Fatal("capture rejected")
		}
		flushAll(t, s)
		clk.advance(time.Second)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxBodyStorageBytes = 200
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Body("cap-0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("oldest body = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"cap-1", "cap-2"} {
		if _, err := s.Body(id); err != nil {
			t.Errorf("retained %s: %v", id, err)
		}
	}
	for _, name := range [][]byte{bucketBodies, bucketBodyMeta, bucketBodyIndex, bucketTraceBody} {
		if got := bucketCount(t, s, name); got != 2 {
			t.Errorf("%s count = %d, want 2", name, got)
		}
	}
	// Flush is a writer barrier before inspecting writer-owned accounting.
	flushAll(t, s)
	if s.bodyBytes != 200 {
		t.Errorf("bodyBytes = %d, want 200", s.bodyBytes)
	}
}

// A nested bucket at a body key makes bbolt Delete return ErrIncompatibleValue.
// The first body and expired usage/stats are deleted before that fault, so
// unchanged buckets/accounting prove the entire cleanup rolled back.
func TestCleanupBodyFailureRollsBack(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.BodyRetention = time.Minute
		c.RequestRetention = time.Minute
		c.StatsRetentionDays = 1
		c.FlushInterval = time.Hour
	})
	clk := &clock{t: time.Now().Add(-time.Minute)}
	s.setNow(clk.now)
	for _, id := range []string{"first", "fault"} {
		captureBody(s, id, `{"a":1}`)
		s.SubmitUsage(usageRecord(id, "openai", "gpt-5", clk.now(), simpleUsage(1, 1)))
		flushAll(t, s)
		clk.advance(time.Second)
	}
	beforeBytes := s.bodyBytes
	counts := make(map[string]int)
	for _, name := range allBuckets {
		counts[string(name)] = bucketCount(t, s, name)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		bb := tx.Bucket(bucketBodies)
		if err := bb.Delete([]byte("fault")); err != nil {
			return err
		}
		_, err := bb.CreateBucket([]byte("fault"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	clk.advance(48 * time.Hour)
	if err := s.runCleanup(); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Errorf("cleanup = %v, want ErrIncompatibleValue", err)
	}
	for _, name := range allBuckets {
		if got := bucketCount(t, s, name); got != counts[string(name)] {
			t.Errorf("%s count = %d, want rollback to %d", name, got, counts[string(name)])
		}
	}
	if s.bodyBytes != beforeBytes {
		t.Errorf("bodyBytes = %d, want rollback to %d", s.bodyBytes, beforeBytes)
	}
	if s.Status().WriteErrors != 1 {
		t.Errorf("WriteErrors = %d, want 1", s.Status().WriteErrors)
	}
	if err := s.Close(); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Errorf("Close = %v, want cleanup error", err)
	}
}

// A cap eviction failure during Open must roll back earlier evictions and
// release the database, so a subsequent opener can inspect/recover the data.
func TestStartupCapFailureRollsBack(t *testing.T) {
	cfg := testConfig(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 7
		c.MaxBodyStorageBytes = 28
		c.FlushInterval = time.Hour
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().Add(-time.Minute)}
	s.setNow(clk.now)
	for _, id := range []string{"first", "fault", "newest"} {
		if !captureBody(s, id, `{"a":1}`) {
			t.Fatal("capture rejected")
		}
		flushAll(t, s)
		clk.advance(time.Second)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		bb := tx.Bucket(bucketBodies)
		if err := bb.Delete([]byte("fault")); err != nil {
			return err
		}
		_, err := bb.CreateBucket([]byte("fault"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.MaxBodyStorageBytes = 7
	failed, err := Open(cfg)
	if failed != nil {
		failed.Close()
	}
	if !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Fatalf("Open = %v, want eviction error", err)
	}
	db, err := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("startup failure did not release database: %v", err)
	}
	defer db.Close()
	inspector := &Store{db: db}
	for _, name := range [][]byte{bucketBodies, bucketBodyMeta, bucketBodyIndex, bucketTraceBody} {
		if got := bucketCount(t, inspector, name); got != 3 {
			t.Errorf("%s count = %d, want rollback to 3", name, got)
		}
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket(bucketBodies).Get([]byte("first"))) != `{"a":1}` {
			t.Error("first eviction was not rolled back")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Drive the barrier's bounded write pass without select scheduling: an
// oversized identifier rolls back the first full batch, then two writes succeed.
func TestDrainReturnsFirstBatchError(t *testing.T) {
	cfg := testConfig(t, nil)
	db, err := bolt.Open(cfg.DataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{cfg: cfg, db: db, usageCh: make(chan Request, 3*writeBatchSize), bodyCh: make(chan pendingBody, 1)}
	if err := s.initBuckets(); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	for i := 0; i < 2*writeBatchSize+1; i++ {
		id := fmt.Sprintf("drain-%d", i)
		if i == writeBatchSize-1 {
			id = strings.Repeat("x", bolt.MaxKeySize+1)
		}
		s.usageCh <- NormalizeUsage(usageRecord(id, "openai", "gpt-5", at, simpleUsage(1, 1)))
	}
	var usage []Request
	var bodies []pendingBody
	var batchSizes []int
	var batchErrors []error
	flush := func() error {
		if len(usage)+len(bodies) == 0 {
			return nil
		}
		batchSizes = append(batchSizes, len(usage))
		err := s.writeBatch(usage, bodies)
		batchErrors = append(batchErrors, err)
		s.pendingCount.Add(-int64(len(usage) + len(bodies)))
		usage = usage[:0]
		bodies = bodies[:0]
		return err
	}
	if err := s.drainQueues(&usage, &bodies, flush); !errors.Is(err, bolt.ErrKeyTooLarge) {
		t.Errorf("drain = %v, want first batch ErrKeyTooLarge", err)
	}
	if fmt.Sprint(batchSizes) != "[100 100 1]" {
		t.Errorf("batches = %v, want [100 100 1]", batchSizes)
	}
	if len(batchErrors) != 3 || !errors.Is(batchErrors[0], bolt.ErrKeyTooLarge) || batchErrors[1] != nil || batchErrors[2] != nil {
		t.Errorf("batch errors = %v, want [ErrKeyTooLarge nil nil]", batchErrors)
	}
	if got := bucketCount(t, s, bucketRequests); got != writeBatchSize+1 {
		t.Errorf("stored requests = %d, want %d", got, writeBatchSize+1)
	}
	if s.pendingCount.Load() != 0 {
		t.Errorf("pending = %d, want 0", s.pendingCount.Load())
	}
	if err := s.drainQueues(&usage, &bodies, flush); err != nil {
		t.Errorf("later empty drain = %v, want nil", err)
	}
}

// A failed barrier reports loss, but does not poison future Flush calls; Close
// still reports the recorded failure. Only one queued item avoids batch races.
func TestFlushFailureDoesNotPoisonLaterBarrier(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.FlushInterval = time.Hour
	})
	if !captureBody(s, strings.Repeat("x", bolt.MaxKeySize+1), `{"a":1}`) {
		t.Fatal("capture rejected")
	}
	if err := s.Flush(context.Background()); !errors.Is(err, bolt.ErrKeyTooLarge) {
		t.Errorf("Flush = %v, want ErrKeyTooLarge", err)
	}
	status := s.Status()
	if status.DroppedBodies != 1 || status.WriteErrors != 1 || status.Queued != 0 {
		t.Errorf("failed batch status = %+v", status)
	}
	if s.queuedBodyBytes.Load() != 0 {
		t.Errorf("queuedBodyBytes = %d, want 0", s.queuedBodyBytes.Load())
	}
	if !captureBody(s, "recovered", `{"a":1}`) {
		t.Fatal("later capture rejected")
	}
	flushAll(t, s)
	if _, err := s.Body("recovered"); err != nil {
		t.Errorf("later body: %v", err)
	}
	if err := s.Close(); !errors.Is(err, bolt.ErrKeyTooLarge) {
		t.Errorf("Close = %v, want recorded write error", err)
	}
}
