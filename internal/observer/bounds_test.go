package observer

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TestRequestsOffsetBounds verifies rejection of negative and overflow offset values,
// as well as safe handling of the maximum supported offset boundary math.MaxInt32.
func TestRequestsOffsetBounds(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 5, func(int) string { return "openai" })

	for _, badOffset := range []int{-1, -100, math.MinInt32} {
		if _, err := s.Requests(Query{Offset: badOffset}); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("Offset %d: err = %v, want ErrInvalidQuery", badOffset, err)
		}
	}

	// Offset at math.MaxInt32 is valid and beyond the end of data.
	page, err := s.Requests(Query{Offset: math.MaxInt32})
	if err != nil {
		t.Fatalf("Requests(Offset=MaxInt32): %v", err)
	}
	if len(page.Items) != 0 || page.HasMore || page.Offset != math.MaxInt32 {
		t.Errorf("expected empty page at MaxInt32: %+v", page)
	}
}

// TestRequestsOffsetBeyondEnd verifies that offsets at or beyond total matching records
// return an empty page with has_more=false without errors.
func TestRequestsOffsetBeyondEnd(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 5, func(int) string { return "openai" })

	page, err := s.Requests(Query{Offset: 10, Limit: 5})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 0 || page.HasMore || page.Offset != 10 {
		t.Errorf("expected empty page beyond end: %+v", page)
	}

	page, err = s.Requests(Query{Offset: 5, Limit: 5})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 0 || page.HasMore || page.Offset != 5 {
		t.Errorf("expected empty page exactly at end: %+v", page)
	}
}

func TestRequestsLimitClamping(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 3, func(int) string { return "openai" })
	page, err := s.Requests(Query{Limit: 0})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 3 {
		t.Errorf("default limit returned %d items", len(page.Items))
	}
	page, err = s.Requests(Query{Limit: maxPageLimit + 500})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 3 {
		t.Errorf("clamped limit returned %d items", len(page.Items))
	}
}

func TestRequestsRangeBeforeRetentionIsEmpty(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	s := openTestStore(t, func(c *Config) { c.RequestRetention = time.Hour })
	s.setNow(clk.now)
	at := clk.now()
	s.SubmitUsage(usageRecord("old-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
	flushAll(t, s)

	clk.advance(2 * time.Hour)
	page, err := s.Requests(Query{From: at.Add(-time.Minute), To: at.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("records outside the retention window leaked: %d", len(page.Items))
	}
}

func TestSummaryRejectsInvertedRange(t *testing.T) {
	s := openTestStore(t, nil)
	now := time.Now()
	for _, q := range []Query{
		{From: now, To: now},
		{From: now, To: now.Add(-time.Hour)},
	} {
		if _, err := s.Summary(q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("Summary(%+v) err = %v, want ErrInvalidQuery", q, err)
		}
	}
}

func TestSummaryClampedToStatsRetention(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, func(c *Config) { c.StatsRetentionDays = 1 })
	s.setNow(clk.now)
	at := clk.now()
	s.SubmitUsage(usageRecord("stat-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
	flushAll(t, s)

	clk.advance(48 * time.Hour)
	summary, err := s.Summary(Query{From: at.Add(-time.Minute), To: at.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 0 {
		t.Errorf("summary outside stats retention leaked: %d", summary.Totals.Requests)
	}
}
