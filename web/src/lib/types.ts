/**
 * Core type definitions for Observer v0.7.0.
 *
 * Matches Go backend API JSON structures directly.
 * Field names must NOT be renamed.
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
}

export interface Rates {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning?: number | null;
}

export interface Price {
  model: string;
  provider?: string;
  rates: Rates;
  source: string;
}

export interface Rule {
  id: string;
  model: string;
  input_tokens_gt?: number | null;
  time_range?: string | null;
  rates: Rates;
}

export interface PricingConfig {
  auto: boolean;
  prices: Price[];
  rules: Rule[];
}

export interface Settings {
  capture_enabled: boolean;
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

export interface SummaryBucket {
  time: string;
  requests: number;
  failures: number;
  cost_micro_usd: number;
  tokens: number;
}

export interface Summary {
  requests: number;
  failures: number;
  cost_micro_usd: number;
  tokens: number;
  granularity: string;
  buckets: SummaryBucket[];
}

export interface Detail {
  record: RequestRecord;
  headers: Record<string, string[]>;
  body: unknown;
  capture_status: string;
}

export interface ModelInfo {
  id: string;
  owned_by?: string;
}
