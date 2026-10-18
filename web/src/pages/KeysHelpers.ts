import type { Limits } from '../lib/types';
import { parseUSD, errorMessage, preserveOriginalExpiry } from '../lib/format';

export interface QuotaDescriptor {
  key: keyof Limits;
  label: string;
  switchLabel: string;
  placeholder: string;
}

export const QUOTA_DESCRIPTORS: QuotaDescriptor[] = [
  {
    key: 'five_hour',
    label: '5小时额度',
    switchLabel: '启用5小时限额',
    placeholder: '金额 USD (如 5)',
  },
  {
    key: 'week',
    label: '每周额度',
    switchLabel: '启用每周限额',
    placeholder: '金额 USD (如 20)',
  },
  {
    key: 'month',
    label: '每月额度',
    switchLabel: '启用每月限额',
    placeholder: '金额 USD (如 80)',
  },
  {
    key: 'total',
    label: '累计总额度',
    switchLabel: '启用总额度限额',
    placeholder: '金额 USD (如 200)',
  },
];

/**
 * 校验自定义密钥。
 * 规则：8..256 可见 ASCII 字符 (0x21..0x7e)，绝不能包含空格、换行或非 ASCII 字符。
 * 绝不能对原文做 trim！
 */
export function validateCustomSecret(secret: string): { valid: boolean; error?: string } {
  if (secret.length < 8 || secret.length > 256) {
    return { valid: false, error: '自定义密钥长度必须在 8 到 256 位之间' };
  }
  if (!/^[\x21-\x7E]+$/.test(secret)) {
    return { valid: false, error: '自定义密钥只能包含可见 ASCII 字符且不能有空格或换行' };
  }
  return { valid: true };
}

/**
 * 校验并发数。
 * 必须为严格整数 1..1000000，拒绝小数（如 '2.5'）、非数字、<=0、>1000000。
 */
export function validateConcurrency(input: string): { valid: boolean; value?: number; error?: string } {
  const trimmed = input.trim();
  if (!trimmed || !/^\d+$/.test(trimmed)) {
    return { valid: false, error: '最大并发数必须为 1 到 1000000 之间的整数' };
  }
  const val = Number(trimmed);
  if (!Number.isSafeInteger(val) || val < 1 || val > 1000000) {
    return { valid: false, error: '最大并发数必须为 1 到 1000000 之间的整数' };
  }
  return { valid: true, value: val };
}

/**
 * 校验额度周期金额。
 * 用户开启限额且留空或输入 <= 0 时必须报错拒绝，避免默默发送 0 变为不限额。
 */
export function parseQuotaLimit(
  enabled: boolean,
  input: string,
  label: string
): { valid: boolean; value: number; error?: string } {
  if (!enabled) {
    return { valid: true, value: 0 };
  }
  const trimmed = input.trim();
  if (!trimmed) {
    return { valid: false, value: 0, error: `${label}已开启，金额不能为空且必须大于 0` };
  }
  try {
    const parsed = parseUSD(trimmed);
    if (parsed <= 0) {
      return { valid: false, value: 0, error: `${label}已开启，金额必须大于 0` };
    }
    return { valid: true, value: parsed };
  } catch (err) {
    return { valid: false, value: 0, error: errorMessage(err) };
  }
}

/**
 * 校验有效期。
 * 开启有效期但未指定日期时拒绝；未修改日期时保留原始 RFC3339 纳秒和时区。
 */
export function validateExpiry(
  enabled: boolean,
  inputVal: string,
  originalISO?: string | null
): { valid: boolean; iso: string | null; error?: string } {
  if (!enabled) {
    return { valid: true, iso: null };
  }
  const trimmed = inputVal.trim();
  if (!trimmed) {
    return { valid: false, iso: null, error: '开启有效期后必须指定到期时间' };
  }
  const inputDate = new Date(trimmed);
  if (isNaN(inputDate.getTime())) {
    return { valid: false, iso: null, error: '指定的过期时间无效' };
  }
  const preserved = preserveOriginalExpiry(originalISO, trimmed);
  return { valid: true, iso: preserved || inputDate.toISOString() };
}

/**
 * 仅通过 navigator.clipboard.writeText 写入剪贴板。
 * 移除 execCommand fallback，失败不静默，返回布尔值。
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) {
      return false;
    }
  }
  return false;
}

/**
 * 截断长 Key ID。
 */
export function maskId(id: string): string {
  if (id.length <= 16) return id;
  return `${id.slice(0, 8)}...${id.slice(-6)}`;
}
