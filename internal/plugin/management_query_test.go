package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The dispatcher, not a UI mock, must connect the view/facet contracts to the
// real ledger and reject unknown/repeated/mixed-range inputs before querying.
func TestManagementRequestViewAndFacets(t *testing.T) {
	m := liveManager(t)
	key, _, err := m.store.CreateKey(engine.KeySpec{Enabled: true, Label: "Historical"})
	if err != nil {
		t.Fatal(err)
	}
	at := m.now().UTC()
	if err := m.store.RecordUsage(pluginapi.UsageRecord{
		RequestID: "view-row", APIKey: "observer:" + key.ID, Generate: true,
		RequestedAt: at, Model: "exact/model:v1", Provider: "openai",
		Detail: pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, TotalTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}
	date := at.Format("2006-01-02")
	var view engine.RequestView
	query := url.Values{"from_date": {date}, "to_date": {date}, "key_id": {key.ID}, "view": {"requests"}}
	response := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", query, "")
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil ||
		view.Summary.Requests != 1 || len(view.Records) != 1 || view.Range.Timezone != "UTC" {
		t.Fatalf("view not connected: %d %s", response.StatusCode, response.Body)
	}
	if view.Range.To.After(time.Now()) {
		t.Fatal("future view end")
	}
	loc, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	localDate := at.In(loc).Format("2006-01-02")
	zoned := url.Values{"from_date": {localDate}, "to_date": {localDate}, "timezone": {"Asia/Kathmandu"}, "view": {"provider"}}
	response = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", zoned, "")
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil ||
		view.Range.Timezone != "Asia/Kathmandu" || view.Summary.Requests != 1 || len(view.Groups) != 1 {
		t.Fatalf("timezone view not connected: %d %s", response.StatusCode, response.Body)
	}
	query.Set("view", "provider")
	response = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", query, "")
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil || len(view.Groups) != 1 || view.Groups[0].Provider != "openai" {
		t.Fatalf("group not connected: %d %s", response.StatusCode, response.Body)
	}
	query.Set("view", "key_model")
	response = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", query, "")
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil || len(view.Groups) != 1 ||
		view.Groups[0].KeyID != key.ID || view.Groups[0].Label != "Historical" ||
		view.Groups[0].Model != "exact/model:v1" || view.Groups[0].Provider != "openai" {
		t.Fatalf("composite group not connected: %d %s", response.StatusCode, response.Body)
	}
	var facets engine.FacetPage
	response = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets", url.Values{"field": {"model"}, "search": {"exact/"}}, "")
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &facets) != nil || len(facets.Items) != 1 || facets.Items[0].Value != "exact/model:v1" {
		t.Fatalf("facets not connected: %d %s", response.StatusCode, response.Body)
	}
	for _, query := range []url.Values{
		{"from": {"2024-01-01T00:00:00Z"}},
		{"from_date": {date, date}},
		{"view": {"unknown"}},
		{"page_size": {"101"}},
		{"from_date": {"2024-02-30"}},
		{"timezone": {"Local"}},
		{"timezone": {"Mars/Olympus"}},
		{"timezone": {"Asia/Kathmandu", "Asia/Kathmandu"}},
		{"unknown": {"1"}},
	} {
		if got := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", query, ""); got.StatusCode != 400 {
			t.Fatalf("invalid view query accepted %v: %d", query, got.StatusCode)
		}
	}
	for _, query := range []url.Values{{"field": {"secret"}}, {"field": {"model", "key_id"}}, {"field": {"model"}, "cursor": {"%%"}}, {"field": {"model"}, "limit": {"101"}}} {
		if got := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets", query, ""); got.StatusCode != 400 {
			t.Fatalf("invalid facet query accepted %v: %d", query, got.StatusCode)
		}
	}
}
