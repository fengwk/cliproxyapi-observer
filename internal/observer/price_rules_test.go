package observer

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

func threshold(n int64) *int64 { return &n }

// First match prices the entire request; tiers are not marginal rate segments.
func TestOrderedPriceRulesAndUTCBoundaries(t *testing.T) {
	rules := []PriceRule{
		{Model: "m", InputTokensGT: threshold(512000), Price: Price{Input: 3}},
		{Model: "m", InputTokensGT: threshold(256000), TimeRange: "22:00-06:00", Price: Price{Input: 2}},
		{Model: "m", Price: Price{Input: 1}},
	}
	for _, tc := range []struct {
		input int64
		at    string
		want  float64
	}{
		{512001, "2026-10-09T12:00:00Z", 3},
		{512000, "2026-10-09T22:00:00Z", 2},
		{256001, "2026-10-09T05:59:59Z", 2},
		{256001, "2026-10-09T06:00:00Z", 1},
		{256000, "2026-10-09T22:00:00Z", 1},
		{256001, "2026-10-10T02:00:00+04:00", 2},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		price, ok := selectRulePrice("m", tc.input, at, rules, nil)
		if !ok || price.Input != tc.want {
			t.Fatalf("rule match %v got %v", tc, price)
		}
	}
	raw := []byte(`{"model":"m","input-tokens-gt":2,"time-range":"00:00-24:00","price":{"input":1}}`)
	var rule PriceRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizePriceRules([]PriceRule{rule}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"model":"m","price":{},"input-tokens-gt":null}`,
		`{"model":"m","price":{},"input-tokens-gt":1.5}`,
		`{"model":"m","price":{},"input-tokens-gt":1,"input_tokens_gt":2}`,
		`{"model":"m","price":{},"extra":true}`,
	} {
		if json.Unmarshal([]byte(raw), &rule) == nil {
			t.Fatal("invalid rule accepted")
		}
	}
	for _, r := range []PriceRule{
		{Model: "m", InputTokensGT: threshold(-1)},
		{Model: "m", TimeRange: "08:00-08:00"},
		{Model: "m", TimeRange: "24:00-08:00"},
		{Model: "m", TimeRange: "08:00-25:00"},
	} {
		if _, err := NormalizePriceRules([]PriceRule{r}); err == nil {
			t.Fatal("invalid condition accepted")
		}
	}
}

// Key/tier stats survive request expiry and repricing without scanning requests.
func TestKeyTierSummarySurvivesRequestExpiryAndRepricing(t *testing.T) {
	cfg := testConfig(t, func(cfg *Config) {
		cfg.PriceRules = []PriceRule{
			{Model: "m", InputTokensGT: threshold(100), Price: Price{Input: 2, Output: 1}},
			{Model: "m", Price: Price{Input: 1, Output: 1}},
		}
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	ids := []string{}
	for i, n := range []int64{100, 101, 200} {
		r := usageRecord(string(rune('a'+i)), "openai", "m", at, simpleUsage(n, 10))
		r.APIKey = []string{"fake-a", "fake-a", "fake-b"}[i]
		r.AuthIndex = "0123456789abcdef"
		s.SubmitUsage(r)
		ids = append(ids, s.clientKeyID(r.APIKey))
	}
	flushAll(t, s)
	q := Query{From: at.Add(-time.Minute), To: at.Add(time.Minute), ClientKeyID: ids[0]}
	sum, err := s.Summary(q)
	if err != nil || sum.Totals.Requests != 2 || len(sum.ClientKeys) != 1 || len(sum.Credentials) != 1 {
		t.Fatal("key grouping failed")
	}
	want := (100.0 + 202 + 20) / 1e6
	if math.Abs(sum.Totals.CostUSD-want) > 1e-12 {
		t.Fatalf("tier cost %g != %g", sum.Totals.CostUSD, want)
	}
	page, _ := s.Requests(q)
	var costs float64
	for _, r := range page.Items {
		if r.CostUSD == nil {
			t.Fatal("unpriced request")
		}
		costs += *r.CostUSD
	}
	if math.Abs(costs-want) > 1e-12 {
		t.Fatal("request and aggregate costs disagree")
	}
	// Read-time and physical expiry must not erase key-dimensional statistics.
	s.setNow(func() time.Time { return at.Add(2 * 24 * time.Hour) })
	if err := s.runCleanup(); err != nil {
		t.Fatal(err)
	}
	if page, _ := s.Requests(q); len(page.Items) != 0 {
		t.Fatal("requests did not expire")
	}
	sum, _ = s.Summary(q)
	if sum.Totals.Requests != 2 || math.Abs(sum.Totals.CostUSD-want) > 1e-12 {
		t.Fatal("key statistics lost at request expiry")
	}
	s.Close()
	cfg.PriceRules[0].Price.Input = 4
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sum, _ = reopened.Summary(q)
	if math.Abs(sum.Totals.CostUSD-(100.0+404+20)/1e6) > 1e-12 {
		t.Fatalf("query-time rules not applied after reopen: got %.18g want %.18g (%d requests)", sum.Totals.CostUSD, (100.0+404+20)/1e6, sum.Totals.Requests)
	}
}

// Migration copies old totals to unknown once; no historic credential is guessed.
func TestKeyStatsMigrationUnknownAndAtomic(t *testing.T) {
	cfg := testConfig(t, nil)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	s.SubmitUsage(usageRecord("legacy", "openai", "m", at, simpleUsage(100, 10)))
	flushAll(t, s)
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(bucketKeyStats) }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfg.PriceRules = []PriceRule{{Model: "m", InputTokensGT: threshold(10), Price: Price{Input: 2}}, {Model: "m", Price: Price{Input: 1}}}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := Query{From: at.Add(-time.Minute), To: at.Add(time.Minute), ClientKeyID: "unknown"}
	sum, err := reopened.Summary(q)
	if err != nil || sum.Totals.Requests != 1 || sum.Totals.UnpricedRequests != 1 || sum.Totals.CostUSD != 0 {
		t.Fatal("legacy tier guessed or totals lost")
	}
	reopened.Close()
	again, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	sum, _ = again.Summary(q)
	if sum.Totals.Requests != 1 {
		t.Fatal("migration duplicated totals")
	}
}

// Cached tokens contribute to tier length but retain their distinct unit price.
func TestPriceRuleUsesTotalInputAndPreservesCacheRates(t *testing.T) {
	s := openTestStore(t, func(cfg *Config) {
		cfg.PriceRules = []PriceRule{
			{Model: "m", InputTokensGT: threshold(90), TimeRange: "00:00-24:00",
				Price: Price{Input: 3, CacheRead: 0.1, Output: 5}},
			{Model: "m", Price: Price{Input: 1}},
		}
	})
	at := time.Now().Add(-time.Minute)
	record := usageRecord("cache-tier", "openai", "m", at, simpleUsage(100, 10))
	record.Detail.CacheReadTokens = 80
	s.SubmitUsage(record)
	flushAll(t, s)
	page, err := s.Requests(Query{})
	want := (20.0*3 + 80*0.1 + 10*5) / 1e6
	if err != nil || len(page.Items) != 1 || page.Items[0].CostUSD == nil ||
		math.Abs(*page.Items[0].CostUSD-want) > 1e-12 {
		t.Fatal("cache tokens were excluded from threshold or double priced")
	}
	summary, err := s.Summary(Query{})
	if err != nil || math.Abs(summary.Totals.CostUSD-want) > 1e-12 ||
		summary.Totals.CacheHits != 1 || summary.Totals.UnpricedRequests != 0 {
		t.Fatal("aggregate tier price differs from request")
	}
}

// 0 is a normal, valid price for free models (e.g. promotional hours or contributor tiers).
// Requests matching a zero-price rule must be marked as priced ($0.0000), not unpriced.
func TestFreeModelZeroPricePricedNotUnpriced(t *testing.T) {
	const model = "opencode-go/step-5-preview-free"
	s := openTestStore(t, func(cfg *Config) {
		cfg.PriceRules = []PriceRule{
			{
				Model:     model,
				TimeRange: "00:00-08:30",
				Price:     Price{Input: 0, Output: 0, CacheRead: 0, CacheCreation: 0},
			},
		}
	})

	// Match: 04:00 UTC is inside 00:00-08:30 UTC
	tMatch := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	// Miss: 12:00 UTC (yesterday, within 24h) is outside 00:00-08:30 UTC
	tMiss := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	s.SubmitUsage(pluginapi.UsageRecord{
		RequestID: "free-in-window", Provider: "opencode-go", Model: model,
		ExecutorType: "executorAdapter", RequestedAt: tMatch,
		Detail: pluginapi.UsageDetail{
			InputTokens: 22800, OutputTokens: 282, CacheReadTokens: 11300, TotalTokens: 23082,
		},
	})
	s.SubmitUsage(pluginapi.UsageRecord{
		RequestID: "free-out-of-window", Provider: "opencode-go", Model: model,
		ExecutorType: "executorAdapter", RequestedAt: tMiss,
		Detail: pluginapi.UsageDetail{
			InputTokens: 22800, OutputTokens: 282, CacheReadTokens: 11300, TotalTokens: 23082,
		},
	})
	flushAll(t, s)

	page, err := s.Requests(Query{Model: model})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("requests: count=%d, err=%v", len(page.Items), err)
	}

	for _, it := range page.Items {
		if it.RequestID == "free-in-window" {
			if it.CostUSD == nil {
				t.Fatalf("in-window free model request must not be nil (unpriced)")
			}
			if *it.CostUSD != 0.0 {
				t.Fatalf("in-window free model cost = %v, want 0.0", *it.CostUSD)
			}
		} else if it.RequestID == "free-out-of-window" {
			if it.CostUSD != nil {
				t.Fatalf("out-of-window request should be unpriced (nil), got %v", *it.CostUSD)
			}
		}
	}

	summary, err := s.Summary(Query{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Requests != 2 {
		t.Fatalf("totals.Requests = %d, want 2", summary.Totals.Requests)
	}
	if summary.Totals.UnpricedRequests != 1 {
		t.Fatalf("totals.UnpricedRequests = %d, want 1 (only the out-of-window request)", summary.Totals.UnpricedRequests)
	}
	if summary.Totals.CostUSD != 0.0 {
		t.Fatalf("totals.CostUSD = %v, want 0.0", summary.Totals.CostUSD)
	}
	if len(summary.Groups) != 1 {
		t.Fatalf("groups count = %d, want 1", len(summary.Groups))
	}
	if summary.Groups[0].UnpricedRequests != 1 {
		t.Fatalf("group.UnpricedRequests = %d, want 1", summary.Groups[0].UnpricedRequests)
	}
}
