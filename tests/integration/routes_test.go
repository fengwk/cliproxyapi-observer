//go:build integration

package integration

import (
	"net/url"
	"testing"
)

// Query IDs must be present exactly once, nonblank and accompanied by no
// unrecognized parameters. Invalid mutations must not rotate/revoke the Key.
func TestStaticRouteIDValidation(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/keys", validSpec()},
		{"DELETE", "/keys", nil},
		{"POST", "/keys/rotate", nil},
		{"POST", "/keys/reset", map[string]any{"windows": []string{"total"}}},
		{"POST", "/keys/recover", map[string]any{"acknowledge": true}},
		{"GET", "/requests/detail", nil},
	} {
		for _, query := range []string{"", "?id=", "?id=%20", "?id=" + url.QueryEscape(created.Key.ID) + "&id=" + url.QueryEscape(created.Key.ID), "?id=" + url.QueryEscape(created.Key.ID) + "&extra=1"} {
			h.api(t, route.method, route.path+query, route.body, 400, nil)
		}
	}
	h.api(t, "PUT", "/keys?id=unknown", validSpec(), 404, nil)
	h.api(t, "GET", "/requests/detail?id=unknown", nil, 404, nil)
	if status, _ := h.infer(t, created.Secret, false); status != 200 {
		t.Fatal("invalid ID query mutated the key")
	}
}
