package observer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type dbSnapshot struct {
	bodyBytes int64
	bodies    map[string]string
	meta      map[string]string
	index     map[string]string
	trace     map[string]string
	refs      map[string]uint64
	rawRefs   map[string]string
}

func (s dbSnapshot) equalData(o dbSnapshot) bool {
	return s.bodyBytes == o.bodyBytes &&
		reflect.DeepEqual(s.bodies, o.bodies) &&
		reflect.DeepEqual(s.meta, o.meta) &&
		reflect.DeepEqual(s.index, o.index) &&
		reflect.DeepEqual(s.trace, o.trace) &&
		reflect.DeepEqual(s.rawRefs, o.rawRefs)
}

func takeSnapshot(t *testing.T, s *Store) dbSnapshot {
	t.Helper()
	snap := dbSnapshot{
		bodyBytes: s.bodyBytes,
		bodies:    make(map[string]string),
		meta:      make(map[string]string),
		index:     make(map[string]string),
		trace:     make(map[string]string),
		refs:      make(map[string]uint64),
		rawRefs:   make(map[string]string),
	}
	err := s.view(func(tx *bolt.Tx) error {
		dump := func(name []byte, target map[string]string) {
			if b := tx.Bucket(name); b != nil {
				_ = b.ForEach(func(k, v []byte) error { target[string(k)] = string(v); return nil })
			}
		}
		dump(bucketBodies, snap.bodies)
		dump(bucketBodyMeta, snap.meta)
		dump(bucketBodyIndex, snap.index)
		dump(bucketTraceBody, snap.trace)
		dump(bucketTraceBodyRefs, snap.rawRefs)
		if rb := tx.Bucket(bucketTraceBodyRefs); rb != nil {
			_ = rb.ForEach(func(k, v []byte) error {
				if len(v) == 8 {
					snap.refs[string(k)] = binary.BigEndian.Uint64(v)
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("takeSnapshot: %v", err)
	}
	return snap
}

func rawBoltUpdate(t *testing.T, path string, fn func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

// Injection runs only after Flush has quiesced the writer's pending batch.
func liveBoltUpdate(t *testing.T, s *Store, fn func(*bolt.Tx) error) {
	t.Helper()
	if err := s.db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

// Reopen repairs legacy/stale counts without exposing live ambiguous captures.
func TestTraceRefs_RebuildLegacyAndStale(t *testing.T) {
	t.Run("legacy_rebuild_and_orphan_cleanup", func(t *testing.T) {
		cfg := testConfig(t, func(c *Config) { c.CaptureBodies = true })
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Second)
		captureTrace(s, "req-norm", "trace-norm", `{"msg":"norm"}`)
		s.SubmitUsage(usageTrace("use-norm", "trace-norm", "p", "m", at, simpleUsage(1, 1)))
		captureTrace(s, "req-amb-1", "trace-amb", `{"msg":"a1"}`)
		captureTrace(s, "req-amb-2", "trace-amb", `{"msg":"a2"}`)
		captureTrace(s, "req-empty", "", `{"empty":"trace"}`)
		s.SubmitUsage(usageTrace("use-amb", "trace-amb", "p", "m", at, simpleUsage(1, 1)))
		flushAll(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		rawBoltUpdate(t, cfg.DataPath, func(tx *bolt.Tx) error {
			if err := tx.DeleteBucket(bucketTraceBodyRefs); err != nil {
				return err
			}
			return tx.Bucket(bucketTraceBody).Put([]byte("orphan"), ambiguousTrace)
		})

		s2, err := Open(cfg)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		snap := takeSnapshot(t, s2)
		if snap.refs["trace-norm"] != 1 || snap.refs["trace-amb"] != 2 || len(snap.rawRefs) != 2 {
			t.Fatalf("unexpected refs: %+v", snap.refs)
		}
		if snap.trace["orphan"] != "" || snap.trace["trace-norm"] != "req-norm" || !bytes.Equal([]byte(snap.trace["trace-amb"]), ambiguousTrace) {
			t.Fatalf("unexpected trace entries: %+v", snap.trace)
		}
		if b, err := s2.Body("req-norm"); err != nil || b.Content != `{"msg":"norm"}` {
			t.Fatalf("direct req-norm: %v", err)
		}
		if b, err := s2.Body("use-norm"); err != nil || b.Content != `{"msg":"norm"}` {
			t.Fatalf("correlated use-norm: %v", err)
		}
		if _, err := s2.Body("use-amb"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("correlated use-amb err = %v, want ErrNotFound", err)
		}
		for _, id := range []string{"req-amb-1", "req-amb-2"} {
			if _, err := s2.Body(id); err != nil {
				t.Fatalf("direct ambiguous body %s: %v", id, err)
			}
		}
		if _, err := s2.Body("req-empty"); err != nil {
			t.Fatal("empty trace direct read", err)
		}
		if err := s2.Close(); err != nil {
			t.Fatal(err)
		}

		s3, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s3.Close()
		if snap3 := takeSnapshot(t, s3); snap3.refs["trace-norm"] != 1 || snap3.refs["trace-amb"] != 2 {
			t.Fatalf("stable reopen refs mismatch: %+v", snap3.refs)
		}
	})

	t.Run("stale_and_corrupt_refs_rebuilt", func(t *testing.T) {
		cfg := testConfig(t, func(c *Config) { c.CaptureBodies = true })
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		captureTrace(s, "req-1", "trace-1", `{"a":1}`)
		captureTrace(s, "req-2", "trace-2", `{"a":2}`)
		flushAll(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		rawBoltUpdate(t, cfg.DataPath, func(tx *bolt.Tx) error {
			if err := tx.Bucket(bucketTraceBodyRefs).Put([]byte("trace-1"), []byte{1, 2, 3}); err != nil {
				return err
			}
			if err := tx.Bucket(bucketTraceBodyRefs).Put([]byte("ghost"), encodeTraceBodyRefs(999)); err != nil {
				return err
			}
			if err := tx.Bucket(bucketTraceBodyRefs).Put([]byte("trace-2"), encodeTraceBodyRefs(999)); err != nil {
				return err
			}
			return tx.Bucket(bucketTraceBody).Put([]byte("ghost"), []byte("ghost-req"))
		})

		s2, err := Open(cfg)
		if err != nil {
			t.Fatalf("reopen Open: %v", err)
		}
		defer s2.Close()
		if snap := takeSnapshot(t, s2); snap.refs["trace-1"] != 1 || snap.refs["trace-2"] != 1 || len(snap.rawRefs) != 2 || snap.trace["ghost"] != "" {
			t.Fatalf("refs/trace after stale repair: %+v / %+v", snap.refs, snap.trace)
		}
	})
}

// Failed reconstruction rolls back its bucket changes and releases the file lock.
func TestTraceRefs_MalformedMetadataOpenErrorRollback(t *testing.T) {
	cfg := testConfig(t, func(c *Config) { c.CaptureBodies = true })
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	captureTrace(s, "req-ok", "trace-ok", `{"ok":true}`)
	flushAll(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	rawBoltUpdate(t, cfg.DataPath, func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketTraceBodyRefs).Put([]byte("seeded-trace"), encodeTraceBodyRefs(42)); err != nil {
			return err
		}
		return tx.Bucket(bucketBodyMeta).Put([]byte("req-bad"), []byte("corrupt-bytes"))
	})

	s2, err := Open(cfg)
	if err == nil {
		s2.Close()
		t.Fatal("expected Open error on malformed body_meta")
	}

	rawDB, lockErr := bolt.Open(cfg.DataPath, 0o600, &bolt.Options{Timeout: time.Second})
	if lockErr != nil {
		t.Fatalf("failed Open did not release lock: %v", lockErr)
	}
	defer rawDB.Close()

	if err := rawDB.View(func(tx *bolt.Tx) error {
		val := tx.Bucket(bucketTraceBodyRefs).Get([]byte("seeded-trace"))
		if len(val) != 8 || binary.BigEndian.Uint64(val) != 42 {
			t.Fatalf("refs transaction did not rollback, seeded-trace = %v", val)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Invalid deletion counts must not partially remove blobs, indexes or byte totals.
func TestTraceRefs_DeletionRollback(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(*bolt.Bucket, []byte) error
	}{
		{"missing", func(b *bolt.Bucket, k []byte) error { return b.Delete(k) }},
		{"malformed", func(b *bolt.Bucket, k []byte) error { return b.Put(k, []byte{1, 2, 3}) }},
		{"zero", func(b *bolt.Bucket, k []byte) error { return b.Put(k, encodeTraceBodyRefs(0)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
			cfg := testConfig(t, func(c *Config) {
				c.CaptureBodies = true
				c.BodyRetention = 10 * time.Minute
				c.FlushInterval = time.Hour
			})
			s, err := openStore(cfg, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			s.setNow(clk.now)
			t.Cleanup(func() { _ = s.Close() })

			captureTrace(s, "req-exp", "trace-exp", `{"purpose":"exp"}`)
			flushAll(t, s)
			clk.advance(5 * time.Minute)
			captureTrace(s, "req-keep", "trace-keep", `{"purpose":"keep"}`)
			flushAll(t, s)

			liveBoltUpdate(t, s, func(tx *bolt.Tx) error { return tc.corrupt(tx.Bucket(bucketTraceBodyRefs), []byte("trace-exp")) })
			beforeSnap := takeSnapshot(t, s)
			beforeWriteErrors := s.Status().WriteErrors

			clk.advance(6 * time.Minute)
			if err := s.runCleanup(); err == nil {
				t.Fatalf("%s: expected cleanup error", tc.name)
			}
			if s.Status().WriteErrors <= beforeWriteErrors {
				t.Fatalf("%s: writeErrors not incremented", tc.name)
			}
			if afterSnap := takeSnapshot(t, s); !afterSnap.equalData(beforeSnap) {
				t.Fatalf("%s: database state not rolled back: %+v != %+v", tc.name, afterSnap, beforeSnap)
			}

			liveBoltUpdate(t, s, func(tx *bolt.Tx) error {
				return tx.Bucket(bucketTraceBodyRefs).Put([]byte("trace-exp"), encodeTraceBodyRefs(1))
			})
			if closeErr := s.Close(); closeErr == nil {
				t.Fatalf("%s: expected Close finalErr", tc.name)
			}
		})
	}
}

// Duplicates keep one reference; invalid counts roll back the whole new capture.
func TestTraceRefs_InsertionRollbackAndDuplicate(t *testing.T) {
	t.Run("duplicate_no_increment", func(t *testing.T) {
		s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
		if !captureTrace(s, "req-dup", "trace-dup", `{"dup":1}`) {
			t.Fatal("capture rejected")
		}
		flushAll(t, s)
		if snap := takeSnapshot(t, s); snap.refs["trace-dup"] != 1 {
			t.Fatalf("initial ref = %d, want 1", snap.refs["trace-dup"])
		}
		if !captureTrace(s, "req-dup", "trace-other", `{"dup":2}`) {
			t.Fatal("duplicate enqueue rejected")
		}
		flushAll(t, s)
		if snap := takeSnapshot(t, s); snap.refs["trace-dup"] != 1 || len(snap.rawRefs) != 1 || snap.bodies["req-dup"] != `{"dup":1}` {
			t.Fatalf("duplicate changed stored state: %+v", snap)
		}
	})

	cases := []struct {
		name    string
		corrupt func(*bolt.Bucket, []byte) error
	}{
		{"missing", func(b *bolt.Bucket, k []byte) error { return b.Delete(k) }},
		{"zero", func(b *bolt.Bucket, k []byte) error { return b.Put(k, encodeTraceBodyRefs(0)) }},
		{"malformed", func(b *bolt.Bucket, k []byte) error { return b.Put(k, []byte{1, 2, 3}) }},
		{"overflow", func(b *bolt.Bucket, k []byte) error { return b.Put(k, encodeTraceBodyRefs(math.MaxUint64)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, func(c *Config) {
				c.CaptureBodies = true
				c.FlushInterval = time.Hour
			})
			s, err := openStore(cfg, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			captureTrace(s, "req-base", "trace-target", `{"base":true}`)
			flushAll(t, s)
			t.Cleanup(func() { _ = s.Close() })
			beforeWriteErrors := s.Status().WriteErrors

			liveBoltUpdate(t, s, func(tx *bolt.Tx) error { return tc.corrupt(tx.Bucket(bucketTraceBodyRefs), []byte("trace-target")) })
			beforeSnap := takeSnapshot(t, s)
			captureTrace(s, "req-new", "trace-target", `{"new":true}`)

			if flushErr := s.Flush(context.Background()); flushErr == nil {
				t.Fatalf("%s: expected nonnil Flush error", tc.name)
			}
			if s.Status().WriteErrors <= beforeWriteErrors || s.Status().DroppedBodies < 1 {
				t.Fatalf("%s: writeErrors or droppedBodies not incremented", tc.name)
			}
			if afterSnap := takeSnapshot(t, s); !afterSnap.equalData(beforeSnap) {
				t.Fatalf("%s: state not atomically preserved: %+v != %+v", tc.name, afterSnap, beforeSnap)
			}

			liveBoltUpdate(t, s, func(tx *bolt.Tx) error {
				return tx.Bucket(bucketTraceBodyRefs).Put([]byte("trace-target"), encodeTraceBodyRefs(1))
			})
			if closeErr := s.Close(); closeErr == nil {
				t.Fatalf("%s: expected Close finalErr", tc.name)
			}
		})
	}
}

// A real compaction preserves references, ambiguity and the request sequence.
func TestTraceRefs_AutoCompactionAndReopen(t *testing.T) {
	clk := &clock{t: time.Now()}
	cfg := compactConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.setNow(clk.now)
	inflateStore(t, s, clk)

	clk.advance(2 * time.Minute)
	captureBody(s, "keep", `{"text":"retained"}`)
	captureTrace(s, "amb-1", "live-amb", `{"text":"a"}`)
	captureTrace(s, "amb-2", "live-amb", `{"text":"b"}`)
	s.SubmitUsage(usageRecord("keep", "p", "m", clk.now(), simpleUsage(1, 1)))
	clk.advance(time.Second)
	flushAll(t, s)

	if s.Status().Compactions < 1 {
		t.Fatalf("compactions = %d, want >= 1", s.Status().Compactions)
	}
	if snap := takeSnapshot(t, s); snap.refs["trace-keep"] != 1 || snap.refs["live-amb"] != 2 || len(snap.rawRefs) != 2 {
		t.Fatalf("compaction did not preserve live refs: %+v", snap.refs)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.setNow(clk.now)

	if snap := takeSnapshot(t, s2); snap.bodies["keep"] != `{"text":"retained"}` || snap.refs["trace-keep"] != 1 ||
		snap.refs["live-amb"] != 2 || !bytes.Equal([]byte(snap.trace["live-amb"]), ambiguousTrace) {
		t.Fatalf("reopened live index changed: %+v", snap)
	}

	s2.SubmitUsage(usageRecord("next", "p", "m", clk.now(), simpleUsage(1, 1)))
	clk.advance(time.Second)
	flushAll(t, s2)

	reqs := allRequests(t, s2)
	if len(reqs) != 2 || reqs[0].RequestID != "next" || reqs[0].Sequence != 26 || reqs[1].RequestID != "keep" || reqs[1].Sequence != 25 {
		t.Fatalf("requests unexpected: %+v", reqs)
	}
}
