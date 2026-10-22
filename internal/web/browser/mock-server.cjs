'use strict';

/*
 * Observer v0.6.0 浏览器验收 mock 宿主（零 npm 依赖）。
 *
 * 只监听 127.0.0.1，使用假管理密钥、假 Key、假请求和假安全数据；不接触真实 CPA、
 * 上游或生产配置。它实现 DESIGN.md 中 Observer 管理 API 的完整形状（固定
 * 静态路由 + query ID，不注册旧动态路由）：
 *
 *   GET  /health
 *   GET  /keys                       POST /keys
 *   PUT  /keys?id={id}               DELETE /keys?id={id}
 *   POST /keys/rotate?id={id}        POST /keys/reset?id={id}
 *   POST /keys/recover?id={id}
 *   GET  /requests                   GET /requests/detail?id={id}
 *   GET  /summary
 *   GET  /settings                   PUT /settings
 *   GET  /prices                     POST /prices/refresh
 *   POST /compact
 *   GET  /ui, /ui/, /ui.html, /ui.js, /ui.css
 *
 * 模型目录能力（不回退到 auth-files / pricing / 配置目录）：
 *
 *   POST <管理前缀>/models/token   -> {token, expires_at}
 *   GET  /v1/models                -> {data:[{id}]}（仅接受该一次性 capability）
 *
 * 资源与 API 均可通过 /v0/resource/plugins/cliproxyapi-observer 和
 * /v0/management/plugins/cliproxyapi-observer 前缀访问；管理数据要求
 * Authorization: Bearer <管理密钥>。
 *
 * 每次请求都记录到返回对象的 calls 数组（不记录密钥或 Authorization 头内容），
 * 供浏览器脚本断言“详情只在点击行后读取”、筛选参数、CRUD 动作和鉴权行为。
 */

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const shellHelper = require('./shell-helper.cjs');

const PLUGIN_ID = 'cliproxyapi-observer';
const RESOURCE_BASE = '/v0/resource/plugins/' + PLUGIN_ID;
const MANAGEMENT_BASE = '/v0/management/plugins/' + PLUGIN_ID;
const DEFAULT_MANAGEMENT_KEY = 'observer-browser-secret';
const MAX_BODY_BYTES = 1024 * 1024;
// 与引擎/设置 UI 相同的请求体总存储上限（1 GiB）。
const MAX_BODY_STORAGE_BYTES = 1024 * 1024 * 1024;
const CATALOG_PATH = '/v1/models';
const CAPABILITY_TTL_MS = 30 * 1000;
const MAX_OUTSTANDING_CAPABILITIES = 32;
const MAX_CATALOG_MODELS = 100;
const ASSET_DIR = path.resolve(__dirname, '../dist');

const SECRET_PREFIX = 'sk-';

// 回归夹具：9 位纳秒 RFC3339 到期时间；64 位十六进制 Key ID。
// 二者都必须能在 UI 截断/格式化后保留完整 API 值。
const NANO_EXPIRY = '2027-03-04T05:06:07.123456789Z';
const NANO_KEY_ID = 'key-nano';
const LONG_KEY_ID = '0123456789abcdef'.repeat(4);

// Server-provided payloads that must never become DOM nodes or execute script.
const XSS = {
  model: '<img src=x onerror="window.__observerXss=1">',
  label: '<script>window.__observerXss=2</script>',
  provider: '<svg onload="window.__observerXss=3">',
  authIndex: '<iframe srcdoc="<script>parent.__observerXss=4</script>">',
  headerName: 'X-Evil',
  headerValue: '<script>window.__observerXss=5</script>',
  bodyPrompt: '<script>window.__observerXss=6</script>',
  bodyNested: '<img src=x onerror="window.__observerXss=7">',
  leakedSecret: 'sk-live-DO-NOT-LEAK'
};

// 权威模型目录夹具。只暴露模型 ID；不读取 pricing、配置或凭证目录。
const PRIMARY_MODEL_IDS = [
  'gpt-5-codex',
  'gpt-5',
  'gpt-5-mini',
  'o4-mini',
  'claude-opus-4-1',
  'claude-sonnet-4-5',
  'claude-haiku-4-5',
  'claude-3-7-sonnet',
  'gemini-2.5-pro',
  'gemini-2.5-flash',
  'gemini-2.0-flash',
  'gemini-2.0-flash-lite',
  'grok-4'
];
// 填充项用于验证服务端响应有界；主模型始终排在前面。
const CATALOG_MODEL_IDS = PRIMARY_MODEL_IDS.concat(
  Array.from({ length: 120 }, (_, index) => 'catalog-filler-' + String(index + 1).padStart(3, '0'))
);

function publicCatalogIDs() {
  return CATALOG_MODEL_IDS.slice(0, MAX_CATALOG_MODELS);
}

function iso(ms) {
  return new Date(ms).toISOString();
}

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function publicKey(key) {
  const out = clone(key);
  delete out.secret_hash;
  delete out.secret_rotated_at;
  return out;
}

function managementKey(state, key) {
  const out = publicKey(key);
  const secret = state.secretValues.get(key.id);
  if (typeof secret === 'string' && secret.length > 0) out.secret = secret;
  return out;
}

function defaultCatalogControl() {
  return {
    token_delay_ms: 0,
    models_delay_ms: 0,
    fail_token: false,
    fail_models: false
  };
}

function tokenKey(token) {
  return crypto.createHash('sha256').update(String(token)).digest('hex');
}

function rate(input, output, cacheRead, cacheWrite, reasoning = null, cacheWriteByTtl) {
  const out = { input, output, cache_read: cacheRead, cache_write: cacheWrite, reasoning };
  if (cacheWriteByTtl !== undefined) out.cache_write_by_ttl = cacheWriteByTtl;
  return out;
}

function pricingDefinition(base, overrides = [], multipliers = []) {
  return {
    base: rate(base.input, base.output, base.cache_read, base.cache_write, base.reasoning ?? null, base.cache_write_by_ttl),
    overrides,
    multipliers
  };
}

function createKey(id, label, overrides) {
  const now = Date.now();
  return Object.assign(
    {
      id,
      label,
      enabled: true,
      expires_at: iso(now + 365 * 24 * 3600 * 1000),
      max_concurrent: 4,
      allowed_models: ['gpt-5-codex', 'claude-opus-4-1'],
      limits: {
        five_hour: 5_000_000,
        week: 20_000_000,
        month: 50_000_000,
        total: 100_000_000
      },
      used: {
        five_hour: 1_250_000,
        week: 4_600_000,
        month: 12_500_000,
        total: 38_000_000
      },
      starts: {
        five_hour: iso(now - 2 * 3600 * 1000),
        week: iso(now - 2 * 24 * 3600 * 1000),
        month: iso(now - 10 * 24 * 3600 * 1000),
        total: iso(now - 30 * 24 * 3600 * 1000)
      },
      resets: {
        five_hour: iso(now - 2 * 3600 * 1000),
        week: iso(now - 2 * 24 * 3600 * 1000),
        month: iso(now - 10 * 24 * 3600 * 1000),
        total: iso(now - 30 * 24 * 3600 * 1000)
      },
      active: 1,
      created_at: iso(now - 30 * 24 * 3600 * 1000),
      blocked_reason: ''
    },
    overrides || {}
  );
}

function createRecords(now) {
  function record(id, minutesAgo, fields) {
    const time = now - minutesAgo * 60 * 1000;
    return Object.assign(
      {
        id,
        trace_id: 'trace-' + id,
        key_id: 'key-alpha',
        time: iso(time),
        model: 'gpt-5-codex',
        provider: 'openai',
        auth_index: 'auth-openai-1',
        failed: false,
        status_code: 200,
        stream: true,
        latency_ms: 812,
        ttft_ms: 133,
        tokens: {
          input: 1200,
          output: 800,
          cache_read: 400,
          cache_write: 100,
          reasoning: 200,
          total: 2500
        },
        raw_usage: {
          input_tokens: 1200,
          output_tokens: 800,
          cache_read_input_tokens: 400,
          cache_creation_input_tokens: 100,
          reasoning_tokens: 200
        },
        cost_micro_usd: 125_000,
        price_source: 'auto',
        accounting_quality: 'complete'
      },
      fields || {}
    );
  }

  return [
    record('req-001', 2, {}),
    record('req-002', 4, {
      key_id: 'key-beta',
      model: XSS.model,
      provider: XSS.provider,
      auth_index: XSS.authIndex,
      failed: true,
      status_code: 500,
      latency_ms: 91,
      ttft_ms: 0,
      tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 0 },
      raw_usage: {},
      cost_micro_usd: 0,
      price_source: 'zero',
      accounting_quality: 'unknown',
      trace_id: '<script>window.__observerXss=8</script>'
    }),
    record('req-003', 6, {
      key_id: 'key-alpha',
      model: 'claude-opus-4-1',
      provider: 'anthropic',
      auth_index: 'auth-anthropic-1',
      stream: false,
      latency_ms: 1502,
      ttft_ms: 0,
      tokens: {
        input: 2400,
        output: 1600,
        cache_read: 800,
        cache_write: 200,
        reasoning: 0,
        total: 5000
      },
      raw_usage: {
        input_tokens: 2400,
        output_tokens: 1600,
        cache_read_input_tokens: 800,
        cache_creation_input_tokens: 200
      },
      cost_micro_usd: 380_000,
      price_source: 'auto',
      accounting_quality: 'complete'
    }),
    record('req-004', 8, {
      key_id: 'key-alpha',
      failed: true,
      status_code: 429,
      stream: false,
      latency_ms: 44,
      ttft_ms: 0,
      tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 0 },
      raw_usage: {},
      cost_micro_usd: 0,
      price_source: 'auto',
      accounting_quality: 'complete'
    }),
    record('req-005', 10, {
      key_id: 'key-alpha',
      model: 'gemini-2.5-pro',
      provider: 'google',
      auth_index: 'auth-google-1',
      tokens: {
        input: 1800,
        output: 900,
        cache_read: 300,
        cache_write: 0,
        reasoning: 100,
        total: 3000
      },
      raw_usage: {
        input_tokens: 1800,
        output_tokens: 900,
        cache_read_input_tokens: 300,
        reasoning_tokens: 100
      },
      cost_micro_usd: 210_000,
      price_source: 'manual',
      accounting_quality: 'complete'
    }),
    record('req-006', 12, {
      key_id: 'key-alpha',
      latency_ms: 640,
      ttft_ms: 98,
      cost_micro_usd: 98_000
    }),
    // 64 位十六进制 Key ID：UI 只显示 12 位前缀，title/复制值必须是完整 ID。
    record('req-007', 3, {
      key_id: LONG_KEY_ID,
      model: 'log-keyid-fixture',
      provider: 'mock',
      auth_index: 'auth-mock-keyid',
      latency_ms: 500,
      ttft_ms: 50,
      tokens: { input: 600, output: 400, cache_read: 0, cache_write: 0, reasoning: 0, total: 1000 },
      raw_usage: { input_tokens: 600, output_tokens: 400 },
      cost_micro_usd: 1_000,
      price_source: 'auto',
      accounting_quality: 'complete'
    }),
    // 未分类核算质量：日志行应给出小型 warning，而不是恒定 TPS 未知文案。
    record('req-008', 1, {
      key_id: 'key-alpha',
      model: 'log-unclassified-fixture',
      provider: 'mock',
      auth_index: 'auth-mock-quality',
      latency_ms: 320,
      ttft_ms: 40,
      tokens: { input: 300, output: 200, cache_read: 0, cache_write: 0, reasoning: 0, total: 500 },
      raw_usage: { input_tokens: 300, output_tokens: 200 },
      cost_micro_usd: 0,
      price_source: 'zero',
      accounting_quality: 'unclassified'
    })
  ];
}

function createSettings() {
  return {
    capture_enabled: false,
    capture_mode: 'off',
    capture_retention_seconds: 86400,
    max_body_bytes: 1_048_576,
    max_body_storage_bytes: 268_435_456,
    raw_retention_hours: 24,
    daily_retention_days: 365,
    pricing: {
      auto: true,
      source: 'models_dev_v2',
      prices: [
        {
          model: 'gpt-5-codex',
          definition: pricingDefinition(
            { input: 1_250_000, output: 10_000_000, cache_read: 125_000, cache_write: 1_250_000 },
            [
              {
                id: 'long-context',
                when: { context: { gt: 200000 } },
                rates: { input: 2_500_000, output: 15_000_000 }
              }
            ],
            [
              {
                id: 'flex-discount',
                when: { serviceTier: ['flex'] },
                factors: { input: '0.5', output: '0.5' }
              }
            ]
          )
        }
      ],
      mappings: []
    }
  };
}

function createState(options) {
  const now = Date.now();
  const state = {
    now,
    keys: [
      createKey('key-alpha', '生产 Key（自动化）'),
      createKey('key-beta', XSS.label, {
        enabled: false,
        max_concurrent: 2,
        allowed_models: [XSS.model],
        active: 0
      }),
      createKey('key-gamma', '已过期 Key', {
        enabled: true,
        expires_at: iso(now - 24 * 3600 * 1000),
        active: 0
      }),
      createKey('key-legacy', '历史受限 Key', {
        allowed_models: ['retired-model-v0', 'gpt-5-codex'],
        max_concurrent: 2,
        active: 0
      }),
      createKey(NANO_KEY_ID, '纳秒到期 Key', {
        allowed_models: ['gpt-5-codex'],
        max_concurrent: 2,
        active: 0,
        expires_at: NANO_EXPIRY
      }),
      createKey('key-blocked', '待恢复计费 Key', {
        allowed_models: ['gpt-5-codex'],
        max_concurrent: 1,
        active: 0,
        blocked_reason: 'unknown usage after crash; pending billing cannot be retried'
      })
    ],
    records: createRecords(now),
    settings: createSettings(),
    capabilities: new Map(),
    catalogControl: defaultCatalogControl(),
    pricesLastSync: iso(now - 15 * 60 * 1000),
    catalogEntries: 150,
    secrets: new Map(),
    secretValues: new Map(),
    nextKey: 1,
    counters: {
      priceRefreshes: 0,
      compactions: 0,
      keyCreates: 0,
      keyRotates: 0,
      keyResets: 0,
      keyRecovers: 0,
      keyRevokes: 0,
      detailReads: 0,
      catalogTokenIssued: 0,
      catalogReads: 0
    },
    managementKey: (options && options.managementKey) || DEFAULT_MANAGEMENT_KEY
  };
  for (const k of state.keys) {
    const rawSecret = SECRET_PREFIX + k.id;
    state.secrets.set(k.id, crypto.createHash('sha256').update(rawSecret).digest('hex'));
    // key-legacy models a historical hash-only row whose plaintext is genuinely unavailable.
    if (k.id !== 'key-legacy') state.secretValues.set(k.id, rawSecret);
  }
  return state;
}

function validateCustomSecret(secret) {
  if (typeof secret !== 'string') {
    throw failure(400, 'invalid_key', 'secret must be a string');
  }
  if (secret.length < 8 || secret.length > 256) {
    throw failure(400, 'invalid_key', 'secret must be between 8 and 256 characters');
  }
  // 严格要求 U+0021..U+007E 可见 ASCII 字符，拒绝空白字符、控制字符与非 ASCII
  if (!/^[\x21-\x7E]+$/.test(secret)) {
    throw failure(400, 'invalid_key', 'secret must only contain visible ASCII characters (U+0021..U+007E)');
  }
}

function hashSecret(secret) {
  return crypto.createHash('sha256').update(secret).digest('hex');
}

function findKeyBySecretHash(state, hash) {
  for (const [keyId, secretHash] of state.secrets) {
    if (secretHash === hash) return keyId;
  }
  return null;
}

function failure(status, code, message) {
  const err = new Error(message);
  err.status = status;
  err.code = code;
  return err;
}

function readCsp() {
  const fallback =
    "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
    "frame-ancestors 'self'; base-uri 'none'; form-action 'none'";
  try {
    const goPath = path.resolve(__dirname, '../../plugin/management.go');
    const match = /cspPolicy\s*=\s*"([^"]+)"/.exec(fs.readFileSync(goPath, 'utf8'));
    return match && match[1] ? match[1] : fallback;
  } catch (err) {
    return fallback;
  }
}

const ASSET_CSP = readCsp();
const ASSET_FILES = {
  '/ui': 'ui.html',
  '/ui/': 'ui.html',
  '/ui.html': 'ui.html',
  '/ui.js': 'ui.js',
  '/ui.css': 'ui.css',
  '/theme.js': 'theme.js'
};
const ASSET_TYPES = {
  'ui.html': 'text/html; charset=utf-8',
  'ui.js': 'text/javascript; charset=utf-8',
  'ui.css': 'text/css; charset=utf-8',
  'theme.js': 'text/javascript; charset=utf-8'
};

function sendJson(res, status, body) {
  const data = JSON.stringify(body);
  res.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': Buffer.byteLength(data),
    'Cache-Control': 'no-store',
    'X-Content-Type-Options': 'nosniff'
  });
  res.end(data);
}

function sendError(res, status, code, message) {
  sendJson(res, status, { error: { code, message } });
}

function sendAsset(res, file, headOnly) {
  let body;
  try {
    body = fs.readFileSync(path.join(ASSET_DIR, file));
  } catch (err) {
    sendError(res, 404, 'not_found', 'embedded asset not found');
    return;
  }
  res.writeHead(200, {
    'Content-Type': ASSET_TYPES[file] || 'application/octet-stream',
    'Content-Length': body.length,
    'Cache-Control': 'no-store',
    'Content-Security-Policy': ASSET_CSP,
    'X-Content-Type-Options': 'nosniff'
  });
  res.end(headOnly ? undefined : body);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const declared = Number(req.headers['content-length'] || 0);
    if (declared > MAX_BODY_BYTES) {
      reject(failure(413, 'too_large', 'request body exceeds 1MiB'));
      req.resume();
      return;
    }
    const chunks = [];
    let size = 0;
    let done = false;
    req.on('data', (chunk) => {
      if (done) return;
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        done = true;
        reject(failure(413, 'too_large', 'request body exceeds 1MiB'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (done) return;
      done = true;
      resolve(Buffer.concat(chunks));
    });
    req.on('error', (err) => {
      if (!done) {
        done = true;
        reject(err);
      }
    });
  });
}

async function readJson(req) {
  const buf = await readBody(req);
  if (buf.length === 0) return {};
  let value;
  try {
    value = JSON.parse(buf.toString('utf8'));
  } catch (err) {
    throw failure(400, 'invalid_json', 'request body must be valid JSON');
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw failure(400, 'invalid_json', 'request body must be a JSON object');
  }
  return value;
}

function rejectUnknown(object, allowed, what) {
  for (const key of Object.keys(object)) {
    if (!allowed.has(key)) {
      throw failure(400, 'unknown_field', 'unknown ' + what + ' field: ' + key);
    }
  }
}

function parseTime(value, name) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(value)) {
    throw failure(400, 'invalid_query', name + ' must be an RFC3339 timestamp');
  }
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) throw failure(400, 'invalid_query', name + ' must be a valid timestamp');
  return ms;
}

const REQUEST_QUERY = new Set([
  'from', 'to', 'key_id', 'model', 'provider', 'auth_index', 'status', 'page', 'page_size'
]);

function parseRequestQuery(url) {
  for (const key of url.searchParams.keys()) {
    if (!REQUEST_QUERY.has(key)) throw failure(400, 'invalid_query', 'unknown query parameter: ' + key);
  }
  const now = Date.now();
  const midnight = new Date(now);
  midnight.setUTCHours(0, 0, 0, 0);
  const from = url.searchParams.has('from') ? parseTime(url.searchParams.get('from'), 'from') : midnight.getTime();
  const to = url.searchParams.has('to') ? parseTime(url.searchParams.get('to'), 'to') : now;
  if (!(from < to)) throw failure(400, 'invalid_query', 'from must be before to');

  const status = url.searchParams.get('status') || '';
  if (status !== '' && status !== 'success' && status !== 'failed') {
    throw failure(400, 'invalid_query', 'status must be empty, success or failed');
  }
  const pageRaw = url.searchParams.get('page');
  const sizeRaw = url.searchParams.get('page_size');
  const page = pageRaw === null ? 1 : Number(pageRaw);
  const pageSize = sizeRaw === null ? 25 : Number(sizeRaw);
  if (!Number.isInteger(page) || page < 1) throw failure(400, 'invalid_query', 'page must be an integer >= 1');
  if (!Number.isInteger(pageSize) || pageSize < 1 || pageSize > 100) {
    throw failure(400, 'invalid_query', 'page_size must be an integer between 1 and 100');
  }
  return {
    from,
    to,
    key_id: url.searchParams.get('key_id') || '',
    model: url.searchParams.get('model') || '',
    provider: url.searchParams.get('provider') || '',
    auth_index: url.searchParams.get('auth_index') || '',
    status,
    page,
    page_size: pageSize
  };
}

function filterRecords(records, query) {
  return records
    .filter((record) => {
      const at = Date.parse(record.time);
      if (!(at >= query.from && at < query.to)) return false;
      if (query.key_id && record.key_id !== query.key_id) return false;
      if (query.model && record.model !== query.model) return false;
      if (query.provider && record.provider !== query.provider) return false;
      if (query.auth_index && record.auth_index !== query.auth_index) return false;
      if (query.status === 'success' && record.failed) return false;
      if (query.status === 'failed' && !record.failed) return false;
      return true;
    })
    .sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
}

const requestViewTimeZoneFormatters = new Map();

function requestViewFormatter(timeZone) {
  if (!requestViewTimeZoneFormatters.has(timeZone)) {
    requestViewTimeZoneFormatters.set(timeZone, new Intl.DateTimeFormat('en-US', {
      timeZone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hourCycle: 'h23'
    }));
  }
  return requestViewTimeZoneFormatters.get(timeZone);
}

function zonedParts(ms, timeZone) {
  if (timeZone === 'UTC') {
    const d = new Date(ms);
    return {
      year: d.getUTCFullYear(), month: d.getUTCMonth() + 1, day: d.getUTCDate(),
      hour: d.getUTCHours(), minute: d.getUTCMinutes(), second: d.getUTCSeconds()
    };
  }
  const out = {};
  for (const part of requestViewFormatter(timeZone).formatToParts(new Date(ms))) {
    if (part.type !== 'literal') out[part.type] = Number(part.value);
  }
  return out;
}

function localDateString(ms, timeZone) {
  const p = zonedParts(ms, timeZone);
  return String(p.year).padStart(4, '0') + '-' +
    String(p.month).padStart(2, '0') + '-' + String(p.day).padStart(2, '0');
}

// Resolve a wall-clock local date-time to the earliest matching UTC instant.
// Offset candidates handle ordinary days and repeated hours; when the local
// midnight does not exist (for example America/Santiago 2025-09-07), the first
// representable instant after the gap is used instead of oscillating to the
// previous day.
function zonedDateTimeToMs(year, month, day, timeZone) {
  const target = Date.UTC(year, month - 1, day, 0, 0, 0);
  if (timeZone === 'UTC') return target;
  const wallAt = (ms) => {
    const p = zonedParts(ms, timeZone);
    return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second);
  };
  const offsets = new Set();
  for (const probe of [target - 36 * 3600000, target, target + 36 * 3600000]) {
    offsets.add(wallAt(probe) - probe);
  }
  const matches = [];
  for (const offset of offsets) {
    const candidate = target - offset;
    if (wallAt(candidate) === target && !matches.includes(candidate)) matches.push(candidate);
  }
  if (matches.length > 0) return Math.min(...matches);

  // Nonexistent wall time (spring-forward gap). Return the first instant whose
  // local wall clock has reached the requested value.
  for (let t = target - 12 * 3600000; t <= target + 12 * 3600000; t += 60000) {
    if (wallAt(t) >= target) return t;
  }
  return target;
}

function parseViewDate(value, timeZone, name) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value)) {
    throw failure(400, 'invalid_query', name + ' must be YYYY-MM-DD');
  }
  const [year, month, day] = value.split('-').map(Number);
  const roundTrip = new Date(Date.UTC(year, month - 1, day));
  if (roundTrip.toISOString().slice(0, 10) !== value) {
    throw failure(400, 'invalid_query', name + ' must be a valid local calendar date');
  }
  return { year, month, day, value };
}

function nextCalendarDate(date) {
  const next = new Date(Date.UTC(date.year, date.month - 1, date.day + 1));
  return { year: next.getUTCFullYear(), month: next.getUTCMonth() + 1, day: next.getUTCDate() };
}

function computeSummary(records, query, timeZone = 'UTC') {
  const elapsed = query.to - query.from;
  const granularity = elapsed <= 48 * 3600 * 1000 ? 'hour' : 'day';
  const buckets = new Map();
  const addBucket = (key, time) => buckets.set(key, {
    time: iso(time), requests: 0, failures: 0, cost_micro_usd: 0, tokens: 0
  });
  if (granularity === 'hour') {
    // Elapsed hours from local midnight: during a fall-back transition this
    // keeps 25 distinct buckets instead of merging the repeated local hour.
    for (let t = query.from; t < query.to && buckets.size < 400; t += 3600 * 1000) {
      addBucket(t, t);
    }
  } else {
    // Day buckets start at each local midnight, so DST days are 23/25 hours
    // and the bucket timestamp is the UTC instant of that local midnight.
    let cursor = parseViewDate(localDateString(query.from, timeZone), timeZone, 'from_date');
    for (let guard = 0; guard < 400; guard++) {
      const start = zonedDateTimeToMs(cursor.year, cursor.month, cursor.day, timeZone);
      if (start >= query.to) break;
      addBucket(cursor.value, start);
      cursor = nextCalendarDate(cursor);
    }
  }
  const summary = {
    requests: 0,
    failures: 0,
    cost_micro_usd: 0,
    tokens: 0,
    granularity,
    buckets: []
  };
  for (const record of records) {
    summary.requests += 1;
    if (record.failed) summary.failures += 1;
    summary.cost_micro_usd += record.cost_micro_usd || 0;
    summary.tokens += record.tokens ? record.tokens.total || 0 : 0;
    const at = Date.parse(record.time);
    const key = granularity === 'hour'
      ? query.from + Math.floor((at - query.from) / 3600000) * 3600000
      : localDateString(at, timeZone);
    let bucket = buckets.get(key);
    if (!bucket) {
      const time = granularity === 'hour' ? key : zonedDateTimeToMs(
        ...Object.values(parseViewDate(String(key), timeZone, 'series')).slice(0, 3), timeZone);
      bucket = { time: iso(time), requests: 0, failures: 0, cost_micro_usd: 0, tokens: 0 };
      buckets.set(key, bucket);
    }
    bucket.requests += 1;
    if (record.failed) bucket.failures += 1;
    bucket.cost_micro_usd += record.cost_micro_usd || 0;
    bucket.tokens += record.tokens ? record.tokens.total || 0 : 0;
  }
  summary.buckets = Array.from(buckets.values()).sort((a, b) => Date.parse(a.time) - Date.parse(b.time));
  // Rendering fixtures use the same flat detailed aggregate contract. Billing
  // semantics themselves are verified against the real engine/native host.
  function detail(rows) {
    const out = { detailed_requests: rows.length, complete_requests: 0,
      input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0,
      reasoning_tokens: 0, latency_ms: 0, latency_samples: 0, ttft_ms: 0, ttft_samples: 0 };
    for (const row of rows) {
      if (row.accounting_quality === 'complete') {
        out.complete_requests++;
        for (const [field, token] of Object.entries({ input_tokens: 'input', output_tokens: 'output', cache_read_tokens: 'cache_read', cache_write_tokens: 'cache_write', reasoning_tokens: 'reasoning' })) out[field] += row.tokens?.[token] || 0;
      }
      if (row.latency_ms > 0) { out.latency_ms += row.latency_ms; out.latency_samples++; }
      if (row.stream && row.ttft_ms > 0 && row.ttft_ms < row.latency_ms) { out.ttft_ms += row.ttft_ms; out.ttft_samples++; }
    }
    return out;
  }
  Object.assign(summary, detail(records));
  for (const bucket of summary.buckets) {
    const rows = records.filter(row => {
      const at = Date.parse(row.time);
      return granularity === 'hour'
        ? query.from + Math.floor((at - query.from) / 3600000) * 3600000 === Date.parse(bucket.time)
        : localDateString(at, timeZone) === localDateString(Date.parse(bucket.time), timeZone);
    });
    Object.assign(bucket, detail(rows));
  }
  return summary;
}

function requestView(state, url) {
  const copy = new URL(url);
  const now = Date.now();
  const timeZoneRaw = copy.searchParams.get('timezone') || 'UTC';
  let timeZone = timeZoneRaw;
  if (!timeZone || timeZone === 'Local') throw failure(400, 'invalid_query', 'timezone must be an IANA name');
  try {
    new Intl.DateTimeFormat('en-US', { timeZone });
  } catch {
    throw failure(400, 'invalid_query', 'timezone must be an IANA name');
  }
  const today = localDateString(now, timeZone);
  if (copy.searchParams.has('from') || copy.searchParams.has('to')) throw failure(400, 'invalid_query', 'mixed ranges');
  for (const key of copy.searchParams.keys()) if (copy.searchParams.getAll(key).length !== 1) throw failure(400, 'invalid_query', 'repeated parameter');
  const fromDate = parseViewDate(copy.searchParams.get('from_date') || today, timeZone, 'from_date');
  const endDate = parseViewDate(copy.searchParams.get('to_date') || today, timeZone, 'to_date');
  const from = zonedDateTimeToMs(fromDate.year, fromDate.month, fromDate.day, timeZone);
  const nextDay = nextCalendarDate(endDate);
  const end = zonedDateTimeToMs(nextDay.year, nextDay.month, nextDay.day, timeZone);
  if (end <= from) throw failure(400, 'invalid_query', 'reversed dates');
  const to = Math.min(now, end);
  if (!(from < to)) throw failure(400, 'invalid_query', 'from must be before to');
  const view = copy.searchParams.get('view') || 'requests';
  if (!['requests', 'model', 'key', 'key_model', 'account', 'provider'].includes(view)) throw failure(400, 'invalid_query', 'invalid view');
  ['from_date', 'to_date', 'view', 'timezone'].forEach(key => copy.searchParams.delete(key));
  copy.searchParams.set('from', iso(from)); copy.searchParams.set('to', iso(to));
  const query = parseRequestQuery(copy), rows = filterRecords(state.records, query), summary = computeSummary(rows, query, timeZone);
  const { buckets, granularity, ...aggregate } = summary;
  let selected = rows, groups = [];
  if (view !== 'requests') {
    const map = new Map();
    for (const row of rows) {
      const id = view === 'model' ? JSON.stringify([row.provider, row.model])
        : view === 'key_model' ? JSON.stringify([row.key_id, row.provider, row.model])
        : row[view === 'key' ? 'key_id' : view === 'account' ? 'auth_index' : 'provider'] || 'unknown';
      if (!map.has(id)) map.set(id, []);
      map.get(id).push(row);
    }
    groups = [...map].map(([id, items]) => {
      const { buckets, granularity, ...totals } = computeSummary(items, query, timeZone);
      const row = items[0];
      const keyed = view === 'key' || view === 'key_model';
      return { ...totals, key: id,
        label: keyed ? state.keys.find(key => key.id === row.key_id)?.label || row.key_id || 'unknown' : id,
        model: view === 'model' || view === 'key_model' ? row.model : '',
        provider: view === 'provider' || view === 'model' || view === 'key_model' ? row.provider : '',
        key_id: keyed ? row.key_id : '', auth_index: view === 'account' ? row.auth_index : '' };
    }).sort((a, b) => b.requests - a.requests || a.key.localeCompare(b.key));
    selected = groups;
  }
  const start = (query.page - 1) * query.page_size, page = selected.slice(start, start + query.page_size);
  return { range: { from: iso(from), to: iso(to), as_of: iso(now), timezone: timeZone }, summary: aggregate,
    series: { granularity, buckets }, records: view === 'requests' ? page : [], groups: view === 'requests' ? [] : page,
    pagination: { page: query.page, page_size: query.page_size, has_more: start + page.length < selected.length },
    coverage: { raw_from: iso(now - state.settings.raw_retention_hours * 3600000), daily_from: iso(now - state.settings.daily_retention_days * 86400000), detailed_requests: rows.length, requests: rows.length, backfill_pending: false } };
}

function pricingSnapshot(state) {
  // 目录快照允许同一模型存在多个 provider 的独立定义；手工价按完整身份覆盖。
  const manual = state.settings.pricing.prices.map((price) => ({
    ...clone(price),
    rates: clone(price.definition.base),
    source: 'manual',
    pricing_supported: true
  }));
  const catalog = [
    {
      model: 'gpt-5-codex',
      provider: 'openai',
      rates: rate(1_000_000, 8_000_000, 100_000, 1_000_000),
      definition: pricingDefinition({ input: 1_000_000, output: 8_000_000, cache_read: 100_000, cache_write: 1_000_000 }),
      source: 'auto',
      pricing_supported: true
    },
    {
      model: 'gpt-5-codex',
      provider: 'azure',
      rates: rate(1_500_000, 12_000_000, 150_000, 1_500_000),
      definition: pricingDefinition({ input: 1_500_000, output: 12_000_000, cache_read: 150_000, cache_write: 1_500_000 }),
      source: 'auto',
      pricing_supported: true
    }
  ];
  return {
    prices: manual.concat(catalog),
    last_sync: state.pricesLastSync,
    catalog_entries: state.catalogEntries,
    error: state.pricesError || '',
    refreshing: false,
    source: state.settings.pricing.source
  };
}

function health(state) {
  return {
    database_bytes: 18_874_368,
    dropped_captures: 0,
    pending_usage: 0,
    active_requests: 1,
    last_error: '',
    version: '0.6.0-test',
    schema_version: 3
  };
}

function ensureInteger(value, name, min) {
  if (!Number.isSafeInteger(value) || value < min) {
    throw failure(400, 'invalid_json', name + ' must be a safe integer >= ' + min);
  }
}

function validateLimits(limits, name) {
  if (limits === null || typeof limits !== 'object' || Array.isArray(limits)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(limits, new Set(['five_hour', 'week', 'month', 'total']), name);
  for (const field of ['five_hour', 'week', 'month', 'total']) {
    if (limits[field] === undefined) throw failure(400, 'invalid_json', name + '.' + field + ' is required');
    ensureInteger(limits[field], name + '.' + field, 0);
  }
}

function normalizeKeySpec(body) {
  rejectUnknown(
    body,
    new Set(['label', 'enabled', 'expires_at', 'max_concurrent', 'allowed_models', 'limits']),
    'KeySpec'
  );
  if (typeof body.label !== 'string' || body.label.trim() === '') {
    throw failure(400, 'invalid_json', 'label must be a nonempty string');
  }
  if (body.enabled !== undefined && typeof body.enabled !== 'boolean') {
    throw failure(400, 'invalid_json', 'enabled must be a boolean');
  }
  if (body.expires_at !== undefined && body.expires_at !== null) {
    parseTime(body.expires_at, 'expires_at');
  }
  if (body.max_concurrent !== undefined) ensureInteger(body.max_concurrent, 'max_concurrent', 0);
  if (body.allowed_models !== undefined) {
    if (!Array.isArray(body.allowed_models) || body.allowed_models.some((m) => typeof m !== 'string')) {
      throw failure(400, 'invalid_json', 'allowed_models must be an array of strings');
    }
  }
  if (body.limits !== undefined) validateLimits(body.limits, 'limits');
  return {
    label: body.label,
    enabled: body.enabled === undefined ? true : body.enabled,
    expires_at: body.expires_at === undefined ? null : body.expires_at,
    max_concurrent: body.max_concurrent === undefined ? 0 : body.max_concurrent,
    allowed_models: body.allowed_models === undefined ? [] : body.allowed_models,
    limits:
      body.limits === undefined
        ? { five_hour: 0, week: 0, month: 0, total: 0 }
        : clone(body.limits)
  };
}

function ensureDecimalString(value, name) {
  if (typeof value !== 'string' || value.length > 64 || !/^(?:0|[1-9]\d*)(?:\.\d+)?$/.test(value)) {
    throw failure(400, 'invalid_json', name + ' must be a non-negative decimal string');
  }
}

function validateTtlMap(value, name) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length > 16) {
    throw failure(400, 'invalid_json', name + ' must contain at most 16 tiers');
  }
  for (const [seconds, price] of Object.entries(value)) {
    if (!/^[1-9]\d*$/.test(seconds) || !Number.isSafeInteger(Number(seconds))) {
      throw failure(400, 'invalid_json', name + ' keys must be positive integer seconds');
    }
    ensureInteger(price, name + '.' + seconds, 0);
  }
}

function validateCondition(condition, name, depth = 0, state = { nodes: 0 }) {
  state.nodes += 1;
  if (depth > 8 || state.nodes > 64) {
    throw failure(400, 'invalid_json', name + ' exceeds the condition depth/node limit');
  }
  if (condition === null || typeof condition !== 'object' || Array.isArray(condition)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(condition, new Set(['context', 'serviceTier', 'date', 'time', 'all', 'any', 'not']), name);
  if (condition.context !== undefined) {
    const context = condition.context;
    if (context === null || typeof context !== 'object' || Array.isArray(context)) {
      throw failure(400, 'invalid_json', name + '.context must be an object');
    }
    rejectUnknown(context, new Set(['gt', 'gte', 'lt', 'lte']), name + '.context');
    if (context.gt !== undefined && context.gte !== undefined) {
      throw failure(400, 'invalid_json', name + '.context cannot set gt and gte together');
    }
    if (context.lt !== undefined && context.lte !== undefined) {
      throw failure(400, 'invalid_json', name + '.context cannot set lt and lte together');
    }
    for (const field of ['gt', 'gte', 'lt', 'lte']) {
      if (context[field] !== undefined) ensureInteger(context[field], name + '.context.' + field, 0);
    }
  }
  if (condition.serviceTier !== undefined) {
    if (!Array.isArray(condition.serviceTier) ||
        condition.serviceTier.length === 0 ||
        condition.serviceTier.length > 16 ||
        new Set(condition.serviceTier).size !== condition.serviceTier.length ||
        condition.serviceTier.some((tier) => typeof tier !== 'string' || !tier || tier.length > 128)) {
      throw failure(400, 'invalid_json', name + '.serviceTier must be a nonempty unique string array');
    }
  }
  if (condition.date !== undefined) {
    const date = condition.date;
    if (date === null || typeof date !== 'object' || Array.isArray(date)) {
      throw failure(400, 'invalid_json', name + '.date must be an object');
    }
    rejectUnknown(date, new Set(['gte', 'lt']), name + '.date');
    for (const field of ['gte', 'lt']) {
      if (date[field] !== undefined && (typeof date[field] !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(date[field]))) {
        throw failure(400, 'invalid_json', name + '.date.' + field + ' must be YYYY-MM-DD');
      }
    }
    if (date.gte && date.lt && date.gte >= date.lt) {
      throw failure(400, 'invalid_json', name + '.date.gte must be before lt');
    }
  }
  if (condition.time !== undefined) {
    const time = condition.time;
    if (time === null || typeof time !== 'object' || Array.isArray(time)) {
      throw failure(400, 'invalid_json', name + '.time must be an object');
    }
    rejectUnknown(time, new Set(['start', 'end', 'timezone', 'weekdays']), name + '.time');
    if (typeof time.start !== 'string' || !/^([01]\d|2[0-3]):[0-5]\d$/.test(time.start)) {
      throw failure(400, 'invalid_json', name + '.time.start must be HH:mm');
    }
    if (typeof time.end !== 'string' || !/^(?:(?:[01]\d|2[0-3]):[0-5]\d|24:00)$/.test(time.end)) {
      throw failure(400, 'invalid_json', name + '.time.end must be HH:mm or 24:00');
    }
    if (time.start === time.end) throw failure(400, 'invalid_json', name + '.time.start must differ from end');
    if (typeof time.timezone !== 'string' || !time.timezone || time.timezone.length > 128) {
      throw failure(400, 'invalid_json', name + '.time.timezone must be an IANA name');
    }
    try {
      new Intl.DateTimeFormat('en-US', { timeZone: time.timezone });
    } catch {
      throw failure(400, 'invalid_json', name + '.time.timezone must be an IANA name');
    }
    if (time.weekdays !== undefined &&
        (!Array.isArray(time.weekdays) ||
         time.weekdays.some((day) => !Number.isInteger(day) || day < 1 || day > 7))) {
      throw failure(400, 'invalid_json', name + '.time.weekdays must contain integers 1..7');
    }
  }
  for (const field of ['all', 'any']) {
    if (condition[field] !== undefined) {
      if (!Array.isArray(condition[field]) || condition[field].length === 0 || condition[field].length > 16) {
        throw failure(400, 'invalid_json', name + '.' + field + ' must be a nonempty array of at most 16 items');
      }
      condition[field].forEach((child, index) =>
        validateCondition(child, name + '.' + field + '[' + index + ']', depth + 1, state));
    }
  }
  if (condition.not !== undefined) {
    validateCondition(condition.not, name + '.not', depth + 1, state);
  }
}

function validateRateBase(rates, name) {
  if (rates === null || typeof rates !== 'object' || Array.isArray(rates)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(rates, new Set(['input', 'output', 'cache_read', 'cache_write', 'cache_write_by_ttl', 'reasoning']), name);
  for (const field of ['input', 'output', 'cache_read', 'cache_write']) {
    if (rates[field] === undefined) throw failure(400, 'invalid_json', name + '.' + field + ' is required');
    ensureInteger(rates[field], name + '.' + field, 0);
  }
  if (rates.cache_write_by_ttl !== undefined) validateTtlMap(rates.cache_write_by_ttl, name + '.cache_write_by_ttl');
  if (rates.reasoning !== undefined && rates.reasoning !== null) {
    ensureInteger(rates.reasoning, name + '.reasoning', 0);
  }
}

function validateRatePatch(rates, name) {
  if (rates === null || typeof rates !== 'object' || Array.isArray(rates)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(rates, new Set(['input', 'output', 'cache_read', 'cache_write', 'cache_write_by_ttl', 'reasoning']), name);
  for (const field of ['input', 'output', 'cache_read', 'cache_write']) {
    if (rates[field] !== undefined) ensureInteger(rates[field], name + '.' + field, 0);
  }
  if (rates.cache_write_by_ttl !== undefined) validateTtlMap(rates.cache_write_by_ttl, name + '.cache_write_by_ttl');
  if (rates.reasoning !== undefined && rates.reasoning !== null) {
    ensureInteger(rates.reasoning, name + '.reasoning', 0);
  }
}

function validatePricingDefinition(definition, name) {
  if (definition === null || typeof definition !== 'object' || Array.isArray(definition)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(definition, new Set(['base', 'overrides', 'multipliers']), name);
  validateRateBase(definition.base, name + '.base');
  if (!Array.isArray(definition.overrides) || !Array.isArray(definition.multipliers)) {
    throw failure(400, 'invalid_json', name + '.overrides and multipliers must be arrays');
  }
  if (definition.overrides.length > 64 || definition.multipliers.length > 64) {
    throw failure(400, 'invalid_json', name + ' has too many rules');
  }
  const overrideIds = new Set();
  definition.overrides.forEach((entry, index) => {
    const itemName = name + '.overrides[' + index + ']';
    rejectUnknown(entry, new Set(['id', 'when', 'rates']), itemName);
    if (typeof entry.id !== 'string' || !entry.id || entry.id.length > 1024) {
      throw failure(400, 'invalid_json', itemName + '.id must be a nonempty string');
    }
    if (overrideIds.has(entry.id)) throw failure(400, 'invalid_json', itemName + '.id is duplicated');
    overrideIds.add(entry.id);
    validateCondition(entry.when, itemName + '.when');
    validateRatePatch(entry.rates, itemName + '.rates');
  });
  const multiplierIds = new Set();
  definition.multipliers.forEach((entry, index) => {
    const itemName = name + '.multipliers[' + index + ']';
    rejectUnknown(entry, new Set(['id', 'when', 'factors']), itemName);
    if (typeof entry.id !== 'string' || !entry.id || entry.id.length > 1024) {
      throw failure(400, 'invalid_json', itemName + '.id must be a nonempty string');
    }
    if (multiplierIds.has(entry.id)) throw failure(400, 'invalid_json', itemName + '.id is duplicated');
    multiplierIds.add(entry.id);
    validateCondition(entry.when, itemName + '.when');
    if (entry.factors === null || typeof entry.factors !== 'object' || Array.isArray(entry.factors)) {
      throw failure(400, 'invalid_json', itemName + '.factors must be an object');
    }
    rejectUnknown(entry.factors, new Set(['input', 'output', 'cache_read', 'cache_write', 'reasoning']), itemName + '.factors');
    for (const field of ['input', 'output', 'cache_read', 'cache_write', 'reasoning']) {
      if (entry.factors[field] !== undefined) ensureDecimalString(entry.factors[field], itemName + '.factors.' + field);
    }
  });
}

function validatePricing(pricing) {
  if (pricing === null || typeof pricing !== 'object' || Array.isArray(pricing)) {
    throw failure(400, 'invalid_json', 'pricing must be an object');
  }
  rejectUnknown(pricing, new Set(['auto', 'source', 'prices', 'mappings']), 'pricing');
  if (typeof pricing.auto !== 'boolean') throw failure(400, 'invalid_json', 'pricing.auto must be a boolean');
  if (!['models_dev_v2', 'models_dev_v1'].includes(pricing.source)) {
    throw failure(400, 'invalid_json', 'pricing.source must be models_dev_v2 or models_dev_v1');
  }
  if (!Array.isArray(pricing.prices)) {
    throw failure(400, 'invalid_json', 'pricing.prices must be an array');
  }
  if (pricing.mappings !== undefined && !Array.isArray(pricing.mappings)) {
    throw failure(400, 'invalid_json', 'pricing.mappings must be an array');
  }
  const identities = new Set();
  pricing.prices.forEach((price, index) => {
    const name = 'prices[' + index + ']';
    rejectUnknown(price, new Set(['model', 'provider', 'definition']), name);
    if (typeof price.model !== 'string' || !price.model || price.model.length > 1024) {
      throw failure(400, 'invalid_json', name + '.model must be a nonempty string');
    }
    if (price.provider !== undefined && (typeof price.provider !== 'string' || price.provider.length > 1024)) {
      throw failure(400, 'invalid_json', name + '.provider must be a string');
    }
    const identity = price.model + '\u0000' + (price.provider || '');
    if (identities.has(identity)) throw failure(400, 'invalid_json', name + ' duplicates a manual identity');
    identities.add(identity);
    validatePricingDefinition(price.definition, name + '.definition');
  });
  (pricing.mappings || []).forEach((mapping, index) => {
    const name = 'mappings[' + index + ']';
    rejectUnknown(mapping, new Set(['model', 'provider', 'catalog_model']), name);
    for (const field of ['model', 'provider', 'catalog_model']) {
      if (typeof mapping[field] !== 'string' || !mapping[field]) {
        throw failure(400, 'invalid_json', name + '.' + field + ' must be a nonempty string');
      }
    }
  });
}

function validateSettings(body, saved) {
  rejectUnknown(
    body,
    new Set([
      'capture_enabled',
      'capture_mode',
      'capture_retention_seconds',
      'max_body_bytes',
      'max_body_storage_bytes',
      'raw_retention_hours',
      'daily_retention_days',
      'pricing'
    ]),
    'Settings'
  );
  const patch = body;
  body = { ...clone(saved || createSettings()), ...patch, pricing: { ...(saved || createSettings()).pricing, ...(patch.pricing || {}) } };
  if (patch.capture_mode !== undefined) {
    if (!['off', 'headers', 'headers_and_body'].includes(patch.capture_mode) ||
        (patch.capture_enabled !== undefined && patch.capture_enabled !== (patch.capture_mode !== 'off'))) throw failure(400, 'invalid_json', 'capture mode conflicts');
    body.capture_enabled = patch.capture_mode !== 'off';
  } else if (patch.capture_enabled !== undefined) {
    body.capture_mode = patch.capture_enabled ? (body.capture_mode === 'headers' ? 'headers' : 'headers_and_body') : 'off';
  }
  if (typeof body.capture_enabled !== 'boolean') {
    throw failure(400, 'invalid_json', 'capture_enabled must be a boolean');
  }
  ensureInteger(body.max_body_bytes, 'max_body_bytes', 1);
  ensureInteger(body.max_body_storage_bytes, 'max_body_storage_bytes', 1);
  if (body.max_body_bytes > 8 * 1024 * 1024 || body.max_body_bytes > body.max_body_storage_bytes) throw failure(400, 'invalid_json', 'body budget exceeded');
  if (body.max_body_storage_bytes > MAX_BODY_STORAGE_BYTES) {
    throw failure(400, 'invalid_json', 'max_body_storage_bytes must be <= ' + MAX_BODY_STORAGE_BYTES);
  }
  ensureInteger(body.raw_retention_hours, 'raw_retention_hours', 1);
  if (body.raw_retention_hours > 168) throw failure(400, 'invalid_json', 'raw_retention_hours must be <= 168');
  ensureInteger(body.capture_retention_seconds, 'capture_retention_seconds', 60);
  if (body.capture_retention_seconds > body.raw_retention_hours * 3600) throw failure(400, 'invalid_json', 'capture retention exceeds raw retention');
  ensureInteger(body.daily_retention_days, 'daily_retention_days', 1);
  if (body.daily_retention_days > 3650) {
    throw failure(400, 'invalid_json', 'daily_retention_days must be <= 3650');
  }
  validatePricing(body.pricing);
  return clone(body);
}

function newSecret() {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  return SECRET_PREFIX + Array.from({ length: 48 }, () => alphabet[crypto.randomInt(alphabet.length)]).join('');
}

function findKey(state, id, includeDeleted = false) {
  const key = state.keys.find((item) => item.id === id);
  if (!key) throw failure(404, 'not_found', 'key not found');
  if (key.deleted_at && !includeDeleted) throw failure(410, 'deleted', 'key deleted');
  return key;
}

function detailFor(state, id) {
  const record = state.records.find((item) => item.id === id);
  if (!record) throw failure(404, 'not_found', 'request not found');
  state.counters.detailReads += 1;
  if (id === 'req-002') {
    return {
      record: clone(record),
      headers: {
        'Content-Type': ['application/json'],
        Authorization: ['Bearer [redacted]'],
        [XSS.headerName]: [XSS.headerValue],
        'X-Fake-Secret': ['[redacted]']
      },
      body: {
        prompt: XSS.bodyPrompt,
        nested: { value: XSS.bodyNested },
        leaked: '[redacted]'
      },
      capture_status: 'redacted'
    };
  }
  if (id === 'req-001' || id === 'req-006') {
    return {
      record: clone(record),
      headers: {
        'Content-Type': ['application/json'],
        'X-Request-Id': [record.trace_id],
        Authorization: ['Bearer [redacted]']
      },
      body: {
        model: record.model,
        stream: record.stream,
        messages: [{ role: 'user', content: 'fake local prompt' }]
      },
      capture_status: 'available'
    };
  }
  return {
    record: clone(record),
    headers: {},
    body: null,
    capture_status: id === 'req-004' ? 'disabled' : 'expired'
  };
}

function parseWindowList(body) {
  rejectUnknown(body, new Set(['windows']), 'reset request');
  if (!Array.isArray(body.windows) || body.windows.length === 0) {
    throw failure(400, 'invalid_json', 'windows must be a nonempty array');
  }
  const allowed = new Set(['five_hour', 'week', 'month', 'total']);
  for (const window of body.windows) {
    if (!allowed.has(window)) throw failure(400, 'invalid_json', 'unknown window: ' + window);
  }
  return body.windows;
}

// 固定路由只接受单个 id query；旧 /keys/{id} 与 /requests/{id} 不再注册。
function parseQueryID(url, what) {
  for (const key of url.searchParams.keys()) {
    if (key !== 'id') throw failure(400, 'invalid_query', 'unknown query parameter: ' + key);
  }
  const values = url.searchParams.getAll('id');
  if (values.length !== 1 || !values[0]) {
    throw failure(400, 'invalid_query', what + ' requires exactly one id query parameter');
  }
  return values[0];
}

function rejectAnyQuery(url) {
  for (const key of url.searchParams.keys()) {
    throw failure(400, 'invalid_query', 'unknown query parameter: ' + key);
  }
}

function parseAcknowledge(body) {
  rejectUnknown(body, new Set(['acknowledge']), 'recover request');
  if (body.acknowledge !== true) {
    throw failure(400, 'invalid_json', 'recover requires acknowledge:true');
  }
  return true;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function createServer(options) {
  const opts = options || {};
  const state = createState(opts);
  const calls = [];
  let sequence = 0;

  function resetState() {
    const fresh = createState(opts);
    state.now = fresh.now;
    state.keys = fresh.keys;
    state.records = fresh.records;
    state.settings = fresh.settings;
    state.capabilities = fresh.capabilities;
    state.catalogControl = fresh.catalogControl;
    state.pricesLastSync = fresh.pricesLastSync;
    state.secrets = fresh.secrets;
    state.secretValues = fresh.secretValues;
    state.nextKey = fresh.nextKey;
    // 保持 startServer() 返回的 counters 引用有效。
    Object.assign(state.counters, fresh.counters);
  }

  function resetCalls() {
    calls.length = 0;
    sequence = 0;
  }

  function record(req, url, kind, call) {
    call.seq = ++sequence;
    call.time = new Date().toISOString();
    call.method = req.method;
    call.path = url.pathname;
    call.kind = kind;
    call.query = Object.fromEntries(url.searchParams.entries());
    call.authorized = false;
    call.request_body = undefined;
    call.body_bytes = undefined;
    call.capability_stage = undefined;
    calls.push(call);
    return call;
  }

  const server = http.createServer(async (req, res) => {
    let url;
    try {
      url = new URL(req.url || '/', 'http://127.0.0.1');
    } catch (err) {
      sendError(res, 400, 'invalid_url', 'invalid request URL');
      return;
    }

    if (url.pathname === '/favicon.ico') {
      res.writeHead(204, { 'Cache-Control': 'no-store' });
      res.end();
      return;
    }

    // The official application's own router/layout creates the plugin iframe.
    if (url.pathname === '/management.html' && opts.shellHTML) {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
      res.end(opts.shellHTML);
      return;
    }
    const hostMatch = /^\/v(?:0|8)\/management\/(config|plugins|auth-files|usage|latest-version|credentials|config\/access\/api-keys|observability\/usage\/api-keys)$/.exec(url.pathname);
    if (hostMatch && opts.shellHTML) {
      if (req.headers.authorization !== `Bearer ${state.managementKey}`) {
        sendError(res, 401, 'unauthorized', 'invalid management key');
        return;
      }
      res.setHeader('x-cpa-version', 'v8.0.33');
      res.setHeader('x-cpa-build-date', '2026-10-18');
      res.setHeader('x-cpa-support-plugin', 'true');
      const bodies = {
        config: { 'api-keys': ['host-browser-probe-only'] },
        plugins: { plugins_enabled: true, plugins_dir: 'plugins', plugins: [{
          id: PLUGIN_ID, enabled: true, effective_enabled: true, registered: true, configured: true,
          metadata: { name: 'Observer', version: '0.8.0' },
          menus: [{ path: RESOURCE_BASE + '/ui', menu: 'Observer', description: 'Key 与用量管理' }],
        }] },
        'auth-files': { files: [] }, usage: { usage: {} }, 'latest-version': { 'latest-version': 'v8.0.33' },
        credentials: { files: [] }, 'config/access/api-keys': ['host-browser-probe-only'],
        'observability/usage/api-keys': { api_keys: [] },
      };
      sendJson(res, 200, bodies[hostMatch[1]]);
      return;
    }

    if (url.pathname.startsWith('/__mock/')) {
      if (req.method === 'GET' && url.pathname === '/__mock/calls') {
        sendJson(res, 200, { calls });
        return;
      }
      if (req.method === 'GET' && url.pathname === '/__mock/state') {
        sendJson(res, 200, {
          keys: state.keys.map(publicKey),
          settings: state.settings,
          catalog_control: state.catalogControl,
          catalog_ids: publicCatalogIDs(),
          outstanding_capabilities: state.capabilities.size,
          counters: state.counters,
          calls: calls.length
        });
        return;
      }
      if (req.method === 'POST' && url.pathname === '/__mock/control') {
        const body = await readJson(req);
        rejectUnknown(body, new Set(['catalog']), 'mock control');
        if (body.catalog !== undefined) {
          const patch = body.catalog;
          rejectUnknown(
            patch,
            new Set(['token_delay_ms', 'models_delay_ms', 'fail_token', 'fail_models']),
            'catalog control'
          );
          for (const field of ['token_delay_ms', 'models_delay_ms']) {
            if (patch[field] === undefined) continue;
            ensureInteger(patch[field], 'catalog.' + field, 0);
            if (patch[field] > 10000) {
              throw failure(400, 'invalid_json', 'catalog.' + field + ' must be <= 10000');
            }
            state.catalogControl[field] = patch[field];
          }
          for (const field of ['fail_token', 'fail_models']) {
            if (patch[field] === undefined) continue;
            if (typeof patch[field] !== 'boolean') {
              throw failure(400, 'invalid_json', 'catalog.' + field + ' must be a boolean');
            }
            state.catalogControl[field] = patch[field];
          }
        }
        sendJson(res, 200, { ok: true, catalog: state.catalogControl });
        return;
      }
      if (req.method === 'POST' && url.pathname === '/__mock/reset-calls') {
        resetCalls();
        sendJson(res, 200, { ok: true });
        return;
      }
      if (req.method === 'POST' && url.pathname === '/__mock/reset') {
        resetState();
        resetCalls();
        sendJson(res, 200, { ok: true });
        return;
      }
      sendError(res, 404, 'not_found', 'unknown mock endpoint');
      return;
    }

    const resourceSuffix = suffixOf(url.pathname, RESOURCE_BASE);
    const managementSuffix = suffixOf(url.pathname, MANAGEMENT_BASE);
    const assetSuffix = resourceSuffix !== null ? resourceSuffix : managementSuffix;
    if ((resourceSuffix !== null || managementSuffix !== null) && Object.prototype.hasOwnProperty.call(ASSET_FILES, assetSuffix)) {
      const call = record(req, url, 'asset', {});
      if (req.method !== 'GET' && req.method !== 'HEAD') {
        res.setHeader('Allow', 'GET, HEAD');
        sendError(res, 405, 'method_not_allowed', 'method not allowed');
        call.status = 405;
        return;
      }
      res.on('finish', () => {
        call.status = res.statusCode;
      });
      sendAsset(res, ASSET_FILES[assetSuffix], req.method === 'HEAD');
      return;
    }

    // 权威模型目录：只接受 POST /models/token 签发的一次性 capability。
    // 这是唯一允许的非管理前缀数据路由，且不会回退到 auth-files/pricing。
    if (url.pathname === CATALOG_PATH) {
      const call = record(req, url, 'catalog', {});
      res.on('finish', () => {
        call.status = res.statusCode;
      });
      res.on('close', () => {
        if (!res.writableEnded) call.aborted = true;
      });
      try {
        requireMethod(req, 'GET');
        rejectAnyQuery(url);
        const auth = req.headers.authorization || '';
        call.has_authorization = Boolean(auth);
        call.has_extra_auth_header = Boolean(
          req.headers['x-api-key'] || req.headers['x-goog-api-key'] || req.headers['x-auth-token']
        );
        const bearer = /^Bearer\s+(.+)$/i.exec(auth);
        // CPA dashboard (not Observer) probes its configured fake client Key.
        // Scope it to shell fixtures so catalog contract tests remain exclusive.
        if (opts.shellHTML && bearer?.[1] === 'host-browser-probe-only') {
          sendJson(res, 200, { data: publicCatalogIDs().map(id => ({ id })) });
          return;
        }
        const key = bearer ? tokenKey(bearer[1]) : '';
        const expiresAt = key ? state.capabilities.get(key) : undefined;
        call.authorized = Boolean(expiresAt && expiresAt > Date.now());
        call.capability_stage = call.authorized ? 'used' : 'rejected';
        if (!call.authorized) throw failure(401, 'unauthorized', 'invalid or expired catalog capability');
        // 一次性 capability：通过校验后立即失效，失败/中止也不再可用。
        state.capabilities.delete(key);

        if (state.catalogControl.models_delay_ms > 0) {
          await sleep(state.catalogControl.models_delay_ms);
        }
        if (res.destroyed || res.writableEnded || req.aborted) {
          call.aborted = true;
          return;
        }
        if (state.catalogControl.fail_models) {
          throw failure(502, 'catalog_failed', 'model catalog unavailable');
        }
        state.counters.catalogReads += 1;
        const data = publicCatalogIDs().map((id) => ({ id }));
        sendJson(res, 200, { data });
      } catch (err) {
        if (res.headersSent) return;
        if (err && err.status) sendError(res, err.status, err.code || 'error', err.message);
        else sendError(res, 500, 'internal_error', 'mock server error');
      }
      return;
    }

    if (managementSuffix === null) {
      sendError(res, 404, 'not_found', 'not found');
      return;
    }

    const call = record(req, url, 'management', {});
    res.on('finish', () => {
      call.status = res.statusCode;
    });
    res.on('close', () => {
      if (!res.writableEnded) call.aborted = true;
    });

    try {
      const auth = req.headers.authorization || '';
      const bearer = /^Bearer\s+(.+)$/i.exec(auth);
      const authorized = Boolean(bearer && bearer[1] === state.managementKey);
      call.authorized = authorized;
      call.has_authorization = Boolean(auth);
      if (!authorized) throw failure(401, 'unauthorized', 'invalid management key');

      const suffix = managementSuffix;

      if (suffix === '/health') {
        requireMethod(req, 'GET');
        sendJson(res, 200, health(state));
        return;
      }

      if (suffix === '/models/token') {
        requireMethod(req, 'POST');
        rejectAnyQuery(url);
        const body = await readBody(req);
        call.body_bytes = body.length;
        if (body.length !== 0) throw failure(400, 'invalid_json', 'models/token requires an empty body');
        if (state.catalogControl.token_delay_ms > 0) {
          await sleep(state.catalogControl.token_delay_ms);
        }
        if (res.destroyed || res.writableEnded || req.aborted) {
          call.aborted = true;
          return;
        }
        if (state.catalogControl.fail_token) {
          throw failure(502, 'token_failed', 'catalog capability unavailable');
        }
        const now = Date.now();
        for (const [key, expiresAt] of state.capabilities) {
          if (expiresAt <= now) state.capabilities.delete(key);
        }
        if (state.capabilities.size >= MAX_OUTSTANDING_CAPABILITIES) {
          throw failure(429, 'capability_limit', 'too many outstanding catalog capabilities');
        }
        const token = crypto.randomBytes(32).toString('base64url');
        const expiresAt = now + CAPABILITY_TTL_MS;
        state.capabilities.set(tokenKey(token), expiresAt);
        state.counters.catalogTokenIssued += 1;
        call.capability_stage = 'minted';
        sendJson(res, 200, { token, expires_at: iso(expiresAt) });
        return;
      }

      if (suffix === '/keys') {
        if (req.method === 'GET') {
          for (const key of url.searchParams.keys()) if (key !== 'include_deleted' || url.searchParams.getAll(key).length !== 1 || !['true', 'false'].includes(url.searchParams.get(key))) throw failure(400, 'invalid_query', 'invalid include_deleted');
          sendJson(res, 200, { items: state.keys.filter(key => !key.deleted_at || url.searchParams.get('include_deleted') === 'true').map(key => managementKey(state, key)) });
          return;
        }
        if (req.method === 'POST') {
          rejectAnyQuery(url);
          const body = await readJson(req);
          const { secret: supplied, ...fields } = body;
          const custom = Object.prototype.hasOwnProperty.call(body, 'secret');
          if (custom) validateCustomSecret(supplied);
          const secret = custom ? supplied : newSecret();
          const digest = hashSecret(secret);
          if (findKeyBySecretHash(state, digest)) throw failure(409, 'conflict', 'client key already exists');
          call.request_body = clone(fields);
          const spec = normalizeKeySpec(fields);
          const id = 'key-' + String(state.nextKey++).padStart(3, '0');
          const key = createKey(id, spec.label, {
            enabled: spec.enabled,
            expires_at: spec.expires_at,
            max_concurrent: spec.max_concurrent,
            allowed_models: spec.allowed_models,
            limits: spec.limits,
            used: { five_hour: 0, week: 0, month: 0, total: 0 },
            active: 0,
            created_at: iso(Date.now())
          });
          state.keys.push(key);
          state.secrets.set(id, digest);
          state.secretValues.set(id, secret);
          state.counters.keyCreates += 1;
          sendJson(res, 201, { key: managementKey(state, key), secret });
          return;
        }
        if (req.method === 'PUT') {
          const id = parseQueryID(url, 'PUT /keys');
          const key = findKey(state, id);
          const body = await readJson(req);
          call.request_body = clone(body);
          const spec = normalizeKeySpec(body);
          Object.assign(key, {
            label: spec.label,
            enabled: spec.enabled,
            expires_at: spec.expires_at,
            max_concurrent: spec.max_concurrent,
            allowed_models: spec.allowed_models,
            limits: spec.limits
          });
          sendJson(res, 200, managementKey(state, key));
          return;
        }
        if (req.method === 'DELETE') {
          const id = parseQueryID(url, 'DELETE /keys');
          const key = findKey(state, id);
          key.enabled = false;
          key.revoked = true;
          key.active = 0;
          key.blocked_reason = 'revoked';
          state.secrets.delete(key.id);
          state.secretValues.delete(key.id);
          state.counters.keyRevokes += 1;
          res.writeHead(204, {
            'Cache-Control': 'no-store',
            'X-Content-Type-Options': 'nosniff'
          });
          res.end();
          return;
        }
        methodNotAllowed(res, 'GET, POST, PUT, DELETE');
        return;
      }

      if (suffix === '/keys/delete') {
        requireMethod(req, 'POST');
        const key = findKey(state, parseQueryID(url, 'delete'), true);
        if (key.active > 0) throw failure(409, 'busy', 'resource busy');
        key.deleted_at ||= iso(Date.now()); key.enabled = false; key.revoked = true;
        state.secrets.delete(key.id); state.secretValues.delete(key.id); res.writeHead(204); res.end(); return;
      }
      if (suffix === '/keys/rotate') {
        requireMethod(req, 'POST');
        const id = parseQueryID(url, 'POST /keys/rotate');
        const key = findKey(state, id);
        const body = await readJson(req);
        rejectUnknown(body, new Set(['secret']), 'rotation');
        const custom = Object.prototype.hasOwnProperty.call(body, 'secret');
        if (custom) validateCustomSecret(body.secret);
        const secret = custom ? body.secret : newSecret();
        const digest = hashSecret(secret);
        const owner = findKeyBySecretHash(state, digest);
        if (owner === id) throw failure(400, 'invalid_key', 'new key must differ');
        if (owner) throw failure(409, 'conflict', 'client key already exists');
        state.secrets.set(key.id, digest);
        state.secretValues.set(key.id, secret);
        key.secret_rotated_at = iso(Date.now());
        state.counters.keyRotates += 1;
        sendJson(res, 200, { key: managementKey(state, key), secret });
        return;
      }

      if (suffix === '/keys/reset') {
        requireMethod(req, 'POST');
        const id = parseQueryID(url, 'POST /keys/reset');
        const key = findKey(state, id);
        const body = await readJson(req);
        call.request_body = clone(body);
        const windows = parseWindowList(body);
        const at = iso(Date.now());
        for (const window of windows) {
          key.used[window] = 0;
          key.starts[window] = at;
          key.resets[window] = at;
        }
        state.counters.keyResets += 1;
        sendJson(res, 200, managementKey(state, key));
        return;
      }

      if (suffix === '/keys/recover') {
        requireMethod(req, 'POST');
        const id = parseQueryID(url, 'POST /keys/recover');
        const key = findKey(state, id);
        const body = await readJson(req);
        call.request_body = clone(body);
        parseAcknowledge(body);
        // 恢复只清除 Key 级阻塞原因并记录审计；不得改动 used/starts/resets/limits。
        key.blocked_reason = '';
        key.recovered_at = iso(Date.now());
        state.counters.keyRecovers += 1;
        sendJson(res, 200, managementKey(state, key));
        return;
      }

      if (suffix === '/requests/view') {
        requireMethod(req, 'GET');
        sendJson(res, 200, requestView(state, url)); return;
      }
      if (suffix === '/requests/facets') {
        requireMethod(req, 'GET');
        for (const key of url.searchParams.keys()) if (!['field', 'search', 'cursor', 'limit'].includes(key) || url.searchParams.getAll(key).length !== 1) throw failure(400, 'invalid_query', 'invalid facet query');
        const field = url.searchParams.get('field');
        if (!['model', 'key_id', 'provider', 'auth_index'].includes(field)) throw failure(400, 'invalid_query', 'invalid facet');
        const limit = Number(url.searchParams.get('limit') || 25);
        if (!Number.isInteger(limit) || limit < 1 || limit > 100) throw failure(400, 'invalid_query', 'invalid limit');
        const search = (url.searchParams.get('search') || '').toLowerCase(), cursor = Buffer.from(url.searchParams.get('cursor') || '', 'base64url').toString();
        const values = new Map(state.records.filter(row => row[field]).map(row => [row[field], row[field]]));
        if (field === 'key_id') state.keys.forEach(key => values.set(key.id, key.label || key.id));
        const all = [...values].filter(([value, label]) => value > cursor && (value.toLowerCase().includes(search) || label.toLowerCase().includes(search))).sort(([a], [b]) => a.localeCompare(b));
        const items = all.slice(0, limit).map(([value, label]) => ({ value, label }));
        sendJson(res, 200, { items, next_cursor: all.length > limit ? Buffer.from(items[items.length - 1].value).toString('base64url') : undefined }); return;
      }
      if (suffix === '/requests') {
        requireMethod(req, 'GET');
        const query = parseRequestQuery(url);
        const filtered = filterRecords(state.records, query);
        const start = (query.page - 1) * query.page_size;
        const items = filtered.slice(start, start + query.page_size).map(clone);
        sendJson(res, 200, {
          items,
          page: query.page,
          page_size: query.page_size,
          has_more: start + items.length < filtered.length
        });
        return;
      }

      if (suffix === '/requests/detail') {
        requireMethod(req, 'GET');
        const id = parseQueryID(url, 'GET /requests/detail');
        sendJson(res, 200, detailFor(state, id));
        return;
      }

      if (suffix === '/summary') {
        requireMethod(req, 'GET');
        const query = parseRequestQuery(url);
        sendJson(res, 200, computeSummary(filterRecords(state.records, query), query));
        return;
      }

      if (suffix === '/settings') {
        if (req.method === 'GET') {
          sendJson(res, 200, clone(state.settings));
          return;
        }
        if (req.method === 'PUT') {
          const body = await readJson(req);
          call.request_body = clone(body);
          state.settings = validateSettings(body, state.settings);
          sendJson(res, 200, clone(state.settings));
          return;
        }
        methodNotAllowed(res, 'GET, PUT');
        return;
      }

      if (suffix === '/prices') {
        requireMethod(req, 'GET');
        sendJson(res, 200, pricingSnapshot(state));
        return;
      }

      if (suffix === '/prices/preview') {
        requireMethod(req, 'POST');
        rejectAnyQuery(url);
        const body = await readJson(req);
        rejectUnknown(body, new Set(['models']), 'preview');
        if (!Array.isArray(body.models) || body.models.length > 4096) throw failure(400, 'invalid_request', 'invalid models');
        const items = body.models.map(identity => {
          rejectUnknown(identity, new Set(['model', 'provider']), 'model identity');
          if (typeof identity.model !== 'string' || !identity.model || identity.model.length > 1024 ||
              (identity.provider !== undefined && (typeof identity.provider !== 'string' || identity.provider.length > 1024))) {
            throw failure(400, 'invalid_request', 'invalid model identity');
          }
          const provider = identity.provider || '';
          const manual = state.settings.pricing.prices.find(price =>
            price.model === identity.model && (price.provider || '') === provider);
          const auto = state.settings.pricing.auto && ['gpt-5', 'gpt-5-mini'].includes(identity.model);
          const autoBase = rate(1_000_000, 8_000_000, 0, 0, null);
          const zeroDefinition = pricingDefinition({ input: 0, output: 0, cache_read: 0, cache_write: 0 });
          const definition = manual
            ? clone(manual.definition)
            : auto ? pricingDefinition(autoBase) : zeroDefinition;
          return {
            model: identity.model,
            provider,
            rates: clone(definition.base),
            definition,
            source: manual ? 'manual' : auto ? 'auto' : 'zero',
            pricing_supported: true
          };
        });
        sendJson(res, 200, { items });
        return;
      }

      if (suffix === '/prices/refresh') {
        requireMethod(req, 'POST');
        if (!state.pricesError) state.pricesLastSync = iso(Date.now());
        state.counters.priceRefreshes += 1;
        sendJson(res, 200, pricingSnapshot(state));
        return;
      }

      if (suffix === '/captures/cleanup' || suffix === '/captures/purge') {
        requireMethod(req, 'POST'); rejectAnyQuery(url);
        if (suffix.endsWith('purge')) {
          const body = await readJson(req); rejectUnknown(body, new Set(['confirm']), 'purge');
          if (body.confirm !== true) throw failure(400, 'invalid_json', 'confirm required');
        } else if ((await readBody(req)).length) throw failure(400, 'invalid_json', 'empty body required');
        sendJson(res, 200, { removed_records: 0, removed_bytes: 0 }); return;
      }
      if (suffix === '/compact') {
        requireMethod(req, 'POST');
        state.counters.compactions += 1;
        sendJson(res, 200, health(state));
        return;
      }

      sendError(res, 404, 'not_found', 'not found');
    } catch (err) {
      if (res.headersSent) return;
      if (err && err.status) {
        sendError(res, err.status, err.code || 'error', err.message);
      } else {
        sendError(res, 500, 'internal_error', 'mock server error');
      }
    }
  });

  return { server, state, calls, resetState, resetCalls };
}

function suffixOf(pathname, base) {
  const index = pathname.indexOf(base);
  if (index < 0) return null;
  const rest = pathname.slice(index + base.length);
  return rest === '' ? '/' : rest;
}

function requireMethod(req, method) {
  if (req.method !== method) {
    const err = failure(405, 'method_not_allowed', 'method not allowed');
    err.allow = method;
    throw err;
  }
}

function methodNotAllowed(res, allow) {
  res.setHeader('Allow', allow);
  sendError(res, 405, 'method_not_allowed', 'method not allowed');
}

async function startServer(options) {
  const opts = Object.assign({ port: 0, host: '127.0.0.1' }, options || {});
  const built = createServer(opts);
  const sockets = new Set();
  built.server.on('connection', (socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
  });

  await new Promise((resolve, reject) => {
    built.server.once('error', reject);
    built.server.listen(opts.port, opts.host, resolve);
  });
  const address = built.server.address();
  const origin = 'http://' + opts.host + ':' + address.port;

  return {
    origin,
    port: address.port,
    resourceURL: origin + RESOURCE_BASE + '/ui',
    managementURL: origin + MANAGEMENT_BASE,
    calls: built.calls,
    state: built.state,
    counters: built.state.counters,
    resetCalls: built.resetCalls,
    resetState: built.resetState,
    async close() {
      for (const socket of sockets) socket.destroy();
      await new Promise((resolve) => built.server.close(resolve));
    }
  };
}

module.exports = {
  ASSET_CSP,
  CAPABILITY_TTL_MS,
  CATALOG_PATH,
  DEFAULT_MANAGEMENT_KEY,
  MAX_CATALOG_MODELS,
  MAX_OUTSTANDING_CAPABILITIES,
  MANAGEMENT_BASE,
  MAX_BODY_BYTES,
  MAX_BODY_STORAGE_BYTES,
  LONG_KEY_ID,
  NANO_EXPIRY,
  NANO_KEY_ID,
  PLUGIN_ID,
  RESOURCE_BASE,
  SECRET_PREFIX,
  XSS,
  createKey,
  createSettings,
  startServer
};
