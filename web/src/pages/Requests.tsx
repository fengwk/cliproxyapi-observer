import { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import type { PageProps } from '../lib/api';
import type {
  RequestView,
  Detail,
  ModelInfo,
  Key,
  FacetPage,
  FacetItem,
} from '../lib/types';
import { errorMessage } from '../lib/format';
import { Button, Notice, Select, type SelectOption } from '../components/ui';
import {
  type FilterState,
  type PresetRange,
  type ViewTab,
  getPresetRange,
  isDynamicRange,
  buildRequestViewParams,
  snapToWholeDateRange,
  isHistoricalDateQueryError,
} from './RequestsHelpers';
import { RequestsMetrics } from './RequestsMetrics';
import { RequestsTrend } from './RequestsTrend';
import { RequestsFilters } from './RequestsFilters';
import { RequestsTable } from './RequestsTable';
import { RequestsDetailModal } from './RequestsDetailModal';
import './Requests.css';

export interface RequestsPageProps extends PageProps {}

const pollOptions: SelectOption[] = [
  { value: '0', label: '关闭自动刷新' },
  { value: '5', label: '每 5 秒刷新' },
  { value: '10', label: '每 10 秒刷新' },
  { value: '30', label: '每 30 秒刷新' },
];

export function RequestsPage({ api, refreshInterval = 5, onDirtyChange }: RequestsPageProps) {
  // Preset range selection
  const [preset, setPreset] = useState<PresetRange>('today');

  // Filter states: draft vs applied
  const [draftFilters, setDraftFilters] = useState<FilterState>(() => {
    const todayBounds = getPresetRange('today');
    return {
      from: todayBounds.from,
      to: todayBounds.to,
      key_id: '',
      model: '',
      provider: '',
      auth_index: '',
      status: '',
    };
  });

  const [appliedFilters, setAppliedFilters] = useState<FilterState>(() => ({
    ...draftFilters,
  }));

  // Active view tab: requests, model, key, account, provider
  const [view, setView] = useState<ViewTab>('requests');

  // Pagination
  const [page, setPage] = useState<number>(1);

  // Dedicated poll interval for Requests page (defaults to 5s, options: 0/5/10/30)
  const [pollInterval, setPollInterval] = useState<number>(() =>
    refreshInterval !== undefined ? refreshInterval : 5
  );

  // View data (single-flight RequestView from GET /requests/view)
  const [viewData, setViewData] = useState<RequestView | null>(null);

  // Stable refs to prevent polling infinite loops
  const appliedFiltersRef = useRef(appliedFilters);
  appliedFiltersRef.current = appliedFilters;
  const viewRef = useRef(view);
  viewRef.current = view;
  const pageRef = useRef(page);
  pageRef.current = page;
  const viewDataRef = useRef(viewData);
  viewDataRef.current = viewData;

  // Metadata lists for dropdown filters
  const [keysList, setKeysList] = useState<Key[]>([]);
  const [availableModels, setAvailableModels] = useState<ModelInfo[]>([]);
  const [facetModels, setFacetModels] = useState<FacetItem[]>([]);
  const [facetKeys, setFacetKeys] = useState<FacetItem[]>([]);
  const [facetProviders, setFacetProviders] = useState<FacetItem[]>([]);
  const [facetAccounts, setFacetAccounts] = useState<FacetItem[]>([]);

  // Loading and error states
  const [loading, setLoading] = useState<boolean>(true);
  const [isRefreshing, setIsRefreshing] = useState<boolean>(false);
  const [queryError, setQueryError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [copiedKeyNotice, setCopiedKeyNotice] = useState<string | null>(null);

  // Detail Modal state
  const [activeDetailId, setActiveDetailId] = useState<string | null>(null);
  const [detailData, setDetailData] = useState<Detail | null>(null);
  const [detailLoading, setDetailLoading] = useState<boolean>(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  // Selection tracking (pause polling when user selects text or reads)
  const [isSelecting, setIsSelecting] = useState<boolean>(false);

  // Refs for race-condition prevention & single-flight aborts
  const seqRef = useRef<number>(0);
  const activeAbortControllerRef = useRef<AbortController | null>(null);
  const detailAbortControllerRef = useRef<AbortController | null>(null);
  const activeDetailIdRef = useRef<string | null>(null);
  const failureCountRef = useRef(0);
  const facetControllers = useRef<Record<string, AbortController>>({});

  const searchFacet = useCallback(async (field: string, search: string) => {
    facetControllers.current[field]?.abort();
    const controller = new AbortController();
    facetControllers.current[field] = controller;
    try {
      const params = new URLSearchParams({ field, search: search.slice(0, 256), limit: '100' });
      const result = await api.request<FacetPage>(`/requests/facets?${params}`, { signal: controller.signal });
      if (controller.signal.aborted) return;
      const setters: Record<string, (items: FacetItem[]) => void> = {
        model: setFacetModels, key_id: setFacetKeys, provider: setFacetProviders, auth_index: setFacetAccounts,
      };
      setters[field]?.(result.items ?? []);
    } catch { /* Preserve previous safe metadata choices on failure. */ }
  }, [api]);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      seqRef.current++;
      Object.values(facetControllers.current).forEach(controller => controller.abort());
      if (activeAbortControllerRef.current) {
        activeAbortControllerRef.current.abort();
        activeAbortControllerRef.current = null;
      }
      if (detailAbortControllerRef.current) {
        detailAbortControllerRef.current.abort();
        detailAbortControllerRef.current = null;
      }
    };
  }, []);

  // Listen to text selection to pause auto-refresh
  useEffect(() => {
    const handleSelectionChange = () => {
      const sel = window.getSelection();
      const hasSelection = Boolean(sel && sel.toString().trim().length > 0);
      setIsSelecting(hasSelection);
    };
    document.addEventListener('selectionchange', handleSelectionChange);
    return () => {
      document.removeEventListener('selectionchange', handleSelectionChange);
    };
  }, []);

  // Dirty state tracking (draft vs applied)
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

  useEffect(() => {
    onDirtyChange(isDirty);
  }, [isDirty, onDirtyChange]);

  // Keys Map for fast ID resolution
  const keysMap = useMemo(() => {
    const map = new Map<string, Key>();
    for (const k of keysList) {
      if (k && k.id) map.set(k.id, k);
    }
    return map;
  }, [keysList]);

  // 1. Initial metadata loading: Keys, Models, Facets
  useEffect(() => {
    let unmounted = false;
    const controller = new AbortController();

    async function loadMetadata() {
      // Fetch /keys
      try {
        const res = await api.request<{ items: Key[] }>('/keys?include_deleted=true', {
          signal: controller.signal,
        });
        if (!unmounted && res && Array.isArray(res.items)) {
          setKeysList(res.items);
        }
      } catch {
        /* tolerated */
      }

      // Fetch api.models()
      try {
        const list = await api.models({ signal: controller.signal });
        if (!unmounted && Array.isArray(list)) {
          setAvailableModels(list);
        }
      } catch {
        /* tolerated */
      }

      // Fetch facets for model, key_id, provider, auth_index
      const facetFields = ['model', 'key_id', 'provider', 'auth_index'] as const;
      await Promise.all(
        facetFields.map(async (field) => {
          try {
            const pageRes = await api.request<FacetPage>(
              `/requests/facets?field=${field}`,
              { signal: controller.signal }
            );
            if (!unmounted && pageRes && Array.isArray(pageRes.items)) {
              if (field === 'model') setFacetModels(pageRes.items);
              else if (field === 'key_id') setFacetKeys(pageRes.items);
              else if (field === 'provider') setFacetProviders(pageRes.items);
              else if (field === 'auth_index') setFacetAccounts(pageRes.items);
            }
          } catch {
            /* tolerated */
          }
        })
      );
    }

    loadMetadata();
    return () => {
      unmounted = true;
      controller.abort();
    };
  }, [api]);

  // 2. Fetch RequestView (Single-flight abort + seq)
  const fetchRequestView = useCallback(
    async (
      signal?: AbortSignal,
      targetFilters = appliedFiltersRef.current,
      targetView = viewRef.current,
      targetPage = pageRef.current,
      _isUserTriggered = false
    ) => {
      if (signal?.aborted) return;

      // Abort previous fetch
      if (activeAbortControllerRef.current) {
        activeAbortControllerRef.current.abort();
      }
      const localController = new AbortController();
      activeAbortControllerRef.current = localController;

      let onAbort: (() => void) | undefined;
      if (signal) {
        onAbort = () => localController.abort();
        signal.addEventListener('abort', onAbort, { once: true });
      }

      const seq = ++seqRef.current;
      if (!viewDataRef.current) {
        setLoading(true);
      } else {
        setIsRefreshing(true);
      }

      try {
        const queryStr = buildRequestViewParams(
          targetFilters,
          targetView,
          targetPage,
          25
        );
        const res = await api.request<RequestView>(`/requests/view?${queryStr}`, {
          signal: localController.signal,
        });

        if (seq !== seqRef.current || localController.signal.aborted) {
          return;
        }

        setViewData(res);
        failureCountRef.current = 0;
        setQueryError(null);
        setLastUpdated(new Date());
      } catch (err: unknown) {
        if (seq !== seqRef.current || localController.signal.aborted) {
          return;
        }
        // Background refresh error retains old viewData! Does not pop toasts.
        setQueryError(errorMessage(err) || '加载请求数据失败');
        failureCountRef.current++;
      } finally {
        if (signal && onAbort) {
          signal.removeEventListener('abort', onAbort);
        }
        if (seq === seqRef.current) {
          setLoading(false);
          setIsRefreshing(false);
          if (activeAbortControllerRef.current === localController) {
            activeAbortControllerRef.current = null;
          }
        }
      }
    },
    [api]
  );

  // 3. Polling via usePoll
  // Dedicated poll interval (0 disables polling repeats, not initial load)
  // Auto-refresh is paused when: page > 1, filters dirty, modal open, historical range, or text selected
  const isDynamic = isDynamicRange(appliedFilters.to);
  const isPollingPaused =
    page !== 1 ||
    isDirty ||
    activeDetailId !== null ||
    !isDynamic ||
    isSelecting;

  // Initial data load is independent of the timer. Resuming a paused timer
  // must not start a second request alongside an explicit Query action.
  useEffect(() => { void fetchRequestView(); }, [fetchRequestView]);
  useEffect(() => {
    if (pollInterval <= 0 || isPollingPaused) return;
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    const schedule = () => {
      timer = setTimeout(async () => {
        if (!document.hidden && !activeAbortControllerRef.current) {
          await fetchRequestView();
        }
        if (!disposed) schedule();
      }, Math.min(30000, pollInterval * 1000 * 2 ** Math.min(failureCountRef.current, 3)));
    };
    schedule();
    return () => { disposed = true; clearTimeout(timer); };
  }, [fetchRequestView, isPollingPaused, pollInterval]);

  // 4. Detail Fetch on demand
  const handleOpenDetail = useCallback(
    async (id: string) => {
      activeDetailIdRef.current = id;
      setActiveDetailId(id);
      setDetailData(null);
      setDetailError(null);
      setDetailLoading(true);

      if (detailAbortControllerRef.current) {
        detailAbortControllerRef.current.abort();
      }
      const controller = new AbortController();
      detailAbortControllerRef.current = controller;

      try {
        const res = await api.request<Detail>(
          `/requests/detail?id=${encodeURIComponent(id)}`,
          { signal: controller.signal }
        );

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
    activeDetailIdRef.current = null;
    if (detailAbortControllerRef.current) {
      detailAbortControllerRef.current.abort();
      detailAbortControllerRef.current = null;
    }
    setActiveDetailId(null);
    setDetailData(null);
    setDetailError(null);
    setDetailLoading(false);
  }, []);

  // 5. Filter Actions
  const handlePresetChange = useCallback((nextPreset: PresetRange) => {
    setPreset(nextPreset);
    if (nextPreset !== 'custom') {
      const bounds = getPresetRange(nextPreset);
      setDraftFilters((prev) => ({ ...prev, from: bounds.from, to: bounds.to }));
    }
  }, []);

  const handleResetFilters = useCallback(() => {
    const todayBounds = getPresetRange('today');
    setPreset('today');
    setDraftFilters({
      from: todayBounds.from,
      to: todayBounds.to,
      key_id: '',
      model: '',
      provider: '',
      auth_index: '',
      status: '',
    });
  }, []);

  const handleApplyFilters = useCallback(() => {
    setAppliedFilters({ ...draftFilters });
    setPage(1);
    fetchRequestView(undefined, draftFilters, view, 1, true);
  }, [draftFilters, view, fetchRequestView]);

  const handleViewChange = useCallback(
    (nextView: ViewTab) => {
      setView(nextView);
      setPage(1);
      fetchRequestView(undefined, appliedFilters, nextView, 1, true);
    },
    [appliedFilters, fetchRequestView]
  );

  const handlePageChange = useCallback(
    (newPage: number) => {
      if (newPage >= 1) {
        setPage(newPage);
        fetchRequestView(undefined, appliedFilters, view, newPage, true);
      }
    },
    [appliedFilters, view, fetchRequestView]
  );

  const handleManualRefresh = useCallback(() => {
    fetchRequestView(undefined, appliedFilters, view, page, true);
  }, [appliedFilters, view, page, fetchRequestView]);

  const handleCorrectiveWholeDate = useCallback(() => {
    const snapped = snapToWholeDateRange(draftFilters.from, draftFilters.to);
    const correctedDraft = { ...draftFilters, from: snapped.from, to: snapped.to };
    setDraftFilters(correctedDraft);
    setAppliedFilters(correctedDraft);
    setPage(1);
    setPreset('custom');
    fetchRequestView(undefined, correctedDraft, view, 1, true);
  }, [draftFilters, view, fetchRequestView]);

  // Copy Key ID with inline notification
  const handleCopyKey = useCallback(async (fullId: string) => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(fullId);
      setCopiedKeyNotice(`已复制完整 Key ID: ${fullId.slice(0, 16)}…`);
      setTimeout(() => setCopiedKeyNotice(null), 3000);
    } catch {
      setCopiedKeyNotice('复制失败，请从 Key ID 提示中复制完整标识。');
      setTimeout(() => setCopiedKeyNotice(null), 3000);
    }
  }, []);

  // Deduplicate Models: availableModels + facetModels
  const modelOptions = useMemo<SelectOption[]>(() => {
    const opts: SelectOption[] = [{ value: '', label: '全部模型' }];
    const seen = new Set<string>();

    for (const m of availableModels) {
      if (m.id && !seen.has(m.id)) {
        seen.add(m.id);
        opts.push({ value: m.id, label: m.id });
      }
    }

    for (const f of facetModels) {
      if (f.value && !seen.has(f.value)) {
        seen.add(f.value);
        opts.push({
          value: f.value,
          label: f.label ? f.label : `${f.value} (历史)`,
        });
      }
    }

    if (draftFilters.model && !seen.has(draftFilters.model)) {
      opts.push({
        value: draftFilters.model,
        label: `${draftFilters.model} (自定义)`,
      });
    }

    return opts;
  }, [availableModels, facetModels, draftFilters.model]);

  // Keys Options: /keys + facetKeys + deleted mark
  const keyOptions = useMemo<SelectOption[]>(() => {
    const opts: SelectOption[] = [{ value: '', label: '全部 Key' }];
    const seen = new Set<string>();

    for (const k of keysList) {
      if (k && k.id && !seen.has(k.id)) {
        seen.add(k.id);
        const isDeleted = Boolean(k.deleted_at);
        const namePart = k.label || (k.id.length === 64 ? `${k.id.slice(0, 12)}…` : k.id);
        const label = isDeleted ? `${namePart} (已删除)` : namePart;
        opts.push({ value: k.id, label });
      }
    }

    for (const f of facetKeys) {
      if (f.value && !seen.has(f.value)) {
        seen.add(f.value);
        const label = f.label
          ? f.label
          : f.value.length === 64
          ? `${f.value.slice(0, 12)}…`
          : f.value;
        opts.push({ value: f.value, label });
      }
    }

    if (draftFilters.key_id && !seen.has(draftFilters.key_id)) {
      const kId = draftFilters.key_id;
      opts.push({
        value: kId,
        label: kId.length === 64 ? `未知 Key (${kId.slice(0, 12)}…)` : `未知 Key (${kId})`,
      });
    }

    return opts;
  }, [keysList, facetKeys, draftFilters.key_id]);

  // Provider Options
  const providerOptions = useMemo<SelectOption[]>(() => {
    const opts: SelectOption[] = [{ value: '', label: '全部提供商' }];
    const seen = new Set<string>();

    for (const p of facetProviders) {
      if (p.value && !seen.has(p.value)) {
        seen.add(p.value);
        opts.push({ value: p.value, label: p.label || p.value });
      }
    }

    if (draftFilters.provider && !seen.has(draftFilters.provider)) {
      opts.push({ value: draftFilters.provider, label: draftFilters.provider });
    }

    return opts;
  }, [facetProviders, draftFilters.provider]);

  // Account Options
  const accountOptions = useMemo<SelectOption[]>(() => {
    const opts: SelectOption[] = [{ value: '', label: '全部账户' }];
    const seen = new Set<string>();

    for (const a of facetAccounts) {
      if (a.value && !seen.has(a.value)) {
        seen.add(a.value);
        opts.push({ value: a.value, label: a.label || a.value });
      }
    }

    if (draftFilters.auth_index && !seen.has(draftFilters.auth_index)) {
      opts.push({ value: draftFilters.auth_index, label: draftFilters.auth_index });
    }

    return opts;
  }, [facetAccounts, draftFilters.auth_index]);

  // Check if historical error
  const isDateError = isHistoricalDateQueryError(
    queryError,
    appliedFilters.from,
    appliedFilters.to
  );

  return (
    <div className="requests-page" data-testid="requests-page">
      {/* 1. Header with Title, Polling interval, and Manual refresh */}
      <header className="requests-header">
        <div className="requests-title-group">
          <h1 className="requests-title">请求日志</h1>
          <span className="requests-title-hint">
            {lastUpdated
              ? `更新于 ${lastUpdated.toTimeString().slice(0, 8)}`
              : '正在加载...'}
          </span>
          {isRefreshing && (
            <span className="requests-refreshing-badge" aria-label="后台更新中">
              更新中...
            </span>
          )}
        </div>

        <div className="requests-header-controls">
          {/* Pause reason hint */}
          {isPollingPaused && pollInterval > 0 && (
            <span className="requests-paused-hint">
              {page !== 1
                ? '分页浏览中，已暂停自动刷新'
                : isDirty
                ? '筛选未应用，已暂停自动刷新'
                : activeDetailId !== null
                ? '详情查看中，已暂停自动刷新'
                : !isDynamic
                ? '历史区间已暂停自动刷新'
                : isSelecting
                ? '文本选择中，已暂停自动刷新'
                : '自动刷新已暂停'}
            </span>
          )}

          {/* Local poll interval select */}
          <div className="requests-poll-selector">
            <Select
              value={String(pollInterval)}
              onChange={(val) => setPollInterval(Number(val))}
              options={pollOptions}
              aria-label="自动刷新周期"
              className="cpa-select-compact"
            />
          </div>

          {/* Manual Refresh Button */}
          <Button
            variant="secondary"
            className="cpa-btn-sm"
            onClick={handleManualRefresh}
            loading={loading && !viewData}
            title="立即手动刷新当前视图"
          >
            刷新
          </Button>
        </div>
      </header>

      {/* Copy notification */}
      {copiedKeyNotice && (
        <div className="requests-action-notice">
          <Notice tone="info">{copiedKeyNotice}</Notice>
        </div>
      )}

      {/* Query Error Notice */}
      {queryError && (
        <div className="requests-error-notice">
          <Notice tone="danger">
            <span>{queryError}</span>
            {isDateError ? (
              <Button
                variant="secondary"
                className="cpa-btn-sm"
                style={{ marginLeft: 12 }}
                onClick={handleCorrectiveWholeDate}
              >
                修正为整日 UTC 范围
              </Button>
            ) : (
              <Button
                variant="secondary"
                className="cpa-btn-sm"
                style={{ marginLeft: 12 }}
                onClick={handleManualRefresh}
              >
                重试
              </Button>
            )}
          </Notice>
        </div>
      )}

      {/* 2. Flat Filters */}
      <RequestsFilters
        draft={draftFilters}
        onDraftChange={setDraftFilters}
        onApply={handleApplyFilters}
        onReset={handleResetFilters}
        isDirty={isDirty}
        loading={loading && !viewData}
        keyOptions={keyOptions}
        modelOptions={modelOptions}
        providerOptions={providerOptions}
        accountOptions={accountOptions}
        preset={preset}
        onPresetChange={handlePresetChange}
        onFacetSearch={searchFacet}
      />

      {/* 3. Metrics (6 cards + coverage) */}
      <RequestsMetrics
        summary={viewData?.summary ?? null}
        coverage={viewData?.coverage}
        loading={loading && !viewData}
      />

      {/* 4. Trend Chart (fixed height ~180px, no jitter) */}
      <RequestsTrend
        series={viewData?.series}
        loading={loading && !viewData}
      />

      {/* 5. Table Area (5 views tabs + records/groups table + pagination) */}
      <RequestsTable
        view={view}
        onViewChange={handleViewChange}
        records={viewData?.records}
        groups={viewData?.groups}
        page={page}
        hasMore={Boolean(viewData?.pagination?.has_more)}
        onPageChange={handlePageChange}
        keysMap={keysMap}
        onOpenDetail={handleOpenDetail}
        onCopyKey={handleCopyKey}
        loading={loading && !viewData}
      />

      {/* 6. Detail Modal */}
      {activeDetailId !== null && (
        <RequestsDetailModal
          detail={detailData}
          loading={detailLoading}
          error={detailError}
          onClose={handleCloseDetail}
          keysMap={keysMap}
        />
      )}
    </div>
  );
}
