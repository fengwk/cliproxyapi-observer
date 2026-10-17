//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"testing"
)

// Host management discovery wire shapes. The test deliberately reads the real
// CPA responses instead of importing plugin or host packages.
type modelsAuthFilesResponse struct {
	Files []struct {
		Name string `json:"name"`
	} `json:"files"`
}

type modelsAuthFileModelsResponse struct {
	Models []struct {
		ID string `json:"id"`
	} `json:"models"`
}

// modelsListAuthFiles asks the actual host for its management auth files. The
// harness configures the local models as runtime compatibility credentials.
func modelsListAuthFiles(h *harness) ([]string, error) {
	status, raw, err := h.rawRequest(http.MethodGet, "/v0/management/auth-files", nil, map[string]string{
		"Authorization": "Bearer " + mgmtKey,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("auth-files status %d: %s", status, truncate(raw, 300))
	}
	var listing modelsAuthFilesResponse
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, fmt.Errorf("auth-files JSON: %w", err)
	}
	names := make([]string, 0, len(listing.Files))
	for _, file := range listing.Files {
		if file.Name != "" {
			names = append(names, file.Name)
		}
	}
	return names, nil
}

// modelsListFileIDs reads the model IDs the host registered for one auth file.
func modelsListFileIDs(h *harness, name string) ([]string, error) {
	path := "/v0/management/auth-files/models?name=" + url.QueryEscape(name)
	status, raw, err := h.rawRequest(http.MethodGet, path, nil, map[string]string{
		"Authorization": "Bearer " + mgmtKey,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("auth-files/models status %d: %s", status, truncate(raw, 300))
	}
	var listing modelsAuthFileModelsResponse
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, fmt.Errorf("auth-files/models JSON: %w", err)
	}
	ids := make([]string, 0, len(listing.Models))
	for _, model := range listing.Models {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids, nil
}

// modelsDiscoverLocalModels uses the actual host management endpoints and polls
// until the runtime model registration is visible. The harness configures
// exactly two local models, so both must be returned and nothing else.
func modelsDiscoverLocalModels(t *testing.T, h *harness) []string {
	t.Helper()

	var ids []string
	// Host registers runtime credentials asynchronously. Poll the real listing,
	// never synthesize a discovery-only credential to hide an empty result.
	h.await(t, "host auth-files lists runtime credentials and auth-files/models exposes both local models", func() bool {
		names, err := modelsListAuthFiles(h)
		if err != nil {
			t.Fatalf("host auth-files discovery: %v", err)
		}
		if len(names) == 0 {
			return false
		}
		set := make(map[string]struct{})
		for _, name := range names {
			got, err := modelsListFileIDs(h, name)
			if err != nil {
				return false
			}
			for _, id := range got {
				set[id] = struct{}{}
			}
		}
		ids = ids[:0]
		for id := range set {
			ids = append(ids, id)
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

// TestLocalModelDiscovery proves the real host management auth-files surface
// lists the two local runtime models. It intentionally fails when the host does
// not expose configured runtime credentials instead of fabricating a
// discovery-only credential, so the cross-layer gap stays visible.
func TestLocalModelDiscovery(t *testing.T) {
	h := newHarness(t)
	discovered := modelsDiscoverLocalModels(t, h)
	if !slices.Equal(discovered, []string{modelName, modelNameTwo}) {
		t.Fatalf("discovered models = %v, want [%s %s]", discovered, modelName, modelNameTwo)
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
