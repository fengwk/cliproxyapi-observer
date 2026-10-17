//go:build integration

package integration

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

// The mock fixture accounts a prompt of 10 tokens (6 uncached, 4 cache reads)
// and 5 completion tokens. With every rate at 1_000_000 micro-USD per million
// tokens, a complete quote is exactly 6+4+5 = 15 micro-USD.
const (
	acceptanceInputTokens     int64 = 6
	acceptanceOutputTokens    int64 = 5
	acceptanceCacheReadTokens int64 = 4
	acceptanceTotalTokens     int64 = 15
)

// acceptanceRates is the wire shape of pricing.Rates. All values are integer
// micro-USD per million tokens so the assertions never involve floating point.
type acceptanceRates struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}

type acceptancePrice struct {
	Model  string          `json:"model"`
	Rates  acceptanceRates `json:"rates"`
	Source string          `json:"source,omitempty"`
}

type acceptanceRule struct {
	ID            string          `json:"id"`
	Model         string          `json:"model"`
	InputTokensGT *int64          `json:"input_tokens_gt,omitempty"`
	TimeRange     string          `json:"time_range,omitempty"`
	Rates         acceptanceRates `json:"rates"`
}

type acceptancePricing struct {
	Auto   bool              `json:"auto"`
	Prices []acceptancePrice `json:"prices"`
	Rules  []acceptanceRule  `json:"rules"`
}

type acceptanceSettings struct {
	CaptureEnabled      bool              `json:"capture_enabled"`
	MaxBodyBytes        int               `json:"max_body_bytes"`
	MaxBodyStorageBytes int64             `json:"max_body_storage_bytes"`
	RawRetentionHours   int               `json:"raw_retention_hours"`
	DailyRetentionDays  int               `json:"daily_retention_days"`
	Pricing             acceptancePricing `json:"pricing"`
}

type acceptanceHealth struct {
	DatabaseBytes   int64  `json:"database_bytes"`
	DroppedCaptures uint64 `json:"dropped_captures"`
	PendingUsage    int    `json:"pending_usage"`
	ActiveRequests  int    `json:"active_requests"`
	LastError       string `json:"last_error,omitempty"`
	Version         string `json:"version"`
	SchemaVersion   int    `json:"schema_version"`
}

type acceptanceSummary struct {
	Requests     int64 `json:"requests"`
	Failures     int64 `json:"failures"`
	CostMicroUSD int64 `json:"cost_micro_usd"`
}

// acceptanceRatesAll builds disjoint bucket rates with one rate per token
// bucket. Reasoning is omitted because the fixture reports no reasoning tokens.
func acceptanceRatesAll(rate int64) acceptanceRates {
	return acceptanceRates{Input: rate, Output: rate, CacheRead: rate, CacheWrite: rate}
}

func acceptanceInt64(v int64) *int64 { return &v }

// acceptanceGetSettings fetches the effective settings as an untyped map so a
// test can replace only pricing and preserve every other field the host
// returned.
func acceptanceGetSettings(t *testing.T, h *harness) map[string]any {
	t.Helper()
	var settings map[string]any
	h.api(t, http.MethodGet, "/settings", nil, http.StatusOK, &settings)
	return settings
}

// acceptancePutPricing replaces only the nested pricing object. This keeps the
// test independent of engine packages and of settings fields added later.
func acceptancePutPricing(t *testing.T, h *harness, cfg acceptancePricing) acceptancePricing {
	t.Helper()
	settings := acceptanceGetSettings(t, h)
	settings["pricing"] = cfg
	var updated acceptanceSettings
	h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, &updated)
	return updated.Pricing
}

// acceptanceAwaitNewRecord waits for a usage record whose ID was not observed
// before and records it as known. Selecting by ID avoids depending on request
// page ordering.
func acceptanceAwaitNewRecord(t *testing.T, h *harness, known map[string]struct{}) recordView {
	t.Helper()
	var found recordView
	h.await(t, "a new usage record is ingested", func() bool {
		for _, record := range h.page(t, "page_size=100").Items {
			if _, seen := known[record.ID]; !seen {
				found = record
				return true
			}
		}
		return false
	})
	if found.ID == "" {
		t.Fatal("a new usage record was reported without an ID")
	}
	known[found.ID] = struct{}{}
	return found
}

func acceptanceRecordByID(t *testing.T, h *harness, id string) recordView {
	t.Helper()
	for _, record := range h.page(t, "page_size=100").Items {
		if record.ID == id {
			return record
		}
	}
	t.Fatalf("usage record %q is missing from the request page", id)
	return recordView{}
}

func acceptanceAssertTokens(t *testing.T, record recordView) {
	t.Helper()
	tokens := record.Tokens
	if tokens.Input != acceptanceInputTokens || tokens.Output != acceptanceOutputTokens ||
		tokens.CacheRead != acceptanceCacheReadTokens || tokens.CacheWrite != 0 ||
		tokens.Reasoning != 0 || tokens.Total != acceptanceTotalTokens {
		t.Errorf("record %s tokens = %+v, want input=%d output=%d cache_read=%d cache_write=0 reasoning=0 total=%d",
			record.ID, tokens, acceptanceInputTokens, acceptanceOutputTokens, acceptanceCacheReadTokens, acceptanceTotalTokens)
	}
}

// acceptanceTimeRange renders a UTC HH:mm-HH:mm window. start and end are
// chosen minutes apart so the window is never empty and can wrap midnight.
func acceptanceTimeRange(start time.Time, span time.Duration) string {
	start = start.UTC()
	return start.Format("15:04") + "-" + start.Add(span).Format("15:04")
}

// acceptanceInferAndAwait issues a synchronous inference and returns its newly
// observed execution record.
func acceptanceInferAndAwait(t *testing.T, h *harness, secret string, known map[string]struct{}) recordView {
	t.Helper()
	status, body := h.infer(t, secret, false)
	if status != http.StatusOK {
		t.Fatalf("inference status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("inference content = %q, want %q", got, fixtureText)
	}
	return acceptanceAwaitNewRecord(t, h, known)
}

func acceptanceAssertCost(t *testing.T, record recordView, wantCost int64, wantSource string) {
	t.Helper()
	if record.CostMicroUSD != wantCost {
		t.Errorf("record %s cost = %d micro-USD, want exactly %d", record.ID, record.CostMicroUSD, wantCost)
	}
	if record.PriceSource != wantSource {
		t.Errorf("record %s price source = %q, want %q", record.ID, record.PriceSource, wantSource)
	}
}

// TestPricingDefaultMissingPriceManualAndImmutable proves unpriced inference is
// charged zero without being blocked, manual exact-model prices produce exact
// integer micro-USD costs, and changing the saved price never reprices already
// ingested records or rollups.
func TestPricingDefaultMissingPriceManualAndImmutable(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	// Pin the deterministic baseline explicitly: no manual price, no rules and
	// auto lookup disabled. The test must not depend on engine package imports.
	empty := acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Prices: []acceptancePrice{},
		Rules:  []acceptanceRule{},
	})
	if empty.Auto || len(empty.Prices) != 0 || len(empty.Rules) != 0 {
		t.Fatalf("baseline pricing = %+v, want auto=false with no prices or rules", empty)
	}

	known := make(map[string]struct{})
	unpriced := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, unpriced)
	acceptanceAssertCost(t, unpriced, 0, "unpriced")

	// Configure the exact model identity carried by the ingested record rather
	// than assuming the client alias or upstream alias.
	model := unpriced.Model
	one := acceptancePricing{
		Auto:   false,
		Prices: []acceptancePrice{{Model: model, Rates: acceptanceRatesAll(1_000_000), Source: "manual"}},
		Rules:  []acceptanceRule{},
	}
	applied := acceptancePutPricing(t, h, one)
	if applied.Auto || len(applied.Prices) != 1 || applied.Prices[0].Model != model {
		t.Fatalf("saved pricing = %+v, want one manual price for %q", applied, model)
	}

	manual := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, manual)
	acceptanceAssertCost(t, manual, 15, "manual")
	if manual.RuleID != "" {
		t.Errorf("manual record %s rule_id = %q, want empty", manual.ID, manual.RuleID)
	}

	two := acceptancePricing{
		Auto:   false,
		Prices: []acceptancePrice{{Model: model, Rates: acceptanceRatesAll(2_000_000), Source: "manual"}},
		Rules:  []acceptanceRule{},
	}
	acceptancePutPricing(t, h, two)

	// The new request uses the new saved price, while the earlier executions
	// keep the quote captured at ingestion time.
	repricedNew := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, repricedNew, 30, "manual")

	manualAgain := acceptanceRecordByID(t, h, manual.ID)
	acceptanceAssertCost(t, manualAgain, 15, "manual")
	unpricedAgain := acceptanceRecordByID(t, h, unpriced.ID)
	acceptanceAssertCost(t, unpricedAgain, 0, "unpriced")

	var summary acceptanceSummary
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 3 {
		t.Errorf("summary requests = %d, want 3", summary.Requests)
	}
	if summary.CostMicroUSD != 45 {
		t.Errorf("summary cost = %d micro-USD, want exactly 15+30=45; history must not be repriced", summary.CostMicroUSD)
	}
	h.mock.requireClean(t)
}

// TestPricingRulesFirstMatchThresholdAndTime proves ordered rules are evaluated
// top-to-bottom, the threshold is strict greater-than on full prompt context,
// the UTC time window is enforced, and each successful rule produces an
// exact micro-USD cost. It also pins rule > manual precedence.
func TestPricingRulesFirstMatchThresholdAndTime(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	acceptancePutPricing(t, h, acceptancePricing{Auto: false, Prices: []acceptancePrice{}, Rules: []acceptanceRule{}})

	known := make(map[string]struct{})
	baseline := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, baseline)
	acceptanceAssertCost(t, baseline, 0, "unpriced")
	model := baseline.Model

	now := time.Now().UTC()
	matchingWindow := acceptanceTimeRange(now.Add(-5*time.Minute), 10*time.Minute)
	nonmatchingWindow := acceptanceTimeRange(now.Add(2*time.Hour), 10*time.Minute)

	// Manual is intentionally more expensive than every rule so a rule match
	// proves rules take precedence. Rule 1 has full prompt == 10, which must not
	// satisfy "> 10". Rule 2 satisfies the threshold but misses the time window.
	// Rule 3 is the first full match. Rule 4 would also match but comes later.
	base := acceptancePricing{
		Auto:   false,
		Prices: []acceptancePrice{{Model: model, Rates: acceptanceRatesAll(7_000_000), Source: "manual"}},
		Rules: []acceptanceRule{
			{ID: "threshold-equal", Model: model, InputTokensGT: acceptanceInt64(10), TimeRange: matchingWindow, Rates: acceptanceRatesAll(5_000_000)},
			{ID: "time-miss", Model: model, InputTokensGT: acceptanceInt64(9), TimeRange: nonmatchingWindow, Rates: acceptanceRatesAll(4_000_000)},
			{ID: "winner", Model: model, InputTokensGT: acceptanceInt64(9), TimeRange: matchingWindow, Rates: acceptanceRatesAll(3_000_000)},
			{ID: "later-match", Model: model, Rates: acceptanceRatesAll(1_000_000)},
		},
	}
	acceptancePutPricing(t, h, base)

	winner := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, winner)
	acceptanceAssertCost(t, winner, 45, "rule")
	if winner.RuleID != "winner" {
		t.Errorf("record %s rule_id = %q, want first full match %q", winner.ID, winner.RuleID, "winner")
	}

	// Moving the later catch-all ahead proves first-match ordering rather than
	// any-match or last-match behavior.
	reordered := base
	reordered.Rules = []acceptanceRule{
		{ID: "later-match", Model: model, Rates: acceptanceRatesAll(1_000_000)},
		{ID: "winner", Model: model, InputTokensGT: acceptanceInt64(9), TimeRange: matchingWindow, Rates: acceptanceRatesAll(3_000_000)},
	}
	acceptancePutPricing(t, h, reordered)

	later := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, later, 15, "rule")
	if later.RuleID != "later-match" {
		t.Errorf("record %s rule_id = %q, want %q after reordering", later.ID, later.RuleID, "later-match")
	}

	// Removing all rules falls back to the immutable manual rate for new
	// requests; earlier rule-priced records keep their original quotes.
	manualOnly := base
	manualOnly.Rules = []acceptanceRule{}
	acceptancePutPricing(t, h, manualOnly)

	manual := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, manual, 105, "manual")
	if manual.RuleID != "" {
		t.Errorf("manual record %s rule_id = %q, want empty", manual.ID, manual.RuleID)
	}

	acceptanceAssertCost(t, acceptanceRecordByID(t, h, winner.ID), 45, "rule")
	acceptanceAssertCost(t, acceptanceRecordByID(t, h, later.ID), 15, "rule")

	var summary acceptanceSummary
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 4 {
		t.Errorf("summary requests = %d, want 4", summary.Requests)
	}
	if summary.CostMicroUSD != 165 {
		t.Errorf("summary cost = %d micro-USD, want exactly 45+15+105=165; history must not be repriced", summary.CostMicroUSD)
	}
	h.mock.requireClean(t)
}

// TestSettingsHealthCompactRestartPreserveKeysAndCosts proves settings survive
// a real host restart, health advertises schema version 2, compaction reports a
// nonempty database, keys remain queryable, historical costs are unchanged, and
// post-restart requests use the persisted prices.
func TestSettingsHealthCompactRestartPreserveKeysAndCosts(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	acceptancePutPricing(t, h, acceptancePricing{Auto: false, Prices: []acceptancePrice{}, Rules: []acceptanceRule{}})

	known := make(map[string]struct{})
	unpriced := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, unpriced, 0, "unpriced")
	model := unpriced.Model

	settings := acceptanceGetSettings(t, h)
	settings["capture_enabled"] = true
	settings["max_body_bytes"] = 262144
	settings["max_body_storage_bytes"] = int64(32 << 20)
	settings["raw_retention_hours"] = 48
	settings["daily_retention_days"] = 730
	settings["pricing"] = acceptancePricing{
		Auto:   false,
		Prices: []acceptancePrice{{Model: model, Rates: acceptanceRatesAll(1_000_000), Source: "manual"}},
		Rules:  []acceptanceRule{},
	}
	var updated acceptanceSettings
	h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, &updated)
	if !updated.CaptureEnabled || updated.MaxBodyBytes != 262144 || updated.MaxBodyStorageBytes != int64(32<<20) ||
		updated.RawRetentionHours != 48 || updated.DailyRetentionDays != 730 {
		t.Fatalf("saved settings = %+v, want capture=true body=262144 storage=%d raw=48 daily=730", updated, int64(32<<20))
	}
	if updated.Pricing.Auto || len(updated.Pricing.Prices) != 1 || updated.Pricing.Prices[0].Model != model {
		t.Fatalf("saved pricing = %+v, want one manual price for %q", updated.Pricing, model)
	}

	priced := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, priced)
	acceptanceAssertCost(t, priced, 15, "manual")

	// Wait for the execution to settle so health and restart observe a quiescent
	// store rather than a transient in-flight request.
	h.await(t, "all requests settle before health inspection", func() bool {
		var health acceptanceHealth
		h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
		return health.ActiveRequests == 0 && health.PendingUsage == 0
	})

	var health acceptanceHealth
	h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
	if health.SchemaVersion != 2 {
		t.Errorf("health schema_version = %d, want 2", health.SchemaVersion)
	}
	if health.DatabaseBytes <= 0 {
		t.Errorf("health database_bytes = %d, want > 0", health.DatabaseBytes)
	}

	var compacted acceptanceHealth
	h.api(t, http.MethodPost, "/compact", nil, http.StatusOK, &compacted)
	if compacted.DatabaseBytes <= 0 {
		t.Errorf("compact database_bytes = %d, want > 0", compacted.DatabaseBytes)
	}

	var beforeSummary acceptanceSummary
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &beforeSummary)
	beforeRecords := h.page(t, "page_size=100").Items

	// Capture the Key's accumulated cost windows before restart. Comparing the
	// whole Used value avoids depending on the helper's internal field layout.
	beforeKeys := h.keys(t)
	if len(beforeKeys) != 1 || beforeKeys[0].ID != created.Key.ID {
		t.Fatalf("keys before restart = %+v, want exactly key %q", beforeKeys, created.Key.ID)
	}

	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if exitErr := h.waitError(); exitErr != nil {
		t.Fatalf("normal SIGTERM shutdown did not exit cleanly (forced kill is not a graceful restart): %v", exitErr)
	}

	afterKeys := restarted.keys(t)
	if len(afterKeys) != 1 || afterKeys[0].ID != created.Key.ID {
		t.Fatalf("keys after restart = %+v, want exactly key %q", afterKeys, created.Key.ID)
	}
	if !reflect.DeepEqual(beforeKeys[0].Used, afterKeys[0].Used) {
		t.Errorf("key used accumulators after restart = %+v, want unchanged %+v", afterKeys[0].Used, beforeKeys[0].Used)
	}

	var persisted acceptanceSettings
	restarted.api(t, http.MethodGet, "/settings", nil, http.StatusOK, &persisted)
	if !persisted.CaptureEnabled || persisted.MaxBodyBytes != 262144 || persisted.MaxBodyStorageBytes != int64(32<<20) ||
		persisted.RawRetentionHours != 48 || persisted.DailyRetentionDays != 730 {
		t.Errorf("settings after restart = %+v, want the values saved before restart", persisted)
	}
	if persisted.Pricing.Auto || len(persisted.Pricing.Prices) != 1 ||
		persisted.Pricing.Prices[0].Model != model || persisted.Pricing.Prices[0].Rates.Input != 1_000_000 {
		t.Errorf("pricing after restart = %+v, want the manual price saved before restart", persisted.Pricing)
	}

	// Every pre-restart execution keeps its exact ID, source and cost, and the
	// summary rollup is unchanged.
	for _, before := range beforeRecords {
		after := acceptanceRecordByID(t, restarted, before.ID)
		if after.CostMicroUSD != before.CostMicroUSD || after.PriceSource != before.PriceSource {
			t.Errorf("record %s after restart cost/source = %d/%q, want unchanged %d/%q",
				before.ID, after.CostMicroUSD, after.PriceSource, before.CostMicroUSD, before.PriceSource)
		}
	}
	var afterSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &afterSummary)
	if afterSummary.Requests != beforeSummary.Requests || afterSummary.CostMicroUSD != beforeSummary.CostMicroUSD {
		t.Errorf("summary after restart = %+v, want unchanged %+v", afterSummary, beforeSummary)
	}

	// A request after restart must be priced by the persisted settings.
	afterRestart := acceptanceInferAndAwait(t, restarted, created.Secret, known)
	acceptanceAssertTokens(t, afterRestart)
	acceptanceAssertCost(t, afterRestart, 15, "manual")

	var finalSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &finalSummary)
	if finalSummary.Requests != beforeSummary.Requests+1 {
		t.Errorf("summary requests after restart = %d, want %d", finalSummary.Requests, beforeSummary.Requests+1)
	}
	if finalSummary.CostMicroUSD != beforeSummary.CostMicroUSD+15 {
		t.Errorf("summary cost after restart = %d, want %d", finalSummary.CostMicroUSD, beforeSummary.CostMicroUSD+15)
	}
	h.mock.requireClean(t)
}
