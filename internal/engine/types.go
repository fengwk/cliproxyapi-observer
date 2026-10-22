package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type Options struct {
	DataPath string
	Now      func() time.Time
	Pricer   pricing.Pricer
}
type Limits struct {
	FiveHour int64 `json:"five_hour"`
	Week     int64 `json:"week"`
	Month    int64 `json:"month"`
	Total    int64 `json:"total"`
}
type Windows struct {
	FiveHour time.Time `json:"five_hour"`
	Week     time.Time `json:"week"`
	Month    time.Time `json:"month"`
	Total    time.Time `json:"total"`
}
type KeySpec struct {
	Label         string     `json:"label"`
	Enabled       bool       `json:"enabled"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxConcurrent int        `json:"max_concurrent"`
	AllowedModels []string   `json:"allowed_models"`
	Limits        Limits     `json:"limits"`
}
type Key struct {
	ID string `json:"id"`
	// Secret is the current plaintext credential, persisted additively for
	// authenticated management display. Legacy hash-only rows leave it empty.
	Secret        string     `json:"secret,omitempty"`
	KeySpec                  // embedded flat JSON
	Used          Limits     `json:"used"`
	Starts        Windows    `json:"starts"`
	Resets        Windows    `json:"resets"`
	Active        int        `json:"active"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
	Revoked       bool       `json:"revoked,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}
type Admission struct {
	RequestID, TraceID, KeyID, Model string
	At                               time.Time
}
type CaptureInput struct {
	RequestID, TraceID, KeyID string
	Headers                   http.Header
	Body                      []byte
	At                        time.Time

	// Queue snapshot fields. They are internal so plugin callers keep using the
	// stable exported shape while the worker can re-check the accepted policy.
	mode           string
	expiresAt      time.Time
	generation     uint64
	bodyTooLarge   bool
	headersTooWide bool
	headersRedact  bool
	originalBytes  int64
}
type Settings struct {
	CaptureEnabled          bool           `json:"capture_enabled"`
	CaptureMode             string         `json:"capture_mode"`
	CaptureRetentionSeconds int            `json:"capture_retention_seconds"`
	MaxBodyBytes            int            `json:"max_body_bytes"`         // default 1MiB
	MaxBodyStorageBytes     int64          `json:"max_body_storage_bytes"` // default 256MiB
	RawRetentionHours       int            `json:"raw_retention_hours"`    // default24, 1..168
	DailyRetentionDays      int            `json:"daily_retention_days"`   // default365, 1..3650
	Pricing                 pricing.Config `json:"pricing"`
}

// Capture modes are explicit; capture_enabled is retained only as a compatible
// projection of the same decision.
const (
	CaptureModeOff            = "off"
	CaptureModeHeaders        = "headers"
	CaptureModeHeadersAndBody = "headers_and_body"
	captureStatusAvailable    = "available"
	captureStatusRedacted     = "redacted"
	captureStatusDisabled     = "disabled"
	captureStatusPending      = "pending"
	captureStatusTooLarge     = "too_large"
	captureStatusUnsupported  = "unsupported"
	captureStatusExpired      = "expired"
	captureStatusEvicted      = "evicted"
	captureStatusPurged       = "purged"
	captureStatusMissing      = "missing"
	captureStatusTruncated    = "truncated" // legacy schema2 display value
	captureReasonOff          = "capture_off"
	captureReasonHeadersOnly  = "headers_only"
	captureReasonBodyTooLarge = "body_too_large"
	captureReasonHeadersLarge = "headers_too_large"
	captureReasonUnsupported  = "unsupported_body"
	captureReasonStorageLimit = "storage_limit"
	captureReasonCapacity     = "capacity"
	captureReasonExpired      = "expired"
	captureReasonPurged       = "purged"
	captureReasonMissing      = "missing"
	captureReasonRedacted     = "redacted"
	captureReasonCorrelation  = "trace_collision"
	captureHeaderLimit        = 16 << 10
	captureEnvelopeVersion    = 1
	supportedObserverFeature  = 3
)

type Health struct {
	DatabaseBytes          int64            `json:"database_bytes"`
	DroppedCaptures        uint64           `json:"dropped_captures"`
	CaptureDrops           map[string]int64 `json:"capture_drops"`
	CountersSince          time.Time        `json:"counters_since"`
	CaptureBytes           int64            `json:"capture_bytes"`
	CaptureLimitBytes      int64            `json:"capture_limit_bytes"`
	ReclaimableBytes       int64            `json:"reclaimable_bytes"`
	PendingUsage           int              `json:"pending_usage"`
	ActiveRequests         int              `json:"active_requests"`
	LastError              string           `json:"last_error,omitempty"`
	MaintenanceError       string           `json:"maintenance_error,omitempty"`
	LastCleanupAt          *time.Time       `json:"last_cleanup_at,omitempty"`
	LastCompactAt          *time.Time       `json:"last_compact_at,omitempty"`
	LastCleanupRecords     int64            `json:"last_cleanup_records"`
	LastCleanupBytes       int64            `json:"last_cleanup_bytes"`
	LastCompactBeforeBytes int64            `json:"last_compact_before_bytes"`
	LastCompactAfterBytes  int64            `json:"last_compact_after_bytes"`
	MaintenanceActive      bool             `json:"maintenance_active"`
}
type Query struct {
	From                                      time.Time `json:"from,omitempty"` // legacy RFC3339 half-open range
	To                                        time.Time `json:"to,omitempty"`
	KeyID, Model, Provider, AuthIndex, Status string    // status empty/success/failed
	Page, PageSize                            int       // 1-based, <=100
}

// ViewQuery is the stable input for RequestView. Date-only fields are inclusive
// calendar dates in Timezone (UTC when omitted). View is one of
// requests/model/key/account/provider.
type ViewQuery struct {
	Query
	FromDate string `json:"from_date,omitempty"` // YYYY-MM-DD in Timezone
	ToDate   string `json:"to_date,omitempty"`   // YYYY-MM-DD, inclusive
	Timezone string `json:"timezone,omitempty"`  // IANA location; empty means UTC
	View     string `json:"view,omitempty"`
}

type FacetQuery struct {
	Field  string `json:"field"`
	Search string `json:"search,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	// Labels is caller-injected display metadata. It never comes from JSON or
	// the ledger and is applied only to auth_index values already observed.
	Labels map[string]string `json:"-"`
}

type FacetItem struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

type FacetPage struct {
	Items      []FacetItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
type Record struct {
	ID                  string                `json:"id"`
	TraceID             string                `json:"trace_id"`
	KeyID               string                `json:"key_id"`
	Time                time.Time             `json:"time"`
	Model               string                `json:"model"`
	Provider            string                `json:"provider"`
	AuthIndex           string                `json:"auth_index"`
	Failed              bool                  `json:"failed"`
	StatusCode          int                   `json:"status_code"`
	Stream              bool                  `json:"stream"`
	LatencyMS           int64                 `json:"latency_ms"`
	TTFTMS              int64                 `json:"ttft_ms"`
	Tokens              pricing.Tokens        `json:"tokens"`
	RawUsage            pluginapi.UsageDetail `json:"raw_usage"`
	ResponseServiceTier string                `json:"response_service_tier,omitempty"`
	CostMicroUSD        int64                 `json:"cost_micro_usd"`
	PriceSource         string                `json:"price_source"`
	AccountingQuality   string                `json:"accounting_quality"`
}
type RequestPage struct {
	Items    []Record `json:"items"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
	HasMore  bool     `json:"has_more"`
}

// Aggregate is flat by contract: the original four fields remain and detailed
// fields are additive. detailed_requests tells callers how many requests have
// observations for the new fields; old history must not be rendered as zero.
type Aggregate struct {
	Requests         int64 `json:"requests"`
	Failures         int64 `json:"failures"`
	CostMicroUSD     int64 `json:"cost_micro_usd"`
	Tokens           int64 `json:"tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CompleteRequests int64 `json:"complete_requests"`
	LatencyMS        int64 `json:"latency_ms"`
	LatencySamples   int64 `json:"latency_samples"`
	TTFTMS           int64 `json:"ttft_ms"`
	TTFTSamples      int64 `json:"ttft_samples"`
	DetailedRequests int64 `json:"detailed_requests"`
}

type Bucket struct {
	Time time.Time `json:"time"`
	Aggregate
}
type Summary struct {
	Aggregate
	Granularity string   `json:"granularity"`
	Buckets     []Bucket `json:"buckets"`
}

type ViewRange struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	AsOf     time.Time `json:"as_of"`
	Timezone string    `json:"timezone"`
}

type Series struct {
	Granularity string   `json:"granularity"`
	Buckets     []Bucket `json:"buckets"`
}

type Group struct {
	Aggregate
	Key       string `json:"key"`
	Label     string `json:"label,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
}

type Pagination struct {
	Page     int  `json:"page"`
	PageSize int  `json:"page_size"`
	HasMore  bool `json:"has_more"`
}

type Coverage struct {
	RawFrom          *time.Time `json:"raw_from"`
	DailyFrom        *time.Time `json:"daily_from"`
	DetailedRequests int64      `json:"detailed_requests"`
	Requests         int64      `json:"requests"`
	BackfillPending  bool       `json:"backfill_pending"`
}

type RequestView struct {
	Range      ViewRange  `json:"range"`
	Summary    Aggregate  `json:"summary"`
	Series     Series     `json:"series"`
	Records    []Record   `json:"records"`
	Groups     []Group    `json:"groups"`
	Pagination Pagination `json:"pagination"`
	Coverage   Coverage   `json:"coverage"`
}
type Detail struct {
	Record          Record          `json:"record"`
	Headers         http.Header     `json:"headers"`
	Body            json.RawMessage `json:"body"`           // null if unavailable
	CaptureStatus   string          `json:"capture_status"` // legacy aggregate status
	HeadersStatus   string          `json:"headers_status"` // independent header state
	BodyStatus      string          `json:"body_status"`    // independent body state
	CaptureReason   string          `json:"capture_reason,omitempty"`
	CapturedAt      *time.Time      `json:"captured_at,omitempty"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	OriginalBytes   int64           `json:"original_bytes"`
	CompressedBytes int64           `json:"compressed_bytes"`
	// PriceQuote is an immutable deep copy of the quote saved with the record.
	PriceQuote pricing.Quote `json:"price_quote"`
}
type CleanupResult struct {
	RemovedRecords int64 `json:"removed_records"`
	RemovedBytes   int64 `json:"removed_bytes"`
}

var (
	ErrNotFound     = errors.New("notfound")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrExpired      = errors.New("expired")
	ErrQuota        = errors.New("quota")
	ErrConcurrency  = errors.New("concurrency")
	ErrAccounting   = errors.New("accounting")
	ErrClosed       = errors.New("closed")
	ErrInvalid      = errors.New("invalid")
	ErrBusy         = errors.New("busy")
	ErrConflict     = errors.New("conflict")
	ErrDeleted      = errors.New("deleted")
	ErrUnsupported  = errors.New("unsupported")
	ErrMaintenance  = errors.New("maintenance")
)
