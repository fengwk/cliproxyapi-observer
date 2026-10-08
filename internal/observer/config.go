package observer

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults and validation bounds for the observer storage configuration.
const (
	defaultDataPath            = "data/cliproxyapi-observer.db"
	defaultStatsRetentionDays  = 365
	defaultRequestRetention    = 24 * time.Hour
	defaultBodyRetention       = 24 * time.Hour
	defaultMaxBodyBytes        = 1 << 20   // 1 MiB
	defaultMaxBodyStorageBytes = 256 << 20 // 256 MiB
	defaultFlushInterval       = time.Second
	defaultCompactInterval     = 15 * time.Minute
	defaultCompactMinBytes     = 8 << 20 // 8 MiB

	MaxStatsRetentionDays = 3650
	MaxRequestRetention   = 30 * 24 * time.Hour
	MinRequestRetention   = time.Minute
	MinBodyRetention      = time.Minute
	MaxBodyRetention      = 24 * time.Hour
	MaxBodyBytesLimit     = 64 << 20
	MaxBodyStorageLimit   = 8 << 30
	MinFlushInterval      = time.Millisecond
	MaxFlushInterval      = time.Hour
	MinCompactInterval    = time.Minute
	MaxCompactInterval    = 24 * time.Hour
	MinCompactMinBytes    = 64 << 10       // 64 KiB
	MaxCompactMinBytes    = int64(8) << 30 // 8 GiB
	defaultCleanupPeriod  = time.Minute

	maxStatsRetentionDays = MaxStatsRetentionDays
	maxRequestRetention   = MaxRequestRetention
	minRequestRetention   = MinRequestRetention
	minBodyRetention      = MinBodyRetention
	maxBodyRetention      = MaxBodyRetention
	maxBodyBytesLimit     = MaxBodyBytesLimit
	maxBodyStorageLimit   = MaxBodyStorageLimit
	minFlushInterval      = MinFlushInterval
	maxFlushInterval      = MaxFlushInterval
	minCompactInterval    = MinCompactInterval
	maxCompactInterval    = MaxCompactInterval
	minCompactMinBytes    = MinCompactMinBytes
	maxCompactMinBytes    = MaxCompactMinBytes
)

// configYAML mirrors the plugin-owned YAML subtree. Pointer fields distinguish
// "absent" (apply default) from an explicit zero value (validation error).
//
// `enabled` is accepted for schema compatibility but is intentionally not part
// of Config: whether the observer is active is owned by the host
// (plugins.<id>.enabled), not by the storage layer.
type configYAML struct {
	Enabled             *bool            `yaml:"enabled"`
	DB                  string           `yaml:"db"`
	StatsRetentionDays  *int             `yaml:"stats-retention-days"`
	RequestRetention    string           `yaml:"request-retention"`
	BodyRetention       string           `yaml:"body-retention"`
	CaptureBodies       *bool            `yaml:"capture-bodies"`
	MaxBodyBytes        *int             `yaml:"max-body-bytes"`
	MaxBodyStorageBytes *int64           `yaml:"max-body-storage-bytes"`
	Flush               string           `yaml:"flush"`
	CompactInterval     string           `yaml:"compact-interval"`
	CompactMinBytes     *int64           `yaml:"compact-min-bytes"`
	Prices              map[string]Price `yaml:"prices"`
}

func defaultConfig() Config {
	return Config{
		DataPath:            defaultDataPath,
		StatsRetentionDays:  defaultStatsRetentionDays,
		RequestRetention:    defaultRequestRetention,
		BodyRetention:       defaultBodyRetention,
		CaptureBodies:       false,
		MaxBodyBytes:        defaultMaxBodyBytes,
		MaxBodyStorageBytes: defaultMaxBodyStorageBytes,
		FlushInterval:       defaultFlushInterval,
		CompactInterval:     defaultCompactInterval,
		CompactMinBytes:     defaultCompactMinBytes,
		Prices:              map[string]Price{},
	}
}

// ParseConfig decodes and validates the observer YAML configuration. An empty
// input yields documented defaults.
func ParseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		var in configYAML
		if err := yaml.Unmarshal(raw, &in); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
		if strings.TrimSpace(in.DB) != "" {
			cfg.DataPath = strings.TrimSpace(in.DB)
		}
		if in.StatsRetentionDays != nil {
			if *in.StatsRetentionDays < 1 {
				return Config{}, fmt.Errorf("stats-retention-days must be positive, got %d", *in.StatsRetentionDays)
			}
			cfg.StatsRetentionDays = *in.StatsRetentionDays
		}
		if strings.TrimSpace(in.RequestRetention) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(in.RequestRetention))
			if err != nil {
				return Config{}, fmt.Errorf("parse request-retention: %w", err)
			}
			if d <= 0 {
				return Config{}, fmt.Errorf("request-retention must be positive, got %s", d)
			}
			cfg.RequestRetention = d
		}
		if strings.TrimSpace(in.BodyRetention) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(in.BodyRetention))
			if err != nil {
				return Config{}, fmt.Errorf("parse body-retention: %w", err)
			}
			if d <= 0 {
				return Config{}, fmt.Errorf("body-retention must be positive, got %s", d)
			}
			cfg.BodyRetention = d
		}
		if in.CaptureBodies != nil {
			cfg.CaptureBodies = *in.CaptureBodies
		}
		if in.MaxBodyBytes != nil {
			cfg.MaxBodyBytes = *in.MaxBodyBytes
		}
		if in.MaxBodyStorageBytes != nil {
			cfg.MaxBodyStorageBytes = *in.MaxBodyStorageBytes
		}
		if strings.TrimSpace(in.Flush) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(in.Flush))
			if err != nil {
				return Config{}, fmt.Errorf("parse flush: %w", err)
			}
			if d <= 0 {
				return Config{}, fmt.Errorf("flush must be positive, got %s", d)
			}
			cfg.FlushInterval = d
		}
		if strings.TrimSpace(in.CompactInterval) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(in.CompactInterval))
			if err != nil {
				return Config{}, fmt.Errorf("parse compact-interval: %w", err)
			}
			if d <= 0 {
				return Config{}, fmt.Errorf("compact-interval must be positive, got %s", d)
			}
			cfg.CompactInterval = d
		}
		if in.CompactMinBytes != nil {
			if *in.CompactMinBytes < minCompactMinBytes {
				return Config{}, fmt.Errorf("compact-min-bytes must be at least %d", minCompactMinBytes)
			}
			cfg.CompactMinBytes = *in.CompactMinBytes
		}
		if in.Prices != nil {
			cfg.Prices = in.Prices
		}
	}
	return normalizeConfig(cfg)
}

// normalizeConfig fills unset fields with defaults and rejects invalid values.
// It is idempotent so Open can safely re-normalize a hand-built Config.
func normalizeConfig(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.DataPath) == "" {
		cfg.DataPath = defaultDataPath
	} else {
		cfg.DataPath = strings.TrimSpace(cfg.DataPath)
	}
	if strings.ContainsRune(cfg.DataPath, 0) {
		return Config{}, fmt.Errorf("db path must not contain NUL")
	}

	if cfg.StatsRetentionDays == 0 {
		cfg.StatsRetentionDays = defaultStatsRetentionDays
	}
	if cfg.StatsRetentionDays < 1 || cfg.StatsRetentionDays > maxStatsRetentionDays {
		return Config{}, fmt.Errorf("stats-retention-days must be between 1 and %d, got %d", maxStatsRetentionDays, cfg.StatsRetentionDays)
	}

	if cfg.RequestRetention == 0 {
		cfg.RequestRetention = defaultRequestRetention
	}
	if cfg.RequestRetention < minRequestRetention || cfg.RequestRetention > maxRequestRetention {
		return Config{}, fmt.Errorf("request-retention must be between %s and %s, got %s", minRequestRetention, maxRequestRetention, cfg.RequestRetention)
	}

	if cfg.BodyRetention == 0 {
		cfg.BodyRetention = defaultBodyRetention
	}
	if cfg.BodyRetention < minBodyRetention || cfg.BodyRetention > maxBodyRetention {
		return Config{}, fmt.Errorf("body-retention must be between %s and %s (24h cap), got %s", minBodyRetention, maxBodyRetention, cfg.BodyRetention)
	}

	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxBodyBytes < 1 || cfg.MaxBodyBytes > maxBodyBytesLimit {
		return Config{}, fmt.Errorf("max-body-bytes must be between 1 and %d, got %d", maxBodyBytesLimit, cfg.MaxBodyBytes)
	}

	if cfg.MaxBodyStorageBytes == 0 {
		cfg.MaxBodyStorageBytes = defaultMaxBodyStorageBytes
	}
	if cfg.MaxBodyStorageBytes < 1 || cfg.MaxBodyStorageBytes > maxBodyStorageLimit {
		return Config{}, fmt.Errorf("max-body-storage-bytes must be between 1 and %d, got %d", maxBodyStorageLimit, cfg.MaxBodyStorageBytes)
	}
	if int64(cfg.MaxBodyBytes) > cfg.MaxBodyStorageBytes {
		return Config{}, fmt.Errorf("max-body-bytes (%d) exceeds max-body-storage-bytes (%d)", cfg.MaxBodyBytes, cfg.MaxBodyStorageBytes)
	}

	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.FlushInterval < minFlushInterval || cfg.FlushInterval > maxFlushInterval {
		return Config{}, fmt.Errorf("flush must be between %s and %s, got %s", minFlushInterval, maxFlushInterval, cfg.FlushInterval)
	}

	if cfg.CompactInterval == 0 {
		cfg.CompactInterval = defaultCompactInterval
	}
	if cfg.CompactInterval < minCompactInterval || cfg.CompactInterval > maxCompactInterval {
		return Config{}, fmt.Errorf("compact-interval must be between %s and %s, got %s", minCompactInterval, maxCompactInterval, cfg.CompactInterval)
	}

	if cfg.CompactMinBytes == 0 {
		cfg.CompactMinBytes = defaultCompactMinBytes
	}
	if cfg.CompactMinBytes < minCompactMinBytes || cfg.CompactMinBytes > maxCompactMinBytes {
		return Config{}, fmt.Errorf("compact-min-bytes must be between %d and %d, got %d", minCompactMinBytes, maxCompactMinBytes, cfg.CompactMinBytes)
	}

	prices := make(map[string]Price, len(cfg.Prices))
	for key, price := range cfg.Prices {
		model := strings.TrimSpace(key)
		if model == "" {
			return Config{}, fmt.Errorf("prices contains an empty model id")
		}
		if _, exists := prices[model]; exists {
			return Config{}, fmt.Errorf("prices contains duplicate model id %q after trimming", model)
		}
		if err := validatePrice(model, price); err != nil {
			return Config{}, err
		}
		prices[model] = price
	}
	cfg.Prices = prices
	return cfg, nil
}

func validatePrice(model string, p Price) error {
	for name, v := range map[string]float64{
		"input":          p.Input,
		"output":         p.Output,
		"cache-read":     p.CacheRead,
		"cache-creation": p.CacheCreation,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("price %q %s must be a finite number", model, name)
		}
		if v < 0 {
			return fmt.Errorf("price %q %s must not be negative", model, name)
		}
	}
	return nil
}

// resolveDataPath makes a relative database path absolute against the current
// CPA working directory, matching the documented default.
func resolveDataPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve db path: %w", err)
	}
	return abs, nil
}

func clonePrices(in map[string]Price) map[string]Price {
	out := make(map[string]Price, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
