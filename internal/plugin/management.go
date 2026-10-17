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

	routeHealth        = routesPrefix + "/health"
	routeKeys          = routesPrefix + "/keys"
	routeRequests      = routesPrefix + "/requests"
	routeSummary       = routesPrefix + "/summary"
	routeSettings      = routesPrefix + "/settings"
	routePrices        = routesPrefix + "/prices"
	routePricesRefresh = routesPrefix + "/prices/refresh"
	routeCompact       = routesPrefix + "/compact"
	routeUI            = routesPrefix + "/ui"
	routeUIJS          = routesPrefix + "/ui.js"
	routeUICSS         = routesPrefix + "/ui.css"

	// maxJSONBodyBytes bounds every JSON request body accepted by this file.
	maxJSONBodyBytes = 1 << 20
	// maxSafeJSONInteger is the largest numeric value accepted from JSON.
	// Billing arithmetic is integer-only, so fractions and unsafe magnitudes
	// are rejected before they reach the engine or pricing service.
	maxSafeJSONInteger = int64(9_000_000_000_000_000)
	// priceRefreshTimeout bounds one on-demand catalog refresh.
	priceRefreshTimeout = 20 * time.Second
	// managementSchemaVersion is the released storage/management schema.
	managementSchemaVersion = 2

	// cspPolicy keeps the management shell same-origin frameable while allowing
	// only its own script, style and fetch sources.
	cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

// managementRegistrationResponse is the host RPC registration shape.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

// managementRegistration declares every static route the host can dispatch.
//
// Limitation: CPA v8.0.30 normalizes plugin routes to an exact method+path map
// and rejects any path containing ':' or '*'. Parameterized paths such as
// /keys/{id}, /keys/{id}/rotate, /keys/{id}/reset and /requests/{id} therefore
// cannot be registered on that host. handleManagement still implements those
// exact documented paths (see dispatchKeyRoute and the /requests/ branch), but
// only static paths are declared here. Revisit when the host supports
// parameterized plugin routes.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: routeHealth, Description: "Observer health and lifecycle counters."},
			{Method: http.MethodGet, Path: routeKeys, Description: "List client keys."},
			{Method: http.MethodPost, Path: routeKeys, Description: "Create a client key; the plaintext secret is returned once."},
			{Method: http.MethodGet, Path: routeRequests, Description: "Paginated request records."},
			{Method: http.MethodGet, Path: routeSummary, Description: "Aggregated request and cost summary."},
			{Method: http.MethodGet, Path: routeSettings, Description: "Effective capture, retention and pricing settings."},
			{Method: http.MethodPut, Path: routeSettings, Description: "Replace capture, retention and pricing settings atomically."},
			{Method: http.MethodGet, Path: routePrices, Description: "Current pricing snapshot."},
			{Method: http.MethodPost, Path: routePricesRefresh, Description: "Refresh the pricing catalog within a bounded interval."},
			{Method: http.MethodPost, Path: routeCompact, Description: "Compact storage and return health counters."},
			{Method: http.MethodGet, Path: routeUI, Description: PluginName + " management UI."},
			{Method: http.MethodGet, Path: routeUIJS, Description: PluginName + " management UI script."},
			{Method: http.MethodGet, Path: routeUICSS, Description: PluginName + " management UI styles."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: PluginName, Description: PluginName + " management UI."},
			{Path: "/ui.js", Description: PluginName + " management UI script."},
			{Path: "/ui.css", Description: PluginName + " management UI styles."},
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
	acceptBody := req.Method == http.MethodPost && req.Path == managementBasePath+"/keys" ||
		req.Method == http.MethodPut && (req.Path == managementBasePath+"/settings" || strings.HasPrefix(req.Path, managementBasePath+"/keys/")) ||
		req.Method == http.MethodPost && strings.HasPrefix(req.Path, managementBasePath+"/keys/") && strings.HasSuffix(req.Path, "/reset")
	if !acceptBody && len(bytes.TrimSpace(req.Body)) != 0 {
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
	if req.Path != managementBasePath+"/requests" && req.Path != managementBasePath+"/summary" && len(req.Query) != 0 {
		return invalidQueryResponse()
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

func (m *Manager) dispatchManagementRoute(req managementRequest, route string) pluginapi.ManagementResponse {
	switch route {
	case "/health":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveHealth()

	case "/keys":
		switch req.Method {
		case http.MethodGet:
			return m.serveKeys()
		case http.MethodPost:
			return m.serveCreateKey(req.Body)
		default:
			return methodNotAllowedResponse()
		}

	case "/requests":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveRequests(req.Query)

	case "/summary":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveSummary(req.Query)

	case "/settings":
		switch req.Method {
		case http.MethodGet:
			return m.serveSettings()
		case http.MethodPut:
			return m.serveUpdateSettings(req.Body)
		default:
			return methodNotAllowedResponse()
		}

	case "/prices":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.servePrices()

	case "/prices/refresh":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.servePriceRefresh()

	case "/compact":
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveCompact()

	case "/ui", "/ui/", "/ui.js", "/ui.css":
		if !requireMethod(req.Method, http.MethodGet) {
			return methodNotAllowedResponse()
		}
		return m.serveAsset(assetNames[route])

	default:
		if strings.HasPrefix(route, "/keys/") {
			return m.dispatchKeyRoute(req, strings.TrimPrefix(route, "/keys/"))
		}
		if strings.HasPrefix(route, "/requests/") {
			if !requireMethod(req.Method, http.MethodGet) {
				return methodNotAllowedResponse()
			}
			id, ok := pathIdentifier(strings.TrimPrefix(route, "/requests/"))
			if !ok {
				return errorResponse(http.StatusNotFound, "not_found", "not found")
			}
			return m.serveRequestDetail(id)
		}
		return errorResponse(http.StatusNotFound, "not_found", "not found")
	}
}

func (m *Manager) dispatchKeyRoute(req managementRequest, rest string) pluginapi.ManagementResponse {
	switch {
	case strings.HasSuffix(rest, "/rotate"):
		id, ok := pathIdentifier(strings.TrimSuffix(rest, "/rotate"))
		if !ok {
			return errorResponse(http.StatusNotFound, "not_found", "not found")
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveRotateKey(id)

	case strings.HasSuffix(rest, "/reset"):
		id, ok := pathIdentifier(strings.TrimSuffix(rest, "/reset"))
		if !ok {
			return errorResponse(http.StatusNotFound, "not_found", "not found")
		}
		if !requireMethod(req.Method, http.MethodPost) {
			return methodNotAllowedResponse()
		}
		return m.serveResetKey(id, req.Body)

	default:
		id, ok := pathIdentifier(rest)
		if !ok {
			return errorResponse(http.StatusNotFound, "not_found", "not found")
		}
		switch req.Method {
		case http.MethodPut:
			return m.serveUpdateKey(id, req.Body)
		case http.MethodDelete:
			return m.serveRevokeKey(id)
		default:
			return methodNotAllowedResponse()
		}
	}
}

func (m *Manager) dispatchResourceRoute(req managementRequest, route string) pluginapi.ManagementResponse {
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
		return jsonResponse(http.StatusOK, healthResponse{
			Health:        store.Health(),
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

func (m *Manager) serveKeys() pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		keys, err := store.Keys()
		if err != nil {
			return storeFailureResponse(err)
		}
		if keys == nil {
			keys = []engine.Key{}
		}
		return jsonResponse(http.StatusOK, keysResponse{Items: keys})
	})
}

type keysResponse struct {
	Items []engine.Key `json:"items"`
}

func (m *Manager) serveCreateKey(body []byte) pluginapi.ManagementResponse {
	var spec engine.KeySpec
	if err := decodeRequestBody(body, &spec); err != nil {
		return bodyFailureResponse(err)
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		key, secret, err := store.CreateKey(spec)
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

func (m *Manager) serveRotateKey(id string) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		key, secret, err := store.RotateKey(id)
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

func (m *Manager) serveRequests(values url.Values) pluginapi.ManagementResponse {
	query, err := m.parseQuery(values)
	if err != nil {
		return invalidQueryResponse()
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		page, err := store.Requests(query)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, page)
	})
}

func (m *Manager) serveRequestDetail(id string) pluginapi.ManagementResponse {
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		detail, err := store.Detail(id)
		if err != nil {
			return storeFailureResponse(err)
		}
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
	var settings engine.Settings
	if err := decodeRequestBody(body, &settings); err != nil {
		return bodyFailureResponse(err)
	}
	// UpdateSettings owns validation and applies Settings.Pricing to the
	// pricing service atomically; this handler must not call Configure itself.
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		updated, err := store.UpdateSettings(settings)
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, updated)
	})
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

func (m *Manager) servePriceRefresh() pluginapi.ManagementResponse {
	m.mu.RLock()
	prices := m.prices
	m.mu.RUnlock()
	if prices == nil {
		return unavailableResponse()
	}

	ctx, cancel := context.WithTimeout(context.Background(), priceRefreshTimeout)
	defer cancel()
	err := prices.Refresh(ctx)

	var snapshot pricing.Snapshot = prices.Snapshot()
	if err != nil && snapshot.Error == "" {
		// Never expose the underlying transport error or response body.
		snapshot.Error = "price refresh failed"
	}
	return jsonResponse(http.StatusOK, snapshot)
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
	if from.After(to) {
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

// pathIdentifier accepts one decoded path segment. Slashes are rejected so a
// crafted path cannot address a different route after unescaping.
func pathIdentifier(raw string) (string, bool) {
	if raw == "" || raw == "." || raw == ".." || strings.ContainsAny(raw, "/\\%\x00\r\n") {
		return "", false
	}
	return raw, true
}

var assetNames = map[string]string{
	"/ui":     "ui.html",
	"/ui/":    "ui.html",
	"/ui.js":  "ui.js",
	"/ui.css": "ui.css",
}

var assetContentTypes = map[string]string{
	"ui.html": "text/html; charset=utf-8",
	"ui.js":   "text/javascript; charset=utf-8",
	"ui.css":  "text/css; charset=utf-8",
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

// decodeJSONObject rejects non-objects, unknown top-level keys, trailing
// values, unsafe numeric forms and unknown nested fields.
func decodeJSONObject(raw []byte, target any, allowedTopLevel map[string]bool) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errJSONInvalid
	}
	if allowedTopLevel != nil {
		if err := validateTopLevelKeys(trimmed, allowedTopLevel); err != nil {
			return err
		}
	}
	if err := validateSafeNumbers(trimmed); err != nil {
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

func validateTopLevelKeys(raw []byte, allowed map[string]bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return errJSONInvalid
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return errJSONInvalid
	}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return errJSONInvalid
		}
		key, ok := token.(string)
		if !ok || !allowed[key] {
			return errJSONInvalid
		}
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			return errJSONInvalid
		}
	}
	token, err = decoder.Token()
	if err != nil {
		return errJSONInvalid
	}
	delim, ok = token.(json.Delim)
	if !ok || delim != '}' {
		return errJSONInvalid
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errJSONInvalid
	}
	return nil
}

// validateSafeNumbers rejects fractions, exponents, negative values and
// integers above the billing-safe bound anywhere in the document.
func validateSafeNumbers(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errJSONInvalid
		}
		number, ok := token.(json.Number)
		if !ok {
			continue
		}
		text := number.String()
		if strings.HasPrefix(text, "-") || strings.ContainsAny(text, ".eE") {
			return errJSONUnsafeNumber
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || value < 0 || value > maxSafeJSONInteger {
			return errJSONUnsafeNumber
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
