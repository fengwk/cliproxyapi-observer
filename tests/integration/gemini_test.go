//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Fixed Gemini identities. The client-facing alias differs from the upstream
// model so routing and accounting must preserve the full model identity.
const (
	geminiModelName     = "observer-mock-gemini"
	geminiUpstreamModel = "observer-upstream-gemini"
	geminiUpstreamKey   = "fake-gemini-upstream-key"
	geminiCountPrompt   = "observer-gemini-count-prompt"
	geminiTotalTokens   = 17
)

// geminiMockUpstream is a local Gemini API endpoint. It records every attempt
// and verifies the original countTokens endpoint, credential and payload.
type geminiMockUpstream struct {
	server *httptest.Server

	mu         sync.Mutex
	calls      []mockCall
	violations []string
}

func newGeminiMockUpstream(t *testing.T) *geminiMockUpstream {
	t.Helper()
	m := &geminiMockUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handle)
	m.server = httptest.NewServer(mux)
	// Register before starting the host: t.Cleanup is LIFO, so the host stops
	// before this upstream that it may still be calling is closed.
	t.Cleanup(m.Close)
	return m
}

func (m *geminiMockUpstream) baseURL() string { return m.server.URL }

func (m *geminiMockUpstream) Close() { m.server.Close() }

func (m *geminiMockUpstream) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *geminiMockUpstream) callsSnapshot() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *geminiMockUpstream) violationf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

func (m *geminiMockUpstream) requireClean(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.violations {
		t.Errorf("gemini upstream contract violation: %s", v)
	}
	m.violations = nil
}

func (m *geminiMockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.violationf("read countTokens body: %v", err)
	}
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	m.mu.Unlock()

	m.validate(r, body)

	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(`{"totalTokens":%d}`, geminiTotalTokens))
}

// validate asserts the exact original Gemini countTokens contract as observed
// by a real local upstream: mapped upstream model, fake credential and the
// client contents normalized only by the host's native model field.
func (m *geminiMockUpstream) validate(r *http.Request, body []byte) {
	if r.Method != http.MethodPost {
		m.violationf("method = %s, want POST", r.Method)
	}
	wantPath := "/v1beta/models/" + geminiUpstreamModel + ":countTokens"
	if r.URL.Path != wantPath {
		m.violationf("path = %q, want %q", r.URL.Path, wantPath)
	}
	if got := r.Header.Get("x-goog-api-key"); got != geminiUpstreamKey {
		m.violationf("x-goog-api-key = %q, want the fake upstream key", got)
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		m.violationf("malformed countTokens body: %v", err)
		return
	}
	want := map[string]any{
		"model": geminiUpstreamModel,
		"contents": []any{
			map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": geminiCountPrompt}},
			},
		},
	}
	if !reflect.DeepEqual(payload, want) {
		m.violationf("countTokens body = %s, want %s", truncate(body, 400), `{"model":"`+geminiUpstreamModel+`","contents":[{"role":"user","parts":[{"text":"`+geminiCountPrompt+`"}]}]}`)
	}
	for _, secret := range []string{clientKey, mgmtKey} {
		if strings.Contains(string(body), secret) {
			m.violationf("countTokens body leaked credential %q", secret)
		}
	}
}

// geminiCountBody is the client-supplied native Gemini payload.
func geminiCountBody() []byte {
	return []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, geminiCountPrompt))
}

// geminiCount sends the original Gemini countTokens endpoint with the observer
// client key in the provider-native header.
func geminiCount(t *testing.T, h *harness, secret string) (int, []byte) {
	t.Helper()
	status, body, err := h.rawRequest(
		http.MethodPost,
		"/v1beta/models/"+geminiModelName+":countTokens",
		geminiCountBody(),
		map[string]string{"x-goog-api-key": secret},
	)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

func requireGeminiCount(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("Gemini countTokens status = %d body %s, want 200", status, truncate(body, 400))
	}
	var counted struct {
		TotalTokens int `json:"totalTokens"`
	}
	if err := json.Unmarshal(body, &counted); err != nil {
		t.Fatalf("Gemini countTokens response %s: %v", truncate(body, 400), err)
	}
	if counted.TotalTokens != geminiTotalTokens {
		t.Fatalf("Gemini countTokens = %d, want %d", counted.TotalTokens, geminiTotalTokens)
	}
}

// TestGeminiCountTokensBypassesExhaustedQuotaAndConcurrency proves a native
// Gemini countTokens request is authorized but never admitted: it succeeds
// while the Key's only concurrency slot is held by a charged generation and its
// total quota is already exhausted, and it neither reserves a slot nor bills.
func TestGeminiCountTokensBypassesExhaustedQuotaAndConcurrency(t *testing.T) {
	gemini := newGeminiMockUpstream(t)
	h := newHarnessWith(t, hostOptions{GeminiURL: gemini.baseURL()})

	spec := validSpec()
	spec.MaxConcurrent = 1
	created := h.createKey(t, spec)

	// A complete OpenAI fixture execution costs exactly 15 micro-USD.
	acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{
			acceptanceManual(modelName, acceptanceProvider, 1_000_000),
			acceptanceManual(upstreamModel, acceptanceProvider, 1_000_000),
		},
	})

	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("first inference status = %d body %s, want 200", status, truncate(body, 400))
	}
	h.await(t, "the first generation is charged 15 micro-USD", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].ID == created.Key.ID &&
			keys[0].Active == 0 && keys[0].Used.Total == 15 &&
			len(h.page(t, "page_size=100").Items) == 1
	})

	// Hold the second generation while the Key is still unlimited. Updating the
	// total limit to 1 while it is admitted makes the quota exhausted for every
	// subsequent admission without cancelling the in-flight execution.
	gate := h.mock.holdNextRequest()
	t.Cleanup(gate.open)
	second := startInference(context.Background(), h, created.Secret, false)
	awaitMockEntered(t, gate)

	spec.Limits.Total = 1
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(created.Key.ID), spec, http.StatusOK, nil)
	keys := h.keys(t)
	if len(keys) != 1 || keys[0].ID != created.Key.ID || keys[0].Active != 1 || keys[0].Used.Total != 15 {
		t.Fatalf("held generation state = %+v, want active=1 used.total=15", keys)
	}

	status, raw := geminiCount(t, h, created.Secret)
	requireGeminiCount(t, status, raw)
	if calls := gemini.callCount(); calls != 1 {
		t.Fatalf("Gemini upstream calls after first count = %d, want 1", calls)
	}
	keys = h.keys(t)
	if len(keys) != 1 || keys[0].Active != 1 || keys[0].Used.Total != 15 {
		t.Fatalf("count changed admission or usage state: %+v", keys)
	}

	// Repeating countTokens must not charge or reserve either.
	status, raw = geminiCount(t, h, created.Secret)
	requireGeminiCount(t, status, raw)
	if calls := gemini.callCount(); calls != 2 {
		t.Fatalf("Gemini upstream calls after repeated count = %d, want 2", calls)
	}
	if got := len(h.page(t, "page_size=100").Items); got != 1 {
		t.Fatalf("usage records after countTokens = %d, want the single generation", got)
	}
	keys = h.keys(t)
	if len(keys) != 1 || keys[0].Active != 1 || keys[0].Used.Total != 15 {
		t.Fatalf("repeated count changed admission or usage state: %+v", keys)
	}

	// Explicit model authorization still applies to countTokens and must reject
	// before any local Gemini upstream dispatch.
	spec.AllowedModels = []string{modelName}
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(created.Key.ID), spec, http.StatusOK, nil)
	before := gemini.callCount()
	status, raw = geminiCount(t, h, created.Secret)
	if status != http.StatusForbidden {
		t.Fatalf("count with disallowed Gemini model status = %d body %s, want 403", status, truncate(raw, 400))
	}
	if got := gemini.callCount(); got != before {
		t.Fatalf("disallowed count reached the Gemini upstream: calls %d -> %d", before, got)
	}

	spec.AllowedModels = []string{}
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(created.Key.ID), spec, http.StatusOK, nil)

	gate.open()
	completed := awaitInference(t, second)
	if completed.err != nil || completed.status != http.StatusOK {
		t.Fatalf("held generation result status=%d err=%v body=%s", completed.status, completed.err, truncate(completed.body, 400))
	}

	// Only the two real generations are usage records; both were charged 15 and
	// the completed generation released the sole concurrency slot.
	h.await(t, "two charged generations and no count usage", func() bool {
		records := h.page(t, "page_size=100").Items
		keys := h.keys(t)
		return len(records) == 2 && len(keys) == 1 &&
			keys[0].Active == 0 && keys[0].Used.Total == 30
	})
	for _, record := range h.page(t, "page_size=100").Items {
		if record.Model != upstreamModel {
			t.Errorf("usage record model = %q, want the complete CPA execution model %q", record.Model, upstreamModel)
		}
		if record.CostMicroUSD != 15 {
			t.Errorf("usage record cost = %d, want 15", record.CostMicroUSD)
		}
	}

	// Generation identity routing is unchanged: two OpenAI executions and two
	// Gemini countTokens calls, no Gemini generation attempt.
	if calls := h.mock.callCount(); calls != 2 {
		t.Errorf("OpenAI generation calls = %d, want 2", calls)
	}
	for _, call := range gemini.callsSnapshot() {
		if !strings.HasSuffix(call.Path, ":countTokens") {
			t.Errorf("Gemini upstream saw a non-count request: %s", call.Path)
		}
	}
	if calls := gemini.callCount(); calls != 2 {
		t.Errorf("Gemini countTokens calls = %d, want 2", calls)
	}

	h.mock.requireClean(t)
	gemini.requireClean(t)
}

// Native CPA access keys stay authorized on the Gemini wildcard route. Observer
// owns only its authenticated count/generation scopes and must not turn a native
// key into an unauthorized response just because the host's FullPath template
// cannot reveal the action. The plugin-disabled host is the exact baseline.
func TestGeminiCountTokensAcceptsNativeHostKey(t *testing.T) {
	gemini := newGeminiMockUpstream(t)
	baseline := newHarnessWith(t, hostOptions{PluginDisabled: true, GeminiURL: gemini.baseURL()})
	h := newHarnessWith(t, hostOptions{GeminiURL: gemini.baseURL()})

	controlStatus, controlBody := geminiCount(t, baseline, clientKey)
	requireGeminiCount(t, controlStatus, controlBody)
	status, body := geminiCount(t, h, clientKey)
	requireGeminiCount(t, status, body)
	var control, observed struct {
		TotalTokens int `json:"totalTokens"`
	}
	if json.Unmarshal(controlBody, &control) != nil || json.Unmarshal(body, &observed) != nil || control != observed {
		t.Fatalf("native Gemini count baseline=%s observer=%s", truncate(controlBody, 300), truncate(body, 300))
	}

	// Repeat on the plugin-enabled host: native keys remain ordinary host
	// credentials, not a one-shot Observer capability.
	status, body = geminiCount(t, h, clientKey)
	requireGeminiCount(t, status, body)
	if calls := gemini.callCount(); calls != 3 {
		t.Fatalf("Gemini upstream calls = %d, want baseline plus two native host counts", calls)
	}
	if keys := h.keys(t); len(keys) != 0 {
		t.Fatalf("native Gemini count created Observer keys: %+v", keys)
	}
	if records := h.page(t, "page_size=100"); len(records.Items) != 0 {
		t.Fatalf("native Gemini count entered Observer accounting: %+v", records.Items)
	}
	gemini.requireClean(t)
}
