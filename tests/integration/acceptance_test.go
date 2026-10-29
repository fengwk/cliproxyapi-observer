//go:build integration

package integration

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

// The mock fixture accounts a prompt of 10 tokens (6 uncached, 4 cache reads)
// and 5 completion tokens. With every base rate at 1_000_000 micro-USD per
// million tokens, a complete quote is exactly 6+4+5 = 15 micro-USD.
const (
	// acceptanceProvider is the real host provider identity for the local
	// OpenAI-compatibility fixture.
	acceptanceProvider              = "openai-compatible-" + compatProviderName
	acceptanceInputTokens     int64 = 6
	acceptanceOutputTokens    int64 = 5
	acceptanceCacheReadTokens int64 = 4
	acceptanceTotalTokens     int64 = 15
)

// acceptanceRates mirrors pricing.RateSet. Values are integer micro-USD per
// million tokens, so assertions never involve floating point.
type acceptanceRates struct {
	Input           int64           `json:"input"`
	Output          int64           `json:"output"`
	CacheRead       int64           `json:"cache_read"`
	CacheWrite      int64           `json:"cache_write"`
	CacheWriteByTTL map[int64]int64 `json:"cache_write_by_ttl,omitempty"`
	Reasoning       *int64          `json:"reasoning,omitempty"`
}

type acceptanceRatePatch struct {
	Input           *int64          `json:"input,omitempty"`
	Output          *int64          `json:"output,omitempty"`
	CacheRead       *int64          `json:"cache_read,omitempty"`
	CacheWrite      *int64          `json:"cache_write,omitempty"`
	CacheWriteByTTL map[int64]int64 `json:"cache_write_by_ttl,omitempty"`
}

type acceptanceContext struct {
	GT  *int64 `json:"gt,omitempty"`
	GTE *int64 `json:"gte,omitempty"`
	LT  *int64 `json:"lt,omitempty"`
	LTE *int64 `json:"lte,omitempty"`
}

type acceptanceDate struct {
	GTE string `json:"gte,omitempty"`
	LT  string `json:"lt,omitempty"`
}

type acceptanceTime struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
	Weekdays []int  `json:"weekdays,omitempty"`
}

type acceptanceCondition struct {
	Context     *acceptanceContext    `json:"context,omitempty"`
	ServiceTier []string              `json:"serviceTier,omitempty"`
	Date        *acceptanceDate       `json:"date,omitempty"`
	Time        *acceptanceTime       `json:"time,omitempty"`
	All         []acceptanceCondition `json:"all,omitempty"`
	Any         []acceptanceCondition `json:"any,omitempty"`
	Not         *acceptanceCondition  `json:"not,omitempty"`
}

type acceptanceOverride struct {
	ID    string              `json:"id"`
	When  acceptanceCondition `json:"when"`
	Rates acceptanceRatePatch `json:"rates"`
}

type acceptanceFactors struct {
	Input      *string `json:"input,omitempty"`
	Output     *string `json:"output,omitempty"`
	CacheRead  *string `json:"cache_read,omitempty"`
	CacheWrite *string `json:"cache_write,omitempty"`
	Reasoning  *string `json:"reasoning,omitempty"`
}

type acceptanceMultiplier struct {
	ID      string              `json:"id"`
	When    acceptanceCondition `json:"when"`
	Factors acceptanceFactors   `json:"factors"`
}

type acceptanceDefinition struct {
	Base        acceptanceRates        `json:"base"`
	Overrides   []acceptanceOverride   `json:"overrides,omitempty"`
	Multipliers []acceptanceMultiplier `json:"multipliers,omitempty"`
}

type acceptancePrice struct {
	Model      string               `json:"model"`
	Provider   string               `json:"provider,omitempty"`
	Definition acceptanceDefinition `json:"definition"`
}

type acceptanceMapping struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	CatalogModel string `json:"catalog_model"`
}

type acceptancePricing struct {
	Auto     bool                `json:"auto"`
	Source   string              `json:"source"`
	Prices   []acceptancePrice   `json:"prices"`
	Mappings []acceptanceMapping `json:"mappings,omitempty"`
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
// bucket. Reasoning inherits output because the fixture reports no reasoning.
func acceptanceRatesAll(rate int64) acceptanceRates {
	return acceptanceRates{Input: rate, Output: rate, CacheRead: rate, CacheWrite: rate}
}

func acceptanceDefinitionAll(rate int64) acceptanceDefinition {
	return acceptanceDefinition{Base: acceptanceRatesAll(rate)}
}

// acceptanceManual binds the complete observed (model, provider) identity.
func acceptanceManual(model, provider string, rate int64) acceptancePrice {
	return acceptancePrice{Model: model, Provider: provider, Definition: acceptanceDefinitionAll(rate)}
}

func acceptanceInt64(v int64) *int64 { return &v }

func acceptanceString(v string) *string { return &v }

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

// acceptanceAccountingError is the sanitized health diagnostic the engine
// reports when a usage record cannot be priced. It is the observable proof
// that an accounting attempt happened and was rejected instead of stored.
const acceptanceAccountingError = "usage pricing failed"

// acceptancePriceView is one pricing snapshot entry: a complete observed or
// configured identity together with the source that priced it.
type acceptancePriceView struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Source   string `json:"source"`
	Reason   string `json:"reason"`
}

// acceptancePricesView is the pricing snapshot wire shape.
type acceptancePricesView struct {
	Source string                `json:"source"`
	Prices []acceptancePriceView `json:"prices"`
}

// acceptancePrices fetches the current pricing snapshot. The snapshot lists
// every observed identity, so it exposes the real host model/provider pair even
// when the quote for that identity failed.
func acceptancePrices(t *testing.T, h *harness) acceptancePricesView {
	t.Helper()
	var out acceptancePricesView
	h.api(t, http.MethodGet, "/prices", nil, http.StatusOK, &out)
	return out
}

// acceptanceAwaitUnpricedAccounting waits until an unpriced execution has been
// accounted and rejected. The pricing snapshot observes the real host identity
// and the health diagnostic reports the sanitized accounting failure. Both are
// observable states, so the barrier never sleeps.
func acceptanceAwaitUnpricedAccounting(t *testing.T, h *harness) acceptancePriceView {
	t.Helper()
	var observed acceptancePriceView
	h.await(t, "the unpriced execution is observed and rejected", func() bool {
		var health acceptanceHealth
		h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
		if health.LastError != acceptanceAccountingError {
			return false
		}
		prices := acceptancePrices(t, h)
		if len(prices.Prices) != 1 {
			return false
		}
		observed = prices.Prices[0]
		return observed.Source == "zero" && observed.Model != "" && observed.Provider != ""
	})
	return observed
}

// acceptanceAssertNoFabrication proves that a rejected accounting attempt left
// no ledger row, no key charge and no rollup behind.
func acceptanceAssertNoFabrication(t *testing.T, h *harness) {
	t.Helper()
	if page := h.page(t, "page_size=100"); len(page.Items) != 0 {
		t.Fatalf("rejected accounting fabricated %d ledger rows: %+v", len(page.Items), page.Items)
	}
	keys := h.keys(t)
	if len(keys) != 1 || keys[0].Used != (limitsView{}) {
		t.Fatalf("rejected accounting charged the key: %+v", keys)
	}
	var summary acceptanceSummary
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 0 || summary.CostMicroUSD != 0 {
		t.Fatalf("rejected accounting changed the summary: %+v", summary)
	}
}

// acceptancePriceFixture pins the explicit manual price for every identity the
// local mock fixture can report. An execution whose identity has no price is
// an accounting error that stores no row, so a test that expects an accounted
// execution must name the identity it prices.
func acceptancePriceFixture(t *testing.T, h *harness, rate int64) {
	t.Helper()
	applied := acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{
			acceptanceManual(modelName, acceptanceProvider, rate),
			acceptanceManual(upstreamModel, acceptanceProvider, rate),
			acceptanceManual(modelNameTwo, acceptanceProvider, rate),
			acceptanceManual(upstreamModelTwo, acceptanceProvider, rate),
		},
	})
	if applied.Auto || len(applied.Prices) != 4 {
		t.Fatalf("saved pricing = %+v, want four explicit fixture prices", applied)
	}
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

// acceptanceTimeWindow renders a UTC HH:mm-HH:mm window of the given duration.
func acceptanceTimeWindow(start time.Time, span time.Duration) acceptanceTime {
	start = start.UTC()
	return acceptanceTime{Start: start.Format("15:04"), End: start.Add(span).Format("15:04"), Timezone: "UTC"}
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

// TestPricingDefaultMissingPriceManualAndImmutable proves a missing catalog
// price is an accounting error that stores no row, an explicit manual complete
// (model, provider) identity produces exact micro-USD costs, and later price
// edits never reprice already ingested records or rollups.
func TestPricingDefaultMissingPriceManualAndImmutable(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	empty := acceptancePutPricing(t, h, acceptancePricing{
		Auto:   true,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{},
	})
	if !empty.Auto || empty.Source != "models_dev_v2" || len(empty.Prices) != 0 || len(empty.Mappings) != 0 {
		t.Fatalf("baseline pricing = %+v, want auto v2 with no manual prices", empty)
	}

	known := make(map[string]struct{})
	// A missing price never blocks the client: the answer is unchanged, the
	// accounting attempt fails and nothing is stored.
	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("unpriced inference status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("unpriced inference content = %q, want %q", got, fixtureText)
	}
	observed := acceptanceAwaitUnpricedAccounting(t, h)
	model, provider := observed.Model, observed.Provider
	t.Logf("real host model/provider identity: %q/%q", model, provider)
	// The rejected attempt leaves no fabricated zero-cost row, no charge and
	// no rollup behind.
	acceptanceAssertNoFabrication(t, h)

	// An explicit zero rate is a genuine free price: it is priced, so the
	// zero-cost row is stored and later price edits must never reprice it.
	free := acceptancePutPricing(t, h, acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{acceptanceManual(model, provider, 0)},
	})
	if free.Auto || len(free.Prices) != 1 ||
		free.Prices[0].Model != model || free.Prices[0].Provider != provider ||
		free.Prices[0].Definition.Base.Input != 0 {
		t.Fatalf("saved pricing = %+v, want the explicit free price for %q/%q", free, model, provider)
	}
	freeRecord := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, freeRecord)
	acceptanceAssertCost(t, freeRecord, 0, "manual")

	// An empty-provider price must not act as a model-wide wildcard; the exact
	// complete identity is the only price that may apply.
	one := acceptancePricing{
		Auto:   false,
		Source: "models_dev_v2",
		Prices: []acceptancePrice{
			acceptanceManual(model, "", 9_000_000),
			acceptanceManual(model, provider, 1_000_000),
		},
	}
	applied := acceptancePutPricing(t, h, one)
	if applied.Source != "models_dev_v2" || len(applied.Prices) != 2 ||
		applied.Prices[1].Model != model || applied.Prices[1].Provider != provider ||
		applied.Prices[1].Definition.Base.Input != 1_000_000 {
		t.Fatalf("saved pricing = %+v, want complete manual identity %q/%q", applied, model, provider)
	}

	manual := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, manual)
	acceptanceAssertCost(t, manual, 15, "manual")

	two := one
	two.Prices = []acceptancePrice{
		acceptanceManual(model, provider, 2_000_000),
	}
	acceptancePutPricing(t, h, two)

	repricedNew := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertCost(t, repricedNew, 30, "manual")

	manualAgain := acceptanceRecordByID(t, h, manual.ID)
	acceptanceAssertCost(t, manualAgain, 15, "manual")
	freeAgain := acceptanceRecordByID(t, h, freeRecord.ID)
	acceptanceAssertCost(t, freeAgain, 0, "manual")

	var summary acceptanceSummary
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 3 || summary.CostMicroUSD != 45 {
		t.Errorf("summary = %+v, want requests=3 cost=45; history must not be repriced", summary)
	}
	h.mock.requireClean(t)
}

// TestPricingOverridesLaterWinsMultipliersAndRequestedAt proves all matching
// overrides apply in order with later values winning, every matching multiplier
// composes, and time conditions use the original RequestedAt rather than the
// ingestion time.
func TestPricingOverridesLaterWinsMultipliersAndRequestedAt(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	known := make(map[string]struct{})

	// The default configuration has no manual price: the execution is an
	// accounting error that stores no row, and the real host identity is the
	// only thing the rejected attempt leaves behind.
	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("unpriced inference status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("unpriced inference content = %q, want %q", got, fixtureText)
	}
	observed := acceptanceAwaitUnpricedAccounting(t, h)
	model, provider := observed.Model, observed.Provider
	acceptanceAssertNoFabrication(t, h)

	matching := acceptanceCondition{Context: &acceptanceContext{GTE: acceptanceInt64(10)}}
	nonmatching := acceptanceCondition{Time: &acceptanceTime{
		Start: time.Now().UTC().Add(2 * time.Hour).Format("15:04"), End: time.Now().UTC().Add(3 * time.Hour).Format("15:04"), Timezone: "UTC",
	}}
	definition := acceptanceDefinition{
		Base: acceptanceRatesAll(1_000_000),
		Overrides: []acceptanceOverride{
			{ID: "first-match", When: matching, Rates: acceptanceRatePatch{Input: acceptanceInt64(9_000_000)}},
			{ID: "time-miss", When: nonmatching, Rates: acceptanceRatePatch{Input: acceptanceInt64(8_000_000)}},
			{ID: "later-match", When: matching, Rates: acceptanceRatePatch{Input: acceptanceInt64(3_000_000)}},
		},
		Multipliers: []acceptanceMultiplier{
			{ID: "first-factor", When: matching, Factors: acceptanceFactors{Input: acceptanceString("0.5"), Output: acceptanceString("2")}},
			{ID: "later-factor", When: matching, Factors: acceptanceFactors{Input: acceptanceString("3"), Output: acceptanceString("1.5")}},
		},
	}
	applied := acceptancePutPricing(t, h, acceptancePricing{
		Auto: false, Source: "models_dev_v2",
		Prices: []acceptancePrice{{Model: model, Provider: provider, Definition: definition}},
	})
	if len(applied.Prices) != 1 || len(applied.Prices[0].Definition.Overrides) != 3 ||
		len(applied.Prices[0].Definition.Multipliers) != 2 ||
		applied.Prices[0].Definition.Overrides[2].ID != "later-match" ||
		applied.Prices[0].Definition.Multipliers[1].Factors.Input == nil ||
		*applied.Prices[0].Definition.Multipliers[1].Factors.Input != "3" {
		t.Fatalf("definition did not round-trip in order: %+v", applied)
	}

	// Base input 1M -> matching overrides 9M then 3M; factors 0.5 then 3
	// produce 4.5M. Output 1M -> factors 2 then 1.5 produce 3M.
	// Cost = 6*4.5 + 4*1 + 5*3 = 46 micro-USD. A first-match implementation
	// would charge 100, and ignoring either multiplier would also differ.
	overridden := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, overridden)
	acceptanceAssertCost(t, overridden, 46, "manual")

	// Leave enough setup room, but hold for at most 42 seconds so the harness's
	// 60-second HTTP deadline cannot race the one-minute pricing boundary.
	for {
		remaining := time.Until(time.Now().UTC().Truncate(time.Minute).Add(time.Minute))
		if remaining >= 10*time.Second && remaining <= 40*time.Second {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	windowStart := time.Now().UTC().Truncate(time.Minute)
	windowEnd := windowStart.Add(time.Minute)
	timeDefinition := acceptanceDefinition{
		Base: acceptanceRatesAll(1_000_000),
		Overrides: []acceptanceOverride{{
			ID: "request-time", When: acceptanceCondition{Time: &acceptanceTime{
				Start: windowStart.Format("15:04"), End: windowEnd.Format("15:04"), Timezone: "UTC",
			}}, Rates: acceptanceRatePatch{Input: acceptanceInt64(3_000_000)},
		}},
	}
	acceptancePutPricing(t, h, acceptancePricing{
		Auto: false, Source: "models_dev_v2",
		Prices: []acceptancePrice{{Model: model, Provider: provider, Definition: timeDefinition}},
	})

	gate := h.mock.holdNextRequest()
	type inferResult struct {
		status int
		body   []byte
		err    error
	}
	result := make(chan inferResult, 1)
	go func() {
		callStatus, callBody, callErr := h.rawRequest(http.MethodPost, "/v1/chat/completions", chatBody(false),
			map[string]string{"Authorization": "Bearer " + created.Secret})
		result <- inferResult{status: callStatus, body: callBody, err: callErr}
	}()
	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		gate.open()
		t.Fatal("request did not reach upstream")
	}
	select {
	case <-gate.canceled:
		gate.open()
		t.Fatal("request was canceled while held")
	default:
	}
	if wait := time.Until(windowEnd) + 2*time.Second; wait > 0 {
		time.Sleep(wait)
	}
	gate.open()
	if time.Now().UTC().Before(windowEnd) {
		t.Fatal("test did not hold the request past the pricing window")
	}
	got := <-result
	if got.err != nil || got.status != http.StatusOK {
		t.Fatalf("held inference = %d %v %s", got.status, got.err, truncate(got.body, 400))
	}
	requestTime := acceptanceAwaitNewRecord(t, h, known)
	acceptanceAssertTokens(t, requestTime)
	acceptanceAssertCost(t, requestTime, 27, "manual")
	if requestTime.Time.Before(windowStart) || !requestTime.Time.Before(windowEnd) {
		t.Errorf("record time = %s, want original request inside [%s,%s)", requestTime.Time, windowStart, windowEnd)
	}
	h.mock.requireClean(t)
}

// TestPricingRejectsLegacyWireShape proves the new settings API rejects the
// removed rules/rates contract with 400 instead of silently ignoring it.
func TestPricingRejectsLegacyWireShape(t *testing.T) {
	h := newHarness(t)
	settings := acceptanceGetSettings(t, h)
	for name, pricing := range map[string]any{
		"rules": map[string]any{
			"auto": false, "source": "models_dev_v2", "prices": []any{},
			"rules": []any{map[string]any{"id": "old", "model": "m", "input_tokens_gt": 1, "rates": acceptanceRatesAll(1_000_000)}},
		},
		"rates": map[string]any{
			"auto": false, "source": "models_dev_v2",
			"prices": []any{map[string]any{"model": "m", "provider": "p", "rates": acceptanceRatesAll(1_000_000)}},
		},
	} {
		legacy := map[string]any{}
		for key, value := range settings {
			legacy[key] = value
		}
		legacy["pricing"] = pricing
		h.api(t, http.MethodPut, "/settings", legacy, http.StatusBadRequest, nil)
		t.Logf("legacy %s payload rejected with 400", name)
	}
	var unchanged acceptanceSettings
	h.api(t, http.MethodGet, "/settings", nil, http.StatusOK, &unchanged)
	if unchanged.Pricing.Source != "models_dev_v2" || len(unchanged.Pricing.Prices) != 0 ||
		len(unchanged.Pricing.Mappings) != 0 {
		t.Fatalf("rejected legacy payload changed settings: %+v", unchanged.Pricing)
	}
	h.mock.requireClean(t)
}

// TestSettingsHealthCompactRestartPreserveKeysAndCosts proves settings survive
// a real host restart, health advertises schema version 3, compaction reports a
// nonempty database, keys remain queryable, historical costs are unchanged, and
// post-restart requests use the persisted complete manual price.
func TestSettingsHealthCompactRestartPreserveKeysAndCosts(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	acceptancePutPricing(t, h, acceptancePricing{Auto: true, Source: "models_dev_v2", Prices: []acceptancePrice{}})

	known := make(map[string]struct{})
	// Without a price the execution is an accounting error that stores no row;
	// the real host identity is the only thing the rejected attempt leaves
	// behind.
	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("unpriced inference status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("unpriced inference content = %q, want %q", got, fixtureText)
	}
	observed := acceptanceAwaitUnpricedAccounting(t, h)
	model, provider := observed.Model, observed.Provider
	acceptanceAssertNoFabrication(t, h)

	pricing := acceptancePricing{
		Auto: false, Source: "models_dev_v2",
		Prices: []acceptancePrice{{Model: model, Provider: provider, Definition: acceptanceDefinitionAll(1_000_000)}},
	}
	settings := acceptanceGetSettings(t, h)
	settings["capture_enabled"] = true
	settings["capture_mode"] = "headers_and_body"
	settings["max_body_bytes"] = 262144
	settings["max_body_storage_bytes"] = int64(32 << 20)
	settings["raw_retention_hours"] = 48
	settings["daily_retention_days"] = 730
	settings["pricing"] = pricing
	var updated acceptanceSettings
	h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, &updated)
	if !updated.CaptureEnabled || updated.MaxBodyBytes != 262144 || updated.MaxBodyStorageBytes != int64(32<<20) ||
		updated.RawRetentionHours != 48 || updated.DailyRetentionDays != 730 {
		t.Fatalf("saved settings = %+v, want capture=true body=262144 storage=%d raw=48 daily=730", updated, int64(32<<20))
	}
	if updated.Pricing.Auto || len(updated.Pricing.Prices) != 1 ||
		updated.Pricing.Prices[0].Model != model || updated.Pricing.Prices[0].Provider != provider {
		t.Fatalf("saved pricing = %+v, want one complete manual price", updated.Pricing)
	}

	priced := acceptanceInferAndAwait(t, h, created.Secret, known)
	acceptanceAssertTokens(t, priced)
	acceptanceAssertCost(t, priced, 15, "manual")

	// The stored row and the completion callback must settle before restart; an
	// HTTP response alone is not a lifecycle fence. The rejected unpriced
	// execution stores no row, so the ledger holds exactly the priced one.
	h.awaitGracefulRestartReady(t, 1)

	var health acceptanceHealth
	h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
	if health.SchemaVersion != 3 {
		t.Errorf("health schema_version = %d, want 3", health.SchemaVersion)
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
	if persisted.Pricing.Auto || persisted.Pricing.Source != "models_dev_v2" ||
		len(persisted.Pricing.Prices) != 1 || persisted.Pricing.Prices[0].Model != model ||
		persisted.Pricing.Prices[0].Provider != provider ||
		persisted.Pricing.Prices[0].Definition.Base.Input != 1_000_000 {
		t.Errorf("pricing after restart = %+v, want the complete manual price saved before restart", persisted.Pricing)
	}

	for _, before := range beforeRecords {
		after := acceptanceRecordByID(t, restarted, before.ID)
		if after.CostMicroUSD != before.CostMicroUSD || after.PriceSource != before.PriceSource ||
			after.ResponseServiceTier != before.ResponseServiceTier {
			t.Errorf("record %s after restart pricing = %d/%q/%q, want unchanged %d/%q/%q",
				before.ID, after.CostMicroUSD, after.PriceSource, after.ResponseServiceTier,
				before.CostMicroUSD, before.PriceSource, before.ResponseServiceTier)
		}
	}
	var afterSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &afterSummary)
	if afterSummary.Requests != beforeSummary.Requests || afterSummary.CostMicroUSD != beforeSummary.CostMicroUSD {
		t.Errorf("summary after restart = %+v, want unchanged %+v", afterSummary, beforeSummary)
	}

	afterRestart := acceptanceInferAndAwait(t, restarted, created.Secret, known)
	acceptanceAssertTokens(t, afterRestart)
	acceptanceAssertCost(t, afterRestart, 15, "manual")

	var finalSummary acceptanceSummary
	restarted.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &finalSummary)
	if finalSummary.Requests != beforeSummary.Requests+1 || finalSummary.CostMicroUSD != beforeSummary.CostMicroUSD+15 {
		t.Errorf("summary after restart = %+v, want requests=%d cost=%d",
			finalSummary, beforeSummary.Requests+1, beforeSummary.CostMicroUSD+15)
	}
	h.mock.requireClean(t)
}
