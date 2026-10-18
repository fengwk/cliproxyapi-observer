import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { deriveBasePath, createApiClient } from './api';

describe('api client', () => {
  beforeEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  afterEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  describe('deriveBasePath', () => {
    it('normalizes various endpoint shapes correctly', () => {
      expect(deriveBasePath('/ui')).toBe('');
      expect(deriveBasePath('/ui/')).toBe('');
      expect(deriveBasePath('/ui.html')).toBe('');
      expect(deriveBasePath('/v0/resource/plugins/cliproxyapi-observer/ui')).toBe(
        '/v0/management/plugins/cliproxyapi-observer'
      );
      expect(deriveBasePath('/v0/resource/plugins/cliproxyapi-observer/ui/')).toBe(
        '/v0/management/plugins/cliproxyapi-observer'
      );
      expect(deriveBasePath('/proxy/v0/resource/plugins/cliproxyapi-observer/ui')).toBe(
        '/proxy/v0/management/plugins/cliproxyapi-observer'
      );
    });
  });

  describe('request', () => {
    it('attaches Bearer token and sends JSON body', async () => {
      sessionStorage.setItem('observer.management-key', 'secret-123');

      const mockFetch = vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        headers: new Headers({ 'content-type': 'application/json' }),
        json: async () => ({ status: 'ok' }),
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt' });
      const res = await client.request<{ status: string }>('/health', {
        method: 'POST',
        body: { check: true },
      });

      expect(res).toEqual({ status: 'ok' });
      expect(mockFetch).toHaveBeenCalledTimes(1);

      const [url, init] = mockFetch.mock.calls[0];
      expect(url).toBe('/mgmt/health');
      expect(init.method).toBe('POST');
      expect(init.headers['Authorization']).toBe('Bearer secret-123');
      expect(init.headers['Content-Type']).toBe('application/json');
      expect(init.body).toBe(JSON.stringify({ check: true }));
    });

    it('handles 401 by clearing session and notifying onUnauthorized', async () => {
      sessionStorage.setItem('observer.management-key', 'invalid-key');
      const onUnauthorized = vi.fn();

      const mockFetch = vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        headers: new Headers(),
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', onUnauthorized });
      await expect(client.request('/keys')).rejects.toThrow('未授权或管理密钥无效 (401)');

      expect(sessionStorage.getItem('observer.management-key')).toBeNull();
      expect(onUnauthorized).toHaveBeenCalledTimes(1);
    });
  });

  describe('models catalog discovery and cache', () => {
    it('uses 5-minute memory cache and honors force bypass', async () => {
      sessionStorage.setItem('observer.management-key', 'mgmt-key');

      let tokenFetchCount = 0;
      let catalogFetchCount = 0;

      const mockFetch = vi.fn().mockImplementation((url) => {
        if (url.includes('/models/token')) {
          tokenFetchCount++;
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            json: async () => ({ token: 'capability-token-' + tokenFetchCount }),
          });
        }
        if (url.includes('/v1/models')) {
          catalogFetchCount++;
          const bodyText = JSON.stringify({
            data: [{ id: 'gpt-4o' }, { id: 'claude-3-5-sonnet' }],
          });
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            body: {
              getReader: () => {
                let sent = false;
                return {
                  read: async () => {
                    if (!sent) {
                      sent = true;
                      return { done: false, value: new TextEncoder().encode(bodyText) };
                    }
                    return { done: true, value: undefined };
                  },
                  cancel: async () => {},
                  releaseLock: () => {},
                };
              },
            },
          });
        }
        return Promise.reject(new Error('Unknown url: ' + url));
      });

      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt' });

      // First call: fetches token and catalog
      const m1 = await client.models();
      expect(m1.map((m) => m.id)).toEqual(['gpt-4o', 'claude-3-5-sonnet']);
      expect(tokenFetchCount).toBe(1);
      expect(catalogFetchCount).toBe(1);

      // Second call: served from 5-minute memory cache
      const m2 = await client.models();
      expect(m2).toEqual(m1);
      expect(tokenFetchCount).toBe(1);
      expect(catalogFetchCount).toBe(1);

      // Third call with force: true -> bypasses cache
      const m3 = await client.models({ force: true });
      expect(m3).toEqual(m1);
      expect(tokenFetchCount).toBe(2);
      expect(catalogFetchCount).toBe(2);
    });
  });
});
