import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { App } from './App';
import { SESSION_KEY_STORAGE } from './lib/api';
import type { PageProps } from './lib/api';

// Mock 三个页面组件，以便精准测试 App 层的生命周期、会话、路由切换与 dirty 守卫
vi.mock('./pages/KeysPage', () => ({
  KeysPage: ({ api, refreshInterval, onDirtyChange }: PageProps) => (
    <div data-testid="mock-keys-page">
      <span>KeysPage Mounted</span>
      <span data-testid="keys-interval">{refreshInterval}</span>
      <button onClick={() => onDirtyChange(true)}>触发 Dirty</button>
      <button onClick={() => onDirtyChange(false)}>清除 Dirty</button>
      <button
        onClick={() => {
          api.request('/trigger-401').catch(() => {});
        }}
      >
        触发 401
      </button>
    </div>
  ),
}));

vi.mock('./pages/Requests', () => ({
  RequestsPage: ({ refreshInterval }: PageProps) => (
    <div data-testid="mock-requests-page">
      <span>RequestsPage Mounted</span>
      <span data-testid="requests-interval">{refreshInterval}</span>
    </div>
  ),
}));

vi.mock('./pages/Settings', () => ({
  SettingsPage: ({ refreshInterval }: PageProps) => (
    <div data-testid="mock-settings-page">
      <span>SettingsPage Mounted</span>
      <span data-testid="settings-interval">{refreshInterval}</span>
    </div>
  ),
}));

describe('App Component Lifecycle & Session Integration', () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  afterEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  /**
   * 意图：验证登录与健康检查流程。
   * 未登录时显示登录界面，输入空密码拒绝；输入密码后调用 /health 验证，
   * 验证失败展示错误信息，验证成功后将 key 写入 sessionStorage 并进入主界面。
   */
  it('handles login with /health verification and saves session on success', async () => {
    const mockFetch = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/health')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          headers: new Headers({ 'content-type': 'application/json' }),
          json: async () => ({ database_bytes: 1000 }),
        });
      }
      return Promise.reject(new Error('Unknown url'));
    });
    global.fetch = mockFetch;

    render(<App />);

    // 验证初始处于登录界面
    expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();

    const input = screen.getByLabelText('管理密钥');
    const submitBtn = screen.getByRole('button', { name: '登录' });

    // 1. 提交空密码
    fireEvent.change(input, { target: { value: '   ' } });
    fireEvent.click(submitBtn);
    expect(screen.getByText('请输入 CPA 管理密钥')).toBeInTheDocument();
    expect(mockFetch).not.toHaveBeenCalled();

    // 2. 模拟验证失败
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 401,
      headers: new Headers(),
    });
    fireEvent.change(input, { target: { value: 'wrong-key' } });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText(/未授权或管理密钥无效/)).toBeInTheDocument();
    });
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();

    // 3. 验证成功
    fireEvent.change(input, { target: { value: 'correct-secret' } });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBe('correct-secret');
    expect(screen.getByText('Observer')).toBeInTheDocument();
  });

  /**
   * 意图：验证当 sessionStorage 已存在 key 时，初始直接进入主界面。
   */
  it('mounts into authenticated app when session key already exists in sessionStorage', () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'existing-secret');

    render(<App />);

    expect(screen.queryByRole('heading', { name: '登录 Observer' })).not.toBeInTheDocument();
    expect(screen.getByText('Observer')).toBeInTheDocument();
    expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
  });

  /**
   * 意图：验证 3 个 Tab 的实际接线（wiring）与切换逻辑。
   */
  it('switches between Requests, Keys, and Settings tabs correctly', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'test-key');

    render(<App />);

    // 默认展示 RequestsPage
    expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();

    // 点击 Key 管理
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();
    expect(screen.queryByTestId('mock-requests-page')).not.toBeInTheDocument();

    // 点击 设置
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    expect(screen.getByTestId('mock-settings-page')).toBeInTheDocument();
    expect(screen.queryByTestId('mock-keys-page')).not.toBeInTheDocument();
  });

  /**
   * 意图：验证自动刷新轮询间隔控制（5秒/关）及在 dirty 时的文案响应。
   */
  it('controls polling interval and responds to dirty state', () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'test-key');

    render(<App />);

    const intervalBtn = screen.getByTitle('自动刷新，每 5 秒一次');
    expect(intervalBtn).toHaveTextContent('每 5 秒刷新');
    expect(screen.getByTestId('requests-interval')).toHaveTextContent('5');

    // 切换为关闭轮询
    fireEvent.click(intervalBtn);
    expect(intervalBtn).toHaveTextContent('自动刷新已关');
    expect(screen.getByTestId('requests-interval')).toHaveTextContent('0');

    // 重新开启轮询
    fireEvent.click(intervalBtn);
    expect(intervalBtn).toHaveTextContent('每 5 秒刷新');

    // 切换到 Keys 页并触发 dirty
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    fireEvent.click(screen.getByRole('button', { name: '触发 Dirty' }));

    // dirty 状态下提示“编辑中，暂停刷新”
    expect(intervalBtn).toHaveTextContent('编辑中，暂停刷新');
  });

  /**
   * 意图：验证主动退出（Logout）会销毁客户端、清空 sessionStorage 并返回登录页。
   */
  it('logs out, disposes client, clears session storage, and returns to login card', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'active-session');

    render(<App />);

    expect(screen.getByText('Observer')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '退出' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证当页面请求遭遇 401 Unauthorized 时，App 自动触发 logout 卸载会话清 session。
   */
  it('handles 401 unauthorized from page by logging out and cleaning session', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'expiring-key');

    const mockFetch = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/trigger-401')) {
        return Promise.resolve({
          ok: false,
          status: 401,
          headers: new Headers(),
        });
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        headers: new Headers({ 'content-type': 'application/json' }),
        json: async () => ({}),
      });
    });
    global.fetch = mockFetch;

    render(<App />);

    // 切到 KeysPage
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();

    // 点击触发 401 请求
    fireEvent.click(screen.getByRole('button', { name: '触发 401' }));

    // 验证 App 响应 401，清空 session 并跳回登录界面
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证 dirty 状态下切换 Tab 需要弹窗确认；
   * 点击“继续编辑”取消切换，点击“放弃修改”确认切换并重置 dirty。
   */
  it('intercepts tab navigation when dirty, requiring confirmation modal', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'test-key');

    render(<App />);

    // 进入 KeysPage 并使其变为 dirty
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    fireEvent.click(screen.getByRole('button', { name: '触发 Dirty' }));

    // 尝试切换到 请求日志
    fireEvent.click(screen.getByRole('button', { name: '请求日志' }));

    // 弹出确认弹窗
    expect(screen.getByText('放弃未保存的修改？')).toBeInTheDocument();
    expect(screen.getByText('当前页面的修改尚未保存。离开后，这些修改将丢失。')).toBeInTheDocument();

    // 点击“继续编辑”取消切换
    fireEvent.click(screen.getByRole('button', { name: '继续编辑' }));
    expect(screen.queryByText('放弃未保存的修改？')).not.toBeInTheDocument();
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();

    // 再次点击切换，并在弹窗中选择“放弃修改”
    fireEvent.click(screen.getByRole('button', { name: '请求日志' }));
    fireEvent.click(screen.getByRole('button', { name: '放弃修改' }));

    await waitFor(() => {
      expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();
    });
    expect(screen.queryByTestId('mock-keys-page')).not.toBeInTheDocument();
  });

  /**
   * 意图：验证 dirty 状态下点击退出也需要弹窗确认拦截。
   */
  it('intercepts logout when dirty, requiring confirmation before exiting', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'test-key');

    render(<App />);

    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    fireEvent.click(screen.getByRole('button', { name: '触发 Dirty' }));

    // 点击退出
    fireEvent.click(screen.getByRole('button', { name: '退出' }));

    // 拦截弹出确认 Modal
    expect(screen.getByText('放弃未保存的修改？')).toBeInTheDocument();

    // 点击“继续编辑”
    fireEvent.click(screen.getByRole('button', { name: '继续编辑' }));
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBe('test-key');

    // 再次点击退出并确认放弃修改
    fireEvent.click(screen.getByRole('button', { name: '退出' }));
    fireEvent.click(screen.getByRole('button', { name: '放弃修改' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证 dirty 状态会注册 window beforeunload 事件保护。
   */
  it('registers beforeunload handler when dirty is true and removes it when clean', () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'test-key');
    const addEventListenerSpy = vi.spyOn(window, 'addEventListener');
    const removeEventListenerSpy = vi.spyOn(window, 'removeEventListener');

    render(<App />);

    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));

    // 初始 clean
    expect(addEventListenerSpy).not.toHaveBeenCalledWith('beforeunload', expect.any(Function));

    // 触发 dirty
    fireEvent.click(screen.getByRole('button', { name: '触发 Dirty' }));
    expect(addEventListenerSpy).toHaveBeenCalledWith('beforeunload', expect.any(Function));

    // 清除 dirty
    fireEvent.click(screen.getByRole('button', { name: '清除 Dirty' }));
    expect(removeEventListenerSpy).toHaveBeenCalledWith('beforeunload', expect.any(Function));
  });
});
