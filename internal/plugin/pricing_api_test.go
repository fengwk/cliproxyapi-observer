package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
)

// The locked Observer pricing API is one complete definition per exact
// (model, provider) identity. Old rule arrays are rejected rather than adapted.
func TestPricingSettingsAndSnapshotContract(t *testing.T) {
	m := liveManager(t)
	body := `{"pricing":{"auto":false,"source":"models_dev_v1","prices":[
		{"model":"m","provider":"p","definition":{"base":{"input":1000000,"output":2000000,
		"cache_read":0,"cache_write":0,"cache_write_by_ttl":{},"reasoning":null},
		"overrides":[{"id":"large","when":{"context":{"gte":100}},"rates":{"input":2000000}}],
		"multipliers":[{"id":"priority","when":{"serviceTier":["priority"]},
		"factors":{"input":"1.5","cache_read":"0.5"}}]}}],"mappings":[]}}`
	res := putSettingsForTest(t, m, body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings rejected: %d %s", res.StatusCode, res.Body)
	}
	settings := readSettingsForTest(t, m)
	if settings.Pricing.Source != pricing.SourceV1 || settings.Pricing.Auto ||
		len(settings.Pricing.Prices) != 1 ||
		settings.Pricing.Prices[0].Provider != "p" ||
		len(settings.Pricing.Prices[0].Definition.Overrides) != 1 ||
		len(settings.Pricing.Prices[0].Definition.Multipliers) != 1 ||
		settings.Pricing.Prices[0].Definition.Multipliers[0].Factors.Input == nil ||
		*settings.Pricing.Prices[0].Definition.Multipliers[0].Factors.Input != "1.5" {
		t.Fatalf("settings round-trip: %+v", settings.Pricing)
	}

	snapshot := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/prices", nil, "")
	var prices pricing.Snapshot
	if snapshot.StatusCode != http.StatusOK || json.Unmarshal(snapshot.Body, &prices) != nil {
		t.Fatalf("prices: %d %s", snapshot.StatusCode, snapshot.Body)
	}
	if prices.Source != pricing.SourceV1 || len(prices.Prices) != 1 ||
		prices.Prices[0].Definition.Base.Input != 1_000_000 ||
		prices.Prices[0].Rates.Input != 1_000_000 {
		t.Fatalf("price snapshot: %+v", prices)
	}
	if strings.Contains(string(snapshot.Body), "missing_rates") || strings.Contains(string(snapshot.Body), "unpriced") {
		t.Fatal("removed pricing states still exposed", string(snapshot.Body))
	}

	preview := dispatchForTest(t, m, http.MethodPost, managementBasePath+"/prices/preview", nil,
		`{"models":[{"model":"m","provider":"p"},{"model":"other","provider":""}]}`)
	var items struct {
		Items []pricing.Price `json:"items"`
	}
	if preview.StatusCode != http.StatusOK || json.Unmarshal(preview.Body, &items) != nil ||
		len(items.Items) != 2 || items.Items[0].Source != "manual" ||
		items.Items[0].Definition.Base.Output != 2_000_000 ||
		items.Items[1].Source != "zero" || items.Items[1].Definition.Base.Input != 0 {
		t.Fatalf("preview: %d %s", preview.StatusCode, preview.Body)
	}
}

func TestPricingSettingsRejectsLegacyAndInvalidIdentity(t *testing.T) {
	m := liveManager(t)
	for _, body := range []string{
		`{"pricing":{"rules":[]}}`,
		`{"pricing":{"source":"models_dev_v3"}}`,
		`{"pricing":{"prices":[{"model":"m","provider":"p","definition":{"base":{"input":1}}},{"model":"m","provider":"p","definition":{"base":{"input":2}}}]}}`,
		`{"pricing":{"prices":[{"model":"m","definition":{"base":{},"overrides":[{"id":"x","when":{"context":{"gt":1,"gte":1}},"rates":{}}]}}]}}`,
	} {
		if res := putSettingsForTest(t, m, body); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid pricing accepted: %d %s", res.StatusCode, res.Body)
		}
	}
	// A failed update leaves the previous (default v2, auto) configuration.
	settings := readSettingsForTest(t, m)
	if settings.Pricing.Source != pricing.SourceV2 || !settings.Pricing.Auto || len(settings.Pricing.Prices) != 0 {
		t.Fatalf("failed update changed settings: %+v", settings.Pricing)
	}
}
