package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxCatalogBytes = 32 << 20
const maxCatalogTiers = 16
const catalogURL = "https://models.dev/api.json"

type identity struct {
	Model    string
	Provider string
}

// tier is a reliably parsed context threshold. It replaces the base rate for
// prompts strictly larger than Size; it is never flattened into the base rate.
type tier struct {
	Size         int64    `json:"size"`
	Rates        Rates    `json:"rates"`
	MissingRates []string `json:"missing_rates,omitempty"`
}

type entry struct {
	Model          string   `json:"model"`
	Provider       string   `json:"provider"`
	Rates          Rates    `json:"rates"`
	MissingRates   []string `json:"missing_rates,omitempty"`
	Tiers          []tier   `json:"tiers,omitempty"`
	TiersInvalid   bool     `json:"tiers_invalid,omitempty"`
	CanonicalModel string   `json:"canonical_model_id,omitempty"`
}

type diskCache struct {
	LastSync time.Time `json:"last_sync"`
	Entries  []entry   `json:"entries"`
}

// Catalog prices are decimal USD/million. Convert exactly, rounding fractional
// micro-USD upward rather than involving binary floating point in billing.
func catalogRate(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	// Bound decimal exponents before big.Rat allocates their expanded value.
	if len(n) > 64 {
		return 0, errInvalid
	}
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		e, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || e < -30 || e > 18 {
			return 0, errInvalid
		}
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 {
		return 0, errInvalid
	}
	r.Mul(r, big.NewRat(1000000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxSafe {
		return 0, errInvalid
	}
	return q.Int64(), nil
}

type rawCost struct {
	Input           json.Number     `json:"input"`
	Output          json.Number     `json:"output"`
	CacheRead       json.Number     `json:"cache_read"`
	CacheWrite      json.Number     `json:"cache_write"`
	Reasoning       json.Number     `json:"reasoning"`
	Tiers           json.RawMessage `json:"tiers"`
	ContextOver200K json.RawMessage `json:"context_over_200k"`
}

type rawTier struct {
	Tier struct {
		Type string      `json:"type"`
		Size json.Number `json:"size"`
	} `json:"tier"`
	rawCost
}

func rateFieldNames() []string {
	return []string{rateInput, rateOutput, rateCacheRead, rateCacheWrite}
}

func parseRates(c rawCost) (Rates, []string, int, error) {
	values := []json.Number{c.Input, c.Output, c.CacheRead, c.CacheWrite}
	r := Rates{}
	dst := []*int64{&r.Input, &r.Output, &r.CacheRead, &r.CacheWrite}
	missing := []string{}
	present := 0
	names := rateFieldNames()
	for i, n := range values {
		if n == "" {
			missing = append(missing, names[i])
			continue
		}
		v, err := catalogRate(n)
		if err != nil {
			return Rates{}, nil, 0, err
		}
		*dst[i] = v
		present++
	}
	if c.Reasoning != "" {
		v, err := catalogRate(c.Reasoning)
		if err != nil {
			return Rates{}, nil, 0, err
		}
		r.Reasoning = &v
		present++
	}
	return r, missing, present, nil
}

func cloneTiers(in []tier) []tier {
	if in == nil {
		return nil
	}
	out := make([]tier, len(in))
	for i, t := range in {
		out[i] = t
		out[i].Rates = cloneRates(t.Rates)
		out[i].MissingRates = cloneStrings(t.MissingRates)
	}
	return out
}

func cloneEntry(e entry) entry {
	e.Rates = cloneRates(e.Rates)
	e.MissingRates = cloneStrings(e.MissingRates)
	e.Tiers = cloneTiers(e.Tiers)
	return e
}

func rawPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// parseTiers returns nil,false when the model has no tiers, nil,true when the
// published tiers cannot be represented safely, and a sorted slice otherwise.
// Malformed tiers deliberately invalidate the entry instead of silently
// treating a nested context price as a flat base price.
func parseTiers(raw, contextRaw json.RawMessage) ([]tier, bool) {
	var out []tier
	if rawPresent(raw) {
		var parsed []rawTier
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, true
		}
		if len(parsed) > maxCatalogTiers {
			return nil, true
		}
		if len(parsed) > 0 {
			out = make([]tier, 0, len(parsed))
			for _, t := range parsed {
				if t.Tier.Type != "context" {
					return nil, true
				}
				size, err := strconv.ParseInt(string(t.Tier.Size), 10, 64)
				if err != nil || size <= 0 || size > maxSafe {
					return nil, true
				}
				r, missing, present, err := parseRates(t.rawCost)
				if err != nil || present == 0 {
					return nil, true
				}
				out = append(out, tier{Size: size, Rates: r, MissingRates: missing})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Size < out[j].Size })
			for i := 1; i < len(out); i++ {
				if out[i].Size == out[i-1].Size {
					return nil, true
				}
			}
			return out, false
		}
	}
	// context_over_200k is a reliable legacy spelling only when tiers is absent.
	if rawPresent(contextRaw) {
		var c rawCost
		if err := json.Unmarshal(contextRaw, &c); err != nil {
			return nil, true
		}
		r, missing, present, err := parseRates(c)
		if err != nil || present == 0 {
			return nil, true
		}
		return []tier{{Size: 200000, Rates: r, MissingRates: missing}}, false
	}
	return nil, false
}

func parseCatalog(body []byte) ([]entry, error) {
	var root map[string]struct {
		Models map[string]struct {
			Cost           *rawCost `json:"cost"`
			CanonicalModel string   `json:"canonical_model_id"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &root) != nil || len(root) == 0 {
		return nil, errInvalid
	}
	out := []entry{}
	for provider, p := range root {
		if !validIdentity(provider) {
			return nil, errInvalid
		}
		for model, m := range p.Models {
			if !validIdentity(model) {
				return nil, errInvalid
			}
			if m.CanonicalModel != "" && !validIdentity(m.CanonicalModel) {
				return nil, errInvalid
			}
			r, missing, present := Rates{}, []string(nil), 0
			var tiers []tier
			tiersInvalid := false
			if m.Cost != nil {
				var err error
				r, missing, present, err = parseRates(*m.Cost)
				if err != nil {
					return nil, err
				}
				tiers, tiersInvalid = parseTiers(m.Cost.Tiers, m.Cost.ContextOver200K)
			}
			if present == 0 && len(tiers) == 0 && !tiersInvalid {
				continue
			}
			out = append(out, entry{
				Model: model, Provider: provider, Rates: r, MissingRates: missing,
				Tiers: tiers, TiersInvalid: tiersInvalid, CanonicalModel: m.CanonicalModel,
			})
		}
	}
	if len(out) == 0 || len(out) > 100000 {
		return nil, errInvalid
	}
	return out, nil
}

func ratesEqual(a, b Rates) bool {
	return a.Input == b.Input && a.Output == b.Output && a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite &&
		((a.Reasoning == nil && b.Reasoning == nil) || (a.Reasoning != nil && b.Reasoning != nil && *a.Reasoning == *b.Reasoning))
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tiersEqual(a, b []tier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Size != b[i].Size || !ratesEqual(a[i].Rates, b[i].Rates) || !stringsEqual(a[i].MissingRates, b[i].MissingRates) {
			return false
		}
	}
	return true
}

func pricingEqual(a, b entry) bool {
	return ratesEqual(a.Rates, b.Rates) && stringsEqual(a.MissingRates, b.MissingRates) &&
		a.TiersInvalid == b.TiersInvalid && tiersEqual(a.Tiers, b.Tiers)
}

func sortEntries(in []entry) {
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.CanonicalModel != b.CanonicalModel {
			return a.CanonicalModel < b.CanonicalModel
		}
		return candidateLess(a, b)
	})
}

// candidateLess gives every diagnostic list a stable order independent of map
// iteration, so repeated previews and snapshots are byte-for-byte comparable.
func candidateLess(a, b entry) bool {
	if a.Rates.Input != b.Rates.Input {
		return a.Rates.Input < b.Rates.Input
	}
	if a.Rates.Output != b.Rates.Output {
		return a.Rates.Output < b.Rates.Output
	}
	if a.Rates.CacheRead != b.Rates.CacheRead {
		return a.Rates.CacheRead < b.Rates.CacheRead
	}
	if a.Rates.CacheWrite != b.Rates.CacheWrite {
		return a.Rates.CacheWrite < b.Rates.CacheWrite
	}
	av, bv := int64(-1), int64(-1)
	if a.Rates.Reasoning != nil {
		av = *a.Rates.Reasoning
	}
	if b.Rates.Reasoning != nil {
		bv = *b.Rates.Reasoning
	}
	if av != bv {
		return av < bv
	}
	if a.TiersInvalid != b.TiersInvalid {
		return !a.TiersInvalid
	}
	return len(a.Tiers) < len(b.Tiers)
}

// catalogIndex is built once per catalog replacement and then treated as
// immutable. It retains all exact identities plus the canonical relation with
// its publisher provider, so a canonical alias can never borrow another
// provider's price.
type catalogIndex struct {
	exact            map[identity][]entry
	byModel          map[string][]entry
	canonical        map[identity][]entry
	canonicalByModel map[string][]entry
}

func indexCatalog(entries []entry) catalogIndex {
	index := catalogIndex{
		exact:            make(map[identity][]entry, len(entries)),
		byModel:          make(map[string][]entry, len(entries)),
		canonical:        make(map[identity][]entry, len(entries)),
		canonicalByModel: make(map[string][]entry, len(entries)),
	}
	for _, e := range entries {
		e = cloneEntry(e)
		key := identity{Model: e.Model, Provider: e.Provider}
		index.exact[key] = append(index.exact[key], e)
		index.byModel[e.Model] = append(index.byModel[e.Model], e)
		if e.CanonicalModel != "" {
			ck := identity{Model: e.CanonicalModel, Provider: e.Provider}
			index.canonical[ck] = append(index.canonical[ck], e)
			index.canonicalByModel[e.CanonicalModel] = append(index.canonicalByModel[e.CanonicalModel], e)
		}
	}
	for key, list := range index.exact {
		sortEntries(list)
		index.exact[key] = list
	}
	for model, list := range index.byModel {
		sortEntries(list)
		index.byModel[model] = list
	}
	for key, list := range index.canonical {
		sortEntries(list)
		index.canonical[key] = list
	}
	for model, list := range index.canonicalByModel {
		sortEntries(list)
		index.canonicalByModel[model] = list
	}
	return index
}

func (idx catalogIndex) exactEntry(model, provider string) (entry, bool) {
	list := idx.exact[identity{Model: model, Provider: provider}]
	if len(list) != 1 {
		return entry{}, false
	}
	return cloneEntry(list[0]), true
}

func (idx catalogIndex) modelEntries(model string) []entry {
	list := idx.byModel[model]
	out := make([]entry, len(list))
	for i, e := range list {
		out[i] = cloneEntry(e)
	}
	return out
}

func (idx catalogIndex) canonicalEntries(model, provider string) []entry {
	list := idx.canonical[identity{Model: model, Provider: provider}]
	out := make([]entry, len(list))
	for i, e := range list {
		out[i] = cloneEntry(e)
	}
	return out
}

func (idx catalogIndex) canonicalAnyEntries(model string) []entry {
	list := idx.canonicalByModel[model]
	out := make([]entry, len(list))
	for i, e := range list {
		out[i] = cloneEntry(e)
	}
	return out
}

func (idx catalogIndex) candidateEntries(model string, excludeProvider string) []entry {
	list := idx.byModel[model]
	out := []entry{}
	for _, e := range list {
		if e.Provider != excludeProvider {
			out = append(out, cloneEntry(e))
		}
	}
	return out
}

// lookup preserves the old package-internal helper semantics: provider-specific
// exact identity is authoritative, while a provider-less identity is accepted
// only when every exact channel has identical full pricing.
func lookup(index catalogIndex, model, provider string) (Rates, bool) {
	if provider != "" {
		list := index.exact[identity{Model: model, Provider: provider}]
		if len(list) == 0 {
			return Rates{}, false
		}
		first := list[0]
		for _, e := range list[1:] {
			if !pricingEqual(first, e) {
				return Rates{}, false
			}
		}
		return cloneRates(first.Rates), true
	}
	list := index.byModel[model]
	if len(list) == 0 {
		return Rates{}, false
	}
	first := list[0]
	for _, e := range list[1:] {
		if !pricingEqual(first, e) {
			return Rates{}, false
		}
	}
	return cloneRates(first.Rates), true
}

func splitKnownNamespace(model string) (string, string, bool) {
	for _, provider := range []string{"commandcode", "opencode-go"} {
		prefix := provider + "/"
		if strings.HasPrefix(model, prefix) && len(model) > len(prefix) {
			return provider, model[len(prefix):], true
		}
	}
	return "", "", false
}

func (s *Service) fetch(ctx context.Context) ([]entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("catalog request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("catalog HTTP status rejected")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return nil, errors.New("catalog read failed or exceeded size limit")
	}
	entries, err := parseCatalog(body)
	if err != nil {
		return nil, errors.New("catalog format invalid")
	}
	return entries, nil
}

func validMissingName(name string) bool {
	return name == rateInput || name == rateOutput || name == rateCacheRead || name == rateCacheWrite
}

func validMissing(r Rates, missing []string) bool {
	seen := map[string]bool{}
	for _, name := range missing {
		if !validMissingName(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	if seen[rateInput] && r.Input != 0 || seen[rateOutput] && r.Output != 0 ||
		seen[rateCacheRead] && r.CacheRead != 0 || seen[rateCacheWrite] && r.CacheWrite != 0 {
		return false
	}
	return true
}

func validTierList(tiers []tier) bool {
	if len(tiers) > maxCatalogTiers {
		return false
	}
	var last int64
	for _, t := range tiers {
		if t.Size <= 0 || t.Size > maxSafe || (last != 0 && t.Size <= last) || !validRates(t.Rates) || !validMissing(t.Rates, t.MissingRates) {
			return false
		}
		last = t.Size
	}
	return true
}

func validEntry(e entry) bool {
	if !validIdentity(e.Model) || !validIdentity(e.Provider) || !validRates(e.Rates) || !validMissing(e.Rates, e.MissingRates) {
		return false
	}
	if e.CanonicalModel != "" && !validIdentity(e.CanonicalModel) {
		return false
	}
	if e.TiersInvalid && len(e.Tiers) > 0 {
		return false
	}
	return validTierList(e.Tiers)
}

func (s *Service) loadCache() {
	if s.cachePath == "" {
		return
	}
	f, err := os.Open(s.cachePath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.lastError = "catalog cache read failed"
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	var d diskCache
	if err != nil || len(body) > maxCatalogBytes || json.Unmarshal(body, &d) != nil || len(d.Entries) == 0 || len(d.Entries) > 100000 || d.LastSync.IsZero() {
		s.lastError = "catalog cache invalid"
		return
	}
	seen := map[identity]bool{}
	for _, e := range d.Entries {
		k := identity{e.Model, e.Provider}
		if !validEntry(e) || seen[k] {
			s.lastError = "catalog cache invalid"
			return
		}
		seen[k] = true
	}
	s.catalog = indexCatalog(d.Entries)
	s.lastSync = d.LastSync
}

func (s *Service) saveCache(d diskCache) error {
	if s.cachePath == "" {
		return nil
	}
	body, err := json.Marshal(d)
	if err != nil || len(body) > maxCatalogBytes {
		return errors.New("catalog cache too large")
	}
	dir := filepath.Dir(s.cachePath)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pricing-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.cachePath)
}
