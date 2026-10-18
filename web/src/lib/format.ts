/**
 * Formatting and conversion utilities.
 */

const MAX_SAFE_MICRO_USD = 9000000000000000n;

/**
 * Parses a USD amount string into integer micro-USD.
 * Enforces non-negative numbers and at most 6 decimal places.
 * Throws on invalid input or overflow.
 */
export function parseUSD(value: string | number): number {
  const str = String(value ?? '').trim();
  if (!str) {
    return 0;
  }
  if (!/^\d+(\.\d{1,6})?$/.test(str)) {
    throw new Error('请输入非负美元金额，最多六位小数');
  }
  const [dollars, cents = ''] = str.split('.');
  const micros = BigInt(dollars) * 1000000n + BigInt(cents.padEnd(6, '0'));
  if (micros > MAX_SAFE_MICRO_USD) {
    throw new Error('金额超过安全上限');
  }
  return Number(micros);
}

/**
 * Formats integer micro-USD into an exact dollar string without a "$" symbol.
 * Strips unnecessary trailing decimal zeros.
 */
export function formatUSD(microUSD: number | bigint): string {
  if (microUSD === undefined || microUSD === null || isNaN(Number(microUSD))) {
    return '0';
  }
  const v = BigInt(Math.max(0, Math.floor(Number(microUSD))));
  const whole = v / 1000000n;
  const rem = v % 1000000n;
  if (rem === 0n) {
    return whole.toString();
  }
  const frac = rem.toString().padStart(6, '0').replace(/0+$/, '');
  return `${whole.toString()}.${frac}`;
}

/**
 * Safely extracts a readable error message from an unknown error value.
 */
export function errorMessage(error: unknown): string {
  if (!error) {
    return '未知错误';
  }
  if (typeof error === 'string') {
    return error;
  }
  if (error instanceof Error) {
    return error.message;
  }
  if (typeof error === 'object' && error !== null) {
    const errObj = error as Record<string, unknown>;
    if (typeof errObj.error === 'string' && errObj.error) {
      return errObj.error;
    }
    if (typeof errObj.message === 'string' && errObj.message) {
      return errObj.message;
    }
  }
  return String(error);
}

/**
 * Formats an ISO timestamp to local readable time with timezone info.
 */
export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) {
    return '';
  }
  const date = new Date(iso);
  if (isNaN(date.getTime())) {
    return String(iso);
  }
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    timeZoneName: 'short',
    hour12: false,
  }).format(date);
}

/**
 * Converts ISO RFC3339 string to datetime-local input format (YYYY-MM-DDTHH:mm).
 */
export function toLocalDatetimeInput(iso: string | null | undefined): string {
  if (!iso) {
    return '';
  }
  const date = new Date(iso);
  if (isNaN(date.getTime())) {
    return '';
  }
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  const hours = String(date.getHours()).padStart(2, '0');
  const minutes = String(date.getMinutes()).padStart(2, '0');
  return `${year}-${month}-${day}T${hours}:${minutes}`;
}

/**
 * Converts a datetime-local input value to ISO string, while preserving the exact original
 * RFC3339 string (including nanoseconds) if unchanged.
 */
export function preserveOriginalExpiry(
  originalISO: string | null | undefined,
  inputVal: string
): string | null {
  const trimmed = inputVal.trim();
  if (!trimmed) {
    return null;
  }
  const inputDate = new Date(trimmed);
  if (isNaN(inputDate.getTime())) {
    return null;
  }

  if (originalISO) {
    const originalDate = new Date(originalISO);
    // Compare up to minute precision since datetime-local inputs standard granularity is minute
    if (!isNaN(originalDate.getTime())) {
      const diffMs = Math.abs(inputDate.getTime() - originalDate.getTime());
      if (diffMs < 60000 && inputDate.getMinutes() === originalDate.getMinutes() && inputDate.getHours() === originalDate.getHours()) {
        return originalISO;
      }
    }
  }

  return inputDate.toISOString();
}

/**
 * Formats bytes to human-readable size.
 */
export function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  const val = bytes / Math.pow(1024, i);
  return `${val.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}
