import type { Rates, Price, Rule, PricingConfig } from '../lib/types';
import { parseUSD, formatUSD } from '../lib/format';

export const MAX_SAFE_MICRO = 9_000_000_000_000_000;
export const DEFAULT_MAX_BODY_BYTES = 1_048_576; // 1 MiB
export const MAX_BODY_BYTES = 8_388_608; // 8 MiB
export const DEFAULT_MAX_BODY_STORAGE_BYTES = 268_435_456; // 256 MiB
export const MAX_BODY_STORAGE_BYTES = 1_073_741_824; // 1 GiB
export const MAX_CONFIG_ENTRIES = 4096;

export const MIN_RAW_RETENTION_HOURS = 1;
export const MAX_RAW_RETENTION_HOURS = 168; // 7 days
export const MIN_DAILY_RETENTION_DAYS = 1;
export const MAX_DAILY_RETENTION_DAYS = 3650; // 10 years

export interface GeneralSettingsDraft {
  capture_enabled: boolean;
  max_body_bytes: number;
  max_body_storage_bytes: number;
  raw_retention_hours: number;
  daily_retention_days: number;
}

export interface ManualPriceDraft {
  id: string; // internal client row id for React keys
  model: string;
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoning: string; // empty string = undefined, "0" = 0 micro-USD
  source?: string;
}

export interface RuleDraft {
  rowId: string; // internal client row id for React keys
  id: string;
  model: string;
  input_tokens_gt: string; // empty = undefined, numeric string = threshold
  time_range: string; // empty = undefined, "HH:mm-HH:mm" in UTC
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoning: string;
}

export type CapacityUnit = 'B' | 'KB' | 'MB' | 'GB';

export const UNIT_BYTES: Record<CapacityUnit, number> = {
  B: 1,
  KB: 1024,
  MB: 1048576,
  GB: 1073741824,
};

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
  const usd = formatUSD(microUsd);
  return `$${usd}`;
}

/**
 * Return user-friendly source label.
 */
export function sourceLabel(source: string): string {
  switch (source) {
    case 'manual':
      return '手工';
    case 'auto':
      return '自动目录';
    case 'rule':
      return '规则';
    case 'unpriced':
      return '未定价';
    default:
      return source || '—';
  }
}

/**
 * Validate UTC HH:mm-HH:mm time range with cross-midnight support.
 * Range is half-open [start, end), start and end must differ.
 */
export function validTimeRange(value: string): boolean {
  const trimmed = value.trim();
  const match = /^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$/.exec(trimmed);
  if (!match) return false;
  const start = trimmed.slice(0, 5);
  const end = trimmed.slice(6, 11);
  return start !== end;
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
  return null;
}

/**
 * Convert Rates into string values for form inputs.
 */
export function ratesToForm(rates: Rates): {
  input: string;
  output: string;
  cache_read: string;
  cache_write: string;
  reasoning: string;
} {
  return {
    input: formatUSD(rates.input ?? 0),
    output: formatUSD(rates.output ?? 0),
    cache_read: formatUSD(rates.cache_read ?? 0),
    cache_write: formatUSD(rates.cache_write ?? 0),
    reasoning: rates.reasoning !== undefined && rates.reasoning !== null ? formatUSD(rates.reasoning) : '',
  };
}

/**
 * Parse and validate rate inputs into Rates object.
 * Reasoning is optional: empty string means undefined; non-empty string is parsed.
 */
export function parseRatesInput(
  input: {
    input: string;
    output: string;
    cache_read: string;
    cache_write: string;
    reasoning?: string;
  },
  contextLabel: string
): Rates {
  let inMicro: number;
  let outMicro: number;
  let crMicro: number;
  let cwMicro: number;

  try {
    inMicro = parseUSD(input.input.trim());
  } catch {
    throw new Error(`${contextLabel}输入价格必须是有效的非负 USD 金额（最多 6 位小数）`);
  }
  try {
    outMicro = parseUSD(input.output.trim());
  } catch {
    throw new Error(`${contextLabel}输出价格必须是有效的非负 USD 金额（最多 6 位小数）`);
  }
  try {
    crMicro = parseUSD(input.cache_read.trim());
  } catch {
    throw new Error(`${contextLabel}缓存读价格必须是有效的非负 USD 金额（最多 6 位小数）`);
  }
  try {
    cwMicro = parseUSD(input.cache_write.trim());
  } catch {
    throw new Error(`${contextLabel}缓存写价格必须是有效的非负 USD 金额（最多 6 位小数）`);
  }

  for (const [name, val] of [
    ['输入价格', inMicro],
    ['输出价格', outMicro],
    ['缓存读价格', crMicro],
    ['缓存写价格', cwMicro],
  ] as const) {
    if (val < 0 || val > MAX_SAFE_MICRO) {
      throw new Error(`${contextLabel}${name}超出允许范围（0 至 $9,000,000,000）`);
    }
  }

  const result: Rates = {
    input: inMicro,
    output: outMicro,
    cache_read: crMicro,
    cache_write: cwMicro,
  };

  const reasoningRaw =
    input.reasoning !== undefined && input.reasoning !== null ? String(input.reasoning).trim() : '';
  if (reasoningRaw !== '') {
    let rMicro: number;
    try {
      rMicro = parseUSD(reasoningRaw);
    } catch {
      throw new Error(`${contextLabel}推理价格必须是有效的非负 USD 金额（最多 6 位小数）`);
    }
    if (rMicro < 0 || rMicro > MAX_SAFE_MICRO) {
      throw new Error(`${contextLabel}推理价格超出允许范围（0 至 $9,000,000,000）`);
    }
    result.reasoning = rMicro;
  }

  return result;
}

/**
 * Validate manual price rows and produce clean Price[] array.
 * Uses prototype-safe record to prevent __proto__ or constructor collisions.
 */
export function parseManualPrices(drafts: ManualPriceDraft[]): Price[] {
  if (drafts.length > MAX_CONFIG_ENTRIES) {
    throw new Error(`手工价格不能超过 ${MAX_CONFIG_ENTRIES} 条`);
  }

  const seenModels = Object.create(null);
  const result: Price[] = [];

  for (let i = 0; i < drafts.length; i++) {
    const d = drafts[i];
    const model = d.model.trim();
    const label = `第 ${i + 1} 条手工价格`;

    if (!model) {
      throw new Error(`${label}的模型 ID 不能为空`);
    }
    if (model.length > 1024) {
      throw new Error(`${label}的模型 ID 不能超过 1024 个字符`);
    }
    if (seenModels[model]) {
      throw new Error(`手工价格模型 ID 重复：${model}`);
    }
    seenModels[model] = true;

    const rates = parseRatesInput(d, `${label}（${model}）`);
    result.push({
      model,
      rates,
      source: d.source || 'manual',
    });
  }

  return result;
}

/**
 * Validate rules and produce clean Rule[] array.
 * Uses prototype-safe lookup to prevent __proto__ or constructor collisions.
 */
export function parseRules(drafts: RuleDraft[]): Rule[] {
  if (drafts.length > MAX_CONFIG_ENTRIES) {
    throw new Error(`规则不能超过 ${MAX_CONFIG_ENTRIES} 条`);
  }

  const seenIDs = Object.create(null);
  const result: Rule[] = [];

  for (let i = 0; i < drafts.length; i++) {
    const d = drafts[i];
    const id = d.id.trim();
    const model = d.model.trim();
    const label = `第 ${i + 1} 条规则`;

    if (!id) {
      throw new Error(`${label}的规则 ID 不能为空`);
    }
    if (id.length > 1024) {
      throw new Error(`${label}的规则 ID 不能超过 1024 个字符`);
    }
    if (seenIDs[id]) {
      throw new Error(`规则 ID 重复：${id}`);
    }
    seenIDs[id] = true;

    if (!model) {
      throw new Error(`${label}的模型 ID 不能为空`);
    }
    if (model.length > 1024) {
      throw new Error(`${label}的模型 ID 不能超过 1024 个字符`);
    }

    const rates = parseRatesInput(d, `${label}（${model}）`);
    const rule: Rule = {
      id,
      model,
      rates,
    };

    const thresholdStr =
      d.input_tokens_gt !== undefined && d.input_tokens_gt !== null ? String(d.input_tokens_gt).trim() : '';
    if (thresholdStr !== '') {
      if (!/^\d+$/.test(thresholdStr)) {
        throw new Error(`${label}的完整提示词 Token 阈值必须是非负整数`);
      }
      const val = Number(thresholdStr);
      if (!Number.isSafeInteger(val) || val < 0 || val > MAX_SAFE_MICRO) {
        throw new Error(`${label}的完整提示词 Token 阈值超出合法范围`);
      }
      rule.input_tokens_gt = val;
    }

    const rangeStr = d.time_range !== undefined && d.time_range !== null ? String(d.time_range).trim() : '';
    if (rangeStr !== '') {
      if (!validTimeRange(rangeStr)) {
        throw new Error(`${label}的时间区间必须是 UTC HH:mm-HH:mm，且起点与终点不同`);
      }
      rule.time_range = rangeStr;
    }

    result.push(rule);
  }

  return result;
}

/**
 * Generate a collision-free rule ID.
 */
export function generateUniqueRuleID(existingIds: Iterable<string>): string {
  const set = new Set(existingIds);
  let id = '';
  let seq = 1;
  do {
    id = `rule-${Date.now().toString(36)}-${seq.toString(36)}`;
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
 * Compare two Rates objects for value equality.
 * Treats undefined and null equivalently, but preserves explicit 0 as distinct from null/undefined.
 */
export function areRatesEqual(a?: Rates | null, b?: Rates | null): boolean {
  if (!a && !b) return true;
  if (!a || !b) return false;
  if (a.input !== b.input) return false;
  if (a.output !== b.output) return false;
  if (a.cache_read !== b.cache_read) return false;
  if (a.cache_write !== b.cache_write) return false;
  const aReasoning = a.reasoning !== undefined && a.reasoning !== null ? a.reasoning : undefined;
  const bReasoning = b.reasoning !== undefined && b.reasoning !== null ? b.reasoning : undefined;
  return aReasoning === bReasoning;
}

/**
 * Check if General settings drafts differ.
 */
export function areGeneralSettingsEqual(a: GeneralSettingsDraft, b: GeneralSettingsDraft): boolean {
  return (
    a.capture_enabled === b.capture_enabled &&
    a.max_body_bytes === b.max_body_bytes &&
    a.max_body_storage_bytes === b.max_body_storage_bytes &&
    a.raw_retention_hours === b.raw_retention_hours &&
    a.daily_retention_days === b.daily_retention_days
  );
}

/**
 * Check if Pricing drafts differ from confirmed PricingConfig.
 */
export function isPricingDraftEqual(
  auto: boolean,
  manualDrafts: ManualPriceDraft[],
  ruleDrafts: RuleDraft[],
  confirmed: PricingConfig
): boolean {
  if (auto !== confirmed.auto) return false;

  let parsedPrices: Price[];
  let parsedRules: Rule[];
  try {
    parsedPrices = parseManualPrices(manualDrafts);
    parsedRules = parseRules(ruleDrafts);
  } catch {
    // If it cannot parse cleanly, user has made in-progress edits that make it dirty
    return false;
  }

  const confirmedPrices = confirmed.prices || [];
  if (parsedPrices.length !== confirmedPrices.length) return false;
  for (let i = 0; i < parsedPrices.length; i++) {
    const p1 = parsedPrices[i];
    const p2 = confirmedPrices[i];
    if (p1.model !== p2.model) return false;
    if (!areRatesEqual(p1.rates, p2.rates)) return false;
  }

  const confirmedRules = confirmed.rules || [];
  if (parsedRules.length !== confirmedRules.length) return false;
  for (let i = 0; i < parsedRules.length; i++) {
    const r1 = parsedRules[i];
    const r2 = confirmedRules[i];
    if (r1.id !== r2.id) return false;
    if (r1.model !== r2.model) return false;
    if (r1.input_tokens_gt !== r2.input_tokens_gt) return false;
    if (r1.time_range !== r2.time_range) return false;
    if (!areRatesEqual(r1.rates, r2.rates)) return false;
  }

  return true;
}
