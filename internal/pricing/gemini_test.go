package pricing

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A known ancillary fee must not discard text rates or their real conditional
// multipliers. Check the adapter through the same Quote path used at ingestion.
func TestV2GeminiTextPricingAndAncillaryCosts(t *testing.T) {
	entries, err := parseCatalog(loadFixture(t, "models.dev.v2.gemini.json"), SourceV2)
	if err != nil {
		t.Fatal(err)
	}
	index := indexCatalog(entries)
	google, ok := index.exactEntry("gemini-3.8-flash", "google")
	if !ok || !google.Supported || google.Definition.Base.Input != 750000 ||
		google.Definition.Base.Output != 3750000 || google.Definition.Base.CacheRead != 75000 ||
		google.Definition.Base.Reasoning != nil || len(google.Definition.Multipliers) != 3 ||
		!contains(google.SupportIssues, "web_search_cost_not_included") ||
		!contains(google.SupportIssues, "cache_storage_cost_not_supported") {
		t.Fatalf("google = %+v", google)
	}
	s := service(t, DefaultConfig())
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = index
	s.mu.Unlock()
	// One million uncached input, output (including reasoning), and cache-read
	// tokens cost $4.575 before tier/date multipliers; search fees are excluded.
	for _, tc := range []struct {
		at   string
		tier string
		want int64
	}{
		{"2026-12-31T23:59:59Z", "standard", 4575000},
		{"2026-12-31T23:59:59Z", "priority", 8235000},
		{"2026-12-31T23:59:59Z", "flex", 2287500},
		{"2027-01-01T00:00:00Z", "standard", 9150000},
		{"2027-01-01T00:00:00Z", "priority", 16470000},
	} {
		t.Run(tc.at+"/"+tc.tier, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			q, err := s.Quote("gemini-3.8-flash", "google",
				Tokens{Input: 1000000, Output: 1000000, Reasoning: 500000, CacheRead: 1000000}, at, tc.tier)
			if err != nil || !q.PricingSupported || q.Source != "auto" || q.CostMicroUSD != tc.want ||
				!contains(q.SupportIssues, "web_search_cost_not_included") {
				t.Fatalf("quote = %+v, err = %v", q, err)
			}
		})
	}
	// Vertex's region-gated multiplier is skipped as a complete rule while its
	// base, cache, tier and date pricing remain usable.
	vertex, ok := index.exactEntry("gemini-3.8-flash", "google-vertex")
	if !ok || !vertex.Supported || vertex.Definition.Base.Input != 750000 ||
		vertex.Definition.Base.Output != 3750000 ||
		len(vertex.Definition.Multipliers) != 3 ||
		!contains(vertex.SupportIssues, "region_multiplier_ignored") ||
		!contains(vertex.SupportIssues, "web_search_cost_not_included") ||
		contains(vertex.SupportIssues, "unsupported_condition_field:region") {
		t.Fatalf("vertex = %+v", vertex)
	}
	for i, want := range []string{"multiplier-0", "multiplier-1", "multiplier-3"} {
		if vertex.Definition.Multipliers[i].ID != want {
			t.Fatalf("vertex multiplier IDs = %+v", vertex.Definition.Multipliers)
		}
	}
	for _, tc := range []struct {
		at   string
		tier string
		want int64
	}{
		{"2026-12-31T23:59:59Z", "standard", 4575000},
		{"2026-12-31T23:59:59Z", "priority", 8235000},
		{"2027-01-01T00:00:00Z", "standard", 9150000},
	} {
		at, err := time.Parse(time.RFC3339, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		q, err := s.Quote("gemini-3.8-flash", "google-vertex",
			Tokens{Input: 1000000, Output: 1000000, Reasoning: 500000, CacheRead: 1000000}, at, tc.tier)
		if err != nil || !q.PricingSupported || q.CostMicroUSD != tc.want ||
			!contains(q.SupportIssues, "region_multiplier_ignored") {
			t.Fatalf("vertex quote = %+v, err = %v", q, err)
		}
	}
}

// Scalar text factors use the existing exact factor validation. Unknown cost
// dimensions still fail closed; adding one known fee is not a blanket bypass.
func TestV2ScalarTextFactorValidation(t *testing.T) {
	factors, issues, ok := parseV2Factor(json.RawMessage(`{"text":1.25,"audio":2}`))
	if !ok || !contains(issues, "non_text_multiplier_ignored") {
		t.Fatal(factors, issues, ok)
	}
	for _, factor := range []*string{factors.Input, factors.Output, factors.CacheRead, factors.CacheWrite, factors.Reasoning} {
		if factor == nil || *factor != "1.25" {
			t.Fatal("text scalar did not cover every text token factor", factors)
		}
	}
	for _, raw := range []string{`-1`, `true`, `"bad"`, `[]`, `1000001`, `1e100`} {
		t.Run(raw, func(t *testing.T) {
			if _, _, ok := parseV2Factor(json.RawMessage(fmt.Sprintf(`{"text":%s}`, raw))); ok {
				t.Fatal("invalid text factor accepted", raw)
			}
		})
	}
	if _, issues, ok := parseV2Definition(json.RawMessage(`{"text":{"input":{"price":1}},"future_token_price":2}`)); ok ||
		!contains(issues, "unsupported_cost_field") {
		t.Fatal("unknown cost accepted", issues, ok)
	}
}
