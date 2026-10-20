import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  createApiClient,
  SESSION_KEY_STORAGE,
  LOCAL_KEY_STORAGE,
  getStoredManagementKey,
  clearStoredManagementCredentials,
  ApiError,
  type ApiClient,
} from './lib/api';
import { errorMessage } from './lib/format';
import { KeysPage } from './pages/KeysPage';
import { RequestsPage } from './pages/Requests';
import { SettingsPage } from './pages/Settings';
import { Button, Field, Input, Modal, Notice, Select } from './components/ui';

type Tab = 'requests' | 'keys' | 'settings';
type Theme = 'auto' | 'light' | 'white' | 'dark';

const tabs: { id: Tab; label: string }[] = [
  { id: 'requests', label: '请求日志' },
  { id: 'keys', label: 'Key 管理' },
  { id: 'settings', label: '设置' },
];

export function App() {
  const [key, setKey] = useState<string>('');
  const [restoring, setRestoring] = useState<boolean>(() => {
    return !!getStoredManagementKey().key;
  });
  const [tab, setTab] = useState<Tab>('requests');
  const [dirty, setDirty] = useState(false);
  const [destination, setDestination] = useState<Tab | 'logout' | null>(null);

  const [loginKey, setLoginKey] = useState('');
  const [rememberMe, setRememberMe] = useState<boolean>(() => {
    const stored = getStoredManagementKey();
    if (stored.key) {
      return stored.isPersistent;
    }
    return true;
  });
  const [loginError, setLoginError] = useState('');
  const [loginBusy, setLoginBusy] = useState(false);
  const [storageWarning, setStorageWarning] = useState('');

  const loginController = useRef<AbortController | null>(null);
  const restoreController = useRef<AbortController | null>(null);
  const apiRef = useRef<ApiClient | null>(null);

  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (sessionStorage.getItem('observer.theme') as Theme) || 'auto';
    } catch {
      return 'auto';
    }
  });

  const logout = useCallback(() => {
    apiRef.current?.dispose?.();
    clearStoredManagementCredentials();
    setKey('');
    setLoginKey('');
    setDirty(false);
    setDestination(null);
    setStorageWarning('');
  }, []);

  // 1. Initial restore verification (validates once on mount)
  useEffect(() => {
    const stored = getStoredManagementKey();
    if (!stored.key) {
      setRestoring(false);
      return;
    }

    // Reflect source: session credentials must NOT set rememberMe to true
    setRememberMe(stored.isPersistent);

    const controller = new AbortController();
    restoreController.current = controller;
    let is401FromCallback = false;
    const temporary = createApiClient({
      getKey: () => stored.key,
      onUnauthorized: () => {
        is401FromCallback = true;
      },
    });

    temporary
      .request('/health', { signal: controller.signal })
      .then(() => {
        if (!controller.signal.aborted) {
          setKey(stored.key!);
          setRestoring(false);
        }
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // Strictly determine 401 via structured ApiError.status or onUnauthorized callback.
        // DO NOT guess from err.message text (which could be a proxy or network error containing "401").
        const is401 =
          is401FromCallback ||
          (err instanceof ApiError && err.status === 401) ||
          (typeof err === 'object' && err !== null && 'status' in err && (err as { status: unknown }).status === 401);

        if (is401) {
          clearStoredManagementCredentials();
          setKey('');
          setLoginError('未授权或管理密钥已失效 (401)，请重新登录');
        } else {
          // 网络失败不删合法凭据，诚实显示错误并允许用户重试
          setLoginError('连接后端服务失败，本地保存的管理凭据已保留。请检查服务后重试。');
          setLoginKey(stored.key!);
          setRememberMe(stored.isPersistent);
        }
        setRestoring(false);
      })
      .finally(() => {
        temporary.dispose?.();
        restoreController.current = null;
      });

    return () => {
      controller.abort();
    };
  }, []);

  const api = useMemo(
    () =>
      createApiClient({
        getKey: () => key || null,
        onUnauthorized: logout,
      }),
    [key, logout]
  );
  apiRef.current = api;

  useEffect(() => () => api.dispose?.(), [api]);
  useEffect(() => () => loginController.current?.abort(), []);

  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty]);

  function navigate(next: Tab | 'logout') {
    if (dirty) {
      setDestination(next);
      return;
    }
    if (next === 'logout') logout();
    else setTab(next);
  }

  async function login(event: React.FormEvent) {
    event.preventDefault();
    if (loginController.current) return;
    const entered = loginKey.trim();
    if (!entered) {
      setLoginError('请输入 CPA 管理密钥');
      return;
    }

    const controller = new AbortController();
    loginController.current = controller;
    const temporary = createApiClient({ getKey: () => entered });
    setLoginBusy(true);
    setLoginError('');
    setStorageWarning('');

    try {
      await temporary.request('/health', { signal: controller.signal });
      if (controller.signal.aborted) return;

      if (rememberMe) {
        // 先清理另一副本
        try {
          sessionStorage.removeItem(SESSION_KEY_STORAGE);
        } catch {
          /* ignore */
        }
        try {
          localStorage.setItem(LOCAL_KEY_STORAGE, entered);
        } catch {
          // Storage failed / blocked: honest warning
          setStorageWarning('本地存储已被禁用，无法记住登录，仅在当前会话生效。');
          try {
            sessionStorage.setItem(SESSION_KEY_STORAGE, entered);
          } catch {
            setStorageWarning('本地与会话存储均已被禁用，凭据仅在当前内存生效。');
          }
        }
      } else {
        // 先清理持久化副本，无论随后 sessionStorage 是否成功，确保绝不残留持久化密钥
        try {
          localStorage.removeItem(LOCAL_KEY_STORAGE);
        } catch {
          /* ignore */
        }
        try {
          sessionStorage.setItem(SESSION_KEY_STORAGE, entered);
        } catch {
          setStorageWarning('会话存储已被禁用，凭据仅在当前内存生效。');
        }
      }

      setKey(entered);
      setLoginKey('');
    } catch (error) {
      if (!controller.signal.aborted) setLoginError(errorMessage(error));
    } finally {
      temporary.dispose?.();
      loginController.current = null;
      if (!controller.signal.aborted) setLoginBusy(false);
    }
  }

  const themeControl = (
    <div className="cpa-theme-control">
      <Select
        value={theme}
        onChange={(value) => {
          setTheme(value as Theme);
          try {
            if (value === 'auto') sessionStorage.removeItem('observer.theme');
            else sessionStorage.setItem('observer.theme', value);
          } catch {
            /* unavailable storage does not block the session */
          }
          window.dispatchEvent(new CustomEvent('observer:theme-change', { detail: value }));
        }}
        aria-label="外观"
        options={[
          { value: 'auto', label: '跟随系统' },
          { value: 'light', label: '暖灰' },
          { value: 'white', label: '纯白' },
          { value: 'dark', label: '深色' },
        ]}
      />
    </div>
  );

  if (restoring) {
    return (
      <main className="cpa-app-layout cpa-login-layout">
        <section className="cpa-card cpa-login-card" style={{ textAlign: 'center', padding: '2.5rem 1.5rem' }}>
          <span className="cpa-spinner" style={{ marginRight: '0.6rem', display: 'inline-block' }} aria-hidden="true" />
          <span style={{ color: 'var(--text-secondary)' }}>正在验证管理会话...</span>
        </section>
      </main>
    );
  }

  if (!key) {
    return (
      <main className="cpa-app-layout cpa-login-layout">
        <section className="cpa-card cpa-login-card">
          <h1 className="cpa-header-title">登录 Observer</h1>
          <p className="cpa-header-subtitle">
            输入 CPA 管理密钥。勾选记住将在本机持久保存管理凭据；不勾选仅在当前会话有效。
          </p>
          {loginError && <Notice tone="danger">{loginError}</Notice>}
          {storageWarning && <Notice tone="warning">{storageWarning}</Notice>}
          <form onSubmit={login}>
            <Field label="管理密钥" htmlFor="login-key">
              <Input
                id="login-key"
                type="password"
                autoComplete="off"
                value={loginKey}
                onChange={(event) => setLoginKey(event.target.value)}
                required
              />
            </Field>

            <div style={{ marginBottom: '1.25rem' }}>
              <label className="cpa-checkbox-label">
                <input
                  type="checkbox"
                  id="remember-login"
                  checked={rememberMe}
                  onChange={(e) => setRememberMe(e.target.checked)}
                />
                <span>记住登录（仅限可信设备）</span>
              </label>
            </div>

            <Button type="submit" variant="primary" loading={loginBusy}>
              登录
            </Button>
          </form>
          {themeControl}
        </section>
      </main>
    );
  }

  const Page = tab === 'keys' ? KeysPage : tab === 'settings' ? SettingsPage : RequestsPage;
  // Per shared contract: App removes global poll button.
  // RequestsPage defaults to 5s locally (compatible), while Keys and Settings use 0 without permanent poll.
  const pageInterval = tab === 'requests' ? 5 : 0;

  return (
    <div className="cpa-app-layout cpa-authenticated-layout">
      {storageWarning && (
        <div style={{ padding: '0.5rem 0' }}>
          <Notice tone="warning">{storageWarning}</Notice>
        </div>
      )}
      <header className="cpa-header">
        <span className="cpa-header-brand cpa-standalone-brand">Observer</span>
        <div className="cpa-header-actions">{themeControl}</div>
      </header>

      <nav className="cpa-tabs" aria-label="页面标签">
        {tabs.map((item) => (
          <button
            key={item.id}
            type="button"
            className={`cpa-tab-item ${tab === item.id ? 'is-active' : ''}`}
            aria-current={tab === item.id ? 'page' : undefined}
            onClick={() => {
              if (item.id !== tab) navigate(item.id);
            }}
          >
            {item.label}
          </button>
        ))}
      </nav>

      <main id="content">
        <Page
          key={tab}
          api={api}
          refreshInterval={pageInterval}
          onDirtyChange={setDirty}
          onLogout={() => navigate('logout')}
        />
      </main>

      {destination && (
        <Modal
          title="放弃未保存的修改？"
          onClose={() => setDestination(null)}
          footer={
            <>
              <Button onClick={() => setDestination(null)}>继续编辑</Button>
              <Button
                variant="danger"
                onClick={() => {
                  const next = destination;
                  setDirty(false);
                  setDestination(null);
                  if (next === 'logout') logout();
                  else setTab(next);
                }}
              >
                放弃修改
              </Button>
            </>
          }
        >
          <p>当前页面的修改尚未保存。离开后，这些修改将丢失。</p>
        </Modal>
      )}
    </div>
  );
}

export default App;
