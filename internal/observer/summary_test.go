package observer

import (
	"testing"
	"time"
)

func TestSummaryGroupsAndSeries(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	for m := 0; m < 10; m++ {
		at := from.Add(time.Duration(m) * time.Minute)
		s.SubmitUsage(usageRecord("o-"+string(rune('a'+m)), "openai", "gpt-5", at, simpleUsage(100, 50)))
		s.SubmitUsage(usageRecord("a-"+string(rune('a'+m)), "anthropic", "claude-4", at, simpleUsage(200, 100)))
	}
	flushAll(t, s)

	to := from.Add(10 * time.Minute)
	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 20 {
		t.Fatalf("total requests = %d, want 20", summary.Totals.Requests)
	}
	if summary.Totals.InputTokens != 10*100+10*200 {
		t.Errorf("input tokens = %d", summary.Totals.InputTokens)
	}
	if len(summary.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(summary.Groups))
	}
	if summary.Groups[0].Provider != "anthropic" || summary.Groups[1].Provider != "openai" {
		t.Errorf("groups not deterministically sorted: %+v", summary.Groups)
	}
	if len(summary.Series) != 10 {
		t.Fatalf("series points = %d, want 10", len(summary.Series))
	}
	for _, p := range summary.Series {
		if p.Requests != 2 {
			t.Fatalf("series point requests = %d, want 2", p.Requests)
		}
	}

	filtered, err := s.Summary(Query{From: from, To: to, Provider: "openai"})
	if err != nil {
		t.Fatalf("Summary filter: %v", err)
	}
	if filtered.Totals.Requests != 10 {
		t.Errorf("filtered total = %d, want 10", filtered.Totals.Requests)
	}
	if len(filtered.Groups) != 1 || filtered.Groups[0].Provider != "openai" {
		t.Errorf("filtered groups = %+v", filtered.Groups)
	}
}

func TestSummarySeriesDownsamplesToMaxPoints(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	const minutes = 1000
	for m := 0; m < minutes; m++ {
		at := from.Add(time.Duration(m) * time.Minute)
		s.SubmitUsage(usageRecord("d-"+time.Duration(m).String(), "openai", "gpt-5", at, simpleUsage(1, 1)))
	}
	flushAll(t, s)

	to := from.Add(minutes * time.Minute)
	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(summary.Series) == 0 || len(summary.Series) > maxSeriesPoints {
		t.Fatalf("series points = %d, want 1..%d", len(summary.Series), maxSeriesPoints)
	}
	var summed uint64
	for _, p := range summary.Series {
		if p.Time.Before(from) || !p.Time.Before(to) {
			t.Errorf("series point %v outside [%v, %v)", p.Time, from, to)
		}
		summed += p.Requests
	}
	if summed != minutes {
		t.Errorf("downsampled request sum = %d, want %d", summed, minutes)
	}
}

// TestSummarySeriesStaysWithinRangeFor301Minutes guards the ceil(width) edge:
// 301 minutes must not emit a trailing point past the query range.
func TestSummarySeriesStaysWithinRangeFor301Minutes(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s := openTestStore(t, nil)
	s.setNow(clk.now)

	from := clk.now()
	const minutes = 301
	for m := 0; m < minutes; m++ {
		at := from.Add(time.Duration(m) * time.Minute)
		s.SubmitUsage(usageRecord("e-"+time.Duration(m).String(), "openai", "gpt-5", at, simpleUsage(1, 1)))
	}
	flushAll(t, s)

	to := from.Add(minutes * time.Minute)
	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(summary.Series) > maxSeriesPoints {
		t.Fatalf("series points = %d, want <= %d", len(summary.Series), maxSeriesPoints)
	}
	var summed uint64
	for _, p := range summary.Series {
		if p.Time.Before(from) || !p.Time.Before(to) {
			t.Fatalf("series point %v outside [%v, %v)", p.Time, from, to)
		}
		summed += p.Requests
	}
	if summed != minutes {
		t.Errorf("downsampled request sum = %d, want %d", summed, minutes)
	}
}

func TestSummaryEmptyRangeIsZeroed(t *testing.T) {
	s := openTestStore(t, nil)
	from := time.Now().Add(-time.Hour)
	summary, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 0 || len(summary.Groups) != 0 {
		t.Errorf("expected empty summary, got %+v", summary)
	}
}
