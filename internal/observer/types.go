// Package observer owns bounded local observation storage and token accounting.
package observer

import (
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	Prices              map[string]Price
}

// Price is expressed in USD per million tokens. Unknown accounting is unpriced.
type Price struct {
	Input         float64 `json:"input" yaml:"input"`
	Output        float64 `json:"output" yaml:"output"`
	CacheRead     float64 `json:"cache_read" yaml:"cache-read"`
	CacheCreation float64 `json:"cache_creation" yaml:"cache-creation"`
}

type Query struct {
	From     time.Time
	To       time.Time
	Provider string
	Model    string
	Limit    int
	Cursor   string
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
	Items      []Request `json:"items"`
	NextCursor string    `json:"next_cursor"`
	HasMore    bool      `json:"has_more"`
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

type Summary struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Totals Counters  `json:"totals"`
	Groups []Group   `json:"groups"`
	Series []Point   `json:"series"`
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
	DroppedUsage  uint64 `json:"dropped_usage"`
	DroppedBodies uint64 `json:"dropped_bodies"`
	WriteErrors   uint64 `json:"write_errors"`
	Queued        int    `json:"queued"`
}
