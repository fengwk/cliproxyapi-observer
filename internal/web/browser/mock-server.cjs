'use strict';

/*
 * Observer 面板浏览器验证用的同源 mock 宿主。
 *
 * 它同时扮演：
 *  - CPA 插件资源路由（/v0/resource/plugins/cliproxyapi-observer/...）——公开、带 CSP。
 *  - CPA 管理 API（/v0/management/plugins/cliproxyapi-observer/...）——要求 Bearer 密钥。
 *  - 一个模拟 management-center 的嵌入宿主页（/embed），用于验证 data-theme 跟随。
 *  - 配置管理与校验：
 *      - POST /validate：认证接收 raw patch 并使用 ui.js 纯函数校验
 *      - GET /config & PATCH /config：仅限 v0 路径（保留反向代理前缀支持）
 *      - GET /settings：由 config 转换而来，含 compact_interval_seconds、compact_min_bytes
 *  - 故障注入控制与计数（可用于单测断言）：
 *      - /__mock/control、/__mock/counters、/__mock/reset 及 startServer 导出的 controls/counters 对象
 *      - 支持模拟 validate failure, write failure, reconfigure temporary 503,
 *        effective never updates, config read fail
 *
 * 仅用于本地验证：绑定 127.0.0.1、使用假密钥与假数据，从不接触任何真实 CPA 或上游。
 */

const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const PLUGIN_ID = 'cliproxyapi-observer';
const RESOURCE_BASE = '/v0/resource/plugins/' + PLUGIN_ID;
const MANAGEMENT_BASE = '/v0/management/plugins/' + PLUGIN_ID;
const SECRET = 'observer-test-secret';

function loadCspPolicy() {
  const goPath = path.resolve(__dirname, '../../plugin/management.go');
  const content = fs.readFileSync(goPath, 'utf8');
  const match = content.match(/cspPolicy\s*=\s*"([^"]+)"/);
  if (!match || !match[1]) {
    throw new Error('Could not find cspPolicy in ' + goPath);
  }
  return match[1];
}

const ASSET_CSP = loadCspPolicy();

const XSS_MODEL = '<img src=x onerror="window.__observerXss=1">';
const XSS_PROMPT = '<script>window.__observerXss=1</script>';

const BASE_TIME = Date.UTC(2026, 0, 2, 12, 0, 0);

function asset(name) {
  return fs.readFileSync(path.join(__dirname, '..', name));
}

function parseDurationSeconds(val, defaultSec) {
  if (typeof val === 'number' && Number.isFinite(val)) return Math.floor(val);
  if (typeof val !== 'string') return defaultSec;
  const s = val.trim();
  if (!s || !/^(\d+(?:\.\d+)?(s|m|h))+$/i.test(s)) return defaultSec;
  let total = 0;
  const re = /(\d+(?:\.\d+)?)(s|m|h)/gi;
  let m;
  while ((m = re.exec(s)) !== null) {
    const n = parseFloat(m[1]);
    const unit = m[2].toLowerCase();
    if (unit === 'h') total += n * 3600;
    else if (unit === 'm') total += n * 60;
    else if (unit === 's') total += n;
  }
  return Math.round(total);
}

function createDefaultConfig(options) {
  const captureBodies =
    options && options.captureBodies !== undefined ? Boolean(options.captureBodies) : true;
  return {
    enabled: true,
    store: { fixture: 'preserve-me' },
    db: 'data/cliproxyapi-observer.db',
    'stats-retention-days': 365,
    'request-retention': '24h',
    'body-retention': '24h',
    'capture-bodies': captureBodies,
    'max-body-bytes': 1048576,
    'max-body-storage-bytes': 268435456,
    flush: '1s',
    'compact-interval': '900s',
    'compact-min-bytes': 8388608,
    prices: {
      'gpt-5.1-codex': {
        input: 1.25,
        output: 5.0,
        'cache-read': 0.3125,
        'cache-creation': 1.25
      }
    }
  };
}

function configToSettings(cfg) {
  const captureBodies =
    cfg['capture-bodies'] !== undefined ? Boolean(cfg['capture-bodies']) : Boolean(cfg.capture_bodies);
  const bodyRetentionSeconds = parseDurationSeconds(cfg['body-retention'] || cfg.body_retention, 86400);
  const requestRetentionSeconds = parseDurationSeconds(cfg['request-retention'] || cfg.request_retention, 86400);
  const statsRetentionDays = Number(cfg['stats-retention-days'] || cfg.stats_retention_days || 365);
  const maxBodyBytes = Number(cfg['max-body-bytes'] || cfg.max_body_bytes || 1048576);
  const maxBodyStorageBytes = Number(cfg['max-body-storage-bytes'] || cfg.max_body_storage_bytes || 268435456);

  const compactIntervalSeconds = Number(
    cfg.compact_interval_seconds !== undefined
      ? cfg.compact_interval_seconds
      : parseDurationSeconds(cfg['compact-interval'] || cfg.compact_interval, 900)
  );
  const compactMinBytes = Number(
    cfg.compact_min_bytes !== undefined ? cfg.compact_min_bytes : cfg['compact-min-bytes'] || 8388608
  );

  return {
    capture_bodies: captureBodies,
    body_retention_seconds: bodyRetentionSeconds,
    request_retention_seconds: requestRetentionSeconds,
    stats_retention_days: statsRetentionDays,
    max_body_bytes: maxBodyBytes,
    max_body_storage_bytes: maxBodyStorageBytes,
    compact_interval_seconds: compactIntervalSeconds,
    compact_min_bytes: compactMinBytes,
    prices: Object.fromEntries(Object.entries(cfg.prices || {}).map(([id, p]) => [id, {
      input: p.input, output: p.output,
      cache_read: p['cache-read'], cache_creation: p['cache-creation']
    }]))
  };
}

function validateSettingsPatch(patch) {
  return require('../ui.js').validateSettingsPatch(patch);
}

function computeModelCost(model, uncachedInput, output, cacheRead, cacheCreation, quality, prices) {
  if (quality !== 'complete') {
    return null;
  }
  if (!prices || !Object.prototype.hasOwnProperty.call(prices, model)) {
    return null;
  }
  const p = prices[model];
  if (!p || typeof p !== 'object') {
    return null;
  }
  const inputPrice = Number(p.input !== undefined ? p.input : 0);
  const outputPrice = Number(p.output !== undefined ? p.output : 0);
  const cacheReadPrice = Number(
    p.cache_read !== undefined
      ? p.cache_read
      : (p['cache-read'] !== undefined ? p['cache-read'] : 0)
  );
  const cacheCreationPrice = Number(
    p.cache_creation !== undefined
      ? p.cache_creation
      : (p['cache-creation'] !== undefined ? p['cache-creation'] : 0)
  );

  const cost =
    (uncachedInput * inputPrice +
      cacheRead * cacheReadPrice +
      cacheCreation * cacheCreationPrice +
      output * outputPrice) /
    1000000.0;
  if (!Number.isFinite(cost)) {
    return null;
  }
  return cost;
}

function buildSummary(prices) {
  const effectivePrices = prices || {};

  const groupTemplates = [
    {
      provider: 'openai',
      model: 'gpt-5.1-codex',
      requests: 640,
      failed_requests: 4,
      input_tokens: 2520000,
      uncached_input_tokens: 1200000,
      output_tokens: 800000,
      cache_read_tokens: 1200000,
      cache_creation_tokens: 120000,
      total_tokens: 3320000,
      quality: 'complete',
      complete_requests: 640
    },
    {
      provider: 'anthropic',
      model: 'claude-opus-4-1',
      requests: 322,
      failed_requests: 6,
      input_tokens: 1490000,
      uncached_input_tokens: 500000,
      output_tokens: 300000,
      cache_read_tokens: 900000,
      cache_creation_tokens: 90000,
      total_tokens: 1790000,
      quality: 'complete',
      complete_requests: 322
    },
    {
      provider: 'unknown-vendor',
      model: XSS_MODEL,
      requests: 120,
      failed_requests: 2,
      input_tokens: 120000,
      uncached_input_tokens: 120000,
      output_tokens: 30000,
      cache_read_tokens: 0,
      cache_creation_tokens: 0,
      total_tokens: 150000,
      quality: 'complete',
      complete_requests: 120
    },
    {
      provider: 'gemini',
      model: 'gemini-3-pro',
      requests: 152,
      failed_requests: 0,
      input_tokens: 648000,
      uncached_input_tokens: 0,
      output_tokens: 104567,
      cache_read_tokens: 245678,
      cache_creation_tokens: 24567,
      total_tokens: 723451,
      quality: 'unclassified',
      complete_requests: 0
    }
  ];

  const groups = [];
  let totalCost = 0;
  let hasAnyPriced = false;
  let totalUnpricedRequests = 0;

  for (const t of groupTemplates) {
    const cost = computeModelCost(
      t.model,
      t.uncached_input_tokens,
      t.output_tokens,
      t.cache_read_tokens,
      t.cache_creation_tokens,
      t.quality,
      effectivePrices
    );

    const isPriced = cost !== null;
    const unpriced = isPriced ? (t.requests - t.complete_requests) : t.requests;
    totalUnpricedRequests += unpriced;

    if (isPriced) {
      totalCost += cost;
      hasAnyPriced = true;
    }

    const g = {
      provider: t.provider,
      model: t.model,
      requests: t.requests,
      failed_requests: t.failed_requests,
      input_tokens: t.input_tokens,
      output_tokens: t.output_tokens,
      cache_read_tokens: t.cache_read_tokens,
      cache_creation_tokens: t.cache_creation_tokens,
      total_tokens: t.total_tokens,
      cost_usd: isPriced ? cost : null
    };
    if (unpriced > 0) {
      g.unpriced_requests = unpriced;
    }
    groups.push(g);
  }

  const series = [];
  for (let i = 0; i < 48; i += 1) {
    const requests = 20 + Math.round(20 * Math.sin(i / 4) + (i % 5));
    const failed = i % 7 === 0 ? 2 : 0;
    // Canonical input contains all three mutually exclusive input buckets.
    const uncachedInput = requests * (300 - 120);
    const cacheRead = requests * 120;
    const cacheCreation = requests * 30;
    const output = requests * 100;
    const seriesCost = computeModelCost(
      'gpt-5.1-codex',
      uncachedInput,
      output,
      cacheRead,
      cacheCreation,
      'complete',
      effectivePrices
    );
    series.push({
      time: new Date(BASE_TIME - (47 - i) * 30 * 60 * 1000).toISOString(),
      requests,
      failed_requests: failed,
      total_tokens: uncachedInput + cacheRead + cacheCreation + output,
      input_tokens: uncachedInput + cacheRead + cacheCreation,
      output_tokens: output,
      cache_read_tokens: cacheRead,
      cache_creation_tokens: cacheCreation,
      latency_ns: requests * 900000000,
      latency_samples: requests,
      ttft_ns: requests * 250000000,
      ttft_samples: requests,
      cost_usd: seriesCost !== null ? seriesCost : null,
      unpriced_requests: seriesCost !== null ? 0 : requests
    });
  }

  return {
    from: new Date(BASE_TIME - 24 * 60 * 60 * 1000).toISOString(),
    to: new Date(BASE_TIME).toISOString(),
    totals: {
      requests: 1234,
      failed_requests: 12,
      input_tokens: 4567890,
      output_tokens: 1234567,
      reasoning_tokens: 45678,
      cache_read_tokens: 2345678,
      cache_creation_tokens: 234567,
      total_tokens: 5983451,
      cache_hits: 812,
      latency_ns: 1234 * 900000000,
      latency_samples: 1234,
      ttft_ns: 1234 * 250000000,
      ttft_samples: 1234,
      cost_usd: hasAnyPriced ? totalCost : null,
      unpriced_requests: totalUnpricedRequests
    },
    groups,
    series
  };
}

function buildRequestBody(index, captureBodies, prices) {
  const failed = index % 11 === 0;
  const model = index === 0 ? XSS_MODEL : index % 3 === 0 ? 'claude-opus-4-1' : 'gpt-5.1-codex';
  const quality = index % 5 === 0 ? 'unclassified' : 'complete';
  const uncachedInput = 100 + index;
  const output = 100 + index;
  const cacheRead = index * 7;
  const cacheCreation = index * 2;
  const cost = computeModelCost(model, uncachedInput, output, cacheRead, cacheCreation, quality, prices);
  return {
    sequence: index + 1,
    request_id: index === 0 ? 'req-body-xss' : 'req-' + index,
    trace_id: 'trace-' + index,
    time: new Date(BASE_TIME - index * 60 * 1000).toISOString(),
    provider: index % 2 === 0 ? 'openai' : 'anthropic',
    model,
    alias: index % 4 === 0 ? 'codex-alias' : '',
    executor: 'native',
    stream: index % 2 === 0,
    failed,
    failure_status: failed ? 500 : 0,
    input_tokens: uncachedInput + cacheRead + cacheCreation,
    uncached_input_tokens: uncachedInput,
    output_tokens: output,
    reasoning_tokens: 10,
    cache_read_tokens: cacheRead,
    cache_creation_tokens: cacheCreation,
    total_tokens: uncachedInput + cacheRead + cacheCreation + output,
    accounting_quality: quality,
    latency_ns: (400 + index * 12) * 1000000,
    ttft_ns: (120 + index * 3) * 1000000,
    tps: index % 5 === 0 ? null : 42.5 + index,
    cache_hit: index % 3 === 0,
    cost_usd: cost,
    body_available: captureBodies !== false && index === 0
  };
}

const TOTAL_FIXTURE_REQUESTS = 52;

function buildRequestsPage(url, captureBodies, prices, controls) {
  if (url.searchParams.has('cursor')) {
    const cursor = url.searchParams.get('cursor');
    if (cursor !== null && cursor !== '') {
      const err = new Error('cursor is not supported; use offset and limit');
      err.statusCode = 400;
      throw err;
    }
  }

  let offset = 0;
  if (url.searchParams.has('offset')) {
    const rawOffset = url.searchParams.get('offset');
    if (!/^\d+$/.test(rawOffset)) {
      const err = new Error('invalid offset: must be a nonnegative integer');
      err.statusCode = 400;
      throw err;
    }
    offset = parseInt(rawOffset, 10);
    if (!Number.isSafeInteger(offset) || offset < 0 || offset > 2147483647) {
      const err = new Error('offset out of range (max 2147483647)');
      err.statusCode = 400;
      throw err;
    }
  }

  let limit = 50;
  if (url.searchParams.has('limit')) {
    const rawLimit = url.searchParams.get('limit');
    if (!/^\d+$/.test(rawLimit)) {
      const err = new Error('invalid limit: must be an integer between 1 and 100');
      err.statusCode = 400;
      throw err;
    }
    limit = parseInt(rawLimit, 10);
    if (limit < 1 || limit > 100) {
      const err = new Error('limit out of range: must be between 1 and 100');
      err.statusCode = 400;
      throw err;
    }
  }

  let allItems;
  if (controls && Array.isArray(controls.customRequestsItems)) {
    allItems = controls.customRequestsItems;
  } else {
    allItems = [];
    const count =
      controls && typeof controls.totalRequestsCount === 'number'
        ? controls.totalRequestsCount
        : TOTAL_FIXTURE_REQUESTS;
    for (let i = 0; i < count; i += 1) {
      allItems.push(buildRequestBody(i, captureBodies, prices));
    }
  }

  const providerFilter = url.searchParams.get('provider') || '';
  const modelFilter = url.searchParams.get('model') || '';
  let filtered = allItems;
  if (providerFilter) {
    filtered = filtered.filter((it) => it.provider === providerFilter);
  }
  if (modelFilter) {
    filtered = filtered.filter((it) => it.model === modelFilter);
  }

  const items = filtered.slice(offset, offset + limit);
  const hasMore = offset + items.length < filtered.length;

  return {
    items,
    offset,
    limit,
    has_more: hasMore
  };
}

function buildBodyDetail(requestId) {
  return {
    request_id: requestId,
    trace_id: 'trace-0',
    created_at: new Date(BASE_TIME).toISOString(),
    expires_at: new Date(BASE_TIME + 24 * 60 * 60 * 1000).toISOString(),
    body: '{\n  "model": "gpt-5.1-codex",\n  "prompt": "' + XSS_PROMPT + '",\n  "authorization": "[redacted]",\n  "max_tokens": 1024\n}',
    redacted: true,
    size_bytes: 512
  };
}

function json(res, status, payload, extraHeaders) {
  const body = Buffer.from(JSON.stringify(payload), 'utf8');
  res.writeHead(
    status,
    Object.assign(
      {
        'Content-Type': 'application/json; charset=utf-8',
        'Cache-Control': 'no-store',
        'X-Content-Type-Options': 'nosniff',
        'Referrer-Policy': 'no-referrer',
        'Content-Length': body.length
      },
      extraHeaders || {}
    )
  );
  res.end(body);
}

function assetResponse(res, name, contentType) {
  const body = asset(name);
  res.writeHead(200, {
    'Content-Type': contentType,
    'Cache-Control': 'no-store',
    'X-Content-Type-Options': 'nosniff',
    'Content-Security-Policy': ASSET_CSP,
    'Referrer-Policy': 'no-referrer',
    'Content-Length': body.length
  });
  res.end(body);
}

function embedPage() {
  return `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="utf-8" />
    <title>management-center 模拟宿主</title>
    <style>
      body { font-family: system-ui, sans-serif; margin: 0; background: #faf9f5; color: #2d2a26; }
      .host-bar { display: flex; gap: 8px; align-items: center; padding: 10px 14px; border-bottom: 1px solid #e3e1db; }
      iframe { display: block; width: 100%; height: 1500px; border: 0; }
      .host-overlay { position: fixed; top: 52px; right: 0; width: 170px; height: 52px; background: #ddd8ce; color: #2d2a26; z-index: 20; padding: 12px; }
    </style>
  </head>
  <body>
    <div class="host-bar">
      <strong>模拟宿主</strong>
      <button type="button" data-theme-value="">浅色</button>
      <button type="button" data-theme-value="white">纯白</button>
      <button type="button" data-theme-value="dark">深色</button>
    </div>
    <iframe id="frame" src="${RESOURCE_BASE}/ui" title="Observer"></iframe>
    <div class="host-overlay" data-testid="host-overlay">CPA 宿主工具</div>
    <script>
      function setTheme(value) {
        if (value) document.documentElement.setAttribute('data-theme', value);
        else document.documentElement.removeAttribute('data-theme');
      }
      document.querySelectorAll('[data-theme-value]').forEach(function (button) {
        button.addEventListener('click', function () {
          setTheme(button.getAttribute('data-theme-value'));
        });
      });
      window.__setTheme = setTheme;
    </script>
  </body>
</html>`;
}

function readJsonBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8').trim();
      if (!text) return resolve({});
      try {
        resolve(JSON.parse(text));
      } catch (err) {
        reject(new Error('Invalid JSON: ' + err.message));
      }
    });
    req.on('error', reject);
  });
}

function createHandlerState(options) {
  const opts = options || {};
  let currentConfig = createDefaultConfig(opts);
  if (opts.config && typeof opts.config === 'object') {
    Object.assign(currentConfig, opts.config);
  }
  let effectiveSettings = configToSettings(currentConfig);

  const controls = {
    simulateValidateFailure: false,
    simulateWriteFailure: false,
    simulateReconfigureTemporary503: false,
    reconfigureTemporary503Count: 0,
    simulateEffectiveNeverUpdates: false,
    simulateConfigReadFail: false,
    simulateRequests500: false,
    requestsDelayMs: 0,
    totalRequestsCount: TOTAL_FIXTURE_REQUESTS,
    customRequestsItems: null
  };

  const counters = {
    validations: 0,
    validateFailures: 0,
    configReads: 0,
    readFailures: 0,
    configWrites: 0,
    writeFailures: 0,
    reconfigure503s: 0,
    settingsReads: 0,
    requestsReads: 0,
    summaryReads: 0
  };

  function reset() {
    controls.simulateValidateFailure = false;
    controls.simulateWriteFailure = false;
    controls.simulateReconfigureTemporary503 = false;
    controls.reconfigureTemporary503Count = 0;
    controls.simulateEffectiveNeverUpdates = false;
    controls.simulateConfigReadFail = false;
    controls.simulateRequests500 = false;
    controls.requestsDelayMs = 0;
    controls.totalRequestsCount = TOTAL_FIXTURE_REQUESTS;
    controls.customRequestsItems = null;

    counters.validations = 0;
    counters.validateFailures = 0;
    counters.configReads = 0;
    counters.readFailures = 0;
    counters.configWrites = 0;
    counters.writeFailures = 0;
    counters.reconfigure503s = 0;
    counters.settingsReads = 0;
    counters.requestsReads = 0;
    counters.summaryReads = 0;
  }

  async function handler(req, res) {
    try {
      const url = new URL(req.url, 'http://127.0.0.1');
      const pathname = url.pathname;

      // 1. Mock Control 接口
      if (pathname === '/__mock/control') {
        if (req.method === 'POST') {
          const body = await readJsonBody(req);
          Object.assign(controls, body);
          return json(res, 200, { ok: true, controls });
        }
        return json(res, 200, controls);
      }

      if (pathname === '/__mock/counters') {
        return json(res, 200, counters);
      }

      if (pathname === '/__mock/reset') {
        reset();
        currentConfig = createDefaultConfig(opts);
        effectiveSettings = configToSettings(currentConfig);
        return json(res, 200, { ok: true, controls, counters });
      }

      // 2. 模拟宿主页
      if (req.method === 'GET' && pathname === '/embed') {
        const page = Buffer.from(embedPage(), 'utf8');
        res.writeHead(200, {
          'Content-Type': 'text/html; charset=utf-8',
          'Cache-Control': 'no-store',
          'X-Content-Type-Options': 'nosniff',
          'Content-Length': page.length
        });
        res.end(page);
        return;
      }

      // 3. 静态资源路由（支持 reverse proxy）
      const matchResource = pathname.match(/^(?:.*\/)?v(?:0|8)\/resource\/plugins\/cliproxyapi-observer\/(.*)$/);
      if (req.method === 'GET' && matchResource) {
        const name = matchResource[1];
        if (name === 'ui' || name === 'ui.html') return assetResponse(res, 'ui.html', 'text/html; charset=utf-8');
        if (name === 'ui.js') return assetResponse(res, 'ui.js', 'text/javascript; charset=utf-8');
        if (name === 'ui.css') return assetResponse(res, 'ui.css', 'text/css; charset=utf-8');
        res.writeHead(404).end();
        return;
      }

      // 4. 管理 API 路由（支持 reverse proxy）
      const matchMgmt = pathname.match(/^(?:.*\/)?(v[0-9]+)\/management\/plugins\/cliproxyapi-observer(?:\/(.*))?$/);
      if (matchMgmt) {
        const version = matchMgmt[1];
        const endpoint = matchMgmt[2] || '';

        // 认证校验
        const auth = req.headers.authorization || '';
        if (auth !== 'Bearer ' + SECRET) {
          return json(res, 403, { error: 'unauthorized' });
        }

        // POST /validate
        if (endpoint === 'validate') {
          if (req.method !== 'POST') {
            return json(res, 405, { error: 'method not allowed' });
          }
          counters.validations += 1;
          if (controls.simulateValidateFailure) {
            counters.validateFailures += 1;
            return json(res, 400, { error: 'simulated validate failure' });
          }
          try {
            const patch = await readJsonBody(req);
            validateSettingsPatch(Object.assign(require('../ui.js').settingsToPatch(effectiveSettings), patch));
            return json(res, 200, { valid: true });
          } catch (err) {
            counters.validateFailures += 1;
            return json(res, 400, { error: err.message });
          }
        }

        // GET/PATCH /config ONLY v0 路径（保留 reverse proxy）
        if (endpoint === 'config') {
          if (version !== 'v0') {
            return json(res, 404, { error: 'not found' });
          }
          if (req.method === 'GET') {
            counters.configReads += 1;
            if (controls.simulateConfigReadFail) {
              counters.readFailures += 1;
              return json(res, 500, { error: 'simulated config read failure' });
            }
            return json(res, 200, currentConfig);
          }
          if (req.method === 'PATCH') {
            counters.configWrites += 1;
            if (controls.simulateWriteFailure) {
              counters.writeFailures += 1;
              return json(res, 500, { error: 'simulated write failure' });
            }
            try {
              const patch = await readJsonBody(req);
              validateSettingsPatch(Object.assign(require('../ui.js').settingsToPatch(effectiveSettings), patch));
              Object.assign(currentConfig, patch);
              if (!controls.simulateEffectiveNeverUpdates) {
                effectiveSettings = configToSettings(currentConfig);
              }
              return json(res, 200, { status: 'ok' });
            } catch (err) {
              counters.writeFailures += 1;
              return json(res, 400, { error: err.message });
            }
          }
          return json(res, 405, { error: 'method not allowed' });
        }

        // 已有 GET 接口
        if (req.method === 'GET') {
          switch (endpoint) {
            case 'summary':
              counters.summaryReads += 1;
              return json(res, 200, buildSummary(effectiveSettings.prices));
            case 'requests': {
              counters.requestsReads += 1;
              if (controls.simulateRequests500) {
                return json(res, 500, { error: 'simulated requests failure' });
              }
              if (controls.requestsDelayMs > 0) {
                await new Promise((r) => setTimeout(r, controls.requestsDelayMs));
              }
              try {
                const capture = effectiveSettings.capture_bodies;
                const page = buildRequestsPage(url, capture, effectiveSettings.prices, controls);
                return json(res, 200, page);
              } catch (err) {
                return json(res, err.statusCode || 400, { error: err.message });
              }
            }
            case 'settings': {
              counters.settingsReads += 1;
              if (controls.simulateReconfigureTemporary503 || controls.reconfigureTemporary503Count > 0) {
                if (controls.reconfigureTemporary503Count > 0) controls.reconfigureTemporary503Count -= 1;
                counters.reconfigure503s += 1;
                return json(res, 503, { error: 'simulated temporary 503 reconfiguring' });
              }
              return json(res, 200, effectiveSettings);
            }
            case 'health':
              return json(res, 200, {
                dropped_usage: 3, dropped_bodies: 0, write_errors: 1, queued: 0,
                database_bytes: 33554432, reclaimable_bytes: 8388608,
                compactions: 2, compaction_errors: 1, last_compaction_unix: BASE_TIME / 1000,
                last_compaction_reclaimed_bytes: 4194304
              });
            case 'body': {
              const capture = effectiveSettings.capture_bodies;
              const requestId = url.searchParams.get('request_id') || '';
              if (!capture || !requestId) return json(res, 404, { error: 'not found' });
              return json(res, 200, buildBodyDetail(requestId));
            }
            default:
              return json(res, 404, { error: 'not found' });
          }
        }
      }

      res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
      res.end('not found');
    } catch (unexpected) {
      json(res, 500, { error: unexpected && unexpected.message ? unexpected.message : 'server error' });
    }
  }

  return {
    handler,
    controls,
    counters,
    reset,
    getConfig: () => currentConfig,
    setConfig: (cfg) => {
      currentConfig = cfg;
      if (!controls.simulateEffectiveNeverUpdates) effectiveSettings = configToSettings(cfg);
    },
    getSettings: () => effectiveSettings
  };
}

function createHandler(options) {
  const state = createHandlerState(options);
  return state.handler;
}

function startServer(options) {
  const state = createHandlerState(options);
  const server = http.createServer(state.handler);
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const port = server.address().port;
      resolve({
        server,
        port,
        origin: 'http://127.0.0.1:' + port,
        resourceURL: 'http://127.0.0.1:' + port + RESOURCE_BASE + '/ui',
        embedURL: 'http://127.0.0.1:' + port + '/embed',
        controls: state.controls,
        counters: state.counters,
        reset: state.reset,
        getConfig: state.getConfig,
        setConfig: state.setConfig,
        getSettings: state.getSettings,
        close: () => new Promise((done) => server.close(done))
      });
    });
  });
}

module.exports = {
  startServer,
  createHandler,
  SECRET,
  PLUGIN_ID,
  RESOURCE_BASE,
  MANAGEMENT_BASE,
  ASSET_CSP,
  XSS_MODEL,
  XSS_PROMPT,
  embedPage,
  configToSettings,
  validateSettingsPatch,
  computeModelCost,
  buildRequestsPage,
  buildSummary,
  TOTAL_FIXTURE_REQUESTS
};
