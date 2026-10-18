//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type browserRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

type browserReport struct {
	Mode              string `json:"mode"`
	UIURL             string `json:"ui_url"`
	Outcome           string `json:"outcome"`
	Readonly          bool   `json:"readonly"`
	FetchInterception bool   `json:"fetch_interception"`
	Fatal             string `json:"fatal"`
	Summary           struct {
		Total  int `json:"total"`
		Passed int `json:"passed"`
		Failed int `json:"failed"`
	} `json:"summary"`
	Results []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	} `json:"results"`
	APIRequests     []browserRequest `json:"api_requests"`
	BlockedRequests []browserRequest `json:"blocked_requests"`
	Screenshots     []struct {
		File  string `json:"file"`
		Bytes int    `json:"bytes"`
	} `json:"screenshots"`
}

// TestBrowserAgainstRealHost runs the committed Node/Chrome candidate against
// a real host. Catalog tokens and price previews are the only allowed POSTs;
// neither changes usage, configuration, keys or historical accounting.
func TestBrowserAgainstRealHost(t *testing.T) {
	if os.Getenv("OBSERVER_BROWSER") != "1" {
		t.Skip("set OBSERVER_BROWSER=1 to run the real-browser smoke")
	}
	node := browserRequireNode(t)
	chrome := browserRequireChrome(t)
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	script := filepath.Join(root, "internal", "web", "browser", "run.cjs")
	if info, err := os.Stat(script); err != nil || info.IsDir() {
		t.Fatalf("candidate browser script %s unavailable: %v", script, err)
	}
	artifacts := browserArtifacts(t, root)
	if os.Getenv("CPA_TEST_ARTIFACTS") == "" {
		// Let the harness preserve its stdout/stderr next to the Node log too.
		t.Setenv("CPA_TEST_ARTIFACTS", artifacts)
	}

	h := newHarness(t)
	created := h.createKey(t, validSpec())

	settings := acceptanceGetSettings(t, h)
	settings["capture_enabled"] = true
	var updated acceptanceSettings
	h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, &updated)
	if !updated.CaptureEnabled {
		t.Fatal("capture_enabled was not persisted before browser smoke")
	}

	known := make(map[string]struct{})
	record := acceptanceInferAndAwait(t, h, created.Secret, known)
	h.await(t, "capture detail available before browser smoke", func() bool {
		var detail struct {
			CaptureStatus string `json:"capture_status"`
		}
		h.api(t, http.MethodGet, "/requests/detail?id="+url.QueryEscape(record.ID), nil, http.StatusOK, &detail)
		return detail.CaptureStatus == "available" || detail.CaptureStatus == "redacted"
	})
	h.await(t, "accounting settled before browser smoke", func() bool {
		var health acceptanceHealth
		h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
		return health.ActiveRequests == 0 && health.PendingUsage == 0
	})

	// These are the only host writes in this test. Everything the browser does
	// below must leave the same authenticated management state behind.
	before := browserSnapshot(t, h)
	reportDir := filepath.Join(artifacts, "browser")
	if err := os.MkdirAll(reportDir, 0o700); err != nil {
		t.Fatalf("create report directory: %v", err)
	}
	uiURL := h.baseURL + "/v0/resource/plugins/" + pluginID + "/ui"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script, "--base-url", uiURL, "--report-dir", reportDir)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"OBSERVER_MANAGEMENT_KEY="+mgmtKey,
		"CHROME_BIN="+chrome,
	)
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	stdoutPath := filepath.Join(reportDir, "node.stdout.log")
	stderrPath := filepath.Join(reportDir, "node.stderr.log")
	for path, data := range map[string][]byte{stdoutPath: stdout.Bytes(), stderrPath: stderr.Bytes()} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Errorf("write browser log %s: %v", path, err)
		}
	}
	t.Logf("browser report: %s", reportDir)
	if timeout := ctx.Err(); timeout != nil {
		runErr = fmt.Errorf("%w: %v", timeout, runErr)
	}

	browserAssertManagementUnchanged(t, h, before)
	if runErr != nil {
		t.Fatalf("browser smoke failed: %v\nstdout:\n%s\nstderr:\n%s", runErr, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}

	reportRaw, err := os.ReadFile(filepath.Join(reportDir, "report.json"))
	if err != nil {
		t.Fatalf("read browser report: %v", err)
	}
	var report browserReport
	if err := json.Unmarshal(reportRaw, &report); err != nil {
		t.Fatalf("decode browser report: %v", err)
	}
	browserValidateReport(t, report, uiURL)

	smoke := filepath.Join(reportDir, "screenshots", "realhost-smoke.png")
	if info, err := os.Stat(smoke); err != nil || info.Size() <= 1000 {
		t.Errorf("browser smoke screenshot missing or empty: %s", smoke)
	}
	// The report must never retain the management credential used by the run.
	for name, data := range map[string][]byte{
		"report.json": reportRaw,
		"node.stdout": stdout.Bytes(),
		"node.stderr": stderr.Bytes(),
	} {
		if bytes.Contains(data, []byte(mgmtKey)) {
			t.Errorf("%s leaked OBSERVER_MANAGEMENT_KEY", name)
		}
	}
}

func browserRequireNode(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("OBSERVER_BROWSER=1 requires Node: %v", err)
	}
	return path
}

func browserRequireChrome(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("CHROME_BIN"))
	if path == "" {
		found, err := exec.LookPath("google-chrome-stable")
		if err != nil {
			t.Fatalf("OBSERVER_BROWSER=1 requires CHROME_BIN or google-chrome-stable: %v", err)
		}
		path = found
	} else if !filepath.IsAbs(path) {
		if found, err := exec.LookPath(path); err == nil {
			path = found
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve Chrome: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("Chrome executable %s unavailable: %v", abs, err)
	}
	return abs
}

// browserArtifacts preserves logs outside the worktree. An explicitly supplied
// root must already be absolute and outside the repository.
func browserArtifacts(t *testing.T, root string) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CPA_TEST_ARTIFACTS"))
	if raw == "" {
		dir, err := os.MkdirTemp("", "cpa-observer-browser-")
		if err != nil {
			t.Fatalf("create external artifact directory: %v", err)
		}
		t.Logf("CPA_TEST_ARTIFACTS unset; preserving browser artifacts in %s", dir)
		return dir
	}
	if !filepath.IsAbs(raw) {
		t.Fatalf("CPA_TEST_ARTIFACTS must be absolute, got %q", raw)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		t.Fatalf("resolve CPA_TEST_ARTIFACTS: %v", err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		t.Fatalf("CPA_TEST_ARTIFACTS %q must be outside repository %q", abs, root)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatalf("create CPA_TEST_ARTIFACTS: %v", err)
	}
	return abs
}

func browserSnapshot(t *testing.T, h *harness) map[string]any {
	t.Helper()
	out := make(map[string]any, 3)
	for _, name := range []string{"keys", "settings", "summary"} {
		var value any
		h.api(t, http.MethodGet, "/"+name, nil, http.StatusOK, &value)
		out[name] = value
	}
	return out
}

func browserAssertManagementUnchanged(t *testing.T, h *harness, before map[string]any) {
	t.Helper()
	for _, name := range []string{"keys", "settings", "summary"} {
		var after any
		h.api(t, http.MethodGet, "/"+name, nil, http.StatusOK, &after)
		if !reflect.DeepEqual(before[name], after) {
			oldJSON, _ := json.Marshal(before[name])
			newJSON, _ := json.Marshal(after)
			t.Errorf("%s changed during browser smoke: before=%s after=%s", name, truncate(oldJSON, 1000), truncate(newJSON, 1000))
		}
	}
}

func browserValidateReport(t *testing.T, report browserReport, uiURL string) {
	t.Helper()
	if report.Mode != "realhost-readonly" || report.Outcome != "completed" || !report.Readonly || !report.FetchInterception {
		t.Errorf("unexpected report mode/outcome: mode=%q outcome=%q readonly=%t fetch_interception=%t", report.Mode, report.Outcome, report.Readonly, report.FetchInterception)
	}
	if report.UIURL != uiURL {
		t.Errorf("report ui_url = %q, want %q", report.UIURL, uiURL)
	}
	if report.Fatal != "" {
		t.Errorf("browser report fatal: %s", report.Fatal)
	}
	if report.Summary.Total == 0 || report.Summary.Failed != 0 || report.Summary.Passed != report.Summary.Total {
		t.Errorf("browser checks failed: passed=%d/%d failed=%d", report.Summary.Passed, report.Summary.Total, report.Summary.Failed)
	}
	for _, result := range report.Results {
		if !result.OK {
			t.Errorf("browser check %q failed: %s", result.Name, result.Detail)
		}
	}
	for _, request := range append(report.APIRequests, report.BlockedRequests...) {
		if browserRequestAllowed(request) {
			continue
		}
		t.Errorf("browser sent or attempted a non-catalog write: %s %s", request.Method, request.URL)
	}
	screenshot := false
	for _, shot := range report.Screenshots {
		if shot.File == filepath.Join("screenshots", "realhost-smoke.png") && shot.Bytes > 1000 {
			screenshot = true
		}
	}
	if !screenshot {
		t.Error("report.json does not record a nonempty realhost-smoke.png screenshot")
	}
}

func browserRequestAllowed(request browserRequest) bool {
	switch strings.ToUpper(request.Method) {
	case "GET", "HEAD", "OPTIONS":
		return true
	case "POST":
		parsed, err := url.Parse(request.URL)
		return err == nil && parsed.RawQuery == "" &&
			(parsed.Path == "/v0/management/plugins/"+pluginID+"/models/token" ||
				parsed.Path == "/v0/management/plugins/"+pluginID+"/prices/preview")
	default:
		return false
	}
}
