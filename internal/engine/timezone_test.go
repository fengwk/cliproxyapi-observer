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

// Minute aggregates expire with daily retention even while much older raw rows
// are still configured to survive.
func TestMinuteAggregatesFollowDailyRetention(t *testing.T) {
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
		if tx.Bucket([]byte("minutes")).Stats().KeyN != 0 || tx.Bucket([]byte("daily")).Stats().KeyN != 0 {
			t.Fatal("minute aggregates did not follow daily retention")
		}
		if tx.Bucket([]byte("records")).Stats().KeyN != 1 {
			t.Fatal("raw row expired with daily retention instead of raw retention")
		}
		return nil
	}))
}
