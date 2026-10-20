package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Thresholds describe full context, while cost buckets remain disjoint.
// Output/Reasoning/Total must not affect prompt conditions.
func TestFullPromptThreshold(t *testing.T) {
	s := service(t, Config{Rules: []Rule{
		{ID: "large", Model: "m", InputTokensGT: ptr(200000), Rates: Rates{Input: 1000000}},
	}})
	for _, tc := range []struct {
		name   string
		tokens Tokens
		source string
		cost   int64
	}{
		{"cached-large", Tokens{Input: 10, CacheRead: 200000, CacheWrite: 5}, "rule", 10},
		{"read-only", Tokens{CacheRead: 200001}, "rule", 0},
		{"write-only", Tokens{CacheWrite: 200001}, "rule", 0},
		{"exact-boundary", Tokens{Input: 10, CacheRead: 199980, CacheWrite: 10}, "unpriced", 0},
		{"below-boundary", Tokens{Input: 10, CacheRead: 199979, CacheWrite: 10}, "unpriced", 0},
		{"ignore-output", Tokens{Input: 10, Output: 300000, Reasoning: 200000, Total: 999999}, "unpriced", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quote(t, s, "m", "", tc.tokens, time.Now())
			if q.Source != tc.source || q.CostMicroUSD != tc.cost {
				t.Fatal(q)
			}
		})
	}
	// Each raw counter fits int64 but the prompt sum does not. A zero-cost
	// matching rule isolates threshold arithmetic from monetary overflow.
	if err := s.Configure(Config{Rules: []Rule{{ID: "huge", Model: "m", InputTokensGT: ptr(maxSafe)}}}); err != nil {
		t.Fatal(err)
	}
	q := quote(t, s, "m", "", Tokens{Input: math.MaxInt64, CacheRead: math.MaxInt64, CacheWrite: math.MaxInt64}, time.Now())
	if q.Source != "rule" || q.RuleID != "huge" {
		t.Fatal(q)
	}
}

// Provider identity remains visible and ordered even for identical model names;
// manual prices still apply uniformly rather than becoming conditional.
func TestProviderSnapshotAndManualValidation(t *testing.T) {
	s := service(t, DefaultConfig())
	s.catalog = indexCatalog([]entry{{Model: "m", Provider: "b", Rates: Rates{Input: 2}},
		{Model: "m", Provider: "a", Rates: Rates{Input: 1}}})
	s.Observe("m", "b")
	s.Observe("m", "a")
	want := []Price{
		{Model: "m", Provider: "a", Source: "auto", Rates: Rates{Input: 1}, Match: matchProvider, CatalogProvider: "a", CatalogModel: "m"},
		{Model: "m", Provider: "b", Source: "auto", Rates: Rates{Input: 2}, Match: matchProvider, CatalogProvider: "b", CatalogModel: "m"},
	}
	for i := 0; i < 20; i++ {
		if got := s.Snapshot().Prices; !reflect.DeepEqual(got, want) {
			t.Fatal(got)
		}
	}
	raw, err := json.Marshal(want[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || string(fields["provider"]) != `"a"` {
		t.Fatal(string(raw), err)
	}
	manual := Config{Prices: []Price{{Model: "m", Source: "manual"}}}
	if err = s.Configure(manual); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Snapshot().Prices {
		if p.Source != "manual" || p.Rates.Input != 0 || p.Provider == "" {
			t.Fatal(p)
		}
	}
	raw, _ = json.Marshal(manual.Prices[0])
	// Unmarshal into a fresh map so an omitted key cannot be retained.
	fields = nil
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["provider"]; present {
		t.Fatal(string(raw))
	}
	for _, p := range []Price{{Model: "m", Provider: "a"}, {Model: "m", Source: "auto"}, {Model: "m", Source: "rule"}, {Model: "m", Source: "unpriced"}} {
		if err = s.Configure(Config{Prices: []Price{p}}); err == nil {
			t.Fatal("invalid manual accepted", p)
		}
		if quote(t, s, "m", "b", Tokens{}, time.Now()).Source != "manual" {
			t.Fatal("failed config changed state")
		}
	}
}

// Indexed lookups retain exact-match priority and sticky ambiguity. Neither
// provider iteration order nor a normalized alias may rescue an ambiguous ID.
func TestCatalogIndexExactAndAmbiguity(t *testing.T) {
	entries := []entry{
		{Model: "openai/m", Provider: "a", Rates: Rates{Input: 1}},
		{Model: "openai/m", Provider: "b", Rates: Rates{Input: 2}},
		{Model: "m", Provider: "a", Rates: Rates{Input: 3, Reasoning: ptr(4)}},
	}
	for i := 0; i < 2; i++ {
		index := indexCatalog(entries)
		if _, ok := lookup(index, "openai/m", ""); ok {
			t.Fatal("ambiguous exact fell through")
		}
		if r, ok := lookup(index, "openai/m", "a"); !ok || r.Input != 1 {
			t.Fatal(r, ok)
		}
		if _, ok := lookup(index, "m(high)", "a"); ok {
			t.Fatal("decorated identity must stay distinct")
		}
		entries[0], entries[2] = entries[2], entries[0]
	}
}

// Refresh builds/publishes a replacement index even if saving fails. Failure
// status is explicit, valid memory remains usable, and later success clears it.
func TestIndexRefreshAndSaveFailureStatus(t *testing.T) {
	var rate atomic.Int64
	rate.Store(1)
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"p":{"models":{"m":{"cost":{"input":%d}}}}}`, rate.Load())
	})
	path := t.TempDir() // renaming a regular temp file over this directory fails
	s, err := NewService(DefaultConfig(), path, client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err == nil {
		t.Fatal("cache write failure hidden")
	}
	if snap := s.Snapshot(); snap.Error != "catalog cache write failed" || snap.LastSync.IsZero() || snap.Refreshing {
		t.Fatal(snap)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 1 || q.Source != "auto" {
		t.Fatal(q)
	}
	s.cachePath = filepath.Join(t.TempDir(), "cache")
	rate.Store(2)
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Error != "" {
		t.Fatal(s.Snapshot())
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 2 {
		t.Fatal("stale index", q)
	}
	loaded, err := NewService(DefaultConfig(), s.cachePath, client)
	if err != nil {
		t.Fatal(err)
	}
	if q := quote(t, loaded, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 2 {
		t.Fatal("load did not index", q)
	}
	// Toggling configuration changes effective selection without stale cached
	// resolutions, while a previously obtained quote remains unchanged.
	old := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now())
	if err = s.Configure(Config{}); err != nil {
		t.Fatal(err)
	}
	if quote(t, s, "m", "p", Tokens{}, time.Now()).Source != "unpriced" {
		t.Fatal("auto off index")
	}
	if err = s.Configure(DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if old.CostMicroUSD != 2 || quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()).CostMicroUSD != 2 {
		t.Fatal("auto re-enable")
	}
}

// Benchmarks cover the request path at the largest admitted catalog size,
// including uncached identities: no per-identity warmup is needed for O(1).
func BenchmarkQuoteIndexedCatalog(b *testing.B) {
	entries := make([]entry, 100000)
	for i := range entries {
		entries[i] = entry{Model: fmt.Sprint(i), Provider: "p", Rates: Rates{Input: 1}}
	}
	s, _ := NewService(DefaultConfig(), "", nil)
	s.catalog = indexCatalog(entries)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Quote("99999", "p", Tokens{Input: 1}, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}
