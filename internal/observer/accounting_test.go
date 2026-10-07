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
	r := NormalizeUsage(record, map[string]Price{
		"gpt-5": {Input: 1, Output: 2, CacheRead: 0.5},
	})
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
	if r.CostUSD == nil || !approx(*r.CostUSD, 185.0/1_000_000) {
		t.Errorf("cost = %v, want 0.000185", r.CostUSD)
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
	r := NormalizeUsage(record, nil)
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
}

func TestNormalizeUsageGeminiSeparateReasoning(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID: "g1", Provider: "gemini", Model: "gemini-2.5-pro",
		RequestedAt: time.Unix(3000, 0).UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, CacheReadTokens: 40, OutputTokens: 50, ReasoningTokens: 20,
		},
	}
	r := NormalizeUsage(record, nil)
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
	r := NormalizeUsage(record, map[string]Price{"unknown-model": {Input: 1, Output: 1}})
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
	if r.CostUSD != nil {
		t.Errorf("unclassified cost must be nil, got %v", *r.CostUSD)
	}
	if r.TPS != nil {
		t.Errorf("unclassified tps must be nil, got %v", *r.TPS)
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
	r := NormalizeUsage(record, nil)
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
	r := NormalizeUsage(record, map[string]Price{})
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q", r.AccountingQuality)
	}
	if r.CostUSD != nil {
		t.Errorf("cost must be nil without a configured price")
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
		r := NormalizeUsage(record, prices)
		if r.Model != model {
			t.Fatalf("model identity changed: %q != %q", r.Model, model)
		}
		if r.CostUSD == nil {
			t.Fatalf("model %q not priced", model)
		}
		want := float64(i) * 1000 / 1_000_000
		if !approx(*r.CostUSD, want) {
			t.Fatalf("model %q cost = %v, want %v", model, *r.CostUSD, want)
		}
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
