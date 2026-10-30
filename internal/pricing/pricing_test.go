package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr(v int64) *int64      { return &v }
func strptr(v string) *string { return &v }

func service(t *testing.T, c Config) *Service {
	t.Helper()
	s, err := NewService(c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func quote(t *testing.T, s *Service, model, provider string, tokens Tokens, at time.Time) Quote {
	t.Helper()
	q, err := s.Quote(model, provider, tokens, at, "")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func definition(base RateSet, overrides []Override, multipliers []Multiplier) PricingDefinition {
	if base.CacheWriteByTTL == nil {
		base.CacheWriteByTTL = map[int64]int64{}
	}
	if overrides == nil {
		overrides = []Override{}
	}
	if multipliers == nil {
		multipliers = []Multiplier{}
	}
	return PricingDefinition{Base: base, Overrides: overrides, Multipliers: multipliers}
}

func manual(model, provider string, def PricingDefinition) Config {
	return Config{Prices: []ManualPrice{{Model: model, Provider: provider, Definition: def}}}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The v1 adapter must keep base prices, normalize explicit tiers and the
// legacy context_over_200k spelling, and never turn an unrepresentable tier
// into a flat price.
func TestV1AdapterNormalizesTiersAndZeroDefaults(t *testing.T) {
	entries, err := parseCatalog(loadFixture(t, "models.dev.sample.json"), SourceV1)
	if err != nil {
		t.Fatal(err)
	}
	index := indexCatalog(entries)
	grok, ok := index.exactEntry("grok-4.7", "opencode-go")
	if !ok || !grok.Supported {
		t.Fatal(grok, ok)
	}
	if grok.Definition.Base.Input != 2_000_000 || grok.Definition.Base.Output != 6_000_000 ||
		len(grok.Definition.Overrides) != 1 || grok.Definition.Overrides[0].When.Context.GT == nil ||
		*grok.Definition.Overrides[0].When.Context.GT != 200000 {
		t.Fatalf("grok definition = %+v", grok.Definition)
	}
	// The legacy spelling is normalized when tiers are absent.
	legacy, ok := index.exactEntry("legacy-tier", "fixture")
	if !ok || !legacy.Supported || len(legacy.Definition.Overrides) != 1 ||
		*legacy.Definition.Overrides[0].Rates.Input != 4_000_000 {
		t.Fatal(legacy)
	}
	// Equally-valued tier and legacy objects deduplicate instead of doubling.
	aihubmix, ok := index.exactEntry("grok-4.7", "aihubmix")
	if !ok || !aihubmix.Supported || len(aihubmix.Definition.Overrides) != 1 {
		t.Fatal(aihubmix)
	}
	partial, ok := index.exactEntry("partial", "fixture")
	if !ok || !partial.Supported || partial.Definition.Base.Input != 1_000_000 || partial.Definition.Base.Output != 0 {
		t.Fatal(partial)
	}
	zeros, ok := index.exactEntry("zeros", "fixture")
	if !ok || !zeros.Supported || zeros.Definition.Base.Input != 0 || zeros.Definition.Base.Output != 0 {
		t.Fatal(zeros)
	}
	bad, ok := index.exactEntry("bad-tier", "fixture")
	if !ok || bad.Supported || bad.Definition.Base.Input != 0 {
		t.Fatal("unrepresentable tier was partially imported", bad)
	}
	conflict, ok := index.exactEntry("conflict-tier", "fixture")
	if !ok || conflict.Supported || !contains(conflict.SupportIssues, "conflicting_context_tier") {
		t.Fatal("conflicting tier spellings accepted", conflict)
	}
}

// Equivalent v1 and v2 catalog prices normalize to the same internal
// definition; neither source retains provider-specific DTO shapes.
func TestV1V2EquivalentDefinition(t *testing.T) {
	v1, err := parseCatalog([]byte(`{"p":{"models":{"m":{"cost":{"input":1.25,"output":2,"cache_read":0.1,"cache_write":0.2}}}}}`), SourceV1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := parseCatalog([]byte(`{"p":{"models":{"m":{"canonical_id":"p/m","cost":{"text":{"input":{"price":1.25,"cache":{"read":0.1,"write":0.2}},"output":{"price":2}}}}}}}`), SourceV2)
	if err != nil {
		t.Fatal(err)
	}
	if !v1[0].Supported || !v2[0].Supported || !reflect.DeepEqual(v1[0].Definition, v2[0].Definition) {
		t.Fatalf("equivalent definitions differ:\nv1=%+v\nv2=%+v", v1[0].Definition, v2[0].Definition)
	}
	def := v1[0].Definition
	if def.Base.Input != 1_250_000 || def.Base.Output != 2_000_000 ||
		def.Base.CacheRead != 100_000 || def.Base.CacheWrite != 200_000 || def.Base.Reasoning != nil {
		t.Fatal(def)
	}
}

// V2 text prices, TTL writes, field overrides and service-tier/date
// multipliers are adapted without binary floating point. Unsupported
// conditions yield an unusable zero candidate, not a silently partial price.
func TestV2AdapterOverridesTTLAndUnsupportedConditions(t *testing.T) {
	entries, err := parseCatalog(loadFixture(t, "models.dev.v2.sample.json"), SourceV2)
	if err != nil {
		t.Fatal(err)
	}
	index := indexCatalog(entries)
	gpt, ok := index.exactEntry("gpt-5.5", "opencode")
	if !ok || !gpt.Supported {
		t.Fatal(gpt, ok)
	}
	if gpt.Definition.Base.Input != 5_000_000 || gpt.Definition.Base.CacheRead != 500_000 ||
		gpt.Definition.Base.Output != 30_000_000 || len(gpt.Definition.Overrides) != 2 {
		t.Fatalf("gpt definition = %+v", gpt.Definition)
	}
	claude, ok := index.exactEntry("claude-sonnet-4-6", "opencode")
	if !ok || claude.Definition.Base.CacheWriteByTTL[300] != 3_750_000 ||
		claude.Definition.Base.CacheWriteByTTL[3600] != 6_000_000 || claude.Definition.Base.CacheWrite != 0 {
		t.Fatal(claude)
	}
	databricks, ok := index.exactEntry("databricks-gpt-5-6-sol", "databricks")
	if !ok || len(databricks.Definition.Multipliers) != 2 {
		t.Fatal(databricks)
	}
	if databricks.Definition.Multipliers[0].Factors.Input == nil ||
		*databricks.Definition.Multipliers[0].Factors.Input != "2" ||
		databricks.Definition.Multipliers[1].Factors.Output == nil ||
		*databricks.Definition.Multipliers[1].Factors.Output != "1.5" {
		t.Fatalf("factors = %+v", databricks.Definition.Multipliers)
	}
	region, ok := index.exactEntry("region-only", "opencode")
	if !ok || !region.Supported || region.Definition.Base.Input != 1_000_000 ||
		region.Definition.Base.Output != 2_000_000 || len(region.Definition.Multipliers) != 0 ||
		!contains(region.SupportIssues, "region_multiplier_ignored") ||
		contains(region.SupportIssues, "unsupported_condition") {
		t.Fatal("region multiplier was not ignored as one complete rule", region)
	}
	character, ok := index.exactEntry("character-priced", "opencode")
	if !ok || character.Supported || !contains(character.SupportIssues, "unsupported_price_unit") {
		t.Fatal("character price imported as tokens", character)
	}
	override, ok := index.exactEntry("unsupported-override", "opencode")
	if !ok || override.Supported || override.Definition.Base.Input != 0 || !contains(override.SupportIssues, "unsupported_condition") {
		t.Fatal("unsupported override imported", override)
	}
	duplicate, ok := index.exactEntry("duplicate-ttl", "opencode")
	if !ok || duplicate.Supported || !contains(duplicate.SupportIssues, "duplicate_cache_write_ttl") {
		t.Fatal("normalized duplicate TTL accepted", duplicate)
	}
	timeModel, ok := index.exactEntry("weekday-day", "time-vendor")
	if !ok || !timeModel.Supported || len(timeModel.Definition.Overrides) != 1 {
		t.Fatal(timeModel)
	}
	// The fixture TTL prices drive the real quote path, including a known TTL
	// and the generic write rate for an unlisted TTL.
	s := service(t, DefaultConfig())
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = indexCatalog(entries)
	s.mu.Unlock()
	q := quote(t, s, "claude-sonnet-4-6", "opencode", Tokens{
		CacheWrite: 100, CacheWriteByTTL: map[int64]int64{300: 40, 3600: 60},
	}, time.Now())
	if q.CostMicroUSD != 510 {
		t.Fatal("fixture TTL quote", q)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestCatalogRatePrecisionAndBounds(t *testing.T) {
	for _, tc := range []struct {
		n    string
		want int64
	}{
		{"0", 0}, {"0.000001", 1}, {"0.0000001", 1}, {"1.25", 1250000}, {"1e-6", 1},
	} {
		got, err := catalogRate(json.Number(tc.n))
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, n := range []string{"-1", "9000000001", "1e999999999", "1e-999999999", strings.Repeat("1", 65)} {
		if _, err := catalogRate(json.Number(n)); err == nil {
			t.Fatal(n)
		}
	}
}

// Context thresholds use the complete prompt and are whole-request tiers.
// Overrides apply in order and multipliers target only their named fields.
func TestQuoteOverridesMultipliersAndRounding(t *testing.T) {
	def := definition(
		RateSet{Input: 1_000_000, Output: 2_000_000, CacheWriteByTTL: map[int64]int64{}},
		[]Override{
			{ID: "context", When: Condition{Context: &ContextCondition{GT: ptr(100)}}, Rates: RatePatch{Input: ptr(2_000_000)}},
			{ID: "context-again", When: Condition{Context: &ContextCondition{GT: ptr(100)}}, Rates: RatePatch{Output: ptr(3_000_000)}},
		},
		[]Multiplier{
			{ID: "priority-input", When: Condition{ServiceTier: []string{"priority"}}, Factors: FactorSet{Input: strptr("1.5")}},
		},
	)
	s := service(t, manual("m", "", def))
	for _, tc := range []struct {
		tokens Tokens
		tier   string
		cost   int64
	}{
		{Tokens{Input: 100}, "standard", 100},            // context 100: base input
		{Tokens{Input: 101}, "standard", 202},            // context 101: input override
		{Tokens{Input: 101, Output: 1}, "standard", 205}, // output override applies too
		{Tokens{Input: 101}, "priority", 303},            // 202 * 1.5, ceiled once
	} {
		q, err := s.Quote("m", "", tc.tokens, time.Now(), tc.tier)
		if err != nil {
			t.Fatal(err)
		}
		if q.CostMicroUSD != tc.cost {
			t.Fatalf("%+v %q = %d want %d (%+v)", tc.tokens, tc.tier, q.CostMicroUSD, tc.cost, q)
		}
	}
	// Fractions are summed before one final ceiling, never rounded per field.
	def = definition(
		RateSet{Input: 1, Output: 1, CacheWriteByTTL: map[int64]int64{}},
		nil,
		[]Multiplier{{ID: "half", When: Condition{}, Factors: FactorSet{Input: strptr("0.5"), Output: strptr("0.5")}}},
	)
	s = service(t, manual("half", "", def))
	if q := quote(t, s, "half", "", Tokens{Input: 1, Output: 1}, time.Now()); q.CostMicroUSD != 1 {
		t.Fatal("per-field rounding", q)
	}
}

// Reasoning is a subset of output and inherits output unless explicitly
// priced. TTL-specific writes override the generic write rate.
func TestReasoningAndCacheWriteTTL(t *testing.T) {
	inherit := service(t, manual("m", "", definition(
		RateSet{Input: 0, Output: 10_000_000, CacheWriteByTTL: map[int64]int64{}}, nil, nil)))
	if q := quote(t, inherit, "m", "", Tokens{Output: 1000, Reasoning: 400}, time.Now()); q.CostMicroUSD != 10_000 {
		t.Fatal("inherited reasoning double charged", q)
	}
	independent := service(t, manual("m", "", definition(
		RateSet{Output: 10_000_000, Reasoning: ptr(20_000_000), CacheWriteByTTL: map[int64]int64{}}, nil, nil)))
	if q := quote(t, independent, "m", "", Tokens{Output: 1000, Reasoning: 400}, time.Now()); q.CostMicroUSD != 14_000 {
		t.Fatal(q)
	}
	zero := service(t, manual("m", "", definition(
		RateSet{Output: 10_000_000, Reasoning: ptr(0), CacheWriteByTTL: map[int64]int64{}}, nil, nil)))
	if q := quote(t, zero, "m", "", Tokens{Output: 1000, Reasoning: 400}, time.Now()); q.CostMicroUSD != 6_000 {
		t.Fatal("explicit reasoning zero not honored", q)
	}
	ttl := service(t, manual("m", "", definition(
		RateSet{CacheWrite: 3_000_000, CacheWriteByTTL: map[int64]int64{300: 6_000_000}}, nil, nil)))
	q := quote(t, ttl, "m", "", Tokens{CacheWrite: 100, CacheWriteByTTL: map[int64]int64{300: 40, 600: 60}}, time.Now())
	if q.CostMicroUSD != 420 {
		t.Fatal("TTL override/unknown split", q)
	}
	if _, err := ttl.Quote("m", "", Tokens{CacheWrite: 100, CacheWriteByTTL: map[int64]int64{300: 40}}, time.Now(), ""); err == nil {
		t.Fatal("non-conserving TTL split accepted")
	}
}

// Time conditions are left-closed/right-open, use IANA zones and attribute a
// cross-midnight range to its start weekday.
func TestTimeConditionsWeekdayOwnershipAndTimezone(t *testing.T) {
	monday := int(1)
	def := definition(
		RateSet{Input: 1_000_000, CacheWriteByTTL: map[int64]int64{}},
		[]Override{{
			ID: "night", When: Condition{Time: &TimeCondition{Start: "22:00", End: "02:00", Timezone: "UTC", Weekdays: []int{monday}}},
			Rates: RatePatch{Input: ptr(2_000_000)},
		}}, nil)
	s := service(t, manual("m", "", def))
	for _, tc := range []struct {
		at   string
		cost int64
	}{{"2026-01-05T23:00:00Z", 2}, {"2026-01-06T01:59:59Z", 2}, {"2026-01-06T02:00:00Z", 1}, {"2026-01-04T23:00:00Z", 1}} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		if q := quote(t, s, "m", "", Tokens{Input: 1}, at); q.CostMicroUSD != tc.cost {
			t.Fatalf("%s = %d want %d", tc.at, q.CostMicroUSD, tc.cost)
		}
	}
	tz := service(t, manual("tokyo", "", definition(
		RateSet{Input: 1_000_000, CacheWriteByTTL: map[int64]int64{}},
		[]Override{{ID: "tokyo", When: Condition{Time: &TimeCondition{Start: "08:00", End: "09:00", Timezone: "Asia/Tokyo"}}, Rates: RatePatch{Input: ptr(3_000_000)}}}, nil)))
	at, _ := time.Parse(time.RFC3339, "2026-01-05T23:30:00Z")
	if q := quote(t, tz, "tokyo", "", Tokens{Input: 1}, at); q.CostMicroUSD != 3 {
		t.Fatal("IANA timezone ignored", q)
	}
}

// AND/ANY/not only poison a result when the missing fact can still change it.
func TestConditionUnknownShortCircuit(t *testing.T) {
	base := RateSet{Input: 1_000_000, CacheWriteByTTL: map[int64]int64{}}
	override := RatePatch{Input: ptr(2_000_000)}
	def := definition(base, []Override{{
		ID: "combined",
		When: Condition{Any: []Condition{
			{ServiceTier: []string{"priority"}},
			{Context: &ContextCondition{GT: ptr(100)}},
		}},
		Rates: override,
	}}, nil)
	s := service(t, manual("m", "", def))
	if q := quote(t, s, "m", "", Tokens{Input: 101}, time.Now()); q.CostMicroUSD != 202 || q.Reason == reasonMissingFacts {
		t.Fatal("known ANY branch should win", q)
	}
	if q := quote(t, s, "m", "", Tokens{Input: 50}, time.Now()); q.CostMicroUSD != 0 || q.Reason != reasonMissingFacts {
		t.Fatal("unknown ANY branch should report missing facts", q)
	}
	all := definition(base, []Override{{
		ID: "all",
		When: Condition{All: []Condition{
			{ServiceTier: []string{"priority"}},
			{Context: &ContextCondition{GT: ptr(100)}},
		}},
		Rates: override,
	}}, nil)
	s = service(t, manual("m", "", all))
	if q := quote(t, s, "m", "", Tokens{Input: 101}, time.Now()); q.CostMicroUSD != 0 || q.Reason != reasonMissingFacts {
		t.Fatal("unknown AND branch should report missing facts", q)
	}
	q, err := s.Quote("m", "", Tokens{Input: 101}, time.Now(), "flex")
	if err != nil || q.CostMicroUSD != 101 {
		t.Fatal("known false AND branch should be conclusive", q, err)
	}
	not := definition(base, []Override{{
		ID: "not", When: Condition{Not: &Condition{ServiceTier: []string{"priority"}}}, Rates: override,
	}}, nil)
	s = service(t, manual("m", "", not))
	if q := quote(t, s, "m", "", Tokens{Input: 1}, time.Now()); q.Reason != reasonMissingFacts || q.CostMicroUSD != 0 {
		t.Fatal("unknown NOT should report missing facts", q)
	}
}

// Missing service-tier/date facts make the cost zero rather than guessing the
// condition was false. The matched definition still explains the quote.
func TestMissingFactsReturnZero(t *testing.T) {
	def := definition(
		RateSet{Input: 5_000_000, CacheWriteByTTL: map[int64]int64{}},
		[]Override{{ID: "priority", When: Condition{ServiceTier: []string{"priority"}}, Rates: RatePatch{Input: ptr(9_000_000)}}}, nil)
	s := service(t, manual("m", "", def))
	q, err := s.Quote("m", "", Tokens{Input: 100}, time.Now(), "")
	if err != nil || q.CostMicroUSD != 0 || q.Reason != reasonMissingFacts || q.Source != "manual" {
		t.Fatal(q, err)
	}
	q, err = s.Quote("m", "", Tokens{Input: 100}, time.Now(), "priority")
	if err != nil || q.CostMicroUSD != 900 {
		t.Fatal("final service tier not applied", q, err)
	}
	dated := definition(
		RateSet{Input: 5_000_000, CacheWriteByTTL: map[int64]int64{}},
		[]Override{{ID: "date", When: Condition{Date: &DateCondition{GTE: "2026-01-01"}}, Rates: RatePatch{Input: ptr(9_000_000)}}}, nil)
	s = service(t, manual("dated", "", dated))
	q, err = s.Quote("dated", "", Tokens{Input: 100}, time.Time{}, "")
	if err != nil || q.CostMicroUSD != 0 || q.Reason != reasonMissingFacts {
		t.Fatal("missing date fact not zero", q, err)
	}
}

// Manual prices bind the complete (model, provider) identity. An empty
// provider is a real identity, never a model-wide fallback.
func TestManualIdentityAndValidation(t *testing.T) {
	c := Config{Prices: []ManualPrice{
		{Model: "m", Definition: definition(RateSet{Input: 1_000_000, CacheWriteByTTL: map[int64]int64{}}, nil, nil)},
		{Model: "m", Provider: "p", Definition: definition(RateSet{Input: 2_000_000, CacheWriteByTTL: map[int64]int64{}}, nil, nil)},
	}}
	s := service(t, c)
	if q := quote(t, s, "m", "", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 1 {
		t.Fatal(q)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 2 {
		t.Fatal(q)
	}
	if q := quote(t, s, "m", "other", Tokens{Input: 1}, time.Now()); q.Source != "zero" || q.CostMicroUSD != 0 {
		t.Fatal("manual model-wide leak", q)
	}
	bad := []Config{
		{Source: "v3"},
		{Prices: []ManualPrice{{Model: "m", Provider: "p"}, {Model: "m", Provider: "p"}}},
		{Prices: []ManualPrice{{Model: "m", Definition: definition(RateSet{Reasoning: ptr(-1)}, nil, nil)}}},
		{Prices: []ManualPrice{{Model: "m", Definition: definition(RateSet{CacheWriteByTTL: map[int64]int64{0: 1}}, nil, nil)}}},
		{Prices: []ManualPrice{{Model: "m", Definition: definition(RateSet{}, []Override{{ID: "x", When: Condition{Context: &ContextCondition{GT: ptr(1), GTE: ptr(1)}}}}, nil)}}},
		{Prices: []ManualPrice{{Model: "m", Definition: definition(RateSet{}, nil, []Multiplier{{ID: "x", Factors: FactorSet{Input: strptr("-1")}}})}}},
	}
	for _, cfg := range bad {
		if Validate(cfg) == nil {
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
	}
}

func TestDefaultConfigShape(t *testing.T) {
	raw, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"auto":true,"source":"models_dev_v2","prices":[]}` {
		t.Fatal(string(raw))
	}
}

// Recursive v2 conditions are bounded while parsing, before validCondition can
// run; otherwise a deeply nested 32 MiB catalog could exhaust the stack.
func TestV2ConditionParseBounds(t *testing.T) {
	valid := `{"context":{"gt":1}}`
	for i := 0; i < maxConditionDepth; i++ {
		valid = `{"not":` + valid + `}`
	}
	if _, _, ok := parseV2Condition([]byte(valid)); !ok {
		t.Fatal("condition at depth limit rejected")
	}
	tooDeep := `{"not":` + valid + `}`
	if _, _, ok := parseV2Condition([]byte(tooDeep)); ok {
		t.Fatal("condition beyond depth limit accepted")
	}

	branch := `{"all":[` + strings.Repeat(`{"context":{"gt":1}},`, 15) + `{"context":{"gt":1}}]}`
	nodeHeavy := `{"all":[` + strings.Repeat(branch+`,`, 3) + branch + `]}`
	if _, _, ok := parseV2Condition([]byte(nodeHeavy)); ok {
		t.Fatal("condition beyond node limit accepted")
	}
}
