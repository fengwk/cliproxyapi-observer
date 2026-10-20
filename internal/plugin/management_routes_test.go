package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func dispatchForTest(t *testing.T, m *Manager, method, path string, query url.Values, body string) pluginapi.ManagementResponse {
	t.Helper()
	var raw []byte
	if body != "" {
		raw = []byte(body)
	}
	return m.dispatchManagement(managementRequest{ManagementRequest: pluginapi.ManagementRequest{
		Method: method,
		Path:   path,
		Query:  query,
		Body:   raw,
	}})
}

// The host only dispatches exact static method+path keys. Dynamic-looking
// routes must be absent from registration, and /ui/ must be declared even
// though the host normalizes its trailing slash.
func TestManagementRegistrationHasOnlyStaticExactRoutes(t *testing.T) {
	reg := managementRegistration()
	want := map[string]bool{
		"POST " + routeModelsToken:     true,
		"GET " + routeHealth:           true,
		"GET " + routeKeys:             true,
		"POST " + routeKeys:            true,
		"PUT " + routeKeys:             true,
		"DELETE " + routeKeys:          true,
		"POST " + routeKeyDelete:       true,
		"POST " + routeKeyRotate:       true,
		"POST " + routeKeyReset:        true,
		"POST " + routeKeyRecover:      true,
		"GET " + routeRequests:         true,
		"GET " + routeRequestView:      true,
		"GET " + routeRequestFacets:    true,
		"GET " + routeRequestDetail:    true,
		"GET " + routeSummary:          true,
		"GET " + routeSettings:         true,
		"PUT " + routeSettings:         true,
		"GET " + routePrices:           true,
		"POST " + routePricesRefresh:   true,
		"POST " + routePricesPreview:   true,
		"POST " + routeCapturesCleanup: true,
		"POST " + routeCapturesPurge:   true,
		"POST " + routeCompact:         true,
		"GET " + routeUI:               true,
		"GET " + routeUIAlt:            true,
		"GET " + routeUIJS:             true,
		"GET " + routeUICSS:            true,
		"GET " + routeThemeJS:          true,
	}

	seen := map[string]bool{}
	for _, route := range reg.Routes {
		key := route.Method + " " + route.Path
		if seen[key] {
			t.Fatalf("duplicate registration: %s", key)
		}
		seen[key] = true
		if !want[key] {
			t.Fatalf("unexpected registration: %s", key)
		}
		if strings.ContainsAny(route.Path, ":*{}") {
			t.Fatalf("dynamic route registered: %s", route.Path)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Fatalf("missing registration: %s", key)
		}
	}

	resources := map[string]bool{}
	for _, resource := range reg.Resources {
		if resources[resource.Path] {
			t.Fatalf("duplicate resource: %s", resource.Path)
		}
		resources[resource.Path] = true
	}
	for _, path := range []string{"/ui", "/ui/", "/ui.js", "/ui.css"} {
		if !resources[path] {
			t.Fatalf("missing resource: %s", path)
		}
	}
}

// Every static route is exercised through the real management dispatcher. A
// live manager covers storage-backed routes; refresh is checked against a
// manager without a pricing service so the test never performs network I/O.
func TestRegisteredManagementRoutesDispatch(t *testing.T) {
	m := liveManager(t)
	assets := map[string]string{
		"ui.html": "<html>ui</html>",
		"ui.js":   "console.log('ui')",
		"ui.css":  "body{}",
	}
	m.assets = func(name string) ([]byte, string, bool) {
		content, ok := assets[name]
		return []byte(content), "text/plain", ok
	}

	if r := dispatchForTest(t, m, http.MethodGet, managementBasePath+"/health", nil, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("health: %d %s", r.StatusCode, r.Body)
	}

	var created keySecretResponse
	r := dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys", nil,
		`{"label":"routes","enabled":true,"max_concurrent":2,"limits":{"total":1000000}}`)
	if r.StatusCode != http.StatusCreated || json.Unmarshal(r.Body, &created) != nil || created.Key.ID == "" || created.Secret == "" {
		t.Fatalf("create key: %d %s", r.StatusCode, r.Body)
	}
	id := created.Key.ID

	var listed keysResponse
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/keys", nil, ""); r.StatusCode != http.StatusOK ||
		json.Unmarshal(r.Body, &listed) != nil || len(listed.Items) != 1 || listed.Items[0].ID != id {
		t.Fatalf("list keys: %d %s", r.StatusCode, r.Body)
	}

	var updated engine.Key
	r = dispatchForTest(t, m, http.MethodPut, managementBasePath+"/keys", url.Values{"id": {id}},
		`{"label":"routes-updated","enabled":true,"max_concurrent":1,"limits":{"total":2000000}}`)
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &updated) != nil ||
		updated.ID != id || updated.Label != "routes-updated" || updated.MaxConcurrent != 1 {
		t.Fatalf("update key: %d %s", r.StatusCode, r.Body)
	}

	var rotated keySecretResponse
	r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys/rotate", url.Values{"id": {id}}, "")
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &rotated) != nil || rotated.Secret == "" || rotated.Key.ID != id {
		t.Fatalf("rotate key: %d %s", r.StatusCode, r.Body)
	}

	r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys/reset", url.Values{"id": {id}},
		`{"windows":["five_hour","week"]}`)
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &updated) != nil || updated.ID != id {
		t.Fatalf("reset key: %d %s", r.StatusCode, r.Body)
	}

	r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys/recover", url.Values{"id": {id}},
		`{"acknowledge":true}`)
	if r.StatusCode != http.StatusOK || json.Unmarshal(r.Body, &updated) != nil || updated.ID != id {
		t.Fatalf("recover key: %d %s", r.StatusCode, r.Body)
	}

	rangeQuery := url.Values{
		"from": {"2020-01-01T00:00:00Z"},
		"to":   {"2030-01-01T00:00:00Z"},
	}
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests", rangeQuery, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("requests: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/requests/detail", url.Values{"id": {"missing"}}, ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("request detail: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/summary", rangeQuery, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("summary: %d %s", r.StatusCode, r.Body)
	}

	var settings engine.Settings
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/settings", nil, ""); r.StatusCode != http.StatusOK ||
		json.Unmarshal(r.Body, &settings) != nil {
		t.Fatalf("settings: %d %s", r.StatusCode, r.Body)
	}
	settingsBody, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if r = dispatchForTest(t, m, http.MethodPut, managementBasePath+"/settings", nil, string(settingsBody)); r.StatusCode != http.StatusOK {
		t.Fatalf("update settings: %d %s", r.StatusCode, r.Body)
	}

	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/prices", nil, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("prices: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, NewManager(nil), http.MethodPost, managementBasePath+"/prices/refresh", nil, ""); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable price refresh: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/captures/cleanup", nil, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("cleanup captures: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/captures/purge", nil, `{"confirm":true}`); r.StatusCode != http.StatusOK {
		t.Fatalf("purge captures: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/compact", nil, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("compact: %d %s", r.StatusCode, r.Body)
	}

	deleted := created
	deleted.Key.ID = ""
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys", nil,
		`{"label":"terminal","enabled":true}`); r.StatusCode != http.StatusCreated || json.Unmarshal(r.Body, &deleted) != nil {
		t.Fatalf("create deleted key: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys/delete", url.Values{"id": {deleted.Key.ID}}, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete key: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys/delete", url.Values{"id": {deleted.Key.ID}}, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent delete key: %d %s", r.StatusCode, r.Body)
	}
	var defaultKeys, allKeys keysResponse
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/keys", nil, ""); r.StatusCode != http.StatusOK ||
		json.Unmarshal(r.Body, &defaultKeys) != nil || len(defaultKeys.Items) != 1 {
		t.Fatalf("default keys: %d %s", r.StatusCode, r.Body)
	}
	if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+"/keys", url.Values{"include_deleted": {"true"}}, ""); r.StatusCode != http.StatusOK ||
		json.Unmarshal(r.Body, &allKeys) != nil || len(allKeys.Items) != 2 {
		t.Fatalf("include deleted keys: %d %s", r.StatusCode, r.Body)
	}
	foundDeleted := false
	for _, key := range allKeys.Items {
		if key.ID == deleted.Key.ID && key.DeletedAt != nil {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatal("deleted key missing from include_deleted listing")
	}

	for _, path := range []string{"/ui", "/ui/", "/ui.js", "/ui.css"} {
		if r = dispatchForTest(t, m, http.MethodGet, managementBasePath+path, nil, ""); r.StatusCode != http.StatusOK {
			t.Fatalf("management asset %s: %d %s", path, r.StatusCode, r.Body)
		}
		if r = dispatchForTest(t, m, http.MethodGet, resourceBasePath+path, nil, ""); r.StatusCode != http.StatusOK {
			t.Fatalf("resource asset %s: %d %s", path, r.StatusCode, r.Body)
		}
	}

	if r = dispatchForTest(t, m, http.MethodDelete, managementBasePath+"/keys", url.Values{"id": {id}}, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke key: %d %s", r.StatusCode, r.Body)
	}
}

// IDs are only accepted as exactly one nonempty, safe "id" query value.
// Unknown or repeated parameters must be rejected before any store access.
func TestManagementIDQueryValidation(t *testing.T) {
	m := NewManager(nil)
	cases := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   string
	}{
		{"put duplicate id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a", "b"}}, `{}`},
		{"put repeated same id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a", "a"}}, `{}`},
		{"put missing id", http.MethodPut, managementBasePath + "/keys", nil, `{}`},
		{"put unknown parameter", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a"}, "extra": {"1"}}, `{}`},
		{"put uppercase parameter", http.MethodPut, managementBasePath + "/keys", url.Values{"ID": {"a"}}, `{}`},
		{"put empty id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {""}}, `{}`},
		{"put padded id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {" a"}}, `{}`},
		{"put spaced id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a b"}}, `{}`},
		{"put slash id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a/b"}}, `{}`},
		{"put backslash id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {`a\b`}}, `{}`},
		{"put percent id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a%b"}}, `{}`},
		{"put dot id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {".."}}, `{}`},
		{"put newline id", http.MethodPut, managementBasePath + "/keys", url.Values{"id": {"a\nb"}}, `{}`},
		{"delete duplicate id", http.MethodDelete, managementBasePath + "/keys", url.Values{"id": {"a", "b"}}, ""},
		{"delete unknown parameter", http.MethodDelete, managementBasePath + "/keys", url.Values{"id": {"a"}, "x": {"1"}}, ""},
		{"terminal delete duplicate id", http.MethodPost, managementBasePath + "/keys/delete", url.Values{"id": {"a", "b"}}, ""},
		{"terminal delete missing id", http.MethodPost, managementBasePath + "/keys/delete", nil, ""},
		{"terminal delete unknown parameter", http.MethodPost, managementBasePath + "/keys/delete", url.Values{"id": {"a"}, "x": {"1"}}, ""},
		{"terminal delete nonempty body", http.MethodPost, managementBasePath + "/keys/delete", url.Values{"id": {"a"}}, `{}`},
		{"rotate duplicate id", http.MethodPost, managementBasePath + "/keys/rotate", url.Values{"id": {"a", "b"}}, ""},
		{"rotate missing id", http.MethodPost, managementBasePath + "/keys/rotate", nil, ""},
		{"reset duplicate id", http.MethodPost, managementBasePath + "/keys/reset", url.Values{"id": {"a", "b"}}, `{"windows":["week"]}`},
		{"reset missing id", http.MethodPost, managementBasePath + "/keys/reset", nil, `{"windows":["week"]}`},
		{"reset unknown parameter", http.MethodPost, managementBasePath + "/keys/reset", url.Values{"id": {"a"}, "x": {"1"}}, `{"windows":["week"]}`},
		{"recover duplicate id", http.MethodPost, managementBasePath + "/keys/recover", url.Values{"id": {"a", "b"}}, `{"acknowledge":true}`},
		{"recover missing id", http.MethodPost, managementBasePath + "/keys/recover", nil, `{"acknowledge":true}`},
		{"detail duplicate id", http.MethodGet, managementBasePath + "/requests/detail", url.Values{"id": {"a", "b"}}, ""},
		{"detail unknown parameter", http.MethodGet, managementBasePath + "/requests/detail", url.Values{"id": {"a"}, "x": {"1"}}, ""},
		{"detail missing id", http.MethodGet, managementBasePath + "/requests/detail", nil, ""},
		{"detail empty id", http.MethodGet, managementBasePath + "/requests/detail", url.Values{"id": {""}}, ""},
		{"list keys query", http.MethodGet, managementBasePath + "/keys", url.Values{"id": {"a"}}, ""},
		{"list keys repeated include_deleted", http.MethodGet, managementBasePath + "/keys", url.Values{"include_deleted": {"true", "false"}}, ""},
		{"list keys invalid include_deleted", http.MethodGet, managementBasePath + "/keys", url.Values{"include_deleted": {"1"}}, ""},
		{"list keys unknown with include_deleted", http.MethodGet, managementBasePath + "/keys", url.Values{"include_deleted": {"true"}, "x": {"1"}}, ""},
		{"create key query", http.MethodPost, managementBasePath + "/keys", url.Values{"id": {"a"}}, `{}`},
		{"cleanup query", http.MethodPost, managementBasePath + "/captures/cleanup", url.Values{"x": {"1"}}, ""},
		{"cleanup body", http.MethodPost, managementBasePath + "/captures/cleanup", nil, `{}`},
		{"purge query", http.MethodPost, managementBasePath + "/captures/purge", url.Values{"x": {"1"}}, `{"confirm":true}`},
		{"purge missing confirmation", http.MethodPost, managementBasePath + "/captures/purge", nil, `{}`},
		{"purge false confirmation", http.MethodPost, managementBasePath + "/captures/purge", nil, `{"confirm":false}`},
		{"purge unknown field", http.MethodPost, managementBasePath + "/captures/purge", nil, `{"confirm":true,"extra":true}`},
		{"purge empty body", http.MethodPost, managementBasePath + "/captures/purge", nil, ""},
		{"health query", http.MethodGet, managementBasePath + "/health", url.Values{"x": {"1"}}, ""},
		{"settings query", http.MethodGet, managementBasePath + "/settings", url.Values{"x": {"1"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dispatchForTest(t, m, tc.method, tc.path, tc.query, tc.body)
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: got %d %s", tc.method, tc.path, r.StatusCode, r.Body)
			}
		})
	}
}

// No parameterized path survives the static registration change. The
// dispatcher treats any such path as not found rather than routing by prefix.
func TestNoDynamicManagementRouteLeftovers(t *testing.T) {
	m := NewManager(nil)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, managementBasePath + "/keys/key-id"},
		{http.MethodDelete, managementBasePath + "/keys/key-id"},
		{http.MethodPost, managementBasePath + "/keys/key-id/rotate"},
		{http.MethodPost, managementBasePath + "/keys/key-id/reset"},
		{http.MethodGet, managementBasePath + "/requests/request-id"},
		{http.MethodGet, managementBasePath + "/requests/../keys"},
		{http.MethodGet, managementBasePath + "/keys//rotate"},
	} {
		if r := dispatchForTest(t, m, tc.method, tc.path, nil, ""); r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s: got %d %s", tc.method, tc.path, r.StatusCode, r.Body)
		}
	}
}

// The single token pass must reject duplicate object keys at every depth,
// including nested pricing rates, while still accepting repeated array values.
func TestNestedJSONDuplicateAndUnsafeNumberValidation(t *testing.T) {
	invalid := []string{
		`{"label":"a","label":"b"}`,
		`{"limits":{"total":1,"total":2}}`,
		`{"expires_at":null,"expires_at":null}`,
		`{"pricing":{"auto":true,"prices":[],"rules":[],"rules":[]}}`,
		`{"pricing":{"prices":[]},"pricing":{"auto":true}}`,
		`{"pricing":{"auto":true,"prices":[{"model":"m","model":"n","rates":{}}],"rules":[]}}`,
		`{"pricing":{"auto":true,"prices":[{"model":"m","rates":{"input":1,"input":2}}],"rules":[]}}`,
		`{"pricing":{"auto":true,"prices":[],"rules":[{"id":"r","model":"m","rates":{"output":1,"output":2}}]}}`,
		`{"limits":{"total":9000000000000001}}`,
		`{"limits":{"total":-1}}`,
		`{"limits":{"total":1.5}}`,
		`{"limits":{"total":1e2}}`,
		`{"pricing":{"auto":true,"prices":[{"model":"m","rates":{"input":9000000000000001}}],"rules":[]}}`,
	}
	for _, raw := range invalid {
		if err := validateJSONDocument([]byte(raw), nil); err == nil {
			t.Fatalf("accepted invalid document: %s", raw)
		}
	}

	valid := []string{
		`{"label":"a","limits":{"five_hour":0,"week":1,"month":2,"total":9000000000000000}}`,
		`{"windows":["five_hour","week","five_hour"]}`,
		`{"pricing":{"auto":true,"prices":[],"rules":[]}}`,
		`{"pricing":{"auto":true,"prices":[{"model":"m","rates":{"input":1,"output":2}}],"rules":[{"id":"r","model":"m","rates":{"input":3}}]}}`,
	}
	for _, raw := range valid {
		if err := validateJSONDocument([]byte(raw), nil); err != nil {
			t.Fatalf("rejected valid document %s: %v", raw, err)
		}
	}

	// The same pass also enforces the management request top-level allowlist.
	if err := validateJSONDocument([]byte(`{"Method":"GET","Method":"POST"}`), managementRequestKeys); err == nil {
		t.Fatal("accepted duplicate top-level management key")
	}
	if err := validateJSONDocument([]byte(`{"Unknown":"GET"}`), managementRequestKeys); err == nil {
		t.Fatal("accepted unknown top-level management key")
	}
}

// Query ranges are half-open. Equal endpoints describe an empty interval and
// must be rejected, while a strictly ordered range remains valid.
func TestManagementQueryRejectsEqualRange(t *testing.T) {
	m := NewManager(nil)
	same := "2026-01-01T00:00:00Z"
	if _, err := m.parseQuery(url.Values{"from": {same}, "to": {same}}); err == nil {
		t.Fatal("accepted from == to")
	}
	if _, err := m.parseQuery(url.Values{
		"from": {"2026-01-01T00:00:00Z"},
		"to":   {"2026-01-02T00:00:00Z"},
	}); err != nil {
		t.Fatalf("rejected ordered range: %v", err)
	}
}

// New lifecycle sentinels must map to distinct, sanitized management statuses.
// Maintenance during admission is a 503, never the old default 403.
func TestLifecycleErrorMappings(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{engine.ErrDeleted, http.StatusGone, "deleted"},
		{engine.ErrUnsupported, http.StatusConflict, "unsupported"},
		{engine.ErrMaintenance, http.StatusConflict, "maintenance"},
	} {
		r := storeFailureResponse(tc.err)
		if r.StatusCode != tc.status || !strings.Contains(string(r.Body), `"code":"`+tc.code+`"`) {
			t.Fatalf("%v: %d %s", tc.err, r.StatusCode, r.Body)
		}
		if strings.Contains(string(r.Body), tc.err.Error()+":") {
			t.Fatalf("raw error leaked: %s", r.Body)
		}
	}
	if got := denialStatus(engine.ErrMaintenance); got != http.StatusServiceUnavailable {
		t.Fatalf("maintenance admission status: %d", got)
	}
}
