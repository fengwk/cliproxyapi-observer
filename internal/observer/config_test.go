package observer

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestParseConfigEmptyUsesDefaults(t *testing.T) {
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.DataPath != defaultDataPath {
		t.Errorf("DataPath = %q, want %q", cfg.DataPath, defaultDataPath)
	}
	if cfg.StatsRetentionDays != defaultStatsRetentionDays {
		t.Errorf("StatsRetentionDays = %d", cfg.StatsRetentionDays)
	}
	if cfg.RequestRetention != defaultRequestRetention || cfg.BodyRetention != defaultBodyRetention {
		t.Errorf("retentions = %s/%s", cfg.RequestRetention, cfg.BodyRetention)
	}
	if cfg.CaptureBodies {
		t.Errorf("CaptureBodies should default to false")
	}
	if cfg.MaxBodyBytes != defaultMaxBodyBytes || cfg.MaxBodyStorageBytes != defaultMaxBodyStorageBytes {
		t.Errorf("body caps = %d/%d", cfg.MaxBodyBytes, cfg.MaxBodyStorageBytes)
	}
	if cfg.FlushInterval != defaultFlushInterval {
		t.Errorf("FlushInterval = %s", cfg.FlushInterval)
	}
	if cfg.CompactInterval != defaultCompactInterval {
		t.Errorf("CompactInterval = %s, want %s", cfg.CompactInterval, defaultCompactInterval)
	}
	if cfg.CompactMinBytes != defaultCompactMinBytes {
		t.Errorf("CompactMinBytes = %d, want %d", cfg.CompactMinBytes, defaultCompactMinBytes)
	}
}

func TestParseConfigExplicitValues(t *testing.T) {
	raw := []byte(`
enabled: true
db: /tmp/observer.db
stats-retention-days: 30
request-retention: 6h
body-retention: 30m
capture-bodies: true
max-body-bytes: 2048
max-body-storage-bytes: 1048576
flush: 2s
compact-interval: 30m
compact-min-bytes: 16777216
prices:
  claude-sonnet-4-5-20250929:
    input: 3
    output: 15
    cache-read: 0.3
    cache-creation: 3.75
`)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.DataPath != "/tmp/observer.db" || cfg.StatsRetentionDays != 30 {
		t.Errorf("unexpected base config: %+v", cfg)
	}
	if cfg.RequestRetention != 6*time.Hour || cfg.BodyRetention != 30*time.Minute {
		t.Errorf("retentions = %s/%s", cfg.RequestRetention, cfg.BodyRetention)
	}
	if !cfg.CaptureBodies || cfg.MaxBodyBytes != 2048 || cfg.MaxBodyStorageBytes != 1048576 {
		t.Errorf("body config = %+v", cfg)
	}
	if cfg.FlushInterval != 2*time.Second {
		t.Errorf("FlushInterval = %s", cfg.FlushInterval)
	}
	if cfg.CompactInterval != 30*time.Minute {
		t.Errorf("CompactInterval = %s, want 30m", cfg.CompactInterval)
	}
	if cfg.CompactMinBytes != 16777216 {
		t.Errorf("CompactMinBytes = %d, want 16777216", cfg.CompactMinBytes)
	}
	price, ok := cfg.Prices["claude-sonnet-4-5-20250929"]
	if !ok || price.Input != 3 || price.Output != 15 || price.CacheRead != 0.3 || price.CacheCreation != 3.75 {
		t.Errorf("price = %+v ok=%v", price, ok)
	}
}

func TestParseConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"body retention over 24h":       "body-retention: 25h",
		"negative body retention":       "body-retention: -1h",
		"zero flush":                    "flush: 0s",
		"negative request retention":    "request-retention: -5m",
		"zero stats days":               "stats-retention-days: 0",
		"negative stats days":           "stats-retention-days: -3",
		"negative max body bytes":       "max-body-bytes: -1",
		"per-body exceeds storage":      "max-body-bytes: 4096\nmax-body-storage-bytes: 1024",
		"negative price":                "prices:\n  m:\n    input: -1",
		"nan price":                     "prices:\n  m:\n    input: .nan",
		"inf price":                     "prices:\n  m:\n    output: .inf",
		"bad duration":                  "flush: soon",
		"bad yaml":                      "enabled: [",
		"compact interval under 1m":     "compact-interval: 59s",
		"compact interval over 24h":     "compact-interval: 25h",
		"compact interval negative":     "compact-interval: -1m",
		"compact min bytes under 64KiB": "compact-min-bytes: 65535",
		"compact min bytes zero":        "compact-min-bytes: 0",
		"compact min bytes over 8GiB":   "compact-min-bytes: 8589934593",
		"prices duplicate after trim":   "prices:\n  m:\n    input: 1\n  \" m \":\n    input: 2",
		"prices empty model id":         "prices:\n  \"   \":\n    input: 1",
	}
	for name, raw := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestNormalizeConfigAcceptsZeroValue(t *testing.T) {
	// Open applies defaults for hand-built configs without re-parsing YAML.
	cfg, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatalf("normalizeConfig: %v", err)
	}
	if cfg.DataPath != defaultDataPath || cfg.FlushInterval != defaultFlushInterval {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestResolveDataPathRelativeToWorkingDir(t *testing.T) {
	if _, err := resolveDataPath("/abs/observer.db"); err != nil {
		t.Fatalf("absolute path rejected: %v", err)
	}
	got, err := resolveDataPath("data/cliproxyapi-observer.db")
	if err != nil {
		t.Fatalf("resolveDataPath: %v", err)
	}
	if got == "data/cliproxyapi-observer.db" {
		t.Errorf("relative path was not resolved: %q", got)
	}
}

func TestValidatePriceFinite(t *testing.T) {
	if err := validatePrice("m", Price{Input: math.NaN()}); err == nil {
		t.Errorf("NaN accepted")
	}
	if err := validatePrice("m", Price{Output: math.Inf(1)}); err == nil {
		t.Errorf("+Inf accepted")
	}
	if err := validatePrice("m", Price{CacheRead: 0.5}); err != nil {
		t.Errorf("valid price rejected: %v", err)
	}
}

// TestPriceUnmarshalJSON verifies that Price JSON decoding strictly accepts valid
// underscore and hyphen keys while rejecting unknown fields, nulls, negative values,
// non-finite floats, arrays and trailing garbage.
func TestPriceUnmarshalJSON(t *testing.T) {
	valid := []string{
		`{"input": 1.5, "output": 2.0}`,
		`{"input": 0, "output": 0, "cache_read": 0.1, "cache_creation": 0.2}`,
		`{"input": 1.0, "output": 2.0, "cache-read": 0.3, "cache-creation": 0.4}`,
	}
	for _, raw := range valid {
		var p Price
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Errorf("valid price %s rejected: %v", raw, err)
		}
	}

	invalid := map[string]string{
		"null":                 "null",
		"empty":                "",
		"array":                "[]",
		"string":               `"1.0"`,
		"unknown field":        `{"input": 1, "unknown": 2}`,
		"duplicate cache read": `{"cache_read": 1, "cache-read": 2}`,
		"null field":           `{"input": null}`,
		"negative input":       `{"input": -0.5}`,
		"string field value":   `{"input": "1.0"}`,
		"trailing data":        `{"input": 1} trailing`,
	}
	for name, raw := range invalid {
		var p Price
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Errorf("%s: raw %q accepted as Price", name, raw)
		}
	}
}

// Core preserves JSON price spelling when persisting settings patches as YAML.
func TestParseConfigPriceJSONKeysRoundTrip(t *testing.T) {
	cfg, err := ParseConfig([]byte(`prices:
  model:
    input: 2.5
    output: 10
    cache_read: 1.25
    cache_creation: 2.5
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Prices["model"]; got != (Price{Input: 2.5, Output: 10, CacheRead: 1.25, CacheCreation: 2.5}) {
		t.Fatalf("persisted JSON price lost fields: %+v", got)
	}
	if _, err := ParseConfig([]byte("prices:\n  model:\n    cache_read: 1\n    cache-read: 2\n")); err == nil {
		t.Fatal("ambiguous alias fields accepted")
	}
}
