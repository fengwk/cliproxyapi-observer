package pricing

import (
	"math/big"
	"sort"
	"strings"
	"time"
)

type resolution struct {
	source          string
	match           string
	reason          string
	rates           Rates
	missing         []string
	tiers           []tier
	catalogProvider string
	catalogModel    string
	catalogTime     *time.Time
	candidates      []Candidate
}

func allRateNames() []string { return rateFieldNames() }

func unpriced(reason, match string) resolution {
	return resolution{source: "unpriced", match: match, reason: reason, missing: allRateNames()}
}

func manualResolution(p Price) resolution {
	return resolution{source: "manual", match: matchManual, reason: reasonManual, rates: cloneRates(p.Rates)}
}

func (s *Service) catalogTimeLocked() *time.Time {
	if s.lastSync.IsZero() {
		return nil
	}
	t := s.lastSync.UTC()
	return &t
}

func candidateList(entries []entry) []Candidate {
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		missing := cloneStrings(e.MissingRates)
		if e.TiersInvalid {
			missing = allRateNames()
		}
		out = append(out, Candidate{Provider: e.Provider, Model: e.Model, Rates: cloneRates(e.Rates), MissingRates: missing})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return candidateLess(entry{Provider: out[i].Provider, Model: out[i].Model, Rates: out[i].Rates, MissingRates: out[i].MissingRates},
			entry{Provider: out[j].Provider, Model: out[j].Model, Rates: out[j].Rates, MissingRates: out[j].MissingRates})
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

func (s *Service) fromEntry(e entry, match string) resolution {
	r := resolution{
		source: "auto", match: match, rates: cloneRates(e.Rates), missing: cloneStrings(e.MissingRates),
		tiers:           cloneTiers(e.Tiers),
		catalogProvider: e.Provider, catalogModel: e.Model, catalogTime: s.catalogTimeLocked(),
	}
	switch {
	case e.TiersInvalid:
		// Never present a partially understood tier table as a flat price.
		r.source, r.reason = "unpriced", reasonTiersUnsupported
		r.rates, r.missing = Rates{}, allRateNames()
	case len(e.Tiers) > 0:
		r.reason = reasonContextTiers
	case len(e.MissingRates) > 0:
		r.reason = reasonMissingRates
	}
	return r
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
		if p.Model == model {
			return manualResolution(p)
		}
	}
	if !s.config.Auto {
		return unpriced(reasonAutoDisabled, matchNone)
	}
	if s.catalog.empty() {
		return unpriced(reasonCatalogUnavailable, matchNone)
	}

	// 1. An administrator mapping is authoritative for the full public ID.
	for _, m := range s.config.Mappings {
		if m.Model != model {
			continue
		}
		e, ok := s.catalog.exactEntry(m.CatalogModel, m.Provider)
		if !ok {
			r := unpriced(reasonMappingMissing, matchNone)
			r.candidates = candidateList(s.catalog.candidateEntries(m.CatalogModel, ""))
			r.catalogProvider, r.catalogModel, r.catalogTime = m.Provider, m.CatalogModel, s.catalogTimeLocked()
			return r
		}
		return s.fromEntry(e, matchMapping)
	}

	// 2. A trusted real provider plus an exact catalog ID.
	if provider != "" {
		if e, ok := s.catalog.exactEntry(model, provider); ok {
			return s.fromEntry(e, matchProvider)
		}
	}

	// 3. Verified public outer namespaces. Only these exact prefixes are
	// removed; inner slashes, dates, version suffixes and :free stay intact.
	if ns, inner, ok := splitKnownNamespace(model); ok {
		if provider != "" && provider != ns {
			// Two trusted clues point at different providers. Do not pick one.
			r := unpriced(reasonProviderConflict, matchAmbiguous)
			r.catalogTime = s.catalogTimeLocked()
			a, aok := s.catalog.exactEntry(inner, provider)
			b, bok := s.catalog.exactEntry(inner, ns)
			r.candidates = mergeCandidates(entrySlice(a, aok), entrySlice(b, bok))
			return r
		}
		if e, ok := s.catalog.exactEntry(inner, ns); ok {
			return s.fromEntry(e, matchNamespace)
		}
		// Canonical aliases are usable only inside the same verified provider.
		variants := s.catalog.canonicalEntries(inner, ns)
		if r, ok := canonicalWithin(s, variants); ok {
			return r
		}
		// Other channels are explicit candidates only, never an automatic price.
		r := unpriced(reasonNotFound, matchNone)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(s.catalog.candidateEntries(inner, ns))
		if len(r.candidates) == 0 && len(variants) > 0 {
			r.reason = reasonVariantOnly
			r.candidates = candidateList(variants)
		}
		return r
	}

	// 4/5. Without a trusted channel, only an exact identity with one complete
	// set of rates across every exact provider may be selected automatically.
	entries := s.catalog.modelEntries(model)
	if len(entries) > 0 {
		first := entries[0]
		consistent := true
		for _, e := range entries[1:] {
			if !pricingEqual(first, e) {
				consistent = false
				break
			}
		}
		if consistent {
			return s.fromEntry(first, matchExact)
		}
		r := unpriced(reasonAmbiguous, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(entries)
		return r
	}

	if provider != "" {
		// A canonical alias from the same provider is explicit catalog evidence.
		variants := s.catalog.canonicalEntries(model, provider)
		if r, ok := canonicalWithin(s, variants); ok {
			return r
		}
		r := unpriced(reasonNotFound, matchNone)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(s.catalog.candidateEntries(model, provider))
		if len(r.candidates) == 0 && len(variants) > 0 {
			r.reason = reasonVariantOnly
			r.candidates = candidateList(variants)
		}
		return r
	}

	// Canonical identity across providers is not a channel price. Surface the
	// candidates instead of choosing an arbitrary provider.
	if canonical := s.catalog.canonicalAnyEntries(model); len(canonical) > 0 {
		r := unpriced(reasonAmbiguous, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(canonical)
		return r
	}
	r := unpriced(reasonNotFound, matchNone)
	r.catalogTime = s.catalogTimeLocked()
	return r
}

func entrySlice(e entry, ok bool) []entry {
	if !ok {
		return nil
	}
	return []entry{e}
}

func canonicalWithin(s *Service, entries []entry) (resolution, bool) {
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
		r := unpriced(reasonAmbiguous, matchAmbiguous)
		r.catalogTime = s.catalogTimeLocked()
		r.candidates = candidateList(safe)
		return r, true
	default:
		// Only discounted/free aliases exist. They are a different pricing
		// variant of the canonical model, never an automatic substitute for
		// the requested ID. The caller adds any other exact-channel candidates.
		return resolution{}, false
	}
}

func (idx catalogIndex) empty() bool { return len(idx.exact) == 0 }

func (r resolution) price(model, provider string) Price {
	return Price{
		Model: model, Provider: provider, Rates: cloneRates(r.rates), Source: r.source,
		Reason: r.reason, Match: r.match, CatalogProvider: r.catalogProvider, CatalogModel: r.catalogModel,
		CatalogTime: cloneTime(r.catalogTime), MissingRates: cloneStrings(r.missing), Candidates: cloneCandidates(r.candidates),
	}
}

func applyTier(r *resolution, prompt *big.Int) {
	for _, t := range r.tiers {
		if prompt.Cmp(big.NewInt(t.Size)) > 0 {
			r.rates = cloneRates(t.Rates)
			r.missing = cloneStrings(t.MissingRates)
		}
	}
}

func missingSet(missing []string) map[string]bool {
	m := make(map[string]bool, len(missing))
	for _, name := range missing {
		m[name] = true
	}
	return m
}
