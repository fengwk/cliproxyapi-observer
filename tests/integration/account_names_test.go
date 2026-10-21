//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	kimiAccountFile = "observer-friendly-account.json"
	kimiModel       = "kimi-k2"
	kimiFakeToken   = "fake-upstream-key"
)

// kimiAccountUpstream is a local OpenAI-compatible fixture used by the fake
// kimi auth file. It accepts exactly one endpoint and the fake credential; it
// never contacts an external service.
type kimiAccountUpstream struct {
	server     *httptest.Server
	mu         sync.Mutex
	calls      int
	violations []string
}

func newKimiAccountUpstream(t *testing.T) *kimiAccountUpstream {
	t.Helper()
	m := &kimiAccountUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", m.handle)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *kimiAccountUpstream) url() string { return m.server.URL }

func (m *kimiAccountUpstream) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	m.mu.Lock()
	m.calls++
	if r.Method != http.MethodPost {
		m.violations = append(m.violations, "method="+r.Method)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+kimiFakeToken {
		m.violations = append(m.violations, "authorization="+got)
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-kimi-fixture","object":"chat.completion","created":1,"model":"k2","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, fixtureText)
}

func (m *kimiAccountUpstream) snapshot() (int, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]string(nil), m.violations...)
}

// A real CPA host reads one fake kimi auth file, routes kimi-k2 to a local
// upstream, and exposes the same auth index/name through the native metadata
// bridge. Account views and auth_index facets must show the file name while
// retaining the AuthIndex identity and aggregate.
func TestAccountNamesNativeMetadata(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := newKimiAccountUpstream(t)
	authJSON := fmt.Sprintf(`{"type":"kimi","access_token":%q,"base_url":%q,"email":"fixture@example.invalid","expired":"2099-01-01T00:00:00Z"}`,
		kimiFakeToken, upstream.url()+"/v1")
	if err := os.WriteFile(filepath.Join(authDir, kimiAccountFile), []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := newMockUpstream(t)
	t.Cleanup(mock.Close)
	h, err := startHost(t, mock, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)

	status, _, body, err := h.rawRequestWithHeaders(http.MethodGet, "/v0/management/auth-files", nil,
		map[string]string{"Authorization": "Bearer " + mgmtKey})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("auth-files status %d: %s", status, truncate(body, 500))
	}
	var authFiles struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &authFiles); err != nil {
		t.Fatalf("auth-files JSON: %v: %s", err, truncate(body, 500))
	}
	authIndex := ""
	for _, file := range authFiles.Files {
		if file.Name == kimiAccountFile {
			authIndex = file.AuthIndex
		}
	}
	if authIndex == "" {
		t.Fatalf("fixture auth file missing from metadata: %s", truncate(body, 500))
	}

	created := h.createKey(t, validSpec())
	payload := []byte(`{"model":"` + kimiModel + `","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	status, _, body, err = h.rawRequestWithHeaders(http.MethodPost, "/v1/chat/completions", payload,
		map[string]string{"Authorization": "Bearer " + created.Secret})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("inference status %d: %s", status, truncate(body, 800))
	}
	calls, violations := upstream.snapshot()
	if len(violations) != 0 {
		t.Fatalf("upstream contract violations: %v", violations)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}

	var record recordView
	h.await(t, "one usage record for the kimi fixture", func() bool {
		page := h.page(t, "page_size=100")
		if len(page.Items) != 1 {
			return false
		}
		record = page.Items[0]
		return true
	})
	if record.AuthIndex != authIndex || record.Model != kimiModel {
		t.Fatalf("usage record identity = auth_index %q model %q, want %q/%q", record.AuthIndex, record.Model, authIndex, kimiModel)
	}

	date := time.Now().UTC().Format("2006-01-02")
	var view struct {
		Groups []struct {
			Key       string `json:"key"`
			Label     string `json:"label"`
			AuthIndex string `json:"auth_index"`
			Requests  int64  `json:"requests"`
		} `json:"groups"`
	}
	h.api(t, http.MethodGet, "/requests/view?from_date="+date+"&to_date="+date+"&view=account", nil, http.StatusOK, &view)
	if len(view.Groups) != 1 {
		t.Fatalf("account groups = %+v", view.Groups)
	}
	group := view.Groups[0]
	if group.Key != authIndex || group.AuthIndex != authIndex || group.Label != kimiAccountFile || group.Requests != 1 {
		t.Fatalf("account group = %+v, want key/auth_index %q label %q requests 1", group, authIndex, kimiAccountFile)
	}

	var facets struct {
		Items []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"items"`
	}
	h.api(t, http.MethodGet, "/requests/facets?field=auth_index&search=observer-friendly", nil, http.StatusOK, &facets)
	if len(facets.Items) != 1 || facets.Items[0].Value != authIndex || facets.Items[0].Label != kimiAccountFile {
		t.Fatalf("auth_index facet = %+v, want value %q label %q", facets.Items, authIndex, kimiAccountFile)
	}

	filtered := h.page(t, "auth_index="+authIndex+"&page_size=100")
	if len(filtered.Items) != 1 || filtered.Items[0].ID != record.ID {
		t.Fatalf("auth_index filter = %+v", filtered.Items)
	}
}
