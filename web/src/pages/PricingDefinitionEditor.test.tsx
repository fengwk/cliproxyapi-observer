import { useState } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { PriceDefinitionEditor } from './PricingDefinitionEditor';
import { definitionToDraft, parseDefinition } from './Settings.helpers';
import type { DefinitionDraft } from './Settings.helpers';
import type { PricingDefinition } from '../lib/types';

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
});
