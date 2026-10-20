// Package pricing calculates immutable micro-USD quotes without network I/O.
package pricing

import (
	"context"
	"errors"
	"time"
)

// Rate names are stable diagnostic identifiers. The empty string is never a
// rate name; absent rates are listed instead of being inferred as zero.
const (
	rateInput      = "input"
	rateOutput     = "output"
	rateCacheRead  = "cache_read"
	rateCacheWrite = "cache_write"
)

// Match and reason strings are part of the management JSON contract. They are
// intentionally specific so the UI never has to guess why a price is absent.
const (
	matchManual    = "manual"
	matchMapping   = "mapping"
	matchProvider  = "provider"
	matchNamespace = "namespace"
	matchCanonical = "canonical"
	matchExact     = "exact"
	matchRule      = "rule"
	matchAmbiguous = "ambiguous"
	matchNone      = "none"

	reasonManual             = "manual"
	reasonRule               = "rule"
	reasonAutoDisabled       = "auto_disabled"
	reasonCatalogUnavailable = "catalog_unavailable"
	reasonNotFound           = "not_found"
	reasonAmbiguous          = "ambiguous"
	reasonProviderConflict   = "provider_conflict"
	reasonMappingMissing     = "mapping_missing"
	reasonTiersUnsupported   = "tiers_unsupported"
	reasonContextTiers       = "context_tiers"
	reasonMissingRates       = "missing_rates"
	reasonVariantOnly        = "variant_only"
)

const maxCandidates = 32

type Rates struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}

// Candidate is one catalog identity that could explain an ambiguous match.
// It is diagnostic only: it never becomes the billed price without an explicit
// mapping (or a provider-qualified request).
type Candidate struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Rates        Rates    `json:"rates"`
	MissingRates []string `json:"missing_rates,omitempty"`
}

type Price struct {
	Model           string      `json:"model"`
	Provider        string      `json:"provider,omitempty"` // requested/observed channel; manual overrides are model-wide
	Rates           Rates       `json:"rates"`
	Source          string      `json:"source"`
	Reason          string      `json:"reason,omitempty"`
	Match           string      `json:"match,omitempty"`
	CatalogProvider string      `json:"catalog_provider,omitempty"`
	CatalogModel    string      `json:"catalog_model,omitempty"`
	CatalogTime     *time.Time  `json:"catalog_time,omitempty"`
	MissingRates    []string    `json:"missing_rates,omitempty"`
	Candidates      []Candidate `json:"candidates,omitempty"`
}

type Rule struct {
	ID            string `json:"id"`
	Model         string `json:"model"`
	InputTokensGT *int64 `json:"input_tokens_gt,omitempty"`
	TimeRange     string `json:"time_range,omitempty"`
	Rates         Rates  `json:"rates"`
}

// Mapping chooses an exact catalog identity for a complete public model ID.
// It affects accounting identity only and never changes request routing.
type Mapping struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	CatalogModel string `json:"catalog_model"`
}

type Config struct {
	Auto     bool      `json:"auto"`
	Prices   []Price   `json:"prices"`
	Rules    []Rule    `json:"rules"`
	Mappings []Mapping `json:"mappings,omitempty"`
}

type Tokens struct {
	// Input is uncached and disjoint from CacheRead and CacheWrite.
	Input int64 `json:"input"`
	// Output includes Reasoning.
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

// Quote is a fully materialized, immutable accounting result. Diagnostic
// fields describe the resolution but never participate in a later repricing.
type Quote struct {
	CostMicroUSD    int64       `json:"cost_micro_usd"`
	Source          string      `json:"source"`
	RuleID          string      `json:"rule_id,omitempty"`
	Rates           Rates       `json:"rates"`
	Reason          string      `json:"reason,omitempty"`
	Match           string      `json:"match,omitempty"`
	CatalogProvider string      `json:"catalog_provider,omitempty"`
	CatalogModel    string      `json:"catalog_model,omitempty"`
	CatalogTime     *time.Time  `json:"catalog_time,omitempty"`
	MissingRates    []string    `json:"missing_rates,omitempty"`
	Candidates      []Candidate `json:"candidates,omitempty"`
}

type Snapshot struct {
	Prices         []Price   `json:"prices"`
	LastSync       time.Time `json:"last_sync"`
	Error          string    `json:"error,omitempty"`
	Refreshing     bool      `json:"refreshing"`
	CatalogEntries int       `json:"catalog_entries"`
}

// Preview gives base rates, not a quote for invented token usage.
type ModelIdentity struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
}
type PreviewPrice struct {
	Price
	HasRules bool `json:"has_rules"`
}
type Pricer interface {
	Quote(string, string, Tokens, time.Time) (Quote, error)
	Observe(string, string)
	Configure(Config) error
	Snapshot() Snapshot
	Refresh(context.Context) error
}

const maxSafe int64 = 9_000_000_000_000_000

var errInvalid = errors.New("invalid pricing configuration or tokens")

func DefaultConfig() Config {
	return Config{Auto: true, Prices: []Price{}, Rules: []Rule{}}
}
func cloneRates(r Rates) Rates {
	if r.Reasoning != nil {
		v := *r.Reasoning
		r.Reasoning = &v
	}
	return r
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
func cloneCandidates(in []Candidate) []Candidate {
	if in == nil {
		return nil
	}
	out := make([]Candidate, len(in))
	for i, c := range in {
		out[i] = c
		out[i].Rates = cloneRates(c.Rates)
		out[i].MissingRates = cloneStrings(c.MissingRates)
	}
	return out
}
func clonePrice(p Price) Price {
	p.Rates = cloneRates(p.Rates)
	p.CatalogTime = cloneTime(p.CatalogTime)
	p.MissingRates = cloneStrings(p.MissingRates)
	p.Candidates = cloneCandidates(p.Candidates)
	return p
}
func cloneConfig(c Config) Config {
	c.Prices = append([]Price{}, c.Prices...)
	for i := range c.Prices {
		c.Prices[i] = clonePrice(c.Prices[i])
		// Configuration entries are model-wide overrides; diagnostics are
		// generated by the resolver and must never be persisted as user input.
		c.Prices[i].Provider = ""
		c.Prices[i].Reason = ""
		c.Prices[i].Match = ""
		c.Prices[i].CatalogProvider = ""
		c.Prices[i].CatalogModel = ""
		c.Prices[i].CatalogTime = nil
		c.Prices[i].MissingRates = nil
		c.Prices[i].Candidates = nil
	}
	c.Rules = append([]Rule{}, c.Rules...)
	for i := range c.Rules {
		c.Rules[i].Rates = cloneRates(c.Rules[i].Rates)
		if c.Rules[i].InputTokensGT != nil {
			v := *c.Rules[i].InputTokensGT
			c.Rules[i].InputTokensGT = &v
		}
	}
	if c.Mappings != nil {
		c.Mappings = append([]Mapping{}, c.Mappings...)
	}
	return c
}
func validRates(r Rates) bool {
	for _, v := range []int64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if v < 0 || v > maxSafe {
			return false
		}
	}
	return r.Reasoning == nil || (*r.Reasoning >= 0 && *r.Reasoning <= maxSafe)
}
func Validate(c Config) error {
	if len(c.Prices) > 4096 || len(c.Rules) > 4096 || len(c.Mappings) > 4096 {
		return errInvalid
	}
	models, ids, mapped := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range c.Prices {
		if !validIdentity(p.Model) || models[p.Model] || !validRates(p.Rates) || p.Provider != "" || (p.Source != "" && p.Source != "manual") {
			return errInvalid
		}
		models[p.Model] = true
	}
	for _, r := range c.Rules {
		if !validIdentity(r.Model) || !validIdentity(r.ID) || ids[r.ID] || !validRates(r.Rates) {
			return errInvalid
		}
		ids[r.ID] = true
		if r.InputTokensGT != nil && (*r.InputTokensGT < 0 || *r.InputTokensGT > maxSafe) {
			return errInvalid
		}
		if r.TimeRange != "" {
			if _, _, ok := parseRange(r.TimeRange); !ok {
				return errInvalid
			}
		}
	}
	for _, m := range c.Mappings {
		if !validIdentity(m.Model) || !validIdentity(m.Provider) || !validIdentity(m.CatalogModel) || mapped[m.Model] {
			return errInvalid
		}
		mapped[m.Model] = true
	}
	return nil
}
func validIdentity(s string) bool { return len(s) > 0 && len(s) <= 1024 }
func parseRange(s string) (int, int, bool) {
	if len(s) != 11 || s[2] != ':' || s[5] != '-' || s[8] != ':' {
		return 0, 0, false
	}
	for _, i := range []int{0, 1, 3, 4, 6, 7, 9, 10} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, false
		}
	}
	h1, m1, h2, m2 := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0'), int(s[6]-'0')*10+int(s[7]-'0'), int(s[9]-'0')*10+int(s[10]-'0')
	return h1*60 + m1, h2*60 + m2, h1 < 24 && h2 < 24 && m1 < 60 && m2 < 60 && h1*60+m1 != h2*60+m2
}
