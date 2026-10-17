package pricing

import (
	"errors"
	"math/big"
	"sort"
	"time"
)

func (s *Service) effective(model, provider string) Quote {
	for _, p := range s.config.Prices {
		if p.Model == model {
			return Quote{Source: "manual", Rates: cloneRates(p.Rates)}
		}
	}
	if s.config.Auto {
		if r, ok := lookup(s.catalog, model, provider); ok {
			return Quote{Source: "auto", Rates: cloneRates(r)}
		}
	}
	return Quote{Source: "unpriced"}
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
	q := s.effective(model, provider)
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
		q = Quote{Source: "rule", RuleID: r.ID, Rates: cloneRates(r.Rates)}
		break
	}
	s.mu.Unlock()
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
	seen := map[string]bool{}
	for k := range s.observed {
		q := s.effective(k.Model, k.Provider)
		out.Prices = append(out.Prices, Price{Model: k.Model, Provider: k.Provider, Rates: q.Rates, Source: q.Source})
		seen[k.Model] = true
	}
	for _, p := range s.config.Prices {
		if !seen[p.Model] {
			out.Prices = append(out.Prices, Price{Model: p.Model, Rates: cloneRates(p.Rates), Source: "manual"})
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
