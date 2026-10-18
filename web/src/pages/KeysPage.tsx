import React, { useState, useCallback, useEffect } from 'react';
import type { ApiClient, PageProps } from '../lib/api';
import type { Key, KeySpec, Limits } from '../lib/types';
import {
  parseUSD,
  formatUSD,
  formatDateTime,
  toLocalDatetimeInput,
  preserveOriginalExpiry,
  errorMessage,
} from '../lib/format';
import { usePoll } from '../lib/usePoll';
import { Button, Input, Field, Modal, Badge, Notice, EmptyState, Switch } from '../components/ui';
import { ModelPicker } from './ModelPicker';

export interface KeysPageProps extends PageProps {}

interface RevealedSecretState {
  secret: string;
  label: string;
  action: 'create' | 'rotate';
}

type ActiveModal =
  | { type: 'create' }
  | { type: 'edit'; key: Key }
  | { type: 'rotate'; key: Key }
  | { type: 'reset'; key: Key }
  | { type: 'revoke'; key: Key }
  | { type: 'recover'; key: Key }
  | { type: 'details'; key: Key }
  | { type: 'revealed'; data: RevealedSecretState }
  | null;

export function KeysPage({ api, refreshInterval, onDirtyChange }: KeysPageProps) {
  const [keys, setKeys] = useState<Key[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [pageError, setPageError] = useState<string>('');
  const [activeModal, setActiveModal] = useState<ActiveModal>(null);
  const [copiedId, setCopiedId] = useState<string | null>(null);

  // Sync dirty state to parent: when any edit/create/action modal is open, dirty is true
  useEffect(() => {
    const isDirty = activeModal !== null && activeModal.type !== 'revealed' && activeModal.type !== 'details';
    onDirtyChange(isDirty);
  }, [activeModal, onDirtyChange]);

  const loadKeys = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const response = await api.request<{ items: Key[] }>('/keys', { signal });
        if (response && Array.isArray(response.items)) {
          setKeys(response.items);
          setPageError('');
        }
      } catch (err: unknown) {
        if (signal?.aborted) return;
        const msg = errorMessage(err);
        setPageError(msg || '加载 Key 列表失败');
      } finally {
        setLoading(false);
      }
    },
    [api]
  );

  // Polling hook (pauses when dirty / refreshInterval <= 0)
  const isPaused = activeModal !== null && activeModal.type !== 'revealed' && activeModal.type !== 'details';
  usePoll(loadKeys, refreshInterval, isPaused);

  const handleCopy = async (text: string, id: string) => {
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        document.execCommand('copy');
        document.body.removeChild(ta);
      }
      setCopiedId(id);
      setTimeout(() => setCopiedId(null), 2000);
    } catch (_) {
      // Clipboard write failed
    }
  };

  const maskId = (id: string) => {
    if (id.length <= 16) return id;
    return `${id.slice(0, 8)}...${id.slice(-6)}`;
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
      {/* Top action header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '0.75rem' }}>
        <div>
          <h2 style={{ fontSize: '1.25rem', fontWeight: 600, margin: 0, color: 'var(--text-primary)' }}>
            客户端 Key
          </h2>
          <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
            管理客户端访问凭据、并发度、模型授权与用量额度
          </span>
        </div>

        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={() => loadKeys()} loading={loading}>
            刷新
          </Button>
          <Button variant="primary" onClick={() => setActiveModal({ type: 'create' })}>
            + 新建 Key
          </Button>
        </div>
      </div>

      {pageError && <Notice tone="danger">{pageError}</Notice>}

      {/* Keys Table / Empty State */}
      {keys.length === 0 && !loading ? (
        <EmptyState
          title="暂无任何 Key"
          description="尚未创建任何客户端 Key。点击下方按钮立即创建第一个 Key。"
          action={
            <Button variant="primary" onClick={() => setActiveModal({ type: 'create' })}>
              新建第一个 Key
            </Button>
          }
        />
      ) : (
        <div className="cpa-table-container">
          <table className="cpa-table" aria-label="Key 列表">
            <thead>
              <tr>
                <th>名称 / 状态</th>
                <th>Key ID</th>
                <th>额度使用 (已用 / 限额)</th>
                <th>并发限制</th>
                <th>模型权限</th>
                <th>有效期</th>
                <th style={{ textAlign: 'right' }}>操作</th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => {
                const isBlocked = !!k.blocked_reason;
                const isExpired = k.expires_at ? new Date(k.expires_at) < new Date() : false;

                return (
                  <tr key={k.id}>
                    {/* Name & Status */}
                    <td>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                          <strong style={{ fontSize: '0.95rem' }}>{k.label || '(未命名)'}</strong>
                          {isBlocked ? (
                            <Badge tone="danger">封锁</Badge>
                          ) : !k.enabled ? (
                            <Badge tone="neutral">已禁用</Badge>
                          ) : isExpired ? (
                            <Badge tone="warning">已过期</Badge>
                          ) : (
                            <Badge tone="success">正常</Badge>
                          )}
                        </div>
                        {isBlocked && (
                          <span style={{ fontSize: '0.78rem', color: 'var(--danger-color)' }}>
                            {k.blocked_reason}
                          </span>
                        )}
                      </div>
                    </td>

                    {/* Masked ID + Copy */}
                    <td>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.4rem' }}>
                        <button
                          type="button"
                          className="cpa-code"
                          style={{
                            border: 'none',
                            cursor: 'pointer',
                            textAlign: 'left',
                          }}
                          onClick={() => setActiveModal({ type: 'details', key: k })}
                          title="点击查看详细信息"
                        >
                          {maskId(k.id)}
                        </button>
                        <Button
                          variant="ghost"
                          style={{ padding: '0.2rem 0.4rem', minHeight: 'auto', fontSize: '0.75rem' }}
                          onClick={() => handleCopy(k.id, k.id)}
                          title="复制完整 ID"
                        >
                          {copiedId === k.id ? '已复制' : '复制'}
                        </Button>
                      </div>
                    </td>

                    {/* Compact Quota Lines (Explicit Unlimited) */}
                    <td>
                      <div
                        style={{
                          display: 'grid',
                          gridTemplateColumns: 'auto 1fr',
                          gap: '0.15rem 0.6rem',
                          fontSize: '0.8rem',
                          whiteSpace: 'nowrap',
                        }}
                      >
                        <span style={{ color: 'var(--text-secondary)' }}>5小时:</span>
                        <span>
                          ${formatUSD(k.used.five_hour)} /{' '}
                          {k.limits.five_hour > 0 ? `$${formatUSD(k.limits.five_hour)}` : '不限'}
                        </span>

                        <span style={{ color: 'var(--text-secondary)' }}>周:</span>
                        <span>
                          ${formatUSD(k.used.week)} /{' '}
                          {k.limits.week > 0 ? `$${formatUSD(k.limits.week)}` : '不限'}
                        </span>

                        <span style={{ color: 'var(--text-secondary)' }}>月:</span>
                        <span>
                          ${formatUSD(k.used.month)} /{' '}
                          {k.limits.month > 0 ? `$${formatUSD(k.limits.month)}` : '不限'}
                        </span>

                        <span style={{ color: 'var(--text-secondary)' }}>总计:</span>
                        <span>
                          ${formatUSD(k.used.total)} /{' '}
                          {k.limits.total > 0 ? `$${formatUSD(k.limits.total)}` : '不限'}
                        </span>
                      </div>
                    </td>

                    {/* Concurrency */}
                    <td>
                      <div style={{ fontSize: '0.85rem' }}>
                        {k.max_concurrent === 0 ? (
                          <span style={{ color: 'var(--text-secondary)' }}>不限并发</span>
                        ) : (
                          <span>
                            {k.active} / {k.max_concurrent} 并发
                          </span>
                        )}
                      </div>
                    </td>

                    {/* Models Permission */}
                    <td>
                      <div style={{ fontSize: '0.85rem' }}>
                        {k.allowed_models.length === 0 ? (
                          <span style={{ color: 'var(--text-secondary)' }}>全部模型</span>
                        ) : (
                          <Badge tone="neutral">{k.allowed_models.length} 个指定模型</Badge>
                        )}
                      </div>
                    </td>

                    {/* Expiry */}
                    <td>
                      <div style={{ fontSize: '0.82rem', whiteSpace: 'nowrap' }}>
                        {!k.expires_at ? (
                          <span style={{ color: 'var(--text-secondary)' }}>永久有效</span>
                        ) : (
                          <span style={{ color: isExpired ? 'var(--warning-color)' : 'inherit' }}>
                            {formatDateTime(k.expires_at)}
                          </span>
                        )}
                      </div>
                    </td>

                    {/* Actions */}
                    <td style={{ textAlign: 'right' }}>
                      <div style={{ display: 'inline-flex', gap: '0.35rem', alignItems: 'center' }}>
                        <Button
                          variant="ghost"
                          onClick={() => setActiveModal({ type: 'edit', key: k })}
                          title="编辑配置与额度"
                        >
                          编辑
                        </Button>

                        <Button
                          variant="ghost"
                          onClick={() => setActiveModal({ type: 'rotate', key: k })}
                          title="轮转生成新密钥"
                        >
                          轮转
                        </Button>

                        <Button
                          variant="ghost"
                          onClick={() => setActiveModal({ type: 'reset', key: k })}
                          title="重置额度周期"
                        >
                          重置
                        </Button>

                        {isBlocked ? (
                          <Button
                            variant="danger"
                            onClick={() => setActiveModal({ type: 'recover', key: k })}
                            title="核对并解除异常封锁"
                          >
                            恢复
                          </Button>
                        ) : k.enabled ? (
                          <Button
                            variant="ghost"
                            onClick={() => setActiveModal({ type: 'revoke', key: k })}
                            title="禁用此 Key"
                          >
                            禁用
                          </Button>
                        ) : (
                          <Button
                            variant="ghost"
                            onClick={() => setActiveModal({ type: 'revoke', key: k })}
                            title="重新启用此 Key"
                          >
                            启用
                          </Button>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {/* ------------------------------------------------------------- MODALS */}

      {/* Create Key Modal */}
      {activeModal?.type === 'create' && (
        <CreateKeyModal
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={(revealed) => {
            setActiveModal({ type: 'revealed', data: revealed });
            loadKeys();
          }}
        />
      )}

      {/* Edit Key Modal */}
      {activeModal?.type === 'edit' && (
        <EditKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={() => {
            setActiveModal(null);
            loadKeys();
          }}
        />
      )}

      {/* Rotate Key Modal */}
      {activeModal?.type === 'rotate' && (
        <RotateKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={(revealed) => {
            setActiveModal({ type: 'revealed', data: revealed });
            loadKeys();
          }}
        />
      )}

      {/* Reset Quota Modal */}
      {activeModal?.type === 'reset' && (
        <ResetKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={() => {
            setActiveModal(null);
            loadKeys();
          }}
        />
      )}

      {/* Revoke / Toggle Status Modal */}
      {activeModal?.type === 'revoke' && (
        <RevokeKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={() => {
            setActiveModal(null);
            loadKeys();
          }}
        />
      )}

      {/* Recover / Unblock Modal */}
      {activeModal?.type === 'recover' && (
        <RecoverKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={() => {
            setActiveModal(null);
            loadKeys();
          }}
        />
      )}

      {/* Details Modal */}
      {activeModal?.type === 'details' && (
        <DetailsModal
          keyData={activeModal.key}
          onClose={() => setActiveModal(null)}
          onCopy={handleCopy}
          copiedId={copiedId}
        />
      )}

      {/* Revealed Secret Modal */}
      {activeModal?.type === 'revealed' && (
        <SecretRevealedModal
          data={activeModal.data}
          onClose={() => setActiveModal(null)}
          onCopy={handleCopy}
          copiedId={copiedId}
        />
      )}
    </div>
  );
}

/* ==========================================================================
   CreateKeyModal
   ========================================================================== */

interface CreateKeyModalProps {
  api: ApiClient;
  onClose: () => void;
  onSuccess: (data: RevealedSecretState) => void;
}

function CreateKeyModal({ api, onClose, onSuccess }: CreateKeyModalProps) {
  const [label, setLabel] = useState<string>('');
  const [secretType, setSecretType] = useState<'auto' | 'custom'>('auto');
  const [customSecret, setCustomSecret] = useState<string>('');

  const [hasExpiry, setHasExpiry] = useState<boolean>(false);
  const [expiryInput, setExpiryInput] = useState<string>('');

  const [hasMaxConcurrent, setHasMaxConcurrent] = useState<boolean>(false);
  const [maxConcurrentInput, setMaxConcurrentInput] = useState<string>('5');

  const [isRestrictedModels, setIsRestrictedModels] = useState<boolean>(false);
  const [allowedModels, setAllowedModels] = useState<string[]>([]);
  const [modelError, setModelError] = useState<string>('');

  // 4 Quota Windows
  const [hasLimit5H, setHasLimit5H] = useState<boolean>(false);
  const [limit5H, setLimit5H] = useState<string>('');

  const [hasLimitWeek, setHasLimitWeek] = useState<boolean>(false);
  const [limitWeek, setLimitWeek] = useState<string>('');

  const [hasLimitMonth, setHasLimitMonth] = useState<boolean>(false);
  const [limitMonth, setLimitMonth] = useState<string>('');

  const [hasLimitTotal, setHasLimitTotal] = useState<boolean>(false);
  const [limitTotal, setLimitTotal] = useState<string>('');

  const [submitting, setSubmitting] = useState<boolean>(false);
  const [formError, setFormError] = useState<string>('');

  // Validate custom secret
  const customSecretTrimmed = customSecret.trim();
  const isWeakCustom =
    secretType === 'custom' &&
    customSecretTrimmed.length >= 8 &&
    customSecretTrimmed.length < 32;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError('');
    setModelError('');

    if (!label.trim()) {
      setFormError('请输入 Key 名称');
      return;
    }

    if (secretType === 'custom') {
      if (customSecretTrimmed.length < 8 || customSecretTrimmed.length > 256) {
        setFormError('自定义密钥长度必须在 8 到 256 位之间');
        return;
      }
      if (!/^[\x21-\x7E]+$/.test(customSecretTrimmed)) {
        setFormError('自定义密钥只能包含可见 ASCII 字符且不能有空格或换行');
        return;
      }
    }

    if (isRestrictedModels && allowedModels.length === 0) {
      setModelError('开启限制模型访问时，必须至少选择或添加一个模型');
      return;
    }

    let parsed5H = 0;
    let parsedWeek = 0;
    let parsedMonth = 0;
    let parsedTotal = 0;

    try {
      if (hasLimit5H && limit5H) parsed5H = parseUSD(limit5H);
      if (hasLimitWeek && limitWeek) parsedWeek = parseUSD(limitWeek);
      if (hasLimitMonth && limitMonth) parsedMonth = parseUSD(limitMonth);
      if (hasLimitTotal && limitTotal) parsedTotal = parseUSD(limitTotal);
    } catch (err: unknown) {
      setFormError(errorMessage(err));
      return;
    }

    let parsedConcurrent = 0;
    if (hasMaxConcurrent) {
      parsedConcurrent = parseInt(maxConcurrentInput, 10);
      if (isNaN(parsedConcurrent) || parsedConcurrent < 1) {
        setFormError('最大并发数必须为大于等于 1 的整数');
        return;
      }
    }

    let isoExpiresAt: string | null = null;
    if (hasExpiry && expiryInput) {
      const d = new Date(expiryInput);
      if (isNaN(d.getTime())) {
        setFormError('指定的过期时间无效');
        return;
      }
      isoExpiresAt = d.toISOString();
    }

    const limits: Limits = {
      five_hour: parsed5H,
      week: parsedWeek,
      month: parsedMonth,
      total: parsedTotal,
    };

    const spec: KeySpec = {
      label: label.trim(),
      enabled: true,
      expires_at: isoExpiresAt,
      max_concurrent: parsedConcurrent,
      allowed_models: isRestrictedModels ? allowedModels : [],
      limits,
      secret: secretType === 'custom' ? customSecretTrimmed : undefined,
    };

    setSubmitting(true);
    try {
      const res = await api.request<{ key: Key; secret: string }>('/keys', {
        method: 'POST',
        body: spec,
      });

      onSuccess({
        secret: res.secret,
        label: res.key?.label || spec.label,
        action: 'create',
      });
    } catch (err: unknown) {
      const msg = errorMessage(err);
      if (msg.includes('409') || msg.toLowerCase().includes('conflict')) {
        setFormError('该密钥已存在冲突，请使用唯一的自定义密钥');
      } else {
        setFormError(msg || '创建 Key 失败');
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title="创建新 Key"
      onClose={onClose}
      size="lg"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="primary" onClick={handleSubmit} loading={submitting}>
            确认创建
          </Button>
        </div>
      }
    >
      <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
        {formError && <Notice tone="danger">{formError}</Notice>}

        {/* 1. 基本信息 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>1. 基本信息</h3>
          <Field label="Key 名称" htmlFor="create-label" hint="用于识别应用或使用者，如 dev-api, mobile-app">
            <Input
              id="create-label"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="例如: 生产环境客户端 / 研发测试"
              required
            />
          </Field>

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
                <Input
                  type="text"
                  value={customSecret}
                  onChange={(e) => setCustomSecret(e.target.value)}
                  placeholder="请输入 8-256 位 ASCII 密钥（无空格）"
                  autoComplete="off"
                />
                {isWeakCustom && (
                  <Notice tone="warning">
                    密钥长度少于 32 位（当前 {customSecretTrimmed.length} 位），建议使用 32 位以上高强度随机密钥。
                  </Notice>
                )}
                <span style={{ fontSize: '0.78rem', color: 'var(--text-tertiary)' }}>
                  规范: 8..256 个可见 ASCII 字符 (U+0021..007E)，禁止空格。系统仅存储哈希，不会明文持久化。
                </span>
              </div>
            )}
          </Field>
        </div>

        {/* 2. 有效期与并发 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>2. 访问控制与生命周期</h3>
          <div className="cpa-form-grid cpa-form-grid-2">
            <div>
              <Field label="有效期">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch checked={hasExpiry} onChange={setHasExpiry} aria-label="启用有效期限制" />
                  <span style={{ fontSize: '0.88rem' }}>{hasExpiry ? '指定到期时间' : '永久有效'}</span>
                </div>
                {hasExpiry && (
                  <Input
                    type="datetime-local"
                    value={expiryInput}
                    onChange={(e) => setExpiryInput(e.target.value)}
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="并发限制">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch checked={hasMaxConcurrent} onChange={setHasMaxConcurrent} aria-label="启用并发限制" />
                  <span style={{ fontSize: '0.88rem' }}>{hasMaxConcurrent ? '限制最大并发' : '不限并发'}</span>
                </div>
                {hasMaxConcurrent && (
                  <Input
                    type="number"
                    min={1}
                    value={maxConcurrentInput}
                    onChange={(e) => setMaxConcurrentInput(e.target.value)}
                    placeholder="最大并发连接数"
                  />
                )}
              </Field>
            </div>
          </div>
        </div>

        {/* 3. 模型权限 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>3. 模型权限</h3>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '1rem' }}>
            <Switch checked={isRestrictedModels} onChange={setIsRestrictedModels} aria-label="仅允许指定模型" />
            <span style={{ fontSize: '0.88rem' }}>
              {isRestrictedModels ? '限制访问指定模型列表' : '允许访问全部模型 (不限)'}
            </span>
          </div>

          {isRestrictedModels && (
            <ModelPicker
              api={api}
              selectedModels={allowedModels}
              onChange={setAllowedModels}
              error={modelError}
            />
          )}
        </div>

        {/* 4. 额度控制 */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>4. 额度上限 (USD)</h3>
          <p style={{ fontSize: '0.82rem', color: 'var(--text-secondary)', margin: '0 0 1rem' }}>
            明确开启限额并输入金额（最多支持 6 位小数，例如 1.5 或 0.05）。未开启项将显示为“不限”。
          </p>

          <div className="cpa-form-grid cpa-form-grid-2">
            <div>
              <Field label="5 小时滑动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimit5H} onChange={setHasLimit5H} aria-label="启用5小时限额" />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimit5H ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimit5H && (
                  <Input
                    type="text"
                    value={limit5H}
                    onChange={(e) => setLimit5H(e.target.value)}
                    placeholder="金额 USD (如 5)"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="每周滚动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitWeek} onChange={setHasLimitWeek} aria-label="启用每周限额" />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitWeek ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitWeek && (
                  <Input
                    type="text"
                    value={limitWeek}
                    onChange={(e) => setLimitWeek(e.target.value)}
                    placeholder="金额 USD (如 20)"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="每月滚动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitMonth} onChange={setHasLimitMonth} aria-label="启用每月限额" />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitMonth ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitMonth && (
                  <Input
                    type="text"
                    value={limitMonth}
                    onChange={(e) => setLimitMonth(e.target.value)}
                    placeholder="金额 USD (如 80)"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="累计总额度限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitTotal} onChange={setHasLimitTotal} aria-label="启用总额度限额" />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitTotal ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitTotal && (
                  <Input
                    type="text"
                    value={limitTotal}
                    onChange={(e) => setLimitTotal(e.target.value)}
                    placeholder="金额 USD (如 200)"
                  />
                )}
              </Field>
            </div>
          </div>
        </div>
      </form>
    </Modal>
  );
}

/* ==========================================================================
   EditKeyModal
   ========================================================================== */

interface EditKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

function EditKeyModal({ keyData, api, onClose, onSuccess }: EditKeyModalProps) {
  const [label, setLabel] = useState<string>(keyData.label || '');
  const [enabled, setEnabled] = useState<boolean>(keyData.enabled);

  const [hasExpiry, setHasExpiry] = useState<boolean>(!!keyData.expires_at);
  const [expiryInput, setExpiryInput] = useState<string>(() =>
    toLocalDatetimeInput(keyData.expires_at)
  );

  const [hasMaxConcurrent, setHasMaxConcurrent] = useState<boolean>(keyData.max_concurrent > 0);
  const [maxConcurrentInput, setMaxConcurrentInput] = useState<string>(
    keyData.max_concurrent > 0 ? String(keyData.max_concurrent) : '5'
  );

  const [isRestrictedModels, setIsRestrictedModels] = useState<boolean>(
    keyData.allowed_models.length > 0
  );
  const [allowedModels, setAllowedModels] = useState<string[]>(keyData.allowed_models || []);
  const [modelError, setModelError] = useState<string>('');

  // 4 Quota limits
  const [hasLimit5H, setHasLimit5H] = useState<boolean>(keyData.limits.five_hour > 0);
  const [limit5H, setLimit5H] = useState<string>(
    keyData.limits.five_hour > 0 ? formatUSD(keyData.limits.five_hour) : ''
  );

  const [hasLimitWeek, setHasLimitWeek] = useState<boolean>(keyData.limits.week > 0);
  const [limitWeek, setLimitWeek] = useState<string>(
    keyData.limits.week > 0 ? formatUSD(keyData.limits.week) : ''
  );

  const [hasLimitMonth, setHasLimitMonth] = useState<boolean>(keyData.limits.month > 0);
  const [limitMonth, setLimitMonth] = useState<string>(
    keyData.limits.month > 0 ? formatUSD(keyData.limits.month) : ''
  );

  const [hasLimitTotal, setHasLimitTotal] = useState<boolean>(keyData.limits.total > 0);
  const [limitTotal, setLimitTotal] = useState<string>(
    keyData.limits.total > 0 ? formatUSD(keyData.limits.total) : ''
  );

  const [submitting, setSubmitting] = useState<boolean>(false);
  const [formError, setFormError] = useState<string>('');

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError('');
    setModelError('');

    if (!label.trim()) {
      setFormError('Key 名称不能为空');
      return;
    }

    if (isRestrictedModels && allowedModels.length === 0) {
      setModelError('开启限制模型访问时，必须至少选择或添加一个模型');
      return;
    }

    let parsed5H = 0;
    let parsedWeek = 0;
    let parsedMonth = 0;
    let parsedTotal = 0;

    try {
      if (hasLimit5H && limit5H) parsed5H = parseUSD(limit5H);
      if (hasLimitWeek && limitWeek) parsedWeek = parseUSD(limitWeek);
      if (hasLimitMonth && limitMonth) parsedMonth = parseUSD(limitMonth);
      if (hasLimitTotal && limitTotal) parsedTotal = parseUSD(limitTotal);
    } catch (err: unknown) {
      setFormError(errorMessage(err));
      return;
    }

    let parsedConcurrent = 0;
    if (hasMaxConcurrent) {
      parsedConcurrent = parseInt(maxConcurrentInput, 10);
      if (isNaN(parsedConcurrent) || parsedConcurrent < 1) {
        setFormError('最大并发数必须为大于等于 1 的整数');
        return;
      }
    }

    // Preserve exact RFC3339 nanoseconds if unchanged!
    let preservedExpiry: string | null = null;
    if (hasExpiry && expiryInput) {
      preservedExpiry = preserveOriginalExpiry(keyData.expires_at, expiryInput);
    }

    const limits: Limits = {
      five_hour: parsed5H,
      week: parsedWeek,
      month: parsedMonth,
      total: parsedTotal,
    };

    const spec: KeySpec = {
      label: label.trim(),
      enabled,
      expires_at: preservedExpiry,
      max_concurrent: parsedConcurrent,
      allowed_models: isRestrictedModels ? allowedModels : [],
      limits,
    };

    setSubmitting(true);
    try {
      await api.request(`/keys?id=${encodeURIComponent(keyData.id)}`, {
        method: 'PUT',
        body: spec,
      });
      onSuccess();
    } catch (err: unknown) {
      setFormError(errorMessage(err) || '更新 Key 失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={`编辑 Key: ${keyData.label}`}
      onClose={onClose}
      size="lg"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="primary" onClick={handleSubmit} loading={submitting}>
            保存修改
          </Button>
        </div>
      }
    >
      <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
        {formError && <Notice tone="danger">{formError}</Notice>}

        {/* Basic Info */}
        <div className="cpa-card">
          <Field label="Key 名称" htmlFor="edit-label">
            <Input
              id="edit-label"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              required
            />
          </Field>

          <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem', marginTop: '0.5rem' }}>
            <Switch checked={enabled} onChange={setEnabled} aria-label="启用该 Key" />
            <span style={{ fontSize: '0.88rem' }}>{enabled ? '已启用该 Key' : '已禁用该 Key'}</span>
          </div>
        </div>

        {/* Life & Concurrent */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>生命周期与并发</h3>
          <div className="cpa-form-grid cpa-form-grid-2">
            <div>
              <Field label="有效期">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch checked={hasExpiry} onChange={setHasExpiry} aria-label="指定有效期" />
                  <span style={{ fontSize: '0.88rem' }}>{hasExpiry ? '指定到期时间' : '永久有效'}</span>
                </div>
                {hasExpiry && (
                  <Input
                    type="datetime-local"
                    value={expiryInput}
                    onChange={(e) => setExpiryInput(e.target.value)}
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="并发限制">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.5rem' }}>
                  <Switch checked={hasMaxConcurrent} onChange={setHasMaxConcurrent} aria-label="限制并发数" />
                  <span style={{ fontSize: '0.88rem' }}>{hasMaxConcurrent ? '限制最大并发' : '不限并发'}</span>
                </div>
                {hasMaxConcurrent && (
                  <Input
                    type="number"
                    min={1}
                    value={maxConcurrentInput}
                    onChange={(e) => setMaxConcurrentInput(e.target.value)}
                  />
                )}
              </Field>
            </div>
          </div>
        </div>

        {/* Models */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>模型权限</h3>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '1rem' }}>
            <Switch checked={isRestrictedModels} onChange={setIsRestrictedModels} aria-label="限制访问指定模型" />
            <span style={{ fontSize: '0.88rem' }}>
              {isRestrictedModels ? '限制访问指定模型列表' : '允许访问全部模型 (不限)'}
            </span>
          </div>

          {isRestrictedModels && (
            <ModelPicker
              api={api}
              selectedModels={allowedModels}
              onChange={setAllowedModels}
              error={modelError}
            />
          )}
        </div>

        {/* Quotas */}
        <div className="cpa-card">
          <h3 style={{ margin: '0 0 1rem', fontSize: '1rem', fontWeight: 600 }}>额度上限 (USD)</h3>
          <div className="cpa-form-grid cpa-form-grid-2">
            <div>
              <Field label="5 小时滑动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimit5H} onChange={setHasLimit5H} />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimit5H ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimit5H && (
                  <Input
                    type="text"
                    value={limit5H}
                    onChange={(e) => setLimit5H(e.target.value)}
                    placeholder="USD 金额"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="每周滚动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitWeek} onChange={setHasLimitWeek} />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitWeek ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitWeek && (
                  <Input
                    type="text"
                    value={limitWeek}
                    onChange={(e) => setLimitWeek(e.target.value)}
                    placeholder="USD 金额"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="每月滚动窗口限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitMonth} onChange={setHasLimitMonth} />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitMonth ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitMonth && (
                  <Input
                    type="text"
                    value={limitMonth}
                    onChange={(e) => setLimitMonth(e.target.value)}
                    placeholder="USD 金额"
                  />
                )}
              </Field>
            </div>

            <div>
              <Field label="累计总额度限额">
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.4rem' }}>
                  <Switch checked={hasLimitTotal} onChange={setHasLimitTotal} />
                  <span style={{ fontSize: '0.85rem' }}>{hasLimitTotal ? '已设限额' : '不限额度'}</span>
                </div>
                {hasLimitTotal && (
                  <Input
                    type="text"
                    value={limitTotal}
                    onChange={(e) => setLimitTotal(e.target.value)}
                    placeholder="USD 金额"
                  />
                )}
              </Field>
            </div>
          </div>
        </div>
      </form>
    </Modal>
  );
}

/* ==========================================================================
   RotateKeyModal
   ========================================================================== */

interface RotateKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: (data: RevealedSecretState) => void;
}

function RotateKeyModal({ keyData, api, onClose, onSuccess }: RotateKeyModalProps) {
  const [rotateType, setRotateType] = useState<'auto' | 'custom'>('auto');
  const [customSecret, setCustomSecret] = useState<string>('');
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

  const customSecretTrimmed = customSecret.trim();
  const isWeakCustom =
    rotateType === 'custom' &&
    customSecretTrimmed.length >= 8 &&
    customSecretTrimmed.length < 32;

  const handleRotate = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');

    if (rotateType === 'custom') {
      if (customSecretTrimmed.length < 8 || customSecretTrimmed.length > 256) {
        setError('自定义新密钥长度必须在 8 到 256 位之间');
        return;
      }
      if (!/^[\x21-\x7E]+$/.test(customSecretTrimmed)) {
        setError('自定义新密钥只能包含可见 ASCII 字符且不能有空格或换行');
        return;
      }
    }

    setSubmitting(true);
    try {
      const body = rotateType === 'custom' ? { secret: customSecretTrimmed } : {};
      const res = await api.request<{ key: Key; secret: string }>(
        `/keys/rotate?id=${encodeURIComponent(keyData.id)}`,
        {
          method: 'POST',
          body,
        }
      );

      onSuccess({
        secret: res.secret,
        label: keyData.label,
        action: 'rotate',
      });
    } catch (err: unknown) {
      const msg = errorMessage(err);
      if (msg.includes('409') || msg.toLowerCase().includes('conflict')) {
        setError('该密钥已存在冲突，请使用唯一的自定义新密钥');
      } else {
        setError(msg || '轮转密钥失败');
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={`轮转密钥: ${keyData.label}`}
      onClose={onClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="primary" onClick={handleRotate} loading={submitting}>
            确认轮转
          </Button>
        </div>
      }
    >
      <form onSubmit={handleRotate} style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {error && <Notice tone="danger">{error}</Notice>}

        <Notice tone="info">
          轮转成功后，原密钥将立即失效且不可找回。Key 的 ID、历史用量、重置窗口与配额封锁状态将完全保留。
        </Notice>

        <Field label="新密钥生成方式">
          <div className="cpa-radio-group" style={{ marginBottom: '0.75rem' }}>
            <label className="cpa-radio-label">
              <input
                type="radio"
                name="rotateType"
                value="auto"
                checked={rotateType === 'auto'}
                onChange={() => setRotateType('auto')}
              />
              自动生成高强度新密钥 (推荐 32+ 位)
            </label>
            <label className="cpa-radio-label">
              <input
                type="radio"
                name="rotateType"
                value="custom"
                checked={rotateType === 'custom'}
                onChange={() => setRotateType('custom')}
              />
              自定义新密钥
            </label>
          </div>

          {rotateType === 'custom' && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
              <Input
                type="text"
                value={customSecret}
                onChange={(e) => setCustomSecret(e.target.value)}
                placeholder="请输入 8-256 位新密钥（无空格）"
                autoComplete="off"
              />
              {isWeakCustom && (
                <Notice tone="warning">
                  密钥长度少于 32 位（当前 {customSecretTrimmed.length} 位），建议使用 32 位以上高强度随机密钥。
                </Notice>
              )}
            </div>
          )}
        </Field>
      </form>
    </Modal>
  );
}

/* ==========================================================================
   ResetKeyModal
   ========================================================================== */

interface ResetKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

function ResetKeyModal({ keyData, api, onClose, onSuccess }: ResetKeyModalProps) {
  const [windows, setWindows] = useState<{ [key: string]: boolean }>({
    five_hour: true,
    week: true,
    month: true,
    total: true,
  });
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

  const toggleWindow = (key: string) => {
    setWindows((prev) => ({ ...prev, [key]: !prev[key] }));
  };

  const handleReset = async (e: React.FormEvent) => {
    e.preventDefault();
    const selected = Object.keys(windows).filter((k) => windows[k]);
    if (selected.length === 0) {
      setError('必须至少选择一个重置窗口');
      return;
    }

    setSubmitting(true);
    setError('');
    try {
      await api.request(`/keys/reset?id=${encodeURIComponent(keyData.id)}`, {
        method: 'POST',
        body: { windows: selected },
      });
      onSuccess();
    } catch (err: unknown) {
      setError(errorMessage(err) || '重置额度用量失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={`重置额度用量: ${keyData.label}`}
      onClose={onClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="danger" onClick={handleReset} loading={submitting}>
            确认重置
          </Button>
        </div>
      }
    >
      <form onSubmit={handleReset} style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {error && <Notice tone="danger">{error}</Notice>}

        <Notice tone="warning">
          重置将把所选周期的已用金额归零，并解除该周期的配额封锁。历史请求记录与实际已发生的成本不会被删除。
        </Notice>

        <Field label="选择要重置的额度周期">
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.6rem' }}>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.five_hour}
                onChange={() => toggleWindow('five_hour')}
              />
              5 小时滑动窗口 (当前已用: ${formatUSD(keyData.used.five_hour)})
            </label>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.week}
                onChange={() => toggleWindow('week')}
              />
              每周滚动窗口 (当前已用: ${formatUSD(keyData.used.week)})
            </label>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.month}
                onChange={() => toggleWindow('month')}
              />
              每月滚动窗口 (当前已用: ${formatUSD(keyData.used.month)})
            </label>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.total}
                onChange={() => toggleWindow('total')}
              />
              累计总额度 (当前已用: ${formatUSD(keyData.used.total)})
            </label>
          </div>
        </Field>
      </form>
    </Modal>
  );
}

/* ==========================================================================
   RevokeKeyModal (Disable / Enable)
   ========================================================================== */

interface RevokeKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

function RevokeKeyModal({ keyData, api, onClose, onSuccess }: RevokeKeyModalProps) {
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

  const isDisabling = keyData.enabled;

  const handleAction = async () => {
    setSubmitting(true);
    setError('');
    try {
      if (isDisabling) {
        // DELETE /keys?id=...
        await api.request(`/keys?id=${encodeURIComponent(keyData.id)}`, {
          method: 'DELETE',
        });
      } else {
        // PUT /keys?id=... with enabled: true
        const spec: KeySpec = {
          label: keyData.label,
          enabled: true,
          expires_at: keyData.expires_at,
          max_concurrent: keyData.max_concurrent,
          allowed_models: keyData.allowed_models,
          limits: keyData.limits,
        };
        await api.request(`/keys?id=${encodeURIComponent(keyData.id)}`, {
          method: 'PUT',
          body: spec,
        });
      }
      onSuccess();
    } catch (err: unknown) {
      setError(errorMessage(err) || `${isDisabling ? '禁用' : '启用'} Key 失败`);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={isDisabling ? `禁用 Key: ${keyData.label}` : `重新启用 Key: ${keyData.label}`}
      onClose={onClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button
            variant={isDisabling ? 'danger' : 'primary'}
            onClick={handleAction}
            loading={submitting}
          >
            {isDisabling ? '确认禁用' : '确认启用'}
          </Button>
        </div>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {error && <Notice tone="danger">{error}</Notice>}

        {isDisabling ? (
          <Notice tone="warning">
            禁用后，客户端使用该 Key 发起的所有请求将被立即拒绝。该 Key 的历史记录与用量将完整保留，之后可随时重新启用。
          </Notice>
        ) : (
          <Notice tone="info">
            重新启用后，该 Key 将恢复访问能力。
          </Notice>
        )}
      </div>
    </Modal>
  );
}

/* ==========================================================================
   RecoverKeyModal (Unblock accounting failure)
   ========================================================================== */

interface RecoverKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

function RecoverKeyModal({ keyData, api, onClose, onSuccess }: RecoverKeyModalProps) {
  const [acknowledged, setAcknowledged] = useState<boolean>(false);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

  const handleRecover = async () => {
    if (!acknowledged) {
      setError('必须明确勾选确认并承担责任后方可解除封锁');
      return;
    }

    setSubmitting(true);
    setError('');
    try {
      await api.request(`/keys/recover?id=${encodeURIComponent(keyData.id)}`, {
        method: 'POST',
        body: { acknowledge: true },
      });
      onSuccess();
    } catch (err: unknown) {
      setError(errorMessage(err) || '恢复 Key 失败');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={`核对并恢复 Key: ${keyData.label}`}
      onClose={onClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            取消
          </Button>
          <Button
            variant="danger"
            onClick={handleRecover}
            disabled={!acknowledged || submitting}
            loading={submitting}
          >
            确认核对后恢复
          </Button>
        </div>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {error && <Notice tone="danger">{error}</Notice>}

        <Notice tone="warning">
          该 Key 因底层计费写入或存储异常已被系统暂停。系统会自动重试保留的计费事件；若服务异常期间有未知或丢失用量，管理员须核对处理后确认承担责任。此操作仅解除封锁，不会修改已用金额或历史费用。
        </Notice>

        <label className="cpa-radio-label" style={{ alignItems: 'flex-start', marginTop: '0.5rem' }}>
          <input
            type="checkbox"
            checked={acknowledged}
            onChange={(e) => setAcknowledged(e.target.checked)}
            style={{ marginTop: '0.2rem' }}
          />
          <span>我已核对并处理缺失用量，确认存储正常，承担未知用量核对责任</span>
        </label>
      </div>
    </Modal>
  );
}

/* ==========================================================================
   DetailsModal
   ========================================================================== */

interface DetailsModalProps {
  keyData: Key;
  onClose: () => void;
  onCopy: (text: string, id: string) => void;
  copiedId: string | null;
}

function DetailsModal({ keyData, onClose, onCopy, copiedId }: DetailsModalProps) {
  return (
    <Modal
      title={`Key 详情: ${keyData.label}`}
      onClose={onClose}
      size="md"
      footer={
        <Button variant="secondary" onClick={onClose}>
          关闭
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem', fontSize: '0.9rem' }}>
        <div>
          <span style={{ color: 'var(--text-secondary)' }}>完整 Key ID:</span>
          <div className="cpa-secret-box" style={{ marginTop: '0.35rem' }}>
            <span style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem' }}>{keyData.id}</span>
            <Button
              variant="secondary"
              style={{ padding: '0.25rem 0.6rem', minHeight: 'auto', fontSize: '0.8rem' }}
              onClick={() => onCopy(keyData.id, 'details-id')}
            >
              {copiedId === 'details-id' ? '已复制' : '复制'}
            </Button>
          </div>
        </div>

        <div className="cpa-form-grid cpa-form-grid-2">
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>创建时间:</span>
            <div>{formatDateTime(keyData.created_at)}</div>
          </div>
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>有效期:</span>
            <div>{keyData.expires_at ? formatDateTime(keyData.expires_at) : '永久有效'}</div>
          </div>
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>当前活跃请求:</span>
            <div>{keyData.active}</div>
          </div>
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>最大并发连接:</span>
            <div>{keyData.max_concurrent === 0 ? '不限并发' : keyData.max_concurrent}</div>
          </div>
        </div>

        <div>
          <span style={{ color: 'var(--text-secondary)' }}>允许访问的模型:</span>
          <div style={{ marginTop: '0.35rem' }}>
            {keyData.allowed_models.length === 0 ? (
              <span>全部模型 (不限)</span>
            ) : (
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: '0.35rem' }}>
                {keyData.allowed_models.map((m) => (
                  <Badge key={m} tone="neutral">
                    {m}
                  </Badge>
                ))}
              </div>
            )}
          </div>
        </div>

        <div>
          <span style={{ color: 'var(--text-secondary)' }}>重置窗口时间 (UTC):</span>
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: 'auto 1fr',
              gap: '0.2rem 1rem',
              fontSize: '0.85rem',
              marginTop: '0.35rem',
            }}
          >
            <span>5 小时开始:</span>
            <span>{formatDateTime(keyData.starts.five_hour)}</span>
            <span>每周开始:</span>
            <span>{formatDateTime(keyData.starts.week)}</span>
            <span>每月开始:</span>
            <span>{formatDateTime(keyData.starts.month)}</span>
            <span>总计开始:</span>
            <span>{formatDateTime(keyData.starts.total)}</span>
          </div>
        </div>
      </div>
    </Modal>
  );
}

/* ==========================================================================
   SecretRevealedModal
   ========================================================================== */

interface SecretRevealedModalProps {
  data: RevealedSecretState;
  onClose: () => void;
  onCopy: (text: string, id: string) => void;
  copiedId: string | null;
}

function SecretRevealedModal({ data, onClose, onCopy, copiedId }: SecretRevealedModalProps) {
  return (
    <Modal
      title={data.action === 'create' ? 'Key 创建成功' : 'Key 轮转成功'}
      onClose={onClose}
      size="md"
      footer={
        <Button variant="primary" onClick={onClose}>
          我已安全保存密钥，关闭
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        <Notice tone="warning">
          <strong>重要安全提示：</strong>
          客户端明文密钥仅在本次关闭前展示一次，关闭后将无法再次查看或找回！请立即妥善保存。
        </Notice>

        <div>
          <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
            客户端密钥 ({data.label}):
          </span>
          <div className="cpa-secret-box">
            <span style={{ fontFamily: 'var(--font-mono)', fontSize: '0.95rem', fontWeight: 600 }}>
              {data.secret}
            </span>
            <Button
              variant="primary"
              style={{ padding: '0.35rem 0.8rem', minHeight: 'auto' }}
              onClick={() => onCopy(data.secret, 'revealed-secret')}
            >
              {copiedId === 'revealed-secret' ? '已复制' : '复制密钥'}
            </Button>
          </div>
        </div>

        <p style={{ fontSize: '0.82rem', color: 'var(--text-tertiary)', margin: 0 }}>
          系统绝不会在浏览器 Storage、Cookie、日志或 URL 中保留明文密钥。
        </p>
      </div>
    </Modal>
  );
}

export default KeysPage;
