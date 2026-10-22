import type {
  CaptureMode,
  Condition,
  FactorSet,
  ManualPrice,
  Multiplier,
  Override,
  PricingConfig,
  PricingDefinition,
  PricingSource,
  RatePatch,
  RateSet,
} from '../lib/types';
import { parseUSD, formatUSD } from '../lib/format';

export const MAX_SAFE_MICRO = 9_000_000_000_000_000;
export const DEFAULT_MAX_BODY_BYTES = 1_048_576; // 1 MiB
export const MAX_BODY_BYTES = 8_388_608; // 8 MiB
export const DEFAULT_MAX_BODY_STORAGE_BYTES = 268_435_456; // 256 MiB
export const MAX_BODY_STORAGE_BYTES = 1_073_741_824; // 1 GiB
export const MAX_CONFIG_ENTRIES = 4096;
export const MAX_RULES_PER_MODEL = 64;
const MAX_IDENTITY_LENGTH = 1024;
const MAX_MULTIPLIER_LENGTH = 64;
const MAX_CONDITION_DEPTH = 8;
const MAX_CONDITION_NODES = 64;
const MAX_CONDITION_LIST = 16;
const MAX_TTL_TIERS = 16;
const MAX_FACTOR = 1_000_000n;

export const MIN_RAW_RETENTION_HOURS = 1;
export const MAX_RAW_RETENTION_HOURS = 168; // 7 days
export const MIN_DAILY_RETENTION_DAYS = 1;
export const MAX_DAILY_RETENTION_DAYS = 3650; // 10 years

export type PricingReasoningMode = 'inherit' | 'custom';
export type PatchReasoningMode = 'inherit' | 'same_output' | 'custom';
export type ContextLowerOp = '' | 'gt' | 'gte';
export type ContextUpperOp = '' | 'lt' | 'lte';

export interface GeneralSettingsDraft {
  capture_enabled: boolean;
  capture_mode?: CaptureMode;
  capture_retention_seconds?: number;
  max_body_bytes: number;
  max_body_storage_bytes: number;
  raw_retention_hours: number;
  daily_retention_days: number;
}

export interface TtlDraft {
  rowId: string;
  seconds: string;
  price: string;
}

export interface RateSetDraft {
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoningMode: PricingReasoningMode;
  reasoning: string;
  ttls: TtlDraft[];
}

export interface RatePatchDraft {
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoningMode: PatchReasoningMode;
  reasoning: string;
  /** inherit omits the property; override emits the map, including an explicit empty map. */
  ttlMode: 'inherit' | 'override';
  ttls: TtlDraft[];
}

export interface ContextDraft {
  lowerOp: ContextLowerOp;
  lowerValue: string;
  upperOp: ContextUpperOp;
  upperValue: string;
}

export interface ConditionDraft {
  context: ContextDraft;
  /** Presence flags preserve explicitly empty semantic collections. */
  serviceTierPresent: boolean;
  serviceTier: string;
  dateGte: string;
  dateLt: string;
  timeStart: string;
  timeEnd: string;
  timezone: string;
  weekdays: number[];
  /** Condition[] groups composed with AND. */
  allPresent: boolean;
  all: ConditionDraft[];
  /** Condition[] groups composed with OR. */
  anyPresent: boolean;
  any: ConditionDraft[];
  /** Negated nested condition. */
  not: ConditionDraft | null;
}

export interface FactorDraft {
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoning: string;
}

export interface OverrideDraft {
  rowId: string;
  id: string;
  condition: ConditionDraft;
  rates: RatePatchDraft;
}

export interface MultiplierDraft {
  rowId: string;
  id: string;
  condition: ConditionDraft;
  factors: FactorDraft;
}

export interface DefinitionDraft {
  base: RateSetDraft;
  overrides: OverrideDraft[];
  multipliers: MultiplierDraft[];
}

export interface ManualPriceDraft {
  id: string; // internal client row id for React keys
  model: string;
  provider: string;
  definition: DefinitionDraft;
}

export type CapacityUnit = 'B' | 'KB' | 'MB' | 'GB';

export const UNIT_BYTES: Record<CapacityUnit, number> = {
  B: 1,
  KB: 1024,
  MB: 1048576,
  GB: 1073741824,
};

export const WEEKDAYS = [
  { value: 1, label: '一' },
  { value: 2, label: '二' },
  { value: 3, label: '三' },
  { value: 4, label: '四' },
  { value: 5, label: '五' },
  { value: 6, label: '六' },
  { value: 7, label: '日' },
];

function randomRowId(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * Split exact integer bytes into an amount string and the best fitting unit without loss.
 */
export function splitBytesToCapacity(bytes: number): { amount: string; unit: CapacityUnit } {
  const n = Number(bytes);
  if (!Number.isSafeInteger(n) || n < 0) {
    return { amount: '1', unit: 'B' };
  }
  if (n === 0) {
    return { amount: '0', unit: 'B' };
  }
  if (n % 1073741824 === 0) {
    return { amount: String(n / 1073741824), unit: 'GB' };
  }
  if (n % 1048576 === 0) {
    return { amount: String(n / 1048576), unit: 'MB' };
  }
  if (n % 1024 === 0) {
    return { amount: String(n / 1024), unit: 'KB' };
  }
  return { amount: String(n), unit: 'B' };
}

/**
 * Parses user input amount string and unit into exact whole integer bytes.
 * Throws if the input cannot be represented as an exact whole integer number of bytes.
 */
export function parseCapacityToBytes(amountStr: string, unit: CapacityUnit, label: string): number {
  const trimmed = amountStr.trim();
  if (!trimmed) {
    throw new Error(`${label}不能为空`);
  }
  if (!/^\d+(\.\d+)?$/.test(trimmed)) {
    throw new Error(`${label}必须是非负数字`);
  }

  const parts = trimmed.split('.');
  const intPart = BigInt(parts[0] || '0');
  const unitMultiplier = BigInt(UNIT_BYTES[unit]);

  let totalBytesBig: bigint;
  if (parts.length > 1 && parts[1]) {
    const fracStr = parts[1];
    const divisor = 10n ** BigInt(fracStr.length);
    const numerator = BigInt(fracStr) * unitMultiplier;
    if (numerator % divisor !== 0n) {
      throw new Error(`${label}无法精确换算为整数字节数，请输入整数字节或有效的小数`);
    }
    const fracBytes = numerator / divisor;
    totalBytesBig = intPart * unitMultiplier + fracBytes;
  } else {
    totalBytesBig = intPart * unitMultiplier;
  }

  const bytes = Number(totalBytesBig);
  if (!Number.isSafeInteger(bytes) || bytes < 1) {
    throw new Error(`${label}超出允许范围`);
  }
  return bytes;
}

/**
 * Format bytes into human-readable string (B, KB, MB, GB).
 */
export function formatBytes(bytes: number): string {
  const n = Number(bytes);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

/**
 * Format number with comma thousands separator.
 */
export function formatInt(value: number): string {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return Math.round(n).toLocaleString('zh-CN');
}

/**
 * Format micro-USD into dollar rate string with $ prefix for display.
 */
export function formatRateDisplay(microUsd: number | undefined | null): string {
  if (microUsd === undefined || microUsd === null) return '—';
  return `$${formatUSD(microUsd)}`;
}

export function formatReasoningDisplay(reasoning: number | null | undefined): string {
  return reasoning === undefined || reasoning === null ? '同输出' : formatRateDisplay(reasoning);
}

export function pricingSourceLabel(source: PricingSource | string | undefined): string {
  if (source === 'models_dev_v1') return 'models.dev v1';
  if (source === 'models_dev_v2') return 'models.dev v2';
  return source || '—';
}

/**
 * Return user-friendly source label. Zero pricing is a normal amount, not a
 * pending/unpriced state.
 */
export function sourceLabel(source: string): string {
  switch (source) {
    case 'manual':
      return '手工价格';
    case 'auto':
      return '自动目录';
    case 'zero':
      return '零价格';
    default:
      return source || '—';
  }
}

/**
 * Validate local HH:mm-HH:mm time range with cross-midnight support.
 * Range is half-open [start, end), start and end must differ.
 */
export function validTimeRange(value: string): boolean {
  const trimmed = value.trim();
  // The backend accepts end 24:00 (only for an end clock) and rejects equal ends.
  const match = /^([01]\d|2[0-3]):[0-5]\d-((?:[01]\d|2[0-3]):[0-5]\d|24:00)$/.exec(trimmed);
  if (!match) return false;
  return trimmed.slice(0, 5) !== trimmed.slice(6, 11);
}

/**
 * Validate storage settings and return error message if invalid, or null if valid.
 */
export function validateGeneralSettings(draft: GeneralSettingsDraft): string | null {
  if (draft.max_body_bytes < 1 || draft.max_body_bytes > MAX_BODY_BYTES) {
    return `单个请求体上限必须在 1 字节至 8 MiB（8,388,608 字节）之间`;
  }
  if (draft.max_body_storage_bytes < 1 || draft.max_body_storage_bytes > MAX_BODY_STORAGE_BYTES) {
    return `请求体总存储上限必须在 1 字节至 1 GiB（1,073,741,824 字节）之间`;
  }
  if (draft.max_body_bytes > draft.max_body_storage_bytes) {
    return `单个请求体上限不能超过请求体总存储上限`;
  }
  if (draft.raw_retention_hours < MIN_RAW_RETENTION_HOURS || draft.raw_retention_hours > MAX_RAW_RETENTION_HOURS) {
    return `原始明细保留时间必须在 1 至 168 小时（7 天）之间`;
  }
  if (draft.daily_retention_days < MIN_DAILY_RETENTION_DAYS || draft.daily_retention_days > MAX_DAILY_RETENTION_DAYS) {
    return `每日汇总保留时间必须在 1 至 3650 天（10 年）之间`;
  }
  const ttl = draft.capture_retention_seconds ?? draft.raw_retention_hours * 3600;
  if (!Number.isSafeInteger(ttl) || ttl < 60 || ttl > draft.raw_retention_hours * 3600) {
    return '内容保留时间必须为整秒，至少 60 秒且不超过原始日志保留时间';
  }
  return null;
}

function parseRateValue(value: string, label: string, allowEmpty: boolean): number | undefined {
  const trimmed = value.trim();
  if (!trimmed) {
    if (allowEmpty) return undefined;
    return 0;
  }
  try {
    return parseUSD(trimmed);
  } catch {
    throw new Error(`${label}必须是有效的非负 USD 金额（最多 6 位小数）`);
  }
}

export function formatTtlSeconds(seconds: number): string {
  if (!Number.isSafeInteger(seconds) || seconds <= 0) return String(seconds);
  const parts: string[] = [];
  let rest = seconds;
  const days = Math.floor(rest / 86400);
  if (days) {
    parts.push(`${days}d`);
    rest -= days * 86400;
  }
  const hours = Math.floor(rest / 3600);
  if (hours) {
    parts.push(`${hours}h`);
    rest -= hours * 3600;
  }
  const minutes = Math.floor(rest / 60);
  if (minutes) {
    parts.push(`${minutes}m`);
    rest -= minutes * 60;
  }
  if (rest) parts.push(`${rest}s`);
  return parts.join('') || '0s';
}

export function parseTtlSeconds(value: string, label: string, optional = true): number | undefined {
  const trimmed = value.trim();
  if (!trimmed) {
    if (optional) return undefined;
    throw new Error(`${label}不能为空`);
  }
  if (/^\d+$/.test(trimmed)) {
    const seconds = Number(trimmed);
    if (!Number.isSafeInteger(seconds) || seconds <= 0) {
      throw new Error(`${label}必须是正整数秒`);
    }
    return seconds;
  }
  const match = /^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(trimmed);
  if (!match || !match.slice(1).some((part) => part !== undefined)) {
    throw new Error(`${label}格式应为 5m、1h 或 3600s`);
  }
  const seconds =
    Number(match[1] || 0) * 86400 +
    Number(match[2] || 0) * 3600 +
    Number(match[3] || 0) * 60 +
    Number(match[4] || 0);
  if (!Number.isSafeInteger(seconds) || seconds <= 0) {
    throw new Error(`${label}必须是正整数秒`);
  }
  return seconds;
}

export function ttlDraftsFrom(record: Record<string, number> | undefined): TtlDraft[] {
  if (!record) return [];
  return Object.entries(record)
    .map(([seconds, price]) => ({ seconds: Number(seconds), price }))
    .filter((entry) => Number.isSafeInteger(entry.seconds) && entry.seconds > 0)
    .sort((a, b) => a.seconds - b.seconds)
    .map((entry) => ({
      rowId: randomRowId('ttl'),
      seconds: formatTtlSeconds(entry.seconds),
      price: formatUSD(entry.price),
    }));
}

function parseTtlDrafts(rows: TtlDraft[], label: string): Record<string, number> | undefined {
  const result: Record<string, number> = Object.create(null);
  for (let i = 0; i < rows.length; i++) {
    const row = rows[i];
    const secondsText = row.seconds.trim();
    const priceText = row.price.trim();
    if (!secondsText && !priceText) continue;
    const seconds = parseTtlSeconds(secondsText, `${label}第 ${i + 1} 档 TTL`, false)!;
    const price = parseRateValue(priceText, `${label}第 ${i + 1} 档价格`, false)!;
    const key = String(seconds);
    if (Object.prototype.hasOwnProperty.call(result, key)) {
      throw new Error(`${label}TTL 档位重复：${formatTtlSeconds(seconds)}`);
    }
    result[key] = price;
  }
  if (Object.keys(result).length > MAX_TTL_TIERS) {
    throw new Error(`${label}TTL 档位不能超过 ${MAX_TTL_TIERS} 个`);
  }
  return Object.keys(result).length ? result : undefined;
}

function parseRateFactor(value: string, label: string): string | undefined {
  const trimmed = value.trim();
  if (!trimmed) return undefined;
  if (trimmed.length > MAX_MULTIPLIER_LENGTH || !/^(?:0|[1-9]\d*)(?:\.\d+)?$/.test(trimmed)) {
    throw new Error(`${label}必须是非负十进制字符串`);
  }
  const [whole, fraction = ''] = trimmed.split('.');
  const tooLarge =
    BigInt(whole) > MAX_FACTOR ||
    (BigInt(whole) === MAX_FACTOR && /[1-9]/.test(fraction));
  if (tooLarge) {
    throw new Error(`${label}不能超过 1000000`);
  }
  return trimmed;
}

export function rateSetToDraft(base: RateSet): RateSetDraft {
  return {
    input: formatUSD(base.input ?? 0),
    output: formatUSD(base.output ?? 0),
    cache_read: formatUSD(base.cache_read ?? 0),
    cache_write: formatUSD(base.cache_write ?? 0),
    reasoningMode: base.reasoning === undefined || base.reasoning === null ? 'inherit' : 'custom',
    reasoning: base.reasoning === undefined || base.reasoning === null ? '' : formatUSD(base.reasoning),
    ttls: ttlDraftsFrom(base.cache_write_by_ttl),
  };
}

export function ratePatchToDraft(rates: RatePatch): RatePatchDraft {
  const ttlMode = rates.cache_write_by_ttl === undefined ? 'inherit' : 'override';
  return {
    input: rates.input === undefined ? '' : formatUSD(rates.input),
    output: rates.output === undefined ? '' : formatUSD(rates.output),
    cache_read: rates.cache_read === undefined ? '' : formatUSD(rates.cache_read),
    cache_write: rates.cache_write === undefined ? '' : formatUSD(rates.cache_write),
    reasoningMode:
      rates.reasoning === undefined ? 'inherit' : rates.reasoning === null ? 'same_output' : 'custom',
    reasoning: rates.reasoning === undefined || rates.reasoning === null ? '' : formatUSD(rates.reasoning),
    ttlMode,
    ttls: ttlDraftsFrom(rates.cache_write_by_ttl),
  };
}

export function emptyRateSetDraft(): RateSetDraft {
  return {
    input: '0',
    output: '0',
    cache_read: '0',
    cache_write: '0',
    reasoningMode: 'inherit',
    reasoning: '',
    ttls: [],
  };
}

export function emptyRatePatchDraft(): RatePatchDraft {
  return {
    input: '',
    output: '',
    cache_read: '',
    cache_write: '',
    reasoningMode: 'inherit',
    reasoning: '',
    ttlMode: 'inherit',
    ttls: [],
  };
}

export function emptyConditionDraft(): ConditionDraft {
  return {
    context: { lowerOp: '', lowerValue: '', upperOp: '', upperValue: '' },
    serviceTierPresent: false,
    serviceTier: '',
    dateGte: '',
    dateLt: '',
    timeStart: '',
    timeEnd: '',
    timezone: 'UTC',
    weekdays: [],
    allPresent: false,
    all: [],
    anyPresent: false,
    any: [],
    not: null,
  };
}

export function conditionToDraft(condition: Condition | undefined | null): ConditionDraft {
  const draft = emptyConditionDraft();
  if (!condition) return draft;

  const { context, serviceTier, date, time, all, any, not } = condition;
  if (context) {
    const lowerOp: ContextLowerOp = context.gt !== undefined ? 'gt' : context.gte !== undefined ? 'gte' : '';
    const lowerValue = lowerOp === 'gt' ? context.gt : lowerOp === 'gte' ? context.gte : undefined;
    const upperOp: ContextUpperOp = context.lt !== undefined ? 'lt' : context.lte !== undefined ? 'lte' : '';
    const upperValue = upperOp === 'lt' ? context.lt : upperOp === 'lte' ? context.lte : undefined;
    draft.context = {
      lowerOp,
      lowerValue: lowerValue === undefined ? '' : String(lowerValue),
      upperOp,
      upperValue: upperValue === undefined ? '' : String(upperValue),
    };
  }
  if (serviceTier !== undefined) {
    draft.serviceTierPresent = true;
    draft.serviceTier = serviceTier.join(', ');
  }
  if (date?.gte) draft.dateGte = date.gte;
  if (date?.lt) draft.dateLt = date.lt;
  if (time) {
    draft.timeStart = time.start;
    draft.timeEnd = time.end;
    draft.timezone = time.timezone || 'UTC';
    draft.weekdays = [...(time.weekdays ?? [])];
  }
  draft.allPresent = Array.isArray(all);
  draft.all = (all ?? []).map(conditionToDraft);
  draft.anyPresent = Array.isArray(any);
  draft.any = (any ?? []).map(conditionToDraft);
  draft.not = not ? conditionToDraft(not) : null;
  return draft;
}

export function hasAdvancedCondition(draft: ConditionDraft): boolean {
  return draft.allPresent || draft.all.length > 0 || draft.anyPresent || draft.any.length > 0 || draft.not !== null;
}

export function conditionSummary(draft: ConditionDraft): string {
  const parts: string[] = [];
  if (draft.context.lowerOp && draft.context.lowerValue.trim()) {
    const opLabel = { gt: '>', gte: '≥' }[draft.context.lowerOp];
    parts.push(`上下文 ${opLabel} ${draft.context.lowerValue.trim()}`);
  }
  if (draft.context.upperOp && draft.context.upperValue.trim()) {
    const opLabel = { lt: '<', lte: '≤' }[draft.context.upperOp];
    parts.push(`上下文 ${opLabel} ${draft.context.upperValue.trim()}`);
  }
  if (draft.serviceTier.trim()) parts.push(`服务档 ${draft.serviceTier.trim()}`);
  if (draft.dateGte || draft.dateLt) parts.push(`日期 ${draft.dateGte || '…'} → ${draft.dateLt || '…'}`);
  if (draft.timeStart || draft.timeEnd) {
    parts.push(`${draft.timezone || 'UTC'} ${draft.timeStart || '…'}-${draft.timeEnd || '…'}`);
  }
  if (draft.weekdays.length) {
    parts.push(`星期 ${draft.weekdays.map((day) => WEEKDAYS.find((item) => item.value === day)?.label).join('')}`);
  }
  if (draft.all.length) parts.push(`全部满足 ${draft.all.length} 组`);
  else if (draft.allPresent) parts.push('全部条件为空（恒真）');
  if (draft.any.length) parts.push(`任一满足 ${draft.any.length} 组`);
  else if (draft.anyPresent) parts.push('任一条件为空（恒假）');
  if (draft.not) parts.push('含非条件');
  return parts.join('；') || '任何请求';
}

function parseContext(draft: ContextDraft, label: string): Condition['context'] | undefined {
  const lowerText = draft.lowerValue.trim();
  const upperText = draft.upperValue.trim();
  if (!draft.lowerOp && lowerText) throw new Error(`${label}上下文下限缺少比较方式`);
  if (draft.lowerOp && !lowerText) throw new Error(`${label}上下文下限缺少阈值`);
  if (!draft.upperOp && upperText) throw new Error(`${label}上下文上限缺少比较方式`);
  if (draft.upperOp && !upperText) throw new Error(`${label}上下文上限缺少阈值`);
  if (!draft.lowerOp && !draft.upperOp) return undefined;

  const parseThreshold = (value: string, name: string): number => {
    if (!/^\d+$/.test(value)) throw new Error(`${label}${name}必须是非负整数`);
    const parsed = Number(value);
    if (!Number.isSafeInteger(parsed) || parsed > MAX_SAFE_MICRO) {
      throw new Error(`${label}${name}超出合法范围`);
    }
    return parsed;
  };

  return {
    ...(draft.lowerOp ? { [draft.lowerOp]: parseThreshold(lowerText, '上下文下限') } : {}),
    ...(draft.upperOp ? { [draft.upperOp]: parseThreshold(upperText, '上下文上限') } : {}),
  };
}

function parseCondition(
  draft: ConditionDraft,
  label: string,
  depth = 0,
  state: { nodes: number } = { nodes: 0 }
): Condition {
  state.nodes += 1;
  if (depth > MAX_CONDITION_DEPTH) {
    throw new Error(`${label}组合条件嵌套超过 ${MAX_CONDITION_DEPTH} 层`);
  }
  if (state.nodes > MAX_CONDITION_NODES) {
    throw new Error(`${label}条件节点不能超过 ${MAX_CONDITION_NODES} 个`);
  }

  const out: Condition = {};

  const context = parseContext(draft.context, label);
  if (context) out.context = context;

  const tiers = Array.from(new Set(
    draft.serviceTier
      .split(/[,\n]/)
      .map((value) => value.trim())
      .filter(Boolean)
  ));
  if (tiers.length > MAX_CONDITION_LIST) {
    throw new Error(`${label}服务档位不能超过 ${MAX_CONDITION_LIST} 个`);
  }
  if (tiers.some((tier) => tier.length > 128)) {
    throw new Error(`${label}服务档位不能超过 128 个字符`);
  }
  if (draft.serviceTierPresent || tiers.length) {
    out.serviceTier = tiers;
  }

  const dateGte = draft.dateGte.trim();
  const dateLt = draft.dateLt.trim();
  if (dateGte || dateLt) {
    for (const value of [dateGte, dateLt]) {
      if (value && !/^\d{4}-\d{2}-\d{2}$/.test(value)) {
        throw new Error(`${label}日期必须是 YYYY-MM-DD`);
      }
    }
    if (dateGte && dateLt && dateGte >= dateLt) {
      throw new Error(`${label}日期下限必须早于上限`);
    }
    out.date = { ...(dateGte ? { gte: dateGte } : {}), ...(dateLt ? { lt: dateLt } : {}) };
  }

  const timeStart = draft.timeStart.trim();
  const timeEnd = draft.timeEnd.trim();
  const timezone = draft.timezone.trim() || 'UTC';
  if (timeStart || timeEnd || draft.weekdays.length) {
    if (!timeStart || !timeEnd) {
      throw new Error(`${label}时间规则需要同时填写开始与结束时间`);
    }
    if (!validTimeRange(`${timeStart}-${timeEnd}`)) {
      throw new Error(`${label}时间区间必须是 HH:mm-HH:mm，且起点与终点不同`);
    }
    try {
      new Intl.DateTimeFormat('en-US', { timeZone: timezone });
    } catch {
      throw new Error(`${label}时区必须是有效的 IANA 名称`);
    }
    const weekdays = Array.from(new Set(draft.weekdays)).sort((a, b) => a - b);
    if (weekdays.some((day) => !Number.isInteger(day) || day < 1 || day > 7)) {
      throw new Error(`${label}星期必须在 1 至 7 之间`);
    }
    out.time = {
      start: timeStart,
      end: timeEnd,
      timezone,
      ...(weekdays.length ? { weekdays } : {}),
    };
  }

  if (draft.allPresent || draft.all.length) {
    if (draft.all.length > MAX_CONDITION_LIST) {
      throw new Error(`${label}全部条件不能超过 ${MAX_CONDITION_LIST} 项`);
    }
    out.all = draft.all.map((child, index) => parseCondition(child, `${label}全部条件 ${index + 1} `, depth + 1, state));
  }
  if (draft.anyPresent || draft.any.length) {
    if (draft.any.length > MAX_CONDITION_LIST) {
      throw new Error(`${label}任一条件不能超过 ${MAX_CONDITION_LIST} 项`);
    }
    out.any = draft.any.map((child, index) => parseCondition(child, `${label}任一条件 ${index + 1} `, depth + 1, state));
  }
  if (draft.not) {
    out.not = parseCondition(draft.not, `${label}非条件 `, depth + 1, state);
  }

  return out;
}

function parseRatePatch(draft: RatePatchDraft, label: string): RatePatch {
  const patch: RatePatch = {};
  const fields: Array<[keyof Pick<RatePatchDraft, 'input' | 'output' | 'cache_read' | 'cache_write'>, keyof RatePatch]> = [
    ['input', 'input'],
    ['output', 'output'],
    ['cache_read', 'cache_read'],
    ['cache_write', 'cache_write'],
  ];
  for (const [formField, apiField] of fields) {
    const value = parseRateValue(draft[formField], `${label}${formField} 价格`, true);
    if (value !== undefined) (patch as Record<string, number>)[apiField] = value;
  }
  if (draft.reasoningMode === 'same_output') {
    patch.reasoning = null;
  } else if (draft.reasoningMode === 'custom') {
    const value = parseRateValue(draft.reasoning, `${label}思考价格`, false);
    if (value !== undefined) patch.reasoning = value;
  }
  if (draft.ttlMode === 'override') {
    patch.cache_write_by_ttl = parseTtlDrafts(draft.ttls, `${label}缓存写入 `) ?? {};
  }
  return patch;
}

export function emptyDefinitionDraft(): DefinitionDraft {
  return { base: emptyRateSetDraft(), overrides: [], multipliers: [] };
}

export function definitionToDraft(definition: PricingDefinition): DefinitionDraft {
  return {
    base: rateSetToDraft(definition.base),
    overrides: (definition.overrides ?? []).map((override) => ({
      rowId: randomRowId('override'),
      id: override.id,
      condition: conditionToDraft(override.when),
      rates: ratePatchToDraft(override.rates ?? {}),
    })),
    multipliers: (definition.multipliers ?? []).map((multiplier) => ({
      rowId: randomRowId('multiplier'),
      id: multiplier.id,
      condition: conditionToDraft(multiplier.when),
      factors: {
        input: multiplier.factors?.input ?? '',
        output: multiplier.factors?.output ?? '',
        cache_read: multiplier.factors?.cache_read ?? '',
        cache_write: multiplier.factors?.cache_write ?? '',
        reasoning: multiplier.factors?.reasoning ?? '',
      },
    })),
  };
}

export function newOverrideDraft(existingIds: string[]): OverrideDraft {
  return {
    rowId: randomRowId('override'),
    id: generateUniqueDefinitionID(existingIds, 'override'),
    condition: emptyConditionDraft(),
    rates: emptyRatePatchDraft(),
  };
}

export function newMultiplierDraft(existingIds: string[]): MultiplierDraft {
  return {
    rowId: randomRowId('multiplier'),
    id: generateUniqueDefinitionID(existingIds, 'multiplier'),
    condition: emptyConditionDraft(),
    factors: { input: '', output: '', cache_read: '', cache_write: '', reasoning: '' },
  };
}

export function newTtlDraft(): TtlDraft {
  return { rowId: randomRowId('ttl'), seconds: '', price: '' };
}

export function parseDefinition(draft: DefinitionDraft, label: string): PricingDefinition {
  const base: RateSet = {
    input: parseRateValue(draft.base.input, `${label}输入价格`, false)!,
    output: parseRateValue(draft.base.output, `${label}输出价格`, false)!,
    cache_read: parseRateValue(draft.base.cache_read, `${label}缓存读价格`, false)!,
    cache_write: parseRateValue(draft.base.cache_write, `${label}缓存写价格`, false)!,
    reasoning:
      draft.base.reasoningMode === 'inherit'
        ? null
        : parseRateValue(draft.base.reasoning, `${label}思考价格`, false)!,
  };
  base.cache_write_by_ttl = parseTtlDrafts(draft.base.ttls, `${label}缓存写入 `) ?? {};

  if (draft.overrides.length > MAX_RULES_PER_MODEL) {
    throw new Error(`${label}价格覆盖不能超过 ${MAX_RULES_PER_MODEL} 条`);
  }
  const overrideIds = Object.create(null);
  const overrides: Override[] = draft.overrides.map((entry, index) => {
    const itemLabel = `${label}价格覆盖 ${index + 1} `;
    const id = entry.id.trim();
    if (!id) throw new Error(`${itemLabel}规则 ID 不能为空`);
    if (id.length > MAX_IDENTITY_LENGTH) throw new Error(`${itemLabel}规则 ID 过长`);
    if (overrideIds[id]) throw new Error(`${label}价格覆盖规则 ID 重复：${id}`);
    overrideIds[id] = true;
    return {
      id,
      when: parseCondition(entry.condition, itemLabel),
      rates: parseRatePatch(entry.rates, itemLabel),
    };
  });

  if (draft.multipliers.length > MAX_RULES_PER_MODEL) {
    throw new Error(`${label}价格倍率不能超过 ${MAX_RULES_PER_MODEL} 条`);
  }
  const multiplierIds = Object.create(null);
  const multipliers: Multiplier[] = draft.multipliers.map((entry, index) => {
    const itemLabel = `${label}价格倍率 ${index + 1} `;
    const id = entry.id.trim();
    if (!id) throw new Error(`${itemLabel}规则 ID 不能为空`);
    if (id.length > MAX_IDENTITY_LENGTH) throw new Error(`${itemLabel}规则 ID 过长`);
    if (multiplierIds[id]) throw new Error(`${label}价格倍率规则 ID 重复：${id}`);
    multiplierIds[id] = true;
    const factors: FactorSet = {};
    const mappings: Array<[keyof FactorDraft, keyof FactorSet]> = [
      ['input', 'input'],
      ['output', 'output'],
      ['cache_read', 'cache_read'],
      ['cache_write', 'cache_write'],
      ['reasoning', 'reasoning'],
    ];
    for (const [formField, apiField] of mappings) {
      const value = parseRateFactor(entry.factors[formField], `${itemLabel}${formField} 倍率`);
      if (value !== undefined) (factors as Record<string, string>)[apiField] = value;
    }
    return { id, when: parseCondition(entry.condition, itemLabel), factors };
  });

  return { base, overrides, multipliers };
}

function manualPriceIdentity(model: string, provider: string): string {
  return `${model}\u0000${provider}`;
}

/**
 * Validate manual price rows and produce complete (model, provider) definitions.
 * Uses prototype-safe records to prevent __proto__ or constructor collisions.
 */
export function parseManualPrices(drafts: ManualPriceDraft[]): ManualPrice[] {
  if (drafts.length > MAX_CONFIG_ENTRIES) {
    throw new Error(`手工价格不能超过 ${MAX_CONFIG_ENTRIES} 条`);
  }

  const seen = Object.create(null);
  return drafts.map((draft, index) => {
    const model = draft.model.trim();
    const provider = draft.provider.trim();
    const label = `第 ${index + 1} 条手工价格`;
    if (!model) throw new Error(`${label}的模型 ID 不能为空`);
    if (model.length > MAX_IDENTITY_LENGTH) throw new Error(`${label}的模型 ID 不能超过 1024 个字符`);
    if (provider.length > MAX_IDENTITY_LENGTH) throw new Error(`${label}的 Provider 不能超过 1024 个字符`);
    const identity = manualPriceIdentity(model, provider);
    if (seen[identity]) {
      throw new Error(`手工价格身份重复：${model}${provider ? ` / ${provider}` : '（空 Provider）'}`);
    }
    seen[identity] = true;
    return {
      model,
      ...(provider ? { provider } : {}),
      definition: parseDefinition(draft.definition, `${label}（${model}）`),
    };
  });
}

function normalizedRates(rates: RateSet): Record<string, unknown> {
  const ttls = rates.cache_write_by_ttl
    ? Object.fromEntries(Object.entries(rates.cache_write_by_ttl).sort(([a], [b]) => Number(a) - Number(b)))
    : undefined;
  return {
    input: rates.input,
    output: rates.output,
    cache_read: rates.cache_read,
    cache_write: rates.cache_write,
    cache_write_by_ttl: ttls ?? {},
    reasoning: rates.reasoning ?? null,
  };
}

function normalizedPatch(rates: RatePatch): Record<string, unknown> {
  const ttls = rates.cache_write_by_ttl
    ? Object.fromEntries(Object.entries(rates.cache_write_by_ttl).sort(([a], [b]) => Number(a) - Number(b)))
    : undefined;
  return {
    ...(rates.input !== undefined ? { input: rates.input } : {}),
    ...(rates.output !== undefined ? { output: rates.output } : {}),
    ...(rates.cache_read !== undefined ? { cache_read: rates.cache_read } : {}),
    ...(rates.cache_write !== undefined ? { cache_write: rates.cache_write } : {}),
    ...(ttls ? { cache_write_by_ttl: ttls } : {}),
    ...(rates.reasoning !== undefined ? { reasoning: rates.reasoning } : {}),
  };
}

function normalizedCondition(condition: Condition): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (condition.context) {
    const context: Record<string, number> = {};
    for (const key of ['gt', 'gte', 'lt', 'lte'] as const) {
      if (condition.context[key] !== undefined) context[key] = condition.context[key]!;
    }
    if (Object.keys(context).length) out.context = context;
  }
  if (condition.serviceTier !== undefined) {
    out.serviceTier = [...condition.serviceTier].sort();
  }
  if (condition.date?.gte || condition.date?.lt) {
    out.date = {
      ...(condition.date.gte ? { gte: condition.date.gte } : {}),
      ...(condition.date.lt ? { lt: condition.date.lt } : {}),
    };
  }
  if (condition.time) {
    out.time = {
      start: condition.time.start,
      end: condition.time.end,
      timezone: condition.time.timezone,
      ...(condition.time.weekdays?.length
        ? { weekdays: [...condition.time.weekdays].sort((a, b) => a - b) }
        : {}),
    };
  }
  if (Array.isArray(condition.all)) out.all = condition.all.map(normalizedCondition);
  if (Array.isArray(condition.any)) out.any = condition.any.map(normalizedCondition);
  if (condition.not) out.not = normalizedCondition(condition.not);
  return out;
}

function normalizedDefinition(definition: PricingDefinition): Record<string, unknown> {
  return {
    base: normalizedRates(definition.base),
    overrides: definition.overrides.map((entry) => ({
      id: entry.id,
      when: normalizedCondition(entry.when),
      rates: normalizedPatch(entry.rates),
    })),
    multipliers: definition.multipliers.map((entry) => ({
      id: entry.id,
      when: normalizedCondition(entry.when),
      factors: entry.factors,
    })),
  };
}

export function roundTripDefinition(definition: PricingDefinition): PricingDefinition {
  return parseDefinition(definitionToDraft(definition), '');
}

export function definitionsSemanticallyEqual(a: PricingDefinition, b: PricingDefinition): boolean {
  return JSON.stringify(normalizedDefinition(a)) === JSON.stringify(normalizedDefinition(b));
}

function normalizedPrice(price: ManualPrice): string {
  return JSON.stringify({
    model: price.model,
    provider: price.provider ?? '',
    definition: normalizedDefinition(price.definition),
  });
}

/**
 * Check if Pricing drafts differ from the confirmed configuration.
 */
export function isPricingDraftEqual(
  auto: boolean,
  source: PricingSource,
  manualDrafts: ManualPriceDraft[],
  confirmed: PricingConfig
): boolean {
  if (auto !== confirmed.auto || source !== confirmed.source) return false;

  let parsedPrices: ManualPrice[];
  try {
    parsedPrices = parseManualPrices(manualDrafts);
  } catch {
    return false;
  }
  const confirmedPrices = confirmed.prices || [];
  if (parsedPrices.length !== confirmedPrices.length) return false;
  for (let i = 0; i < parsedPrices.length; i++) {
    if (normalizedPrice(parsedPrices[i]) !== normalizedPrice(confirmedPrices[i])) return false;
  }
  return true;
}

/**
 * Generate a collision-free rule ID for a price definition.
 */
export function generateUniqueDefinitionID(
  existingIds: Iterable<string>,
  prefix: 'override' | 'multiplier'
): string {
  const set = new Set(existingIds);
  let id = '';
  let seq = 1;
  do {
    id = `${prefix}-${Date.now().toString(36)}-${seq.toString(36)}`;
    seq++;
  } while (set.has(id));
  return id;
}

/**
 * Move item up or down in an array, returning a new array.
 */
export function moveItem<T>(items: T[], fromIndex: number, direction: 'up' | 'down'): T[] {
  const toIndex = direction === 'up' ? fromIndex - 1 : fromIndex + 1;
  if (toIndex < 0 || toIndex >= items.length) return items;
  const next = [...items];
  const [removed] = next.splice(fromIndex, 1);
  next.splice(toIndex, 0, removed);
  return next;
}

/**
 * Check if General settings drafts differ.
 */
export function areGeneralSettingsEqual(a: GeneralSettingsDraft, b: GeneralSettingsDraft): boolean {
  return (
    a.capture_enabled === b.capture_enabled &&
    (!a.capture_enabled || (a.capture_mode ?? 'headers_and_body') === (b.capture_mode ?? 'headers_and_body')) &&
    (a.capture_retention_seconds ?? a.raw_retention_hours * 3600) ===
      (b.capture_retention_seconds ?? b.raw_retention_hours * 3600) &&
    a.max_body_bytes === b.max_body_bytes &&
    a.max_body_storage_bytes === b.max_body_storage_bytes &&
    a.raw_retention_hours === b.raw_retention_hours &&
    a.daily_retention_days === b.daily_retention_days
  );
}
