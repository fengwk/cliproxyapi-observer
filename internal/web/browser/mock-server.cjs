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

const PLUGIN_ID = 'cliproxyapi-observer';
const RESOURCE_BASE = '/v0/resource/plugins/' + PLUGIN_ID;
const MANAGEMENT_BASE = '/v0/management/plugins/' + PLUGIN_ID;
const DEFAULT_MANAGEMENT_KEY = 'observer-browser-secret';
const MAX_BODY_BYTES = 1024 * 1024;
const CATALOG_PATH = '/v1/models';
const CAPABILITY_TTL_MS = 30 * 1000;
const MAX_OUTSTANDING_CAPABILITIES = 32;
const MAX_CATALOG_MODELS = 100;
const ASSET_DIR = path.resolve(__dirname, '..');

const SECRET_PREFIX = 'observer_sk_';

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
  delete out.revoked;
  delete out.secret_hash;
  delete out.secret_rotated_at;
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

function rate(input, output, cacheRead, cacheWrite) {
  return { input, output, cache_read: cacheRead, cache_write: cacheWrite };
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
        rule_id: '',
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
      price_source: 'unpriced',
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
      price_source: 'rule',
      rule_id: 'rule-night',
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
    })
  ];
}

function createSettings() {
  return {
    capture_enabled: false,
    max_body_bytes: 1_048_576,
    max_body_storage_bytes: 268_435_456,
    raw_retention_hours: 24,
    daily_retention_days: 365,
    pricing: {
      auto: true,
      prices: [
        {
          model: 'gpt-5-codex',
          rates: rate(1_250_000, 10_000_000, 125_000, 1_250_000),
          source: 'manual'
        }
      ],
      rules: [
        {
          id: 'rule-night',
          model: 'claude-opus-4-1',
          input_tokens_gt: 1000,
          time_range: '00:00-23:59',
          rates: rate(3_000_000, 15_000_000, 300_000, 3_000_000)
        }
      ]
    }
  };
}

function createState(options) {
  const now = Date.now();
  return {
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
    secrets: new Map(),
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
  '/ui.css': 'ui.css'
};
const ASSET_TYPES = {
  'ui.html': 'text/html; charset=utf-8',
  'ui.js': 'text/javascript; charset=utf-8',
  'ui.css': 'text/css; charset=utf-8'
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
    if (file === 'ui.js') {
      // Go 的 Asset("ui.js") 是 settings.js + "\n" + models.js + "\n" + ui.js；
      // 固定端点只暴露 bundle，不单独暴露源文件。
      body = Buffer.concat([
        fs.readFileSync(path.join(ASSET_DIR, 'settings.js')),
        Buffer.from('\n'),
        fs.readFileSync(path.join(ASSET_DIR, 'models.js')),
        Buffer.from('\n'),
        fs.readFileSync(path.join(ASSET_DIR, 'ui.js'))
      ]);
    } else {
      body = fs.readFileSync(path.join(ASSET_DIR, file));
    }
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

function computeSummary(records, query) {
  const bucketMs = query.to - query.from <= 48 * 3600 * 1000 ? 3600 * 1000 : 24 * 3600 * 1000;
  const granularity = bucketMs === 3600 * 1000 ? 'hour' : 'day';
  const buckets = new Map();
  const start = Math.floor(query.from / bucketMs) * bucketMs;
  for (let t = start; t < query.to && buckets.size < 400; t += bucketMs) {
    buckets.set(t, { time: iso(t), requests: 0, failures: 0, cost_micro_usd: 0, tokens: 0 });
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
    const key = Math.floor(Date.parse(record.time) / bucketMs) * bucketMs;
    let bucket = buckets.get(key);
    if (!bucket) {
      bucket = { time: iso(key), requests: 0, failures: 0, cost_micro_usd: 0, tokens: 0 };
      buckets.set(key, bucket);
    }
    bucket.requests += 1;
    if (record.failed) bucket.failures += 1;
    bucket.cost_micro_usd += record.cost_micro_usd || 0;
    bucket.tokens += record.tokens ? record.tokens.total || 0 : 0;
  }
  summary.buckets = Array.from(buckets.values()).sort((a, b) => Date.parse(a.time) - Date.parse(b.time));
  return summary;
}

function pricingSnapshot(state) {
  // 目录快照允许同一模型存在多个 provider 的独立费率；手工价仍是 model-wide。
  const catalog = [
    {
      model: 'gpt-5-codex',
      provider: 'openai',
      rates: rate(1_000_000, 8_000_000, 100_000, 1_000_000),
      source: 'auto'
    },
    {
      model: 'gpt-5-codex',
      provider: 'azure',
      rates: rate(1_500_000, 12_000_000, 150_000, 1_500_000),
      source: 'auto'
    }
  ];
  return {
    prices: clone(state.settings.pricing.prices).concat(catalog),
    last_sync: state.pricesLastSync,
    error: '',
    refreshing: false
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
    schema_version: 2
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

function validateRates(rates, name) {
  if (rates === null || typeof rates !== 'object' || Array.isArray(rates)) {
    throw failure(400, 'invalid_json', name + ' must be an object');
  }
  rejectUnknown(rates, new Set(['input', 'output', 'cache_read', 'cache_write', 'reasoning']), name);
  for (const field of ['input', 'output', 'cache_read', 'cache_write']) {
    if (rates[field] === undefined) throw failure(400, 'invalid_json', name + '.' + field + ' is required');
    ensureInteger(rates[field], name + '.' + field, 0);
  }
  if (rates.reasoning !== undefined && rates.reasoning !== null) {
    ensureInteger(rates.reasoning, name + '.reasoning', 0);
  }
}

function validatePricing(pricing) {
  if (pricing === null || typeof pricing !== 'object' || Array.isArray(pricing)) {
    throw failure(400, 'invalid_json', 'pricing must be an object');
  }
  rejectUnknown(pricing, new Set(['auto', 'prices', 'rules']), 'pricing');
  if (typeof pricing.auto !== 'boolean') throw failure(400, 'invalid_json', 'pricing.auto must be a boolean');
  if (!Array.isArray(pricing.prices) || !Array.isArray(pricing.rules)) {
    throw failure(400, 'invalid_json', 'pricing.prices and pricing.rules must be arrays');
  }
  pricing.prices.forEach((price, index) => {
    rejectUnknown(price, new Set(['model', 'rates', 'source']), 'Price');
    if (typeof price.model !== 'string' || price.model === '') {
      throw failure(400, 'invalid_json', 'prices[' + index + '].model must be nonempty');
    }
    validateRates(price.rates, 'prices[' + index + '].rates');
  });
  pricing.rules.forEach((rule, index) => {
    rejectUnknown(rule, new Set(['id', 'model', 'input_tokens_gt', 'time_range', 'rates']), 'Rule');
    if (typeof rule.id !== 'string' || typeof rule.model !== 'string') {
      throw failure(400, 'invalid_json', 'rules[' + index + '] requires id and model strings');
    }
    if (rule.input_tokens_gt !== undefined && rule.input_tokens_gt !== null) {
      ensureInteger(rule.input_tokens_gt, 'rules[' + index + '].input_tokens_gt', 0);
    }
    if (rule.time_range !== undefined && typeof rule.time_range !== 'string') {
      throw failure(400, 'invalid_json', 'rules[' + index + '].time_range must be a string');
    }
    validateRates(rule.rates, 'rules[' + index + '].rates');
  });
}

function validateSettings(body) {
  rejectUnknown(
    body,
    new Set([
      'capture_enabled',
      'max_body_bytes',
      'max_body_storage_bytes',
      'raw_retention_hours',
      'daily_retention_days',
      'pricing'
    ]),
    'Settings'
  );
  if (typeof body.capture_enabled !== 'boolean') {
    throw failure(400, 'invalid_json', 'capture_enabled must be a boolean');
  }
  ensureInteger(body.max_body_bytes, 'max_body_bytes', 0);
  ensureInteger(body.max_body_storage_bytes, 'max_body_storage_bytes', 0);
  ensureInteger(body.raw_retention_hours, 'raw_retention_hours', 1);
  if (body.raw_retention_hours > 168) throw failure(400, 'invalid_json', 'raw_retention_hours must be <= 168');
  ensureInteger(body.daily_retention_days, 'daily_retention_days', 1);
  if (body.daily_retention_days > 3650) {
    throw failure(400, 'invalid_json', 'daily_retention_days must be <= 3650');
  }
  validatePricing(body.pricing);
  return clone(body);
}

function newSecret() {
  return SECRET_PREFIX + crypto.randomBytes(32).toString('hex');
}

function findKey(state, id) {
  const key = state.keys.find((item) => item.id === id);
  if (!key) throw failure(404, 'not_found', 'key not found');
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
          rejectAnyQuery(url);
          sendJson(res, 200, { items: state.keys.map(publicKey) });
          return;
        }
        if (req.method === 'POST') {
          rejectAnyQuery(url);
          const body = await readJson(req);
          call.request_body = clone(body);
          const spec = normalizeKeySpec(body);
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
          const secret = newSecret();
          state.keys.push(key);
          state.secrets.set(id, crypto.createHash('sha256').update(secret).digest('hex'));
          state.counters.keyCreates += 1;
          sendJson(res, 201, { key: publicKey(key), secret });
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
          sendJson(res, 200, publicKey(key));
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

      if (suffix === '/keys/rotate') {
        requireMethod(req, 'POST');
        const id = parseQueryID(url, 'POST /keys/rotate');
        const key = findKey(state, id);
        const secret = newSecret();
        state.secrets.set(key.id, crypto.createHash('sha256').update(secret).digest('hex'));
        key.secret_rotated_at = iso(Date.now());
        state.counters.keyRotates += 1;
        sendJson(res, 200, { key: publicKey(key), secret });
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
        sendJson(res, 200, publicKey(key));
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
        sendJson(res, 200, publicKey(key));
        return;
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
          state.settings = validateSettings(body);
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

      if (suffix === '/prices/refresh') {
        requireMethod(req, 'POST');
        state.pricesLastSync = iso(Date.now());
        state.counters.priceRefreshes += 1;
        sendJson(res, 200, pricingSnapshot(state));
        return;
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
  PLUGIN_ID,
  RESOURCE_BASE,
  SECRET_PREFIX,
  XSS,
  createKey,
  createSettings,
  startServer
};
