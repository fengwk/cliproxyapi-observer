import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createApiClient, SESSION_KEY_STORAGE, type ApiClient } from './lib/api';
import { errorMessage } from './lib/format';
import { KeysPage } from './pages/KeysPage';
import { RequestsPage } from './pages/Requests';
import { SettingsPage } from './pages/Settings';
import { Button, Field, Input, Modal, Notice, Select } from './components/ui';

type Tab = 'requests' | 'keys' | 'settings';
type Theme = 'auto' | 'light' | 'white' | 'dark';
const tabs: { id: Tab; label: string }[] = [
  { id: 'requests', label: '请求日志' }, { id: 'keys', label: 'Key 管理' }, { id: 'settings', label: '设置' },
];
function readSession() {
  try { return sessionStorage.getItem(SESSION_KEY_STORAGE) ?? ''; } catch { return ''; }
}

export function App() {
  const [key, setKey] = useState(readSession);
  const [tab, setTab] = useState<Tab>('requests');
  const [interval, setInterval] = useState(5);
  const [dirty, setDirty] = useState(false);
  const [destination, setDestination] = useState<Tab | 'logout' | null>(null);
  const [loginKey, setLoginKey] = useState('');
  const [loginError, setLoginError] = useState('');
  const [loginBusy, setLoginBusy] = useState(false);
  const loginController = useRef<AbortController | null>(null);
  const apiRef = useRef<ApiClient | null>(null);
  const [theme, setTheme] = useState<Theme>(() => {
    try { return (sessionStorage.getItem('observer.theme') as Theme) || 'auto'; } catch { return 'auto'; }
  });
  const logout = useCallback(() => {
    apiRef.current?.dispose?.();
    try { sessionStorage.removeItem(SESSION_KEY_STORAGE); } catch { /* session-only fallback */ }
    setKey('');
    setLoginKey('');
    setDirty(false);
    setDestination(null);
  }, []);
  const api = useMemo(() => createApiClient({
    getKey: () => key || null,
    onUnauthorized: logout,
  }), [key, logout]);
  apiRef.current = api;
  useEffect(() => () => api.dispose?.(), [api]);
  useEffect(() => () => loginController.current?.abort(), []);
  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [dirty]);

  function navigate(next: Tab | 'logout') {
    if (dirty) { setDestination(next); return; }
    if (next === 'logout') logout(); else setTab(next);
  }
  async function login(event: React.FormEvent) {
    event.preventDefault();
    if (loginController.current) return;
    const entered = loginKey.trim();
    if (!entered) { setLoginError('请输入 CPA 管理密钥'); return; }
    const controller = new AbortController();
    loginController.current = controller;
    const temporary = createApiClient({ getKey: () => entered });
    setLoginBusy(true);
    setLoginError('');
    try {
      await temporary.request('/health', { signal: controller.signal });
      if (controller.signal.aborted) return;
      try { sessionStorage.setItem(SESSION_KEY_STORAGE, entered); } catch { /* keep only in memory */ }
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
      <Select value={theme} onChange={value => {
        setTheme(value as Theme);
        try {
          if (value === 'auto') sessionStorage.removeItem('observer.theme');
          else sessionStorage.setItem('observer.theme', value);
        } catch { /* unavailable storage does not block the session */ }
        window.dispatchEvent(new CustomEvent('observer:theme-change', { detail: value }));
      }} aria-label="外观" options={[
        { value: 'auto', label: '跟随系统' }, { value: 'light', label: '暖灰' },
        { value: 'white', label: '纯白' }, { value: 'dark', label: '深色' },
      ]} />
    </div>
  );
  if (!key) return (
    <main className="cpa-app-layout cpa-login-layout">
      <section className="cpa-card cpa-login-card">
        <h1 className="cpa-header-title">登录 Observer</h1>
        <p className="cpa-header-subtitle">输入 CPA 管理密钥。仅在当前标签页的会话中保存。</p>
        {loginError && <Notice tone="danger">{loginError}</Notice>}
        <form onSubmit={login}>
          <Field label="管理密钥" htmlFor="login-key">
            <Input id="login-key" type="password" autoComplete="off" value={loginKey}
              onChange={event => setLoginKey(event.target.value)} required />
          </Field>
          <Button type="submit" variant="primary" loading={loginBusy}>登录</Button>
        </form>
        {themeControl}
      </section>
    </main>
  );
  const Page = tab === 'keys' ? KeysPage : tab === 'settings' ? SettingsPage : RequestsPage;
  return (
    <div className="cpa-app-layout">
      <header className="cpa-header">
        <span className="cpa-header-brand cpa-standalone-brand">Observer</span>
        <div className="cpa-header-actions">
          {themeControl}
          <Button variant="ghost" onClick={() => setInterval(value => value ? 0 : 5)}
            aria-pressed={interval > 0} title="自动刷新，每 5 秒一次">
            {interval === 0 ? '自动刷新已关' : dirty ? '编辑中，暂停刷新' : '每 5 秒刷新'}
          </Button>
          <Button variant="ghost" onClick={() => navigate('logout')}>退出</Button>
        </div>
      </header>
      <nav className="cpa-tabs" aria-label="页面标签">
        {tabs.map(item => (
          <button key={item.id} type="button" className={`cpa-tab-item ${tab === item.id ? 'is-active' : ''}`}
            aria-current={tab === item.id ? 'page' : undefined}
            onClick={() => { if (item.id !== tab) navigate(item.id); }}>{item.label}</button>
        ))}
      </nav>
      <main id="content">
        <Page key={tab} api={api} refreshInterval={interval} onDirtyChange={setDirty} />
      </main>
      {destination && (
        <Modal title="放弃未保存的修改？" onClose={() => setDestination(null)} footer={<>
          <Button onClick={() => setDestination(null)}>继续编辑</Button>
          <Button variant="danger" onClick={() => {
            const next = destination;
            setDirty(false);
            setDestination(null);
            if (next === 'logout') logout(); else setTab(next);
          }}>放弃修改</Button>
        </>}>
          <p>当前页面的修改尚未保存。离开后，这些修改将丢失。</p>
        </Modal>
      )}
    </div>
  );
}
