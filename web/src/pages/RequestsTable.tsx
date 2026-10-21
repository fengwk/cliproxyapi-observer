import React from 'react';
import type { RequestRecord, RequestViewGroup, Key } from '../lib/types';
import type { ViewTab } from './RequestsHelpers';
import {
  calculateCacheHitRate,
  calculateFailureRate,
  calculateTPS,
  formatAvgSeconds,
  formatCostExact,
  formatNumber,
  formatPriceSource,
  formatSecondsFromMs,
  formatUTCDateTime,
  resolveKeyDisplay,
} from './RequestsHelpers';
import { Badge, Button, EmptyState } from '../components/ui';

export interface RequestsTableProps {
  view: ViewTab;
  onViewChange: (nextView: ViewTab) => void;
  records?: RequestRecord[];
  groups?: RequestViewGroup[];
  page: number;
  hasMore: boolean;
  onPageChange: (newPage: number) => void;
  keysMap: Map<string, Key>;
  onOpenDetail: (id: string) => void;
  loading?: boolean;
}

export function RequestsTable({
  view,
  onViewChange,
  records = [],
  groups = [],
  page,
  hasMore,
  onPageChange,
  keysMap,
  onOpenDetail,
  loading = false,
}: RequestsTableProps) {
  const isRecordsView = view === 'requests';
  const isKeyModel = view === 'key_model';

  return (
    <div className="requests-table-section" data-testid="requests-table-section">
      {/* 6 Views Tabs */}
      <div className="requests-tabs-nav" role="tablist" aria-label="请求视图切换">
        <button
          type="button"
          role="tab"
          aria-selected={view === 'requests'}
          className={`requests-tab-btn ${view === 'requests' ? 'is-active' : ''}`}
          onClick={() => onViewChange('requests')}
        >
          请求明细
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'model'}
          className={`requests-tab-btn ${view === 'model' ? 'is-active' : ''}`}
          onClick={() => onViewChange('model')}
        >
          模型分组
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'key'}
          className={`requests-tab-btn ${view === 'key' ? 'is-active' : ''}`}
          onClick={() => onViewChange('key')}
        >
          Key 分组
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'key_model'}
          className={`requests-tab-btn ${view === 'key_model' ? 'is-active' : ''}`}
          onClick={() => onViewChange('key_model')}
        >
          Key × 模型
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'account'}
          className={`requests-tab-btn ${view === 'account' ? 'is-active' : ''}`}
          onClick={() => onViewChange('account')}
        >
          账户分组
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'provider'}
          className={`requests-tab-btn ${view === 'provider' ? 'is-active' : ''}`}
          onClick={() => onViewChange('provider')}
        >
          提供商分组
        </button>
      </div>

      {/* Main Table Container with inner horizontal scroll */}
      <div className="table-wrap">
        {isRecordsView ? (
          /* ======================================================== Records Table */
          records.length === 0 ? (
            <EmptyState
              title="暂无请求记录"
              description="在当前筛选条件下未发现日志记录，请尝试调整日期或筛选条件。"
            />
          ) : (
            <table className="requests-table">
              <thead>
                <tr>
                  <th>时间 / 模型</th>
                  <th>客户端 Key</th>
                  <th>提供商 / 账户</th>
                  <th>状态</th>
                  <th>未缓存入 / 出</th>
                  <th>缓存读 / 写</th>
                  <th>缓存命中率</th>
                  <th>耗时 / TTFT</th>
                  <th>TPS</th>
                  <th>费用</th>
                  <th style={{ textAlign: 'center' }}>内容</th>
                </tr>
              </thead>
              <tbody>
                {records.map((r) => {
                  const keyInfo = resolveKeyDisplay(r.key_id, keysMap);
                  const hitRate = calculateCacheHitRate(
                    r.tokens?.cache_read,
                    r.tokens?.input,
                    r.tokens?.cache_write
                  );
                  const tps = calculateTPS(
                    r.tokens?.output,
                    r.latency_ms,
                    r.ttft_ms,
                    r.stream
                  );
                  const priceSource = formatPriceSource(r.price_source, r.rule_id);
                  const isSuccess = !r.failed;
                  const complete = r.accounting_quality === 'complete';

                  return (
                    <tr key={r.id} data-testid={`record-row-${r.id}`}>
                      {/* 1. Time / Model */}
                      <td>
                        <div className="requests-cell-stacked">
                          <button
                            type="button"
                            className="requests-model-link"
                            onClick={() => onOpenDetail(r.id)}
                            title={`查看模型详情: ${r.model}`}
                          >
                            {r.model}
                          </button>
                          <span className="requests-subtext">
                            {formatUTCDateTime(r.time)}
                          </span>
                        </div>
                      </td>

                      {/* 2. Client Key */}
                      <td className="requests-key-cell">
                        <div className="requests-cell-stacked">
                          <div className="requests-key-row">
                            <span
                              className="requests-key-name"
                              title={keyInfo.fullId}
                            >
                              {keyInfo.label}
                            </span>
                            {keyInfo.isDeleted && (
                              <Badge tone="danger" className="requests-deleted-tag">
                                已删除
                              </Badge>
                            )}
                          </div>
                        </div>
                      </td>

                      {/* 3. Provider / Account */}
                      <td>
                        <div className="requests-cell-stacked">
                          <span className="requests-primary-text">
                            {r.provider || '—'}
                          </span>
                          <span className="requests-subtext" title={r.auth_index}>
                            {r.auth_index || '—'}
                          </span>
                        </div>
                      </td>

                      {/* 4. Status */}
                      <td>
                        <div className="requests-status-cell">
                          <Badge tone={isSuccess ? 'success' : 'danger'}>
                            {r.status_code > 0 ? `${r.status_code} ` : ''}
                            {isSuccess ? '成功' : '失败'}
                          </Badge>
                          {r.stream && (
                            <Badge tone="neutral" className="requests-stream-badge">
                              流式
                            </Badge>
                          )}
                        </div>
                      </td>

                      {/* 5. Input / Output Tokens */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span className="requests-subtext">总 {formatNumber(r.tokens?.total)}{!complete ? ' · 细分不完整' : ''}</span>
                          <span>
                            入 {formatNumber(complete ? r.tokens?.input : undefined)} / 出{' '}
                            {formatNumber(complete ? r.tokens?.output : undefined)}
                          </span>
                          {r.tokens?.reasoning ? (
                            <span className="requests-reasoning-sub">
                              (含思考 {formatNumber(r.tokens.reasoning)})
                            </span>
                          ) : null}
                        </div>
                      </td>

                      {/* 6. Cache Read / Write */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span>
                            读 {formatNumber(complete ? r.tokens?.cache_read : undefined)} / 写{' '}
                            {formatNumber(complete ? r.tokens?.cache_write : undefined)}
                          </span>
                        </div>
                      </td>

                      {/* 7. Cache Hit Rate */}
                      <td className="requests-num-cell">
                        {complete && hitRate !== null ? hitRate : '—'}
                      </td>

                      {/* 8. Latency / TTFT */}
                      <td className="requests-duration-cell">
                        <div className="requests-cell-stacked requests-num-cell">
                          <span>{formatSecondsFromMs(r.latency_ms)}</span>
                          {r.stream && r.ttft_ms > 0 ? (
                            <span className="requests-subtext">
                              TTFT: {formatSecondsFromMs(r.ttft_ms)}
                            </span>
                          ) : null}
                        </div>
                      </td>

                      {/* 9. TPS */}
                      <td className="requests-num-cell">
                        {complete && tps !== null ? tps : '—'}
                      </td>

                      {/* 10. Cost */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span className="requests-cost-val">
                            ${formatCostExact(r.cost_micro_usd)}
                          </span>
                          <span
                            className={`requests-source-sub ${priceSource.isWarning ? 'is-warning' : ''}`}
                          >
                            {priceSource.label}
                          </span>
                        </div>
                      </td>

                      {/* 11. Action: View detail */}
                      <td style={{ textAlign: 'center' }}>
                        <Button
                          variant="secondary"
                          className="cpa-btn-sm"
                          onClick={() => onOpenDetail(r.id)}
                          aria-label={`查看请求详情 ${r.id}`}
                        >
                          查看
                        </Button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )
        ) : (
          /* ======================================================== Group Table */
          groups.length === 0 ? (
            <EmptyState
              title="暂无分组数据"
              description="在当前筛选条件下未发现该分组的聚合数据。"
            />
          ) : (
            <table className="requests-table">
              <thead>
                <tr>
                  {isKeyModel ? (
                    <>
                      <th>客户端 Key</th>
                      <th>模型</th>
                    </>
                  ) : (
                    <th>
                      {view === 'model'
                        ? '模型'
                        : view === 'key'
                        ? '客户端 Key'
                        : view === 'account'
                        ? '账户'
                        : '提供商'}
                    </th>
                  )}
                  <th>请求数</th>
                  <th>失败 / 失败率</th>
                  <th>未缓存入 / 出</th>
                  <th>缓存读 / 写</th>
                  <th>缓存命中率</th>
                  <th>平均耗时 / TTFT</th>
                  <th>总费用</th>
                </tr>
              </thead>
              <tbody>
                {groups.map((g) => {
                  const failRate = calculateFailureRate(g.failures, g.requests);
                  const detail = g.detailed_requests === 0 || g.complete_requests === 0 ? null : g;
                  const hitRate = calculateCacheHitRate(
                    detail?.cache_read_tokens,
                    detail?.input_tokens,
                    detail?.cache_write_tokens
                  );
                  const avgLat = formatAvgSeconds(g.latency_ms, g.latency_samples);
                  const avgTTFT = formatAvgSeconds(g.ttft_ms, g.ttft_samples);

                  // Group identity rendering
                  let groupLabel: React.ReactNode = g.key || '未归属';
                  if (view === 'model') {
                    const mName = g.model || g.key || '未指定模型';
                    groupLabel = (
                      <div className="requests-cell-stacked">
                        <span className="requests-primary-text">{mName}</span>
                        {g.provider && (
                          <span className="requests-subtext">渠道: {g.provider}</span>
                        )}
                      </div>
                    );
                  } else if (view === 'key' || isKeyModel) {
                    const kId = g.key_id || (view === 'key' ? g.key : '');
                    const kInfo = resolveKeyDisplay(kId, keysMap);
                    groupLabel = (
                      <div className="requests-cell-stacked">
                        <div className="requests-key-row">
                          <span className="requests-key-name" title={kInfo.fullId}>
                            {kInfo.label}
                          </span>
                          {kInfo.isDeleted && (
                            <Badge tone="danger" className="requests-deleted-tag">
                              已删除
                            </Badge>
                          )}
                        </div>
                      </div>
                    );
                  } else if (view === 'account') {
                    const accountId = g.auth_index || g.key;
                    const accountName = g.label || accountId || '未归属';
                    groupLabel = (
                      <span
                        className="requests-primary-text requests-account-name"
                        title={accountId ? `${accountName}\nAuthIndex: ${accountId}` : accountName}
                      >
                        {accountName}
                      </span>
                    );
                  } else if (view === 'provider') {
                    groupLabel = (
                      <span className="requests-primary-text">
                        {g.provider || g.key || '未归属'}
                      </span>
                    );
                  }

                  return (
                    <tr key={g.key}>
                      {/* 1. Group identity */}
                      <td className={view === 'key' || isKeyModel ? 'requests-key-cell' : undefined}>
                        {groupLabel}
                      </td>

                      {/* 2. Composite model identity (key_model only) */}
                      {isKeyModel && (
                        <td>
                          <div className="requests-cell-stacked">
                            <span className="requests-primary-text">
                              {g.model || '未指定模型'}
                            </span>
                            {g.provider && (
                              <span className="requests-subtext">渠道: {g.provider}</span>
                            )}
                          </div>
                        </td>
                      )}

                      {/* 3. Requests */}
                      <td className="requests-num-cell">
                        {formatNumber(g.requests)}
                      </td>

                      {/* 3. Failures & Rate */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span>{formatNumber(g.failures)}</span>
                          <span className="requests-subtext">
                            {failRate !== null ? failRate : '—'}
                          </span>
                        </div>
                      </td>

                      {/* 4. Input / Output tokens */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span className="requests-subtext">总 {formatNumber(g.tokens)} · 完整样本 {g.complete_requests ?? '—'}/{g.requests}</span>
                          <span>
                            入 {formatNumber(detail?.input_tokens)} / 出{' '}
                            {formatNumber(detail?.output_tokens)}
                          </span>
                          {g.reasoning_tokens ? (
                            <span className="requests-reasoning-sub">
                              (含思考 {formatNumber(g.reasoning_tokens)})
                            </span>
                          ) : null}
                        </div>
                      </td>

                      {/* 5. Cache Read / Write */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span>
                            读 {formatNumber(detail?.cache_read_tokens)} / 写{' '}
                            {formatNumber(detail?.cache_write_tokens)}
                          </span>
                        </div>
                      </td>

                      {/* 6. Hit Rate */}
                      <td className="requests-num-cell">
                        {hitRate !== null ? hitRate : '—'}
                      </td>

                      {/* 7. Avg Latency & TTFT */}
                      <td className="requests-duration-cell">
                        <div className="requests-cell-stacked requests-num-cell">
                          <span>{avgLat}</span>
                          <span className="requests-subtext">
                            TTFT: {avgTTFT}
                          </span>
                        </div>
                      </td>

                      {/* 8. Total Cost */}
                      <td>
                        <div className="requests-cell-stacked requests-num-cell">
                          <span className="requests-cost-val">
                            ${formatCostExact(g.cost_micro_usd)}
                          </span>
                          {g.unpriced_requests ? (
                            <span className="requests-source-sub is-warning">
                              {g.unpriced_requests} 未定价
                            </span>
                          ) : null}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )
        )}
      </div>

      {/* Pagination & Footer note */}
      <div className="requests-pagination">
        <div className="requests-footer-note">
          仅统计 Observer 受管 Key · 保留范围以设置与覆盖提示为准
        </div>
        <div className="requests-page-controls">
          <Button
            variant="secondary"
            className="cpa-btn-sm"
            disabled={page <= 1 || loading}
            onClick={() => onPageChange(page - 1)}
          >
            上一页
          </Button>
          <span className="requests-page-indicator">第 {page} 页</span>
          <Button
            variant="secondary"
            className="cpa-btn-sm"
            disabled={!hasMore || loading}
            onClick={() => onPageChange(page + 1)}
          >
            下一页
          </Button>
        </div>
      </div>
    </div>
  );
}
