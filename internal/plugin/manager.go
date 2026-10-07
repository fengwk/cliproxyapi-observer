package plugin

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

// Manager owns the observation store lifecycle and dispatches every RPC method.
// It is safe for concurrent use. Observation paths never fail the host: when no
// store is live every hook returns an empty acknowledgement.
type Manager struct {
	opener Opener
	assets AssetFunc
	now    func() time.Time

	// mu guards the live store and its configuration snapshot. Read paths hold
	// it for the duration of a store call so a concurrent close can never leave
	// them using a released database.
	mu        sync.RWMutex
	store     Store
	cfg       observer.Config
	raw       []byte
	hasConfig bool
	closed    bool

	// lifeMu serializes lifecycle transitions (register, reconfigure, quiesce,
	// shutdown) so a quiesce/reopen cycle can not interleave.
	lifeMu sync.Mutex
}

// NewManager returns a dispatcher backed by opener and, for management
// resources, assets. assets may be nil when no UI slice is wired in.
func NewManager(opener Opener, assets AssetFunc) *Manager {
	return &Manager{opener: opener, assets: assets, now: time.Now}
}

// lifecycleRequest mirrors the host plugin.register/plugin.reconfigure payload.
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error envelope so the host
// process is never taken down by this plugin.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = mustEnvelope(errorResult("plugin_error", "internal error handling "+method, http.StatusInternalServerError))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce:
		m.quiesce()
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		m.shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		m.handleUsage(request)
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		m.handleInterceptBefore(request)
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		// The observer never modifies an after-auth request.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	default:
		return mustEnvelope(errorResult("unknown_method", "unknown method: "+method, 0))
	}
}

func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope(&PluginError{Code: "invalid_request", Message: "malformed lifecycle request", HTTPStatus: http.StatusBadRequest})
		}
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	return m.applyLocked(req.ConfigYAML, req.SchemaVersion)
}

// applyLocked validates a candidate configuration, quiesces the live store and
// opens the candidate. A failed open rolls back to the previous configuration
// so an in-place reconfigure never leaves the plugin without a working store.
func (m *Manager) applyLocked(raw []byte, schema uint32) ([]byte, error) {
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return errorEnvelope(&PluginError{Code: "unavailable", Message: "observer is shut down", HTTPStatus: http.StatusServiceUnavailable})
	}

	// Validate the candidate before the live store is released.
	cfg, err := m.opener.Parse(raw)
	if err != nil {
		return errorEnvelope(&PluginError{Code: "invalid_config", Message: "invalid observer configuration", HTTPStatus: http.StatusBadRequest})
	}

	m.mu.Lock()
	previousRaw := m.raw
	if m.store != nil {
		_ = m.store.Close()
		m.store = nil
	}
	m.mu.Unlock()

	store, err := m.opener.Open(cfg)
	if err != nil {
		m.restore(previousRaw)
		return errorEnvelope(&PluginError{Code: "storage_unavailable", Message: "observer storage unavailable", HTTPStatus: http.StatusServiceUnavailable})
	}

	m.mu.Lock()
	m.store = store
	m.cfg = cfg
	m.raw = append([]byte(nil), raw...)
	m.hasConfig = true
	m.mu.Unlock()
	return okEnvelope(registrationResponse(schema))
}

// restore reopens the configuration that was live before a failed reconfigure.
// It is best effort: if the previous store can not be reopened the plugin stays
// degraded and keeps serving inference without observation.
func (m *Manager) restore(raw []byte) {
	if len(raw) == 0 {
		return
	}
	cfg, err := m.opener.Parse(raw)
	if err != nil {
		return
	}
	store, err := m.opener.Open(cfg)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.store = store
	m.cfg = cfg
	m.raw = append([]byte(nil), raw...)
	m.hasConfig = true
	m.mu.Unlock()
}

// quiesce releases the database and drains the writer. It is reversible: a
// later reconfigure reopens the store from the same configuration.
func (m *Manager) quiesce() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if m.store != nil {
		_ = m.store.Close()
		m.store = nil
	}
	m.mu.Unlock()
}

// shutdown releases the store and makes the manager terminal. It is idempotent.
func (m *Manager) shutdown() {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	if m.store != nil {
		_ = m.store.Close()
		m.store = nil
	}
	m.closed = true
	m.mu.Unlock()
}

// handleUsage enqueues one usage record. A malformed payload is acknowledged
// without observation rather than failing the host call.
func (m *Manager) handleUsage(request []byte) {
	var record pluginapi.UsageRecord
	if len(request) > 0 {
		if err := json.Unmarshal(request, &record); err != nil {
			return
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store != nil {
		m.store.SubmitUsage(record)
	}
}

// handleInterceptBefore enqueues the observed request body association. The
// store decides whether capture is enabled; the plugin never modifies the
// request and never fails the host call.
func (m *Manager) handleInterceptBefore(request []byte) {
	var req pluginapi.RequestInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store != nil {
		m.store.Capture(req)
	}
}
