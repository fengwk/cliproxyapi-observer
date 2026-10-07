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

	// A single unauthenticated probe: the host must deny it (never 200).
	status, _, _, err := h.rawRequestWithHeaders(http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests", nil, nil)
	if err != nil {
		t.Fatalf("unauthorized probe: %v", err)
	}
	if status == http.StatusOK {
		t.Fatalf("unauthenticated management route unexpectedly succeeded")
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
	RequestID     string `json:"request_id"`
	TraceID       string `json:"trace_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	BodyAvailable bool   `json:"body_available"`
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
		Requests uint64 `json:"requests"`
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
// complete provider/model identities for a model that has no configured price.
func TestSummaryPreservesFullModelIdentity(t *testing.T) {
	h := newHarness(t)
	if status, body := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", chatBody(false)); status != http.StatusOK {
		t.Fatalf("chat status %d body %s", status, truncate(body, 300))
	}
	h.sendAndObserve(t, 25*time.Second, anyObservedRequest)

	deadline := time.Now().Add(10 * time.Second)
	var summary summaryView
	for time.Now().Before(deadline) {
		status, _, body := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary")
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &summary); err == nil && summary.Totals.Requests > 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
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
	invalidConfig := hostConfig(h.port, h.authDir, invalidDir, h.dbPath, h.mock.baseURL())

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
