package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func call(t *testing.T, m *Manager, method string, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.HandleCall(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &env) != nil || !env.OK {
		t.Fatalf("%s failed: %s", method, out)
	}
	return env.Result
}

func liveManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(nil)
	path := filepath.Join(t.TempDir(), "observer.db")
	call(t, m, "plugin.register", lifecycleRequest{[]byte("db: " + path), 6})
	t.Cleanup(func() { call(t, m, "plugin.shutdown", nil) })
	return m
}

// Schema refusal must precede opening storage. Observer never claims global
// exclusivity, so disabled/faulted storage only lets its own recognized scopes
// fail closed while native host authentication continues.
func TestNoGlobalExclusivityAndUnavailable(t *testing.T) {
	m := NewManager(nil)
	out, _ := m.HandleCall("plugin.register", []byte(`{"schema_version":5}`))
	if !strings.Contains(string(out), `"ok":false`) {
		t.Fatal("old schema accepted")
	}
	reg := call(t, m, "plugin.register", lifecycleRequest{[]byte("enabled: false"), 6})
	var r registration
	if json.Unmarshal(reg, &r) != nil || r.SchemaVersion != 6 || len(r.Capabilities) != 5 ||
		r.Capabilities["frontend_auth_provider_exclusive"] || !r.Capabilities["frontend_auth_provider"] {
		t.Fatalf("registration: %s", reg)
	}
	auth := call(t, m, "frontend_auth.authenticate", pluginapi.FrontendAuthRequest{
		Method: http.MethodPost, Path: "/v1/chat/completions",
		Headers: http.Header{"Authorization": {"Bearer fake-secret"}},
	})
	if strings.Contains(string(auth), "fake-secret") || strings.Contains(string(auth), `"Authenticated":true`) {
		t.Fatal("invalid Key was globally accepted")
	}
	// No scope on an unknown path: pass through untouched even with no store.
	var before pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{}), &before)
	if before.Terminate {
		t.Fatal("unowned admission rejected")
	}
	// A scope issued by this plugin still fails closed when storage is absent.
	id := strings.Repeat("a", 64)
	own := session.CallerScope("observer:" + id)
	m.scopes.Store(own, callerScope{KeyID: id, Operation: operationGeneration})
	_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "own", Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: own, coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}}), &before)
	if !before.Terminate || before.StatusCode != 503 {
		t.Fatal("owned admission did not fail closed")
	}
	// A storage fault fails registration; it must not install an exclusive
	// provider that would shadow the host's native Keys.
	faulted := NewManager(nil)
	faultedRaw, _ := json.Marshal(lifecycleRequest{[]byte("db: " + t.TempDir()), 6})
	faultedOut, _ := faulted.HandleCall("plugin.register", faultedRaw)
	if !strings.Contains(string(faultedOut), `"ok":false`) || !strings.Contains(string(faultedOut), "storage_unavailable") || faulted.store != nil {
		t.Fatalf("storage fault registration: %s", faultedOut)
	}
}

// Authenticate a generated Key, then exercise the real host caller_scope
// projection of Gin userApiKey. Before/completion IDs are admission IDs; usage
// execution IDs are deliberately not used to release concurrency.
func TestAdmissionRotationAndResume(t *testing.T) {
	m := liveManager(t)
	spec := engine.KeySpec{Label: "test", Enabled: true, MaxConcurrent: 1, AllowedModels: []string{"model/full"}}
	k, secret, err := m.store.CreateKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func(s string) pluginapi.FrontendAuthResponse {
		var a pluginapi.FrontendAuthResponse
		_ = json.Unmarshal(call(t, m, "frontend_auth.authenticate", pluginapi.FrontendAuthRequest{
			Method: http.MethodPost, Path: "/v1/chat/completions",
			Headers: http.Header{"Authorization": {"Bearer " + s}},
		}), &a)
		return a
	}
	if !authenticate(secret).Authenticated {
		t.Fatal("generated Key rejected")
	}
	principal := "observer:" + k.ID
	req := pluginapi.RequestInterceptRequest{RequestID: "admission-1", TraceID: "trace", RequestedModel: "model/full", Body: []byte(`{"secret":"must-not-echo"}`), Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal),
		coreexecutor.GenerateMetadataKey:    true,
		coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}}
	before := func(r pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
		var b pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", r), &b)
		if len(b.Body) != 0 || len(b.Headers) != 0 {
			t.Fatal("interceptor rewrote inference")
		}
		return b
	}
	if before(req).Terminate || before(req).Terminate || m.store.Health().ActiveRequests != 1 {
		t.Fatal("admission replay not idempotent")
	}
	// Config changes cannot close storage while an admitted request is live.
	activeConfig := lifecycleRequest{[]byte("enabled: false\ndb: " + m.cfg.DB), 6}
	activeRaw, _ := json.Marshal(activeConfig)
	activeOut, _ := m.HandleCall("plugin.reconfigure", activeRaw)
	if !strings.Contains(string(activeOut), `"ok":false`) || m.store.Health().ActiveRequests != 1 {
		t.Fatal("reconfigure closed an active request")
	}
	other := req
	other.RequestID = "admission-2"
	if b := before(other); !b.Terminate || b.StatusCode != 429 {
		t.Fatal("concurrency not enforced")
	}
	bad := other
	bad.RequestedModel = "forbidden"
	if !before(bad).Terminate {
		t.Fatal("model restriction bypassed")
	}
	other.Metadata = map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal), coreexecutor.GenerateMetadataKey: false, coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"}
	if b := before(other); !b.Terminate || b.StatusCode != 429 || m.store.Health().ActiveRequests != 1 {
		t.Fatal("Generate false bypassed generation concurrency")
	}
	out, _ := m.HandleCall("plugin.quiesce", nil)
	if !strings.Contains(string(out), `"ok":false`) {
		t.Fatal("quiesce closed live admission")
	}
	// Completion events also arrive for native requests. Only an admission this
	// plugin created may release its own slot.
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "native-request", CompletedAt: time.Now()})
	if m.store.Health().ActiveRequests != 1 {
		t.Fatal("foreign completion released an owned slot")
	}
	for i := 0; i < 2; i++ {
		call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: req.RequestID, CompletedAt: time.Now()})
	}
	if m.store.Health().ActiveRequests != 0 {
		t.Fatal("completion leaked slot")
	}
	_, rotated, err := m.store.RotateKey(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authenticate(secret).Authenticated || !authenticate(rotated).Authenticated {
		t.Fatal("rotation did not invalidate old Key")
	}
	cfg := m.cfg
	previousStore, previousCancel := m.store, m.cancel
	call(t, m, "plugin.register", lifecycleRequest{[]byte("db: " + cfg.DB), 6})
	if m.store != previousStore || m.cancel == nil || previousCancel == nil {
		t.Fatal("identical registration restarted storage")
	}
	call(t, m, "plugin.quiesce", nil)
	if authenticate(rotated).Authenticated {
		t.Fatal("quiesced authentication succeeded")
	}
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + cfg.DB), 6})
	if !authenticate(rotated).Authenticated {
		t.Fatal("unchanged replacement rollback did not reopen")
	}
	// Even with no active requests, changing a configured DB requires a new
	// Manager/process; a rejected change must preserve the live store and worker.
	invalidRaw, _ := json.Marshal(lifecycleRequest{[]byte("db: " + t.TempDir()), 6})
	failed, _ := m.HandleCall("plugin.reconfigure", invalidRaw)
	if !strings.Contains(string(failed), `"ok":false`) || !authenticate(rotated).Authenticated {
		t.Fatal("immutable DB guard did not preserve old storage")
	}
}

// Public resource requests can only retrieve a static shell; they cannot select
// a data route via path tricks or query parameters. Management secrets are host
// authenticated, never checked against frontend client Keys by this handler.
func TestManagementBoundaries(t *testing.T) {
	m := NewManager(func(name string) ([]byte, string, bool) {
		return []byte("<html>shell</html>"), "text/html", name == "ui.html"
	})
	for _, path := range []string{resourceBasePath + "/health", resourceBasePath + "/../health", resourceBasePath + "/%2e%2e/keys", managementBasePath + "/keys/..", "/v8/management/plugins/" + PluginID + "/health"} {
		r := m.dispatchManagement(managementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: "GET", Path: path}})
		if r.StatusCode != 404 {
			t.Fatalf("%s: %d", path, r.StatusCode)
		}
	}
	r := m.dispatchManagement(managementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: "GET", Path: resourceBasePath + "/ui/"}})
	if r.StatusCode != 200 || string(r.Body) != "<html>shell</html>" || r.Headers.Get("Cache-Control") != "no-store" || r.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset response: %+v", r)
	}
	r = m.dispatchManagement(managementRequest{ManagementRequest: pluginapi.ManagementRequest{Method: "GET", Path: managementBasePath + "/keys", Query: url.Values{"unknown": {"x"}}}})
	if r.StatusCode != 400 {
		t.Fatal("unknown query ignored")
	}
}

// Reject structurally valid but unsafe JSON and query inputs before storage.
func TestStrictManagementInputs(t *testing.T) {
	for _, raw := range []string{`null`, `{} {}`, `{"unknown":1}`, `{"max_concurrent":9000000000000001}`, `{"max_concurrent":1.1}`, `{"max_concurrent":1e2}`} {
		var spec engine.KeySpec
		if decodeRequestBody([]byte(raw), &spec) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var spec engine.KeySpec
	if decodeRequestBody([]byte(strings.Repeat(" ", maxJSONBodyBytes)+`{}`), &spec) == nil {
		t.Fatal("oversize whitespace bypass")
	}
	m := NewManager(nil)
	for _, q := range []url.Values{{"unknown": {"1"}}, {"page_size": {"101"}}, {"status": {"other"}}, {"page": {"1", "2"}}, {"from": {"invalid"}}} {
		if _, err := m.parseQuery(q); err == nil {
			t.Fatalf("query accepted: %v", q)
		}
	}
	for _, raw := range []string{"capture-bodies: true", "enabled: invalid", "db: ''", "enabled: true\n---\nenabled: false"} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("old/malformed config accepted: %s", raw)
		}
	}
}

// Usage belongs to the exact access provider, not any observer-looking string.
// Distinct upstream execution IDs bill separately; replay and Generate=false
// do not bill, and a reached limit blocks the next admission, not the response.
func TestUsageOwnershipAndQuota(t *testing.T) {
	m := liveManager(t)
	settings, err := m.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Pricing = pricing.Config{Prices: []pricing.ManualPrice{{Model: "fixed", Provider: "openai", Definition: pricing.PricingDefinition{Base: pricing.RateSet{Input: 1_000_000}}}}}
	if _, err = m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true, Limits: engine.Limits{Total: 1}})
	if err != nil {
		t.Fatal(err)
	}
	call(t, m, "frontend_auth.authenticate", pluginapi.FrontendAuthRequest{Method: http.MethodPost, Path: "/v1/chat/completions", Query: url.Values{"key": {secret}}})
	u := pluginapi.UsageRecord{RequestID: "execution-1", TraceID: "trace", Provider: "openai", Model: "fixed", APIKey: "observer:" + k.ID, AccessProvider: "config-inline", Generate: true, RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}}
	call(t, m, "usage.handle", u)
	u.AccessProvider = AccessProvider
	u.Generate = false
	call(t, m, "usage.handle", u)
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].Used.Total != 0 {
		t.Fatal("foreign/non-generation usage charged")
	}
	u.Generate = true
	call(t, m, "usage.handle", u)
	call(t, m, "usage.handle", u)
	u.RequestID = "execution-2"
	call(t, m, "usage.handle", u)
	keys, err = m.store.Keys()
	if err != nil || keys[0].Used.Total != 2 {
		t.Fatalf("execution accounting: %+v, %v", keys, err)
	}
	var response pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "next", RequestedModel: "fixed", Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope("observer:" + k.ID),
		coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}}), &response)
	if !response.Terminate || response.StatusCode != 429 {
		t.Fatal("quota did not block next admission")
	}
}
