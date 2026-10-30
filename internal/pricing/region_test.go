package pricing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The fixed anthropic cost contains a fast service-tier multiplier and a
// region multiplier. The fast rule and TTL rates must remain usable while the
// complete region rule is absent from the imported definition and quotes.
func TestV2RegionMultiplierIgnoredFromFixedCost(t *testing.T) {
	entries, err := parseCatalog(loadFixture(t, "models.dev.v2.anthropic.json"), SourceV2)
	if err != nil {
		t.Fatal(err)
	}
	index := indexCatalog(entries)
	opus, ok := index.exactEntry("claude-opus-5-5", "anthropic")
	if !ok || !opus.Supported {
		t.Fatal(opus, ok)
	}
	def := opus.Definition
	if def.Base.Input != 4_000_000 || def.Base.Output != 20_000_000 ||
		def.Base.CacheRead != 200_000 || def.Base.CacheWrite != 0 ||
		def.Base.CacheWriteByTTL[300] != 5_000_000 || def.Base.CacheWriteByTTL[3600] != 8_000_000 {
		t.Fatalf("base/TTL rates = %+v", def.Base)
	}
	if !contains(opus.SupportIssues, "region_multiplier_ignored") ||
		!contains(opus.SupportIssues, "web_search_cost_not_included") ||
		contains(opus.SupportIssues, "unsupported_condition") {
		t.Fatalf("support issues = %v", opus.SupportIssues)
	}
	if len(def.Multipliers) != 1 || def.Multipliers[0].ID != "multiplier-0" ||
		len(def.Multipliers[0].When.ServiceTier) != 1 ||
		def.Multipliers[0].When.ServiceTier[0] != "fast" ||
		def.Multipliers[0].Factors.Input == nil || *def.Multipliers[0].Factors.Input != "2" {
		t.Fatalf("kept multipliers = %+v", def.Multipliers)
	}

	s := service(t, DefaultConfig())
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = index
	s.mu.Unlock()
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	tokens := Tokens{
		Input: 1_000_000, Output: 100_000, CacheRead: 100_000,
		CacheWrite: 100, CacheWriteByTTL: map[int64]int64{300: 40, 3600: 60},
	}
	for _, tc := range []struct {
		tier string
		want int64
	}{
		{"standard", 6_020_680},
		{"fast", 12_041_360},
	} {
		q, err := s.Quote("claude-opus-5-5", "anthropic", tokens, at, tc.tier)
		if err != nil || !q.PricingSupported || q.CostMicroUSD != tc.want ||
			!contains(q.SupportIssues, "region_multiplier_ignored") {
			t.Fatalf("%s quote = %+v, err = %v", tc.tier, q, err)
		}
	}
}

// A region anywhere inside all/any/not discards that complete multiplier, not
// just the region branch. Other rules keep their order and source indexes.
func TestV2NestedRegionMultiplierSkipsWholeRule(t *testing.T) {
	cost := `{
		"text": {"input": {"price": 1}, "output": {"price": 2}},
		"multipliers": [
			{"when": {"service_tier": {"equals": "priority"}}, "factor": 1.5},
			{"when": {"all": [
				{"service_tier": {"equals": "priority"}},
				{"any": [
					{"context": {"gt": 100}},
					{"not": {"region": {"in": ["us", "eu"]}}}
				]}
			]}, "factor": 9},
			{"when": {"date": {"gte": "2027-01-01"}}, "factor": 2}
		]
	}`
	def, issues, ok := parseV2Definition(json.RawMessage(cost))
	if !ok || !contains(issues, "region_multiplier_ignored") || len(def.Multipliers) != 2 {
		t.Fatalf("definition = %+v, issues = %v, ok = %v", def, issues, ok)
	}
	for i, want := range []string{"multiplier-0", "multiplier-2"} {
		if def.Multipliers[i].ID != want {
			t.Fatalf("multiplier IDs = %+v", def.Multipliers)
		}
	}
	s := service(t, manual("m", "", def))
	at := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	q, err := s.Quote("m", "", Tokens{Input: 101}, at, "priority")
	if err != nil || q.CostMicroUSD != 303 {
		t.Fatalf("skipped nested region rule changed the quote: %+v, err = %v", q, err)
	}
}

// Region tolerance is limited to the multiplier import path. Unsupported
// fields, malformed region operators, bad factors and override region
// conditions must still invalidate the whole model price.
func TestV2RegionMultiplierValidationStillRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cost  string
		issue string
	}{
		{
			name:  "unknown condition field",
			cost:  `{"text":{"input":{"price":1},"output":{"price":2}},"multipliers":[{"when":{"region":{"equals":"us"},"future":true},"factor":1.1}]}`,
			issue: "unsupported_condition",
		},
		{
			name:  "unsupported nested condition",
			cost:  `{"text":{"input":{"price":1},"output":{"price":2}},"multipliers":[{"when":{"all":[{"region":{"equals":"us"}},{"thinking":{"equals":true}}]},"factor":1.1}]}`,
			issue: "unsupported_condition_field:thinking",
		},
		{
			name:  "invalid region operator",
			cost:  `{"text":{"input":{"price":1},"output":{"price":2}},"multipliers":[{"when":{"region":{"gte":"us"}},"factor":1.1}]}`,
			issue: "unsupported_condition_field:region",
		},
		{
			name:  "invalid factor after valid region",
			cost:  `{"text":{"input":{"price":1},"output":{"price":2}},"multipliers":[{"when":{"region":{"in":["us","eu"]}},"factor":{"text":-1}}]}`,
			issue: "invalid_multiplier_factor",
		},
		{
			name:  "region override",
			cost:  `{"text":{"input":{"price":1,"overrides":[{"when":{"region":{"equals":"us"}},"price":2}]},"output":{"price":2}}}`,
			issue: "unsupported_condition_field:region",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, issues, ok := parseV2Definition(json.RawMessage(tc.cost))
			if ok || def.Base.Input != 0 || !contains(issues, tc.issue) {
				t.Fatalf("definition = %+v, issues = %v, ok = %v", def, issues, ok)
			}
		})
	}
	if _, issues, ok := parseV2Condition(json.RawMessage(`{"region":{"equals":"us"}}`)); ok ||
		!contains(issues, "unsupported_condition_field:region") {
		t.Fatalf("strict condition parsing accepted region: %v, %v", issues, ok)
	}
}

// Region recognition must not bypass the existing recursion, node, list or
// rule-count limits before a rule can be skipped.
func TestV2RegionMultiplierKeepsImportBounds(t *testing.T) {
	tooDeep := `{"region":{"equals":"us"}}`
	for i := 0; i <= maxConditionDepth; i++ {
		tooDeep = `{"not":` + tooDeep + `}`
	}
	if _, _, ok := parseV2Multipliers(json.RawMessage(`[{"when":` + tooDeep + `,"factor":1.1}]`)); ok {
		t.Fatal("region multiplier beyond depth limit accepted")
	}

	leaf := `{"context":{"gt":1}}`
	branch := `{"all":[` + strings.Repeat(leaf+`,`, maxConditionList-1) + leaf + `]}`
	nodeHeavy := `{"region":{"equals":"us"},"all":[` + strings.Repeat(branch+`,`, 3) + branch + `]}`
	if _, _, ok := parseV2Multipliers(json.RawMessage(`[{"when":` + nodeHeavy + `,"factor":1.1}]`)); ok {
		t.Fatal("region multiplier beyond node limit accepted")
	}

	listHeavy := `{"region":{"equals":"us"},"any":[` +
		strings.Repeat(`{"context":{"gt":1}},`, maxConditionList) + `{"context":{"gt":1}}]}`
	if _, _, ok := parseV2Multipliers(json.RawMessage(`[{"when":` + listHeavy + `,"factor":1.1}]`)); ok {
		t.Fatal("region multiplier beyond list limit accepted")
	}

	ruleHeavy := `[` + strings.Repeat(`{"when":{"region":{"equals":"us"}},"factor":1.1},`, maxRulesPerModel) +
		`{"when":{"region":{"equals":"us"}},"factor":1.1}]`
	if _, issues, ok := parseV2Multipliers(json.RawMessage(ruleHeavy)); ok || !contains(issues, "invalid_multipliers") {
		t.Fatalf("region multipliers beyond rule count accepted: %v, %v", issues, ok)
	}
}
