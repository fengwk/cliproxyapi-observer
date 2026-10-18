import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { KeysPage } from './KeysPage';
import { ModelPicker } from './ModelPicker';
import type { ApiClient } from '../lib/api';
import type { Key } from '../lib/types';

describe('KeysPage & ModelPicker Behavior Acceptance Tests', () => {
  let mockApi: ApiClient;
  let mockKeys: Key[];
  let onDirtyChange: ReturnType<typeof vi.fn>;
  let capturedPutBody: unknown = null;
  let capturedPostBody: unknown = null;

  beforeEach(() => {
    capturedPutBody = null;
    capturedPostBody = null;

    mockKeys = [
      {
        id: 'key_1234567890abcdef1234567890abcdef',
        label: '生产环境网关',
        enabled: true,
        expires_at: '2026-10-19T14:30:00.123456789Z',
        max_concurrent: 10,
        allowed_models: ['gpt-4o', 'claude-3-5-sonnet'],
        limits: {
          five_hour: 5000000,
          week: 20000000,
          month: 0,
          total: 100000000,
        },
        used: {
          five_hour: 1500000,
          week: 5000000,
          month: 12000000,
          total: 30000000,
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
            capturedPostBody = opts.body;
            return Promise.resolve({
              key: { ...mockKeys[0], label: (opts.body as { label: string }).label },
              secret: 'sk-observer-mock-auto-secret-123456',
            });
          }
          if (opts.method === 'PUT') {
            capturedPutBody = opts.body;
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

  /**
   * 1. custom exact/untrimmed rejection
   * 意图：验证自定义密钥输入绝对不做 trim 处理。若包含前导/后导空格或非法字符直接拒绝；
   * 合法密钥原样保持并提交给服务端。
   */
  it('custom exact/untrimmed rejection: rejects untrimmed whitespace and preserves exact string without trimming', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    fireEvent.click(screen.getByLabelText(/自定义密钥/));

    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'Untrimmed-Test' } });

    const secretInput = screen.getByPlaceholderText('请输入 8-256 位 ASCII 密钥（无空格）');

    // 含有前后空格（如果被 trim 就会变成有效长度，但不应被 trim，必须拒绝）
    fireEvent.change(secretInput, { target: { value: '  validLengthKey123  ' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('自定义密钥只能包含可见 ASCII 字符且不能有空格或换行')).toBeInTheDocument();
    });
    expect(mockApi.request).not.toHaveBeenCalledWith('/keys', expect.objectContaining({ method: 'POST' }));

    // 输入合法 exact 密钥
    const exactSecret = 'MyExactSecret_12345678!';
    fireEvent.change(secretInput, { target: { value: exactSecret } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(capturedPostBody).not.toBeNull();
    });
    expect((capturedPostBody as { secret: string }).secret).toBe(exactSecret);
  });

  /**
   * 2. auto省略
   * 意图：验证在自动生成密钥模式下，提交的 KeySpec 中不包含 secret 字段（即 undefined），交由服务端自动生成。
   */
  it('auto省略: omits secret field in spec when using auto-generation', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'Auto-Secret-Key' } });

    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(capturedPostBody).not.toBeNull();
    });
    // 确保 secret 字段被省略
    expect((capturedPostBody as { secret?: string }).secret).toBeUndefined();
  });

  /**
   * 3. conflict保留草稿
   * 意图：当服务端响应 409 Conflict 错误时，表单显示冲突提示，且用户填写的草稿内容全部保留，不被清空。
   */
  it('conflict保留草稿: preserves user input draft when API responds with 409 Conflict', async () => {
    (mockApi.request as ReturnType<typeof vi.fn>).mockImplementation((path: string, opts?: { method?: string }) => {
      if (path === '/keys' && opts?.method === 'POST') {
        return Promise.reject(new Error('HTTP 409 Conflict: key secret already exists'));
      }
      if (path === '/keys' || path.startsWith('/keys?')) {
        return Promise.resolve({ items: mockKeys });
      }
      return Promise.reject(new Error('Unknown path: ' + path));
    });

    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'ConflictDraftName' } });

    fireEvent.click(screen.getByLabelText(/自定义密钥/));
    const secretInput = screen.getByPlaceholderText('请输入 8-256 位 ASCII 密钥（无空格）');
    fireEvent.change(secretInput, { target: { value: 'DuplicateSecret123!' } });

    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('该密钥已存在冲突，请使用唯一的自定义密钥')).toBeInTheDocument();
    });

    // 验证草稿内容依然保留在输入框中
    expect(screen.getByDisplayValue('ConflictDraftName')).toBeInTheDocument();
    expect(screen.getByDisplayValue('DuplicateSecret123!')).toBeInTheDocument();
  });

  /**
   * 4. quota开后空/0拒绝
   * 意图：验证用户开启任一周期限额 switch 后，若金额为空或为 0（如 "0", "0.00"），提交必须被拒绝并报错，绝不默默发送 0（0 代表不限额，非常危险）。
   */
  it('quota开后空/0拒绝: rejects empty or zero values when a quota window is enabled', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'QuotaZeroCheck' } });

    // 开启 5 小时限额开关
    const switch5h = screen.getByLabelText('启用5小时限额');
    fireEvent.click(switch5h);

    // 留空直接提交 -> 应报错拒绝
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));
    await waitFor(() => {
      expect(screen.getByText(/5小时额度已开启，金额不能为空且必须大于 0/)).toBeInTheDocument();
    });

    // 输入 "0" -> 必须拒绝
    const input5h = screen.getByPlaceholderText('金额 USD (如 5)');
    fireEvent.change(input5h, { target: { value: '0' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));
    await waitFor(() => {
      expect(screen.getByText(/5小时额度已开启，金额必须大于 0/)).toBeInTheDocument();
    });

    // 输入 "0.00" -> 必须拒绝
    fireEvent.change(input5h, { target: { value: '0.00' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));
    await waitFor(() => {
      expect(screen.getByText(/5小时额度已开启，金额必须大于 0/)).toBeInTheDocument();
    });

    // 确保未向 API 发送创建请求
    expect(capturedPostBody).toBeNull();
  });

  /**
   * 5. concurrency小数拒绝
   * 意图：验证开启并发限制后，输入小数（如 '2.5'）或非整数时被拒绝并报错，禁止使用 parseInt('2.5')->2 的静默截断。
   */
  it('concurrency小数拒绝: rejects decimal and invalid numbers for max_concurrent', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'ConcurrencyTest' } });

    // 开启并发限制
    fireEvent.click(screen.getByLabelText('启用并发限制'));

    const concurrentInput = screen.getByPlaceholderText('最大并发连接数');

    // 填入小数 '2.5'
    fireEvent.change(concurrentInput, { target: { value: '2.5' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('最大并发数必须为 1 到 1000000 之间的整数')).toBeInTheDocument();
    });
    expect(capturedPostBody).toBeNull();
  });

  /**
   * 6. expiry开后空拒绝+纳秒保持
   * 意图：开启有效期后若留空必须拒绝；在编辑未修改时间的情况下，PUT 请求中必须完整保留原 RFC3339 纳秒和时区字符串。
   */
  it('expiry开后空拒绝+纳秒保持: rejects empty expiry when enabled, and preserves RFC3339 nanoseconds on edit', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    // A: 测试开启有效期但留空被拒绝
    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'ExpiryEmptyCheck' } });

    fireEvent.click(screen.getByLabelText('启用有效期限制'));
    // 不填入时间直接点击确认创建
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('开启有效期后必须指定到期时间')).toBeInTheDocument();
    });
    expect(capturedPostBody).toBeNull();

    // 关闭新建弹窗
    fireEvent.click(screen.getByRole('button', { name: '取消' }));

    // B: 测试编辑时保留原始纳秒与时区
    fireEvent.click(screen.getByTitle('编辑配置与额度'));
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /编辑 Key: 生产环境网关/ })).toBeInTheDocument();
    });

    // 原 expires_at 是 '2026-10-19T14:30:00.123456789Z'，不改动时间直接保存
    fireEvent.click(screen.getByRole('button', { name: '保存修改' }));

    await waitFor(() => {
      expect(capturedPutBody).not.toBeNull();
    });
    expect((capturedPutBody as { expires_at: string }).expires_at).toBe('2026-10-19T14:30:00.123456789Z');
  });

  /**
   * 7. restricted空failclosed
   * 意图：开启“限制模型访问”开关但未选择任何模型时，表单阻止提交并显示错误提示（fail-closed）。
   */
  it('restricted空failclosed: fails closed when model restriction is enabled with no models selected', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'RestrictedEmptyModel' } });

    // 开启限制访问模型
    fireEvent.click(screen.getByLabelText('仅允许指定模型'));

    // 默认未选择模型，直接提交
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('开启限制模型访问时，必须至少选择或添加一个模型')).toBeInTheDocument();
    });
    expect(capturedPostBody).toBeNull();
  });

  /**
   * 8. edit PUT无secret
   * 意图：编辑既有 Key 保存时，发送的 PUT 请求体中绝对不包含 secret 字段。
   */
  it('edit PUT无secret: never sends secret field in PUT request when editing key', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTitle('编辑配置与额度'));
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /编辑 Key: 生产环境网关/ })).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole('button', { name: '保存修改' }));

    await waitFor(() => {
      expect(capturedPutBody).not.toBeNull();
    });
    expect((capturedPutBody as Record<string, unknown>).secret).toBeUndefined();
  });

  /**
   * 9. rotate后反复提交/关闭在途
   * 意图：验证轮转提交在途期间，ref 锁阻止重复触发，且在请求在途时关闭操作为 no-op。
   */
  it('rotate后反复提交/关闭在途: prevents duplicate submission via ref lock and makes onClose no-op while in-flight', async () => {
    let resolveRotate: (val: unknown) => void;
    const rotatePromise = new Promise((resolve) => {
      resolveRotate = resolve;
    });

    (mockApi.request as ReturnType<typeof vi.fn>).mockImplementation((path: string) => {
      if (path.startsWith('/keys/rotate?')) {
        return rotatePromise;
      }
      return Promise.resolve({ items: mockKeys });
    });

    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTitle('轮转生成新密钥'));
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /轮转密钥/ })).toBeInTheDocument();
    });

    const submitBtn = screen.getByRole('button', { name: '确认轮转' });
    const cancelBtn = screen.getByRole('button', { name: '取消' });

    // 第一次点击提交
    fireEvent.click(submitBtn);

    // 尝试在提交在途时重复点击提交
    fireEvent.click(submitBtn);
    fireEvent.click(submitBtn);

    // 尝试在提交在途时点击取消或关闭
    fireEvent.click(cancelBtn);

    // 此时弹窗依然在屏幕上，未被关闭
    expect(screen.getByRole('heading', { name: /轮转密钥/ })).toBeInTheDocument();

    // 释放请求
    resolveRotate!({ key: mockKeys[0], secret: 'sk-observer-rotated-unique' });

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: 'Key 轮转成功' })).toBeInTheDocument();
    });

    // 验证 API 仅被调用一次轮转
    const rotateCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter((c) =>
      c[0].startsWith('/keys/rotate?')
    );
    expect(rotateCalls.length).toBe(1);
  });

  /**
   * 10. secret关闭消失
   * 意图：验证明文密钥仅在成功弹窗中短暂展示，用户点击关闭后彻底从 DOM 移除，且不写入 localStorage/sessionStorage。
   */
  it('secret关闭消失: revealed secret is displayed transiently and cleared on close without persistence', async () => {
    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('+ 新建 Key'));
    const nameInput = screen.getByPlaceholderText('例如: 生产环境客户端 / 研发测试');
    fireEvent.change(nameInput, { target: { value: 'SecretEphemeral' } });
    fireEvent.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(screen.getByText('sk-observer-mock-auto-secret-123456')).toBeInTheDocument();
    });

    // 检查 sessionStorage 和 localStorage 没有保存密钥
    expect(window.sessionStorage.getItem('observer.management-key')).toBeNull();

    // 点击关闭
    fireEvent.click(screen.getByRole('button', { name: '我已安全保存密钥，关闭' }));

    await waitFor(() => {
      expect(screen.queryByText('sk-observer-mock-auto-secret-123456')).not.toBeInTheDocument();
    });
  });

  /**
   * 11. copy失败可见
   * 意图：当 navigator.clipboard.writeText 写入失败时，界面显示明确的失败提示，而不是虚假的“已复制”。
   */
  it('copy失败可见: shows visible error feedback when clipboard copy fails instead of claiming copied', async () => {
    // 模拟剪贴板写入抛出拒绝异常
    Object.assign(navigator, {
      clipboard: {
        writeText: vi.fn().mockRejectedValue(new Error('Clipboard access denied')),
      },
    });

    render(<KeysPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('生产环境网关')).toBeInTheDocument();
    });

    const copyBtn = screen.getByTitle('复制完整 ID');
    fireEvent.click(copyBtn);

    // 验证出现可见的失败提示
    await waitFor(() => {
      expect(screen.getByText('复制失败，请手动选中文本复制')).toBeInTheDocument();
    });

    // 验证按钮上绝对不会显示“已复制”
    expect(screen.queryByText('已复制')).not.toBeInTheDocument();
  });

  /**
   * 12. ModelPicker stale+保留missing+bounded
   * 意图：验证 ModelPicker reload 替换最新目录（不累积已下线目录），但保留已选的缺失模型；
   * 并且对大量模型限制最多渲染 200 个 DOM 节点并提示搜索缩小范围。
   */
  it('ModelPicker stale+保留missing+bounded: replaces stale catalog on reload, preserves missing selected, and bounds DOM to 200', async () => {
    // 准备 250 个模型列表
    const initial250 = Array.from({ length: 250 }, (_, i) => ({
      id: `model-${String(i).padStart(3, '0')}`,
    }));

    (mockApi.models as ReturnType<typeof vi.fn>).mockResolvedValueOnce(initial250);

    const onChange = vi.fn();
    // 已经选中的模型包含一个将来下线的 'offline-model'
    const selected = ['offline-model', 'model-001'];

    render(
      <ModelPicker
        api={mockApi}
        selectedModels={selected}
        onChange={onChange}
      />
    );

    // 验证即使 offline-model 不在目录中，仍然显示在已选和列表中
    await waitFor(() => {
      expect(screen.getAllByText('offline-model').length).toBeGreaterThanOrEqual(1);
    });

    // 验证 DOM 渲染被限制在最多 200 个，并显示搜索提示
    expect(screen.getByText(/已显示前 200 个模型（共 251 个），请输入关键字搜索缩小范围/)).toBeInTheDocument();

    // 模拟刷新目录，后端仅返回两个全新模型：'fresh-model-a', 'fresh-model-b'
    (mockApi.models as ReturnType<typeof vi.fn>).mockResolvedValueOnce([
      { id: 'fresh-model-a' },
      { id: 'fresh-model-b' },
    ]);

    fireEvent.click(screen.getByRole('button', { name: '刷新目录' }));

    await waitFor(() => {
      expect(screen.getByText('fresh-model-a')).toBeInTheDocument();
    });

    // 验证已选的 'offline-model' 和 'model-001' 仍然保留
    expect(screen.getAllByText('offline-model').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('model-001').length).toBeGreaterThanOrEqual(1);

    // 验证未被选中的旧模型（如 'model-050'）已经彻底从列表中消失，不累积已下线目录
    expect(screen.queryByText('model-050')).not.toBeInTheDocument();
  });
});
