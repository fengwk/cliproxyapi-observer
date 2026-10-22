import { describe, it, expect } from 'vitest';
import {
  parseUSD,
  formatUSD,
  errorMessage,
  preserveOriginalExpiry,
  toLocalDatetimeInput,
  formatDateTime,
  formatUTCOffset,
  getBrowserTimeZone,
  parseLocalDatetimeInput,
  isUnchangedLocalDatetime,
  formatBytes,
} from './format';

describe('format utilities', () => {
  describe('parseUSD', () => {
    it('parses valid USD strings to integer micro-USD', () => {
      expect(parseUSD('0')).toBe(0);
      expect(parseUSD('')).toBe(0);
      expect(parseUSD('1')).toBe(1000000);
      expect(parseUSD('1.5')).toBe(1500000);
      expect(parseUSD('0.000001')).toBe(1);
      expect(parseUSD('0.000123')).toBe(123);
      expect(parseUSD('100.99')).toBe(100990000);
    });

    it('rejects invalid, negative, or excessive precision inputs', () => {
      expect(() => parseUSD('-1')).toThrow('请输入非负美元金额，最多六位小数');
      expect(() => parseUSD('abc')).toThrow('请输入非负美元金额，最多六位小数');
      expect(() => parseUSD('1.1234567')).toThrow('请输入非负美元金额，最多六位小数');
      expect(() => parseUSD('9000000001')).toThrow('金额超过安全上限');
    });
  });

  describe('formatUSD', () => {
    it('formats micro-USD to exact dollar string without $ and trailing zeros', () => {
      expect(formatUSD(0)).toBe('0');
      expect(formatUSD(1000000)).toBe('1');
      expect(formatUSD(1500000)).toBe('1.5');
      expect(formatUSD(123)).toBe('0.000123');
      expect(formatUSD(100990000)).toBe('100.99');
      expect(formatUSD(NaN)).toBe('0');
    });
  });

  describe('local datetime input', () => {
    it('round-trips a stored instant through the browser-local input value', () => {
      const original = '2026-10-19T14:30:00.123456789Z';
      const input = toLocalDatetimeInput(original);
      expect(input).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
      expect(preserveOriginalExpiry(original, input)).toBe(original);
    });

    it('rejects malformed local input', () => {
      expect(parseLocalDatetimeInput('not-a-date').date).toBeNull();
      expect(parseLocalDatetimeInput('2026-13-01T00:00').date).toBeNull();
    });

    it('only treats the exact minute-precision control value as unchanged', () => {
      const original = '2026-10-19T14:30:00.123456789Z';
      const input = toLocalDatetimeInput(original);
      expect(isUnchangedLocalDatetime(original, input)).toBe(true);
      expect(isUnchangedLocalDatetime(original, ` ${input} `)).toBe(true);
      expect(isUnchangedLocalDatetime(original, `${input}junk`)).toBe(false);
      expect(isUnchangedLocalDatetime(original, `${input}:30`)).toBe(false);
    });

    it('exposes the browser IANA timezone and explicit numeric offsets', () => {
      expect(getBrowserTimeZone()).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC');
      expect(formatUTCOffset(480)).toBe('UTC+08:00');
      expect(formatUTCOffset(-300)).toBe('UTC-05:00');
      expect(formatUTCOffset(0)).toBe('UTC');
    });
  });

  describe('preserveOriginalExpiry', () => {
    it('preserves exact original RFC3339 nanoseconds when unchanged', () => {
      const original = '2026-10-19T14:30:00.123456789Z';
      const input = toLocalDatetimeInput(original);
      const preserved = preserveOriginalExpiry(original, input);
      expect(preserved).toBe(original);
    });

    it('generates new ISO string when changed', () => {
      const original = '2026-10-19T14:30:00.123456789Z';
      const input = '2026-10-20T18:00';
      const result = preserveOriginalExpiry(original, input);
      expect(result).not.toBe(original);
      expect(result).toContain('2026-10-20');
    });

    it('parses pseudo-input with trailing junk or explicit seconds instead of preserving', () => {
      const original = '2026-10-19T14:30:00.123456789Z';
      const input = toLocalDatetimeInput(original);
      expect(preserveOriginalExpiry(original, `${input}junk`)).toBeNull();
      const secondsChanged = preserveOriginalExpiry(original, `${input}:30`);
      expect(secondsChanged).not.toBe(original);
      expect(new Date(secondsChanged as string).getSeconds()).toBe(30);
    });

    it('returns null on empty input', () => {
      expect(preserveOriginalExpiry('2026-10-19T14:30:00Z', '')).toBeNull();
    });
  });

  describe('errorMessage', () => {
    it('extracts human-readable messages', () => {
      expect(errorMessage('网络异常')).toBe('网络异常');
      expect(errorMessage(new Error('拒绝访问'))).toBe('拒绝访问');
      expect(errorMessage({ error: 'Conflict' })).toBe('Conflict');
      expect(errorMessage({ message: 'Not found' })).toBe('Not found');
      expect(errorMessage(null)).toBe('未知错误');
    });
  });

  describe('formatDateTime & formatBytes', () => {
    it('formats date and bytes properly', () => {
      expect(formatDateTime(null)).toBe('');
      expect(formatDateTime('2026-10-19T00:00:00Z')).toBeTruthy();
      expect(formatBytes(0)).toBe('0 B');
      expect(formatBytes(1024)).toBe('1.0 KB');
      expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    });
  });
});
