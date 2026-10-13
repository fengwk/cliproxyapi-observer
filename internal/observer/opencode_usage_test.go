package observer

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func openCodeRecord(model string, detail pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID:    "oc",
		Provider:     openCodeGoProvider,
		Model:        model,
		ExecutorType: openCodeGoExecutor,
		RequestedAt:  time.Unix(1700, 0).UTC(),
		Latency:      2 * time.Second,
		TTFT:         time.Second,
		Detail:       detail,
	}
}

// A valid OpenAI/Responses subset record (cache inside input, reasoning inside
// output) must be recovered to complete accounting with correct pricing.
func TestOpenCodeSubsetAccountingRecovered(t *testing.T) {
	const model = "opencode-go/glm-5.2"
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 2, CacheRead: 0.1}}
	})
	at := time.Now().UTC().Add(-time.Minute)
	for i, stream := range []bool{false, true} {
		id := []string{"opencode-sync", "opencode-stream"}[i]
		if !s.SubmitUsage(pluginapi.UsageRecord{
			RequestID: id, Provider: openCodeGoProvider, Model: model,
			ExecutorType: openCodeGoExecutor, RequestedAt: at, Stream: stream,
			Latency: time.Second, TTFT: 500 * time.Millisecond,
			Detail: pluginapi.UsageDetail{
				InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8,
			},
		}) {
			t.Fatal("OpenCode usage rejected")
		}
	}
	flushAll(t, s)

	query := Query{Provider: openCodeGoProvider, Model: model}
	page, err := s.Requests(query)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("requests: count=%d, err=%v", len(page.Items), err)
	}
	for _, r := range page.Items {
		if r.AccountingQuality != "complete" {
			t.Fatalf("quality = %q, want complete: %+v", r.AccountingQuality, r)
		}
		// Cache read is carved out of the authoritative input total.
		if r.InputTokens != 5 || r.UncachedInputTokens != 3 || r.CacheReadTokens != 2 ||
			r.OutputTokens != 3 || r.ReasoningTokens != 0 || r.TotalTokens != 8 {
			t.Fatalf("recovered buckets wrong: %+v", r)
		}
		if r.TPS == nil {
			t.Fatalf("TPS not restored: %+v", r)
		}
		// Raw counters and identity are preserved.
		if r.RawUsage.InputTokens != 5 || r.RawUsage.CacheReadTokens != 2 ||
			r.Provider != openCodeGoProvider || r.Executor != openCodeGoExecutor {
			t.Fatalf("raw/identity changed: %+v", r)
		}
	}
	// (3*1 + 2*0.1 + 3*2) / 1e6
	wantCost := 9.2 / 1_000_000
	summary, err := s.Summary(query)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(summary.Totals.CostUSD, 2*wantCost) || summary.Totals.UnpricedRequests != 0 {
		t.Fatalf("summary pricing wrong: %+v", summary.Totals)
	}
	// The plugin ID must not alias the upstream provider key.
	other, err := s.Requests(Query{Provider: "cliproxyapi-opencode-provider"})
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("plugin ID incorrectly matched provider ID: %+v, err=%v", other, err)
	}
}

// A valid Anthropic Messages record (input excludes cache, raw output still
// contains reasoning) must be recovered as independent accounting: reasoning is
// not added to output and the input total includes the cache buckets.
func TestOpenCodeAnthropicIndependentRecovered(t *testing.T) {
	const model = "opencode-go/claude-sonnet-4-5"
	record := openCodeRecord(model, pluginapi.UsageDetail{
		InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20,
		OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370,
	})
	r := NormalizeUsage(record)
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q, want complete", r.AccountingQuality)
	}
	if r.InputTokens != 170 || r.UncachedInputTokens != 100 ||
		r.CacheReadTokens != 50 || r.CacheCreationTokens != 20 {
		t.Fatalf("input buckets wrong: %+v", r)
	}
	if r.OutputTokens != 200 || r.ReasoningTokens != 100 {
		t.Fatalf("output buckets wrong: %+v", r)
	}
	if r.TotalTokens != 370 {
		t.Fatalf("total = %d, want 370", r.TotalTokens)
	}
	if r.RawUsage.OutputTokens != 200 || r.RawUsage.InputTokens != 100 {
		t.Fatalf("raw usage changed: %+v", r.RawUsage)
	}
	// (100*3 + 50*0.3 + 20*3.75 + 200*15) / 1e6
	cost := RequestCost(r, map[string]Price{model: {Input: 3, Output: 15, CacheRead: 0.3, CacheCreation: 3.75}})
	if cost == nil || !approx(*cost, 3390.0/1_000_000) {
		t.Fatalf("cost = %v, want 3.39e-3", cost)
	}
}

// With no cache and no reasoning both interpretations coincide, so the record is
// uniquely determined and recovered.
func TestOpenCodeNoCacheConsensusRecovered(t *testing.T) {
	r := NormalizeUsage(openCodeRecord("opencode-go/glm-5.2", pluginapi.UsageDetail{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
	}))
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q, want complete", r.AccountingQuality)
	}
	if r.InputTokens != 10 || r.UncachedInputTokens != 10 || r.OutputTokens != 5 || r.TotalTokens != 15 {
		t.Fatalf("buckets wrong: %+v", r)
	}
}

// A failed request with zero usage must never be repaired into invented tokens.
func TestOpenCodeFailedZeroUsageNotInvented(t *testing.T) {
	record := openCodeRecord("opencode-go/glm-5.2", pluginapi.UsageDetail{})
	record.Failed = true
	record.Failure.StatusCode = 500
	r := NormalizeUsage(record)
	if r.InputTokens != 0 || r.OutputTokens != 0 || r.TotalTokens != 0 ||
		r.UncachedInputTokens != 0 || r.CacheReadTokens != 0 || r.CacheCreationTokens != 0 {
		t.Fatalf("zero usage invented tokens: %+v", r)
	}
}

// Invalid, contradictory or ambiguous raw counters must stay unclassified with
// the raw display counters preserved, never silently zeroed or repriced.
func TestOpenCodeAccountingRejectsAmbiguousOrInvalid(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		executor     string
		model        string
		detail       pluginapi.UsageDetail
		wantComplete bool
	}{
		{
			name: "missing_total", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 5, CacheReadTokens: 10},
		},
		{
			name: "contradictory_total", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, TotalTokens: 999},
		},
		{
			name: "alias_unknown", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, CachedTokens: 40, CacheReadTokens: 10, TotalTokens: 150},
		},
		{
			name: "alias_only", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, CachedTokens: 40, TotalTokens: 150},
		},
		{
			name: "reasoning_exceeds_output", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 60, TotalTokens: 150},
		},
		{
			name: "negative_counter", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: -5, OutputTokens: 50, TotalTokens: 150},
		},
		{
			name: "overflow_total", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64, TotalTokens: math.MaxInt64},
		},
		{
			name: "unknown_executor", provider: openCodeGoProvider, executor: "otherExecutor", model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8},
		},
		{
			name: "outside_namespace", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "glm-5.2",
			detail: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8},
		},
		{
			// A bare prefix has no native model id and is never a target.
			name: "bare_namespace", provider: openCodeGoProvider, executor: openCodeGoExecutor, model: "opencode-go/",
			detail: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8},
		},
		{
			// A known provider is never reinterpreted by the observer.
			name: "other_provider_unchanged", provider: "mystery-provider", executor: openCodeGoExecutor, model: "opencode-go/m",
			detail: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NormalizeUsage(pluginapi.UsageRecord{
				RequestID: "x", Provider: tc.provider, ExecutorType: tc.executor,
				Model: tc.model, RequestedAt: time.Unix(1000, 0).UTC(), Detail: tc.detail,
			})
			if tc.wantComplete {
				if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
					t.Fatalf("quality = %q, want complete", r.AccountingQuality)
				}
				return
			}
			if r.AccountingQuality == string(usage.TokenAccountingQualityComplete) {
				t.Fatalf("record wrongly classified: %+v", r)
			}
			// Unpriced records keep their raw positive counters for display.
			if r.InputTokens != positive(tc.detail.InputTokens) {
				t.Fatalf("raw input not preserved: got %d, want %d", r.InputTokens, positive(tc.detail.InputTokens))
			}
			if cost := RequestCost(r, map[string]Price{tc.model: {Input: 1, Output: 1}}); cost != nil {
				t.Fatalf("unclassified record repriced: %v", *cost)
			}
		})
	}
}

// A known provider record must keep the SDK's accounting untouched.
func TestOpenCodeOtherProviderUnaffected(t *testing.T) {
	r := NormalizeUsage(pluginapi.UsageRecord{
		RequestID: "p", Provider: "openai", ExecutorType: "openaiCompatExecutor",
		Model: "gpt-5", RequestedAt: time.Unix(1000, 0).UTC(),
		Detail: pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 30},
	})
	if r.AccountingQuality != string(usage.TokenAccountingQualityComplete) {
		t.Fatalf("quality = %q", r.AccountingQuality)
	}
	if r.InputTokens != 100 || r.UncachedInputTokens != 70 || r.CacheReadTokens != 30 {
		t.Fatalf("subset accounting changed: %+v", r)
	}
}

// isAdapterCompatTarget must stay narrow: exact provider, the shared executor,
// and a non-empty native id inside each cohort's own namespace.
func TestOpenCodeCompatTargetIsolation(t *testing.T) {
	cases := []struct {
		provider, executor, model string
		want                      bool
	}{
		{openCodeGoProvider, openCodeGoExecutor, "opencode-go/glm-5.2", true},
		{openCodeGoProvider, openCodeGoExecutor, "opencode-go/", false},
		{openCodeGoProvider, openCodeGoExecutor, "opencode-go", false},
		{openCodeGoProvider, openCodeGoExecutor, "other/glm", false},
		{openCodeGoProvider, "executor", "opencode-go/glm", false},
		{openCodeGoExecutor, openCodeGoExecutor, "opencode-go/glm", false},
		{"opencode-go-x", openCodeGoExecutor, "opencode-go/glm", false},
		{commandCodeProvider, adapterExecutor, "commandcode/deepseek/deepseek-v4.1-flash", true},
		{commandCodeProvider, adapterExecutor, "commandcode/", false},
		{commandCodeProvider, adapterExecutor, "opencode-go/glm", false},
		{openCodeGoProvider, adapterExecutor, "commandcode/glm", false},
		{commandCodeProvider, "executor", "commandcode/glm", false},
	}
	for _, tc := range cases {
		if got := isAdapterCompatTarget(tc.provider, tc.executor, tc.model); got != tc.want {
			t.Errorf("isAdapterCompatTarget(%q,%q,%q) = %v, want %v",
				tc.provider, tc.executor, tc.model, got, tc.want)
		}
	}
}

// Input-length price tiers must follow the recovered input total.
func TestOpenCodeInputThresholdTier(t *testing.T) {
	const model = "opencode-go/claude-sonnet-4-5"
	threshold := int64(150)
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 1}}
		c.PriceRules = []PriceRule{{
			Model:         model,
			InputTokensGT: &threshold,
			Price:         Price{Input: 10, Output: 10, CacheRead: 5, CacheCreation: 5},
		}}
	})
	at := time.Now().UTC().Add(-time.Minute)
	if !s.SubmitUsage(pluginapi.UsageRecord{
		RequestID: "tier", Provider: openCodeGoProvider, Model: model,
		ExecutorType: openCodeGoExecutor, RequestedAt: at,
		Detail: pluginapi.UsageDetail{
			InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20,
			OutputTokens: 200, ReasoningTokens: 100, TotalTokens: 370,
		},
	}) {
		t.Fatal("usage rejected")
	}
	flushAll(t, s)

	summary, err := s.Summary(Query{Provider: openCodeGoProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	// Recovered input total 170 > 150 selects the rule price.
	want := (100.0*10 + 50.0*5 + 20.0*5 + 200.0*10) / 1_000_000
	if !approx(summary.Totals.CostUSD, want) {
		t.Fatalf("cost = %v, want %v (rule tier not applied)", summary.Totals.CostUSD, want)
	}
}
