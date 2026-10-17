//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Compare raw upstream bytes/headers with a separate real CPA baseline rather
// than assuming CPA forwards client bytes unchanged (it maps model and stream
// options). Both hosts use the identical fixture and routing configuration.
func TestInferenceBytesMatchUninstrumentedCPA(t *testing.T) {
	baseline := newHarnessWith(t, hostOptions{PluginDisabled: true})
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	for _, stream := range []bool{false, true} {
		for _, host := range []*harness{baseline, h} {
			secret := created.Secret
			if host == baseline {
				secret = clientKey
			}
			status, _, err := host.rawRequest("POST", "/v1/chat/completions", chatBody(stream), map[string]string{
				"Authorization": "Bearer " + secret, "Content-Type": "application/json", "User-Agent": "observer-acceptance",
			})
			if err != nil || status != 200 {
				t.Fatalf("inference status=%d err=%v", status, err)
			}
		}
		control := baseline.mock.callsSnapshot()
		observed := h.mock.callsSnapshot()
		a, b := control[len(control)-1], observed[len(observed)-1]
		if !bytes.Equal(a.Body, b.Body) {
			t.Fatalf("plugin altered upstream bytes\nbaseline %s\nobserver %s", a.Body, b.Body)
		}
		if !reflect.DeepEqual(a.Header, b.Header) {
			t.Fatalf("plugin altered upstream headers\nbaseline %v\nobserver %v", a.Header, b.Header)
		}
	}
	h.mock.requireClean(t)
	baseline.mock.requireClean(t)
}

// Count-tokens remains authorized, but must not reserve a concurrency slot or
// charge usage even while a generation occupies the Key's only slot.
func TestCountTokensDoesNotAdmitOrCharge(t *testing.T) {
	h := newHarness(t)
	spec := validSpec()
	spec.MaxConcurrent = 1
	created := h.createKey(t, spec)
	gate := h.mock.holdNextRequest()
	t.Cleanup(gate.open)
	first := startInference(context.Background(), h, created.Secret, false)
	awaitMockEntered(t, gate)
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"hello"}]}`)
	status, raw, err := h.rawRequest("POST", "/v1/messages/count_tokens", body, map[string]string{"x-api-key": created.Secret})
	var count struct {
		InputTokens int `json:"input_tokens"`
	}
	if err != nil || status != 200 || json.Unmarshal(raw, &count) != nil || count.InputTokens <= 0 {
		t.Fatalf("count tokens status=%d err=%v response=%s", status, err, raw)
	}
	if keys := h.keys(t); len(keys) != 1 || keys[0].Active != 1 || keys[0].Used != (limitsView{}) {
		t.Fatalf("count changed admission state: %+v", keys)
	}
	if h.mock.callCount() != 1 {
		t.Fatal("local token counting dispatched inference")
	}
	gate.open()
	result := awaitInference(t, first)
	if result.err != nil || result.status != 200 {
		t.Fatal("generation failed")
	}
	h.await(t, "generation recorded without token-count usage", func() bool { return len(h.page(t, "page_size=100").Items) == 1 && h.keys(t)[0].Active == 0 })
	spec.AllowedModels = []string{"other-model"}
	h.api(t, "PUT", "/keys?id="+url.QueryEscape(created.Key.ID), spec, 200, nil)
	status, _, err = h.rawRequest("POST", "/v1/messages/count_tokens", body, map[string]string{"x-api-key": created.Secret})
	if err != nil || status != 403 {
		t.Fatalf("count bypassed model authorization: %d %v", status, err)
	}
	spec.AllowedModels = nil
	for _, expired := range []bool{false, true} {
		spec.Enabled = expired
		if expired {
			past := time.Now().Add(-time.Hour)
			spec.ExpiresAt = &past
		}
		h.api(t, "PUT", "/keys?id="+url.QueryEscape(created.Key.ID), spec, 200, nil)
		status, _, err = h.rawRequest("POST", "/v1/messages/count_tokens", body, map[string]string{"x-api-key": created.Secret})
		if err != nil || (status != 401 && status != 403) {
			t.Fatalf("count bypassed disabled/expiry authorization: %d %v", status, err)
		}
	}
}

// A real c-shared library copied to a new path exercises host replacement and
// storage release/reopen. No stub/invalid library or CPA internals are used.
func TestCleanNativeReplacementPreservesKeys(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	before := acceptanceInferAndAwait(t, h, created.Secret, map[string]struct{}{})
	h.waitForLog(t, "file watcher started", 20*time.Second)
	next := filepath.Join(h.dir, "replacement")
	if err := installPlugin(next); err != nil {
		t.Fatal(err)
	}
	cfg := hostConfig(h.port, h.authDir, next, h.mock.baseURL(), defaultHostOptions())
	if err := os.WriteFile(filepath.Join(h.dir, "config.yaml"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	h.await(t, "host reports replacement registered", func() bool {
		status, raw, err := h.rawRequest("GET", "/v0/management/plugins", nil, map[string]string{"Authorization": "Bearer " + mgmtKey})
		var list struct {
			Plugins []struct {
				ID, Path   string
				Registered bool
			} `json:"plugins"`
		}
		if err != nil || status != 200 || json.Unmarshal(raw, &list) != nil {
			return false
		}
		for _, p := range list.Plugins {
			if p.ID == pluginID && p.Registered && strings.HasPrefix(p.Path, next+string(os.PathSeparator)) {
				return true
			}
		}
		return false
	})
	acceptanceRecordByID(t, h, before.ID)
	if status, _ := h.infer(t, created.Secret, false); status != http.StatusOK {
		t.Fatal("replacement lost key/store")
	}
	denyInference(t, h, clientKey)
}
