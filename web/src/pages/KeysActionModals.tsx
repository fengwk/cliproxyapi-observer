import { useState, useRef, useEffect, useCallback } from 'react';
import type { FormEvent } from 'react';
import type { ApiClient } from '../lib/api';
import type { Key } from '../lib/types';
import { formatUSD, formatDateTime, errorMessage } from '../lib/format';
import { Button, Input, Field, Modal, Badge, Notice } from '../components/ui';
import { validateCustomSecret } from './KeysHelpers';
import type { RevealedSecretState } from './KeysEditor';

/* ==========================================================================
   RotateKeyModal
   ========================================================================== */

export interface RotateKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: (data: RevealedSecretState) => void;
}

export function RotateKeyModal({ keyData, api, onClose, onSuccess }: RotateKeyModalProps) {
  const [rotateType, setRotateType] = useState<'auto' | 'custom'>('auto');
  const [customSecret, setCustomSecret] = useState<string>('');
  const [showCustomSecret, setShowCustomSecret] = useState<boolean>(false);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

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

  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  const isWeakCustom =
    rotateType === 'custom' &&
    customSecret.length >= 8 &&
    customSecret.length < 32;

  const handleRotate = async (e: FormEvent) => {
    e.preventDefault();
    if (isSubmittingRef.current) {
      return;
    }

    setError('');

    if (rotateType === 'custom') {
      const secRes = validateCustomSecret(customSecret);
      if (!secRes.valid) {
        setError(secRes.error || '自定义新密钥不合法');
        return;
      }
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    abortControllerRef.current = new AbortController();

    try {
      const body = rotateType === 'custom' ? { secret: customSecret } : {};
      const res = await api.request<{ key: Key; secret: string }>(
        `/keys/rotate?id=${encodeURIComponent(keyData.id)}`,
        {
          method: 'POST',
          body,
          signal: abortControllerRef.current.signal,
        }
      );

      if (mountedRef.current) {
        onSuccess({
          secret: res.secret,
          label: keyData.label,
          action: 'rotate',
        });
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
        setError('该密钥已存在冲突，请使用唯一的自定义新密钥');
      } else {
        setError(msg || '轮转密钥失败');
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
      title={`轮转密钥: ${keyData.label}`}
      onClose={handleSafeClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
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
              <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                <Input
                  id="rotate-custom-secret"
                  type={showCustomSecret ? 'text' : 'password'}
                  value={customSecret}
                  onChange={(e) => setCustomSecret(e.target.value)}
                  placeholder="请输入 8-256 位新密钥（无空格）"
                  autoComplete="off"
                  aria-label="自定义新密钥"
                />
                <Button
                  variant="secondary"
                  onClick={() => setShowCustomSecret((prev) => !prev)}
                  aria-label={showCustomSecret ? '隐藏自定义新密钥' : '显示自定义新密钥'}
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

export interface ResetKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

export function ResetKeyModal({ keyData, api, onClose, onSuccess }: ResetKeyModalProps) {
  const [windows, setWindows] = useState<{ [key: string]: boolean }>({
    five_hour: true,
    week: true,
    month: true,
    total: true,
  });
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

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

  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  const toggleWindow = (key: string) => {
    setWindows((prev) => ({ ...prev, [key]: !prev[key] }));
  };

  const handleReset = async (e: FormEvent) => {
    e.preventDefault();
    if (isSubmittingRef.current) {
      return;
    }

    const selected = Object.keys(windows).filter((k) => windows[k]);
    if (selected.length === 0) {
      setError('必须至少选择一个重置窗口');
      return;
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    setError('');
    abortControllerRef.current = new AbortController();

    try {
      await api.request(`/keys/reset?id=${encodeURIComponent(keyData.id)}`, {
        method: 'POST',
        body: { windows: selected },
        signal: abortControllerRef.current.signal,
      });
      if (mountedRef.current) {
        onSuccess();
      }
    } catch (err: unknown) {
      if (abortControllerRef.current?.signal.aborted) {
        return;
      }
      if (!mountedRef.current) {
        return;
      }
      setError(errorMessage(err) || '重置额度用量失败');
    } finally {
      if (mountedRef.current) {
        setSubmitting(false);
      }
      isSubmittingRef.current = false;
    }
  };

  return (
    <Modal
      title={`重置额度用量: ${keyData.label}`}
      onClose={handleSafeClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
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
              5 小时周期 (当前已用: ${formatUSD(keyData.used.five_hour)})
            </label>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.week}
                onChange={() => toggleWindow('week')}
              />
              每周周期 (当前已用: ${formatUSD(keyData.used.week)})
            </label>
            <label className="cpa-radio-label">
              <input
                type="checkbox"
                checked={windows.month}
                onChange={() => toggleWindow('month')}
              />
              每月周期 (当前已用: ${formatUSD(keyData.used.month)})
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

export interface RevokeKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

export function RevokeKeyModal({ keyData, api, onClose, onSuccess }: RevokeKeyModalProps) {
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

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

  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  const isDisabling = keyData.enabled;

  const handleAction = async () => {
    if (isSubmittingRef.current) {
      return;
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    setError('');
    abortControllerRef.current = new AbortController();

    try {
      // Disable via KeySpec, not DELETE: revocation destroys the hash and
      // cannot honestly be presented as a reversible on/off switch.
      await api.request(`/keys?id=${encodeURIComponent(keyData.id)}`, {
        method: 'PUT',
        body: {
          label: keyData.label,
          enabled: !isDisabling,
          expires_at: keyData.expires_at,
          max_concurrent: keyData.max_concurrent,
          allowed_models: keyData.allowed_models,
          limits: keyData.limits,
        },
        signal: abortControllerRef.current.signal,
      });
      if (mountedRef.current) {
        onSuccess();
      }
    } catch (err: unknown) {
      if (abortControllerRef.current?.signal.aborted) {
        return;
      }
      if (!mountedRef.current) {
        return;
      }
      setError(errorMessage(err) || `${isDisabling ? '禁用' : '启用'} Key 失败`);
    } finally {
      if (mountedRef.current) {
        setSubmitting(false);
      }
      isSubmittingRef.current = false;
    }
  };

  return (
    <Modal
      title={isDisabling ? `禁用 Key: ${keyData.label}` : `重新启用 Key: ${keyData.label}`}
      onClose={handleSafeClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
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
            禁用后将拒绝该 Key 的新请求，已受理的请求不受影响。密钥、历史记录与用量将完整保留，之后可重新启用。
          </Notice>
        ) : (
          <Notice tone="info">重新启用后，新请求仍须满足有效期、模型权限和额度等限制。</Notice>
        )}
      </div>
    </Modal>
  );
}

/* ==========================================================================
   RecoverKeyModal (Unblock accounting failure)
   ========================================================================== */

export interface RecoverKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

export function RecoverKeyModal({ keyData, api, onClose, onSuccess }: RecoverKeyModalProps) {
  const [acknowledged, setAcknowledged] = useState<boolean>(false);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

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

  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  const handleRecover = async () => {
    if (isSubmittingRef.current) {
      return;
    }

    if (!acknowledged) {
      setError('必须明确勾选确认并承担责任后方可解除封锁');
      return;
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    setError('');
    abortControllerRef.current = new AbortController();

    try {
      await api.request(`/keys/recover?id=${encodeURIComponent(keyData.id)}`, {
        method: 'POST',
        body: { acknowledge: true },
        signal: abortControllerRef.current.signal,
      });
      if (mountedRef.current) {
        onSuccess();
      }
    } catch (err: unknown) {
      if (abortControllerRef.current?.signal.aborted) {
        return;
      }
      if (!mountedRef.current) {
        return;
      }
      setError(errorMessage(err) || '恢复 Key 失败');
    } finally {
      if (mountedRef.current) {
        setSubmitting(false);
      }
      isSubmittingRef.current = false;
    }
  };

  return (
    <Modal
      title={`核对并恢复 Key: ${keyData.label}`}
      onClose={handleSafeClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
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

export interface DetailsModalProps {
  keyData: Key;
  onClose: () => void;
  onCopy: (text: string, id: string) => void;
  copiedId: string | null;
  copyError?: string | null;
}

export function DetailsModal({ keyData, onClose, onCopy, copiedId, copyError }: DetailsModalProps) {
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
        {copyError && <Notice tone="danger">{copyError}</Notice>}

        <div>
          <span style={{ color: 'var(--text-secondary)' }}>完整内部 ID（用于记账关联，不可用于 API 认证）:</span>
          <div className="cpa-secret-box" style={{ marginTop: '0.35rem' }}>
            <span style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem' }}>{keyData.id}</span>
            <Button
              variant="secondary"
              style={{ padding: '0.25rem 0.6rem', minHeight: 'auto', fontSize: '0.8rem' }}
              onClick={() => onCopy(keyData.id, 'details-id')}
            >
              {copiedId === 'details-id' ? '已复制 ID' : '复制内部 ID'}
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
          <span style={{ color: 'var(--text-secondary)' }}>额度周期起始时间 (本地时间):</span>
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: 'auto 1fr',
              gap: '0.2rem 1rem',
              fontSize: '0.85rem',
              marginTop: '0.35rem',
            }}
          >
            <span>5小时额度:</span>
            <span>{formatDateTime(keyData.starts.five_hour)}</span>
            <span>每周额度:</span>
            <span>{formatDateTime(keyData.starts.week)}</span>
            <span>每月额度:</span>
            <span>{formatDateTime(keyData.starts.month)}</span>
            <span>累计总额度:</span>
            <span>{formatDateTime(keyData.starts.total)}</span>
          </div>
        </div>
      </div>
    </Modal>
  );
}

/* ==========================================================================
   DeleteKeyModal
   ========================================================================== */

export interface DeleteKeyModalProps {
  keyData: Key;
  api: ApiClient;
  onClose: () => void;
  onSuccess: () => void;
}

export function DeleteKeyModal({ keyData, api, onClose, onSuccess }: DeleteKeyModalProps) {
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [error, setError] = useState<string>('');

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

  const handleSafeClose = useCallback(() => {
    if (isSubmittingRef.current || submitting) {
      return;
    }
    onClose();
  }, [submitting, onClose]);

  const handleDelete = async () => {
    if (isSubmittingRef.current) {
      return;
    }

    isSubmittingRef.current = true;
    setSubmitting(true);
    setError('');
    abortControllerRef.current = new AbortController();

    try {
      // POST /keys/delete?id=... -> 204
      await api.request(`/keys/delete?id=${encodeURIComponent(keyData.id)}`, {
        method: 'POST',
        signal: abortControllerRef.current.signal,
      });

      if (mountedRef.current) {
        onSuccess();
      }
    } catch (err: unknown) {
      if (abortControllerRef.current?.signal.aborted) {
        return;
      }
      if (!mountedRef.current) {
        return;
      }
      const msg = errorMessage(err);
      if (
        msg.includes('busy') ||
        msg.includes('409') ||
        msg.includes('503') ||
        msg.includes('在处理') ||
        msg.includes('正在处理')
      ) {
        setError('当前仍有活跃请求或待记账操作正在处理中，暂无法删除，请稍后重试');
      } else {
        setError(msg || '删除 Key 失败');
      }
      // Per contract: pending/busy 错误不关闭弹窗
    } finally {
      if (mountedRef.current) {
        setSubmitting(false);
      }
      isSubmittingRef.current = false;
    }
  };

  return (
    <Modal
      title={`删除 Key: ${keyData.label || keyData.id}`}
      onClose={handleSafeClose}
      size="md"
      footer={
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <Button variant="ghost" onClick={handleSafeClose} disabled={submitting}>
            取消
          </Button>
          <Button variant="danger" onClick={handleDelete} loading={submitting}>
            确认永久删除
          </Button>
        </div>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
        {error && <Notice tone="danger">{error}</Notice>}

        <Notice tone="danger">
          <strong>终态操作警告：</strong>
          删除为终态操作，将清除该客户端 Key 的认证索引并永久终止其调用权限。删除后不可恢复，且无法再次启用或轮换密钥。
        </Notice>

        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', fontSize: '0.9rem' }}>
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>Key 名称：</span>
            <strong>{keyData.label || '(未命名)'}</strong>
          </div>
          <div>
            <span style={{ color: 'var(--text-secondary)' }}>内部 ID：</span>
            <span style={{ fontFamily: 'var(--font-mono)' }}>{keyData.id}</span>
          </div>
          <p style={{ margin: '0.5rem 0 0', fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
            该 Key 既往产生的用量、费用明细与统计记录将作为最小审计账目予以保留，原有历史数据与累计金额不受影响。
          </p>
        </div>
      </div>
    </Modal>
  );
}

/* ==========================================================================
   SecretRevealedModal
   ========================================================================== */

export interface SecretRevealedModalProps {
  data: RevealedSecretState;
  onClose: () => void;
  onCopy: (text: string, id: string) => void;
  copiedId: string | null;
  copyError?: string | null;
}

export function SecretRevealedModal({ data, onClose, onCopy, copiedId, copyError }: SecretRevealedModalProps) {
  const [hidden, setHidden] = useState(false);
  const secretSpanRef = useRef<HTMLSpanElement | null>(null);

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
        {copyError && <Notice tone="danger">{copyError}</Notice>}

        <Notice tone="warning">
          <strong>重要安全提示：</strong>
          客户端明文密钥仅在本次展示一次，关闭后将无法再次查看或找回！请立即妥善保存。关闭后立即清除内存。
        </Notice>

        <div>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.35rem' }}>
            <span style={{ fontSize: '0.85rem', color: 'var(--text-secondary)' }}>
              客户端密钥 ({data.label}):
            </span>
            <span style={{ fontSize: '0.8rem', color: 'var(--text-tertiary)' }}>
              格式为 sk-... (共 51 位)
            </span>
          </div>
          <div className="cpa-secret-box">
            <span
              ref={secretSpanRef}
              style={{
                fontFamily: 'var(--font-mono)',
                fontSize: '0.95rem',
                fontWeight: 600,
                userSelect: 'all',
                wordBreak: 'break-all',
              }}
              aria-label="客户端明文密钥"
            >
              {hidden ? '•••••••••••••••••••••••••••••••••••••••••••••••••••' : data.secret}
            </span>
            <div style={{ display: 'flex', gap: '0.35rem', flexShrink: 0 }}>
              <Button
                variant="ghost"
                style={{ padding: '0.3rem 0.6rem', minHeight: 'auto', fontSize: '0.8rem' }}
                onClick={() => setHidden(!hidden)}
              >
                {hidden ? '显示' : '隐藏'}
              </Button>
              <Button
                variant="ghost"
                style={{ padding: '0.3rem 0.6rem', minHeight: 'auto', fontSize: '0.8rem' }}
                onClick={() => {
                  if (secretSpanRef.current && typeof window !== 'undefined') {
                    const range = document.createRange();
                    range.selectNodeContents(secretSpanRef.current);
                    const sel = window.getSelection();
                    sel?.removeAllRanges();
                    sel?.addRange(range);
                  }
                }}
              >
                全选文本
              </Button>
              <Button
                variant="primary"
                style={{ padding: '0.35rem 0.8rem', minHeight: 'auto' }}
                onClick={() => onCopy(data.secret, 'revealed-secret')}
              >
                {copiedId === 'revealed-secret' ? '已复制' : '复制密钥'}
              </Button>
            </div>
          </div>
        </div>

        <p style={{ fontSize: '0.82rem', color: 'var(--text-tertiary)', margin: 0 }}>
          系统绝不会在浏览器 Storage、Cookie、日志或 URL 中保留明文密钥。
        </p>
      </div>
    </Modal>
  );
}
