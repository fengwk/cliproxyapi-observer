package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"

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
	validateRoute = routesPrefix + "/validate"

	// resourcePrefix is the public, secret-free browser resource prefix.
	resourcePrefix = "/resource/plugins/" + PluginID

	defaultQueryWindow  = 24 * time.Hour
	defaultRequestLimit = 50
	maxRequestLimit     = 100

	// maxStatsRetentionDays hard-caps the accepted query range regardless of the
	// configured statistics retention.
	maxStatsRetentionDays = 3650

	// maxValidateBodyBytes bounds the raw JSON patch accepted by /validate.
	maxValidateBodyBytes = 256 * 1024

	// cspPolicy keeps the embedded page same-origin frameable without widening
	// any directive. The UI slice must not rely on inline scripts or styles.
	cspPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

// managementRegistrationResponse mirrors the host's expected registration shape.
type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []pluginapi.ResourceRoute   `json:"resources"`
}

// managementRegistration declares the read-only data routes, the validate route,
// and the public resource menu.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: summaryRoute, Description: "Aggregated observation summary for the selected period."},
			{Method: http.MethodGet, Path: requestsRoute, Description: "Offset-paginated observed request metadata."},
			{Method: http.MethodGet, Path: bodyRoute, Description: "One captured request body by request_id."},
			{Method: http.MethodGet, Path: settingsRoute, Description: "Effective capture and retention settings."},
			{Method: http.MethodGet, Path: healthRoute, Description: "Writer and drop counters."},
			{Method: http.MethodPost, Path: validateRoute, Description: "Validate candidate configuration patch."},
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
	if route, ok := managementRoute(req.Path); ok {
		if route == validateRoute {
			if req.Method != http.MethodPost {
				return okEnvelope(managementJSON(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
			}
			return okEnvelope(m.serveValidate(req.Body))
		}
		if req.Method != http.MethodGet {
			return okEnvelope(managementJSON(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
		}
		return okEnvelope(m.serveManagement(route, req.Query))
	}
	if route, ok := resourceRoute(req.Path); ok {
		if req.Method != http.MethodGet {
			return okEnvelope(managementJSON(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
		}
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
	prices := cfg.Prices
	if prices == nil {
		prices = map[string]observer.Price{}
	}
	return managementJSON(http.StatusOK, settingsResponse{
		CaptureBodies:           cfg.CaptureBodies,
		BodyRetentionSeconds:    int64(cfg.BodyRetention / time.Second),
		RequestRetentionSeconds: int64(cfg.RequestRetention / time.Second),
		StatsRetentionDays:      cfg.StatsRetentionDays,
		MaxBodyBytes:            cfg.MaxBodyBytes,
		MaxBodyStorageBytes:     cfg.MaxBodyStorageBytes,
		CompactIntervalSeconds:  int64(cfg.CompactInterval / time.Second),
		CompactMinBytes:         cfg.CompactMinBytes,
		Prices:                  prices,
	})
}

func (m *Manager) serveHealth() pluginapi.ManagementResponse {
	store, _, _ := m.snapshot()
	if store == nil {
		return managementError(http.StatusServiceUnavailable, "observer unavailable")
	}
	return managementJSON(http.StatusOK, store.Status())
}

var (
	errInvalidSettingsPatch = errors.New("invalid settings patch")
	errObserverUnavailable  = errors.New("observer unavailable")
)

const (
	validateErrorMessage       = "invalid settings patch"
	observerUnavailableMessage = "observer unavailable"
	payloadTooLargeMessage     = "request body exceeds 256KiB limit"
)

var allowedValidateKeys = map[string]bool{
	"capture-bodies":         true,
	"request-retention":      true,
	"body-retention":         true,
	"stats-retention-days":   true,
	"max-body-bytes":         true,
	"max-body-storage-bytes": true,
	"compact-interval":       true,
	"compact-min-bytes":      true,
	"prices":                 true,
}

func parseJSONInteger(raw []byte, min, max int64) (int64, error) {
	if len(raw) == 0 || raw[0] == '"' || bytes.Contains(raw, []byte(".")) {
		return 0, errInvalidSettingsPatch
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&num); err != nil {
		return 0, errInvalidSettingsPatch
	}
	val, err := num.Int64()
	if err != nil || val < min || val > max {
		return 0, errInvalidSettingsPatch
	}
	return val, nil
}

func parseJSONDuration(raw []byte, min, max time.Duration) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", errInvalidSettingsPatch
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errInvalidSettingsPatch
	}
	s = strings.TrimSpace(s)
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d < min || d > max {
		return "", errInvalidSettingsPatch
	}
	return s, nil
}

func validatePatch(rawCopy []byte, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return errInvalidSettingsPatch
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return errInvalidSettingsPatch
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errInvalidSettingsPatch
	}

	patchMap := make(map[string]any)
	seenKeys := make(map[string]bool)

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return errInvalidSettingsPatch
		}
		key, ok := keyTok.(string)
		if !ok || !allowedValidateKeys[key] || seenKeys[key] {
			return errInvalidSettingsPatch
		}
		seenKeys[key] = true

		var rawVal json.RawMessage
		if err := dec.Decode(&rawVal); err != nil {
			return errInvalidSettingsPatch
		}
		rawTrimmed := bytes.TrimSpace(rawVal)
		if len(rawTrimmed) == 0 || bytes.Equal(rawTrimmed, []byte("null")) || bytes.HasPrefix(rawTrimmed, []byte("[")) {
			return errInvalidSettingsPatch
		}

		switch key {
		case "capture-bodies":
			if !bytes.Equal(rawTrimmed, []byte("true")) && !bytes.Equal(rawTrimmed, []byte("false")) {
				return errInvalidSettingsPatch
			}
			patchMap[key] = bytes.Equal(rawTrimmed, []byte("true"))

		case "request-retention":
			s, err := parseJSONDuration(rawTrimmed, observer.MinRequestRetention, observer.MaxRequestRetention)
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = s

		case "body-retention":
			s, err := parseJSONDuration(rawTrimmed, observer.MinBodyRetention, observer.MaxBodyRetention)
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = s

		case "compact-interval":
			s, err := parseJSONDuration(rawTrimmed, observer.MinCompactInterval, observer.MaxCompactInterval)
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = s

		case "stats-retention-days":
			val, err := parseJSONInteger(rawTrimmed, 1, int64(observer.MaxStatsRetentionDays))
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = int(val)

		case "max-body-bytes":
			val, err := parseJSONInteger(rawTrimmed, 1, int64(observer.MaxBodyBytesLimit))
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = int(val)

		case "max-body-storage-bytes":
			val, err := parseJSONInteger(rawTrimmed, 1, observer.MaxBodyStorageLimit)
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = val

		case "compact-min-bytes":
			val, err := parseJSONInteger(rawTrimmed, observer.MinCompactMinBytes, observer.MaxCompactMinBytes)
			if err != nil {
				return errInvalidSettingsPatch
			}
			patchMap[key] = val

		case "prices":
			pricesDec := json.NewDecoder(bytes.NewReader(rawTrimmed))
			pTok, err := pricesDec.Token()
			if err != nil {
				return errInvalidSettingsPatch
			}
			pDelim, ok := pTok.(json.Delim)
			if !ok || pDelim != '{' {
				return errInvalidSettingsPatch
			}

			pricesMap := make(map[string]observer.Price)
			seenModels := make(map[string]bool)

			for pricesDec.More() {
				mTok, err := pricesDec.Token()
				if err != nil {
					return errInvalidSettingsPatch
				}
				modelStr, ok := mTok.(string)
				if !ok {
					return errInvalidSettingsPatch
				}
				trimmedModel := strings.TrimSpace(modelStr)
				if trimmedModel == "" || seenModels[trimmedModel] {
					return errInvalidSettingsPatch
				}
				seenModels[trimmedModel] = true

				var pRaw json.RawMessage
				if err := pricesDec.Decode(&pRaw); err != nil {
					return errInvalidSettingsPatch
				}
				pRawTrimmed := bytes.TrimSpace(pRaw)
				if len(pRawTrimmed) == 0 || bytes.Equal(pRawTrimmed, []byte("null")) || bytes.HasPrefix(pRawTrimmed, []byte("[")) {
					return errInvalidSettingsPatch
				}

				var price observer.Price
				if err := json.Unmarshal(pRawTrimmed, &price); err != nil {
					return errInvalidSettingsPatch
				}
				pricesMap[trimmedModel] = price
			}

			pTok, err = pricesDec.Token()
			if err != nil || pTok != json.Delim('}') || pricesDec.More() {
				return errInvalidSettingsPatch
			}
			var pExtra json.RawMessage
			if err := pricesDec.Decode(&pExtra); err != io.EOF {
				return errInvalidSettingsPatch
			}

			patchMap["prices"] = pricesMap
		}
	}

	endTok, err := dec.Token()
	if err != nil || endTok != json.Delim('}') || dec.More() {
		return errInvalidSettingsPatch
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errInvalidSettingsPatch
	}

	var baseMap map[string]any
	if len(bytes.TrimSpace(rawCopy)) > 0 {
		if err := yaml.Unmarshal(rawCopy, &baseMap); err != nil {
			return errObserverUnavailable
		}
	}
	if baseMap == nil {
		baseMap = make(map[string]any)
	}

	for k, v := range patchMap {
		baseMap[k] = v
	}

	mergedYAML, err := yaml.Marshal(baseMap)
	if err != nil {
		return errObserverUnavailable
	}

	if _, err := observer.ParseConfig(mergedYAML); err != nil {
		return errInvalidSettingsPatch
	}

	return nil
}

func (m *Manager) serveValidate(body []byte) pluginapi.ManagementResponse {
	if len(body) > maxValidateBodyBytes {
		return managementError(http.StatusRequestEntityTooLarge, payloadTooLargeMessage)
	}

	m.mu.RLock()
	hasConfig := m.hasConfig
	var rawCopy []byte
	if hasConfig && len(m.raw) > 0 {
		rawCopy = append([]byte(nil), m.raw...)
	}
	m.mu.RUnlock()

	if !hasConfig {
		return managementError(http.StatusServiceUnavailable, observerUnavailableMessage)
	}

	if err := validatePatch(rawCopy, body); err != nil {
		if errors.Is(err, errObserverUnavailable) {
			return managementError(http.StatusServiceUnavailable, observerUnavailableMessage)
		}
		return managementError(http.StatusBadRequest, validateErrorMessage)
	}

	return managementJSON(http.StatusOK, map[string]bool{"valid": true})
}

// settingsResponse is the effective capture and retention contract exposed to
// the dashboard. It mirrors the configured observer.Config, not the raw YAML.
type settingsResponse struct {
	CaptureBodies           bool                      `json:"capture_bodies"`
	BodyRetentionSeconds    int64                     `json:"body_retention_seconds"`
	RequestRetentionSeconds int64                     `json:"request_retention_seconds"`
	StatsRetentionDays      int                       `json:"stats_retention_days"`
	MaxBodyBytes            int                       `json:"max_body_bytes"`
	MaxBodyStorageBytes     int64                     `json:"max_body_storage_bytes"`
	CompactIntervalSeconds  int64                     `json:"compact_interval_seconds"`
	CompactMinBytes         int64                     `json:"compact_min_bytes"`
	Prices                  map[string]observer.Price `json:"prices"`
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
// is included. Validate the requested interval before bucket alignment so
// rounding neither rejects a valid maximum-width range nor repairs invalid bounds.
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
	if !from.Before(to) {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "'from' must be before 'to'"}
	}
	if maxRange > 0 && to.Sub(from) > maxRange {
		return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "range exceeds the retained window"}
	}
	if align {
		from = from.Truncate(time.Minute)
		to = alignUp(to)
	}

	query := observer.Query{
		From:     from,
		To:       to,
		Provider: strings.TrimSpace(values.Get("provider")),
		Model:    strings.TrimSpace(values.Get("model")),
	}
	if withLimit {
		if rawCursor := strings.TrimSpace(values.Get("cursor")); rawCursor != "" {
			return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "legacy cursor is not supported"}
		}
		limit := defaultRequestLimit
		if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
			parsed, errLimit := strconv.Atoi(raw)
			if errLimit != nil || parsed < 1 || parsed > maxRequestLimit {
				return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "limit must be between 1 and 100"}
			}
			limit = parsed
		}
		query.Limit = limit

		offset := 0
		if values.Has("offset") {
			rawOffset := values.Get("offset")
			if rawOffset == "" {
				return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "offset must be a non-negative integer"}
			}
			for _, ch := range rawOffset {
				if ch < '0' || ch > '9' {
					return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "offset must be a non-negative integer"}
				}
			}
			parsedOffset, errOffset := strconv.ParseInt(rawOffset, 10, 64)
			if errOffset != nil || parsedOffset < 0 || parsedOffset > math.MaxInt32 {
				return observer.Query{}, &httpError{status: http.StatusBadRequest, message: "offset must be between 0 and 2147483647"}
			}
			offset = int(parsedOffset)
		}
		query.Offset = offset
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
	case errors.Is(err, observer.ErrInvalidQuery):
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
