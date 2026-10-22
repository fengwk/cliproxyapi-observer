package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxCatalogBytes = 32 << 20
const maxCatalogEntries = 100000
const diskCacheVersion = 1

const (
	catalogURLV1 = "https://models.dev/api.json"
	catalogURLV2 = "https://models.dev/experimental/v2.0/api.json"
)

type identity struct {
	Model    string
	Provider string
}

type entry struct {
	Model          string            `json:"model"`
	Provider       string            `json:"provider"`
	Definition     PricingDefinition `json:"definition"`
	Supported      bool              `json:"supported"`
	SupportIssues  []string          `json:"support_issues,omitempty"`
	CanonicalModel string            `json:"canonical_model_id,omitempty"`
}

type diskCache struct {
	Version  int       `json:"version"`
	Source   string    `json:"source"`
	LastSync time.Time `json:"last_sync"`
	Entries  []entry   `json:"entries"`
}

func cloneEntry(e entry) entry {
	e.Definition = CloneDefinition(e.Definition)
	e.SupportIssues = cloneStrings(e.SupportIssues)
	return e
}

func cloneEntries(in []entry) []entry {
	out := make([]entry, len(in))
	for i := range in {
		out[i] = cloneEntry(in[i])
	}
	return out
}

// Catalog prices are decimal USD/million. Convert exactly, rounding fractional
// micro-USD upward rather than involving binary floating point in billing.
func catalogRate(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if len(n) > 64 {
		return 0, errInvalid
	}
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		e, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || e < -30 || e > 18 {
			return 0, errInvalid
		}
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 {
		return 0, errInvalid
	}
	return microUSDRate(r)
}

func microUSDRate(r *big.Rat) (int64, error) {
	r.Mul(r, big.NewRat(1_000_000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxSafe {
		return 0, errInvalid
	}
	return q.Int64(), nil
}

func strictKeys(raw json.RawMessage, allowed map[string]bool) (map[string]json.RawMessage, bool) {
	if jsonNull(raw) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	for key := range fields {
		if !allowed[key] {
			return nil, false
		}
	}
	return fields, true
}

func objectFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

func numberField(fields map[string]json.RawMessage, key string) (int64, bool, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false, true
	}
	if jsonNull(raw) {
		return 0, false, false
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, false, false
	}
	v, err := catalogRate(n)
	if err != nil {
		return 0, false, false
	}
	return v, true, true
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false, false
	}
	return s, true, true
}

// ---------------------------------------------------------------------------
// V1 adapter

var v1CostKeys = map[string]bool{
	"input": true, "output": true, "cache_read": true, "cache_write": true,
	"reasoning": true, "tiers": true, "context_over_200k": true,
}
var v1TierKeys = map[string]bool{
	"input": true, "output": true, "cache_read": true, "cache_write": true,
	"reasoning": true, "tier": true,
}
var v1TierMetaKeys = map[string]bool{"type": true, "size": true}

func parseV1Definition(raw json.RawMessage) (PricingDefinition, []string, bool) {
	def := zeroDefinition()
	if len(raw) == 0 || jsonNull(raw) {
		return def, nil, true
	}
	fields, ok := strictKeys(raw, v1CostKeys)
	if !ok {
		return def, []string{"invalid_or_unknown_cost_field"}, false
	}
	base, ok := parseV1Rates(fields)
	if !ok {
		return def, []string{"invalid_price"}, false
	}
	def.Base = base
	tiers, tierIssues, ok := parseV1Tiers(fields)
	if !ok {
		return zeroDefinition(), tierIssues, false
	}
	def.Overrides = tiers
	return def, tierIssues, true
}

func parseV1Rates(fields map[string]json.RawMessage) (RateSet, bool) {
	r := zeroRateSet()
	dst := map[string]*int64{"input": &r.Input, "output": &r.Output, "cache_read": &r.CacheRead, "cache_write": &r.CacheWrite}
	for key, target := range dst {
		v, present, ok := numberField(fields, key)
		if !ok {
			return RateSet{}, false
		}
		if present {
			*target = v
		}
	}
	if raw, ok := fields["reasoning"]; ok {
		v, _, valid := numberField(map[string]json.RawMessage{"reasoning": raw}, "reasoning")
		if !valid {
			return RateSet{}, false
		}
		r.Reasoning = &v
	}
	return r, true
}

func parseV1Tiers(fields map[string]json.RawMessage) ([]Override, []string, bool) {
	type tierValue struct {
		size      int64
		rates     RateSet
		hasReason bool
	}
	var tiers []tierValue
	issues := []string{}
	if raw, ok := fields["tiers"]; ok && !jsonNull(raw) {
		var elements []json.RawMessage
		if json.Unmarshal(raw, &elements) != nil || len(elements) > maxTiers {
			return nil, []string{"invalid_tiers"}, false
		}
		for _, element := range elements {
			elementFields, ok := strictKeys(element, v1TierKeys)
			if !ok {
				return nil, []string{"unsupported_tier"}, false
			}
			meta, ok := strictKeys(elementFields["tier"], v1TierMetaKeys)
			if !ok {
				return nil, []string{"unsupported_tier"}, false
			}
			kind, _, valid := stringField(meta, "type")
			if !valid || kind != "context" {
				return nil, []string{"unsupported_tier_type"}, false
			}
			var size json.Number
			if json.Unmarshal(meta["size"], &size) != nil {
				return nil, []string{"invalid_tier_size"}, false
			}
			n, err := strconv.ParseInt(string(size), 10, 64)
			if err != nil || n <= 0 || n > maxSafe {
				return nil, []string{"invalid_tier_size"}, false
			}
			rates, ok := parseV1Rates(elementFields)
			if !ok {
				return nil, []string{"invalid_tier_price"}, false
			}
			_, hasReason := elementFields["reasoning"]
			tiers = append(tiers, tierValue{size: n, rates: rates, hasReason: hasReason})
		}
	}
	if raw, ok := fields["context_over_200k"]; ok && !jsonNull(raw) {
		legacyFields, ok := strictKeys(raw, map[string]bool{
			"input": true, "output": true, "cache_read": true, "cache_write": true, "reasoning": true,
		})
		if !ok {
			return nil, []string{"invalid_legacy_tier"}, false
		}
		rates, ok := parseV1Rates(legacyFields)
		if !ok {
			return nil, []string{"invalid_legacy_tier"}, false
		}
		_, hasReason := legacyFields["reasoning"]
		legacy := tierValue{size: 200000, rates: rates, hasReason: hasReason}
		duplicate := false
		for _, tier := range tiers {
			if tier.size == legacy.size {
				if !rateSetsEqual(tier.rates, legacy.rates) {
					return nil, []string{"conflicting_context_tier"}, false
				}
				duplicate = true
				break
			}
		}
		if !duplicate {
			tiers = append(tiers, legacy)
		}
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].size < tiers[j].size })
	for i := 1; i < len(tiers); i++ {
		if tiers[i].size == tiers[i-1].size {
			return nil, []string{"duplicate_context_tier"}, false
		}
	}
	out := make([]Override, 0, len(tiers))
	for i, tier := range tiers {
		patch := RatePatch{
			Input: int64Ptr(tier.rates.Input), Output: int64Ptr(tier.rates.Output),
			CacheRead: int64Ptr(tier.rates.CacheRead), CacheWrite: int64Ptr(tier.rates.CacheWrite),
			ReasoningSet: true,
		}
		if tier.hasReason && tier.rates.Reasoning != nil {
			v := *tier.rates.Reasoning
			patch.Reasoning = &v
		}
		out = append(out, Override{
			ID:    "tier-" + strconv.Itoa(i),
			When:  Condition{Context: &ContextCondition{GT: int64Ptr(tier.size)}},
			Rates: patch,
		})
	}
	return out, issues, true
}

func int64Ptr(v int64) *int64 { return &v }

func rateSetsEqual(a, b RateSet) bool {
	return a.Input == b.Input && a.Output == b.Output && a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite &&
		((a.Reasoning == nil && b.Reasoning == nil) || (a.Reasoning != nil && b.Reasoning != nil && *a.Reasoning == *b.Reasoning)) &&
		reflectMapEqual(a.CacheWriteByTTL, b.CacheWriteByTTL)
}

func reflectMapEqual(a, b map[int64]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func parseV1Catalog(body []byte) ([]entry, error) {
	var root map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(body, &root) != nil || len(root) == 0 {
		return nil, errInvalid
	}
	out := []entry{}
	for provider, p := range root {
		if !validIdentity(provider) {
			return nil, errInvalid
		}
		for model, raw := range p.Models {
			if !validIdentity(model) {
				return nil, errInvalid
			}
			var fields struct {
				Cost           json.RawMessage `json:"cost"`
				CanonicalModel string          `json:"canonical_model_id"`
			}
			if json.Unmarshal(raw, &fields) != nil {
				return nil, errInvalid
			}
			if fields.CanonicalModel != "" && !validIdentity(fields.CanonicalModel) {
				return nil, errInvalid
			}
			def, issues, supported := parseV1Definition(fields.Cost)
			out = append(out, entry{
				Model: model, Provider: provider, Definition: def, Supported: supported,
				SupportIssues: issues, CanonicalModel: fields.CanonicalModel,
			})
		}
	}
	if len(out) == 0 || len(out) > maxCatalogEntries {
		return nil, errInvalid
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// V2 adapter

var (
	v2CostKeys       = map[string]bool{"text": true, "audio": true, "image": true, "video": true, "multipliers": true}
	v2TextKeys       = map[string]bool{"input": true, "output": true}
	v2InputKeys      = map[string]bool{"price": true, "cache": true, "overrides": true, "unit": true}
	v2OutputKeys     = map[string]bool{"price": true, "reasoning": true, "overrides": true}
	v2CacheKeys      = map[string]bool{"read": true, "write": true, "explicit": true, "storage": true}
	v2InputOverKeys  = map[string]bool{"when": true, "price": true, "cache": true}
	v2OutputOverKeys = map[string]bool{"when": true, "price": true, "reasoning": true}
	v2MultiplierKeys = map[string]bool{"when": true, "factor": true}
)

func parseV2Definition(raw json.RawMessage) (PricingDefinition, []string, bool) {
	def := zeroDefinition()
	if len(raw) == 0 || jsonNull(raw) {
		return def, nil, true
	}
	fields, ok := strictKeys(raw, v2CostKeys)
	if !ok {
		return def, []string{"unsupported_cost_field"}, false
	}
	issues := []string{}
	for _, key := range []string{"audio", "image", "video"} {
		if value, present := fields[key]; present && !jsonNull(value) {
			issues = append(issues, "non_text_cost_ignored")
		}
	}
	textRaw, hasText := fields["text"]
	if !hasText || jsonNull(textRaw) {
		return def, issues, true
	}
	textFields, ok := strictKeys(textRaw, v2TextKeys)
	if !ok {
		return zeroDefinition(), append(issues, "unsupported_text_cost"), false
	}
	base := zeroRateSet()
	if inputRaw, ok := textFields["input"]; ok && !jsonNull(inputRaw) {
		rates, inputIssues, supported := parseV2Input(inputRaw)
		if !supported {
			return zeroDefinition(), append(issues, inputIssues...), false
		}
		base = rates
		issues = append(issues, inputIssues...)
	}
	var overrides []Override
	if inputRaw, ok := textFields["input"]; ok && !jsonNull(inputRaw) {
		ov, inputIssues, supported := parseV2InputOverrides(inputRaw)
		if !supported {
			return zeroDefinition(), append(issues, inputIssues...), false
		}
		overrides = append(overrides, ov...)
		issues = append(issues, inputIssues...)
	}
	if outputRaw, ok := textFields["output"]; ok && !jsonNull(outputRaw) {
		var outputIssues []string
		var supported bool
		base, outputIssues, supported = parseV2Output(outputRaw, &base)
		if !supported {
			return zeroDefinition(), append(issues, outputIssues...), false
		}
		issues = append(issues, outputIssues...)
		ov, outputIssues, supported := parseV2OutputOverrides(outputRaw)
		if !supported {
			return zeroDefinition(), append(issues, outputIssues...), false
		}
		overrides = append(overrides, ov...)
		issues = append(issues, outputIssues...)
	}
	def.Base = base
	def.Overrides = overrides
	if rawMultipliers, ok := fields["multipliers"]; ok && !jsonNull(rawMultipliers) {
		multipliers, multiplierIssues, supported := parseV2Multipliers(rawMultipliers)
		if !supported {
			return zeroDefinition(), append(issues, multiplierIssues...), false
		}
		def.Multipliers = multipliers
		issues = append(issues, multiplierIssues...)
	}
	issues = uniqueStrings(issues)
	return def, issues, true
}

func parseV2Input(raw json.RawMessage) (RateSet, []string, bool) {
	fields, ok := strictKeys(raw, v2InputKeys)
	if !ok {
		return RateSet{}, []string{"unsupported_input_price"}, false
	}
	if unit, present, valid := stringField(fields, "unit"); !valid {
		return RateSet{}, []string{"invalid_price_unit"}, false
	} else if present && unit != "" && unit != "per_million" && unit != "million_tokens" {
		return RateSet{}, []string{"unsupported_price_unit"}, false
	}
	set := zeroRateSet()
	price, _, valid := numberField(fields, "price")
	if !valid {
		return RateSet{}, []string{"invalid_input_price"}, false
	}
	set.Input = price
	issues := []string{}
	if cacheRaw, ok := fields["cache"]; ok && !jsonNull(cacheRaw) {
		cacheFields, ok := strictKeys(cacheRaw, v2CacheKeys)
		if !ok {
			return RateSet{}, []string{"unsupported_cache_price"}, false
		}
		read, _, valid := numberField(cacheFields, "read")
		if !valid {
			return RateSet{}, []string{"invalid_cache_read_price"}, false
		}
		set.CacheRead = read
		if writeRaw, ok := cacheFields["write"]; ok && !jsonNull(writeRaw) {
			write, ttl, message, supported := parseCacheWrite(writeRaw)
			if !supported {
				return RateSet{}, []string{message}, false
			}
			set.CacheWrite = write
			set.CacheWriteByTTL = ttl
		}
		if _, present := cacheFields["storage"]; present {
			issues = append(issues, "cache_storage_cost_not_supported")
		}
		if _, present := cacheFields["explicit"]; present {
			issues = append(issues, "explicit_cache_pricing_not_supported")
		}
	}
	return set, issues, true
}

func parseCacheWrite(raw json.RawMessage) (int64, map[int64]int64, string, bool) {
	if len(raw) == 0 || jsonNull(raw) {
		return 0, map[int64]int64{}, "", true
	}
	// A scalar write price applies to every TTL.
	if raw[0] != '{' {
		var n json.Number
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if dec.Decode(&n) != nil {
			return 0, nil, "invalid_cache_write_price", false
		}
		v, err := catalogRate(n)
		if err != nil {
			return 0, nil, "invalid_cache_write_price", false
		}
		return v, map[int64]int64{}, "", true
	}
	fields, ok := objectFields(raw)
	if !ok || len(fields) > maxTiers {
		return 0, nil, "invalid_cache_write_ttl", false
	}
	ttl := make(map[int64]int64, len(fields))
	for key, rawPrice := range fields {
		seconds, ok := normalizeTTL(key)
		if !ok {
			return 0, nil, "invalid_cache_write_ttl", false
		}
		if _, duplicate := ttl[seconds]; duplicate {
			return 0, nil, "duplicate_cache_write_ttl", false
		}
		n, err := jsonNumber(rawPrice)
		if err != nil {
			return 0, nil, "invalid_cache_write_price", false
		}
		v, err := catalogRate(n)
		if err != nil {
			return 0, nil, "invalid_cache_write_price", false
		}
		ttl[seconds] = v
	}
	return 0, ttl, "", true
}

func jsonNumber(raw json.RawMessage) (json.Number, error) {
	if jsonNull(raw) {
		return "", errInvalid
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return "", err
	}
	return n, nil
}

func normalizeTTL(s string) (int64, bool) {
	if s == "" || len(s) > 16 {
		return 0, false
	}
	var number int64
	var unit byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= '0' && ch <= '9' && unit == 0 {
			if number > (maxSafe-int64(ch-'0'))/10 {
				return 0, false
			}
			number = number*10 + int64(ch-'0')
			continue
		}
		if i != len(s)-1 || unit != 0 {
			return 0, false
		}
		unit = ch
	}
	if number <= 0 {
		return 0, false
	}
	var multiplier int64
	switch unit {
	case 's':
		multiplier = 1
	case 'm':
		multiplier = 60
	case 'h':
		multiplier = 3600
	case 'd':
		multiplier = 86400
	default:
		return 0, false
	}
	if number > maxSafe/multiplier {
		return 0, false
	}
	return number * multiplier, true
}

func parseV2InputOverrides(raw json.RawMessage) ([]Override, []string, bool) {
	fields, ok := strictKeys(raw, v2InputKeys)
	if !ok {
		return nil, nil, true
	}
	rawOverrides, ok := fields["overrides"]
	if !ok || jsonNull(rawOverrides) {
		return nil, nil, true
	}
	var elements []json.RawMessage
	if json.Unmarshal(rawOverrides, &elements) != nil || len(elements) > maxRulesPerModel {
		return nil, []string{"invalid_overrides"}, false
	}
	out := make([]Override, 0, len(elements))
	for i, element := range elements {
		fields, ok := strictKeys(element, v2InputOverKeys)
		if !ok {
			return nil, []string{"unsupported_override"}, false
		}
		when := Condition{}
		if rawWhen, exists := fields["when"]; exists {
			var supported bool
			when, supported = parseV2Condition(rawWhen)
			if !supported {
				return nil, []string{"unsupported_condition"}, false
			}
		}
		patch := RatePatch{}
		if _, present := fields["price"]; present {
			v, _, valid := numberField(fields, "price")
			if !valid {
				return nil, []string{"invalid_override_price"}, false
			}
			patch.Input = int64Ptr(v)
		}
		if cacheRaw, exists := fields["cache"]; exists && !jsonNull(cacheRaw) {
			cacheFields, ok := strictKeys(cacheRaw, v2CacheKeys)
			if !ok {
				return nil, []string{"unsupported_override_cache"}, false
			}
			if _, present := cacheFields["read"]; present {
				v, _, valid := numberField(cacheFields, "read")
				if !valid {
					return nil, []string{"invalid_override_cache_read"}, false
				}
				patch.CacheRead = int64Ptr(v)
			}
			if writeRaw, present := cacheFields["write"]; present {
				write, ttl, message, supported := parseCacheWrite(writeRaw)
				if !supported {
					return nil, []string{message}, false
				}
				if len(ttl) == 0 {
					patch.CacheWrite = int64Ptr(write)
				} else {
					patch.CacheWriteByTTL, patch.HasCacheWriteByTTL = ttl, true
				}
			}
		}
		out = append(out, Override{ID: "input-" + strconv.Itoa(i), When: when, Rates: patch})
	}
	return out, nil, true
}

func parseV2Output(raw json.RawMessage, base *RateSet) (RateSet, []string, bool) {
	fields, ok := strictKeys(raw, v2OutputKeys)
	if !ok {
		return RateSet{}, []string{"unsupported_output_price"}, false
	}
	price, _, valid := numberField(fields, "price")
	if !valid {
		return RateSet{}, []string{"invalid_output_price"}, false
	}
	base.Output = price
	if _, present := fields["reasoning"]; present {
		v, _, valid := numberField(fields, "reasoning")
		if !valid {
			return RateSet{}, []string{"invalid_reasoning_price"}, false
		}
		base.Reasoning = &v
	}
	return *base, nil, true
}

func parseV2OutputOverrides(raw json.RawMessage) ([]Override, []string, bool) {
	fields, ok := strictKeys(raw, v2OutputKeys)
	if !ok {
		return nil, nil, true
	}
	rawOverrides, ok := fields["overrides"]
	if !ok || jsonNull(rawOverrides) {
		return nil, nil, true
	}
	var elements []json.RawMessage
	if json.Unmarshal(rawOverrides, &elements) != nil || len(elements) > maxRulesPerModel {
		return nil, []string{"invalid_overrides"}, false
	}
	out := make([]Override, 0, len(elements))
	for i, element := range elements {
		fields, ok := strictKeys(element, v2OutputOverKeys)
		if !ok {
			return nil, []string{"unsupported_override"}, false
		}
		when := Condition{}
		if rawWhen, exists := fields["when"]; exists {
			var supported bool
			when, supported = parseV2Condition(rawWhen)
			if !supported {
				return nil, []string{"unsupported_condition"}, false
			}
		}
		patch := RatePatch{}
		if _, present := fields["price"]; present {
			v, _, valid := numberField(fields, "price")
			if !valid {
				return nil, []string{"invalid_override_price"}, false
			}
			patch.Output = int64Ptr(v)
		}
		if _, present := fields["reasoning"]; present {
			patch.ReasoningSet = true
			if !jsonNull(fields["reasoning"]) {
				v, _, valid := numberField(fields, "reasoning")
				if !valid {
					return nil, []string{"invalid_override_reasoning"}, false
				}
				patch.Reasoning = int64Ptr(v)
			}
		}
		out = append(out, Override{ID: "output-" + strconv.Itoa(i), When: when, Rates: patch})
	}
	return out, nil, true
}

func parseV2Multipliers(raw json.RawMessage) ([]Multiplier, []string, bool) {
	var elements []json.RawMessage
	if json.Unmarshal(raw, &elements) != nil || len(elements) > maxRulesPerModel {
		return nil, []string{"invalid_multipliers"}, false
	}
	out := make([]Multiplier, 0, len(elements))
	issues := []string{}
	for i, element := range elements {
		fields, ok := strictKeys(element, v2MultiplierKeys)
		if !ok {
			return nil, []string{"unsupported_multiplier"}, false
		}
		when := Condition{}
		if rawWhen, exists := fields["when"]; exists {
			var supported bool
			when, supported = parseV2Condition(rawWhen)
			if !supported {
				return nil, []string{"unsupported_condition"}, false
			}
		}
		factors, factorIssues, supported := parseV2Factor(fields["factor"])
		if !supported {
			return nil, append(issues, factorIssues...), false
		}
		issues = append(issues, factorIssues...)
		out = append(out, Multiplier{ID: "multiplier-" + strconv.Itoa(i), When: when, Factors: factors})
	}
	return out, uniqueStrings(issues), true
}

func parseV2Factor(raw json.RawMessage) (FactorSet, []string, bool) {
	if len(raw) == 0 || jsonNull(raw) {
		return FactorSet{}, nil, false
	}
	if raw[0] != '{' {
		return scalarFactor(raw)
	}
	fields, ok := strictKeys(raw, map[string]bool{"text": true, "audio": true})
	if !ok {
		return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
	}
	issues := []string{}
	if _, present := fields["audio"]; present {
		issues = append(issues, "non_text_multiplier_ignored")
	}
	textRaw, ok := fields["text"]
	if !ok || jsonNull(textRaw) {
		return FactorSet{}, issues, true
	}
	textFields, ok := strictKeys(textRaw, map[string]bool{"input": true, "output": true})
	if !ok {
		return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
	}
	factors := FactorSet{}
	if inputRaw, ok := textFields["input"]; ok && !jsonNull(inputRaw) {
		inputFields, ok := strictKeys(inputRaw, map[string]bool{"price": true, "cache": true})
		if !ok {
			return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
		}
		if rawPrice, ok := inputFields["price"]; ok {
			s, ok := factorString(rawPrice)
			if !ok {
				return FactorSet{}, []string{"invalid_multiplier_factor"}, false
			}
			factors.Input = &s
		}
		if cacheRaw, ok := inputFields["cache"]; ok && !jsonNull(cacheRaw) {
			cacheFields, ok := strictKeys(cacheRaw, map[string]bool{"read": true, "write": true})
			if !ok {
				return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
			}
			if rawRead, ok := cacheFields["read"]; ok {
				s, ok := factorString(rawRead)
				if !ok {
					return FactorSet{}, []string{"invalid_multiplier_factor"}, false
				}
				factors.CacheRead = &s
			}
			if rawWrite, ok := cacheFields["write"]; ok {
				if rawWrite[0] == '{' {
					return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
				}
				s, ok := factorString(rawWrite)
				if !ok {
					return FactorSet{}, []string{"invalid_multiplier_factor"}, false
				}
				factors.CacheWrite = &s
			}
		}
	}
	if outputRaw, ok := textFields["output"]; ok && !jsonNull(outputRaw) {
		if outputRaw[0] != '{' {
			s, ok := factorString(outputRaw)
			if !ok {
				return FactorSet{}, []string{"invalid_multiplier_factor"}, false
			}
			factors.Output = &s
		} else {
			outputFields, ok := strictKeys(outputRaw, map[string]bool{"price": true, "reasoning": true})
			if !ok {
				return FactorSet{}, []string{"unsupported_multiplier_factor"}, false
			}
			if rawPrice, ok := outputFields["price"]; ok {
				s, ok := factorString(rawPrice)
				if !ok {
					return FactorSet{}, []string{"invalid_multiplier_factor"}, false
				}
				factors.Output = &s
			}
			if rawReasoning, ok := outputFields["reasoning"]; ok {
				s, ok := factorString(rawReasoning)
				if !ok {
					return FactorSet{}, []string{"invalid_multiplier_factor"}, false
				}
				factors.Reasoning = &s
			}
		}
	}
	return factors, issues, true
}

func scalarFactor(raw json.RawMessage) (FactorSet, []string, bool) {
	s, ok := factorString(raw)
	if !ok {
		return FactorSet{}, []string{"invalid_multiplier_factor"}, false
	}
	return FactorSet{
		Input: &s, Output: &s, CacheRead: &s, CacheWrite: &s, Reasoning: &s,
	}, nil, true
}

func factorString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || jsonNull(raw) {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || !validFactorValue(s) {
			return "", false
		}
		return s, true
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&n) != nil {
		return "", false
	}
	s := string(n)
	if !validFactorValue(s) {
		return "", false
	}
	return s, true
}

func parseV2Condition(raw json.RawMessage) (Condition, bool) {
	fields, ok := strictKeys(raw, map[string]bool{
		"context": true, "service_tier": true, "date": true, "utc_days": true,
		"utc_hours": true, "all": true, "any": true, "not": true,
		"thinking": true, "region": true, "holiday": true, "utc_dates": true,
	})
	if !ok {
		return Condition{}, false
	}
	cond := Condition{}
	if rawCtx, ok := fields["context"]; ok {
		ctxFields, ok := strictKeys(rawCtx, map[string]bool{"gt": true, "gte": true, "lt": true, "lte": true})
		if !ok {
			return Condition{}, false
		}
		ctx := &ContextCondition{}
		for key, target := range map[string]**int64{"gt": &ctx.GT, "gte": &ctx.GTE, "lt": &ctx.LT, "lte": &ctx.LTE} {
			if value, ok := ctxFields[key]; ok {
				v, err := strconv.ParseInt(string(value), 10, 64)
				if err != nil || v < 0 || v > maxSafe {
					return Condition{}, false
				}
				*target = &v
			}
		}
		cond.Context = ctx
	}
	if rawTier, ok := fields["service_tier"]; ok {
		tierFields, ok := strictKeys(rawTier, map[string]bool{"equals": true, "in": true})
		if !ok {
			return Condition{}, false
		}
		tiers, ok := stringListCondition(tierFields)
		if !ok || len(tiers) == 0 {
			return Condition{}, false
		}
		cond.ServiceTier = tiers
	}
	if rawDate, ok := fields["date"]; ok {
		dateFields, ok := strictKeys(rawDate, map[string]bool{"gte": true, "lt": true})
		if !ok {
			return Condition{}, false
		}
		date := &DateCondition{}
		if value, ok := dateFields["gte"]; ok {
			if json.Unmarshal(value, &date.GTE) != nil {
				return Condition{}, false
			}
		}
		if value, ok := dateFields["lt"]; ok {
			if json.Unmarshal(value, &date.LT) != nil {
				return Condition{}, false
			}
		}
		cond.Date = date
	}
	hasDays, hasHours := false, false
	var weekdays []int
	start, end := "00:00", "24:00"
	if rawDays, ok := fields["utc_days"]; ok {
		hasDays = true
		dayFields, ok := strictKeys(rawDays, map[string]bool{"in": true, "equals": true})
		if !ok {
			return Condition{}, false
		}
		var names []string
		if value, ok := dayFields["in"]; ok {
			if json.Unmarshal(value, &names) != nil {
				return Condition{}, false
			}
		}
		if value, ok := dayFields["equals"]; ok {
			var name string
			if json.Unmarshal(value, &name) != nil {
				return Condition{}, false
			}
			names = append(names, name)
		}
		for _, name := range names {
			day, ok := weekdayNumber(name)
			if !ok {
				return Condition{}, false
			}
			weekdays = append(weekdays, day)
		}
	}
	if rawHours, ok := fields["utc_hours"]; ok {
		hasHours = true
		hourFields, ok := strictKeys(rawHours, map[string]bool{"gte": true, "lt": true})
		if !ok {
			return Condition{}, false
		}
		if value, ok := hourFields["gte"]; ok {
			if json.Unmarshal(value, &start) != nil {
				return Condition{}, false
			}
		}
		if value, ok := hourFields["lt"]; ok {
			if json.Unmarshal(value, &end) != nil {
				return Condition{}, false
			}
			if end == "00:00" {
				end = "24:00"
			}
		}
	}
	if hasDays || hasHours {
		cond.Time = &TimeCondition{Start: start, End: end, Timezone: "UTC", Weekdays: weekdays}
	}
	if rawAll, ok := fields["all"]; ok {
		children, ok := parseConditionList(rawAll)
		if !ok {
			return Condition{}, false
		}
		cond.All = children
	}
	if rawAny, ok := fields["any"]; ok {
		children, ok := parseConditionList(rawAny)
		if !ok {
			return Condition{}, false
		}
		cond.Any = children
	}
	if rawNot, ok := fields["not"]; ok {
		child, ok := parseV2Condition(rawNot)
		if !ok {
			return Condition{}, false
		}
		cond.Not = &child
	}
	// thinking/region/holiday/utc_dates cannot be represented by the internal
	// fact set. Treating them as always-true would silently change the price.
	for _, key := range []string{"thinking", "region", "holiday", "utc_dates"} {
		if _, present := fields[key]; present {
			return Condition{}, false
		}
	}
	if !validCondition(cond, 0, new(int)) {
		return Condition{}, false
	}
	return cond, true
}

func stringListCondition(fields map[string]json.RawMessage) ([]string, bool) {
	out := []string{}
	if value, ok := fields["equals"]; ok {
		var s string
		if json.Unmarshal(value, &s) != nil {
			return nil, false
		}
		out = append(out, s)
	}
	if value, ok := fields["in"]; ok {
		var list []string
		if json.Unmarshal(value, &list) != nil {
			return nil, false
		}
		out = append(out, list...)
	}
	return uniqueStrings(out), true
}

func parseConditionList(raw json.RawMessage) ([]Condition, bool) {
	var elements []json.RawMessage
	if json.Unmarshal(raw, &elements) != nil || len(elements) > maxConditionList {
		return nil, false
	}
	out := make([]Condition, 0, len(elements))
	for _, element := range elements {
		cond, ok := parseV2Condition(element)
		if !ok {
			return nil, false
		}
		out = append(out, cond)
	}
	return out, true
}

func weekdayNumber(name string) (int, bool) {
	switch strings.ToLower(name) {
	case "mon":
		return 1, true
	case "tue":
		return 2, true
	case "wed":
		return 3, true
	case "thu":
		return 4, true
	case "fri":
		return 5, true
	case "sat":
		return 6, true
	case "sun":
		return 7, true
	default:
		return 0, false
	}
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func parseV2Catalog(body []byte) ([]entry, error) {
	var root map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(body, &root) != nil || len(root) == 0 {
		return nil, errInvalid
	}
	out := []entry{}
	for provider, p := range root {
		if !validIdentity(provider) {
			return nil, errInvalid
		}
		for model, raw := range p.Models {
			if !validIdentity(model) {
				return nil, errInvalid
			}
			var fields struct {
				Cost        json.RawMessage `json:"cost"`
				CanonicalID string          `json:"canonical_id"`
			}
			if json.Unmarshal(raw, &fields) != nil {
				return nil, errInvalid
			}
			if fields.CanonicalID != "" && !validIdentity(fields.CanonicalID) {
				return nil, errInvalid
			}
			def, issues, supported := parseV2Definition(fields.Cost)
			out = append(out, entry{
				Model: model, Provider: provider, Definition: def, Supported: supported,
				SupportIssues: issues, CanonicalModel: fields.CanonicalID,
			})
		}
	}
	if len(out) == 0 || len(out) > maxCatalogEntries {
		return nil, errInvalid
	}
	return out, nil
}

func parseCatalog(body []byte, source string) ([]entry, error) {
	var entries []entry
	var err error
	switch source {
	case SourceV1:
		entries, err = parseV1Catalog(body)
	case SourceV2:
		entries, err = parseV2Catalog(body)
	default:
		return nil, errInvalid
	}
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Definition = completeDefinition(entries[i].Definition)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// Index and cache

func sortEntries(in []entry) {
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.CanonicalModel != b.CanonicalModel {
			return a.CanonicalModel < b.CanonicalModel
		}
		if a.Supported != b.Supported {
			return !a.Supported
		}
		if a.Definition.Base.Input != b.Definition.Base.Input {
			return a.Definition.Base.Input < b.Definition.Base.Input
		}
		return a.Definition.Base.Output < b.Definition.Base.Output
	})
}

type catalogIndex struct {
	exact            map[identity][]entry
	byModel          map[string][]entry
	canonical        map[identity][]entry
	canonicalByModel map[string][]entry
}

func indexCatalog(entries []entry) catalogIndex {
	index := catalogIndex{
		exact:            make(map[identity][]entry, len(entries)),
		byModel:          make(map[string][]entry, len(entries)),
		canonical:        make(map[identity][]entry, len(entries)),
		canonicalByModel: make(map[string][]entry, len(entries)),
	}
	for _, e := range entries {
		e = cloneEntry(e)
		key := identity{Model: e.Model, Provider: e.Provider}
		index.exact[key] = append(index.exact[key], e)
		index.byModel[e.Model] = append(index.byModel[e.Model], e)
		if e.CanonicalModel != "" {
			ck := identity{Model: e.CanonicalModel, Provider: e.Provider}
			index.canonical[ck] = append(index.canonical[ck], e)
			index.canonicalByModel[e.CanonicalModel] = append(index.canonicalByModel[e.CanonicalModel], e)
		}
	}
	for key, list := range index.exact {
		sortEntries(list)
		index.exact[key] = list
	}
	for model, list := range index.byModel {
		sortEntries(list)
		index.byModel[model] = list
	}
	for key, list := range index.canonical {
		sortEntries(list)
		index.canonical[key] = list
	}
	for model, list := range index.canonicalByModel {
		sortEntries(list)
		index.canonicalByModel[model] = list
	}
	return index
}

func (idx catalogIndex) exactEntry(model, provider string) (entry, bool) {
	list := idx.exact[identity{Model: model, Provider: provider}]
	if len(list) != 1 {
		return entry{}, false
	}
	return cloneEntry(list[0]), true
}

func (idx catalogIndex) modelEntries(model string) []entry {
	return cloneEntries(idx.byModel[model])
}

func (idx catalogIndex) canonicalEntries(model, provider string) []entry {
	return cloneEntries(idx.canonical[identity{Model: model, Provider: provider}])
}

func (idx catalogIndex) canonicalAnyEntries(model string) []entry {
	return cloneEntries(idx.canonicalByModel[model])
}

func (idx catalogIndex) candidateEntries(model string, excludeProvider string) []entry {
	list := idx.byModel[model]
	out := []entry{}
	for _, e := range list {
		if e.Provider != excludeProvider {
			out = append(out, cloneEntry(e))
		}
	}
	return out
}

func splitKnownNamespace(model string) (string, string, bool) {
	for _, provider := range []string{"commandcode", "opencode-go"} {
		prefix := provider + "/"
		if strings.HasPrefix(model, prefix) && len(model) > len(prefix) {
			return provider, model[len(prefix):], true
		}
	}
	return "", "", false
}

func catalogURL(source string) string {
	if source == SourceV1 {
		return catalogURLV1
	}
	return catalogURLV2
}

func (s *Service) fetch(ctx context.Context, source string) ([]entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL(source), nil)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("catalog request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("catalog HTTP status rejected")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return nil, errors.New("catalog read failed or exceeded size limit")
	}
	entries, err := parseCatalog(body, source)
	if err != nil {
		return nil, errors.New("catalog format invalid")
	}
	return entries, nil
}

func validEntry(e entry) bool {
	if !validIdentity(e.Model) || !validIdentity(e.Provider) {
		return false
	}
	if e.CanonicalModel != "" && !validIdentity(e.CanonicalModel) {
		return false
	}
	if e.Supported {
		return validDefinition(e.Definition)
	}
	return reflectMapEqual(e.Definition.Base.CacheWriteByTTL, zeroRateSet().CacheWriteByTTL)
}

func (s *Service) cacheFile(source string) string {
	if s.cachePath == "" {
		return ""
	}
	return s.cachePath + "." + source
}

func (s *Service) loadSourceCacheLocked(source string) {
	state := s.sources[source]
	if state == nil {
		state = &sourceState{}
		s.sources[source] = state
	}
	if state.loaded {
		return
	}
	state.loaded = true
	path := s.cacheFile(source)
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		state.lastError = "catalog cache read failed"
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	var d diskCache
	if err != nil || len(body) > maxCatalogBytes || json.Unmarshal(body, &d) != nil ||
		d.Version != diskCacheVersion || d.Source != source || len(d.Entries) == 0 ||
		len(d.Entries) > maxCatalogEntries || d.LastSync.IsZero() {
		state.lastError = "catalog cache invalid"
		return
	}
	seen := map[identity]bool{}
	for _, e := range d.Entries {
		k := identity{e.Model, e.Provider}
		if !validEntry(e) || seen[k] {
			state.lastError = "catalog cache invalid"
			return
		}
		seen[k] = true
	}
	state.catalog = indexCatalog(d.Entries)
	state.lastSync = d.LastSync
}

func (s *Service) saveCacheLocked(source string, entries []entry, lastSync time.Time) error {
	path := s.cacheFile(source)
	if path == "" {
		return nil
	}
	body, err := json.Marshal(diskCache{Version: diskCacheVersion, Source: source, LastSync: lastSync, Entries: entries})
	if err != nil || len(body) > maxCatalogBytes {
		return errors.New("catalog cache too large")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pricing-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
