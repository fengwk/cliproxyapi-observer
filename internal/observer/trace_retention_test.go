package observer

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A formerly ambiguous trace must not leak after the final captured body
// expires. Partial expiry must not promote the surviving body to unique.
func TestAmbiguousTraceFinalExpiryRemovesIndex(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.BodyRetention = 10 * time.Minute
	})
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s.setNow(clk.now)
	captureTrace(s, "first", "shared", `{"body":1}`)
	clk.advance(5 * time.Minute)
	captureTrace(s, "second", "shared", `{"body":2}`)
	s.SubmitUsage(usageTrace("usage", "shared", "p", "m", clk.now(), simpleUsage(1, 1)))
	clk.advance(time.Second)
	flushAll(t, s)
	if snap := takeSnapshot(t, s); snap.refs["shared"] != 2 {
		t.Fatalf("initial shared refs: %+v", snap.refs)
	}
	clk.advance(6 * time.Minute)
	if err := s.runCleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Body("second"); err != nil {
		t.Fatal("surviving direct body unavailable", err)
	}
	if _, err := s.Body("usage"); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial expiry promoted ambiguous trace", err)
	}
	if snap := takeSnapshot(t, s); snap.refs["shared"] != 1 {
		t.Fatalf("partial expiry refs: %+v", snap.refs)
	}
	if err := s.view(func(tx *bolt.Tx) error {
		if !bytes.Equal(tx.Bucket(bucketTraceBody).Get([]byte("shared")), ambiguousTrace) {
			t.Error("ambiguity not retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Startup must rebuild a count of one without promoting old ambiguity.
	cfg := s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	s.setNow(clk.now)
	snap := takeSnapshot(t, s)
	if snap.refs["shared"] != 1 || !bytes.Equal([]byte(snap.trace["shared"]), ambiguousTrace) {
		t.Fatalf("reopen promoted partial ambiguity: %+v", snap)
	}
	if _, err := s.Body("usage"); !errors.Is(err, ErrNotFound) {
		t.Fatal("reopen resolved ambiguous trace", err)
	}
	clk.advance(5 * time.Minute)
	if err := s.runCleanup(); err != nil {
		t.Fatal(err)
	}
	if got := bucketCount(t, s, bucketBodyMeta); got != 0 {
		t.Fatalf("metadata not expired: %d", got)
	}
	if got := bucketCount(t, s, bucketTraceBody); got != 0 {
		t.Fatalf("ambiguous trace index leaked after final body expiry: %d", got)
	}
	if got := bucketCount(t, s, bucketTraceBodyRefs); got != 0 {
		t.Fatalf("trace references leaked after final body expiry: %d", got)
	}
}

// Byte-cap eviction must retire the marker when the last old shared body is
// evicted, not just when an unambiguous trace loses its single body.
func TestAmbiguousTraceFinalEvictionRemovesIndex(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 200
		c.MaxBodyStorageBytes = 200
	})
	clk := &clock{t: time.Now()}
	s.setNow(clk.now)
	body := `{"p":"` + strings.Repeat("x", 92) + `"}`
	// Keep two 100-byte bodies under the cap; the next two evict them in order.
	for _, id := range []string{"a", "b", "c", "d"} {
		trace := "shared"
		if id == "c" || id == "d" {
			trace = id
		}
		if !captureTrace(s, id, trace, body) {
			t.Fatal("capture rejected")
		}
		flushAll(t, s)
		if id == "b" {
			if snap := takeSnapshot(t, s); snap.refs["shared"] != 2 {
				t.Fatalf("initial eviction refs: %+v", snap.refs)
			}
		}
		if id == "c" {
			if _, err := s.Body("b"); err != nil {
				t.Fatal("partial eviction lost surviving body", err)
			}
			if _, ok := lookupTraceBodyDirect(t, s, "shared"); ok {
				t.Fatal("partial eviction promoted ambiguity")
			}
			snap := takeSnapshot(t, s)
			if snap.refs["shared"] != 1 || !bytes.Equal([]byte(snap.trace["shared"]), ambiguousTrace) {
				t.Fatalf("partial eviction lost count or sentinel: %+v", snap)
			}
		}
		clk.advance(time.Second)
	}
	if err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketTraceBody).Get([]byte("shared")) != nil {
			t.Error("ambiguous trace index leaked after final eviction")
		}
		if tx.Bucket(bucketTraceBodyRefs).Get([]byte("shared")) != nil {
			t.Error("trace references leaked after final eviction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
