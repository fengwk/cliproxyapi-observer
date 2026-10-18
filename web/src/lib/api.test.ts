import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { deriveBasePath, createApiClient, ApiError, SESSION_KEY_STORAGE } from './api';

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

  describe('request & session lifecycle', () => {
    it('attaches Bearer token and sends JSON body', async () => {
      sessionStorage.setItem(SESSION_KEY_STORAGE, 'secret-123');

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

    /**
     * 行为回归：401 中止其他在途请求、清空缓存、拒绝后续请求
     */
    it('401 aborts other in-flight requests, invalidates cache, and rejects disposed client', async () => {
      sessionStorage.setItem(SESSION_KEY_STORAGE, 'mgmt-key');
      const onUnauthorized = vi.fn();

      let slowSignal: AbortSignal | null = null;
      let rejectSlow: (err: unknown) => void;
      const slowPromise = new Promise((_, reject) => {
        rejectSlow = reject;
      });

      const mockFetch = vi.fn().mockImplementation((url, init) => {
        if (url.includes('/slow')) {
          slowSignal = init.signal;
          init.signal.addEventListener('abort', () => {
            rejectSlow(new DOMException('Aborted', 'AbortError'));
          });
          return slowPromise;
        }
        if (url.includes('/fail-401')) {
          return Promise.resolve({
            ok: false,
            status: 401,
            headers: new Headers(),
          });
        }
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', onUnauthorized });

      // 启动一个长时间在途的慢请求
      const slowReq = client.request('/slow');

      // 启动一个返回 401 的管理请求
      await expect(client.request('/fail-401')).rejects.toThrow('未授权或管理密钥无效 (401)');

      // 验证在途慢请求的 AbortController 被 401 dispose 中止
      expect(slowSignal).not.toBeNull();
      expect(slowSignal!.aborted).toBe(true);
      await expect(slowReq).rejects.toThrow();

      // 验证 onUnauthorized 被调用，sessionStorage 被清除
      expect(onUnauthorized).toHaveBeenCalledTimes(1);
      expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBeNull();

      // 验证已 disposed 的客户端再次发起请求直接被拦截拒绝，不再调用 fetch
      mockFetch.mockClear();
      await expect(client.request('/keys')).rejects.toThrow('管理会话已结束，请重新登录');
      expect(mockFetch).not.toHaveBeenCalled();
    });

    /**
     * 行为回归：新 session 不受旧 client 401 影响（old getKey changed must reject, 不 logout new storage）
     */
    it('does not remove new session credentials when old client receives 401', async () => {
      // 模拟旧 client 持有 'old-token'
      sessionStorage.setItem(SESSION_KEY_STORAGE, 'old-token');
      const onUnauthorizedOld = vi.fn();

      let resolve401: (val: unknown) => void;
      const pending401 = new Promise((resolve) => {
        resolve401 = resolve;
      });

      const mockFetch = vi.fn().mockImplementation(() => pending401);
      global.fetch = mockFetch;

      const oldClient = createApiClient({
        basePath: '/mgmt',
        getKey: () => 'old-token',
        onUnauthorized: onUnauthorizedOld,
      });

      // 旧 client 发起请求，此时挂起
      const reqOld = oldClient.request('/keys');

      // 此时用户在另一个页面或重新登录，sessionStorage 写入了 'new-token'
      sessionStorage.setItem(SESSION_KEY_STORAGE, 'new-token');

      // 旧请求终于返回 401
      resolve401!({
        ok: false,
        status: 401,
        headers: new Headers(),
      });

      await expect(reqOld).rejects.toThrow('未授权或管理密钥无效 (401)');
      expect(onUnauthorizedOld).toHaveBeenCalledTimes(1);

      // 核心断言：由于 sessionStorage 中的 key 已经属于新 session ('new-token')，旧 client 401 绝对不能清除它！
      expect(sessionStorage.getItem(SESSION_KEY_STORAGE)).toBe('new-token');
    });

    /**
     * 行为回归：getKey 发生变化时，旧 client 的后续请求被拒绝
     */
    it('rejects requests from client when getKey returns changed token', async () => {
      let currentKey = 'token-v1';
      const client = createApiClient({
        basePath: '/mgmt',
        getKey: () => currentKey,
      });

      const mockFetch = vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        headers: new Headers({ 'content-type': 'application/json' }),
        json: async () => ({ ok: true }),
      });
      global.fetch = mockFetch;

      // 第一次请求使用 token-v1 成功
      await expect(client.request('/test')).resolves.toEqual({ ok: true });

      // key 发生了变更
      currentKey = 'token-v2';

      // 旧 client 再次发起请求必须直接拒绝
      await expect(client.request('/test')).rejects.toThrow('管理会话已结束，请重新登录');
    });

    /**
     * 行为回归：ApiError 解析 nested { error: { code, message } } 并保留映射与 status/code
     */
    it('parses nested error bodies and preserves mapped messages and status code without [object Object]', async () => {
      const mockFetch = vi.fn().mockImplementation((url: string) => {
        if (url.includes('/conflict-err')) {
          return Promise.resolve({
            ok: false,
            status: 409,
            json: async () => ({
              error: {
                code: 'conflict',
                message: 'Internal collision',
              },
            }),
          });
        }
        if (url.includes('/busy-err')) {
          return Promise.resolve({
            ok: false,
            status: 503,
            json: async () => ({
              error: {
                code: 'busy',
                message: 'System is loaded',
              },
            }),
          });
        }
        if (url.includes('/nested-unknown')) {
          return Promise.resolve({
            ok: false,
            status: 400,
            json: async () => ({
              error: {
                code: 'custom_error_code',
                message: 'Custom detailed reason',
              },
            }),
          });
        }
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', getKey: () => 'valid-key' });

      // 1. Conflict 映射
      try {
        await client.request('/conflict-err');
        expect.unreachable('Should fail');
      } catch (err: unknown) {
        expect(err).toBeInstanceOf(ApiError);
        const apiErr = err as ApiError;
        expect(apiErr.status).toBe(409);
        expect(apiErr.code).toBe('conflict');
        expect(apiErr.message).toBe('该客户端密钥已存在，请使用不同的密钥');
        expect(apiErr.message).not.toContain('[object Object]');
      }

      // 2. Busy 映射
      try {
        await client.request('/busy-err');
        expect.unreachable('Should fail');
      } catch (err: unknown) {
        expect(err).toBeInstanceOf(ApiError);
        const apiErr = err as ApiError;
        expect(apiErr.status).toBe(503);
        expect(apiErr.code).toBe('busy');
        expect(apiErr.message).toBe('当前仍有请求在处理，请稍后重试');
      }

      // 3. Nested custom message (没有内置映射但有详细说明)
      try {
        await client.request('/nested-unknown');
        expect.unreachable('Should fail');
      } catch (err: unknown) {
        expect(err).toBeInstanceOf(ApiError);
        const apiErr = err as ApiError;
        expect(apiErr.status).toBe(400);
        expect(apiErr.code).toBe('custom_error_code');
        expect(apiErr.message).toBe('Custom detailed reason');
        expect(apiErr.message).not.toContain('[object Object]');
      }
    });
  });

  describe('models catalog discovery and cache', () => {
    it('uses 5-minute memory cache and honors force bypass', async () => {
      sessionStorage.setItem(SESSION_KEY_STORAGE, 'mgmt-key');

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

    /**
     * 行为回归：capability token 仅发送至 /v1/models Header，不出现在 URL 或存储中
     */
    it('capability token exists only in memory and is sent only in Authorization header to /v1/models', async () => {
      const mockFetch = vi.fn().mockImplementation((url, init) => {
        if (url.includes('/models/token')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            json: async () => ({ token: 'secret-token-xyz-999' }),
          });
        }
        if (url.includes('/v1/models')) {
          expect(url).toBe('/v1/models'); // 没有 token query 参数
          expect(init.headers['Authorization']).toBe('Bearer secret-token-xyz-999');
          // 确保 sessionStorage 没有任何 capability token
          expect(sessionStorage.getItem('secret-token-xyz-999')).toBeNull();

          const bodyText = JSON.stringify({ data: [{ id: 'gpt-4o' }] });
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
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', getKey: () => 'mgmt-key' });
      const models = await client.models();
      expect(models).toEqual([{ id: 'gpt-4o' }]);
    });

    /**
     * 行为回归：catalog 流读取超过 2 MiB 时报错并调用 cancel
     */
    it('enforces 2 MiB stream limit on models catalog', async () => {
      let cancelCalled = false;
      const mockFetch = vi.fn().mockImplementation((url) => {
        if (url.includes('/models/token')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            json: async () => ({ token: 'tok-123' }),
          });
        }
        if (url.includes('/v1/models')) {
          // 提供一块 2.5 MiB 的数据
          const chunk = new Uint8Array(2.5 * 1024 * 1024);
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            body: {
              getReader: () => ({
                read: async () => ({ done: false, value: chunk }),
                cancel: async () => {
                  cancelCalled = true;
                },
                releaseLock: () => {},
              }),
            },
          });
        }
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', getKey: () => 'mgmt-key' });
      await expect(client.models()).rejects.toThrow('模型目录超过 2 MiB 上限，请手动添加精确 ID');
      expect(cancelCalled).toBe(true);
    });

    /**
     * 行为回归：去重 exact ids、保留 owned_by metadata、不推导 provider，并返回 clone
     */
    it('preserves exact ids and owned_by metadata without provider inference, and returns cloned items', async () => {
      const mockFetch = vi.fn().mockImplementation((url) => {
        if (url.includes('/models/token')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            json: async () => ({ token: 'tok' }),
          });
        }
        if (url.includes('/v1/models')) {
          const bodyText = JSON.stringify({
            data: [
              { id: 'meta-llama/Llama-3-70b-instruct', owned_by: 'meta' },
              { id: 'meta-llama/Llama-3-70b-instruct', owned_by: 'duplicate-meta' }, // 重复项去重
              { id: 'custom-model-without-owned' },
              { id: 'x'.repeat(1025) }, // 超出 1024 过滤
              { invalid: true }, // 无效格式过滤
            ],
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
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', getKey: () => 'mgmt-key' });
      const list1 = await client.models();

      expect(list1).toEqual([
        { id: 'meta-llama/Llama-3-70b-instruct', owned_by: 'meta' },
        { id: 'custom-model-without-owned' },
      ]);
      // 绝对不推导 provider 属性
      expect((list1[0] as unknown as Record<string, unknown>).provider).toBeUndefined();

      // 验证返回的是 clone：修改 list1 不污染缓存
      list1[0].id = 'mutated-id';
      const list2 = await client.models();
      expect(list2[0].id).toBe('meta-llama/Llama-3-70b-instruct');
    });

    /**
     * 行为回归：delayed old catalog response 不能 override 新 catalog 的缓存 (generation 守卫)
     */
    it('prevents delayed old catalog response from overriding newer cache via generation guard', async () => {
      let resolveFirstCatalog: (val: unknown) => void;
      const firstCatalogPromise = new Promise((resolve) => {
        resolveFirstCatalog = resolve;
      });

      let callCount = 0;
      const mockFetch = vi.fn().mockImplementation((url) => {
        if (url.includes('/models/token')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            headers: new Headers({ 'content-type': 'application/json' }),
            json: async () => ({ token: 'tok' }),
          });
        }
        if (url.includes('/v1/models')) {
          callCount++;
          if (callCount === 1) {
            return firstCatalogPromise;
          }
          const bodyText = JSON.stringify({ data: [{ id: 'new-model-v2' }] });
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
        return Promise.reject(new Error('Unknown url'));
      });
      global.fetch = mockFetch;

      const client = createApiClient({ basePath: '/mgmt', getKey: () => 'mgmt-key' });

      // 第 1 次请求：挂起
      const p1 = client.models();

      // 第 2 次请求带 force: true，递增了 catalogGeneration，并且迅速完成并写入缓存
      const p2 = client.models({ force: true });
      const res2 = await p2;
      expect(res2).toEqual([{ id: 'new-model-v2' }]);

      // 此时第 1 次旧请求终于返回了旧数据
      const oldBodyText = JSON.stringify({ data: [{ id: 'old-model-v1' }] });
      resolveFirstCatalog!({
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
                  return { done: false, value: new TextEncoder().encode(oldBodyText) };
                }
                return { done: true, value: undefined };
              },
              cancel: async () => {},
              releaseLock: () => {},
            };
          },
        },
      });

      const res1 = await p1;
      expect(res1).toEqual([{ id: 'old-model-v1' }]);

      // 核心断言：由于 generation 不匹配，旧请求绝不能覆盖新请求建立的缓存！
      const cached = await client.models();
      expect(cached).toEqual([{ id: 'new-model-v2' }]);
    });
  });
});
