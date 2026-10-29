//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Exercise the reconstruction API through the actual SDK/host rather than a
// UI mock. Delete/purge must preserve paid records and counters across restart.
func TestRebuiltViewsContentAndTerminalKeyLifecycle(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	settings := acceptanceGetSettings(t, h)
	settings["capture_enabled"], settings["capture_mode"], settings["capture_retention_seconds"] = true, "headers_and_body", 60
	h.api(t, http.MethodPut, "/settings", settings, 200, nil)
	known := map[string]struct{}{}
	record := acceptanceInferAndAwait(t, h, created.Secret, known)
	h.awaitGracefulRestartReady(t, 1)

	type detailView struct {
		Body          json.RawMessage `json:"body"`
		HeadersStatus string          `json:"headers_status"`
		BodyStatus    string          `json:"body_status"`
		PriceQuote    map[string]any  `json:"price_quote"`
	}
	var detail detailView
	detailPath := "/requests/detail?id=" + url.QueryEscape(record.ID)
	h.await(t, "native capture joins the saved execution", func() bool {
		h.api(t, http.MethodGet, detailPath, nil, 200, &detail)
		return detail.BodyStatus == "available" || detail.BodyStatus == "redacted"
	})
	if string(detail.Body) == "null" || detail.PriceQuote == nil {
		t.Fatal("detail quote/content absent")
	}

	type aggregate struct {
		Requests int64 `json:"requests"`
		Detailed int64 `json:"detailed_requests"`
		Cost     int64 `json:"cost_micro_usd"`
	}
	type requestView struct {
		Summary aggregate        `json:"summary"`
		Records []recordView     `json:"records"`
		Groups  []map[string]any `json:"groups"`
		Range   struct {
			Timezone string `json:"timezone"`
		} `json:"range"`
	}
	date := time.Now().UTC().Format("2006-01-02")
	q := "?from_date=" + date + "&to_date=" + date + "&key_id=" + created.Key.ID
	var view requestView
	for _, group := range []string{"requests", "model", "key", "account", "provider"} {
		h.api(t, http.MethodGet, "/requests/view"+q+"&view="+group, nil, 200, &view)
		if view.Summary.Requests != 1 || view.Summary.Detailed != 1 || view.Range.Timezone != "UTC" {
			t.Fatalf("view %s: %+v", group, view)
		}
		if group == "requests" && (len(view.Records) != 1 || view.Records[0].ID != record.ID) {
			t.Fatal("records not wired")
		}
		if group != "requests" && len(view.Groups) != 1 {
			t.Fatal("groups not wired", group, view.Groups)
		}
	}
	var facets struct {
		Items []struct {
			Value string `json:"value"`
		} `json:"items"`
	}
	h.api(t, http.MethodGet, "/requests/facets?field=model&search="+url.QueryEscape(record.Model), nil, 200, &facets)
	if len(facets.Items) != 1 || facets.Items[0].Value != record.Model {
		t.Fatal("historical facets not wired")
	}
	beforeKeys, beforeCost := h.keys(t), view.Summary.Cost
	h.api(t, http.MethodPost, "/captures/purge", map[string]bool{"confirm": false}, 400, nil)
	h.api(t, http.MethodPost, "/captures/cleanup", nil, 200, nil)
	h.api(t, http.MethodPost, "/captures/purge", map[string]bool{"confirm": true}, 200, nil)
	h.api(t, http.MethodGet, detailPath, nil, 200, &detail)
	if detail.BodyStatus != "purged" || string(detail.Body) != "null" {
		t.Fatal("purge did not hide body", detail)
	}
	if got := h.keys(t); len(got) != 1 || got[0].Used != beforeKeys[0].Used {
		t.Fatal("purge changed key accounting")
	}
	deletePath := "/keys/delete?id=" + created.Key.ID
	h.api(t, http.MethodPost, deletePath, nil, 204, nil)
	h.api(t, http.MethodPost, deletePath, nil, 204, nil)
	if len(h.keys(t)) != 0 {
		t.Fatal("deleted key still listed by default")
	}
	denyInference(t, h, created.Secret)
	h.api(t, http.MethodPost, "/keys/rotate?id="+created.Key.ID, nil, 410, nil)
	h.api(t, http.MethodPut, "/keys?id="+created.Key.ID, validSpec(), 410, nil)
	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	h = restarted
	h.api(t, http.MethodGet, "/requests/view"+q, nil, 200, &view)
	if view.Summary.Requests != 1 || view.Summary.Cost != beforeCost {
		t.Fatal("delete/restart changed immutable history")
	}
	h.api(t, http.MethodGet, detailPath, nil, 200, &detail)
	if string(detail.Body) != "null" {
		t.Fatal("purged content reappeared on restart")
	}
	for _, route := range []string{"/requests/view", "/requests/facets?field=model"} {
		status, _, err := h.rawRequest(http.MethodGet, "/v0/management/plugins/"+pluginID+route, nil, map[string]string{"Authorization": "Bearer " + created.Secret})
		if err != nil || status != 401 {
			t.Fatal("new management route accepted client credential", status, err)
		}
	}
}
