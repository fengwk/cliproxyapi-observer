import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { KeysPage } from './KeysPage';
import type { ApiClient } from '../lib/api';
import type { Key } from '../lib/types';

describe('KeysPage Foundation Features', () => {
  let mockApi: ApiClient;
  let mockKeys: Key[];
  let onDirtyChange: ReturnType<typeof vi.fn<(dirty: boolean) => void>>;
  let requestedPaths: string[];

  beforeEach(() => {
    // Mock scrollIntoView for Radix Select in jsdom
    window.HTMLElement.prototype.scrollIntoView = vi.fn();

    requestedPaths = [];
    mockKeys = [
      {
        id: 'key_active_123',
        label: '活跃服务 Key',
        secret: 'sk-active-secret-123',
        enabled: true,
        revoked: false,
        created_at: '2026-01-01T00:00:00Z',
      },
      {
        id: 'key_disabled_456',
        label: '已禁用 Key',
        secret: 'sk-disabled-secret-456',
        enabled: false,
        revoked: false,
        created_at: '2026-01-02T00:00:00Z',
      },
      {
        id: 'key_revoked_789',
        label: '已吊销 Key',
        enabled: false,
        revoked: true,
        created_at: '2026-01-03T00:00:00Z',
      },
      {
        id: 'key_deleted_999',
        label: '已彻底删除 Key',
        enabled: false,
        deleted_at: '2026-10-20T12:00:00Z',
        created_at: '2026-01-04T00:00:00Z',
      },
    ] as unknown as Key[];

    onDirtyChange = vi.fn();

    mockApi = {
      request: vi.fn().mockImplementation((path: string, opts?: { method?: string; body?: unknown }) => {
        requestedPaths.push(path);
        if (path.startsWith('/keys/delete?')) {
          if (path.includes('busy_key')) {
            const err = new Error('Key has active in-flight requests (busy)');
            (err as unknown as { status: number }).status = 409;
            return Promise.reject(err);
          }
          const id = new URLSearchParams(path.split('?')[1] || '').get('id');
          const key = mockKeys.find((item) => item.id === id);
          if (key) {
            key.deleted_at = '2026-10-21T00:00:00Z';
            key.enabled = false;
            key.revoked = true;
            key.secret = undefined;
          }
          return Promise.resolve(null);
        }
        if (path === '/keys' || path.startsWith('/keys?')) {
          if (!opts?.method || opts.method === 'GET') {
            const hasIncludeDeleted = path.includes('include_deleted=true');
            if (hasIncludeDeleted) {
              return Promise.resolve({ items: mockKeys });
            }
            return Promise.resolve({ items: mockKeys.filter((k) => !k.deleted_at) });
          }
          if (opts.method === 'PUT') {
            const id = new URLSearchParams(path.split('?')[1] || '').get('id');
            const key = mockKeys.find((item) => item.id === id);
            if (key) Object.assign(key, opts.body);
            return Promise.resolve(key || {});
          }
        }
        return Promise.resolve({});
      }),
      models: vi.fn().mockResolvedValue([]),
    };
  });

  it('queries include_deleted=true only when deleted filter is chosen', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    // Initially loads without include_deleted=true
    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
    });
    expect(requestedPaths.some((p) => p.includes('include_deleted=true'))).toBe(false);

    // Switch status filter to '已删除'
    const statusSelect = screen.getByLabelText('状态筛选');
    fireEvent.keyDown(statusSelect, { key: 'ArrowDown' });

    const deletedOption = await screen.findByRole('option', { name: '已删除' });
    fireEvent.click(deletedOption);

    // Now it should request with include_deleted=true
    await waitFor(() => {
      expect(requestedPaths.some((p) => p.includes('include_deleted=true'))).toBe(true);
    });
  });

  it('distinguishes badges for active, disabled, revoked, and deleted keys, disabling actions on deleted keys', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
    });

    // Check non-deleted badges
    expect(screen.getByText('正常')).toBeInTheDocument();
    expect(screen.getByText('已禁用')).toBeInTheDocument();
    expect(screen.getByText('已撤销')).toBeInTheDocument();

    // Now switch to deleted filter to inspect deleted key
    const statusSelect = screen.getByLabelText('状态筛选');
    fireEvent.keyDown(statusSelect, { key: 'ArrowDown' });
    const deletedOption = await screen.findByRole('option', { name: '已删除' });
    fireEvent.click(deletedOption);

    await waitFor(() => {
      expect(screen.getByText('已彻底删除 Key')).toBeInTheDocument();
    });

    const deletedRow = screen.getByText('已彻底删除 Key').closest('tr');
    expect(deletedRow).not.toBeNull();
    if (deletedRow) {
      expect(within(deletedRow).getByText('已删除')).toBeInTheDocument();
      expect(deletedRow).toHaveTextContent('已删除（终态）');
      expect(deletedRow.querySelector('button[title="编辑配置与额度"]')).toBeNull();
      expect(deletedRow.querySelector('button[title="永久删除此 Key"]')).toBeNull();
    }
  });

  it('handles permanent delete flow POST /keys/delete and handles busy errors gracefully', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
    });

    // Find delete button for active key
    const activeRow = screen.getByText('活跃服务 Key').closest('tr');
    const deleteBtn = activeRow?.querySelector('button[title="永久删除此 Key"]') as HTMLButtonElement;
    fireEvent.click(deleteBtn);

    // Modal opens with permanent delete warning
    expect(await screen.findByRole('heading', { name: /删除 Key:/ })).toBeInTheDocument();
    expect(screen.getByText(/终态操作警告/)).toBeInTheDocument();
    expect(screen.getByText(/该 Key 既往产生的用量、费用明细与统计记录将作为最小审计账目予以保留/)).toBeInTheDocument();

    // Confirm deletion
    const confirmBtn = screen.getByRole('button', { name: '确认永久删除' });
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      expect(requestedPaths.some((p) => p.startsWith('/keys/delete?id=key_active_123'))).toBe(true);
    });

    // Modal closes on success
    await waitFor(() => {
      expect(screen.queryByRole('heading', { name: /删除 Key:/ })).not.toBeInTheDocument();
    });
  });

  it('keeps delete modal open and displays error when backend rejects deletion due to busy in-flight requests', async () => {
    // Modify active key ID to trigger busy error
    mockKeys[0].id = 'busy_key_123';

    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
    });

    const activeRow = screen.getByText('活跃服务 Key').closest('tr');
    const deleteBtn = activeRow?.querySelector('button[title="永久删除此 Key"]') as HTMLButtonElement;
    fireEvent.click(deleteBtn);

    const confirmBtn = await screen.findByRole('button', { name: '确认永久删除' });
    fireEvent.click(confirmBtn);

    // Modal stays open and displays the error message
    await waitFor(() => {
      expect(screen.getByText(/当前仍有活跃请求.*暂无法删除/)).toBeInTheDocument();
    });
    expect(screen.getByRole('heading', { name: /删除 Key:/ })).toBeInTheDocument();
  });

  it('keeps the actual secret copy when disabling and removes it after permanent deletion', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
    });

    const activeRow = screen.getByText('活跃服务 Key').closest('tr');
    expect(activeRow).not.toBeNull();
    expect(within(activeRow as HTMLElement).getByText('sk-active-secret-123')).toBeInTheDocument();
    expect(within(activeRow as HTMLElement).getByTitle('复制密钥')).toBeInTheDocument();

    // Reversible disable must reload the list with the same current secret.
    fireEvent.click(activeRow!.querySelector('button[title="禁用此 Key"]') as HTMLButtonElement);
    fireEvent.click(await screen.findByRole('button', { name: '确认禁用' }));
    await waitFor(() => {
      expect(screen.queryByRole('heading', { name: /禁用 Key:/ })).not.toBeInTheDocument();
    });

    const disabledRow = screen.getByText('活跃服务 Key').closest('tr');
    expect(disabledRow).not.toBeNull();
    expect(within(disabledRow as HTMLElement).getByText('sk-active-secret-123')).toBeInTheDocument();
    expect(within(disabledRow as HTMLElement).getByTitle('复制密钥')).toBeInTheDocument();

    // Permanent deletion removes the credential material from the reloaded list.
    fireEvent.click(disabledRow!.querySelector('button[title="永久删除此 Key"]') as HTMLButtonElement);
    fireEvent.click(await screen.findByRole('button', { name: '确认永久删除' }));
    await waitFor(() => {
      expect(screen.queryByText('活跃服务 Key')).not.toBeInTheDocument();
    });
    expect(screen.queryByText('sk-active-secret-123')).not.toBeInTheDocument();
  });

  it('filters keys by search input matching label or actual secret, never hidden internal ID', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('活跃服务 Key')).toBeInTheDocument();
      expect(screen.getByText('已禁用 Key')).toBeInTheDocument();
    });

    const searchInput = screen.getByPlaceholderText('搜索名称或密钥...');
    fireEvent.change(searchInput, { target: { value: 'disabled-secret' } });

    expect(screen.queryByText('活跃服务 Key')).not.toBeInTheDocument();
    expect(screen.getByText('已禁用 Key')).toBeInTheDocument();

    // Internal IDs drive mutations/history but are not a credential or a searchable display value.
    fireEvent.change(searchInput, { target: { value: 'key_active_123' } });
    expect(screen.queryByText('活跃服务 Key')).not.toBeInTheDocument();
    expect(screen.getByText('未找到匹配的 Key')).toBeInTheDocument();
  });
});
