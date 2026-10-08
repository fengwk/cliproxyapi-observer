package observer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const MaxPriceRules = 1000
const MaxTokenThreshold = int64(9007199254740991)

// PriceRule is an ordered, first-match rule. Conditions combine with AND.
// Input length includes cached input; the daily time range is always UTC.
type PriceRule struct {
	Model         string `json:"model" yaml:"model"`
	InputTokensGT *int64 `json:"input_tokens_gt,omitempty" yaml:"input-tokens-gt,omitempty"`
	TimeRange     string `json:"time_range,omitempty" yaml:"time-range,omitempty"`
	Price         Price  `json:"price" yaml:"price"`
}

func (r *PriceRule) UnmarshalJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("price rule must be an object")
	}
	var value PriceRule
	seen := make(map[string]bool)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return fmt.Errorf("invalid price rule")
		}
		field, ok := token.(string)
		if !ok {
			return fmt.Errorf("invalid price rule field")
		}
		field = strings.ReplaceAll(field, "_", "-")
		if seen[field] {
			return fmt.Errorf("duplicate price rule field")
		}
		seen[field] = true
		var data json.RawMessage
		if err := dec.Decode(&data); err != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return fmt.Errorf("invalid price rule value")
		}
		switch field {
		case "model":
			err = json.Unmarshal(data, &value.Model)
		case "input-tokens-gt":
			var n int64
			err = json.Unmarshal(data, &n)
			value.InputTokensGT = &n
		case "time-range":
			err = json.Unmarshal(data, &value.TimeRange)
		case "price":
			err = json.Unmarshal(data, &value.Price)
		default:
			return fmt.Errorf("unknown price rule field")
		}
		if err != nil {
			return fmt.Errorf("invalid price rule value")
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') || !seen["model"] || !seen["price"] {
		return fmt.Errorf("price rule requires model and price")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing price rule data")
	}
	*r = value
	return nil
}

func (r *PriceRule) UnmarshalYAML(node *yaml.Node) error {
	var fields map[string]any
	if err := node.Decode(&fields); err != nil {
		return err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return r.UnmarshalJSON(raw)
}

func NormalizePriceRules(raw []PriceRule) ([]PriceRule, error) {
	if len(raw) > MaxPriceRules {
		return nil, fmt.Errorf("too many price rules")
	}
	rules := make([]PriceRule, 0, len(raw))
	for _, rule := range raw {
		rule.Model = strings.TrimSpace(rule.Model)
		if rule.Model == "" {
			return nil, fmt.Errorf("price rule requires a model")
		}
		if err := validatePrice(rule.Model, rule.Price); err != nil {
			return nil, err
		}
		if rule.InputTokensGT != nil {
			if *rule.InputTokensGT < 0 || *rule.InputTokensGT > MaxTokenThreshold {
				return nil, fmt.Errorf("invalid token threshold")
			}
			n := *rule.InputTokensGT
			rule.InputTokensGT = &n
		}
		if rule.TimeRange != "" {
			if _, _, ok := dailyRange(rule.TimeRange); !ok {
				return nil, fmt.Errorf("time range must use UTC HH:mm-HH:mm with distinct bounds")
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func dailyMinute(raw string, end bool) (int, bool) {
	if len(raw) != 5 || raw[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	h, _ := strconv.Atoi(raw[:2])
	m, _ := strconv.Atoi(raw[3:])
	if m > 59 || h > 23 && !(end && h == 24 && m == 0) {
		return 0, false
	}
	return h*60 + m, true
}

func dailyRange(raw string) (int, int, bool) {
	if len(raw) != 11 || raw[5] != '-' {
		return 0, 0, false
	}
	from, okFrom := dailyMinute(raw[:5], false)
	to, okTo := dailyMinute(raw[6:], true)
	return from, to, okFrom && okTo && from != to
}

func inDailyRange(raw string, at time.Time) bool {
	if raw == "" {
		return true
	}
	from, to, ok := dailyRange(raw)
	if !ok {
		return false
	}
	utc := at.UTC()
	minute := utc.Hour()*60 + utc.Minute()
	if from < to {
		return minute >= from && minute < to
	}
	return minute >= from || minute < to
}

func selectRulePrice(model string, input int64, at time.Time, rules []PriceRule, fallback map[string]Price) (Price, bool) {
	for _, rule := range rules {
		if rule.Model != model || !inDailyRange(rule.TimeRange, at) {
			continue
		}
		if rule.InputTokensGT != nil {
			if input < 0 {
				// A legacy aggregate cannot prove which tier applied. Do not
				// silently skip a possible match and misprice it as default.
				return Price{}, false
			}
			if input <= *rule.InputTokensGT {
				continue
			}
		}
		return rule.Price, true
	}
	price, ok := fallback[model]
	return price, ok
}

func (s *Store) requestCost(r Request) *float64 {
	if !completeTokens(r) {
		return nil
	}
	price, ok := selectRulePrice(r.Model, r.InputTokens, r.Time, s.cfg.PriceRules, s.prices)
	if !ok {
		return nil
	}
	return tokenCost(uint64(r.UncachedInputTokens), uint64(r.CacheReadTokens), uint64(r.CacheCreationTokens),
		uint64(r.OutputTokens), r.Model, map[string]Price{r.Model: price})
}

func (s *Store) priceKeyCounters(c *Counters, model string, input int64, minute int64) {
	price, ok := selectRulePrice(model, input, time.Unix(minute, 0), s.cfg.PriceRules, s.prices)
	if !ok {
		priceCounters(c, model, nil)
		return
	}
	priceCounters(c, model, map[string]Price{model: price})
}
