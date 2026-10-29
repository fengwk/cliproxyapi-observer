package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled bool   `yaml:"enabled"`
	DB      string `yaml:"db"`
}

func parseConfig(raw []byte) (Config, error) {
	c := Config{true, "data/cliproxyapi-observer-v2.db"}
	// CPA injects priority and retains store provenance in lifecycle YAML.
	// They belong to the host, not to Observer settings or data compatibility.
	wire := struct {
		Enabled  bool      `yaml:"enabled"`
		DB       string    `yaml:"db"`
		Priority int       `yaml:"priority"`
		Store    yaml.Node `yaml:"store"`
	}{Enabled: c.Enabled, DB: c.DB}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if len(bytes.TrimSpace(raw)) > 0 {
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return c, engine.ErrInvalid
		}
		fields := doc.Content[0].Content
		for i := 0; i < len(fields); i += 2 {
			key, value := fields[i], fields[i+1]
			if key.Value == "store" {
				if value.Kind != yaml.MappingNode {
					return c, engine.ErrInvalid
				}
				continue
			}
			if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode ||
				(key.Value == "enabled" && value.Tag != "!!bool") ||
				(key.Value == "priority" && value.Tag != "!!int") ||
				(key.Value == "db" && value.Tag != "!!str") {
				return c, engine.ErrInvalid
			}
		}
		if err := d.Decode(&wire); err != nil {
			return c, err
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			return c, engine.ErrInvalid
		}
		c.Enabled, c.DB = wire.Enabled, wire.DB
	}
	if strings.TrimSpace(c.DB) == "" || strings.ContainsRune(c.DB, 0) {
		return c, engine.ErrInvalid
	}
	c.DB = filepath.Clean(c.DB)
	return c, nil
}

// mu fences every live store operation against close; lifeMu serializes config.
// Network workers are canceled and joined before releasing the database.
type Manager struct {
	mu                 sync.RWMutex
	lifeMu             sync.Mutex
	store              *engine.Store
	prices             *pricing.Service
	assets             AssetFunc
	now                func() time.Time
	cfg                Config
	configured, closed bool
	cancel             context.CancelFunc
	workers            sync.WaitGroup
	scopes             sync.Map // host caller_scope -> callerScope; never contains secrets
	admitted           sync.Map // our request ID -> our Key ID; completion ownership gate
	capsMu             sync.Mutex
	catalogCaps        map[[32]byte]time.Time
	// accountMetadata returns the raw host.auth.list envelope for one
	// management callback. It is set only by the native host bridge.
	accountMetadata func(callbackID string) []byte
}

// A host caller_scope is owned only after this plugin authenticated that exact
// request shape. The authenticated operation is authoritative for Gemini's
// FullPath wildcard, which cannot reveal generateContent vs countTokens.
type callerScope struct {
	KeyID     string
	Operation requestOperation
}

func NewManager(assets AssetFunc) *Manager { return &Manager{assets: assets, now: time.Now} }

// SetAccountMetadataLookup installs the native read-only metadata bridge. The
// lookup is consulted under the lifecycle read lock only for account views and
// auth_index facets; it must never be called on usage or authentication paths.
func (m *Manager) SetAccountMetadataLookup(lookup func(callbackID string) []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountMetadata = lookup
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func (m *Manager) HandleCall(method string, raw []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			if method == pluginabi.MethodRequestInterceptBefore {
				// m.before has a panic boundary that fails closed only after
				// ownership. This outer fallback must never reject unrelated
				// traffic because this plugin panicked.
				out, err = okEnvelope(pluginapi.RequestInterceptResponse{})
			} else {
				out, err = errorEnvelope(&PluginError{"plugin_error", "internal error", 503})
			}
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var r lifecycleRequest
		if json.Unmarshal(raw, &r) != nil {
			return fail("invalid_request", 400)
		}
		if r.SchemaVersion < 6 {
			return fail("unsupported_schema", 400)
		}
		c, e := parseConfig(r.ConfigYAML)
		if e != nil {
			return fail("invalid_config", 400)
		}
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return fail("unavailable", 503)
		}
		if m.configured && c.DB != m.cfg.DB {
			return fail("immutable_database", 409)
		}
		if m.configured && c == m.cfg && (m.store != nil || !c.Enabled) {
			return okEnvelope(registrationResponse())
		}
		if m.store != nil {
			h := m.store.Health()
			if h.ActiveRequests != 0 {
				return fail("busy", 409)
			}
		}
		old, had := m.cfg, m.configured
		if e = m.closeLocked(); e != nil {
			if had {
				_ = m.openLocked(old)
			}
			return fail("storage_unavailable", 503)
		}
		if e = m.openLocked(c); e != nil {
			if had {
				_ = m.openLocked(old)
			}
			// Registration no longer has to stay installed to protect an exclusive
			// provider. Failing here leaves native CPA authentication untouched.
			return fail("storage_unavailable", 503)
		}
		m.cfg = c
		m.configured = true
		return okEnvelope(registrationResponse())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.store != nil {
			h := m.store.Health()
			if h.ActiveRequests != 0 {
				return fail("busy", 409)
			}
		}
		if m.closeLocked() != nil {
			return fail("storage_unavailable", 503)
		}
		if method == pluginabi.MethodPluginShutdown {
			m.closed = true
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodFrontendAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": AuthIdentifier})
	case pluginabi.MethodFrontendAuthAuthenticate:
		return m.authenticate(raw)
	case pluginabi.MethodRequestInterceptBefore:
		return m.before(raw)
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestComplete:
		var r pluginapi.RequestCompletion
		if json.Unmarshal(raw, &r) != nil {
			return fail("invalid_request", 400)
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		if m.store == nil {
			m.admitted.Delete(r.RequestID)
			return okEnvelope(struct{}{})
		}
		// Lifecycle events also arrive for native host requests that this
		// interceptor deliberately passed through. Release only an admission
		// this plugin created.
		if _, ok := m.admitted.LoadAndDelete(r.RequestID); ok {
			_ = m.store.Complete(r.RequestID, r.CompletedAt)
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		return m.handleUsage(raw)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(raw)
	default:
		return fail("unknown_method", 400)
	}
}
func fail(code string, status int) ([]byte, error) {
	return errorEnvelope(&PluginError{code, strings.ReplaceAll(code, "_", " "), status})
}
func (m *Manager) openLocked(c Config) error {
	if !c.Enabled {
		return nil
	}
	p, e := pricing.NewService(pricing.DefaultConfig(), c.DB+".prices.json", nil)
	if e != nil {
		return e
	}
	s, e := engine.Open(engine.Options{DataPath: c.DB, Now: m.now, Pricer: p})
	if e != nil {
		return e
	}
	m.store = s
	m.prices = p
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.workers.Add(2)
	go func() { defer m.workers.Done(); p.Run(ctx) }()
	go func() {
		defer m.workers.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = s.Maintenance()
			}
		}
	}()
	return nil
}
func (m *Manager) closeLocked() error {
	m.capsMu.Lock()
	m.catalogCaps = nil
	m.capsMu.Unlock()
	m.admitted.Range(func(id, _ any) bool {
		m.admitted.Delete(id)
		return true
	})
	// Keep caller scopes across store replacement. Clearing them would turn an
	// already authenticated request into pass-through; while the store stays
	// closed, owned scopes fail closed, and after reopen Authorize revalidates
	// the Key. Scopes are hashed principals, never credentials.
	if m.cancel != nil {
		m.cancel()
		m.workers.Wait()
		m.cancel = nil
	}
	store := m.store
	m.store = nil
	m.prices = nil
	if store != nil {
		return store.Close()
	}
	return nil
}

// handleUsage ingests one usage record.
//
// Accounting failures are genuine errors: an invalid record, a pricing error
// or a failed write is reported to the host instead of being swallowed. No Key
// is ever latched, no marker is written and no later admission is restricted
// because of accounting. A failed or missing charge stays missing; a successful
// quote, including an unpriced zero-cost quote, is recorded as returned.
func (m *Manager) handleUsage(raw []byte) ([]byte, error) {
	var r pluginapi.UsageRecord
	if json.Unmarshal(raw, &r) != nil {
		return fail("invalid_usage", http.StatusBadRequest)
	}
	if !r.Generate || r.AccessProvider != AccessProvider || !strings.HasPrefix(r.APIKey, "observer:") {
		return okEnvelope(struct{}{})
	}
	if !validPrincipal(r.APIKey) {
		return fail("invalid_usage", http.StatusBadRequest)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil {
		return fail("accounting_unavailable", http.StatusServiceUnavailable)
	}
	if err := m.store.RecordUsage(r); err != nil {
		code, status := usageFailure(err)
		return fail(code, status)
	}
	return okEnvelope(struct{}{})
}

// usageFailure maps an engine accounting error to a sanitized code and status.
// The engine message, database paths and request payloads never reach the
// host log; only the operator-facing reason survives.
func usageFailure(err error) (string, int) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		return "unknown_key", http.StatusNotFound
	case errors.Is(err, engine.ErrInvalid):
		return "invalid_usage", http.StatusBadRequest
	default:
		return "accounting_unavailable", http.StatusServiceUnavailable
	}
}

func validPrincipal(s string) bool {
	if !strings.HasPrefix(s, "observer:") {
		return false
	}
	id := strings.TrimPrefix(s, "observer:")
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (m *Manager) authenticate(raw []byte) ([]byte, error) {
	var r pluginapi.FrontendAuthRequest
	if json.Unmarshal(raw, &r) != nil {
		return okEnvelope(pluginapi.FrontendAuthResponse{})
	}
	// Unsupported operations are not handled at all, so native CPA Keys keep
	// their existing authentication path. Never report global success.
	operation := operationFor(r.Method, r.Path)
	if operation == operationUnknown {
		return okEnvelope(pluginapi.FrontendAuthResponse{})
	}
	secret, e := credential(r)
	if e != nil {
		return okEnvelope(pluginapi.FrontendAuthResponse{})
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil {
		return okEnvelope(pluginapi.FrontendAuthResponse{})
	}
	if m.consumeCatalog(r, secret) {
		return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: true, Principal: "observer-catalog"})
	}
	k, e := m.store.Authenticate(secret)
	if e != nil {
		return okEnvelope(pluginapi.FrontendAuthResponse{})
	}
	principal := "observer:" + k.ID
	switch operation {
	case operationCount:
		principal = "observer-count:" + k.ID
	case operationDirectory:
		principal = "observer-directory:" + k.ID
	}
	// handlers.requestCallerScope hashes Gin userApiKey via the pinned SDK helper.
	// userApiKey itself is NOT present in RequestInterceptRequest.Metadata. The
	// principal already separates operations; LoadOrStore also prevents a
	// concurrent request from replacing the first recorded operation.
	m.scopes.LoadOrStore(session.CallerScope(principal), callerScope{KeyID: k.ID, Operation: operation})
	return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: true, Principal: principal})
}
func denied(status int) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: status, ResponseBody: []byte(`{"error":{"code":"admission_denied","message":"request not admitted"}}`)}
}
func (m *Manager) before(raw []byte) (out []byte, err error) {
	// Fail closed only after ownership is proven. A panic while classifying
	// unknown/native traffic must not reject that traffic globally.
	owned := false
	defer func() {
		if recover() != nil {
			if owned {
				out, err = okEnvelope(denied(503))
			} else {
				out, err = okEnvelope(pluginapi.RequestInterceptResponse{})
			}
		}
	}()
	var r pluginapi.RequestInterceptRequest
	if json.Unmarshal(raw, &r) != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	scope, _ := r.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	if scope == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	v, ok := m.scopes.Load(scope)
	if !ok {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	caller, ok := v.(callerScope)
	if !ok || caller.KeyID == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// The host supplies an exact path for most routes, but Gin's FullPath for
	// Gemini is "/v1beta/models/*action". That template cannot distinguish
	// generateContent from countTokens, so the authenticated operation governs.
	path, _ := r.Metadata[coreexecutor.RequestPathMetadataKey].(string)
	operation, template := beforeOperation(path)
	if template {
		switch caller.Operation {
		case operationGeneration, operationCount:
			operation = caller.Operation
		default:
			// A directory (or otherwise non-execution) scope never enters a
			// model execution, even if a path was rewritten upstream.
			return okEnvelope(denied(403))
		}
	} else if operation == operationGeneration || operation == operationCount {
		// Exact routes must agree with the scope's authenticated operation:
		// generation cannot masquerade as count, and count cannot bypass
		// generation quota/concurrency.
		if caller.Operation != operation {
			return okEnvelope(denied(403))
		}
	} else {
		// Unknown, absent, directory, or unsupported paths are not execution
		// operations Observer owns. Native/unowned scopes were already returned.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	owned = true
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil {
		return okEnvelope(denied(503))
	}
	model := r.RequestedModel
	if model == "" {
		model = r.Model
	}
	id := caller.KeyID
	e := m.store.Authorize(id, model)
	if operation == operationCount {
		if e != nil {
			return okEnvelope(denied(denialStatus(e)))
		}
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// The exact supported operation, not a generic Generate flag, owns
	// admission. Count helpers were returned above. A misleading false flag on
	// a real generation route must never bypass quotas or concurrency.
	if e == nil {
		if r.RequestID == "" {
			return okEnvelope(denied(400))
		}
		e = m.store.Admit(engine.Admission{RequestID: r.RequestID, TraceID: r.TraceID, KeyID: id, Model: model, At: m.now()})
		if e == nil {
			m.admitted.Store(r.RequestID, id)
		}
	}
	if e != nil {
		return okEnvelope(denied(denialStatus(e)))
	}
	m.store.Capture(engine.CaptureInput{RequestID: r.RequestID, TraceID: r.TraceID, KeyID: id, Headers: r.Headers, Body: r.Body, At: m.now()})
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func denialStatus(e error) int {
	status := 403
	if errors.Is(e, engine.ErrQuota) || errors.Is(e, engine.ErrConcurrency) {
		status = 429
	}
	if errors.Is(e, engine.ErrAccounting) || errors.Is(e, engine.ErrClosed) || errors.Is(e, engine.ErrMaintenance) {
		status = 503
	}
	return status
}
