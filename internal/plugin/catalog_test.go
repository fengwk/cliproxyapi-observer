package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func authenticateTest(t *testing.T, m *Manager, req pluginapi.FrontendAuthRequest) pluginapi.FrontendAuthResponse {
	t.Helper()
	var a pluginapi.FrontendAuthResponse
	if err := json.Unmarshal(call(t, m, "frontend_auth.authenticate", req), &a); err != nil {
		t.Fatal(err)
	}
	return a
}
func issueCatalogTest(t *testing.T, m *Manager) catalogToken {
	t.Helper()
	r := dispatchForTest(t, m, "POST", managementBasePath+"/models/token", nil, "")
	var token catalogToken
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &token) != nil || len(token.Token) < 43 {
		t.Fatalf("issue: %d %s", r.StatusCode, r.Body)
	}
	return token
}

// Capabilities only authorize one exact GET catalog request; they never create
// a billable Key or supply an observer admission principal.
func TestCatalogCapabilityScopeAndSingleUse(t *testing.T) {
	m := liveManager(t)
	token := issueCatalogTest(t, m)
	headers := http.Header{"Authorization": {"Bearer " + token.Token}}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages/count_tokens", "/v1beta/models/m:countTokens", "/v1/models/", "/v1/models/m", managementBasePath + "/keys"} {
		if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: path, Headers: headers}).Authenticated {
			t.Fatal("catalog token escaped scope")
		}
	}
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/models", Headers: headers}).Authenticated {
		t.Fatal("catalog POST accepted")
	}
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Query: url.Values{"key": {token.Token}}}).Authenticated {
		t.Fatal("query capability accepted")
	}
	a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: headers})
	if !a.Authenticated || a.Principal != "observer-catalog" {
		t.Fatalf("catalog auth: %+v", a)
	}
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: headers}).Authenticated {
		t.Fatal("capability replay")
	}
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 0 {
		t.Fatal("catalog token created a DB Key")
	}
	var before pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "catalog", RequestedModel: "model", Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope(a.Principal), coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}}), &before)
	if before.Terminate || m.store.Health().ActiveRequests != 0 {
		t.Fatal("catalog capability entered admission")
	}
}

// A deterministic clock proves TTL boundaries, bounded outstanding state, and
// close invalidation without sleeps. Only digests are retained in memory.
func TestCatalogTTLBoundAndQuiesce(t *testing.T) {
	m := liveManager(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	first := issueCatalogTest(t, m)
	for i := 1; i < 32; i++ {
		issueCatalogTest(t, m)
	}
	if r := m.serveCatalogToken(); r.StatusCode != 429 {
		t.Fatal("outstanding cap not bounded")
	}
	if expiry, ok := m.catalogCaps[sha256.Sum256([]byte(first.Token))]; !ok || !expiry.Equal(first.ExpiresAt) {
		t.Fatal("capability not indexed by digest")
	}
	database, err := os.ReadFile(m.cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(database, []byte(first.Token)) {
		t.Fatal("plaintext capability persisted")
	}
	now = now.Add(30 * time.Second)
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + first.Token}}}).Authenticated {
		t.Fatal("expired capability accepted")
	}
	if len(m.catalogCaps) != 0 {
		t.Fatal("auth did not prune expirations")
	}
	fresh := issueCatalogTest(t, m)
	call(t, m, "plugin.quiesce", nil)
	if len(m.catalogCaps) != 0 || m.serveCatalogToken().StatusCode != 503 {
		t.Fatal("quiesce retained capabilities")
	}
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + fresh.Token}}}).Authenticated {
		t.Fatal("quiesced token accepted")
	}
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 6})
	if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + fresh.Token}}}).Authenticated {
		t.Fatal("reopen resurrected token")
	}
	for _, r := range []pluginapi.ManagementRequest{
		{Method: "POST", Path: managementBasePath + "/models/token", Query: url.Values{"x": {"1"}}},
		{Method: "POST", Path: managementBasePath + "/models/token", Body: []byte(`{}`)},
		{Method: "POST", Path: managementBasePath + "/models/token", Body: []byte(" ")},
		{Method: "POST", Path: resourceBasePath + "/models/token"},
		{Method: "GET", Path: managementBasePath + "/models/token"},
	} {
		if out := m.dispatchManagement(managementRequest{ManagementRequest: r}); out.StatusCode < 400 {
			t.Fatal("token route accepted invalid request")
		}
	}
}

// Original auth URL distinguishes identical Gemini count/generation payloads
// even when before metadata has the same wildcard path and Generate=true.
// Count enforces model authorization but bypasses exhausted quota/concurrency.
func TestCountScopeFromOriginalPath(t *testing.T) {
	m := liveManager(t)
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true, MaxConcurrent: 1, AllowedModels: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"contents":[{"parts":[{"text":"same"}]}]}`)
	headers := http.Header{"Authorization": {"Bearer " + secret}}
	generation := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:generateContent", Body: body, Headers: headers})
	count := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:countTokens", Body: body, Headers: headers})
	if generation.Principal != "observer:"+k.ID || count.Principal != "observer-count:"+k.ID {
		t.Fatal("operation principal not distinct")
	}
	before := func(principal, model, id, path string) pluginapi.RequestInterceptResponse {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: id, RequestedModel: model, Body: body, Metadata: map[string]any{
			coreexecutor.CallerScopeMetadataKey: session.CallerScope(principal), coreexecutor.GenerateMetadataKey: true, coreexecutor.RequestPathMetadataKey: path,
		}}), &r)
		return r
	}
	if before(generation.Principal, "model", "generation", "/v1beta/models/model:generateContent").Terminate {
		t.Fatal("generation rejected")
	}
	if before(count.Principal, "model", "count", "/v1beta/models/model:countTokens").Terminate || m.store.Health().ActiveRequests != 1 {
		t.Fatal("count reserved a slot")
	}
	if !before(count.Principal, "other", "bad-count", "/v1beta/models/model:countTokens").Terminate {
		t.Fatal("count bypassed model restriction")
	}
	call(t, m, "usage.handle", pluginapi.UsageRecord{RequestID: "count-usage", Generate: true, AccessProvider: AccessProvider, APIKey: count.Principal, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 100}})
	if page, e := m.store.Requests(engine.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Page: 1, PageSize: 25}); e != nil || len(page.Items) != 0 {
		t.Fatal("count usage billed")
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "generation", CompletedAt: time.Now()})
	anthropic := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/messages/count_tokens", Headers: headers})
	if anthropic.Principal != count.Principal {
		t.Fatal("count principal not stable")
	}
	for _, path := range []string{"/v1beta/models/:countTokens", "/v1beta/models/m:countTokens/", "/v1beta/models/a/b:countTokens", "/v1/messages/count_tokens/"} {
		if countTokenPath(path) {
			t.Fatal("overbroad count path")
		}
	}
}

// Full quota blocks generation only; count authorization uses the same Key's
// enabled/expiry/model policy without reserving or charging it.
func TestCountWhenQuotaFull(t *testing.T) {
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
	call(t, m, "usage.handle", pluginapi.UsageRecord{RequestID: "full", APIKey: "observer:" + k.ID, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1}})
	keys, err := m.store.Keys()
	if err != nil || keys[0].Used.Total != 1 {
		t.Fatal("quota fixture did not bill")
	}
	for _, operation := range []string{"generateContent", "countTokens"} {
		a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1beta/models/model:" + operation, Headers: http.Header{"Authorization": {"Bearer " + secret}}})
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: operation, RequestedModel: "model", Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(a.Principal), coreexecutor.GenerateMetadataKey: true, coreexecutor.RequestPathMetadataKey: "/v1beta/models/model:" + operation}}), &r)
		if r.Terminate != (operation == "generateContent") {
			t.Fatal("quota applied to wrong operation")
		}
	}
	if m.store.Health().ActiveRequests != 0 {
		t.Fatal("count reserved while quota full")
	}
}

// Simultaneous requests cannot redeem the same capability twice.
func TestCatalogConcurrentRedemption(t *testing.T) {
	m := liveManager(t)
	token := issueCatalogTest(t, m)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: "GET", Path: "/v1/models", Headers: http.Header{"Authorization": {"Bearer " + token.Token}}}).Authenticated {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("capability was not single-use under concurrency")
	}
	for _, state := range []string{"disabled", "closed"} {
		m := NewManager(nil)
		if state == "disabled" {
			call(t, m, "plugin.register", lifecycleRequest{[]byte("enabled: false"), 6})
		} else {
			call(t, m, "plugin.shutdown", nil)
		}
		if m.serveCatalogToken().StatusCode != 503 {
			t.Fatal("inactive manager issued capability")
		}
	}
}
