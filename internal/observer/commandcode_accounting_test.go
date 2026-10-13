package observer

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The literal provider/executor/model identifiers used by the commandcode
// adapter cohort. They are spelled out here so the regression pins the exact
// external strings the host sends, independently of the internal constants.
const (
	ccProvider    = "commandcode"
	ccExecutor    = "executorAdapter"
	ccSubsetModel = "commandcode/deepseek/deepseek-v4.1-flash"
	ccClaudeModel = "commandcode/claude-sonnet-4-5"
)

// commandCodeRecord builds a commandcode adapter usage record carrying the
// shared executor implementation and a native model id in its own namespace.
func commandCodeRecord(model string, detail pluginapi.UsageDetail) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		RequestID:    "cc",
		Provider:     ccProvider,
		Model:        model,
		ExecutorType: ccExecutor,
		RequestedAt:  time.Unix(1700, 0).UTC(),
		Latency:      2 * time.Second,
		TTFT:         time.Second,
		Detail:       detail,
	}
}

// An OpenAI/Responses commandcode record (cache nested inside input, reasoning
// inside output) must be recovered to complete accounting with the raw total
// authoritative, for both streaming and non-streaming requests, and priced from
// the canonical buckets.
func TestCommandCodeSubsetAccountingRecovered(t *testing.T) {
	const model = ccSubsetModel
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 2, CacheRead: 0.1, CacheCreation: 0.5}}
	})
	at := time.Now().UTC().Add(-time.Minute)
	for i, stream := range []bool{false, true} {
		id := []string{"cc-sync", "cc-stream"}[i]
		if !s.SubmitUsage(pluginapi.UsageRecord{
			RequestID: id, Provider: ccProvider, Model: model, Alias: "flash",
			ExecutorType: ccExecutor, RequestedAt: at, Stream: stream,
			Latency: time.Second, TTFT: 500 * time.Millisecond,
			Detail: pluginapi.UsageDetail{
				InputTokens: 100, CacheReadTokens: 80, CacheCreationTokens: 10,
				OutputTokens: 30, ReasoningTokens: 12, TotalTokens: 130,
			},
		}) {
			t.Fatal("CommandCode usage rejected")
		}
	}
	flushAll(t, s)

	query := Query{Provider: ccProvider, Model: model}
	page, err := s.Requests(query)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("requests: count=%d, err=%v", len(page.Items), err)
	}
	for _, r := range page.Items {
		if r.AccountingQuality != "complete" {
			t.Fatalf("quality = %q, want complete: %+v", r.AccountingQuality, r)
		}
		// Cache read/write are carved out of the authoritative input total;
		// reasoning stays inside the output total.
		if r.InputTokens != 100 || r.UncachedInputTokens != 10 || r.CacheReadTokens != 80 ||
			r.CacheCreationTokens != 10 || r.OutputTokens != 30 || r.ReasoningTokens != 12 ||
			r.TotalTokens != 130 {
			t.Fatalf("recovered buckets wrong: %+v", r)
		}
		if r.TPS == nil {
			t.Fatalf("TPS not restored: %+v", r)
		}
		// Full identity, alias, model name and raw counters are preserved.
		if r.Provider != ccProvider || r.Executor != ccExecutor || r.Model != model || r.Alias != "flash" {
			t.Fatalf("identity changed: %+v", r)
		}
		if r.RawUsage.InputTokens != 100 || r.RawUsage.CacheReadTokens != 80 ||
			r.RawUsage.CacheCreationTokens != 10 || r.RawUsage.OutputTokens != 30 ||
			r.RawUsage.ReasoningTokens != 12 || r.RawUsage.TotalTokens != 130 {
			t.Fatalf("raw usage changed: %+v", r.RawUsage)
		}
	}
	// (10*1 + 80*0.1 + 10*0.5 + 30*2) / 1e6
	want := 83.0 / 1_000_000
	summary, err := s.Summary(query)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(summary.Totals.CostUSD, 2*want) || summary.Totals.UnpricedRequests != 0 {
		t.Fatalf("summary pricing wrong: %+v", summary.Totals)
	}
}

// An Anthropic Messages commandcode record (input excludes cache, raw output
// still contains reasoning) must be recovered as independent accounting.
func TestCommandCodeAnthropicIndependentRecovered(t *testing.T) {
	const model = ccClaudeModel
	record := commandCodeRecord(model, pluginapi.UsageDetail{
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
	if r.OutputTokens != 200 || r.ReasoningTokens != 100 || r.TotalTokens != 370 {
		t.Fatalf("output buckets wrong: %+v", r)
	}
	if r.RawUsage.InputTokens != 100 || r.RawUsage.OutputTokens != 200 {
		t.Fatalf("raw usage changed: %+v", r.RawUsage)
	}
	// (100*3 + 50*0.3 + 20*3.75 + 200*15) / 1e6
	cost := RequestCost(r, map[string]Price{model: {Input: 3, Output: 15, CacheRead: 0.3, CacheCreation: 3.75}})
	if cost == nil || !approx(*cost, 3390.0/1_000_000) {
		t.Fatalf("cost = %v, want 3.39e-3", cost)
	}
}

// A commandcode record with a positive input and no cache is uniquely complete:
// the cache ratio is a legitimate 0%, and the complete partition is preserved
// identically across every summary view.
func TestCommandCodeZeroCacheCompleteAndViews(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	const model = ccSubsetModel
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 2}}
	})
	if !s.SubmitUsage(pluginapi.UsageRecord{
		RequestID: "cc-zero", Provider: ccProvider, Model: model,
		ExecutorType: ccExecutor, RequestedAt: at,
		APIKey: "cc-client", AuthIndex: "0123456789abcdef",
		Latency: 2 * time.Second, TTFT: time.Second,
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}) {
		t.Fatal("CommandCode usage rejected")
	}
	flushAll(t, s)

	sum, err := s.Summary(Query{Provider: ccProvider, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	// Complete accounting with a legitimate zero cache numerator.
	ca := sum.Totals.CacheAccounting
	if ca == nil || ca.Requests != 1 || ca.InputTokens != 10 || ca.CacheReadTokens != 0 {
		t.Fatalf("totals cache_accounting = %+v, want 1/10/0", ca)
	}
	if sum.Totals.UnpricedRequests != 0 {
		t.Fatalf("unpriced = %d, want 0", sum.Totals.UnpricedRequests)
	}
	// The same complete basis must appear in the group, client key and
	// credential views, not just the totals.
	if len(sum.Groups) != 1 || len(sum.ClientKeys) != 1 || len(sum.Credentials) != 1 {
		t.Fatalf("views = groups %d keys %d creds %d, want 1/1/1",
			len(sum.Groups), len(sum.ClientKeys), len(sum.Credentials))
	}
	for _, view := range []*CacheAccounting{
		sum.Groups[0].CacheAccounting,
		sum.ClientKeys[0].CacheAccounting,
		sum.Credentials[0].CacheAccounting,
	} {
		if view == nil || *view != *ca {
			t.Fatalf("view cache_accounting = %+v, want %+v", view, ca)
		}
	}
}

// Only the exact commandcode / executorAdapter / commandcode/<native> cohort is
// reinterpreted; plugin ids, foreign namespaces, cross-provider namespaces,
// unknown executors and bare prefixes must stay unclassified and unpriced.
func TestCommandCodeAdapterTargetIsolation(t *testing.T) {
	cases := []struct {
		name                      string
		provider, executor, model string
	}{
		{"plugin_id_as_provider", "cliproxyapi-commandcode-provider", ccExecutor, ccSubsetModel},
		{"foreign_namespace", ccProvider, ccExecutor, "deepseek/deepseek-v4.1-flash"},
		{"opencode_namespace_on_commandcode", ccProvider, ccExecutor, "opencode-go/deepseek"},
		{"commandcode_namespace_on_opencode", "opencode-go", ccExecutor, ccSubsetModel},
		{"unknown_executor", ccProvider, "otherExecutor", ccSubsetModel},
		{"bare_prefix", ccProvider, ccExecutor, "commandcode/"},
		{"no_separator", ccProvider, ccExecutor, "commandcode"},
		{"empty_model", ccProvider, ccExecutor, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NormalizeUsage(pluginapi.UsageRecord{
				RequestID: "x", Provider: tc.provider, ExecutorType: tc.executor,
				Model: tc.model, RequestedAt: time.Unix(1000, 0).UTC(),
				Detail: pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8},
			})
			if r.AccountingQuality == string(usage.TokenAccountingQualityComplete) {
				t.Fatalf("out-of-cohort record wrongly recovered: %+v", r)
			}
			if cost := RequestCost(r, map[string]Price{tc.model: {Input: 1, Output: 1}}); cost != nil {
				t.Fatalf("unclassified record repriced: %v", *cost)
			}
		})
	}
}

// Invalid or ambiguous raw counters must never be repaired into fabricated
// complete accounting or cost for the commandcode cohort.
func TestCommandCodeRejectsInvalidRawCounters(t *testing.T) {
	cases := []struct {
		name   string
		detail pluginapi.UsageDetail
	}{
		{"missing_total", pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 5, CacheReadTokens: 10}},
		{"contradictory_total", pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, TotalTokens: 999}},
		{"alias_unknown", pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, CachedTokens: 40, CacheReadTokens: 10, TotalTokens: 150}},
		{"alias_only", pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, CachedTokens: 40, TotalTokens: 150}},
		{"reasoning_exceeds_output", pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 60, TotalTokens: 150}},
		{"negative_counter", pluginapi.UsageDetail{InputTokens: -5, OutputTokens: 50, TotalTokens: 150}},
		{"overflow_total", pluginapi.UsageDetail{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64, TotalTokens: math.MaxInt64}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NormalizeUsage(commandCodeRecord(ccSubsetModel, tc.detail))
			if r.AccountingQuality == string(usage.TokenAccountingQualityComplete) {
				t.Fatalf("invalid record wrongly classified complete: %+v", r)
			}
			// Raw positive counters stay visible for display.
			if r.InputTokens != positive(tc.detail.InputTokens) {
				t.Fatalf("raw input not preserved: got %d, want %d", r.InputTokens, positive(tc.detail.InputTokens))
			}
			if cost := RequestCost(r, map[string]Price{ccSubsetModel: {Input: 1, Output: 1}}); cost != nil {
				t.Fatalf("unclassified record repriced: %v", *cost)
			}
		})
	}
}

// A failed commandcode request with zero usage must never invent tokens.
func TestCommandCodeFailedZeroUsageNotInvented(t *testing.T) {
	record := commandCodeRecord(ccSubsetModel, pluginapi.UsageDetail{})
	record.Failed = true
	record.Failure.StatusCode = 500
	r := NormalizeUsage(record)
	if r.InputTokens != 0 || r.OutputTokens != 0 || r.TotalTokens != 0 ||
		r.UncachedInputTokens != 0 || r.CacheReadTokens != 0 || r.CacheCreationTokens != 0 {
		t.Fatalf("zero usage invented tokens: %+v", r)
	}
}
