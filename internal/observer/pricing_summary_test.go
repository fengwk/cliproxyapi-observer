package observer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

// assertRawBucketNoCostUSD iterates over all raw request records stored in
// bucketRequests and asserts that no record persists the monetary "cost_usd" field.
func assertRawBucketNoCostUSD(t *testing.T, s *Store) {
	t.Helper()
	err := s.view(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRequests)
		if rb == nil {
			t.Fatalf("bucketRequests does not exist")
		}
		c := rb.Cursor()
		count := 0
		for k, v := c.First(); k != nil; k, v = c.Next() {
			count++
			if bytes.Contains(v, []byte("cost_usd")) {
				t.Fatalf("raw request bytes contain cost_usd: %s", string(v))
			}
			var r Request
			if err := json.Unmarshal(v, &r); err != nil {
				t.Fatalf("unmarshal raw request JSON: %v", err)
			}
			if r.CostUSD != nil {
				t.Fatalf("unmarshaled raw request has non-nil CostUSD: %v", *r.CostUSD)
			}
		}
		if count == 0 {
			t.Fatalf("expected at least one raw request, got none")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assertRawBucketNoCostUSD failed: %v", err)
	}
}

// TestQueryTimePricingLifecycle tests the end-to-end pricing lifecycle across reopen cycles:
// 1) Open without prices -> requests read with nil CostUSD.
// 2) Reopen with prices -> same persisted complete row priced at current price, unknown remains nil.
// 3) Reopen with increased prices -> cost updates dynamically.
// 4) Reopen with removed prices -> cost returns to nil.
// 5) Reopen with zero prices -> cost is 0.0 and non-nil.
func TestQueryTimePricingLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "observer.db")
	cfg := Config{
		DataPath:           dbPath,
		StatsRetentionDays: 365,
		RequestRetention:   24 * time.Hour,
		BodyRetention:      24 * time.Hour,
		FlushInterval:      10 * time.Millisecond,
		Prices:             nil, // 1. Open without prices
	}

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open step 1: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	recComplete := usageRecord("req-complete", "openai", "test-model", now.Add(10*time.Second), simpleUsage(1000, 500))
	recUnknown := usageRecord("req-unknown", "unclassified-provider", "test-model", now.Add(20*time.Second), simpleUsage(2000, 1000))

	if !s.SubmitUsage(recComplete) || !s.SubmitUsage(recUnknown) {
		t.Fatalf("SubmitUsage failed")
	}
	flushAll(t, s)

	// Step 1: Requests read with nil CostUSD
	page, err := s.Requests(Query{From: now.Add(-time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Requests step 1: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.CostUSD != nil {
			t.Errorf("expected nil CostUSD without price, got %v for %s", *it.CostUSD, it.RequestID)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close step 1: %v", err)
	}

	// Step 2: Reopen with price: complete row priced at current price, unknown remains nil
	// input: 1000, output: 500. price: input 10, output 20 => (1000*10 + 500*20)/1e6 = 0.02
	cfg.Prices = map[string]Price{
		"test-model": {Input: 10, Output: 20},
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("Open step 2: %v", err)
	}
	page, err = s.Requests(Query{From: now.Add(-time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Requests step 2: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.RequestID == "req-complete" {
			if it.CostUSD == nil || !approx(*it.CostUSD, 0.02) {
				t.Fatalf("req-complete cost = %v, want 0.02", it.CostUSD)
			}
		} else if it.RequestID == "req-unknown" {
			if it.CostUSD != nil {
				t.Fatalf("req-unknown cost should remain nil, got %v", *it.CostUSD)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close step 2: %v", err)
	}

	// Step 3: Reopen with increased price: cost updates dynamically
	// price: input 30, output 50 => (1000*30 + 500*50)/1e6 = 0.055
	cfg.Prices = map[string]Price{
		"test-model": {Input: 30, Output: 50},
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("Open step 3: %v", err)
	}
	page, err = s.Requests(Query{From: now.Add(-time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Requests step 3: %v", err)
	}
	for _, it := range page.Items {
		if it.RequestID == "req-complete" {
			if it.CostUSD == nil || !approx(*it.CostUSD, 0.055) {
				t.Fatalf("req-complete repriced cost = %v, want 0.055", it.CostUSD)
			}
		} else if it.RequestID == "req-unknown" {
			if it.CostUSD != nil {
				t.Fatalf("req-unknown cost should remain nil, got %v", *it.CostUSD)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close step 3: %v", err)
	}

	// Step 4: Reopen after deleting price: cost returns to nil
	cfg.Prices = nil
	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("Open step 4: %v", err)
	}
	page, err = s.Requests(Query{From: now.Add(-time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Requests step 4: %v", err)
	}
	for _, it := range page.Items {
		if it.CostUSD != nil {
			t.Fatalf("expected nil CostUSD after deleting price, got %v for %s", *it.CostUSD, it.RequestID)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close step 4: %v", err)
	}

	// Step 5: Reopen with zero prices: cost is 0.0 and non-nil for all requests
	cfg.Prices = map[string]Price{
		"test-model": {Input: 0, Output: 0, CacheRead: 0, CacheCreation: 0},
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatalf("Open step 5: %v", err)
	}
	defer s.Close()
	page, err = s.Requests(Query{From: now.Add(-time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("Requests step 5: %v", err)
	}
	for _, it := range page.Items {
		if it.RequestID == "req-complete" {
			if it.CostUSD == nil {
				t.Fatalf("req-complete cost must be non-nil for zero price")
			}
			if *it.CostUSD != 0.0 {
				t.Fatalf("req-complete cost = %v, want 0.0", *it.CostUSD)
			}
		} else if it.RequestID == "req-unknown" {
			if it.CostUSD == nil || *it.CostUSD != 0.0 {
				t.Fatalf("req-unknown cost with zero price should be 0.0, got %v", it.CostUSD)
			}
		}
	}
}

// TestPricingRawStoreNoCost verifies that persisted raw request JSON bytes omit "cost_usd" entirely.
func TestPricingRawStoreNoCost(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{
			"model-priced": {Input: 10, Output: 20, CacheRead: 2, CacheCreation: 5},
		}
	})
	now := time.Now().UTC()
	s.SubmitUsage(usageRecord("r-comp", "openai", "model-priced", now.Add(time.Second), simpleUsage(500, 200)))
	s.SubmitUsage(usageRecord("r-cache", "openai", "model-priced", now.Add(2*time.Second), pluginapi.UsageDetail{
		InputTokens: 1000, CacheReadTokens: 800, OutputTokens: 100,
	}))
	s.SubmitUsage(usageRecord("r-unknown", "unknown-prov", "model-priced", now.Add(3*time.Second), simpleUsage(400, 300)))

	flushAll(t, s)

	assertRawBucketNoCostUSD(t, s)
}

// TestSummaryMixedCompleteAndUnknown verifies that summaries only bill complete requests,
// that unpriced requests reflect the unknown count, and that groups, series, and totals stay consistent.
func TestSummaryMixedCompleteAndUnknown(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	model := "mixed-quality-model"
	prices := map[string]Price{
		model: {Input: 10, Output: 20},
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = prices
	})
	s.setNow(clk.now)

	from := clk.now()
	at := from.Add(10 * time.Second)

	// 3 complete requests: 1000 in, 500 out -> cost = (1000*10 + 500*20)/1e6 = 0.02 each -> 0.06 total
	for i := 0; i < 3; i++ {
		s.SubmitUsage(usageRecord(fmt.Sprintf("comp-%d", i), "openai", model, at, simpleUsage(1000, 500)))
	}
	// 2 unknown requests: unclassified provider
	for i := 0; i < 2; i++ {
		s.SubmitUsage(usageRecord(fmt.Sprintf("unkn-%d", i), "mystery", model, at, simpleUsage(2000, 1000)))
	}
	flushAll(t, s)

	to := from.Add(time.Minute)
	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if summary.Totals.Requests != 5 {
		t.Fatalf("Totals.Requests = %d, want 5", summary.Totals.Requests)
	}
	if summary.Totals.UnpricedRequests != 2 {
		t.Fatalf("Totals.UnpricedRequests = %d, want 2", summary.Totals.UnpricedRequests)
	}
	if !approx(summary.Totals.CostUSD, 0.06) {
		t.Fatalf("Totals.CostUSD = %v, want 0.06", summary.Totals.CostUSD)
	}

	if len(summary.Groups) != 2 {
		t.Fatalf("Groups count = %d, want 2", len(summary.Groups))
	}
	var (
		sumGroupReq      uint64
		sumGroupUnpriced uint64
		sumGroupCost     float64
	)
	for _, g := range summary.Groups {
		sumGroupReq += g.Requests
		sumGroupUnpriced += g.UnpricedRequests
		sumGroupCost += g.CostUSD
		if g.Provider == "openai" {
			if g.Requests != 3 || g.UnpricedRequests != 0 {
				t.Errorf("openai group requests/unpriced = %d/%d, want 3/0", g.Requests, g.UnpricedRequests)
			}
			if !approx(g.CostUSD, 0.06) {
				t.Errorf("openai group CostUSD = %v, want 0.06", g.CostUSD)
			}
		} else if g.Provider == "mystery" {
			if g.Requests != 2 || g.UnpricedRequests != 2 {
				t.Errorf("mystery group requests/unpriced = %d/%d, want 2/2", g.Requests, g.UnpricedRequests)
			}
			if g.CostUSD != 0 {
				t.Errorf("mystery group CostUSD = %v, want 0", g.CostUSD)
			}
		}
	}
	if sumGroupReq != summary.Totals.Requests {
		t.Errorf("sum(group.Requests) = %d != Totals.Requests = %d", sumGroupReq, summary.Totals.Requests)
	}
	if sumGroupUnpriced != summary.Totals.UnpricedRequests {
		t.Errorf("sum(group.UnpricedRequests) = %d != Totals.UnpricedRequests = %d", sumGroupUnpriced, summary.Totals.UnpricedRequests)
	}
	if !approx(sumGroupCost, summary.Totals.CostUSD) {
		t.Errorf("sum(group.CostUSD) = %v != Totals.CostUSD = %v", sumGroupCost, summary.Totals.CostUSD)
	}

	var (
		sumSeriesReq      uint64
		sumSeriesUnpriced uint64
		sumSeriesCost     float64
	)
	for _, p := range summary.Series {
		sumSeriesReq += p.Requests
		sumSeriesUnpriced += p.UnpricedRequests
		sumSeriesCost += p.CostUSD
	}
	if sumSeriesReq != summary.Totals.Requests {
		t.Errorf("sum(series.Requests) = %d != Totals.Requests = %d", sumSeriesReq, summary.Totals.Requests)
	}
	if sumSeriesUnpriced != summary.Totals.UnpricedRequests {
		t.Errorf("sum(series.UnpricedRequests) = %d != Totals.UnpricedRequests = %d", sumSeriesUnpriced, summary.Totals.UnpricedRequests)
	}
	if !approx(sumSeriesCost, summary.Totals.CostUSD) {
		t.Errorf("sum(series.CostUSD) = %v != Totals.CostUSD = %v", sumSeriesCost, summary.Totals.CostUSD)
	}
}

// TestCostCachedHeavyCodexReasoning tests provider openai model "gpt-6.1-sol" with heavy cache reads,
// verifying uncached=400, read=669600, output=54, exact cost computation, and that reasoning is not double-counted.
func TestCostCachedHeavyCodexReasoning(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	model := "gpt-6.1-sol"
	prices := map[string]Price{
		model: {
			Input:         2.50,
			Output:        10.00,
			CacheRead:     0.25,
			CacheCreation: 1.25,
		},
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = prices
	})
	s.setNow(clk.now)

	rec := pluginapi.UsageRecord{
		RequestID:   "codex-sol-rec-1",
		Provider:    "openai",
		Model:       model,
		RequestedAt: clk.now().Add(15 * time.Second),
		Detail: pluginapi.UsageDetail{
			InputTokens:     670000,
			CacheReadTokens: 669600,
			OutputTokens:    54,
			ReasoningTokens: 20,
		},
	}

	r := NormalizeUsage(rec)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never set CostUSD, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("AccountingQuality = %q, want complete", r.AccountingQuality)
	}
	if r.UncachedInputTokens != 400 {
		t.Fatalf("UncachedInputTokens = %d, want 400 (670000 - 669600)", r.UncachedInputTokens)
	}
	if r.CacheReadTokens != 669600 {
		t.Fatalf("CacheReadTokens = %d, want 669600", r.CacheReadTokens)
	}
	if r.CacheCreationTokens != 0 {
		t.Fatalf("CacheCreationTokens = %d, want 0", r.CacheCreationTokens)
	}
	if r.OutputTokens != 54 {
		t.Fatalf("OutputTokens = %d, want 54", r.OutputTokens)
	}
	if r.ReasoningTokens != 20 {
		t.Fatalf("ReasoningTokens = %d, want 20", r.ReasoningTokens)
	}

	wantCost := (400.0*2.50 + 669600.0*0.25 + 0.0*1.25 + 54.0*10.00) / 1_000_000
	cost := RequestCost(r, prices)
	if cost == nil {
		t.Fatalf("RequestCost returned nil")
	}
	if !approx(*cost, wantCost) {
		t.Fatalf("RequestCost = %v, want %v", *cost, wantCost)
	}
	doubleCounted := (400.0*2.50 + 669600.0*0.25 + (54.0+20.0)*10.00) / 1_000_000
	if approx(*cost, doubleCounted) {
		t.Fatalf("RequestCost double-counted reasoning tokens: %v", *cost)
	}

	if !s.SubmitUsage(rec) {
		t.Fatalf("SubmitUsage rejected")
	}
	flushAll(t, s)

	from := clk.now()
	to := from.Add(time.Minute)
	page, err := s.Requests(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(page.Items))
	}
	stored := page.Items[0]
	if stored.UncachedInputTokens != 400 || stored.CacheReadTokens != 669600 || stored.OutputTokens != 54 {
		t.Fatalf("stored tokens mismatch: %+v", stored)
	}
	if stored.CostUSD == nil || !approx(*stored.CostUSD, wantCost) {
		t.Fatalf("stored CostUSD = %v, want %v", stored.CostUSD, wantCost)
	}

	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 1 || summary.Totals.UnpricedRequests != 0 {
		t.Fatalf("Summary Totals req/unpriced = %d/%d, want 1/0", summary.Totals.Requests, summary.Totals.UnpricedRequests)
	}
	if !approx(summary.Totals.CostUSD, wantCost) {
		t.Fatalf("Summary Totals CostUSD = %v, want %v", summary.Totals.CostUSD, wantCost)
	}
}

// TestPricingAliasMismatchAndManyModels tests that alias-only prices return nil,
// that co-existing alias and exact prices select the exact id, and that 200 model identities stay intact.
func TestPricingAliasMismatchAndManyModels(t *testing.T) {
	recAlias := pluginapi.UsageRecord{
		RequestID: "alias-rec-1",
		Provider:  "openai",
		Model:     "full-org/model-exact-name",
		Alias:     "model-alias-name",
		Detail:    simpleUsage(1000, 500),
	}
	rAlias := NormalizeUsage(recAlias)
	aliasOnlyPrices := map[string]Price{
		"model-alias-name": {Input: 5.0, Output: 10.0},
	}
	if cost := RequestCost(rAlias, aliasOnlyPrices); cost != nil {
		t.Fatalf("alias only price must return nil, got %v", *cost)
	}

	bothPrices := map[string]Price{
		"full-org/model-exact-name": {Input: 1.0, Output: 2.0},
		"model-alias-name":          {Input: 99.0, Output: 99.0},
	}
	exactCost := RequestCost(rAlias, bothPrices)
	wantExact := (1000.0*1.0 + 500.0*2.0) / 1_000_000
	if exactCost == nil || !approx(*exactCost, wantExact) {
		t.Fatalf("RequestCost with both must use exact model id: got %v, want %v", exactCost, wantExact)
	}
	wrongAliasCost := (1000.0*99.0 + 500.0*99.0) / 1_000_000
	if approx(*exactCost, wrongAliasCost) {
		t.Fatalf("RequestCost erroneously used alias price: %v", *exactCost)
	}

	const numModels = 200
	priceMap := make(map[string]Price, numModels)
	for i := 0; i < numModels; i++ {
		modelID := fmt.Sprintf("vendor-%02d/family-%03d-suffix-%d", i%7, i, i*13)
		priceMap[modelID] = Price{
			Input:  float64(i + 1),
			Output: float64((i + 1) * 2),
		}
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = priceMap
	})
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	s.setNow(clk.now)

	at := clk.now().Add(5 * time.Second)
	for i := 0; i < numModels; i++ {
		modelID := fmt.Sprintf("vendor-%02d/family-%03d-suffix-%d", i%7, i, i*13)
		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-model-%d", i),
			"openai",
			modelID,
			at,
			simpleUsage(1000, 500),
		))
	}
	flushAll(t, s)

	page, err := s.Requests(Query{
		From:  clk.now(),
		To:    clk.now().Add(time.Minute),
		Limit: maxPageLimit,
	})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != numModels {
		t.Fatalf("got %d items, want %d", len(page.Items), numModels)
	}
	seenModels := make(map[string]bool)
	for _, it := range page.Items {
		seenModels[it.Model] = true
		p, ok := priceMap[it.Model]
		if !ok {
			t.Fatalf("unknown model returned: %s", it.Model)
		}
		want := (1000.0*p.Input + 500.0*p.Output) / 1_000_000
		if it.CostUSD == nil || !approx(*it.CostUSD, want) {
			t.Fatalf("model %s CostUSD = %v, want %v", it.Model, it.CostUSD, want)
		}
	}
	if len(seenModels) != numModels {
		t.Fatalf("distinct model identities preserved = %d, want %d", len(seenModels), numModels)
	}
}

// TestPricingNonfiniteAndPoisonGuards tests that nonfinite prices (NaN, +Inf, -Inf, negative, product overflow)
// fail closed to nil, and that summaries are not poisoned by pathological price configurations.
func TestPricingNonfiniteAndPoisonGuards(t *testing.T) {
	rec := pluginapi.UsageRecord{
		RequestID: "nonfinite-rec-1",
		Provider:  "openai",
		Model:     "target-model",
		Detail:    simpleUsage(1000, 500),
	}
	r := NormalizeUsage(rec)

	badCases := []struct {
		name  string
		price Price
	}{
		{"NaN input", Price{Input: math.NaN(), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"NaN output", Price{Input: 1, Output: math.NaN(), CacheRead: 1, CacheCreation: 1}},
		{"NaN cache_read", Price{Input: 1, Output: 1, CacheRead: math.NaN(), CacheCreation: 1}},
		{"NaN cache_creation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.NaN()}},
		{"+Inf input", Price{Input: math.Inf(1), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"+Inf output", Price{Input: 1, Output: math.Inf(1), CacheRead: 1, CacheCreation: 1}},
		{"+Inf cache_read", Price{Input: 1, Output: 1, CacheRead: math.Inf(1), CacheCreation: 1}},
		{"+Inf cache_creation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.Inf(1)}},
		{"-Inf input", Price{Input: math.Inf(-1), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"-Inf output", Price{Input: 1, Output: math.Inf(-1), CacheRead: 1, CacheCreation: 1}},
		{"-Inf cache_read", Price{Input: 1, Output: 1, CacheRead: math.Inf(-1), CacheCreation: 1}},
		{"-Inf cache_creation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.Inf(-1)}},
		{"Negative input", Price{Input: -0.01, Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"Negative output", Price{Input: 1, Output: -0.01, CacheRead: 1, CacheCreation: 1}},
		{"Negative cache_read", Price{Input: 1, Output: 1, CacheRead: -0.01, CacheCreation: 1}},
		{"Negative cache_creation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: -0.01}},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			prices := map[string]Price{"target-model": tc.price}
			if c := RequestCost(r, prices); c != nil {
				t.Fatalf("%s must yield nil cost, got %v", tc.name, *c)
			}
		})
	}

	overflowPrice := Price{Input: 1e308, Output: 1}
	if c := RequestCost(r, map[string]Price{"target-model": overflowPrice}); c != nil {
		t.Fatalf("pathological overflow must yield nil cost, got %v", *c)
	}

	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	storePrices := map[string]Price{
		"normal-model":    {Input: 10, Output: 20},
		"bad-price-model": {Input: 1e308, Output: 1},
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = storePrices
	})
	s.setNow(clk.now)

	at := clk.now().Add(10 * time.Second)
	s.SubmitUsage(usageRecord("good-req", "openai", "normal-model", at, simpleUsage(1000, 500)))
	s.SubmitUsage(usageRecord("bad-req", "openai", "bad-price-model", at, simpleUsage(1000, 500)))
	flushAll(t, s)

	summary, err := s.Summary(Query{From: clk.now(), To: clk.now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if math.IsNaN(summary.Totals.CostUSD) || math.IsInf(summary.Totals.CostUSD, 0) {
		t.Fatalf("Summary.Totals.CostUSD was poisoned: %v", summary.Totals.CostUSD)
	}
	if !approx(summary.Totals.CostUSD, 0.02) {
		t.Fatalf("Summary.Totals.CostUSD = %v, want 0.02", summary.Totals.CostUSD)
	}
	if summary.Totals.Requests != 2 {
		t.Fatalf("Totals.Requests = %d, want 2", summary.Totals.Requests)
	}
	if summary.Totals.UnpricedRequests != 1 {
		t.Fatalf("Totals.UnpricedRequests = %d, want 1", summary.Totals.UnpricedRequests)
	}
}

// TestSummaryAggregatesAfterDetailPurge verifies that stats aggregates bill using currently effective prices
// even after raw requests have expired and been physically purged from disk.
func TestSummaryAggregatesAfterDetailPurge(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	dbPath := filepath.Join(t.TempDir(), "observer.db")
	cfg := Config{
		DataPath:           dbPath,
		StatsRetentionDays: 30,
		RequestRetention:   time.Minute,
		BodyRetention:      time.Minute,
		FlushInterval:      10 * time.Millisecond,
		Prices: map[string]Price{
			"model-keep-stats": {Input: 10, Output: 20},
		},
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.setNow(clk.now)

	from := clk.now()
	at := from.Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		s.SubmitUsage(usageRecord(fmt.Sprintf("purge-req-%d", i), "openai", "model-keep-stats", at, simpleUsage(1000, 500)))
	}
	flushAll(t, s)

	if got := bucketCount(t, s, bucketRequests); got != 5 {
		t.Fatalf("bucketRequests count = %d, want 5", got)
	}

	clk.advance(2 * time.Minute)
	if err := s.runCleanup(); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}

	if got := bucketCount(t, s, bucketRequests); got != 0 {
		t.Fatalf("bucketRequests count after cleanup = %d, want 0", got)
	}
	page, err := s.Requests(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Requests query: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("Requests returned %d items, want 0 after purge", len(page.Items))
	}

	summary, err := s.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary query: %v", err)
	}
	if summary.Totals.Requests != 5 {
		t.Fatalf("Summary.Totals.Requests = %d, want 5", summary.Totals.Requests)
	}
	if summary.Totals.UnpricedRequests != 0 {
		t.Fatalf("Summary.Totals.UnpricedRequests = %d, want 0", summary.Totals.UnpricedRequests)
	}
	if !approx(summary.Totals.CostUSD, 0.10) {
		t.Fatalf("Summary.Totals.CostUSD = %v, want 0.10", summary.Totals.CostUSD)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg.Prices = map[string]Price{
		"model-keep-stats": {Input: 20, Output: 40},
	}
	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	s2.setNow(clk.now)

	summary2, err := s2.Summary(Query{From: from, To: from.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary after reopen: %v", err)
	}
	if !approx(summary2.Totals.CostUSD, 0.20) {
		t.Fatalf("Summary after reopen CostUSD = %v, want 0.20", summary2.Totals.CostUSD)
	}
}

// TestSummaryConsistencyMultiMinuteMultiModel verifies subtotals, series, and groups consistency
// across multiple minutes and multiple models with diverse accounting qualities.
func TestSummaryConsistencyMultiMinuteMultiModel(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	prices := map[string]Price{
		"model-a": {Input: 2.0, Output: 5.0, CacheRead: 0.5, CacheCreation: 1.0},
		"model-b": {Input: 1.5, Output: 3.0},
		"model-m": {Input: 4.0, Output: 8.0},
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = prices
	})
	s.setNow(clk.now)

	from := clk.now()
	const numMinutes = 6

	for m := 0; m < numMinutes; m++ {
		minuteTime := from.Add(time.Duration(m) * time.Minute)

		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-a-%d", m), "openai", "model-a", minuteTime.Add(5*time.Second),
			pluginapi.UsageDetail{
				InputTokens:     1000,
				CacheReadTokens: 600,
				OutputTokens:    200,
				ReasoningTokens: 50,
			},
		))
		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-b-%d", m), "anthropic", "model-b", minuteTime.Add(15*time.Second),
			simpleUsage(500, 300),
		))
		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-m-comp-%d", m), "openai", "model-m", minuteTime.Add(25*time.Second),
			simpleUsage(400, 200),
		))
		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-m-unkn-%d", m), "mystery", "model-m", minuteTime.Add(35*time.Second),
			simpleUsage(600, 400),
		))
		s.SubmitUsage(usageRecord(
			fmt.Sprintf("req-u-%d", m), "openai", "model-u", minuteTime.Add(45*time.Second),
			simpleUsage(100, 50),
		))
	}
	flushAll(t, s)

	to := from.Add(numMinutes * time.Minute)
	summary, err := s.Summary(Query{From: from, To: to})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	var (
		gRequests            uint64
		gFailedRequests      uint64
		gInputTokens         uint64
		gOutputTokens        uint64
		gReasoningTokens     uint64
		gCacheReadTokens     uint64
		gCacheCreationTokens uint64
		gTotalTokens         uint64
		gCacheHits           uint64
		gUnpriced            uint64
		gCostUSD             float64
	)
	for _, g := range summary.Groups {
		gRequests += g.Requests
		gFailedRequests += g.FailedRequests
		gInputTokens += g.InputTokens
		gOutputTokens += g.OutputTokens
		gReasoningTokens += g.ReasoningTokens
		gCacheReadTokens += g.CacheReadTokens
		gCacheCreationTokens += g.CacheCreationTokens
		gTotalTokens += g.TotalTokens
		gCacheHits += g.CacheHits
		gUnpriced += g.UnpricedRequests
		gCostUSD += g.CostUSD

		switch g.Model {
		case "model-a", "model-b":
			if g.UnpricedRequests != 0 {
				t.Errorf("group %s unpriced = %d, want 0", g.Model, g.UnpricedRequests)
			}
			if g.CostUSD <= 0 {
				t.Errorf("group %s cost = %v, want > 0", g.Model, g.CostUSD)
			}
		case "model-m":
			if g.Provider == "openai" {
				if g.UnpricedRequests != 0 {
					t.Errorf("group m openai unpriced = %d, want 0", g.UnpricedRequests)
				}
				if g.CostUSD <= 0 {
					t.Errorf("group m openai cost = %v, want > 0", g.CostUSD)
				}
			} else if g.Provider == "mystery" {
				if g.UnpricedRequests != g.Requests {
					t.Errorf("group m mystery unpriced = %d, want %d", g.UnpricedRequests, g.Requests)
				}
				if g.CostUSD != 0 {
					t.Errorf("group m mystery cost = %v, want 0", g.CostUSD)
				}
			}
		case "model-u":
			if g.UnpricedRequests != g.Requests {
				t.Errorf("group u unpriced = %d, want %d", g.UnpricedRequests, g.Requests)
			}
			if g.CostUSD != 0 {
				t.Errorf("group u cost = %v, want 0", g.CostUSD)
			}
		}
	}

	if summary.Totals.Requests != gRequests {
		t.Errorf("Totals.Requests = %d, Σgroups = %d", summary.Totals.Requests, gRequests)
	}
	if summary.Totals.FailedRequests != gFailedRequests {
		t.Errorf("Totals.FailedRequests = %d, Σgroups = %d", summary.Totals.FailedRequests, gFailedRequests)
	}
	if summary.Totals.InputTokens != gInputTokens {
		t.Errorf("Totals.InputTokens = %d, Σgroups = %d", summary.Totals.InputTokens, gInputTokens)
	}
	if summary.Totals.OutputTokens != gOutputTokens {
		t.Errorf("Totals.OutputTokens = %d, Σgroups = %d", summary.Totals.OutputTokens, gOutputTokens)
	}
	if summary.Totals.ReasoningTokens != gReasoningTokens {
		t.Errorf("Totals.ReasoningTokens = %d, Σgroups = %d", summary.Totals.ReasoningTokens, gReasoningTokens)
	}
	if summary.Totals.CacheReadTokens != gCacheReadTokens {
		t.Errorf("Totals.CacheReadTokens = %d, Σgroups = %d", summary.Totals.CacheReadTokens, gCacheReadTokens)
	}
	if summary.Totals.CacheCreationTokens != gCacheCreationTokens {
		t.Errorf("Totals.CacheCreationTokens = %d, Σgroups = %d", summary.Totals.CacheCreationTokens, gCacheCreationTokens)
	}
	if summary.Totals.TotalTokens != gTotalTokens {
		t.Errorf("Totals.TotalTokens = %d, Σgroups = %d", summary.Totals.TotalTokens, gTotalTokens)
	}
	if summary.Totals.CacheHits != gCacheHits {
		t.Errorf("Totals.CacheHits = %d, Σgroups = %d", summary.Totals.CacheHits, gCacheHits)
	}
	if summary.Totals.UnpricedRequests != gUnpriced {
		t.Errorf("Totals.UnpricedRequests = %d, Σgroups = %d", summary.Totals.UnpricedRequests, gUnpriced)
	}
	if !approx(summary.Totals.CostUSD, gCostUSD) {
		t.Errorf("Totals.CostUSD = %v, Σgroups = %v", summary.Totals.CostUSD, gCostUSD)
	}

	var (
		sRequests            uint64
		sFailedRequests      uint64
		sInputTokens         uint64
		sOutputTokens        uint64
		sReasoningTokens     uint64
		sCacheReadTokens     uint64
		sCacheCreationTokens uint64
		sTotalTokens         uint64
		sCacheHits           uint64
		sUnpriced            uint64
		sCostUSD             float64
	)
	for _, p := range summary.Series {
		sRequests += p.Requests
		sFailedRequests += p.FailedRequests
		sInputTokens += p.InputTokens
		sOutputTokens += p.OutputTokens
		sReasoningTokens += p.ReasoningTokens
		sCacheReadTokens += p.CacheReadTokens
		sCacheCreationTokens += p.CacheCreationTokens
		sTotalTokens += p.TotalTokens
		sCacheHits += p.CacheHits
		sUnpriced += p.UnpricedRequests
		sCostUSD += p.CostUSD
	}

	if summary.Totals.Requests != sRequests {
		t.Errorf("Totals.Requests = %d, Σseries = %d", summary.Totals.Requests, sRequests)
	}
	if summary.Totals.FailedRequests != sFailedRequests {
		t.Errorf("Totals.FailedRequests = %d, Σseries = %d", summary.Totals.FailedRequests, sFailedRequests)
	}
	if summary.Totals.InputTokens != sInputTokens {
		t.Errorf("Totals.InputTokens = %d, Σseries = %d", summary.Totals.InputTokens, sInputTokens)
	}
	if summary.Totals.OutputTokens != sOutputTokens {
		t.Errorf("Totals.OutputTokens = %d, Σseries = %d", summary.Totals.OutputTokens, sOutputTokens)
	}
	if summary.Totals.ReasoningTokens != sReasoningTokens {
		t.Errorf("Totals.ReasoningTokens = %d, Σseries = %d", summary.Totals.ReasoningTokens, sReasoningTokens)
	}
	if summary.Totals.CacheReadTokens != sCacheReadTokens {
		t.Errorf("Totals.CacheReadTokens = %d, Σseries = %d", summary.Totals.CacheReadTokens, sCacheReadTokens)
	}
	if summary.Totals.CacheCreationTokens != sCacheCreationTokens {
		t.Errorf("Totals.CacheCreationTokens = %d, Σseries = %d", summary.Totals.CacheCreationTokens, sCacheCreationTokens)
	}
	if summary.Totals.TotalTokens != sTotalTokens {
		t.Errorf("Totals.TotalTokens = %d, Σseries = %d", summary.Totals.TotalTokens, sTotalTokens)
	}
	if summary.Totals.CacheHits != sCacheHits {
		t.Errorf("Totals.CacheHits = %d, Σseries = %d", summary.Totals.CacheHits, sCacheHits)
	}
	if summary.Totals.UnpricedRequests != sUnpriced {
		t.Errorf("Totals.UnpricedRequests = %d, Σseries = %d", summary.Totals.UnpricedRequests, sUnpriced)
	}
	if !approx(summary.Totals.CostUSD, sCostUSD) {
		t.Errorf("Totals.CostUSD = %v, Σseries = %v", summary.Totals.CostUSD, sCostUSD)
	}
}

// TestPricingZeroCostVsUnpricedDistinction distinguishes configured zero cost (valid finite 0.0, not unpriced)
// from missing model price (unpriced, returns nil and increments unpriced requests count).
func TestPricingZeroCostVsUnpricedDistinction(t *testing.T) {
	clk := &clock{t: time.Now().UTC().Truncate(time.Minute)}
	prices := map[string]Price{
		"free-model": {Input: 0, Output: 0, CacheRead: 0, CacheCreation: 0},
	}
	s := openTestStore(t, func(c *Config) {
		c.Prices = prices
	})
	s.setNow(clk.now)

	at := clk.now().Add(10 * time.Second)
	s.SubmitUsage(usageRecord("r-free", "openai", "free-model", at, simpleUsage(1000, 500)))
	s.SubmitUsage(usageRecord("r-unpriced", "openai", "unpriced-model", at, simpleUsage(1000, 500)))
	flushAll(t, s)

	recFree := usageRecord("r-free", "openai", "free-model", at, simpleUsage(1000, 500))
	rFree := NormalizeUsage(recFree)
	costFree := RequestCost(rFree, prices)
	if costFree == nil {
		t.Fatalf("free-model RequestCost must not be nil")
	}
	if *costFree != 0.0 {
		t.Fatalf("free-model RequestCost = %v, want 0.0", *costFree)
	}

	recUnpriced := usageRecord("r-unpriced", "openai", "unpriced-model", at, simpleUsage(1000, 500))
	rUnpriced := NormalizeUsage(recUnpriced)
	costUnpriced := RequestCost(rUnpriced, prices)
	if costUnpriced != nil {
		t.Fatalf("unpriced-model RequestCost must be nil, got %v", *costUnpriced)
	}

	page, err := s.Requests(Query{From: clk.now(), To: clk.now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("page.Items len = %d, want 2", len(page.Items))
	}
	for _, it := range page.Items {
		if it.Model == "free-model" {
			if it.CostUSD == nil {
				t.Fatalf("free-model request CostUSD is nil, want 0.0")
			}
			if *it.CostUSD != 0.0 {
				t.Fatalf("free-model request CostUSD = %v, want 0.0", *it.CostUSD)
			}
		} else if it.Model == "unpriced-model" {
			if it.CostUSD != nil {
				t.Fatalf("unpriced-model request CostUSD = %v, want nil", *it.CostUSD)
			}
		}
	}

	summary, err := s.Summary(Query{From: clk.now(), To: clk.now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Totals.Requests != 2 {
		t.Fatalf("Totals.Requests = %d, want 2", summary.Totals.Requests)
	}
	if summary.Totals.UnpricedRequests != 1 {
		t.Fatalf("Totals.UnpricedRequests = %d, want 1 (zero price is valid, not unpriced)", summary.Totals.UnpricedRequests)
	}
	if summary.Totals.CostUSD != 0.0 {
		t.Fatalf("Totals.CostUSD = %v, want 0.0", summary.Totals.CostUSD)
	}

	for _, g := range summary.Groups {
		if g.Model == "free-model" {
			if g.UnpricedRequests != 0 {
				t.Fatalf("free-model group UnpricedRequests = %d, want 0", g.UnpricedRequests)
			}
			if g.CostUSD != 0.0 {
				t.Fatalf("free-model group CostUSD = %v, want 0.0", g.CostUSD)
			}
		} else if g.Model == "unpriced-model" {
			if g.UnpricedRequests != 1 {
				t.Fatalf("unpriced-model group UnpricedRequests = %d, want 1", g.UnpricedRequests)
			}
			if g.CostUSD != 0.0 {
				t.Fatalf("unpriced-model group CostUSD = %v, want 0.0", g.CostUSD)
			}
		}
	}
}

// TestSummaryCorruptStatsFailsLoudly verifies that a corrupt persisted stats
// record surfaces as an error instead of being silently dropped from summaries,
// preserving the "never silently lose history" guarantee.
func TestSummaryCorruptStatsFailsLoudly(t *testing.T) {
	s := openTestStore(t, nil)
	at := time.Now().Add(-time.Minute)
	if !s.SubmitUsage(usageRecord("c-1", "openai", "gpt-5", at, simpleUsage(10, 5))) {
		t.Fatalf("submit rejected")
	}
	flushAll(t, s)

	// Overwrite the persisted stats value with a length that is neither the
	// legacy nor the versioned layout.
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(statsKey(statsMinute(at), "openai", "gpt-5"), []byte("bad"))
	})
	if err != nil {
		t.Fatalf("corrupt stats: %v", err)
	}

	if _, err := s.Summary(Query{}); err == nil {
		t.Fatalf("Summary tolerated a corrupt stats record")
	}
}
