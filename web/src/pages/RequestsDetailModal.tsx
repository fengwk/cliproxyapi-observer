import { useState } from 'react';
import { accountDisplay, accountingQualityLabel, captureReasonLabel } from './RequestsHelpers';
import type { Detail, Key } from '../lib/types';
import {
  calculateCacheHitRate,
  calculateTPS,
  CAPTURE_STATUS_MAP,
  formatCostExact,
  formatNumber,
  formatPriceSource,
  formatSecondsFromMs,
  formatLocalDateTime,
  redactBody,
  redactHeaders,
  resolveKeyDisplay,
} from './RequestsHelpers';
import { formatBytes } from '../lib/format';
import { Badge, Button, EmptyState, Modal, Notice } from '../components/ui';

export interface RequestsDetailModalProps {
  detail: Detail | null;
  loading: boolean;
  error: string | null;
  onClose: () => void;
  keysMap: Map<string, Key>;
}

export function RequestsDetailModal({
  detail,
  loading,
  error,
  onClose,
  keysMap,
}: RequestsDetailModalProps) {
  const [tab, setTab] = useState<'summary' | 'headers' | 'body'>('summary');
  const [copiedNotice, setCopiedNotice] = useState<string | null>(null);

  const copyToClipboard = async (text: string, label: string) => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(text);
      setCopiedNotice(`已复制${label}`);
      setTimeout(() => setCopiedNotice(null), 3000);
    } catch {
      setCopiedNotice(`复制失败，请手动选取复制。`);
      setTimeout(() => setCopiedNotice(null), 3000);
    }
  };

  const record = detail?.record;
  const accountName = accountDisplay(record?.auth_index, record?.account_name);
  const complete = record?.accounting_quality === 'complete';
  const keyInfo = record ? resolveKeyDisplay(record.key_id, keysMap) : null;
  const isSuccess = record ? !record.failed : false;
  const hitRate = record && complete
    ? calculateCacheHitRate(record.tokens?.cache_read, record.tokens?.input, record.tokens?.cache_write)
    : null;
  const tps = record && complete
    ? calculateTPS(record.tokens?.output, record.latency_ms, record.ttft_ms, record.stream)
    : null;
  const priceSource = record ? formatPriceSource(record.price_source) : null;

  // Header and Body statuses
  const rawHeadersStatus = detail?.headers_status || detail?.capture_status || 'missing';
  const headersStatusInfo = CAPTURE_STATUS_MAP[rawHeadersStatus] || {
    label: '未知采集状态',
    tone: 'neutral',
  };

  const rawBodyStatus = detail?.body_status || detail?.capture_status || 'missing';
  const bodyStatusInfo = CAPTURE_STATUS_MAP[rawBodyStatus] || {
    label: '未知采集状态',
    tone: 'neutral',
  };

  const safeHeaders = redactHeaders(detail?.headers);
  const hasHeaders = safeHeaders.length > 0;
  const hasBody = detail?.body !== null && detail?.body !== undefined;
  const redactedBodyObj = hasBody ? redactBody(detail.body) : null;
  const formattedBodyJson = hasBody ? JSON.stringify(redactedBodyObj, null, 2) : '';

  // Optional price quote snapshot
  const priceQuote = detail?.price_quote;

  return (
    <Modal
      title="请求详情"
      onClose={onClose}
      size="lg"
    >
      <div className="requests-detail-content">
        {/* Inline notification for copy feedback */}
        {copiedNotice && (
          <div className="requests-detail-notice">
            <Notice tone="info">{copiedNotice}</Notice>
          </div>
        )}

        {/* Loading state */}
        {loading && (
          <div className="requests-detail-loading" role="status">
            <span className="cpa-spinner" aria-hidden="true" />
            <span>正在加载详情...</span>
          </div>
        )}

        {/* Error state */}
        {!loading && error && (
          <Notice tone="danger">获取详情失败: {error}</Notice>
        )}

        {/* Loaded Content */}
        {!loading && !error && record && (
          <>
            {/* Modal Navigation Tabs */}
            <div className="requests-tabs-nav" role="tablist">
              <button
                type="button"
                role="tab"
                aria-selected={tab === 'summary'}
                className={`requests-tab-btn ${tab === 'summary' ? 'is-active' : ''}`}
                onClick={() => setTab('summary')}
              >
                概览
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={tab === 'headers'}
                className={`requests-tab-btn ${tab === 'headers' ? 'is-active' : ''}`}
                onClick={() => setTab('headers')}
              >
                请求头
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={tab === 'body'}
                className={`requests-tab-btn ${tab === 'body' ? 'is-active' : ''}`}
                onClick={() => setTab('body')}
              >
                请求体 (JSON)
              </button>
            </div>

            {/* TAB 1: SUMMARY */}
            {tab === 'summary' && (
              <div className="requests-detail-pane" data-testid="detail-tab-summary">
                {/* Basic Info Grid */}
                <div className="requests-detail-grid">
                  <details style={{ gridColumn: '1 / -1', overflowWrap: 'anywhere' }}>
                    <summary>技术信息</summary>
                    <div>账户标识：{record.auth_index || '—'}</div>
                    <div>Key 标识：{record.key_id || '—'}</div>
                    <div>记账质量原始码：{record.accounting_quality || '—'}</div>
                    <div>采集原因原始码：{detail.capture_reason || '—'}</div>
                    <div>定价来源原始码：{record.price_source || '—'}</div>
                    <div className="requests-detail-item">
                      <span className="requests-detail-item-label">请求标识</span>
                      <span className="requests-detail-item-val font-mono">{record.id}</span>
                    </div>
                    <div className="requests-detail-item">
                      <span className="requests-detail-item-label">追踪标识</span>
                      <span className="requests-detail-item-val font-mono">
                        {record.trace_id || '—'}
                      </span>
                    </div>
                    {record.key_id && (
                      <Button variant="ghost" onClick={() => copyToClipboard(record.key_id, 'Key 标识')}>
                        复制 Key 标识
                      </Button>
                    )}
                  </details>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">时间 (本地)</span>
                    <span className="requests-detail-item-val">{formatLocalDateTime(record.time)}</span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">状态</span>
                    <span className="requests-detail-item-val">
                      <Badge tone={isSuccess ? 'success' : 'danger'}>
                        {record.status_code > 0 ? `${record.status_code} ` : ''}
                        {isSuccess ? '成功' : '失败'}
                      </Badge>
                      {record.stream && (
                        <Badge tone="neutral" className="requests-stream-badge">
                          流式
                        </Badge>
                      )}
                    </span>
                  </div>

                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">模型</span>
                    <span className="requests-detail-item-val font-mono">{record.model}</span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">提供商</span>
                    <span className="requests-detail-item-val">{record.provider || '—'}</span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">账户</span>
                    <span
                      className="requests-detail-item-val requests-account-name"
                      title={accountName}
                    >
                      {accountName}
                    </span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">客户端 Key</span>
                    <div className="requests-detail-item-val">
                      <span>{keyInfo?.label}</span>
                      {keyInfo?.isDeleted && (
                        <Badge tone="danger" className="requests-deleted-tag">
                          已删除
                        </Badge>
                      )}
                    </div>
                  </div>

                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">耗时 / TTFT</span>
                    <span className="requests-detail-item-val">
                      {formatSecondsFromMs(record.latency_ms)}
                      {record.stream && record.ttft_ms > 0
                        ? ` (TTFT: ${formatSecondsFromMs(record.ttft_ms)})`
                        : ''}
                    </span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">TPS</span>
                    <span className="requests-detail-item-val">{tps !== null ? tps : '—'}</span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">记账质量</span>
                    <span className="requests-detail-item-val">
                      {accountingQualityLabel(record.accounting_quality)}
                    </span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">定价来源</span>
                    <span className="requests-detail-item-val">
                      {priceSource?.label}
                    </span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">费用</span>
                    <span className="requests-detail-item-val font-mono">
                      ${formatCostExact(record.cost_micro_usd)} ({record.cost_micro_usd} µUSD)
                    </span>
                  </div>
                  <div className="requests-detail-item">
                    <span className="requests-detail-item-label">缓存命中率</span>
                    <span className="requests-detail-item-val">{hitRate !== null ? hitRate : '—'}</span>
                  </div>
                </div>

                {/* Tokens Detailed Breakdown */}
                <div className="requests-detail-section-title">Token 用量分布</div>
                <div className="requests-tokens-grid">
                  <div className="requests-token-stat">
                    <span className="requests-token-label">未缓存输入 Token</span>
                    <span className="requests-token-value">{formatNumber(complete ? record.tokens?.input : undefined)}</span>
                  </div>
                  <div className="requests-token-stat">
                    <span className="requests-token-label">输出 Token</span>
                    <span className="requests-token-value">{formatNumber(complete ? record.tokens?.output : undefined)}</span>
                  </div>
                  <div className="requests-token-stat">
                    <span className="requests-token-label">思考 Token (输出子集)</span>
                    <span className="requests-token-value">
                      {formatNumber(complete ? record.tokens?.reasoning : undefined)}
                    </span>
                  </div>
                  <div className="requests-token-stat">
                    <span className="requests-token-label">缓存读取 Token</span>
                    <span className="requests-token-value">
                      {formatNumber(complete ? record.tokens?.cache_read : undefined)}
                    </span>
                  </div>
                  <div className="requests-token-stat">
                    <span className="requests-token-label">缓存写入 Token</span>
                    <span className="requests-token-value">
                      {formatNumber(complete ? record.tokens?.cache_write : undefined)}
                    </span>
                  </div>
                  <div className="requests-token-stat requests-token-stat-total">
                    <span className="requests-token-label">总 Token (权威值)</span>
                    <span className="requests-token-value">{formatNumber(record.tokens?.total)}</span>
                  </div>
                </div>

                {/* Optional Price Quote / Diagnostic Snapshot */}
                {Boolean(priceQuote) && (
                  <div className="requests-detail-pricing-box">
                    <div className="requests-detail-section-title">定价诊断快照</div>
                    <pre className="requests-json-body">
                      {JSON.stringify(priceQuote, null, 2)}
                    </pre>
                  </div>
                )}
              </div>
            )}

            {/* TAB 2: HEADERS */}
            {tab === 'headers' && (
              <div className="requests-detail-pane" data-testid="detail-tab-headers">
                {/* Headers Metadata & Status Row */}
                <div className="requests-capture-meta-bar">
                  <div className="requests-capture-status-item">
                    <span className="requests-subtext">采集状态:</span>
                    <Badge tone={headersStatusInfo.tone}>{headersStatusInfo.label}</Badge>
                  </div>
                  {detail.capture_reason && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">原因:</span>
                      <span>{captureReasonLabel(detail.capture_reason)}</span>
                    </div>
                  )}
                  {detail.captured_at && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">采集于:</span>
                      <span>{formatLocalDateTime(detail.captured_at)}</span>
                    </div>
                  )}
                  {detail.expires_at && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">到期于:</span>
                      <span>{formatLocalDateTime(detail.expires_at)}</span>
                    </div>
                  )}
                  {detail.original_bytes !== undefined && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">原始/压缩大小:</span>
                      <span>
                        {formatBytes(detail.original_bytes)} /{' '}
                        {formatBytes(detail.compressed_bytes ?? 0)}
                      </span>
                    </div>
                  )}
                </div>

                {/* Headers Table or Honest Empty State */}
                {hasHeaders ? (
                  <>
                    <div className="requests-json-actions">
                      <Button
                        variant="secondary"
                        className="cpa-btn-sm"
                        onClick={() =>
                          copyToClipboard(
                            JSON.stringify(
                              safeHeaders.reduce((acc, h) => ({ ...acc, [h.name]: h.value }), {}),
                              null,
                              2
                            ),
                            '请求头 JSON'
                          )
                        }
                      >
                        复制请求头 JSON
                      </Button>
                    </div>
                    <div className="table-wrap">
                      <table className="requests-headers-table">
                        <thead>
                          <tr>
                            <th style={{ width: '35%' }}>Header 名称</th>
                            <th>Header 值 (已脱敏)</th>
                          </tr>
                        </thead>
                        <tbody>
                          {safeHeaders.map((h) => (
                            <tr key={h.name}>
                              <td className="font-mono">{h.name}</td>
                              <td className="font-mono">{h.value}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </>
                ) : (
                  <EmptyState
                    title={
                      rawHeadersStatus === 'disabled'
                        ? '采集未开启 (默认)'
                        : rawHeadersStatus === 'expired'
                        ? '请求头已过期已清理'
                        : '无可用请求头'
                    }
                    description={
                      rawHeadersStatus === 'disabled'
                        ? '插件默认不保存请求内容以保护隐私。可在“设置”中配置采集模式为“安全请求头”或“安全请求头与 JSON 正文”。'
                        : `状态: ${headersStatusInfo.label}`
                    }
                  />
                )}
              </div>
            )}

            {/* TAB 3: BODY (JSON) */}
            {tab === 'body' && (
              <div className="requests-detail-pane" data-testid="detail-tab-body">
                {/* Body Metadata & Status Row */}
                <div className="requests-capture-meta-bar">
                  <div className="requests-capture-status-item">
                    <span className="requests-subtext">正文状态:</span>
                    <Badge tone={bodyStatusInfo.tone}>{bodyStatusInfo.label}</Badge>
                  </div>
                  {detail.capture_reason && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">原因:</span>
                      <span>{captureReasonLabel(detail.capture_reason)}</span>
                    </div>
                  )}
                  {detail.captured_at && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">采集于:</span>
                      <span>{formatLocalDateTime(detail.captured_at)}</span>
                    </div>
                  )}
                  {detail.expires_at && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">到期于:</span>
                      <span>{formatLocalDateTime(detail.expires_at)}</span>
                    </div>
                  )}
                  {detail.original_bytes !== undefined && (
                    <div className="requests-capture-status-item">
                      <span className="requests-subtext">原始/压缩大小:</span>
                      <span>
                        {formatBytes(detail.original_bytes)} /{' '}
                        {formatBytes(detail.compressed_bytes ?? 0)}
                      </span>
                    </div>
                  )}
                </div>

                {/* Body JSON or Honest Empty State */}
                {hasBody ? (
                  <>
                    <div className="requests-json-actions">
                      <Button
                        variant="secondary"
                        className="cpa-btn-sm"
                        onClick={() => copyToClipboard(formattedBodyJson, '请求体 JSON')}
                      >
                        复制请求体 JSON
                      </Button>
                    </div>
                    <pre className="requests-json-body font-mono">{formattedBodyJson}</pre>
                  </>
                ) : (
                  <EmptyState
                    title={
                      rawBodyStatus === 'disabled'
                        ? '无可用正文'
                        : rawBodyStatus === 'too_large' || rawBodyStatus === 'truncated'
                        ? '正文过大未保存'
                        : rawBodyStatus === 'unsupported'
                        ? '格式不支持未保存'
                        : rawBodyStatus === 'expired'
                        ? '正文已过期已清理'
                        : '无可用正文'
                    }
                    description={
                      rawBodyStatus === 'disabled'
                        ? '采集未开启 (默认)。插件默认不保存请求正文以保护隐私。可在“设置”中配置采集模式为“安全请求头与 JSON 正文”。'
                        : rawBodyStatus === 'too_large' || rawBodyStatus === 'truncated'
                        ? '请求正文大小超过单条正文大小上限，已整份跳过以防止截断泄密。'
                        : rawBodyStatus === 'unsupported'
                        ? '当前仅支持有效的 JSON 格式入站正文。'
                        : `正文状态: ${bodyStatusInfo.label}`
                    }
                  />
                )}
              </div>
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
