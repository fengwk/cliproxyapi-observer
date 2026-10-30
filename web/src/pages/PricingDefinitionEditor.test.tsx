import { useState } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { PriceDefinitionEditor } from './PricingDefinitionEditor';
import { definitionToDraft, parseDefinition } from './Settings.helpers';
import type { DefinitionDraft } from './Settings.helpers';
import type { PricingDefinition } from '../lib/types';
import { formatDateTime } from '../lib/format';
import { RawJSONNumber, isRawJSONNumber } from '../lib/rawJson';

function Harness({ initial, onChange }: {
  initial: PricingDefinition;
  onChange: (draft: DefinitionDraft) => void;
}) {
  const [draft, setDraft] = useState(() => definitionToDraft(initial));
  return (
    <PriceDefinitionEditor
      label="测试"
      draft={draft}
      disabled={false}
      onChange={(next) => { setDraft(next); onChange(next); }}
    />
  );
}

describe('PriceDefinitionEditor semantic controls', () => {
  it('keeps an explicit empty TTL override after deleting the last tier and accepts 24:00', () => {
    const definition: PricingDefinition = {
      base: { input: 1, output: 2, cache_read: 0, cache_write: 0, cache_write_by_ttl: {}, reasoning: null },
      overrides: [{
        id: 'rule',
        when: { time: { start: '00:00', end: '24:00', timezone: 'UTC', weekdays: [1] } },
        rates: { cache_write_by_ttl: { '300': 7 } },
      }],
      multipliers: [],
    };
    // The form must not silently turn a 24:00 end into 00:00 (an empty window).
    const onChange = vi.fn();
    render(<Harness initial={definition} onChange={onChange} />);

    expect(screen.getByLabelText('测试 价格覆盖 1 时间结束')).toHaveValue('24:00');
    expect(screen.getByLabelText('测试 价格覆盖 1 TTL 模式')).toHaveTextContent('显式覆盖');
    fireEvent.click(screen.getByLabelText('测试 价格覆盖 1 TTL 覆盖 删除第 1 档 TTL'));

    const latest = onChange.mock.calls.at(-1)?.[0] as DefinitionDraft;
    expect(latest.overrides[0].rates.ttlMode).toBe('override');
    expect(latest.overrides[0].rates.ttls).toEqual([]);
    expect(parseDefinition(latest, '测试').overrides[0].rates.cache_write_by_ttl).toEqual({});
    expect(parseDefinition(latest, '测试').overrides[0].when.time?.end).toBe('24:00');
    expect(screen.getByText(/显式空覆盖/)).toBeInTheDocument();
  });

  it('previews UTC date boundaries in browser-local time without rewriting the saved UTC rule', () => {
    const definition: PricingDefinition = {
      base: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: null },
      overrides: [{
        id: 'date-rule',
        when: {
          date: { gte: '2026-10-20', lt: '2026-10-21' },
          time: { start: '08:00', end: '09:00', timezone: 'UTC' },
        },
        rates: { input: 2 },
      }],
      multipliers: [],
    };
    render(<Harness initial={definition} onChange={vi.fn()} />);

    const startLocal = formatDateTime('2026-10-20T00:00:00Z');
    const endLocal = formatDateTime('2026-10-21T00:00:00Z');
    expect(screen.getByText((content) => content.includes(`本地生效：从 ${startLocal} 起`))).toBeInTheDocument();
    expect(screen.getByText((content) => content.includes(`本地生效：至 ${endLocal} 前`))).toBeInTheDocument();
    expect(screen.getByLabelText('测试 价格覆盖 1 时区')).toHaveValue('UTC');
  });
});

describe('PriceDefinitionEditor request-body leaf', () => {
  it('edits path/type/value and preserves an exact numeric condition', () => {
    const exact = '123456789012345678901234567890.000000000000000001';
    const definition: PricingDefinition = {
      base: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: null },
      overrides: [{
        id: 'body-rule',
        when: { requestBody: { path: '/speed', equals: new RawJSONNumber(exact) } },
        rates: { input: 3 },
      }],
      multipliers: [],
    };
    const onChange = vi.fn();
    render(<Harness initial={definition} onChange={onChange} />);

    const path = screen.getByLabelText('测试 价格覆盖 1 请求体路径');
    const value = screen.getByLabelText('测试 价格覆盖 1 请求体值');
    expect(path).toHaveValue('/speed');
    expect(value).toHaveValue(exact);
    expect(screen.getByLabelText('测试 价格覆盖 1 请求体值类型')).toHaveTextContent('数字');
    expect(screen.getByText(/客户端控制/)).toBeInTheDocument();

    fireEvent.change(path, { target: { value: '/options/0/mode' } });
    fireEvent.change(value, { target: { value: '-2.500' } });
    const latest = onChange.mock.calls.at(-1)?.[0] as DefinitionDraft;
    const parsed = parseDefinition(latest, '测试').overrides[0].when.requestBody;
    expect(parsed?.path).toBe('/options/0/mode');
    expect(isRawJSONNumber(parsed?.equals)).toBe(true);
    expect((parsed?.equals as RawJSONNumber).text).toBe('-2.5');
  });

  it('adds and removes the structured leaf without a raw expression language', () => {
    const definition: PricingDefinition = {
      base: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: null },
      overrides: [{ id: 'rule', when: {}, rates: { input: 2 } }],
      multipliers: [],
    };
    const onChange = vi.fn();
    render(<Harness initial={definition} onChange={onChange} />);
    fireEvent.click(screen.getByTestId('condition-request-body-add'));
    expect(screen.getByLabelText('测试 价格覆盖 1 请求体路径')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('condition-request-body-remove'));
    expect(screen.queryByLabelText('测试 价格覆盖 1 请求体路径')).not.toBeInTheDocument();
  });
});
