package observer

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestNormalizeUsageKnownSubsetAccounting(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "r1", Provider: "openai", Model: "gpt-5",
		RequestedAt: time.Unix(1000, 0).UTC(), Latency: 2 * time.Second, TTFT: time.Second,
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, OutputTokens: 50, ReasoningTokens: 20, CacheReadTokens: 30,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q", r.AccountingQuality)
	}
	// Subset semantics: cache read sits inside input, reasoning inside output.
	if r.InputTokens != 100 || r.UncachedInputTokens != 70 || r.CacheReadTokens != 30 {
		t.Errorf("input breakdown = %+v", r)
	}
	if r.OutputTokens != 50 || r.ReasoningTokens != 20 {
		t.Errorf("output breakdown = %+v", r)
	}
	if r.TotalTokens != 150 {
		t.Errorf("total = %d", r.TotalTokens)
	}
	if !r.CacheHit {
		t.Errorf("cache hit not detected")
	}
	prices := map[string]Price{
		"gpt-5": {Input: 1, Output: 2, CacheRead: 0.5},
	}
	cost := RequestCost(r, prices)
	if cost == nil || !approx(*cost, 185.0/1_000_000) {
		t.Errorf("cost = %v, want 0.000185", cost)
	}
	if r.TPS == nil || !approx(*r.TPS, 50.0) {
		t.Errorf("tps = %v, want 50", r.TPS)
	}
	// Raw counters are preserved verbatim.
	if r.RawUsage.InputTokens != 100 || r.RawUsage.CacheReadTokens != 30 {
		t.Errorf("raw usage not preserved: %+v", r.RawUsage)
	}
}

func TestNormalizeUsageClaudeReadWriteIndependent(t *testing.T) {
	// Claude is independent: input is uncached input; cache read/write are
	// separate. CachedTokens must NOT be folded into cache read when a cache
	// creation count is present.
	record := pluginapi.UsageRecord{
		RequestID: "c1", Provider: "claude", Model: "claude-sonnet-4-5",
		RequestedAt: time.Unix(2000, 0).UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20,
			CachedTokens: 70, OutputTokens: 200, ReasoningTokens: 100,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q", r.AccountingQuality)
	}
	if r.InputTokens != 170 || r.UncachedInputTokens != 100 {
		t.Errorf("input total/uncached = %d/%d, want 170/100", r.InputTokens, r.UncachedInputTokens)
	}
	if r.CacheReadTokens != 50 {
		t.Errorf("cache read = %d, want 50 (CachedTokens must not override)", r.CacheReadTokens)
	}
	if r.CacheCreationTokens != 20 {
		t.Errorf("cache creation = %d", r.CacheCreationTokens)
	}
	if r.OutputTokens != 300 || r.ReasoningTokens != 100 {
		t.Errorf("output total/reasoning = %d/%d, want 300/100", r.OutputTokens, r.ReasoningTokens)
	}
	if r.TotalTokens != 470 {
		t.Errorf("total = %d, want 470", r.TotalTokens)
	}
	prices := map[string]Price{
		"claude-sonnet-4-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheCreation: 3.75},
	}
	cost := RequestCost(r, prices)
	wantCost := (100.0*3.0 + 50.0*0.3 + 20.0*3.75 + 300.0*15.0) / 1_000_000
	if cost == nil || !approx(*cost, wantCost) {
		t.Errorf("RequestCost = %v, want %v", cost, wantCost)
	}
}

func TestNormalizeUsageGeminiSeparateReasoning(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "g1", Provider: "gemini", Model: "gemini-2.5-pro",
		RequestedAt: time.Unix(3000, 0).UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, CacheReadTokens: 40, OutputTokens: 50, ReasoningTokens: 20,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q (want complete distinct reasoning)", r.AccountingQuality)
	}
	// Input total stays authoritative; cache read is carved out.
	if r.InputTokens != 100 || r.UncachedInputTokens != 60 || r.CacheReadTokens != 40 {
		t.Errorf("input breakdown = %+v", r)
	}
	// Reasoning is distinct and additive to non-reasoning output.
	if r.ReasoningTokens != 20 || r.OutputTokens != 70 {
		t.Errorf("output = %d reasoning = %d, want 70/20", r.OutputTokens, r.ReasoningTokens)
	}
	prices := map[string]Price{
		"gemini-2.5-pro": {Input: 1.25, Output: 5.0, CacheRead: 0.3},
	}
	cost := RequestCost(r, prices)
	wantCost := (60.0*1.25 + 40.0*0.3 + 70.0*5.0) / 1_000_000
	if cost == nil || !approx(*cost, wantCost) {
		t.Errorf("RequestCost = %v, want %v", cost, wantCost)
	}
}

func TestNormalizeUsageUnknownSemanticsExposesRawPositiveCounters(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "u1", Provider: "mystery-provider", Model: "unknown-model",
		RequestedAt: time.Unix(4000, 0).UTC(), Latency: time.Second, TTFT: 500 * time.Millisecond,
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, OutputTokens: 50, ReasoningTokens: 12,
			CacheReadTokens: 7, CacheCreationTokens: 3,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityUnclassified) {
		t.Fatalf("quality = %q, want unclassified", r.AccountingQuality)
	}
	// Authoritative total preserved; raw positive counters exposed for display;
	// reasoning overlap not guessed and the uncertain uncached bucket stays 0.
	if r.TotalTokens != 150 {
		t.Errorf("total = %d, want 150", r.TotalTokens)
	}
	if r.InputTokens != 100 || r.OutputTokens != 50 || r.ReasoningTokens != 12 {
		t.Errorf("raw display counters not exposed: %+v", r)
	}
	if r.CacheReadTokens != 7 || r.CacheCreationTokens != 3 {
		t.Errorf("raw cache counters not exposed: %+v", r)
	}
	if r.UncachedInputTokens != 0 {
		t.Errorf("uncertain uncached bucket = %d, want 0", r.UncachedInputTokens)
	}
	if !r.CacheHit {
		t.Errorf("explicit read field should mark a cache hit")
	}
	if r.RawUsage.InputTokens != 100 || r.RawUsage.OutputTokens != 50 {
		t.Errorf("raw counters not preserved: %+v", r.RawUsage)
	}
	if r.TPS != nil {
		t.Errorf("unclassified tps must be nil, got %v", *r.TPS)
	}
	// Unclassified accounting must yield nil cost even if price is provided.
	if cost := RequestCost(r, map[string]Price{"unknown-model": {Input: 1, Output: 1}}); cost != nil {
		t.Errorf("unclassified cost must be nil, got %v", *cost)
	}
}

func TestNormalizeUsageCacheHitUsesExplicitReadOnly(t *testing.T) {
	// CachedTokens (legacy) plus creation must not be treated as a cache read.
	record := pluginapi.UsageRecord{
		RequestID: "c2", Provider: "mystery-provider", Model: "m",
		RequestedAt: time.Unix(4100, 0).UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens: 10, OutputTokens: 5, CachedTokens: 40, CacheCreationTokens: 4,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.CacheHit {
		t.Errorf("legacy CachedTokens/creation must not set CacheHit: %+v", r)
	}
}

func TestNormalizeUsageUnpricedModelHasNoCost(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "p1", Provider: "openai", Model: "unpriced-model",
		RequestedAt: time.Unix(5000, 0).UTC(),
		Detail:      pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 10},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q", r.AccountingQuality)
	}
	if cost := RequestCost(r, map[string]Price{}); cost != nil {
		t.Errorf("cost must be nil without a configured price, got %v", *cost)
	}
}

// TestNormalizeUsagePreservesManyModelIdentities guarantees that 160+ distinct
// full model ids keep their identity and price exactly (no truncation).
func TestNormalizeUsagePreservesManyModelIdentities(t *testing.T) {
	const models = 200
	prices := make(map[string]Price, models)
	for i := 0; i < models; i++ {
		prices[fmt.Sprintf("vendor-%02d/model-family-%03d-20250101", i%7, i)] = Price{Input: float64(i), Output: 1}
	}
	for i := 0; i < models; i++ {
		model := fmt.Sprintf("vendor-%02d/model-family-%03d-20250101", i%7, i)
		record := pluginapi.UsageRecord{
			RequestID: fmt.Sprintf("m-%d", i), Provider: "openai", Model: model,
			RequestedAt: time.Unix(int64(10000+i), 0).UTC(),
			Detail:      pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 0},
		}
		r := NormalizeUsage(record)
		if r.CostUSD != nil {
			t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
		}
		if r.Model != model {
			t.Fatalf("model identity changed: %q != %q", r.Model, model)
		}
		cost := RequestCost(r, prices)
		if cost == nil {
			t.Fatalf("model %q not priced by RequestCost", model)
		}
		want := float64(i) * 1000 / 1_000_000
		if !approx(*cost, want) {
			t.Fatalf("model %q cost = %v, want %v", model, *cost, want)
		}
	}
}

// TestRequestCostCachedHeavyCodexReasoningAndAliases tests cached-heavy codex gpt-6.1-sol
// (670000 input, 669600 cache, 54 output), alias mismatch exact id, and ensures output reasoning
// is not double-counted.
func TestRequestCostCachedHeavyCodexReasoningAndAliases(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID:   "codex-sol-1",
		Provider:    "codex",
		Model:       "gpt-6.1-sol",
		Alias:       "gpt-6.1",
		RequestedAt: time.Unix(6000, 0).UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens:     670000,
			CacheReadTokens: 669600,
			OutputTokens:    54,
			ReasoningTokens: 20, // output already contains reasoning tokens; must not double-count
		},
	}
	r := NormalizeUsage(record)
	// NormalizeUsage must never compute cost
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}

	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q, want complete", r.AccountingQuality)
	}
	// Under codex subset accounting:
	// InputTokens: 670000, CacheReadTokens: 669600, UncachedInputTokens: 670000 - 669600 = 400
	// OutputTokens: 54, ReasoningTokens: 20
	// TotalTokens: 670000 + 54 = 670054
	if r.InputTokens != 670000 || r.CacheReadTokens != 669600 || r.UncachedInputTokens != 400 {
		t.Fatalf("input breakdown mismatch: %+v", r)
	}
	if r.OutputTokens != 54 || r.ReasoningTokens != 20 {
		t.Fatalf("output breakdown mismatch: %+v", r)
	}
	if r.TotalTokens != 670054 {
		t.Fatalf("total = %d, want 670054", r.TotalTokens)
	}
	if !r.CacheHit {
		t.Fatalf("CacheHit = false, want true")
	}

	// Price.Input / Output / CacheRead / CacheCreation per-million.
	// Output includes reasoning tokens (20); reasoning must not be double counted.
	prices := map[string]Price{
		"gpt-6.1-sol": {
			Input:         2.50,
			Output:        10.00,
			CacheRead:     0.25,
			CacheCreation: 1.25,
		},
	}
	cost := RequestCost(r, prices)
	if cost == nil {
		t.Fatalf("RequestCost returned nil for model %q", r.Model)
	}
	// (400 * 2.50 + 669600 * 0.25 + 0 * 1.25 + 54 * 10.00) / 1_000_000
	// = (1000 + 167400 + 0 + 540) / 1_000_000 = 168940 / 1_000_000 = 0.16894
	wantCost := (400.0*2.50 + 669600.0*0.25 + 54.0*10.00) / 1_000_000
	if !approx(*cost, wantCost) {
		t.Fatalf("RequestCost = %v, want %v", *cost, wantCost)
	}
	// Verify that reasoning tokens are not double-counted (e.g. 54 + 20)
	doubleCountCost := (400.0*2.50 + 669600.0*0.25 + (54.0+20.0)*10.00) / 1_000_000
	if approx(*cost, doubleCountCost) {
		t.Fatalf("RequestCost double-counted reasoning tokens: %v", *cost)
	}

	// Alias mismatch exact id:
	// 1. When prices only contains the alias "gpt-6.1", RequestCost must return nil because it queries exact r.Model.
	aliasOnlyPrices := map[string]Price{
		"gpt-6.1": {Input: 2.50, Output: 10.00, CacheRead: 0.25, CacheCreation: 1.25},
	}
	if aliasCost := RequestCost(r, aliasOnlyPrices); aliasCost != nil {
		t.Errorf("RequestCost with alias-only price must return nil, got %v", *aliasCost)
	}

	// 2. When both exact id and alias are configured with different prices, exact id must be used.
	bothPrices := map[string]Price{
		"gpt-6.1-sol": {Input: 2.50, Output: 10.00, CacheRead: 0.25, CacheCreation: 1.25},
		"gpt-6.1":     {Input: 99.0, Output: 99.0, CacheRead: 99.0, CacheCreation: 99.0},
	}
	exactCost := RequestCost(r, bothPrices)
	if exactCost == nil || !approx(*exactCost, wantCost) {
		t.Errorf("RequestCost must use exact model id instead of alias, got %v, want %v", exactCost, wantCost)
	}
}

// TestRequestCostSpecialPrices tests NaN/Inf/zero/missing price behavior across all Price fields.
func TestRequestCostSpecialPrices(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "s1", Provider: "openai", Model: "gpt-6.1-sol",
		Detail: pluginapi.UsageDetail{
			InputTokens: 670000, CacheReadTokens: 669600, OutputTokens: 54, ReasoningTokens: 20,
		},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}

	// Zero price: valid finite numbers, must yield 0.0 (not nil)
	zeroPrices := map[string]Price{
		"gpt-6.1-sol": {Input: 0, Output: 0, CacheRead: 0, CacheCreation: 0},
	}
	zeroCost := RequestCost(r, zeroPrices)
	if zeroCost == nil {
		t.Fatalf("RequestCost with zero price returned nil, want 0.0")
	}
	if *zeroCost != 0.0 {
		t.Errorf("RequestCost with zero price = %v, want 0.0", *zeroCost)
	}

	// Missing price: not configured in map, should return nil
	missingPrices := map[string]Price{
		"other-model": {Input: 1, Output: 1},
	}
	if cost := RequestCost(r, missingPrices); cost != nil {
		t.Errorf("RequestCost for missing model price must be nil, got %v", *cost)
	}
	if cost := RequestCost(r, map[string]Price{}); cost != nil {
		t.Errorf("RequestCost for empty prices map must be nil, got %v", *cost)
	}
	if cost := RequestCost(r, nil); cost != nil {
		t.Errorf("RequestCost for nil prices map must be nil, got %v", *cost)
	}

	// NaN, Inf (+Inf, -Inf), and negative prices across all fields: Input, Output, CacheRead, CacheCreation
	badPrices := []struct {
		name  string
		price Price
	}{
		{"NaN Input", Price{Input: math.NaN(), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"NaN Output", Price{Input: 1, Output: math.NaN(), CacheRead: 1, CacheCreation: 1}},
		{"NaN CacheRead", Price{Input: 1, Output: 1, CacheRead: math.NaN(), CacheCreation: 1}},
		{"NaN CacheCreation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.NaN()}},
		{"+Inf Input", Price{Input: math.Inf(1), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"+Inf Output", Price{Input: 1, Output: math.Inf(1), CacheRead: 1, CacheCreation: 1}},
		{"+Inf CacheRead", Price{Input: 1, Output: 1, CacheRead: math.Inf(1), CacheCreation: 1}},
		{"+Inf CacheCreation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.Inf(1)}},
		{"-Inf Input", Price{Input: math.Inf(-1), Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"-Inf Output", Price{Input: 1, Output: math.Inf(-1), CacheRead: 1, CacheCreation: 1}},
		{"-Inf CacheRead", Price{Input: 1, Output: 1, CacheRead: math.Inf(-1), CacheCreation: 1}},
		{"-Inf CacheCreation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: math.Inf(-1)}},
		{"Negative Input", Price{Input: -0.1, Output: 1, CacheRead: 1, CacheCreation: 1}},
		{"Negative Output", Price{Input: 1, Output: -0.1, CacheRead: 1, CacheCreation: 1}},
		{"Negative CacheRead", Price{Input: 1, Output: 1, CacheRead: -0.1, CacheCreation: 1}},
		{"Negative CacheCreation", Price{Input: 1, Output: 1, CacheRead: 1, CacheCreation: -0.1}},
	}
	for _, tc := range badPrices {
		t.Run(tc.name, func(t *testing.T) {
			prices := map[string]Price{"gpt-6.1-sol": tc.price}
			if cost := RequestCost(r, prices); cost != nil {
				t.Errorf("RequestCost for %s must be nil, got %v", tc.name, *cost)
			}
		})
	}
}

func TestGenerationNSAndTPSGuards(t *testing.T) {
	if got := generationNS(0, 0); got != 0 {
		t.Errorf("generationNS(0,0) = %d", got)
	}
	if got := generationNS(1000, 500); got != 500 {
		t.Errorf("generationNS = %d, want 500", got)
	}
	if got := generationNS(500, 1000); got != 0 {
		t.Errorf("TTFT after latency must yield 0, got %d", got)
	}
	complete := usage.TokenBreakdown{Quality: usage.TokenAccountingQualityComplete, Output: usage.TokenOutputBreakdown{TotalTokens: 10}}
	if tpsFor(complete, 0) != nil {
		t.Errorf("zero generation window must yield nil tps")
	}
}
