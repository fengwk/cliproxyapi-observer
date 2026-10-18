import { useState, useRef, useEffect, useCallback } from 'react';
import type { FormEvent } from 'react';
import type { ApiClient } from '../lib/api';
import type { Key, KeySpec, Limits } from '../lib/types';
import { formatUSD, toLocalDatetimeInput, errorMessage } from '../lib/format';
import { Button, Input, Field, Modal, Switch, Notice } from '../components/ui';
import { ModelPicker } from './ModelPicker';
import {
  QUOTA_DESCRIPTORS,
  validateCustomSecret,
  validateConcurrency,
  parseQuotaLimit,
  validateExpiry,
} from './KeysHelpers';

export interface RevealedSecretState {
  secret: string;
  label: string;
  action: 'create' | 'rotate';
}

export interface KeyEditorProps {
  keyData?: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: (revealed?: RevealedSecretState) => void;
}

export function KeyEditor({ keyData, api, onClose, onSuccess }: KeyEditorProps) {
  const isEdit = !!keyData;

  const [label, setLabel] = useState<string>(keyData ? keyData.label || '' : '');
  const [enabled, setEnabled] = useState<boolean>(keyData ? keyData.enabled : true);

  // Secret state (only for create)
  const [secretType, setSecretType] = useState<'auto' | 'custom'>('auto');
  const [customSecret, setCustomSecret] = useState<string>('');
  const [showCustomSecret, setShowCustomSecret] = useState<boolean>(false);

  // Lifecycle & Concurrency
  const [hasExpiry, setHasExpiry] = useState<boolean>(keyData ? !!keyData.expires_at : false);
  const [expiryInput, setExpiryInput] = useState<string>(() =>
    keyData ? toLocalDatetimeInput(keyData.expires_at) : ''
  );

  const [hasMaxConcurrent, setHasMaxConcurrent] = useState<boolean>(
    keyData ? keyData.max_concurrent > 0 : false
  );
  const [maxConcurrentInput, setMaxConcurrentInput] = useState<string>(
    keyData && keyData.max_concurrent > 0 ? String(keyData.max_concurrent) : '5'
  );

  // Model permissions
  const [isRestrictedModels, setIsRestrictedModels] = useState<boolean>(
    keyData ? keyData.allowed_models.length > 0 : false
  );
  const [allowedModels, setAllowedModels] = useState<string[]>(
    keyData ? [...keyData.allowed_models] : []
  );
  const [modelError, setModelError] = useState<string>('');

  // 4 Quota Windows state
  const [quotas, setQuotas] = useState<{ [K in keyof Limits]: { enabled: boolean; value: string } }>(() => ({
    five_hour: {
      enabled: keyData ? keyData.limits.five_hour > 0 : false,
      value: keyData && keyData.limits.five_hour > 0 ? formatUSD(keyData.limits.five_hour) : '',
    },
    week: {
      enabled: keyData ? keyData.limits.week > 0 : false,
      value: keyData && keyData.limits.week > 0 ? formatUSD(keyData.limits.week) : '',
    },
    month: {
      enabled: keyData ? keyData.limits.month > 0 : false,
      value: keyData && keyData.limits.month > 0 ? formatUSD(keyData.limits.month) : '',
    },
    total: {
      enabled: keyData ? keyData.limits.total > 0 : false,
      value: keyData && keyData.limits.total > 0 ? formatUSD(keyData.limits.total) : '',
    },
  }));

  const [submitting, setSubmitting] = useState<boolean>(false);
  const [formError, setFormError] = useState<string>('');

  const isSubmittingRef = useRef<boolean>(false);
  const abortControllerRef = useRef<AbortController | null>(null);
  const mountedRef = useRef<boolean>(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
    };
  }, []);

  // Safe close wrapper: no-op during submission
  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  // Weak password hint (untrimmed!)
  const isWeakCustom =
    !isEdit &&
    secretType === 'custom' &&
    customSecret.length >= 8 &&
    customSecret.length < 32;

  const handleQuotaToggle = (k: keyof Limits, enabledVal: boolean) => {
    setQuotas((prev) => ({
      ...prev,
      [k]: {
        ...prev[k],
        enabled: enabledVal,
      },
    }));
  };

  const handleQuotaValueChange = (k: keyof Limits, val: string) => {
    setQuotas((prev) => ({
      ...prev,
      [k]: {
        ...prev[k],
        value: val,
      },
    }));
  };

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (isSubmittingRef.current) {
      return;
    }

    setFormError('');
    setModelError('');

    // 1. Label
    if (!label.trim()) {
      setFormError(isEdit ? 'Key 名称不能为空' : '请输入 Key 名称');
      return;
    }

    // 2. Custom Secret (Create only) - untrimmed validation!
    if (!isEdit && secretType === 'custom') {
      const secVal = validateCustomSecret(customSecret);
      if (!secVal.valid) {
        setFormError(secVal.error || '自定义密钥不合法');
        return;
      }
    }

    // 3. Restricted Models
    if (isRestrictedModels && allowedModels.length === 0) {
      setModelError('开启限制模型访问时，必须至少选择或添加一个模型');
      return;
    }

    // 4. Concurrency
    let parsedConcurrent = 0;
    if (hasMaxConcurrent) {
      const concRes = validateConcurrency(maxConcurrentInput);
      if (!concRes.valid) {
        setFormError(concRes.error || '最大并发数不合法');
        return;
      }
      parsedConcurrent = concRes.value!;
    }

    // 5. Expiry
    const expRes = validateExpiry(hasExpiry, expiryInput, keyData?.expires_at);
    if (!expRes.valid) {
      setFormError(expRes.error || '到期时间不合法');
      return;
    }

    // 6. Quotas
    const parsedLimits: Limits = {
      five_hour: 0,
      week: 0,
      month: 0,
      total: 0,
    };
    for (const desc of QUOTA_DESCRIPTORS) {
      const qState = quotas[desc.key];
      const qRes = parseQuotaLimit(qState.enabled, qState.value, desc.label);
      if (!qRes.valid) {
        setFormError(qRes.error || `${desc.label}金额不合法`);
        return;
      }
      parsedLimits[desc.key] = qRes.value;
    }

    const spec: KeySpec = {
      label: label.trim(),
      enabled,
      expires_at: expRes.iso,
      max_concurrent: parsedConcurrent,
      allowed_models: isRestrictedModels ? allowedModels : [],
      limits: parsedLimits,
    };

    // Auto omits secret; custom preserves exact untrimmed secret; edit PUT never sends secret
    if (!isEdit && secretType === 'custom') {
      spec.secret = customSecret;
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    abortControllerRef.current = new AbortController();

    try {
      if (isEdit) {
        await api.request(`/keys?id=${encodeURIComponent(keyData.id)}`, {
          method: 'PUT',
          body: spec,
          signal: abortControllerRef.current.signal,
        });
        if (mountedRef.current) {
          onSuccess();
        }
      } else {
        const res = await api.request<{ key: Key; secret: string }>('/keys', {
          method: 'POST',
          body: spec,
          signal: abortControllerRef.current.signal,
        });
        if (mountedRef.current) {
          onSuccess({
            secret: res.secret,
            label: res.key?.label || spec.label,
            action: 'create',
          });
        }
      }
    } catch (err: unknown) {
      if (abortControllerRef.current?.signal.aborted) {
        return;
      }
      if (!mountedRef.current) {
        return;
      }
      const msg = errorMessage(err);
      if (msg.includes('409') || msg.toLowerCase().includes('conflict')) {
        setFormError('该密钥已存在冲突，请使用唯一的自定义密钥');
      } else {
        setFormError(msg || (isEdit ? '更新 Key 失败' : '创建 Key 失败'));
      }
    } finally {
      if (mountedRef.current) {
        setSubmitting(false);
      }
      isSubmittingRef.current = false;
    }
  };

  return (
    <Modal
      title={isEdit ? `编辑 Key: ${keyData.label}` : '创建新 Key'}
      onClose={handleSafeClose}
      size="lg"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="primary" onClick={handleSubmit} loading={submitting}>
            {isEdit ? '保存修改' : '确认创建'}
          </Button>
        </div>
      }
    >
      <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
        {formError && <Notice tone="danger">{formError}</Notice>}

        {/* 1. 基本信息 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>1. 基本信息</h3>
          <Field
            label="Key 名称"
            htmlFor={isEdit ? 'edit-label' : 'create-label'}
            hint={!isEdit ? '用于识别应用或使用者，如 dev-api, mobile-app' : undefined}
          >
            <Input
              id={isEdit ? 'edit-label' : 'create-label'}
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder={isEdit ? '' : '例如: 生产环境客户端 / 研发测试'}
              required
            />
          </Field>

          {isEdit ? (
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem', marginTop: '0.5rem' }}>
              <Switch checked={enabled} onChange={setEnabled} aria-label="启用该 Key" />
              <span style={{ fontSize: '0.88rem' }}>{enabled ? '已启用该 Key' : '已禁用该 Key'}</span>
            </div>
          ) : (
            <Field label="密钥生成方式">
              <div className="cpa-radio-group" style={{ marginBottom: '0.75rem' }}>
                <label className="cpa-radio-label">
                  <input
                    type="radio"
                    name="secretType"
                    value="auto"
                    checked={secretType === 'auto'}
                    onChange={() => setSecretType('auto')}
                  />
                  自动生成高强度密钥 (推荐 32+ 位)
                </label>
                <label className="cpa-radio-label">
                  <input
                    type="radio"
                    name="secretType"
                    value="custom"
                    checked={secretType === 'custom'}
                    onChange={() => setSecretType('custom')}
                  />
                  自定义密钥
                </label>
              </div>

              {secretType === 'custom' && (
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                    <Input
                      id="create-custom-secret"
                      type={showCustomSecret ? 'text' : 'password'}
                      value={customSecret}
                      onChange={(e) => setCustomSecret(e.target.value)}
                      placeholder="请输入 8-256 位 ASCII 密钥（无空格）"
                      autoComplete="off"
                      aria-label="自定义密钥"
                    />
                    <Button
                      variant="secondary"
                      onClick={() => setShowCustomSecret((prev) => !prev)}
                      aria-label={showCustomSecret ? '隐藏自定义密钥' : '显示自定义密钥'}
                      style={{ padding: '0.4rem 0.6rem', fontSize: '0.8rem', minHeight: '38px', whiteSpace: 'nowrap' }}
                    >
                      {showCustomSecret ? '隐藏' : '显示'}
                    </Button>
                  </div>
                  {isWeakCustom && (
                    <Notice tone="warning">
                      密钥长度少于 32 位（当前 {customSecret.length} 位），建议使用 32 位以上高强度随机密钥。
                    </Notice>
                  )}
                  <span style={{ fontSize: '0.78rem', color: 'var(--text-tertiary)' }}>
                    规范: 8..256 个可见 ASCII 字符 (U+0021..007E)，禁止空格。系统仅存储哈希，不会明文持久化。
                  </span>
                </div>
              )}
            </Field>
          )}
        </div>

        {/* 2. 访问控制与生命周期 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>
            {isEdit ? '生命周期与并发' : '2. 访问控制与生命周期'}
          </h3>
          <div className="cpa-form-grid cpa-form-grid-2">
            <div>
              <Field label="有效期">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch
                    checked={hasExpiry}
                    onChange={setHasExpiry}
                    aria-label={isEdit ? '指定有效期' : '启用有效期限制'}
                  />
                  <span style={{ fontSize: '0.88rem' }}>{hasExpiry ? '指定到期时间' : '永久有效'}</span>
                </div>
                {hasExpiry && (
                  <Input
                    type="datetime-local"
                    value={expiryInput}
                    onChange={(e) => setExpiryInput(e.target.value)}
                    aria-label="到期时间"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="并发限制">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch
                    checked={hasMaxConcurrent}
                    onChange={setHasMaxConcurrent}
                    aria-label={isEdit ? '限制并发数' : '启用并发限制'}
                  />
                  <span style={{ fontSize: '0.88rem' }}>{hasMaxConcurrent ? '限制最大并发' : '不限并发'}</span>
                </div>
                {hasMaxConcurrent && (
                  <Input
                    type="number"
                    min={1}
                    max={1000000}
                    value={maxConcurrentInput}
                    onChange={(e) => setMaxConcurrentInput(e.target.value)}
                    placeholder="最大并发连接数"
                    aria-label="最大并发数"
                  />
                )}
              </Field>
            </div>
          </div>
        </div>

        {/* 3. 模型权限 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>
            {isEdit ? '模型权限' : '3. 模型权限'}
          </h3>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '1rem' }}>
            <Switch
              checked={isRestrictedModels}
              onChange={setIsRestrictedModels}
              aria-label={isEdit ? '限制访问指定模型' : '仅允许指定模型'}
            />
            <span style={{ fontSize: '0.88rem' }}>
              {isRestrictedModels ? '限制访问指定模型列表' : '允许访问全部模型 (不限)'}
            </span>
          </div>

          {isRestrictedModels && (
            <ModelPicker
              api={api}
              selectedModels={allowedModels}
              onChange={(models) => {
                setAllowedModels(models);
                if (models.length > 0 && modelError) {
                  setModelError('');
                }
              }}
              error={modelError}
            />
          )}
        </div>

        {/* 4. 额度上限 (USD) */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>
            {isEdit ? '额度上限 (USD)' : '4. 额度上限 (USD)'}
          </h3>
          <p style={{ fontSize: '0.82rem', color: 'var(--text-secondary)', margin: '0 0 1rem' }}>
            周期是首次请求起固定 5 小时 / 7 天 / 月窗口。开启限额必须输入大于 0 的有效金额；未开启项为“不限”。
          </p>

          <div className="cpa-form-grid cpa-form-grid-2">
            {QUOTA_DESCRIPTORS.map((desc) => {
              const current = quotas[desc.key];
              return (
                <div key={desc.key}>
                  <Field label={desc.label}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                      <Switch
                        checked={current.enabled}
                        onChange={(checked) => handleQuotaToggle(desc.key, checked)}
                        aria-label={desc.switchLabel}
                      />
                      <span style={{ fontSize: '0.85rem' }}>{current.enabled ? '已设限额' : '不限额度'}</span>
                    </div>
                    {current.enabled && (
                      <Input
                        type="text"
                        value={current.value}
                        onChange={(e) => handleQuotaValueChange(desc.key, e.target.value)}
                        placeholder={desc.placeholder}
                        aria-label={desc.label}
                      />
                    )}
                  </Field>
                </div>
              );
            })}
          </div>
        </div>
      </form>
    </Modal>
  );
}
