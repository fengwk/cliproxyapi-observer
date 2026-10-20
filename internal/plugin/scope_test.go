package plugin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Only the verified v8.0.30 model execution, count, and directory routes are
// recognized. Trailing slashes, wrong methods, media endpoints, aliases, and
// path tricks remain unhandled so the host chain stays authoritative.
func TestOperationWhitelist(t *testing.T) {
	generation := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/messages",
		"/v1/responses",
		"/backend-api/codex/responses",
		"/v1beta/models/model:generateContent",
		"/v1beta/models/model:streamGenerateContent",
	}
	for _, path := range generation {
		if got := operationFor(http.MethodPost, path); got != operationGeneration {
			t.Fatalf("%s: %v", path, got)
		}
	}
	for _, path := range []string{"/v1/messages/count_tokens", "/v1beta/models/model:countTokens"} {
		if got := operationFor(http.MethodPost, path); got != operationCount {
			t.Fatalf("%s: %v", path, got)
		}
	}
	for _, path := range []string{"/v1/models", "/v1/models/model", "/v1beta/models", "/v1beta/models/model"} {
		if got := operationFor(http.MethodGet, path); got != operationDirectory {
			t.Fatalf("%s: %v", path, got)
		}
	}
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/models"},
		{http.MethodGet, "/v1/models/"},
		{http.MethodPost, "/v1/chat/completions/"},
		{http.MethodPost, "/v1/responses/compact"},
		{http.MethodPost, "/backend-api/codex/responses/compact"},
		{http.MethodPost, "/v1/alpha/search"},
		{http.MethodPost, "/v1/realtime"},
		{http.MethodPost, "/v1/images/generations"},
		{http.MethodPost, "/v1/audio/speech"},
		{http.MethodPost, "/v1/videos"},
		{http.MethodPost, "/v1beta/interactions"},
		{http.MethodPost, "/v1beta/models/:generateContent"},
		{http.MethodPost, "/v1beta/models/a/b:generateContent"},
		{http.MethodPost, "/v1beta/models/model:generateContentExtra"},
		{http.MethodPost, "/v1beta/models/model:CountTokens"},
		{http.MethodGet, "/v1beta/models/"},
		{http.MethodGet, "/v1beta/models/:countTokens"},
		{http.MethodGet, "/v1/models/../management.html"},
		{http.MethodGet, "/v1/models//model"},
		{http.MethodGet, "/v1beta/models/./model"},
	} {
		if got := operationFor(test.method, test.path); got != operationUnknown {
			t.Fatalf("%s %s: %v", test.method, test.path, got)
		}
	}
}

// Authentication is limited to the same operation whitelist. Unsupported paths
// and invalid Keys report unhandled (false), never global authentication success.
func TestAuthenticationScopesByOperation(t *testing.T) {
	m := liveManager(t)
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, principal string
	}{
		{"POST", "/v1/chat/completions", "observer:" + k.ID},
		{"POST", "/v1/messages", "observer:" + k.ID},
		{"POST", "/v1/responses", "observer:" + k.ID},
		{"POST", "/backend-api/codex/responses", "observer:" + k.ID},
		{"POST", "/v1beta/models/model:generateContent", "observer:" + k.ID},
		{"POST", "/v1beta/models/model:streamGenerateContent", "observer:" + k.ID},
		{"POST", "/v1/messages/count_tokens", "observer-count:" + k.ID},
		{"POST", "/v1beta/models/model:countTokens", "observer-count:" + k.ID},
		{"GET", "/v1/models", "observer-directory:" + k.ID},
		{"GET", "/v1/models/model", "observer-directory:" + k.ID},
		{"GET", "/v1beta/models", "observer-directory:" + k.ID},
		{"GET", "/v1beta/models/model", "observer-directory:" + k.ID},
	} {
		a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: test.method, Path: test.path, Headers: http.Header{"Authorization": {"Bearer " + secret}}})
		if !a.Authenticated || a.Principal != test.principal {
			t.Fatalf("%s %s: %+v", test.method, test.path, a)
		}
	}
	for _, test := range []struct{ method, path string }{
		{"", ""},
		{"POST", "/v1/chat/completions/"},
		{"GET", "/v1/chat/completions"},
		{"POST", "/v1/images/generations"},
		{"POST", "/v1/responses/compact"},
		{"POST", "/v1beta/models/model:unknown"},
	} {
		if a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: test.method, Path: test.path, Headers: http.Header{"Authorization": {"Bearer " + secret}}}); a.Authenticated {
			t.Fatalf("%s %s was handled: %+v", test.method, test.path, a)
		}
	}
	if a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer invalid"}}}); a.Authenticated {
		t.Fatal("invalid Key authenticated")
	}
}

// Unowned/malformed traffic passes through without touching a closed store.
// Owned generation/count scopes fail closed; directory scopes remain reads.
func TestBeforeOwnershipAndStoreFailure(t *testing.T) {
	m := liveManager(t)
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	generation := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/messages/count_tokens", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	directory := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	if generation.Principal != "observer:"+k.ID || count.Principal != "observer-count:"+k.ID || directory.Principal != "observer-directory:"+k.ID {
		t.Fatal("unexpected principals")
	}
	before := func(metadata map[string]any) pluginapi.RequestInterceptResponse {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "request", RequestedModel: "model", Headers: http.Header{"X-Test": {"value"}}, Body: []byte(`{"model":"model"}`), Metadata: metadata}), &r)
		if r.Path != "" || len(r.Headers) != 0 || len(r.Body) != 0 || len(r.ClearHeaders) != 0 {
			t.Fatal("interceptor rewrote request")
		}
		return r
	}
	metadata := func(scope, path string) map[string]any {
		return map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(scope), coreexecutor.RequestPathMetadataKey: path}
	}
	if before(metadata(generation.Principal, "/v1/chat/completions")).Terminate {
		t.Fatal("owned generation rejected")
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "request", CompletedAt: time.Now()})

	m.mu.Lock()
	if err := m.closeLocked(); err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	m.mu.Unlock()

	for _, test := range []struct {
		name string
		meta map[string]any
		deny bool
	}{
		{"own generation", metadata(generation.Principal, "/v1/chat/completions"), true},
		{"own count", metadata(count.Principal, "/v1/messages/count_tokens"), true},
		{"own directory", metadata(directory.Principal, "/v1/models"), false},
		{"own missing path", metadata(generation.Principal, ""), false},
		{"own unsupported path", metadata(generation.Principal, "/v1/images/generations"), false},
		{"host scope", metadata("native-key", "/v1/chat/completions"), false},
		{"no scope", map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"}, false},
		{"malformed scope", map[string]any{coreexecutor.CallerScopeMetadataKey: 17, coreexecutor.RequestPathMetadataKey: "/v1/chat/completions"}, false},
	} {
		r := before(test.meta)
		if r.Terminate != test.deny {
			t.Fatalf("%s: %+v", test.name, r)
		}
		if test.deny && r.StatusCode != 503 {
			t.Fatalf("%s status: %d", test.name, r.StatusCode)
		}
	}
	// Malformed before payloads carry no ownership and must pass.
	out, err := m.HandleCall("request.intercept_before", []byte("{"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result pluginapi.RequestInterceptResponse `json:"result"`
	}
	if json.Unmarshal(out, &envelope) != nil || envelope.Result.Terminate {
		t.Fatalf("malformed before: %s", out)
	}

	// The host replaces the native instance while this process keeps serving.
	// Reopening the same database restores owned admission for existing scopes.
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 6})
	if r := before(metadata(generation.Principal, "/v1/chat/completions")); r.Terminate {
		t.Fatal("recovered store rejected owned generation")
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "request", CompletedAt: time.Now()})
}

// Scopes outlive credential changes inside the same manager. Disabled or
// expired Keys therefore stay fail-closed for already authenticated requests.
func TestOwnScopeDisabledAndExpired(t *testing.T) {
	m := liveManager(t)
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true, AllowedModels: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func() pluginapi.FrontendAuthResponse {
		return authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	}
	generation := authenticate()
	count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/messages/count_tokens", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	before := func(principal, path string) pluginapi.RequestInterceptResponse {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "request", RequestedModel: "model", Metadata: map[string]any{
			coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal), coreexecutor.RequestPathMetadataKey: path,
		}}), &r)
		return r
	}
	spec := k.KeySpec
	spec.Enabled = false
	if _, err = m.store.UpdateKey(k.ID, spec); err != nil {
		t.Fatal(err)
	}
	if !before(generation.Principal, "/v1/chat/completions").Terminate || !before(count.Principal, "/v1/messages/count_tokens").Terminate {
		t.Fatal("disabled Key kept an owned scope alive")
	}
	if authenticate().Authenticated {
		t.Fatal("disabled Key authenticated")
	}
	spec.Enabled = true
	if _, err = m.store.UpdateKey(k.ID, spec); err != nil {
		t.Fatal(err)
	}
	generation = authenticate()
	expired := time.Now().Add(-time.Minute)
	spec.ExpiresAt = &expired
	if _, err = m.store.UpdateKey(k.ID, spec); err != nil {
		t.Fatal(err)
	}
	if !before(generation.Principal, "/v1/chat/completions").Terminate || !before(count.Principal, "/v1/messages/count_tokens").Terminate {
		t.Fatal("expired Key kept an owned scope alive")
	}
	if authenticate().Authenticated {
		t.Fatal("expired Key authenticated")
	}
}
