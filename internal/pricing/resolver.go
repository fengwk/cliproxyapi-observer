package pricing

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

type resolution struct {
	source          string
	match           string
	reason          string
	definition      PricingDefinition
	supported       bool
	supportIssues   []string
	catalogProvider string
	catalogModel    string
	catalogTime     *time.Time
	candidates      []Candidate
}

func zeroResolution(reason, match string) resolution {
	return resolution{source: "zero", match: match, reason: reason, definition: zeroDefinition(), supported: true}
}

func manualResolution(p ManualPrice) resolution {
	return resolution{
		source: "manual", match: matchManual, reason: reasonManual,
		definition: CloneDefinition(p.Definition), supported: true,
	}
}

func (s *Service) catalogTimeLocked() *time.Time {
	state := s.sourceStateLocked(s.config.Source)
	if state.lastSync.IsZero() {
		return nil
	}
	t := state.lastSync.UTC()
	return &t
}

func candidateFromEntry(e entry) Candidate {
	return Candidate{
		Provider: e.Provider, Model: e.Model, Rates: ratesFromSet(e.Definition.Base),
		Definition: CloneDefinition(e.Definition), PricingSupported: e.Supported,
		SupportIssues: cloneStrings(e.SupportIssues),
	}
}

func candidateList(entries []entry) []Candidate {
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, candidateFromEntry(e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		if out[i].PricingSupported != out[j].PricingSupported {
			return !out[i].PricingSupported
		}
		return out[i].Definition.Base.Input < out[j].Definition.Base.Input ||
			out[i].Definition.Base.Input == out[j].Definition.Base.Input &&
				out[i].Definition.Base.Output < out[j].Definition.Base.Output
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

func (s *Service) fromEntry(e entry, match string) resolution {
	if !e.Supported {
		r := zeroResolution(reasonPricingUnsupported, matchAmbiguous)
		r.supported = false
		r.supportIssues = cloneStrings(e.SupportIssues)
		r.catalogProvider, r.catalogModel, r.catalogTime = e.Provider, e.Model, s.catalogTimeLocked()
		r.candidates = []Candidate{candidateFromEntry(e)}
		return r
	}
	return resolution{
		source: "auto", match: match, definition: CloneDefinition(e.Definition), supported: true,
		supportIssues:   cloneStrings(e.SupportIssues),
		catalogProvider: e.Provider, catalogModel: e.Model, catalogTime: s.catalogTimeLocked(),
	}
}

func definitionsEqual(a, b PricingDefinition) bool {
	return reflect.DeepEqual(a, b)
}

func entriesPricingEqual(a, b entry) bool {
	return a.Supported == b.Supported && definitionsEqual(a.Definition, b.Definition) &&
		stringsEqual(a.SupportIssues, b.SupportIssues)
}

func isPriceVariant(model string) bool {
	lower := strings.ToLower(model)
	for _, suffix := range []string{":free", "-free", ":paid", "-paid"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func mergeCandidates(lists ...[]entry) []Candidate {
	seen := map[identity]bool{}
	merged := []entry{}
	for _, list := range lists {
		for _, e := range list {
			k := identity{Model: e.Model, Provider: e.Provider}
			if !seen[k] {
				seen[k] = true
				merged = append(merged, e)
			}
		}
	}
	return candidateList(merged)
}

// resolveLocked implements the single identity resolver used by Preview,
// Quote and Snapshot. The caller must hold s.mu.
func (s *Service) resolveLocked(model, provider string) resolution {
	for _, p := range s.config.Prices {
		if p.Model == model && p.Provider == provider {
			return manualResolution(p)
		}
	}
	if !s.config.Auto {
		return zeroResolution(reasonAutoDisabled, matchNone)
	}
	index := s.sourceStateLocked(s.config.Source).catalog
	if index.empty() {
		return zeroResolution(reasonCatalogUnavailable, matchNone)
	}

	// An administrator mapping is authoritative for the complete public ID.
	for _, m := range s.config.Mappings {
		if m.Model != model {
			continue
		}
		e, ok := index.exactEntry(m.CatalogModel, m.Provider)
		if !ok {
			r := zeroResolution(reasonMappingMissing, matchNone)
			r.candidates = candidateList(index.candidateEntries(m.CatalogModel, ""))
			r.catalogProvider, r.catalogModel, r.catalogTime = m.Provider, m.CatalogModel, s.catalogTimeLocked()
			return r
		}
		return s.fromEntry(e, matchMapping)
	}

	if ns, inner, ok := splitKnownNamespace(model); ok {
		return s.resolveNamespaceLocked(model, provider, ns, inner)
	}
	if provider != "" {
		return s.resolveProviderLocked(model, provider)
	}
	return s.resolveProviderlessLocked(model)
}

func (s *Service) resolveNamespaceLocked(model, provider, ns, inner string) resolution {
	index := s.sourceStateLocked(s.config.Source).catalog
	if provider != "" && provider != ns {
		r := zeroResolution(reasonProviderConflict, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		lists := [][]entry{}
		for _, candidate := range []struct{ model, provider string }{
			{model, provider}, {model, ns}, {inner, provider}, {inner, ns},
		} {
			if e, ok := index.exactEntry(candidate.model, candidate.provider); ok {
				lists = append(lists, []entry{e})
			}
		}
		r.candidates = mergeCandidates(lists...)
		return r
	}
	if provider != "" {
		if e, ok := index.exactEntry(model, provider); ok {
			return s.fromEntry(e, matchProvider)
		}
	}
	if e, ok := index.exactEntry(inner, ns); ok {
		return s.fromEntry(e, matchNamespace)
	}
	variants := index.canonicalEntries(inner, ns)
	if r, ok := s.canonicalWithin(variants); ok {
		return r
	}
	r := zeroResolution(reasonNotFound, matchNone)
	r.catalogTime = s.catalogTimeLocked()
	r.candidates = candidateList(index.candidateEntries(inner, ns))
	return r
}

func (s *Service) resolveProviderLocked(model, provider string) resolution {
	index := s.sourceStateLocked(s.config.Source).catalog
	if e, ok := index.exactEntry(model, provider); ok {
		return s.fromEntry(e, matchProvider)
	}
	variants := index.canonicalEntries(model, provider)
	if r, ok := s.canonicalWithin(variants); ok {
		return r
	}
	r := zeroResolution(reasonNotFound, matchNone)
	r.catalogTime = s.catalogTimeLocked()
	r.candidates = candidateList(index.candidateEntries(model, provider))
	if len(r.candidates) == 0 && len(variants) > 0 {
		r.candidates = candidateList(variants)
	}
	return r
}

func (s *Service) resolveProviderlessLocked(model string) resolution {
	index := s.sourceStateLocked(s.config.Source).catalog
	entries := index.modelEntries(model)
	if len(entries) > 0 {
		first := entries[0]
		consistent := true
		for _, e := range entries[1:] {
			if !entriesPricingEqual(first, e) {
				consistent = false
				break
			}
		}
		if !consistent {
			r := zeroResolution(reasonAmbiguous, matchAmbiguous)
			r.catalogTime = s.catalogTimeLocked()
			r.candidates = candidateList(entries)
			return r
		}
		return s.fromEntry(first, matchExact)
	}
	if canonical := index.canonicalAnyEntries(model); len(canonical) > 0 {
		r := zeroResolution(reasonAmbiguous, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(canonical)
		return r
	}
	r := zeroResolution(reasonNotFound, matchNone)
	r.catalogTime = s.catalogTimeLocked()
	return r
}

func (s *Service) canonicalWithin(entries []entry) (resolution, bool) {
	safe := make([]entry, 0, len(entries))
	for _, e := range entries {
		if !isPriceVariant(e.Model) {
			safe = append(safe, e)
		}
	}
	switch {
	case len(safe) == 1:
		return s.fromEntry(safe[0], matchCanonical), true
	case len(safe) > 1:
		r := zeroResolution(reasonAmbiguous, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(safe)
		return r, true
	default:
		return resolution{}, false
	}
}

func (idx catalogIndex) empty() bool { return len(idx.exact) == 0 }

func (idx catalogIndex) count() int {
	n := 0
	for _, list := range idx.exact {
		n += len(list)
	}
	return n
}

func (r resolution) price(model, provider string) Price {
	return Price{
		Model: model, Provider: provider, Rates: ratesFromSet(r.definition.Base),
		Definition: CloneDefinition(r.definition), Source: r.source, Reason: r.reason, Match: r.match,
		CatalogProvider: r.catalogProvider, CatalogModel: r.catalogModel, CatalogTime: cloneTime(r.catalogTime),
		PricingSupported: r.supported, SupportIssues: cloneStrings(r.supportIssues), Candidates: cloneCandidates(r.candidates),
	}
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
