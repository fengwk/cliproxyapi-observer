'use strict';

/*
 * Observer 面板浏览器验证用的同源 mock 宿主。
 *
 * 它同时扮演：
 *  - CPA 插件资源路由（/v0/resource/plugins/cliproxyapi-observer/...）——公开、带 CSP。
 *  - CPA 管理 API（/v0/management/plugins/cliproxyapi-observer/...）——要求 Bearer 密钥。
 *  - 一个模拟 management-center 的嵌入宿主页（/embed），用于验证 data-theme 跟随。
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

const ASSET_CSP =
  "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'";

const XSS_MODEL = '<img src=x onerror="window.__observerXss=1">';
const XSS_PROMPT = '<script>window.__observerXss=1</script>';

const BASE_TIME = Date.UTC(2026, 0, 2, 12, 0, 0);

function asset(name) {
  return fs.readFileSync(path.join(__dirname, '..', name));
}

function buildSummary() {
  const series = [];
  for (let i = 0; i < 48; i += 1) {
    const requests = 20 + Math.round(20 * Math.sin(i / 4) + (i % 5));
    const failed = i % 7 === 0 ? 2 : 0;
    series.push({
      time: new Date(BASE_TIME - (47 - i) * 30 * 60 * 1000).toISOString(),
      requests,
      failed_requests: failed,
      total_tokens: requests * 400,
      input_tokens: requests * 300,
      output_tokens: requests * 100,
      cache_read_tokens: requests * 120,
      cache_creation_tokens: requests * 30,
      latency_ns: requests * 900000000,
      latency_samples: requests,
      ttft_ns: requests * 250000000,
      ttft_samples: requests,
      cost_usd: requests * 0.0012,
      unpriced_requests: 0
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
      cost_usd: 1.2345,
      unpriced_requests: 3
    },
    groups: [
      { provider: 'openai', model: 'gpt-5.1-codex', requests: 640, failed_requests: 4, input_tokens: 2400000, output_tokens: 800000, cache_read_tokens: 1200000, cache_creation_tokens: 120000, total_tokens: 3320000, cost_usd: 0.8123 },
      { provider: 'anthropic', model: 'claude-opus-4-1', requests: 322, failed_requests: 6, input_tokens: 1400000, output_tokens: 300000, cache_read_tokens: 900000, cache_creation_tokens: 90000, total_tokens: 1790000, cost_usd: 0.4021 },
      { provider: 'unknown-vendor', model: XSS_MODEL, requests: 120, failed_requests: 2, input_tokens: 120000, output_tokens: 30000, cache_read_tokens: 0, cache_creation_tokens: 0, total_tokens: 150000, cost_usd: 0.0201 },
      { provider: 'gemini', model: 'gemini-3-pro', requests: 152, failed_requests: 0, input_tokens: 648000, output_tokens: 104567, cache_read_tokens: 245678, cache_creation_tokens: 24567, total_tokens: 723451, cost_usd: null }
    ],
    series
  };
}

function buildRequestBody(index, captureBodies) {
  const failed = index % 11 === 0;
  return {
    sequence: index + 1,
    request_id: index === 0 ? 'req-body-xss' : 'req-' + index,
    trace_id: 'trace-' + index,
    time: new Date(BASE_TIME - index * 60 * 1000).toISOString(),
    provider: index % 2 === 0 ? 'openai' : 'anthropic',
    model: index === 0 ? XSS_MODEL : index % 3 === 0 ? 'claude-opus-4-1' : 'gpt-5.1-codex',
    alias: index % 4 === 0 ? 'codex-alias' : '',
    executor: 'native',
    stream: index % 2 === 0,
    failed,
    failure_status: failed ? 500 : 0,
    input_tokens: 300 + index,
    uncached_input_tokens: 100 + index,
    output_tokens: 100 + index,
    reasoning_tokens: 10,
    cache_read_tokens: index * 7,
    cache_creation_tokens: index * 2,
    total_tokens: 410 + index * 2,
    accounting_quality: index % 5 === 0 ? 'unclassified' : 'exact',
    latency_ns: (400 + index * 12) * 1000000,
    ttft_ns: (120 + index * 3) * 1000000,
    tps: index % 5 === 0 ? null : 42.5 + index,
    cache_hit: index % 3 === 0,
    cost_usd: index % 5 === 0 ? null : 0.001 * (index + 1),
    body_available: captureBodies !== false && index === 0
  };
}

function buildRequestsPage(url, captureBodies) {
  const cursor = url.searchParams.get('cursor') || '';
  if (cursor === 'page-2') {
    return {
      items: [buildRequestBody(60, captureBodies), buildRequestBody(61, captureBodies)],
      next_cursor: '',
      has_more: false
    };
  }
  const items = [];
  for (let i = 0; i < 50; i += 1) items.push(buildRequestBody(i, captureBodies));
  return { items, next_cursor: 'page-2', has_more: true };
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

function createHandler(options) {
  const captureBodies = options.captureBodies !== false;
  return function handler(req, res) {
    const url = new URL(req.url, 'http://127.0.0.1');
    const pathname = url.pathname;

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

    if (req.method === 'GET' && pathname.startsWith(RESOURCE_BASE + '/')) {
      const name = pathname.slice(RESOURCE_BASE.length + 1);
      if (name === 'ui' || name === 'ui.html') return assetResponse(res, 'ui.html', 'text/html; charset=utf-8');
      if (name === 'ui.js') return assetResponse(res, 'ui.js', 'text/javascript; charset=utf-8');
      if (name === 'ui.css') return assetResponse(res, 'ui.css', 'text/css; charset=utf-8');
      res.writeHead(404).end();
      return;
    }

    if (req.method === 'GET' && pathname.startsWith(MANAGEMENT_BASE + '/')) {
      const auth = req.headers.authorization || '';
      if (auth !== 'Bearer ' + SECRET) {
        json(res, 403, { error: 'unauthorized' });
        return;
      }
      const endpoint = pathname.slice(MANAGEMENT_BASE.length + 1);
      switch (endpoint) {
        case 'summary':
          return json(res, 200, buildSummary());
        case 'requests':
          return json(res, 200, buildRequestsPage(url, captureBodies));
        case 'settings':
          return json(res, 200, {
            capture_bodies: captureBodies,
            body_retention_seconds: 86400,
            request_retention_seconds: 86400,
            stats_retention_days: 365,
            max_body_bytes: 1048576,
            max_body_storage_bytes: 268435456
          });
        case 'health':
          return json(res, 200, { dropped_usage: 3, dropped_bodies: 0, write_errors: 1, queued: 0 });
        case 'body': {
          const requestId = url.searchParams.get('request_id') || '';
          if (!captureBodies || !requestId) return json(res, 404, { error: 'not found' });
          return json(res, 200, buildBodyDetail(requestId));
        }
        default:
          return json(res, 404, { error: 'not found' });
      }
    }

    res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end('not found');
  };
}

function startServer(options) {
  const server = http.createServer(createHandler(options || {}));
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const port = server.address().port;
      resolve({
        server,
        port,
        origin: 'http://127.0.0.1:' + port,
        resourceURL: 'http://127.0.0.1:' + port + RESOURCE_BASE + '/ui',
        embedURL: 'http://127.0.0.1:' + port + '/embed',
        close: () => new Promise((done) => server.close(done))
      });
    });
  });
}

module.exports = {
  startServer,
  SECRET,
  PLUGIN_ID,
  RESOURCE_BASE,
  MANAGEMENT_BASE,
  XSS_MODEL,
  XSS_PROMPT,
  embedPage
};
