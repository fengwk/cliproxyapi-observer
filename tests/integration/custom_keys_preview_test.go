//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"testing"
)

// Exercise custom credentials through the actual host/native ABI, including
// invalid rotation rollback, stable accounting and schema2 process restart.
func TestCustomKeysAndPreviewThroughNativeHost(t *testing.T) {
	h := newHarness(t)
	secret, next := "local-custom-key-exact-123456!", "local-replacement-key-exact-123456!"
	input := struct {
		keySpec
		Secret string `json:"secret"`
	}{keySpec: validSpec(), Secret: secret}
	var created createdKey
	h.api(t, "POST", "/keys", input, 201, &created)
	if created.Secret != secret {
		t.Fatal("custom credential was normalized")
	}
	acceptancePutPricing(t, h, acceptancePricing{Prices: []acceptancePrice{
		{Model: modelName, Rates: acceptanceRatesAll(1_000_000)},
		{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000)},
	}, Rules: []acceptanceRule{}})
	acceptanceInferAndAwait(t, h, secret, map[string]struct{}{})
	before := browserSnapshot(t, h)
	query := "?id=" + url.QueryEscape(created.Key.ID)
	h.api(t, "POST", "/keys", input, 409, nil)
	h.api(t, "POST", "/keys/rotate"+query, map[string]any{"secret": secret}, 400, nil)
	for _, invalid := range []any{"", "short", " padded-custom-key", nil, 123} {
		h.api(t, "POST", "/keys/rotate"+query, map[string]any{"secret": invalid}, 400, nil)
	}
	for _, invalid := range []any{"", " padded-custom-key", nil} {
		h.api(t, "POST", "/keys", map[string]any{"enabled": true, "secret": invalid}, 400, nil)
	}
	browserAssertManagementUnchanged(t, h, before)
	preview := map[string]any{"models": []map[string]string{{"model": modelName, "provider": ""}, {"model": "unpriced-local"}}}
	var prices struct {
		Items []struct{ Model, Source string } `json:"items"`
	}
	h.api(t, "POST", "/prices/preview", preview, 200, &prices)
	if len(prices.Items) != 2 || prices.Items[0].Source != "manual" || prices.Items[1].Source != "unpriced" {
		t.Fatal("native preview source is misleading", prices)
	}
	browserAssertManagementUnchanged(t, h, before)
	status, _, err := h.rawRequest("POST", "/v0/management/plugins/"+pluginID+"/prices/preview",
		[]byte(`{"models":[]}`), nil)
	if err != nil || status != 401 {
		t.Fatalf("public preview status=%d err=%v", status, err)
	}
	var rotated createdKey
	h.api(t, "POST", "/keys/rotate"+query, map[string]any{"secret": next}, 200, &rotated)
	if rotated.Secret != next || created.Key.ID != rotated.Key.ID {
		t.Fatal("rotation changed identity")
	}
	denyInference(t, h, secret)
	// Rotation intentionally replaces the current display material. Every other
	// management field, including fixed accounting, must remain unchanged.
	for _, item := range before["keys"].(map[string]any)["items"].([]any) {
		key := item.(map[string]any)
		if key["id"] == created.Key.ID {
			if key["secret"] != secret {
				t.Fatal("pre-rotation list did not expose current material")
			}
			key["secret"] = next
		}
	}
	browserAssertManagementUnchanged(t, h, before)
	if current := listKeyMaterials(t, h, "/keys")[created.Key.ID]; current.Secret != next {
		t.Fatal("management list did not expose the replacement material")
	}
	for _, name := range []string{"/requests", "/summary", "/health"} {
		raw := h.api(t, "GET", name, nil, 200, nil)
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(next)) {
			t.Fatal("secret exposed by read API")
		}
	}
	h.awaitGracefulRestartReady(t, 1)
	h.Stop()
	raw, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte(h.stdout.String() + h.stderr.String())
	if !bytes.Contains(raw, []byte(next)) {
		t.Fatal("current plaintext not saved for management")
	}
	for _, material := range []string{secret, next} {
		if bytes.Contains(logs, []byte(material)) {
			t.Fatal("client plaintext leaked to logs")
		}
	}
	h, err = h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := h.infer(t, next, false); status != 200 {
		t.Fatal("custom authentication lost on restart")
	}
	if current := listKeyMaterials(t, h, "/keys")[created.Key.ID]; current.Secret != next {
		t.Fatal("current plaintext unavailable after restart")
	}
	var snapshot map[string]json.RawMessage
	h.api(t, "GET", "/prices", nil, 200, &snapshot)
	if snapshot["catalog_entries"] == nil {
		t.Fatal("catalog entry count missing")
	}
}
