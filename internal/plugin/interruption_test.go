package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// A late callback/panic in an old native instance must block the affected Key
// in a new instance, even if the new Store is already open. Only explicit
// recovery acknowledges the missing event; reset/rotation preserve the fence.
func TestInterruptionAcrossManagers(t *testing.T) {
	for _, panicCallback := range []bool{false, true} {
		name := "late"
		if panicCallback {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			old := liveManager(t)
			key, secret, err := old.store.CreateKey(engine.KeySpec{Enabled: true, Limits: engine.Limits{Total: 1234}})
			if err != nil {
				t.Fatal(err)
			}
			settings, err := old.store.Settings()
			if err != nil {
				t.Fatal(err)
			}
			settings.Pricing = pricing.Config{Prices: []pricing.Price{{Model: "model", Rates: pricing.Rates{Input: 5_000_000}}}, Rules: []pricing.Rule{}}
			if _, err = old.store.UpdateSettings(settings); err != nil {
				t.Fatal(err)
			}
			call(t, old, "usage.handle", pluginapi.UsageRecord{RequestID: "charged", APIKey: "observer:" + key.ID, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1}})
			call(t, old, "plugin.quiesce", nil)
			next := NewManager(nil)
			call(t, next, "plugin.register", lifecycleRequest{[]byte("db: " + old.cfg.DB), 6})
			t.Cleanup(func() { call(t, next, "plugin.shutdown", nil) })
			if panicCallback {
				old.mu.RLock()
				old.accountUsage(key.ID, func() error { panic("sensitive callback panic") })
				old.mu.RUnlock()
			} else {
				call(t, old, "usage.handle", pluginapi.UsageRecord{RequestID: "late", Generate: true, AccessProvider: AccessProvider, APIKey: "observer:" + key.ID, Provider: "openai", Model: "model", RequestedAt: time.Now()})
			}
			if _, fallback := old.billingFaults.Load(key.ID); fallback {
				t.Fatal("durable marker unexpectedly failed")
			}
			keys, err := next.store.Keys()
			if err != nil || len(keys) != 1 || keys[0].BlockedReason == "" {
				t.Fatal("new Store did not observe interruption")
			}
			a := authenticateTest(t, next, pluginapi.FrontendAuthRequest{Headers: http.Header{"Authorization": {"Bearer " + secret}}})
			if !a.Authenticated {
				t.Fatal("authentication should remain distinct from admission")
			}
			before := func() pluginapi.RequestInterceptResponse {
				var r pluginapi.RequestInterceptResponse
				_ = json.Unmarshal(call(t, next, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: "next", RequestedModel: "model", Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(a.Principal)}}), &r)
				return r
			}
			if !before().Terminate {
				t.Fatal("interrupted Key admitted")
			}
			if _, err = next.store.ResetKey(key.ID, []string{"five_hour"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err = next.store.RotateKey(key.ID); err != nil {
				t.Fatal(err)
			}
			if !before().Terminate {
				t.Fatal("reset/rotate acknowledged unknown usage")
			}
			if r := next.serveRecoverKey(key.ID, []byte(`{"acknowledge":false}`)); r.StatusCode != 400 {
				t.Fatal("implicit acknowledgement")
			}
			if r := next.serveRecoverKey(key.ID, []byte(`{"acknowledge":true}`)); r.StatusCode != 200 {
				t.Fatalf("recover: %s", r.Body)
			}
			keys, err = next.store.Keys()
			if err != nil || keys[0].BlockedReason != "" || keys[0].Used.Total != 5 || keys[0].Limits.Total != 1234 {
				t.Fatal("recovery altered money or retained fence")
			}
			if before().Terminate {
				t.Fatal("explicitly recovered Key still blocked")
			}
			call(t, next, "request.complete", pluginapi.RequestCompletion{RequestID: "next", CompletedAt: time.Now()})
		})
	}
}

// If the interruption marker cannot be persisted, retain a local fail-closed
// gate. Ordinary retained accounting errors must not be converted to unknowns.
func TestInterruptionWriteFailureFallback(t *testing.T) {
	m := NewManager(nil)
	parent := t.TempDir()
	file := filepath.Join(parent, "not-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m.cfg.DB = filepath.Join(file, "db")
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	m.accountUsage(id, func() error { return engine.ErrClosed })
	if _, ok := m.billingFaults.Load(id); !ok {
		t.Fatal("failed marker did not gate Key")
	}
	m.billingFaults.Delete(id)
	m.accountUsage(id, func() error { return engine.ErrAccounting })
	if _, ok := m.billingFaults.Load(id); ok {
		t.Fatal("ordinary pending accounting forced unknown interruption")
	}
	for _, principal := range []string{"observer:../escape", "observer:short", "observer-count:" + id, "observer-catalog"} {
		if validPrincipal(principal) {
			t.Fatal("unvalidated interruption identity")
		}
	}
}
