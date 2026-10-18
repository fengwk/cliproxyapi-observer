import { useState, useCallback, useEffect, useRef } from 'react';
import type { PageProps } from '../lib/api';
import type { Key } from '../lib/types';
import { formatUSD, formatDateTime, errorMessage } from '../lib/format';
import { usePoll } from '../lib/usePoll';
import { Button, Badge, Notice, EmptyState } from '../components/ui';
import { copyToClipboard, maskId } from './KeysHelpers';
import { KeyEditor, type RevealedSecretState } from './KeysEditor';
import {
  RotateKeyModal,
  ResetKeyModal,
  RevokeKeyModal,
  RecoverKeyModal,
  DetailsModal,
  SecretRevealedModal,
} from './KeysActionModals';

export interface KeysPageProps extends PageProps {}

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
  const [copyError, setCopyError] = useState<string | null>(null);

  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const loadGenRef = useRef<number>(0);
  const loadAbortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef<boolean>(true);

  // Sync dirty state: editing/action modals make page dirty; cleanup on unmount
  useEffect(() => {
    const isDirty =
      activeModal !== null &&
      activeModal.type !== 'revealed' &&
      activeModal.type !== 'details';
    onDirtyChange(isDirty);

    return () => {
      onDirtyChange(false);
    };
  }, [activeModal, onDirtyChange]);

  // Load keys with generation guard and abort controller
  const loadKeys = useCallback(
    async (signal?: AbortSignal) => {
      if (loadAbortRef.current) {
        loadAbortRef.current.abort();
      }
      const ac = new AbortController();
      loadAbortRef.current = ac;
      const curGen = ++loadGenRef.current;

      setLoading(true);
      try {
        const response = await api.request<{ items: Key[] }>('/keys', {
          signal: signal || ac.signal,
        });
        if (!mountedRef.current || curGen !== loadGenRef.current) {
          return;
        }
        if (response && Array.isArray(response.items)) {
          setKeys(response.items);
          setPageError('');
        }
      } catch (err: unknown) {
        if (ac.signal.aborted || signal?.aborted) {
          return;
        }
        if (!mountedRef.current || curGen !== loadGenRef.current) {
          return;
        }
        const msg = errorMessage(err);
        setPageError(msg || '加载 Key 列表失败');
      } finally {
        if (mountedRef.current && curGen === loadGenRef.current) {
          setLoading(false);
        }
      }
    },
    [api]
  );

  useEffect(() => {
    mountedRef.current = true;
    loadKeys();

    return () => {
      mountedRef.current = false;
      if (loadAbortRef.current) {
        loadAbortRef.current.abort();
      }
      if (copyTimerRef.current) {
        clearTimeout(copyTimerRef.current);
      }
    };
  }, [loadKeys]);

  // All modals (including details & revealed secret) pause polling
  const isPaused = activeModal !== null;
  usePoll(loadKeys, refreshInterval, isPaused);

  // Robust clipboard copy with timer cleanup and visible failure
  const handleCopy = async (text: string, id: string) => {
    if (copyTimerRef.current) {
      clearTimeout(copyTimerRef.current);
      copyTimerRef.current = null;
    }
    setCopyError(null);

    const ok = await copyToClipboard(text);
    if (ok) {
      setCopiedId(id);
      copyTimerRef.current = setTimeout(() => {
        setCopiedId(null);
      }, 2000);
    } else {
      setCopyError('复制失败，请手动选中文本复制');
      copyTimerRef.current = setTimeout(() => {
        setCopyError(null);
      }, 3000);
    }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
      {/* Top action header */}
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          flexWrap: 'wrap',
          gap: '0.75rem',
        }}
      >
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
      {copyError && <Notice tone="danger">{copyError}</Notice>}

      {/* Loading state when first load */}
      {loading && keys.length === 0 ? (
        <div style={{ padding: '3rem 1.5rem', textAlign: 'center', color: 'var(--text-secondary)' }}>
          <span className="cpa-spinner" style={{ marginRight: '0.5rem', display: 'inline-block' }} aria-hidden="true" />
          正在加载客户端 Key 列表...
        </div>
      ) : keys.length === 0 && !pageError ? (
        <EmptyState
          title="暂无任何 Key"
          description="尚未创建任何客户端 Key。点击下方按钮立即创建第一个 Key。"
          action={
            <Button variant="primary" onClick={() => setActiveModal({ type: 'create' })}>
              新建第一个 Key
            </Button>
          }
        />
      ) : keys.length > 0 ? (
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
      ) : null}

      {/* ------------------------------------------------------------- MODALS */}

      {/* Create Key Modal */}
      {activeModal?.type === 'create' && (
        <KeyEditor
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={(revealed) => {
            if (revealed) {
              setActiveModal({ type: 'revealed', data: revealed });
            } else {
              setActiveModal(null);
            }
            loadKeys();
          }}
        />
      )}

      {/* Edit Key Modal */}
      {activeModal?.type === 'edit' && (
        <KeyEditor
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
          copyError={copyError}
        />
      )}

      {/* Revealed Secret Modal */}
      {activeModal?.type === 'revealed' && (
        <SecretRevealedModal
          data={activeModal.data}
          onClose={() => setActiveModal(null)}
          onCopy={handleCopy}
          copiedId={copiedId}
          copyError={copyError}
        />
      )}
    </div>
  );
}

export default KeysPage;
