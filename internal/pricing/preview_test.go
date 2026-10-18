package pricing

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Preview must show truthful base prices before any inference, without
// inventing a token context, observing models, touching disk or fetching.
func TestPricePreviewIsReadOnlyAndConservative(t *testing.T) {
	var fetches atomic.Int32
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		fmt.Fprint(w, catalogFixture)
	})
	cache := filepath.Join(t.TempDir(), "catalog.json")
	s, err := NewService(Config{Auto: true,
		Prices: []Price{{Model: "manual-free", Rates: Rates{Reasoning: ptr(0)}}},
		Rules:  []Rule{{ID: "conditional", Model: "dated-2026", InputTokensGT: ptr(10), Rates: Rates{Input: 99}}},
	}, cache, client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	diskBefore, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	identities := []ModelIdentity{
		{Model: "manual-free"}, {Model: "free"}, {Model: "m"}, {Model: "m", Provider: "openai"},
		{Model: "m", Provider: "not-an-executor"}, {Model: "dated-2026"},
		{Model: "openai/dated-2026(high)"}, {Model: "unknown"},
	}
	items, err := s.Preview(identities)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"manual", "auto", "unpriced", "auto", "unpriced", "auto", "auto", "unpriced"} {
		if items[i].Source != want {
			t.Fatalf("item %d: %+v", i, items[i])
		}
	}
	if !items[5].HasRules || items[5].Rates.Input != 5_000_000 || items[0].Rates.Reasoning == nil {
		t.Fatal("preview evaluated a conditional rule or lost a zero override", items)
	}
	*items[0].Rates.Reasoning = 123
	again, _ := s.Preview(identities[:1])
	if *again[0].Rates.Reasoning != 0 {
		t.Fatal("preview aliases configuration")
	}
	diskAfter, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 || !reflect.DeepEqual(diskBefore, diskAfter) || !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("preview fetched, observed or mutated pricing state")
	}
	if before.CatalogEntries != 4 || len(before.Prices) != 1 {
		t.Fatal("catalog count mixed downloaded entries with observed/manual models", before)
	}
	if err := s.Configure(Config{Auto: false}); err != nil {
		t.Fatal(err)
	}
	off, _ := s.Preview([]ModelIdentity{{Model: "free"}})
	if off[0].Source != "unpriced" {
		t.Fatal("preview ignored auto=false")
	}
}

// The explicit bound prevents large management previews from tying up the
// accounting mutex. Invalid requests must be rejected as a whole.
func TestPreviewInputBoundsAndConcurrentConfigure(t *testing.T) {
	s := service(t, Config{})
	for _, models := range [][]ModelIdentity{{{Model: ""}}, {{Model: strings.Repeat("m", 1025)}},
		{{Model: "m", Provider: strings.Repeat("p", 1025)}}, make([]ModelIdentity, 4097)} {
		if _, err := s.Preview(models); err == nil {
			t.Fatal("accepted invalid preview")
		}
	}
	models := make([]ModelIdentity, 4096)
	for i := range models {
		models[i].Model = "m"
	}
	if items, err := s.Preview(models); err != nil || len(items) != 4096 {
		t.Fatal("valid bound rejected", err)
	}
	if items, err := s.Preview(nil); err != nil || items == nil {
		t.Fatal("empty preview is not a JSON array", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := s.Preview(models[:1]); err != nil {
					t.Error(err)
				}
				if err := s.Configure(Config{Prices: []Price{{Model: "m", Rates: Rates{Reasoning: ptr(1)}}}}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
