import React, { useState, useCallback, useMemo } from 'react';
import type { ApiClient, PageProps } from './lib/api';
import { createApiClient } from './lib/api';
import { KeysPage } from './pages/KeysPage';
import { Button, Input, Field, Notice } from './components/ui';

export type TabKey = 'requests' | 'keys' | 'settings';

export interface AppProps {
  api?: ApiClient;
  RequestsPage?: React.ComponentType<PageProps>;
  SettingsPage?: React.ComponentType<PageProps>;
}

const SESSION_KEY_NAME = 'observer.management-key';

export function App({ api: customApi, RequestsPage: CustomRequestsPage, SettingsPage: CustomSettingsPage }: AppProps) {
  const [managementKey, setManagementKey] = useState<string>(() => {
    try {
      if (typeof window !== 'undefined' && window.sessionStorage) {
        return window.sessionStorage.getItem(SESSION_KEY_NAME) || '';
      }
    } catch (_) {
      // Storage access blocked
    }
    return '';
  });

  const [activeTab, setActiveTab] = useState<TabKey>('keys');
  const [refreshInterval, setRefreshInterval] = useState<number>(5);
  const [isDirty, setIsDirty] = useState<boolean>(false);
  const [loginInput, setLoginInput] = useState<string>('');
  const [loginError, setLoginError] = useState<string>('');
  const [loginLoading, setLoginLoading] = useState<boolean>(false);

  const handleLogout = useCallback(() => {
    setManagementKey('');
    setLoginInput('');
    setLoginError('');
    setIsDirty(false);
    try {
      if (typeof window !== 'undefined' && window.sessionStorage) {
        window.sessionStorage.removeItem(SESSION_KEY_NAME);
      }
    } catch (_) {
      // Storage access blocked
    }
  }, []);

  const api = useMemo<ApiClient>(() => {
    if (customApi) {
      return customApi;
    }
    return createApiClient({
      getKey: () => managementKey || null,
      onUnauthorized: () => {
        handleLogout();
      },
    });
  }, [customApi, managementKey, handleLogout]);

  const handleLoginSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const key = loginInput.trim();
    if (!key) {
      setLoginError('请输入管理密钥');
      return;
    }

    setLoginLoading(true);
    setLoginError('');

    try {
      // Verify key by checking health or keys endpoint
      const tempApi = createApiClient({
        getKey: () => key,
      });
      await tempApi.request('/health');

      // Key valid
      try {
        if (typeof window !== 'undefined' && window.sessionStorage) {
          window.sessionStorage.setItem(SESSION_KEY_NAME, key);
        }
      } catch (_) {
        // Storage access blocked
      }
      setManagementKey(key);
      setLoginInput('');
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      setLoginError(msg || '管理密钥验证失败，请重试');
    } finally {
      setLoginLoading(false);
    }
  };

  const handleDirtyChange = useCallback((dirty: boolean) => {
    setIsDirty(dirty);
  }, []);

  // Standalone theme toggle
  const [themeChoice, setThemeChoice] = useState<'light' | 'white' | 'dark'>(() => {
    try {
      if (typeof window !== 'undefined') {
        const t = document.documentElement.getAttribute('data-theme');
        if (t === 'dark' || t === 'white' || t === 'light') return t;
      }
    } catch (_) {}
    return 'light';
  });

  const handleThemeChange = (nextTheme: 'light' | 'white' | 'dark') => {
    setThemeChoice(nextTheme);
    try {
      document.documentElement.setAttribute('data-theme', nextTheme);
      sessionStorage.setItem('observer.theme', nextTheme);
    } catch (_) {}
  };

  // If not authenticated, render Login Screen
  if (!managementKey) {
    return (
      <main className="cpa-app-layout" style={{ justifyContent: 'center', alignItems: 'center' }}>
        <div className="cpa-card" style={{ width: '100%', maxWidth: '420px', marginTop: '4rem' }}>
          <div style={{ marginBottom: '1.5rem', textAlign: 'center' }}>
            <h1 className="cpa-header-title" style={{ fontSize: '1.4rem' }}>Observer 管理登录</h1>
            <p className="cpa-header-subtitle" style={{ marginTop: '0.4rem' }}>
              使用同源 CPA 管理密钥；仅保存在本次标签页会话。
            </p>
          </div>

          {loginError && <Notice tone="danger">{loginError}</Notice>}

          <form onSubmit={handleLoginSubmit} id="login">
            <Field label="管理密钥" htmlFor="login-key">
              <Input
                id="login-key"
                type="password"
                autoComplete="off"
                required
                value={loginInput}
                onChange={(e) => setLoginInput(e.target.value)}
                placeholder="请输入 CPA 管理密钥"
              />
            </Field>

            <div style={{ marginTop: '1.25rem' }}>
              <Button
                type="submit"
                variant="primary"
                loading={loginLoading}
                style={{ width: '100%' }}
              >
                登录
              </Button>
            </div>
          </form>
        </div>
      </main>
    );
  }

  const effectivePollInterval = isDirty ? 0 : refreshInterval;

  return (
    <div className="cpa-app-layout">
      <header className="cpa-header">
        <div className="cpa-header-brand">
          <h1 className="cpa-header-title">Observer</h1>
          <span className="cpa-header-subtitle">Key 与用量管理</span>
        </div>

        <div className="cpa-header-actions">
          {/* Duplicate theme controls hidden in embedded mode */}
          <div className="cpa-theme-control cpa-theme-switcher" style={{ display: 'flex', gap: '0.25rem' }}>
            <Button
              variant={themeChoice === 'light' ? 'primary' : 'ghost'}
              onClick={() => handleThemeChange('light')}
              title="切换为暖灰浅色"
            >
              暖灰
            </Button>
            <Button
              variant={themeChoice === 'white' ? 'primary' : 'ghost'}
              onClick={() => handleThemeChange('white')}
              title="切换为纯白"
            >
              纯白
            </Button>
            <Button
              variant={themeChoice === 'dark' ? 'primary' : 'ghost'}
              onClick={() => handleThemeChange('dark')}
              title="切换为深色"
            >
              深色
            </Button>
          </div>

          <Button
            variant="ghost"
            onClick={() => setRefreshInterval((prev) => (prev > 0 ? 0 : 5))}
            title={refreshInterval > 0 ? '暂停轮询' : '开启轮询 (5s)'}
          >
            {refreshInterval > 0 ? (isDirty ? '轮询暂停(编辑中)' : '轮询 5s') : '轮询已关'}
          </Button>

          <Button variant="secondary" onClick={handleLogout} title="退出当前管理会话">
            退出
          </Button>
        </div>
      </header>

      {/* Navigation Tabs */}
      <nav className="cpa-tabs" aria-label="页面标签">
        <button
          type="button"
          className={`cpa-tab-item ${activeTab === 'requests' ? 'is-active' : ''}`}
          onClick={() => setActiveTab('requests')}
        >
          请求日志
        </button>
        <button
          type="button"
          className={`cpa-tab-item ${activeTab === 'keys' ? 'is-active' : ''}`}
          onClick={() => setActiveTab('keys')}
        >
          Key 管理
        </button>
        <button
          type="button"
          className={`cpa-tab-item ${activeTab === 'settings' ? 'is-active' : ''}`}
          onClick={() => setActiveTab('settings')}
        >
          设置
        </button>
      </nav>

      {/* Tab Panels */}
      <main id="content">
        {activeTab === 'keys' && (
          <KeysPage
            api={api}
            refreshInterval={effectivePollInterval}
            onDirtyChange={handleDirtyChange}
          />
        )}

        {activeTab === 'requests' && (
          CustomRequestsPage ? (
            <CustomRequestsPage
              api={api}
              refreshInterval={effectivePollInterval}
              onDirtyChange={handleDirtyChange}
            />
          ) : (
            <div className="cpa-card" style={{ textAlign: 'center', padding: '3rem' }}>
              <p style={{ color: 'var(--text-secondary)' }}>请求日志切片等待集成</p>
            </div>
          )
        )}

        {activeTab === 'settings' && (
          CustomSettingsPage ? (
            <CustomSettingsPage
              api={api}
              refreshInterval={effectivePollInterval}
              onDirtyChange={handleDirtyChange}
            />
          ) : (
            <div className="cpa-card" style={{ textAlign: 'center', padding: '3rem' }}>
              <p style={{ color: 'var(--text-secondary)' }}>设置切片等待集成</p>
            </div>
          )
        )}
      </main>
    </div>
  );
}

export default App;
