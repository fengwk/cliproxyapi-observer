import type { ModelInfo } from './types';

export interface ApiClient {
  request<T>(
    path: string,
    options?: {
      method?: string;
      body?: unknown;
      signal?: AbortSignal;
    }
  ): Promise<T>;
  models(options?: { signal?: AbortSignal; force?: boolean }): Promise<ModelInfo[]>;
}

export interface PageProps {
  api: ApiClient;
  refreshInterval: number; // seconds, 0 disables polling
  onDirtyChange: (dirty: boolean) => void;
}

export interface ApiClientOptions {
  basePath?: string;
  getKey?: () => string | null;
  onUnauthorized?: () => void;
}

const SESSION_KEY_STORAGE = 'observer.management-key';
const MODEL_CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

/**
 * Derives the plugin management API base path from the current page pathname.
 */
export function deriveBasePath(pathname?: string): string {
  const path = pathname ?? (typeof window !== 'undefined' ? window.location.pathname : '');
  return path
    .replace(/\/ui(?:\.html)?\/?$/, '')
    .replace('/resource/plugins/', '/management/plugins/');
}

/**
 * Creates an ApiClient instance.
 */
export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  let modelCache: { items: ModelInfo[]; timestamp: number } | null = null;

  const getManagementKey = (): string | null => {
    if (options.getKey) {
      return options.getKey();
    }
    try {
      if (typeof window !== 'undefined' && window.sessionStorage) {
        return window.sessionStorage.getItem(SESSION_KEY_STORAGE);
      }
    } catch (_) {
      // Storage access blocked
    }
    return null;
  };

  const clearSession = () => {
    modelCache = null;
    try {
      if (typeof window !== 'undefined' && window.sessionStorage) {
        window.sessionStorage.removeItem(SESSION_KEY_STORAGE);
      }
    } catch (_) {
      // Storage access blocked
    }
    if (options.onUnauthorized) {
      options.onUnauthorized();
    }
  };

  const client: ApiClient = {
    async request<T>(path: string, opts: { method?: string; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
      const base = options.basePath !== undefined ? options.basePath : deriveBasePath();
      const normalizedPath = path.startsWith('/') ? path : `/${path}`;
      const url = `${base}${normalizedPath}`;

      const headers: Record<string, string> = {
        Accept: 'application/json',
      };

      const key = getManagementKey();
      if (key) {
        headers['Authorization'] = `Bearer ${key}`;
      }

      let reqBody: string | undefined;
      if (opts.body !== undefined && opts.body !== null) {
        headers['Content-Type'] = 'application/json';
        reqBody = JSON.stringify(opts.body);
      }

      const response = await fetch(url, {
        method: opts.method || (reqBody ? 'POST' : 'GET'),
        headers,
        body: reqBody,
        signal: opts.signal,
        cache: 'no-store',
        mode: 'same-origin',
        credentials: 'omit',
      });

      if (response.status === 401) {
        clearSession();
        throw new Error('未授权或管理密钥无效 (401)');
      }

      if (!response.ok) {
        let errMessage = `HTTP ${response.status} ${response.statusText}`;
        try {
          const errData = await response.json();
          if (errData && typeof errData === 'object') {
            if (typeof errData.error === 'string') {
              errMessage = errData.error;
            } else if (typeof errData.message === 'string') {
              errMessage = errData.message;
            }
          }
        } catch (_) {
          // Non-JSON error body
        }
        throw new Error(errMessage);
      }

      if (response.status === 204) {
        return {} as T;
      }

      const contentType = response.headers.get('content-type') || '';
      if (contentType.includes('application/json')) {
        return (await response.json()) as T;
      }
      return (await response.text()) as unknown as T;
    },

    async models(opts: { signal?: AbortSignal; force?: boolean } = {}): Promise<ModelInfo[]> {
      const now = Date.now();
      if (!opts.force && modelCache && now - modelCache.timestamp < MODEL_CACHE_TTL_MS) {
        return modelCache.items;
      }

      let capability: { token?: string } | null = null;
      try {
        capability = await client.request<{ token: string }>('/models/token', {
          method: 'POST',
          signal: opts.signal,
        });

        if (!capability || typeof capability.token !== 'string' || !capability.token) {
          throw new Error('获取模型目录凭据失败');
        }

        const token = capability.token;
        capability.token = ''; // Clear immediately

        const response = await fetch('/v1/models', {
          method: 'GET',
          signal: opts.signal,
          cache: 'no-store',
          mode: 'same-origin',
          credentials: 'omit',
          redirect: 'error',
          headers: {
            Authorization: `Bearer ${token}`,
          },
        });

        if (!response.ok) {
          throw new Error('权威模型目录读取失败，请重试或手动输入精确 ID');
        }

        // Bound to 2 MiB
        const reader = response.body?.getReader();
        if (!reader) {
          throw new Error('无法读取模型响应流');
        }

        const decoder = new TextDecoder();
        let text = '';
        let bytes = 0;
        try {
          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            bytes += value.byteLength;
            if (bytes > 2 * 1024 * 1024) {
              await reader.cancel();
              throw new Error('模型目录超过 2 MiB 上限，请手动添加精确 ID');
            }
            text += decoder.decode(value, { stream: true });
          }
          text += decoder.decode();
        } finally {
          reader.releaseLock();
        }

        const json = JSON.parse(text);
        if (!Array.isArray(json.data)) {
          throw new Error('模型目录响应结构无效');
        }

        const items: ModelInfo[] = json.data
          .filter((m: unknown): m is { id: string; owned_by?: string } =>
            typeof m === 'object' && m !== null && typeof (m as Record<string, unknown>).id === 'string'
          )
          .map((m: { id: string; owned_by?: string }) => ({
            id: m.id,
            owned_by: m.owned_by,
          }));

        modelCache = {
          items,
          timestamp: Date.now(),
        };

        return items;
      } catch (err) {
        if (opts.signal?.aborted) {
          throw err;
        }
        throw err;
      } finally {
        if (capability) {
          capability.token = '';
          capability = null;
        }
      }
    },
  };

  return client;
}
