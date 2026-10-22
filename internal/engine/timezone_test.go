package engine

import (
	"errors"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func localDate(value string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		panic(err)
	}
	return t
}

// Inclusive local calendar dates must select the exact UTC instants of local
// midnight boundaries, including the UTC date changing inside the local day.
func TestRequestViewLocalDayBoundariesAndUTCInstants(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	loc, err := time.LoadLocation("Asia/Kathmandu")
	must(t, err)
	from := localDate("2024-01-30", loc).UTC()
	to := localDate("2024-01-31", loc).UTC()
	if from.Format(time.RFC3339) != "2024-01-29T18:15:00Z" || to.Format(time.RFC3339) != "2024-01-30T18:15:00Z" {
		t.Fatalf("fixture boundary drift: %v %v", from, to)
	}
	for id, at := range map[string]time.Time{
		"before": from.Add(-time.Minute),
		"first":  from,
		"inside": time.Date(2024, 1, 30, 6, 0, 0, 0, time.UTC),
		"last":   to.Add(-time.Minute),
		"after":  to,
		"next":   to.Add(time.Minute),
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}

	view, err := s.RequestView(ViewQuery{FromDate: "2024-01-30", ToDate: "2024-01-30", Timezone: "Asia/Kathmandu", View: viewProvider})
	must(t, err)
	if view.Summary.Requests != 3 || view.Summary.CostMicroUSD != 21 || view.Summary.Failures != 0 {
		t.Fatalf("local day summary = %+v", view.Summary)
	}
	if !view.Range.From.Equal(from) || !view.Range.To.Equal(to) || view.Range.Timezone != "Asia/Kathmandu" {
		t.Fatalf("local day range = %+v", view.Range)
	}
	if view.Series.Granularity != "day" || len(view.Series.Buckets) != 1 ||
		!view.Series.Buckets[0].Time.Equal(from) || view.Series.Buckets[0].Requests != 3 {
		t.Fatalf("local day series = %+v", view.Series)
	}

	// Omitted bounds mean the local today, not the UTC today.
	defaults, err := s.RequestView(ViewQuery{Timezone: "Asia/Kathmandu", View: viewProvider})
	must(t, err)
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc).UTC()
	if !defaults.Range.From.Equal(today) || !defaults.Range.To.Equal(*now) || defaults.Range.Timezone != "Asia/Kathmandu" {
		t.Fatalf("local today range = %+v", defaults.Range)
	}
}

// Complete minutes use aggregates while partial edge minutes use exact raw
// timestamps. Records after q.To in the shared minute must never be counted.
func TestRequestViewLocalPartialMinutesExact(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	*now = time.Date(2024, 1, 31, 12, 1, 0, 0, time.UTC)
	for id, at := range map[string]time.Time{
		"same-before": time.Date(2024, 1, 31, 10, 0, 15, 0, time.UTC),
		"same-first":  time.Date(2024, 1, 31, 10, 0, 20, 0, time.UTC),
		"same-inside": time.Date(2024, 1, 31, 10, 0, 40, 0, time.UTC),
		"same-last":   time.Date(2024, 1, 31, 10, 0, 50, 0, time.UTC),
		"same-after":  time.Date(2024, 1, 31, 10, 0, 55, 0, time.UTC),

		"cross-before": time.Date(2024, 1, 31, 11, 0, 29, 0, time.UTC),
		"cross-first":  time.Date(2024, 1, 31, 11, 0, 30, 0, time.UTC),
		"cross-edge-1": time.Date(2024, 1, 31, 11, 0, 59, 0, time.UTC),
		"cross-full-1": time.Date(2024, 1, 31, 11, 1, 0, 0, time.UTC),
		"cross-full-2": time.Date(2024, 1, 31, 11, 1, 59, 0, time.UTC),
		"cross-last-1": time.Date(2024, 1, 31, 11, 2, 0, 0, time.UTC),
		"cross-last-2": time.Date(2024, 1, 31, 11, 2, 14, 0, time.UTC),
		"cross-after":  time.Date(2024, 1, 31, 11, 2, 15, 0, time.UTC),

		"future-inside": time.Date(2024, 1, 31, 12, 0, 35, 0, time.UTC),
		"future-after":  time.Date(2024, 1, 31, 12, 0, 45, 0, time.UTC),
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}
	count := func(from, to time.Time) int64 {
		t.Helper()
		view, err := s.RequestView(ViewQuery{Query: Query{From: from, To: to}, Timezone: "Europe/Berlin", View: viewProvider})
		must(t, err)
		return view.Summary.Requests
	}

	// Both boundaries fall inside one minute, so only the raw edge is valid.
	if got := count(time.Date(2024, 1, 31, 10, 0, 20, 0, time.UTC), time.Date(2024, 1, 31, 10, 0, 50, 0, time.UTC)); got != 2 {
		t.Fatalf("same-minute range counted %d, want 2", got)
	}
	if got := count(time.Date(2024, 1, 31, 11, 0, 30, 0, time.UTC), time.Date(2024, 1, 31, 11, 2, 15, 0, time.UTC)); got != 6 {
		t.Fatalf("cross-minute range counted %d, want 6", got)
	}
	// A record already committed at 12:00:45 is later than q.To=12:00:40 even
	// though it shares the 12:00 minute bucket.
	if got := count(time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC), time.Date(2024, 1, 31, 12, 0, 40, 0, time.UTC)); got != 1 {
		t.Fatalf("range with future record counted %d, want 1", got)
	}

	// Non-minute boundaries cannot be reconstructed once raw rows expire.
	*now = now.Add(3 * 24 * time.Hour)
	must(t, s.Maintenance())
	if _, err := s.RequestView(ViewQuery{Query: Query{From: time.Date(2024, 1, 31, 10, 0, 20, 0, time.UTC), To: time.Date(2024, 1, 31, 10, 0, 50, 0, time.UTC)}, Timezone: "Europe/Berlin", View: viewProvider}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired partial-minute range was not rejected: %v", err)
	}
}

// Historical local days must remain exact from minute aggregates after raw
// records and hourly rollups are pruned. Dimension and status filtering still
// operate on the same rows.
func TestHistoricalMinuteAggregatesSurviveRawCleanup(t *testing.T) {
	s, now, _ := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true, Label: "Alpha"})
	b, _ := key(t, s, KeySpec{Enabled: true, Label: "Beta"})
	loc, err := time.LoadLocation("Asia/Kathmandu")
	must(t, err)
	from := localDate("2024-01-30", loc).UTC()
	to := localDate("2024-01-31", loc).UTC()

	must(t, s.RecordUsage(event(a, "a-first", from)))
	failed := event(a, "a-failed", time.Date(2024, 1, 30, 6, 0, 0, 0, time.UTC))
	failed.Failed = true
	must(t, s.RecordUsage(failed))
	must(t, s.RecordUsage(event(b, "b-last", to.Add(-time.Minute))))
	must(t, s.RecordUsage(event(a, "a-after", to)))

	*now = now.Add(3 * 24 * time.Hour)
	must(t, s.Maintenance())
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("records")).Stats().KeyN != 0 || tx.Bucket([]byte("hourly")).Stats().KeyN != 0 {
			t.Fatal("raw or hourly retention did not expire")
		}
		if tx.Bucket([]byte("minutes")).Stats().KeyN == 0 {
			t.Fatal("minute aggregates followed raw retention")
		}
		return nil
	}))

	view, err := s.RequestView(ViewQuery{FromDate: "2024-01-30", ToDate: "2024-01-30", Timezone: "Asia/Kathmandu", View: viewKey})
	must(t, err)
	if view.Summary.Requests != 3 || view.Summary.Failures != 1 || view.Summary.CostMicroUSD != 21 {
		t.Fatalf("historical summary = %+v", view.Summary)
	}
	if len(view.Groups) != 2 {
		t.Fatalf("historical groups = %+v", view.Groups)
	}
	groups := map[string]Group{}
	for _, g := range view.Groups {
		groups[g.KeyID] = g
	}
	if groups[a.ID].Requests != 2 || groups[a.ID].Failures != 1 || groups[a.ID].Label != "Alpha" ||
		groups[b.ID].Requests != 1 || groups[b.ID].Failures != 0 || groups[b.ID].Label != "Beta" {
		t.Fatalf("historical group totals = %+v", groups)
	}

	filtered, err := s.RequestView(ViewQuery{FromDate: "2024-01-30", ToDate: "2024-01-30", Timezone: "Asia/Kathmandu", View: viewKey, Query: Query{Status: "failed"}})
	must(t, err)
	if filtered.Summary.Requests != 1 || filtered.Summary.Failures != 1 || len(filtered.Groups) != 1 ||
		filtered.Groups[0].KeyID != a.ID || filtered.Groups[0].Requests != 1 {
		t.Fatalf("historical status filter = %+v/%+v", filtered.Summary, filtered.Groups)
	}
}

// Local days are calendar-based across DST, while recent trends step elapsed
// hours so repeated fall-back hours stay distinct.
func TestRequestViewLocalDayDSTAndElapsedHours(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	loc, err := time.LoadLocation("Europe/Berlin")
	must(t, err)

	*now = time.Date(2025, 4, 2, 12, 0, 0, 0, time.UTC)
	springFrom := localDate("2025-03-30", loc).UTC()
	springTo := localDate("2025-03-31", loc).UTC()
	if springTo.Sub(springFrom) != 23*time.Hour {
		t.Fatalf("spring day length = %v", springTo.Sub(springFrom))
	}
	for id, at := range map[string]time.Time{
		"spring-before": springFrom.Add(-time.Minute),
		"spring-first":  springFrom,
		"spring-last":   springTo.Add(-time.Minute),
		"spring-after":  springTo,
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}
	spring, err := s.RequestView(ViewQuery{FromDate: "2025-03-30", ToDate: "2025-03-30", Timezone: "Europe/Berlin", View: viewProvider})
	must(t, err)
	if !spring.Range.From.Equal(springFrom) || !spring.Range.To.Equal(springTo) || spring.Range.To.Sub(spring.Range.From) != 23*time.Hour {
		t.Fatalf("spring range = %+v", spring.Range)
	}
	if spring.Summary.Requests != 2 || spring.Series.Granularity != "day" || len(spring.Series.Buckets) != 1 ||
		!spring.Series.Buckets[0].Time.Equal(springFrom) || spring.Series.Buckets[0].Requests != 2 {
		t.Fatalf("spring view = %+v/%+v", spring.Summary, spring.Series)
	}

	*now = time.Date(2025, 10, 27, 1, 0, 0, 0, time.UTC)
	settings, err := s.Settings()
	must(t, err)
	settings.RawRetentionHours = 48
	_, err = s.UpdateSettings(settings)
	must(t, err)
	fallFrom := localDate("2025-10-26", loc).UTC()
	fallTo := localDate("2025-10-27", loc).UTC()
	if fallTo.Sub(fallFrom) != 25*time.Hour {
		t.Fatalf("fall day length = %v", fallTo.Sub(fallFrom))
	}
	for id, at := range map[string]time.Time{
		"fall-before":   fallFrom.Add(-time.Minute),
		"fall-first":    fallFrom,
		"fall-repeat-1": fallFrom.Add(2 * time.Hour),
		"fall-repeat-2": fallFrom.Add(3 * time.Hour),
		"fall-last":     fallTo.Add(-time.Minute),
		"fall-after":    fallTo,
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}
	fall, err := s.RequestView(ViewQuery{FromDate: "2025-10-26", ToDate: "2025-10-26", Timezone: "Europe/Berlin", View: viewProvider})
	must(t, err)
	if !fall.Range.From.Equal(fallFrom) || !fall.Range.To.Equal(fallTo) || fall.Range.To.Sub(fall.Range.From) != 25*time.Hour {
		t.Fatalf("fall range = %+v", fall.Range)
	}
	if fall.Summary.Requests != 4 || fall.Series.Granularity != "hour" || len(fall.Series.Buckets) != 25 {
		t.Fatalf("fall view = %+v/%+v", fall.Summary, fall.Series)
	}
	if fall.Series.Buckets[2].Requests != 1 || fall.Series.Buckets[3].Requests != 1 {
		t.Fatalf("repeated hours were merged or empty: %+v", fall.Series.Buckets)
	}
	if !fall.Series.Buckets[2].Time.Equal(fallFrom.Add(2*time.Hour)) ||
		!fall.Series.Buckets[3].Time.Equal(fallFrom.Add(3*time.Hour)) ||
		fall.Series.Buckets[2].Time.Equal(fall.Series.Buckets[3].Time) {
		t.Fatalf("fall elapsed-hour instants = %v/%v", fall.Series.Buckets[2].Time, fall.Series.Buckets[3].Time)
	}
}

// A DST transition can skip local midnight while keeping the calendar date
// valid. The day starts at its first representable instant and still ends at the
// next date's own first instant, not at the carried 01:00 wall clock.
func TestRequestViewSkippedMidnightStartsAtFirstInstant(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	loc, err := time.LoadLocation("America/Santiago")
	must(t, err)
	first, err := parseLocalDate("2025-09-07", loc)
	must(t, err)
	end, err := localDayEnd(first, loc)
	must(t, err)
	wantFirst := time.Date(2025, 9, 7, 4, 0, 0, 0, time.UTC) // 01:00 -03
	wantEnd := time.Date(2025, 9, 8, 3, 0, 0, 0, time.UTC)   // next 00:00 -03
	if !first.Equal(wantFirst) || !end.Equal(wantEnd) || end.Sub(first) != 23*time.Hour {
		t.Fatalf("Santiago day = %v..%v (%v), want %v..%v", first, end, end.Sub(first), wantFirst, wantEnd)
	}

	saoLoc, err := time.LoadLocation("America/Sao_Paulo")
	must(t, err)
	saoFirst, err := parseLocalDate("2018-11-04", saoLoc)
	must(t, err)
	saoEnd, err := localDayEnd(saoFirst, saoLoc)
	must(t, err)
	if wantFrom, wantTo := time.Date(2018, 11, 4, 3, 0, 0, 0, time.UTC), time.Date(2018, 11, 5, 2, 0, 0, 0, time.UTC); !saoFirst.Equal(wantFrom) || !saoEnd.Equal(wantTo) || saoEnd.Sub(saoFirst) != 23*time.Hour {
		t.Fatalf("Sao Paulo day = %v..%v (%v), want %v..%v", saoFirst, saoEnd, saoEnd.Sub(saoFirst), wantFrom, wantTo)
	}

	*now = time.Date(2025, 9, 10, 12, 0, 0, 0, time.UTC)
	for id, at := range map[string]time.Time{
		"santiago-before": wantFirst.Add(-time.Minute),
		"santiago-first":  wantFirst,
		"santiago-last":   wantEnd.Add(-time.Minute),
		"santiago-after":  wantEnd,
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}
	view, err := s.RequestView(ViewQuery{FromDate: "2025-09-07", ToDate: "2025-09-07", Timezone: "America/Santiago", View: viewProvider})
	must(t, err)
	if !view.Range.From.Equal(wantFirst) || !view.Range.To.Equal(wantEnd) ||
		view.Range.To.Sub(view.Range.From) != 23*time.Hour {
		t.Fatalf("Santiago range = %+v", view.Range)
	}
	if view.Summary.Requests != 2 || len(view.Series.Buckets) != 1 ||
		!view.Series.Buckets[0].Time.Equal(wantFirst) || view.Series.Buckets[0].Requests != 2 {
		t.Fatalf("Santiago view = %+v/%+v", view.Summary, view.Series)
	}
}

// Invalid or magic zones and calendar dates that would normalize to a different
// local day must be rejected rather than silently reinterpreted.
func TestRequestViewTimezoneValidation(t *testing.T) {
	s, _, _ := fixture(t)
	for _, zone := range []string{"Local", "Mars/Olympus", " Asia/Tokyo"} {
		if _, err := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", Timezone: zone}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("zone %q accepted: %v", zone, err)
		}
	}
	// Pacific/Apia skipped 2011-12-30 entirely; parsing must not move it to the
	// next calendar day.
	if _, err := s.RequestView(ViewQuery{FromDate: "2011-12-30", ToDate: "2011-12-30", Timezone: "Pacific/Apia"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("skipped local date accepted: %v", err)
	}
	if _, err := s.RequestView(ViewQuery{FromDate: "2024-02-30", ToDate: "2024-02-30", Timezone: "UTC"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid calendar date accepted: %v", err)
	}
	if _, err := s.RequestView(ViewQuery{FromDate: "2024-02-02", ToDate: "2024-02-01", Timezone: "Asia/Kathmandu"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inverted local dates accepted: %v", err)
	}
}

// Minute aggregates follow the union of daily and raw retention: they outlive
// daily-only rollups while the raw window can still answer the same period, and
// only disappear once both windows have expired.
func TestMinuteAggregatesFollowRetentionUnion(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	settings, err := s.Settings()
	must(t, err)
	settings.RawRetentionHours = 168
	settings.DailyRetentionDays = 1
	_, err = s.UpdateSettings(settings)
	must(t, err)
	must(t, s.RecordUsage(event(k, "old", *now)))

	*now = now.Add(3 * 24 * time.Hour)
	must(t, s.Maintenance())
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("daily")).Stats().KeyN != 0 {
			t.Fatal("daily rollup did not expire")
		}
		if tx.Bucket([]byte("minutes")).Stats().KeyN == 0 || tx.Bucket([]byte("records")).Stats().KeyN != 1 {
			t.Fatal("minute aggregates or raw rows expired inside the raw window")
		}
		return nil
	}))
	view, err := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", Timezone: "Europe/Berlin", View: viewProvider})
	must(t, err)
	if view.Summary.Requests != 1 {
		t.Fatalf("non-UTC view lost data still available to raw retention: %+v", view.Summary)
	}

	*now = now.Add(7 * 24 * time.Hour)
	must(t, s.Maintenance())
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("minutes")).Stats().KeyN != 0 || tx.Bucket([]byte("daily")).Stats().KeyN != 0 || tx.Bucket([]byte("records")).Stats().KeyN != 0 {
			t.Fatal("history survived after both daily and raw retention expired")
		}
		return nil
	}))
}
