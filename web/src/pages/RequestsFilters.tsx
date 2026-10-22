import React from 'react';
import type { FilterState, PresetRange } from './RequestsHelpers';
import { getBrowserTimeZone } from '../lib/format';
import type { SelectOption } from '../components/ui';
import { Button, Input, Select } from '../components/ui';

export interface RequestsFiltersProps {
  draft: FilterState;
  onDraftChange: (next: FilterState) => void;
  onApply: () => void;
  onReset: () => void;
  isDirty: boolean;
  loading?: boolean;
  keyOptions: SelectOption[];
  modelOptions: SelectOption[];
  providerOptions: SelectOption[];
  accountOptions: SelectOption[];
  preset: PresetRange;
  onPresetChange: (preset: PresetRange) => void;
  onFacetSearch?: (field: string, search: string) => void;
}

const statusOptions: SelectOption[] = [
  { value: '', label: '全部状态' },
  { value: 'success', label: '成功' },
  { value: 'failed', label: '失败' },
];

export function RequestsFilters({
  draft,
  onDraftChange,
  onApply,
  onReset,
  isDirty,
  loading = false,
  keyOptions,
  modelOptions,
  providerOptions,
  accountOptions,
  preset,
  onPresetChange,
  onFacetSearch,
}: RequestsFiltersProps) {
  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    onApply();
  };

  const updateField = (key: keyof FilterState, val: string) => {
    onDraftChange({ ...draft, [key]: val });
  };

  const handleDateChange = (field: 'from' | 'to', val: string) => {
    onPresetChange('custom');
    updateField(field, val);
  };

  return (
    <div className="requests-filters-card" data-testid="requests-filters">
      <form onSubmit={handleSubmit} className="requests-filters-form">
        {/* Preset & Timezone bar */}
        <div className="requests-presets-bar">
          <div className="requests-preset-buttons">
            <button
              type="button"
              className={`cpa-btn cpa-btn-ghost cpa-btn-sm ${preset === 'today' ? 'is-active' : ''}`}
              onClick={() => onPresetChange('today')}
            >
              今天
            </button>
            <button
              type="button"
              className={`cpa-btn cpa-btn-ghost cpa-btn-sm ${preset === 'last7days' ? 'is-active' : ''}`}
              onClick={() => onPresetChange('last7days')}
            >
              近 7 天
            </button>
            <button
              type="button"
              className={`cpa-btn cpa-btn-ghost cpa-btn-sm ${preset === 'last30days' ? 'is-active' : ''}`}
              onClick={() => onPresetChange('last30days')}
            >
              近 30 天
            </button>
            <button
              type="button"
              className={`cpa-btn cpa-btn-ghost cpa-btn-sm ${preset === 'custom' ? 'is-active' : ''}`}
              onClick={() => onPresetChange('custom')}
            >
              自定义
            </button>
          </div>
          <div
            className="requests-tz-indicator"
            title="日期筛选按浏览器本地日历解释，查询会携带该 IANA 时区。"
          >
            本地时区：{getBrowserTimeZone()}
          </div>
          {isDirty && (
            <div className="requests-dirty-badge" role="status">
              筛选未应用（点击查询或按 Enter 应用）
            </div>
          )}
        </div>

        {/* Flat inputs row */}
        <div className="requests-flat-grid">
          {/* Start Date */}
          <div className="requests-filter-field">
            <label htmlFor="req-from-date" className="cpa-label">
              开始日期
            </label>
            <Input
              id="req-from-date"
              type="date"
              value={draft.from}
              onChange={(e) => handleDateChange('from', e.target.value)}
              aria-label="开始日期"
            />
          </div>

          {/* End Date */}
          <div className="requests-filter-field">
            <label htmlFor="req-to-date" className="cpa-label">
              结束日期
            </label>
            <Input
              id="req-to-date"
              type="date"
              value={draft.to}
              onChange={(e) => handleDateChange('to', e.target.value)}
              aria-label="结束日期"
            />
          </div>

          {/* Client Key */}
          <div className="requests-filter-field">
            <label htmlFor="req-key-id" className="cpa-label">
              客户端 Key
            </label>
            <Select
              id="req-key-id"
              value={draft.key_id}
              onChange={(val) => updateField('key_id', val)}
              options={keyOptions}
              searchable
              onSearchChange={text => onFacetSearch?.('key_id', text)}
              searchPlaceholder="搜索 Key 名称或 ID..."
              aria-label="客户端 Key"
            />
          </div>

          {/* Model */}
          <div className="requests-filter-field">
            <label htmlFor="req-model" className="cpa-label">
              模型
            </label>
            <Select
              id="req-model"
              value={draft.model}
              onChange={(val) => updateField('model', val)}
              options={modelOptions}
              searchable
              onSearchChange={text => onFacetSearch?.('model', text)}
              searchPlaceholder="搜索模型 ID..."
              aria-label="模型"
            />
          </div>

          {/* Provider */}
          <div className="requests-filter-field">
            <label htmlFor="req-provider" className="cpa-label">
              提供商
            </label>
            <Select
              id="req-provider"
              value={draft.provider}
              onChange={(val) => updateField('provider', val)}
              options={providerOptions}
              searchable
              onSearchChange={text => onFacetSearch?.('provider', text)}
              searchPlaceholder="搜索提供商..."
              aria-label="提供商"
            />
          </div>

          {/* Account */}
          <div className="requests-filter-field">
            <label htmlFor="req-auth-index" className="cpa-label">
              账户
            </label>
            <Select
              id="req-auth-index"
              value={draft.auth_index}
              onChange={(val) => updateField('auth_index', val)}
              options={accountOptions}
              searchable
              onSearchChange={text => onFacetSearch?.('auth_index', text)}
              searchPlaceholder="搜索账户..."
              aria-label="账户"
            />
          </div>

          {/* Status */}
          <div className="requests-filter-field">
            <label htmlFor="req-status" className="cpa-label">
              状态
            </label>
            <Select
              id="req-status"
              value={draft.status}
              onChange={(val) => updateField('status', val as '' | 'success' | 'failed')}
              options={statusOptions}
              aria-label="状态"
            />
          </div>

          {/* Actions: Reset & Query */}
          <div className="requests-filter-actions-inline">
            <Button variant="ghost" onClick={onReset} disabled={loading}>
              重置
            </Button>
            <Button variant="primary" type="submit" loading={loading}>
              查询
            </Button>
          </div>
        </div>
      </form>
    </div>
  );
}
