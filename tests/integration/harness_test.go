//go:build integration

// Package integration drives a real CLIProxyAPI v8 host binary loading the
// Observer native plugin against a local httptest upstream. The suite is
// black-box: it never imports plugin or SDK internals, only the standard
// library and the CPA HTTP surface.
package integration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixed test identities. Every credential here is fake and safe to log.
const (
	pluginID   = "cliproxyapi-observer"
	pluginName = "Observer"

	clientKey   = "fake-client-key"
	mgmtKey     = "fake-management-password"
	upstreamKey = "fake-upstream-key"

	// modelName is the client-facing alias; upstreamModel is what the upstream
	// sees. Keeping them different proves full model identity is preserved.
	modelName     = "observer-mock-model"
	upstreamModel = "observer-upstream-model"

	// compatProviderName is the OpenAI-compatibility provider identifier. It
	// contains "openai" so the host applies OpenAI subset token semantics.
	compatProviderName = "observer-openai"

	pluginLibName = "cliproxyapi-observer.so"
)

// pluginBuildRoot is populated by TestMain with the directory that holds the
// once-built native plugin library.
var pluginBuildRoot string

// requireCPABinary enforces the runtime contract: with the integration tag set,
// a missing or non-executable CPA_BINARY is a hard failure, never a skip.
func requireCPABinary() (string, error) {
	raw := strings.TrimSpace(os.Getenv("CPA_BINARY"))
	if raw == "" {
		return "", errors.New("CPA_BINARY is required for integration tests (absolute path to a CLIProxyAPI v8 host binary); refusing to skip")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("CPA_BINARY must be an absolute path, got %q", raw)
	}
	info, err := os.Stat(raw)
	if err != nil {
		return "", fmt.Errorf("CPA_BINARY %q is not accessible: %w", raw, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("CPA_BINARY %q is a directory, not an executable file", raw)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("CPA_BINARY %q is not executable", raw)
	}
	return raw, nil
}

// repoRoot locates the plugin module root. PLUGIN_SOURCE overrides the derived
// path for early local validation; it must not be required in CI.
func repoRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv("PLUGIN_SOURCE")); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve PLUGIN_SOURCE: %w", err)
		}
		if _, errStat := os.Stat(filepath.Join(abs, "go.mod")); errStat != nil {
			return "", fmt.Errorf("PLUGIN_SOURCE %q has no go.mod: %w", abs, errStat)
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, errStat := os.Stat(filepath.Join(dir, "go.mod")); errStat == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find repository root (go.mod) above the test working directory")
		}
		dir = parent
	}
}

// TestMain validates the runtime contract and builds the native plugin once so
// every test case only pays the (small) copy cost.
func TestMain(m *testing.M) {
	if _, err := requireCPABinary(); err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	build, err := os.MkdirTemp("", "cpa-observer-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.Exit(2)
	}
	if err := buildPlugin(root, build); err != nil {
		fmt.Fprintln(os.Stderr, "integration: "+err.Error())
		os.RemoveAll(build)
		os.Exit(2)
	}
	pluginBuildRoot = build
	code := m.Run()
	os.RemoveAll(build)
	os.Exit(code)
}

// buildPlugin compiles the c-shared native plugin into the plugin store layout
// the host expects: <dir>/<goos>/<goarch>/cliproxyapi-observer.so.
func buildPlugin(root, outRoot string) error {
	dest := filepath.Join(outRoot, runtime.GOOS, runtime.GOARCH, pluginLibName)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("prepare plugin output: %w", err)
	}
	cmd := exec.Command("go", "build", "-mod=readonly", "-buildmode=c-shared", "-trimpath", "-o", dest, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build native plugin in %s: %w\n%s", root, err, out)
	}
	if info, errStat := os.Stat(dest); errStat != nil || info.Size() == 0 {
		return fmt.Errorf("native plugin build produced no library at %s", dest)
	}
	return nil
}

// lockedBuffer is a thread-safe sink for captured host output.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a running CPA host with its mock upstream.
type harness struct {
	t       *testing.T
	cpaBin  string
	dir     string
	authDir string
	dbPath  string
	port    int
	baseURL string
	opts    hostOptions
	mock    *mockUpstream
	cmd     *exec.Cmd
	stdout  *lockedBuffer
	stderr  *lockedBuffer

	mu      sync.Mutex
	stopped bool
	waitErr error
	done    chan struct{}
}

// hostOptions selects the observer plugin settings a test host runs with.
type hostOptions struct {
	// CaptureBodies toggles request-body capture for the observer plugin.
	CaptureBodies bool
	// StatsRetentionDays overrides the statistics retention; zero keeps the
	// host/plugin default.
	StatsRetentionDays int
	// DatabasePath overrides the observer database file; empty derives one
	// inside the harness directory.
	DatabasePath string
}

func defaultHostOptions() hostOptions { return hostOptions{CaptureBodies: true} }

// newHarness starts one mock upstream and one CPA host against it and registers
// teardown. The host is stopped before the mock is closed.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, defaultHostOptions())
}

// newHarnessWith is newHarness with explicit observer plugin options.
func newHarnessWith(t *testing.T, opts hostOptions) *harness {
	t.Helper()
	mock := newMockUpstream(t)
	h, err := startHostWith(t, mock, t.TempDir(), opts)
	if err != nil {
		mock.Close()
		t.Fatalf("start host: %v", err)
	}
	t.Cleanup(mock.Close)
	t.Cleanup(h.Stop)
	return h
}

// startHost launches a host with the default observer plugin options.
func startHost(t *testing.T, mock *mockUpstream, dir string) (*harness, error) {
	t.Helper()
	return startHostWith(t, mock, dir, defaultHostOptions())
}

// startHostWith writes an isolated config, copies the prebuilt plugin into a
// host plugin store and launches the real binary with the minimal environment.
func startHostWith(t *testing.T, mock *mockUpstream, dir string, opts hostOptions) (*harness, error) {
	t.Helper()
	cpaBin, err := requireCPABinary()
	if err != nil {
		return nil, err
	}
	if pluginBuildRoot == "" {
		return nil, errors.New("plugin was not built (missing TestMain)")
	}

	pluginsDir := filepath.Join(dir, "plugins")
	if err := installPlugin(pluginsDir); err != nil {
		return nil, err
	}
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := opts.DatabasePath
	if dbPath == "" {
		dbPath = filepath.Join(dir, "observer.db")
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, hostConfig(port, authDir, pluginsDir, dbPath, mock.baseURL(), opts), 0o600); err != nil {
		return nil, err
	}

	h := &harness{
		t:       t,
		cpaBin:  cpaBin,
		dir:     dir,
		authDir: authDir,
		dbPath:  dbPath,
		port:    port,
		baseURL: baseURL,
		opts:    opts,
		mock:    mock,
		stdout:  &lockedBuffer{},
		stderr:  &lockedBuffer{},
		done:    make(chan struct{}),
	}

	cmd := exec.Command(cpaBin, "--config", configPath)
	cmd.Dir = dir
	cmd.Env = hostEnv(dir)
	cmd.Stdout = h.stdout
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", cpaBin, err)
	}
	h.cmd = cmd
	go func() {
		waitErr := cmd.Wait()
		h.mu.Lock()
		h.waitErr = waitErr
		h.mu.Unlock()
		close(h.done)
	}()

	if err := h.awaitReady(15 * time.Second); err != nil {
		h.Stop()
		return nil, fmt.Errorf("%w\n--- host stdout ---\n%s\n--- host stderr ---\n%s", err, h.stdout.String(), h.stderr.String())
	}
	return h, nil
}

// installPlugin copies the shared build artifact into a per-test plugin store.
func installPlugin(pluginsDir string) error {
	src := filepath.Join(pluginBuildRoot, runtime.GOOS, runtime.GOARCH, pluginLibName)
	dst := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH, pluginLibName)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	in, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read built plugin: %w", err)
	}
	if err := os.WriteFile(dst, in, 0o755); err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	return nil
}

// hostConfig renders an isolated v8 config that routes one model to the mock
// upstream through OpenAI compatibility and enables the observer plugin.
//
// The compatibility provider name contains "openai" so the host classifies it
// with OpenAI subset token semantics (cached tokens are a subset of the prompt),
// matching the accounting the mock fixture exercises.
func hostConfig(port int, authDir, pluginsDir, dbPath, mockURL string, opts hostOptions) []byte {
	var retention string
	if opts.StatsRetentionDays > 0 {
		retention = fmt.Sprintf("\n      stats-retention-days: %d", opts.StatsRetentionDays)
	}
	return []byte(fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
access:
  api-keys:
    - %q
oauth:
  auth-dir: %q
openai-compatibility:
  - name: %q
    base-url: %q
    api-key-entries:
      - api-key: %q
    models:
      - name: %q
        alias: %q
plugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      db: %q
      flush: "1s"
      capture-bodies: %t%s
`, port, mgmtKey, clientKey, authDir, compatProviderName, mockURL+"/v1", upstreamKey, upstreamModel, modelName, pluginsDir, pluginID, dbPath, opts.CaptureBodies, retention))
}

// rewriteConfig rewrites the host config with new observer plugin options and
// the same port/auth/plugin paths, letting the host's config watcher apply a
// plugin reconfigure. An empty DatabasePath keeps the current database.
func (h *harness) rewriteConfig(t *testing.T, opts hostOptions) error {
	t.Helper()
	dbPath := opts.DatabasePath
	if dbPath == "" {
		dbPath = h.dbPath
	}
	content := hostConfig(h.port, h.authDir, filepath.Join(h.dir, "plugins"), dbPath, h.mock.baseURL(), opts)
	return os.WriteFile(filepath.Join(h.dir, "config.yaml"), content, 0o600)
}

// hostEnv builds a minimal environment that cannot inherit production CPA
// secrets (no MANAGEMENT_PASSWORD, CPA_HOME, proxy variables or .env).
func hostEnv(home string) []string {
	env := []string{"HOME=" + home}
	for _, key := range []string{"PATH", "GOROOT", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "NO_PROXY=*", "no_proxy=*")
	return env
}

// freePort reserves a loopback TCP port and releases it for the host to bind.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// awaitReady checks the server, client catalog and authenticated observer.
// Startup synchronization may temporarily leave the observer unavailable.
func (h *harness) awaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			return fmt.Errorf("host exited early: %v", h.waitError())
		default:
		}
		status, _, err := h.rawRequest(http.MethodGet, "/healthz", nil, nil)
		if err == nil && status == http.StatusOK {
			break
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("healthz status %d", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("host did not become ready: %v", lastErr)
	}
	status, body, err := h.rawRequest(http.MethodGet, "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		return fmt.Errorf("readiness models request: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("readiness /v1/models status %d body %s", status, truncate(body, 400))
	}
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			return fmt.Errorf("host exited during observer readiness: %v", h.waitError())
		default:
		}
		status, body, err = h.rawRequest(http.MethodGet, "/v0/management/plugins/"+pluginID+"/health", nil, map[string]string{"Authorization": "Bearer " + mgmtKey})
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			lastErr = err
		} else if status == http.StatusServiceUnavailable || status == http.StatusNotFound {
			lastErr = fmt.Errorf("observer health status %d", status)
		} else {
			return fmt.Errorf("observer health status %d body %s", status, truncate(body, 400))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("observer did not become ready: %v", lastErr)
}

func (h *harness) waitError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waitErr
}

// waitForLog blocks until the host has emitted a log line containing substr.
// It is used to avoid racing the config watcher installation.
func (h *harness) waitForLog(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.stdout.String()+h.stderr.String(), substr) {
			return
		}
		select {
		case <-h.done:
			t.Fatalf("host exited before log %q: %v", substr, h.waitError())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("log %q not observed within %s", substr, timeout)
}

// Stop signals the host and waits a bounded time before killing only its own
// process.
func (h *harness) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	h.mu.Unlock()

	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(os.Interrupt)
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			_ = h.cmd.Process.Kill()
			select {
			case <-h.done:
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// rawRequest performs one host HTTP call. Headers and body may be nil.
func (h *harness) rawRequest(method, path string, body []byte, headers map[string]string) (int, []byte, error) {
	status, _, data, err := h.rawRequestWithHeaders(method, path, body, headers)
	return status, data, err
}

// rawRequestWithHeaders performs one host HTTP call and returns response headers.
func (h *harness) rawRequestWithHeaders(method, path string, body []byte, headers map[string]string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

func (h *harness) httpClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// clientRequest issues a client API request authenticated with the fake client
// key.
func (h *harness) clientRequest(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	status, data, err := h.rawRequest(method, path, body, map[string]string{
		"Authorization": "Bearer " + clientKey,
		"Content-Type":  "application/json",
	})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return status, data
}

// managementRequest issues a management API request authenticated with the fake
// management key.
func (h *harness) managementRequest(t *testing.T, method, path string) (int, http.Header, []byte) {
	t.Helper()
	status, header, data, err := h.rawRequestWithHeaders(method, path, nil, map[string]string{"Authorization": "Bearer " + mgmtKey})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return status, header, data
}

func truncate(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "...(truncated)"
}
