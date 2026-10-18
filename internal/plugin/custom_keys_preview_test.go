package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
)

// Exercise strict JSON through the dispatcher so optional secrets cannot bypass
// its body/query validation or appear in lists, errors and failed rotations.
func TestCustomKeyManagementContract(t *testing.T) {
	m := liveManager(t)
	for _, body := range []string{`null`, `[]`, `{"secret":null}`, `{"secret":8}`, `{"secret":""}`,
		`{"secret":"12345678","secret":"abcdefgh"}`, `{"secret":" abcdefgh"}`, `{"unknown":true}`,
		`{"key_spec":{"enabled":true}}`} {
		res := dispatchForTest(t, m, "POST", managementBasePath+"/keys", nil, body)
		if res.StatusCode != 400 {
			t.Fatalf("invalid body accepted: %d %s", res.StatusCode, res.Body)
		}
	}
	create := func(secret string) keySecretResponse {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"label": "custom", "enabled": true, "secret": secret})
		res := dispatchForTest(t, m, "POST", managementBasePath+"/keys", nil, string(body))
		var out keySecretResponse
		if res.StatusCode != 201 || json.Unmarshal(res.Body, &out) != nil || out.Secret != secret {
			t.Fatalf("custom create: %d %s", res.StatusCode, res.Body)
		}
		return out
	}
	first := create("first-custom-secret!")
	create("second-custom-secret!")
	query := url.Values{"id": {first.Key.ID}}
	before, _ := m.store.Keys()
	for _, body := range []string{`{"secret":null}`, `{"secret":""}`, `{"secret":false}`,
		`{"secret":"first-custom-secret!"}`, `{"unknown":"x"}`} {
		if res := dispatchForTest(t, m, "POST", managementBasePath+"/keys/rotate", query, body); res.StatusCode != 400 {
			t.Fatal("invalid rotation accepted", res.StatusCode, string(res.Body))
		}
	}
	res := dispatchForTest(t, m, "POST", managementBasePath+"/keys/rotate", query, `{"secret":"second-custom-secret!"}`)
	if res.StatusCode != 409 || strings.Contains(string(res.Body), "second-custom-secret!") {
		t.Fatal("collision response unsafe")
	}
	res = dispatchForTest(t, m, "PUT", managementBasePath+"/keys", query, `{"secret":"replacement-secret!"}`)
	if res.StatusCode != 400 {
		t.Fatal("PUT accepts secret")
	}
	after, _ := m.store.Keys()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed requests changed Key state")
	}
	res = dispatchForTest(t, m, "GET", managementBasePath+"/keys", nil, "")
	if strings.Contains(string(res.Body), first.Secret) || strings.Contains(string(res.Body), `"secret"`) {
		t.Fatal("list exposes secret")
	}
	for _, body := range []string{`{"secret":"replacement-secret!"}`, "", "{}"} {
		res = dispatchForTest(t, m, "POST", managementBasePath+"/keys/rotate", query, body)
		var out keySecretResponse
		if res.StatusCode != 200 || json.Unmarshal(res.Body, &out) != nil || len(out.Secret) < 8 || out.Key.ID != first.Key.ID {
			t.Fatal("custom/legacy automatic rotation failed", res.StatusCode, string(res.Body))
		}
	}
}

// A preview is authenticated management data, never a public resource. Exact
// static routing and the same static-asset protections apply to the theme bridge.
func TestPricePreviewAndThemeManagementBoundary(t *testing.T) {
	// No background refresh worker: comparing whole snapshots on a live manager
	// races its startup catalog sync rather than measuring the preview's effects.
	// A transport tripwire also proves this read-only route never fetches prices.
	m := NewManager(nil)
	service, err := pricing.NewService(pricing.DefaultConfig(), filepath.Join(t.TempDir(), "catalog.json"),
		&http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			t.Error("preview performed network I/O")
			return nil, errors.New("unexpected preview fetch")
		})})
	if err != nil {
		t.Fatal(err)
	}
	m.prices = service
	before := m.prices.Snapshot()
	for _, body := range []string{`{}`, `{"models":null}`, `{"models":[{"model":""}]}`,
		`{"models":[{"model":"m","unknown":true}]}`, `{"models":[],"unknown":true}`, `{"models":[],"models":[]}`} {
		if res := dispatchForTest(t, m, "POST", managementBasePath+"/prices/preview", nil, body); res.StatusCode != 400 {
			t.Fatal("invalid preview accepted", res.StatusCode, string(res.Body))
		}
	}
	tooMany := make([]pricing.ModelIdentity, 4097)
	for i := range tooMany {
		tooMany[i].Model = "m"
	}
	body, _ := json.Marshal(map[string]any{"models": tooMany})
	if res := dispatchForTest(t, m, "POST", managementBasePath+"/prices/preview", nil, string(body)); res.StatusCode != 400 {
		t.Fatal("unbounded preview")
	}
	for _, query := range []url.Values{{"extra": {"1"}}, {"id": {"x"}}} {
		if res := dispatchForTest(t, m, "POST", managementBasePath+"/prices/preview", query, `{"models":[]}`); res.StatusCode != 400 {
			t.Fatal("preview query accepted")
		}
	}
	res := dispatchForTest(t, m, "POST", managementBasePath+"/prices/preview", nil, `{"models":[{"model":"unknown","provider":""}]}`)
	var out struct {
		Items []pricing.PreviewPrice `json:"items"`
	}
	if res.StatusCode != 200 || json.Unmarshal(res.Body, &out) != nil || len(out.Items) != 1 || out.Items[0].Source != "unpriced" {
		t.Fatal("preview failed", res.StatusCode, string(res.Body))
	}
	if !reflect.DeepEqual(before, m.prices.Snapshot()) {
		t.Fatal("preview observed or wrote models")
	}
	if res := dispatchForTest(t, m, "GET", resourceBasePath+"/prices/preview", nil, ""); res.StatusCode != 404 {
		t.Fatal("preview publicly exposed")
	}
	m.assets = func(name string) ([]byte, string, bool) {
		return []byte("/* theme */"), "", name == "theme.js"
	}
	for _, prefix := range []string{managementBasePath, resourceBasePath} {
		res := dispatchForTest(t, m, http.MethodGet, prefix+"/theme.js", nil, "")
		if res.StatusCode != 200 || res.Headers.Get("Content-Type") != "text/javascript; charset=utf-8" ||
			res.Headers.Get("Content-Security-Policy") != cspPolicy || res.Headers.Get("Cache-Control") != "no-store" {
			t.Fatal("theme bridge lacks asset protections", res)
		}
	}
}
