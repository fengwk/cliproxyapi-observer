//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// This is a documented SDK-gap reproduction, NOT a test proving a repaired
// all-route gateway. The native Realtime credential route bypasses request.before
// and can issue a local ephemeral token with zero Observer Keys. No real upstream
// is contacted; execution-chain gaps are also traced in docs/model-policy.md.
func TestSDKRealtimeTokenRouteBypassesModelGuard(t *testing.T) {
	h := newHarness(t)
	status, body, err := h.rawRequest(http.MethodPost, "/v1/realtime/client_secrets",
		[]byte(`{"session":{"type":"realtime","model":"gpt-realtime"}}`),
		map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Value string `json:"value"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &issued) != nil || !strings.HasPrefix(issued.Value, "ek_") {
		t.Fatalf("SDK gap behavior changed: status=%d; recheck route-level protection", status)
	}
	if len(h.keys(t)) != 0 || len(h.page(t, "page_size=100").Items) != 0 || h.mock.callCount() != 0 {
		t.Fatal("SDK gap probe must use zero Observer keys and no upstream execution")
	}
}

// Reproduce actual execution, not just token issuance or a missing-credential
// 503: a native key on either Alpha Search alias reaches a loopback fake upstream
// while Observer is enabled with zero Keys. Expected success records the host
// gap; it is deliberately NOT evidence that the all-model requirement is met.
func TestSDKAlphaSearchExecutionBypassesModelGuard(t *testing.T) {
	var upstreamCalls atomic.Int32
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/alpha/search" ||
			r.Header.Get("Authorization") != "Bearer fake-alpha-upstream-key" {
			http.Error(w, "unexpected fake Alpha Search request", http.StatusBadRequest)
			return
		}
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"observer_sdk_gap_probe":true}`))
	}))
	defer alpha.Close()

	h := newHarness(t)
	h.awaitGracefulRestartReady(t, 0)
	path := filepath.Join(h.dir, "config.yaml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(cfg, []byte("\nplugins:")) != 1 {
		t.Fatal("isolated fixture must have exactly one plugin config section")
	}
	// The v8 config keeps the endpoint/models on the Codex group and opts
	// the fake API key into Alpha Search. Every upstream URL stays loopback.
	addition := fmt.Sprintf(`
 codex:
  - name: "observer-alpha-gap-fixture"
    base-url: %q
    keys:
      - api-key: "fake-alpha-upstream-key"
        alpha-search: true
    models:
      - name: "observer-alpha-gap-model"
        alias: "observer-alpha-gap-model"
`, alpha.URL+"/v1")
	cfg = bytes.Replace(cfg, []byte("\nplugins:"), []byte(addition+"\nplugins:"), 1)
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err = h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + clientKey}
	h.await(t, "local Alpha Search fixture registered", func() bool {
		status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, headers)
		return err == nil && status == http.StatusOK && bytes.Contains(body, []byte(`"observer-alpha-gap-model"`))
	})
	for _, route := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		status, body, err := h.rawRequest(http.MethodPost, route,
			[]byte(`{"model":"observer-alpha-gap-model","query":"observer"}`), headers)
		if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte(`"observer_sdk_gap_probe":true`)) {
			t.Fatalf("SDK gap behavior changed on %s: status=%d err=%v; recheck protection", route, status, err)
		}
	}
	if upstreamCalls.Load() != 2 || len(h.keys(t)) != 0 ||
		len(h.page(t, "page_size=100").Items) != 0 || h.mock.callCount() != 0 {
		t.Fatal("expected two unguarded Alpha Search calls with zero Observer Keys/accounting")
	}
}
