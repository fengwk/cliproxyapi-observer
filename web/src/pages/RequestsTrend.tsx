import { useMemo } from 'react';
import type { SummaryBucket } from '../lib/types';
import { formatUTCShort } from './RequestsHelpers';

export interface RequestsTrendProps {
  series?: {
    granularity: 'hour' | 'day';
    buckets: SummaryBucket[];
  };
  loading?: boolean;
}

interface TimeLabel {
  text: string;
  x: number;
}

export function RequestsTrend({ series }: RequestsTrendProps) {
  const buckets = series?.buckets || [];
  const granularity = series?.granularity || 'day';

  const { reqPoints, failPoints, maxVal, hasData, timeLabels } = useMemo<{
    reqPoints: string;
    failPoints: string;
    maxVal: number;
    hasData: boolean;
    timeLabels: TimeLabel[];
  }>(() => {
    if (!buckets.length) {
      return { reqPoints: '', failPoints: '', maxVal: 0, hasData: false, timeLabels: [] };
    }

    let max = 0;
    for (const b of buckets) {
      if ((b.requests || 0) > max) max = b.requests || 0;
      if ((b.failures || 0) > max) max = b.failures || 0;
    }

    const len = buckets.length;
    const labels: TimeLabel[] = [];
    if (len > 0) {
      labels.push({
        text: formatUTCShort(buckets[0].time, false),
        x: 20,
      });
      if (len > 2) {
        const midIdx = Math.floor(len / 2);
        labels.push({
          text: formatUTCShort(buckets[midIdx].time, false),
          x: 500,
        });
      }
      if (len > 1) {
        labels.push({
          text: formatUTCShort(buckets[len - 1].time, false),
          x: 980,
        });
      }
    }

    if (max === 0) {
      return {
        reqPoints: '',
        failPoints: '',
        maxVal: 0,
        hasData: false,
        timeLabels: labels,
      };
    }

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
      timeLabels: labels,
    };
  }, [buckets]);

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

      <div className="requests-trend-svg-wrap">
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

            {/* Time labels along the bottom */}
            {timeLabels.map((lbl, idx) => (
              <text
                key={idx}
                x={lbl.x}
                y="132"
                textAnchor={lbl.x < 100 ? 'start' : lbl.x > 800 ? 'end' : 'middle'}
                fill="var(--text-secondary)"
                fontSize="11"
                fontFamily="var(--font-mono, monospace)"
              >
                {lbl.text}
              </text>
            ))}
          </svg>
        )}
      </div>
    </div>
  );
}
