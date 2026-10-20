import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { App } from './App';
import { SESSION_KEY_STORAGE, LOCAL_KEY_STORAGE } from './lib/api';
import type { PageProps } from './lib/api';

// Mock the three page components to test App-level session lifecycle, routing, and intervals
vi.mock('./pages/KeysPage', () => ({
  KeysPage: ({ api, refreshInterval, onDirtyChange }: PageProps) => (
    <div data-testid="mock-keys-page">
      <span>KeysPage Mounted</span>
      <span data-testid="keys-interval">{refreshInterval}</span>
      <button onClick={() => onDirtyChange?.(true)}>触发 Dirty</button>
      <button onClick={() => onDirtyChange?.(false)}>清除 Dirty</button>
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
  SettingsPage: ({ refreshInterval, onDirtyChange, onLogout }: PageProps) => (
    <div data-testid="mock-settings-page">
      <span>SettingsPage Mounted</span>
      <span data-testid="settings-interval">{refreshInterval}</span>
      <button onClick={() => onDirtyChange?.(true)}>触发 Settings Dirty</button>
      <button onClick={() => onLogout?.()}>退出</button>
    </div>
  ),
}));

describe('App Component Lifecycle, Remember Login & Polling Integration', () => {
  beforeEach(() => {
    localStorage.clear();
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  afterEach(() => {
    localStorage.clear();
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  /**
   * 意图：验证默认勾选“记住登录”，验证成功后写入自有 localStorage（而非 sessionStorage），并进入主界面。
   */
  it('logs in with remember-login checked by default, writing to localStorage', async () => {
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

    expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();

    const input = screen.getByLabelText('管理密钥');
    const rememberCheckbox = screen.getByLabelText('记住登录（仅限可信设备）') as HTMLInputElement;
    const submitBtn = screen.getByRole('button', { name: '登录' });

    // 默认勾选
    expect(rememberCheckbox.checked).toBe(true);

    // 空密钥拒绝
    fireEvent.change(input, { target: { value: '   ' } });
    fireEvent.click(submitBtn);
    expect(screen.getByText('请输入 CPA 管理密钥')).toBeInTheDocument();
    expect(mockFetch).not.toHaveBeenCalled();

    // 提交合法密钥
    fireEvent.change(input, { target: { value: 'admin-secret-persist' } });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });

    // 成功写入 localStorage，sessionStorage 为空
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBe('admin-secret-persist');
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
    expect(screen.getByText('Observer')).toBeInTheDocument();
  });

  /**
   * 意图：验证取消勾选“记住登录”后，验证成功写入 sessionStorage（session-only），且不写入 localStorage。
   */
  it('logs in with remember-login unchecked, saving only to sessionStorage', async () => {
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

    const input = screen.getByLabelText('管理密钥');
    const rememberCheckbox = screen.getByLabelText('记住登录（仅限可信设备）');
    const submitBtn = screen.getByRole('button', { name: '登录' });

    // 取消勾选记住登录
    fireEvent.click(rememberCheckbox);

    fireEvent.change(input, { target: { value: 'session-only-secret' } });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });

    // 成功写入 sessionStorage，localStorage 为空
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBe('session-only-secret');
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证页面加载时从 localStorage 恢复凭据，并执行一次 /health 验证；验证成功后直接进入主页。
   */
  it('restores remembered credentials from localStorage, verifies via /health once, and mounts', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'saved-persistent-key');

    const mockFetch = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/health')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          headers: new Headers({ 'content-type': 'application/json' }),
          json: async () => ({ database_bytes: 2000 }),
        });
      }
      return Promise.reject(new Error('Unknown url'));
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });

    expect(screen.getByText('Observer')).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining('/health'),
      expect.objectContaining({
        headers: expect.objectContaining({
          Authorization: 'Bearer saved-persistent-key',
        }),
      })
    );
  });

  /**
   * 意图：验证 restore 恢复时若验证遇到 401，立即清除本地凭据并取消，回到登录页。
   */
  it('clears credentials from both storages and shows 401 error if restore verification fails with 401', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'expired-persistent-key');
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'expired-persistent-key');

    const mockFetch = vi.fn().mockImplementation((url: string) => {
      if (url.includes('/health')) {
        return Promise.resolve({
          ok: false,
          status: 401,
          headers: new Headers(),
        });
      }
      return Promise.reject(new Error('Unknown url'));
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
      expect(screen.getByText(/未授权或管理密钥已失效/)).toBeInTheDocument();
    });

    // 两处存储均被清空
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证 restore 遇到网络错误时，不删除本地合法凭据（网络失败不删合法凭据），提示用户重试。
   */
  it('preserves valid credentials in localStorage when restore verification fails due to network error', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'valid-key-network-down');

    const mockFetch = vi.fn().mockRejectedValue(new TypeError('Failed to fetch (network offline)'));
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
      expect(screen.getByText(/连接后端服务失败，本地保存的管理凭据已保留/)).toBeInTheDocument();
    });

    // 凭据依然保留在 localStorage
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBe('valid-key-network-down');
    // 输入框自动带入以方便重试
    const input = screen.getByLabelText('管理密钥') as HTMLInputElement;
    expect(input.value).toBe('valid-key-network-down');
  });

  /**
   * 意图：验证网络错误消息中偶然包含 "401" 字符串（如网关代理日志或错误描述），
   * 但没有真正的 HTTP status 401 时，绝不误判为凭据失效，必须保留合法凭据。
   */
  it('preserves valid credentials when network error message contains "401" without HTTP 401 status', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'valid-key-proxy-401-msg');

    // 模拟网络抛错（例如代理层报 401 DNS 或上游文本，但这是一个普通的 TypeError/Error，没有 status: 401）
    const networkErrorWith401Text = new Error('Network error: proxy gateway upstream error 401 failed to connect');
    const mockFetch = vi.fn().mockRejectedValue(networkErrorWith401Text);
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
      expect(screen.getByText(/连接后端服务失败，本地保存的管理凭据已保留/)).toBeInTheDocument();
    });

    // 凭据绝不删除！
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBe('valid-key-proxy-401-msg');
    const input = screen.getByLabelText('管理密钥') as HTMLInputElement;
    expect(input.value).toBe('valid-key-proxy-401-msg');
  });

  /**
   * 意图：验证从 sessionStorage 恢复凭据时，rememberMe 复选框应初始化为 false，
   * 避免网络失败重试时意外将临时会话凭据持久化到 localStorage。
   */
  it('restores credentials from sessionStorage with rememberMe set to false', async () => {
    sessionStorage.setItem(SESSION_KEY_STORAGE, 'session-only-key');

    const mockFetch = vi.fn().mockRejectedValue(new Error('Connection timeout'));
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });

    const rememberCheckbox = screen.getByLabelText(/记住登录/) as HTMLInputElement;
    expect(rememberCheckbox.checked).toBe(false);
  });

  /**
   * 意图：验证取消持久化登录时，即使 sessionStorage.setItem 失败，
   * 也必须先清理 localStorage 中的旧持久化凭据，防止残留旧凭据误恢复。
   */
  it('clears localStorage even if sessionStorage.setItem fails when rememberMe is false', async () => {
    // 渲染前 storage 为空以展示登录卡片
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    const input = screen.getByLabelText('管理密钥');
    const rememberCheckbox = screen.getByLabelText(/记住登录/);
    const submitBtn = screen.getByRole('button', { name: '登录' });

    // 在提交前预设 localStorage 有旧 secret，并让 sessionStorage.setItem 抛出异常
    localStorage.setItem(LOCAL_KEY_STORAGE, 'old-persistent-secret');

    const sessionSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage) {
      if (this === window.sessionStorage) {
        throw new DOMException('Session quota exceeded', 'QuotaExceededError');
      }
    });

    // 输入新密钥并取消勾选 rememberMe
    fireEvent.change(input, { target: { value: 'new-session-key' } });
    fireEvent.click(rememberCheckbox);
    expect((rememberCheckbox as HTMLInputElement).checked).toBe(false);

    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });

    // 核心断言：无论 sessionStorage 是否报错，localStorage 中的旧 persistent secret 必须已被清理！
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
    // 并且展示诚实通知
    expect(screen.getByText(/会话存储已被禁用，凭据仅在当前内存生效/)).toBeInTheDocument();

    sessionSpy.mockRestore();
  });

  /**
   * 意图：验证 localStorage 抛异常（storageblock，如无痕浏览配额限制）时诚实提示，回退到当前会话。
   */
  it('honestly warns user when storage is blocked and falls back to session/memory', async () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, key: string, value: string) {
      if (this === window.localStorage) {
        throw new DOMException('QuotaExceededError or security block', 'QuotaExceededError');
      }
      // sessionStorage behaves normally
      Object.defineProperty(this, key, { value, configurable: true, writable: true });
    });

    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    const input = screen.getByLabelText('管理密钥');
    const submitBtn = screen.getByRole('button', { name: '登录' });

    fireEvent.change(input, { target: { value: 'admin-pass' } });
    fireEvent.click(submitBtn);

    await waitFor(() => {
      expect(screen.getByText('RequestsPage Mounted')).toBeInTheDocument();
    });

    // 诚实展示警告，不谎报已记住
    expect(screen.getByText('本地存储已被禁用，无法记住登录，仅在当前会话生效。')).toBeInTheDocument();

    setItemSpy.mockRestore();
  });

  /**
   * 意图：验证主动退出（Logout）会销毁客户端，清空两处存储，并返回登录页。
   */
  it('logs out, disposes client, clears both local and session storage, and returns to login card', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'saved-admin-key');
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('Observer')).toBeInTheDocument();
    });

    // 退出入口位于设置页底部，App 继续负责确认与双存储清理。
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    fireEvent.click(screen.getByRole('button', { name: '退出' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });

    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证业务请求遭遇 401 Unauthorized 时，App 自动触发 logout，清理两处存储并跳回登录页。
   */
  it('handles 401 unauthorized from page by logging out and cleaning both storages', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'active-key');

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

    await waitFor(() => {
      expect(screen.getByText('Observer')).toBeInTheDocument();
    });

    // 切到 KeysPage
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();

    // 触发 401 请求
    fireEvent.click(screen.getByRole('button', { name: '触发 401' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });

    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
    expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证去全局 poll UI，由各页面独立管理轮询周期：
   * 头部不再展示全局刷新按钮；Requests 默认接收 5s interval 兼容，Keys 和 Settings 接收 0。
   */
  it('provides default poll intervals per page without global polling UI', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'test-key');
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();
    });

    // 全局 header 不再存在“每 5 秒刷新”按钮
    expect(screen.queryByTitle(/自动刷新/)).not.toBeInTheDocument();

    // RequestsPage 获得默认兼容 5 秒
    expect(screen.getByTestId('requests-interval')).toHaveTextContent('5');

    // 切换到 KeysPage
    fireEvent.click(screen.getByRole('button', { name: 'Key 管理' }));
    expect(screen.getByTestId('mock-keys-page')).toBeInTheDocument();
    // KeysPage 获得 0（不常驻 poll）
    expect(screen.getByTestId('keys-interval')).toHaveTextContent('0');

    // 切换到 SettingsPage
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    expect(screen.getByTestId('mock-settings-page')).toBeInTheDocument();
    // SettingsPage 获得 0（不常驻 poll）
    expect(screen.getByTestId('settings-interval')).toHaveTextContent('0');
  });

  /**
   * 意图：验证 dirty 状态下切换 Tab 需要弹窗确认；
   * 点击“继续编辑”取消切换，点击“放弃修改”确认切换并重置 dirty。
   */
  it('intercepts tab navigation when dirty, requiring confirmation modal', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'test-key');
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();
    });

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
    localStorage.setItem(LOCAL_KEY_STORAGE, 'test-key');
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();
    });

    // 进入设置页并使其变为 dirty；退出入口在设置页底部。
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    fireEvent.click(screen.getByRole('button', { name: '触发 Settings Dirty' }));

    // 点击退出
    fireEvent.click(screen.getByRole('button', { name: '退出' }));

    // 拦截弹出确认 Modal
    expect(screen.getByText('放弃未保存的修改？')).toBeInTheDocument();

    // 点击“继续编辑”
    fireEvent.click(screen.getByRole('button', { name: '继续编辑' }));
    expect(screen.getByTestId('mock-settings-page')).toBeInTheDocument();
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBe('test-key');

    // 再次点击退出并确认放弃修改
    fireEvent.click(screen.getByRole('button', { name: '退出' }));
    fireEvent.click(screen.getByRole('button', { name: '放弃修改' }));

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '登录 Observer' })).toBeInTheDocument();
    });
    expect(localStorage.getItem(LOCAL_KEY_STORAGE)).toBeNull();
  });

  /**
   * 意图：验证 dirty 状态会注册 window beforeunload 事件保护。
   */
  it('registers beforeunload handler when dirty is true and removes it when clean', async () => {
    localStorage.setItem(LOCAL_KEY_STORAGE, 'test-key');
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ database_bytes: 1000 }),
    });
    global.fetch = mockFetch;

    const addEventListenerSpy = vi.spyOn(window, 'addEventListener');
    const removeEventListenerSpy = vi.spyOn(window, 'removeEventListener');

    render(<App />);

    await waitFor(() => {
      expect(screen.getByTestId('mock-requests-page')).toBeInTheDocument();
    });

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
