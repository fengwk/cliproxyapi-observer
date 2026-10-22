package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
)

const (
	// managementBasePath is the only data API prefix owned by this plugin.
	managementBasePath = "/v0/management/plugins/" + PluginID
	// resourceBasePath serves only the public, secret-free UI shell.
	resourceBasePath = "/v0/resource/plugins/" + PluginID

	// Registration paths are relative to the host Management API prefix.
	routesPrefix = "/plugins/" + PluginID

	routeHealth          = routesPrefix + "/health"
	routeModelsToken     = routesPrefix + "/models/token"
	routeKeys            = routesPrefix + "/keys"
	routeKeyDelete       = routeKeys + "/delete"
	routeKeyRotate       = routeKeys + "/rotate"
	routeKeyReset        = routeKeys + "/reset"
	routeKeyRecover      = routeKeys + "/recover"
	routeCapturesCleanup = routesPrefix + "/captures/cleanup"
	routeCapturesPurge   = routesPrefix + "/captures/purge"
	routeRequests        = routesPrefix + "/requests"
	routeRequestDetail   = routeRequests + "/detail"
	routeRequestView     = routeRequests + "/view"
	routeRequestFacets   = routeRequests + "/facets"
	routeSummary         = routesPrefix + "/summary"
	routeSettings        = routesPrefix + "/settings"
	routePrices          = routesPrefix + "/prices"
	routePricesRefresh   = routesPrefix + "/prices/refresh"
	routePricesPreview   = routesPrefix + "/prices/preview"
	routeCompact         = routesPrefix + "/compact"
	routeUI              = routesPrefix + "/ui"
	routeUIAlt           = routeUI + "/"
	routeUIJS            = routesPrefix + "/ui.js"
	routeUICSS           = routesPrefix + "/ui.css"
	routeThemeJS         = routesPrefix + "/theme.js"

	// maxJSONBodyBytes bounds every JSON request body accepted by this file.
	maxJSONBodyBytes = 1 << 20
	// maxSafeJSONInteger is the largest numeric value accepted from JSON.
	// Billing arithmetic is integer-only, so fractions and unsafe magnitudes
	// are rejected before they reach the engine or pricing service.
	maxSafeJSONInteger = int64(9_000_000_000_000_000)
	// priceRefreshTimeout bounds one on-demand catalog refresh.
	priceRefreshTimeout = 20 * time.Second
	// managementSchemaVersion is the released storage/management schema.
	managementSchemaVersion = 3

	// cspPolicy keeps the management shell same-origin frameable while allowing
	// only its own script, style and fetch sources.
	// Radix injects bounded scroll-lock CSS for accessible dialogs/menus. Permit
	// inline styles only; scripts, connections and frame ancestors stay same-origin.
	cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

// managementRegistrationResponse is the host RPC registration shape.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

// managementRegistration declares every static route the host can dispatch.
//
// CPA v8.0.30 normalizes plugin routes to an exact method+path map and rejects
// any path containing ':' or '*'. Key and request identifiers therefore travel
// in the query string; no parameterized path is registered or dispatched.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: routeHealth, Description: "Observer health and lifecycle counters."},
			{Method: http.MethodPost, Path: routeModelsToken, Description: "Issue a single-use capability for GET /v1/models only."},
			{Method: http.MethodGet, Path: routeKeys, Description: "List client keys."},
			{Method: http.MethodPost, Path: routeKeys, Description: "Create a client key; plaintext is available in authenticated management lists."},
			{Method: http.MethodPut, Path: routeKeys, Description: "Replace a client key selected by the id query parameter."},
			{Method: http.MethodDelete, Path: routeKeys, Description: "Revoke a client key selected by the id query parameter."},
			{Method: http.MethodPost, Path: routeKeyDelete, Description: "Permanently delete a client key selected by the id query parameter."},
			{Method: http.MethodPost, Path: routeKeyRotate, Description: "Rotate a client key selected by the id query parameter."},
			{Method: http.MethodPost, Path: routeKeyReset, Description: "Reset selected usage windows for a client key."},
			{Method: http.MethodPost, Path: routeKeyRecover, Description: "Acknowledge unknown accounting for a client key."},
			{Method: http.MethodGet, Path: routeRequests, Description: "Paginated request records."},
			{Method: http.MethodGet, Path: routeRequestView, Description: "Consistent timezone-local summary, series and records or groups."},
			{Method: http.MethodGet, Path: routeRequestFacets, Description: "Searchable historical request dimensions."},
			{Method: http.MethodGet, Path: routeRequestDetail, Description: "One request record plus captured details selected by the id query parameter."},
			{Method: http.MethodGet, Path: routeSummary, Description: "Aggregated request and cost summary."},
			{Method: http.MethodGet, Path: routeSettings, Description: "Effective capture, retention and pricing settings."},
			{Method: http.MethodPut, Path: routeSettings, Description: "Replace capture, retention and pricing settings atomically."},
			{Method: http.MethodGet, Path: routePrices, Description: "Current pricing snapshot."},
			{Method: http.MethodPost, Path: routePricesRefresh, Description: "Refresh the pricing catalog within a bounded interval."},
			{Method: http.MethodPost, Path: routePricesPreview, Description: "Preview available-model base prices without recording usage."},
			{Method: http.MethodPost, Path: routeCapturesCleanup, Description: "Remove expired captured content and return counters."},
			{Method: http.MethodPost, Path: routeCapturesPurge, Description: "Purge all captured content after explicit confirmation."},
			{Method: http.MethodPost, Path: routeCompact, Description: "Compact storage and return health counters."},
			{Method: http.MethodGet, Path: routeUI, Description: PluginName + " management UI."},
			{Method: http.MethodGet, Path: routeUIAlt, Description: PluginName + " management UI with trailing slash."},
			{Method: http.MethodGet, Path: routeUIJS, Description: PluginName + " management UI script."},
			{Method: http.MethodGet, Path: routeUICSS, Description: PluginName + " management UI styles."},
			{Method: http.MethodGet, Path: routeThemeJS, Description: PluginName + " pre-render theme bridge."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: PluginName, Description: PluginName + " management UI."},
			{Path: "/ui/", Description: PluginName + " management UI with trailing slash."},
			{Path: "/ui.js", Description: PluginName + " management UI script."},
			{Path: "/ui.css", Description: PluginName + " management UI styles."},
			{Path: "/theme.js", Description: PluginName + " pre-render theme bridge."},
		},
	}
}

// managementRequest mirrors the host management RPC wire shape. The embedded
// SDK fields intentionally have no JSON tags, so the host sends PascalCase.
type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

var managementRequestKeys = map[string]bool{
	"Method":           true,
	"Path":             true,
	"Headers":          true,
	"Query":            true,
	"Body":             true,
	"host_callback_id": true,
}

// handleManagement parses one management RPC request and returns a host
// envelope. Management payloads themselves are direct JSON responses, never
// nested RPC envelopes.
func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	req, err := parseManagementRequest(request)
	if err != nil {
		return errorEnvelope(&PluginError{
			Code:       "invalid_request",
			Message:    "malformed management request",
			HTTPStatus: http.StatusBadRequest,
		})
	}
	if len(req.Body) > maxJSONBodyBytes {
		return okEnvelope(errorResponse(http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MiB limit"))
	}
	return okEnvelope(m.dispatchManagement(req))
}

func parseManagementRequest(raw []byte) (managementRequest, error) {
	var req managementRequest
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return req, nil
	}
	if err := decodeJSONObject(trimmed, &req, managementRequestKeys); err != nil {
		return managementRequest{}, err
	}
	return req, nil
}

func (m *Manager) dispatchManagement(req managementRequest) pluginapi.ManagementResponse {
	if !acceptsManagementBody(req) && len(bytes.TrimSpace(req.Body)) != 0 {
		return bodyFailureResponse(errJSONInvalid)
	}
	// URL.Path is already decoded by the host. Never unescape it a second time.
	if strings.ContainsAny(req.Path, "%\\\x00\r\n") || strings.Contains(req.Path, "//") {
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
	for _, part := range strings.Split(req.Path, "/") {
		if part == "." || part == ".." {
			return errorResponse(http.StatusNotFound, "not_found", "not found")
		}
	}
	switch {
	case strings.HasPrefix(req.Path, managementBasePath+"/"):
		return m.dispatchManagementRoute(req, strings.TrimPrefix(req.Path, managementBasePath))
	case strings.HasPrefix(req.Path, resourceBasePath+"/"):
		return m.dispatchResourceRoute(req, strings.TrimPrefix(req.Path, resourceBasePath))
	default:
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
}

// acceptsManagementBody lists the exact static routes that take a JSON body.
// Everything else rejects a nonempty body, including methods that share a path.
func acceptsManagementBody(req managementRequest) bool {
	switch req.Method {
	case http.MethodPost:
		return req.Path == managementBasePath+"/keys" ||
			req.Path == managementBasePath+"/keys/rotate" ||
			req.Path == managementBasePath+"/prices/preview" ||
			req.Path == managementBasePath+"/keys/reset" ||
			req.Path == managementBasePath+"/keys/recover" ||
			req.Path == managementBasePath+"/captures/purge"
	case http.MethodPut:
		return req.Path == managementBasePath+"/keys" ||
			req.Path == managementBasePath+"/settings"
	default:
		return false
	}
}

func (m *Manager) dispatchManagementRoute(req managementRequest, route string) pluginapi.ManagementResponse {
	switch route {
	case "/models/token":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if len(req.Body) != 0 {
			return bodyFailureResponse(errJSONInvalid)
		}
		if req.Method != http.MethodPost {
			return methodNotAllowedResponse()
		}
		return m.serveCatalogToken()
	case "/health":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveHealth()

	case "/keys":
		switch req.Method {
		case http.MethodGet:
			includeDeleted, ok := parseKeysQuery(req.Query)
			if !ok {
				return invalidQueryResponse()
			}
			return m.serveKeys(includeDeleted)
		case http.MethodPost:
			if len(req.Query) != 0 {
				return invalidQueryResponse()
			}
			return m.serveCreateKey(req.Body)
		case http.MethodPut:
			id, ok := parseIDQuery(req.Query)
			if !ok {
				return invalidQueryResponse()
			}
			return m.serveUpdateKey(id, req.Body)
		case http.MethodDelete:
			id, ok := parseIDQuery(req.Query)
			if !ok {
				return invalidQueryResponse()
			}
			return m.serveRevokeKey(id)
		default:
			return methodNotAllowedResponse()
		}

	case "/keys/delete":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		id, ok := parseIDQuery(req.Query)
		if !ok {
			return invalidQueryResponse()
		}
		return m.serveDeleteKey(id)

	case "/keys/rotate":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		id, ok := parseIDQuery(req.Query)
		if !ok {
			return invalidQueryResponse()
		}
		return m.serveRotateKey(id, req.Body)

	case "/keys/reset":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		id, ok := parseIDQuery(req.Query)
		if !ok {
			return invalidQueryResponse()
		}
		return m.serveResetKey(id, req.Body)

	case "/keys/recover":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		id, ok := parseIDQuery(req.Query)
		if !ok {
			return invalidQueryResponse()
		}
		return m.serveRecoverKey(id, req.Body)

	case "/requests":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveRequests(req.Query, req.HostCallbackID)

	case "/requests/detail":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		id, ok := parseIDQuery(req.Query)
		if !ok {
			return invalidQueryResponse()
		}
		return m.serveRequestDetail(id, req.HostCallbackID)

	case "/requests/view":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveRequestView(req.Query, req.HostCallbackID)

	case "/requests/facets":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveRequestFacets(req.Query, req.HostCallbackID)

	case "/summary":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveSummary(req.Query)

	case "/settings":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		switch req.Method {
		case http.MethodGet:
			return m.serveSettings()
		case http.MethodPut:
			return m.serveUpdateSettings(req.Body)
		default:
			return methodNotAllowedResponse()
		}

	case "/prices":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.servePrices()

	case "/prices/preview":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if req.Method != http.MethodPost {
			return methodNotAllowedResponse()
		}
		return m.servePricePreview(req.Body)

	case "/prices/refresh":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.servePriceRefresh()

	case "/captures/cleanup":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveCapturesCleanup()

	case "/captures/purge":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveCapturesPurge(req.Body)

	case "/compact":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveCompact()

	case "/ui", "/ui/", "/ui.js", "/ui.css", "/theme.js":
		if len(req.Query) != 0 {
			return invalidQueryResponse()
		}
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveAsset(assetNames[route])

	default:
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
}

func (m *Manager) dispatchResourceRoute(req managementRequest, route string) pluginapi.ManagementResponse {
	if len(req.Query) != 0 {
		return invalidQueryResponse()
	}
	if !requireMethod(req.Method, http.MethodGet) {
		return methodNotAllowedResponse()
	}
	name, ok := assetNames[route]
	if !ok {
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
	return m.serveAsset(name)
}

func requireMethod(got, want string) bool {
	return got == want
}

// serveHealth returns the engine health snapshot plus the released versions.
func (m *Manager) serveHealth() pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		health := store.Health()
		m.billingFaults.Range(func(_, _ any) bool {
			health.LastError = "accounting callback failed; explicit Key recovery required"
			return false
		})
		return jsonResponse(http.StatusOK, healthResponse{
			Health:        health,
			Version:       Version,
			SchemaVersion: managementSchemaVersion,
		})
	})
}

type healthResponse struct {
	engine.Health
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
}

func (m *Manager) serveKeys(includeDeleted bool) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		var keys []engine.Key
		var err error
		if includeDeleted {
			keys, err = store.KeysIncludingDeleted()
		} else {
			keys, err = store.Keys()
		}
		if err != nil {
			return storeFailureResponse(err)
		}
		if keys == nil {
			keys = []engine.Key{}
		}
		for i := range keys {
			if _, fault := m.billingFaults.Load(keys[i].ID); fault {
				keys[i].BlockedReason = "accounting callback failed; explicit recovery required"
			}
		}
		return jsonResponse(http.StatusOK, keysResponse{Items: keys})
	})
}

type keysResponse struct {
	Items []engine.Key `json:"items"`
}

func (m *Manager) serveCreateKey(body []byte) pluginapi.ManagementResponse {
	var input struct {
		engine.KeySpec
		Secret json.RawMessage `json:"secret"`
	}
	if err := decodeRequestBody(body, &input); err != nil {
		return bodyFailureResponse(err)
	}
	var supplied string
	if len(input.Secret) != 0 {
		if err := json.Unmarshal(input.Secret, &supplied); err != nil || !bytes.HasPrefix(input.Secret, []byte(`"`)) {
			return bodyFailureResponse(errJSONInvalid)
		}
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		var key engine.Key
		var secret string
		var err error
		if len(input.Secret) == 0 {
			key, secret, err = store.CreateKey(input.KeySpec)
		} else {
			key, secret, err = store.CreateKeyWithSecret(input.KeySpec, supplied)
		}
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusCreated, keySecretResponse{Key: key, Secret: secret})
	})
}

type keySecretResponse struct {
	Key    engine.Key `json:"key"`
	Secret string     `json:"secret"`
}

func (m *Manager) serveUpdateKey(id string, body []byte) pluginapi.ManagementResponse {
	var spec engine.KeySpec
	if err := decodeRequestBody(body, &spec); err != nil {
		return bodyFailureResponse(err)
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		key, err := store.UpdateKey(id, spec)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, key)
	})
}

func (m *Manager) serveRevokeKey(id string) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		if err := store.RevokeKey(id); err != nil {
			return storeFailureResponse(err)
		}
		return emptyResponse(http.StatusNoContent)
	})
}

// Delete is terminal; revoke remains a separate DELETE operation.
func (m *Manager) serveDeleteKey(id string) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		if err := store.DeleteKey(id); err != nil {
			return storeFailureResponse(err)
		}
		return emptyResponse(http.StatusNoContent)
	})
}

func (m *Manager) serveRotateKey(id string, body []byte) pluginapi.ManagementResponse {
	var input struct {
		Secret json.RawMessage `json:"secret"`
	}
	if len(body) > 0 {
		if err := decodeRequestBody(body, &input); err != nil {
			return bodyFailureResponse(err)
		}
	}
	var supplied string
	if len(input.Secret) != 0 {
		if err := json.Unmarshal(input.Secret, &supplied); err != nil || !bytes.HasPrefix(input.Secret, []byte(`"`)) {
			return bodyFailureResponse(errJSONInvalid)
		}
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		var key engine.Key
		var secret string
		var err error
		if len(input.Secret) == 0 {
			key, secret, err = store.RotateKey(id)
		} else {
			key, secret, err = store.RotateKeyWithSecret(id, supplied)
		}
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, keySecretResponse{Key: key, Secret: secret})
	})
}

type resetRequest struct {
	Windows []string `json:"windows"`
}

var resetWindows = map[string]bool{
	"five_hour": true,
	"week":      true,
	"month":     true,
	"total":     true,
}

func (m *Manager) serveResetKey(id string, body []byte) pluginapi.ManagementResponse {
	var request resetRequest
	if err := decodeRequestBody(body, &request); err != nil {
		return bodyFailureResponse(err)
	}
	if len(request.Windows) == 0 {
		return errorResponse(http.StatusBadRequest, "invalid_request", "windows must not be empty")
	}
	seen := make(map[string]bool, len(request.Windows))
	for _, window := range request.Windows {
		if !resetWindows[window] || seen[window] {
			return errorResponse(http.StatusBadRequest, "invalid_request", "invalid windows")
		}
		seen[window] = true
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		key, err := store.ResetKey(id, request.Windows)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, key)
	})
}

type recoverRequest struct {
	Acknowledge bool `json:"acknowledge"`
}

// serveRecoverKey only acknowledges unknown/missing usage after an explicit
// confirmation; it never changes counters, limits or historical costs.
func (m *Manager) serveRecoverKey(id string, body []byte) pluginapi.ManagementResponse {
	var request recoverRequest
	if err := decodeRequestBody(body, &request); err != nil {
		return bodyFailureResponse(err)
	}
	if !request.Acknowledge {
		return errorResponse(http.StatusBadRequest, "invalid_request", "acknowledge must be true")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil {
		return unavailableResponse()
	}
	key, err := m.store.RecoverKey(id, true)
	if err != nil {
		return storeFailureResponse(err)
	}
	m.billingFaults.Delete(id)
	return jsonResponse(http.StatusOK, key)
}

func (m *Manager) serveRequests(values url.Values, callbackID string) pluginapi.ManagementResponse {
	query, err := m.parseQuery(values)
	if err != nil {
		return invalidQueryResponse()
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		page, err := store.Requests(query)
		if err != nil {
			return storeFailureResponse(err)
		}
		m.projectAccountLabels(page.Items, nil, callbackID)
		return jsonResponse(http.StatusOK, page)
	})
}

func (m *Manager) serveRequestDetail(id string, callbackID string) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		detail, err := store.Detail(id)
		if err != nil {
			return storeFailureResponse(err)
		}
		records := []engine.Record{detail.Record}
		m.projectAccountLabels(records, nil, callbackID)
		detail.Record = records[0]
		return jsonResponse(http.StatusOK, detail)
	})
}

func (m *Manager) serveSummary(values url.Values) pluginapi.ManagementResponse {
	query, err := m.parseQuery(values)
	if err != nil {
		return invalidQueryResponse()
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		summary, err := store.Summary(query)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, summary)
	})
}

func (m *Manager) serveSettings() pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		settings, err := store.Settings()
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, settings)
	})
}

func (m *Manager) serveUpdateSettings(body []byte) pluginapi.ManagementResponse {
	patch, err := parseSettingsPatch(body)
	if err != nil {
		return bodyFailureResponse(err)
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		saved, err := store.Settings()
		if err != nil {
			return storeFailureResponse(err)
		}
		settings, err := mergeSettings(saved, patch)
		if err != nil {
			return errorResponse(http.StatusBadRequest, "invalid_request", "capture_mode and capture_enabled conflict")
		}
		// UpdateSettings owns validation and applies Settings.Pricing to the
		// pricing service atomically; this handler must not call Configure itself.
		updated, err := store.UpdateSettings(settings)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, updated)
	})
}

var settingsRequestKeys = map[string]bool{
	"capture_enabled":           true,
	"capture_mode":              true,
	"capture_retention_seconds": true,
	"max_body_bytes":            true,
	"max_body_storage_bytes":    true,
	"raw_retention_hours":       true,
	"daily_retention_days":      true,
	"pricing":                   true,
}

var pricingRequestKeys = map[string]bool{
	"auto":     true,
	"source":   true,
	"prices":   true,
	"mappings": true,
}

// settingsPatch records field presence, not zero values. Absent new optional
// fields (mode, retention, mappings) and absent sections keep saved values.
type settingsPatch struct {
	hasCaptureEnabled bool
	captureEnabled    bool
	hasCaptureMode    bool
	captureMode       string
	hasRetention      bool
	retention         int
	hasMaxBodyBytes   bool
	maxBodyBytes      int
	hasMaxStorage     bool
	maxBodyStorage    int64
	hasRawRetention   bool
	rawRetentionHours int
	hasDailyRetention bool
	dailyRetention    int
	hasPricing        bool
	pricing           pricingPatch
}

type pricingPatch struct {
	hasAuto     bool
	auto        bool
	hasSource   bool
	source      string
	hasPrices   bool
	prices      []pricing.ManualPrice
	hasMappings bool
	mappings    []pricing.Mapping
}

func parseSettingsPatch(body []byte) (settingsPatch, error) {
	var patch settingsPatch
	trimmed := bytes.TrimSpace(body)
	if len(body) > maxJSONBodyBytes {
		return patch, errJSONTooLarge
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return patch, errJSONInvalid
	}
	if err := validateJSONDocument(trimmed, settingsRequestKeys); err != nil {
		return patch, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return patch, errJSONInvalid
	}
	for key, raw := range fields {
		if jsonNull(raw) {
			return patch, errJSONInvalid
		}
		switch key {
		case "capture_enabled":
			if err := decodeStrictJSON(raw, &patch.captureEnabled); err != nil {
				return patch, err
			}
			patch.hasCaptureEnabled = true
		case "capture_mode":
			if err := decodeStrictJSON(raw, &patch.captureMode); err != nil {
				return patch, err
			}
			patch.hasCaptureMode = true
		case "capture_retention_seconds":
			if err := decodeStrictJSON(raw, &patch.retention); err != nil {
				return patch, err
			}
			patch.hasRetention = true
		case "max_body_bytes":
			if err := decodeStrictJSON(raw, &patch.maxBodyBytes); err != nil {
				return patch, err
			}
			patch.hasMaxBodyBytes = true
		case "max_body_storage_bytes":
			if err := decodeStrictJSON(raw, &patch.maxBodyStorage); err != nil {
				return patch, err
			}
			patch.hasMaxStorage = true
		case "raw_retention_hours":
			if err := decodeStrictJSON(raw, &patch.rawRetentionHours); err != nil {
				return patch, err
			}
			patch.hasRawRetention = true
		case "daily_retention_days":
			if err := decodeStrictJSON(raw, &patch.dailyRetention); err != nil {
				return patch, err
			}
			patch.hasDailyRetention = true
		case "pricing":
			parsed, err := parsePricingPatch(raw)
			if err != nil {
				return patch, err
			}
			patch.pricing = parsed
			patch.hasPricing = true
		default:
			return patch, errJSONInvalid
		}
	}
	return patch, nil
}

func parsePricingPatch(raw []byte) (pricingPatch, error) {
	var patch pricingPatch
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return patch, errJSONInvalid
	}
	if err := validateJSONDocument(trimmed, pricingRequestKeys); err != nil {
		return patch, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return patch, errJSONInvalid
	}
	for key, value := range fields {
		if jsonNull(value) {
			return patch, errJSONInvalid
		}
		switch key {
		case "auto":
			if err := decodeStrictJSON(value, &patch.auto); err != nil {
				return patch, err
			}
			patch.hasAuto = true
		case "source":
			if err := decodeStrictJSON(value, &patch.source); err != nil {
				return patch, err
			}
			patch.hasSource = true
		case "prices":
			if err := decodeStrictJSON(value, &patch.prices); err != nil {
				return patch, err
			}
			patch.hasPrices = true
		case "mappings":
			if err := decodeStrictJSON(value, &patch.mappings); err != nil {
				return patch, err
			}
			patch.hasMappings = true
		default:
			return patch, errJSONInvalid
		}
	}
	return patch, nil
}

func jsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errJSONInvalid
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

var errSettingsConflict = errors.New("settings conflict")

// mergeSettings normalizes at the management boundary before engine's own
// effectiveSettings compatibility pass. A legacy boolean never widens an
// already enabled header-only mode; an explicit mode and boolean must agree.
func mergeSettings(saved engine.Settings, patch settingsPatch) (engine.Settings, error) {
	out := saved
	if patch.hasMaxBodyBytes {
		out.MaxBodyBytes = patch.maxBodyBytes
	}
	if patch.hasMaxStorage {
		out.MaxBodyStorageBytes = patch.maxBodyStorage
	}
	if patch.hasRawRetention {
		out.RawRetentionHours = patch.rawRetentionHours
	}
	if patch.hasDailyRetention {
		out.DailyRetentionDays = patch.dailyRetention
	}
	if patch.hasPricing {
		out.Pricing = saved.Pricing
		if patch.pricing.hasAuto {
			out.Pricing.Auto = patch.pricing.auto
		}
		if patch.pricing.hasSource {
			out.Pricing.Source = patch.pricing.source
		}
		if patch.pricing.hasPrices {
			out.Pricing.Prices = patch.pricing.prices
		}
		if patch.pricing.hasMappings {
			out.Pricing.Mappings = patch.pricing.mappings
		}
	}
	if patch.hasCaptureMode {
		expected := patch.captureMode != engine.CaptureModeOff
		if patch.hasCaptureEnabled && patch.captureEnabled != expected {
			return engine.Settings{}, errSettingsConflict
		}
		out.CaptureMode = patch.captureMode
		out.CaptureEnabled = expected
	} else if patch.hasCaptureEnabled {
		if patch.captureEnabled {
			switch saved.CaptureMode {
			case engine.CaptureModeHeaders, engine.CaptureModeHeadersAndBody:
				out.CaptureMode = saved.CaptureMode
			default:
				out.CaptureMode = engine.CaptureModeHeadersAndBody
			}
			out.CaptureEnabled = true
		} else {
			out.CaptureMode = engine.CaptureModeOff
			out.CaptureEnabled = false
		}
	}
	if patch.hasRetention {
		out.CaptureRetentionSeconds = patch.retention
	}
	return out, nil
}

func (m *Manager) servePrices() pluginapi.ManagementResponse {
	m.mu.RLock()
	prices := m.prices
	m.mu.RUnlock()
	if prices == nil {
		return unavailableResponse()
	}
	return jsonResponse(http.StatusOK, prices.Snapshot())
}

func (m *Manager) servePricePreview(body []byte) pluginapi.ManagementResponse {
	var input struct {
		Models []pricing.ModelIdentity `json:"models"`
	}
	if err := decodeRequestBody(body, &input); err != nil {
		return bodyFailureResponse(err)
	}
	if input.Models == nil {
		return bodyFailureResponse(errJSONInvalid)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.prices == nil {
		return unavailableResponse()
	}
	items, err := m.prices.Preview(input.Models)
	if err != nil {
		return bodyFailureResponse(errJSONInvalid)
	}
	return jsonResponse(http.StatusOK, struct {
		Items []pricing.Price `json:"items"`
	}{Items: items})
}

func (m *Manager) servePriceRefresh() pluginapi.ManagementResponse {
	// Hold the read lock for the whole network refresh so a concurrent
	// reconfigure cannot replace or close the pricing service mid-operation.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.prices == nil {
		return unavailableResponse()
	}

	ctx, cancel := context.WithTimeout(context.Background(), priceRefreshTimeout)
	defer cancel()
	err := m.prices.Refresh(ctx)

	var snapshot pricing.Snapshot = m.prices.Snapshot()
	if err != nil && snapshot.Error == "" {
		// Never expose the underlying transport error or response body.
		snapshot.Error = "price refresh failed"
	}
	return jsonResponse(http.StatusOK, snapshot)
}

func (m *Manager) serveCapturesCleanup() pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		result, err := store.CleanupCaptures()
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, result)
	})
}

func (m *Manager) serveCapturesPurge(body []byte) pluginapi.ManagementResponse {
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if err := decodeRequestBody(body, &request); err != nil {
		return bodyFailureResponse(err)
	}
	if !request.Confirm {
		return errorResponse(http.StatusBadRequest, "invalid_request", "confirm must be true")
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		result, err := store.PurgeCaptures()
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, result)
	})
}

func (m *Manager) serveCompact() pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		if err := store.Compact(); err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, store.Health())
	})
}

// withStore holds the manager read lock for the complete store interaction.
// Lifecycle replacement therefore cannot detach and close a store mid-call.
func (m *Manager) withStore(fn func(*engine.Store) pluginapi.ManagementResponse) pluginapi.ManagementResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil {
		return unavailableResponse()
	}
	return fn(m.store)
}

// parseQuery validates the complete query parameter set and builds an engine
// query. Unknown or repeated parameters are rejected rather than ignored.
func (m *Manager) parseQuery(values url.Values) (engine.Query, error) {
	allowed := map[string]bool{
		"from":       true,
		"to":         true,
		"key_id":     true,
		"model":      true,
		"provider":   true,
		"auth_index": true,
		"status":     true,
		"page":       true,
		"page_size":  true,
	}
	for key, list := range values {
		if !allowed[key] || len(list) != 1 {
			return engine.Query{}, errJSONInvalid
		}
	}

	now := m.currentTime().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	to := now

	if values.Has("from") {
		parsed, err := parseRFC3339(values.Get("from"))
		if err != nil {
			return engine.Query{}, err
		}
		from = parsed
	}
	if values.Has("to") {
		parsed, err := parseRFC3339(values.Get("to"))
		if err != nil {
			return engine.Query{}, err
		}
		to = parsed
	}
	if !from.Before(to) {
		// Query ranges are half-open [from,to); an empty range is invalid.
		return engine.Query{}, errJSONInvalid
	}

	status := values.Get("status")
	if status != "" && status != "success" && status != "failed" {
		return engine.Query{}, errJSONInvalid
	}

	page, err := parsePageNumber(values.Get("page"), 1, 0)
	if err != nil {
		return engine.Query{}, err
	}
	pageSize, err := parsePageNumber(values.Get("page_size"), 25, 100)
	if err != nil {
		return engine.Query{}, err
	}

	return engine.Query{
		From:      from,
		To:        to,
		KeyID:     values.Get("key_id"),
		Model:     values.Get("model"),
		Provider:  values.Get("provider"),
		AuthIndex: values.Get("auth_index"),
		Status:    status,
		Page:      page,
		PageSize:  pageSize,
	}, nil
}

func parseRFC3339(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errJSONInvalid
	}
	return parsed.UTC(), nil
}

// parsePageNumber accepts only unsigned base-10 digits. A missing value uses
// the default; max > 0 enforces an inclusive upper bound.
func parsePageNumber(value string, defaultValue, max int) (int, error) {
	if value == "" {
		return defaultValue, nil
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errJSONInvalid
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || parsed > maxSafeJSONInteger || parsed > int64(^uint(0)>>1) {
		return 0, errJSONInvalid
	}
	if max > 0 && parsed > int64(max) {
		return 0, errJSONInvalid
	}
	return int(parsed), nil
}

// parseIDQuery accepts exactly one query parameter, "id", with exactly one
// nonempty, decoded identifier. Unknown or repeated parameters are rejected.
// parseKeysQuery accepts only an optional single include_deleted boolean.
// Omitting it keeps the historical default of excluding tombstones.
func parseKeysQuery(values url.Values) (bool, bool) {
	if len(values) == 0 {
		return false, true
	}
	if len(values) != 1 {
		return false, false
	}
	list, ok := values["include_deleted"]
	if !ok || len(list) != 1 {
		return false, false
	}
	switch list[0] {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func parseIDQuery(values url.Values) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	list, ok := values["id"]
	if !ok || len(list) != 1 {
		return "", false
	}
	return queryIdentifier(list[0])
}

// queryIdentifier rejects empty, padded, separator-bearing or otherwise
// unsafe identifiers. The host has already decoded the query value.
func queryIdentifier(raw string) (string, bool) {
	if raw == "" || raw != strings.TrimSpace(raw) ||
		raw == "." || raw == ".." ||
		strings.ContainsAny(raw, " \t/\\%\x00\r\n") {
		return "", false
	}
	return raw, true
}

var assetNames = map[string]string{
	"/ui":       "ui.html",
	"/ui/":      "ui.html",
	"/ui.js":    "ui.js",
	"/ui.css":   "ui.css",
	"/theme.js": "theme.js",
}

var assetContentTypes = map[string]string{
	"ui.html":  "text/html; charset=utf-8",
	"ui.js":    "text/javascript; charset=utf-8",
	"ui.css":   "text/css; charset=utf-8",
	"theme.js": "text/javascript; charset=utf-8",
}

func (m *Manager) serveAsset(name string) pluginapi.ManagementResponse {
	if m.assets == nil || name == "" {
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
	body, contentType, ok := m.assets(name)
	if !ok {
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
	if contentType == "" {
		contentType = assetContentTypes[name]
	}
	headers := baseHeaders()
	headers.Set("Content-Type", contentType)
	headers.Set("Content-Security-Policy", cspPolicy)
	headers.Set("Referrer-Policy", "no-referrer")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}
}

// managementFailure pairs a sanitized API error with its HTTP status.
type managementFailure struct {
	status  int
	code    string
	message string
}

// managementFailureStatuses is the sentinel-to-status map for every public
// engine error. errors.Is keeps wrapped engine errors working without leaking
// their text.
var managementFailureStatuses = map[error]managementFailure{
	engine.ErrNotFound:     {status: http.StatusNotFound, code: "not_found", message: "not found"},
	engine.ErrInvalid:      {status: http.StatusBadRequest, code: "invalid_request", message: "invalid request"},
	engine.ErrUnauthorized: {status: http.StatusUnauthorized, code: "unauthorized", message: "unauthorized"},
	engine.ErrExpired:      {status: http.StatusUnauthorized, code: "expired", message: "key expired"},
	engine.ErrForbidden:    {status: http.StatusForbidden, code: "forbidden", message: "forbidden"},
	engine.ErrQuota:        {status: http.StatusTooManyRequests, code: "quota_exceeded", message: "quota exceeded"},
	engine.ErrConcurrency:  {status: http.StatusTooManyRequests, code: "concurrency_exceeded", message: "concurrency limit exceeded"},
	engine.ErrBusy:         {status: http.StatusConflict, code: "busy", message: "resource busy"},
	engine.ErrConflict:     {status: http.StatusConflict, code: "conflict", message: "client key already exists"},
	engine.ErrDeleted:      {status: http.StatusGone, code: "deleted", message: "client key deleted"},
	engine.ErrUnsupported:  {status: http.StatusConflict, code: "unsupported", message: "unsupported observer feature version"},
	engine.ErrMaintenance:  {status: http.StatusConflict, code: "maintenance", message: "maintenance in progress"},
	engine.ErrAccounting:   {status: http.StatusServiceUnavailable, code: "accounting_unavailable", message: "accounting unavailable"},
	engine.ErrClosed:       {status: http.StatusServiceUnavailable, code: "unavailable", message: "observer unavailable"},
}

func storeFailureResponse(err error) pluginapi.ManagementResponse {
	for sentinel, failure := range managementFailureStatuses {
		if errors.Is(err, sentinel) {
			return errorResponse(failure.status, failure.code, failure.message)
		}
	}
	return errorResponse(http.StatusInternalServerError, "internal_error", "internal error")
}

func unavailableResponse() pluginapi.ManagementResponse {
	return errorResponse(http.StatusServiceUnavailable, "unavailable", "observer unavailable")
}

func methodNotAllowedResponse() pluginapi.ManagementResponse {
	return errorResponse(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func bodyFailureResponse(err error) pluginapi.ManagementResponse {
	if errors.Is(err, errJSONTooLarge) {
		return errorResponse(http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1MiB limit")
	}
	return errorResponse(http.StatusBadRequest, "invalid_request", "invalid request body")
}

func invalidQueryResponse() pluginapi.ManagementResponse {
	return errorResponse(http.StatusBadRequest, "invalid_request", "invalid query parameters")
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, "internal_error", "internal error")
	}
	headers := baseHeaders()
	headers.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}

func emptyResponse(status int) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: status, Headers: baseHeaders()}
}

type managementErrorResponse struct {
	Error managementErrorDetail `json:"error"`
}

type managementErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorResponse(status int, code, message string) pluginapi.ManagementResponse {
	body, err := json.Marshal(managementErrorResponse{
		Error: managementErrorDetail{Code: code, Message: message},
	})
	if err != nil {
		body = []byte(`{"error":{"code":"internal_error","message":"internal error"}}`)
		status = http.StatusInternalServerError
	}
	headers := baseHeaders()
	headers.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}

func baseHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	return headers
}

var (
	errJSONInvalid      = errors.New("invalid JSON input")
	errJSONTooLarge     = errors.New("JSON input exceeds 1MiB limit")
	errJSONUnsafeNumber = errors.New("unsafe JSON number")
)

// decodeRequestBody enforces the 1MiB body limit before strict decoding.
func decodeRequestBody(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errJSONInvalid
	}
	if len(raw) > maxJSONBodyBytes {
		return errJSONTooLarge
	}
	return decodeJSONObject(trimmed, target, nil)
}

// decodeJSONObject rejects non-objects, unknown top-level keys, duplicate
// object keys at any depth, trailing values, unsafe numeric forms and unknown
// nested fields.
func decodeJSONObject(raw []byte, target any, allowedTopLevel map[string]bool) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errJSONInvalid
	}
	if err := validateJSONDocument(trimmed, allowedTopLevel); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errJSONInvalid
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

type jsonFrame struct {
	object    bool
	keys      map[string]bool
	expectKey bool
}

// validateJSONDocument performs one token pass over the complete document. It
// tracks object keys and arrays, rejects duplicate keys recursively (including
// nested keys/rates), optionally restricts top-level keys, and rejects
// fractions, exponents, negatives and integers above the billing-safe bound.
func validateJSONDocument(raw []byte, allowedTopLevel map[string]bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var stack []*jsonFrame
	rootSeen := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !rootSeen || len(stack) != 0 {
				return errJSONInvalid
			}
			return nil
		}
		if err != nil {
			return errJSONInvalid
		}
		if rootSeen && len(stack) == 0 {
			// A second root value is trailing input.
			return errJSONInvalid
		}

		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				if len(stack) != 0 {
					parent := stack[len(stack)-1]
					if parent.object && parent.expectKey {
						return errJSONInvalid
					}
					if parent.object {
						parent.expectKey = true
					}
				}
				frame := &jsonFrame{object: value == '{'}
				if frame.object {
					frame.keys = map[string]bool{}
					frame.expectKey = true
				}
				stack = append(stack, frame)
				rootSeen = true
			case '}', ']':
				if len(stack) == 0 {
					return errJSONInvalid
				}
				frame := stack[len(stack)-1]
				if frame.object != (value == '}') {
					return errJSONInvalid
				}
				stack = stack[:len(stack)-1]
			default:
				return errJSONInvalid
			}

		case string:
			if len(stack) == 0 {
				return errJSONInvalid
			}
			frame := stack[len(stack)-1]
			if !frame.object {
				continue
			}
			if !frame.expectKey {
				frame.expectKey = true
				continue
			}
			if frame.keys[value] {
				return errJSONInvalid
			}
			frame.keys[value] = true
			if len(stack) == 1 && allowedTopLevel != nil && !allowedTopLevel[value] {
				return errJSONInvalid
			}
			frame.expectKey = false

		case json.Number:
			if len(stack) == 0 {
				return errJSONInvalid
			}
			frame := stack[len(stack)-1]
			if frame.object {
				if frame.expectKey {
					return errJSONInvalid
				}
				frame.expectKey = true
			}
			text := value.String()
			if strings.HasPrefix(text, "-") || strings.ContainsAny(text, ".eE") {
				return errJSONUnsafeNumber
			}
			parsed, err := strconv.ParseInt(text, 10, 64)
			if err != nil || parsed < 0 || parsed > maxSafeJSONInteger {
				return errJSONUnsafeNumber
			}

		default: // bool, nil
			if len(stack) == 0 {
				return errJSONInvalid
			}
			frame := stack[len(stack)-1]
			if frame.object {
				if frame.expectKey {
					return errJSONInvalid
				}
				frame.expectKey = true
			}
		}
	}
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errJSONInvalid
	}
	return nil
}

// currentTime keeps tests deterministic when now is injected.
func (m *Manager) currentTime() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}
