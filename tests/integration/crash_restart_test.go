//go:build integration

package integration

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Crash contract: an unclean host exit never fences a Key, never requires a
// recovery call and never fabricates the usage of an execution that never
// committed.
//
// Graceful restart (SIGTERM through harness.Stop) is covered by
// TestSettingsHealthCompactRestartPreserveKeysAndCosts in acceptance_test.go.

// crashKey returns the public state of one Key.
func crashKey(t *testing.T, h *harness, id string) keyView {
	t.Helper()
	for _, key := range h.keys(t) {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %q is missing from the public key list", id)
	return keyView{}
}

// TestCrashRestartKeepsKeysUsableAndCommitsOnlyRealUsage kills the host while
// an admitted request is blocked upstream and restarts against the same
// database. The pre-crash Key must work on the first request after the
// restart, the committed history and totals must survive, the missing
// execution must stay missing and the next execution must accumulate on the
// durable totals.
func TestCrashRestartKeepsKeysUsableAndCommitsOnlyRealUsage(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	// Pin the client alias and the upstream identity to the same integer rate
	// so the fixture always costs 6+4+5 = 15 micro-USD.
	applied := acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{
			acceptanceManual(modelName, acceptanceProvider, 1_000_000),
			acceptanceManual(upstreamModel, acceptanceProvider, 1_000_000),
		},
	})
	if applied.Auto || len(applied.Prices) != 2 {
		t.Fatalf("saved pricing = %+v, want two manual model prices", applied)
	}

	known := make(map[string]struct{})
	committed := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, committed)
	acceptanceAssertCost(t, committed, 15, "manual")
	if committed.KeyID != created.Key.ID {
		t.Fatalf("committed record key_id = %q, want %q", committed.KeyID, created.Key.ID)
	}

	wantUsed := limitsView{FiveHour: 15, Week: 15, Month: 15, Total: 15}
	if before := crashKey(t, h, created.Key.ID); before.Used != wantUsed {
		t.Fatalf("pre-crash counters = %+v, want %+v", before.Used, wantUsed)
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
	// Release the blocked handler synchronously; cleanup stays as a second
	// idempotent release for early failures.
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

	// The crash must not fabricate the missing execution: the ledger holds
	// exactly the one committed record and the totals describe exactly it.
	page := restarted.page(t, "page_size=100")
	if page.HasMore || len(page.Items) != 1 {
		t.Fatalf("ledger after the crash = %+v, want exactly the 1 committed execution", page.Items)
	}
	if page.Items[0].ID != committed.ID {
		t.Fatalf("ledger record after the crash = %q, want the committed %q", page.Items[0].ID, committed.ID)
	}
	var summary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 1 || summary.CostMicroUSD != 15 {
		t.Fatalf("summary after the crash = %+v, want requests=1 cost=15", summary)
	}
	if after := crashKey(t, restarted, created.Key.ID); after.Used != wantUsed {
		t.Fatalf("counters after the crash = %+v, want the durable %+v", after.Used, wantUsed)
	}

	// The pre-crash Key must work immediately: no fence, no acknowledgment
	// and no recovery request may stand between the client and the upstream.
	calls := restarted.mock.callCount()
	status, body := restarted.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("post-crash inference status %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("post-crash content = %q, want %q", got, fixtureText)
	}
	if restarted.mock.callCount() != calls+1 {
		t.Fatal("the old key did not reach the upstream after the crash")
	}

	// The new usage is accounted normally and accumulates on the durable
	// totals instead of replacing them.
	after := acceptanceAwaitNewRecord(t, restarted, known)
	acceptanceAssertTokens(t, after)
	acceptanceAssertCost(t, after, 15, "manual")
	if after.KeyID != created.Key.ID {
		t.Fatalf("post-crash record key_id = %q, want %q", after.KeyID, created.Key.ID)
	}
	wantTotal := limitsView{FiveHour: 30, Week: 30, Month: 30, Total: 30}
	if final := crashKey(t, restarted, created.Key.ID); final.Used != wantTotal {
		t.Errorf("counters after the new execution = %+v, want %+v", final.Used, wantTotal)
	}
	var finalSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &finalSummary)
	if finalSummary.Requests != 2 || finalSummary.CostMicroUSD != 30 {
		t.Errorf("summary after the new execution = %+v, want requests=2 cost=30", finalSummary)
	}
	if finalPage := restarted.page(t, "page_size=100"); finalPage.HasMore || len(finalPage.Items) != 2 {
		t.Errorf("ledger after the new execution = %+v, want the committed and the new execution", finalPage.Items)
	}

	// The committed history keeps its identity, quote and amount across the
	// crash.
	kept := acceptanceRecordByID(t, restarted, committed.ID)
	if kept.CostMicroUSD != committed.CostMicroUSD || kept.PriceSource != committed.PriceSource ||
		kept.KeyID != committed.KeyID || !kept.Time.Equal(committed.Time) {
		t.Errorf("committed record changed after the crash: before=%+v after=%+v", committed, kept)
	}
	h.mock.requireClean(t)
}

// TestLegacyInterruptionArtifactsDoNotFenceKeys proves that the obsolete
// sidecar artifacts of the removed fence mechanism have no effect: a Key left
// next to the database by an older release stays usable, its durable counters
// and history are untouched and the next execution is accounted normally.
func TestLegacyInterruptionArtifactsDoNotFenceKeys(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{
			acceptanceManual(modelName, acceptanceProvider, 1_000_000),
			acceptanceManual(upstreamModel, acceptanceProvider, 1_000_000),
		},
	})

	known := make(map[string]struct{})
	committed := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, committed, 15, "manual")

	// Stop the host first: the obsolete artifacts are exactly what an older
	// release left behind next to the database of a crashed session.
	h.awaitGracefulRestartReady(t, 1)
	h.Stop()
	markerDir := h.dbPath + ".interrupted"
	if err := os.MkdirAll(markerDir, 0o700); err != nil {
		t.Fatalf("create legacy marker directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, created.Key.ID), []byte("unknown accounting\n"), 0o600); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}

	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatalf("restart with legacy artifacts: %v", err)
	}

	// The obsolete bytes must not fence the Key and must not rewrite its
	// durable state.
	wantUsed := limitsView{FiveHour: 15, Week: 15, Month: 15, Total: 15}
	if key := crashKey(t, restarted, created.Key.ID); key.Used != wantUsed {
		t.Fatalf("counters after legacy artifacts = %+v, want %+v", key.Used, wantUsed)
	}
	kept := acceptanceRecordByID(t, restarted, committed.ID)
	if kept.CostMicroUSD != committed.CostMicroUSD || kept.PriceSource != committed.PriceSource ||
		!kept.Time.Equal(committed.Time) {
		t.Errorf("committed record changed after legacy artifacts: before=%+v after=%+v", committed, kept)
	}

	// The Key is usable immediately and the next execution is accounted.
	status, body := restarted.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("post-legacy inference status %d body %s, want 200", status, truncate(body, 400))
	}
	after := acceptanceAwaitNewRecord(t, restarted, known)
	acceptanceAssertCost(t, after, 15, "manual")
	if after.KeyID != created.Key.ID {
		t.Fatalf("post-legacy record key_id = %q, want %q", after.KeyID, created.Key.ID)
	}
	if got := crashKey(t, restarted, created.Key.ID).Used.Total; got != 30 {
		t.Errorf("total counter after legacy artifacts = %d, want 30", got)
	}
	h.mock.requireClean(t)
}
