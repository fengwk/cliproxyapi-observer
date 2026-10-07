package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

const (
	// routesPrefix is the plugin-owned Management API path declared to the host.
	routesPrefix = "/plugins/" + PluginID

	summaryRoute  = routesPrefix + "/summary"
	requestsRoute = routesPrefix + "/requests"
	bodyRoute     = routesPrefix + "/body"
	settingsRoute = routesPrefix + "/settings"
	healthRoute   = routesPrefix + "/health"

	// resourcePrefix is the public, secret-free browser resource prefix.
	resourcePrefix = "/resource/plugins/" + PluginID

	defaultQueryWindow  = 24 * time.Hour
	defaultRequestLimit = 50
	maxRequestLimit     = 100

	// maxStatsRetentionDays hard-caps the accepted query range regardless of the
	// configured statistics retention.
	maxStatsRetentionDays = 3650

	// cspPolicy keeps the embedded page same-origin frameable without widening
	// any directive. The UI slice must not rely on inline scripts or styles.
	cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'"
)

// managementRegistrationResponse mirrors the host's expected registration shape.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

// managementRegistration declares the read-only data routes and the public
// resource menu. Every route is a GET; the observer never mutates host state.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: summaryRoute, Description: "Aggregated observation summary for the selected period."},
			{Method: http.MethodGet, Path: requestsRoute, Description: "Cursor-paginated observed request metadata."},
			{Method: http.MethodGet, Path: bodyRoute, Description: "One captured request body by request_id."},
			{Method: http.MethodGet, Path: settingsRoute, Description: "Effective capture and retention settings."},
			{Method: http.MethodGet, Path: healthRoute, Description: "Writer and drop counters."},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/ui", Menu: PluginName, Description: PluginName + " observation dashboard."},
			{Path: "/ui.js", Description: PluginName + " dashboard script."},
			{Path: "/ui.css", Description: PluginName + " dashboard styles."},
		},
	}
}

// managementRequest mirrors the host management request wire shape.
type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope(&PluginError{Code: "invalid_request", Message: "malformed management request", HTTPStatus: http.StatusBadRequest})
		}
	}
	if req.Method != http.MethodGet {
		return okEnvelope(managementJSON(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
	}
	if route, ok := managementRoute(req.Path); ok {
		return okEnvelope(m.serveManagement(route, req.Query))
	}
	if route, ok := resourceRoute(req.Path); ok {
		return okEnvelope(m.serveResource(route))
	}
	return okEnvelope(managementJSON(http.StatusNotFound, map[string]string{"error": "not found"}))
}

// managementRoute strips a /v0/management or /v8/management prefix. Both
// prefixes are accepted so the plugin works regardless of how a host build
// exposes plugin-owned management routes.
func managementRoute(path string) (string, bool) {
	for _, prefix := range []string{"/v0/management", "/v8/management"} {
		if path == prefix {
			return "", true
		}
		if strings.HasPrefix(path, prefix+"/") {
			return path[len(prefix):], true
		}
	}
	return "", false
}

// resourceRoute strips the public resource prefix.
func resourceRoute(path string) (string, bool) {
	for _, base := range []string{"/v0" + resourcePrefix, "/v8" + resourcePrefix} {
		if strings.HasPrefix(path, base+"/") {
			return path[len(base):], true
		}
	}
	return "", false
}

func (m *Manager) serveManagement(route string, query url.Values) pluginapi.ManagementResponse {
	switch route {
	case summaryRoute:
		return m.serveSummary(query)
	case requestsRoute:
		return m.serveRequests(query)
	case bodyRoute:
		return m.serveBody(query)
	case settingsRoute:
		return m.serveSettings()
	case healthRoute:
		return m.serveHealth()
	default:
		return managementJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (m *Manager) serveSummary(values url.Values) pluginapi.ManagementResponse {
	store, cfg, _ := m.snapshot()
	if store == nil {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	query, herr := m.parseQuery(values, statsRange(cfg), true, false)
	if herr != nil {
		return managementError(herr.status, herr.message)
	}
	summary, err := store.Summary(query)
	if err != nil {
		return managementStoreError(err)
	}
	return managementJSON(http.StatusOK, summary)
}

func (m *Manager) serveRequests(values url.Values) pluginapi.ManagementResponse {
	store, cfg, _ := m.snapshot()
	if store == nil {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	// The accepted range is bounded only by the statistics retention; the store
	// clamps the actual reads to the retained request window, so a 7d/30d
	// dashboard selection returns the retained subset instead of a 400.
	query, herr := m.parseQuery(values, statsRange(cfg), false, true)
	if herr != nil {
		return managementError(herr.status, herr.message)
	}
	page, err := store.Requests(query)
	if err != nil {
		return managementStoreError(err)
	}
	return managementJSON(http.StatusOK, page)
}

func (m *Manager) serveBody(values url.Values) pluginapi.ManagementResponse {
	store, _, _ := m.snapshot()
	if store == nil {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	requestID := strings.TrimSpace(values.Get("request_id"))
	if requestID == "" {
		return managementError(http.StatusBadRequest, "request_id is required")
	}
	detail, err := store.Body(requestID)
	if err != nil {
		switch {
		case errors.Is(err, observer.ErrNotFound):
			return managementError(http.StatusNotFound, "body not available")
		case errors.Is(err, observer.ErrClosed):
			return managementError(http.StatusServiceUnavailable, "observer unavailable")
		default:
			return managementError(http.StatusInternalServerError, "internal error")
		}
	}
	return managementJSON(http.StatusOK, detail)
}

func (m *Manager) serveSettings() pluginapi.ManagementResponse {
	_, cfg, hasConfig := m.snapshot()
	if !hasConfig {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	return managementJSON(http.StatusOK, settingsResponse{
		CaptureBodies:           cfg.CaptureBodies,
		BodyRetentionSeconds:    int64(cfg.BodyRetention / time.Second),
		RequestRetentionSeconds: int64(cfg.RequestRetention / time.Second),
		StatsRetentionDays:      cfg.StatsRetentionDays,
		MaxBodyBytes:            cfg.MaxBodyBytes,
		MaxBodyStorageBytes:     cfg.MaxBodyStorageBytes,
	})
}

func (m *Manager) serveHealth() pluginapi.ManagementResponse {
	store, _, _ := m.snapshot()
	if store == nil {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	return managementJSON(http.StatusOK, store.Status())
}

// settingsResponse is the effective capture and retention contract exposed to
// the dashboard. It mirrors the configured observer.Config, not the raw YAML.
type settingsResponse struct {
	CaptureBodies           bool  `json:"capture_bodies"`
	BodyRetentionSeconds    int64 `json:"body_retention_seconds"`
	RequestRetentionSeconds int64 `json:"request_retention_seconds"`
	StatsRetentionDays      int   `json:"stats_retention_days"`
	MaxBodyBytes            int   `json:"max_body_bytes"`
	MaxBodyStorageBytes     int64 `json:"max_body_storage_bytes"`
}

func (m *Manager) serveResource(route string) pluginapi.ManagementResponse {
	name, ok := resourceAssets[route]
	if !ok || m.assets == nil {
		return resourceNotFound()
	}
	body, contentType, ok := m.assets(name)
	if !ok {
		return resourceNotFound()
	}
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	headers.Set("Content-Security-Policy", cspPolicy)
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Cache-Control", "no-store")
	headers.Set("Referrer-Policy", "no-referrer")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}
}

var resourceAssets = map[string]string{
	"/ui":     "ui.html",
	"/ui.js":  "ui.js",
	"/ui.css": "ui.css",
}

// httpError is a management-local HTTP failure with a sanitized message.
type httpError struct {
	status  int
	message string
}

// statsRange returns the maximum accepted query width: the configured
// statistics retention, hard-capped and defaulted.
func statsRange(cfg observer.Config) time.Duration {
	days := cfg.StatsRetentionDays
	if days <= 0 || days > maxStatsRetentionDays {
		days = maxStatsRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// parseQuery builds an observation query from management query parameters.
//
// The range defaults to the last 24h. When align is set (summary) the bounds are
// snapped to whole minutes so the returned Summary.From/To are minute-aligned,
// and the default upper bound is the next minute boundary so the current minute
// is included. Explicit future ranges, empty/inverted ranges and ranges wider
// than the accepted maximum are rejected with 400.
func (m *Manager) parseQuery(values url.Values, maxRange time.Duration, align, withLimit bool) (observer.Query, *httpError) {
	now := m.now()
	upper := nextMinute(now)

	to, err := parseTimeParam(values.Get("to"))
	if err != nil {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "invalid 'to' timestamp"}
	}
	if to.IsZero() {
		to = upper
	}
	if align {
		to = alignUp(to)
	}
	if to.After(upper) {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "'to' must not be in the future"}
	}

	from, err := parseTimeParam(values.Get("from"))
	if err != nil {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "invalid 'from' timestamp"}
	}
	if from.IsZero() {
		from = to.Add(-defaultQueryWindow)
	}
	if align {
		from = from.Truncate(time.Minute)
	}
	if !from.Before(to) {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "'from' must be before 'to'"}
	}
	if maxRange > 0 && to.Sub(from) > maxRange {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "range exceeds the retained window"}
	}

	query := observer.Query{
		From:     from,
		To:       to,
		Provider: strings.TrimSpace(values.Get("provider")),
		Model:    strings.TrimSpace(values.Get("model")),
	}
	if withLimit {
		limit := defaultRequestLimit
		if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
			parsed, errLimit := strconv.Atoi(raw)
			if errLimit != nil || parsed < 1 || parsed > maxRequestLimit {
				return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "limit must be between 1 and 100"}
			}
			limit = parsed
		}
		query.Limit = limit
		query.Cursor = strings.TrimSpace(values.Get("cursor"))
	}
	return query, nil
}

// nextMinute returns the whole-minute boundary strictly after t, so a half-open
// [from, to) query includes the minute that contains t.
func nextMinute(t time.Time) time.Time {
	return t.Truncate(time.Minute).Add(time.Minute)
}

// alignUp rounds t up to a whole minute, leaving an already aligned value
// unchanged.
func alignUp(t time.Time) time.Time {
	if t.Truncate(time.Minute).Equal(t) {
		return t
	}
	return t.Truncate(time.Minute).Add(time.Minute)
}

// managementStoreError maps a store error to a sanitized HTTP status. Only
// client-driven validation failures are 400; storage failures are surfaced as
// 503/500 instead of being masked as bad requests.
func managementStoreError(err error) pluginapi.ManagementResponse {
	switch {
	case errors.Is(err, observer.ErrInvalidCursor), errors.Is(err, observer.ErrInvalidQuery):
		return managementError(http.StatusBadRequest, "invalid request query")
	case errors.Is(err, observer.ErrClosed):
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	default:
		return managementError(http.StatusInternalServerError, "internal error")
	}
}

func parseTimeParam(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func managementJSON(status int, payload any) pluginapi.ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return resourceRaw(http.StatusInternalServerError, "text/plain; charset=utf-8", []byte("internal error"))
	}
	return resourceRaw(status, "application/json; charset=utf-8", body)
}

func managementError(status int, message string) pluginapi.ManagementResponse {
	return managementJSON(status, map[string]string{"error": message})
}

func resourceNotFound() pluginapi.ManagementResponse {
	return resourceRaw(http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found"))
}

// resourceRaw builds a hardened response: same-origin only, no sniffing and no
// caching. Sanitized error bodies never carry store internals.
func resourceRaw(status int, contentType string, body []byte) pluginapi.ManagementResponse {
	headers := http.Header{}
	headers.Set("Content-Type", contentType)
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}
