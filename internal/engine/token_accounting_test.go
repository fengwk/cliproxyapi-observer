package engine

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func usageRecord(provider string, d pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{RequestID: "execution", Provider: provider, Model: "gpt-exact", Detail: d}
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
			if native.AccountingQuality != string(usage.TokenAccountingQualityComplete) || native.Tokens != tc.want {
				t.Fatalf("native baseline = %+v", native)
			}
			for _, provider := range []string{"commandcode", "opencode-go", "custom-unlisted"} {
				r := normalize(usageRecord(provider, tc.raw))
				if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
					t.Fatalf("%s quality = %q, want complete", provider, r.AccountingQuality)
				}
				if r.Tokens != tc.want {
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
// without a read/write split remain unclassified. Known-provider inconsistent
// results are never reclassified.
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
	settings.Pricing = pricing.Config{Prices: []pricing.Price{{
		Model: "gpt-exact",
		Rates: pricing.Rates{Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000, CacheWrite: 1_000_000},
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
	if d.Record.Tokens != want || d.Record.CostMicroUSD != 30 || d.Record.PriceSource != "manual" ||
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
