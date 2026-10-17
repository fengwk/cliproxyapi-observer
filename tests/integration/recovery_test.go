//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// This file covers the crash-recovery contract from DESIGN.md: an unclean
// native-host exit fences only the Keys that existed at crash time, the fence
// is visible through the public management API, RecoverKey requires an explicit
// acknowledgment, and recovery never rewrites counters, limits or history.
//
// Graceful restart (SIGTERM through harness.Stop) preserving counters and the
// original client secret is already covered by
// TestSettingsHealthCompactRestartPreserveKeysAndCosts in acceptance_test.go;
// this file deliberately does not duplicate that scenario.

// recoveryKeyState mirrors the public Key wire shape. It is local to this file
// because the acceptance suite must not import engine internals.
type recoveryKeyState struct {
	ID            string         `json:"id"`
	Label         string         `json:"label"`
	Enabled       bool           `json:"enabled"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	MaxConcurrent int            `json:"max_concurrent"`
	AllowedModels []string       `json:"allowed_models"`
	Limits        limitsView     `json:"limits"`
	Used          limitsView     `json:"used"`
	Starts        recoveryWindow `json:"starts"`
	Resets        recoveryWindow `json:"resets"`
	Active        int            `json:"active"`
	BlockedReason string         `json:"blocked_reason"`
	CreatedAt     time.Time      `json:"created_at"`
}

type recoveryWindow struct {
	FiveHour time.Time `json:"five_hour"`
	Week     time.Time `json:"week"`
	Month    time.Time `json:"month"`
	Total    time.Time `json:"total"`
}

type recoveryErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func recoveryKeys(t *testing.T, h *harness) []recoveryKeyState {
	t.Helper()
	var out struct {
		Items []recoveryKeyState `json:"items"`
	}
	h.api(t, http.MethodGet, "/keys", nil, http.StatusOK, &out)
	return out.Items
}

func recoveryKeyByID(t *testing.T, h *harness, id string) recoveryKeyState {
	t.Helper()
	for _, key := range recoveryKeys(t, h) {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %q is missing from the public key list", id)
	return recoveryKeyState{}
}

// recoveryAssertStableKey proves recovery and rejection do not touch the
// identity, limits, cycle starts or counters of the affected Key.
func recoveryAssertStableKey(t *testing.T, label string, before, after recoveryKeyState) {
	t.Helper()
	if after.ID != before.ID || after.Label != before.Label || after.Enabled != before.Enabled ||
		after.MaxConcurrent != before.MaxConcurrent {
		t.Errorf("%s identity/spec changed: before=%+v after=%+v", label, before, after)
	}
	if !reflect.DeepEqual(after.ExpiresAt, before.ExpiresAt) {
		t.Errorf("%s expires_at changed: before=%v after=%v", label, before.ExpiresAt, after.ExpiresAt)
	}
	if !reflect.DeepEqual(after.AllowedModels, before.AllowedModels) {
		t.Errorf("%s allowed_models changed: before=%v after=%v", label, before.AllowedModels, after.AllowedModels)
	}
	if after.Limits != before.Limits {
		t.Errorf("%s limits changed: before=%+v after=%+v", label, before.Limits, after.Limits)
	}
	if after.Used != before.Used {
		t.Errorf("%s counters changed: before=%+v after=%+v", label, before.Used, after.Used)
	}
	if after.Starts != before.Starts || after.Resets != before.Resets {
		t.Errorf("%s cycle windows changed: starts %+v/%+v resets %+v/%+v",
			label, before.Starts, after.Starts, before.Resets, after.Resets)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("%s created_at changed: before=%s after=%s", label, before.CreatedAt, after.CreatedAt)
	}
}

type recoveryHistory struct {
	Summary acceptanceSummary
	Records map[string]recordView
}

func recoveryTakeHistory(t *testing.T, h *harness) recoveryHistory {
	t.Helper()
	history := recoveryHistory{Records: make(map[string]recordView)}
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &history.Summary)
	for _, record := range h.page(t, "page_size=100").Items {
		history.Records[record.ID] = record
	}
	return history
}

// recoveryAssertHistoryUnchanged proves recovery neither inserts a synthetic
// usage record nor rewrites an existing quote or rollup.
func recoveryAssertHistoryUnchanged(t *testing.T, label string, before, after recoveryHistory) {
	t.Helper()
	if after.Summary != before.Summary {
		t.Errorf("%s summary changed: before=%+v after=%+v", label, before.Summary, after.Summary)
	}
	if len(after.Records) != len(before.Records) {
		t.Errorf("%s record count changed: before=%d after=%d", label, len(before.Records), len(after.Records))
	}
	for id, want := range before.Records {
		got, ok := after.Records[id]
		if !ok {
			t.Errorf("%s record %s disappeared", label, id)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s record %s changed: before=%+v after=%+v", label, id, want, got)
		}
	}
}

// TestCrashRecoveryIsKeyScopedAndRequiresAcknowledgment kills the real native
// host while an admitted request is blocked upstream, then restarts against the
// same database. The crash must block the pre-crash Key (with its 15 micro-USD
// counter intact), must not poison a newly created Key, must reject recovery
// without acknowledgment, and must preserve counters, limits and history when
// the acknowledged recovery is applied.
func TestCrashRecoveryIsKeyScopedAndRequiresAcknowledgment(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	// Pin both the client alias and the upstream identity to the same exact
	// integer rate so the fixture always costs 6+4+5 = 15 micro-USD.
	applied := acceptancePutPricing(t, h, acceptancePricing{
		Auto: false,
		Prices: []acceptancePrice{
			{Model: modelName, Rates: acceptanceRatesAll(1_000_000), Source: "manual"},
			{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000), Source: "manual"},
		},
		Rules: []acceptanceRule{},
	})
	if applied.Auto || len(applied.Prices) != 2 {
		t.Fatalf("saved pricing = %+v, want two manual model prices", applied)
	}

	known := make(map[string]struct{})
	first := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, first)
	acceptanceAssertCost(t, first, 15, "manual")
	if first.KeyID != created.Key.ID {
		t.Fatalf("first record key_id = %q, want %q", first.KeyID, created.Key.ID)
	}

	wantUsed := limitsView{FiveHour: 15, Week: 15, Month: 15, Total: 15}
	beforeCrash := recoveryKeyByID(t, h, created.Key.ID)
	if beforeCrash.Used != wantUsed {
		t.Fatalf("pre-crash counters = %+v, want %+v", beforeCrash.Used, wantUsed)
	}

	// The barrier is consumed after observer admission but before the upstream
	// response, so SIGKILL leaves an admitted execution with no usage record.
	gate := h.mock.holdNextRequest()
	t.Cleanup(gate.open)
	inFlight := startInference(t.Context(), h, created.Secret, false)
	awaitMockEntered(t, gate)

	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill native host: %v", err)
	}
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("native host did not exit after SIGKILL")
	}
	// Release the blocked httptest handler synchronously. Cleanup remains as a
	// second idempotent release for early failures.
	gate.open()
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("client did not observe the killed host")
	}

	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatalf("restart after crash: %v", err)
	}

	crashed := recoveryKeyByID(t, restarted, created.Key.ID)
	if crashed.BlockedReason == "" {
		t.Fatal("pre-crash key is not blocked after an unclean shutdown")
	}
	if crashed.Used != wantUsed {
		t.Fatalf("post-crash counters = %+v, want the durable %+v", crashed.Used, wantUsed)
	}
	calls := h.mock.callCount()
	if status, _ := restarted.infer(t, created.Secret, false); status != http.StatusServiceUnavailable {
		t.Fatalf("crash-blocked key admission status %d, want 503", status)
	}
	if h.mock.callCount() != calls {
		t.Fatal("crash-blocked key dispatched upstream")
	}

	// A Key created after the crash must not inherit the pre-crash fence.
	fresh := restarted.createKey(t, validSpec())
	freshState := recoveryKeyByID(t, restarted, fresh.Key.ID)
	if freshState.BlockedReason != "" {
		t.Fatalf("new key blocked_reason = %q, want empty (the crash fence must be key-scoped)", freshState.BlockedReason)
	}
	freshRecord := acceptanceInferAndAwait(t, restarted, fresh.Secret, known)
	acceptanceAssertTokens(t, freshRecord)
	acceptanceAssertCost(t, freshRecord, 15, "manual")
	if freshRecord.KeyID != fresh.Key.ID {
		t.Fatalf("fresh record key_id = %q, want %q", freshRecord.KeyID, fresh.Key.ID)
	}
	if got := recoveryKeyByID(t, restarted, fresh.Key.ID).Used.Total; got != 15 {
		t.Errorf("new key total counter = %d, want 15", got)
	}
	if still := recoveryKeyByID(t, restarted, created.Key.ID); still.Used != wantUsed || still.BlockedReason == "" {
		t.Fatalf("old key changed while a new key was used: %+v", still)
	}

	// Query IDs are URL-escaped exactly as required by the management API.
	recoverPath := "/keys/recover?id=" + url.QueryEscape(created.Key.ID)

	// DESIGN.md does not publish an authoritative error code for a missing
	// acknowledgment; the binding behavior is the 400 rejection. Assert the
	// sanitized error object without pinning an unpublished code string.
	var rejected recoveryErrorResponse
	restarted.api(t, http.MethodPost, recoverPath, map[string]any{"acknowledge": false}, http.StatusBadRequest, &rejected)
	if rejected.Error.Code == "" || rejected.Error.Message == "" {
		t.Errorf("recovery rejection = %+v, want a sanitized error code and message", rejected)
	}
	if still := recoveryKeyByID(t, restarted, created.Key.ID); still.BlockedReason == "" || still.Used != wantUsed {
		t.Fatalf("rejected recovery changed the blocked key: %+v", still)
	}

	beforeRecovery := recoveryKeyByID(t, restarted, created.Key.ID)
	beforeHistory := recoveryTakeHistory(t, restarted)

	var recovered recoveryKeyState
	restarted.api(t, http.MethodPost, recoverPath, map[string]any{"acknowledge": true}, http.StatusOK, &recovered)
	if recovered.ID != created.Key.ID {
		t.Fatalf("recovered key id = %q, want %q", recovered.ID, created.Key.ID)
	}
	if recovered.BlockedReason != "" {
		t.Errorf("recovered key blocked_reason = %q, want empty", recovered.BlockedReason)
	}
	afterRecovery := recoveryKeyByID(t, restarted, created.Key.ID)
	if afterRecovery.BlockedReason != "" {
		t.Errorf("blocked_reason after acknowledged recovery = %q, want empty", afterRecovery.BlockedReason)
	}
	recoveryAssertStableKey(t, "acknowledged recovery", beforeRecovery, afterRecovery)
	recoveryAssertHistoryUnchanged(t, "acknowledged recovery", beforeHistory, recoveryTakeHistory(t, restarted))

	// Recovery only unblocks admission; the next real execution must be
	// accounted normally and increment the existing counters.
	after := acceptanceInferAndAwait(t, restarted, created.Secret, known)
	acceptanceAssertTokens(t, after)
	acceptanceAssertCost(t, after, 15, "manual")
	if after.KeyID != created.Key.ID {
		t.Fatalf("post-recovery record key_id = %q, want %q", after.KeyID, created.Key.ID)
	}
	finalKey := recoveryKeyByID(t, restarted, created.Key.ID)
	if finalKey.Used.Total != 30 {
		t.Errorf("post-recovery total counter = %d, want 30 (15 durable + 15 new)", finalKey.Used.Total)
	}
	if finalKey.BlockedReason != "" {
		t.Errorf("post-recovery key blocked_reason = %q, want empty", finalKey.BlockedReason)
	}

	var finalSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &finalSummary)
	if finalSummary.Requests != beforeHistory.Summary.Requests+1 {
		t.Errorf("summary requests = %d, want %d", finalSummary.Requests, beforeHistory.Summary.Requests+1)
	}
	if finalSummary.CostMicroUSD != beforeHistory.Summary.CostMicroUSD+15 {
		t.Errorf("summary cost = %d, want %d", finalSummary.CostMicroUSD, beforeHistory.Summary.CostMicroUSD+15)
	}
	h.mock.requireClean(t)
}
