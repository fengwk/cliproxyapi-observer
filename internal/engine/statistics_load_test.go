package engine

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	bolt "go.etcd.io/bbolt"
)

// A corrupt row after the first 1,000-row batch must roll back its entire
// batch, leave a durable checkpoint, and resume after repair without repricing
// or adding the old base totals twice.
func TestStatisticsBackfillResumesCommittedBatch(t *testing.T) {
	s, now, pricer := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	const count = 2005
	row := func(i int) recordData {
		return recordData{Record: Record{ID: fmt.Sprintf("legacy-%04d", i), KeyID: k.ID,
			Time: now.Add(-time.Hour), Model: "gpt-exact", Provider: "openai",
			Tokens:       pricing.Tokens{Input: 8, Output: 2, Total: 10},
			CostMicroUSD: 7, PriceSource: "manual", AccountingQuality: "complete"}}
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		for i := 0; i < count; i++ {
			r := row(i)
			if e := put(tx.Bucket([]byte("records")), r.ID, r); e != nil {
				return e
			}
			if e := tx.Bucket([]byte("timeline")).Put([]byte(timeID(r.Record)), []byte(r.ID)); e != nil {
				return e
			}
		}
		d := rollupDaily(row(0).Record, day(*now))
		d.Bucket.Aggregate = Aggregate{Requests: count, Tokens: 10 * count, CostMicroUSD: 7 * count}
		if e := put(tx.Bucket([]byte("daily")), dailyID(d), d); e != nil {
			return e
		}
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), k.ID, &stored); e != nil {
			return e
		}
		stored.Used.Total = 7 * count
		if e := put(tx.Bucket([]byte("keys")), k.ID, stored); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("meta")).Delete([]byte(statsBackfillDoneKey)); e != nil {
			return e
		}
		return tx.Bucket([]byte("records")).Put([]byte(row(1002).ID), []byte("not-json-or-gzip"))
	}))
	if err := s.backfillStatistics(); err == nil {
		t.Fatal("corruption did not abort backfill")
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if cursor := string(tx.Bucket([]byte("meta")).Get([]byte(statsBackfillCursor))); cursor != row(1000).ID {
			t.Fatal("batch checkpoint not durable", cursor)
		}
		for _, i := range []int{999, 1000} {
			var got recordData
			if e := get(tx.Bucket([]byte("records")), row(i).ID, &got); e != nil {
				return e
			}
			want := 0
			if i == 999 {
				want = statsVersion
			}
			if got.StatsVersion != want {
				t.Fatal("batch did not commit/roll back atomically", i, got.StatsVersion)
			}
		}
		return nil
	}))
	must(t, s.db.Update(func(tx *bolt.Tx) error { return put(tx.Bucket([]byte("records")), row(1002).ID, row(1002)) }))
	path := s.path
	must(t, s.Close())
	reopened, err := Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: pricer})
	must(t, err)
	defer reopened.Close()
	view, err := reopened.RequestView(ViewQuery{})
	must(t, err)
	if view.Summary.Requests != count || view.Summary.DetailedRequests != count ||
		view.Summary.InputTokens != 8*count || view.Summary.CostMicroUSD != 7*count || view.Coverage.BackfillPending {
		t.Fatal("resumed backfill omitted or doubled data", view.Summary)
	}
	if total(t, reopened).Used.Total != 7*count || pricer.calls != 0 {
		t.Fatal("backfill changed quota or queried the pricer")
	}
}

// A cross-day grouped view must not decompress raw rows for complete hours.
// Deliberately corrupt one such row while keeping the transactional rollups;
// partial edge hours still read raw rows and must retain exact UTC totals.
func TestDailyViewReusesHourlyRollups(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	for id, at := range map[string]time.Time{
		"old":           day(*now).AddDate(0, 0, -2).Add(time.Hour),
		"complete-hour": day(*now).Add(10 * time.Hour),
		"partial-hour":  day(*now).Add(11*time.Hour + 30*time.Minute),
	} {
		must(t, s.RecordUsage(event(k, id, at)))
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("records")).Put([]byte("complete-hour"), []byte("corrupt-full-hour-must-not-be-read"))
	}))
	view, err := s.RequestView(ViewQuery{View: viewProvider, Query: Query{From: day(*now).AddDate(0, 0, -2), To: now.Add(-15 * time.Minute)}})
	must(t, err)
	if view.Summary.Requests != 3 || view.Summary.CostMicroUSD != 21 || view.Summary.InputTokens != 24 ||
		len(view.Groups) != 1 || len(view.Series.Buckets) != 2 {
		t.Fatal("hourly/daily edge merge lost or duplicated totals", view)
	}
	for _, bucket := range view.Series.Buckets {
		if !bucket.Time.Equal(day(bucket.Time)) {
			t.Fatal("hourly data leaked into a daily trend", bucket)
		}
	}
}

// Seed real compressed metadata plus 36,500 daily rows in one transaction so
// load measurements exclude setup/fsync rather than excluding query work.
func seedReadLoad(tb testing.TB, records int) (*Store, Key, time.Time) {
	tb.Helper()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s, err := Open(Options{DataPath: filepath.Join(tb.TempDir(), "load.db"), Now: func() time.Time { return now }, Pricer: &fakePrice{cost: 7}})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	k, _, err := s.CreateKey(KeySpec{Enabled: true})
	if err != nil {
		tb.Fatal(err)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		for i := 0; i < records; i++ {
			r := Record{ID: fmt.Sprintf("record-%06d", i), KeyID: k.ID, Time: day(now).Add(8*time.Hour + time.Duration(i)*time.Millisecond),
				Model: fmt.Sprintf("model-%03d", i%100), Provider: "openai", Tokens: pricing.Tokens{Input: 8, Output: 2, Total: 10},
				CostMicroUSD: 7, PriceSource: "manual", AccountingQuality: "complete"}
			if e := put(tx.Bucket([]byte("records")), r.ID, recordData{Record: r, StatsVersion: statsVersion}); e != nil {
				return e
			}
			if e := tx.Bucket([]byte("timeline")).Put([]byte(timeID(r)), []byte(r.ID)); e != nil {
				return e
			}
			for _, bucket := range []string{"hourly", "daily"} {
				at := r.Time.Truncate(time.Hour)
				if bucket == "daily" {
					at = day(r.Time)
				}
				if e := applyRollup(tx, bucket, r, at); e != nil {
					return e
				}
			}
		}
		for days := 1; days <= 365; days++ {
			for model := 0; model < 100; model++ {
				d := rollupDaily(Record{KeyID: k.ID, Model: fmt.Sprintf("model-%03d", model), Provider: "openai"}, day(now).AddDate(0, 0, -days))
				d.Bucket.Aggregate = Aggregate{Requests: 1000, Tokens: 10000, CostMicroUSD: 7000, InputTokens: 8000,
					OutputTokens: 2000, DetailedRequests: 1000, CompleteRequests: 1000}
				if e := put(tx.Bucket([]byte("daily")), dailyID(d), d); e != nil {
					return e
				}
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return s, k, now
}

// Assert each result as well as reporting time/allocation measurements.
func BenchmarkLargeRequestView(b *testing.B) {
	s, _, now := seedReadLoad(b, 10000)
	for _, test := range []struct {
		name, view string
		days       int
		want       int64
	}{
		{"today-10000-raw", "requests", 0, 10000},
		{"month-models", "model", 29, 2910000},
		{"year-providers", "provider", 365, 36510000},
	} {
		b.Run(test.name, func(b *testing.B) {
			q := ViewQuery{FromDate: day(now).AddDate(0, 0, -test.days).Format("2006-01-02"), ToDate: day(now).Format("2006-01-02"), View: test.view}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				v, e := s.RequestView(q)
				if e != nil || v.Summary.Requests != test.want {
					b.Fatal("large view", v.Summary, e)
				}
			}
		})
	}
	b.Run("historical-facets", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			v, e := s.Facets(FacetQuery{Field: facetModel, Limit: 100})
			if e != nil || len(v.Items) != 100 {
				b.Fatal("large facets", v, e)
			}
		}
	})
}
