import type { Aggregate, RequestViewCoverage } from '../lib/types';
import {
  calculateCacheHitRate,
  calculateFailureRate,
  formatAvgLatency,
  formatAvgTTFT,
  formatCostExact,
  formatNumber,
} from './RequestsHelpers';

export interface RequestsMetricsProps {
  summary: Aggregate | null;
  coverage?: RequestViewCoverage;
  loading?: boolean;
}

export function RequestsMetrics({ summary, coverage }: RequestsMetricsProps) {
  const details = summary?.detailed_requests === 0 || summary?.complete_requests === 0 ? null : summary;
  // 1. Requests
  const totalRequests = summary?.requests !== undefined ? formatNumber(summary.requests) : '—';
  const completedRequestsSub =
    summary?.complete_requests !== undefined
      ? `完整记账 ${formatNumber(summary.complete_requests)} 次`
      : '全区间受管请求';

  // 2. Failures & Failure rate
  const totalFailures = summary?.failures !== undefined ? formatNumber(summary.failures) : '—';
  const failRateStr = calculateFailureRate(summary?.failures, summary?.requests);
  const failureSub = failRateStr !== null ? `失败率 ${failRateStr}` : '失败率 —';

  // 3. Tokens (authoritative total value, reasoning subset)
  const totalTokens = summary?.tokens !== undefined ? formatNumber(summary.tokens) : '—';
  const inTokens = details?.input_tokens !== undefined ? formatNumber(details.input_tokens) : '—';
  const outTokens = details?.output_tokens !== undefined ? formatNumber(details.output_tokens) : '—';
  const reasoningNote =
    summary?.reasoning_tokens && summary.reasoning_tokens > 0
      ? `(含思考 ${formatNumber(summary.reasoning_tokens)})`
      : '';
  const tokensSub = `未缓存入 ${inTokens} / 出 ${outTokens} ${reasoningNote}`.trim();

  // 4. Cache Hit Rate & Read/Write
  const hitRateStr = calculateCacheHitRate(
    details?.cache_read_tokens,
    details?.input_tokens,
    details?.cache_write_tokens
  );
  const cacheHitRate = hitRateStr !== null ? hitRateStr : '—';
  const cacheRead =
    details?.cache_read_tokens !== undefined ? formatNumber(details.cache_read_tokens) : '—';
  const cacheWrite =
    details?.cache_write_tokens !== undefined ? formatNumber(details.cache_write_tokens) : '—';
  const cacheSub = `读 ${cacheRead} / 写 ${cacheWrite} · 样本 ${details?.complete_requests ?? '—'}/${summary?.requests ?? '—'}`;

  // 5. Latency & TTFT
  const avgLatency = formatAvgLatency(summary?.latency_ms, summary?.latency_samples);
  const avgTTFT = formatAvgTTFT(summary?.ttft_ms, summary?.ttft_samples);
  const latencySub = `TTFT: ${avgTTFT} · 样本 ${summary?.ttft_samples ?? '—'} / 耗时样本 ${summary?.latency_samples ?? '—'}`;

  // 6. Cost
  const totalCost = summary ? `$${formatCostExact(summary.cost_micro_usd)}` : '—';
  const costSub =
    summary?.unpriced_requests && summary.unpriced_requests > 0
      ? `${summary.unpriced_requests} 条未定价按 $0`
      : summary?.detailed_requests === 0 ? '历史定价覆盖未保存' : '按请求入库价格，历史不重算';

  return (
    <div className="requests-metrics-section">
      <div className="requests-metrics-grid">
        {/* Card 1: Requests */}
        <div className="requests-metric-card" data-testid="metric-requests">
          <div className="requests-metric-label">
            <span>请求数</span>
          </div>
          <div className="requests-metric-value">{totalRequests}</div>
          <div className="requests-metric-sub">{completedRequestsSub}</div>
        </div>

        {/* Card 2: Failures & Rate */}
        <div className="requests-metric-card" data-testid="metric-failures">
          <div className="requests-metric-label">
            <span>失败 / 失败率</span>
          </div>
          <div className="requests-metric-value">{totalFailures}</div>
          <div className="requests-metric-sub">{failureSub}</div>
        </div>

        {/* Card 3: Tokens (authoritative total, reasoning subset) */}
        <div className="requests-metric-card" data-testid="metric-tokens">
          <div className="requests-metric-label">
            <span>Token · 总量</span>
          </div>
          <div className="requests-metric-value">{totalTokens}</div>
          <div className="requests-metric-sub" title={tokensSub}>
            {tokensSub}
          </div>
        </div>

        {/* Card 4: Cache Hit Rate & Read/Write */}
        <div className="requests-metric-card" data-testid="metric-cache">
          <div className="requests-metric-label">
            <span>缓存命中率</span>
          </div>
          <div className="requests-metric-value">{cacheHitRate}</div>
          <div className="requests-metric-sub">{cacheSub}</div>
        </div>

        {/* Card 5: Latency & TTFT */}
        <div className="requests-metric-card" data-testid="metric-latency">
          <div className="requests-metric-label">
            <span>平均耗时</span>
          </div>
          <div className="requests-metric-value">{avgLatency}</div>
          <div className="requests-metric-sub">{latencySub}</div>
        </div>

        {/* Card 6: Cost */}
        <div className="requests-metric-card" data-testid="metric-cost">
          <div className="requests-metric-label">
            <span>总费用</span>
          </div>
          <div className="requests-metric-value">{totalCost}</div>
          <div className="requests-metric-sub">{costSub}</div>
        </div>
      </div>

      {/* Historical Coverage Notices */}
      {coverage?.backfill_pending && (
        <div className="requests-coverage-notice" role="status">
          <span className="cpa-badge cpa-badge-warning">统计补全中</span>
          <span className="requests-coverage-text">
            正在为历史日志回填新增细分指标，部分指标可能正在更新。
          </span>
        </div>
      )}

      {!coverage?.backfill_pending &&
        coverage?.detailed_requests !== undefined &&
        coverage?.requests !== undefined &&
        coverage.detailed_requests < coverage.requests && (
          <div className="requests-coverage-notice" role="status">
            <span className="cpa-badge cpa-badge-neutral">部分历史指标未保存</span>
            <span className="requests-coverage-text">
              该时间区间内仅 {coverage.detailed_requests}/{coverage.requests} 条包含细分指标，缺失指标未填 0。
            </span>
          </div>
        )}
    </div>
  );
}
