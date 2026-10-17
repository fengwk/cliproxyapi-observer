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
	ID            string    `json:"id"`
	KeySpec                 // embedded flat JSON
	Used          Limits    `json:"used"`
	Starts        Windows   `json:"starts"`
	Resets        Windows   `json:"resets"`
	Active        int       `json:"active"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
}
type Settings struct {
	CaptureEnabled      bool           `json:"capture_enabled"`
	MaxBodyBytes        int            `json:"max_body_bytes"`         // default 1MiB
	MaxBodyStorageBytes int64          `json:"max_body_storage_bytes"` // default 256MiB
	RawRetentionHours   int            `json:"raw_retention_hours"`    // default24, 1..168
	DailyRetentionDays  int            `json:"daily_retention_days"`   // default365, 1..3650
	Pricing             pricing.Config `json:"pricing"`
}
type Health struct {
	DatabaseBytes   int64  `json:"database_bytes"`
	DroppedCaptures uint64 `json:"dropped_captures"`
	PendingUsage    int    `json:"pending_usage"`
	ActiveRequests  int    `json:"active_requests"`
	LastError       string `json:"last_error,omitempty"`
}
type Query struct {
	From, To                                  time.Time
	KeyID, Model, Provider, AuthIndex, Status string // status empty/success/failed
	Page, PageSize                            int    // 1-based, <=100
}
type Record struct {
	ID                string                `json:"id"`
	TraceID           string                `json:"trace_id"`
	KeyID             string                `json:"key_id"`
	Time              time.Time             `json:"time"`
	Model             string                `json:"model"`
	Provider          string                `json:"provider"`
	AuthIndex         string                `json:"auth_index"`
	Failed            bool                  `json:"failed"`
	StatusCode        int                   `json:"status_code"`
	Stream            bool                  `json:"stream"`
	LatencyMS         int64                 `json:"latency_ms"`
	TTFTMS            int64                 `json:"ttft_ms"`
	Tokens            pricing.Tokens        `json:"tokens"`
	RawUsage          pluginapi.UsageDetail `json:"raw_usage"`
	CostMicroUSD      int64                 `json:"cost_micro_usd"`
	PriceSource       string                `json:"price_source"`
	RuleID            string                `json:"rule_id,omitempty"`
	AccountingQuality string                `json:"accounting_quality"`
}
type RequestPage struct {
	Items    []Record `json:"items"`
	Page     int      `json:"page"`
	PageSize int      `json:"page_size"`
	HasMore  bool     `json:"has_more"`
}
type Bucket struct {
	Time         time.Time `json:"time"`
	Requests     int64     `json:"requests"`
	Failures     int64     `json:"failures"`
	CostMicroUSD int64     `json:"cost_micro_usd"`
	Tokens       int64     `json:"tokens"`
}
type Summary struct {
	Requests     int64    `json:"requests"`
	Failures     int64    `json:"failures"`
	CostMicroUSD int64    `json:"cost_micro_usd"`
	Tokens       int64    `json:"tokens"`
	Granularity  string   `json:"granularity"`
	Buckets      []Bucket `json:"buckets"`
}
type Detail struct {
	Record        Record          `json:"record"`
	Headers       http.Header     `json:"headers"`
	Body          json.RawMessage `json:"body"`           // null if unavailable
	CaptureStatus string          `json:"capture_status"` // available/disabled/expired/missing/truncated/redacted
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
)
