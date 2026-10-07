package observer

import (
	"encoding/base64"
	"errors"
	"math"
	"testing"
	"time"
)

func TestDecodeCursorRejectsMalformed(t *testing.T) {
	cases := []string{
		"not-base64!!",
		base64.RawURLEncoding.EncodeToString([]byte("short")),
		base64.RawURLEncoding.EncodeToString(make([]byte, 24)),
		"",
	}
	for _, cursor := range cases {
		if cursor == "" {
			continue
		}
		if _, _, err := decodeCursor(cursor); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("decodeCursor(%q) err = %v, want ErrInvalidCursor", cursor, err)
		}
	}
}

func TestCursorRoundTripIncludingEpoch(t *testing.T) {
	for _, at := range []time.Time{time.Unix(0, 0).UTC(), time.Unix(1_700_000_000, 123456789).UTC()} {
		got, seq, err := decodeCursor(encodeCursor(at, 42))
		if err != nil {
			t.Fatalf("decodeCursor: %v", err)
		}
		if got.UnixNano() != at.UnixNano() || seq != 42 {
			t.Errorf("round trip = %v/%d, want %v/42", got, seq, at)
		}
	}
}

func TestRequestsCursorWithExtremeTimestampIsSafe(t *testing.T) {
	s := openTestStore(t, nil)
	seedRequests(t, s, 5, func(int) string { return "openai" })
	future := encodeCursor(time.Unix(0, math.MaxInt64).UTC(), 0)
	page, err := s.Requests(Query{Cursor: future})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 5 {
		t.Errorf("far-future cursor should return every older record, got %d", len(page.Items))
	}
	// A cursor at the epoch is older than everything, so nothing older remains.
	epoch := encodeCursor(time.Unix(0, 0).UTC(), 0)
	page, err = s.Requests(Query{Cursor: epoch})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 0 || page.HasMore {
		t.Errorf("epoch cursor should yield an empty page: %+v", page)
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
