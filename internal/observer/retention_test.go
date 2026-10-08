package observer

import (
	"errors"
	"testing"
	"time"
)

// TestRetentionWindowsAreIndependent verifies request metadata, minute
// aggregates and bodies expire on separate schedules.
func TestRetentionWindowsAreIndependent(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.RequestRetention = time.Minute
		c.BodyRetention = time.Minute
		c.StatsRetentionDays = 1
	})
	s.setNow(clk.now)

	at := clk.now()
	s.SubmitUsage(usageRecord("ret-1", "openai", "gpt-5", at, simpleUsage(100, 50)))
	captureBody(s, "ret-1", `{"a":1}`)
	flushAll(t, s)

	if got := bucketCount(t, s, bucketRequests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}

	// Past the 1m request/body windows but well inside the 1 day stats window.
	clk.advance(2 * time.Minute)
	if err := s.runCleanup(); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	if got := bucketCount(t, s, bucketRequests); got != 0 {
		t.Errorf("request metadata not expired: %d", got)
	}
	if got := bucketCount(t, s, bucketBodyIndex); got != 0 {
		t.Errorf("body index not expired: %d", got)
	}
	if got := bucketCount(t, s, bucketStats); got == 0 {
		t.Errorf("minute stats should survive request/body expiry")
	}
	if _, err := s.Body("ret-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired body = %v", err)
	}

	// Past the stats window too.
	clk.advance(48 * time.Hour)
	if err := s.runCleanup(); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	if got := bucketCount(t, s, bucketStats); got != 0 {
		t.Errorf("stats not expired after %d days: %d", 2, got)
	}
}

// TestStatsExpireWhileRequestsSurvive covers the opposite independence: short
// stats retention with long request retention.
func TestStatsExpireWhileRequestsSurvive(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) {
		c.RequestRetention = 30 * 24 * time.Hour
		c.StatsRetentionDays = 1
	})
	s.setNow(clk.now)

	at := clk.now()
	s.SubmitUsage(usageRecord("ind-1", "openai", "gpt-5", at, simpleUsage(100, 50)))
	flushAll(t, s)

	clk.advance(2 * 24 * time.Hour)
	if err := s.runCleanup(); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	if got := bucketCount(t, s, bucketStats); got != 0 {
		t.Errorf("stats should have expired: %d", got)
	}
	page, err := s.Requests(Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("request metadata should survive stats expiry: %d", len(page.Items))
	}
	summary, err := s.Summary(Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 0 {
		t.Errorf("summary should be empty after stats expiry: %d", summary.Totals.Requests)
	}
}
