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
  Price,
  Rule,
  PriceSnapshot,
  PricePreview,
  PricePreviewItem,
  Health,
  ModelInfo,
} from '../lib/types';
import { errorMessage } from '../lib/format';
import {
  formatBytes,
  formatInt,
  formatRateDisplay,
  validateGeneralSettings,
  ratesToForm,
  parseManualPrices,
  parseRules,
  generateUniqueRuleID,
  moveItem,
  areGeneralSettingsEqual,
  isPricingDraftEqual,
  GeneralSettingsDraft,
  ManualPriceDraft,
  RuleDraft,
  DEFAULT_MAX_BODY_BYTES,
  DEFAULT_MAX_BODY_STORAGE_BYTES,
  MAX_CONFIG_ENTRIES,
  MAX_RAW_RETENTION_HOURS,
  MAX_DAILY_RETENTION_DAYS,
} from './Settings.helpers';
import './Settings.css';

const DEFAULT_SETTINGS: Settings = {
  capture_enabled: false,
  max_body_bytes: DEFAULT_MAX_BODY_BYTES,
  max_body_storage_bytes: DEFAULT_MAX_BODY_STORAGE_BYTES,
  raw_retention_hours: 24,
  daily_retention_days: 365,
  pricing: {
    auto: true,
    prices: [],
    rules: [],
  },
};

const DEFAULT_SNAPSHOT: PriceSnapshot = {
  prices: [],
  last_sync: '',
  refreshing: false,
  catalog_entries: 0,
};

const DEFAULT_HEALTH: Health = {
  database_bytes: 0,
  dropped_captures: 0,
  pending_usage: 0,
  active_requests: 0,
};

export const SettingsPage: React.FC<PageProps> = ({ api, refreshInterval, onDirtyChange }) => {
  // ---------------------------------------------------------------------------
  // Server-confirmed state
  // ---------------------------------------------------------------------------
  const [confirmedSettings, setConfirmedSettings] = useState<Settings>(DEFAULT_SETTINGS);
  const [snapshot, setSnapshot] = useState<PriceSnapshot>(DEFAULT_SNAPSHOT);
  const [health, setHealth] = useState<Health>(DEFAULT_HEALTH);
  const [availableModels, setAvailableModels] = useState<ModelInfo[]>([]);
  const [previewItems, setPreviewItems] = useState<PricePreviewItem[]>([]);
  const [previewLoading, setPreviewLoading] = useState(false);

  // Initial load status
  const [initialLoading, setInitialLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);

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
  const [maxBodyMB, setMaxBodyMB] = useState<string>('1');
  const [maxStorageMB, setMaxStorageMB] = useState<string>('256');
  const [generalSaving, setGeneralSaving] = useState(false);
  const [generalStatus, setGeneralStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // ---------------------------------------------------------------------------
  // Section 2: 模型价格 Draft
  // ---------------------------------------------------------------------------
  const [pricingAuto, setPricingAuto] = useState<boolean>(true);
  const [manualDrafts, setManualDrafts] = useState<ManualPriceDraft[]>([]);
  const [ruleDrafts, setRuleDrafts] = useState<RuleDraft[]>([]);
  const [pricingSaving, setPricingSaving] = useState(false);
  const [pricingRefreshing, setPricingRefreshing] = useState(false);
  const [pricingStatus, setPricingStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // Preview filtering & pagination
  const [previewFilter, setPreviewFilter] = useState('');
  const [previewPage, setPreviewPage] = useState(1);
  const PREVIEW_PAGE_SIZE = 10;

  // ---------------------------------------------------------------------------
  // Section 3: 存储维护
  // ---------------------------------------------------------------------------
  const [compactModalOpen, setCompactModalOpen] = useState(false);
  const [compacting, setCompacting] = useState(false);
  const [compactStatus, setCompactStatus] = useState<{ tone: 'success' | 'danger'; message: string } | null>(null);

  // Ref tracking dirty state to protect drafts from poll overwrite
  const isGeneralDirtyRef = useRef(false);
  const isPricingDirtyRef = useRef(false);

  // ---------------------------------------------------------------------------
  // Helper: sync drafts from confirmed Settings
  // ---------------------------------------------------------------------------
  const syncGeneralFromConfirmed = useCallback((settings: Settings) => {
    setGeneralDraft({
      capture_enabled: settings.capture_enabled === true,
      max_body_bytes: settings.max_body_bytes ?? DEFAULT_MAX_BODY_BYTES,
      max_body_storage_bytes: settings.max_body_storage_bytes ?? DEFAULT_MAX_BODY_STORAGE_BYTES,
      raw_retention_hours: settings.raw_retention_hours ?? 24,
      daily_retention_days: settings.daily_retention_days ?? 365,
    });
    const rh = settings.raw_retention_hours ?? 24;
    setRawPreset(['24', '72', '168'].includes(String(rh)) ? String(rh) : 'custom');

    const dd = settings.daily_retention_days ?? 365;
    setDailyPreset(['90', '180', '365'].includes(String(dd)) ? String(dd) : 'custom');

    setMaxBodyMB(String(Math.round((settings.max_body_bytes ?? DEFAULT_MAX_BODY_BYTES) / (1024 * 1024))));
    setMaxStorageMB(String(Math.round((settings.max_body_storage_bytes ?? DEFAULT_MAX_BODY_STORAGE_BYTES) / (1024 * 1024))));
  }, []);

  const syncPricingFromConfirmed = useCallback((pricing: PricingConfig) => {
    setPricingAuto(pricing.auto !== false);

    const manuals: ManualPriceDraft[] = (pricing.prices || []).map((p, index) => {
      const f = ratesToForm(p.rates);
      return {
        id: `manual-${index}-${p.model}`,
        model: p.model,
        input: f.input,
        output: f.output,
        cache_read: f.cache_read,
        cache_write: f.cache_write,
        reasoning: f.reasoning,
        source: p.source,
      };
    });
    setManualDrafts(manuals);

    const rules: RuleDraft[] = (pricing.rules || []).map((r, index) => {
      const f = ratesToForm(r.rates);
      return {
        rowId: `rule-${index}-${r.id}`,
        id: r.id,
        model: r.model,
        input_tokens_gt: r.input_tokens_gt !== undefined && r.input_tokens_gt !== null ? String(r.input_tokens_gt) : '',
        time_range: r.time_range || '',
        input: f.input,
        output: f.output,
        cache_read: f.cache_read,
        cache_write: f.cache_write,
        reasoning: f.reasoning,
      };
    });
    setRuleDrafts(rules);
  }, []);

  // ---------------------------------------------------------------------------
  // Dirty state calculation
  // ---------------------------------------------------------------------------
  const isGeneralDirty = useMemo(() => {
    const confirmedGen: GeneralSettingsDraft = {
      capture_enabled: confirmedSettings.capture_enabled === true,
      max_body_bytes: confirmedSettings.max_body_bytes ?? DEFAULT_MAX_BODY_BYTES,
      max_body_storage_bytes: confirmedSettings.max_body_storage_bytes ?? DEFAULT_MAX_BODY_STORAGE_BYTES,
      raw_retention_hours: confirmedSettings.raw_retention_hours ?? 24,
      daily_retention_days: confirmedSettings.daily_retention_days ?? 365,
    };
    return !areGeneralSettingsEqual(generalDraft, confirmedGen);
  }, [generalDraft, confirmedSettings]);

  const isPricingDirty = useMemo(() => {
    return !isPricingDraftEqual(pricingAuto, manualDrafts, ruleDrafts, confirmedSettings.pricing);
  }, [pricingAuto, manualDrafts, ruleDrafts, confirmedSettings.pricing]);

  isGeneralDirtyRef.current = isGeneralDirty;
  isPricingDirtyRef.current = isPricingDirty;

  useEffect(() => {
    onDirtyChange(isGeneralDirty || isPricingDirty);
  }, [isGeneralDirty, isPricingDirty, onDirtyChange]);

  // ---------------------------------------------------------------------------
  // Load data & Preview fetching
  // ---------------------------------------------------------------------------
  const fetchPreview = useCallback(
    async (models: ModelInfo[], signal?: AbortSignal) => {
      if (!models.length) {
        setPreviewItems([]);
        return;
      }
      setPreviewLoading(true);
      try {
        const payload = {
          models: models.slice(0, 4096).map((m) => ({
            model: m.id,
            provider: m.owned_by || '',
          })),
        };
        const res = await api.request<PricePreview>('/prices/preview', {
          method: 'POST',
          body: payload,
          signal,
        });
        if (res && Array.isArray(res.items)) {
          setPreviewItems(res.items);
        }
      } catch (err: unknown) {
        if (signal?.aborted) return;
      } finally {
        if (!signal?.aborted) {
          setPreviewLoading(false);
        }
      }
    },
    [api]
  );

  const loadAll = useCallback(
    async (isPoll = false, signal?: AbortSignal) => {
      try {
        const [settingsRes, pricesRes, healthRes, modelsRes] = await Promise.all([
          api.request<Settings>('/settings', { signal }),
          api.request<PriceSnapshot>('/prices', { signal }),
          api.request<Health>('/health', { signal }),
          api.models({ signal }).catch(() => [] as ModelInfo[]),
        ]);

        if (signal?.aborted) return;

        setConfirmedSettings(settingsRes);
        setSnapshot(pricesRes);
        setHealth(healthRes);
        setAvailableModels(modelsRes);

        // Only overwrite drafts from server if not dirty
        if (!isPoll || !isGeneralDirtyRef.current) {
          syncGeneralFromConfirmed(settingsRes);
        }
        if (!isPoll || !isPricingDirtyRef.current) {
          syncPricingFromConfirmed(settingsRes.pricing);
        }

        // Fetch prices preview for available CPA models
        await fetchPreview(modelsRes, signal);

        setLoadError(null);
      } catch (err: unknown) {
        if (signal?.aborted) return;
        if (!isPoll) {
          setLoadError(errorMessage(err) || '加载设置失败');
        }
      } finally {
        if (!signal?.aborted && !isPoll) {
          setInitialLoading(false);
        }
      }
    },
    [api, syncGeneralFromConfirmed, syncPricingFromConfirmed, fetchPreview]
  );

  useEffect(() => {
    const controller = new AbortController();
    loadAll(false, controller.signal);
    return () => {
      controller.abort();
    };
  }, [loadAll]);

  // Polling effect
  useEffect(() => {
    if (!refreshInterval || refreshInterval <= 0) return;
    const intervalMs = refreshInterval * 1000;
    const timer = setInterval(() => {
      loadAll(true);
    }, intervalMs);
    return () => {
      clearInterval(timer);
    };
  }, [refreshInterval, loadAll]);

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

  const handleMaxBodyMBChange = (mbStr: string) => {
    setMaxBodyMB(mbStr);
    const mb = Number(mbStr);
    if (Number.isFinite(mb) && mb >= 1) {
      setGeneralDraft((prev) => ({ ...prev, max_body_bytes: Math.round(mb * 1024 * 1024) }));
    }
  };

  const handleMaxStorageMBChange = (mbStr: string) => {
    setMaxStorageMB(mbStr);
    const mb = Number(mbStr);
    if (Number.isFinite(mb) && mb >= 1) {
      setGeneralDraft((prev) => ({ ...prev, max_body_storage_bytes: Math.round(mb * 1024 * 1024) }));
    }
  };

  const handleSaveGeneral = async (e?: React.FormEvent) => {
    if (e) e.preventDefault();
    setGeneralStatus(null);

    const err = validateGeneralSettings(generalDraft);
    if (err) {
      setGeneralStatus({ tone: 'danger', message: err });
      return;
    }

    setGeneralSaving(true);
    try {
      const payload: Settings = {
        capture_enabled: generalDraft.capture_enabled,
        max_body_bytes: generalDraft.max_body_bytes,
        max_body_storage_bytes: generalDraft.max_body_storage_bytes,
        raw_retention_hours: generalDraft.raw_retention_hours,
        daily_retention_days: generalDraft.daily_retention_days,
        // CRITICAL: Preserve the confirmed server pricing snapshot, not unsaved pricing drafts!
        pricing: confirmedSettings.pricing,
      };

      const res = await api.request<Settings>('/settings', {
        method: 'PUT',
        body: payload,
      });

      const updated = res || payload;
      setConfirmedSettings(updated);
      syncGeneralFromConfirmed(updated);
      setGeneralStatus({ tone: 'success', message: '请求记录与存储限制设置已保存' });
    } catch (saveErr) {
      setGeneralStatus({ tone: 'danger', message: errorMessage(saveErr) || '保存失败' });
    } finally {
      setGeneralSaving(false);
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
    setPricingRefreshing(true);
    setPricingStatus(null);
    try {
      const res = await api.request<{ snapshot?: PriceSnapshot }>('/prices/refresh', {
        method: 'POST',
      });
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
          // Refresh preview after new catalog sync
          fetchPreview(availableModels);
        }
      }
    } catch (err) {
      setPricingStatus({ tone: 'danger', message: errorMessage(err) || '刷新价格目录失败' });
    } finally {
      setPricingRefreshing(false);
    }
  };

  const handleAddManualPrice = (initialModel = '') => {
    if (manualDrafts.length >= MAX_CONFIG_ENTRIES) {
      setPricingStatus({ tone: 'danger', message: `手工价格不能超过 ${MAX_CONFIG_ENTRIES} 条` });
      return;
    }
    const newDraft: ManualPriceDraft = {
      id: `manual-new-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      model: initialModel,
      input: '0',
      output: '0',
      cache_read: '0',
      cache_write: '0',
      reasoning: '',
      source: 'manual',
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

  const handleAddRule = () => {
    if (ruleDrafts.length >= MAX_CONFIG_ENTRIES) {
      setPricingStatus({ tone: 'danger', message: `规则不能超过 ${MAX_CONFIG_ENTRIES} 条` });
      return;
    }
    const existingIds = ruleDrafts.map((r) => r.id);
    const newRuleId = generateUniqueRuleID(existingIds);
    const newDraft: RuleDraft = {
      rowId: `rule-new-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      id: newRuleId,
      model: '',
      input_tokens_gt: '',
      time_range: '',
      input: '0',
      output: '0',
      cache_read: '0',
      cache_write: '0',
      reasoning: '',
    };
    setRuleDrafts((prev) => [...prev, newDraft]);
  };

  const handleUpdateRuleDraft = (index: number, patch: Partial<RuleDraft>) => {
    setRuleDrafts((prev) => {
      const next = [...prev];
      next[index] = { ...next[index], ...patch };
      return next;
    });
  };

  const handleMoveRule = (index: number, direction: 'up' | 'down') => {
    setRuleDrafts((prev) => moveItem(prev, index, direction));
  };

  const handleRemoveRule = (index: number) => {
    setRuleDrafts((prev) => prev.filter((_, i) => i !== index));
  };

  const handleSavePricing = async () => {
    setPricingStatus(null);

    let parsedPrices: Price[];
    let parsedRules: Rule[];
    try {
      parsedPrices = parseManualPrices(manualDrafts);
    } catch (err) {
      setPricingStatus({ tone: 'danger', message: errorMessage(err) });
      return;
    }
    try {
      parsedRules = parseRules(ruleDrafts);
    } catch (err) {
      setPricingStatus({ tone: 'danger', message: errorMessage(err) });
      return;
    }

    setPricingSaving(true);
    try {
      const newPricing: PricingConfig = {
        auto: pricingAuto,
        prices: parsedPrices,
        rules: parsedRules,
      };

      const payload: Settings = {
        // CRITICAL: Preserve the confirmed server general settings, not unsaved general drafts!
        capture_enabled: confirmedSettings.capture_enabled,
        max_body_bytes: confirmedSettings.max_body_bytes,
        max_body_storage_bytes: confirmedSettings.max_body_storage_bytes,
        raw_retention_hours: confirmedSettings.raw_retention_hours,
        daily_retention_days: confirmedSettings.daily_retention_days,
        pricing: newPricing,
      };

      const res = await api.request<Settings>('/settings', {
        method: 'PUT',
        body: payload,
      });

      const updated = res || payload;
      setConfirmedSettings(updated);
      syncPricingFromConfirmed(updated.pricing);
      setPricingStatus({ tone: 'success', message: '价格设置已保存' });

      // Update preview to reflect newly saved manual overrides and rules
      fetchPreview(availableModels);
    } catch (saveErr) {
      setPricingStatus({ tone: 'danger', message: errorMessage(saveErr) || '保存失败' });
    } finally {
      setPricingSaving(false);
    }
  };

  const handleCancelPricing = () => {
    syncPricingFromConfirmed(confirmedSettings.pricing);
    setPricingStatus(null);
  };

  // Filtered and paginated preview items
  const filteredPreview = useMemo(() => {
    if (!previewFilter.trim()) return previewItems;
    const lower = previewFilter.trim().toLowerCase();
    return previewItems.filter(
      (item) => item.model.toLowerCase().includes(lower) || (item.provider && item.provider.toLowerCase().includes(lower))
    );
  }, [previewItems, previewFilter]);

  const totalPreviewPages = Math.max(1, Math.ceil(filteredPreview.length / PREVIEW_PAGE_SIZE));
  const currentPreviewPageItems = useMemo(() => {
    const start = (previewPage - 1) * PREVIEW_PAGE_SIZE;
    return filteredPreview.slice(start, start + PREVIEW_PAGE_SIZE);
  }, [filteredPreview, previewPage]);

  // ---------------------------------------------------------------------------
  // Section 3: 存储维护 Handlers
  // ---------------------------------------------------------------------------
  const handleCompactConfirm = async () => {
    setCompacting(true);
    setCompactStatus(null);
    try {
      const res = await api.request<{ health?: Health }>('/compact', {
        method: 'POST',
      });
      if (res && res.health) {
        setHealth(res.health);
      }
      setCompactStatus({ tone: 'success', message: '数据库压缩整理完成' });
      setCompactModalOpen(false);
    } catch (err) {
      setCompactStatus({ tone: 'danger', message: errorMessage(err) || '压缩数据库失败' });
      setCompactModalOpen(false);
    } finally {
      setCompacting(false);
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

  if (loadError) {
    return (
      <div className="settings-page" data-testid="settings-error">
        <Notice tone="danger">{loadError}</Notice>
        <div style={{ marginTop: 12 }}>
          <Button variant="secondary" onClick={() => loadAll(false)}>
            重试
          </Button>
        </div>
      </div>
    );
  }

  const hasFault = Boolean(health.last_error || health.dropped_captures > 0 || health.pending_usage > 0);

  return (
    <div className="settings-page" data-testid="observer-settings">
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
                  ? '已开启：将完整记录请求提示词与模型响应，便于调试分析。请勿在请求中发送保密凭据。'
                  : '已关闭：仅记录请求时间、模型、状态码及费用等元数据，不保存提示词与响应体（推荐）。'}
              </span>
            </div>
            <Switch
              id="settings-capture-enabled"
              aria-label="保存请求内容"
              checked={generalDraft.capture_enabled}
              onChange={(checked) => setGeneralDraft((prev) => ({ ...prev, capture_enabled: checked }))}
            />
          </div>

          <div className="settings-grid" style={{ marginTop: 12 }}>
            <Field label="原始日志保留" hint="超过保留期的明细记录将被清理" htmlFor="settings-raw-retention-select">
              <Select
                id="settings-raw-retention-select"
                aria-label="原始日志保留时长"
                value={rawPreset}
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
          <details className="settings-details">
            <summary>更多存储限制（单条与总量上限）</summary>
            <p className="settings-note" style={{ marginBottom: 12 }}>
              存储采用精确 MiB 二进制换算（1 MB = 1,048,576 字节）。超出限制时系统将丢弃过大的请求体，但不阻断推理。
            </p>
            <div className="settings-grid">
              <Field
                label="单条请求体上限"
                hint={`当前 ${generalDraft.max_body_bytes.toLocaleString()} 字节，最大 8 MB`}
                htmlFor="settings-max-body-input"
              >
                <Input
                  type="number"
                  min="1"
                  max="8"
                  id="settings-max-body-input"
                  aria-label="单条请求体上限（MB）"
                  value={maxBodyMB}
                  onChange={(e) => handleMaxBodyMBChange(e.target.value)}
                  placeholder="1..8 MB"
                />
              </Field>

              <Field
                label="请求体总存储上限"
                hint={`当前 ${generalDraft.max_body_storage_bytes.toLocaleString()} 字节，最大 1 GB (1024 MB)`}
                htmlFor="settings-max-storage-input"
              >
                <Input
                  type="number"
                  min="1"
                  max="1024"
                  id="settings-max-storage-input"
                  aria-label="请求体总存储上限（MB）"
                  value={maxStorageMB}
                  onChange={(e) => handleMaxStorageMBChange(e.target.value)}
                  placeholder="1..1024 MB"
                />
              </Field>
            </div>
          </details>

          <div className="settings-actions">
            <Button
              type="submit"
              variant="primary"
              loading={generalSaving}
              data-testid="settings-general-save"
            >
              保存记录设置
            </Button>
            {isGeneralDirty && (
              <Button
                type="button"
                variant="secondary"
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
          管理模型费率与多级计费规则。支持 models.dev 公共目录自动同步与精确手工覆盖；新价格仅对后续请求生效。
        </p>

        {pricingStatus && <Notice tone={pricingStatus.tone}>{pricingStatus.message}</Notice>}

        <div className="settings-toggle-row">
          <div className="settings-toggle-info">
            <span className="settings-toggle-title">自动使用 models.dev 公共价格目录</span>
            <span className="settings-toggle-hint">
              开启后自动同步官方主流模型基准费率。可在下方通过手工价格或规则针对特定模型进行覆盖。
            </span>
          </div>
          <Switch
            id="settings-pricing-auto"
            aria-label="自动使用 models.dev 公共价格目录"
            checked={pricingAuto}
            onChange={(checked) => setPricingAuto(checked)}
          />
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
            <span>公共目录总收录：{formatInt(snapshot.catalog_entries ?? 0)} 条</span>
            <span style={{ margin: '0 8px' }}>·</span>
            <span>当前生效条目：{formatInt(snapshot.prices?.length ?? 0)} 条</span>
            <span style={{ margin: '0 8px' }}>·</span>
            <span>上次同步：{snapshot.last_sync ? new Date(snapshot.last_sync).toLocaleString('zh-CN') : '尚未同步'}</span>
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
            <div className="settings-search-input">
              <Input
                placeholder="搜索模型名称…"
                value={previewFilter}
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

          {previewLoading ? (
            <Notice tone="info">正在获取价格预检…</Notice>
          ) : currentPreviewPageItems.length === 0 ? (
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
                    const isUnpriced = item.source === 'unpriced';
                    return (
                      <tr key={item.model}>
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
                        <td>{item.rates.reasoning !== undefined && item.rates.reasoning !== null ? formatRateDisplay(item.rates.reasoning) : '—'}</td>
                        <td>
                          <div style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
                            {isUnpriced ? (
                              <Badge tone="warning">未定价（按 $0 计费）</Badge>
                            ) : item.source === 'manual' ? (
                              <Badge tone="neutral">手工</Badge>
                            ) : (
                              <Badge tone="neutral">自动目录</Badge>
                            )}
                            {item.has_rules && <Badge tone="neutral">含条件规则</Badge>}
                          </div>
                        </td>
                        <td>
                          <Button
                            variant="ghost"
                            onClick={() => handleAddManualPrice(item.model)}
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
                  disabled={previewPage <= 1}
                  onClick={() => setPreviewPage((p) => Math.max(1, p - 1))}
                  aria-label="上一页"
                >
                  上一页
                </Button>
                <Button
                  variant="secondary"
                  disabled={previewPage >= totalPreviewPages}
                  onClick={() => setPreviewPage((p) => Math.min(totalPreviewPages, p + 1))}
                  aria-label="下一页"
                >
                  下一页
                </Button>
              </div>
            </div>
          )}
        </div>

        {/* Manual Overrides */}
        <div style={{ marginTop: 20 }}>
          <div className="settings-section-header" style={{ borderBottom: 'none' }}>
            <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600 }}>手工价格覆盖</h3>
            <Button
              type="button"
              variant="secondary"
              onClick={() => handleAddManualPrice('')}
              data-testid="manual-price-add"
            >
              + 添加手工价格
            </Button>
          </div>
          <p className="settings-note" style={{ marginBottom: 12 }}>
            为特定完整模型 ID 指定费率，精确覆盖公共目录。显式输入的 $0 为有效费率，不同于未定价。
          </p>

          {manualDrafts.length === 0 ? (
            <EmptyState title="暂无手工价格覆盖" description="未配置手工价格时，将优先使用公共目录或按未定价计费。" />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
              {manualDrafts.map((draft, index) => (
                <div key={draft.id} className="settings-rule-card">
                  <div className="settings-rule-header">
                    <div style={{ flex: 1, minWidth: 200 }}>
                      <Field label={`第 ${index + 1} 条：完整模型 ID`}>
                        <Input
                          placeholder="例如 openai/gpt-4o 或 custom-model"
                          value={draft.model}
                          onChange={(e) => handleUpdateManualDraft(index, { model: e.target.value })}
                          aria-label={`第 ${index + 1} 条手工价格模型 ID`}
                        />
                      </Field>
                    </div>
                    <div className="settings-rule-actions">
                      <Button
                        type="button"
                        variant="danger"
                        onClick={() => handleRemoveManualPrice(index)}
                        aria-label={`删除第 ${index + 1} 条手工价格（${draft.model || '未命名'}）`}
                        data-testid="manual-price-remove"
                      >
                        删除
                      </Button>
                    </div>
                  </div>

                  <div className="settings-rates-grid">
                    <Field label="输入 ($/1M)">
                      <Input
                        type="number"
                        step="0.000001"
                        min="0"
                        value={draft.input}
                        onChange={(e) => handleUpdateManualDraft(index, { input: e.target.value })}
                        aria-label={`第 ${index + 1} 条输入价格`}
                      />
                    </Field>
                    <Field label="输出 ($/1M)">
                      <Input
                        type="number"
                        step="0.000001"
                        min="0"
                        value={draft.output}
                        onChange={(e) => handleUpdateManualDraft(index, { output: e.target.value })}
                        aria-label={`第 ${index + 1} 条输出价格`}
                      />
                    </Field>
                    <Field label="缓存读 ($/1M)">
                      <Input
                        type="number"
                        step="0.000001"
                        min="0"
                        value={draft.cache_read}
                        onChange={(e) => handleUpdateManualDraft(index, { cache_read: e.target.value })}
                        aria-label={`第 ${index + 1} 条缓存读价格`}
                      />
                    </Field>
                    <Field label="缓存写 ($/1M)">
                      <Input
                        type="number"
                        step="0.000001"
                        min="0"
                        value={draft.cache_write}
                        onChange={(e) => handleUpdateManualDraft(index, { cache_write: e.target.value })}
                        aria-label={`第 ${index + 1} 条缓存写价格`}
                      />
                    </Field>
                    <Field label="推理 ($/1M，可选)">
                      <Input
                        type="number"
                        step="0.000001"
                        min="0"
                        placeholder="留空 = 无"
                        value={draft.reasoning}
                        onChange={(e) => handleUpdateManualDraft(index, { reasoning: e.target.value })}
                        aria-label={`第 ${index + 1} 条推理价格`}
                      />
                    </Field>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Conditional Rules (Collapsed) */}
        <details className="settings-details" style={{ marginTop: 20 }}>
          <summary>条件匹配规则（高级，{ruleDrafts.length} 条已配置）</summary>
          <div style={{ marginTop: 10 }}>
            <div className="settings-section-header" style={{ borderBottom: 'none' }}>
              <span style={{ fontSize: 13, color: 'var(--text-secondary)' }}>
                规则自上而下匹配，首条条件全部满足（AND）的规则生效。完整提示词 Token 严格大于生效；留空项表示不设限。
              </span>
              <Button type="button" variant="secondary" onClick={handleAddRule} data-testid="rule-add">
                + 添加规则
              </Button>
            </div>

            {ruleDrafts.length === 0 ? (
              <EmptyState title="暂无条件规则" description="如需根据 Token 上下文大小或 UTC 时间段调整费率，可添加规则。" />
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                {ruleDrafts.map((rule, index) => (
                  <div key={rule.rowId} className="settings-rule-card">
                    <div className="settings-rule-header">
                      <div className="settings-rule-conditions-grid" style={{ flex: 1 }}>
                        <Field label="规则 ID（唯一）">
                          <Input
                            value={rule.id}
                            onChange={(e) => handleUpdateRuleDraft(index, { id: e.target.value })}
                            aria-label={`第 ${index + 1} 条规则 ID`}
                          />
                        </Field>
                        <Field label="模型 ID">
                          <Input
                            placeholder="例如 claude-3-5-sonnet"
                            value={rule.model}
                            onChange={(e) => handleUpdateRuleDraft(index, { model: e.target.value })}
                            aria-label={`第 ${index + 1} 条规则模型 ID`}
                          />
                        </Field>
                        <Field label="提示词 Token 严格大于" hint="含缓存读写，留空不限制">
                          <Input
                            type="number"
                            min="0"
                            placeholder="留空 = 不限制"
                            value={rule.input_tokens_gt}
                            onChange={(e) => handleUpdateRuleDraft(index, { input_tokens_gt: e.target.value })}
                            aria-label={`第 ${index + 1} 条规则 Token 阈值`}
                          />
                        </Field>
                        <Field label="UTC 时间区间" hint="格式 HH:mm-HH:mm，支持跨夜">
                          <Input
                            placeholder="例如 00:00-08:00"
                            value={rule.time_range}
                            onChange={(e) => handleUpdateRuleDraft(index, { time_range: e.target.value })}
                            aria-label={`第 ${index + 1} 条规则 UTC 时间区间`}
                          />
                        </Field>
                      </div>

                      <div className="settings-rule-actions">
                        <Button
                          type="button"
                          variant="secondary"
                          disabled={index === 0}
                          onClick={() => handleMoveRule(index, 'up')}
                          aria-label={`上移第 ${index + 1} 条规则`}
                          data-testid="rule-up"
                        >
                          上移
                        </Button>
                        <Button
                          type="button"
                          variant="secondary"
                          disabled={index === ruleDrafts.length - 1}
                          onClick={() => handleMoveRule(index, 'down')}
                          aria-label={`下移第 ${index + 1} 条规则`}
                          data-testid="rule-down"
                        >
                          下移
                        </Button>
                        <Button
                          type="button"
                          variant="danger"
                          onClick={() => handleRemoveRule(index)}
                          aria-label={`删除第 ${index + 1} 条规则`}
                          data-testid="rule-remove"
                        >
                          删除
                        </Button>
                      </div>
                    </div>

                    <div className="settings-rates-grid">
                      <Field label="输入 ($/1M)">
                        <Input
                          type="number"
                          step="0.000001"
                          min="0"
                          value={rule.input}
                          onChange={(e) => handleUpdateRuleDraft(index, { input: e.target.value })}
                          aria-label={`第 ${index + 1} 条规则输入价格`}
                        />
                      </Field>
                      <Field label="输出 ($/1M)">
                        <Input
                          type="number"
                          step="0.000001"
                          min="0"
                          value={rule.output}
                          onChange={(e) => handleUpdateRuleDraft(index, { output: e.target.value })}
                          aria-label={`第 ${index + 1} 条规则输出价格`}
                        />
                      </Field>
                      <Field label="缓存读 ($/1M)">
                        <Input
                          type="number"
                          step="0.000001"
                          min="0"
                          value={rule.cache_read}
                          onChange={(e) => handleUpdateRuleDraft(index, { cache_read: e.target.value })}
                          aria-label={`第 ${index + 1} 条规则缓存读价格`}
                        />
                      </Field>
                      <Field label="缓存写 ($/1M)">
                        <Input
                          type="number"
                          step="0.000001"
                          min="0"
                          value={rule.cache_write}
                          onChange={(e) => handleUpdateRuleDraft(index, { cache_write: e.target.value })}
                          aria-label={`第 ${index + 1} 条规则缓存写价格`}
                        />
                      </Field>
                      <Field label="推理 ($/1M，可选)">
                        <Input
                          type="number"
                          step="0.000001"
                          min="0"
                          placeholder="留空 = 无"
                          value={rule.reasoning}
                          onChange={(e) => handleUpdateRuleDraft(index, { reasoning: e.target.value })}
                          aria-label={`第 ${index + 1} 条规则推理价格`}
                        />
                      </Field>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </details>

        <div className="settings-actions">
          <Button
            type="button"
            variant="primary"
            loading={pricingSaving}
            onClick={handleSavePricing}
            data-testid="settings-pricing-save"
          >
            保存价格设置
          </Button>
          {isPricingDirty && (
            <Button
              type="button"
              variant="secondary"
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

        {/* Fault warning always visible outside */}
        {hasFault && (
          <div style={{ marginBottom: 14 }}>
            {health.last_error && <Notice tone="danger">最近系统错误：{health.last_error}</Notice>}
            {health.pending_usage > 0 && (
              <Notice tone="warning">待重试计费队列存在 {health.pending_usage} 条事件正在排队写入。</Notice>
            )}
            {health.dropped_captures > 0 && (
              <Notice tone="warning">
                因超出存储上限，已累计丢弃 {health.dropped_captures} 条请求体捕获。
              </Notice>
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
              整理期间将重建数据文件以回收已释放的磁盘空间，需要少量临时磁盘配额。整理过程不会丢失已用额度或计费日志；失败时保留原文件，不会自动重试。
            </p>
          </Modal>
        )}
      </section>
    </div>
  );
};

export default SettingsPage;
