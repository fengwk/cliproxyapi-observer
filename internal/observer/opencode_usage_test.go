package observer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Unknown plugin semantics must not hide traffic or fabricate billable tokens.
func TestOpenCodeUsageVisibleWithoutAssumedAccounting(t *testing.T) {
	const model = "opencode-go/glm-5.2"
	s := openTestStore(t, func(c *Config) {
		c.Prices = map[string]Price{model: {Input: 1, Output: 2, CacheRead: 0.1}}
	})
	at := time.Now().UTC().Add(-time.Minute)
	for i, stream := range []bool{false, true} {
		id := []string{"opencode-sync", "opencode-stream"}[i]
		if !s.SubmitUsage(pluginapi.UsageRecord{
			RequestID: id, Provider: "opencode-go", Model: model,
			ExecutorType: "executorAdapter", RequestedAt: at, Stream: stream,
			Latency: time.Second, TTFT: 500 * time.Millisecond,
			Detail: pluginapi.UsageDetail{
				InputTokens: 5, OutputTokens: 3, CacheReadTokens: 2, TotalTokens: 8,
			},
		}) {
			t.Fatal("OpenCode usage rejected")
		}
	}
	flushAll(t, s)

	query := Query{Provider: "opencode-go", Model: model}
	page, err := s.Requests(query)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("requests: count=%d, err=%v", len(page.Items), err)
	}
	for _, r := range page.Items {
		if r.InputTokens != 5 || r.OutputTokens != 3 || r.CacheReadTokens != 2 || r.TotalTokens != 8 {
			t.Fatalf("raw usage not visible: %+v", r)
		}
		if r.AccountingQuality != "unclassified" || r.UncachedInputTokens != 0 || r.TPS != nil || r.CostUSD != nil {
			t.Fatalf("unknown accounting incorrectly priced: %+v", r)
		}
	}
	summary, err := s.Summary(query)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Requests != 2 || summary.Totals.InputTokens != 10 ||
		summary.Totals.OutputTokens != 6 || summary.Totals.CacheReadTokens != 4 ||
		summary.Totals.TotalTokens != 16 || summary.Totals.UnpricedRequests != 2 ||
		summary.Totals.CostUSD != 0 {
		t.Fatalf("OpenCode traffic not aggregated: %+v", summary.Totals)
	}

	page, err = s.Requests(Query{Provider: "cliproxyapi-opencode-provider"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("plugin ID incorrectly matched provider ID: %+v, err=%v", page, err)
	}
}
