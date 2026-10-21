//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// Native credentials keep their own CPA behavior outside the finite Observer
// operation whitelist. An Observer Key, restricted or not, is not a native
// credential and must not authenticate these unsupported routes.
func TestNativeRealtimeTokenAndObserverBoundary(t *testing.T) {
	h := newHarness(t)
	request := []byte(`{"session":{"type":"realtime","model":"gpt-realtime"}}`)
	status, body, err := h.rawRequest(http.MethodPost, "/v1/realtime/client_secrets",
		request, map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Value string `json:"value"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &issued) != nil || !strings.HasPrefix(issued.Value, "ek_") {
		t.Fatalf("native Realtime token status=%d body=%s", status, truncate(body, 300))
	}
	// The native issuance path stays outside Observer: no Key, execution or
	// upstream call, and the ephemeral token is not exposed by Observer reads.
	if len(h.keys(t)) != 0 || len(h.page(t, "page_size=100").Items) != 0 || h.mock.callCount() != 0 {
		t.Fatal("native Realtime token issuance entered Observer state")
	}
	for _, path := range []string{"/keys", "/requests?page_size=100", "/summary"} {
		if bytes.Contains(h.api(t, http.MethodGet, path, nil, http.StatusOK, nil), []byte(issued.Value)) {
			t.Fatalf("%s exposed the native ephemeral token", path)
		}
	}
	db, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte(h.stdout.String() + h.stderr.String())
	if bytes.Contains(db, []byte(issued.Value)) || bytes.Contains(logs, []byte(issued.Value)) {
		t.Fatal("native Realtime token entered Observer persistence or logs")
	}

	spec := validSpec()
	spec.AllowedModels = []string{modelName}
	restricted := h.createKey(t, spec)
	before := h.mock.callCount()
	status, body, err = h.rawRequest(http.MethodPost, "/v1/realtime/client_secrets",
		request, map[string]string{"Authorization": "Bearer " + restricted.Secret})
	if err != nil || status != http.StatusUnauthorized {
		t.Fatalf("Observer Key Realtime token status=%d err=%v body=%s, want 401",
			status, err, truncate(body, 300))
	}
	if h.mock.callCount() != before {
		t.Fatal("Observer Key Realtime token request reached an upstream")
	}
}

// A native API key on either Alpha Search alias keeps native execution while
// Observer is enabled with zero Keys. Emulation is a local loopback fixture;
// no real upstream or credential is contacted.
func TestNativeAlphaSearchExecutionUnchanged(t *testing.T) {
	var upstreamCalls atomic.Int32
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/alpha/search" ||
			r.Header.Get("Authorization") != "Bearer fake-alpha-upstream-key" {
			http.Error(w, "unexpected fake Alpha Search request", http.StatusBadRequest)
			return
		}
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"native_alpha_probe":true}`))
	}))
	defer alpha.Close()

	h := newHarness(t)
	h.awaitGracefulRestartReady(t, 0)
	h.Stop()
	path := filepath.Join(h.dir, "config.yaml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(cfg, &fields); err != nil {
		t.Fatal(err)
	}
	families, ok := fields["api-keys"].(map[string]any)
	if !ok {
		t.Fatal("isolated fixture must have a v8 api-keys map")
	}
	// The v8 config keeps the endpoint/models on the Codex group and opts
	// the fake API key into Alpha Search. The host normalizes YAML indentation
	// on startup, so modify decoded data rather than splice indented text.
	families["codex"] = []map[string]any{{
		"name":     "observer-alpha-fixture",
		"base-url": alpha.URL + "/v1",
		"keys":     []map[string]any{{"api-key": "fake-alpha-upstream-key", "alpha-search": true}},
		"models":   []map[string]any{{"name": "observer-alpha-model", "alias": "observer-alpha-model"}},
	}}
	cfg, err = yaml.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
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
		return err == nil && status == http.StatusOK && bytes.Contains(body, []byte(`"observer-alpha-model"`))
	})
	for _, route := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		status, body, err := h.rawRequest(http.MethodPost, route,
			[]byte(`{"model":"observer-alpha-model","query":"observer"}`), headers)
		if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte(`"native_alpha_probe":true`)) {
			t.Fatalf("native Alpha Search %s status=%d err=%v body=%s", route, status, err, truncate(body, 300))
		}
	}
	if upstreamCalls.Load() != 2 || len(h.keys(t)) != 0 ||
		len(h.page(t, "page_size=100").Items) != 0 || h.mock.callCount() != 0 {
		t.Fatal("native Alpha Search entered Observer state")
	}

	// A restricted Observer Key is still not native. It must not authenticate
	// either Alpha Search alias or reach the local upstream.
	spec := validSpec()
	spec.AllowedModels = []string{modelName}
	restricted := h.createKey(t, spec)
	for _, route := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		status, body, err := h.rawRequest(http.MethodPost, route,
			[]byte(`{"model":"observer-alpha-model","query":"observer"}`),
			map[string]string{"Authorization": "Bearer " + restricted.Secret})
		if err != nil || status != http.StatusUnauthorized {
			t.Fatalf("Observer Key Alpha Search %s status=%d err=%v body=%s, want 401",
				route, status, err, truncate(body, 300))
		}
	}
	if upstreamCalls.Load() != 2 {
		t.Fatal("Observer Key Alpha Search reached the upstream")
	}
}
