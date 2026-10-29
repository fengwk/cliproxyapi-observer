package engine

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

func detailedEvent(k Key, id string, at time.Time) pluginapi.UsageRecord {
	u := event(k, id, at)
	u.Stream = true
	u.Latency = 2 * time.Second
	u.TTFT = 500 * time.Millisecond
	u.AuthIndex = "auth-a"
	return u
}

// Detailed aggregates are written in the accounting transaction and returned
// flat by Summary, View and group views without repricing.
func TestDetailedAggregatesAndRequestViews(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true, Label: "Alpha"})
	b, _ := key(t, s, KeySpec{Enabled: true, Label: "Beta"})
	first := detailedEvent(a, "exec-a", day(*now).Add(10*time.Hour))
	must(t, s.RecordUsage(first))
	second := detailedEvent(a, "exec-b", day(*now).Add(11*time.Hour))
	second.Failed = true
	second.TTFT = time.Second
	second.Latency = 4 * time.Second
	p.cost, p.source = 0, "zero"
	must(t, s.RecordUsage(second))
	p.cost, p.source = 7, ""
	third := detailedEvent(b, "exec-c", day(*now).Add(11*time.Hour))
	third.Provider = "anthropic"
	third.Model = "other-model"
	third.AuthIndex = "auth-b"
	third.Stream = false
	third.TTFT = 0
	third.Latency = 1500 * time.Millisecond
	must(t, s.RecordUsage(third))

	view, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewRequests, Query: Query{PageSize: 2}})
	must(t, e)
	if view.Summary.Requests != 3 || view.Summary.Failures != 1 || view.Summary.CostMicroUSD != 14 || view.Summary.Tokens != 30 {
		t.Fatal("base aggregate changed", view.Summary)
	}
	if view.Summary.InputTokens != 24 || view.Summary.OutputTokens != 6 || view.Summary.DetailedRequests != 3 || view.Summary.CompleteRequests != 3 {
		t.Fatal("token detail missing", view.Summary)
	}
	if view.Summary.LatencyMS != 7500 || view.Summary.LatencySamples != 3 || view.Summary.TTFTMS != 1500 || view.Summary.TTFTSamples != 2 {
		t.Fatal("latency detail missing", view.Summary)
	}
	if view.Range.Timezone != "UTC" || !view.Range.AsOf.Equal(*now) || !view.Range.To.Equal(*now) || view.Series.Granularity != "hour" {
		t.Fatal("range/series contract", view.Range, view.Series.Granularity)
	}
	if len(view.Records) != 2 || !view.Pagination.HasMore || view.Pagination.Page != 1 || view.Pagination.PageSize != 2 {
		t.Fatal("request page", view.Records, view.Pagination)
	}
	if view.Records[0].ID != "exec-c" || view.Records[1].ID != "exec-b" {
		t.Fatal("request order", view.Records)
	}
	if len(view.Groups) != 0 || view.Coverage.BackfillPending || view.Coverage.DetailedRequests != 3 || view.Coverage.Requests != 3 {
		t.Fatal("requests view coverage", view)
	}
	if view.Coverage.RawFrom == nil || view.Coverage.DailyFrom == nil {
		t.Fatal("coverage retention bounds missing")
	}

	checks := []struct {
		view   string
		length int
		find   func(Group) bool
	}{
		{viewModel, 2, func(g Group) bool {
			return g.Model == "gpt-exact" && g.Provider == "openai" && g.Requests == 2 && g.Label == "gpt-exact"
		}},
		{viewKey, 2, func(g Group) bool { return g.KeyID == a.ID && g.Label == "Alpha" && g.Requests == 2 }},
		{viewAccount, 2, func(g Group) bool { return g.AuthIndex == "auth-a" && g.Requests == 2 }},
		{viewProvider, 2, func(g Group) bool { return g.Provider == "openai" && g.Requests == 2 }},
	}
	for _, check := range checks {
		v, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: check.view})
		must(t, e)
		if len(v.Groups) != check.length || len(v.Records) != 0 {
			t.Fatal("group page", check.view, v.Groups, v.Records)
		}
		found := false
		for _, g := range v.Groups {
			if check.find(g) {
				found = true
			}
		}
		if !found {
			t.Fatal("group missing", check.view, v.Groups)
		}
		if v.Groups[0].Requests < v.Groups[1].Requests {
			t.Fatal("group order", check.view, v.Groups)
		}
	}

	// JSON is flat by contract: no nested aggregate object.
	raw, e := json.Marshal(view)
	must(t, e)
	var parsed map[string]any
	must(t, json.Unmarshal(raw, &parsed))
	summary := parsed["summary"].(map[string]any)
	if _, ok := summary["input_tokens"]; !ok {
		t.Fatal("summary not flat", string(raw))
	}
	if _, ok := summary["aggregate"]; ok {
		t.Fatal("summary wrapped", string(raw))
	}
	series := parsed["series"].(map[string]any)
	bucket := series["buckets"].([]any)[0].(map[string]any)
	if _, ok := bucket["latency_samples"]; !ok {
		t.Fatal("bucket detail missing", bucket)
	}
}

// The composite view keeps the full identity in a JSON triple: equal labels do
// not merge Keys, and equal model names do not merge providers. Filtering and
// paging reuse the existing group query, and expired raw details still rely on
// the daily rollup.
func TestKeyModelViewIdentityFilteringAndDailyRollup(t *testing.T) {
	s, now, _ := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true, Label: "Shared Label"})
	b, _ := key(t, s, KeySpec{Enabled: true, Label: "Shared Label"})
	base := day(*now)
	add := func(k Key, id, provider, model string, at time.Time) {
		u := event(k, id, at)
		u.Provider, u.Model = provider, model
		must(t, s.RecordUsage(u))
	}
	add(a, "a-openai-1", "openai", "gpt-exact", base.Add(8*time.Hour))
	add(a, "a-openai-2", "openai", "gpt-exact", base.Add(9*time.Hour))
	add(a, "a-azure", "azure", "gpt-exact", base.Add(10*time.Hour))
	add(a, "a-other", "openai", "other-model", base.Add(10*time.Hour))
	add(b, "b-openai", "openai", "gpt-exact", base.Add(11*time.Hour))

	expected := map[string]struct {
		keyID, provider, model string
		requests               int64
	}{
		a.ID + "/openai/gpt-exact":   {a.ID, "openai", "gpt-exact", 2},
		a.ID + "/azure/gpt-exact":    {a.ID, "azure", "gpt-exact", 1},
		a.ID + "/openai/other-model": {a.ID, "openai", "other-model", 1},
		b.ID + "/openai/gpt-exact":   {b.ID, "openai", "gpt-exact", 1},
	}
	view, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewKeyModel})
	must(t, e)
	if view.Summary.Requests != 5 || len(view.Groups) != len(expected) {
		t.Fatalf("composite summary/groups = %+v/%d", view.Summary, len(view.Groups))
	}
	var groupTotal int64
	keyIDs := map[string]bool{}
	for _, g := range view.Groups {
		var identity []string
		must(t, json.Unmarshal([]byte(g.Key), &identity))
		if len(identity) != 3 {
			t.Fatalf("identity is not a JSON triple: %q", g.Key)
		}
		lookup := identity[0] + "/" + identity[1] + "/" + identity[2]
		want, ok := expected[lookup]
		if !ok || identity[0] != want.keyID || identity[1] != want.provider || identity[2] != want.model {
			t.Fatalf("unexpected composite identity %q: %+v", g.Key, g)
		}
		if g.KeyID != want.keyID || g.Provider != want.provider || g.Model != want.model ||
			g.Label != "Shared Label" || g.Requests != want.requests {
			t.Fatalf("group identity/label/count = %+v, want %+v", g, want)
		}
		groupTotal += g.Requests
		keyIDs[g.KeyID] = true
	}
	if groupTotal != view.Summary.Requests || len(keyIDs) != 2 {
		t.Fatalf("group totals/labels merged: total=%d keys=%v", groupTotal, keyIDs)
	}

	filtered, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewKeyModel, Query: Query{KeyID: a.ID, PageSize: 2}})
	must(t, e)
	if filtered.Summary.Requests != 4 || len(filtered.Groups) != 2 || !filtered.Pagination.HasMore {
		t.Fatalf("filtered first page = %+v/%d/%+v", filtered.Summary, len(filtered.Groups), filtered.Pagination)
	}
	second, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewKeyModel, Query: Query{KeyID: a.ID, Page: 2, PageSize: 2}})
	must(t, e)
	if len(second.Groups) != 1 || second.Pagination.HasMore {
		t.Fatalf("filtered second page = %d/%+v", len(second.Groups), second.Pagination)
	}

	// Expire raw records and hourly rollups; the complete-day query must still
	// reconstruct the same composite identities from the daily rollup.
	*now = now.Add(26 * time.Hour)
	must(t, s.Maintenance())
	rollup, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewKeyModel})
	must(t, e)
	if rollup.Summary.Requests != 5 || len(rollup.Groups) != len(expected) {
		t.Fatalf("daily rollup lost composite groups: %+v/%d", rollup.Summary, len(rollup.Groups))
	}
}

// Cache/input denominators must use the same complete sample set. A total-only
// unknown provider remains visible in raw usage and authoritative totals/cost,
// but its unclassified buckets never blend into a misleading aggregate hit rate.
func TestMixedAccountingCoverageKeepsOneTokenSampleSet(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "complete", now.Add(-time.Hour))))
	unknown := event(k, "unknown", now.Add(-time.Hour))
	unknown.Provider = "unknown-provider"
	unknown.Detail = pluginapi.UsageDetail{TotalTokens: 10}
	must(t, s.RecordUsage(unknown))
	v, err := s.RequestView(ViewQuery{})
	must(t, err)
	if v.Summary.Requests != 2 || v.Summary.DetailedRequests != 2 || v.Summary.CompleteRequests != 1 ||
		v.Summary.Tokens != 20 || v.Summary.InputTokens != 8 || v.Summary.OutputTokens != 2 || v.Summary.CostMicroUSD != 14 {
		t.Fatal("incomplete raw buckets contaminated the complete sample set", v.Summary)
	}
}

// Date inputs are inclusive UTC dates, today clamps to the query instant, and
// invalid or over-large paging is rejected.
func TestRequestViewDateAndPagingBounds(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	yesterday := day(*now).AddDate(0, 0, -1)
	must(t, s.RecordUsage(event(k, "yesterday", yesterday.Add(23*time.Hour+59*time.Minute))))
	must(t, s.RecordUsage(event(k, "today", day(*now).Add(time.Hour))))

	full, e := s.RequestView(ViewQuery{FromDate: "2024-01-30", ToDate: "2024-01-30", View: viewRequests})
	must(t, e)
	if full.Summary.Requests != 1 || !full.Range.To.Equal(day(*now)) {
		t.Fatal("inclusive end date", full.Range, full.Summary)
	}
	defaults, e := s.RequestView(ViewQuery{View: viewRequests})
	must(t, e)
	if defaults.Summary.Requests != 1 || !defaults.Range.From.Equal(day(*now)) || !defaults.Range.To.Equal(*now) {
		t.Fatal("default today/clamp", defaults.Range, defaults.Summary)
	}
	future, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2099-01-01", View: viewRequests})
	must(t, e)
	if !future.Range.To.Equal(*now) {
		t.Fatal("future clamp", future.Range)
	}
	for _, q := range []ViewQuery{
		{FromDate: "2024-1-1", ToDate: "2024-01-02"},
		{FromDate: "2024-01-03", ToDate: "2024-01-02"},
		{FromDate: "2024-01-31", ToDate: "2024-01-31", Query: Query{PageSize: 101}},
		{FromDate: "2024-01-31", ToDate: "2024-01-31", View: "unknown"},
		{FromDate: "2024-01-31", ToDate: "2024-01-31", Query: Query{Page: -1}},
	} {
		if _, e := s.RequestView(q); !errors.Is(e, ErrInvalid) {
			t.Fatal("invalid view accepted", q, e)
		}
	}
}

// Facets are stable, bounded metadata options and retain deleted Key labels.
func TestFacetsSearchCursorAndDeletedKey(t *testing.T) {
	s, now, _ := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true, Label: "Alpha"})
	b, _ := key(t, s, KeySpec{Enabled: true, Label: "Beta"})
	u := event(a, "one", *now)
	u.AuthIndex = "auth-a"
	must(t, s.RecordUsage(u))
	u = event(b, "two", *now)
	u.Provider = "other"
	u.Model = "other-model"
	u.AuthIndex = "auth-b"
	must(t, s.RecordUsage(u))
	must(t, s.DeleteKey(b.ID))

	first, e := s.Facets(FacetQuery{Field: facetModel, Limit: 1})
	must(t, e)
	if len(first.Items) != 1 || first.Items[0].Value != "gpt-exact" || first.NextCursor == "" {
		t.Fatal("model facet page", first)
	}
	second, e := s.Facets(FacetQuery{Field: facetModel, Limit: 1, Cursor: first.NextCursor})
	must(t, e)
	if len(second.Items) != 1 || second.Items[0].Value != "other-model" || second.NextCursor != "" {
		t.Fatal("model facet cursor", second)
	}
	search, e := s.Facets(FacetQuery{Field: facetModel, Search: "OTHER"})
	must(t, e)
	if len(search.Items) != 1 || search.Items[0].Value != "other-model" {
		t.Fatal("facet search", search)
	}
	keys, e := s.Facets(FacetQuery{Field: facetKeyID})
	must(t, e)
	if len(keys.Items) != 2 {
		t.Fatal("key facets", keys)
	}
	foundDeleted := false
	for _, item := range keys.Items {
		if item.Value == b.ID && item.Label == "Beta" {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatal("deleted key history missing", keys)
	}
	for _, field := range []string{facetProvider, facetAuthIndex} {
		page, e := s.Facets(FacetQuery{Field: field})
		must(t, e)
		if len(page.Items) != 2 {
			t.Fatal("facet field", field, page)
		}
	}
	for _, q := range []FacetQuery{{Field: "secret"}, {Field: facetModel, Limit: 101}, {Field: facetModel, Limit: -1}, {Field: facetModel, Cursor: "%%%"}} {
		if _, e := s.Facets(q); !errors.Is(e, ErrInvalid) {
			t.Fatal("invalid facet accepted", q, e)
		}
	}
}

// Read-time retention applies to facet dimensions even when the maintenance
// worker has not physically removed the old rollups.
func TestFacetsIgnoreExpiredRollupsBeforeCleanup(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "old", *now)))
	*now = now.AddDate(2, 0, 0)
	v := event(k, "current", *now)
	v.Model = "current-model"
	must(t, s.RecordUsage(v))
	out, err := s.Facets(FacetQuery{Field: facetModel})
	must(t, err)
	if len(out.Items) != 1 || out.Items[0].Value != "current-model" {
		t.Fatal("expired dimensions survived read retention", out)
	}
}

// Old daily rollups keep their original totals but have detailed_requests=0;
// callers must not render the unavailable detail as real zero use.
func TestOldDailyCoverageNotZeroFilled(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "old", *now)))
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("daily"))
		return b.ForEach(func(key, value []byte) error {
			var d daily
			if e := decode(value, &d); e != nil {
				return e
			}
			d.Bucket.Aggregate = Aggregate{Requests: d.Bucket.Requests, Failures: d.Bucket.Failures, CostMicroUSD: d.Bucket.CostMicroUSD, Tokens: d.Bucket.Tokens}
			return b.Put(key, mustJSON(t, d))
		})
	}))
	*now = now.Add(26 * time.Hour)
	view, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewRequests})
	must(t, e)
	if view.Summary.Requests != 1 || view.Summary.DetailedRequests != 0 || view.Coverage.DetailedRequests != 0 || view.Coverage.Requests != 1 {
		t.Fatal("old history was filled as zero", view.Summary, view.Coverage)
	}
	for _, bucket := range view.Series.Buckets {
		if bucket.DetailedRequests != 0 {
			t.Fatal("old bucket detailed fields filled", bucket)
		}
	}
}

// Checkpointed backfill adds detail to old daily rows and creates hourly rows
// without adding the original four totals again; rerunning is idempotent.
func TestStatisticsBackfillIdempotent(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	u := event(k, "legacy", *now)
	u.Latency = 2 * time.Second
	u.TTFT = 500 * time.Millisecond
	u.Stream = true
	must(t, s.RecordUsage(u))
	path := s.path
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket([]byte("records"))
		raw := rb.Get([]byte("legacy"))
		var stored recordData
		if e := decode(raw, &stored); e != nil {
			return e
		}
		stored.StatsVersion = 0
		if e := put(rb, "legacy", stored); e != nil {
			return e
		}
		db := tx.Bucket([]byte("daily"))
		if e := db.ForEach(func(key, value []byte) error {
			var d daily
			if e := decode(value, &d); e != nil {
				return e
			}
			d.Bucket.Aggregate = Aggregate{Requests: d.Bucket.Requests, Failures: d.Bucket.Failures, CostMicroUSD: d.Bucket.CostMicroUSD, Tokens: d.Bucket.Tokens}
			return db.Put(key, mustJSON(t, d))
		}); e != nil {
			return e
		}
		if e := tx.DeleteBucket([]byte("hourly")); e != nil && e != bolt.ErrBucketNotFound {
			return e
		}
		if _, e := tx.CreateBucket([]byte("hourly")); e != nil {
			return e
		}
		m := tx.Bucket([]byte("meta"))
		if e := m.Delete([]byte(statsBackfillDoneKey)); e != nil {
			return e
		}
		if e := m.Delete([]byte(statsBackfillCursor)); e != nil {
			return e
		}
		return m.Delete([]byte(statsBackfillVersion))
	}))
	must(t, s.Close())
	*now = now.Add(26 * time.Hour)
	r, e := Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	must(t, r.db.View(func(tx *bolt.Tx) error {
		var stored recordData
		if e := get(tx.Bucket([]byte("records")), "legacy", &stored); e != nil {
			return e
		}
		if stored.StatsVersion != statsVersion {
			t.Fatal("record not marked")
		}
		if tx.Bucket([]byte("hourly")).Stats().KeyN == 0 {
			t.Fatal("hourly backfill missing")
		}
		if tx.Bucket([]byte("meta")).Get([]byte(statsBackfillDoneKey)) == nil {
			t.Fatal("backfill not checkpointed")
		}
		return nil
	}))
	summary, e := r.Summary(Query{From: day(time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)), To: day(time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC)).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 || summary.DetailedRequests != 1 || summary.InputTokens != 8 {
		t.Fatal("backfill changed base totals or missed detail", summary)
	}
	before := summary
	if e := r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Delete([]byte(statsBackfillDoneKey))
	}); e != nil {
		t.Fatal(e)
	}
	must(t, r.backfillStatistics())
	again, e := r.Summary(Query{From: day(*now).AddDate(0, 0, -1), To: day(*now)})
	must(t, e)
	if again.Requests != before.Requests || again.DetailedRequests != before.DetailedRequests {
		t.Fatal("backfill double counted", before, again)
	}
}

// Aggregate overflow aborts the whole accounting transaction.
func TestAggregateOverflowRollsBack(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	u := event(k, "overflow", *now)
	u.Latency = time.Millisecond
	d := rollupDaily(Record{KeyID: k.ID, Model: u.Model, Provider: u.Provider, AuthIndex: u.AuthIndex}, day(*now))
	d.Bucket.Aggregate = Aggregate{LatencyMS: maxSafe}
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		return put(tx.Bucket([]byte("daily")), dailyID(d), d)
	}))
	if e := s.RecordUsage(u); !errors.Is(e, ErrAccounting) {
		t.Fatal("overflow not rejected", e)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("records")).Get([]byte("overflow")) != nil {
			t.Fatal("overflowing record persisted")
		}
		var got daily
		if e := get(tx.Bucket([]byte("daily")), dailyID(d), &got); e != nil {
			return e
		}
		if got.Bucket.LatencyMS != maxSafe || got.Bucket.Requests != 0 {
			t.Fatal("overflow transaction not rolled back", got)
		}
		return nil
	}))
}

// Detail exposes the saved quote and callers cannot mutate its nested slices or
// pointers back into the store.
func TestDetailPriceQuoteImmutable(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "quote", *now)))
	reasoning := int64(3)
	catalogTime := *now
	quote := pricing.Quote{
		CostMicroUSD: 7, Source: "manual",
		Rates:       pricing.Rates{Input: 1, Reasoning: &reasoning},
		Definition:  pricing.PricingDefinition{Base: pricing.RateSet{Input: 1, CacheWriteByTTL: map[int64]int64{300: 1}}},
		CatalogTime: &catalogTime, SupportIssues: []string{"output"},
		Candidates: []pricing.Candidate{{Provider: "p", Model: "m", Rates: pricing.Rates{Reasoning: &reasoning}, SupportIssues: []string{"input"}}},
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		var stored recordData
		if e := get(tx.Bucket([]byte("records")), "quote", &stored); e != nil {
			return e
		}
		stored.Quote = quote
		return put(tx.Bucket([]byte("records")), "quote", stored)
	}))
	d, e := s.Detail("quote")
	must(t, e)
	if d.PriceQuote.CostMicroUSD != 7 || d.PriceQuote.Source != "manual" || d.PriceQuote.Rates.Reasoning == nil || *d.PriceQuote.Rates.Reasoning != 3 {
		t.Fatal("price quote missing", d.PriceQuote)
	}
	*d.PriceQuote.Rates.Reasoning = 99
	d.PriceQuote.Definition.Base.CacheWriteByTTL[300] = 9
	d.PriceQuote.SupportIssues[0] = "changed"
	d.PriceQuote.Candidates[0].SupportIssues[0] = "changed"
	*d.PriceQuote.CatalogTime = d.PriceQuote.CatalogTime.Add(time.Hour)
	again, e := s.Detail("quote")
	must(t, e)
	if again.PriceQuote.Rates.Reasoning == nil || *again.PriceQuote.Rates.Reasoning != 3 ||
		again.PriceQuote.Definition.Base.CacheWriteByTTL[300] != 1 ||
		again.PriceQuote.SupportIssues[0] != "output" ||
		again.PriceQuote.Candidates[0].SupportIssues[0] != "input" ||
		!again.PriceQuote.CatalogTime.Equal(catalogTime) {
		t.Fatal("saved quote mutated", again.PriceQuote)
	}
}

// Querying releases the admission/accounting state mutex before scanning and
// remains race-safe while new usage is committed.
func TestConcurrentQueriesAndAccounting(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "seed", *now)))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31", View: viewRequests}); e != nil {
					errs <- e
					return
				}
				if _, e := s.Summary(Query{From: day(*now), To: day(*now).Add(24 * time.Hour)}); e != nil {
					errs <- e
					return
				}
				if _, e := s.Facets(FacetQuery{Field: facetModel}); e != nil {
					errs <- e
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		must(t, s.RecordUsage(event(k, "concurrent-"+string(rune('a'+i)), *now)))
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// Close waits for an in-flight read transaction through dbMu instead of racing
// the bbolt handle. Queries started after shutdown fail closed.
func TestQueryCloseLifecycle(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "one", *now)))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				_, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31"})
				if errors.Is(e, ErrClosed) {
					return
				}
				if e != nil {
					t.Error(e)
					return
				}
			}
		}()
	}
	close(start)
	must(t, s.Close())
	wg.Wait()
	if _, e := s.RequestView(ViewQuery{FromDate: "2024-01-31", ToDate: "2024-01-31"}); !errors.Is(e, ErrClosed) {
		t.Fatal("query after close", e)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, e := json.Marshal(v)
	must(t, e)
	return raw
}
