//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Real core shallow patches persist ordered rules and reprice the same history.
func TestConditionalPricingPatchOrderAndRestart(t *testing.T) {
	h := newHarness(t)
	page := h.sendAndObserve(t, 15*time.Second, anyObservedRequest)
	target := page.Items[0]
	patch := func(threshold int, conditionalFirst bool) {
		conditional := fmt.Sprintf(`{"model":%q,"input-tokens-gt":%d,"time-range":"00:00-24:00","price":{"input":2,"output":4,"cache-read":1}}`, target.Model, threshold)
		defaultRule := fmt.Sprintf(`{"model":%q,"price":{"input":1,"output":2,"cache-read":0.5}}`, target.Model)
		rules := conditional + "," + defaultRule
		if !conditionalFirst {
			rules = defaultRule + "," + conditional
		}
		body := []byte(`{"prices":{},"price-rules":[` + rules + `]}`)
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			route := "validate"
			if method == http.MethodPatch {
				route = "config"
			}
			status, _, _, err := h.rawRequestWithHeaders(method, "/v0/management/plugins/"+pluginID+"/"+route, body,
				map[string]string{"Authorization": "Bearer " + mgmtKey, "Content-Type": "application/json"})
			if err != nil || status != 200 {
				t.Fatalf("rules %s: status %d err %v", route, status, err)
			}
		}
	}
	waitPrice := func(host *harness, want float64) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			requestMatch := false
			for _, r := range host.fetchRequests(t).Items {
				if r.RequestID == target.RequestID && r.CostUSD != nil && approxEqual(*r.CostUSD, want, 1e-12) {
					requestMatch = true
				}
			}
			summary, err := host.fetchCurrentSummary(t)
			if err == nil && requestMatch && summary.Totals.Requests > 0 && summary.Totals.UnpricedRequests == 0 &&
				approxEqual(summary.Totals.CostUSD, float64(summary.Totals.Requests)*want, 1e-12) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("rule price not reflected consistently in request and summary")
	}
	patch(9, true) // Ten input tokens (including four cached) satisfy >9.
	waitPrice(h, 36.0/1e6)
	status, _, raw := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings")
	var settings struct {
		Rules []struct {
			Threshold *int64 `json:"input_tokens_gt"`
		} `json:"price_rules"`
	}
	if status != 200 || json.Unmarshal(raw, &settings) != nil || len(settings.Rules) != 2 ||
		settings.Rules[0].Threshold == nil || *settings.Rules[0].Threshold != 9 {
		t.Fatal("ordered effective rules missing")
	}
	patch(10, true) // Strict >, not >=.
	waitPrice(h, 18.0/1e6)
	patch(9, false) // Moving the unconditional rule above conditional takes priority.
	waitPrice(h, 18.0/1e6)
	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	waitPrice(restarted, 18.0/1e6)
	h.mock.requireClean(t)
}
