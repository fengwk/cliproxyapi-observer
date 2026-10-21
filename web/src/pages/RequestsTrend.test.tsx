import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { act, render, screen } from '@testing-library/react';
import type { SummaryBucket } from '../lib/types';
import { RequestsTrend } from './RequestsTrend';

type ResizeCallback = ConstructorParameters<typeof ResizeObserver>[0];
let resizeCallback: ResizeCallback | null = null;

class MockResizeObserver {
  constructor(callback: ResizeCallback) {
    resizeCallback = callback;
  }
  observe() {}
  unobserve() {}
  disconnect() {
    resizeCallback = null;
  }
}

function setWidth(width: number) {
  act(() => {
    resizeCallback?.(
      [{ contentRect: { width } } as ResizeObserverEntry],
      {} as ResizeObserver
    );
  });
}

function makeBuckets(count: number): SummaryBucket[] {
  return Array.from({ length: count }, (_, index) => ({
    time: new Date(Date.UTC(2026, 9, 1 + index)).toISOString(),
    requests: index + 1,
    failures: index % 2,
    cost_micro_usd: 0,
    tokens: 0,
  }));
}

function tickCount() {
  return screen.queryAllByTestId('requests-trend-axis-tick').length;
}

describe('RequestsTrend time axis', () => {
  beforeEach(() => {
    resizeCallback = null;
    vi.stubGlobal('ResizeObserver', MockResizeObserver);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // Wide charts add ticks beyond the old fixed first/middle/last labels, while
  // all time text lives in plain HTML rather than the horizontally stretched SVG.
  it('renders adaptive HTML ticks outside the stretched SVG on wide charts', () => {
    render(<RequestsTrend series={{ granularity: 'day', buckets: makeBuckets(12) }} />);
    setWidth(900);

    expect(tickCount()).toBe(6);
    const svg = screen.getByRole('img', { name: '请求与失败趋势图' });
    expect(svg.querySelectorAll('text')).toHaveLength(0);

    const axis = screen.getByTestId('requests-trend-axis');
    expect(axis.closest('svg')).toBeNull();
    expect(axis).toHaveTextContent('10-01 00:00 UTC');
    expect(axis).toHaveTextContent('10-12 00:00 UTC');
  });

  it('reduces ticks on narrow charts and centers a single bucket label', () => {
    const { rerender } = render(
      <RequestsTrend series={{ granularity: 'day', buckets: makeBuckets(12) }} />
    );
    setWidth(300);
    expect(tickCount()).toBe(2);

    const ticks = screen.getAllByTestId('requests-trend-axis-tick');
    expect(ticks[0]).toHaveStyle({ left: '0%' });
    expect(ticks[ticks.length - 1]).toHaveStyle({ left: '100%' });

    rerender(<RequestsTrend series={{ granularity: 'day', buckets: makeBuckets(1) }} />);
    expect(tickCount()).toBe(1);
    const single = screen.getByTestId('requests-trend-axis-tick');
    expect(single).toHaveStyle({ left: '50%', transform: 'translateX(-50%)' });
    expect(single).toHaveTextContent('10-01 00:00 UTC');
  });

  it('shows an honest empty state without axis ticks when there is no data', () => {
    render(<RequestsTrend series={{ granularity: 'day', buckets: [] }} />);
    expect(screen.getByText('所选区间暂无趋势数据')).toBeInTheDocument();
    expect(screen.queryByTestId('requests-trend-axis')).not.toBeInTheDocument();
    expect(tickCount()).toBe(0);
  });
});
