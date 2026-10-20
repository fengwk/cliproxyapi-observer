package plugin

import (
	"net/http"
	"net/url"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (m *Manager) serveRequestView(values url.Values) pluginapi.ManagementResponse {
	// Date-only ranges belong to normalizeView, including the inclusive end.
	// Do not mix them with the legacy RFC3339 half-open parameters.
	copy := url.Values{}
	for key, list := range values {
		if len(list) != 1 {
			return invalidQueryResponse()
		}
		switch key {
		case "from_date", "to_date", "view":
		case "from", "to":
			return invalidQueryResponse()
		default:
			copy[key] = list
		}
	}
	query, err := parseViewFilters(copy)
	if err != nil {
		return invalidQueryResponse()
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		view, err := store.RequestView(engine.ViewQuery{Query: query, FromDate: values.Get("from_date"), ToDate: values.Get("to_date"), View: values.Get("view")})
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, view)
	})
}

func parseViewFilters(values url.Values) (engine.Query, error) {
	for key := range values {
		switch key {
		case "key_id", "model", "provider", "auth_index", "status", "page", "page_size":
		default:
			return engine.Query{}, errJSONInvalid
		}
	}
	status := values.Get("status")
	if status != "" && status != "success" && status != "failed" {
		return engine.Query{}, errJSONInvalid
	}
	page, err := parsePageNumber(values.Get("page"), 1, 0)
	if err != nil {
		return engine.Query{}, err
	}
	size, err := parsePageNumber(values.Get("page_size"), 25, 100)
	if err != nil {
		return engine.Query{}, err
	}
	return engine.Query{KeyID: values.Get("key_id"), Model: values.Get("model"), Provider: values.Get("provider"), AuthIndex: values.Get("auth_index"), Status: status, Page: page, PageSize: size}, nil
}

func (m *Manager) serveRequestFacets(values url.Values) pluginapi.ManagementResponse {
	for key, list := range values {
		if len(list) != 1 || (key != "field" && key != "search" && key != "cursor" && key != "limit") {
			return invalidQueryResponse()
		}
	}
	limit, err := parsePageNumber(values.Get("limit"), 25, 100)
	if err != nil || len(values.Get("search")) > 1024 || len(values.Get("cursor")) > 4096 {
		return invalidQueryResponse()
	}
	return m.withStore(func(store *engine.Store) pluginapi.ManagementResponse {
		page, err := store.Facets(engine.FacetQuery{Field: values.Get("field"), Search: values.Get("search"), Cursor: values.Get("cursor"), Limit: limit})
		if err != nil {
			return storeFailureResponse(err)
		}
		return jsonResponse(http.StatusOK, page)
	})
}
