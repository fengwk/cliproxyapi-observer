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

// Native CPA access keys and Observer client keys are independent providers.
// Observer must neither shadow nor account native traffic, while its generated,
// custom, disabled, expired, rotated and revoked credentials keep their own
// admission boundaries.
func TestKeyAuthenticationCoexistenceAndInvalidation(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	// Native host keys keep host authentication but model execution now requires
	// an Observer-owned scope. They never enter the Observer ledger or create
	// Observer Keys.
	for _, secret := range []string{clientKey, clientKeyTwo} {
		before := h.mock.callCount()
		status, body := h.infer(t, secret, false)
		requireObserverKeyRequired(t, status, body)
		if h.mock.callCount() != before {
			t.Fatal("native key model execution reached the upstream")
		}
	}
	if keys := h.keys(t); len(keys) != 1 || keys[0].ID != created.Key.ID {
		t.Fatalf("native traffic changed Observer Keys: %+v", keys)
	}
	if records := h.page(t, "page_size=100"); len(records.Items) != 0 {
		t.Fatalf("native traffic entered Observer accounting: %+v", records.Items)
	}

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
	// Settle the five authenticated Observer transports before using the ledger
	// as a discriminator for credential-source conflicts below.
	h.await(t, "generated key transports are accounted", func() bool {
		return len(h.page(t, "page_size=100").Items) == 5
	})

	// Conflicting Observer credentials are not authenticated by Observer. With no
	// native key present, CPA has no fallback and rejects the request.
	before := h.mock.callCount()
	status, _, err := h.rawRequest("POST", "/v1/chat/completions?key="+url.QueryEscape(created.Secret), chatBody(false), map[string]string{"Authorization": "Bearer fake-unconfigured-observer-key"})
	if err != nil || (status != 401 && status != 403) || h.mock.callCount() != before {
		t.Fatal("conflicting Observer credentials were admitted")
	}

	// A native credential in a second source is a real coexistence case: Observer
	// refuses the conflicting credential set, then CPA's native provider resolves
	// the request. That native-winning scope is still denied at the model
	// boundary and is never accounted by Observer.
	for _, variant := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"observer query plus native header", "/v1/chat/completions?key=" + url.QueryEscape(created.Secret), map[string]string{"Authorization": "Bearer " + clientKey}},
		{"native query plus observer header", "/v1/chat/completions?key=" + url.QueryEscape(clientKey), map[string]string{"Authorization": "Bearer " + created.Secret}},
	} {
		beforeRecords := len(h.page(t, "page_size=100").Items)
		beforeCalls := h.mock.callCount()
		status, body, err := h.rawRequest("POST", variant.path, chatBody(false), variant.headers)
		if err != nil {
			t.Fatalf("%s: %v", variant.name, err)
		}
		requireObserverKeyRequired(t, status, body)
		if got := h.mock.callCount(); got != beforeCalls {
			t.Fatalf("%s reached the upstream: %d -> %d", variant.name, beforeCalls, got)
		}
		if got := len(h.page(t, "page_size=100").Items); got != beforeRecords {
			t.Fatalf("%s entered Observer accounting: %d -> %d records", variant.name, beforeRecords, got)
		}
	}

	// Identical duplicate Observer credentials are still one credential, so this
	// path remains Observer-authenticated and is accounted normally.
	beforeRecords := len(h.page(t, "page_size=100").Items)
	status, body, err := h.rawRequest("POST", "/v1/chat/completions", chatBody(false), map[string]string{
		"Authorization": "Bearer " + created.Secret,
		"x-api-key":     created.Secret,
	})
	if err != nil || status != 200 || jsonContent(body) != fixtureText {
		t.Fatalf("duplicate identical credentials status=%d err=%v", status, err)
	}
	h.await(t, "duplicate identical credentials are accounted once", func() bool {
		return len(h.page(t, "page_size=100").Items) == beforeRecords+1
	})

	spec := validSpec()
	spec.Enabled = false
	h.api(t, "PUT", "/keys?id="+url.QueryEscape(created.Key.ID), spec, 200, nil)
	denyInference(t, h, created.Secret)
	spec.Enabled = true
	expired := time.Now().UTC().Add(-time.Hour)
	spec.ExpiresAt = &expired
	h.api(t, "PUT", "/keys?id="+url.QueryEscape(created.Key.ID), spec, 200, nil)
	denyInference(t, h, created.Secret)
	spec.ExpiresAt = nil
	h.api(t, "PUT", "/keys?id="+url.QueryEscape(created.Key.ID), spec, 200, nil)
	var rotated createdKey
	h.api(t, "POST", "/keys/rotate?id="+url.QueryEscape(created.Key.ID), nil, 200, &rotated)
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatal("rotation did not replace secret")
	}
	denyInference(t, h, created.Secret)
	if status, _ := h.infer(t, rotated.Secret, false); status != 200 {
		t.Fatal("rotated key rejected")
	}
	h.api(t, "DELETE", "/keys?id="+url.QueryEscape(created.Key.ID), nil, 204, nil)
	denyInference(t, h, rotated.Secret)
	// Revoking an Observer Key must not revoke host authentication: the native
	// key still reaches the host, but model execution remains Observer-only.
	beforeCalls := h.mock.callCount()
	status, body = h.infer(t, clientKey, false)
	requireObserverKeyRequired(t, status, body)
	if h.mock.callCount() != beforeCalls {
		t.Fatal("native key after Observer revocation reached the upstream")
	}
	if status, raw, err := h.rawRequest("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + clientKey}); err != nil || status != http.StatusOK {
		t.Fatalf("native catalog after Observer revocation status=%d err=%v body=%s", status, err, truncate(raw, 300))
	}
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
		if bytes.Contains(logs, []byte(secret)) {
			t.Fatal("plaintext credential in logs")
		}
	}
	// Client material is intentionally persisted for management; bbolt's freed
	// pages are not securely erased by revocation. Host/upstream credentials,
	// unlike managed client secrets, must never enter this database.
	for _, secret := range []string{mgmtKey, upstreamKey} {
		if bytes.Contains(db, []byte(secret)) {
			t.Fatal("host or upstream credential in database")
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
			h.api(t, "POST", "/keys/reset?id="+url.QueryEscape(created.Key.ID), map[string]any{"windows": []string{window}}, 200, &reset)
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
		BodyStatus    string          `json:"body_status"`
		CaptureReason string          `json:"capture_reason"`
		OriginalBytes int64           `json:"original_bytes"`
	}
	h.api(t, "GET", "/requests/detail?id="+url.QueryEscape(disabled.ID), nil, 200, &detail)
	if detail.CaptureStatus != "disabled" || string(detail.Body) != "null" {
		t.Fatalf("disabled capture: %+v", detail)
	}
	settings["capture_enabled"] = true
	settings["capture_mode"] = "headers_and_body"
	h.api(t, "PUT", "/settings", settings, 200, nil)
	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"` + upstreamPrompt + `"}],"metadata":{"api_key":"fake-nested-secret","nested":{"password":"fake-nested-password"}},"stream":false}`)
	status, _, err := h.rawRequest("POST", "/v1/chat/completions?key="+url.QueryEscape(created.Secret), body, map[string]string{"X-Api-Key": created.Secret})
	if err != nil || status != 200 {
		t.Fatalf("capture inference status=%d err=%v", status, err)
	}
	r := acceptanceAwaitNewRecord(t, h, known)
	h.await(t, "capture detail available", func() bool {
		h.api(t, "GET", "/requests/detail?id="+url.QueryEscape(r.ID), nil, 200, &detail)
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
	bounded := acceptanceAwaitNewRecord(t, h, known)
	h.await(t, "bounded body is marked too_large", func() bool {
		h.api(t, "GET", "/requests/detail?id="+url.QueryEscape(bounded.ID), nil, 200, &detail)
		return detail.CaptureStatus == "too_large" && detail.BodyStatus == "too_large" && detail.CaptureReason == "body_too_large"
	})
	if string(detail.Body) != "null" {
		t.Fatal("bounded capture retained a partial body")
	}
	if detail.OriginalBytes <= 64 {
		t.Fatalf("original body size = %d, want the observed request size above the 64-byte bound", detail.OriginalBytes)
	}
}

// Each intentional unauthenticated probe has its own host so CPA's management
// IP-ban threshold cannot contaminate subsequent valid-auth contract checks.
func TestManagementRoutesRequireAuthentication(t *testing.T) {
	for _, path := range []string{"/keys", "/requests", "/summary", "/settings", "/health", "/requests/detail?id=nonexistent", "/models/token"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t)
			method := "GET"
			if path == "/models/token" {
				method = "POST"
			}
			status, body, err := h.rawRequest(method, "/v0/management/plugins/"+pluginID+path, nil, nil)
			if err != nil || status != 401 {
				t.Fatalf("management auth %s status=%d err=%v body=%s", path, status, err, body)
			}
		})
	}
}

// Reject malformed/legacy query inputs and unknown JSON with valid management
// credentials; this host has never received an intentional bad-auth request.
func TestManagementStrictInputs(t *testing.T) {
	h := newHarness(t)
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
