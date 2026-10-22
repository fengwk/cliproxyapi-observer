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
 * Returns the browser's current IANA timezone.
 * An empty environment (some embedded webviews) falls back to UTC, matching the
 * backend's missing-timezone semantics instead of sending an invalid value.
 */
export function getBrowserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

/**
 * Formats a numeric UTC offset in minutes east of UTC as `UTC+08:00`.
 * The explicit offset is what distinguishes both occurrences of a DST-repeated
 * local hour; a bare zone abbreviation is not sufficient in every locale.
 */
export function formatUTCOffset(offsetMinutesEast: number): string {
  if (offsetMinutesEast === 0) return 'UTC';
  const sign = offsetMinutesEast > 0 ? '+' : '-';
  const abs = Math.abs(offsetMinutesEast);
  const hours = String(Math.floor(abs / 60)).padStart(2, '0');
  const minutes = String(abs % 60).padStart(2, '0');
  return `UTC${sign}${hours}:${minutes}`;
}

/**
 * Formats an ISO timestamp to browser-local readable time with timezone info.
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
    hourCycle: 'h23',
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
 * Parses a `datetime-local` value as browser-local wall-clock time.
 * Nonexistent times (spring-forward gap) and repeated times (fall-back overlap)
 * are rejected instead of silently drifting to another instant.
 */
export function parseLocalDatetimeInput(
  inputVal: string
): { date: Date | null; error?: string } {
  const trimmed = inputVal.trim();
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(trimmed);
  if (!match) {
    return { date: null, error: '指定的过期时间无效' };
  }
  const [year, month, day, hours, minutes, seconds] = match
    .slice(1)
    .map((value) => (value === undefined ? 0 : Number(value)));
  const date = new Date(0);
  date.setFullYear(year, month - 1, day);
  date.setHours(hours, minutes, seconds, 0);

  const matchesWallClock = (candidate: Date) =>
    candidate.getFullYear() === year &&
    candidate.getMonth() === month - 1 &&
    candidate.getDate() === day &&
    candidate.getHours() === hours &&
    candidate.getMinutes() === minutes &&
    candidate.getSeconds() === seconds;

  if (!matchesWallClock(date)) {
    return { date: null, error: '该本地时间在时区切换时不存在，请选择其他时间' };
  }

  // A repeated wall-clock time maps to two distinct instants. Scan the nearby
  // transition window so a fall-back overlap cannot be applied silently.
  const start = date.getTime();
  const instants = new Set<number>();
  for (let delta = -180; delta <= 180; delta += 15) {
    const candidate = new Date(start + delta * 60000);
    if (matchesWallClock(candidate)) instants.add(candidate.getTime());
  }
  if (instants.size > 1) {
    return { date: null, error: '该本地时间在时区切换时重复，请避开重复时段' };
  }

  return { date };
}

/**
 * Returns true only when the control's exact minute-precision value equals the
 * stored instant's browser-local minute representation. Strings with trailing
 * junk, explicit seconds, or another wall-clock time must be parsed normally.
 */
export function isUnchangedLocalDatetime(
  originalISO: string | null | undefined,
  inputVal: string
): boolean {
  if (!originalISO) return false;
  return inputVal.trim() === toLocalDatetimeInput(originalISO);
}

/**
 * Converts a datetime-local input value to ISO string, while preserving the exact original
 * RFC3339 string (including nanoseconds) if its local wall-clock minute is unchanged.
 */
export function preserveOriginalExpiry(
  originalISO: string | null | undefined,
  inputVal: string
): string | null {
  const trimmed = inputVal.trim();
  if (!trimmed) {
    return null;
  }
  if (isUnchangedLocalDatetime(originalISO, trimmed)) {
    return originalISO ?? null;
  }
  const parsed = parseLocalDatetimeInput(trimmed);
  return parsed.date ? parsed.date.toISOString() : null;
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
