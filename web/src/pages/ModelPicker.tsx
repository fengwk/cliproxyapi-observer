import React, { useState, useEffect, useMemo, useCallback } from 'react';
import type { ApiClient } from '../lib/api';
import { Button, Input, Notice } from '../components/ui';

export interface ModelPickerProps {
  api: ApiClient;
  selectedModels: string[];
  onChange: (models: string[]) => void;
  error?: string;
}

export function ModelPicker({ api, selectedModels, onChange, error }: ModelPickerProps) {
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loading, setLoading] = useState<boolean>(false);
  const [fetchError, setFetchError] = useState<string>('');
  const [searchQuery, setSearchQuery] = useState<string>('');
  const [manualInput, setManualInput] = useState<string>('');
  const [manualError, setManualError] = useState<string>('');

  const loadModels = useCallback(async (force = false) => {
    setLoading(true);
    setFetchError('');
    try {
      const list = await api.models({ force });
      const ids = list.map((m) => m.id.trim()).filter(Boolean);
      setAvailableModels((prev) => {
        const combined = Array.from(new Set([...prev, ...ids]));
        return combined.sort();
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      setFetchError(msg || '获取模型目录失败，您仍可手动添加精确模型 ID');
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => {
    // Automatically load model catalog on mount
    loadModels();
  }, [loadModels]);

  // Merge selected models into available options so preserved/custom IDs never disappear
  const allKnownModels = useMemo(() => {
    const set = new Set([...selectedModels, ...availableModels]);
    return Array.from(set).sort();
  }, [selectedModels, availableModels]);

  const filteredModels = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    if (!q) {
      return allKnownModels;
    }
    return allKnownModels.filter((id) => id.toLowerCase().includes(q));
  }, [allKnownModels, searchQuery]);

  const handleToggle = (id: string) => {
    if (selectedModels.includes(id)) {
      onChange(selectedModels.filter((m) => m !== id));
    } else {
      onChange([...selectedModels, id]);
    }
  };

  const handleSelectAllFiltered = () => {
    const set = new Set([...selectedModels, ...filteredModels]);
    onChange(Array.from(set));
  };

  const handleDeselectAll = () => {
    onChange([]);
  };

  const handleAddManual = (e: React.FormEvent) => {
    e.preventDefault();
    const id = manualInput.trim();
    if (!id) {
      setManualError('请输入精确模型 ID');
      return;
    }
    if (selectedModels.includes(id)) {
      setManualError('该模型已在选择列表中');
      return;
    }
    setManualError('');
    setManualInput('');
    setAvailableModels((prev) => Array.from(new Set([...prev, id])));
    onChange([...selectedModels, id]);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      {fetchError && (
        <Notice tone="warning" className="cpa-notice-compact">
          {fetchError}
        </Notice>
      )}

      {error && (
        <div className="cpa-error-text" role="alert">
          {error}
        </div>
      )}

      {/* Manual Input Form */}
      <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'flex-start' }}>
        <div style={{ flex: 1 }}>
          <Input
            value={manualInput}
            onChange={(e) => {
              setManualInput(e.target.value);
              if (manualError) setManualError('');
            }}
            placeholder="手动添加模型 ID (例如: gpt-4o, claude-3-5-sonnet)"
            error={!!manualError}
            aria-label="手动添加精确模型 ID"
          />
          {manualError && (
            <div className="cpa-error-text" style={{ marginTop: '0.25rem' }}>
              {manualError}
            </div>
          )}
        </div>
        <Button variant="secondary" onClick={handleAddManual}>
          添加
        </Button>
      </div>

      {/* Search and Action Bar */}
      <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center', flexWrap: 'wrap' }}>
        <div style={{ flex: 1, minWidth: '180px' }}>
          <Input
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="搜索模型..."
            aria-label="搜索模型"
          />
        </div>
        <Button
          variant="ghost"
          onClick={() => loadModels(true)}
          loading={loading}
          title="强制刷新权威模型目录"
        >
          刷新目录
        </Button>
        <Button variant="ghost" onClick={handleSelectAllFiltered} disabled={filteredModels.length === 0}>
          全选当前
        </Button>
        <Button variant="ghost" onClick={handleDeselectAll} disabled={selectedModels.length === 0}>
          清空已选
        </Button>
      </div>

      {/* Selected summary */}
      <div style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
        已选择: <strong>{selectedModels.length}</strong> 个模型
      </div>

      {/* Selected Tags list */}
      {selectedModels.length > 0 && (
        <div
          style={{
            display: 'flex',
            flexWrap: 'wrap',
            gap: '0.35rem',
            maxHeight: '100px',
            overflowY: 'auto',
            padding: '0.5rem',
            backgroundColor: 'var(--bg-secondary)',
            border: '1px solid var(--border-color)',
            borderRadius: 'var(--radius-md)',
          }}
        >
          {selectedModels.map((id) => (
            <span
              key={id}
              className="cpa-badge cpa-badge-neutral"
              style={{ display: 'inline-flex', alignItems: 'center', gap: '0.35rem' }}
            >
              <span>{id}</span>
              <button
                type="button"
                onClick={() => handleToggle(id)}
                style={{
                  background: 'none',
                  border: 'none',
                  cursor: 'pointer',
                  padding: 0,
                  fontSize: '0.85rem',
                  color: 'var(--text-tertiary)',
                  lineHeight: 1,
                }}
                aria-label={`移除 ${id}`}
              >
                ✕
              </button>
            </span>
          ))}
        </div>
      )}

      {/* Models Checklist Container */}
      <div
        style={{
          border: '1px solid var(--border-color)',
          borderRadius: 'var(--radius-md)',
          maxHeight: '180px',
          overflowY: 'auto',
          backgroundColor: 'var(--bg-primary)',
          padding: '0.35rem 0.5rem',
        }}
      >
        {filteredModels.length === 0 ? (
          <div style={{ padding: '1rem', textAlign: 'center', color: 'var(--text-tertiary)' }}>
            {loading ? '正在加载模型目录...' : '未找到匹配的模型'}
          </div>
        ) : (
          filteredModels.map((id) => {
            const isChecked = selectedModels.includes(id);
            return (
              <label
                key={id}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: '0.5rem',
                  padding: '0.35rem 0.5rem',
                  borderRadius: 'var(--radius-sm)',
                  cursor: 'pointer',
                  fontSize: '0.88rem',
                  backgroundColor: isChecked ? 'var(--bg-tertiary)' : 'transparent',
                }}
              >
                <input
                  type="checkbox"
                  checked={isChecked}
                  onChange={() => handleToggle(id)}
                  style={{ cursor: 'pointer' }}
                />
                <span style={{ fontFamily: 'var(--font-mono)', wordBreak: 'break-all' }}>{id}</span>
              </label>
            );
          })
        )}
      </div>
    </div>
  );
}
