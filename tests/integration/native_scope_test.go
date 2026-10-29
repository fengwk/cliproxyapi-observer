//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"
)

// nativeScopeCase is one native-host request compared between a plugin-disabled
// baseline and the same host with Observer enabled.
type nativeScopeCase struct {
	name    string
	method  string
	path    string
	body    []byte
	withKey bool
	compare func(t *testing.T, control, observed []byte)
}

func nativeRequest(t *testing.T, h *harness, tc nativeScopeCase) (int, []byte) {
	t.Helper()
	var headers map[string]string
	if tc.withKey {
		headers = map[string]string{"Authorization": "Bearer " + clientKey}
	}
	status, body, err := h.rawRequest(tc.method, tc.path, tc.body, headers)
	if err != nil {
		t.Fatalf("%s %s: %v", tc.method, tc.path, err)
	}
	return status, body
}

func nativeJSONEqual(t *testing.T, control, observed []byte) {
	t.Helper()
	if len(bytes.TrimSpace(control)) == 0 || len(bytes.TrimSpace(observed)) == 0 {
		if bytes.Equal(bytes.TrimSpace(control), bytes.TrimSpace(observed)) {
			return
		}
		t.Fatalf("response emptiness changed\nbaseline %q\nobserver %q", control, observed)
	}
	var a, b any
	if err := json.Unmarshal(control, &a); err != nil {
		t.Fatalf("baseline JSON: %v (%s)", err, truncate(control, 300))
	}
	if err := json.Unmarshal(observed, &b); err != nil {
		t.Fatalf("observer JSON: %v (%s)", err, truncate(observed, 300))
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("response changed with Observer enabled\nbaseline %s\nobserver %s", truncate(control, 500), truncate(observed, 500))
	}
}

func nativeOpenAIModelIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("model catalog JSON: %v (%s)", err, truncate(raw, 300))
	}
	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func nativeOpenAIModelCatalogEqual(t *testing.T, control, observed []byte) {
	t.Helper()
	baseline := nativeOpenAIModelIDs(t, control)
	ids := nativeOpenAIModelIDs(t, observed)
	if !reflect.DeepEqual(baseline, ids) {
		t.Fatalf("native model catalog changed\nbaseline %v\nobserver %v", baseline, ids)
	}
	want := []string{modelName, modelNameTwo}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("native model catalog = %v, want %v", ids, want)
	}
}

func nativeGeminiModelNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Gemini model catalog JSON: %v (%s)", err, truncate(raw, 300))
	}
	names := make([]string, 0, len(payload.Models))
	for _, item := range payload.Models {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}

func nativeGeminiModelCatalogEqual(t *testing.T, control, observed []byte) {
	t.Helper()
	baseline := nativeGeminiModelNames(t, control)
	names := nativeGeminiModelNames(t, observed)
	if !reflect.DeepEqual(baseline, names) {
		t.Fatalf("native Gemini catalog changed\nbaseline %v\nobserver %v", baseline, names)
	}
	want := []string{"models/" + modelName, "models/" + modelNameTwo}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("native Gemini catalog = %v, want %v", names, want)
	}
}

// Model creation timestamps come from each host's config load, so compare the
// stable identity fields while still rejecting any meaningful response change.
func nativeModelDetailEqual(t *testing.T, control, observed []byte) {
	t.Helper()
	var a, b map[string]any
	if err := json.Unmarshal(control, &a); err != nil {
		t.Fatalf("baseline detail JSON: %v", err)
	}
	if err := json.Unmarshal(observed, &b); err != nil {
		t.Fatalf("observer detail JSON: %v", err)
	}
	delete(a, "created")
	delete(b, "created")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("native model detail changed\nbaseline %v\nobserver %v", a, b)
	}
}

// Native CPA keys must retain exactly the host's original authentication and
// response behavior on every supported surface while Observer is enabled. The
// same host without the plugin supplies the authoritative baseline.
func TestNativeAuthenticationScopeCoexistence(t *testing.T) {
	baseline := newHarnessWith(t, hostOptions{PluginDisabled: true})
	h := newHarness(t)

	cases := []nativeScopeCase{
		{name: "health", method: http.MethodGet, path: "/healthz", compare: nativeJSONEqual},
		{name: "models list", method: http.MethodGet, path: "/v1/models", withKey: true, compare: nativeOpenAIModelCatalogEqual},
		{name: "model detail", method: http.MethodGet, path: "/v1/models/" + modelName, withKey: true, compare: nativeModelDetailEqual},
		{name: "Gemini models list", method: http.MethodGet, path: "/v1beta/models", withKey: true, compare: nativeGeminiModelCatalogEqual},
		{name: "Gemini model detail", method: http.MethodGet, path: "/v1beta/models/" + modelName, withKey: true, compare: nativeJSONEqual},
		{name: "token count", method: http.MethodPost, path: "/v1/messages/count_tokens", body: chatBody(false), withKey: true, compare: nativeJSONEqual},
		{name: "generation", method: http.MethodPost, path: "/v1/chat/completions", body: chatBody(false), withKey: true, compare: nativeJSONEqual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controlStatus, controlBody := nativeRequest(t, baseline, tc)
			status, body := nativeRequest(t, h, tc)
			if controlStatus != status {
				t.Fatalf("status baseline=%d observer=%d body=%s", controlStatus, status, truncate(body, 300))
			}
			if status != http.StatusOK {
				t.Fatalf("native request status=%d body=%s, want 200", status, truncate(body, 300))
			}
			tc.compare(t, controlBody, body)
		})
	}

	// Native traffic must not create Observer Keys, executions or active slots.
	h.await(t, "native traffic creates no Observer accounting", func() bool {
		var health acceptanceHealth
		h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
		return len(h.keys(t)) == 0 && len(h.page(t, "page_size=100").Items) == 0 &&
			health.ActiveRequests == 0
	})

	// Only the native generation reaches an upstream. Its bytes and headers must
	// be identical to the uninstrumented CPA baseline.
	controlCalls := baseline.mock.callsSnapshot()
	observedCalls := h.mock.callsSnapshot()
	if len(controlCalls) != 1 || len(observedCalls) != 1 {
		t.Fatalf("native upstream attempts baseline=%d observer=%d, want one each", len(controlCalls), len(observedCalls))
	}
	control, observed := controlCalls[0], observedCalls[0]
	if !bytes.Equal(control.Body, observed.Body) {
		t.Fatalf("Observer altered native upstream bytes\nbaseline %s\nobserver %s", control.Body, observed.Body)
	}
	if !reflect.DeepEqual(control.Header, observed.Header) {
		t.Fatalf("Observer altered native upstream headers\nbaseline %v\nobserver %v", control.Header, observed.Header)
	}
	h.mock.requireClean(t)
	baseline.mock.requireClean(t)

	t.Run("management authentication unchanged", func(t *testing.T) {
		for _, probe := range []struct {
			name   string
			path   string
			secret string
			want   int
		}{
			{"host management rejects native key", "/v0/management/config", clientKey, http.StatusUnauthorized},
			{"host management accepts management key", "/v0/management/config", mgmtKey, http.StatusOK},
			{"Observer management rejects native key", "/v0/management/plugins/" + pluginID + "/health", clientKey, http.StatusUnauthorized},
			{"Observer management accepts management key", "/v0/management/plugins/" + pluginID + "/health", mgmtKey, http.StatusOK},
		} {
			status, body, err := h.rawRequest(http.MethodGet, probe.path, nil, map[string]string{"Authorization": "Bearer " + probe.secret})
			if err != nil || status != probe.want {
				t.Fatalf("%s status=%d err=%v body=%s", probe.name, status, err, truncate(body, 300))
			}
		}

		// The management secret remains distinct from a frontend client key.
		before := h.mock.callCount()
		status, body := h.infer(t, mgmtKey, false)
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("management key admitted inference status=%d body=%s", status, truncate(body, 300))
		}
		if h.mock.callCount() != before {
			t.Fatal("management key inference reached the upstream")
		}
		status, _, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + mgmtKey})
		if err != nil || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
			t.Fatalf("management key admitted catalog status=%d err=%v", status, err)
		}
	})

	// Path tricks and unsupported method/path combinations stay with CPA's own
	// status and response. Observer must not convert malformed paths into an
	// authenticated 200.
	t.Run("malformed paths keep CPA behavior", func(t *testing.T) {
		for _, tc := range []nativeScopeCase{
			{name: "dot segment", method: http.MethodGet, path: "/v1/models/..", withKey: true},
			{name: "encoded traversal", method: http.MethodGet, path: "/v1/models/%2e%2e/keys", withKey: true},
			{name: "dot relative model", method: http.MethodGet, path: "/v1/models/./" + modelName, withKey: true},
			{name: "nested detail", method: http.MethodGet, path: "/v1/models/" + modelName + "/extra", withKey: true},
			{name: "wrong method on catalog", method: http.MethodPost, path: "/v1/models", withKey: true},
			{name: "missing Gemini action", method: http.MethodPost, path: "/v1beta/models/foo", withKey: true},
			{name: "unknown Gemini model count", method: http.MethodPost, path: "/v1beta/models/foo:countTokens", body: geminiCountBody(), withKey: true},
			{name: "GET on Gemini action", method: http.MethodGet, path: "/v1beta/models/foo:countTokens", withKey: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				controlStatus, controlBody := nativeRequest(t, baseline, tc)
				status, body := nativeRequest(t, h, tc)
				if controlStatus != status {
					t.Fatalf("status baseline=%d observer=%d body=%s", controlStatus, status, truncate(body, 300))
				}
				if status == http.StatusOK {
					t.Fatalf("malformed path was accepted with status 200: %s", truncate(body, 300))
				}
				nativeJSONEqual(t, controlBody, body)
			})
		}
	})
}

// Pin the SDK limitation instead of "fixing" anonymous access by returning
// universal auth success. Registering a provider creates an authentication
// chain even when the host has no native API keys.
func TestSDKAnonymousAuthenticationBoundary(t *testing.T) {
	baseline := newHarnessWith(t, hostOptions{PluginDisabled: true, NoNativeKeys: true})
	h := newHarnessWith(t, hostOptions{NoNativeKeys: true})
	for _, probe := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/v1/models", nil},
		{http.MethodPost, "/v1/chat/completions", chatBody(false)},
	} {
		status, _, err := baseline.rawRequest(probe.method, probe.path, probe.body, nil)
		if err != nil || status != 200 {
			t.Fatal("anonymous baseline changed", status, err)
		}
		status, _, err = h.rawRequest(probe.method, probe.path, probe.body, nil)
		if err != nil || status != 401 {
			t.Fatal("SDK boundary changed; review before promising anonymous coexistence", status, err)
		}
	}
	created := h.createKey(t, validSpec())
	if status, _ := h.infer(t, created.Secret, false); status != 200 {
		t.Fatal("owned authentication unavailable without native keys")
	}
}

// Observer cannot invalidate the host's independent credential of the same
// plaintext value. This is a deployment boundary, not permission to read the
// host's secret store or force exclusive authentication.
func TestDuplicateNativeAndObserverSecretBoundary(t *testing.T) {
	h := newHarness(t)
	spec := map[string]any{"label": "duplicate-boundary", "enabled": false, "max_concurrent": 1,
		"allowed_models": []string{}, "limits": limitsView{}, "secret": clientKey}
	var created createdKey
	h.api(t, http.MethodPost, "/keys", spec, 201, &created)
	for _, deleted := range []bool{false, true} {
		if deleted {
			h.api(t, http.MethodPost, "/keys/delete?id="+created.Key.ID, nil, 204, nil)
		}
		if status, body := h.infer(t, clientKey, false); status != 200 || jsonContent(body) != fixtureText {
			t.Fatal("Observer took over the host's independent native credential", status)
		}
	}
	if records := h.page(t, "page_size=100"); len(records.Items) != 0 {
		t.Fatal("disabled/deleted duplicate entered owned ledger")
	}
}
