package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Empty allowed_models means all models, while explicit selections are exact,
// including provider prefixes/suffixes. Neither path mutates client input.
func TestDefaultModelAccessAndSelectedModels(t *testing.T) {
	m := liveManager(t)
	spec := engine.KeySpec{Enabled: true}
	k, secret, err := m.store.CreateKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	call(t, m, "frontend_auth.authenticate", pluginapi.FrontendAuthRequest{Method: "POST", Path: "/v1/chat/completions", Query: map[string][]string{"key": {secret}}})
	defer call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "model-test", CompletedAt: time.Now()})
	req := pluginapi.RequestInterceptRequest{RequestID: "model-test", RequestedModel: "provider/model:variant", Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope("observer:" + k.ID),
		coreexecutor.GenerateMetadataKey:    false,
		coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}}
	before := func() bool {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", req), &r)
		return r.Terminate
	}
	if before() {
		t.Fatal("default unrestricted Key rejected model")
	}
	spec.AllowedModels = []string{"provider/model"}
	if _, err = m.store.UpdateKey(k.ID, spec); err != nil {
		t.Fatal(err)
	}
	if !before() {
		t.Fatal("unlisted exact model accepted")
	}
	req.RequestedModel = "provider/model"
	req.RequestID = "model-selected"
	defer call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "model-selected", CompletedAt: time.Now()})
	if before() {
		t.Fatal("selected exact model rejected")
	}
}

// Closing detaches the store and stops pricing workers; even an active request
// cannot leave an accounting gate when the unchanged configuration reopens.
func TestCloseDetachesStoreAndReopens(t *testing.T) {
	m := liveManager(t)
	k, _, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.store.Admit(engine.Admission{RequestID: "unsettled", KeyID: k.ID, Model: "any", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	err = m.closeLocked()
	detached := m.store == nil && m.prices == nil && m.cancel == nil
	m.mu.Unlock()
	if err != nil || !detached {
		t.Fatal("close failed or store advertised as live", err)
	}
	// Unchanged config can reopen; no accounting state may survive as a
	// permanent admission restriction.
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 6})
	if err = m.store.Admit(engine.Admission{RequestID: "after-reopen", KeyID: k.ID}); err != nil {
		t.Fatal("reopened key unavailable", err)
	}
	if err = m.store.Complete("after-reopen", time.Now()); err != nil {
		t.Fatal(err)
	}
}
