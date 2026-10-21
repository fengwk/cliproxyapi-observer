# Observer: core implementation and integration contract

The v0.6.0 baseline replaced the former observation-only backend. No migration
from pre-v0.6 databases or credentials. Subsequent schema2 releases preserve
their own databases, keys and fixed costs. Current additions and superseding
auth/UI/API contracts: [reconstruction guide](docs/reconstruction.md) and
[requirements](docs/REFACTOR-next.md). Code examples below describe the original
core shape; additive fields are documented in the current guide. Default database:
`data/cliproxyapi-observer-v2.db`. Reject nonempty databases without this schema
marker without changing them. CPA core, upstream selection, routing, retries and
session affinity remain untouched. SDK baseline v8.0.30, schema 6 minimum.

## Ownership

- `internal/engine/**`: storage/Key/auth/admission/accounting/capture/rollups/
  retention/physical compaction and their tests (core Coder).
- `internal/pricing/**`: catalog matching, immutable quote calculation, rules,
  cache/refresh and their tests (pricing Coder).
- `internal/plugin/**`, main.go: native lifecycle, authentication and RPC/HTTP
  wiring (plugin Coder).
- `internal/web/**`: three-page UI, embedded assets, Node and automated actual
  Chrome screenshot matrix (frontend Coder).
- `tests/integration/**`: new reusable real-host acceptance (acceptance Coder).
- Parent: contract, SDK/build/CI/docs/release, full code review, fixes, integration
  and final acceptance. Do not write another slice's files.

## Units and semantics

- Money and rates are signed int64 **micro-USD**; rates are per million tokens.
  JSON numeric inputs are nonnegative safe integers <= 9_000_000_000_000_000.
  Checked integer arithmetic, no floating point billing. Ceil once per execution
  to whole micro-USD after summing buckets. USD display divides by 1e6.
- Zero limits mean unlimited. Empty allowed_models means unrestricted. Exact
  requested model matching, preserve full model identifiers in records.
- A Key starts 5h/week/month cycles at creation; on admission expired windows
  restart at current time, week = 7*24h, month = UTC AddDate(0,1,0) calendar month
  with last-day clamping, not 30 days. Total has no automatic reset.
- Reset selected windows to now/zero; it must never clear an accounting failure
  unless all pending events are durably stored. Usage may overshoot after an
  admitted concurrent request; no speculative reservation/precharge.
- Authentication principal = `observer:<key ID>`. SHA-256 hashes authenticate;
  the user-authorized current client secret also persists for repeated internal
  management display/copy. Revoke/delete clear both, disable retains them.
  Stable IDs are not credentials. Feature marker 2 prevents feature-1 writers.
- RequestInterceptRequest.RequestID is the admission ID; UsageRecord.RequestID
  is an execution ID (retries can have multiple IDs). TraceID correlates them.
  Charge every distinct execution once; replay never charges twice. Key cycles
  charge by ingestion time (current usage); historical record day by RequestedAt.
- Ignore non-generation/count-tokens usage. Do not admit/reserve count-tokens
  requests, but still enforce enabled/expiry/model authorization.
- Preserve raw SDK counters and canonical normalized token buckets. Use the
  pinned CPA token helper for known providers; unknown semantics must not
  invent overlapping buckets. Missing price costs zero, never blocks inference.
- Capture is opt-in, queued, bounded, gzip, expires on reads. Store only the
  client inbound, pre-auth header copy plus recursively redacted JSON; never
  upstream-sent headers, upstream credentials or failure bodies. Preserve every
  non-credential header name and multi-value with case-insensitive exclusion of
  auth/cookie/API-key/token/secret/password/credential/private-key fields.
  Header names and retained values share a 16KiB cap; overflow discards the
  whole group as too_large, while excluded credential headers consume no budget.
  Disabled capture is explicit, not an empty successful body.
- Usage write failure: retain the already quoted event for bounded retry, block
  subsequent admissions for the affected Key, expose health; recovery retries
  the same quote. Never silently drop money or reprice a retried event.
- Daily rollup retention cannot erase independent Key used amounts. Raw detail
  default 24h; daily default 365d. Cleanup plus physical bbolt compaction.
- UTC, half-open [from,to) ranges. Summary must state bucket granularity.
  For historical dates with only daily rollups, reject partial-day boundaries
  instead of silently fabricating exact totals. UI offers whole-date queries.

## Pricing public API (package pricing)

Implement these exact names. Add private helpers freely; do not add dependencies.

```go
type Rates struct {
    Input int64 `json:"input"`
    Output int64 `json:"output"`
    CacheRead int64 `json:"cache_read"`
    CacheWrite int64 `json:"cache_write"`
    Reasoning *int64 `json:"reasoning,omitempty"`
}
type Price struct {
    Model string `json:"model"`
    Rates Rates `json:"rates"`
    Source string `json:"source"`
}
type Rule struct {
    ID string `json:"id"`
    Model string `json:"model"`
    InputTokensGT *int64 `json:"input_tokens_gt,omitempty"`
    TimeRange string `json:"time_range,omitempty"` // "HH:mm-HH:mm", UTC
    Rates Rates `json:"rates"`
}
type Config struct {
    Auto bool `json:"auto"`
    Prices []Price `json:"prices"` // manual exact overrides only
    Rules []Rule `json:"rules"`   // ordered first-match overrides
}
type Tokens struct {
    Input int64 `json:"input"` // uncached, canonical, disjoint from read/write
    Output int64 `json:"output"` // includes reasoning
    CacheRead int64 `json:"cache_read"`
    CacheWrite int64 `json:"cache_write"`
    Reasoning int64 `json:"reasoning"` // subset of Output
    Total int64 `json:"total"`
}
type Quote struct {
    CostMicroUSD int64 `json:"cost_micro_usd"`
    Source string `json:"source"` // rule/manual/auto/unpriced
    RuleID string `json:"rule_id,omitempty"`
    Rates Rates `json:"rates"`
}
type Snapshot struct {
    Prices []Price `json:"prices"` // effective observed + manual entries
    LastSync time.Time `json:"last_sync"`
    Error string `json:"error,omitempty"` // sanitized, no response body
    Refreshing bool `json:"refreshing"`
}
type Pricer interface {
    Quote(model, provider string, t Tokens, at time.Time) (Quote,error)
    Observe(model, provider string)
    Configure(Config) error
    Snapshot() Snapshot
    Refresh(context.Context) error
}
func DefaultConfig() Config // Auto true, nonnil empty arrays
func Validate(Config) error
func NewService(Config, string /* cache file */, *http.Client) (*Service,error)
func (*Service) Run(context.Context) // one worker, refresh <=6h, cancelable
// Service implements Pricer. Quote never performs network.
```

Catalog: trusted HTTPS models.dev whole catalog, provider-aware exact match,
conservative explicit recognized prefix/suffix normalization, never merge
ambiguous model identities. Atomic bounded public-data cache, last good cache
on network errors. Avoid per-request retry storms for misses. Rules > manual >
auto > zero; quote records rate/source at calculation time.

## Engine public API (package engine)

```go
type Options struct { DataPath string; Now func() time.Time; Pricer pricing.Pricer }
func Open(Options) (*Store,error) // nil Pricer invalid; do not own worker
type Limits struct {
    FiveHour int64 `json:"five_hour"`; Week int64 `json:"week"`
    Month int64 `json:"month"`; Total int64 `json:"total"`
}
type Windows struct {
    FiveHour time.Time `json:"five_hour"`; Week time.Time `json:"week"`
    Month time.Time `json:"month"`; Total time.Time `json:"total"`
}
type KeySpec struct {
    Label string `json:"label"`; Enabled bool `json:"enabled"`
    ExpiresAt *time.Time `json:"expires_at,omitempty"`
    MaxConcurrent int `json:"max_concurrent"`; AllowedModels []string `json:"allowed_models"`
    Limits Limits `json:"limits"`
}
type Key struct {
    ID string `json:"id"`; KeySpec // embedded flat JSON
    Secret string `json:"secret,omitempty"` // authenticated management display
    Used Limits `json:"used"`; Starts Windows `json:"starts"`; Resets Windows `json:"resets"`
    Active int `json:"active"`; BlockedReason string `json:"blocked_reason,omitempty"`
    CreatedAt time.Time `json:"created_at"`
}
type Admission struct {
    RequestID, TraceID, KeyID, Model string
    At time.Time
}
type CaptureInput struct {
    RequestID, TraceID, KeyID string
    Headers http.Header; Body []byte; At time.Time
}
func (*Store) CreateKey(KeySpec) (Key,string,error)
func (*Store) UpdateKey(string,KeySpec) (Key,error)
func (*Store) RotateKey(string) (Key,string,error)
func (*Store) RevokeKey(string) error // disable + invalidate secret, retain history
func (*Store) ResetKey(string,[]string) (Key,error) // five_hour/week/month/total
func (*Store) RecoverKey(string,bool /* acknowledge unknown usage */) (Key,error)
func (*Store) Keys() ([]Key,error)
func (*Store) Authenticate(string) (Key,error) // enabled + expiry only
func (*Store) Authorize(keyID, model string) error // enabled/expiry/model, no reservation
func (*Store) Admit(Admission) error // idempotent, checks limits+concurrency
func (*Store) Complete(requestID string, at time.Time) error // idempotent slot release
func (*Store) RecordUsage(pluginapi.UsageRecord) error // APIKey=observer:<id>
func (*Store) Capture(CaptureInput) bool
func (*Store) Maintenance() error // retry billing, expiry, opportunistic compaction
func (*Store) Compact() error // explicit physical compaction
func (*Store) Close() error // drain diagnostics; no silently discarded accounting
type Settings struct {
    CaptureEnabled bool `json:"capture_enabled"`
    MaxBodyBytes int `json:"max_body_bytes"` // default 1MiB
    MaxBodyStorageBytes int64 `json:"max_body_storage_bytes"` // default 256MiB
    RawRetentionHours int `json:"raw_retention_hours"` // default24, 1..168
    DailyRetentionDays int `json:"daily_retention_days"` // default365, 1..3650
    Pricing pricing.Config `json:"pricing"`
}
func DefaultSettings() Settings
func (*Store) Settings() (Settings,error)
func (*Store) UpdateSettings(Settings) (Settings,error)
type Health struct {
    DatabaseBytes int64 `json:"database_bytes"`
    DroppedCaptures uint64 `json:"dropped_captures"`
    PendingUsage int `json:"pending_usage"`
    ActiveRequests int `json:"active_requests"`
    LastError string `json:"last_error,omitempty"`
}
func (*Store) Health() Health
type Query struct {
    From,To time.Time
    KeyID,Model,Provider,AuthIndex,Status string // status empty/success/failed
    Page,PageSize int // 1-based, <=100
}
type Record struct {
    ID string `json:"id"`; TraceID string `json:"trace_id"`; KeyID string `json:"key_id"`
    Time time.Time `json:"time"`; Model string `json:"model"`; Provider string `json:"provider"`
    AuthIndex string `json:"auth_index"`; Failed bool `json:"failed"`; StatusCode int `json:"status_code"`
    Stream bool `json:"stream"`; LatencyMS int64 `json:"latency_ms"`; TTFTMS int64 `json:"ttft_ms"`
    Tokens pricing.Tokens `json:"tokens"`; RawUsage pluginapi.UsageDetail `json:"raw_usage"`
    CostMicroUSD int64 `json:"cost_micro_usd"`; PriceSource string `json:"price_source"`
    RuleID string `json:"rule_id,omitempty"`; AccountingQuality string `json:"accounting_quality"`
}
type RequestPage struct {
    Items []Record `json:"items"`; Page int `json:"page"`; PageSize int `json:"page_size"`
    HasMore bool `json:"has_more"`
}
type Bucket struct {
    Time time.Time `json:"time"`; Requests int64 `json:"requests"`; Failures int64 `json:"failures"`
    CostMicroUSD int64 `json:"cost_micro_usd"`; Tokens int64 `json:"tokens"`
}
type Summary struct {
    Requests int64 `json:"requests"`; Failures int64 `json:"failures"`
    CostMicroUSD int64 `json:"cost_micro_usd"`; Tokens int64 `json:"tokens"`
    Granularity string `json:"granularity"`; Buckets []Bucket `json:"buckets"`
}
type Detail struct {
    Record Record `json:"record"`; Headers http.Header `json:"headers"`
    Body json.RawMessage `json:"body"` // null if unavailable
    CaptureStatus string `json:"capture_status"` // available/disabled/expired/missing/truncated/redacted
}
func (*Store) Requests(Query) (RequestPage,error)
func (*Store) Summary(Query) (Summary,error)
func (*Store) Detail(string) (Detail,error)
```

Export `ErrNotFound`, `ErrUnauthorized`, `ErrForbidden`, `ErrExpired`,
`ErrQuota`, `ErrConcurrency`, `ErrAccounting`, `ErrClosed`, `ErrInvalid`,
`ErrBusy` for errors.Is. New private wrapped errors must not leak secrets.

## Management HTTP API (owned by plugin Coder)

Base `/v0/management/plugins/cliproxyapi-observer`, host management auth.
Standard direct JSON response (not RPC envelope) and errors
`{"error":{"code":"...","message":"..."}}`.

- GET `/health` -> Health plus `version`, `schema_version:2`
- POST `/models/token` -> `{"token":"...","expires_at":"..."}` (single-use catalog-only capability)
- GET `/keys` -> `{"items":[Key...]}`; POST `/keys` KeySpec ->201 `{"key":Key,"secret":"..."}`
- PUT `/keys?id={id}` KeySpec -> Key; DELETE `/keys?id={id}` ->204 (revoke)
- POST `/keys/rotate?id={id}` -> `{"key":Key,"secret":"..."}`
- POST `/keys/reset?id={id}` `{"windows":["five_hour",...]}` ->Key
- POST `/keys/recover?id={id}` `{"acknowledge":true}` ->Key (see safety below)
- GET `/requests` ->RequestPage, GET `/requests/detail?id={id}` ->Detail
- GET `/summary` ->Summary
- GET/PUT `/settings` ->Settings; GET `/prices` ->pricing.Snapshot
- POST `/prices/refresh` ->Snapshot, POST `/compact` ->Health
- GET `/ui` (trailing slash supported), `/ui.js`, `/ui.css` ->embedded assets.
  Same assets via resource base `/v0/resource/plugins/cliproxyapi-observer/`.

Query params: from/to RFC3339, key_id/model/provider/auth_index/status,
page (default1)/page_size(default25,max100). Reject malformed/unknown inputs.
Default from UTC today midnight, to now. Reject unsupported methods.
Input JSON limited 1MiB, reject trailing values/unknown fields. No CORS.
Cache-Control:no-store; body/header details only fetched on row click.
Management login uses Observer localStorage only with explicit remember-login
consent, otherwise sessionStorage/in-memory. Logout/definite structured 401 clears
both; never read host credentials. Don't require a second client Key for manager.
Resource shell may be public, but all data authenticated.

Plugin frontend auth extracts bearer/x-api-key/x-goog-api-key and supported
query key/auth_token, disallows conflicting material. Authentication is
nonexclusive and only recognizes finite supported operations. Native/unknown
scopes pass through without touching Observer storage. Authenticated owned
generation fails closed across reconfiguration/store failures and enforces exact
requested-model permissions, quota and concurrency. Unsupported Observer routes
stay unhandled; no global model-surface classifier or native execution denylist.
CPA Realtime tokens remain host-owned; Observer catalog tokens remain one-use,
GET-directory-only capabilities. See docs/model-policy.md for current scope.
Anonymous/no-native-provider and duplicate/mixed-key limitations remain explicit.
An unloaded plugin cannot enforce its own authentication or admission.
Before-interceptor validates principal metadata, makes admission once;
after-interceptor no-op. Complete releases by RequestID. Do not mutate or
return incoming headers/body. Correct lifecycle fence/failed-reconfigure restore.
Refuse unsafe schema<6 rather than silently downgrading security.

## UI and reusable acceptance

Only 请求日志 / Key 管理 / 设置. White/light/dark/auto neutral CPA colors and
typography; no colored card top borders. Full available iframe width. Only the
Requests page owns refresh (off/5(default)/10/30s); Keys/Settings never poll.
Avoid overlapping fetches; pause during reading/editing and on hidden pages;
stop on 401. One explicit filter query button. Request/failure trend
SVG plus readable table, currency `used/limit$`, expiry, concurrency, model limits,
cycle reset timestamps. Safe text-only JSON/header rendering, native dialogs,
fully formatted JSON. Rule/manual rates editors use USD per million display,
persist integers; unknown rates show em-dash, known free rates show zero.
Settings: capture modes/independent TTL, explainable pricing and mappings,
expired cleanup, confirmed content purge and physical compaction. No fourth page.

Node unit tests and real Chrome automation, with persistent screenshot reports
outside worktree before workspace release. Desktop1280 and mobile390, light/dark
and auto, all pages, key form and request details, fake local data only.
Integration tests must load actual c-shared plugin into real CPA .30 and .32
hosts, use local mock upstream, prove strict auth, correct request bytes, fixed
prices, limits, reset, concurrency slot release, stream/cancel/count-tokens,
filtered logs/details/redaction, settings and persisted restart behavior.
Document commands; tests comment their intent. No manual-only validation.

## Review amendments (binding)

The SDK management router only matches registered static paths. The query-ID
routes above supersede the originally planned dynamic paths; do not retain dead
compatibility routes. Register the trailing `/ui/` route as well as `/ui`.

Key model selection: default all; explicit searchable checkbox multi-select
restricts to selected exact requested IDs. Real .30 acceptance proved config
credentials are not reliably listed by auth-files, so discover the authoritative
host `/v1/models` instead. A management-authenticated POST `/models/token` issues
a random >=256-bit single-use token valid30s, bounded32 outstanding capabilities.
That token authenticates GET `/v1/models` ONLY, not inference, count-tokens,
management or any other route. Use it in an Authorization header, never a URL or
storage. UI fetches it once when catalog is needed and immediately fetches the
same-origin `/v1/models`; no second user credential. Cache IDs5min, preserve
unavailable selected IDs, surface errors and allow manual exact IDs. No pricing
catalog or config/credential JSON downloads as availability sources. Failed
discovery never makes an existing restricted Key unrestricted.

Per-Key recovery: retained pending billing retries automatically with original
quote. For a crash or irrecoverably lost event, persist a Key-scoped blocked
reason; do not permanently poison a whole database including future new Keys.
An administrator may explicitly acknowledge unknown/missing usage via RecoverKey
only after retries have settled and storage works. Expose a clearly warned
confirmation in UI; reset/rotate must never implicitly acknowledge it. Recovery
does not change counters, historical costs or limits. Close must release handles
even on debt, and a failed close must not leave the plugin holding a closed Store.

Bound all event-derived state: completed admissions/correlations expire, dedup
has a documented replay horizon (7d) with durable monotonic cutoff so expired
replays cannot be billed again. A too-old unseen event must fail closed for its
Key, not silently recharge it. Max captured body <=8MiB; queue and storage are
bounded. Compress request metadata/details as well as bodies. Short ranges fully
covered by raw retention return useful hourly trends; older whole-day ranges use
daily buckets. Do not count full daily rollups into an hourly chart.

Pricing rule input_tokens_gt uses full prompt context (uncached+read+write),
not only uncached billable input. Expose optional provider in price snapshots
when the same model has provider-specific rates, with no effect on model-wide
manual overrides.

The pinned host has no guaranteed usage-queue drain before hot replacement or
shutdown. A callback delivered to a detached store or an unexpected accounting
panic must persist a Key-scoped interruption marker outside the closed bbolt
handle. Every new Store/Open/admission/list reads it; only explicit recovery may
clear it. Use a small atomic marker and file lock for recovery races, not another
billing database. `engine.RecordInterruption(dataPath,keyID string) error` is the
shared entry point, safe across different native-library runtimes. Source events
never delivered by CPA cannot be reconstructed; document this boundary and don't
claim exactly-once delivery from a non-durable host queue.

Gemini countTokens: before-hook receives a wildcard path, but frontend auth has
the original URL.Path. Use a distinct stable nonbilling principal/caller_scope
for count-token authentication, preserve normal generation principal, and carry
the operation in the scope lookup. Do not guess from a potentially identical body
or mutate CPA headers/body/path/session.
