//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

// TestInferenceUnchangedSyncAndStream proves the observer is purely passive:
// synchronous and streaming completions reach the client unchanged and the
// upstream sees the unmodified request.
func TestInferenceUnchangedSyncAndStream(t *testing.T) {
	h := newHarness(t)

	status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false))
	if status != http.StatusOK {
		t.Fatalf("sync status %d body %s", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("sync content = %q, want %q", got, fixtureText)
	}

	status, body = h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(true))
	if status != http.StatusOK {
		t.Fatalf("stream status %d body %s", status, truncate(body, 400))
	}
	if got := sseContent(body); got != fixtureText {
		t.Errorf("stream content = %q, want %q", got, fixtureText)
	}

	h.mock.requireClean(t)
}

// TestManagementRoutesAuthenticatedAndHardened verifies the host protects the
// data routes, the data responses are hardened, and the resources are public
// but secret-free and same-origin only.
func TestManagementRoutesAuthenticatedAndHardened(t *testing.T) {
	h := newHarness(t)

	// Unauthenticated and wrongly-authenticated probes must be denied with an
	// exact 401/403 (never a redirect, 404 or 200).
	for _, headers := range []map[string]string{
		nil,
		{"Authorization": "Bearer wrong-management-key"},
	} {
		status, _, _, err := h.rawRequestWithHeaders(http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests", nil, headers)
		if err != nil {
			t.Fatalf("unauthorized probe: %v", err)
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("unauthorized management route status = %d, want 401 or 403", status)
		}
	}

	for _, route := range []string{"/settings", "/health"} {
		status, header, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+route)
		if status != http.StatusOK {
			t.Fatalf("GET %s status %d body %s", route, status, truncate(body, 300))
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", route, got)
		}
		if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", route, got)
		}
		for _, secret := range []string{clientKey, mgmtKey, upstreamKey} {
			if strings.Contains(string(body), secret) {
				t.Errorf("GET %s leaked secret %q", route, secret)
			}
		}
	}

	// The data routes must also be reachable with management auth.
	if status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary"); status != http.StatusOK {
		t.Errorf("summary status %d body %s", status, truncate(body, 300))
	}

	resources := []struct {
		path        string
		contentType string
	}{
		{"/v0/resource/plugins/" + pluginID + "/ui", "text/html"},
		{"/v0/resource/plugins/" + pluginID + "/ui.js", "javascript"},
		{"/v0/resource/plugins/" + pluginID + "/ui.css", "text/css"},
	}
	for _, res := range resources {
		status, header, body, err := h.rawRequestWithHeaders(http.MethodGet, res.path, nil, nil)
		if err != nil {
			t.Fatalf("GET %s: %v", res.path, err)
		}
		if status != http.StatusOK {
			t.Fatalf("GET %s status %d", res.path, status)
		}
		if got := header.Get("Content-Type"); !strings.Contains(got, res.contentType) {
			t.Errorf("GET %s Content-Type = %q, want %q", res.path, got, res.contentType)
		}
		if csp := header.Get("Content-Security-Policy"); csp != wantCSP {
			t.Errorf("GET %s CSP = %q, want %q", res.path, csp, wantCSP)
		}
		if got := header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("GET %s Referrer-Policy = %q, want no-referrer", res.path, got)
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", res.path, got)
		}
		for _, secret := range []string{clientKey, mgmtKey, upstreamKey} {
			if strings.Contains(string(body), secret) {
				t.Errorf("GET %s leaked secret %q", res.path, secret)
			}
		}
	}
}

// observedRequest is one observed request metadata entry.
type observedRequest struct {
	RequestID           string   `json:"request_id"`
	TraceID             string   `json:"trace_id"`
	Provider            string   `json:"provider"`
	Model               string   `json:"model"`
	InputTokens         int64    `json:"input_tokens"`
	UncachedInputTokens int64    `json:"uncached_input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
	AccountingQuality   string   `json:"accounting_quality"`
	CacheHit            bool     `json:"cache_hit"`
	CostUSD             *float64 `json:"cost_usd"`
	BodyAvailable       bool     `json:"body_available"`
}

// requestPage mirrors the observer RequestPage wire shape.
type requestPage struct {
	Items   []observedRequest `json:"items"`
	Offset  int               `json:"offset"`
	Limit   int               `json:"limit"`
	HasMore bool              `json:"has_more"`
}

// summaryView mirrors the observer Summary wire shape.
type summaryView struct {
	Totals struct {
		Requests         uint64  `json:"requests"`
		InputTokens      uint64  `json:"input_tokens"`
		OutputTokens     uint64  `json:"output_tokens"`
		CacheReadTokens  uint64  `json:"cache_read_tokens"`
		TotalTokens      uint64  `json:"total_tokens"`
		CacheHits        uint64  `json:"cache_hits"`
		UnpricedRequests uint64  `json:"unpriced_requests"`
		CostUSD          float64 `json:"cost_usd"`
	} `json:"totals"`
	Groups []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"groups"`
}

func (h *harness) fetchRequests(t *testing.T) requestPage {
	t.Helper()
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?limit=50")
	if status != http.StatusOK {
		t.Fatalf("requests status %d body %s\n--- host log tail ---\n%s", status, truncate(body, 300), logTail(h.stdout.String(), h.stderr.String()))
	}
	var page requestPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode requests: %v\n%s", err, truncate(body, 300))
	}
	return page
}

// sendAndObserve sends a chat request and, because the host registers usage
// plugins during its asynchronous runtime sync, resends until the polled page
// satisfies ready. The assertion is still that the request was observed;
// retrying only tolerates the host's startup ordering.
func (h *harness) sendAndObserve(t *testing.T, timeout time.Duration, ready func(requestPage) bool) requestPage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		lastStatus, lastBody = h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false))
		if lastStatus != http.StatusOK {
			t.Fatalf("chat status %d body %s", lastStatus, truncate(lastBody, 300))
		}
		poll := time.Now().Add(2 * time.Second)
		for time.Now().Before(poll) && time.Now().Before(deadline) {
			page := h.fetchRequests(t)
			if ready(page) {
				return page
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatalf("no observed request within %s (last status %d body %s)\n--- host log tail ---\n%s", timeout, lastStatus, truncate(lastBody, 300), logTail(h.stdout.String(), h.stderr.String()))
	return requestPage{}
}

// anyObservedRequest is the common readiness predicate: at least one record.
func anyObservedRequest(page requestPage) bool { return len(page.Items) > 0 }

// bodyObserved is the readiness predicate for body correlation: a record that
// reports an available captured body.
func bodyObserved(page requestPage) bool {
	for _, item := range page.Items {
		if item.BodyAvailable {
			return true
		}
	}
	return false
}

func logTail(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 2000 {
			p = p[len(p)-2000:]
		}
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// TestRequestBodyObservedAndCorrelated drives one request and verifies the body
// is retrievable by the observed request's request_id with no credentials or
// headers persisted.
//
// Cross-slice note: the host mints a distinct RequestID for request interception
// and for the usage record (verified on v8.0.15 and v8.0.19), sharing only the
// TraceID. Correlation therefore requires the storage slice to resolve a
// request's body through its TraceID. If that join is missing this test fails
// by design, which is the intended integration signal.
func TestRequestBodyObservedAndCorrelated(t *testing.T) {
	h := newHarness(t)
	page := h.sendAndObserve(t, 25*time.Second, bodyObserved)

	var entry *observedRequest
	for i := range page.Items {
		if page.Items[i].BodyAvailable {
			entry = &page.Items[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("no observed request reported a captured body: %+v", page.Items)
	}
	if entry.RequestID == "" {
		t.Fatalf("observed request missing request_id: %+v", entry)
	}
	if entry.Model != modelName && entry.Model != upstreamModel {
		t.Errorf("observed model = %q, want %q or %q", entry.Model, modelName, upstreamModel)
	}

	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/body?request_id="+url.QueryEscape(entry.RequestID))
	if status != http.StatusOK {
		t.Fatalf("body status %d body %s", status, truncate(body, 300))
	}
	var detail struct {
		RequestID string `json:"request_id"`
		Body      string `json:"body"`
		Redacted  bool   `json:"redacted"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.Contains(detail.Body, upstreamPrompt) {
		t.Errorf("captured body missing the prompt marker: %s", truncate([]byte(detail.Body), 200))
	}
	for _, secret := range []string{clientKey, mgmtKey, upstreamKey, "Authorization"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("body response leaked %q", secret)
		}
	}
}

// TestSummaryPreservesFullModelIdentity verifies aggregated statistics keep the
// complete provider/model identities and the canonical OpenAI token accounting
// for a model that has no configured price.
func TestSummaryPreservesFullModelIdentity(t *testing.T) {
	h := newHarness(t)
	if status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false)); status != http.StatusOK {
		t.Fatalf("chat status %d body %s", status, truncate(body, 300))
	}
	page := h.sendAndObserve(t, 25*time.Second, anyObservedRequest)

	// Every observed request comes from the same fixture, so the canonical
	// counters are identical across the page.
	for _, item := range page.Items {
		if item.InputTokens != fixturePromptTokens {
			t.Errorf("request input_tokens = %d, want %d (%+v)", item.InputTokens, fixturePromptTokens, item)
		}
		if item.UncachedInputTokens != fixturePromptTokens-fixtureCachedTokens {
			t.Errorf("request uncached_input_tokens = %d, want %d", item.UncachedInputTokens, fixturePromptTokens-fixtureCachedTokens)
		}
		if item.OutputTokens != fixtureCompletionTokens {
			t.Errorf("request output_tokens = %d, want %d", item.OutputTokens, fixtureCompletionTokens)
		}
		if item.CacheReadTokens != fixtureCachedTokens {
			t.Errorf("request cache_read_tokens = %d, want %d", item.CacheReadTokens, fixtureCachedTokens)
		}
		if item.TotalTokens != fixtureTotalTokens {
			t.Errorf("request total_tokens = %d, want %d", item.TotalTokens, fixtureTotalTokens)
		}
		if !item.CacheHit {
			t.Errorf("request cache_hit = false, want true (%+v)", item)
		}
		if item.AccountingQuality != "complete" {
			t.Errorf("request accounting_quality = %q, want complete", item.AccountingQuality)
		}
		if item.CostUSD != nil {
			t.Errorf("unpriced request reported a cost: %v", *item.CostUSD)
		}
	}

	summary := h.fetchSummary(t)
	if summary.Totals.Requests == 0 {
		t.Fatalf("summary never reported the observed request")
	}
	preserved := false
	for _, group := range summary.Groups {
		if group.Model == modelName || group.Model == upstreamModel {
			preserved = true
		}
	}
	if !preserved {
		t.Errorf("summary did not preserve the full model identity: %+v", summary.Groups)
	}

	// Aggregates must be the per-request fixture multiplied by the observed
	// request count.
	requests := summary.Totals.Requests
	if want := requests * fixturePromptTokens; summary.Totals.InputTokens != want {
		t.Errorf("summary input_tokens = %d, want %d", summary.Totals.InputTokens, want)
	}
	if want := requests * fixtureCompletionTokens; summary.Totals.OutputTokens != want {
		t.Errorf("summary output_tokens = %d, want %d", summary.Totals.OutputTokens, want)
	}
	if want := requests * fixtureCachedTokens; summary.Totals.CacheReadTokens != want {
		t.Errorf("summary cache_read_tokens = %d, want %d", summary.Totals.CacheReadTokens, want)
	}
	if want := requests * fixtureTotalTokens; summary.Totals.TotalTokens != want {
		t.Errorf("summary total_tokens = %d, want %d", summary.Totals.TotalTokens, want)
	}
	if summary.Totals.CacheHits != requests {
		t.Errorf("summary cache_hits = %d, want %d", summary.Totals.CacheHits, requests)
	}
	if summary.Totals.UnpricedRequests != requests {
		t.Errorf("summary unpriced_requests = %d, want %d", summary.Totals.UnpricedRequests, requests)
	}
	if summary.Totals.CostUSD != 0 {
		t.Errorf("summary cost_usd = %v, want 0", summary.Totals.CostUSD)
	}
}

// fetchSummary polls the summary endpoint until it reports at least one request
// and its totals have settled, so per-request and aggregate counters are read
// from the same stable state.
func (h *harness) fetchSummary(t *testing.T) summaryView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var prev summaryView
	havePrev := false
	for time.Now().Before(deadline) {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary")
		if status == http.StatusOK {
			var summary summaryView
			if err := json.Unmarshal(body, &summary); err == nil && summary.Totals.Requests > 0 {
				if havePrev && summary.Totals == prev.Totals && len(summary.Groups) == len(prev.Groups) {
					return summary
				}
				prev = summary
				havePrev = true
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Fatalf("summary never reported a stable request count")
	return prev
}

// TestDatabasePersistsAcrossRestart verifies observed requests survive a host
// restart against the same database file.
func TestDatabasePersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	before := h.sendAndObserve(t, 25*time.Second, anyObservedRequest)
	wantID := before.Items[0].RequestID
	h.Stop()

	restarted, err := startHost(t, h.mock, h.dir)
	if err != nil {
		t.Fatalf("restart host: %v", err)
	}
	t.Cleanup(restarted.Stop)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range restarted.fetchRequests(t).Items {
			if item.RequestID == wantID {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("observed request %s did not survive restart", wantID)
}

// TestFailedPluginReplacementRollback verifies a broken native replacement
// rolls back to the previous working plugin and inference keeps serving.
func TestFailedPluginReplacementRollback(t *testing.T) {
	h := newHarness(t)

	// Prove inference works before the replacement.
	if status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false)); status != http.StatusOK {
		t.Fatalf("pre-replacement chat status %d body %s", status, truncate(body, 300))
	}

	invalidDir := filepath.Join(h.dir, "invalid-plugins")
	invalidLib := filepath.Join(invalidDir, runtime.GOOS, runtime.GOARCH, pluginLibName)
	if err := os.MkdirAll(filepath.Dir(invalidLib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidLib, []byte("not a shared library\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidConfig := hostConfig(h.port, h.authDir, invalidDir, h.dbPath, h.mock.baseURL(), defaultHostOptions())

	// The config watcher is installed shortly after readiness; wait for it so
	// the first write is not raced away, then retry the write if it was missed.
	h.waitForLog(t, "file watcher started", 10*time.Second)

	deadline := time.Now().Add(20 * time.Second)
	attempted := false
	lastWrite := time.Time{}
	for time.Now().Before(deadline) {
		if time.Since(lastWrite) > 3*time.Second {
			if err := os.WriteFile(filepath.Join(h.dir, "config.yaml"), invalidConfig, 0o600); err != nil {
				t.Fatal(err)
			}
			lastWrite = time.Now()
		}
		logs := h.stdout.String() + h.stderr.String()
		if strings.Contains(logs, "failed to load plugin "+pluginID) && strings.Contains(logs, invalidLib) {
			attempted = true
			break
		}
		select {
		case <-h.done:
			t.Fatalf("host exited during rollback: %v", h.waitError())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !attempted {
		t.Fatalf("host did not attempt the invalid replacement\n%s\n%s", h.stdout.String(), h.stderr.String())
	}

	for _, stream := range []bool{false, true} {
		status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(stream))
		if status != http.StatusOK {
			t.Fatalf("rollback stream=%t status %d body %s", stream, status, truncate(body, 300))
		}
		if stream {
			if got := sseContent(body); got != fixtureText {
				t.Errorf("rollback stream content = %q, want %q", got, fixtureText)
			}
		} else if got := jsonContent(body); got != fixtureText {
			t.Errorf("rollback sync content = %q, want %q", got, fixtureText)
		}
	}
	h.mock.requireClean(t)
}

// settingsResponse mirrors the observer settings wire shape.
type settingsResponse struct {
	CaptureBodies bool `json:"capture_bodies"`
}

// fetchBody retrieves a captured request body by request_id.
func (h *harness) fetchBody(t *testing.T, requestID string) (int, []byte) {
	t.Helper()
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/body?request_id="+url.QueryEscape(requestID))
	return status, body
}

// TestBodyCaptureDisabledBySetting verifies that with capture-bodies disabled
// the observer records request metadata but never stores a body, and the body
// route answers a sanitized 404 for an observed request.
func TestBodyCaptureDisabledBySetting(t *testing.T) {
	opts := defaultHostOptions()
	opts.CaptureBodies = false
	h := newHarnessWith(t, opts)

	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings")
	if status != http.StatusOK {
		t.Fatalf("settings status %d body %s", status, truncate(body, 300))
	}
	var settings settingsResponse
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings.CaptureBodies {
		t.Fatalf("settings capture_bodies = true, want false")
	}

	page := h.sendAndObserve(t, 25*time.Second, anyObservedRequest)
	entry := page.Items[0]
	if entry.BodyAvailable {
		t.Errorf("observed request reported a body while capture is disabled: %+v", entry)
	}
	status, body = h.fetchBody(t, entry.RequestID)
	if status != http.StatusNotFound {
		t.Fatalf("body status %d, want 404 for a disabled capture; body %s", status, truncate(body, 200))
	}
	for _, secret := range []string{clientKey, mgmtKey, upstreamKey, "Authorization"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("404 body leaked %q", secret)
		}
	}
}

// TestQueryRangesAcceptedAndBounded verifies the dashboard's 7d/30d selections
// are accepted (the store clamps to retention) while genuinely invalid ranges
// are rejected with 400.
func TestQueryRangesAcceptedAndBounded(t *testing.T) {
	h := newHarness(t)
	h.sendAndObserve(t, 25*time.Second, anyObservedRequest)

	now := time.Now().UTC()
	at := func(d time.Duration) string { return url.QueryEscape(now.Add(d).Format(time.RFC3339)) }

	cases := []struct {
		name   string
		route  string
		query  string
		status int
	}{
		{"requests 7d", "/requests", "?from=" + at(-7*24*time.Hour), http.StatusOK},
		{"requests 30d", "/requests", "?from=" + at(-30*24*time.Hour), http.StatusOK},
		{"summary 7d", "/summary", "?from=" + at(-7*24*time.Hour), http.StatusOK},
		{"summary 30d", "/summary", "?from=" + at(-30*24*time.Hour), http.StatusOK},
		{"beyond retention", "/requests", "?from=" + at(-4000*24*time.Hour), http.StatusBadRequest},
		{"future upper", "/requests", "?to=" + at(time.Hour), http.StatusBadRequest},
		{"empty range", "/requests", "?from=" + at(0) + "&to=" + at(0), http.StatusBadRequest},
		{"inverted range", "/summary", "?from=" + at(0) + "&to=" + at(-time.Hour), http.StatusBadRequest},
	}
	for _, tc := range cases {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+tc.route+tc.query)
		if status != tc.status {
			t.Errorf("%s status = %d, want %d; body %s", tc.name, status, tc.status, truncate(body, 200))
		}
	}

	// A 7d request window must still return the retained observation.
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?from="+at(-7*24*time.Hour))
	if status != http.StatusOK {
		t.Fatalf("7d requests status %d body %s", status, truncate(body, 200))
	}
	var page requestPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode 7d page: %v", err)
	}
	if len(page.Items) == 0 {
		t.Errorf("7d request window returned no retained observations")
	}
}

// TestConcurrentRequestsKeepBodiesIsolated drives two simultaneous requests with
// distinct prompts and verifies each captured body carries exactly its own
// prompt, proving concurrent traces never cross their request bodies.
func TestConcurrentRequestsKeepBodiesIsolated(t *testing.T) {
	h := newHarness(t)
	h.mock.allowAnyPrompt()

	// Warm up plugin registration and record the pre-existing request IDs.
	baseline := h.sendAndObserve(t, 25*time.Second, anyObservedRequest)
	seen := make(map[string]bool, len(baseline.Items))
	for _, item := range baseline.Items {
		seen[item.RequestID] = true
	}

	const (
		promptA = "observer-concurrent-alpha"
		promptB = "observer-concurrent-beta"
	)
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, prompt := range []string{promptA, promptB} {
		wg.Add(1)
		go func(index int, content string) {
			defer wg.Done()
			var status int
			status, _ = h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBodyWithPrompt(false, content))
			statuses[index] = status
		}(i, prompt)
	}
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent request %d status %d", i, status)
		}
	}

	// Wait until both new requests appear with captured bodies.
	deadline := time.Now().Add(25 * time.Second)
	var fresh []observedRequest
	for time.Now().Before(deadline) {
		page := h.fetchRequests(t)
		fresh = fresh[:0]
		for _, item := range page.Items {
			if !seen[item.RequestID] && item.BodyAvailable {
				fresh = append(fresh, item)
			}
		}
		if len(fresh) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(fresh) < 2 {
		t.Fatalf("expected 2 freshly observed requests with bodies, got %d\n%s", len(fresh), logTail(h.stdout.String(), h.stderr.String()))
	}

	containsA, containsB := 0, 0
	for _, entry := range fresh[:2] {
		status, body := h.fetchBody(t, entry.RequestID)
		if status != http.StatusOK {
			t.Fatalf("body %s status %d body %s", entry.RequestID, status, truncate(body, 200))
		}
		var detail struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &detail); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		hasA := strings.Contains(detail.Body, promptA)
		hasB := strings.Contains(detail.Body, promptB)
		if hasA == hasB {
			t.Errorf("body %s does not map to exactly one prompt (alpha=%t beta=%t): %s", entry.RequestID, hasA, hasB, truncate([]byte(detail.Body), 200))
		}
		if hasA {
			containsA++
		}
		if hasB {
			containsB++
		}
	}
	if containsA != 1 || containsB != 1 {
		t.Fatalf("concurrent bodies crossed: alpha=%d beta=%d", containsA, containsB)
	}
	h.mock.requireClean(t)
}

// reconfigureUntil rewrites the host config until done reports the new plugin
// configuration has taken effect, retrying in case the watcher misses a write.
func (h *harness) reconfigureUntil(t *testing.T, opts hostOptions, done func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastWrite := time.Time{}
	for time.Now().Before(deadline) {
		if time.Since(lastWrite) > 3*time.Second {
			if err := h.rewriteConfig(t, opts); err != nil {
				t.Fatalf("rewrite config: %v", err)
			}
			lastWrite = time.Now()
		}
		if done() {
			return
		}
		select {
		case <-h.done:
			t.Fatalf("host exited during reconfigure: %v", h.waitError())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reconfigure did not take effect within %s\n%s", timeout, logTail(h.stdout.String(), h.stderr.String()))
}

// TestReconfigureSwitchesDatabase verifies a plugin reconfigure to a new
// database isolates the previous data: the old request is no longer readable
// (404), the previous database file is preserved, and new traffic is observed
// into the new database while inference keeps serving.
func TestReconfigureSwitchesDatabase(t *testing.T) {
	h := newHarness(t)
	page := h.sendAndObserve(t, 25*time.Second, bodyObserved)
	var old observedRequest
	for _, item := range page.Items {
		if item.BodyAvailable {
			old = item
			break
		}
	}
	if old.RequestID == "" {
		t.Fatalf("no prior observation with a captured body: %+v", page.Items)
	}
	if status, _ := h.fetchBody(t, old.RequestID); status != http.StatusOK {
		t.Fatalf("pre-reconfigure body status %d, want 200", status)
	}

	h.waitForLog(t, "file watcher started", 10*time.Second)
	newDB := filepath.Join(h.dir, "observer-rotated.db")
	opts := defaultHostOptions()
	opts.DatabasePath = newDB

	// The new database is empty, so the previous request disappears from the
	// list; that is the reconfigure signal.
	h.reconfigureUntil(t, opts, func() bool {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?limit=50")
		if status != http.StatusOK {
			return false
		}
		var p requestPage
		if err := json.Unmarshal(body, &p); err != nil {
			return false
		}
		for _, item := range p.Items {
			if item.RequestID == old.RequestID {
				return false
			}
		}
		return true
	}, 25*time.Second)

	// The old body must no longer be retrievable through the new store.
	if status, _ := h.fetchBody(t, old.RequestID); status != http.StatusNotFound {
		t.Errorf("old body status after reconfigure = %d, want 404", status)
	}
	// The previous database file is preserved on disk, not deleted.
	if _, err := os.Stat(h.dbPath); err != nil {
		t.Errorf("previous database file was removed: %v", err)
	}
	if _, err := os.Stat(newDB); err != nil {
		t.Errorf("new database file was not created: %v", err)
	}

	// Inference still serves and the new database records fresh traffic.
	if status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false)); status != http.StatusOK {
		t.Fatalf("post-reconfigure chat status %d body %s", status, truncate(body, 300))
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if page := h.fetchRequests(t); len(page.Items) > 0 {
			h.mock.requireClean(t)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("new database never observed fresh traffic\n%s", logTail(h.stdout.String(), h.stderr.String()))
}

// TestManagementSettingsAndValidateRealHost verifies GET /settings returns compaction
// and price configuration, and POST /validate enforces auth, sanitization, and candidate
// validity on a real CPA host.
func TestManagementSettingsAndValidateRealHost(t *testing.T) {
	h := newHarness(t)

	// 1. GET /settings contains compact fields and prices.
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings")
	if status != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200, body = %s", status, truncate(body, 300))
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("parse settings json: %v", err)
	}
	if _, ok := settings["compact_interval_seconds"]; !ok {
		t.Errorf("settings missing compact_interval_seconds: %v", settings)
	}
	if _, ok := settings["compact_min_bytes"]; !ok {
		t.Errorf("settings missing compact_min_bytes: %v", settings)
	}
	if _, ok := settings["prices"]; !ok {
		t.Errorf("settings missing prices: %v", settings)
	}

	// 2. POST /validate authentication enforcement: 401/403 for unauthorized probes.
	for _, headers := range []map[string]string{
		nil,
		{"Authorization": "Bearer wrong-management-key"},
	} {
		status, _, _, err := h.rawRequestWithHeaders(http.MethodPost, "/v0/management/plugins/"+pluginID+"/validate", []byte(`{"compact-interval": "20m"}`), headers)
		if err != nil {
			t.Fatalf("unauthorized validate probe error: %v", err)
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("unauthorized POST /validate status = %d, want 401 or 403", status)
		}
	}

	// 3. POST /validate valid patch returns 200 {"valid": true}.
	validPatch := []byte(`{
		"capture-bodies": true,
		"compact-interval": "20m",
		"compact-min-bytes": 10485760,
		"prices": {
			"gpt-4": {
				"input": 2.5,
				"output": 10.0,
				"cache_read": 0.25,
				"cache_creation": 2.5
			}
		}
	}`)
	status, _, body, err := h.rawRequestWithHeaders(http.MethodPost, "/v0/management/plugins/"+pluginID+"/validate", validPatch, map[string]string{"Authorization": "Bearer " + mgmtKey})
	if err != nil {
		t.Fatalf("POST /validate valid patch error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("POST /validate valid patch status = %d, want 200, body = %s", status, truncate(body, 300))
	}
	var validResp map[string]bool
	if err := json.Unmarshal(body, &validResp); err != nil {
		t.Fatalf("parse validate response: %v", err)
	}
	if !validResp["valid"] {
		t.Errorf("validate response valid = false, want true")
	}

	// 4. POST /validate invalid patch returns 400 and sanitized error "invalid settings patch".
	invalidPatch := []byte(`{"compact-interval": "30s"}`) // below 1m bound
	status, _, body, err = h.rawRequestWithHeaders(http.MethodPost, "/v0/management/plugins/"+pluginID+"/validate", invalidPatch, map[string]string{"Authorization": "Bearer " + mgmtKey})
	if err != nil {
		t.Fatalf("POST /validate invalid patch error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("POST /validate invalid patch status = %d, want 400, body = %s", status, truncate(body, 300))
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if errResp["error"] != "invalid settings patch" {
		t.Errorf("POST /validate error = %q, want %q", errResp["error"], "invalid settings patch")
	}
}

// TestCorePatchRoundTripObserverConfig verifies that a shallow PATCH to core config
// updates compaction settings and prices with cache_read/cache_creation, persists to
// the host config file, propagates to GET /settings, and preserves existing db/enabled/flush.
func TestCorePatchRoundTripObserverConfig(t *testing.T) {
	h := newHarness(t)

	// 1. Shallow PATCH candidate configuration to core config.
	shallowPatch := []byte(`{
		"compact-interval": "20m",
		"compact-min-bytes": 10485760,
		"prices": {
			"gpt-4": {
				"input": 2.5,
				"output": 10.0,
				"cache_read": 1.25,
				"cache_creation": 2.5
			}
		}
	}`)

	status, _, body, err := h.rawRequestWithHeaders(http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", shallowPatch, map[string]string{
		"Authorization": "Bearer " + mgmtKey,
		"Content-Type":  "application/json",
	})
	if err != nil {
		t.Fatalf("PATCH observer config error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("PATCH observer config status = %d, want 200, body = %s", status, truncate(body, 300))
	}

	// 2. Poll GET /settings until the new compaction settings and prices take effect.
	deadline := time.Now().Add(15 * time.Second)
	var lastSettings map[string]any
	var matched bool
	for time.Now().Before(deadline) {
		status, _, body, err := h.rawRequestWithHeaders(http.MethodGet, "/v0/management/plugins/"+pluginID+"/settings", nil, map[string]string{
			"Authorization": "Bearer " + mgmtKey,
		})
		if err == nil && status == http.StatusOK {
			var s map[string]any
			if err := json.Unmarshal(body, &s); err == nil {
				lastSettings = s
				interval, _ := s["compact_interval_seconds"].(float64)
				minBytes, _ := s["compact_min_bytes"].(float64)
				prices, _ := s["prices"].(map[string]any)
				if interval == 1200 && minBytes == 10485760 && prices != nil {
					if gpt4, ok := prices["gpt-4"].(map[string]any); ok {
						cr, _ := gpt4["cache_read"].(float64)
						cc, _ := gpt4["cache_creation"].(float64)
						if cr == 1.25 && cc == 2.5 {
							matched = true
							break
						}
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !matched {
		t.Fatalf("settings did not reflect patched values in time: %+v", lastSettings)
	}

	// 3. Read host config file on disk to prove persistence.
	configPath := filepath.Join(h.dir, "config.yaml")
	rawFile, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read host config file: %v", err)
	}
	configStr := string(rawFile)
	if !strings.Contains(configStr, "20m") || !strings.Contains(configStr, "10485760") {
		t.Errorf("host config file did not persist compaction settings: %s", configStr)
	}
	if !strings.Contains(configStr, "gpt-4") {
		t.Errorf("host config file did not persist prices: %s", configStr)
	}

	// 4. GET config and prove that db, enabled, and flush are preserved.
	status, _, body, err = h.rawRequestWithHeaders(http.MethodGet, "/v0/management/plugins/"+pluginID+"/config", nil, map[string]string{
		"Authorization": "Bearer " + mgmtKey,
	})
	var pluginCfg map[string]any
	if err == nil && status == http.StatusOK {
		if err := json.Unmarshal(body, &pluginCfg); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatalf("GET config failed: status %d body %s error %v", status, string(body), err)
	}
	if pluginCfg == nil {
		t.Fatalf("failed to decode observer plugin config from GET config: %s", body)
	}

	dbVal, _ := pluginCfg["db"].(string)
	if !strings.Contains(dbVal, "observer.db") {
		t.Errorf("db setting was not preserved: %v", pluginCfg["db"])
	}
	if enabledVal, ok := pluginCfg["enabled"].(bool); !ok || !enabledVal {
		t.Errorf("enabled setting was not preserved: %v", pluginCfg["enabled"])
	}
	if flushVal, _ := pluginCfg["flush"].(string); flushVal != "1s" {
		t.Errorf("flush setting was not preserved: %v", pluginCfg["flush"])
	}
}

func approxEqual(a, b, tol float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= tol
}

func (h *harness) fetchCurrentSummary(t *testing.T) (summaryView, error) {
	t.Helper()
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary")
	if status != http.StatusOK {
		return summaryView{}, fmt.Errorf("summary status %d body %s", status, truncate(body, 200))
	}
	var s summaryView
	if err := json.Unmarshal(body, &s); err != nil {
		return summaryView{}, fmt.Errorf("unmarshal summary: %w", err)
	}
	return s, nil
}

// TestQueryTimePricingAcrossConfigPatchAndRestart drives an observed request without configured
// prices, validates it remains unpriced, then PATCHes prices to verify query-time pricing on the
// same historical request and summary, updates prices again to verify dynamic recalculation, and
// restarts the host to ensure persisted token counts continue pricing under effective config.
func TestQueryTimePricingAcrossConfigPatchAndRestart(t *testing.T) {
	h := newHarness(t)

	// Step 1: Record a request with no prices configured.
	page := h.sendAndObserve(t, 25*time.Second, anyObservedRequest)
	if len(page.Items) == 0 {
		t.Fatalf("expected at least 1 observed request")
	}
	target := page.Items[0]
	targetID := target.RequestID
	if targetID == "" {
		t.Fatalf("observed request has empty request_id")
	}
	if target.CostUSD != nil {
		t.Fatalf("unpriced request reported cost: %v, want nil", *target.CostUSD)
	}

	initialSummary := h.fetchSummary(t)
	if initialSummary.Totals.Requests == 0 {
		t.Fatalf("summary reported 0 requests")
	}
	if initialSummary.Totals.UnpricedRequests == 0 {
		t.Errorf("summary unpriced_requests = %d, want > 0", initialSummary.Totals.UnpricedRequests)
	}
	if initialSummary.Totals.CostUSD != 0 {
		t.Errorf("summary cost_usd = %v, want 0 for unpriced requests", initialSummary.Totals.CostUSD)
	}

	// Calculate prices:
	// fixture prompt=10 tokens (uncached=6, cached=4), completion=5 tokens.
	// Formula: (uncached * input + cached * cache_read + cache_write * cache_creation + completion * output) / 1,000,000
	const (
		// Price map 1
		p1Input     = 2.5
		p1Output    = 10.0
		p1CacheRead = 1.25
		p1CacheGen  = 2.5

		// Price map 2 (double)
		p2Input     = 5.0
		p2Output    = 20.0
		p2CacheRead = 2.5
		p2CacheGen  = 5.0
	)
	wantCost1 := (6.0*p1Input + 4.0*p1CacheRead + 0.0*p1CacheGen + 5.0*p1Output) / 1_000_000.0 // 0.000070
	wantCost2 := (6.0*p2Input + 4.0*p2CacheRead + 0.0*p2CacheGen + 5.0*p2Output) / 1_000_000.0 // 0.000140

	patchPrice := func(in, out, cr, cg float64) {
		patchPayload := fmt.Sprintf(`{
			"prices": {
				%q: {"input": %f, "output": %f, "cache_read": %f, "cache_creation": %f},
				%q: {"input": %f, "output": %f, "cache_read": %f, "cache_creation": %f}
			}
		}`, modelName, in, out, cr, cg, upstreamModel, in, out, cr, cg)

		status, _, body, err := h.rawRequestWithHeaders(
			http.MethodPatch,
			"/v0/management/plugins/"+pluginID+"/config",
			[]byte(patchPayload),
			map[string]string{
				"Authorization": "Bearer " + mgmtKey,
				"Content-Type":  "application/json",
			},
		)
		if err != nil {
			t.Fatalf("PATCH config error: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("PATCH config status = %d, body = %s", status, truncate(body, 300))
		}
	}

	// Step 2: PATCH prices (map 1) and verify the SAME historical request and summary reflect current price.
	patchPrice(p1Input, p1Output, p1CacheRead, p1CacheGen)

	// The config watcher may briefly quiesce the store. Only 503 is retryable;
	// all other failures remain fatal, and the surrounding deadline is unchanged.
	fetchDuringReload := func(host *harness) (requestPage, bool) {
		status, _, body := host.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?limit=50")
		if status == http.StatusServiceUnavailable {
			return requestPage{}, false
		}
		if status != http.StatusOK {
			t.Fatalf("requests status %d body %s\n--- host log tail ---\n%s", status, truncate(body, 300), logTail(host.stdout.String(), host.stderr.String()))
		}
		var page requestPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode requests: %v\n%s", err, truncate(body, 300))
		}
		return page, true
	}

	deadline := time.Now().Add(15 * time.Second)
	var matchedRequest bool
	var matchedSummary bool
	var lastReqCost *float64
	var lastSumCost float64
	var lastUnpriced uint64

	for time.Now().Before(deadline) {
		reqs, ready := fetchDuringReload(h)
		if !ready {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for _, it := range reqs.Items {
			if it.RequestID == targetID {
				lastReqCost = it.CostUSD
				if it.CostUSD != nil && approxEqual(*it.CostUSD, wantCost1, 1e-9) {
					matchedRequest = true
				}
				break
			}
		}

		s, err := h.fetchCurrentSummary(t)
		if err == nil {
			lastSumCost = s.Totals.CostUSD
			lastUnpriced = s.Totals.UnpricedRequests
			wantTotal := float64(s.Totals.Requests) * wantCost1
			if approxEqual(s.Totals.CostUSD, wantTotal, 1e-9) && s.Totals.UnpricedRequests == 0 {
				matchedSummary = true
			}
		}

		if matchedRequest && matchedSummary {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !matchedRequest {
		t.Fatalf("request %s did not reflect price 1 (%v) at query-time; last cost = %v", targetID, wantCost1, lastReqCost)
	}
	if !matchedSummary {
		t.Fatalf("summary did not reflect price 1 (%v) at query-time; last cost = %v, unpriced = %d", wantCost1, lastSumCost, lastUnpriced)
	}

	// Step 3: Modify prices (map 2) and verify dynamic update on the same request and summary.
	patchPrice(p2Input, p2Output, p2CacheRead, p2CacheGen)

	deadline = time.Now().Add(15 * time.Second)
	matchedRequest = false
	matchedSummary = false

	for time.Now().Before(deadline) {
		reqs, ready := fetchDuringReload(h)
		if !ready {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for _, it := range reqs.Items {
			if it.RequestID == targetID {
				lastReqCost = it.CostUSD
				if it.CostUSD != nil && approxEqual(*it.CostUSD, wantCost2, 1e-9) {
					matchedRequest = true
				}
				break
			}
		}

		s, err := h.fetchCurrentSummary(t)
		if err == nil {
			lastSumCost = s.Totals.CostUSD
			lastUnpriced = s.Totals.UnpricedRequests
			wantTotal := float64(s.Totals.Requests) * wantCost2
			if approxEqual(s.Totals.CostUSD, wantTotal, 1e-9) && s.Totals.UnpricedRequests == 0 {
				matchedSummary = true
			}
		}

		if matchedRequest && matchedSummary {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !matchedRequest {
		t.Fatalf("request %s did not reflect updated price 2 (%v); last cost = %v", targetID, wantCost2, lastReqCost)
	}
	if !matchedSummary {
		t.Fatalf("summary did not reflect updated price 2 (%v); last cost = %v, unpriced = %d", wantCost2, lastSumCost, lastUnpriced)
	}

	// Step 4: Reboot host preserving updated config and database, then verify request and aggregate.
	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatalf("reboot host failed: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	matchedRequest = false
	matchedSummary = false

	for time.Now().Before(deadline) {
		reqs, ready := fetchDuringReload(restarted)
		if !ready {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for _, it := range reqs.Items {
			if it.RequestID == targetID {
				lastReqCost = it.CostUSD
				if it.CostUSD != nil && approxEqual(*it.CostUSD, wantCost2, 1e-9) {
					matchedRequest = true
				}
				break
			}
		}

		s, err := restarted.fetchCurrentSummary(t)
		if err == nil {
			lastSumCost = s.Totals.CostUSD
			lastUnpriced = s.Totals.UnpricedRequests
			wantTotal := float64(s.Totals.Requests) * wantCost2
			if approxEqual(s.Totals.CostUSD, wantTotal, 1e-9) && s.Totals.UnpricedRequests == 0 {
				matchedSummary = true
			}
		}

		if matchedRequest && matchedSummary {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !matchedRequest {
		t.Fatalf("after reboot, request %s did not reflect price (%v); last cost = %v", targetID, wantCost2, lastReqCost)
	}
	if !matchedSummary {
		t.Fatalf("after reboot, summary did not reflect aggregate cost (%v); last cost = %v, unpriced = %d", wantCost2, lastSumCost, lastUnpriced)
	}
}

// TestRequestPaginationOffsetLimitAndLegacyCursorValidation validates:
// 1. Rejection of nonempty legacy cursor with 400.
// 2. Rejection of invalid offsets (negative, non-numeric, overflow, empty) with 400.
// 3. Offset/limit navigation controls (page 1, next page, previous page return).
func TestRequestPaginationOffsetLimitAndLegacyCursorValidation(t *testing.T) {
	h := newHarness(t)
	h.mock.allowAnyPrompt()

	// 1. Validation of nonempty legacy cursor returns 400.
	cursorCases := []string{
		"legacy-cursor",
		"eyJ0cyI6MTIzNDU2fQ==",
		"1",
	}
	for _, c := range cursorCases {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?cursor="+url.QueryEscape(c))
		if status != http.StatusBadRequest {
			t.Errorf("cursor=%q status = %d, want 400; body = %s", c, status, truncate(body, 200))
		}
	}

	// 2. Validation of invalid offset returns 400.
	invalidOffsetCases := []struct {
		name  string
		query string
	}{
		{"negative offset", "?offset=-1"},
		{"alpha offset", "?offset=abc"},
		{"overflow offset", "?offset=2147483648"},
		{"empty offset", "?offset="},
		{"float offset", "?offset=1.5"},
	}
	for _, tc := range invalidOffsetCases {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests"+tc.query)
		if status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400; body = %s", tc.name, status, truncate(body, 200))
		}
	}

	// 3. Record multiple requests to test pagination controls (page 1 -> next page -> return).
	// Warm up observer and record first request.
	h.sendAndObserve(t, 25*time.Second, anyObservedRequest)

	// Drive additional distinct requests.
	prompts := []string{"paging-req-2", "paging-req-3"}
	for _, prompt := range prompts {
		time.Sleep(20 * time.Millisecond)
		status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBodyWithPrompt(false, prompt))
		if status != http.StatusOK {
			t.Fatalf("client chat status %d body %s", status, truncate(body, 200))
		}
	}

	// Poll until at least 3 requests are observed.
	deadline := time.Now().Add(25 * time.Second)
	var allObserved requestPage
	for time.Now().Before(deadline) {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?limit=50&offset=0")
		if status == http.StatusOK {
			var p requestPage
			if err := json.Unmarshal(body, &p); err == nil && len(p.Items) >= 3 {
				allObserved = p
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(allObserved.Items) < 3 {
		t.Fatalf("expected at least 3 observed requests, got %d", len(allObserved.Items))
	}

	allIDs := make([]string, len(allObserved.Items))
	for i, item := range allObserved.Items {
		allIDs[i] = item.RequestID
	}

	// Page 1: offset=0, limit=2
	status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?offset=0&limit=2")
	if status != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200; body = %s", status, truncate(body, 200))
	}
	var page1 requestPage
	if err := json.Unmarshal(body, &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v", err)
	}
	if page1.Offset != 0 {
		t.Errorf("page 1 offset = %d, want 0", page1.Offset)
	}
	if page1.Limit != 2 {
		t.Errorf("page 1 limit = %d, want 2", page1.Limit)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page 1 len = %d, want 2", len(page1.Items))
	}
	if !page1.HasMore {
		t.Errorf("page 1 has_more = false, want true")
	}
	if page1.Items[0].RequestID != allIDs[0] || page1.Items[1].RequestID != allIDs[1] {
		t.Errorf("page 1 items mismatch: got [%s, %s], want [%s, %s]",
			page1.Items[0].RequestID, page1.Items[1].RequestID, allIDs[0], allIDs[1])
	}

	// Page 2 (Next page): offset=2, limit=2
	status, _, body = h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?offset=2&limit=2")
	if status != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200; body = %s", status, truncate(body, 200))
	}
	var page2 requestPage
	if err := json.Unmarshal(body, &page2); err != nil {
		t.Fatalf("unmarshal page 2: %v", err)
	}
	if page2.Offset != 2 {
		t.Errorf("page 2 offset = %d, want 2", page2.Offset)
	}
	if page2.Limit != 2 {
		t.Errorf("page 2 limit = %d, want 2", page2.Limit)
	}
	if len(page2.Items) < 1 {
		t.Fatalf("page 2 len = %d, want at least 1", len(page2.Items))
	}
	if page2.Items[0].RequestID != allIDs[2] {
		t.Errorf("page 2 item[0] mismatch: got %s, want %s", page2.Items[0].RequestID, allIDs[2])
	}

	// Return (Previous page): offset=0, limit=2
	status, _, body = h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?offset=0&limit=2")
	if status != http.StatusOK {
		t.Fatalf("return page 1 status = %d, want 200; body = %s", status, truncate(body, 200))
	}
	var pageBack requestPage
	if err := json.Unmarshal(body, &pageBack); err != nil {
		t.Fatalf("unmarshal return page: %v", err)
	}
	if pageBack.Offset != 0 {
		t.Errorf("return page offset = %d, want 0", pageBack.Offset)
	}
	if len(pageBack.Items) != 2 {
		t.Fatalf("return page len = %d, want 2", len(pageBack.Items))
	}
	if pageBack.Items[0].RequestID != page1.Items[0].RequestID || pageBack.Items[1].RequestID != page1.Items[1].RequestID {
		t.Errorf("return page items differed from original page 1")
	}
}
