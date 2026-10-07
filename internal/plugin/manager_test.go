package plugin

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

func register(t *testing.T, m *Manager, raw string) {
	t.Helper()
	resp, err := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte(raw), SchemaVersion: pluginabi.SchemaVersion}))
	if err != nil {
		t.Fatalf("register %q: %v", raw, err)
	}
	var reg registration
	mustResult(t, resp, &reg)
}

func sendUsage(t *testing.T, m *Manager, id string) {
	t.Helper()
	resp, err := m.HandleCall(pluginabi.MethodUsageHandle, mustJSON(t, pluginapi.UsageRecord{RequestID: id, Provider: "openai", Model: "gpt"}))
	if err != nil {
		t.Fatalf("usage %q: %v", id, err)
	}
	if !decodeEnvelope(t, resp).OK {
		t.Fatalf("usage %q not acknowledged: %s", id, resp)
	}
}

func TestLifecycleValidationKeepsLiveStore(t *testing.T) {
	m, opener := newTestManager()
	register(t, m, "live")
	live := opener.last()

	opener.parseErr = errStoreFailure
	resp, err := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("broken")}))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if code := mustError(t, resp); code != "invalid_config" {
		t.Fatalf("code = %q, want invalid_config", code)
	}
	if live.isClosed() {
		t.Fatal("a rejected candidate must not quiesce the live store")
	}
	sendUsage(t, m, "after-reject")
	if live.usageCount() != 1 {
		t.Fatalf("live store did not receive usage after a rejected candidate")
	}
}

func TestReconfigureRollsBackAfterFailedOpen(t *testing.T) {
	m, opener := newTestManager()
	register(t, m, "live")
	live := opener.last()

	opener.openErrFor = func(cfg observer.Config) error {
		if cfg.DataPath == "candidate" {
			return errStoreFailure
		}
		return nil
	}
	resp, err := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("candidate")}))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if code := mustError(t, resp); code != "storage_unavailable" {
		t.Fatalf("code = %q, want storage_unavailable", code)
	}
	if !live.isClosed() {
		t.Fatal("the previous store must be released before opening the candidate")
	}
	restored := opener.last()
	if restored == live || restored.isClosed() {
		t.Fatal("rollback did not reopen a working previous store")
	}
	sendUsage(t, m, "after-rollback")
	if restored.usageCount() != 1 {
		t.Fatal("rolled-back store did not receive usage")
	}
}

func TestQuiesceReleasesAndReconfigureReopens(t *testing.T) {
	m, opener := newTestManager()
	register(t, m, "live")
	first := opener.last()

	resp, err := m.HandleCall(pluginabi.MethodPluginQuiesce, nil)
	if err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	mustResult(t, resp, nil)
	if !first.isClosed() {
		t.Fatal("quiesce must release the store")
	}
	// Observation stays fail-open while quiesced.
	sendUsage(t, m, "while-quiesced")
	if first.usageCount() != 0 {
		t.Fatal("usage must not reach a released store")
	}

	register(t, m, "live")
	reopened := opener.last()
	if reopened == first || reopened.isClosed() {
		t.Fatal("reconfigure did not reopen the store")
	}
	sendUsage(t, m, "after-reopen")
	if reopened.usageCount() != 1 {
		t.Fatal("reopened store did not receive usage")
	}
}

func TestShutdownIsIdempotentAndTerminal(t *testing.T) {
	m, opener := newTestManager()
	register(t, m, "live")
	live := opener.last()

	for i := 0; i < 2; i++ {
		resp, err := m.HandleCall(pluginabi.MethodPluginShutdown, nil)
		if err != nil {
			t.Fatalf("shutdown #%d: %v", i, err)
		}
		if !decodeEnvelope(t, resp).OK {
			t.Fatalf("shutdown #%d not acknowledged: %s", i, resp)
		}
	}
	if !live.isClosed() {
		t.Fatal("shutdown must release the store")
	}
	sendUsage(t, m, "after-shutdown")
	if live.usageCount() != 0 {
		t.Fatal("usage must not reach a shut down store")
	}
	resp, err := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("live")}))
	if err != nil {
		t.Fatalf("reconfigure after shutdown: %v", err)
	}
	if code := mustError(t, resp); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
}

func TestMalformedLifecycleRequest(t *testing.T) {
	m, _ := newTestManager()
	resp, err := m.HandleCall(pluginabi.MethodPluginRegister, []byte("{"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if code := mustError(t, resp); code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", code)
	}
}
