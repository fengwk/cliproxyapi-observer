package observer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

// TestBodyCorrelationViaTrace covers the v8.0.15/v8.0.19 reality where the
// interception lifecycle RequestID and the usage execution RequestID differ and
// only the shared TraceID links them: a body captured under the interception ID
// must resolve from the usage RequestID, and vice versa in either arrival order.
func TestBodyCorrelationViaTrace(t *testing.T) {
	t.Run("usage-then-body", func(t *testing.T) {
		s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
		at := time.Now().Add(-time.Minute)
		if !s.SubmitUsage(usageTrace("usage-1", "trace-xyz", "openai", "gpt-5", at, simpleUsage(10, 5))) {
			t.Fatalf("submit rejected")
		}
		flushAll(t, s)
		if !captureTrace(s, "before-1", "trace-xyz", `{"prompt":"hello"}`) {
			t.Fatalf("capture rejected")
		}
		flushAll(t, s)

		detail, err := s.Body("usage-1")
		if err != nil {
			t.Fatalf("Body(usage-1): %v", err)
		}
		if detail.Content != `{"prompt":"hello"}` {
			t.Errorf("content = %q", detail.Content)
		}
		if detail.TraceID != "trace-xyz" {
			t.Errorf("trace = %q", detail.TraceID)
		}
		if _, err := s.Body("before-1"); err != nil {
			t.Errorf("direct interception id lookup failed: %v", err)
		}
		assertBodyAvailable(t, s, "usage-1", true)
	})

	t.Run("body-then-usage", func(t *testing.T) {
		s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
		if !captureTrace(s, "before-2", "trace-abc", `{"prompt":"world"}`) {
			t.Fatalf("capture rejected")
		}
		flushAll(t, s)
		at := time.Now().Add(-time.Minute)
		s.SubmitUsage(usageTrace("usage-2", "trace-abc", "openai", "gpt-5", at, simpleUsage(10, 5)))
		flushAll(t, s)

		detail, err := s.Body("usage-2")
		if err != nil {
			t.Fatalf("Body(usage-2): %v", err)
		}
		if detail.Content != `{"prompt":"world"}` {
			t.Errorf("content = %q", detail.Content)
		}
		assertBodyAvailable(t, s, "usage-2", true)
	})
}

// TestRetriedUsagesShareInboundBody verifies several execution records on one
// trace resolve to the single captured inbound body.
func TestRetriedUsagesShareInboundBody(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	at := time.Now().Add(-time.Minute)
	captureTrace(s, "before-retry", "trace-retry", `{"prompt":"retry-body"}`)
	for i := 0; i < 3; i++ {
		s.SubmitUsage(usageTrace(fmt.Sprintf("attempt-%d", i), "trace-retry", "openai", "gpt-5", at, simpleUsage(1, 1)))
	}
	flushAll(t, s)

	for i := 0; i < 3; i++ {
		detail, err := s.Body(fmt.Sprintf("attempt-%d", i))
		if err != nil {
			t.Fatalf("Body(attempt-%d): %v", i, err)
		}
		if detail.Content != `{"prompt":"retry-body"}` {
			t.Errorf("attempt %d content = %q", i, detail.Content)
		}
		assertBodyAvailable(t, s, fmt.Sprintf("attempt-%d", i), true)
	}
}

// TestAmbiguousTraceFailsClosed verifies two distinct captures on one trace do
// not let trace-level lookup arbitrarily pick a body, while an exact
// interception RequestID remains directly readable.
func TestAmbiguousTraceFailsClosed(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	at := time.Now().Add(-time.Minute)
	captureTrace(s, "before-a", "trace-ambiguous", `{"which":"a"}`)
	captureTrace(s, "before-b", "trace-ambiguous", `{"which":"b"}`)
	s.SubmitUsage(usageTrace("usage-amb", "trace-ambiguous", "openai", "gpt-5", at, simpleUsage(1, 1)))
	flushAll(t, s)

	if _, err := s.Body("usage-amb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ambiguous trace resolved to a body: %v", err)
	}
	assertBodyAvailable(t, s, "usage-amb", false)
	// Exact ids are not a trace-level gamble and stay readable.
	if _, err := s.Body("before-a"); err != nil {
		t.Errorf("direct before-a lookup failed: %v", err)
	}
	if _, err := s.Body("before-b"); err != nil {
		t.Errorf("direct before-b lookup failed: %v", err)
	}
}

// TestBodyDisabledServesNothingEvenFromOldDatabase reproduces the reported bug:
// reopening an existing database with capture disabled must not leak bodies.
func TestBodyDisabledServesNothingEvenFromOldDatabase(t *testing.T) {
	cfg := testConfig(t, func(c *Config) { c.CaptureBodies = true })
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	at := time.Now().Add(-time.Minute)
	s.SubmitUsage(usageTrace("usage-old", "trace-old", "openai", "gpt-5", at, simpleUsage(1, 1)))
	captureTrace(s, "before-old", "trace-old", `{"secret-free":"ok"}`)
	flushAll(t, s)
	if _, err := s.Body("before-old"); err != nil {
		t.Fatalf("body should be readable while enabled: %v", err)
	}
	if _, err := s.Body("usage-old"); err != nil {
		t.Fatalf("correlated body should be readable while enabled: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same file with capture disabled.
	cfg.CaptureBodies = false
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Body("before-old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled store served a retained body: %v", err)
	}
	if _, err := reopened.Body("usage-old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled store served a trace-resolved body: %v", err)
	}
	assertBodyAvailable(t, reopened, "usage-old", false)
}

// TestExpiredBodyNotReadableDespiteIndex verifies strict read expiry applies to
// trace-resolved bodies even before the sweeper removes the indexes.
func TestExpiredBodyNotReadableDespiteIndex(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.BodyRetention = 30 * time.Minute
		c.RequestRetention = 24 * time.Hour
	})
	s.setNow(clk.now)

	at := clk.now()
	s.SubmitUsage(usageTrace("usage-exp", "trace-exp", "openai", "gpt-5", at, simpleUsage(1, 1)))
	captureTrace(s, "before-exp", "trace-exp", `{"a":1}`)
	flushAll(t, s)
	if _, err := s.Body("usage-exp"); err != nil {
		t.Fatalf("fresh correlated body unreadable: %v", err)
	}

	clk.advance(31 * time.Minute)
	if _, err := s.Body("usage-exp"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired correlated body read = %v", err)
	}
	assertBodyAvailable(t, s, "usage-exp", false)
}

// TestRetentionCleansCorrelationIndexes verifies TTL removes the usage->trace
// and trace->body index entries alongside the bodies they reference.
func TestRetentionCleansCorrelationIndexes(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.BodyRetention = 30 * time.Minute
		c.RequestRetention = 30 * time.Minute
		c.StatsRetentionDays = 1
	})
	s.setNow(clk.now)

	at := clk.now()
	s.SubmitUsage(usageTrace("usage-idx", "trace-idx", "openai", "gpt-5", at, simpleUsage(1, 1)))
	captureTrace(s, "before-idx", "trace-idx", `{"a":1}`)
	flushAll(t, s)
	if got := bucketCount(t, s, bucketUsageTrace); got != 1 {
		t.Fatalf("usage_trace entries = %d, want 1", got)
	}
	if got := bucketCount(t, s, bucketTraceBody); got != 1 {
		t.Fatalf("trace_body entries = %d, want 1", got)
	}

	clk.advance(31 * time.Minute)
	if err := s.runCleanup(); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	for _, b := range [][]byte{bucketBodies, bucketBodyIndex, bucketTraceBody, bucketUsageTrace} {
		if got := bucketCount(t, s, b); got != 0 {
			t.Errorf("bucket %s still holds %d entries", b, got)
		}
	}
}

// TestBodyEvictionRemovesTraceIndex verifies the byte cap eviction also drops
// the evicted body's trace association.
func TestBodyEvictionRemovesTraceIndex(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 200
		c.MaxBodyStorageBytes = 260
	})
	body := `{"p":"` + strings.Repeat("x", 92) + `"}`
	if !s.Capture(pluginapi.RequestInterceptRequest{RequestID: "ev-a", TraceID: "trace-ev-a", Body: []byte(body)}) {
		t.Fatalf("capture a rejected")
	}
	flushAll(t, s)
	time.Sleep(2 * time.Millisecond)
	if !s.Capture(pluginapi.RequestInterceptRequest{RequestID: "ev-b", TraceID: "trace-ev-b", Body: []byte(body)}) {
		t.Fatalf("capture b rejected")
	}
	flushAll(t, s)
	time.Sleep(2 * time.Millisecond)
	if !s.Capture(pluginapi.RequestInterceptRequest{RequestID: "ev-c", TraceID: "trace-ev-c", Body: []byte(body)}) {
		t.Fatalf("capture c rejected")
	}
	flushAll(t, s)

	if _, err := s.Body("ev-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("evicted body still readable: %v", err)
	}
	if _, ok := lookupTraceBodyDirect(t, s, "trace-ev-a"); ok {
		t.Errorf("evicted body trace index not cleaned")
	}
	if _, err := s.Body("ev-c"); err != nil {
		t.Errorf("newest body should survive: %v", err)
	}
}

func assertBodyAvailable(t *testing.T, s *Store, requestID string, want bool) {
	t.Helper()
	for _, item := range allRequests(t, s) {
		if item.RequestID == requestID {
			if item.BodyAvailable != want {
				t.Errorf("BodyAvailable(%s) = %v, want %v", requestID, item.BodyAvailable, want)
			}
			return
		}
	}
	t.Errorf("request %s not found in listing", requestID)
}

func lookupTraceBodyDirect(t *testing.T, s *Store, traceID string) (string, bool) {
	t.Helper()
	var id string
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		id, ok = lookupTraceBody(tx, traceID)
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	return id, ok
}
