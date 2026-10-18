import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { KeysPage } from './KeysPage';
import type { ApiClient } from '../lib/api';
import type { Key } from '../lib/types';

describe('KeysPage', () => {
  let mockApi: ApiClient;
  let mockKeys: Key[];
  let onDirtyChange: ReturnType<typeof vi.fn<(dirty: boolean) => void>>;

  beforeEach(() => {
    mockKeys = [
      {
        id: 'key_1234567890abcdef1234567890abcdef',
        label: '生产环境网关',
        enabled: true,
        expires_at: '2026-10-19T14:30:00.123456789Z',
        max_concurrent: 10,
        allowed_models: ['gpt-4o', 'claude-3-5-sonnet'],
        limits: {
          five_hour: 5000000, // $5
          week: 20000000,    // $20
          month: 0,          // 不限
          total: 100000000,  // $100
        },
        used: {
          five_hour: 1500000, // $1.5
          week: 5000000,     // $5
          month: 12000000,   // $12
          total: 30000000,   // $30
        },
        starts: {
          five_hour: '2026-10-19T10:00:00Z',
          week: '2026-10-15T00:00:00Z',
          month: '2026-10-01T00:00:00Z',
          total: '2026-01-01T00:00:00Z',
        },
        resets: {
          five_hour: '2026-10-19T15:00:00Z',
          week: '2026-10-22T00:00:00Z',
          month: '2026-11-01T00:00:00Z',
          total: '2099-01-01T00:00:00Z',
        },
        active: 3,
        created_at: '2026-01-01T00:00:00Z',
      },
    ];

    onDirtyChange = vi.fn();

    mockApi = {
      request: vi.fn().mockImplementation((path: string, opts?: { method?: string; body?: unknown }) => {
        if (path === '/keys' || path.startsWith('/keys?')) {
          if (!opts?.method || opts.method === 'GET') {
            return Promise.resolve({ items: mockKeys });
          }
          if (opts.method === 'POST') {
            return Promise.resolve({
              key: { ...mockKeys[0], label: (opts.body as { label: string }).label },
              secret: 'sk-observer-mock-auto-secret-123456',
            });
          }
          if (opts.method === 'PUT') {
            return Promise.resolve(mockKeys[0]);
          }
          if (opts.method === 'DELETE') {
            return Promise.resolve({});
          }
        }
        if (path.startsWith('/keys/rotate?')) {
          return Promise.resolve({
            key: mockKeys[0],
            secret: 'sk-observer-mock-rotated-secret-654321',
          });
        }
        if (path.startsWith('/keys/reset?')) {
          return Promise.resolve(mockKeys[0]);
        }
        if (path.startsWith('/keys/recover?')) {
          return Promise.resolve(mockKeys[0]);
        }
        return Promise.reject(new Error('Unknown path: ' + path));
      }),
      models: vi.fn().mockResolvedValue([{ id: 'gpt-4o' }, { id: 'claude-3-5-sonnet' }]),
    };
  });

  it('renders keys table with name-led columns, masked ID, and explicit unlimited quotas', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    // Wait for keys to load
    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    // Masked ID: key_1234...abcdef
    expect(screen.getByText(/key_1234\.\.\.abcdef/)).toBeInTheDocument();

    // 4 compact quota lines: month limit is 0 so should display '不限', not $0 / $0
    expect(screen.getByText(/\$1\.5 \/ \$5/)).toBeInTheDocument();
    expect(screen.getByText(/\$12 \/ 不限/)).toBeInTheDocument();

    // Concurrency and models
    expect(screen.getByText('3 / 10 并发')).toBeInTheDocument();
    expect(screen.getByText('2 个指定模型')).toBeInTheDocument();
  });

  it('creates new key and displays revealed secret transiently, clearing on close', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    // Open create modal
    fireEvent.click(screen.getByText('+ 新建 Key'));
    expect(screen.getByRole('heading', { name: '创建新 Key' })).toBeInTheDocument();
    expect(onDirtyChange).toHaveBeenCalledWith(true);

    // Enter name
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: '新客户端测试' } });

    // Submit auto-generated key
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    // Wait for secret revealed modal
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: 'Key 创建成功' })).toBeInTheDocument();
    });
    expect(screen.getByText('sk-observer-mock-auto-secret-123456')).toBeInTheDocument();

    // Close revealed modal -> secret is removed from DOM
    fireEvent.click(screen.getByRole('button', { name: '我已安全保存密钥，关闭' }));
    await waitFor(() => {
      expect(screen.queryByText('sk-observer-mock-auto-secret-123456')).not.toBeInTheDocument();
    });
  });

  it('validates custom secret rules: 8-256 ASCII, weak warning for <32 chars, and no whitespace', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));

    // Switch to custom secret
    fireEvent.click(screen.getByLabelText(/自定义密钥/));

    const secretInput = screen.getByPlaceholderText('请输入 8-256 位 ASCII 密钥（无空格）');

    // 1. Weak warning for 8-31 chars
    fireEvent.change(secretInput, { target: { value: 'short123' } });
    expect(screen.getByText(/密钥长度少于 32 位（当前 8 位）/)).toBeInTheDocument();

    // 2. Reject spaces
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: '测试密钥' } });
    fireEvent.change(secretInput, { target: { value: 'invalid secret with spaces' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('自定义密钥只能包含可见 ASCII 字符且不能有空格或换行')).toBeInTheDocument();
    });

    // 3. Reject < 8 chars
    fireEvent.change(secretInput, { target: { value: 'short' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));
    await waitFor(() => {
      expect(screen.getByText('自定义密钥长度必须在 8 到 256 位之间')).toBeInTheDocument();
    });
  });

  it('rotates key independently and reveals new secret', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    // Click rotate
    fireEvent.click(screen.getByTitle('轮转生成新密钥'));
    expect(screen.getByRole('heading', { name: /轮转密钥/ })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '确认轮转' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: 'Key 轮转成功' })).toBeInTheDocument();
    });
    expect(screen.getByText('sk-observer-mock-rotated-secret-654321')).toBeInTheDocument();
  });

  it('handles reset quota confirmation with honest accounting warnings', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    // Click reset
    fireEvent.click(screen.getByTitle('重置额度周期'));
    expect(screen.getByRole('heading', { name: /重置额度用量/ })).toBeInTheDocument();
    expect(screen.getByText(/历史请求记录与实际已发生的成本不会被删除/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '确认重置' }));

    await waitFor(() => {
      expect(mockApi.request).toHaveBeenCalledWith(
        expect.stringContaining('/keys/reset?id='),
        expect.objectContaining({ method: 'POST' })
      );
    });
  });
});
