package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Zero is a real override, including the first of overlapping ordered rules.
func TestOrderedRulesAndImmutableQuote(t *testing.T) {
	c := DefaultConfig()
	c.Prices = []Price{{Model: "m", Rates: Rates{Input: 90}}}
	c.Rules = []Rule{{ID: "first", Model: "m", Rates: Rates{Reasoning: ptr(0)}}, {ID: "second", Model: "m", Rates: Rates{Input: 100}}}
	s := service(t, c)
	q := quote(t, s, "m", "", Tokens{Input: 1000000}, time.Now())
	if q.Source != "rule" || q.RuleID != "first" || q.CostMicroUSD != 0 {
		t.Fatal(q)
	}
	c.Rules = nil
	c.Prices[0].Rates.Input = 500
	if err := s.Configure(c); err != nil {
		t.Fatal(err)
	}
	if q.Source != "rule" || q.CostMicroUSD != 0 || *q.Rates.Reasoning != 0 {
		t.Fatal("quote was repriced")
	}
}

func TestPublicDefaultsAndCatalogNumbers(t *testing.T) {
	// Default arrays are deliberately non-null for settings editors.
	raw, err := json.Marshal(DefaultConfig())
	if err != nil || string(raw) != `{"auto":true,"prices":[],"rules":[]}` {
		t.Fatal(string(raw), err)
	}
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
	entries, err := parseCatalog([]byte(`{"p":{"models":{"unknown":{"cost":{}},"free":{"cost":{"input":0}}}}}`))
	if err != nil || len(entries) != 1 || entries[0].Model != "free" {
		t.Fatal(entries, err)
	}
	// Equal cross-provider rates are safe, unlike differently priced matches.
	entries = []entry{{Model: "m", Provider: "a", Rates: Rates{Input: 1}}, {Model: "m", Provider: "b", Rates: Rates{Input: 1}}}
	if r, ok := lookup(entries, "m", ""); !ok || r.Input != 1 {
		t.Fatal(r, ok)
	}
	entries[1].Rates.Reasoning = ptr(0)
	if _, ok := lookup(entries, "m", ""); ok {
		t.Fatal("reasoning ambiguity")
	}
}

// Replacement of the whole catalog is exercised concurrently with all read
// paths and configuration changes, not just concurrent reads of a static map.
func TestRefreshReadConfigureRaces(t *testing.T) {
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, catalogFixture) })
	s, err := NewService(DefaultConfig(), filepath.Join(t.TempDir(), "cache"), client)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for j := 0; j < 6; j++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				switch j % 3 {
				case 0:
					if err := s.Refresh(context.Background()); err != nil {
						t.Error(err)
					}
				case 1:
					c := DefaultConfig()
					c.Prices = []Price{{Model: "manual", Rates: Rates{Reasoning: ptr(int64(i))}}}
					if err := s.Configure(c); err != nil {
						t.Error(err)
					}
				case 2:
					s.Observe("m", "openai")
					s.Snapshot()
					if _, err := s.Quote("m", "openai", Tokens{Input: 10}, time.Now()); err != nil {
						t.Error(err)
					}
				}
			}
		}(j)
	}
	wg.Wait()
}
