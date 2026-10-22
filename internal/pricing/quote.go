package pricing

import (
	"errors"
	"math/big"
	"sort"
	"time"
)

type evalFacts struct {
	context *big.Int
	at      time.Time
	tier    string
}

func contextTokens(t Tokens) *big.Int {
	n := new(big.Int).SetInt64(t.Input)
	n.Add(n, big.NewInt(t.CacheRead))
	n.Add(n, big.NewInt(t.CacheWrite))
	return n
}

// evalCondition reports match and whether every required fact was available.
// A known false/true branch short-circuits an unknown sibling in AND/ANY, so
// missing facts only poison a result that actually depends on them.
func evalCondition(c Condition, f evalFacts) (bool, bool) {
	result, known := true, true
	apply := func(match, childKnown bool) {
		if childKnown {
			if !match {
				result, known = false, true
			}
			return
		}
		if result {
			known = false
		}
	}
	if c.Context != nil {
		ctx := c.Context
		match := !(ctx.GT != nil && f.context.Cmp(big.NewInt(*ctx.GT)) <= 0 ||
			ctx.GTE != nil && f.context.Cmp(big.NewInt(*ctx.GTE)) < 0 ||
			ctx.LT != nil && f.context.Cmp(big.NewInt(*ctx.LT)) >= 0 ||
			ctx.LTE != nil && f.context.Cmp(big.NewInt(*ctx.LTE)) > 0)
		apply(match, true)
	}
	if c.ServiceTier != nil {
		if f.tier == "" {
			apply(false, false)
		} else {
			found := false
			for _, tier := range c.ServiceTier {
				if tier == f.tier {
					found = true
					break
				}
			}
			apply(found, true)
		}
	}
	if c.Date != nil {
		if f.at.IsZero() {
			apply(false, false)
		} else {
			date := f.at.UTC().Format("2006-01-02")
			match := (c.Date.GTE == "" || date >= c.Date.GTE) && (c.Date.LT == "" || date < c.Date.LT)
			apply(match, true)
		}
	}
	if c.Time != nil {
		if f.at.IsZero() {
			apply(false, false)
		} else {
			apply(evalTime(*c.Time, f.at))
		}
	}
	if c.All != nil {
		for _, child := range c.All {
			apply(evalCondition(child, f))
		}
	}
	if c.Any != nil {
		if len(c.Any) == 0 {
			apply(false, true)
		} else {
			anyMatch, anyUnknown := false, false
			for _, child := range c.Any {
				match, childKnown := evalCondition(child, f)
				if !childKnown {
					anyUnknown = true
					continue
				}
				if match {
					anyMatch, anyUnknown = true, false
					break
				}
			}
			if anyMatch {
				apply(true, true)
			} else if anyUnknown {
				apply(false, false)
			} else {
				apply(false, true)
			}
		}
	}
	if c.Not != nil {
		match, childKnown := evalCondition(*c.Not, f)
		if childKnown {
			apply(!match, true)
		} else {
			apply(false, false)
		}
	}
	return result, known
}

func evalTime(t TimeCondition, at time.Time) (bool, bool) {
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return false, false
	}
	local := at.In(loc)
	minute := local.Hour()*60 + local.Minute()
	start, _ := parseClock(t.Start, false)
	end, _ := parseClock(t.End, true)
	cross := start > end
	matched := minute >= start && minute < end
	if cross {
		matched = minute >= start || minute < end
	}
	if len(t.Weekdays) > 0 {
		day := isoWeekday(local.Weekday())
		// A range after midnight still belongs to its start day.
		if cross && minute < end {
			day--
			if day == 0 {
				day = 7
			}
		}
		inDay := false
		for _, allowed := range t.Weekdays {
			if allowed == day {
				inDay = true
				break
			}
		}
		matched = matched && inDay
	}
	return matched, true
}

func isoWeekday(d time.Weekday) int {
	if d == time.Sunday {
		return 7
	}
	return int(d)
}

func applyPatch(set *RateSet, patch RatePatch) {
	if patch.Input != nil {
		set.Input = *patch.Input
	}
	if patch.Output != nil {
		set.Output = *patch.Output
	}
	if patch.CacheRead != nil {
		set.CacheRead = *patch.CacheRead
	}
	if patch.CacheWrite != nil {
		set.CacheWrite = *patch.CacheWrite
	}
	if patch.HasCacheWriteByTTL {
		set.CacheWriteByTTL = make(map[int64]int64, len(patch.CacheWriteByTTL))
		for k, v := range patch.CacheWriteByTTL {
			set.CacheWriteByTTL[k] = v
		}
	}
	if patch.ReasoningSet {
		if patch.Reasoning == nil {
			set.Reasoning = nil
		} else {
			v := *patch.Reasoning
			set.Reasoning = &v
		}
	}
}

type appliedMultiplier struct {
	id      string
	factors FactorSet
}

func resolveDefinition(def PricingDefinition, facts evalFacts) (RateSet, []appliedMultiplier, bool, error) {
	set := cloneRateSet(def.Base)
	for _, override := range def.Overrides {
		match, known := evalCondition(override.When, facts)
		if !known {
			return set, nil, true, nil
		}
		if match {
			applyPatch(&set, override.Rates)
		}
	}
	var multipliers []appliedMultiplier
	for _, multiplier := range def.Multipliers {
		match, known := evalCondition(multiplier.When, facts)
		if !known {
			return set, nil, true, nil
		}
		if match {
			multipliers = append(multipliers, appliedMultiplier{id: multiplier.ID, factors: multiplier.Factors})
		}
	}
	return set, multipliers, false, nil
}

func factorRat(s *string) (*big.Rat, error) {
	if s == nil {
		return big.NewRat(1, 1), nil
	}
	if !validFactorValue(*s) {
		return nil, errInvalid
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok || r.Sign() < 0 {
		return nil, errInvalid
	}
	return r, nil
}

func addTokenCost(sum *big.Rat, rate, tokens int64, factors []appliedMultiplier, field func(FactorSet) *string) error {
	if tokens == 0 || rate == 0 {
		return nil
	}
	term := new(big.Rat).SetInt64(rate)
	term.Mul(term, new(big.Rat).SetInt64(tokens))
	for _, multiplier := range factors {
		factor, err := factorRat(field(multiplier.factors))
		if err != nil {
			return err
		}
		term.Mul(term, factor)
	}
	term.Quo(term, new(big.Rat).SetInt64(1_000_000))
	sum.Add(sum, term)
	return nil
}

func calculateCost(set RateSet, factors []appliedMultiplier, t Tokens) (int64, error) {
	sum := new(big.Rat)
	if err := addTokenCost(sum, set.Input, t.Input, factors, func(f FactorSet) *string { return f.Input }); err != nil {
		return 0, err
	}
	output := t.Output
	if set.Reasoning != nil {
		output -= t.Reasoning
	}
	if err := addTokenCost(sum, set.Output, output, factors, func(f FactorSet) *string { return f.Output }); err != nil {
		return 0, err
	}
	if err := addTokenCost(sum, set.CacheRead, t.CacheRead, factors, func(f FactorSet) *string { return f.CacheRead }); err != nil {
		return 0, err
	}
	if len(t.CacheWriteByTTL) > 0 {
		var total int64
		for ttl, tokens := range t.CacheWriteByTTL {
			if tokens < 0 || total > maxSafe-tokens {
				return 0, errInvalid
			}
			total += tokens
			rate := set.CacheWrite
			if v, ok := set.CacheWriteByTTL[ttl]; ok {
				rate = v
			}
			if err := addTokenCost(sum, rate, tokens, factors, func(f FactorSet) *string { return f.CacheWrite }); err != nil {
				return 0, err
			}
		}
		if total != t.CacheWrite {
			return 0, errInvalid
		}
	} else if err := addTokenCost(sum, set.CacheWrite, t.CacheWrite, factors, func(f FactorSet) *string { return f.CacheWrite }); err != nil {
		return 0, err
	}
	if set.Reasoning != nil {
		if err := addTokenCost(sum, *set.Reasoning, t.Reasoning, factors, func(f FactorSet) *string { return f.Reasoning }); err != nil {
			return 0, err
		}
	}
	num, den := sum.Num(), sum.Denom()
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxSafe {
		return 0, errors.New("pricing cost overflow")
	}
	return q.Int64(), nil
}

func validateTokens(t Tokens) error {
	for _, v := range []int64{t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Reasoning, t.Total} {
		if v < 0 || v > maxSafe {
			return errInvalid
		}
	}
	if t.Reasoning > t.Output {
		return errInvalid
	}
	if len(t.CacheWriteByTTL) > maxTiers {
		return errInvalid
	}
	var split int64
	for ttl, tokens := range t.CacheWriteByTTL {
		if ttl <= 0 || tokens < 0 || split > maxSafe-tokens {
			return errInvalid
		}
		split += tokens
	}
	if len(t.CacheWriteByTTL) > 0 && split != t.CacheWrite {
		return errInvalid
	}
	return nil
}

func (s *Service) Quote(model, provider string, t Tokens, at time.Time, serviceTier string) (Quote, error) {
	if !validIdentity(model) || (provider != "" && !validIdentity(provider)) {
		return Quote{}, errInvalid
	}
	if err := validateTokens(t); err != nil {
		return Quote{}, err
	}
	facts := evalFacts{context: contextTokens(t), at: at, tier: serviceTier}
	s.mu.Lock()
	res := s.resolveLocked(model, provider)
	s.mu.Unlock()

	q := Quote{
		Source: res.source, Definition: CloneDefinition(res.definition), Reason: res.reason, Match: res.match,
		CatalogProvider: res.catalogProvider, CatalogModel: res.catalogModel, CatalogTime: cloneTime(res.catalogTime),
		PricingSupported: res.supported, SupportIssues: cloneStrings(res.supportIssues),
		Candidates: cloneCandidates(res.candidates),
	}
	if res.source == "zero" {
		q.Rates = Rates{}
		return q, nil
	}
	set, multipliers, missingFacts, err := resolveDefinition(res.definition, facts)
	if err != nil {
		return Quote{}, err
	}
	q.Rates = ratesFromSet(set)
	if missingFacts {
		q.Reason, q.CostMicroUSD = reasonMissingFacts, 0
		return q, nil
	}
	cost, err := calculateCost(set, multipliers, t)
	if err != nil {
		return Quote{}, err
	}
	q.CostMicroUSD = cost
	return q, nil
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sourceStateLocked(s.config.Source)
	out := Snapshot{Source: s.config.Source, Prices: []Price{}, LastSync: state.lastSync, Error: state.lastError, Refreshing: s.refreshingLocked()}
	out.CatalogEntries = state.catalog.count()
	seen := map[identity]bool{}
	for k := range s.observed {
		res := s.resolveLocked(k.Model, k.Provider)
		out.Prices = append(out.Prices, res.price(k.Model, k.Provider))
		seen[k] = true
	}
	for _, p := range s.config.Prices {
		k := identity{Model: p.Model, Provider: p.Provider}
		if !seen[k] {
			out.Prices = append(out.Prices, manualResolution(p).price(p.Model, p.Provider))
		}
	}
	sortPrices(out.Prices)
	return out
}

func sortPrices(prices []Price) {
	sort.Slice(prices, func(i, j int) bool {
		a, b := prices[i], prices[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Definition.Base.Input < b.Definition.Base.Input ||
			a.Definition.Base.Input == b.Definition.Base.Input && a.Definition.Base.Output < b.Definition.Base.Output
	})
}

// Preview returns base prices and the complete definition without evaluating
// conditions against fictional usage.
func (s *Service) Preview(models []ModelIdentity) ([]Price, error) {
	if len(models) > maxDefinitions {
		return nil, errInvalid
	}
	for _, model := range models {
		if !validIdentity(model.Model) || (model.Provider != "" && !validIdentity(model.Provider)) {
			return nil, errInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Price, 0, len(models))
	for _, model := range models {
		out = append(out, s.resolveLocked(model.Model, model.Provider).price(model.Model, model.Provider))
	}
	return out, nil
}
