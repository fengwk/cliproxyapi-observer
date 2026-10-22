package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func putSettingsForTest(t *testing.T, m *Manager, body string) pluginapi.ManagementResponse {
	t.Helper()
	return dispatchForTest(t, m, http.MethodPut, managementBasePath+"/settings", nil, body)
}

func readSettingsForTest(t *testing.T, m *Manager) engine.Settings {
	t.Helper()
	var settings engine.Settings
	r := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/settings", nil, "")
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &settings) != nil {
		t.Fatalf("settings: %d %s", r.StatusCode, r.Body)
	}
	return settings
}

func saveSettingsForTest(t *testing.T, m *Manager, settings engine.Settings) {
	t.Helper()
	if _, err := m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
}

// Legacy clients submit only the old settings fields. New optional mode,
// retention and pricing mappings must keep their saved values, and a legacy
// boolean must never widen an already headers-only capture mode.
func TestSettingsPreserveOptionalFields(t *testing.T) {
	m := liveManager(t)
	saved := engine.DefaultSettings()
	saved.CaptureMode = engine.CaptureModeHeaders
	saved.CaptureEnabled = true
	saved.CaptureRetentionSeconds = 120
	saved.Pricing = pricing.Config{
		Auto:   true,
		Source: pricing.SourceV2,
		Prices: []pricing.ManualPrice{{Model: "saved", Definition: pricing.PricingDefinition{Base: pricing.RateSet{Input: 1}}}},
		Mappings: []pricing.Mapping{{
			Model: "public", Provider: "catalog", CatalogModel: "catalog/model",
		}},
	}
	saveSettingsForTest(t, m, saved)

	legacy := saved
	legacy.CaptureEnabled = true
	legacy.CaptureMode = ""
	legacy.CaptureRetentionSeconds = 0
	legacy.Pricing.Mappings = nil
	oldBody, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var oldFields map[string]any
	if err := json.Unmarshal(oldBody, &oldFields); err != nil {
		t.Fatal(err)
	}
	delete(oldFields, "capture_mode")
	delete(oldFields, "capture_retention_seconds")
	delete(oldFields["pricing"].(map[string]any), "mappings")
	oldBody, err = json.Marshal(oldFields)
	if err != nil {
		t.Fatal(err)
	}
	if r := putSettingsForTest(t, m, string(oldBody)); r.StatusCode != http.StatusOK {
		t.Fatalf("legacy settings update: %d %s", r.StatusCode, r.Body)
	}
	got := readSettingsForTest(t, m)
	if got.CaptureMode != engine.CaptureModeHeaders || !got.CaptureEnabled {
		t.Fatalf("legacy true widened headers: %+v", got)
	}
	if got.CaptureRetentionSeconds != 120 {
		t.Fatalf("retention not preserved: %d", got.CaptureRetentionSeconds)
	}
	if len(got.Pricing.Mappings) != 1 || got.Pricing.Mappings[0].Model != "public" {
		t.Fatalf("pricing mappings not preserved: %+v", got.Pricing.Mappings)
	}

	// Legacy false maps to off without touching other saved sections.
	delete(oldFields, "pricing")
	oldFields["capture_enabled"] = false
	oldBody, err = json.Marshal(oldFields)
	if err != nil {
		t.Fatal(err)
	}
	if r := putSettingsForTest(t, m, string(oldBody)); r.StatusCode != http.StatusOK {
		t.Fatalf("legacy disable: %d %s", r.StatusCode, r.Body)
	}
	got = readSettingsForTest(t, m)
	if got.CaptureMode != engine.CaptureModeOff || got.CaptureEnabled || len(got.Pricing.Mappings) != 1 || len(got.Pricing.Prices) != 1 {
		t.Fatalf("legacy disable overwrote sections: %+v", got)
	}

	// Explicit mode may widen intentionally; omitted retention/mappings survive.
	body := `{"capture_mode":"headers_and_body","capture_enabled":true}`
	if r := putSettingsForTest(t, m, body); r.StatusCode != http.StatusOK {
		t.Fatalf("explicit widen: %d %s", r.StatusCode, r.Body)
	}
	got = readSettingsForTest(t, m)
	if got.CaptureMode != engine.CaptureModeHeadersAndBody || !got.CaptureEnabled ||
		got.CaptureRetentionSeconds != 120 || len(got.Pricing.Mappings) != 1 {
		t.Fatalf("explicit widen lost saved fields: %+v", got)
	}

	// A capture-only update must not wipe saved pricing; a pricing-only update
	// must not wipe capture settings.
	body = `{"capture_mode":"headers"}`
	if r := putSettingsForTest(t, m, body); r.StatusCode != http.StatusOK {
		t.Fatalf("capture-only update: %d %s", r.StatusCode, r.Body)
	}
	got = readSettingsForTest(t, m)
	if got.CaptureMode != engine.CaptureModeHeaders || len(got.Pricing.Prices) != 1 || len(got.Pricing.Mappings) != 1 {
		t.Fatalf("capture-only update overwrote pricing: %+v", got)
	}
	body = `{"pricing":{"auto":false}}`
	if r := putSettingsForTest(t, m, body); r.StatusCode != http.StatusOK {
		t.Fatalf("pricing-only update: %d %s", r.StatusCode, r.Body)
	}
	got = readSettingsForTest(t, m)
	if got.CaptureMode != engine.CaptureModeHeaders || got.CaptureRetentionSeconds != 120 ||
		got.Pricing.Auto || len(got.Pricing.Prices) != 1 || len(got.Pricing.Mappings) != 1 {
		t.Fatalf("pricing-only update overwrote capture or pricing sections: %+v", got)
	}

	// Explicit mode and boolean contradictions are rejected before the store.
	for _, invalid := range []string{
		`{"capture_mode":"off","capture_enabled":true}`,
		`{"capture_mode":"headers","capture_enabled":false}`,
		`{"capture_mode":"headers_and_body","capture_enabled":false}`,
	} {
		if r := putSettingsForTest(t, m, invalid); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", invalid, r.StatusCode, r.Body)
		}
	}
	if got = readSettingsForTest(t, m); got.CaptureMode != engine.CaptureModeHeaders || got.Pricing.Auto {
		t.Fatalf("conflicting update mutated settings: %+v", got)
	}

	// Explicit mode-only updates derive the legacy boolean projection.
	if r := putSettingsForTest(t, m, `{"capture_mode":"off"}`); r.StatusCode != http.StatusOK {
		t.Fatalf("mode-only disable: %d %s", r.StatusCode, r.Body)
	}
	if got = readSettingsForTest(t, m); got.CaptureMode != engine.CaptureModeOff || got.CaptureEnabled {
		t.Fatalf("mode-only off projection: %+v", got)
	}
}

func TestSettingsStrictInputs(t *testing.T) {
	m := liveManager(t)
	for _, body := range []string{
		`{} {}`,
		`{"unknown":true}`,
		`{"capture_mode":null}`,
		`{"capture_retention_seconds":-1}`,
		`{"pricing":null}`,
		`{"pricing":{"unknown":true}}`,
		`{"pricing":{"prices":null}}`,
		`{"pricing":{"mappings":null}}`,
		`{"pricing":{"prices":[{"model":"m","unknown":1,"rates":{}}],"rules":[]}}`,
		`{"capture_mode":"headers","capture_enabled":true,"capture_enabled":false}`,
	} {
		if r := putSettingsForTest(t, m, body); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, r.StatusCode, r.Body)
		}
	}
}
