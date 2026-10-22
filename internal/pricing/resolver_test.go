package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSnapshotKeepsProviderIdentityAndManualDefinitions(t *testing.T) {
	s := service(t, Config{Auto: true, Prices: []ManualPrice{
		{Model: "m", Definition: definition(RateSet{Input: 1_000_000}, nil, nil)},
		{Model: "m", Provider: "p", Definition: definition(RateSet{Input: 2_000_000}, nil, nil)},
	}})
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = indexCatalog([]entry{
		{Model: "auto", Provider: "b", Supported: true, Definition: definition(RateSet{Input: 3_000_000}, nil, nil)},
		{Model: "auto", Provider: "a", Supported: true, Definition: definition(RateSet{Input: 4_000_000}, nil, nil)},
	})
	s.mu.Unlock()
	s.Observe("auto", "a")
	s.Observe("m", "p")
	snap := s.Snapshot()
	if snap.Source != SourceV2 || len(snap.Prices) != 3 {
		t.Fatal(snap)
	}
	got := map[string]int64{}
	for _, p := range snap.Prices {
		got[p.Model+"/"+p.Provider] = p.Rates.Input
		if p.Definition.Base.Input != p.Rates.Input {
			t.Fatal("definition/rates mismatch", p)
		}
	}
	want := map[string]int64{"auto/a": 4_000_000, "m/p": 2_000_000, "m/": 1_000_000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v", got)
	}
}

// Unsupported catalog pricing is exposed as a full zero definition with a
// support issue and can never contribute an automatic quote.
func TestUnsupportedCandidateIsZeroAndNotPriced(t *testing.T) {
	bad := entry{
		Model: "m", Provider: "p", Supported: false,
		Definition: zeroDefinition(), SupportIssues: []string{"unsupported_condition"},
	}
	s := service(t, DefaultConfig())
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = indexCatalog([]entry{bad})
	s.mu.Unlock()
	q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now())
	if q.Source != "zero" || q.CostMicroUSD != 0 || q.PricingSupported || q.Reason != reasonPricingUnsupported {
		t.Fatal(q)
	}
	if len(q.Candidates) != 1 || q.Candidates[0].Definition.Base.Input != 0 || q.Candidates[0].PricingSupported {
		t.Fatal(q.Candidates)
	}
}

// Preview never fetches, observes or mutates configuration and always carries
// the complete definition. Missing prices are zero, not unpriced.
func TestPreviewReadOnlyAndDefinition(t *testing.T) {
	s := service(t, Config{
		Auto: false,
		Prices: []ManualPrice{{
			Model: "manual", Definition: definition(RateSet{Input: 5_000_000, Reasoning: ptr(0)}, nil, nil),
		}},
	})
	before := s.Snapshot()
	items, err := s.Preview([]ModelIdentity{{Model: "manual"}, {Model: "unknown"}, {Model: "manual", Provider: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Source != "manual" || items[0].Definition.Base.Input != 5_000_000 || items[0].Rates.Reasoning == nil {
		t.Fatal(items[0])
	}
	if items[1].Source != "zero" || items[2].Source != "zero" {
		t.Fatal("auto off or exact identity leaked", items)
	}
	for _, item := range items {
		if item.Definition.Overrides == nil || item.Definition.Multipliers == nil {
			t.Fatal("definition arrays are not stable JSON arrays", item.Definition)
		}
	}
	*items[0].Rates.Reasoning = 123
	again, _ := s.Preview([]ModelIdentity{{Model: "manual"}})
	if *again[0].Rates.Reasoning != 0 {
		t.Fatal("preview aliased configuration")
	}
	if !reflect.DeepEqual(before, s.Snapshot()) {
		t.Fatal("preview changed state")
	}
}

func TestPreviewBoundsAndConcurrentConfigure(t *testing.T) {
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
				if err := s.Configure(Config{Prices: []ManualPrice{{Model: "m", Definition: definition(RateSet{Reasoning: ptr(1)}, nil, nil)}}}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestExactProviderAndAmbiguityResolution(t *testing.T) {
	entries := []entry{
		{Model: "openai/m", Provider: "a", Supported: true, Definition: definition(RateSet{Input: 1_000_000}, nil, nil)},
		{Model: "openai/m", Provider: "b", Supported: true, Definition: definition(RateSet{Input: 2_000_000}, nil, nil)},
		{Model: "m", Provider: "a", Supported: true, Definition: definition(RateSet{Input: 3_000_000}, nil, nil)},
	}
	s := service(t, DefaultConfig())
	s.mu.Lock()
	s.sourceStateLocked(SourceV2).catalog = indexCatalog(entries)
	s.mu.Unlock()
	if q := quote(t, s, "openai/m", "a", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 1 {
		t.Fatal(q)
	}
	if q := quote(t, s, "openai/m", "", Tokens{Input: 1}, time.Now()); q.Source != "zero" || q.Match != matchAmbiguous || len(q.Candidates) != 2 {
		t.Fatal("ambiguous providerless identity", q)
	}
	if q := quote(t, s, "m", "", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 3 {
		t.Fatal("single exact identity", q)
	}
}

func TestDefinitionJSONPatchNullAndOmission(t *testing.T) {
	raw := []byte(`{"base":{"input":1,"output":2,"cache_read":0,"cache_write":0,"cache_write_by_ttl":{},"reasoning":null},"overrides":[{"id":"r","when":{},"rates":{"input":5,"reasoning":null}}],"multipliers":[]}`)
	var def PricingDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	if def.Base.Reasoning != nil || def.Overrides[0].Rates.ReasoningSet != true ||
		def.Overrides[0].Rates.Reasoning != nil || def.Overrides[0].Rates.Output != nil {
		t.Fatalf("patch presence lost: %+v", def.Overrides[0].Rates)
	}
	encoded, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	_ = json.Unmarshal(encoded, &round)
	rates := round["overrides"].([]any)[0].(map[string]any)["rates"].(map[string]any)
	if _, present := rates["output"]; present {
		t.Fatal("omitted field serialized", string(encoded))
	}
	if value, present := rates["reasoning"]; !present || value != nil {
		t.Fatal("explicit reasoning null lost", string(encoded))
	}
}

func TestCacheFileIsSourceSpecific(t *testing.T) {
	dir := t.TempDir()
	s, err := NewService(DefaultConfig(), filepath.Join(dir, "catalog"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.cacheFile(SourceV2); got != filepath.Join(dir, "catalog")+"."+SourceV2 {
		t.Fatal(got)
	}
	if got := s.cacheFile(SourceV1); got == s.cacheFile(SourceV2) {
		t.Fatal("sources share a cache file")
	}
	if _, err := os.Stat(s.cacheFile(SourceV2)); !os.IsNotExist(err) {
		t.Fatal("unexpected cache")
	}
}
