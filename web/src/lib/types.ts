/**
 * Core type definitions for Observer.
 *
 * Matches Go backend API JSON structures directly.
 * Field names must NOT be renamed.
 * All new fields are optional for backwards compatibility with historical fixtures.
 */

export interface Limits {
  five_hour: number;
  week: number;
  month: number;
  total: number;
}

export interface Windows {
  five_hour: string;
  week: string;
  month: string;
  total: string;
}

export interface KeySpec {
  label: string;
  enabled: boolean;
  expires_at?: string | null;
  max_concurrent: number;
  allowed_models: string[];
  limits: Limits;
  secret?: string;
}

export interface Key {
  id: string;
  label: string;
  enabled: boolean;
  expires_at?: string | null;
  max_concurrent: number;
  allowed_models: string[];
  limits: Limits;
  used: Limits;
  starts: Windows;
  resets: Windows;
  active: number;
  blocked_reason?: string;
  created_at: string;
  deleted_at?: string | null;
  revoked?: boolean;
}

export interface Rates {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning?: number | null;
}

export interface PriceCandidate {
  provider: string;
  model: string;
  rates?: Rates;
  missing_rates?: string[];
}

export interface Price {
  model: string;
  provider?: string;
  rates: Rates;
  source: string;
  reason?: string;
  match?: string;
  catalog_provider?: string;
  catalog_model?: string;
  catalog_time?: string;
  missing_rates?: string[];
  candidates?: PriceCandidate[];
}

export interface Rule {
  id: string;
  model: string;
  input_tokens_gt?: number | null;
  time_range?: string | null;
  rates: Rates;
}

export interface ModelMapping {
  model: string;
  provider: string;
  catalog_model: string;
}

export interface PricingConfig {
  auto: boolean;
  prices: Price[];
  rules: Rule[];
  mappings?: ModelMapping[];
}

export type CaptureMode = 'off' | 'headers' | 'headers_and_body';

export interface Settings {
  capture_enabled: boolean;
  capture_mode?: CaptureMode;
  capture_retention_seconds?: number;
  max_body_bytes: number;
  max_body_storage_bytes: number;
  raw_retention_hours: number;
  daily_retention_days: number;
  pricing: PricingConfig;
}

export interface PriceSnapshot {
  prices: Price[];
  last_sync: string;
  error?: string;
  refreshing: boolean;
  catalog_entries?: number;
}

export interface PricePreviewItem {
  model: string;
  provider?: string;
  rates: Rates;
  source: string;
  has_rules: boolean;
  reason?: string;
  match?: string;
  catalog_provider?: string;
  catalog_model?: string;
  catalog_time?: string;
  missing_rates?: string[];
  candidates?: PriceCandidate[];
}

export interface PricePreview {
  items: PricePreviewItem[];
}

export interface Health {
  database_bytes: number;
  dropped_captures: number;
  pending_usage: number;
  active_requests: number;
  last_error?: string;
  capture_bytes?: number;
  capture_limit_bytes?: number;
  reclaimable_bytes?: number;
  capture_drops?: Record<string, number>;
  counters_since?: string;
  maintenance_error?: string;
  last_cleanup_at?: string;
  last_compact_at?: string;
  last_cleanup_records?: number;
  last_cleanup_bytes?: number;
  last_compact_before_bytes?: number;
  last_compact_after_bytes?: number;
  maintenance_active?: boolean;
}

export interface CleanupResult {
  removed_records: number;
  removed_bytes: number;
}

export interface RequestTokens {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning: number;
  total: number;
}

export interface RequestRecord {
  id: string;
  trace_id: string;
  key_id: string;
  time: string;
  model: string;
  provider: string;
  auth_index: string;
  failed: boolean;
  status_code: number;
  stream: boolean;
  latency_ms: number;
  ttft_ms: number;
  tokens: RequestTokens;
  raw_usage?: unknown;
  cost_micro_usd: number;
  price_source: string;
  rule_id?: string;
  accounting_quality: string;
}

export interface RequestPage {
  items: RequestRecord[];
  page: number;
  page_size: number;
  has_more: boolean;
}

export interface Aggregate {
  requests: number;
  failures: number;
  cost_micro_usd: number;
  tokens: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  reasoning_tokens?: number;
  complete_requests?: number;
  latency_ms?: number;
  latency_samples?: number;
  ttft_ms?: number;
  ttft_samples?: number;
  unpriced_requests?: number;
  detailed_requests?: number;
}

export interface SummaryBucket extends Aggregate {
  time: string;
}

export interface Summary extends Aggregate {
  granularity: string;
  buckets: SummaryBucket[];
}

export interface Detail {
  record: RequestRecord;
  headers: Record<string, string[]>;
  body: unknown;
  capture_status: string;
  headers_status?: string;
  body_status?: string;
  capture_reason?: string;
  captured_at?: string;
  expires_at?: string;
  original_bytes?: number;
  compressed_bytes?: number;
}

export interface ModelInfo {
  id: string;
  owned_by?: string;
}

export interface RequestViewRange {
  from: string;
  to: string;
  as_of: string;
  timezone: 'UTC';
}

export interface RequestViewGroup extends Aggregate {
  key: string;
  label?: string;
  model?: string;
  provider?: string;
  key_id?: string;
  auth_index?: string;
}

export interface RequestViewPagination {
  page: number;
  page_size: number;
  has_more: boolean;
}

export interface RequestViewCoverage {
  raw_from?: string;
  daily_from?: string;
  detailed_requests?: number;
  requests?: number;
  backfill_pending: boolean;
}

export interface RequestView {
  range: RequestViewRange;
  summary: Aggregate;
  series: {
    granularity: 'hour' | 'day';
    buckets: SummaryBucket[];
  };
  records?: RequestRecord[];
  groups?: RequestViewGroup[];
  pagination: RequestViewPagination;
  coverage: RequestViewCoverage;
}

export interface FacetItem {
  value: string;
  label?: string;
}

export interface FacetPage {
  items: FacetItem[];
  next_cursor?: string;
}
