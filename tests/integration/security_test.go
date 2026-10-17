//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func denyInference(t *testing.T, h *harness, secret string) {
	t.Helper()
	before := h.mock.callCount()
	status, _ := h.infer(t, secret, false)
	if status != 401 && status != 403 {
		t.Fatalf("denied inference status %d", status)
	}
	if h.mock.callCount() != before {
		t.Fatal("denied inference reached upstream")
	}
}

// Generated secrets work through every supported transport, but host builtin
// keys, conflicting credentials and invalidated secrets never fall back.
func TestExclusiveKeyAuthenticationAndInvalidation(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	denyInference(t, h, clientKey)
	denyInference(t, h, clientKeyTwo)
	denyInference(t, h, "")
	for _, variant := range []struct {
		query   string
		headers map[string]string
	}{
		{"", map[string]string{"Authorization": "Bearer " + created.Secret}},
		{"", map[string]string{"x-api-key": created.Secret}},
		{"", map[string]string{"x-goog-api-key": created.Secret}},
		{"?key=" + url.QueryEscape(created.Secret), nil},
		{"?auth_token=" + url.QueryEscape(created.Secret), nil},
	} {
		status, body, err := h.rawRequest("POST", "/v1/chat/completions"+variant.query, chatBody(false), variant.headers)
		if err != nil || status != 200 || jsonContent(body) != fixtureText {
			t.Fatalf("key transport status=%d err=%v", status, err)
		}
	}
	before := h.mock.callCount()
	status, _, err := h.rawRequest("POST", "/v1/chat/completions?key="+url.QueryEscape(created.Secret), chatBody(false), map[string]string{"Authorization": "Bearer " + clientKey})
	if err != nil || (status != 401 && status != 403) || h.mock.callCount() != before {
		t.Fatal("conflicting credentials admitted")
	}
	spec := validSpec()
	spec.Enabled = false
	h.api(t, "PUT", "/keys/"+created.Key.ID, spec, 200, nil)
	denyInference(t, h, created.Secret)
	spec.Enabled = true
	expired := time.Now().UTC().Add(-time.Hour)
	spec.ExpiresAt = &expired
	h.api(t, "PUT", "/keys/"+created.Key.ID, spec, 200, nil)
	denyInference(t, h, created.Secret)
	spec.ExpiresAt = nil
	h.api(t, "PUT", "/keys/"+created.Key.ID, spec, 200, nil)
	var rotated createdKey
	h.api(t, "POST", "/keys/"+created.Key.ID+"/rotate", nil, 200, &rotated)
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatal("rotation did not replace secret")
	}
	denyInference(t, h, created.Secret)
	if status, _ := h.infer(t, rotated.Secret, false); status != 200 {
		t.Fatal("rotated key rejected")
	}
	h.api(t, "DELETE", "/keys/"+created.Key.ID, nil, 204, nil)
	denyInference(t, h, rotated.Secret)
	for _, path := range []string{"/keys", "/health", "/requests?page_size=100", "/summary"} {
		raw := h.api(t, "GET", path, nil, 200, nil)
		for _, secret := range []string{created.Secret, rotated.Secret, mgmtKey, upstreamKey} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("%s leaked secret", path)
			}
		}
	}
	h.Stop()
	db, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte(h.stdout.String() + h.stderr.String())
	for _, secret := range []string{created.Secret, rotated.Secret, mgmtKey, upstreamKey} {
		if bytes.Contains(db, []byte(secret)) || bytes.Contains(logs, []byte(secret)) {
			t.Fatal("plaintext credential in database/logs")
		}
	}
}

// Each quota is enforced on the next admission, not by rewriting an in-flight
// response. Reset clears only selected windows and never historical costs.
func TestQuotaWindowsResetAndModelAuthorization(t *testing.T) {
	for _, window := range []string{"five_hour", "week", "month", "total"} {
		t.Run(window, func(t *testing.T) {
			h := newHarness(t)
			acceptancePutPricing(t, h, acceptancePricing{Prices: []acceptancePrice{
				{Model: modelName, Rates: acceptanceRatesAll(1_000_000)},
				{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000)},
			}, Rules: []acceptanceRule{}})
			spec := validSpec()
			switch window {
			case "five_hour":
				spec.Limits.FiveHour = 1
			case "week":
				spec.Limits.Week = 1
			case "month":
				spec.Limits.Month = 1
			case "total":
				spec.Limits.Total = 1
			}
			created := h.createKey(t, spec)
			r := acceptanceInferAndAwait(t, h, created.Secret, map[string]struct{}{})
			acceptanceAssertCost(t, r, 15, "manual")
			h.await(t, "all quota accumulators charged", func() bool {
				keys := h.keys(t)
				return len(keys) == 1 && keys[0].Used == (limitsView{15, 15, 15, 15})
			})
			calls := h.mock.callCount()
			if status, _ := h.infer(t, created.Secret, false); status != 429 {
				t.Fatalf("quota status %d want 429", status)
			}
			if h.mock.callCount() != calls {
				t.Fatal("exhausted quota reached upstream")
			}
			var reset keyView
			h.api(t, "POST", "/keys/"+created.Key.ID+"/reset", map[string]any{"windows": []string{window}}, 200, &reset)
			want := limitsView{15, 15, 15, 15}
			switch window {
			case "five_hour":
				want.FiveHour = 0
			case "week":
				want.Week = 0
			case "month":
				want.Month = 0
			case "total":
				want.Total = 0
			}
			if reset.Used != want {
				t.Fatalf("reset used=%+v want=%+v", reset.Used, want)
			}
			acceptanceAssertCost(t, acceptanceRecordByID(t, h, r.ID), 15, "manual")
			if status, _ := h.infer(t, created.Secret, false); status != 200 {
				t.Fatal("reset failed to restore admission")
			}
		})
	}
	t.Run("exact models and principal", func(t *testing.T) {
		h := newHarness(t)
		spec := validSpec()
		spec.AllowedModels = []string{"not-" + modelName}
		a := h.createKey(t, spec)
		b := h.createKey(t, validSpec())
		denyInference(t, h, a.Secret)
		r := acceptanceInferAndAwait(t, h, b.Secret, map[string]struct{}{})
		if r.KeyID != b.Key.ID || r.TraceID == "" || r.ID == "" {
			t.Fatalf("wrong execution attribution: %+v", r)
		}
		if len(h.page(t, "key_id="+url.QueryEscape(a.Key.ID)).Items) != 0 {
			t.Fatal("principal attribution crossed keys")
		}
	})
}

// The same Query must select the same records and summary totals. Content is
// absent from the table and fetched only through authenticated detail.
func TestFilteredLogsCaptureRedactionAndBounds(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	settings := acceptanceGetSettings(t, h)
	settings["capture_enabled"] = false
	h.api(t, "PUT", "/settings", settings, 200, nil)
	known := map[string]struct{}{}
	disabled := acceptanceInferAndAwait(t, h, created.Secret, known)
	var detail struct {
		Record        recordView      `json:"record"`
		Body          json.RawMessage `json:"body"`
		Headers       http.Header     `json:"headers"`
		CaptureStatus string          `json:"capture_status"`
	}
	h.api(t, "GET", "/requests/"+disabled.ID, nil, 200, &detail)
	if detail.CaptureStatus != "disabled" || string(detail.Body) != "null" {
		t.Fatalf("disabled capture: %+v", detail)
	}
	settings["capture_enabled"] = true
	h.api(t, "PUT", "/settings", settings, 200, nil)
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"` + upstreamPrompt + `"}],"metadata":{"api_key":"fake-nested-secret","nested":{"password":"fake-nested-password"}},"stream":false}`)
	status, _, err := h.rawRequest("POST", "/v1/chat/completions?key="+url.QueryEscape(created.Secret), body, map[string]string{"X-Api-Key": created.Secret})
	if err != nil || status != 200 {
		t.Fatalf("capture inference status=%d err=%v", status, err)
	}
	r := acceptanceAwaitNewRecord(t, h, known)
	h.await(t, "capture detail available", func() bool {
		h.api(t, "GET", "/requests/"+r.ID, nil, 200, &detail)
		return detail.CaptureStatus == "available" || detail.CaptureStatus == "redacted"
	})
	if detail.Record.ID != r.ID || !bytes.Contains(detail.Body, []byte(upstreamPrompt)) {
		t.Fatal("execution/trace detail join failed")
	}
	raw, _ := json.Marshal(detail)
	for _, secret := range []string{created.Secret, "fake-nested-secret", "fake-nested-password", upstreamKey} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("detail secret redaction failed")
		}
	}
	metadata := h.api(t, "GET", "/requests?page_size=100", nil, 200, nil)
	var table map[string]json.RawMessage
	var rows struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if json.Unmarshal(metadata, &rows) != nil {
		t.Fatal("invalid page")
	}
	for _, table = range rows.Items {
		for _, field := range []string{"body", "headers"} {
			if _, ok := table[field]; ok {
				t.Fatal("content included in table")
			}
		}
	}
	for _, query := range []string{
		"key_id=" + created.Key.ID,
		"model=" + url.QueryEscape(r.Model),
		"provider=" + url.QueryEscape(r.Provider),
		"auth_index=" + url.QueryEscape(r.AuthIndex),
		"status=success",
		"key_id=" + created.Key.ID + "&model=" + url.QueryEscape(r.Model) + "&status=success",
		"key_id=missing", "status=failed",
	} {
		page := h.page(t, query+"&page_size=100")
		var summary acceptanceSummary
		h.api(t, "GET", "/summary?"+query, nil, 200, &summary)
		var sum int64
		for _, item := range page.Items {
			sum += item.CostMicroUSD
		}
		if summary.Requests != int64(len(page.Items)) || summary.CostMicroUSD != sum {
			t.Fatalf("query disagreement %s", query)
		}
	}
	page1 := h.page(t, "page=1&page_size=1")
	page2 := h.page(t, "page=2&page_size=1")
	if page1.Page != 1 || page1.PageSize != 1 || !page1.HasMore || page2.HasMore || len(page1.Items) != 1 || len(page2.Items) != 1 || page1.Items[0].ID == page2.Items[0].ID {
		t.Fatal("1-based pagination failed")
	}
	settings["max_body_bytes"] = 64
	h.api(t, "PUT", "/settings", settings, 200, nil)
	large := chatBodyWithPrompt(false, strings.Repeat("x", 2048))
	h.mock.allowAnyPrompt()
	status, _, err = h.rawRequest("POST", "/v1/chat/completions", large, map[string]string{"Authorization": "Bearer " + created.Secret})
	if err != nil || status != 200 {
		t.Fatal("bounded capture affected inference")
	}
	truncated := acceptanceAwaitNewRecord(t, h, known)
	h.await(t, "bounded capture status", func() bool {
		h.api(t, "GET", "/requests/"+truncated.ID, nil, 200, &detail)
		return detail.CaptureStatus == "truncated"
	})
	if len(detail.Body) > 64 {
		t.Fatal("capture size bound exceeded")
	}
}

// Reject malformed/legacy query inputs, unknown JSON and unauthenticated detail.
func TestManagementStrictInputs(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/keys", "/requests", "/summary", "/settings", "/health", "/requests/nonexistent"} {
		status, _, err := h.rawRequest("GET", "/v0/management/plugins/"+pluginID+path, nil, nil)
		if err != nil || (status != 401 && status != 403) {
			t.Fatalf("management auth %s status %d err %v", path, status, err)
		}
	}
	for _, query := range []string{"limit=1", "offset=0", "unknown=x", "page=0", "page_size=101", "status=bogus", "from=bad", "from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		for _, route := range []string{"/requests?", "/summary?"} {
			raw := h.api(t, "GET", route+query, nil, 400, nil)
			var response struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			if json.Unmarshal(raw, &response) != nil || response.Error.Code == "" || response.Error.Message == "" {
				t.Fatal("missing standard error")
			}
		}
	}
	h.api(t, "POST", "/keys", map[string]any{"unknown": true}, 400, nil)
	for _, body := range [][]byte{[]byte(`{} {}`), bytes.Repeat([]byte(" "), 1<<20+1)} {
		status, _, err := h.rawRequest("POST", "/v0/management/plugins/"+pluginID+"/keys", body, map[string]string{"Authorization": "Bearer " + mgmtKey})
		if err != nil || (status != 400 && status != 413) {
			t.Fatalf("invalid JSON status %d err %v", status, err)
		}
	}
}

// Identical half-open time bounds must select identical executions in logs and
// summary. In particular, an execution at `to` must be excluded.
func TestHalfOpenQueryBounds(t *testing.T) {
	h := newHarness(t)
	key := h.createKey(t, validSpec())
	r := acceptanceInferAndAwait(t, h, key.Secret, map[string]struct{}{})
	if r.Time.IsZero() {
		t.Fatal("missing requested-at timestamp")
	}
	for _, include := range []bool{true, false} {
		from, to := r.Time, r.Time.Add(time.Nanosecond)
		if !include {
			from, to = r.Time.Add(-time.Nanosecond), r.Time
		}
		query := url.Values{"key_id": {key.Key.ID}, "from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}.Encode()
		page := h.page(t, query)
		var summary struct {
			Requests    int64  `json:"requests"`
			Granularity string `json:"granularity"`
		}
		h.api(t, "GET", "/summary?"+query, nil, 200, &summary)
		want := 0
		if include {
			want = 1
		}
		if len(page.Items) != want || summary.Requests != int64(want) || summary.Granularity == "" {
			t.Fatalf("half-open include=%v page=%+v summary=%+v", include, page, summary)
		}
	}
}
