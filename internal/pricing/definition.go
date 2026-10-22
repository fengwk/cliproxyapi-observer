package pricing

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var decimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)

// UnmarshalJSON preserves the difference between an omitted rate and an
// explicit zero/null, which is required for patch inheritance.
func (p *RatePatch) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errInvalid
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return errInvalid
	}
	*p = RatePatch{}
	for key, value := range fields {
		switch key {
		case "input", "output", "cache_read", "cache_write":
			n, err := decodeRate(value)
			if err != nil {
				return err
			}
			switch key {
			case "input":
				p.Input = &n
			case "output":
				p.Output = &n
			case "cache_read":
				p.CacheRead = &n
			case "cache_write":
				p.CacheWrite = &n
			}
		case "cache_write_by_ttl":
			ttl, err := decodeTTLRates(value)
			if err != nil {
				return err
			}
			p.CacheWriteByTTL = ttl
			p.HasCacheWriteByTTL = true
		case "reasoning":
			p.ReasoningSet = true
			if jsonNull(value) {
				continue
			}
			n, err := decodeRate(value)
			if err != nil {
				return err
			}
			p.Reasoning = &n
		default:
			return errInvalid
		}
	}
	return nil
}

func (p RatePatch) MarshalJSON() ([]byte, error) {
	fields := map[string]any{}
	if p.Input != nil {
		fields["input"] = *p.Input
	}
	if p.Output != nil {
		fields["output"] = *p.Output
	}
	if p.CacheRead != nil {
		fields["cache_read"] = *p.CacheRead
	}
	if p.CacheWrite != nil {
		fields["cache_write"] = *p.CacheWrite
	}
	if p.HasCacheWriteByTTL {
		m := p.CacheWriteByTTL
		if m == nil {
			m = map[int64]int64{}
		}
		fields["cache_write_by_ttl"] = m
	}
	if p.ReasoningSet {
		if p.Reasoning == nil {
			fields["reasoning"] = nil
		} else {
			fields["reasoning"] = *p.Reasoning
		}
	}
	return json.Marshal(fields)
}

func jsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeRate(raw []byte) (int64, error) {
	if jsonNull(raw) {
		return 0, errInvalid
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, errInvalid
	}
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return 0, errInvalid
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > maxSafe {
		return 0, errInvalid
	}
	return v, nil
}

func decodeTTLRates(raw []byte) (map[int64]int64, error) {
	if jsonNull(raw) {
		return nil, errInvalid
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errInvalid
	}
	if len(fields) > maxTiers {
		return nil, errInvalid
	}
	out := make(map[int64]int64, len(fields))
	for key, value := range fields {
		seconds, err := strconv.ParseInt(key, 10, 64)
		if err != nil || seconds <= 0 {
			return nil, errInvalid
		}
		v, err := decodeRate(value)
		if err != nil {
			return nil, err
		}
		out[seconds] = v
	}
	return out, nil
}

func validRateSet(r RateSet) bool {
	for _, v := range []int64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if v < 0 || v > maxSafe {
			return false
		}
	}
	if r.Reasoning != nil && (*r.Reasoning < 0 || *r.Reasoning > maxSafe) {
		return false
	}
	if len(r.CacheWriteByTTL) > maxTiers {
		return false
	}
	for ttl, v := range r.CacheWriteByTTL {
		if ttl <= 0 || ttl > maxSafe || v < 0 || v > maxSafe {
			return false
		}
	}
	return true
}

func validFactorValue(s string) bool {
	if len(s) == 0 || len(s) > 64 || !decimalPattern.MatchString(s) {
		return false
	}
	r, ok := new(big.Rat).SetString(s)
	return ok && r.Sign() >= 0 && r.Cmp(big.NewRat(1_000_000, 1)) <= 0
}

func validFactorSet(f FactorSet) bool {
	for _, ptr := range []*string{f.Input, f.Output, f.CacheRead, f.CacheWrite, f.Reasoning} {
		if ptr != nil && !validFactorValue(*ptr) {
			return false
		}
	}
	return true
}

// Validate is the single public configuration gate. Invalid definitions are
// errors, never silently converted to zero.
func Validate(c Config) error {
	if c.Source == "" {
		c.Source = SourceV2
	}
	if c.Source != SourceV1 && c.Source != SourceV2 {
		return errInvalid
	}
	if len(c.Prices) > maxDefinitions || len(c.Mappings) > maxMappings {
		return errInvalid
	}
	seen := map[string]bool{}
	for _, p := range c.Prices {
		if !validIdentity(p.Model) || (p.Provider != "" && !validIdentity(p.Provider)) {
			return errInvalid
		}
		key := p.Model + "\x00" + p.Provider
		if seen[key] {
			return errInvalid
		}
		seen[key] = true
		if !validDefinition(p.Definition) {
			return errInvalid
		}
	}
	mapped := map[string]bool{}
	for _, m := range c.Mappings {
		if !validIdentity(m.Model) || !validIdentity(m.Provider) || !validIdentity(m.CatalogModel) || mapped[m.Model] {
			return errInvalid
		}
		mapped[m.Model] = true
	}
	return nil
}

func validDefinition(d PricingDefinition) bool {
	if !validRateSet(d.Base) || len(d.Overrides) > maxRulesPerModel || len(d.Multipliers) > maxRulesPerModel {
		return false
	}
	ids := map[string]bool{}
	for _, o := range d.Overrides {
		if !validIdentity(o.ID) || ids[o.ID] || !validCondition(o.When, 0, new(int)) || !validPatch(o.Rates) {
			return false
		}
		ids[o.ID] = true
	}
	ids = map[string]bool{}
	for _, m := range d.Multipliers {
		if !validIdentity(m.ID) || ids[m.ID] || !validCondition(m.When, 0, new(int)) || !validFactorSet(m.Factors) {
			return false
		}
		ids[m.ID] = true
	}
	return true
}

func validPatch(p RatePatch) bool {
	for _, ptr := range []*int64{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.Reasoning} {
		if ptr != nil && (*ptr < 0 || *ptr > maxSafe) {
			return false
		}
	}
	if !p.HasCacheWriteByTTL {
		return true
	}
	if len(p.CacheWriteByTTL) > maxTiers {
		return false
	}
	for ttl, v := range p.CacheWriteByTTL {
		if ttl <= 0 || ttl > maxSafe || v < 0 || v > maxSafe {
			return false
		}
	}
	return true
}

func validCondition(c Condition, depth int, nodes *int) bool {
	(*nodes)++
	if depth > maxConditionDepth || *nodes > maxConditionNodes {
		return false
	}
	if c.Context != nil {
		v := c.Context
		if v.GT != nil && v.GTE != nil || v.LT != nil && v.LTE != nil {
			return false
		}
		for _, ptr := range []*int64{v.GT, v.GTE, v.LT, v.LTE} {
			if ptr != nil && (*ptr < 0 || *ptr > maxSafe) {
				return false
			}
		}
	}
	if c.ServiceTier != nil {
		if len(c.ServiceTier) > maxConditionList {
			return false
		}
		seen := map[string]bool{}
		for _, tier := range c.ServiceTier {
			if !validIdentity(tier) || len(tier) > 128 || seen[tier] {
				return false
			}
			seen[tier] = true
		}
	}
	if c.Date != nil {
		if c.Date.GTE != "" {
			if _, err := time.Parse("2006-01-02", c.Date.GTE); err != nil {
				return false
			}
		}
		if c.Date.LT != "" {
			if _, err := time.Parse("2006-01-02", c.Date.LT); err != nil {
				return false
			}
		}
		if c.Date.GTE != "" && c.Date.LT != "" && c.Date.GTE >= c.Date.LT {
			return false
		}
	}
	if c.Time != nil {
		if !validTimeCondition(*c.Time) {
			return false
		}
	}
	if c.All != nil {
		if len(c.All) > maxConditionList {
			return false
		}
		for _, child := range c.All {
			if !validCondition(child, depth+1, nodes) {
				return false
			}
		}
	}
	if c.Any != nil {
		if len(c.Any) > maxConditionList {
			return false
		}
		for _, child := range c.Any {
			if !validCondition(child, depth+1, nodes) {
				return false
			}
		}
	}
	if c.Not != nil {
		if !validCondition(*c.Not, depth+1, nodes) {
			return false
		}
	}
	return true
}

func validTimeCondition(t TimeCondition) bool {
	if _, err := time.LoadLocation(t.Timezone); err != nil {
		return false
	}
	start, ok := parseClock(t.Start, false)
	if !ok {
		return false
	}
	end, ok := parseClock(t.End, true)
	if !ok {
		return false
	}
	if start == 0 && end == 24*60 {
		// Explicit all-day range.
	} else if start == end {
		return false
	}
	if len(t.Weekdays) > 7 {
		return false
	}
	seen := map[int]bool{}
	for _, day := range t.Weekdays {
		if day < 1 || day > 7 || seen[day] {
			return false
		}
		seen[day] = true
	}
	return true
}

func parseClock(s string, allowEnd bool) (int, bool) {
	if s == "24:00" && allowEnd {
		return 24 * 60, true
	}
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}
