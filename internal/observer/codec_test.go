package observer

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAddSatSaturates(t *testing.T) {
	if got := addSat(1, 2); got != 3 {
		t.Errorf("addSat(1,2) = %d", got)
	}
	if got := addSat(math.MaxUint64-1, 5); got != math.MaxUint64 {
		t.Errorf("addSat overflow = %d, want MaxUint64", got)
	}
	if got := addSat(math.MaxUint64, 1); got != math.MaxUint64 {
		t.Errorf("addSat(Max,1) = %d, want MaxUint64", got)
	}
}

func TestAddCountersSaturates(t *testing.T) {
	dst := Counters{Requests: math.MaxUint64 - 1, TotalTokens: math.MaxUint64}
	addCounters(&dst, Counters{Requests: 10, TotalTokens: 10})
	if dst.Requests != math.MaxUint64 {
		t.Errorf("Requests = %d, want saturated MaxUint64", dst.Requests)
	}
	if dst.TotalTokens != math.MaxUint64 {
		t.Errorf("TotalTokens = %d, want saturated MaxUint64", dst.TotalTokens)
	}
}

func TestAddCostSatKeepsFinite(t *testing.T) {
	if got := addCostSat(0.5, math.Inf(1)); got != 0.5 {
		t.Errorf("addCostSat(0.5,+Inf) = %v, want 0.5", got)
	}
	if got := addCostSat(math.NaN(), 0.25); got != 0.25 {
		t.Errorf("addCostSat(NaN,0.25) = %v, want 0.25", got)
	}
	if got := addCostSat(math.Inf(1), math.NaN()); math.IsInf(got, 0) || math.IsNaN(got) {
		t.Errorf("addCostSat(two non-finite) = %v, want finite", got)
	}
	if got := addCostSat(1.5, 2.5); got != 4 {
		t.Errorf("addCostSat(1.5,2.5) = %v, want 4", got)
	}
}

// TestCostForRejectsNonFinite guards against a pathological price/token product
// producing an infinite cost that would poison summary aggregates.
func TestCostForRejectsNonFinite(t *testing.T) {
	record := pluginapi.UsageRecord{
		RequestID:   "inf-1",
		Provider:    "openai",
		Model:       "m",
		RequestedAt: time.Unix(100, 0).UTC(),
		Detail:      pluginapi.UsageDetail{InputTokens: 1_000_000_000_000_000_000},
	}
	r := NormalizeUsage(record)
	if r.CostUSD != nil {
		t.Fatalf("NormalizeUsage must never compute cost, got %v", *r.CostUSD)
	}

	// RequestCost must reject non-finite price/token product
	if cost := RequestCost(r, map[string]Price{"m": {Input: 1e300}}); cost != nil {
		t.Errorf("RequestCost = %v, want nil for non-finite product", *cost)
	}
	// RequestCost must also reject non-finite price values (+Inf, NaN)
	if cost := RequestCost(r, map[string]Price{"m": {Input: math.Inf(1)}}); cost != nil {
		t.Errorf("RequestCost = %v, want nil for +Inf price", *cost)
	}
	if cost := RequestCost(r, map[string]Price{"m": {Input: math.NaN()}}); cost != nil {
		t.Errorf("RequestCost = %v, want nil for NaN price", *cost)
	}
}
