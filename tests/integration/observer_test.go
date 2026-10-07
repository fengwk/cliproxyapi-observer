//go:build integration

package integration

import (
	"encoding/json"
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

const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'"

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
	Items      []observedRequest `json:"items"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
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
		t.Fatalf("requests status %d body %s", status, truncate(body, 300))
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
