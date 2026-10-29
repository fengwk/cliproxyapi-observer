//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Wire types deliberately do not import the unfinished engine implementation.
type limitsView struct {
	FiveHour int64 `json:"five_hour"`
	Week     int64 `json:"week"`
	Month    int64 `json:"month"`
	Total    int64 `json:"total"`
}
type keySpec struct {
	Label         string     `json:"label"`
	Enabled       bool       `json:"enabled"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxConcurrent int        `json:"max_concurrent"`
	AllowedModels []string   `json:"allowed_models"`
	Limits        limitsView `json:"limits"`
}
type keyView struct {
	keySpec
	ID     string     `json:"id"`
	Used   limitsView `json:"used"`
	Active int        `json:"active"`
}
type createdKey struct {
	Key    keyView `json:"key"`
	Secret string  `json:"secret"`
}
type tokensView struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}
type recordView struct {
	Time                time.Time  `json:"time"`
	ID                  string     `json:"id"`
	TraceID             string     `json:"trace_id"`
	KeyID               string     `json:"key_id"`
	Model               string     `json:"model"`
	Provider            string     `json:"provider"`
	AuthIndex           string     `json:"auth_index"`
	AccountName         string     `json:"account_name,omitempty"`
	PriceSource         string     `json:"price_source"`
	ResponseServiceTier string     `json:"response_service_tier,omitempty"`
	CostMicroUSD        int64      `json:"cost_micro_usd"`
	Failed              bool       `json:"failed"`
	Stream              bool       `json:"stream"`
	Tokens              tokensView `json:"tokens"`
}
type requestPage struct {
	Items    []recordView `json:"items"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
	HasMore  bool         `json:"has_more"`
}

func validSpec() keySpec {
	return keySpec{Label: "local acceptance", Enabled: true, AllowedModels: []string{}}
}
func (h *harness) api(t *testing.T, method, path string, input any, want int, out any) []byte {
	t.Helper()
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	status, headers, data, err := h.rawRequestWithHeaders(method, "/v0/management/plugins/"+pluginID+path, body, map[string]string{"Authorization": "Bearer " + mgmtKey})
	if err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("%s %s status %d want %d: %s", method, path, status, want, truncate(data, 500))
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Errorf("%s cache policy = %q", path, headers.Get("Cache-Control"))
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s JSON: %v", path, err)
		}
	}
	return data
}
func (h *harness) createKey(t *testing.T, spec keySpec) createdKey {
	t.Helper()
	var out createdKey
	h.api(t, http.MethodPost, "/keys", spec, 201, &out)
	if out.Secret == "" || out.Key.ID == "" {
		t.Fatal("create must reveal a secret and key ID")
	}
	return out
}
func (h *harness) keys(t *testing.T) []keyView {
	t.Helper()
	var out struct {
		Items []keyView `json:"items"`
	}
	h.api(t, "GET", "/keys", nil, 200, &out)
	return out.Items
}
func (h *harness) page(t *testing.T, query string) requestPage {
	t.Helper()
	var out requestPage
	h.api(t, "GET", "/requests?"+query, nil, 200, &out)
	return out
}
func (h *harness) infer(t *testing.T, secret string, stream bool) (int, []byte) {
	t.Helper()
	status, body, err := h.rawRequest("POST", "/v1/chat/completions", chatBody(stream), map[string]string{"Authorization": "Bearer " + secret})
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

// Poll only observable state; never resend inference to hide lost callbacks.
func (h *harness) await(t *testing.T, description string, fn func() bool) {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("deadline: %s", description)
		case <-tick.C:
		}
	}
}

// TestRemovedAccountingFencesAreAbsentFromTheWire proves that the removed
// accounting fences are gone from the public management contract: the health
// payload no longer advertises a pending usage queue, a Key no longer carries
// a blocked reason and the removed recovery route answers with a normal
// missing-route response instead of an alias or a silent no-op.
func TestRemovedAccountingFencesAreAbsentFromTheWire(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	health := h.api(t, http.MethodGet, "/health", nil, http.StatusOK, nil)
	var healthFields map[string]any
	if err := json.Unmarshal(health, &healthFields); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if _, ok := healthFields["pending_usage"]; ok {
		t.Errorf("health still advertises pending_usage: %s", truncate(health, 300))
	}

	keys := h.api(t, http.MethodGet, "/keys", nil, http.StatusOK, nil)
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(keys, &listed); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	if len(listed.Items) != 1 {
		t.Fatalf("keys = %d, want exactly the created key", len(listed.Items))
	}
	if _, ok := listed.Items[0]["blocked_reason"]; ok {
		t.Errorf("key still carries blocked_reason: %s", truncate(keys, 300))
	}

	// The removed route must not be aliased to another handler and must not
	// silently succeed: an unregistered management path is a normal missing
	// route.
	for _, path := range []string{
		"/v0/management/plugins/" + pluginID + "/keys/recover",
		"/v0/management/plugins/" + pluginID + "/keys/recover?id=" + url.QueryEscape(created.Key.ID),
	} {
		status, body, err := h.rawRequest(http.MethodPost, path, []byte(`{"acknowledge":true}`),
			map[string]string{"Authorization": "Bearer " + mgmtKey, "Content-Type": "application/json"})
		if err != nil {
			t.Fatalf("removed recovery route %s: %v", path, err)
		}
		if status != http.StatusNotFound {
			t.Fatalf("removed recovery route %s status %d body %s, want 404", path, status, truncate(body, 300))
		}
	}

	// The rejected calls must not have touched the Key or its history.
	if after := h.keys(t); len(after) != 1 || after[0].ID != created.Key.ID || after[0].Used != (limitsView{}) {
		t.Fatalf("keys after the removed route = %+v, want the untouched created key", after)
	}
	if page := h.page(t, "page_size=100"); len(page.Items) != 0 {
		t.Fatalf("ledger after the removed route = %+v, want no records", page.Items)
	}
}
