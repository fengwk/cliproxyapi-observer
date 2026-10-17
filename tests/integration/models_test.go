//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"testing"
	"time"
)

type catalogCapability struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func issueCatalogToken(t *testing.T, h *harness) catalogCapability {
	t.Helper()
	var token catalogCapability
	before := time.Now()
	h.api(t, "POST", "/models/token", nil, 200, &token)
	if token.Token == "" || !token.ExpiresAt.After(before) || token.ExpiresAt.After(time.Now().Add(31*time.Second)) {
		t.Fatal("invalid catalog capability lifetime")
	}
	return token
}

// The management nonce authenticates the actual host catalog, not a copied
// pricing/config catalog. Refresh a single-use nonce on each bounded poll.
func modelsDiscoverLocalModels(t *testing.T, h *harness) []string {
	t.Helper()
	var ids []string
	h.await(t, "catalog capability exposes both local models", func() bool {
		token := issueCatalogToken(t, h)
		status, raw, err := h.rawRequest("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + token.Token})
		if err != nil || status != 200 {
			t.Fatalf("catalog status=%d error=%v body=%s", status, err, raw)
		}
		var catalog struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &catalog); err != nil {
			t.Fatal(err)
		}
		ids = nil
		for _, model := range catalog.Data {
			ids = append(ids, model.ID)
		}
		sort.Strings(ids)
		return slices.Contains(ids, modelName) && slices.Contains(ids, modelNameTwo)
	})

	if len(ids) != 2 || !slices.Contains(ids, modelName) || !slices.Contains(ids, modelNameTwo) {
		t.Fatalf("host discovery returned models %v, want exactly %q and %q", ids, modelName, modelNameTwo)
	}
	return ids
}

// modelsChatBody builds an inference request for one configured client model.
func modelsChatBody(model string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"stream":false}`, model, upstreamPrompt))
}

func modelsInfer(t *testing.T, h *harness, secret, model string) (int, []byte) {
	t.Helper()
	status, body, err := h.rawRequest(http.MethodPost, "/v1/chat/completions", modelsChatBody(model), map[string]string{
		"Authorization": "Bearer " + secret,
	})
	if err != nil {
		t.Fatalf("inference for %q: %v", model, err)
	}
	return status, body
}

// modelsAllow proves the model is admitted and reaches the local mock.
func modelsAllow(t *testing.T, h *harness, secret, model string) {
	t.Helper()
	before := h.mock.callCount()
	status, body := modelsInfer(t, h, secret, model)
	if status != http.StatusOK || jsonContent(body) != fixtureText {
		t.Fatalf("model %q status=%d body=%s, want an upstream success", model, status, truncate(body, 300))
	}
	if got := h.mock.callCount(); got != before+1 {
		t.Fatalf("model %q dispatched %d upstream attempts, want exactly one", model, got-before)
	}
}

// modelsDeny proves the model is rejected before any upstream dispatch.
func modelsDeny(t *testing.T, h *harness, secret, model string) {
	t.Helper()
	before := h.mock.callCount()
	status, body := modelsInfer(t, h, secret, model)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("model %q status=%d body=%s, want an authorization rejection", model, status, truncate(body, 300))
	}
	if got := h.mock.callCount(); got != before {
		t.Fatalf("rejected model %q reached the upstream (%d new attempts)", model, got-before)
	}
}

func modelsKeyByID(t *testing.T, h *harness, id string) keyView {
	t.Helper()
	for _, key := range h.keys(t) {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %q is missing from the management listing", id)
	return keyView{}
}

// Catalog access is a one-shot, read-only capability, never a frontend bypass
// for management/builtin keys and never usable for generation or token counting.
func TestLocalModelDiscovery(t *testing.T) {
	h := newHarness(t)
	discovered := modelsDiscoverLocalModels(t, h)
	if !slices.Equal(discovered, []string{modelName, modelNameTwo}) {
		t.Fatalf("discovered models = %v, want [%s %s]", discovered, modelName, modelNameTwo)
	}
	for _, secret := range []string{clientKey, mgmtKey} {
		status, _, err := h.rawRequest("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + secret})
		if err != nil || (status != 401 && status != 403) {
			t.Fatalf("catalog bypass status=%d err=%v", status, err)
		}
	}
	token := issueCatalogToken(t, h)
	for i := 0; i < 2; i++ {
		status, _, err := h.rawRequest("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + token.Token})
		if err != nil || (i == 0 && status != 200) || (i == 1 && status != 401 && status != 403) {
			t.Fatalf("single-use catalog attempt %d status=%d err=%v", i, status, err)
		}
	}
	tokens := []string{token.Token}
	before := h.mock.callCount()
	for _, path := range []string{"/v1/chat/completions", "/v1/messages/count_tokens"} {
		fresh := issueCatalogToken(t, h)
		tokens = append(tokens, fresh.Token)
		status, _, err := h.rawRequest("POST", path, chatBody(false), map[string]string{"Authorization": "Bearer " + fresh.Token})
		if err != nil || (status != 401 && status != 403) {
			t.Fatalf("catalog token escaped read-only scope: %s status=%d err=%v", path, status, err)
		}
	}
	if h.mock.callCount() != before {
		t.Fatal("catalog token forwarded inference")
	}
	for _, path := range []string{"/keys", "/health", "/requests", "/settings"} {
		raw := h.api(t, "GET", path, nil, 200, nil)
		for _, secret := range tokens {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("catalog token persisted in metadata")
			}
		}
	}
	h.Stop()
	db, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte(h.stdout.String() + h.stderr.String())
	for _, secret := range tokens {
		if bytes.Contains(db, []byte(secret)) || bytes.Contains(logs, []byte(secret)) {
			t.Fatal("catalog token persisted in DB/logs")
		}
	}
}

// TestGeneratedKeyModelSelectionsPersist uses the known local model constants so
// Key permission behavior is tested independently of discovery availability.
// Empty allowed_models permits both models; exact selections are enforced
// without dispatch, updates swap the selection, and a restart preserves it.
func TestGeneratedKeyModelSelectionsPersist(t *testing.T) {
	h := newHarness(t)

	created := h.createKey(t, validSpec())
	modelsAllow(t, h, created.Secret, modelName)
	modelsAllow(t, h, created.Secret, modelNameTwo)
	h.mock.requireClean(t)

	spec := validSpec()
	spec.AllowedModels = []string{modelName}
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(created.Key.ID), spec, http.StatusOK, nil)
	if got := modelsKeyByID(t, h, created.Key.ID).AllowedModels; !slices.Equal(got, []string{modelName}) {
		t.Fatalf("saved allowed models = %v, want [%s]", got, modelName)
	}
	modelsAllow(t, h, created.Secret, modelName)
	modelsDeny(t, h, created.Secret, modelNameTwo)

	spec.AllowedModels = []string{modelNameTwo}
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(created.Key.ID), spec, http.StatusOK, nil)
	if got := modelsKeyByID(t, h, created.Key.ID).AllowedModels; !slices.Equal(got, []string{modelNameTwo}) {
		t.Fatalf("swapped allowed models = %v, want [%s]", got, modelNameTwo)
	}
	modelsDeny(t, h, created.Secret, modelName)
	modelsAllow(t, h, created.Secret, modelNameTwo)

	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatalf("restart host: %v", err)
	}
	if got := modelsKeyByID(t, restarted, created.Key.ID).AllowedModels; !slices.Equal(got, []string{modelNameTwo}) {
		t.Fatalf("post-restart allowed models = %v, want [%s]", got, modelNameTwo)
	}
	modelsDeny(t, restarted, created.Secret, modelName)
	modelsAllow(t, restarted, created.Secret, modelNameTwo)
	restarted.mock.requireClean(t)
}
