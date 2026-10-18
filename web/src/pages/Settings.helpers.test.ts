import { describe, it, expect } from 'vitest';
import {
  formatBytes,
  formatRateDisplay,
  validTimeRange,
  validateGeneralSettings,
  parseRatesInput,
  parseManualPrices,
  parseRules,
  generateUniqueRuleID,
  moveItem,
  areRatesEqual,
  isPricingDraftEqual,
  MAX_BODY_BYTES,
  MAX_BODY_STORAGE_BYTES,
  MIN_RAW_RETENTION_HOURS,
  MAX_RAW_RETENTION_HOURS,
  MIN_DAILY_RETENTION_DAYS,
  MAX_DAILY_RETENTION_DAYS,
} from './Settings.helpers';
import type { PricingConfig } from '../lib/types';

describe('Settings.helpers', () => {
  // Test formatBytes outputs correct human-friendly units
  it('formats byte numbers with appropriate SI/binary scale', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1048576)).toBe('1.0 MB');
    expect(formatBytes(268435456)).toBe('256.0 MB');
    expect(formatBytes(1073741824)).toBe('1.00 GB');
    expect(formatBytes(-10)).toBe('—');
  });

  // Test formatRateDisplay formats micro-USD correctly
  it('formats micro-USD rates into dollar strings with $ prefix', () => {
    expect(formatRateDisplay(0)).toBe('$0');
    expect(formatRateDisplay(150000)).toBe('$0.15');
    expect(formatRateDisplay(1234567)).toBe('$1.234567');
    expect(formatRateDisplay(undefined)).toBe('—');
    expect(formatRateDisplay(null)).toBe('—');
  });

  // Test storage bounds validation: 1GiB max storage, 8MiB max body, 168h raw, 3650d daily
  describe('validateGeneralSettings', () => {
    const baseValid = {
      capture_enabled: false,
      max_body_bytes: 1048576, // 1 MiB
      max_body_storage_bytes: 268435456, // 256 MiB
      raw_retention_hours: 24,
      daily_retention_days: 365,
    };

    it('accepts compliant default general settings', () => {
      expect(validateGeneralSettings(baseValid)).toBeNull();
    });

    it('enforces single body upper limit of 8 MiB (8,388,608 bytes)', () => {
      expect(validateGeneralSettings({ ...baseValid, max_body_bytes: MAX_BODY_BYTES })).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, max_body_bytes: MAX_BODY_BYTES + 1 })).toMatch(
        /8 MiB/
      );
      expect(validateGeneralSettings({ ...baseValid, max_body_bytes: 0 })).toMatch(/1 字节/);
    });

    it('enforces total storage upper limit of 1 GiB (1,073,741,824 bytes)', () => {
      expect(
        validateGeneralSettings({ ...baseValid, max_body_storage_bytes: MAX_BODY_STORAGE_BYTES })
      ).toBeNull();
      expect(
        validateGeneralSettings({
          ...baseValid,
          max_body_storage_bytes: MAX_BODY_STORAGE_BYTES + 1,
        })
      ).toMatch(/1 GiB/);
      expect(validateGeneralSettings({ ...baseValid, max_body_storage_bytes: 0 })).toMatch(
        /1 字节/
      );
    });

    it('enforces single body byte limit does not exceed total storage limit', () => {
      expect(
        validateGeneralSettings({
          ...baseValid,
          max_body_bytes: 5000,
          max_body_storage_bytes: 4000,
        })
      ).toMatch(/单个请求体上限不能超过请求体总存储上限/);
    });

    it('enforces raw retention bounds between 1 and 168 hours', () => {
      expect(
        validateGeneralSettings({ ...baseValid, raw_retention_hours: MIN_RAW_RETENTION_HOURS })
      ).toBeNull();
      expect(
        validateGeneralSettings({ ...baseValid, raw_retention_hours: MAX_RAW_RETENTION_HOURS })
      ).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, raw_retention_hours: 0 })).toMatch(
        /1 至 168 小时/
      );
      expect(validateGeneralSettings({ ...baseValid, raw_retention_hours: 169 })).toMatch(
        /1 至 168 小时/
      );
    });

    it('enforces daily retention bounds between 1 and 3650 days', () => {
      expect(
        validateGeneralSettings({ ...baseValid, daily_retention_days: MIN_DAILY_RETENTION_DAYS })
      ).toBeNull();
      expect(
        validateGeneralSettings({ ...baseValid, daily_retention_days: MAX_DAILY_RETENTION_DAYS })
      ).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, daily_retention_days: 0 })).toMatch(
        /1 至 3650 天/
      );
      expect(validateGeneralSettings({ ...baseValid, daily_retention_days: 3651 })).toMatch(
        /1 至 3650 天/
      );
    });
  });

  // Test UTC time range validation: half-open, cross-midnight, invalid formats
  describe('validTimeRange', () => {
    it('accepts valid standard daytime range', () => {
      expect(validTimeRange('08:00-18:00')).toBe(true);
      expect(validTimeRange('00:00-12:00')).toBe(true);
    });

    it('accepts valid cross-midnight range', () => {
      expect(validTimeRange('22:00-06:00')).toBe(true);
      expect(validTimeRange('23:30-00:30')).toBe(true);
    });

    it('rejects identical start and end times', () => {
      expect(validTimeRange('12:00-12:00')).toBe(false);
      expect(validTimeRange('00:00-00:00')).toBe(false);
    });

    it('rejects malformed syntax or out-of-range hours/minutes', () => {
      expect(validTimeRange('')).toBe(false);
      expect(validTimeRange('24:00-06:00')).toBe(false);
      expect(validTimeRange('12:60-14:00')).toBe(false);
      expect(validTimeRange('8:00-18:00')).toBe(false);
      expect(validTimeRange('08:00 - 18:00')).toBe(false);
    });
  });

  // Test parseRatesInput handles zero vs reasoning nullness
  describe('parseRatesInput', () => {
    it('parses valid rate inputs accurately to integer micro-USD', () => {
      const res = parseRatesInput(
        {
          input: '0.15',
          output: '0.6',
          cache_read: '0.075',
          cache_write: '0.15',
          reasoning: '0.3',
        },
        'Test '
      );
      expect(res).toEqual({
        input: 150000,
        output: 600000,
        cache_read: 75000,
        cache_write: 150000,
        reasoning: 300000,
      });
    });

    it('preserves blank reasoning as undefined and explicit "0" as 0 micro-USD', () => {
      const blankRes = parseRatesInput(
        {
          input: '0',
          output: '0',
          cache_read: '0',
          cache_write: '0',
          reasoning: '',
        },
        'Test '
      );
      expect(blankRes.reasoning).toBeUndefined();

      const zeroRes = parseRatesInput(
        {
          input: '0',
          output: '0',
          cache_read: '0',
          cache_write: '0',
          reasoning: '0',
        },
        'Test '
      );
      expect(zeroRes.reasoning).toBe(0);
    });

    it('rejects negative rates and invalid numbers', () => {
      expect(() =>
        parseRatesInput(
          {
            input: '-0.1',
            output: '0',
            cache_read: '0',
            cache_write: '0',
          },
          'Test '
        )
      ).toThrow();
    });
  });

  // Test parseManualPrices prototype-safety with __proto__ and constructor models
  describe('parseManualPrices prototype safety & duplicates', () => {
    it('correctly handles special constructor and __proto__ model names without crash or collision', () => {
      const drafts = [
        {
          id: '1',
          model: '__proto__',
          input: '0.1',
          output: '0.2',
          cache_read: '0.05',
          cache_write: '0.1',
          reasoning: '',
        },
        {
          id: '2',
          model: 'constructor',
          input: '0.3',
          output: '0.4',
          cache_read: '0.15',
          cache_write: '0.3',
          reasoning: '',
        },
      ];

      const res = parseManualPrices(drafts);
      expect(res).toHaveLength(2);
      expect(res[0].model).toBe('__proto__');
      expect(res[1].model).toBe('constructor');
    });

    it('detects duplicate model IDs accurately', () => {
      const drafts = [
        {
          id: '1',
          model: 'gpt-4o',
          input: '2.5',
          output: '10',
          cache_read: '1.25',
          cache_write: '2.5',
          reasoning: '',
        },
        {
          id: '2',
          model: 'gpt-4o',
          input: '5',
          output: '15',
          cache_read: '2.5',
          cache_write: '5',
          reasoning: '',
        },
      ];

      expect(() => parseManualPrices(drafts)).toThrow(/模型 ID 重复：gpt-4o/);
    });
  });

  // Test parseRules ordering and rule condition bounds
  describe('parseRules and ordering', () => {
    it('parses rules with conditions and retains ordering', () => {
      const drafts = [
        {
          rowId: 'row-1',
          id: 'r1',
          model: 'claude-3-5-sonnet',
          input_tokens_gt: '200000',
          time_range: '00:00-08:00',
          input: '1.5',
          output: '7.5',
          cache_read: '0.3',
          cache_write: '1.5',
          reasoning: '',
        },
        {
          rowId: 'row-2',
          id: 'r2',
          model: 'claude-3-5-sonnet',
          input_tokens_gt: '',
          time_range: '',
          input: '3',
          output: '15',
          cache_read: '0.3',
          cache_write: '3.75',
          reasoning: '',
        },
      ];

      const rules = parseRules(drafts);
      expect(rules).toHaveLength(2);
      expect(rules[0].id).toBe('r1');
      expect(rules[0].input_tokens_gt).toBe(200000);
      expect(rules[0].time_range).toBe('00:00-08:00');
      expect(rules[1].id).toBe('r2');
      expect(rules[1].input_tokens_gt).toBeUndefined();
      expect(rules[1].time_range).toBeUndefined();
    });

    it('rejects duplicate rule IDs including special names', () => {
      const drafts = [
        {
          rowId: '1',
          id: 'toString',
          model: 'model-a',
          input_tokens_gt: '',
          time_range: '',
          input: '1',
          output: '2',
          cache_read: '0',
          cache_write: '0',
          reasoning: '',
        },
        {
          rowId: '2',
          id: 'toString',
          model: 'model-b',
          input_tokens_gt: '',
          time_range: '',
          input: '1',
          output: '2',
          cache_read: '0',
          cache_write: '0',
          reasoning: '',
        },
      ];

      expect(() => parseRules(drafts)).toThrow(/规则 ID 重复：toString/);
    });

    it('generates unique non-colliding rule IDs', () => {
      const existing = ['rule-1', 'rule-2'];
      const newId = generateUniqueRuleID(existing);
      expect(newId).toMatch(/^rule-/);
      expect(existing).not.toContain(newId);
    });

    it('moves item up and down cleanly', () => {
      const arr = ['a', 'b', 'c'];
      expect(moveItem(arr, 1, 'up')).toEqual(['b', 'a', 'c']);
      expect(moveItem(arr, 1, 'down')).toEqual(['a', 'c', 'b']);
      expect(moveItem(arr, 0, 'up')).toEqual(['a', 'b', 'c']);
      expect(moveItem(arr, 2, 'down')).toEqual(['a', 'b', 'c']);
    });
  });

  // Test equality logic for dirty state tracking
  describe('areRatesEqual and isPricingDraftEqual', () => {
    it('compares rates accurately including reasoning nullness', () => {
      const r1 = { input: 100, output: 200, cache_read: 50, cache_write: 50, reasoning: undefined };
      const r2 = { input: 100, output: 200, cache_read: 50, cache_write: 50 };
      expect(areRatesEqual(r1, r2)).toBe(true);

      const r3 = { input: 100, output: 200, cache_read: 50, cache_write: 50, reasoning: 0 };
      expect(areRatesEqual(r1, r3)).toBe(false);
    });

    it('identifies clean vs dirty pricing drafts', () => {
      const confirmed: PricingConfig = {
        auto: true,
        prices: [
          {
            model: 'test/model',
            rates: { input: 1000000, output: 2000000, cache_read: 500000, cache_write: 500000 },
            source: 'manual',
          },
        ],
        rules: [],
      };

      const cleanDraft = [
        {
          id: '1',
          model: 'test/model',
          input: '1',
          output: '2',
          cache_read: '0.5',
          cache_write: '0.5',
          reasoning: '',
        },
      ];

      expect(isPricingDraftEqual(true, cleanDraft, [], confirmed)).toBe(true);

      const modifiedDraft = [
        {
          id: '1',
          model: 'test/model',
          input: '1.2',
          output: '2',
          cache_read: '0.5',
          cache_write: '0.5',
          reasoning: '',
        },
      ];
      expect(isPricingDraftEqual(true, modifiedDraft, [], confirmed)).toBe(false);
      expect(isPricingDraftEqual(false, cleanDraft, [], confirmed)).toBe(false);
    });
  });
});
