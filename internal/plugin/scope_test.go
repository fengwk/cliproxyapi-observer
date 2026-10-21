package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"go.etcd.io/bbolt"
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
		{"GET", "/v1/responses"},
		{"GET", "/backend-api/codex/responses"},
		{"POST", "/v1/realtime/client_secrets"},
		{"POST", "/v1/alpha/search"},
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
		{"own generation template", metadata(generation.Principal, "/v1beta/models/*action"), true},
		{"own count", metadata(count.Principal, "/v1/messages/count_tokens"), true},
		{"own count template", metadata(count.Principal, "/v1beta/models/*action"), true},
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
	if r := before(metadata(generation.Principal, "/v1beta/models/*action")); r.Terminate {
		t.Fatal("recovered store rejected owned generation template")
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

// Gin's Gemini interceptor metadata is the FullPath template, not the exact
// action. The authenticated operation must still enforce quota/concurrency and
// keep count free of admission, charging, and capture.
func TestGeminiWildcardAdmissionEnforcement(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		m := liveManager(t)
		settings, err := m.store.Settings()
		if err != nil {
			t.Fatal(err)
		}
		settings.Pricing = pricing.Config{Prices: []pricing.Price{{Model: "model", Rates: pricing.Rates{Input: 1_000_000}}}, Rules: []pricing.Rule{}}
		if _, err = m.store.UpdateSettings(settings); err != nil {
			t.Fatal(err)
		}
		k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true, Limits: engine.Limits{Total: 1}})
		if err != nil {
			t.Fatal(err)
		}
		headers := http.Header{"Authorization": {"Bearer " + secret}}
		generation := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:generateContent", Headers: headers})
		count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:countTokens", Headers: headers})
		call(t, m, "usage.handle", pluginapi.UsageRecord{RequestID: "full", APIKey: "observer:" + k.ID, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1}})
		before := func(principal, id string) pluginapi.RequestInterceptResponse {
			var r pluginapi.RequestInterceptResponse
			_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: id, RequestedModel: "model", Metadata: map[string]any{
				coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal),
				coreexecutor.GenerateMetadataKey:    true,
				coreexecutor.RequestPathMetadataKey: "/v1beta/models/*action",
			}}), &r)
			return r
		}
		if r := before(generation.Principal, "generation"); !r.Terminate || r.StatusCode != 429 {
			t.Fatalf("wildcard generation bypassed quota: %+v", r)
		}
		if r := before(count.Principal, "count"); r.Terminate || m.store.Health().ActiveRequests != 0 {
			t.Fatalf("wildcard count entered admission: %+v", r)
		}
		if _, ok := m.admitted.Load("count"); ok {
			t.Fatal("count was recorded as an admission")
		}
		// Count usage is never billable, even when the host sets Generate=true.
		call(t, m, "usage.handle", pluginapi.UsageRecord{RequestID: "count-usage", Generate: true, AccessProvider: AccessProvider, APIKey: count.Principal, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 100}})
		if page, err := m.store.Requests(engine.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Page: 1, PageSize: 25}); err != nil || len(page.Items) != 1 || page.Items[0].ID != "full" {
			t.Fatalf("count usage was billed: %+v %v", page, err)
		}
	})

	t.Run("model and concurrency", func(t *testing.T) {
		m := liveManager(t)
		_, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true, MaxConcurrent: 1, AllowedModels: []string{"model"}})
		if err != nil {
			t.Fatal(err)
		}
		headers := http.Header{"Authorization": {"Bearer " + secret}}
		generation := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:generateContent", Headers: headers})
		count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:countTokens", Headers: headers})
		before := func(principal, model, id string) pluginapi.RequestInterceptResponse {
			var r pluginapi.RequestInterceptResponse
			_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: id, TraceID: "trace-" + id, RequestedModel: model, Metadata: map[string]any{
				coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal),
				coreexecutor.GenerateMetadataKey:    true,
				coreexecutor.RequestPathMetadataKey: "/v1beta/models/*action",
			}}), &r)
			return r
		}
		if r := before(generation.Principal, "other", "restricted"); !r.Terminate {
			t.Fatal("wildcard generation bypassed model restriction")
		}
		if r := before(generation.Principal, "model", "generation-1"); r.Terminate || m.store.Health().ActiveRequests != 1 {
			t.Fatal("wildcard generation was not admitted")
		}
		if r := before(count.Principal, "model", "count-1"); r.Terminate || m.store.Health().ActiveRequests != 1 {
			t.Fatal("wildcard count reserved concurrency")
		}
		if _, ok := m.admitted.Load("count-1"); ok {
			t.Fatal("count was recorded as an admission")
		}
		if r := before(generation.Principal, "model", "generation-2"); !r.Terminate || r.StatusCode != 429 {
			t.Fatal("wildcard generation bypassed concurrency")
		}
		call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "generation-1", CompletedAt: time.Now()})
	})
}

// A caller scope may only perform the operation authenticated on its original
// exact path. This prevents generation->count bypass and directory execution.
func TestScopeOperationCoordination(t *testing.T) {
	m := liveManager(t)
	_, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer " + secret}}
	generation := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:generateContent", Headers: headers})
	count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:countTokens", Headers: headers})
	directory := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1beta/models/model", Headers: headers})
	before := func(principal, path string) pluginapi.RequestInterceptResponse {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "request", RequestedModel: "model", Metadata: map[string]any{
			coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal), coreexecutor.RequestPathMetadataKey: path,
		}}), &r)
		return r
	}
	for _, test := range []struct{ name, principal, path string }{
		{"generation as count", generation.Principal, "/v1/messages/count_tokens"},
		{"count as generation", count.Principal, "/v1/chat/completions"},
		{"directory as generation", directory.Principal, "/v1/chat/completions"},
		{"directory as template", directory.Principal, "/v1beta/models/*action"},
	} {
		if r := before(test.principal, test.path); !r.Terminate || r.StatusCode != 403 {
			t.Fatalf("%s: %+v", test.name, r)
		}
	}
	if r := before(generation.Principal, "/v1beta/models/*action"); r.Terminate || m.store.Health().ActiveRequests != 1 {
		t.Fatal("generation template was not admitted")
	}
	if r := before(count.Principal, "/v1beta/models/*action"); r.Terminate || m.store.Health().ActiveRequests != 1 {
		t.Fatal("count template reserved admission")
	}
	if r := before("native-host-scope", "/v1beta/models/*action"); r.Terminate {
		t.Fatal("unknown host scope was rejected")
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "request", CompletedAt: time.Now()})
}

// Concurrent authentication for different operations must not rewrite the
// caller scope's recorded operation.
func TestConcurrentScopesKeepAuthenticatedOperation(t *testing.T) {
	m := liveManager(t)
	_, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer " + secret}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		for _, spec := range []struct{ method, path string }{
			{"POST", "/v1beta/models/model:generateContent"},
			{"POST", "/v1beta/models/model:countTokens"},
			{"GET", "/v1beta/models/model"},
		} {
			wg.Add(1)
			go func(method, path string) {
				defer wg.Done()
				<-start
				authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: method, Path: path, Headers: headers})
			}(spec.method, spec.path)
		}
	}
	close(start)
	wg.Wait()
	for principal, want := range map[string]requestOperation{
		"observer:" + mustKeyID(t, m):           operationGeneration,
		"observer-count:" + mustKeyID(t, m):     operationCount,
		"observer-directory:" + mustKeyID(t, m): operationDirectory,
	} {
		v, ok := m.scopes.Load(session.CallerScope(principal))
		if !ok {
			t.Fatalf("missing scope for %s", principal)
		}
		if got := v.(callerScope).Operation; got != want {
			t.Fatalf("%s operation: %v", principal, got)
		}
	}
}

func mustKeyID(t *testing.T, m *Manager) string {
	t.Helper()
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v %v", keys, err)
	}
	return keys[0].ID
}

// Count bodies and correlation entries must never be captured, even with
// Generate=true and capture enabled. The correlation bucket is written by the
// engine's capture worker, so its absence after a joined close is deterministic.
func TestCountDoesNotCapture(t *testing.T) {
	m := liveManager(t)
	settings, err := m.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.CaptureEnabled = true
	if _, err = m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	_, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:countTokens", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	var r pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "count-capture", TraceID: "trace-capture", RequestedModel: "model", Body: []byte(`{"contents":[]}`), Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope(count.Principal),
		coreexecutor.GenerateMetadataKey:    true,
		coreexecutor.RequestPathMetadataKey: "/v1beta/models/*action",
	}}), &r)
	if r.Terminate {
		t.Fatal("count rejected")
	}
	m.mu.Lock()
	if err := m.closeLocked(); err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	m.mu.Unlock()
	database, err := bbolt.Open(m.cfg.DB, 0600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	err = database.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("correlations"))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, value []byte) error {
			if bytes.Contains(value, []byte("count-capture")) {
				return errors.New("count capture correlation persisted")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
