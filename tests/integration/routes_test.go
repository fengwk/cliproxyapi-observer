//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Verify canonical UI assets remain reachable and the real host reports no
// normalized-route collisions at startup or after native replacement.
func assertUIRoutesWithoutConflicts(t *testing.T, h *harness) {
	t.Helper()
	for _, prefix := range []string{"/v0/management/plugins/", "/v0/resource/plugins/"} {
		var headers map[string]string
		if prefix == "/v0/management/plugins/" {
			headers = map[string]string{"Authorization": "Bearer " + mgmtKey}
		}
		for _, suffix := range []string{"/ui", "/ui.js", "/ui.css", "/theme.js"} {
			path := prefix + pluginID + suffix
			status, responseHeaders, body, err := h.rawRequestWithHeaders(http.MethodGet, path, nil, headers)
			if err != nil || status != http.StatusOK || len(body) == 0 {
				t.Fatalf("UI asset %s status=%d err=%v bytes=%d", path, status, err, len(body))
			}
			if responseHeaders.Get("Cache-Control") != "no-store" {
				t.Errorf("UI asset %s cache policy=%q", path, responseHeaders.Get("Cache-Control"))
			}
		}
	}
	for _, line := range strings.Split(h.stdout.String()+"\n"+h.stderr.String(), "\n") {
		if strings.Contains(line, "pluginhost: plugin "+pluginID+" ") &&
			strings.Contains(line, " route ") && strings.Contains(line, " conflicts with ") {
			t.Errorf("Observer route conflict: %s", line)
		}
	}
}

// Query IDs must be present exactly once, nonblank and accompanied by no
// unrecognized parameters. Invalid mutations must not rotate/revoke the Key.
// The removed recovery route is gone and therefore owns no ID contract.
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
