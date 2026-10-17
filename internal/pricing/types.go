// Package pricing calculates immutable micro-USD quotes without network I/O.
package pricing

import (
	"context"
	"errors"
	"time"
)

type Rates struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
}
type Price struct {
	Model  string `json:"model"`
	Rates  Rates  `json:"rates"`
	Source string `json:"source"`
}
type Rule struct {
	ID            string `json:"id"`
	Model         string `json:"model"`
	InputTokensGT *int64 `json:"input_tokens_gt,omitempty"`
	TimeRange     string `json:"time_range,omitempty"`
	Rates         Rates  `json:"rates"`
}
type Config struct {
	Auto   bool    `json:"auto"`
	Prices []Price `json:"prices"`
	Rules  []Rule  `json:"rules"`
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
type Quote struct {
	CostMicroUSD int64  `json:"cost_micro_usd"`
	Source       string `json:"source"`
	RuleID       string `json:"rule_id,omitempty"`
	Rates        Rates  `json:"rates"`
}
type Snapshot struct {
	Prices     []Price   `json:"prices"`
	LastSync   time.Time `json:"last_sync"`
	Error      string    `json:"error,omitempty"`
	Refreshing bool      `json:"refreshing"`
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

func DefaultConfig() Config { return Config{Auto: true, Prices: []Price{}, Rules: []Rule{}} }
func cloneRates(r Rates) Rates {
	if r.Reasoning != nil {
		v := *r.Reasoning
		r.Reasoning = &v
	}
	return r
}
func cloneConfig(c Config) Config {
	c.Prices = append([]Price{}, c.Prices...)
	c.Rules = append([]Rule{}, c.Rules...)
	for i := range c.Prices {
		c.Prices[i].Rates = cloneRates(c.Prices[i].Rates)
	}
	for i := range c.Rules {
		c.Rules[i].Rates = cloneRates(c.Rules[i].Rates)
		if c.Rules[i].InputTokensGT != nil {
			v := *c.Rules[i].InputTokensGT
			c.Rules[i].InputTokensGT = &v
		}
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
	if len(c.Prices) > 4096 || len(c.Rules) > 4096 {
		return errInvalid
	}
	models, ids := map[string]bool{}, map[string]bool{}
	for _, p := range c.Prices {
		if !validIdentity(p.Model) || models[p.Model] || !validRates(p.Rates) {
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
