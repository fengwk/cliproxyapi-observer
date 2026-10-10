package observer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// A nonzero DeepSeek price must apply only inside its configured UTC window.
func TestOpenCodeDeepSeekNonzeroTimeRule(t *testing.T) {
	const model = "opencode-go/deepseek-v4.1-flash"
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	s := openTestStore(t, func(c *Config) {
		c.RequestRetention = 48 * time.Hour
		c.PriceRules = []PriceRule{{
			Model: model, TimeRange: "00:00-08:30",
			Price: Price{Input: 0.3, Output: 1.2, CacheRead: 0.01},
		}}
	})
	for _, tc := range []struct {
		id   string
		hour time.Duration
	}{
		{"inside-window", 6},
		{"outside-window", 12},
	} {
		r := openCodeRecord(model, pluginapi.UsageDetail{
			InputTokens: 100000, OutputTokens: 1000,
			CacheReadTokens: 98000, CachedTokens: 98000, TotalTokens: 101000,
		})
		r.RequestID, r.RequestedAt = tc.id, day.Add(tc.hour*time.Hour)
		if !s.SubmitUsage(r) {
			t.Fatal("usage rejected")
		}
	}
	flushAll(t, s)
	q := Query{Model: model, From: day, To: day.Add(24 * time.Hour)}
	page, err := s.Requests(q)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("requests = %+v, error = %v", page, err)
	}
	const wantCost = (2000*0.3 + 98000*0.01 + 1000*1.2) / 1e6
	for _, r := range page.Items {
		if r.AccountingQuality != "complete" || r.UncachedInputTokens != 2000 {
			t.Fatalf("DeepSeek accounting not recovered: %+v", r)
		}
		switch r.RequestID {
		case "inside-window":
			if r.CostUSD == nil || !approx(*r.CostUSD, wantCost) {
				t.Fatalf("inside-window cost = %v, want %v", r.CostUSD, wantCost)
			}
		case "outside-window":
			if r.CostUSD != nil {
				t.Fatalf("outside-window request must remain unpriced: %v", *r.CostUSD)
			}
		}
	}
	summary, err := s.Summary(q)
	if err != nil || summary.Totals.Requests != 2 ||
		summary.Totals.UnpricedRequests != 1 || !approx(summary.Totals.CostUSD, wantCost) {
		t.Fatalf("DeepSeek summary = %+v, error = %v", summary, err)
	}
}
