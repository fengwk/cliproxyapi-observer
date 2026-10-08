package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
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

func callManagementWithBody(t *testing.T, m *Manager, method, path string, body []byte) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := m.HandleCall(pluginabi.MethodManagementHandle, mustJSON(t, managementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Body: body},
	}))
	if err != nil {
		t.Fatalf("management call with body: %v", err)
	}
	var resp pluginapi.ManagementResponse
	mustResult(t, raw, &resp)
	return resp
}

func registeredManager(t *testing.T, opener *fakeOpener) *Manager {
	t.Helper()
	m := NewManager(opener, nil)
	m.now = func() time.Time { return managementNow }
	register(t, m, "enabled: true\n")
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

	want := map[string]string{
		"/plugins/cliproxyapi-observer/summary":  http.MethodGet,
		"/plugins/cliproxyapi-observer/requests": http.MethodGet,
		"/plugins/cliproxyapi-observer/body":     http.MethodGet,
		"/plugins/cliproxyapi-observer/settings": http.MethodGet,
		"/plugins/cliproxyapi-observer/health":   http.MethodGet,
		"/plugins/cliproxyapi-observer/validate": http.MethodPost,
	}
	for _, route := range reg.Routes {
		expectedMethod, ok := want[route.Path]
		if !ok {
			t.Errorf("unexpected route %s", route.Path)
			continue
		}
		if route.Method != expectedMethod {
			t.Errorf("route %s method = %s, want %s", route.Path, route.Method, expectedMethod)
		}
		delete(want, route.Path)
	}
	for path := range want {
		t.Errorf("route %s not declared", path)
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

func TestSummaryMinuteAlignedBoundsAndValidation(t *testing.T) {
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
	// The default upper bound is the next minute boundary so the current minute
	// is included, and every bound is minute-aligned.
	if want := managementNow.Truncate(time.Minute).Add(time.Minute); !query.To.Equal(want) {
		t.Errorf("default to = %s, want %s", query.To, want)
	}
	if want := query.To.Add(-defaultQueryWindow); !query.From.Equal(want) {
		t.Errorf("default from = %s, want %s", query.From, want)
	}
	if !query.To.Equal(query.To.Truncate(time.Minute)) || !query.From.Equal(query.From.Truncate(time.Minute)) {
		t.Errorf("summary bounds are not minute-aligned: %+v", query)
	}

	// Explicit, already-aligned bounds are forwarded unchanged (no fractional
	// minute drift), and the filters travel with them.
	values := url.Values{"from": {managementNow.Add(-time.Hour).Format(time.RFC3339)}, "to": {managementNow.Format(time.RFC3339)}, "provider": {"openai"}, "model": {"gpt"}}
	resp = callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", values)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summary status = %d", resp.StatusCode)
	}
	query, _ = opener.last().lastQuery()
	if query.Provider != "openai" || query.Model != "gpt" {
		t.Errorf("filters not forwarded: %+v", query)
	}
	if !query.To.Equal(managementNow) || !query.From.Equal(managementNow.Add(-time.Hour)) {
		t.Errorf("explicit aligned bounds drifted: %+v", query)
	}

	// A range wider than the retained statistics window is rejected.
	broad := url.Values{"from": {managementNow.Add(-48 * time.Hour).Format(time.RFC3339)}, "to": {managementNow.Format(time.RFC3339)}}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", broad); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("broad summary status = %d, want 400", resp.StatusCode)
	}
	// An empty range is rejected rather than silently widening.
	empty := url.Values{"from": {managementNow.Format(time.RFC3339)}, "to": {managementNow.Format(time.RFC3339)}}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", empty); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty summary status = %d, want 400", resp.StatusCode)
	}
	// A future upper bound is rejected.
	future := url.Values{"to": {managementNow.Add(time.Hour).Format(time.RFC3339)}}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", future); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("future summary status = %d, want 400", resp.StatusCode)
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

	opener.last().summaryErr = observer.ErrInvalidQuery
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid query status = %d, want 400", resp.StatusCode)
	}
	opener.last().summaryErr = observer.ErrClosed
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("closed store status = %d, want 503", resp.StatusCode)
	}
}

// Validate the requested interval before rounding it to minute buckets.
// Otherwise legal 24h windows fail, and empty/reversed intervals become valid.
func TestSummaryFractionalMinuteQueryBounds(t *testing.T) {
	cfg := observerConfigFixture()
	cfg.StatsRetentionDays = 1
	opener := &fakeOpener{cfg: cfg}
	m := registeredManager(t, opener)
	now := managementNow.Add(37*time.Second + 123*time.Millisecond)
	m.now = func() time.Time { return now }
	format := func(value time.Time) string { return value.Format(time.RFC3339Nano) }

	for _, tc := range []struct {
		name   string
		from   time.Time
		to     time.Time
		status int
	}{
		{"exact-24h", now.Add(-24 * time.Hour), now, http.StatusOK},
		{"over-24h", now.Add(-24*time.Hour - time.Nanosecond), now, http.StatusBadRequest},
		{"empty", now, now, http.StatusBadRequest},
		{"reversed-same-minute", now.Add(time.Second), now, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{"from": {format(tc.from)}, "to": {format(tc.to)}}
			resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/summary", query)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, resp.Body)
			}
			if tc.status == http.StatusOK {
				got, _ := opener.last().lastQuery()
				if !got.From.Equal(tc.from.Truncate(time.Minute)) || !got.To.Equal(nextMinute(tc.to)) {
					t.Errorf("aligned bounds = %+v", got)
				}
			}
		})
	}
}

func TestRequestsLimitOffsetAndValidation(t *testing.T) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	m := registeredManager(t, opener)

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("requests status = %d", resp.StatusCode)
	}
	query, _ := opener.last().lastQuery()
	if query.Limit != defaultRequestLimit || query.Offset != 0 {
		t.Errorf("default limit/offset = %d/%d, want %d/0", query.Limit, query.Offset, defaultRequestLimit)
	}

	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"limit": {"100"}, "offset": {"50"}}); resp.StatusCode != http.StatusOK {
		t.Errorf("limit 100 offset 50 status = %d", resp.StatusCode)
	}
	query, _ = opener.last().lastQuery()
	if query.Offset != 50 || query.Limit != 100 {
		t.Errorf("offset/limit not forwarded: %+v", query)
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"offset": {"2147483647"}}); resp.StatusCode != http.StatusOK {
		t.Errorf("max offset status = %d", resp.StatusCode)
	}
	query, _ = opener.last().lastQuery()
	if query.Offset != 2147483647 {
		t.Errorf("max offset not forwarded: %+v", query)
	}

	for _, bad := range []string{"0", "101", "abc", "-1"} {
		if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"limit": {bad}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit %q status = %d, want 400", bad, resp.StatusCode)
		}
	}

	for _, bad := range []string{"-1", "2147483648", "99999999999999999999999999", "abc", "1.5", "1e2", "", " 10 "} {
		if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"offset": {bad}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("offset %q status = %d, want 400", bad, resp.StatusCode)
		}
	}

	// Reject nonempty legacy cursor instead of silently resetting to page one.
	for _, legacy := range []string{"abc", "eyJhIjoxfQ", "123"} {
		if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", url.Values{"cursor": {legacy}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("legacy cursor %q status = %d, want 400", legacy, resp.StatusCode)
		}
	}

	// A 7d/30d dashboard selection stays within the statistics retention and
	// must not be rejected even though the request retention is only 24h.
	wide := url.Values{
		"from": {managementNow.Add(-7 * 24 * time.Hour).Format(time.RFC3339)},
		"to":   {managementNow.Format(time.RFC3339)},
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", wide); resp.StatusCode != http.StatusOK {
		t.Errorf("7d requests status = %d, want 200", resp.StatusCode)
	}
	// Beyond the statistics retention the range is rejected.
	tooWide := url.Values{"from": {managementNow.Add(-400 * 24 * time.Hour).Format(time.RFC3339)}}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", tooWide); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("400d requests status = %d, want 400", resp.StatusCode)
	}

	// Only query validation failures are 400; storage failures must not
	// be masked as bad requests.
	opener.last().requestsErr = observer.ErrInvalidQuery
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid query status = %d, want 400", resp.StatusCode)
	}
	opener.last().requestsErr = observer.ErrClosed
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("closed store status = %d, want 503", resp.StatusCode)
	}
	opener.last().requestsErr = errStoreFailure
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/requests", nil); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("store failure status = %d, want 500", resp.StatusCode)
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
	opener.last().bodyErr = observer.ErrClosed
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/body", url.Values{"request_id": {"req-1"}}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("closed store status = %d, want 503", resp.StatusCode)
	}
	opener.last().bodyErr = errStoreFailure
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/body", url.Values{"request_id": {"req-1"}}); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("store failure status = %d, want 500", resp.StatusCode)
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
	cfg.CompactInterval = 30 * time.Minute
	cfg.CompactMinBytes = 16777216
	cfg.Prices = map[string]observer.Price{
		"m1": {Input: 1.0, Output: 2.0},
	}
	opener := &fakeOpener{cfg: cfg}
	m := registeredManager(t, opener)

	resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings status = %d", resp.StatusCode)
	}
	var settings settingsResponse
	decodeBody(t, resp, &settings)
	want := settingsResponse{
		CaptureBodies:           true,
		BodyRetentionSeconds:    43200,
		RequestRetentionSeconds: 21600,
		StatsRetentionDays:      30,
		MaxBodyBytes:            4096,
		MaxBodyStorageBytes:     8192,
		CompactIntervalSeconds:  1800,
		CompactMinBytes:         16777216,
		Prices: map[string]observer.Price{
			"m1": {Input: 1.0, Output: 2.0},
		},
	}
	if !reflect.DeepEqual(settings, want) {
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
		csp := resp.Headers.Get("Content-Security-Policy")
		if csp != cspPolicy {
			t.Errorf("%s CSP = %q", route, csp)
		}
		if !strings.Contains(csp, "base-uri 'none'") || !strings.Contains(csp, "form-action 'none'") {
			t.Errorf("%s CSP missing base-uri or form-action: %q", route, csp)
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
		t.Errorf("POST summary status = %d, want 405", resp.StatusCode)
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/validate", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET validate status = %d, want 405", resp.StatusCode)
	}
	if resp := callManagement(t, m, http.MethodPut, "/v0/management/plugins/cliproxyapi-observer/validate", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT validate status = %d, want 405", resp.StatusCode)
	}
	if resp := callManagement(t, m, http.MethodGet, "/v0/management/plugins/cliproxyapi-observer/unknown", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

// TestManagementValidatePatch covers comprehensive validation behavior for POST /validate:
// ensuring valid patches succeed, invalid payloads are rejected according to the contract,
// manager raw state is merged, and no lifecycle changes or persistence occur.
func TestManagementValidatePatch(t *testing.T) {
	// 1. Valid patches succeed.
	t.Run("valid single field patch", func(t *testing.T) {
		m := registeredManager(t, &fakeOpener{cfg: observerConfigFixture()})
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", []byte(`{"compact-interval": "30m"}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, resp.Body)
		}
		var res map[string]bool
		decodeBody(t, resp, &res)
		if !res["valid"] {
			t.Errorf("response valid = false, want true")
		}
	})

	t.Run("valid comprehensive patch with prices", func(t *testing.T) {
		m := registeredManager(t, &fakeOpener{cfg: observerConfigFixture()})
		body := []byte(`{
			"capture-bodies": true,
			"request-retention": "12h",
			"body-retention": "12h",
			"stats-retention-days": 60,
			"max-body-bytes": 2048,
			"max-body-storage-bytes": 1048576,
			"compact-interval": "20m",
			"compact-min-bytes": 10485760,
			"prices": {
				"claude-3-5-sonnet": {
					"input": 3.0,
					"output": 15.0,
					"cache_read": 0.3,
					"cache_creation": 3.75
				},
				"gpt-4o-mini": {
					"input": 0.15,
					"output": 0.60,
					"cache-read": 0.075,
					"cache-creation": 0.15
				}
			}
		}`)
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, resp.Body)
		}
	})

	// 2. Cross-field validation with manager raw YAML clone.
	t.Run("cross field check with live raw", func(t *testing.T) {
		opener := &fakeOpener{cfg: observerConfigFixture()}
		m := NewManager(opener, nil)
		// Register with an explicit small max-body-storage-bytes
		initialRaw := []byte("max-body-storage-bytes: 1048576\n")
		register(t, m, string(initialRaw))

		// Try to patch max-body-bytes to 2MiB, which exceeds 1MiB storage limit
		patch := []byte(`{"max-body-bytes": 2097152}`)
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", patch)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, resp.Body)
		}
		var errResp map[string]string
		decodeBody(t, resp, &errResp)
		if errResp["error"] != "invalid settings patch" {
			t.Errorf("expected error message %q, got %s", "invalid settings patch", resp.Body)
		}
	})

	// 3. Payload size bound (256KiB).
	t.Run("rejects payload over 256KiB", func(t *testing.T) {
		m := registeredManager(t, &fakeOpener{cfg: observerConfigFixture()})
		oversized := make([]byte, 256*1024+1)
		for i := range oversized {
			oversized[i] = ' '
		}
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", oversized)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
		var errResp map[string]string
		decodeBody(t, resp, &errResp)
		if errResp["error"] != "request body exceeds 256KiB limit" {
			t.Errorf("error = %q, want %q", errResp["error"], "request body exceeds 256KiB limit")
		}
	})

	// 4. Rejection of null, array, non-object, unknown, duplicate, invalid types/bounds, trailing.
	invalidCases := map[string]string{
		"empty body":                        "",
		"whitespace only":                   "   \n  \t ",
		"top level null":                    "null",
		"top level array":                   "[]",
		"top level string":                  `"capture-bodies"`,
		"top level number":                  "123",
		"unknown key":                       `{"unknown": 123}`,
		"schema key enabled not allowed":    `{"enabled": true}`,
		"schema key db not allowed":         `{"db": "data/db.bolt"}`,
		"schema key flush not allowed":      `{"flush": "2s"}`,
		"field value null boolean":          `{"capture-bodies": null}`,
		"field value null duration":         `{"compact-interval": null}`,
		"field value null int":              `{"stats-retention-days": null}`,
		"field value null prices":           `{"prices": null}`,
		"field value array":                 `{"compact-interval": ["15m"]}`,
		"field value array prices":          `{"prices": []}`,
		"duplicate patch key":               `{"compact-interval": "15m", "compact-interval": "20m"}`,
		"type error boolean with string":    `{"capture-bodies": "true"}`,
		"type error int with string":        `{"stats-retention-days": "365"}`,
		"type error int with float":         `{"stats-retention-days": 10.5}`,
		"type error duration with int":      `{"compact-interval": 900}`,
		"bound error compact-interval min":  `{"compact-interval": "30s"}`,
		"bound error compact-interval max":  `{"compact-interval": "25h"}`,
		"bound error compact-min-bytes min": `{"compact-min-bytes": 65535}`,
		"bound error compact-min-bytes max": `{"compact-min-bytes": 8589934593}`,
		"trailing data after json":          `{"compact-interval": "20m"} extra_trailing`,
		"trailing token after json":         `{"compact-interval": "20m"} {"compact-interval": "20m"}`,
		"prices empty model id":             `{"prices": {"": {"input": 1}}}`,
		"prices whitespace model id":        `{"prices": {"   ": {"input": 1}}}`,
		"prices duplicate trimmed model":    `{"prices": {"gpt-4": {"input": 1}, " gpt-4 ": {"input": 2}}}`,
		"prices null model entry":           `{"prices": {"gpt-4": null}}`,
		"prices array model entry":          `{"prices": {"gpt-4": []}}`,
		"prices negative value":             `{"prices": {"gpt-4": {"input": -1}}}`,
		"prices unknown field":              `{"prices": {"gpt-4": {"unknown": 1}}}`,
		"prices trailing in model":          `{"prices": {"gpt-4": {"input": 1} trailing}}`,
	}

	for name, raw := range invalidCases {
		t.Run(name, func(t *testing.T) {
			m := registeredManager(t, &fakeOpener{cfg: observerConfigFixture()})
			resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", []byte(raw))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400 (body = %s)", name, resp.StatusCode, resp.Body)
			}
			var errResp map[string]string
			decodeBody(t, resp, &errResp)
			if errResp["error"] != "invalid settings patch" {
				t.Errorf("%s: error = %q, want %q", name, errResp["error"], "invalid settings patch")
			}
		})
	}

	// 5. Manager raw snapshot availability (503 when no config or corrupt YAML).
	t.Run("service unavailable when no config", func(t *testing.T) {
		opener := &fakeOpener{cfg: observerConfigFixture()}
		m := NewManager(opener, nil)
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", []byte(`{"compact-interval": "20m"}`))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body = %s", resp.StatusCode, resp.Body)
		}
		var errResp map[string]string
		decodeBody(t, resp, &errResp)
		if errResp["error"] != "observer unavailable" {
			t.Errorf("error = %q, want %q", errResp["error"], "observer unavailable")
		}
	})

	t.Run("service unavailable when raw yaml corrupt", func(t *testing.T) {
		opener := &fakeOpener{cfg: observerConfigFixture()}
		m := NewManager(opener, nil)
		register(t, m, ": [invalid yaml\n")
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", []byte(`{"compact-interval": "20m"}`))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body = %s", resp.StatusCode, resp.Body)
		}
		var errResp map[string]string
		decodeBody(t, resp, &errResp)
		if errResp["error"] != "observer unavailable" {
			t.Errorf("error = %q, want %q", errResp["error"], "observer unavailable")
		}
	})

	// 6. Verification of no lifecycle side effects or state mutation.
	t.Run("no lifecycle side effects or mutation", func(t *testing.T) {
		opener := &fakeOpener{cfg: observerConfigFixture()}
		m := registeredManager(t, opener)
		liveStore, liveCfg, liveHasConfig := m.snapshot()

		// Send valid patch
		resp := callManagementWithBody(t, m, http.MethodPost, "/v0/management/plugins/cliproxyapi-observer/validate", []byte(`{"compact-interval": "20m"}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("validate status = %d, want 200", resp.StatusCode)
		}

		afterStore, afterCfg, afterHasConfig := m.snapshot()
		if liveStore != afterStore {
			t.Errorf("store instance mutated during validate: %p != %p", liveStore, afterStore)
		}
		if liveHasConfig != afterHasConfig {
			t.Errorf("hasConfig changed")
		}
		if liveCfg.CompactInterval != afterCfg.CompactInterval {
			t.Errorf("live config mutated: %v -> %v", liveCfg.CompactInterval, afterCfg.CompactInterval)
		}
		if len(opener.opened()) != 1 {
			t.Errorf("opener opened new store count = %d, want 1", len(opener.opened()))
		}
	})
}

// Identity filtering rejects ambiguous or secret-shaped input.
func TestManagementIdentityFilters(t *testing.T) {
	m, _ := newTestManager()
	m.now = func() time.Time { return managementNow }
	fingerprint := strings.Repeat("a", 64)
	query, herr := m.parseQuery(url.Values{"client_key_id": {fingerprint}, "auth_index": {"0123456789abcdef"}}, 24*time.Hour, false, true)
	if herr != nil || query.ClientKeyID != fingerprint || query.AuthIndex != "0123456789abcdef" {
		t.Fatal("identity query not forwarded")
	}
	for _, values := range []url.Values{
		{"client_key_id": {"short"}},
		{"auth_index": {"fake-secret"}},
		{"client_key_id": {strings.Repeat("A", 64)}},
	} {
		if _, herr := m.parseQuery(values, 24*time.Hour, false, true); herr == nil {
			t.Fatal("accepted invalid identity filter")
		}
	}
	if _, herr := m.parseQuery(url.Values{"client_key_id": {"unknown"}}, 24*time.Hour, true, false); herr != nil {
		t.Fatal("summary identity filter rejected")
	}
}

// A rule list is validated in order and aliases cannot disguise duplicate fields.
func TestValidatePriceRuleList(t *testing.T) {
	for _, body := range []string{
		`{"price-rules":[]}`,
		`{"price-rules":[{"model":"m","input-tokens-gt":256000,"time-range":"22:00-06:00","price":{"input":2}}]}`,
		`{"price-rules":[{"model":"m","price":{"input":2}},{"model":"m","price":{"input":1}}]}`,
	} {
		if err := validatePatch(nil, []byte(body)); err != nil {
			t.Fatalf("valid rule rejected %v", err)
		}
	}
	for _, body := range []string{
		`{"price-rules":null}`, `{"price-rules":{}}`,
		`{"price-rules":[{"model":"m","input-tokens-gt":-1,"price":{}}]}`,
		`{"price-rules":[{"model":"m","time-range":"08:00-08:00","price":{}}]}`,
	} {
		if validatePatch(nil, []byte(body)) == nil {
			t.Fatal("invalid rule accepted")
		}
	}
}
