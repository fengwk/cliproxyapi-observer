package pricing

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleService loads only the small checked-in extract of the fixed
// models.dev catalog (SHA-256 8700799debe7b107f438bcc77305847333b6eb232274bb43d4b285b2b0dbca41)
// so these identity tests never touch the
// network or depend on a future live catalog.
func sampleService(t *testing.T) *Service {
	t.Helper()
	body, err := os.ReadFile("testdata/models.dev.sample.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(DefaultConfig(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.catalog = indexCatalog(entries)
	s.lastSync = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return s
}

func previewOne(t *testing.T, s *Service, model, provider string) PreviewPrice {
	t.Helper()
	items, err := s.Preview([]ModelIdentity{{Model: model, Provider: provider}})
	if err != nil || len(items) != 1 {
		t.Fatal(err, items)
	}
	return items[0]
}

// These ten names are the fixed regression sample from the design. The
// assertions intentionally distinguish exact namespace hits, explicit
// conflicts and genuinely unlisted identities; none is allowed to fall back
// to a fuzzy name or another channel's price.
func TestFixedCatalogIdentitySamples(t *testing.T) {
	s := sampleService(t)
	cases := []struct {
		model, provider string
		source, reason  string
		catalogProvider string
		catalogModel    string
		input           int64
		minCandidates   int
	}{
		{"gemini-3-flash", "", "unpriced", reasonAmbiguous, "", "", 0, 2},
		{"codex-auto-review", "", "unpriced", reasonNotFound, "", "", 0, 0},
		{"grok-4.7", "", "unpriced", reasonAmbiguous, "", "", 0, 2},
		{"grok-composer-2.5-fast", "", "unpriced", reasonNotFound, "", "", 0, 0},
		{"commandcode/inclusionai/ling-3.1-flash", "", "unpriced", reasonNotFound, "", "", 0, 1},
		{"opencode-go/hy3", "", "auto", reasonMissingRates, "opencode-go", "hy3", 140_000, 0},
		{"opencode-go/longcat-2.0", "", "auto", reasonMissingRates, "opencode-go", "longcat-2.0", 300_000, 0},
		{"commandcode/claude-haiku-4-5-20251001", "", "auto", "", "commandcode", "claude-haiku-4-5-20251001", 1_000_000, 0},
		{"commandcode/moonshotai/Kimi-K2.7-Code", "", "auto", reasonMissingRates, "commandcode", "moonshotai/Kimi-K2.7-Code", 950_000, 0},
		{"commandcode/xiaomi/mimo-v2.6-flash", "", "unpriced", reasonNotFound, "", "", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			p := previewOne(t, s, tc.model, tc.provider)
			if p.Source != tc.source || p.Reason != tc.reason || p.CatalogProvider != tc.catalogProvider ||
				p.CatalogModel != tc.catalogModel || p.Rates.Input != tc.input || len(p.Candidates) < tc.minCandidates {
				t.Fatalf("%+v", p)
			}
			if p.Source == "unpriced" && len(p.MissingRates) != 4 {
				t.Fatalf("unpriced value must expose absent rates: %+v", p)
			}
			if p.Source == "auto" && p.CatalogTime == nil {
				t.Fatalf("auto match lost catalog time: %+v", p)
			}
			for _, c := range p.Candidates {
				if strings.HasSuffix(c.Model, ":free") && tc.model == "commandcode/inclusionai/ling-3.1-flash" {
					t.Fatalf(":free variant treated as candidate for full identity: %+v", c)
				}
			}
		})
	}
	// Preserving :free means an explicit :free request still resolves to the
	// free entry rather than being normalized away.
	free := previewOne(t, s, "commandcode/inclusionai/ling-3.1-flash:free", "")
	if free.Source != "auto" || free.Rates.Input != 0 || free.Match != matchNamespace {
		t.Fatal(free)
	}
}

// A provider-qualified request and a preview for the same identity must use
// the same resolver and therefore produce the same diagnostic identity.
func TestResolverSharedByPreviewQuoteAndSnapshot(t *testing.T) {
	s := sampleService(t)
	p := previewOne(t, s, "opencode-go/hy3", "")
	q, err := s.Quote("opencode-go/hy3", "", Tokens{Input: 1_000_000}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Source != p.Source || q.Match != p.Match || q.CatalogProvider != p.CatalogProvider || q.CatalogModel != p.CatalogModel ||
		q.Rates.Input != p.Rates.Input || q.CostMicroUSD != 140_000 {
		t.Fatal(p, q)
	}
	s.Observe("opencode-go/hy3", "")
	snap := s.Snapshot()
	if len(snap.Prices) != 1 {
		t.Fatal(snap)
	}
	sp := snap.Prices[0]
	if sp.Source != p.Source || sp.Match != p.Match || sp.CatalogProvider != p.CatalogProvider || sp.CatalogModel != p.CatalogModel ||
		!reflect.DeepEqual(sp.Candidates, p.Candidates) || sp.Rates.Input != p.Rates.Input {
		t.Fatal(sp, p)
	}
}

func TestMappingsAndValidation(t *testing.T) {
	s := sampleService(t)
	c := DefaultConfig()
	c.Mappings = []Mapping{{Model: "public-alias", Provider: "opencode-go", CatalogModel: "hy3"}}
	if err := s.Configure(c); err != nil {
		t.Fatal(err)
	}
	p := previewOne(t, s, "public-alias", "")
	if p.Source != "auto" || p.Match != matchMapping || p.CatalogProvider != "opencode-go" || p.CatalogModel != "hy3" || p.Rates.Input != 140_000 {
		t.Fatal(p)
	}
	q, err := s.Quote("public-alias", "", Tokens{Input: 1_000_000}, time.Now())
	if err != nil || q.CostMicroUSD != 140_000 || q.Match != matchMapping {
		t.Fatal(q, err)
	}
	// Manual model-wide pricing keeps priority over a mapping.
	c.Prices = []Price{{Model: "public-alias", Rates: Rates{Input: 7_000_000}}}
	if err = s.Configure(c); err != nil {
		t.Fatal(err)
	}
	if p = previewOne(t, s, "public-alias", ""); p.Source != "manual" || p.Match != matchManual || p.Rates.Input != 7_000_000 {
		t.Fatal(p)
	}
	// A configured mapping that no longer names an exact catalog entry is
	// explicit failure, not a quiet fallback to another channel.
	c.Prices = nil
	c.Mappings = []Mapping{{Model: "public-alias", Provider: "opencode-go", CatalogModel: "missing-model"}}
	if err = s.Configure(c); err != nil {
		t.Fatal(err)
	}
	if p = previewOne(t, s, "public-alias", ""); p.Source != "unpriced" || p.Reason != reasonMappingMissing ||
		p.CatalogProvider != "opencode-go" || p.CatalogModel != "missing-model" || len(p.MissingRates) != 4 {
		t.Fatal(p)
	}
	// Auto=false disables mapping resolution along with the rest of automatic
	// matching; it does not silently bypass the administrator switch.
	c.Auto = false
	if err = s.Configure(c); err != nil {
		t.Fatal(err)
	}
	if p = previewOne(t, s, "public-alias", ""); p.Source != "unpriced" || p.Reason != reasonAutoDisabled {
		t.Fatal(p)
	}
	for _, bad := range []Config{
		{Mappings: []Mapping{{Model: "", Provider: "p", CatalogModel: "m"}}},
		{Mappings: []Mapping{{Model: "m", Provider: "", CatalogModel: "m"}}},
		{Mappings: []Mapping{{Model: "m", Provider: "p", CatalogModel: ""}}},
		{Mappings: []Mapping{{Model: "m", Provider: "p", CatalogModel: "m"}, {Model: "m", Provider: "q", CatalogModel: "n"}}},
	} {
		if Validate(bad) == nil || s.Configure(bad) == nil {
			t.Fatal("invalid mapping accepted", bad)
		}
	}
}

func TestProviderConflictsAndCanonicalEvidence(t *testing.T) {
	s := sampleService(t)
	// The namespace and a trusted provider point at different channels.
	p := previewOne(t, s, "commandcode/xai/grok-4.7", "xai")
	if p.Source != "unpriced" || p.Reason != reasonProviderConflict || p.Match != matchAmbiguous || len(p.Candidates) != 1 ||
		p.Candidates[0].Provider != "commandcode" {
		t.Fatal(p)
	}
	// With two exact channel entries, the conflict exposes both candidates.
	conflict, _ := NewService(DefaultConfig(), "", nil)
	conflict.catalog = indexCatalog([]entry{
		{Model: "xai/grok-4.7", Provider: "commandcode", Rates: Rates{Input: 1}},
		{Model: "xai/grok-4.7", Provider: "xai", Rates: Rates{Input: 2}},
	})
	p = previewOne(t, conflict, "commandcode/xai/grok-4.7", "xai")
	if p.Reason != reasonProviderConflict || len(p.Candidates) != 2 ||
		!reflect.DeepEqual([]string{p.Candidates[0].Provider, p.Candidates[1].Provider}, []string{"commandcode", "xai"}) {
		t.Fatal(p)
	}
	// A canonical relationship is usable only inside the requested provider.
	p = previewOne(t, s, "tencent/hy3", "opencode-go")
	if p.Source != "auto" || p.Match != matchCanonical || p.CatalogProvider != "opencode-go" || p.CatalogModel != "hy3" {
		t.Fatal(p)
	}
	// The same provider only offers a "-paid" variant for a different requested
	// canonical ID; that pricing variant is not silently substituted.
	p = previewOne(t, s, "tencent/hy3", "commandcode")
	if p.Source != "unpriced" || p.Reason != reasonVariantOnly || len(p.Candidates) != 1 ||
		p.Candidates[0].Model != "tencent/hy3-paid" {
		t.Fatal(p)
	}
	// Without a trusted channel, canonical aliases are candidates, not a price.
	p = previewOne(t, s, "tencent/hy3", "")
	if p.Source != "unpriced" || p.Reason != reasonAmbiguous || len(p.Candidates) < 2 {
		t.Fatal(p)
	}
}

func TestContextTierBoundariesAndInvalidTiers(t *testing.T) {
	s := sampleService(t)
	p := previewOne(t, s, "opencode-go/grok-4.7", "")
	if p.Source != "auto" || p.Reason != reasonContextTiers || p.Rates.Input != 2_000_000 {
		t.Fatal(p)
	}
	for _, tc := range []struct {
		tokens   Tokens
		wantRate int64
		wantCost int64
	}{
		{Tokens{Input: 200000}, 2_000_000, 400_000},
		{Tokens{Input: 200001}, 4_000_000, 800_004},
		// Thresholds use the complete prompt, not only uncached input.
		{Tokens{Input: 100000, CacheRead: 100000}, 2_000_000, 200_000 + 50_000},
		{Tokens{Input: 100000, CacheRead: 100001}, 4_000_000, 400_000 + 100_001},
	} {
		q, err := s.Quote("opencode-go/grok-4.7", "", tc.tokens, time.Now())
		if err != nil || q.Rates.Input != tc.wantRate || q.CostMicroUSD != tc.wantCost {
			t.Fatal(tc, q, err)
		}
	}
	// context_over_200k is used only when a tier list is absent.
	q, err := s.Quote("legacy-tier", "fixture", Tokens{Input: 200001}, time.Now())
	if err != nil || q.Rates.Input != 4_000_000 || q.Source != "auto" {
		t.Fatal(q, err)
	}
	// An unsupported tier shape must not flatten to the base rate.
	for _, call := range []func() (string, string, int64){
		func() (string, string, int64) {
			p := previewOne(t, s, "bad-tier", "fixture")
			return p.Source, p.Reason, p.Rates.Input
		},
		func() (string, string, int64) {
			q, _ := s.Quote("bad-tier", "fixture", Tokens{Input: 1000}, time.Now())
			return q.Source, q.Reason, q.Rates.Input
		},
	} {
		source, reason, rate := call()
		if source != "unpriced" || reason != reasonTiersUnsupported || rate != 0 {
			t.Fatal(source, reason, rate)
		}
	}
	// A reliable tier missing a needed rate is still unpriced, while zero usage
	// for that dimension may use the explicit rates that do exist.
	s.catalog = indexCatalog([]entry{{
		Model: "tier-missing", Provider: "p", Rates: Rates{Input: 1, Output: 1},
		Tiers: []tier{{Size: 100, Rates: Rates{Input: 2}, MissingRates: []string{rateOutput, rateCacheRead, rateCacheWrite}}},
	}})
	q, err = s.Quote("tier-missing", "p", Tokens{Input: 101, Output: 1}, time.Now())
	if err != nil || q.Source != "unpriced" || q.Reason != reasonMissingRates || !reflect.DeepEqual(q.MissingRates, []string{rateOutput}) {
		t.Fatal(q, err)
	}
	q, err = s.Quote("tier-missing", "p", Tokens{Input: 101}, time.Now())
	if err != nil || q.Source != "auto" || q.CostMicroUSD != 1 {
		t.Fatal(q, err)
	}
}

func TestMissingRatesVersusExplicitZero(t *testing.T) {
	s := sampleService(t)
	p := previewOne(t, s, "partial", "fixture")
	if p.Source != "auto" || p.Reason != reasonMissingRates || p.Rates.Input != 1_000_000 ||
		!reflect.DeepEqual(p.MissingRates, []string{rateOutput, rateCacheRead, rateCacheWrite}) {
		t.Fatal(p)
	}
	q, err := s.Quote("partial", "fixture", Tokens{Input: 1_000_000}, time.Now())
	if err != nil || q.Source != "auto" || q.CostMicroUSD != 1_000_000 {
		t.Fatal(q, err)
	}
	q, err = s.Quote("partial", "fixture", Tokens{Output: 1}, time.Now())
	if err != nil || q.Source != "unpriced" || q.Reason != reasonMissingRates || q.CostMicroUSD != 0 ||
		!reflect.DeepEqual(q.MissingRates, []string{rateOutput}) {
		t.Fatal(q, err)
	}
	zeros := previewOne(t, s, "zeros", "fixture")
	if zeros.Source != "auto" || len(zeros.MissingRates) != 0 || zeros.Reason != "" {
		t.Fatal(zeros)
	}
	q, err = s.Quote("zeros", "fixture", Tokens{Input: 1_000_000, Output: 1}, time.Now())
	if err != nil || q.Source != "auto" || q.CostMicroUSD != 0 || q.Reason != "" {
		t.Fatal(q, err)
	}
}

func TestDiagnosticCandidateBoundAndOrder(t *testing.T) {
	entries := make([]entry, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, entry{Model: "m", Provider: string(rune('a'+i%26)) + string(rune('a'+i/26)), Rates: Rates{Input: int64(i + 1)}})
	}
	s, _ := NewService(DefaultConfig(), "", nil)
	s.catalog = indexCatalog(entries)
	p := previewOne(t, s, "m", "")
	if p.Source != "unpriced" || p.Reason != reasonAmbiguous || len(p.Candidates) != maxCandidates {
		t.Fatal(len(p.Candidates), p)
	}
	for i := 1; i < len(p.Candidates); i++ {
		if p.Candidates[i-1].Provider > p.Candidates[i].Provider {
			t.Fatal("candidates are not deterministic")
		}
	}
}

// Old catalog caches only had model/provider/rates; they must still load and
// produce exact automatic quotes without inventing missing-rate diagnostics.
func TestLegacyCatalogCacheReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	body := `{"last_sync":"2026-10-20T12:00:00Z","entries":[{"model":"m","provider":"p","rates":{"input":1250000,"output":2000000,"cache_read":100000,"cache_write":200000}}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(DefaultConfig(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Error != "" {
		t.Fatal(s.Snapshot())
	}
	p := previewOne(t, s, "m", "p")
	if p.Source != "auto" || p.Match != matchProvider || p.Rates.Input != 1_250_000 || len(p.MissingRates) != 0 {
		t.Fatal(p)
	}
	q, err := s.Quote("m", "p", Tokens{Input: 1_000_000}, time.Now())
	if err != nil || q.CostMicroUSD != 1_250_000 || q.CatalogTime == nil {
		t.Fatal(q, err)
	}
}

// Preview and snapshot never alter the catalog; a failed refresh keeps the
// last valid generation and reports the failure through Snapshot.Error.
func TestRefreshFailureKeepsResolvedCatalog(t *testing.T) {
	var fail bool
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "failure body must stay private", 500)
			return
		}
		body, _ := os.ReadFile("testdata/models.dev.sample.json")
		_, _ = w.Write(body)
	})
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := s.Snapshot()
	p := previewOne(t, s, "opencode-go/hy3", "")
	if p.Source != "auto" {
		t.Fatal(p)
	}
	fail = true
	if err = s.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	after := s.Snapshot()
	if after.Error == "" || after.LastSync != before.LastSync || strings.Contains(after.Error, "failure body") {
		t.Fatal(after)
	}
	if p = previewOne(t, s, "opencode-go/hy3", ""); p.Source != "auto" || p.Rates.Input != 140_000 {
		t.Fatal("last valid catalog lost", p)
	}
}

// New cache entries retain partial-rate and tier diagnostics; the round trip
// must not turn missing fields into explicit zeros or lose tier metadata.
func TestCatalogCacheRoundTripDiagnostics(t *testing.T) {
	body, err := os.ReadFile("testdata/models.dev.sample.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	s, err := NewService(DefaultConfig(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	sync := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err = s.saveCache(diskCache{LastSync: sync, Entries: entries}); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewService(DefaultConfig(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Snapshot().Error != "" || !loaded.Snapshot().LastSync.Equal(sync) {
		t.Fatal(loaded.Snapshot())
	}
	partial := previewOne(t, loaded, "partial", "fixture")
	if !reflect.DeepEqual(partial.MissingRates, []string{rateOutput, rateCacheRead, rateCacheWrite}) {
		t.Fatal(partial)
	}
	tiered := previewOne(t, loaded, "opencode-go/grok-4.7", "")
	if tiered.Reason != reasonContextTiers || tiered.Rates.Input != 2_000_000 {
		t.Fatal(tiered)
	}
	q, err := loaded.Quote("opencode-go/grok-4.7", "", Tokens{Input: 200001}, time.Now())
	if err != nil || q.Rates.Input != 4_000_000 {
		t.Fatal(q, err)
	}
}

// A trusted provider is a channel boundary, not a hint. If that channel has no
// exact entry (and no same-channel canonical evidence), another channel's
// identical model name must never become an automatic price.
func TestTrustedProviderDoesNotFallBackToAnotherChannel(t *testing.T) {
	s, err := NewService(DefaultConfig(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.catalog = indexCatalog([]entry{
		{Model: "only", Provider: "other", Rates: Rates{Input: 1_000_000}},
		{Model: "same", Provider: "a", Rates: Rates{Input: 2_000_000}},
		{Model: "same", Provider: "b", Rates: Rates{Input: 2_000_000}},
	})
	p := previewOne(t, s, "only", "unrelated")
	if p.Source != "unpriced" || p.Reason != reasonNotFound || p.Match != matchNone ||
		len(p.Candidates) != 1 || p.Candidates[0].Provider != "other" || len(p.MissingRates) != 4 {
		t.Fatal(p)
	}
	q, err := s.Quote("only", "unrelated", Tokens{Input: 1_000_000}, time.Now())
	if err != nil || q.Source != "unpriced" || q.CostMicroUSD != 0 || q.Reason != reasonNotFound {
		t.Fatal(q, err)
	}
	// The same identity is auto-priced only without a trusted channel or with
	// the exact owning channel.
	if p = previewOne(t, s, "only", ""); p.Source != "auto" || p.Match != matchExact || p.Rates.Input != 1_000_000 {
		t.Fatal(p)
	}
	if p = previewOne(t, s, "only", "other"); p.Source != "auto" || p.Match != matchProvider {
		t.Fatal(p)
	}
	// Complete-rate equality remains the only provider-less multi-channel case.
	if p = previewOne(t, s, "same", ""); p.Source != "auto" || p.Match != matchExact {
		t.Fatal(p)
	}
}

// A full public model that happens to exist under a different provider is not
// allowed to override the verified namespace clue. Both trusted clues conflict.
func TestNamespaceConflictIgnoresWrongProviderExact(t *testing.T) {
	s, err := NewService(DefaultConfig(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.catalog = indexCatalog([]entry{
		{Model: "commandcode/foo", Provider: "other", Rates: Rates{Input: 1_000_000}},
		{Model: "foo", Provider: "commandcode", Rates: Rates{Input: 2_000_000}},
	})
	p := previewOne(t, s, "commandcode/foo", "other")
	if p.Source != "unpriced" || p.Reason != reasonProviderConflict || len(p.Candidates) != 2 {
		t.Fatal(p)
	}
	if p.Candidates[0].Provider != "commandcode" || p.Candidates[1].Provider != "other" {
		t.Fatal(p.Candidates)
	}
	if q, err := s.Quote("commandcode/foo", "other", Tokens{Input: 1_000_000}, time.Now()); err != nil ||
		q.Source != "unpriced" || q.CostMicroUSD != 0 {
		t.Fatal(q, err)
	}
	// The namespace remains resolvable when no contradictory channel is given.
	p = previewOne(t, s, "commandcode/foo", "")
	if p.Source != "auto" || p.Match != matchNamespace || p.CatalogModel != "foo" || p.Rates.Input != 2_000_000 {
		t.Fatal(p)
	}
}

// Unsupported dimensions inside a nested tier (for example audio) invalidate
// the tier table instead of being silently ignored and billed as text rates.
func TestTierSchemaRejectsUnsupportedDimensions(t *testing.T) {
	body := `{"p":{"models":{
		"audio-tier":{"cost":{"input":1,"output":2,"tiers":[{"input":1,"output":2,"input_audio":3,"tier":{"type":"context","size":100}}]}},
		"metadata-tier":{"cost":{"input":1,"output":2,"tiers":[{"input":1,"output":2,"tier":{"type":"context","size":100,"extra":true}}]}},
		"legacy-audio":{"cost":{"input":1,"output":2,"context_over_200k":{"input":2,"output":3,"output_audio":4}}},
		"supported":{"cost":{"input":1,"output":2,"tiers":[{"input":2,"output":3,"reasoning":1,"tier":{"type":"context","size":100}}]}}
	}}}`
	entries, err := parseCatalog([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewService(DefaultConfig(), "", nil)
	s.catalog = indexCatalog(entries)
	for _, model := range []string{"audio-tier", "metadata-tier", "legacy-audio"} {
		p := previewOne(t, s, model, "p")
		if p.Source != "unpriced" || p.Reason != reasonTiersUnsupported {
			t.Fatal(model, p)
		}
	}
	p := previewOne(t, s, "supported", "p")
	if p.Source != "auto" || p.Reason != reasonContextTiers || p.Rates.Input != 1_000_000 {
		t.Fatal(p)
	}
	q, err := s.Quote("supported", "p", Tokens{Input: 101}, time.Now())
	if err != nil || q.Source != "auto" || q.Rates.Input != 2_000_000 {
		t.Fatal(q, err)
	}
}

// The fixture's grok conflict is grounded in extracted source entries with
// different complete rates, not in an accidental missing fixture row.
func TestFixtureGrokEvidence(t *testing.T) {
	s := sampleService(t)
	p := previewOne(t, s, "grok-4.7", "")
	if p.Source != "unpriced" || p.Reason != reasonAmbiguous || len(p.Candidates) < 5 {
		t.Fatal(p)
	}
	want := map[string]Rates{
		"xai":         {Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000},
		"opencode-go": {Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000},
		"aihubmix":    {Input: 2_200_000, Output: 6_600_000, CacheRead: 550_000},
		"302ai":       {Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000, CacheWrite: 500_000},
		"requesty":    {Input: 2_000_000, Output: 6_000_000, CacheRead: 500_000, CacheWrite: 2_000_000},
	}
	got := map[string]Candidate{}
	for _, c := range p.Candidates {
		got[c.Provider] = c
	}
	for provider, rates := range want {
		c, ok := got[provider]
		if !ok || !ratesEqual(c.Rates, rates) {
			t.Fatal(provider, c, ok)
		}
	}
}
