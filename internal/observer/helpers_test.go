package observer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

// testConfig returns a valid configuration rooted in a per-test temp dir.
func testConfig(t *testing.T, mutate func(*Config)) Config {
	t.Helper()
	cfg := Config{
		DataPath:            filepath.Join(t.TempDir(), "observer.db"),
		StatsRetentionDays:  365,
		RequestRetention:    24 * time.Hour,
		BodyRetention:       24 * time.Hour,
		MaxBodyBytes:        1 << 20,
		MaxBodyStorageBytes: 8 << 20,
		FlushInterval:       10 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func openTestStore(t *testing.T, mutate func(*Config)) *Store {
	t.Helper()
	s, err := Open(testConfig(t, mutate))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// clock is a race-safe fake clock for retention/expiry tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func flushAll(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func usageRecord(id, provider, model string, at time.Time, detail pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID:   id,
		TraceID:     "trace-" + id,
		Provider:    provider,
		Model:       model,
		RequestedAt: at,
		Latency:     2 * time.Second,
		TTFT:        time.Second,
		Detail:      detail,
	}
}

// simpleUsage is an openai-style "subset" record: complete accounting.
func simpleUsage(input, output int64) pluginapi.UsageDetail {
	return pluginapi.UsageDetail{InputTokens: input, OutputTokens: output}
}

// bucketCount counts raw records in a bucket so retention tests can observe
// physical deletion independently of read-time clamps.
func bucketCount(t *testing.T, s *Store, name []byte) int {
	t.Helper()
	count := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(name)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, _ []byte) error {
			count++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("bucket %s count: %v", name, err)
	}
	return count
}

// usageTrace builds a usage record with an explicit execution RequestID and the
// shared inbound TraceID used to correlate a captured body.
func usageTrace(id, trace, provider, model string, at time.Time, detail pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID:   id,
		TraceID:     trace,
		Provider:    provider,
		Model:       model,
		RequestedAt: at,
		Latency:     2 * time.Second,
		TTFT:        time.Second,
		Detail:      detail,
	}
}

// allRequests pages through every request the store currently retains.
func allRequests(t *testing.T, s *Store) []Request {
	t.Helper()
	var out []Request
	query := Query{Limit: maxPageLimit}
	for {
		page, err := s.Requests(query)
		if err != nil {
			t.Fatalf("Requests: %v", err)
		}
		out = append(out, page.Items...)
		if !page.HasMore {
			break
		}
		query.Cursor = page.NextCursor
	}
	return out
}

// captureTrace captures a body with an explicit TraceID, mirroring the host's
// distinct interception RequestID sharing one inbound TraceID.
func captureTrace(s *Store, requestID, traceID, body string) bool {
	return s.Capture(pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		TraceID:   traceID,
		Body:      []byte(body),
	})
}
