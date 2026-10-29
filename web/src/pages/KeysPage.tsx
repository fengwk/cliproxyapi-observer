import { useState, useCallback, useEffect, useRef, useMemo } from 'react';
import type { PageProps } from '../lib/api';
import type { Key } from '../lib/types';
import { formatUSD, formatDateTime, errorMessage } from '../lib/format';
import { Button, Input, Select, Badge, Notice, EmptyState } from '../components/ui';
import { copyToClipboard } from './KeysHelpers';
import { KeyEditor, type RevealedSecretState } from './KeysEditor';
import {
  RotateKeyModal,
  ResetKeyModal,
  RevokeKeyModal,
  DeleteKeyModal,
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
  | { type: 'delete'; key: Key }
  | { type: 'details'; key: Key }
  | { type: 'revealed'; data: RevealedSecretState }
  | null;

export type KeyStatusFilter =
  | 'active'
  | 'enabled'
  | 'disabled'
  | 'revoked'
  | 'deleted'
  | 'expired'
  | 'all';

export function KeysPage({ api, onDirtyChange }: KeysPageProps) {
  const [keys, setKeys] = useState<Key[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [pageError, setPageError] = useState<string>('');
  const [activeModal, setActiveModal] = useState<ActiveModal>(null);

  // Search & Status filter state
  const [searchQuery, setSearchQuery] = useState<string>('');
  const [statusFilter, setStatusFilter] = useState<KeyStatusFilter>('active');

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
    onDirtyChange?.(isDirty);

    return () => {
      onDirtyChange?.(false);
    };
  }, [activeModal, onDirtyChange]);

  const needIncludeDeleted = statusFilter === 'deleted';

  // Load keys with generation guard and abort controller.
  // Per contract: Keys() defaults to hiding deleted items; if filtering deleted, use /keys?include_deleted=true.
  const loadKeys = useCallback(
    async (signal?: AbortSignal, includeDeleted = needIncludeDeleted) => {
      if (loadAbortRef.current) {
        loadAbortRef.current.abort();
      }
      const ac = new AbortController();
      loadAbortRef.current = ac;
      const curGen = ++loadGenRef.current;
      const abort = () => ac.abort();
      signal?.addEventListener('abort', abort, { once: true });
      if (signal?.aborted) ac.abort();

      setLoading(true);
      try {
        const path = includeDeleted ? '/keys?include_deleted=true' : '/keys';
        const response = await api.request<{ items: Key[] }>(path, {
          signal: ac.signal,
        });
        if (ac.signal.aborted || !mountedRef.current || curGen !== loadGenRef.current) {
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
        signal?.removeEventListener('abort', abort);
        if (mountedRef.current && curGen === loadGenRef.current) {
          setLoading(false);
        }
      }
    },
    [api, needIncludeDeleted]
  );

  // When status filter switches between requiring deleted items and not, reload
  useEffect(() => {
    loadKeys(undefined, needIncludeDeleted);
  }, [loadKeys, needIncludeDeleted]);

  useEffect(() => {
    mountedRef.current = true;

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

  // No periodic loads: entry, deleted-history scope changes, explicit refresh
  // and successful mutations are the only list reload triggers.

  // Robust clipboard copy with timer cleanup and visible failure
  const handleCopy = async (text: string, id: string) => {
    if (copyTimerRef.current) {
      clearTimeout(copyTimerRef.current);
      copyTimerRef.current = null;
    }
    setCopyError(null);

    const ok = await copyToClipboard(text);
    if (!mountedRef.current) return;
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

  // Filtered keys based on status and search query
  const filteredKeys = useMemo(() => {
    let list = keys;

    // Status filter
    if (statusFilter === 'deleted') {
      list = list.filter((k) => !!k.deleted_at);
    } else if (statusFilter === 'revoked') {
      list = list.filter((k) => !!k.revoked && !k.deleted_at);
    } else if (statusFilter === 'disabled') {
      list = list.filter((k) => !k.enabled && !k.deleted_at && !k.revoked);
    } else if (statusFilter === 'enabled') {
      list = list.filter(
        (k) =>
          k.enabled &&
          !k.deleted_at &&
          !k.revoked &&
          !(k.expires_at && new Date(k.expires_at) < new Date())
      );
    } else if (statusFilter === 'expired') {
      list = list.filter(
        (k) => k.expires_at && new Date(k.expires_at) < new Date() && !k.deleted_at
      );
    } else if (statusFilter === 'active') {
      // Default active: non-deleted keys
      list = list.filter((k) => !k.deleted_at);
    }
    // 'all' includes everything returned

    // Search query filter
    const q = searchQuery.trim().toLowerCase();
    if (q) {
      list = list.filter(
        (k) =>
          (k.label && k.label.toLowerCase().includes(q)) ||
          (typeof k.secret === 'string' && k.secret.toLowerCase().includes(q))
      );
    }

    return list;
  }, [keys, statusFilter, searchQuery]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
      {/* Top title & create action */}
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

        <Button variant="primary" onClick={() => setActiveModal({ type: 'create' })}>
          + 新建 Key
        </Button>
      </div>

      {/* Filter and search toolbar */}
      <div
        style={{
          display: 'flex',
          gap: '0.75rem',
          flexWrap: 'wrap',
          alignItems: 'center',
          padding: '0.75rem 1rem',
          backgroundColor: 'var(--bg-primary)',
          borderRadius: 'var(--radius-md)',
          border: '1px solid var(--border-color)',
        }}
      >
        <div style={{ flex: '1 1 240px', minWidth: '180px' }}>
          <Input
            placeholder="搜索名称或密钥..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            aria-label="搜索 Key"
          />
        </div>

        <div style={{ width: '160px' }}>
          <Select
            value={statusFilter}
            onChange={(val) => setStatusFilter(val as KeyStatusFilter)}
            aria-label="状态筛选"
            options={[
              { value: 'active', label: '全部状态' },
              { value: 'enabled', label: '正常 / 启用' },
              { value: 'disabled', label: '已禁用' },
              { value: 'revoked', label: '已撤销' },
              { value: 'deleted', label: '已删除' },
              { value: 'expired', label: '已过期' },
              { value: 'all', label: '全部（含已删除）' },
            ]}
          />
        </div>

        <Button
          variant="ghost"
          onClick={() => loadKeys(undefined, needIncludeDeleted)}
          loading={loading}
        >
          刷新
        </Button>
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
      ) : filteredKeys.length === 0 ? (
        <EmptyState
          title="未找到匹配的 Key"
          description="没有符合当前筛选或搜索条件的 Key，请尝试更改条件。"
          action={
            <Button
              variant="secondary"
              onClick={() => {
                setSearchQuery('');
                setStatusFilter('active');
              }}
            >
              清除筛选条件
            </Button>
          }
        />
      ) : (
        <div className="cpa-table-container">
          <table className="cpa-table" aria-label="Key 列表">
            <thead>
              <tr>
                <th>名称 / 状态</th>
                <th>密钥</th>
                <th>额度使用 (已用 / 限额)</th>
                <th>并发限制</th>
                <th>模型权限</th>
                <th>有效期</th>
                <th style={{ textAlign: 'right' }}>操作</th>
              </tr>
            </thead>
            <tbody>
              {filteredKeys.map((k) => {
                const isDeleted = !!k.deleted_at;
                const isRevoked = !!k.revoked && !isDeleted;
                const isExpired = k.expires_at ? new Date(k.expires_at) < new Date() : false;

                return (
                  <tr key={k.id}>
                    {/* Name & Status */}
                    <td>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                          <button
                            type="button"
                            onClick={() => setActiveModal({ type: 'details', key: k })}
                            title="查看 Key 详情"
                            style={{
                              border: 'none',
                              background: 'transparent',
                              padding: 0,
                              font: 'inherit',
                              color: 'inherit',
                              cursor: 'pointer',
                              textAlign: 'left',
                              minWidth: 0,
                            }}
                          >
                            <strong style={{ fontSize: '0.95rem' }}>{k.label || '未命名 Key'}</strong>
                          </button>
                          {isDeleted ? (
                            <Badge tone="danger">已删除</Badge>
                          ) : isRevoked ? (
                            <Badge tone="neutral">已撤销</Badge>
                          ) : !k.enabled ? (
                            <Badge tone="neutral">已禁用</Badge>
                          ) : isExpired ? (
                            <Badge tone="warning">已过期</Badge>
                          ) : (
                            <Badge tone="success">正常</Badge>
                          )}
                        </div>
                        {isDeleted && (
                          <span style={{ fontSize: '0.78rem', color: 'var(--text-tertiary)' }}>
                            已于 {formatDateTime(k.deleted_at)} 已删除（历史账目已保留）
                          </span>
                        )}
                      </div>
                    </td>

                    {/* Full client secret + copy (never an internal ID) */}
                    <td>
                      {typeof k.secret === 'string' && k.secret.length > 0 ? (
                        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.35rem', minWidth: 0 }}>
                          <div
                            style={{
                              fontFamily: 'var(--font-mono)',
                              fontSize: '0.8rem',
                              backgroundColor: 'var(--bg-tertiary)',
                              borderRadius: 'var(--radius-sm)',
                              padding: '0.3rem 0.4rem',
                              maxHeight: '7rem',
                              overflow: 'auto',
                              whiteSpace: 'pre-wrap',
                              overflowWrap: 'anywhere',
                              wordBreak: 'break-all',
                              userSelect: 'all',
                            }}
                          >
                            {k.secret}
                          </div>
                          <Button
                            variant="ghost"
                            style={{ padding: '0.2rem 0.4rem', minHeight: 'auto', fontSize: '0.75rem', alignSelf: 'flex-start' }}
                            onClick={() => handleCopy(k.secret as string, k.id)}
                            title="复制密钥"
                          >
                            {copiedId === k.id ? '已复制密钥' : '复制密钥'}
                          </Button>
                        </div>
                      ) : (
                        <span style={{ fontSize: '0.8rem', color: 'var(--text-tertiary)' }}>
                          未保存明文，请重新生成
                        </span>
                      )}
                    </td>

                    {/* Compact Quota Lines (Explicit Unlimited) */}
                    <td>
                      {k.limits && k.used ? (
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
                      ) : (
                        <span style={{ color: 'var(--text-tertiary)', fontSize: '0.85rem' }}>-</span>
                      )}
                    </td>

                    {/* Concurrency */}
                    <td>
                      <div style={{ fontSize: '0.85rem' }}>
                        {k.max_concurrent === 0 ? (
                          <span style={{ color: 'var(--text-secondary)' }}>不限并发</span>
                        ) : (
                          <span>
                            {k.active ?? 0} / {k.max_concurrent ?? '-'} 并发
                          </span>
                        )}
                      </div>
                    </td>

                    {/* Models Permission */}
                    <td>
                      <div style={{ fontSize: '0.85rem' }}>
                        {(!k.allowed_models || k.allowed_models.length === 0) ? (
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
                      {isDeleted ? (
                        <span style={{ fontSize: '0.8rem', color: 'var(--text-tertiary)' }}>
                          已删除（终态）
                        </span>
                      ) : (
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

                          {isRevoked ? (
                            <span
                              style={{ fontSize: '0.78rem', color: 'var(--text-tertiary)', padding: '0 0.3rem' }}
                              title="已撤销哈希，需先轮转新密钥再启用"
                            >
                              需轮转
                            </span>
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

                          <Button
                            variant="ghost"
                            style={{ color: 'var(--danger-color)' }}
                            onClick={() => setActiveModal({ type: 'delete', key: k })}
                            title="永久删除此 Key"
                          >
                            删除
                          </Button>
                        </div>
                      )}
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
        <KeyEditor
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={(revealed) => {
            if (revealed) {
              setActiveModal({ type: 'revealed', data: revealed });
            } else {
              setActiveModal(null);
            }
            loadKeys(undefined, needIncludeDeleted);
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
            loadKeys(undefined, needIncludeDeleted);
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
            loadKeys(undefined, needIncludeDeleted);
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
            loadKeys(undefined, needIncludeDeleted);
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
            loadKeys(undefined, needIncludeDeleted);
          }}
        />
      )}

      {/* Delete Key Modal */}
      {activeModal?.type === 'delete' && (
        <DeleteKeyModal
          keyData={activeModal.key}
          api={api}
          onClose={() => setActiveModal(null)}
          onSuccess={() => {
            setActiveModal(null);
            loadKeys(undefined, needIncludeDeleted);
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
