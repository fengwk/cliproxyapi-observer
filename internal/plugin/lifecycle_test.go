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
	call(t, m, "frontend_auth.authenticate", pluginapi.FrontendAuthRequest{Query: map[string][]string{"key": {secret}}})
	req := pluginapi.RequestInterceptRequest{RequestID: "model-test", RequestedModel: "provider/model:variant", Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope("observer:" + k.ID),
		coreexecutor.GenerateMetadataKey:    false,
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
	if before() {
		t.Fatal("selected exact model rejected")
	}
}

// Engine Close releases its handle even when reporting unsettled accounting.
// Manager must detach that closed object and stopped pricing workers.
func TestCloseErrorDetachesStore(t *testing.T) {
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
	if err == nil || m.store != nil || m.prices != nil || m.cancel != nil {
		m.mu.Unlock()
		t.Fatal("closed store advertised as live")
	}
	m.mu.Unlock()
	// Unchanged config can reopen; persisted engine debt remains fenced.
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 6})
}
