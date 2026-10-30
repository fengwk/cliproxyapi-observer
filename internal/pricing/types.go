// Package pricing resolves an internal pricing definition and calculates an
// immutable micro-USD quote without network I/O on the request path.
package pricing

import (
	"context"
	"errors"
	"time"
)

const (
	// SourceV1 and SourceV2 are the only accepted external catalog sources.
	// V2 is the default; the two sources are never mixed.
	SourceV1 = "models_dev_v1"
	SourceV2 = "models_dev_v2"

	maxSafe           = 9_000_000_000_000_000
	maxCandidates     = 32
	maxDefinitions    = 4096
	maxMappings       = 4096
	maxRulesPerModel  = 64
	maxConditionNodes = 64
	maxConditionDepth = 8
	maxConditionList  = 16
	maxTiers          = 16
)

// Rate names are diagnostic identifiers for support issues, not absence
// markers. Missing catalog prices are normalized to zero.
const (
	rateInput      = "input"
	rateOutput     = "output"
	rateCacheRead  = "cache_read"
	rateCacheWrite = "cache_write"
)

const (
	matchManual    = "manual"
	matchMapping   = "mapping"
	matchProvider  = "provider"
	matchNamespace = "namespace"
	matchCanonical = "canonical"
	matchExact     = "exact"
	matchAmbiguous = "ambiguous"
	matchNone      = "none"

	reasonManual             = "manual"
	reasonAutoDisabled       = "auto_disabled"
	reasonCatalogUnavailable = "catalog_unavailable"
	reasonNotFound           = "not_found"
	reasonAmbiguous          = "ambiguous"
	reasonProviderConflict   = "provider_conflict"
	reasonMappingMissing     = "mapping_missing"
	reasonPricingUnsupported = "pricing_unsupported"
	reasonMissingFacts       = "missing_facts"
)

// RateSet is an effective token price in USD per million tokens represented as
// integer micro-USD. Reasoning is null to inherit output, or an independent
// price (including an explicit zero). CacheWriteByTTL overrides CacheWrite for
// an exactly matching positive TTL in seconds.
type RateSet struct {
	Input           int64           `json:"input"`
	Output          int64           `json:"output"`
	CacheRead       int64           `json:"cache_read"`
	CacheWrite      int64           `json:"cache_write"`
	CacheWriteByTTL map[int64]int64 `json:"cache_write_by_ttl"`
	Reasoning       *int64          `json:"reasoning"`
}

// Condition combines every populated field with AND. Empty objects are always
// true. Time ranges are left-closed/right-open; weekdays use ISO 1=Monday and
// belong to the start day for a range crossing midnight.
type Condition struct {
	Context     *ContextCondition     `json:"context,omitempty"`
	ServiceTier []string              `json:"serviceTier,omitempty"`
	Date        *DateCondition        `json:"date,omitempty"`
	Time        *TimeCondition        `json:"time,omitempty"`
	RequestBody *RequestBodyCondition `json:"requestBody,omitempty"`
	All         []Condition           `json:"all,omitempty"`
	Any         []Condition           `json:"any,omitempty"`
	Not         *Condition            `json:"not,omitempty"`
}

type ContextCondition struct {
	GT  *int64 `json:"gt,omitempty"`
	GTE *int64 `json:"gte,omitempty"`
	LT  *int64 `json:"lt,omitempty"`
	LTE *int64 `json:"lte,omitempty"`
}

type DateCondition struct {
	GTE string `json:"gte,omitempty"`
	LT  string `json:"lt,omitempty"`
}

type TimeCondition struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
	Weekdays []int  `json:"weekdays,omitempty"`
}

// RatePatch leaves omitted fields unchanged. ReasoningSet distinguishes an
// omitted reasoning field from an explicit null, which restores inheritance.
type RatePatch struct {
	Input              *int64
	Output             *int64
	CacheRead          *int64
	CacheWrite         *int64
	CacheWriteByTTL    map[int64]int64
	HasCacheWriteByTTL bool
	Reasoning          *int64
	ReasoningSet       bool
}

type Override struct {
	ID    string    `json:"id"`
	When  Condition `json:"when"`
	Rates RatePatch `json:"rates"`
}

type FactorSet struct {
	Input      *string `json:"input,omitempty"`
	Output     *string `json:"output,omitempty"`
	CacheRead  *string `json:"cache_read,omitempty"`
	CacheWrite *string `json:"cache_write,omitempty"`
	Reasoning  *string `json:"reasoning,omitempty"`
}

type Multiplier struct {
	ID      string    `json:"id"`
	When    Condition `json:"when"`
	Factors FactorSet `json:"factors"`
}

// PricingDefinition is the complete internal price. The base rates are always
// present; overrides and multipliers are applied in array order.
type PricingDefinition struct {
	Base        RateSet      `json:"base"`
	Overrides   []Override   `json:"overrides"`
	Multipliers []Multiplier `json:"multipliers"`
}

// ManualPrice is a complete manual identity. An empty provider only matches an
// actual empty observed provider; it is never a model-wide wildcard.
type ManualPrice struct {
	Model      string            `json:"model"`
	Provider   string            `json:"provider,omitempty"`
	Definition PricingDefinition `json:"definition"`
}

// Mapping chooses an exact catalog identity for a complete public model ID.
// It affects accounting identity only and never changes request routing.
type Mapping struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	CatalogModel string `json:"catalog_model"`
}

type Config struct {
	Auto     bool          `json:"auto"`
	Source   string        `json:"source"`
	Prices   []ManualPrice `json:"prices"`
	Mappings []Mapping     `json:"mappings,omitempty"`
}

// Rates is the outward legacy-shaped base/effective rate projection. Its keys
// remain snake_case because the existing management/UI contract uses them.
type Rates struct {
	Input           int64           `json:"input"`
	Output          int64           `json:"output"`
	CacheRead       int64           `json:"cache_read"`
	CacheWrite      int64           `json:"cache_write"`
	CacheWriteByTTL map[int64]int64 `json:"cache_write_by_ttl,omitempty"`
	Reasoning       *int64          `json:"reasoning,omitempty"`
}

// Candidate is one exact catalog identity that could explain an ambiguous or
// unsupported identity. An unsupported candidate carries a complete zero
// definition so callers never import a half-understood price.
type Candidate struct {
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	Rates            Rates             `json:"rates"`
	Definition       PricingDefinition `json:"definition"`
	PricingSupported bool              `json:"pricing_supported"`
	SupportIssues    []string          `json:"support_issues,omitempty"`
}

type Price struct {
	Model            string            `json:"model"`
	Provider         string            `json:"provider,omitempty"`
	Rates            Rates             `json:"rates"`
	Definition       PricingDefinition `json:"definition"`
	Source           string            `json:"source"`
	Reason           string            `json:"reason,omitempty"`
	Match            string            `json:"match,omitempty"`
	CatalogProvider  string            `json:"catalog_provider,omitempty"`
	CatalogModel     string            `json:"catalog_model,omitempty"`
	CatalogTime      *time.Time        `json:"catalog_time,omitempty"`
	PricingSupported bool              `json:"pricing_supported"`
	SupportIssues    []string          `json:"support_issues,omitempty"`
	Candidates       []Candidate       `json:"candidates,omitempty"`
}

type Tokens struct {
	// Input is uncached and disjoint from CacheRead and CacheWrite.
	Input int64 `json:"input"`
	// Output includes Reasoning.
	Output int64 `json:"output"`
	// CacheWrite is the total cache-write token count. CacheWriteByTTL, when
	// populated, must partition it exactly; unknown TTLs use the generic rate.
	CacheWrite      int64           `json:"cache_write"`
	CacheWriteByTTL map[int64]int64 `json:"cache_write_by_ttl,omitempty"`
	CacheRead       int64           `json:"cache_read"`
	Reasoning       int64           `json:"reasoning"`
	Total           int64           `json:"total"`
}

// UsageFacts is the complete fact set used by a quote. RequestedAt is the
// original call start, never the ingestion time; ServiceTier is the final
// upstream-reported tier, not a requested preference.
type UsageFacts struct {
	Tokens      Tokens
	RequestedAt time.Time
	ServiceTier string
	// Conditions maps condition fingerprints to known boolean results. A
	// missing fingerprint leaves a requestBody condition unknown.
	Conditions map[string]bool
}

// Quote is a fully materialized, immutable accounting result. Definition and
// Rates explain the price; diagnostics never participate in later repricing.
type Quote struct {
	CostMicroUSD     int64             `json:"cost_micro_usd"`
	Source           string            `json:"source"`
	Rates            Rates             `json:"rates"`
	Definition       PricingDefinition `json:"definition"`
	Reason           string            `json:"reason,omitempty"`
	Match            string            `json:"match,omitempty"`
	CatalogProvider  string            `json:"catalog_provider,omitempty"`
	CatalogModel     string            `json:"catalog_model,omitempty"`
	CatalogTime      *time.Time        `json:"catalog_time,omitempty"`
	PricingSupported bool              `json:"pricing_supported"`
	SupportIssues    []string          `json:"support_issues,omitempty"`
	Candidates       []Candidate       `json:"candidates,omitempty"`
}

// Snapshot is the selected source's state. Nothing from an unselected source
// is exposed or used to fill gaps.
type Snapshot struct {
	Source         string    `json:"source"`
	Prices         []Price   `json:"prices"`
	LastSync       time.Time `json:"last_sync"`
	Error          string    `json:"error,omitempty"`
	Refreshing     bool      `json:"refreshing"`
	CatalogEntries int       `json:"catalog_entries"`
}

type ModelIdentity struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type Pricer interface {
	Quote(string, string, Tokens, time.Time, string) (Quote, error)
	QuoteWithFacts(string, string, UsageFacts) (Quote, error)
	SnapshotConditions([]byte) map[string]bool
	Observe(string, string)
	Configure(Config) error
	Snapshot() Snapshot
	Preview([]ModelIdentity) ([]Price, error)
	Refresh(context.Context) error
}

var errInvalid = errors.New("invalid pricing configuration or usage")

func DefaultConfig() Config {
	return Config{Auto: true, Source: SourceV2, Prices: []ManualPrice{}, Mappings: []Mapping{}}
}

func zeroRateSet() RateSet {
	return RateSet{CacheWriteByTTL: map[int64]int64{}}
}

func completeDefinition(d PricingDefinition) PricingDefinition {
	d.Base = cloneRateSet(d.Base)
	if d.Overrides == nil {
		d.Overrides = []Override{}
	}
	if d.Multipliers == nil {
		d.Multipliers = []Multiplier{}
	}
	return d
}

func zeroDefinition() PricingDefinition {
	return PricingDefinition{Base: zeroRateSet(), Overrides: []Override{}, Multipliers: []Multiplier{}}
}

func cloneRateSet(r RateSet) RateSet {
	m := make(map[int64]int64, len(r.CacheWriteByTTL))
	for k, v := range r.CacheWriteByTTL {
		m[k] = v
	}
	r.CacheWriteByTTL = m
	if r.Reasoning != nil {
		v := *r.Reasoning
		r.Reasoning = &v
	}
	return r
}

func cloneRatePatch(p RatePatch) RatePatch {
	for _, ptr := range []**int64{&p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.Reasoning} {
		if *ptr != nil {
			v := **ptr
			*ptr = &v
		}
	}
	if p.CacheWriteByTTL != nil {
		m := make(map[int64]int64, len(p.CacheWriteByTTL))
		for k, v := range p.CacheWriteByTTL {
			m[k] = v
		}
		p.CacheWriteByTTL = m
	}
	return p
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func cloneCondition(c Condition) Condition {
	if c.Context != nil {
		v := *c.Context
		if v.GT != nil {
			n := *v.GT
			v.GT = &n
		}
		if v.GTE != nil {
			n := *v.GTE
			v.GTE = &n
		}
		if v.LT != nil {
			n := *v.LT
			v.LT = &n
		}
		if v.LTE != nil {
			n := *v.LTE
			v.LTE = &n
		}
		c.Context = &v
	}
	c.ServiceTier = cloneStrings(c.ServiceTier)
	if c.Date != nil {
		v := *c.Date
		c.Date = &v
	}
	if c.Time != nil {
		v := *c.Time
		v.Weekdays = cloneInt64sToInt(v.Weekdays)
		c.Time = &v
	}
	if c.RequestBody != nil {
		v := *c.RequestBody
		c.RequestBody = &v
	}
	if c.All != nil {
		v := make([]Condition, len(c.All))
		for i := range c.All {
			v[i] = cloneCondition(c.All[i])
		}
		c.All = v
	}
	if c.Any != nil {
		v := make([]Condition, len(c.Any))
		for i := range c.Any {
			v[i] = cloneCondition(c.Any[i])
		}
		c.Any = v
	}
	if c.Not != nil {
		v := cloneCondition(*c.Not)
		c.Not = &v
	}
	return c
}

func cloneInt64sToInt(in []int) []int {
	if in == nil {
		return nil
	}
	return append([]int{}, in...)
}

func cloneFactorSet(f FactorSet) FactorSet {
	for _, ptr := range []**string{&f.Input, &f.Output, &f.CacheRead, &f.CacheWrite, &f.Reasoning} {
		if *ptr != nil {
			v := **ptr
			*ptr = &v
		}
	}
	return f
}

func CloneDefinition(d PricingDefinition) PricingDefinition {
	out := PricingDefinition{Base: cloneRateSet(d.Base), Overrides: make([]Override, len(d.Overrides)), Multipliers: make([]Multiplier, len(d.Multipliers))}
	for i, o := range d.Overrides {
		o.When = cloneCondition(o.When)
		o.Rates = cloneRatePatch(o.Rates)
		out.Overrides[i] = o
	}
	for i, m := range d.Multipliers {
		m.When = cloneCondition(m.When)
		m.Factors = cloneFactorSet(m.Factors)
		out.Multipliers[i] = m
	}
	return out
}

// NormalizeConfig returns a detached configuration with default source and
// complete definition arrays/maps for the management JSON contract.
func NormalizeConfig(c Config) Config {
	normalizeConfig(&c)
	return cloneConfig(c)
}

func cloneConfig(c Config) Config {
	if c.Source == "" {
		c.Source = SourceV2
	}
	prices := make([]ManualPrice, len(c.Prices))
	for i := range c.Prices {
		prices[i] = c.Prices[i]
		prices[i].Definition = CloneDefinition(c.Prices[i].Definition)
	}
	c.Prices = prices
	c.Mappings = append([]Mapping{}, c.Mappings...)
	return c
}

func ratesFromSet(r RateSet) Rates {
	out := Rates{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}
	if r.CacheWriteByTTL != nil {
		out.CacheWriteByTTL = make(map[int64]int64, len(r.CacheWriteByTTL))
		for k, v := range r.CacheWriteByTTL {
			out.CacheWriteByTTL[k] = v
		}
	}
	if r.Reasoning != nil {
		v := *r.Reasoning
		out.Reasoning = &v
	}
	return out
}

func cloneRates(r Rates) Rates {
	return ratesFromSet(cloneRateSet(RateSet{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, CacheWriteByTTL: r.CacheWriteByTTL, Reasoning: r.Reasoning}))
}

func cloneCandidate(c Candidate) Candidate {
	c.Rates = cloneRates(c.Rates)
	c.Definition = CloneDefinition(c.Definition)
	c.SupportIssues = cloneStrings(c.SupportIssues)
	return c
}

func cloneCandidates(in []Candidate) []Candidate {
	if in == nil {
		return nil
	}
	out := make([]Candidate, len(in))
	for i := range in {
		out[i] = cloneCandidate(in[i])
	}
	return out
}

func clonePrice(p Price) Price {
	p.Rates = cloneRates(p.Rates)
	p.Definition = CloneDefinition(p.Definition)
	p.CatalogTime = cloneTime(p.CatalogTime)
	p.SupportIssues = cloneStrings(p.SupportIssues)
	p.Candidates = cloneCandidates(p.Candidates)
	return p
}

func CloneQuote(q Quote) Quote {
	q.Rates = cloneRates(q.Rates)
	q.Definition = CloneDefinition(q.Definition)
	q.CatalogTime = cloneTime(q.CatalogTime)
	q.SupportIssues = cloneStrings(q.SupportIssues)
	q.Candidates = cloneCandidates(q.Candidates)
	return q
}

func validIdentity(s string) bool { return len(s) > 0 && len(s) <= 1024 }
