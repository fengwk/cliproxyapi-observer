import React, { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import type { PageProps } from '../lib/api';
import type { RequestPage, Summary, Detail, ModelInfo, Key } from '../lib/types';
import { formatDateTime, errorMessage } from '../lib/format';
import { usePoll } from '../lib/usePoll';
import { Button, Input, Field, Select, Modal, Badge, Notice, EmptyState } from '../components/ui';
import {
  FilterState,
  PresetRange,
  getTodayPresetRange,
  getLast7DaysPresetRange,
  snapToWholeDateRange,
  buildRequestsQueryParams,
  CAPTURE_STATUS_LABELS,
  formatPriceSource,
  formatCostExact,
  resolveKeyDisplay,
  redactHeaders,
  redactBody,
} from './RequestsHelpers';
import './Requests.css';

export interface RequestsPageProps extends PageProps {}

export function RequestsPage({ api, refreshInterval, onDirtyChange }: RequestsPageProps) {
  // Preset range selection
  const [preset, setPreset] = useState<PresetRange>('today');

  // Filter states: draft (being edited) vs applied (active on query)
  const [draftFilters, setDraftFilters] = useState<FilterState>(() => ({
    ...getTodayPresetRange(),
    key_id: '',
    model: '',
    provider: '',
    auth_index: '',
    status: '',
  }));

  const [appliedFilters, setAppliedFilters] = useState<FilterState>(() => ({
    ...draftFilters,
  }));

  // Pagination
  const [page, setPage] = useState<number>(1);

  // Data states
  const [summary, setSummary] = useState<Summary | null>(null);
  const [pageData, setPageData] = useState<RequestPage | null>(null);
  const [keysList, setKeysList] = useState<Key[]>([]);
  const [modelsList, setModelsList] = useState<ModelInfo[]>([]);

  // Page level loading and errors
  const [loading, setLoading] = useState<boolean>(true);
  const [queryError, setQueryError] = useState<string | null>(null);
  const [actionNotice, setActionNotice] = useState<{ tone: 'success' | 'danger'; text: string } | null>(null);

  // Detail modal state
  const [activeDetailId, setActiveDetailId] = useState<string | null>(null);
  const [detailData, setDetailData] = useState<Detail | null>(null);
  const [detailLoading, setDetailLoading] = useState<boolean>(false);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [detailTab, setDetailTab] = useState<'summary' | 'headers' | 'body'>('summary');

  // Generation gate and abort controllers for race conditions
  const generationRef = useRef<number>(0);
  const activeAbortControllerRef = useRef<AbortController | null>(null);
  const detailAbortControllerRef = useRef<AbortController | null>(null);
  const activeDetailIdRef = useRef<string | null>(null);

  // Keep ref to activeDetailId for late-response guards
  useEffect(() => {
    activeDetailIdRef.current = activeDetailId;
  }, [activeDetailId]);

  // Keys map for fast lookup
  const keysMap = useMemo(() => {
    const map = new Map<string, Key>();
    for (const k of keysList) {
      if (k && k.id) {
        map.set(k.id, k);
      }
    }
    return map;
  }, [keysList]);

  // Determine if draft is dirty compared to applied filters
  const isDirty = useMemo(() => {
    return (
      draftFilters.from !== appliedFilters.from ||
      draftFilters.to !== appliedFilters.to ||
      draftFilters.key_id !== appliedFilters.key_id ||
      draftFilters.model !== appliedFilters.model ||
      draftFilters.provider !== appliedFilters.provider ||
      draftFilters.auth_index !== appliedFilters.auth_index ||
      draftFilters.status !== appliedFilters.status
    );
  }, [draftFilters, appliedFilters]);

  // Sync dirty flag to parent without object reference loop
  useEffect(() => {
    onDirtyChange(isDirty);
  }, [isDirty, onDirtyChange]);

  // 1. Load keys authoritatively on mount for name-led key filter
  useEffect(() => {
    let unmounted = false;
    const controller = new AbortController();

    async function fetchKeys() {
      try {
        const res = await api.request<{ items: Key[] }>('/keys', { signal: controller.signal });
        if (!unmounted && res && Array.isArray(res.items)) {
          setKeysList(res.items);
        }
      } catch {
        // Tolerated on keys fetch
      }
    }

    fetchKeys();
    return () => {
      unmounted = true;
      controller.abort();
    };
  }, [api]);

  // 2. Load models authoritatively via api.models()
  useEffect(() => {
    let unmounted = false;
    const controller = new AbortController();

    async function fetchModels() {
      try {
        const list = await api.models({ signal: controller.signal });
        if (!unmounted && Array.isArray(list)) {
          setModelsList(list);
        }
      } catch {
        // Tolerated on models fetch
      }
    }

    fetchModels();
    return () => {
      unmounted = true;
      controller.abort();
    };
  }, [api]);

  // 3. Main Data Fetch Callback (Summary + Requests Page)
  const fetchData = useCallback(
    async (signal?: AbortSignal, currentFilters = appliedFilters, currentPage = page) => {
      // Abort any previous in-flight request
      if (activeAbortControllerRef.current) {
        activeAbortControllerRef.current.abort();
      }
      const localController = new AbortController();
      activeAbortControllerRef.current = localController;

      // Link with external signal if provided
      if (signal) {
        signal.addEventListener('abort', () => localController.abort(), { once: true });
      }

      const generation = ++generationRef.current;
      setLoading(true);

      try {
        const queryStr = buildRequestsQueryParams(currentFilters, currentPage, 25);
        const [sumRes, pageRes] = await Promise.all([
          api.request<Summary>(`/summary?${queryStr}`, { signal: localController.signal }),
          api.request<RequestPage>(`/requests?${queryStr}`, { signal: localController.signal }),
        ]);

        // Guard against stale responses or cancelled generations
        if (generation !== generationRef.current || localController.signal.aborted) {
          return;
        }

        setSummary(sumRes);
        setPageData(pageRes);
        setQueryError(null);
      } catch (err: unknown) {
        if (generation !== generationRef.current || localController.signal.aborted) {
          return;
        }
        setQueryError(errorMessage(err) || '加载请求数据失败');
      } finally {
        if (generation === generationRef.current) {
          setLoading(false);
          if (activeAbortControllerRef.current === localController) {
            activeAbortControllerRef.current = null;
          }
        }
      }
    },
    [api, appliedFilters, page]
  );

  // Trigger load on appliedFilters or page changes
  useEffect(() => {
    const controller = new AbortController();
    fetchData(controller.signal);
    return () => {
      controller.abort();
    };
  }, [fetchData]);

  // Polling via usePoll: automatically pauses when dirty, when modal is open, or refreshInterval <= 0
  const isPollingPaused = isDirty || activeDetailId !== null || refreshInterval <= 0;
  usePoll(
    useCallback(
      async (signal: AbortSignal) => {
        await fetchData(signal);
      },
      [fetchData]
    ),
    refreshInterval,
    isPollingPaused
  );

  // 4. Detail Fetch on demand
  const handleOpenDetail = useCallback(
    async (id: string) => {
      setActiveDetailId(id);
      setDetailData(null);
      setDetailError(null);
      setDetailLoading(true);
      setDetailTab('summary');

      // Abort any existing detail fetch
      if (detailAbortControllerRef.current) {
        detailAbortControllerRef.current.abort();
      }
      const controller = new AbortController();
      detailAbortControllerRef.current = controller;

      try {
        const res = await api.request<Detail>(`/requests/detail?id=${encodeURIComponent(id)}`, {
          signal: controller.signal,
        });

        // If modal was closed while request was in-flight, discard and do not reopen
        if (activeDetailIdRef.current !== id || controller.signal.aborted) {
          return;
        }

        setDetailData(res);
      } catch (err: unknown) {
        if (activeDetailIdRef.current !== id || controller.signal.aborted) {
          return;
        }
        setDetailError(errorMessage(err) || '获取请求详情失败');
      } finally {
        if (activeDetailIdRef.current === id) {
          setDetailLoading(false);
        }
      }
    },
    [api]
  );

  const handleCloseDetail = useCallback(() => {
    // Abort in-flight request immediately
    if (detailAbortControllerRef.current) {
      detailAbortControllerRef.current.abort();
      detailAbortControllerRef.current = null;
    }
    setActiveDetailId(null);
    setDetailData(null);
    setDetailError(null);
    setDetailLoading(false);
  }, []);

  // Filter Presets Click Handlers
  const applyPreset = useCallback((targetPreset: PresetRange) => {
    setPreset(targetPreset);
    if (targetPreset === 'today') {
      const bounds = getTodayPresetRange();
      setDraftFilters((prev) => ({ ...prev, from: bounds.from, to: bounds.to }));
    } else if (targetPreset === 'last7days') {
      const bounds = getLast7DaysPresetRange();
      setDraftFilters((prev) => ({ ...prev, from: bounds.from, to: bounds.to }));
    }
  }, []);

  // Form submit: apply draft, reset page to 1, clear dirty
  const handleQuerySubmit = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      setAppliedFilters({ ...draftFilters });
      setPage(1);
    },
    [draftFilters]
  );

  // Corrective action: snap to whole UTC dates upon query error
  const handleCorrectiveWholeDate = useCallback(() => {
    const snapped = snapToWholeDateRange(draftFilters.from, draftFilters.to);
    setDraftFilters((prev) => ({ ...prev, from: snapped.from, to: snapped.to }));
    setAppliedFilters((prev) => ({ ...prev, from: snapped.from, to: snapped.to }));
    setPage(1);
    setPreset('custom');
  }, [draftFilters]);

  // Pagination Next/Prev: preserves current applied filters
  const handlePageChange = useCallback((newPage: number) => {
    if (newPage >= 1) {
      setPage(newPage);
    }
  }, []);

  // Copy Key ID with inline notification (no alert/confirm)
  const handleCopyKey = useCallback(async (fullId: string) => {
    try {
      if (typeof window !== 'undefined') {
        (window as unknown as Record<string, unknown>).__observerCopied = fullId;
      }
      if (navigator && navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(fullId);
      }
      setActionNotice({ tone: 'success', text: `已复制完整 Key ID: ${fullId.slice(0, 16)}…` });
    } catch {
      setActionNotice({ tone: 'danger', text: '复制失败，请从 Key ID 提示中复制完整标识。' });
    }
  }, []);

  // Copy JSON body with inline notification
  const handleCopyJson = useCallback(async (text: string) => {
    try {
      if (typeof window !== 'undefined') {
        (window as unknown as Record<string, unknown>).__observerCopied = text;
      }
      if (navigator && navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
      }
      setActionNotice({ tone: 'success', text: '已复制请求 JSON 到剪贴板' });
    } catch {
      setActionNotice({ tone: 'danger', text: '复制 JSON 失败，请手动选取复制。' });
    }
  }, []);

  // Key filter select options
  const keyOptions = useMemo(() => {
    const opts: Array<{ value: string; label: string }> = [{ value: '', label: '全部 Key' }];
    for (const k of keysList) {
      const label = k.label ? k.label : k.id.length === 64 ? `${k.id.slice(0, 12)}…` : k.id;
      opts.push({ value: k.id, label });
    }
    // If draft contains an unknown key_id, preserve it in options
    if (draftFilters.key_id && !opts.some((o) => o.value === draftFilters.key_id)) {
      const kId = draftFilters.key_id;
      opts.push({
        value: kId,
        label: kId.length === 64 ? `未知 Key (${kId.slice(0, 12)}…)` : `未知 Key (${kId})`,
      });
    }
    return opts;
  }, [keysList, draftFilters.key_id]);

  // Distinct list of models for datalist suggestions
  const modelSuggestions = useMemo(() => {
    const set = new Set<string>();
    for (const m of modelsList) {
      if (m.id) set.add(m.id);
    }
    if (pageData && Array.isArray(pageData.items)) {
      for (const item of pageData.items) {
        if (item.model) set.add(item.model);
      }
    }
    return Array.from(set).sort();
  }, [modelsList, pageData]);

  // Trend chart SVG coordinates calculation
  const buckets = summary?.buckets || [];
  const maxRequests = Math.max(1, ...buckets.map((b) => b.requests || 0));

  const trendPoints = useMemo(() => {
    if (!buckets.length) return { requests: '', failures: '' };
    const len = buckets.length;
    const reqPts: string[] = [];
    const failPts: string[] = [];

    buckets.forEach((b, i) => {
      const x = 10 + (i * 980) / Math.max(1, len - 1);
      const reqY = 130 - ((b.requests || 0) * 120) / maxRequests;
      const failY = 130 - ((b.failures || 0) * 120) / maxRequests;
      reqPts.push(`${x.toFixed(1)},${reqY.toFixed(1)}`);
      failPts.push(`${x.toFixed(1)},${failY.toFixed(1)}`);
    });

    return {
      requests: reqPts.join(' '),
      failures: failPts.join(' '),
    };
  }, [buckets, maxRequests]);

  const timeRangeLabel = useMemo(() => {
    if (!buckets.length) return '暂无趋势数据';
    const first = buckets[0].time;
    const last = buckets[buckets.length - 1].time;
    return `${first} — ${last} · 请求峰值 ${maxRequests} · 深线：请求 / 红线：失败`;
  }, [buckets, maxRequests]);

  const failureRate = useMemo(() => {
    if (!summary || !summary.requests) return '0%';
    const rate = ((summary.failures / summary.requests) * 100).toFixed(1);
    return `${rate}% 失败率`;
  }, [summary]);

  return (
    <div className="requests-page" id="content">
      {/* Action Notification */}
      {actionNotice && (
        <div className="requests-action-notice">
          <Notice tone={actionNotice.tone}>{actionNotice.text}</Notice>
        </div>
      )}

      {/* Query Error Notice with Whole-Date Corrective Action */}
      {queryError && (
        <Notice tone="danger">
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '8px' }}>
            <span>{queryError}</span>
            <Button variant="secondary" onClick={handleCorrectiveWholeDate}>
              修正为整日 UTC 范围
            </Button>
          </div>
        </Notice>
      )}

      {/* 1. Header & 4 Compact CPA Metrics Cards */}
      <div className="requests-header">
        <div className="requests-title-group">
          <h2 className="requests-title">请求日志</h2>
          <span className="requests-title-hint">
            模型执行记录 · 按实际计费规则核算
          </span>
        </div>
      </div>

      <div className="requests-metrics-grid">
        <div className="requests-metric-card">
          <span className="requests-metric-label">总费用</span>
          <span className="requests-metric-value">
            ${formatCostExact(summary?.cost_micro_usd ?? 0)}
          </span>
          <span className="requests-metric-sub">按实际定价核算</span>
        </div>

        <div className="requests-metric-card">
          <span className="requests-metric-label">Token 消耗</span>
          <span className="requests-metric-value">
            {(summary?.tokens ?? 0).toLocaleString()}
          </span>
          <span className="requests-metric-sub">输入 + 输出合计</span>
        </div>

        <div className="requests-metric-card">
          <span className="requests-metric-label">执行次数</span>
          <span className="requests-metric-value">
            {(summary?.requests ?? 0).toLocaleString()}
          </span>
          <span className="requests-metric-sub">模型执行记录（非 HTTP 原始连接）</span>
        </div>

        <div className="requests-metric-card">
          <span className="requests-metric-label">失败次数</span>
          <span className="requests-metric-value">
            {(summary?.failures ?? 0).toLocaleString()}
          </span>
          <span className="requests-metric-sub">{failureRate}</span>
        </div>
      </div>

      {/* 2. SVG Request / Failure Trend Chart */}
      <div className="requests-trend-container">
        <div className="requests-trend-header">
          <div className="requests-trend-legend">
            <span className="requests-legend-item">
              <span className="requests-legend-color requests-line-color" />
              请求 (深线)
            </span>
            <span className="requests-legend-item">
              <span className="requests-legend-color failures-line-color" />
              失败 (红线)
            </span>
            <span className="requests-trend-meta">
              粒度：{summary?.granularity || 'day'} · UTC
            </span>
          </div>
          <span className="requests-trend-meta">{timeRangeLabel}</span>
        </div>

        {buckets.length > 0 ? (
          <svg
            className="requests-trend-svg trend"
            viewBox="0 0 1000 140"
            role="img"
            aria-label="请求与失败趋势"
          >
            <polyline className="request-line" points={trendPoints.requests} />
            <polyline className="failure-line" points={trendPoints.failures} />
          </svg>
        ) : (
          <div className="requests-trend-empty">暂无趋势数据</div>
        )}
      </div>

      {/* 3. Task Header & Filters Form */}
      <div className="requests-filters-card">
        <form
          id="filters"
          className="requests-filters-form"
          aria-label="筛选条件"
          onSubmit={handleQuerySubmit}
        >
          {/* Preset Buttons */}
          <div className="requests-presets-bar">
            <span className="requests-preset-label">快速预设：</span>
            <Button
              variant={preset === 'today' ? 'primary' : 'ghost'}
              onClick={() => applyPreset('today')}
            >
              今天 (UTC)
            </Button>
            <Button
              variant={preset === 'last7days' ? 'primary' : 'ghost'}
              onClick={() => applyPreset('last7days')}
            >
              最近 7 天 (UTC)
            </Button>
            <Button
              variant={preset === 'custom' ? 'primary' : 'ghost'}
              onClick={() => applyPreset('custom')}
            >
              自定义
            </Button>
          </div>

          {/* Primary Filters Grid */}
          <div className="requests-filter-grid">
            <Field label="开始日期 (UTC)" htmlFor="from">
              <Input
                id="from"
                name="from"
                type="date"
                value={draftFilters.from}
                onChange={(e) => {
                  setPreset('custom');
                  setDraftFilters((prev) => ({ ...prev, from: e.target.value }));
                }}
              />
            </Field>

            <Field label="结束日期 (UTC，不含)" htmlFor="to" hint="留空默认截止当前时间 (NOW)">
              <Input
                id="to"
                name="to"
                type="date"
                value={draftFilters.to}
                onChange={(e) => {
                  setPreset('custom');
                  setDraftFilters((prev) => ({ ...prev, to: e.target.value }));
                }}
              />
            </Field>

            <Field label="Key" htmlFor="key_id">
              <Select
                id="key_id"
                aria-label="Key"
                value={draftFilters.key_id}
                onChange={(val) => setDraftFilters((prev) => ({ ...prev, key_id: val }))}
                options={keyOptions}
              />
              <input type="hidden" name="key_id" value={draftFilters.key_id} />
            </Field>

            <Field label="模型" htmlFor="model">
              <Input
                id="model"
                name="model"
                list="requests-models-list"
                placeholder="全部模型"
                value={draftFilters.model}
                onChange={(e) => setDraftFilters((prev) => ({ ...prev, model: e.target.value }))}
              />
              <datalist id="requests-models-list">
                {modelSuggestions.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </datalist>
            </Field>
          </div>

          {/* Collapsible Advanced Filters (Provider / AuthIndex / Status) */}
          <details className="requests-filter-advanced">
            <summary>高级筛选（提供商 / 账户索引 / 状态）</summary>
            <div className="requests-filter-advanced-grid">
              <Field label="提供商" htmlFor="provider">
                <Input
                  id="provider"
                  name="provider"
                  placeholder="全部提供商"
                  value={draftFilters.provider}
                  onChange={(e) => setDraftFilters((prev) => ({ ...prev, provider: e.target.value }))}
                />
              </Field>

              <Field label="账户索引" htmlFor="auth_index">
                <Input
                  id="auth_index"
                  name="auth_index"
                  placeholder="全部账户"
                  value={draftFilters.auth_index}
                  onChange={(e) => setDraftFilters((prev) => ({ ...prev, auth_index: e.target.value }))}
                />
              </Field>

              <Field label="状态" htmlFor="status">
                <Select
                  id="status"
                  aria-label="状态"
                  value={draftFilters.status}
                  onChange={(val) => setDraftFilters((prev) => ({ ...prev, status: val as FilterState['status'] }))}
                  options={[
                    { value: '', label: '全部状态' },
                    { value: 'success', label: '成功' },
                    { value: 'failed', label: '失败' },
                  ]}
                />
                <input type="hidden" name="status" value={draftFilters.status} />
              </Field>
            </div>
          </details>

          {/* Single Clear Query Button */}
          <div className="requests-filter-actions">
            <Button type="submit" variant="primary" loading={loading}>
              查询
            </Button>
          </div>
        </form>
      </div>

      {/* 4. Requests Table */}
      <div className="requests-table-section">
        <div className="table-wrap">
          <table className="requests-table">
            <thead>
              <tr>
                <th scope="col">时间 / 模型</th>
                <th scope="col">Key</th>
                <th scope="col">提供商 / 账户</th>
                <th scope="col">状态</th>
                <th scope="col">延迟 / Token</th>
                <th scope="col">费用</th>
              </tr>
            </thead>
            <tbody>
              {pageData && pageData.items && pageData.items.length > 0 ? (
                pageData.items.map((record) => {
                  const keyDisplay = resolveKeyDisplay(record.key_id, keysMap);
                  const priceSourceInfo = formatPriceSource(record.price_source, record.rule_id);
                  const isUnclassified = record.accounting_quality === 'unclassified';
                  const formattedTime = formatDateTime(record.time) || record.time;

                  return (
                    <tr key={record.id}>
                      {/* Time & Model Button */}
                      <td>
                        <div title={record.time} className="requests-time-cell">
                          {formattedTime}
                        </div>
                        <Button
                          variant="ghost"
                          className="requests-model-btn"
                          onClick={() => handleOpenDetail(record.id)}
                          title={`查看 ${record.model} 详情`}
                        >
                          {record.model}
                        </Button>
                      </td>

                      {/* Key Name / Truncated ID fallback */}
                      <td>
                        <div className="requests-key-cell">
                          <span
                            className="requests-key-label"
                            title={keyDisplay.fullId}
                          >
                            {keyDisplay.label}
                          </span>
                          {keyDisplay.isFallback && record.key_id ? (
                            <Button
                              variant="ghost"
                              className="requests-copy-btn"
                              aria-label="复制完整 Key ID"
                              onClick={() => handleCopyKey(record.key_id)}
                            >
                              复制
                            </Button>
                          ) : null}
                        </div>
                      </td>

                      {/* Provider & Auth Index */}
                      <td>
                        {record.provider || '—'} / {record.auth_index || '—'}
                      </td>

                      {/* Status */}
                      <td>
                        <Badge tone={record.failed ? 'danger' : 'success'}>
                          {record.failed ? '失败' : '成功'} {record.status_code || ''}
                        </Badge>
                        {record.stream && (
                          <span style={{ marginLeft: 4 }}>
                            <Badge tone="neutral">流式</Badge>
                          </span>
                        )}
                      </td>

                      {/* Latency & Tokens */}
                      <td>
                        <div className="requests-tokens-cell">
                          <span>
                            {record.latency_ms}ms / {(record.tokens?.total ?? 0).toLocaleString()} Token
                          </span>
                          {isUnclassified && (
                            <small className="requests-unclassified-tag muted" role="status">
                              Token 口径未分类
                            </small>
                          )}
                        </div>
                      </td>

                      {/* Cost & Price Source */}
                      <td>
                        <div className="requests-cost-cell">
                          <span>${formatCostExact(record.cost_micro_usd)}</span>
                          <span className="requests-source-tag muted">
                            {priceSourceInfo.isWarning ? (
                              <Badge tone="warning">未定价（0）</Badge>
                            ) : (
                              priceSourceInfo.label
                            )}
                          </span>
                        </div>
                      </td>
                    </tr>
                  );
                })
              ) : (
                <tr>
                  <td colSpan={6}>
                    <EmptyState
                      title="暂无请求记录"
                      description="当前时间范围或筛选条件下无匹配的调用记录"
                    />
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        {/* 5. Pagination */}
        <div className="requests-pagination">
          <span className="requests-page-indicator">第 {page} 页</span>
          <div className="requests-page-controls">
            <Button
              variant="secondary"
              disabled={page <= 1 || loading}
              onClick={() => handlePageChange(page - 1)}
            >
              上一页
            </Button>
            <Button
              variant="secondary"
              disabled={!pageData?.has_more || loading}
              onClick={() => handlePageChange(page + 1)}
            >
              下一页
            </Button>
          </div>
        </div>
      </div>

      {/* 6. Detail Modal (On-Demand) */}
      {activeDetailId && (
        <Modal title="请求详情" onClose={handleCloseDetail} size="lg">
          {detailLoading && <div>正在加载详情...</div>}

          {detailError && <Notice tone="danger">{detailError}</Notice>}

          {detailData && (
            <div className="requests-detail-content">
              {/* Capture Status Banner */}
              <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <span style={{ fontSize: 13, fontWeight: 500 }}>采集状态：</span>
                <Badge
                  tone={
                    detailData.capture_status === 'available' || detailData.capture_status === 'redacted'
                      ? 'success'
                      : detailData.capture_status === 'disabled' || detailData.capture_status === 'expired'
                      ? 'warning'
                      : 'neutral'
                  }
                >
                  {CAPTURE_STATUS_LABELS[detailData.capture_status] || detailData.capture_status}
                </Badge>
              </div>

              {/* Tabs Navigation */}
              <div className="requests-tabs-nav" role="tablist">
                <button
                  type="button"
                  role="tab"
                  aria-selected={detailTab === 'summary'}
                  className={`requests-tab-btn ${detailTab === 'summary' ? 'is-active' : ''}`}
                  onClick={() => setDetailTab('summary')}
                >
                  概览
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={detailTab === 'headers'}
                  className={`requests-tab-btn ${detailTab === 'headers' ? 'is-active' : ''}`}
                  onClick={() => setDetailTab('headers')}
                >
                  请求头
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={detailTab === 'body'}
                  className={`requests-tab-btn ${detailTab === 'body' ? 'is-active' : ''}`}
                  onClick={() => setDetailTab('body')}
                >
                  请求体 (JSON)
                </button>
              </div>

              {/* Tab 1: Summary */}
              {detailTab === 'summary' && (
                <div className="requests-summary-grid">
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">请求 ID</span>
                    <span className="requests-summary-item-val">{detailData.record.id}</span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">Trace ID</span>
                    <span className="requests-summary-item-val">{detailData.record.trace_id || '—'}</span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">模型</span>
                    <span className="requests-summary-item-val">{detailData.record.model}</span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">提供商 / 账户</span>
                    <span className="requests-summary-item-val">
                      {detailData.record.provider || '—'} / {detailData.record.auth_index || '—'}
                    </span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">状态</span>
                    <span className="requests-summary-item-val">
                      {detailData.record.failed ? '失败' : '成功'} ({detailData.record.status_code || '—'})
                    </span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">耗时</span>
                    <span className="requests-summary-item-val">
                      {detailData.record.latency_ms}ms
                      {detailData.record.ttft_ms > 0 ? ` (首字延迟 TTFT ${detailData.record.ttft_ms}ms)` : ''}
                    </span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">费用</span>
                    <span className="requests-summary-item-val">
                      ${formatCostExact(detailData.record.cost_micro_usd)}
                      {detailData.record.price_source === 'unpriced' ? ' (未定价按 0)' : ` · ${detailData.record.price_source}`}
                    </span>
                  </div>
                  <div className="requests-summary-item">
                    <span className="requests-summary-item-label">Token 构成</span>
                    <span className="requests-summary-item-val">
                      输入 {detailData.record.tokens?.input ?? 0} / 输出 {detailData.record.tokens?.output ?? 0} / 总计 {detailData.record.tokens?.total ?? 0}
                    </span>
                  </div>

                  {detailData.record.price_source === 'unpriced' && (
                    <div style={{ gridColumn: '1 / -1' }}>
                      <Notice tone="warning">当前模型未配置价格规则，费用按 $0 核算</Notice>
                    </div>
                  )}

                  {detailData.record.accounting_quality === 'unclassified' && (
                    <div style={{ gridColumn: '1 / -1' }}>
                      <Notice tone="warning">Token 口径未分类：该模型返回的 Usage 未能映射至标准分类</Notice>
                    </div>
                  )}
                </div>
              )}

              {/* Tab 2: Redacted Headers */}
              {detailTab === 'headers' && (
                <div>
                  {Object.keys(detailData.headers || {}).length > 0 ? (
                    <table className="requests-headers-table">
                      <thead>
                        <tr>
                          <th>Header 名称（已脱敏）</th>
                          <th>值</th>
                        </tr>
                      </thead>
                      <tbody>
                        {redactHeaders(detailData.headers).map((h) => (
                          <tr key={h.name}>
                            <td>{h.name}</td>
                            <td>{h.value}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  ) : (
                    <EmptyState title="无请求头数据" />
                  )}
                </div>
              )}

              {/* Tab 3: Pretty JSON Body */}
              {detailTab === 'body' && (
                <div>
                  {detailData.body !== null && detailData.body !== undefined ? (
                    <div>
                      <pre className="requests-json-body">
                        {JSON.stringify(redactBody(detailData.body), null, 2)}
                      </pre>
                      <div className="requests-json-actions">
                        <Button
                          variant="secondary"
                          onClick={() =>
                            handleCopyJson(JSON.stringify(redactBody(detailData.body), null, 2))
                          }
                        >
                          复制 JSON
                        </Button>
                      </div>
                    </div>
                  ) : (
                    <Notice tone="info">
                      无可用正文（采集状态：{CAPTURE_STATUS_LABELS[detailData.capture_status] || detailData.capture_status}）
                    </Notice>
                  )}
                </div>
              )}
            </div>
          )}
        </Modal>
      )}
    </div>
  );
}

export default RequestsPage;
