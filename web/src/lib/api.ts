import type { ModelInfo } from './types';

export interface ApiClient {
  request<T>(path: string, options?: {
    method?: string; body?: unknown; signal?: AbortSignal;
  }): Promise<T>;
  models(options?: { signal?: AbortSignal; force?: boolean }): Promise<ModelInfo[]>;
  dispose?(): void;
}

export interface PageProps {
  api: ApiClient;
  refreshInterval?: number; // Requests only: seconds; zero disables its timer.
  onDirtyChange: (dirty: boolean) => void;
}

export interface ApiClientOptions {
  basePath?: string;
  getKey?: () => string | null;
  onUnauthorized?: () => void;
}

export const SESSION_KEY_STORAGE = 'observer.management-key';
export const LOCAL_KEY_STORAGE = 'observer.management-key';

export function getStoredManagementKey(): { key: string | null; isPersistent: boolean } {
  try {
    const local = localStorage.getItem(LOCAL_KEY_STORAGE);
    if (local) return { key: local, isPersistent: true };
  } catch { /* storage may be disabled */ }
  try {
    const session = sessionStorage.getItem(SESSION_KEY_STORAGE);
    if (session) return { key: session, isPersistent: false };
  } catch { /* storage may be disabled */ }
  return { key: null, isPersistent: false };
}

export function clearStoredManagementCredentials(): void {
  try { localStorage.removeItem(LOCAL_KEY_STORAGE); } catch { /* storage may be disabled */ }
  try { sessionStorage.removeItem(SESSION_KEY_STORAGE); } catch { /* storage may be disabled */ }
}

const unauthorizedMessage = '未授权或管理密钥无效 (401)';
const errorMessages: Record<string, string> = {
  conflict: '该客户端密钥已存在，请使用不同的密钥',
  busy: '当前仍有请求在处理，请稍后重试',
  unavailable: 'Observer 暂不可用，请稍后重试',
  accounting_unavailable: '用量记录暂时异常，请先检查存储状态',
};

export class ApiError extends Error {
  constructor(public readonly status: number, public readonly code: string, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

export function deriveBasePath(pathname = window.location.pathname): string {
  return pathname.replace(/\/ui(?:\.html)?\/?$/, '')
    .replace('/resource/plugins/', '/management/plugins/');
}

function storedKey(): string | null {
  return getStoredManagementKey().key;
}

/** Each client owns exactly one session, its requests and its in-memory catalog. */
export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  const getKey = options.getKey ?? storedKey;
  const sessionKey = getKey();
  const controllers = new Set<AbortController>();
  let closed = false;
  let catalogGeneration = 0;
  let cache: { items: ModelInfo[]; time: number } | null = null;

  function dispose() {
    closed = true;
    cache = null;
    catalogGeneration++;
    for (const controller of controllers) controller.abort();
    controllers.clear();
  }

  function assertActive(signal?: AbortSignal) {
    if (signal?.aborted) throw new DOMException('请求已取消', 'AbortError');
    if (closed || getKey() !== sessionKey) throw new Error('管理会话已结束，请重新登录');
  }

  // Keep a controller alive until body parsing finishes, not just until headers
  // arrive: logout/401 must cancel slow catalog streams and late JSON responses.
  async function execute<T>(
    url: string, init: RequestInit, signal: AbortSignal | undefined,
    consume: (response: Response, signal: AbortSignal) => Promise<T>,
    management = true,
  ): Promise<T> {
    assertActive(signal);
    const controller = new AbortController();
    const abort = () => controller.abort();
    signal?.addEventListener('abort', abort, { once: true });
    controllers.add(controller);
    try {
      assertActive(signal);
      const response = await fetch(url, {
        ...init, signal: controller.signal, cache: 'no-store',
        mode: 'same-origin', credentials: 'omit', redirect: 'error',
      });
      assertActive(controller.signal);
      if (response.status === 401 && management) {
        dispose();
        // An old client cannot remove credentials belonging to a new session.
        try {
          if (storedKey() === sessionKey) clearStoredManagementCredentials();
        } catch { /* storage may be disabled */ }
        options.onUnauthorized?.();
        throw new ApiError(401, 'unauthorized', unauthorizedMessage);
      }
      if (!response.ok) {
        let code = '';
        let message = `请求失败（HTTP ${response.status}）`;
        try {
          const data = await response.json();
          const detail = typeof data?.error === 'object' ? data.error : data;
          if (typeof detail?.code === 'string') code = detail.code;
          if (typeof detail?.message === 'string') message = detail.message;
          else if (typeof data?.error === 'string') message = data.error;
        } catch { /* the status is sufficient for non-JSON failures */ }
        assertActive(controller.signal);
        throw new ApiError(response.status, code, errorMessages[code] ?? message);
      }
      const result = await consume(response, controller.signal);
      assertActive(controller.signal);
      return result;
    } finally {
      signal?.removeEventListener('abort', abort);
      controllers.delete(controller);
    }
  }

  const client: ApiClient = {
    dispose,
    request<T>(path: string, opts: { method?: string; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
      const headers: Record<string, string> = { Accept: 'application/json' };
      if (sessionKey) headers.Authorization = `Bearer ${sessionKey}`;
      const body = opts.body === undefined ? undefined : JSON.stringify(opts.body);
      if (body !== undefined) headers['Content-Type'] = 'application/json';
      const base = options.basePath ?? deriveBasePath();
      return execute(`${base}${path.startsWith('/') ? path : `/${path}`}`, {
        method: opts.method ?? (body === undefined ? 'GET' : 'POST'), headers, body,
      }, opts.signal, async response => {
        if (response.status === 204) return {} as T;
        return response.json() as Promise<T>;
      });
    },
    async models(opts = {}) {
      assertActive(opts.signal);
      if (!opts.force && cache && Date.now() - cache.time < 300_000) {
        return cache.items.map(item => ({ ...item }));
      }
      const generation = ++catalogGeneration;
      // Capability tokens exist only in memory and are sent only to /v1/models.
      const capability = await client.request<{ token: string }>('/models/token', {
        method: 'POST', signal: opts.signal,
      });
      const token = capability.token;
      capability.token = '';
      if (typeof token !== 'string' || !token) throw new Error('无法获取模型目录凭据，请重试');
      const items = await execute('/v1/models', {
        headers: { Authorization: `Bearer ${token}` },
      }, opts.signal, async (response, signal) => {
        const reader = response.body?.getReader();
        if (!reader) throw new Error('无法读取模型目录');
        const decoder = new TextDecoder();
        let text = '', size = 0;
        try {
          while (true) {
            assertActive(signal);
            const { done, value } = await reader.read();
            if (done) break;
            size += value.byteLength;
            if (size > 2 * 1024 * 1024) {
              await reader.cancel();
              throw new Error('模型目录超过 2 MiB 上限，请手动添加精确 ID');
            }
            text += decoder.decode(value, { stream: true });
          }
          text += decoder.decode();
        } finally { reader.releaseLock(); }
        const data = JSON.parse(text);
        if (!Array.isArray(data?.data)) throw new Error('模型目录响应格式无效');
        const seen = new Set<string>();
        const result: ModelInfo[] = [];
        for (const row of data.data) {
          // Preserve exact identities; owned_by is metadata, never a price provider.
          if (typeof row?.id !== 'string' || !row.id || row.id.length > 1024 || seen.has(row.id)) continue;
          seen.add(row.id);
          result.push({ id: row.id, ...(typeof row.owned_by === 'string' ? { owned_by: row.owned_by } : {}) });
        }
        return result;
      }, false);
      if (generation === catalogGeneration) cache = { items, time: Date.now() };
      return items.map(item => ({ ...item }));
    },
  };
  return client;
}
