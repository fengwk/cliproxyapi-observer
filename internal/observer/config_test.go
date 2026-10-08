package observer

import (
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
	price, ok := cfg.Prices["claude-sonnet-4-5-20250929"]
	if !ok || price.Input != 3 || price.Output != 15 || price.CacheRead != 0.3 || price.CacheCreation != 3.75 {
		t.Errorf("price = %+v ok=%v", price, ok)
	}
}

func TestParseConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"body retention over 24h":    "body-retention: 25h",
		"negative body retention":    "body-retention: -1h",
		"zero flush":                 "flush: 0s",
		"negative request retention": "request-retention: -5m",
		"zero stats days":            "stats-retention-days: 0",
		"negative stats days":        "stats-retention-days: -3",
		"negative max body bytes":    "max-body-bytes: -1",
		"per-body exceeds storage":   "max-body-bytes: 4096\nmax-body-storage-bytes: 1024",
		"negative price":             "prices:\n  m:\n    input: -1",
		"nan price":                  "prices:\n  m:\n    input: .nan",
		"inf price":                  "prices:\n  m:\n    output: .inf",
		"bad duration":               "flush: soon",
		"bad yaml":                   "enabled: [",
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
