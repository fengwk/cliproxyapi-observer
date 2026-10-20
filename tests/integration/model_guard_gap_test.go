//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// This is a documented SDK-gap reproduction, NOT a test proving a repaired
// all-route gateway. The native Realtime credential route bypasses request.before
// and can issue a local ephemeral token with zero Observer Keys. No real upstream
// is contacted; execution-chain gaps are also traced in docs/model-policy.md.
func TestSDKRealtimeTokenRouteBypassesModelGuard(t *testing.T) {
	h := newHarness(t)
	status, body, err := h.rawRequest(http.MethodPost, "/v1/realtime/client_secrets",
		[]byte(`{"session":{"type":"realtime","model":"gpt-realtime"}}`),
		map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Value string `json:"value"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &issued) != nil || !strings.HasPrefix(issued.Value, "ek_") {
		t.Fatalf("SDK gap behavior changed: status=%d; recheck route-level protection", status)
	}
	if len(h.keys(t)) != 0 || len(h.page(t, "page_size=100").Items) != 0 || h.mock.callCount() != 0 {
		t.Fatal("SDK gap probe must use zero Observer keys and no upstream execution")
	}
}
