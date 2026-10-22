import React, { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import {
  Button,
  Field,
  Input,
  Select,
  Switch,
  Modal,
  Notice,
  Badge,
  EmptyState,
} from '../components/ui';
import type { PageProps } from '../lib/api';
import type {
  Settings,
  PricingConfig,
  ManualPrice,
  PricingSource,
  PriceSnapshot,
  PricePreview,
  PricePreviewItem,
  Health,
  ModelInfo,
  ModelMapping,
  CleanupResult,
} from '../lib/types';
import { errorMessage, formatDateTime } from '../lib/format';
import {
  formatBytes,
  formatInt,
  formatRateDisplay,
  formatReasoningDisplay,
  sourceLabel,
  pricingSourceLabel,
  validateGeneralSettings,
  definitionToDraft,
  emptyDefinitionDraft,
  parseManualPrices,
  areGeneralSettingsEqual,
  isPricingDraftEqual,
  GeneralSettingsDraft,
  ManualPriceDraft,
  CapacityUnit,
  splitBytesToCapacity,
  parseCapacityToBytes,
  DEFAULT_MAX_BODY_BYTES,
  DEFAULT_MAX_BODY_STORAGE_BYTES,
  MAX_CONFIG_ENTRIES,
  MAX_RAW_RETENTION_HOURS,
  MAX_DAILY_RETENTION_DAYS,
} from './Settings.helpers';
import { PriceDefinitionEditor } from './PricingDefinitionEditor';
import './Settings.css';

const DEFAULT_SETTINGS: Settings = {
  capture_enabled: false,
  max_body_bytes: DEFAULT_MAX_BODY_BYTES,
  max_body_storage_bytes: DEFAULT_MAX_BODY_STORAGE_BYTES,
  raw_retention_hours: 24,
  daily_retention_days: 365,
  pricing: {
    auto: true,
    source: 'models_dev_v2',
    prices: [],
  },
};

const DEFAULT_SNAPSHOT: PriceSnapshot = {
  prices: [],
  last_sync: '',
  refreshing: false,
  catalog_entries: 0,
  source: 'models_dev_v2',
};

const DEFAULT_HEALTH: Health = {
  database_bytes: 0,
  dropped_captures: 0,
  pending_usage: 0,
  active_requests: 0,
};

const CAPACITY_OPTIONS = [
  { value: 'B', label: '字节 (B)' },
  { value: 'KB', label: 'KiB (1,024 字节)' },
  { value: 'MB', label: 'MiB (1,048,576 字节)' },
  { value: 'GB', label: 'GiB (1,073,741,824 字节)' },
];

export const SettingsPage: React.FC<PageProps> = ({ api, onDirtyChange, onLogout }) => {
  // ---------------------------------------------------------------------------
  // Lifecycle & concurrency guards
  // ---------------------------------------------------------------------------
  const isMountedRef = useRef(true);
  const lifetimeAbortRef = useRef<AbortController | null>(null);
  const hasLoadedSuccessfullyRef = useRef(false);
  const isMutatingRef = useRef(false);
  const previewGenRef = useRef(0);
  const loadControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    isMountedRef.current = true;
    lifetimeAbortRef.current = new AbortController();
    return () => {
      isMountedRef.current = false;
      lifetimeAbortRef.current?.abort();
    };
  }, []);

  // ---------------------------------------------------------------------------
  // Server-confirmed state
  // ---------------------------------------------------------------------------
  const [confirmedSettings, setConfirmedSettings] = useState<Settings>(DEFAULT_SETTINGS);
  const [snapshot, setSnapshot] = useState<PriceSnapshot>(DEFAULT_SNAPSHOT);
  const [health, setHealth] = useState<Health>(DEFAULT_HEALTH);
  const [availableModels, setAvailableModels] = useState<ModelInfo[]>([]);
  const [previewItems, setPreviewItems] = useState<PricePreviewItem[]>([]);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // Initial load status
  const [initialLoading, setInitialLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [stateRefreshing, setStateRefreshing] = useState(false);

  // ---------------------------------------------------------------------------
  // Section 1: 请求记录 Draft
  // ---------------------------------------------------------------------------
  const [generalDraft, setGeneralDraft] = useState<GeneralSettingsDraft>({
    capture_enabled: false,
    max_body_bytes: DEFAULT_MAX_BODY_BYTES,
    max_body_storage_bytes: DEFAULT_MAX_BODY_STORAGE_BYTES,
    raw_retention_hours: 24,
    daily_retention_days: 365,
  });
  const [rawPreset, setRawPreset] = useState<string>('24');
  const [dailyPreset, setDailyPreset] = useState<string>('365');

  // Exact number + unit inputs for capacity
  const [maxBodyAmount, setMaxBodyAmount] = useState<string>('1');
  const [maxBodyUnit, setMaxBodyUnit] = useState<CapacityUnit>('MB');
  const [maxStorageAmount, setMaxStorageAmount] = useState<string>('256');
  const [maxStorageUnit, setMaxStorageUnit] = useState<CapacityUnit>('MB');

  const [generalSaving, setGeneralSaving] = useState(false);
  const [generalStatus, setGeneralStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // ---------------------------------------------------------------------------
  // Section 2: 模型价格 Draft
  // ---------------------------------------------------------------------------
  const [pricingAuto, setPricingAuto] = useState<boolean>(true);
  const [pricingSource, setPricingSource] = useState<PricingSource>('models_dev_v2');
  const [manualDrafts, setManualDrafts] = useState<ManualPriceDraft[]>([]);
  const [mappingDrafts, setMappingDrafts] = useState<ModelMapping[]>([]);
  const [mappingItem, setMappingItem] = useState<PricePreviewItem | null>(null);
  const [pricingSaving, setPricingSaving] = useState(false);
  const [pricingRefreshing, setPricingRefreshing] = useState(false);
  const [pricingStatus, setPricingStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // Preview filtering & pagination
  const [previewFilter, setPreviewFilter] = useState('');
  const [previewPage, setPreviewPage] = useState(1);
  const PREVIEW_PAGE_SIZE = 25;
  const [previewStatus, setPreviewStatus] = useState('all');

  // ---------------------------------------------------------------------------
  // Section 3: 存储维护
  // ---------------------------------------------------------------------------
  const [compactModalOpen, setCompactModalOpen] = useState(false);
  const [contentOperation, setContentOperation] = useState<'cleanup' | 'purge' | null>(null);
  const [compacting, setCompacting] = useState(false);
  const [compactStatus, setCompactStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // Global mutation busy guard: mutual exclusion across save, compact, and price refresh
  const isMutationBusy = generalSaving || pricingSaving || compacting || pricingRefreshing || stateRefreshing;

  // Ref tracking dirty state to protect drafts from poll overwrite
  const isGeneralDirtyRef = useRef(false);
  const isPricingDirtyRef = useRef(false);

  // ---------------------------------------------------------------------------
  // Helper: sync drafts from confirmed Settings
  // ---------------------------------------------------------------------------
  const syncGeneralFromConfirmed = useCallback((settings: Settings) => {
    const bBytes = settings.max_body_bytes ?? DEFAULT_MAX_BODY_BYTES;
    const sBytes = settings.max_body_storage_bytes ?? DEFAULT_MAX_BODY_STORAGE_BYTES;

    setGeneralDraft({
      capture_enabled: settings.capture_enabled === true,
      capture_mode: settings.capture_mode === 'headers' ? 'headers' : 'headers_and_body',
      capture_retention_seconds: settings.capture_retention_seconds ?? (settings.raw_retention_hours ?? 24) * 3600,
      max_body_bytes: bBytes,
      max_body_storage_bytes: sBytes,
      raw_retention_hours: settings.raw_retention_hours ?? 24,
      daily_retention_days: settings.daily_retention_days ?? 365,
    });
    const rh = settings.raw_retention_hours ?? 24;
    setRawPreset(['24', '72', '168'].includes(String(rh)) ? String(rh) : 'custom');

    const dd = settings.daily_retention_days ?? 365;
    setDailyPreset(['90', '180', '365'].includes(String(dd)) ? String(dd) : 'custom');

    const bodyCap = splitBytesToCapacity(bBytes);
    setMaxBodyAmount(bodyCap.amount);
    setMaxBodyUnit(bodyCap.unit);

    const storageCap = splitBytesToCapacity(sBytes);
    setMaxStorageAmount(storageCap.amount);
    setMaxStorageUnit(storageCap.unit);
  }, []);

  const syncPricingFromConfirmed = useCallback((pricing: PricingConfig) => {
    setPricingAuto(pricing.auto !== false);
    setPricingSource(pricing.source === 'models_dev_v1' ? 'models_dev_v1' : 'models_dev_v2');
    setMappingDrafts(pricing.mappings ?? []);

    const manuals: ManualPriceDraft[] = (pricing.prices || []).map((price, index) => ({
      id: `manual-${index}-${price.model}-${price.provider ?? ''}`,
      model: price.model,
      provider: price.provider ?? '',
      definition: definitionToDraft(price.definition),
    }));
    setManualDrafts(manuals);
  }, []);

  // ---------------------------------------------------------------------------
  // Dirty state calculation
  // ---------------------------------------------------------------------------
  const isGeneralDirty = useMemo(() => {
    let bodyBytes = generalDraft.max_body_bytes;
    try {
      bodyBytes = parseCapacityToBytes(maxBodyAmount, maxBodyUnit, '');
    } catch {
      return true;
    }
    let storageBytes = generalDraft.max_body_storage_bytes;
    try {
      storageBytes = parseCapacityToBytes(maxStorageAmount, maxStorageUnit, '');
    } catch {
      return true;
    }

    const currentDraft: GeneralSettingsDraft = {
      ...generalDraft,
      max_body_bytes: bodyBytes,
      max_body_storage_bytes: storageBytes,
    };

    const confirmedGen: GeneralSettingsDraft = {
      capture_enabled: confirmedSettings.capture_enabled === true,
      capture_mode: confirmedSettings.capture_mode === 'headers' ? 'headers' : 'headers_and_body',
      capture_retention_seconds: confirmedSettings.capture_retention_seconds,
      max_body_bytes: confirmedSettings.max_body_bytes ?? DEFAULT_MAX_BODY_BYTES,
      max_body_storage_bytes: confirmedSettings.max_body_storage_bytes ?? DEFAULT_MAX_BODY_STORAGE_BYTES,
      raw_retention_hours: confirmedSettings.raw_retention_hours ?? 24,
      daily_retention_days: confirmedSettings.daily_retention_days ?? 365,
    };
    return !areGeneralSettingsEqual(currentDraft, confirmedGen);
  }, [generalDraft, maxBodyAmount, maxBodyUnit, maxStorageAmount, maxStorageUnit, confirmedSettings]);

  const isPricingDirty = useMemo(() => {
    return !isPricingDraftEqual(pricingAuto, pricingSource, manualDrafts, confirmedSettings.pricing) ||
      JSON.stringify(mappingDrafts) !== JSON.stringify(confirmedSettings.pricing.mappings ?? []);
  }, [pricingAuto, pricingSource, manualDrafts, mappingDrafts, confirmedSettings.pricing]);

  isGeneralDirtyRef.current = isGeneralDirty;
  isPricingDirtyRef.current = isPricingDirty;

  useEffect(() => {
    onDirtyChange(isGeneralDirty || isPricingDirty);
  }, [isGeneralDirty, isPricingDirty, onDirtyChange]);

  // ---------------------------------------------------------------------------
  // Preview fetching
  // ---------------------------------------------------------------------------
  const fetchPreview = useCallback(
    async (models: ModelInfo[], signal?: AbortSignal) => {
      if (!models.length) {
        if (isMountedRef.current && !signal?.aborted) {
          setPreviewItems([]);
          setPreviewError(null);
        }
        return;
      }
      if (isMountedRef.current) {
        setPreviewLoading(true);
        setPreviewError(null);
      }
      const currentGen = ++previewGenRef.current;
      try {
        const payload = {
          models: models.slice(0, 4096).map((m) => ({
            model: m.id,
            provider: '', // Never guess provider from owned_by or labels; CPA directory available models send empty string
          })),
        };
        const res = await api.request<PricePreview>('/prices/preview', {
          method: 'POST',
          body: payload,
          signal,
        });
        if (!isMountedRef.current || signal?.aborted || currentGen !== previewGenRef.current) return;
        if (res && Array.isArray(res.items)) {
          setPreviewItems(res.items);
          setPreviewError(null);
        }
      } catch (err: unknown) {
        if (!isMountedRef.current || signal?.aborted || currentGen !== previewGenRef.current) return;
        setPreviewError(`无法读取可用模型价格预检：${errorMessage(err)}`);
      } finally {
        if (isMountedRef.current && !signal?.aborted && currentGen === previewGenRef.current) {
          setPreviewLoading(false);
        }
      }
    },
    [api]
  );

  // ---------------------------------------------------------------------------
  // Manual/entry load. Reads are cancellable and cannot overwrite a later save;
  // mutations wait for the current state refresh, and superseded loads abort.
  // ---------------------------------------------------------------------------
  const load = useCallback(
    async (outerSignal: AbortSignal) => {
      if (outerSignal.aborted || isMutatingRef.current) return;
      loadControllerRef.current?.abort();
      const controller = new AbortController();
      const signal = controller.signal;
      loadControllerRef.current = controller;
      const abort = () => controller.abort();
      outerSignal.addEventListener('abort', abort, { once: true });
      setStateRefreshing(true);
      try {
        let modelsRes: ModelInfo[] = [];
        let modelsErr: string | null = null;
        try {
          modelsRes = await api.models({ signal });
        } catch (err: unknown) {
          if (signal.aborted || !isMountedRef.current) return;
          modelsErr = errorMessage(err) || '获取可用模型列表失败';
        }

        const [settingsRes, pricesRes, healthRes] = await Promise.all([
          api.request<Settings>('/settings', { signal }),
          api.request<PriceSnapshot>('/prices', { signal }),
          api.request<Health>('/health', { signal }),
        ]);

        if (signal.aborted || !isMountedRef.current) return;

        hasLoadedSuccessfullyRef.current = true;
        setConfirmedSettings(settingsRes);
        setSnapshot(pricesRes);
        setHealth(healthRes);

        // Always check dirty refs so background updates never overwrite user drafts
        if (!isGeneralDirtyRef.current) {
          syncGeneralFromConfirmed(settingsRes);
        }
        if (!isPricingDirtyRef.current) {
          syncPricingFromConfirmed(settingsRes.pricing);
        }

        if (modelsErr) {
          setPreviewError(`无法读取可用模型：${modelsErr}`);
        } else {
          setAvailableModels(modelsRes);
          await fetchPreview(modelsRes, signal);
        }

        setLoadError(null);
      } catch (err: unknown) {
        if (signal.aborted || !isMountedRef.current) return;
        setLoadError(errorMessage(err) || '加载设置失败');
      } finally {
        outerSignal.removeEventListener('abort', abort);
        if (loadControllerRef.current === controller) {
          loadControllerRef.current = null;
          if (isMountedRef.current) setStateRefreshing(false);
          if (!signal.aborted && isMountedRef.current) setInitialLoading(false);
        }
      }
    },
    [api, syncGeneralFromConfirmed, syncPricingFromConfirmed, fetchPreview]
  );

  // Settings is a draft editor, not a live dashboard: one entry load only.
  // No timer or modal/dirty transition may reload or unmount the price table.
  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const handleRetry = () => {
    setLoadError(null);
    if (!hasLoadedSuccessfullyRef.current) setInitialLoading(true);
    void load(lifetimeAbortRef.current!.signal);
  };

  // ---------------------------------------------------------------------------
  // Section 1: 请求记录 Handlers
  // ---------------------------------------------------------------------------
  const handleRawPresetChange = (val: string) => {
    setRawPreset(val);
    if (val !== 'custom') {
      const hours = Number(val);
      setGeneralDraft((prev) => ({ ...prev, raw_retention_hours: hours }));
    }
  };

  const handleDailyPresetChange = (val: string) => {
    setDailyPreset(val);
    if (val !== 'custom') {
      const days = Number(val);
      setGeneralDraft((prev) => ({ ...prev, daily_retention_days: days }));
    }
  };

  const handleMaxBodyAmountChange = (val: string) => {
    setMaxBodyAmount(val);
    try {
      const bytes = parseCapacityToBytes(val, maxBodyUnit, '单条请求体上限');
      setGeneralDraft((prev) => ({ ...prev, max_body_bytes: bytes }));
    } catch {
      // Keep string, validation in handleSaveGeneral will catch and report error if submitted
    }
  };

  const handleMaxBodyUnitChange = (val: string) => {
    const unit = val as CapacityUnit;
    setMaxBodyUnit(unit);
    try {
      const bytes = parseCapacityToBytes(maxBodyAmount, unit, '单条请求体上限');
      setGeneralDraft((prev) => ({ ...prev, max_body_bytes: bytes }));
    } catch {
      // Keep unit
    }
  };

  const handleMaxStorageAmountChange = (val: string) => {
    setMaxStorageAmount(val);
    try {
      const bytes = parseCapacityToBytes(val, maxStorageUnit, '请求体总存储上限');
      setGeneralDraft((prev) => ({ ...prev, max_body_storage_bytes: bytes }));
    } catch {
      // Keep string
    }
  };

  const handleMaxStorageUnitChange = (val: string) => {
    const unit = val as CapacityUnit;
    setMaxStorageUnit(unit);
    try {
      const bytes = parseCapacityToBytes(maxStorageAmount, unit, '请求体总存储上限');
      setGeneralDraft((prev) => ({ ...prev, max_body_storage_bytes: bytes }));
    } catch {
      // Keep unit
    }
  };

  const handleSaveGeneral = async (e?: React.FormEvent) => {
    if (e) e.preventDefault();
    if (isMutatingRef.current || !hasLoadedSuccessfullyRef.current || isMutationBusy) return;
    isMutatingRef.current = true;
    setGeneralStatus(null);

    let bodyBytes = generalDraft.max_body_bytes;
    try {
      bodyBytes = parseCapacityToBytes(maxBodyAmount, maxBodyUnit, '单条请求体上限');
    } catch (err) {
      isMutatingRef.current = false;
      setGeneralStatus({ tone: 'danger', message: errorMessage(err) });
      return;
    }

    let storageBytes = generalDraft.max_body_storage_bytes;
    try {
      storageBytes = parseCapacityToBytes(maxStorageAmount, maxStorageUnit, '请求体总存储上限');
    } catch (err) {
      isMutatingRef.current = false;
      setGeneralStatus({ tone: 'danger', message: errorMessage(err) });
      return;
    }

    const draftToSave: GeneralSettingsDraft = {
      ...generalDraft,
      max_body_bytes: bodyBytes,
      max_body_storage_bytes: storageBytes,
    };

    const err = validateGeneralSettings(draftToSave);
    if (err) {
      isMutatingRef.current = false;
      setGeneralStatus({ tone: 'danger', message: err });
      return;
    }

    setGeneralSaving(true);
    try {
      const payload: Settings = {
        capture_enabled: draftToSave.capture_enabled,
        capture_mode: draftToSave.capture_enabled ? (draftToSave.capture_mode ?? 'headers_and_body') : 'off',
        capture_retention_seconds: draftToSave.capture_retention_seconds ?? draftToSave.raw_retention_hours * 3600,
        max_body_bytes: draftToSave.max_body_bytes,
        max_body_storage_bytes: draftToSave.max_body_storage_bytes,
        raw_retention_hours: draftToSave.raw_retention_hours,
        daily_retention_days: draftToSave.daily_retention_days,
        // CRITICAL: Preserve the confirmed server pricing snapshot, not unsaved pricing drafts!
        pricing: confirmedSettings.pricing,
      };

      const res = await api.request<Settings>('/settings', {
        method: 'PUT',
        body: payload,
        signal: lifetimeAbortRef.current?.signal,
      });

      if (!isMountedRef.current) return;
      const updated = res || payload;
      setConfirmedSettings(updated);
      syncGeneralFromConfirmed(updated);
      setGeneralStatus({ tone: 'success', message: '请求记录与存储限制设置已保存' });
    } catch (saveErr) {
      if (!isMountedRef.current) return;
      setGeneralStatus({ tone: 'danger', message: errorMessage(saveErr) || '保存失败' });
    } finally {
      isMutatingRef.current = false;
      if (isMountedRef.current) {
        setGeneralSaving(false);
      }
    }
  };

  const handleCancelGeneral = () => {
    syncGeneralFromConfirmed(confirmedSettings);
    setGeneralStatus(null);
  };

  // ---------------------------------------------------------------------------
  // Section 2: 模型价格 Handlers
  // ---------------------------------------------------------------------------
  const handleRefreshCatalog = async () => {
    if (isMutatingRef.current || isMutationBusy) return;
    isMutatingRef.current = true;
    setPricingRefreshing(true);
    setPricingStatus(null);
    try {
      const res = await api.request<{ snapshot?: PriceSnapshot }>('/prices/refresh', {
        method: 'POST',
        signal: lifetimeAbortRef.current?.signal,
      });
      if (!isMountedRef.current) return;
      const nextSnapshot = res?.snapshot || (res as unknown as PriceSnapshot);

      if (nextSnapshot) {
        setSnapshot(nextSnapshot);
        // CRITICAL: Honest error handling. Even with HTTP 200, if snapshot.error is present,
        // never render "已刷新" and never erase cache!
        if (nextSnapshot.error) {
          setPricingStatus({
            tone: 'danger',
            message: `价格目录刷新失败，已保留有效缓存：${nextSnapshot.error}`,
          });
        } else {
          setPricingStatus({ tone: 'success', message: '价格目录已刷新' });
          fetchPreview(availableModels);
        }
      }
    } catch (err) {
      if (!isMountedRef.current) return;
      setPricingStatus({ tone: 'danger', message: errorMessage(err) || '刷新价格目录失败' });
    } finally {
      isMutatingRef.current = false;
      if (isMountedRef.current) {
        setPricingRefreshing(false);
      }
    }
  };

  const handleAddManualPrice = (initialModel = '', initialProvider = '') => {
    if (manualDrafts.length >= MAX_CONFIG_ENTRIES) {
      setPricingStatus({ tone: 'danger', message: `手工价格不能超过 ${MAX_CONFIG_ENTRIES} 条` });
      return;
    }
    const newDraft: ManualPriceDraft = {
      id: `manual-new-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      model: initialModel,
      provider: initialProvider,
      definition: emptyDefinitionDraft(),
    };
    setManualDrafts((prev) => [...prev, newDraft]);
  };

  const handleUpdateManualDraft = (index: number, patch: Partial<ManualPriceDraft>) => {
    setManualDrafts((prev) => {
      const next = [...prev];
      next[index] = { ...next[index], ...patch };
      return next;
    });
  };

  const handleRemoveManualPrice = (index: number) => {
    setManualDrafts((prev) => prev.filter((_, i) => i !== index));
  };

  const handleSavePricing = async () => {
    if (isMutatingRef.current || !hasLoadedSuccessfullyRef.current || isMutationBusy) return;
    isMutatingRef.current = true;
    setPricingStatus(null);

    let parsedPrices: ManualPrice[];
    try {
      parsedPrices = parseManualPrices(manualDrafts);
    } catch (err) {
      isMutatingRef.current = false;
      setPricingStatus({ tone: 'danger', message: errorMessage(err) });
      return;
    }

    setPricingSaving(true);
    try {
      const newPricing: PricingConfig = {
        auto: pricingAuto,
        source: pricingSource,
        prices: parsedPrices,
        mappings: mappingDrafts,
      };

      const payload: Settings = {
        // CRITICAL: Preserve the confirmed server general settings, not unsaved general drafts!
        capture_enabled: confirmedSettings.capture_enabled,
        capture_mode: confirmedSettings.capture_mode,
        capture_retention_seconds: confirmedSettings.capture_retention_seconds,
        max_body_bytes: confirmedSettings.max_body_bytes,
        max_body_storage_bytes: confirmedSettings.max_body_storage_bytes,
        raw_retention_hours: confirmedSettings.raw_retention_hours,
        daily_retention_days: confirmedSettings.daily_retention_days,
        pricing: newPricing,
      };

      const res = await api.request<Settings>('/settings', {
        method: 'PUT',
        body: payload,
        signal: lifetimeAbortRef.current?.signal,
      });

      if (!isMountedRef.current) return;
      const updated = res || payload;
      setConfirmedSettings(updated);
      syncPricingFromConfirmed(updated.pricing);
      setPricingStatus({ tone: 'success', message: '价格设置已保存' });

      // Update preview to reflect newly saved manual definitions.
      fetchPreview(availableModels);
    } catch (saveErr) {
      if (!isMountedRef.current) return;
      setPricingStatus({ tone: 'danger', message: errorMessage(saveErr) || '保存失败' });
    } finally {
      isMutatingRef.current = false;
      if (isMountedRef.current) {
        setPricingSaving(false);
      }
    }
  };

  const handleCancelPricing = () => {
    syncPricingFromConfirmed(confirmedSettings.pricing);
    setPricingStatus(null);
  };

  // Filtered and paginated preview items
  const filteredPreview = useMemo(() => {
    const lower = previewFilter.trim().toLowerCase();
    return previewItems.filter((item) => {
      const ambiguous = item.reason?.includes('ambiguous') || (item.candidates?.length ?? 0) > 1;
      const matchesStatus =
        previewStatus === 'all' ||
        (previewStatus === 'matched'
          ? item.source === 'manual' || item.source === 'auto'
          : previewStatus === 'ambiguous'
            ? ambiguous
            : item.source === 'zero');
      return (
        matchesStatus &&
        (item.model.toLowerCase().includes(lower) ||
          (item.provider && item.provider.toLowerCase().includes(lower)))
      );
    });
  }, [previewItems, previewFilter, previewStatus]);

  const totalPreviewPages = Math.max(1, Math.ceil(filteredPreview.length / PREVIEW_PAGE_SIZE));
  const currentPreviewPageItems = useMemo(() => {
    const start = (previewPage - 1) * PREVIEW_PAGE_SIZE;
    return filteredPreview.slice(start, start + PREVIEW_PAGE_SIZE);
  }, [filteredPreview, previewPage]);

  // ---------------------------------------------------------------------------
  // Section 3: 存储维护 Handlers
  // ---------------------------------------------------------------------------
  const handleCompactConfirm = async () => {
    if (isMutatingRef.current || isMutationBusy) return;
    isMutatingRef.current = true;
    setCompacting(true);
    setCompactStatus(null);
    try {
      const res = await api.request<Health | { health?: Health }>('/compact', {
        method: 'POST',
        signal: lifetimeAbortRef.current?.signal,
      });
      if (!isMountedRef.current) return;
      const nextHealth = res && 'database_bytes' in res ? (res as Health) : (res as { health?: Health })?.health;
      if (nextHealth) {
        setHealth(nextHealth);
      }
      setCompactStatus({ tone: 'success', message: '数据库压缩整理完成' });
      setCompactModalOpen(false);
    } catch (err) {
      if (!isMountedRef.current) return;
      setCompactStatus({ tone: 'danger', message: errorMessage(err) || '压缩数据库失败' });
      setCompactModalOpen(false);
    } finally {
      isMutatingRef.current = false;
      if (isMountedRef.current) {
        setCompacting(false);
      }
    }
  };

  const handleContentConfirm = async () => {
    if (!contentOperation || isMutatingRef.current || isMutationBusy) return;
    isMutatingRef.current = true;
    setCompacting(true);
    setCompactStatus(null);
    try {
      const result = await api.request<CleanupResult>(`/captures/${contentOperation}`, {
        method: 'POST', body: contentOperation === 'purge' ? { confirm: true } : undefined,
        signal: lifetimeAbortRef.current?.signal,
      });
      if (!isMountedRef.current) return;
      setCompactStatus({ tone: 'success', message: `已移除 ${result.removed_records} 条内容，释放逻辑空间 ${formatBytes(result.removed_bytes)}；物理文件需要另外整理。额度与费用未改变。` });
      setContentOperation(null);
      const next = await api.request<Health>('/health', { signal: lifetimeAbortRef.current?.signal });
      if (isMountedRef.current) setHealth(next);
    } catch (err) {
      if (isMountedRef.current) setCompactStatus({ tone: 'danger', message: errorMessage(err) || '维护失败，请稍后重试' });
    } finally {
      isMutatingRef.current = false;
      if (isMountedRef.current) setCompacting(false);
    }
  };

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------
  if (initialLoading) {
    return (
      <div className="settings-page" data-testid="settings-loading">
        <Notice tone="info">正在加载设置…</Notice>
      </div>
    );
  }

  if (loadError && !hasLoadedSuccessfullyRef.current) {
    return (
      <div className="settings-page" data-testid="settings-error">
        <Notice tone="danger">{loadError}</Notice>
        <div style={{ marginTop: 12 }}>
          <Button variant="secondary" onClick={handleRetry}>
            重试
          </Button>
        </div>
      </div>
    );
  }

  const hasFault = Boolean(health.last_error || health.maintenance_error || health.pending_usage > 0);
  const isLastSyncValid = Boolean(snapshot.last_sync && !snapshot.last_sync.startsWith('0001'));

  return (
    <div className="settings-page" data-testid="observer-settings">
      <div className="settings-toolbar">
        <Button variant="secondary" disabled={isMutationBusy} onClick={handleRetry}>刷新状态</Button>
      </div>
      {loadError && <Notice tone="danger">{loadError}（保留上次成功状态，草稿未覆盖）</Notice>}
      {/* =================================================================== */}
      {/* Section 1: 请求记录 */}
      {/* =================================================================== */}
      <section className="settings-section" aria-labelledby="settings-general-heading">
        <div className="settings-section-header">
          <h2 className="settings-section-title" id="settings-general-heading">
            请求记录
          </h2>
          {isGeneralDirty && <Badge tone="warning">有未保存更改</Badge>}
        </div>
        <p className="settings-section-desc">
          配置请求体保存策略与数据保留周期。敏感请求体默认关闭，以保障数据私密性。
        </p>

        {generalStatus && <Notice tone={generalStatus.tone}>{generalStatus.message}</Notice>}

        <form onSubmit={handleSaveGeneral}>
          <div className="settings-toggle-row">
            <div className="settings-toggle-info">
              <span className="settings-toggle-title">保存请求内容</span>
              <span className="settings-toggle-hint">
                {generalDraft.capture_enabled
                  ? '已开启：保存脱敏请求头和请求体，可能包含敏感提示词；超限内容不保存。'
                  : '已关闭：仅记录请求时间、模型、状态码及费用等元数据，不保存提示词与请求体（推荐）。'}
              </span>
            </div>
            <Switch
              id="settings-capture-enabled"
              aria-label="保存请求内容"
              checked={generalDraft.capture_enabled}
              disabled={isMutationBusy}
              onChange={(checked) => setGeneralDraft((prev) => ({ ...prev, capture_enabled: checked }))}
            />
          </div>

          <div className="settings-grid" style={{ marginTop: 12 }}>
            <Field label="采集内容" htmlFor="settings-capture-mode" hint="仅安全白名单请求头及 JSON；不保存响应或上游密钥。关闭不是删除。">
              <Select id="settings-capture-mode" aria-label="采集内容"
                value={generalDraft.capture_mode ?? 'headers_and_body'}
                disabled={isMutationBusy || !generalDraft.capture_enabled}
                options={[{ value: 'headers', label: '仅安全请求头' }, { value: 'headers_and_body', label: '安全请求头与 JSON 正文' }]}
                onChange={(value) => setGeneralDraft(prev => ({ ...prev, capture_mode: value as 'headers' | 'headers_and_body' }))} />
            </Field>
            <Field label="内容保留时间（秒）" htmlFor="settings-capture-retention" hint="至少 60 秒，且不能超过原始日志保留。过期内容不会因延长策略恢复。">
              <Input id="settings-capture-retention" aria-label="内容保留时间（秒）" type="number" min="60"
                max={generalDraft.raw_retention_hours * 3600} step="1" disabled={isMutationBusy}
                value={generalDraft.capture_retention_seconds ?? generalDraft.raw_retention_hours * 3600}
                onChange={e => setGeneralDraft(prev => ({ ...prev, capture_retention_seconds: Number(e.target.value) }))} />
            </Field>
            <Field label="原始日志保留" hint="超过保留期的明细记录将被清理" htmlFor="settings-raw-retention-select">
              <Select
                id="settings-raw-retention-select"
                aria-label="原始日志保留时长"
                value={rawPreset}
                disabled={isMutationBusy}
                onChange={handleRawPresetChange}
                options={[
                  { value: '24', label: '1 天 (24 小时)' },
                  { value: '72', label: '3 天 (72 小时)' },
                  { value: '168', label: '7 天 (168 小时)' },
                  { value: 'custom', label: '自定义小时数' },
                ]}
              />
              {rawPreset === 'custom' && (
                <div style={{ marginTop: 8 }}>
                  <Input
                    type="number"
                    min="1"
                    max={String(MAX_RAW_RETENTION_HOURS)}
                    id="settings-raw-retention-input"
                    aria-label="自定义原始日志保留小时数"
                    value={String(generalDraft.raw_retention_hours)}
                    disabled={isMutationBusy}
                    onChange={(e) => {
                      const val = Number(e.target.value);
                      setGeneralDraft((prev) => ({ ...prev, raw_retention_hours: val }));
                    }}
                    placeholder="1..168 小时"
                  />
                  <p className="settings-note">范围 1 至 168 小时（最长 7 天）</p>
                </div>
              )}
            </Field>

            <Field label="统计汇总保留" hint="按日聚合的计费与用量汇总数据" htmlFor="settings-daily-retention-select">
              <Select
                id="settings-daily-retention-select"
                aria-label="统计汇总保留天数"
                value={dailyPreset}
                disabled={isMutationBusy}
                onChange={handleDailyPresetChange}
                options={[
                  { value: '90', label: '90 天' },
                  { value: '180', label: '180 天' },
                  { value: '365', label: '365 天 (1 年)' },
                  { value: 'custom', label: '自定义天数' },
                ]}
              />
              {dailyPreset === 'custom' && (
                <div style={{ marginTop: 8 }}>
                  <Input
                    type="number"
                    min="1"
                    max={String(MAX_DAILY_RETENTION_DAYS)}
                    id="settings-daily-retention-input"
                    aria-label="自定义统计汇总保留天数"
                    value={String(generalDraft.daily_retention_days)}
                    disabled={isMutationBusy}
                    onChange={(e) => {
                      const val = Number(e.target.value);
                      setGeneralDraft((prev) => ({ ...prev, daily_retention_days: val }));
                    }}
                    placeholder="1..3650 天"
                  />
                  <p className="settings-note">范围 1 至 3650 天（最长 10 年），不影响 Key 累计额度</p>
                </div>
              )}
            </Field>
          </div>

          {/* 更多存储限制 (Collapsed Details) */}
          <div className="settings-details">
            <h3 style={{ fontSize: 14 }}>存储限制</h3>
            <p className="settings-note" style={{ marginBottom: 12 }}>
              超出限制时系统将丢弃过大的请求体，但不阻断推理。
            </p>
            <div className="settings-grid">
              <Field
                label="单条请求体上限"
                hint="范围 1 字节至最高 8 MiB"
                htmlFor="settings-max-body-amount"
              >
                <div className="settings-capacity-control">
                  <Input
                    id="settings-max-body-amount"
                    aria-label="单条请求体上限数值"
                    value={maxBodyAmount}
                    disabled={isMutationBusy}
                    onChange={(e) => handleMaxBodyAmountChange(e.target.value)}
                    placeholder="例如 1 或 1048576"
                  />
                  <Select
                    id="settings-max-body-unit"
                    aria-label="单条请求体上限单位"
                    value={maxBodyUnit}
                    disabled={isMutationBusy}
                    onChange={handleMaxBodyUnitChange}
                    options={CAPACITY_OPTIONS}
                  />
                </div>
              </Field>

              <Field
                label="请求体总存储上限"
                hint="范围 1 字节至最高 1 GiB"
                htmlFor="settings-max-storage-amount"
              >
                <div className="settings-capacity-control">
                  <Input
                    id="settings-max-storage-amount"
                    aria-label="请求体总存储上限数值"
                    value={maxStorageAmount}
                    disabled={isMutationBusy}
                    onChange={(e) => handleMaxStorageAmountChange(e.target.value)}
                    placeholder="例如 256 或 1"
                  />
                  <Select
                    id="settings-max-storage-unit"
                    aria-label="请求体总存储上限单位"
                    value={maxStorageUnit}
                    disabled={isMutationBusy}
                    onChange={handleMaxStorageUnitChange}
                    options={CAPACITY_OPTIONS}
                  />
                </div>
              </Field>
            </div>
          </div>

          <div className="settings-actions">
            <Button
              type="submit"
              variant="primary"
              loading={generalSaving}
              disabled={!hasLoadedSuccessfullyRef.current || isMutationBusy}
              data-testid="settings-general-save"
            >
              保存记录设置
            </Button>
            {isGeneralDirty && (
              <Button
                type="button"
                variant="secondary"
                disabled={isMutationBusy}
                onClick={handleCancelGeneral}
                data-testid="settings-general-reload"
              >
                放弃更改
              </Button>
            )}
          </div>
        </form>
      </section>

      {/* =================================================================== */}
      {/* Section 2: 模型价格 */}
      {/* =================================================================== */}
      <section className="settings-section" aria-labelledby="settings-pricing-heading">
        <div className="settings-section-header">
          <h2 className="settings-section-title" id="settings-pricing-heading">
            模型价格
          </h2>
          {isPricingDirty && <Badge tone="warning">价格有未保存更改</Badge>}
        </div>
        <p className="settings-section-desc">
          管理模型价格定义与条件规则。支持 models.dev v1/v2 固定目录同步，并按完整
          (model, provider) 身份精确手工覆盖；缺价按 $0 计费，新价格仅对后续请求生效。
        </p>

        {pricingStatus && <Notice tone={pricingStatus.tone}>{pricingStatus.message}</Notice>}

        <div className="settings-toggle-row">
          <div className="settings-toggle-info">
            <span className="settings-toggle-title">自动使用 models.dev 公共价格目录</span>
            <span className="settings-toggle-hint">
              开启后按所选目录自动同步参考价格，可能与实际账单不同。手工价格以完整定义替换目录价格。
            </span>
          </div>
          <Switch
            id="settings-pricing-auto"
            aria-label="自动使用 models.dev 公共价格目录"
            checked={pricingAuto}
            disabled={isMutationBusy}
            onChange={(checked) => setPricingAuto(checked)}
          />
        </div>

        <div className="settings-grid">
          <Field
            label="价格目录来源"
            hint="固定 models.dev 端点；保存后刷新目录并使用所选版本，缺失价格按 $0 计费。"
          >
            <Select
              aria-label="价格目录来源"
              value={pricingSource}
              disabled={isMutationBusy}
              options={[
                { value: 'models_dev_v2', label: 'models.dev v2（默认）' },
                { value: 'models_dev_v1', label: 'models.dev v1' },
              ]}
              onChange={(value) => setPricingSource(value as PricingSource)}
            />
          </Field>
        </div>

        {/* Public Catalog Sync Status */}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 12,
            padding: '10px 0',
            borderBottom: '1px solid var(--border-color)',
            flexWrap: 'wrap',
          }}
        >
          <div style={{ fontSize: 13, color: 'var(--text-secondary)' }}>
            <span>目录来源：{pricingSourceLabel(snapshot.source)}</span>
            <span style={{ margin: '0 8px' }}>·</span>
            <span>公共目录总收录：{formatInt(snapshot.catalog_entries ?? 0)} 条</span>
            <span style={{ margin: '0 8px' }}>·</span>
            <span>已记录模型与手工价格：{formatInt(snapshot.prices?.length ?? 0)} 条</span>
            <span style={{ margin: '0 8px' }}>·</span>
            <span>上次同步：{isLastSyncValid ? formatDateTime(snapshot.last_sync) : '尚未同步'}</span>
            {pricingSource !== snapshot.source && (
              <span style={{ marginLeft: 8 }}>（保存并刷新后切换目录）</span>
            )}
            {snapshot.error && (
              <span style={{ color: 'var(--error-color, #c65746)', marginLeft: 8 }}>
                （目录同步警告：{snapshot.error}）
              </span>
            )}
          </div>
          <Button
            type="button"
            variant="secondary"
            loading={pricingRefreshing}
            disabled={isMutationBusy}
            onClick={handleRefreshCatalog}
            data-testid="prices-refresh"
          >
            立即刷新价格目录
          </Button>
        </div>

        {/* Available Models Pricing Preview */}
        <div style={{ marginTop: 16 }}>
          <div className="settings-section-header" style={{ borderBottom: 'none', marginBottom: 6 }}>
            <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>可用模型价格预检</h3>
            <span style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
              基于 CPA 当前可用模型 ID 与计费目录预估首请求定价
            </span>
          </div>

          <div className="settings-table-filter-bar">
            <Select aria-label="价格匹配状态" value={previewStatus}
              options={[{ value: 'all', label: '全部状态' }, { value: 'matched', label: '已匹配' }, { value: 'ambiguous', label: '存在候选 / 歧义' }, { value: 'zero', label: '零价格' }]}
              onChange={value => { setPreviewStatus(value); setPreviewPage(1); }} />
            <div className="settings-search-input">
              <Input
                placeholder="搜索模型名称…"
                value={previewFilter}
                disabled={isMutationBusy}
                onChange={(e) => {
                  setPreviewFilter(e.target.value);
                  setPreviewPage(1);
                }}
                aria-label="搜索可用模型"
              />
            </div>
            <div style={{ fontSize: 12, color: 'var(--text-secondary)' }}>
              共 {filteredPreview.length} 个模型
            </div>
          </div>

          {previewError && <Notice tone="danger">{previewError}（保留上次成功数据）</Notice>}
          {previewLoading && <Notice tone="info">正在获取价格预检…</Notice>}
          {currentPreviewPageItems.length === 0 ? (
            previewError || previewLoading ? null :
            <EmptyState
              title={previewFilter ? '未找到匹配的模型' : '暂无可用模型'}
              description={previewFilter ? '请尝试不同的搜索关键词' : 'CPA 尚未发现可用模型或未登录'}
            />
          ) : (
            <div className="settings-table-container">
              <table className="settings-table">
                <thead>
                  <tr>
                    <th>模型</th>
                    <th>输入费率 ($/1M)</th>
                    <th>输出费率 ($/1M)</th>
                    <th>缓存读 / 写 ($/1M)</th>
                    <th>推理费率 ($/1M)</th>
                    <th>来源与状态</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {currentPreviewPageItems.map((item) => {
                    const ruleCount =
                      (item.definition?.overrides?.length ?? 0) +
                      (item.definition?.multipliers?.length ?? 0);
                    return (
                      <tr key={`${item.model}\u0000${item.provider ?? ''}`}>
                        <td>
                          <strong>{item.model}</strong>
                          {item.provider && (
                            <span style={{ fontSize: 11, color: 'var(--text-secondary)', display: 'block' }}>
                              {item.provider}
                            </span>
                          )}
                        </td>
                        <td>{formatRateDisplay(item.rates.input)}</td>
                        <td>{formatRateDisplay(item.rates.output)}</td>
                        <td>
                          {formatRateDisplay(item.rates.cache_read)} / {formatRateDisplay(item.rates.cache_write)}
                        </td>
                        <td>{formatReasoningDisplay(item.rates.reasoning)}</td>
                        <td>
                          <div style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                            <Badge tone="neutral">{sourceLabel(item.source)}</Badge>
                            {ruleCount > 0 && <Badge tone="neutral">{ruleCount} 条条件规则</Badge>}
                            {item.pricing_supported === false && (
                              <Badge tone="warning">价格规则不受支持</Badge>
                            )}
                            {(item.candidates?.length ?? 0) > 1 && (
                              <Badge tone="warning">候选 {item.candidates?.length}</Badge>
                            )}
                          </div>
                          <div className="settings-note">{item.reason || item.match || '—'}</div>
                          {item.support_issues?.length ? (
                            <div className="settings-note">适配问题：{item.support_issues.join('；')}</div>
                          ) : null}
                          {item.catalog_provider && <div className="settings-note">{item.catalog_provider} / {item.catalog_model}</div>}
                        </td>
                        <td>
                          {Boolean(item.candidates?.length) && <Button variant="secondary" disabled={isMutationBusy} onClick={() => setMappingItem(item)} aria-label={`为 ${item.model} 选择价格来源`}>选择来源</Button>}
                          <Button
                            variant="ghost"
                            disabled={isMutationBusy}
                            onClick={() => handleAddManualPrice(item.model, item.provider ?? '')}
                            aria-label={`为 ${item.model} 添加手工价格覆盖`}
                          >
                            手工覆盖
                          </Button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          {totalPreviewPages > 1 && (
            <div className="settings-pagination">
              <span>
                第 {previewPage} / {totalPreviewPages} 页
              </span>
              <div className="settings-pagination-buttons">
                <Button
                  variant="secondary"
                  disabled={previewPage <= 1 || isMutationBusy}
                  onClick={() => setPreviewPage((p) => Math.max(1, p - 1))}
                  aria-label="上一页"
                >
                  上一页
                </Button>
                <Button
                  variant="secondary"
                  disabled={previewPage >= totalPreviewPages || isMutationBusy}
                  onClick={() => setPreviewPage((p) => Math.min(totalPreviewPages, p + 1))}
                  aria-label="下一页"
                >
                  下一页
                </Button>
              </div>
            </div>
          )}
        </div>

        {mappingDrafts.length > 0 && <div className="settings-details">
          <h3 style={{ fontSize: 14 }}>显式价格来源（仅计费，不改变模型路由）</h3>
          {mappingDrafts.map(mapping => <div key={mapping.model} className="settings-row">
            <span>{mapping.model} → {mapping.provider} / {mapping.catalog_model}</span>
            <Button variant="ghost" disabled={isMutationBusy} onClick={() => setMappingDrafts(prev => prev.filter(v => v.model !== mapping.model))}>移除映射</Button>
          </div>)}
        </div>}
        {mappingItem && <Modal title={`选择价格来源：${mappingItem.model}`} size="lg" onClose={() => setMappingItem(null)}>
          <p className="settings-note">这是显式计费映射，不修改 CPA 路由。加入草稿后需点击“保存价格设置”，只影响后续请求。</p>
          {(mappingItem.candidates ?? []).map(candidate => <div className="settings-row" key={`${candidate.provider}/${candidate.model}`}>
            <div>
              <strong>{candidate.provider} / {candidate.model}</strong>
              <div className="settings-note">
                输入 {formatRateDisplay(candidate.definition.base.input)} · 输出 {formatRateDisplay(candidate.definition.base.output)} · 思考 {formatReasoningDisplay(candidate.definition.base.reasoning)}
              </div>
              {candidate.pricing_supported === false && (
                <div className="settings-note">不可导入：{candidate.support_issues?.join('；') || '价格规则不受支持'}</div>
              )}
            </div>
            <Button disabled={isMutationBusy || candidate.pricing_supported === false} onClick={() => {
              setMappingDrafts(prev => [...prev.filter(v => v.model !== mappingItem.model), { model: mappingItem.model, provider: candidate.provider, catalog_model: candidate.model }]);
              setMappingItem(null);
            }}>加入草稿</Button>
          </div>)}
        </Modal>}

        {/* Manual Overrides */}
        <div style={{ marginTop: 20 }}>
          <div className="settings-section-header" style={{ borderBottom: 'none' }}>
            <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>手工价格覆盖</h3>
            <Button
              type="button"
              variant="secondary"
              disabled={isMutationBusy}
              onClick={() => handleAddManualPrice('')}
              data-testid="manual-price-add"
            >
              + 添加手工价格
            </Button>
          </div>
          <p className="settings-note" style={{ marginBottom: 12 }}>
            使用完整 (model, provider) 身份精确覆盖公共目录。显式输入的 $0 是有效费率；目录缺价同样按 $0 计费。
          </p>

          {manualDrafts.length === 0 ? (
            <EmptyState title="暂无手工价格覆盖" description="未配置手工价格时使用所选公共目录；目录缺价按 $0 计费。" />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              {manualDrafts.map((draft, index) => (
                <div key={draft.id} className="settings-rule-card">
                  <div className="settings-rule-header">
                    <strong>第 {index + 1} 条手工价格</strong>
                    <div className="settings-rule-actions">
                      <Button
                        type="button"
                        variant="danger"
                        disabled={isMutationBusy}
                        onClick={() => handleRemoveManualPrice(index)}
                        aria-label={`删除第 ${index + 1} 条手工价格（${draft.model || '未命名'}）`}
                        data-testid="manual-price-remove"
                      >
                        删除
                      </Button>
                    </div>
                  </div>

                  <div className="settings-rule-conditions-grid">
                    <Field label="完整模型 ID">
                      <Input
                        placeholder="例如 openai/gpt-4o 或 custom-model"
                        value={draft.model}
                        disabled={isMutationBusy}
                        onChange={(e) => handleUpdateManualDraft(index, { model: e.target.value })}
                        aria-label={`第 ${index + 1} 条手工价格模型 ID`}
                      />
                    </Field>
                    <Field label="Provider（可空）" hint="空 Provider 只匹配真实的空 Provider，不会通配其他渠道。">
                      <Input
                        placeholder="例如 openai"
                        value={draft.provider}
                        disabled={isMutationBusy}
                        onChange={(e) => handleUpdateManualDraft(index, { provider: e.target.value })}
                        aria-label={`第 ${index + 1} 条手工价格 Provider`}
                      />
                    </Field>
                  </div>

                  <PriceDefinitionEditor
                    label={`第 ${index + 1} 条手工价格`}
                    draft={draft.definition}
                    disabled={isMutationBusy}
                    onChange={(definition) => handleUpdateManualDraft(index, { definition })}
                  />
                </div>
              ))}
            </div>
          )}
        </div>

        <div className="settings-actions">
          <Button
            type="button"
            variant="primary"
            loading={pricingSaving}
            disabled={!hasLoadedSuccessfullyRef.current || isMutationBusy}
            onClick={handleSavePricing}
            data-testid="settings-pricing-save"
          >
            保存价格设置
          </Button>
          {isPricingDirty && (
            <Button
              type="button"
              variant="secondary"
              disabled={isMutationBusy}
              onClick={handleCancelPricing}
              data-testid="settings-pricing-reload"
            >
              放弃更改
            </Button>
          )}
        </div>
      </section>

      {/* =================================================================== */}
      {/* Section 3: 存储维护 */}
      {/* =================================================================== */}
      <section className="settings-section" aria-labelledby="settings-health-heading">
        <div className="settings-section-header">
          <h2 className="settings-section-title" id="settings-health-heading">
            存储维护
          </h2>
          {hasFault ? (
            <Badge tone="warning">存储或计费状态有异常</Badge>
          ) : (
            <Badge tone="success">计费与存储状态正常</Badge>
          )}
        </div>
        <p className="settings-section-desc">
          查看数据库占用与计费队列健康状态；通过空间整理回收磁盘空闲碎片。
        </p>

        {compactStatus && <Notice tone={compactStatus.tone}>{compactStatus.message}</Notice>}
        {health.maintenance_error && <Notice tone="danger">最近维护错误：{health.maintenance_error}</Notice>}
        <div className="settings-diagnostic-grid">
          <div>采集压缩数据 / 预算：{formatBytes(health.capture_bytes ?? 0)} / {formatBytes(health.capture_limit_bytes ?? confirmedSettings.max_body_storage_bytes)}</div>
          <div>可回收物理空闲空间：{formatBytes(health.reclaimable_bytes ?? 0)}</div>
          <div>上次内容清理：{health.last_cleanup_at ? formatDateTime(health.last_cleanup_at) : '暂无'} · {health.last_cleanup_records ?? 0} 条</div>
          <div>上次整理前 / 后：{formatBytes(health.last_compact_before_bytes ?? 0)} / {formatBytes(health.last_compact_after_bytes ?? 0)}</div>
        </div>
        <div className="settings-actions">
          <Button variant="secondary" disabled={isMutationBusy} onClick={() => setContentOperation('cleanup')}>清理过期内容</Button>
          <Button variant="danger" disabled={isMutationBusy} onClick={() => setContentOperation('purge')}>立即清空全部内容</Button>
        </div>
        {health.dropped_captures > 0 && <Notice tone="warning">
          本次运行自 {health.counters_since ? formatDateTime(health.counters_since) : '启动'} 累计丢弃 {health.dropped_captures} 条诊断采集，不代表计费丢失或仍有故障。
          {Object.entries(health.capture_drops ?? {}).map(([reason, count]) => <span key={reason}> {reason}: {count}；</span>)}
        </Notice>}
        {contentOperation && <Modal title={contentOperation === 'purge' ? '清空全部内容' : '清理过期内容'} onClose={() => { if (!compacting) setContentOperation(null); }}
          footer={<><Button variant="secondary" disabled={compacting} onClick={() => setContentOperation(null)}>取消</Button><Button variant={contentOperation === 'purge' ? 'danger' : 'primary'} loading={compacting} onClick={handleContentConfirm}>确认执行</Button></>}>
          <p>仅删除{contentOperation === 'purge' ? '全部已保存的请求头和正文，且不可恢复' : '已过期请求头和正文'}。不删除请求元数据、汇总，不重置额度或重算费用。物理文件空间需另外整理。</p>
        </Modal>}

        {/* Fault warning always visible outside */}
        {hasFault && (
          <div style={{ marginBottom: 14 }}>
            {health.last_error && <Notice tone="danger">最近系统错误：{health.last_error}</Notice>}
            {health.pending_usage > 0 && (
              <Notice tone="warning">待重试计费队列存在 {health.pending_usage} 条事件正在排队写入。</Notice>
            )}
          </div>
        )}

        <div className="settings-row" style={{ padding: '8px 0', borderBottom: '1px solid var(--border-color)' }}>
          <div>
            <span style={{ fontSize: 14, fontWeight: 500 }}>当前数据库占用空间</span>
            <span style={{ fontSize: 18, fontWeight: 700, marginLeft: 12, color: 'var(--text-primary)' }}>
              {formatBytes(health.database_bytes)}
            </span>
          </div>
          <Button
            type="button"
            variant="secondary"
            disabled={isMutationBusy}
            onClick={() => setCompactModalOpen(true)}
            data-testid="health-compact"
          >
            压缩数据库 / 释放空间
          </Button>
        </div>

        {/* Diagnostic counters folded */}
        <details className="settings-details" style={{ marginTop: 14 }}>
          <summary>详细诊断指标</summary>
          <div className="settings-diagnostic-grid">
            <div className="settings-diagnostic-item">
              <span className="settings-diagnostic-label">数据库文件大小</span>
              <span className="settings-diagnostic-value">{formatBytes(health.database_bytes)}</span>
            </div>
            <div className="settings-diagnostic-item">
              <span className="settings-diagnostic-label">当前活跃请求</span>
              <span className="settings-diagnostic-value">{health.active_requests}</span>
            </div>
            <div className="settings-diagnostic-item">
              <span className="settings-diagnostic-label">待重试计费事件</span>
              <span className="settings-diagnostic-value">{health.pending_usage}</span>
            </div>
            <div className="settings-diagnostic-item">
              <span className="settings-diagnostic-label">超限丢弃捕获数</span>
              <span className="settings-diagnostic-value">{health.dropped_captures}</span>
            </div>
            <div className="settings-diagnostic-item" style={{ gridColumn: '1 / -1' }}>
              <span className="settings-diagnostic-label">最近错误</span>
              <span className="settings-diagnostic-value" style={{ fontWeight: 400 }}>
                {health.last_error || '无'}
              </span>
            </div>
          </div>
        </details>

        {/* Compaction confirmation modal */}
        {compactModalOpen && (
          <Modal
            title="压缩数据库"
            onClose={() => {
              if (!compacting) setCompactModalOpen(false);
            }}
            footer={
              <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
                <Button
                  variant="secondary"
                  disabled={compacting}
                  onClick={() => setCompactModalOpen(false)}
                  data-testid="compact-cancel"
                >
                  取消
                </Button>
                <Button
                  variant="primary"
                  loading={compacting}
                  onClick={handleCompactConfirm}
                  data-testid="compact-confirm"
                >
                  确认压缩
                </Button>
              </div>
            }
          >
            <p style={{ margin: 0, fontSize: 14, lineHeight: 1.5 }}>
              确定要执行数据库空间整理吗？当前数据库占用 <strong>{formatBytes(health.database_bytes)}</strong>。
            </p>
            <p className="settings-note" style={{ marginTop: 8 }}>
              仅在无活跃请求、无待写计费时整理，忙时请稍后重试。整理期间新受管请求可能收到可重试的 503，不影响已接受请求或 CPA 原生 Key。需要临时磁盘空间；复制或替换前失败保留原文件，替换后同步错误会如实报告。
            </p>
          </Modal>
        )}
      </section>

      {onLogout && (
        <section className="settings-section" aria-labelledby="settings-session-heading">
          <div className="settings-section-header">
            <h2 className="settings-section-title" id="settings-session-heading">
              会话
            </h2>
          </div>
          <p className="settings-section-desc">
            退出会清除本机保存的管理凭据。若有未保存的设置草稿，会先要求确认。
          </p>
          <div className="settings-actions">
            <Button variant="danger" onClick={onLogout} data-testid="settings-logout">
              退出
            </Button>
          </div>
        </section>
      )}
    </div>
  );
};

export default SettingsPage;
