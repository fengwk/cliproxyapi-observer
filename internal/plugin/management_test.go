package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

var managementNow = time.Date(2026, 10, 8, 12, 30, 0, 0, time.UTC)

func callManagement(t *testing.T, m *Manager, method, path string, query url.Values) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := m.HandleCall(pluginabi.MethodManagementHandle, mustJSON(t, managementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Query: query},
	}))
	if err != nil {
		t.Fatalf("management call: %v", err)
	}
	var resp pluginapi.ManagementResponse
	mustResult(t, raw, &resp)
	return resp
}

func registeredManager(t *testing.T, opener *fakeOpener) *Manager {
	t.Helper()
	m := NewManager(opener, nil)
	m.now = func() time.Time { return managementNow }
	register(t, m, "live")
	return m
}

func decodeBody(t *testing.T, resp pluginapi.ManagementResponse, target any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body, target); err != nil {
		t.Fatalf("decode response body %s: %v", resp.Body, err)
	}
}

func TestManagementDeclaresReadOnlyRoutesAndResources(t *testing.T) {
	m, _ := newTestManager()
	raw, err := m.HandleCall(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("register management: %v", err)
	}
	var reg managementRegistrationResponse
	mustResult(t, raw, &reg)

	want := map[string]bool{
		"/plugins/cliproxyapi-observer/summary":  false,
		"/plugins/cliproxyapi-observer/requests": false,
		"/plugins/cliproxyapi-observer/body":     false,
		"/plugins/cliproxyapi-observer/settings": false,
		"/plugins/cliproxyapi-observer/health":   false,
	}
	for _, route := range reg.Routes {
		if route.Method != http.MethodGet {
			t.Errorf("route %s method = %s, want GET", route.Path, route.Method)
		}
		if _, ok := want[route.Path]; !ok {
			t.Errorf("unexpected route %s", route.Path)
			continue
		}
		want[route.Path] = true
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("route %s not declared", path)
		}
	}
	if len(reg.Resources) != 3 {
		t.Fatalf("resources = %d, want 3", len(reg.Resources))
	}
	menuSet := false
	for _, res := range reg.Resources {
		switch res.Path {
		case "/ui":
			menuSet = res.Menu == PluginName
		case "/ui.js", "/ui.css":
		default:
			t.Errorf("unexpected resource %s", res.Path)
		}
	}
	if !menuSet {
		t.Errorf("resource /ui must carry the %q menu", PluginName)
	}
}

func TestManagementAcceptsBothManagementPrefixes(t *testing.T) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	m := registeredManager(t, opener)
	opener.last().status = observer.Status{Queued: 1}
	for _, prefix := range []string{"/v0/management", "/v8/management"} {
		resp := callManagement(t, m, http.MethodGet, prefix+"/plugins/cliproxyapi-observer/health", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s health status = %d, want 200", prefix, resp.StatusCode)
		}
	}
}

func TestSummaryDefaultsAndRejectsBroadRanges(t *testing.T) {
	cfg := observerConfigFixture()
	cfg.StatsRetentionDays = 1 // keep the retained statistics window at 24h
	opener := &fakeOpener{cfg: cfg}
	m := registeredManager(t, opener)

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d body %s", resp.StatusCode, resp.Body)
	}
	query, ok := opener.last().lastQuery()
	if !ok {
		t.Fatal("summary was not forwarded to the store")
	}
	if !query.To.Equal(managementNow) {
		t.Errorf("default to = %s, want %s", query.To, managementNow)
	}
	if want := managementNow.Add(-defaultQueryWindow); !query.From.Equal(want) {
		t.Errorf("default from = %s, want %s", query.From, want)
	}

	values := url.Values{"from": {managementNow.Add(-time.Hour).Format(time.RFC3339)}, "to": {managementNow.Format(time.RFC3339)}, "provider": {"openai"}, "model": {"gpt"}}
	resp = callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", values)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d", resp.StatusCode)
	}
	query, _ = opener.last().lastQuery()
	if query.Provider != "openai" || query.Model != "gpt" {
		t.Errorf("filters not forwarded: %+v", query)
	}

	// A range wider than the retained statistics window is rejected.
	broad := url.Values{"from": {managementNow.Add(-48 * time.Hour).Format(time.RFC3339)}, "to": {managementNow.Format(time.RFC3339)}}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", broad); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("broad summary status = %d, want 400", resp.StatusCode)
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", url.Values{"from": {"not-a-time"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed summary status = %d, want 400", resp.StatusCode)
	}
}

func TestSummaryErrorsAreSanitized(t *testing.T) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	m := registeredManager(t, opener)
	opener.last().summaryErr = errStoreFailure

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if body := string(resp.Body); len(body) == 0 || strings.Contains(body, "/secret/path") {
		t.Errorf("error body leaked store internals: %s", body)
	}
}

func TestRequestsLimitCursorAndValidation(t *testing.T) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	m := registeredManager(t, opener)

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("requests status = %d", resp.StatusCode)
	}
	query, _ := opener.last().lastQuery()
	if query.Limit != defaultRequestLimit {
		t.Errorf("default limit = %d, want %d", query.Limit, defaultRequestLimit)
	}

	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"limit": {"100"}, "cursor": {"abc"}}); resp.StatusCode != http.StatusOK {
		t.Errorf("limit 100 status = %d", resp.StatusCode)
	}
	query, _ = opener.last().lastQuery()
	if query.Cursor != "abc" || query.Limit != 100 {
		t.Errorf("cursor/limit not forwarded: %+v", query)
	}
	for _, bad := range []string{"0", "101", "abc"} {
		if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"limit": {bad}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit %q status = %d, want 400", bad, resp.StatusCode)
		}
	}
	opener.last().requestsErr = observer.ErrNotFound
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"cursor": {"bad"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad cursor status = %d, want 400", resp.StatusCode)
	}
}

func TestBodyRoute(t *testing.T) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	m := registeredManager(t, opener)

	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/body", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing request_id status = %d, want 400", resp.StatusCode)
	}
	opener.last().bodyErr = observer.ErrNotFound
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/body", url.Values{"request_id": {"gone"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing body status = %d, want 404", resp.StatusCode)
	}
	opener.last().bodyErr = nil
	opener.last().body = observer.BodyDetail{RequestID: "req-1", Content: "hello", Redacted: true}
	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/body", url.Values{"request_id": {"req-1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("body status = %d", resp.StatusCode)
	}
	var detail observer.BodyDetail
	decodeBody(t, resp, &detail)
	if detail.Content != "hello" || !detail.Redacted {
		t.Errorf("body detail = %+v", detail)
	}
}

func TestSettingsAndHealthRoutes(t *testing.T) {
	cfg := observerConfigFixture()
	cfg.CaptureBodies = true
	cfg.BodyRetention = 12 * time.Hour
	cfg.RequestRetention = 6 * time.Hour
	cfg.StatsRetentionDays = 30
	cfg.MaxBodyBytes = 4096
	cfg.MaxBodyStorageBytes = 8192
	opener := &fakeOpener{cfg: cfg}
	m := registeredManager(t, opener)

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings status = %d", resp.StatusCode)
	}
	var settings settingsResponse
	decodeBody(t, resp, &settings)
	want := settingsResponse{CaptureBodies: true, BodyRetentionSeconds: 43200, RequestRetentionSeconds: 21600, StatsRetentionDays: 30, MaxBodyBytes: 4096, MaxBodyStorageBytes: 8192}
	if settings != want {
		t.Errorf("settings = %+v, want %+v", settings, want)
	}

	opener.last().status = observer.Status{DroppedUsage: 2, DroppedBodies: 1, WriteErrors: 3, Queued: 4}
	resp = callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	var status observer.Status
	decodeBody(t, resp, &status)
	if status.DroppedUsage != 2 || status.DroppedBodies != 1 || status.WriteErrors != 3 || status.Queued != 4 {
		t.Errorf("health = %+v", status)
	}
}

func TestResourceRoutesAreHardened(t *testing.T) {
	assets := func(name string) ([]byte, string, bool) {
		contentType := map[string]string{"ui.html": "text/html; charset=utf-8", "ui.js": "text/javascript; charset=utf-8", "ui.css": "text/css; charset=utf-8"}[name]
		if contentType == "" {
			return nil, "", false
		}
		return []byte("asset:" + name), contentType, true
	}
	m := NewManager(&fakeOpener{cfg: observerConfigFixture()}, assets)
	register(t, m, "live")

	cases := map[string]string{"/ui": "text/html", "/ui.js": "javascript", "/ui.css": "text/css"}
	for route, contentType := range cases {
		resp := callManagement(t, m, http.MethodGet, "/v0/resource/plugins/cliproxyapi-observer"+route, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", route, resp.StatusCode)
		}
		if got := resp.Headers.Get("Content-Type"); !strings.Contains(got, contentType) {
			t.Errorf("%s Content-Type = %q", route, got)
		}
		if resp.Headers.Get("Content-Security-Policy") != cspPolicy {
			t.Errorf("%s CSP = %q", route, resp.Headers.Get("Content-Security-Policy"))
		}
		if resp.Headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s missing nosniff", route)
		}
		if resp.Headers.Get("Cache-Control") != "no-store" {
			t.Errorf("%s missing no-store", route)
		}
		if resp.Headers.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s missing referrer-policy", route)
		}
	}

	if resp := callManagement(t, m, http.MethodGet, "/v0/resource/plugins/cliproxyapi-observer/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown resource status = %d, want 404", resp.StatusCode)
	}
	// Without a wired asset source every resource is unavailable.
	bare := NewManager(&fakeOpener{cfg: observerConfigFixture()}, nil)
	register(t, bare, "live")
	if resp := callManagement(t, bare, http.MethodGet, "/v0/resource/plugins/cliproxyapi-observer/ui", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("nil assets status = %d, want 404", resp.StatusCode)
	}
}

func TestManagementRejectsNonGetAndUnknownPaths(t *testing.T) {
	m := registeredManager(t, &fakeOpener{cfg: observerConfigFixture()})
	if resp := callManagement(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/summary", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/unknown", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}
}
