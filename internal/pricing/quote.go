package pricing

import (
	"errors"
	"math/big"
	"sort"
	"time"
)

func requiredMissing(t Tokens, rates Rates, missing map[string]bool) []string {
	out := []string{}
	output := t.Output
	if rates.Reasoning != nil {
		output -= t.Reasoning
	}
	if t.Input > 0 && missing[rateInput] {
		out = append(out, rateInput)
	}
	if output > 0 && missing[rateOutput] {
		out = append(out, rateOutput)
	}
	if t.CacheRead > 0 && missing[rateCacheRead] {
		out = append(out, rateCacheRead)
	}
	if t.CacheWrite > 0 && missing[rateCacheWrite] {
		out = append(out, rateCacheWrite)
	}
	return out
}

func (s *Service) Quote(model, provider string, t Tokens, at time.Time) (Quote, error) {
	for _, v := range []int64{t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Reasoning, t.Total} {
		if v < 0 {
			return Quote{}, errInvalid
		}
	}
	if t.Reasoning > t.Output {
		return Quote{}, errInvalid
	}
	// Context thresholds include the whole prompt, not just billable uncached
	// input. big.Int also handles legitimate sums exceeding signed int64.
	prompt := new(big.Int).SetInt64(t.Input)
	prompt.Add(prompt, big.NewInt(t.CacheRead))
	prompt.Add(prompt, big.NewInt(t.CacheWrite))
	s.mu.Lock()
	res := s.resolveLocked(model, provider)
	ruleID := ""
	for _, r := range s.config.Rules {
		if r.Model != model || (r.InputTokensGT != nil && prompt.Cmp(big.NewInt(*r.InputTokensGT)) <= 0) {
			continue
		}
		if r.TimeRange != "" {
			a, b, _ := parseRange(r.TimeRange)
			u := at.UTC()
			n := u.Hour()*60 + u.Minute()
			if a < b && !(n >= a && n < b) || a > b && !(n >= a || n < b) {
				continue
			}
		}
		res = resolution{source: "rule", match: matchRule, reason: reasonRule, rates: cloneRates(r.Rates)}
		ruleID = r.ID
		break
	}
	if res.source == "auto" {
		applyTier(&res, prompt)
	}
	required := []string(nil)
	if res.source == "auto" || res.source == "manual" || res.source == "rule" {
		required = requiredMissing(t, res.rates, missingSet(res.missing))
	}
	if len(required) > 0 {
		res.source, res.reason, res.rates, res.missing = "unpriced", reasonMissingRates, Rates{}, required
	}
	s.mu.Unlock()

	q := Quote{
		Source: res.source, RuleID: ruleID, Rates: cloneRates(res.rates), Reason: res.reason, Match: res.match,
		CatalogProvider: res.catalogProvider, CatalogModel: res.catalogModel, CatalogTime: cloneTime(res.catalogTime),
		MissingRates: cloneStrings(res.missing), Candidates: cloneCandidates(res.candidates),
	}
	if res.source == "unpriced" || len(required) > 0 {
		return q, nil
	}
	output := t.Output
	sum := new(big.Int)
	add := func(rate, tokens int64) { sum.Add(sum, new(big.Int).Mul(big.NewInt(rate), big.NewInt(tokens))) }
	if q.Rates.Reasoning != nil {
		output -= t.Reasoning
		add(*q.Rates.Reasoning, t.Reasoning)
	}
	add(q.Rates.Input, t.Input)
	add(q.Rates.Output, output)
	add(q.Rates.CacheRead, t.CacheRead)
	add(q.Rates.CacheWrite, t.CacheWrite)
	sum.Add(sum, big.NewInt(999999))
	sum.Quo(sum, big.NewInt(1000000))
	if !sum.IsInt64() {
		return Quote{}, errors.New("pricing cost overflow")
	}
	q.CostMicroUSD = sum.Int64()
	return q, nil
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Prices: []Price{}, LastSync: s.lastSync, Error: s.lastError, Refreshing: s.flight != nil}
	for _, list := range s.catalog.exact {
		out.CatalogEntries += len(list)
	}
	seen := map[string]bool{}
	for k := range s.observed {
		q := s.resolveLocked(k.Model, k.Provider)
		out.Prices = append(out.Prices, q.price(k.Model, k.Provider))
		seen[k.Model] = true
	}
	for _, p := range s.config.Prices {
		if !seen[p.Model] {
			q := manualResolution(p)
			out.Prices = append(out.Prices, q.price(p.Model, ""))
		}
	}
	sort.Slice(out.Prices, func(i, j int) bool {
		if out.Prices[i].Model != out.Prices[j].Model {
			return out.Prices[i].Model < out.Prices[j].Model
		}
		if out.Prices[i].Provider != out.Prices[j].Provider {
			return out.Prices[i].Provider < out.Prices[j].Provider
		}
		if out.Prices[i].Source != out.Prices[j].Source {
			return out.Prices[i].Source < out.Prices[j].Source
		}
		a, b := out.Prices[i].Rates, out.Prices[j].Rates
		av, bv := []int64{a.Input, a.Output, a.CacheRead, a.CacheWrite}, []int64{b.Input, b.Output, b.CacheRead, b.CacheWrite}
		for n := range av {
			if av[n] != bv[n] {
				return av[n] < bv[n]
			}
		}
		if a.Reasoning == nil {
			return b.Reasoning != nil
		}
		if b.Reasoning == nil {
			return false
		}
		return *a.Reasoning < *b.Reasoning
	})
	return out
}

// Preview does not Observe or fetch. Conditional rules are flagged instead of
// evaluated against a fictional zero-token request, preserving accounting.
func (s *Service) Preview(models []ModelIdentity) ([]PreviewPrice, error) {
	if len(models) > 4096 {
		return nil, errInvalid
	}
	for _, model := range models {
		if !validIdentity(model.Model) || len(model.Provider) > 1024 || (model.Provider != "" && !validIdentity(model.Provider)) {
			return nil, errInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := make(map[string]bool, len(s.config.Rules))
	for _, rule := range s.config.Rules {
		rules[rule.Model] = true
	}
	out := make([]PreviewPrice, 0, len(models))
	for _, model := range models {
		q := s.resolveLocked(model.Model, model.Provider)
		out = append(out, PreviewPrice{Price: q.price(model.Model, model.Provider), HasRules: rules[model.Model]})
	}
	return out, nil
}
