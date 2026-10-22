import type { Key } from '../lib/types';
import { formatUSD, formatUTCOffset, getBrowserTimeZone } from '../lib/format';

export interface FilterState {
  from: string;
  to: string;
  key_id: string;
  model: string;
  provider: string;
  auth_index: string;
  status: '' | 'success' | 'failed';
}

export type PresetRange = 'today' | 'last7days' | 'last30days' | 'custom';
export type ViewTab = 'requests' | 'model' | 'key' | 'key_model' | 'account' | 'provider';

/**
 * Returns the browser-local date as YYYY-MM-DD.
 */
export function getTodayLocalDateString(now: Date = new Date()): string {
  const year = now.getFullYear();
  const month = String(now.getMonth() + 1).padStart(2, '0');
  const day = String(now.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Returns the date N local calendar days before today as YYYY-MM-DD.
 * Uses calendar setters rather than fixed 24-hour arithmetic so DST days keep
 * the same wall-clock date boundary.
 */
export function getPastLocalDateString(daysAgo: number, now: Date = new Date()): string {
  const d = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  d.setDate(d.getDate() - daysAgo);
  return getTodayLocalDateString(d);
}

/**
 * Returns inclusive preset range in the browser-local calendar:
 * - today: [today, today]
 * - last7days: [6 days ago, today] (covers 7 inclusive days)
 * - last30days: [29 days ago, today] (covers 30 inclusive days)
 */
export function getPresetRange(preset: PresetRange, now: Date = new Date()): { from: string; to: string } {
  const today = getTodayLocalDateString(now);
  if (preset === 'today') {
    return { from: today, to: today };
  }
  if (preset === 'last7days') {
    return { from: getPastLocalDateString(6, now), to: today };
  }
  if (preset === 'last30days') {
    return { from: getPastLocalDateString(29, now), to: today };
  }
  return { from: today, to: today };
}

/**
 * Returns whether the query range is currently dynamic (ending on local today).
 * Historical queries ending in the past pause auto-refresh.
 */
export function isDynamicRange(toDate: string, now: Date = new Date()): boolean {
  if (!toDate) return true;
  const today = getTodayLocalDateString(now);
  return toDate.slice(0, 10) === today;
}

/**
 * Builds URLSearchParams query string for GET /requests/view.
 * Dates are inclusive browser-local calendar dates (YYYY-MM-DD); `timezone`
 * tells the backend which local calendar interprets them.
 */
export function buildRequestViewParams(
  filters: FilterState,
  view: ViewTab,
  page: number,
  pageSize = 25,
  now: Date = new Date(),
  timeZone: string = getBrowserTimeZone()
): string {
  const params = new URLSearchParams();
  const today = getTodayLocalDateString(now);
  const fromVal = filters.from.trim() ? filters.from.trim().slice(0, 10) : today;
  const toVal = filters.to.trim() ? filters.to.trim().slice(0, 10) : today;

  params.set('from_date', fromVal);
  params.set('to_date', toVal);
  params.set('timezone', timeZone || 'UTC');
  params.set('view', view);
  params.set('page', String(page));
  params.set('page_size', String(pageSize));

  if (filters.key_id) params.set('key_id', filters.key_id);
  if (filters.model) params.set('model', filters.model);
  if (filters.provider) params.set('provider', filters.provider);
  if (filters.auth_index) params.set('auth_index', filters.auth_index);
  if (filters.status) params.set('status', filters.status);

  return params.toString();
}

/**
 * Snaps range to whole local calendar dates for error recovery.
 */
export function snapToWholeDateRange(
  fromVal: string,
  toVal: string,
  now: Date = new Date()
): { from: string; to: string } {
  const today = getTodayLocalDateString(now);
  let cleanFrom = fromVal ? fromVal.slice(0, 10) : today;
  if (!/^\d{4}-\d{2}-\d{2}$/.test(cleanFrom)) {
    cleanFrom = today;
  }
  let cleanTo = toVal ? toVal.slice(0, 10) : today;
  if (!cleanTo || !/^\d{4}-\d{2}-\d{2}$/.test(cleanTo)) {
    cleanTo = today;
  }
  return { from: cleanFrom, to: cleanTo };
}

/**
 * Detects whether a query error specifically relates to historical dates.
 */
export function isHistoricalDateQueryError(
  errorMsg: string | null | undefined,
  fromVal: string,
  toVal: string
): boolean {
  if (!errorMsg) return false;
  const lower = errorMsg.toLowerCase();
  if (
    lower.includes('401') ||
    lower.includes('unauthorized') ||
    lower.includes('network') ||
    lower.includes('failed to fetch')
  ) {
    return false;
  }
  if (lower.includes('historical') || lower.includes('whole date') || lower.includes('整日')) {
    return true;
  }
  const isQueryInvalid =
    lower.includes('invalid') || lower.includes('date') || lower.includes('range') || lower.includes('400');
  if (!isQueryInvalid) return false;
  const today = getTodayLocalDateString();
  const fromDay = fromVal ? fromVal.slice(0, 10) : today;
  return fromDay < today || fromVal.includes('T') || toVal.includes('T');
}

/**
 * Capture status honest user copy mapping.
 */
export const CAPTURE_STATUS_MAP: Record<
  string,
  { label: string; tone: 'neutral' | 'success' | 'warning' | 'danger' }
> = {
  available: { label: '可用', tone: 'neutral' },
  redacted: { label: '已脱敏', tone: 'neutral' },
  disabled: { label: '采集未开启 (默认)', tone: 'warning' },
  pending: { label: '采集中', tone: 'neutral' },
  too_large: { label: '正文过大未保存', tone: 'warning' },
  unsupported: { label: '格式不支持未保存', tone: 'warning' },
  expired: { label: '已过期已清理', tone: 'neutral' },
  evicted: { label: '容量淘汰', tone: 'warning' },
  purged: { label: '已清空', tone: 'neutral' },
  missing: { label: '未捕获', tone: 'warning' },
  truncated: { label: '超限未保存', tone: 'warning' },
};

export const CAPTURE_STATUS_LABELS: Record<string, string> = {
  available: '可用',
  redacted: '已脱敏',
  disabled: '采集未开启',
  pending: '采集中',
  too_large: '过大未保存',
  unsupported: '不支持格式',
  expired: '已过期',
  evicted: '容量淘汰',
  purged: '已清空',
  missing: '未捕获',
  truncated: '超限未保存',
};

/**
 * Pricing source human label mapping.
 */
export function formatPriceSource(source?: string): { label: string; isWarning: boolean } {
  if (!source) {
    return { label: '—', isWarning: false };
  }
  if (source === 'zero') {
    return { label: '零价格（按 $0 计费）', isWarning: false };
  }
  if (source === 'manual') {
    return { label: '手工价格', isWarning: false };
  }
  if (source === 'catalog' || source === 'auto') {
    return { label: '自动目录', isWarning: false };
  }
  return { label: source, isWarning: false };
}

/**
 * Exact micro-USD to dollar string.
 * Never rounds small non-zero numbers to 0.
 */
export function formatCostExact(microUSD?: number): string {
  if (microUSD === undefined || microUSD === null || isNaN(microUSD) || microUSD === 0) {
    return '0';
  }
  const formatted = formatUSD(microUSD);
  if (microUSD > 0 && formatted === '0') {
    return (microUSD / 1000000).toFixed(6).replace(/0+$/, '');
  }
  return formatted;
}

/**
 * Resolves display label and fallback for Key ID.
 * Identifies deleted keys from deleted_at.
 */
export function resolveKeyDisplay(
  keyId: string,
  keysMap: Map<string, Key>
): { label: string; isFallback: boolean; fullId: string; isDeleted: boolean } {
  if (!keyId) {
    return { label: '—', isFallback: false, fullId: '', isDeleted: false };
  }
  const keyObj = keysMap.get(keyId);
  const isDeleted = Boolean(keyObj?.deleted_at);

  if (keyObj && keyObj.label) {
    return { label: keyObj.label, isFallback: false, fullId: keyId, isDeleted };
  }
  const is64Hex = /^[a-f0-9]{64}$/i.test(keyId);
  if (is64Hex) {
    return { label: `${keyId.slice(0, 12)}…`, isFallback: true, fullId: keyId, isDeleted };
  }
  return { label: keyId, isFallback: false, fullId: keyId, isDeleted };
}

/**
 * Redacts sensitive headers.
 */
export function redactHeaders(
  headers: Record<string, string | string[]> | undefined | null
): Array<{ name: string; value: string }> {
  if (!headers) return [];
  const sensitiveRegex = /authorization|cookie|api.?key|token|secret/i;
  const entries: Array<{ name: string; value: string }> = [];

  for (const [key, val] of Object.entries(headers)) {
    const isSensitive = sensitiveRegex.test(key);
    let strVal: string;
    if (isSensitive) {
      strVal = '[redacted]';
    } else if (Array.isArray(val)) {
      strVal = val.join(', ');
    } else {
      strVal = String(val ?? '');
    }
    entries.push({ name: key, value: strVal });
  }

  return entries.sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * Recursively redacts sensitive keys from a JSON body.
 */
export function redactBody(body: unknown): unknown {
  if (body === null || body === undefined) {
    return body;
  }
  if (Array.isArray(body)) {
    return body.map(redactBody);
  }
  if (typeof body === 'object') {
    // Match credential fields, not legitimate generation options such as
    // max_tokens / token_limit. The server remains the redaction boundary.
    const sensitiveRegex = /^(authorization|cookie|set-cookie|api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password)$/i;
    const output: Record<string, unknown> = Object.create(null);
    for (const [k, v] of Object.entries(body as Record<string, unknown>)) {
      if (sensitiveRegex.test(k)) {
        output[k] = '[redacted]';
      } else {
        output[k] = redactBody(v);
      }
    }
    return output;
  }
  return body;
}

/**
 * Formats a raw ISO timestamp in browser-local time with an explicit numeric
 * UTC offset, e.g. `2026-10-20 18:00:00 UTC+08:00`.
 */
export function formatLocalDateTime(iso?: string | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(iso);
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  const hours = String(d.getHours()).padStart(2, '0');
  const minutes = String(d.getMinutes()).padStart(2, '0');
  const seconds = String(d.getSeconds()).padStart(2, '0');
  return `${year}-${month}-${day} ${hours}:${minutes}:${seconds} ${formatUTCOffset(-d.getTimezoneOffset())}`;
}

/**
 * Formats a chart timestamp in browser-local time. The numeric offset keeps
 * the two wall-clock hours of a DST fall-back distinguishable.
 */
export function formatLocalShort(iso: string, withYear = true): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(iso);
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  const hours = String(d.getHours()).padStart(2, '0');
  const minutes = String(d.getMinutes()).padStart(2, '0');
  const datePart = withYear ? `${year}-${month}-${day}` : `${month}-${day}`;
  return `${datePart} ${hours}:${minutes} ${formatUTCOffset(-d.getTimezoneOffset())}`;
}

/**
 * Calculates Cache Hit Rate percentage.
 * Cache Hit Rate (Token) = Σcache_read_tokens / Σfull_input_tokens
 * where full_input_tokens = input_tokens + cache_read_tokens + cache_write_tokens.
 * Returns null if fullInput is 0 or input/cache_read undefined.
 */
export function calculateCacheHitRate(
  cacheRead?: number,
  input?: number,
  cacheWrite?: number
): string | null {
  if (cacheRead === undefined || input === undefined) return null;
  const read = Number(cacheRead) || 0;
  const inTokens = Number(input) || 0;
  const write = Number(cacheWrite) || 0;
  const fullInput = inTokens + read + write;
  if (fullInput <= 0) return null;
  return `${((read / fullInput) * 100).toFixed(2)}%`;
}

/**
 * Calculates Failure Rate percentage.
 */
export function calculateFailureRate(failures?: number, requests?: number): string | null {
  if (requests === undefined || requests <= 0) return null;
  const f = Number(failures) || 0;
  return `${((f / requests) * 100).toFixed(1)}%`;
}

/**
 * Calculates TPS (Tokens per second) for a single request:
 * TPS = output / ((latency_ms - ttft_ms) / 1000)
 * Only valid if isStream is true, output > 0, and 0 < ttft_ms < latency_ms.
 */
export function calculateTPS(
  output?: number,
  latencyMs?: number,
  ttftMs?: number,
  isStream?: boolean
): string | null {
  if (!isStream) return null;
  if (!output || output <= 0) return null;
  if (!latencyMs || !ttftMs) return null;
  if (ttftMs <= 0 || ttftMs >= latencyMs) return null;
  const genSec = (latencyMs - ttftMs) / 1000;
  if (genSec <= 0) return null;
  return (output / genSec).toFixed(1);
}

/**
 * Formats a millisecond duration as seconds with fixed two decimals.
 * Missing values stay unknown; an explicit zero remains zero.
 */
export function formatSecondsFromMs(ms?: number | null): string {
  if (ms === undefined || ms === null || isNaN(ms)) return '—';
  // Round half-up at centisecond precision to avoid binary float truncation
  // such as 45ms rendering as 0.04 s.
  return `${(Math.round(ms / 10) / 100).toFixed(2)} s`;
}

/**
 * Formats an averaged millisecond duration as seconds with fixed two decimals.
 */
export function formatAvgSeconds(totalMs?: number, samples?: number): string {
  if (!samples || samples <= 0 || totalMs === undefined) return '—';
  return formatSecondsFromMs(totalMs / samples);
}

/**
 * Formats numbers with comma separators.
 */
export function formatNumber(n?: number): string {
  if (n === undefined || n === null || isNaN(n)) return '—';
  return n.toLocaleString('en-US');
}
