package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	billingFaults      sync.Map // marker-write failure fallback; explicit recovery or durable handoff only
	capsMu             sync.Mutex
	catalogCaps        map[[32]byte]time.Time
}

type callerScope struct {
	KeyID    string
	Generate bool
}

func NewManager(assets AssetFunc) *Manager { return &Manager{assets: assets, now: time.Now} }

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func (m *Manager) HandleCall(method string, raw []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			if method == pluginabi.MethodRequestInterceptBefore {
				out, err = okEnvelope(denied(503))
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
			if h.ActiveRequests != 0 || h.PendingUsage != 0 {
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
			} else if method == pluginabi.MethodPluginRegister {
				// Keep the exclusive provider installed on storage faults. Failing
				// registration here would allow the host's built-in keys to win.
				m.cfg, m.configured = c, true
				return okEnvelope(registrationResponse())
			}
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
		callbackFault := false
		m.billingFaults.Range(func(_, _ any) bool { callbackFault = true; return false })
		if callbackFault {
			return fail("accounting_unsettled", 409)
		}
		if m.store != nil {
			h := m.store.Health()
			if h.ActiveRequests != 0 || h.PendingUsage != 0 {
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
		if m.store != nil {
			_ = m.store.Complete(r.RequestID, r.CompletedAt)
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		m.handleUsage(raw)
		return okEnvelope(struct{}{})
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

// Accounting errors never fail an already running response. Engine errors own
// durable pending events. Unexpected callback panics and late deliveries persist
// an interruption outside bbolt so even a different native instance fails closed.
// This cannot reconstruct source events that the host never delivers.
func (m *Manager) handleUsage(raw []byte) {
	var r pluginapi.UsageRecord
	if json.Unmarshal(raw, &r) != nil || !r.Generate || r.AccessProvider != AccessProvider || !validPrincipal(r.APIKey) {
		return
	}
	id := strings.TrimPrefix(r.APIKey, "observer:")
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.accountUsage(id, func() error {
		if m.store == nil {
			return engine.ErrClosed
		}
		return m.store.RecordUsage(r)
	})
}

// Caller holds the lifecycle read fence. Keep this panic boundary around the
// engine call only, not around JSON parsing or credential extraction.
func (m *Manager) accountUsage(id string, write func() error) {
	defer func() {
		if recover() != nil {
			m.recordInterruption(id)
		}
	}()
	if err := write(); errors.Is(err, engine.ErrClosed) {
		m.recordInterruption(id)
	}
}

func (m *Manager) recordInterruption(id string) {
	if err := engine.RecordInterruption(m.cfg.DB, id); err != nil {
		// Only marker write failures need a second, in-memory safety gate.
		// Never expose filesystem errors, callback panic text, or credentials.
		m.billingFaults.Store(id, true)
	} else {
		m.billingFaults.Delete(id)
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
	generate := !countTokenPath(r.Path)
	principal := "observer:" + k.ID
	if !generate {
		principal = "observer-count:" + k.ID
	}
	// handlers.requestCallerScope hashes Gin userApiKey via the pinned SDK helper.
	// userApiKey itself is NOT present in RequestInterceptRequest.Metadata.
	m.scopes.Store(session.CallerScope(principal), callerScope{KeyID: k.ID, Generate: generate})
	return okEnvelope(pluginapi.FrontendAuthResponse{Authenticated: true, Principal: principal})
}
func denied(status int) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: status, ResponseBody: []byte(`{"error":{"code":"admission_denied","message":"request not admitted"}}`)}
}
func (m *Manager) before(raw []byte) ([]byte, error) {
	var r pluginapi.RequestInterceptRequest
	if json.Unmarshal(raw, &r) != nil {
		return okEnvelope(denied(400))
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil {
		return okEnvelope(denied(503))
	}
	scope, _ := r.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	v, ok := m.scopes.Load(scope)
	if !ok {
		return okEnvelope(denied(401))
	}
	caller, ok := v.(callerScope)
	if !ok || caller.KeyID == "" {
		return okEnvelope(denied(401))
	}
	model := r.RequestedModel
	if model == "" {
		model = r.Model
	}
	id := caller.KeyID
	e := m.store.Authorize(id, model)
	generate := caller.Generate
	if b, ok := r.Metadata[coreexecutor.GenerateMetadataKey].(bool); ok {
		generate = generate && b
	}
	if e == nil && generate {
		if _, fault := m.billingFaults.Load(id); fault {
			return okEnvelope(denied(503))
		}
		if r.RequestID == "" {
			return okEnvelope(denied(400))
		}
		e = m.store.Admit(engine.Admission{RequestID: r.RequestID, TraceID: r.TraceID, KeyID: id, Model: model, At: m.now()})
	}
	if e != nil {
		status := 403
		if errors.Is(e, engine.ErrQuota) || errors.Is(e, engine.ErrConcurrency) {
			status = 429
		}
		if errors.Is(e, engine.ErrAccounting) || errors.Is(e, engine.ErrClosed) {
			status = 503
		}
		return okEnvelope(denied(status))
	}
	if generate {
		m.store.Capture(engine.CaptureInput{RequestID: r.RequestID, TraceID: r.TraceID, KeyID: id, Headers: r.Headers, Body: r.Body, At: m.now()})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}
