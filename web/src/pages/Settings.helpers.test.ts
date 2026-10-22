import { describe, it, expect } from 'vitest';
import {
  MAX_BODY_BYTES,
  MAX_BODY_STORAGE_BYTES,
  MAX_DAILY_RETENTION_DAYS,
  MAX_RAW_RETENTION_HOURS,
  MIN_DAILY_RETENTION_DAYS,
  MIN_RAW_RETENTION_HOURS,
  conditionSummary,
  conditionToDraft,
  definitionToDraft,
  definitionsSemanticallyEqual,
  emptyConditionDraft,
  emptyDefinitionDraft,
  formatRateDisplay,
  formatReasoningDisplay,
  formatTtlSeconds,
  generateUniqueDefinitionID,
  isPricingDraftEqual,
  moveItem,
  parseCapacityToBytes,
  parseDefinition,
  parseManualPrices,
  parseTtlSeconds,
  pricingSourceLabel,
  rateSetToDraft,
  roundTripDefinition,
  sourceLabel,
  splitBytesToCapacity,
  validTimeRange,
  validateGeneralSettings,
} from './Settings.helpers';
import type {
  Condition,
  ManualPrice,
  PricingConfig,
  PricingDefinition,
  RateSet,
} from '../lib/types';
import { getBrowserTimeZone } from '../lib/format';

function baseDefinition(overrides: Partial<RateSet> = {}): PricingDefinition {
  return {
    base: {
      input: 1_000_000,
      output: 2_000_000,
      cache_read: 100_000,
      cache_write: 1_000_000,
      reasoning: null,
      ...overrides,
    },
    overrides: [],
    multipliers: [],
  };
}

function manualDraft(
  model: string,
  provider: string,
  definition: PricingDefinition = baseDefinition()
) {
  return {
    id: `${model}-${provider}`,
    model,
    provider,
    definition: definitionToDraft(definition),
  };
}

describe('Settings.helpers', () => {
  describe('display and labels', () => {
    it('formats exact micro-USD and reasoning inheritance without floating point loss', () => {
      expect(formatRateDisplay(0)).toBe('$0');
      expect(formatRateDisplay(150000)).toBe('$0.15');
      expect(formatRateDisplay(1234567)).toBe('$1.234567');
      expect(formatRateDisplay(undefined)).toBe('—');
      expect(formatReasoningDisplay(null)).toBe('同输出');
      expect(formatReasoningDisplay(undefined)).toBe('同输出');
      expect(formatReasoningDisplay(0)).toBe('$0');
      expect(sourceLabel('manual')).toBe('手工价格');
      expect(sourceLabel('auto')).toBe('自动目录');
      expect(sourceLabel('zero')).toBe('零价格');
      expect(pricingSourceLabel('models_dev_v2')).toBe('models.dev v2');
      expect(pricingSourceLabel('models_dev_v1')).toBe('models.dev v1');
    });

    it('validates half-open time ranges including cross-midnight', () => {
      expect(validTimeRange('08:00-18:00')).toBe(true);
      expect(validTimeRange('22:00-06:00')).toBe(true);
      expect(validTimeRange('12:00-12:00')).toBe(false);
      expect(validTimeRange('24:00-06:00')).toBe(false);
      expect(validTimeRange('00:00-24:00')).toBe(true);
      expect(validTimeRange('08:00-24:00')).toBe(true);
      expect(validTimeRange('8:00-18:00')).toBe(false);
    });
  });

  describe('general settings', () => {
    const baseValid = {
      capture_enabled: false,
      max_body_bytes: 1048576,
      max_body_storage_bytes: 268435456,
      raw_retention_hours: 24,
      daily_retention_days: 365,
    };

    it('accepts defaults and enforces body, storage and retention limits', () => {
      expect(validateGeneralSettings(baseValid)).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, max_body_bytes: MAX_BODY_BYTES + 1 })).toMatch(/8 MiB/);
      expect(validateGeneralSettings({ ...baseValid, max_body_storage_bytes: MAX_BODY_STORAGE_BYTES + 1 })).toMatch(/1 GiB/);
      expect(validateGeneralSettings({ ...baseValid, max_body_bytes: 5000, max_body_storage_bytes: 4000 })).toMatch(/不能超过/);
      expect(validateGeneralSettings({ ...baseValid, raw_retention_hours: MIN_RAW_RETENTION_HOURS })).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, raw_retention_hours: MAX_RAW_RETENTION_HOURS + 1 })).toMatch(/168/);
      expect(validateGeneralSettings({ ...baseValid, daily_retention_days: MIN_DAILY_RETENTION_DAYS })).toBeNull();
      expect(validateGeneralSettings({ ...baseValid, daily_retention_days: MAX_DAILY_RETENTION_DAYS + 1 })).toMatch(/3650/);
    });

    it('converts capacity to exact integer bytes and rejects lossy fractions', () => {
      expect(splitBytesToCapacity(1073741824)).toEqual({ amount: '1', unit: 'GB' });
      expect(splitBytesToCapacity(268435456)).toEqual({ amount: '256', unit: 'MB' });
      expect(splitBytesToCapacity(1048577)).toEqual({ amount: '1048577', unit: 'B' });
      expect(parseCapacityToBytes('0.5', 'KB', '上限')).toBe(512);
      expect(() => parseCapacityToBytes('0.3', 'KB', '上限')).toThrow(/整数字节/);
      expect(() => parseCapacityToBytes('0', 'MB', '上限')).toThrow(/超出允许范围/);
    });
  });

  describe('TTL and reasoning drafts', () => {
    it('parses human TTL values to positive integer seconds and formats them back', () => {
      expect(parseTtlSeconds('5m', 'TTL')).toBe(300);
      expect(parseTtlSeconds('1h', 'TTL')).toBe(3600);
      expect(parseTtlSeconds('1h30m', 'TTL')).toBe(5400);
      expect(parseTtlSeconds('3600', 'TTL')).toBe(3600);
      expect(() => parseTtlSeconds('0', 'TTL')).toThrow(/正整数/);
      expect(formatTtlSeconds(300)).toBe('5m');
      expect(formatTtlSeconds(5400)).toBe('1h30m');
      expect(formatTtlSeconds(90061)).toBe('1d1h1m1s');
    });

    it('preserves explicit zero reasoning and distinguishes inherited reasoning', () => {
      const inherited = rateSetToDraft(baseDefinition().base);
      expect(inherited.reasoningMode).toBe('inherit');
      expect(inherited.reasoning).toBe('');

      const custom = rateSetToDraft(baseDefinition({ reasoning: 0 }).base);
      expect(custom.reasoningMode).toBe('custom');
      expect(custom.reasoning).toBe('0');
    });
  });

  describe('pricing definitions', () => {
    it('parses exact micro-USD, inherited reasoning, ordered overrides, TTL and multipliers', () => {
      const draft = emptyDefinitionDraft();
      draft.base.input = '0.000001';
      draft.base.output = '1.234567';
      draft.base.cache_read = '0.5';
      draft.base.cache_write = '2';
      draft.base.ttls = [
        { rowId: 'ttl-1', seconds: '5m', price: '0.25' },
        { rowId: 'ttl-2', seconds: '1h', price: '1.5' },
      ];
      draft.overrides = [
        {
          rowId: 'override-1',
          id: 'off-peak',
          condition: {
            ...emptyConditionDraft(),
            context: { lowerOp: 'gt', lowerValue: '200000', upperOp: 'lte', upperValue: '500000' },
            serviceTier: 'default, flex, default',
            dateGte: '2026-01-01',
            dateLt: '2026-12-31',
            timeStart: '22:00',
            timeEnd: '06:00',
            timezone: 'Asia/Shanghai',
            weekdays: [7, 1, 1],
          },
          rates: {
            input: '0.75',
            output: '',
            cache_read: '',
            cache_write: '',
            reasoningMode: 'same_output',
            reasoning: '',
            ttlMode: 'inherit',
            ttls: [],
          },
        },
      ];
      draft.multipliers = [
        {
          rowId: 'multiplier-1',
          id: 'discount',
          condition: { ...emptyConditionDraft(), serviceTier: 'flex' },
          factors: {
            input: '0.5',
            output: '1.0000000000000001',
            cache_read: '',
            cache_write: '',
            reasoning: '0',
          },
        },
      ];

      const definition = parseDefinition(draft, '测试 ');
      expect(definition.base).toEqual({
        input: 1,
        output: 1234567,
        cache_read: 500000,
        cache_write: 2000000,
        cache_write_by_ttl: { '300': 250000, '3600': 1500000 },
        reasoning: null,
      });
      expect(definition.overrides).toHaveLength(1);
      expect(definition.overrides[0]).toEqual({
        id: 'off-peak',
        when: {
          context: { gt: 200000, lte: 500000 },
          serviceTier: ['default', 'flex'],
          date: { gte: '2026-01-01', lt: '2026-12-31' },
          time: {
            start: '22:00',
            end: '06:00',
            timezone: 'Asia/Shanghai',
            weekdays: [1, 7],
          },
        },
        rates: { input: 750000, reasoning: null },
      });
      expect(definition.multipliers[0]).toEqual({
        id: 'discount',
        when: { serviceTier: ['flex'] },
        factors: { input: '0.5', output: '1.0000000000000001', reasoning: '0' },
      });
    });

    it('round-trips nested all/any/not conditions and describes them in the editor', () => {
      expect(emptyConditionDraft().timezone).toBe(getBrowserTimeZone());
      const condition: Condition = {
        any: [
          { serviceTier: ['priority'] },
          { not: { context: { gte: 100 } } },
        ],
        all: [{ time: { start: '00:00', end: '08:00', timezone: 'UTC', weekdays: [1] } }],
      };
      const draft = emptyDefinitionDraft();
      draft.overrides = [
        {
          rowId: 'override-nested',
          id: 'nested',
          condition: conditionToDraft(condition),
          rates: {
            input: '1',
            output: '',
            cache_read: '',
            cache_write: '',
            reasoningMode: 'inherit',
            reasoning: '',
            ttlMode: 'inherit',
            ttls: [],
          },
        },
      ];
      const parsed = parseDefinition(draft, '测试 ');
      expect(parsed.overrides[0].when).toEqual(condition);
      expect(conditionSummary(conditionToDraft(condition))).toContain('全部满足 1 组');
      expect(conditionSummary(conditionToDraft(condition))).toContain('任一满足 2 组');
    });

    it('rejects malformed conditions, duplicate rule IDs and invalid multipliers', () => {
      const badTimezone = emptyDefinitionDraft();
      badTimezone.overrides = [
        {
          rowId: 'o1',
          id: 'o1',
          condition: {
            ...emptyConditionDraft(),
            timeStart: '08:00',
            timeEnd: '09:00',
            timezone: 'Not/A_Zone',
          },
          rates: {
            input: '',
            output: '',
            cache_read: '',
            cache_write: '',
            reasoningMode: 'inherit',
            reasoning: '',
            ttlMode: 'inherit',
            ttls: [],
          },
        },
      ];
      expect(() => parseDefinition(badTimezone, '测试 ')).toThrow(/IANA/);

      const badContext = emptyDefinitionDraft();
      badContext.overrides = [
        {
          rowId: 'o1',
          id: 'o1',
          condition: { ...emptyConditionDraft(), context: { lowerOp: '', lowerValue: '5', upperOp: '', upperValue: '' } },
          rates: {
            input: '',
            output: '',
            cache_read: '',
            cache_write: '',
            reasoningMode: 'inherit',
            reasoning: '',
            ttlMode: 'inherit',
            ttls: [],
          },
        },
      ];
      expect(() => parseDefinition(badContext, '测试 ')).toThrow(/缺少比较方式/);

      const duplicateIds = emptyDefinitionDraft();
      const blankRates = {
        input: '',
        output: '',
        cache_read: '',
        cache_write: '',
        reasoningMode: 'inherit' as const,
        reasoning: '',
        ttlMode: 'inherit' as const,
        ttls: [],
      };
      duplicateIds.overrides = [
        { rowId: 'o1', id: 'same', condition: emptyConditionDraft(), rates: blankRates },
        { rowId: 'o2', id: 'same', condition: emptyConditionDraft(), rates: blankRates },
      ];
      expect(() => parseDefinition(duplicateIds, '测试 ')).toThrow(/规则 ID 重复/);

      const duplicateTtl = emptyDefinitionDraft();
      duplicateTtl.base.ttls = [
        { rowId: 't1', seconds: '60', price: '1' },
        { rowId: 't2', seconds: '1m', price: '2' },
      ];
      expect(() => parseDefinition(duplicateTtl, '测试 ')).toThrow(/TTL 档位重复/);

      const badMultiplier = emptyDefinitionDraft();
      badMultiplier.multipliers = [
        {
          rowId: 'm1',
          id: 'm1',
          condition: emptyConditionDraft(),
          factors: { input: '1e-3', output: '', cache_read: '', cache_write: '', reasoning: '' },
        },
      ];
      expect(() => parseDefinition(badMultiplier, '测试 ')).toThrow(/十进制字符串/);
    });
  });

  describe('manual prices', () => {
    it('uses the complete model/provider identity and preserves explicit zero reasoning', () => {
      const zeroReasoning = baseDefinition({ reasoning: 0 });
      const prices = parseManualPrices([
        manualDraft('shared/model', '', zeroReasoning),
        manualDraft('shared/model', 'openai', baseDefinition()),
      ]);
      expect(prices).toEqual([
        {
          model: 'shared/model',
          definition: expect.objectContaining({
            base: expect.objectContaining({ reasoning: 0 }),
          }),
        },
        {
          model: 'shared/model',
          provider: 'openai',
          definition: expect.objectContaining({
            base: expect.objectContaining({ reasoning: null }),
          }),
        },
      ]);
      expect(() =>
        parseManualPrices([
          manualDraft('shared/model', 'openai'),
          manualDraft('shared/model', 'openai'),
        ])
      ).toThrow(/身份重复/);
    });

    it('handles prototype-like model names without collisions', () => {
      const prices = parseManualPrices([
        manualDraft('__proto__', ''),
        manualDraft('constructor', ''),
      ]);
      expect(prices.map((price) => price.model)).toEqual(['__proto__', 'constructor']);
    });

    it('formats manual prices in the layout expected by the settings form', () => {
      const draft = manualDraft('model-a', 'provider-a');
      const parsed = parseManualPrices([draft]);
      expect(parsed[0]).toMatchObject({ model: 'model-a', provider: 'provider-a' });
      expect(parsed[0].definition.base).toMatchObject({ input: 1000000, output: 2000000 });
    });
  });

  describe('dirty comparison and ordering', () => {
    it('detects source, identity and definition changes against confirmed pricing', () => {
      const confirmed: PricingConfig = {
        auto: true,
        source: 'models_dev_v2',
        prices: parseManualPrices([manualDraft('test/model', '')]),
      };
      const clean = [manualDraft('test/model', '')];
      expect(isPricingDraftEqual(true, 'models_dev_v2', clean, confirmed)).toBe(true);
      expect(isPricingDraftEqual(false, 'models_dev_v2', clean, confirmed)).toBe(false);
      expect(isPricingDraftEqual(true, 'models_dev_v1', clean, confirmed)).toBe(false);

      const changedIdentity = [manualDraft('test/model', 'openai')];
      expect(isPricingDraftEqual(true, 'models_dev_v2', changedIdentity, confirmed)).toBe(false);

      const changedDefinition = [manualDraft('test/model', '', baseDefinition({ input: 1 }))];
      expect(isPricingDraftEqual(true, 'models_dev_v2', changedDefinition, confirmed)).toBe(false);
    });

    it('round-trips explicit empty TTL overrides, empty condition collections and 24:00', () => {
      const source: PricingDefinition = {
        base: {
          input: 1, output: 2, cache_read: 3, cache_write: 4,
          cache_write_by_ttl: {}, reasoning: null,
        },
        overrides: [
          {
            id: 'clear-ttl',
            when: { serviceTier: [], all: [], any: [] },
            rates: { cache_write_by_ttl: {} },
          },
          {
            id: 'all-day',
            when: { time: { start: '00:00', end: '24:00', timezone: 'UTC', weekdays: [1] } },
            rates: { input: 5 },
          },
        ],
        multipliers: [],
      };
      const roundTripped = roundTripDefinition(source);
      // Empty patch TTL means clear, and empty any means false; neither may collapse to omission.
      expect(roundTripped.overrides[0].rates.cache_write_by_ttl).toEqual({});
      expect(roundTripped.overrides[0].when).toEqual({ serviceTier: [], all: [], any: [] });
      expect(roundTripped.overrides[1].when.time?.end).toBe('24:00');
      expect(definitionsSemanticallyEqual(source, roundTripped)).toBe(true);
    });

    it('moves rules and generates collision-free IDs', () => {
      expect(moveItem(['a', 'b', 'c'], 1, 'up')).toEqual(['b', 'a', 'c']);
      expect(moveItem(['a', 'b', 'c'], 1, 'down')).toEqual(['a', 'c', 'b']);
      const id = generateUniqueDefinitionID(['override-1', 'override-2'], 'override');
      expect(id).toMatch(/^override-/);
      expect(['override-1', 'override-2']).not.toContain(id);
    });

    it('maps both context bounds and nested conditions without loss', () => {
      const source: Condition = {
        context: { gt: 10, lte: 99 },
        serviceTier: ['default'],
        any: [{ not: { serviceTier: ['flex'] } }],
      };
      const draft = conditionToDraft(source);
      expect(draft.context).toEqual({
        lowerOp: 'gt',
        lowerValue: '10',
        upperOp: 'lte',
        upperValue: '99',
      });
      expect(draft.any[0].not?.serviceTier).toBe('flex');

      const definition: PricingDefinition = {
        ...baseDefinition(),
        overrides: [
          {
            id: 'rule',
            when: source,
            rates: { input: 1 },
          },
        ],
      };
      const parsed = parseDefinition(definitionToDraft(definition), '测试 ');
      expect(parsed.overrides[0].when).toEqual(source);
      const manual: ManualPrice[] = parseManualPrices([manualDraft('m', '', definition)]);
      expect(manual[0].definition.overrides[0].when).toEqual(source);
    });
  });
});
