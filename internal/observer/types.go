// Package observer owns bounded local observation storage and token accounting.
package observer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var ErrNotFound = errors.New("observation not found")

type Config struct {
	DataPath            string
	StatsRetentionDays  int
	RequestRetention    time.Duration
	BodyRetention       time.Duration
	CaptureBodies       bool
	MaxBodyBytes        int
	MaxBodyStorageBytes int64
	FlushInterval       time.Duration
	CompactInterval     time.Duration
	CompactMinBytes     int64
	Prices              map[string]Price
	PriceRules          []PriceRule
}

// Price is expressed in USD per million tokens. Unknown accounting is unpriced.
type Price struct {
	Input         float64 `json:"input" yaml:"input"`
	Output        float64 `json:"output" yaml:"output"`
	CacheRead     float64 `json:"cache_read" yaml:"cache-read"`
	CacheCreation float64 `json:"cache_creation" yaml:"cache-creation"`
}

// UnmarshalJSON strictly decodes and validates a JSON Price object, accepting
// both underscore and hyphen keys while rejecting unknown, null, negative or
// non-finite values.
func (p *Price) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return errors.New("price object must not be null or empty")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("malformed price JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("price must be a JSON object")
	}

	var parsed Price
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("malformed price key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return errors.New("price key must be a string")
		}
		canonicalKey := ""
		switch key {
		case "input":
			canonicalKey = "input"
		case "output":
			canonicalKey = "output"
		case "cache_read", "cache-read":
			canonicalKey = "cache-read"
		case "cache_creation", "cache-creation":
			canonicalKey = "cache-creation"
		default:
			return fmt.Errorf("unknown price field %q", key)
		}
		if seen[canonicalKey] {
			return fmt.Errorf("duplicate price field %q", key)
		}
		seen[canonicalKey] = true

		var rawVal json.RawMessage
		if err := dec.Decode(&rawVal); err != nil {
			return fmt.Errorf("decode price field %q: %w", key, err)
		}
		if string(bytes.TrimSpace(rawVal)) == "null" {
			return fmt.Errorf("price field %q must not be null", key)
		}
		var val float64
		if err := json.Unmarshal(rawVal, &val); err != nil {
			return fmt.Errorf("price field %q must be a number: %w", key, err)
		}
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return fmt.Errorf("price field %q must be a finite number", key)
		}
		if val < 0 {
			return fmt.Errorf("price field %q must not be negative", key)
		}

		switch canonicalKey {
		case "input":
			parsed.Input = val
		case "output":
			parsed.Output = val
		case "cache-read":
			parsed.CacheRead = val
		case "cache-creation":
			parsed.CacheCreation = val
		}
	}

	tok, err = dec.Token()
	if err != nil || tok != json.Delim('}') {
		return errors.New("malformed price JSON ending")
	}
	if dec.More() {
		return errors.New("trailing data in price JSON")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing data in price JSON")
	}

	*p = parsed
	return nil
}

// Core persists raw JSON patches as YAML, including underscore price keys.
// Reuse the strict price decoder for both UI JSON and existing hyphen YAML.
func (p *Price) UnmarshalYAML(node *yaml.Node) error {
	var values map[string]any
	if err := node.Decode(&values); err != nil {
		return err
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return p.UnmarshalJSON(raw)
}

type Query struct {
	From     time.Time
	To       time.Time
	Provider string
	Model    string
	// Identity filters apply to requests and key-dimensional preaggregated stats.
	ClientKeyID string
	AuthIndex   string
	Limit       int
	Offset      int
}

type Counters struct {
	Requests            uint64  `json:"requests"`
	FailedRequests      uint64  `json:"failed_requests"`
	InputTokens         uint64  `json:"input_tokens"`
	OutputTokens        uint64  `json:"output_tokens"`
	ReasoningTokens     uint64  `json:"reasoning_tokens"`
	CacheReadTokens     uint64  `json:"cache_read_tokens"`
	CacheCreationTokens uint64  `json:"cache_creation_tokens"`
	TotalTokens         uint64  `json:"total_tokens"`
	CacheHits           uint64  `json:"cache_hits"`
	LatencyNS           uint64  `json:"latency_ns"`
	LatencySamples      uint64  `json:"latency_samples"`
	TTFTNS              uint64  `json:"ttft_ns"`
	TTFTSamples         uint64  `json:"ttft_samples"`
	CostUSD             float64 `json:"cost_usd"`
	UnpricedRequests    uint64  `json:"unpriced_requests"`
	// Only these complete-accounting partitions are billable. They are stored,
	// never exposed, and must not include ambiguous raw display counters.
	completeRequests uint64
	billableInput    uint64
	billableRead     uint64
	billableWrite    uint64
	billableOutput   uint64
}

type Request struct {
	Sequence            uint64                `json:"sequence"`
	RequestID           string                `json:"request_id"`
	TraceID             string                `json:"trace_id"`
	Time                time.Time             `json:"time"`
	Provider            string                `json:"provider"`
	Model               string                `json:"model"`
	Alias               string                `json:"alias,omitempty"`
	Executor            string                `json:"executor"`
	AuthType            string                `json:"auth_type,omitempty"`
	ClientKeyID         string                `json:"client_key_id,omitempty"`
	AuthIndex           string                `json:"auth_index,omitempty"`
	ServiceTier         string                `json:"service_tier,omitempty"`
	ReasoningEffort     string                `json:"reasoning_effort,omitempty"`
	Stream              bool                  `json:"stream"`
	Failed              bool                  `json:"failed"`
	FailureStatus       int                   `json:"failure_status,omitempty"`
	InputTokens         int64                 `json:"input_tokens"`
	UncachedInputTokens int64                 `json:"uncached_input_tokens"`
	OutputTokens        int64                 `json:"output_tokens"`
	ReasoningTokens     int64                 `json:"reasoning_tokens"`
	CacheReadTokens     int64                 `json:"cache_read_tokens"`
	CacheCreationTokens int64                 `json:"cache_creation_tokens"`
	TotalTokens         int64                 `json:"total_tokens"`
	AccountingQuality   string                `json:"accounting_quality"`
	RawUsage            pluginapi.UsageDetail `json:"raw_usage"`
	LatencyNS           int64                 `json:"latency_ns"`
	TTFTNS              int64                 `json:"ttft_ns"`
	GenerationNS        int64                 `json:"generation_ns"`
	TPS                 *float64              `json:"tps"`
	CacheHit            bool                  `json:"cache_hit"`
	CostUSD             *float64              `json:"cost_usd"`
	BodyAvailable       bool                  `json:"body_available"`
}

type RequestPage struct {
	Items   []Request `json:"items"`
	Offset  int       `json:"offset"`
	Limit   int       `json:"limit"`
	HasMore bool      `json:"has_more"`
}

type Group struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Counters
}

type Point struct {
	Time time.Time `json:"time"`
	Counters
}

type KeyGroup struct {
	ID string `json:"id"`
	Counters
}

type Summary struct {
	From        time.Time  `json:"from"`
	To          time.Time  `json:"to"`
	Totals      Counters   `json:"totals"`
	Groups      []Group    `json:"groups"`
	Series      []Point    `json:"series"`
	ClientKeys  []KeyGroup `json:"client_keys"`
	Credentials []KeyGroup `json:"credentials"`
}

type BodyDetail struct {
	RequestID string    `json:"request_id"`
	TraceID   string    `json:"trace_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Content   string    `json:"body"`
	Redacted  bool      `json:"redacted"`
	SizeBytes int64     `json:"size_bytes"`
}

type Status struct {
	DatabaseBytes                int64  `json:"database_bytes"`
	ReclaimableBytes             int64  `json:"reclaimable_bytes"`
	Compactions                  uint64 `json:"compactions"`
	CompactionErrors             uint64 `json:"compaction_errors"`
	LastCompactionUnix           int64  `json:"last_compaction_unix"`
	LastCompactionReclaimedBytes int64  `json:"last_compaction_reclaimed_bytes"`
	DroppedUsage                 uint64 `json:"dropped_usage"`
	DroppedBodies                uint64 `json:"dropped_bodies"`
	WriteErrors                  uint64 `json:"write_errors"`
	Queued                       int    `json:"queued"`
}
