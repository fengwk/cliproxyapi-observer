import { useEffect, useMemo, useRef, useState } from 'react';
import type { SummaryBucket } from '../lib/types';
import { formatUTCShort } from './RequestsHelpers';

export interface RequestsTrendProps {
  series?: {
    granularity: 'hour' | 'day';
    buckets: SummaryBucket[];
  };
  loading?: boolean;
}

interface TimeTick {
  index: number;
  text: string;
  left: number;
  align: 'start' | 'center' | 'end';
}

const MIN_TICK_WIDTH = 130;

export function RequestsTrend({ series }: RequestsTrendProps) {
  const buckets = series?.buckets || [];
  const granularity = series?.granularity || 'day';
  const chartRef = useRef<HTMLDivElement>(null);
  const [chartWidth, setChartWidth] = useState(0);

  useEffect(() => {
    const el = chartRef.current;
    if (!el) return;
    const applyWidth = (next: number) => {
      setChartWidth((prev) => (prev === next ? prev : next));
    };
    const fallbackWidth = () => el.getBoundingClientRect().width || el.clientWidth || 0;
    const updateWidth = () => applyWidth(fallbackWidth());
    updateWidth();
    if (typeof ResizeObserver === 'undefined') {
      window.addEventListener('resize', updateWidth);
      return () => window.removeEventListener('resize', updateWidth);
    }
    const observer = new ResizeObserver((entries) => {
      const measured = entries[0]?.contentRect.width;
      applyWidth(measured && measured > 0 ? measured : fallbackWidth());
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const { reqPoints, failPoints, maxVal, hasData } = useMemo<{
    reqPoints: string;
    failPoints: string;
    maxVal: number;
    hasData: boolean;
  }>(() => {
    if (!buckets.length) {
      return { reqPoints: '', failPoints: '', maxVal: 0, hasData: false };
    }

    let max = 0;
    for (const b of buckets) {
      if ((b.requests || 0) > max) max = b.requests || 0;
      if ((b.failures || 0) > max) max = b.failures || 0;
    }

    if (max === 0) {
      return { reqPoints: '', failPoints: '', maxVal: 0, hasData: false };
    }

    const len = buckets.length;
    const reqPts: string[] = [];
    const failPts: string[] = [];

    buckets.forEach((b, i) => {
      const x = len === 1 ? 500 : 20 + (i * 960) / (len - 1);
      const reqY = 120 - ((b.requests || 0) * 100) / max;
      const failY = 120 - ((b.failures || 0) * 100) / max;
      reqPts.push(`${x.toFixed(1)},${reqY.toFixed(1)}`);
      failPts.push(`${x.toFixed(1)},${failY.toFixed(1)}`);
    });

    return {
      reqPoints: reqPts.join(' '),
      failPoints: failPts.join(' '),
      maxVal: max,
      hasData: true,
    };
  }, [buckets]);

  const timeTicks = useMemo<TimeTick[]>(() => {
    const len = buckets.length;
    if (!hasData || len === 0) return [];

    if (len === 1) {
      return [{
        index: 0,
        text: formatUTCShort(buckets[0].time, false),
        left: 50,
        align: 'center',
      }];
    }

    const maxTicks = Math.max(2, Math.min(8, Math.floor(chartWidth / MIN_TICK_WIDTH)));
    const tickCount = Math.min(len, maxTicks);
    const indices = new Set<number>();
    for (let i = 0; i < tickCount; i++) {
      indices.add(Math.round((i * (len - 1)) / (tickCount - 1)));
    }

    return Array.from(indices).map((index) => ({
      index,
      text: formatUTCShort(buckets[index].time, false),
      left: (index / (len - 1)) * 100,
      align: index === 0 ? 'start' : index === len - 1 ? 'end' : 'center',
    }));
  }, [buckets, chartWidth, hasData]);

  return (
    <div className="requests-trend-container" data-testid="requests-trend">
      <div className="requests-trend-header">
        <div className="requests-trend-title">
          <span>趋势图</span>
          <span className="requests-trend-granularity">
            ({granularity === 'hour' ? '按小时' : '按天'})
          </span>
        </div>
        <div className="requests-trend-legend">
          <span className="requests-legend-item">
            <span className="requests-legend-color requests-line-color" aria-hidden="true" />
            <span>请求数</span>
          </span>
          <span className="requests-legend-item">
            <span className="requests-legend-color failures-line-color" aria-hidden="true" />
            <span>失败数</span>
          </span>
          {maxVal > 0 && (
            <span className="requests-trend-meta">峰值: {maxVal.toLocaleString()}</span>
          )}
        </div>
      </div>

      <div className="requests-trend-svg-wrap" ref={chartRef}>
        {!hasData ? (
          <div className="requests-trend-empty">所选区间暂无趋势数据</div>
        ) : (
          <svg
            className="requests-trend-svg"
            viewBox="0 0 1000 135"
            preserveAspectRatio="none"
            aria-label="请求与失败趋势图"
            role="img"
          >
            {/* Horizontal reference grid lines */}
            <line x1="20" y1="20" x2="980" y2="20" stroke="var(--border-color)" strokeDasharray="4 4" strokeWidth="1" />
            <line x1="20" y1="70" x2="980" y2="70" stroke="var(--border-color)" strokeDasharray="4 4" strokeWidth="1" />
            <line x1="20" y1="120" x2="980" y2="120" stroke="var(--border-color)" strokeWidth="1" />

            {/* Requests line */}
            {reqPoints && (
              <polyline
                className="request-line"
                points={reqPoints}
                stroke="var(--text-primary)"
                fill="none"
                strokeWidth="2.5"
                strokeLinejoin="round"
                strokeLinecap="round"
              />
            )}

            {/* Failures line */}
            {failPoints && (
              <polyline
                className="failure-line"
                points={failPoints}
                stroke="var(--error-color)"
                fill="none"
                strokeWidth="2.5"
                strokeLinejoin="round"
                strokeLinecap="round"
              />
            )}
          </svg>
        )}
      </div>

      {hasData && (
        <div
          className="requests-trend-axis"
          data-testid="requests-trend-axis"
          role="group"
          aria-label="趋势时间轴"
        >
          {timeTicks.map((tick) => (
            <span
              key={`${tick.index}-${tick.text}`}
              className="requests-trend-axis-tick"
              data-testid="requests-trend-axis-tick"
              data-index={tick.index}
              style={{
                left: `${tick.left}%`,
                transform: tick.align === 'start'
                  ? undefined
                  : tick.align === 'end'
                  ? 'translateX(-100%)'
                  : 'translateX(-50%)',
              }}
            >
              {tick.text}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
