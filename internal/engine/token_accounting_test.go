package engine

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func usageRecord(provider string, d pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{RequestID: "execution", Provider: provider, Model: "gpt-exact", Detail: d}
}

// The native plugin bridge omits CPA's complete breakdown but preserves Claude's
// raw output, which already includes thinking. Rebuilding must not add it twice.
func TestClaudeOutputSubsetTokens(t *testing.T) {
	cases := []struct {
		name string
		raw  pluginapi.UsageDetail
		want pricing.Tokens
	}{
		{
			name: "cached-thinking",
			raw:  pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, ReasoningTokens: 40, CachedTokens: 44225, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302},
			want: pricing.Tokens{Input: 2, Output: 244, Reasoning: 40, CacheRead: 44225, CacheWrite: 831, Total: 45302},
		},
		{
			name: "uncached-thinking",
			raw:  pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1, TotalTokens: 9},
			want: pricing.Tokens{Input: 6, Output: 3, Reasoning: 1, Total: 9},
		},
		{
			name: "no-thinking-control",
			raw:  pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, CachedTokens: 44225, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302},
			want: pricing.Tokens{Input: 2, Output: 244, CacheRead: 44225, CacheWrite: 831, Total: 45302},
		},
		{
			name: "all-output-is-thinking",
			raw:  pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 3, TotalTokens: 9},
			want: pricing.Tokens{Input: 6, Output: 3, Reasoning: 3, Total: 9},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, executor := range []string{"ClaudeExecutor", ""} {
				u := usageRecord("claude", tc.raw)
				u.ExecutorType = executor
				r := normalize(u)
				if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) || !reflect.DeepEqual(r.Tokens, tc.want) {
					t.Fatalf("executor %q: quality=%q tokens=%+v, want complete %+v", executor, r.AccountingQuality, r.Tokens, tc.want)
				}
				if r.RawUsage != tc.raw || r.Provider != u.Provider || r.Model != u.Model {
					t.Fatalf("raw counters or identity changed: %+v", r)
				}
			}
		})
	}
}

// Without an explicit total, the adapter cannot prove the subset contract and
// must leave the SDK's existing interpretation alone.
func TestClaudeOutputSubsetRequiresExplicitTotal(t *testing.T) {
	d := pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1}
	if b, ok := nativeClaudeOutputSubset(d, "claude", "ClaudeExecutor"); ok {
		t.Fatalf("adapted without a reported total: %+v", b)
	}
}

// The native Claude adapter is not a general inconsistent-usage fallback:
// malformed counters and unrelated executors must keep the SDK classification.
func TestClaudeOutputSubsetSafety(t *testing.T) {
	cases := []struct {
		name, provider, executor string
		raw                      pluginapi.UsageDetail
		quality                  usage.TokenAccountingQuality
	}{
		{"total-mismatch", "claude", "ClaudeExecutor", pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1, TotalTokens: 8}, usage.TokenAccountingQualityInconsistent},
		{"thinking-exceeds-output", "claude", "ClaudeExecutor", pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 4, TotalTokens: 9}, usage.TokenAccountingQualityInconsistent},
		{"negative-legacy-cache", "claude", "ClaudeExecutor", pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1, CachedTokens: -1, TotalTokens: 9}, usage.TokenAccountingQualityInconsistent},
		{"ambiguous-legacy-cache", "claude", "ClaudeExecutor", pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1, CachedTokens: 4, TotalTokens: 9}, usage.TokenAccountingQualityInconsistent},
		{"overflow", "claude", "ClaudeExecutor", pluginapi.UsageDetail{InputTokens: math.MaxInt64, OutputTokens: 3, ReasoningTokens: 1, TotalTokens: math.MaxInt64}, usage.TokenAccountingQualityInconsistent},
		{"unknown-provider", "custom", "", pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, ReasoningTokens: 40, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302}, usage.TokenAccountingQualityUnclassified},
		{"misleading-provider", "custom-claude", "", pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, ReasoningTokens: 40, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302}, usage.TokenAccountingQualityInconsistent},
		{"compat-executor", "claude", "OpenAICompatExecutor", pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, ReasoningTokens: 40, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302}, usage.TokenAccountingQualityInconsistent},
		{"other-executor", "claude", "CustomExecutor", pluginapi.UsageDetail{InputTokens: 2, OutputTokens: 244, ReasoningTokens: 40, CacheReadTokens: 44225, CacheCreationTokens: 831, TotalTokens: 45302}, usage.TokenAccountingQualityInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := usageRecord(tc.provider, tc.raw)
			u.ExecutorType = tc.executor
			r := normalize(u)
			if r.AccountingQuality != string(tc.quality) || r.RawUsage != tc.raw || r.Tokens.Total != tc.raw.TotalTokens {
				t.Fatalf("quality/raw/total changed: %+v, want %s", r, tc.quality)
			}
			if r.Tokens.Input != 0 || r.Tokens.Output != 0 || r.Tokens.CacheRead != 0 || r.Tokens.CacheWrite != 0 || r.Tokens.Reasoning != 0 {
				t.Fatalf("invented partition: %+v", r.Tokens)
			}
		})
	}
}

// Custom provider names are not token-semantic markers for the pinned SDK.
// When the raw counters and explicit total prove a canonical partition, the
// recovered buckets must match the native provider with the same semantics.
func TestCustomProviderCanonicalTokens(t *testing.T) {
	cases := []struct {
		name   string
		native string
		raw    pluginapi.UsageDetail
		want   pricing.Tokens
	}{
		{
			name:   "subset",
			native: "openai",
			raw:    pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 3, ReasoningTokens: 1, CacheReadTokens: 4, TotalTokens: 13},
			want:   pricing.Tokens{Input: 6, Output: 3, CacheRead: 4, Reasoning: 1, Total: 13},
		},
		{
			// Subset and separate-reasoning constructors coincide when reasoning
			// is absent, which is acceptable because the canonical result is equal.
			name:   "subset-cache-write",
			native: "openai",
			raw:    pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4, CacheCreationTokens: 2, TotalTokens: 13},
			want:   pricing.Tokens{Input: 4, Output: 3, CacheRead: 4, CacheWrite: 2, Total: 13},
		},
		{
			name:   "independent",
			native: "claude",
			raw:    pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 1, CacheReadTokens: 4, TotalTokens: 14},
			want:   pricing.Tokens{Input: 6, Output: 4, CacheRead: 4, Reasoning: 1, Total: 14},
		},
		{
			name:   "separate-reasoning",
			native: "gemini",
			raw:    pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 3, ReasoningTokens: 1, CacheReadTokens: 4, TotalTokens: 14},
			want:   pricing.Tokens{Input: 6, Output: 4, CacheRead: 4, Reasoning: 1, Total: 14},
		},
		{
			name:   "no-auxiliary-buckets",
			native: "openai",
			raw:    pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, TotalTokens: 10},
			want:   pricing.Tokens{Input: 8, Output: 2, Total: 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			native := normalize(usageRecord(tc.native, tc.raw))
			if native.AccountingQuality != string(usage.TokenAccountingQualityComplete) || !reflect.DeepEqual(native.Tokens, tc.want) {
				t.Fatalf("native baseline = %+v", native)
			}
			for _, provider := range []string{"commandcode", "opencode-go", "custom-unlisted"} {
				r := normalize(usageRecord(provider, tc.raw))
				if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
					t.Fatalf("%s quality = %q, want complete", provider, r.AccountingQuality)
				}
				if !reflect.DeepEqual(r.Tokens, tc.want) {
					t.Fatalf("%s tokens = %+v, want %+v", provider, r.Tokens, tc.want)
				}
				if r.Provider != provider || r.Model != "gpt-exact" || r.RawUsage != tc.raw {
					t.Fatalf("%s identity changed: %+v", provider, r)
				}
			}
		})
	}
}

// Recovery is deliberately conservative: authoritative totals alone, missing
// totals, malformed counters, unsupported totals, and legacy cached counters
// without a read/write split remain unclassified. Known-provider inconsistencies
// outside the native Claude bridge adaptation are never reclassified.
func TestCustomProviderCanonicalFallbackSafety(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		raw      pluginapi.UsageDetail
		quality  usage.TokenAccountingQuality
	}{
		{"total-only", "commandcode", pluginapi.UsageDetail{TotalTokens: 10}, usage.TokenAccountingQualityUnclassified},
		{"no-explicit-total", "opencode-go", pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, CacheReadTokens: 4}, usage.TokenAccountingQualityUnclassified},
		{"negative-counter", "commandcode", pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, CacheReadTokens: -1, TotalTokens: 10}, usage.TokenAccountingQualityUnclassified},
		{"overflow", "opencode-go", pluginapi.UsageDetail{InputTokens: math.MaxInt64, OutputTokens: 1, TotalTokens: math.MaxInt64}, usage.TokenAccountingQualityUnclassified},
		{"cache-exceeds-input-unsupported-total", "commandcode", pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 10, TotalTokens: 7}, usage.TokenAccountingQualityUnclassified},
		{"legacy-cached-without-split", "opencode-go", pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, CachedTokens: 4, TotalTokens: 10}, usage.TokenAccountingQualityUnclassified},
		{"total-mismatch", "commandcode", pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, TotalTokens: 9}, usage.TokenAccountingQualityUnclassified},
		{"sdk-inconsistent", "openai", pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 10, TotalTokens: 7}, usage.TokenAccountingQualityInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := normalize(usageRecord(tc.provider, tc.raw))
			if r.AccountingQuality != string(tc.quality) {
				t.Fatalf("quality = %q, want %q", r.AccountingQuality, tc.quality)
			}
			if r.Tokens.Input != 0 || r.Tokens.Output != 0 || r.Tokens.CacheRead != 0 ||
				r.Tokens.CacheWrite != 0 || r.Tokens.Reasoning != 0 {
				t.Fatalf("synthetic detailed buckets: %+v", r.Tokens)
			}
		})
	}
}

// Existing events must quote canonically recovered buckets, charge the Key and
// land in the detailed rollups. Manual rates keep this independent of network
// catalog state. The cache-hit denominator is uncached input + reads + writes.
func TestCustomProviderUsageChargesCanonicalRollup(t *testing.T) {
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	pricer, e := pricing.NewService(pricing.Config{}, filepath.Join(t.TempDir(), "pricing.json"), nil)
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(Options{DataPath: filepath.Join(t.TempDir(), "engine.db"), Now: func() time.Time { return now }, Pricer: pricer})
	if e != nil {
		t.Fatal(e)
	}
	settings, e := s.Settings()
	must(t, e)
	settings.Pricing = pricing.Config{Prices: []pricing.ManualPrice{{
		Model: "gpt-exact", Provider: "commandcode",
		Definition: pricing.PricingDefinition{Base: pricing.RateSet{
			Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000, CacheWrite: 1_000_000,
		}},
	}}}
	_, e = s.UpdateSettings(settings)
	must(t, e)
	t.Cleanup(func() {
		if !s.closed {
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}
	})
	k, _ := key(t, s, KeySpec{Enabled: true})

	u := event(k, "custom", now)
	u.Provider = "commandcode"
	u.Detail = pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 4, CacheCreationTokens: 2, TotalTokens: 13}
	must(t, s.RecordUsage(u))

	// 4 uncached input * 2 + 3 output * 6 + 4 read * 0.5 + 2 write * 1 = 30.
	want := pricing.Tokens{Input: 4, Output: 3, CacheRead: 4, CacheWrite: 2, Total: 13}
	d, e := s.Detail(u.RequestID)
	must(t, e)
	if !reflect.DeepEqual(d.Record.Tokens, want) || d.Record.CostMicroUSD != 30 || d.Record.PriceSource != "manual" ||
		d.Record.AccountingQuality != string(usage.TokenAccountingQualityComplete) || d.Record.RawUsage != u.Detail {
		t.Fatalf("record = %+v", d.Record)
	}
	if fullInput := want.Input + want.CacheRead + want.CacheWrite; fullInput != 10 {
		t.Fatalf("cache-hit denominator = %d, want original input 10", fullInput)
	}
	if got := total(t, s).Used.Total; got != 30 {
		t.Fatalf("charged = %d, want 30", got)
	}
	summary, e := s.Summary(Query{From: day(now), To: day(now).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 || summary.CompleteRequests != 1 || summary.CostMicroUSD != 30 ||
		summary.Tokens != 13 || summary.InputTokens != 4 || summary.OutputTokens != 3 ||
		summary.CacheReadTokens != 4 || summary.CacheWriteTokens != 2 || summary.ReasoningTokens != 0 {
		t.Fatalf("summary = %+v", summary.Aggregate)
	}
}
