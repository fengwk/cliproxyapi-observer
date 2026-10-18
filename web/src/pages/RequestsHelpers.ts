import type { Key } from '../lib/types';
import { formatUSD } from '../lib/format';

export interface FilterState {
  from: string;
  to: string;
  key_id: string;
  model: string;
  provider: string;
  auth_index: string;
  status: '' | 'success' | 'failed';
}

export type PresetRange = 'today' | 'last7days' | 'custom';

/**
 * Returns today's UTC date as YYYY-MM-DD.
 */
export function getTodayUTCDateString(): string {
  const now = new Date();
  const year = now.getUTCFullYear();
  const month = String(now.getUTCMonth() + 1).padStart(2, '0');
  const day = String(now.getUTCDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Returns the date N days before today in UTC as YYYY-MM-DD.
 */
export function getPastUTCDateString(daysAgo: number): string {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() - daysAgo);
  const year = d.getUTCFullYear();
  const month = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Returns tomorrow's UTC date as YYYY-MM-DD (next UTC midnight).
 */
export function getTomorrowUTCDateString(): string {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() + 1);
  const year = d.getUTCFullYear();
  const month = String(d.getUTCMonth() + 1).padStart(2, '0');
  const day = String(d.getUTCDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

/**
 * Default today preset bounds:
 * from = today's start in UTC (YYYY-MM-DD)
 * to = '' (will resolve to current time NOW at query time to avoid querying future bounds).
 */
export function getTodayPresetRange(): { from: string; to: string } {
  return {
    from: getTodayUTCDateString(),
    to: '',
  };
}

/**
 * Last 7 days preset bounds:
 * Historical daily data before raw retention requires whole UTC day boundaries.
 * from = 6 days ago UTC midnight (YYYY-MM-DD, covering 7 full days)
 * to = tomorrow UTC midnight (YYYY-MM-DD, next UTC midnight boundary).
 */
export function getLast7DaysPresetRange(): { from: string; to: string } {
  return {
    from: getPastUTCDateString(6),
    to: getTomorrowUTCDateString(),
  };
}

/**
 * Corrective whole-date action for historical daily error.
 * Historically, queries before raw_retention_hours must use whole UTC dates.
 * Snaps from and to to YYYY-MM-DD boundaries without fabricated hours.
 */
export function snapToWholeDateRange(fromVal: string, toVal: string): { from: string; to: string } {
  let cleanFrom = fromVal ? fromVal.slice(0, 10) : getTodayUTCDateString();
  if (!/^\d{4}-\d{2}-\d{2}$/.test(cleanFrom)) {
    cleanFrom = getTodayUTCDateString();
  }

  let cleanTo = toVal ? toVal.slice(0, 10) : '';
  if (!cleanTo || !/^\d{4}-\d{2}-\d{2}$/.test(cleanTo)) {
    cleanTo = getTomorrowUTCDateString();
  }

  return {
    from: cleanFrom,
    to: cleanTo,
  };
}

/**
 * Detects whether a query error is specifically a historical date range error.
 * Excludes generic network failures, 401 unauthorized, or pricing refresh errors.
 */
export function isHistoricalDateQueryError(
  errorMsg: string | null | undefined,
  fromVal: string,
  toVal: string
): boolean {
  if (!errorMsg) return false;
  const lower = errorMsg.toLowerCase();

  // Exclude 401, unauthorized, network, fetch failure
  if (
    lower.includes('401') ||
    lower.includes('unauthorized') ||
    lower.includes('network') ||
    lower.includes('failed to fetch')
  ) {
    return false;
  }

  // If backend error explicitly mentions historical or whole date
  if (lower.includes('historical') || lower.includes('whole date') || lower.includes('整日')) {
    return true;
  }

  const isQueryInvalid =
    lower.includes('invalid') ||
    lower.includes('date') ||
    lower.includes('range') ||
    lower.includes('400');

  if (!isQueryInvalid) return false;

  const today = getTodayUTCDateString();
  const fromDay = fromVal ? fromVal.slice(0, 10) : today;
  const isOlder = fromDay < today;
  const hasHours = fromVal.includes('T') || toVal.includes('T');

  return isOlder || hasHours;
}

/**
 * Builds the URLSearchParams query string for /requests and /summary.
 * For today's query, defaults 'to' to NOW when empty to avoid querying future timestamps.
 * For historical queries (older than today), defaults 'to' to tomorrow UTC midnight to satisfy whole-day boundaries.
 */
export function buildRequestsQueryParams(
  filters: FilterState,
  page: number,
  pageSize = 25,
  nowIso = new Date().toISOString()
): string {
  const params = new URLSearchParams();
  params.set('page', String(page));
  params.set('page_size', String(pageSize));

  // from: if already RFC3339 with 'T', keep it; otherwise append T00:00:00Z
  const rawFrom = filters.from.trim();
  const today = getTodayUTCDateString();
  if (rawFrom) {
    params.set('from', rawFrom.includes('T') ? rawFrom : `${rawFrom}T00:00:00Z`);
  } else {
    params.set('from', `${today}T00:00:00Z`);
  }

  // to:
  const rawTo = filters.to.trim();
  if (rawTo) {
    params.set('to', rawTo.includes('T') ? rawTo : `${rawTo}T00:00:00Z`);
  } else {
    const fromDay = rawFrom ? rawFrom.slice(0, 10) : today;
    if (fromDay < today) {
      params.set('to', `${getTomorrowUTCDateString()}T00:00:00Z`);
    } else {
      params.set('to', nowIso);
    }
  }

  if (filters.key_id) params.set('key_id', filters.key_id);
  if (filters.model) params.set('model', filters.model);
  if (filters.provider) params.set('provider', filters.provider);
  if (filters.auth_index) params.set('auth_index', filters.auth_index);
  if (filters.status) params.set('status', filters.status);

  return params.toString();
}

/**
 * Capture status honest user copy mapping.
 */
export const CAPTURE_STATUS_LABELS: Record<string, string> = {
  available: '可用',
  disabled: '采集未开启',
  expired: '采集已过期',
  missing: '未捕获',
  truncated: '内容已截断',
  redacted: '内容已脱敏',
};

/**
 * Pricing source human label mapping.
 */
export function formatPriceSource(source: string, ruleId?: string): { label: string; isWarning: boolean } {
  if (!source || source === 'unpriced') {
    return { label: '未定价按 0', isWarning: true };
  }
  if (source === 'manual') {
    return { label: '手动定价', isWarning: false };
  }
  if (source === 'catalog' || source === 'auto') {
    return { label: '公共参考价格', isWarning: false };
  }
  if (source === 'rule') {
    return { label: ruleId ? `规则 (${ruleId})` : '自定义规则', isWarning: false };
  }
  return { label: source, isWarning: false };
}

/**
 * Exact micro-USD to dollar string.
 * Never rounds small non-zero numbers to 0.
 */
export function formatCostExact(microUSD: number): string {
  if (microUSD === 0) return '0';
  const formatted = formatUSD(microUSD);
  if (microUSD > 0 && formatted === '0') {
    return (microUSD / 1000000).toFixed(6).replace(/0+$/, '');
  }
  return formatted;
}

/**
 * Resolves display label and fallback for Key ID.
 * Avoids showing raw 64-hex IDs as labels.
 */
export function resolveKeyDisplay(
  keyId: string,
  keysMap: Map<string, Key>
): { label: string; isFallback: boolean; fullId: string } {
  if (!keyId) {
    return { label: '—', isFallback: false, fullId: '' };
  }
  const keyObj = keysMap.get(keyId);
  if (keyObj && keyObj.label) {
    return { label: keyObj.label, isFallback: false, fullId: keyId };
  }
  // Fallback: 64-hex opaque Key ID truncated to 12 chars
  const is64Hex = /^[a-f0-9]{64}$/i.test(keyId);
  if (is64Hex) {
    return { label: `${keyId.slice(0, 12)}…`, isFallback: true, fullId: keyId };
  }
  return { label: keyId, isFallback: false, fullId: keyId };
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
    const sensitiveRegex = /authorization|cookie|api.?key|token|secret|password/i;
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
 * Formats a raw ISO timestamp to UTC short string for tooltips and fallback display.
 * e.g. YYYY-MM-DD HH:mm UTC
 */
export function formatUTCShort(iso: string, withYear = true): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(iso);
  const isoStr = d.toISOString();
  return (withYear ? isoStr.slice(0, 10) : isoStr.slice(5, 10)) + ' ' + isoStr.slice(11, 16) + ' UTC';
}
