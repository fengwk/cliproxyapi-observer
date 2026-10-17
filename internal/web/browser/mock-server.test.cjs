'use strict';

/*
 * 零依赖 mock 合约回归。运行：
 *   node --test internal/web/browser/mock-server.test.cjs
 *
 * 这里不启动浏览器，只验证 run.cjs 所依赖的假宿主确实实现了 DESIGN.md 的管理
 * API 形状、鉴权、输入拒绝、筛选/分页/汇总和假安全数据。浏览器断言由 run.cjs
 * 在真实 /usr/bin/google-chrome-stable 中完成。
 */

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const mock = require('./mock-server.cjs');

const KEY = mock.DEFAULT_MANAGEMENT_KEY;
const BASE = mock.MANAGEMENT_BASE;
const AUTH = { Authorization: 'Bearer ' + KEY };

async function withServer(fn) {
  const server = await mock.startServer();
  try {
    return await fn(server);
  } finally {
    await server.close();
  }
}

async function json(server, path, options) {
  const opts = Object.assign({}, options || {});
  opts.headers = Object.assign({}, AUTH, opts.headers || {});
  if (opts.body && typeof opts.body !== 'string') {
    opts.body = JSON.stringify(opts.body);
    opts.headers['Content-Type'] = 'application/json';
  }
  const res = await fetch(server.origin + BASE + path, opts);
  const text = await res.text();
  return { res, body: text ? JSON.parse(text) : null };
}

function errorShape(body) {
  return body && body.error && typeof body.error.code === 'string' && typeof body.error.message === 'string';
}

test('assets: public, CSP-protected, no-store, and available from both prefixes', async () => {
  await withServer(async (server) => {
    for (const prefix of [mock.RESOURCE_BASE, BASE]) {
      for (const suffix of ['/ui', '/ui/', '/ui.html', '/ui.js', '/ui.css']) {
        const res = await fetch(server.origin + prefix + suffix);
        assert.equal(res.status, 200, prefix + suffix);
        assert.equal(res.headers.get('content-security-policy'), mock.ASSET_CSP);
        assert.equal(res.headers.get('cache-control'), 'no-store');
        assert.equal(res.headers.get('access-control-allow-origin'), null);
      }
    }
    const html = await (await fetch(server.origin + mock.RESOURCE_BASE + '/ui')).text();
    assert.match(html, /<html/i);

    // Go Asset("ui.js") 固定为 settings.js + "\n" + ui.js；mock 必须一致，
    // 且 settings.js 不能作为独立公开资源泄漏。
    const bundle = await (await fetch(server.origin + mock.RESOURCE_BASE + '/ui.js')).text();
    const settingsSource = fs.readFileSync(path.resolve(__dirname, '../settings.js'), 'utf8');
    const uiSource = fs.readFileSync(path.resolve(__dirname, '../ui.js'), 'utf8');
    assert.equal(bundle, settingsSource + '\n' + uiSource);
    assert.ok(bundle.indexOf('ObserverSettings') < bundle.lastIndexOf('ObserverUI'));
    const hidden = await fetch(server.origin + mock.RESOURCE_BASE + '/settings.js');
    assert.equal(hidden.status, 404);
  });
});

test('management auth: bearer required and errors use the standard envelope', async () => {
  await withServer(async (server) => {
    const unauthorized = await fetch(server.origin + BASE + '/health');
    assert.equal(unauthorized.status, 401);
    const unauthorizedBody = await unauthorized.json();
    assert.ok(errorShape(unauthorizedBody));
    assert.match(unauthorizedBody.error.message, /invalid management key/);

    const wrong = await fetch(server.origin + BASE + '/health', {
      headers: { Authorization: 'Bearer wrong-secret' }
    });
    assert.equal(wrong.status, 401);

    const ok = await json(server, '/health');
    assert.equal(ok.res.status, 200);
    assert.equal(ok.body.schema_version, 2);
    assert.equal(typeof ok.body.version, 'string');
    for (const field of ['database_bytes', 'dropped_captures', 'pending_usage', 'active_requests']) {
      assert.equal(typeof ok.body[field], 'number', field);
    }
    assert.equal(ok.res.headers.get('cache-control'), 'no-store');
    assert.equal(ok.res.headers.get('access-control-allow-origin'), null);
  });
});

test('requests: filters, RFC3339 validation, pagination and no accidental detail reads', async () => {
  await withServer(async (server) => {
    const first = await json(server, '/requests');
    assert.equal(first.res.status, 200);
    assert.equal(first.body.page, 1);
    assert.equal(first.body.page_size, 25);
    assert.ok(Array.isArray(first.body.items) && first.body.items.length > 0);
    assert.equal(typeof first.body.has_more, 'boolean');

    const item = first.body.items[0];
    for (const field of [
      'id', 'trace_id', 'key_id', 'time', 'model', 'provider', 'auth_index', 'failed',
      'status_code', 'stream', 'latency_ms', 'ttft_ms', 'tokens', 'raw_usage',
      'cost_micro_usd', 'price_source', 'accounting_quality'
    ]) {
      assert.ok(Object.prototype.hasOwnProperty.call(item, field), field);
    }
    for (const field of ['input', 'output', 'cache_read', 'cache_write', 'reasoning', 'total']) {
      assert.equal(typeof item.tokens[field], 'number', 'tokens.' + field);
    }

    const failed = await json(server, '/requests?status=failed');
    assert.ok(failed.body.items.length > 0);
    assert.ok(failed.body.items.every((record) => record.failed === true));

    const malicious = await json(server, '/requests?model=' + encodeURIComponent(mock.XSS.model));
    assert.equal(malicious.body.items.length, 1);
    assert.equal(malicious.body.items[0].id, 'req-002');

    const filteredKey = await json(server, '/requests?key_id=key-alpha');
    assert.ok(filteredKey.body.items.every((record) => record.key_id === 'key-alpha'));

    const paged = await json(server, '/requests?page=1&page_size=1');
    assert.equal(paged.body.items.length, 1);
    assert.equal(paged.body.has_more, true);

    for (const query of [
      '?bogus=1',
      '?from=not-a-time',
      '?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z',
      '?page=0',
      '?page_size=101',
      '?status=unknown',
      '?page=x'
    ]) {
      const bad = await json(server, '/requests' + query);
      assert.equal(bad.res.status, 400, query);
      assert.ok(errorShape(bad.body), query);
    }

    const detailCalls = server.calls.filter(
      (call) => call.method === 'GET' && /\/requests\/[^/]+$/.test(call.path)
    );
    assert.equal(detailCalls.length, 0, '列表读取不得顺带读取详情');
  });
});

test('details: explicit only, fake redacted headers/body, XSS payload stays data', async () => {
  await withServer(async (server) => {
    const missing = await json(server, '/requests/does-not-exist');
    assert.equal(missing.res.status, 404);

    const detail = await json(server, '/requests/req-002');
    assert.equal(detail.res.status, 200);
    assert.equal(detail.body.record.id, 'req-002');
    assert.equal(detail.body.capture_status, 'redacted');
    assert.equal(detail.body.headers.Authorization[0], 'Bearer [redacted]');
    assert.equal(detail.body.headers[mock.XSS.headerName][0], mock.XSS.headerValue);
    assert.equal(detail.body.body.prompt, mock.XSS.bodyPrompt);
    assert.equal(detail.body.body.leaked, '[redacted]');
    assert.equal(server.counters.detailReads, 1);
    assert.equal(
      server.calls.filter((call) => /\/requests\/req-002$/.test(call.path)).length,
      1
    );

    const disabled = await json(server, '/requests/req-004');
    assert.equal(disabled.body.capture_status, 'disabled');
    assert.equal(disabled.body.body, null);
  });
});

test('summary: bucket granularity, totals and filters', async () => {
  await withServer(async (server) => {
    const all = await json(server, '/summary');
    assert.equal(all.res.status, 200);
    assert.ok(all.body.requests >= 1);
    assert.ok(['hour', 'day'].includes(all.body.granularity));
    assert.ok(Array.isArray(all.body.buckets) && all.body.buckets.length > 0);
    const bucketTotal = all.body.buckets.reduce((sum, bucket) => sum + bucket.requests, 0);
    assert.equal(bucketTotal, all.body.requests);

    const failed = await json(server, '/summary?status=failed');
    assert.equal(failed.body.requests, failed.body.failures);
    assert.ok(failed.body.requests > 0);

    const bad = await json(server, '/summary?unknown=1');
    assert.equal(bad.res.status, 400);
  });
});

test('keys: create/update/rotate/reset/revoke with hashed-only secrets', async () => {
  await withServer(async (server) => {
    const list = await json(server, '/keys');
    assert.equal(list.res.status, 200);
    assert.ok(Array.isArray(list.body.items));
    assert.ok(list.body.items.some((key) => key.label === mock.XSS.label));
    assert.ok(list.body.items.every((key) => !('secret' in key) && !('secret_hash' in key)));

    const created = await json(server, '/keys', {
      method: 'POST',
      body: {
        label: '合约测试 Key',
        enabled: true,
        expires_at: null,
        max_concurrent: 3,
        allowed_models: ['gpt-5-codex'],
        limits: { five_hour: 1000000, week: 2000000, month: 3000000, total: 4000000 }
      }
    });
    assert.equal(created.res.status, 201);
    assert.match(created.body.secret, /^observer_sk_[0-9a-f]{64}$/);
    assert.equal(created.body.key.label, '合约测试 Key');
    assert.equal(created.body.key.secret, undefined);
    const id = created.body.key.id;
    assert.ok(server.state.secrets.has(id), 'mock 只保存 secret 哈希');
    assert.notEqual(server.state.secrets.get(id), created.body.secret);

    const updated = await json(server, '/keys/' + id, {
      method: 'PUT',
      body: {
        label: '合约测试 Key（已编辑）',
        enabled: false,
        expires_at: null,
        max_concurrent: 1,
        allowed_models: [],
        limits: { five_hour: 0, week: 0, month: 0, total: 0 }
      }
    });
    assert.equal(updated.res.status, 200);
    assert.equal(updated.body.label, '合约测试 Key（已编辑）');
    assert.equal(updated.body.enabled, false);

    const rotated = await json(server, '/keys/' + id + '/rotate', { method: 'POST' });
    assert.equal(rotated.res.status, 200);
    assert.match(rotated.body.secret, /^observer_sk_[0-9a-f]{64}$/);
    assert.notEqual(rotated.body.secret, created.body.secret);

    const reset = await json(server, '/keys/' + id + '/reset', {
      method: 'POST',
      body: { windows: ['five_hour', 'total'] }
    });
    assert.equal(reset.res.status, 200);
    assert.equal(reset.body.used.five_hour, 0);
    assert.equal(reset.body.used.total, 0);

    const revoked = await fetch(server.origin + BASE + '/keys/' + id, {
      method: 'DELETE',
      headers: AUTH
    });
    assert.equal(revoked.status, 204);
    assert.equal(server.state.counters.keyRevokes, 1);
    assert.equal(server.state.secrets.has(id), false);

    const invalid = await json(server, '/keys', {
      method: 'POST',
      body: {
        label: 'bad',
        unknown: true,
        limits: { five_hour: 0, week: 0, month: 0, total: 0 }
      }
    });
    assert.equal(invalid.res.status, 400);
  });
});

test('settings/prices/compact: round-trip, refresh and method rejection', async () => {
  await withServer(async (server) => {
    const settings = await json(server, '/settings');
    assert.equal(settings.res.status, 200);
    assert.equal(settings.body.capture_enabled, false);
    assert.equal(settings.body.raw_retention_hours, 24);
    assert.equal(settings.body.daily_retention_days, 365);
    assert.equal(typeof settings.body.pricing.auto, 'boolean');
    assert.ok(Array.isArray(settings.body.pricing.prices));
    assert.ok(Array.isArray(settings.body.pricing.rules));
    // UI 只接受 00:00..23:59 的 HH:mm，种子必须落在合法范围内。
    assert.match(
      settings.body.pricing.rules[0].time_range,
      /^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$/
    );
    // reasoning 可选：seed 不写该键，UI 才能区分“留空”与显式 0。
    assert.equal(
      Object.prototype.hasOwnProperty.call(settings.body.pricing.prices[0].rates, 'reasoning'),
      false
    );

    settings.body.capture_enabled = true;
    settings.body.raw_retention_hours = 48;
    const saved = await json(server, '/settings', { method: 'PUT', body: settings.body });
    assert.equal(saved.res.status, 200);
    assert.equal(saved.body.capture_enabled, true);
    assert.equal(saved.body.raw_retention_hours, 48);

    const invalid = await json(server, '/settings', {
      method: 'PUT',
      body: Object.assign({}, saved.body, { raw_retention_hours: 169 })
    });
    assert.equal(invalid.res.status, 400);

    const prices = await json(server, '/prices');
    assert.equal(prices.res.status, 200);
    assert.ok(Array.isArray(prices.body.prices));
    assert.equal(typeof prices.body.last_sync, 'string');

    const refreshed = await json(server, '/prices/refresh', { method: 'POST' });
    assert.equal(refreshed.res.status, 200);
    assert.equal(server.counters.priceRefreshes, 1);

    const compact = await json(server, '/compact', { method: 'POST' });
    assert.equal(compact.res.status, 200);
    assert.equal(server.counters.compactions, 1);

    const wrongMethod = await json(server, '/health', { method: 'POST' });
    assert.equal(wrongMethod.res.status, 405);
    assert.ok(errorShape(wrongMethod.body));
  });
});

test('mock control endpoints expose calls/state only on loopback', async () => {
  await withServer(async (server) => {
    await json(server, '/health');
    const calls = await (await fetch(server.origin + '/__mock/calls')).json();
    assert.ok(Array.isArray(calls.calls));
    const healthCall = calls.calls.find((call) => call.path.endsWith('/health'));
    assert.equal(healthCall.authorized, true);
    assert.equal(healthCall.has_authorization, true);
    assert.equal('authorization' in healthCall, false);

    const state = await (await fetch(server.origin + '/__mock/state')).json();
    assert.ok(Array.isArray(state.keys));
    assert.ok(state.keys.every((key) => !('secret' in key) && !('secret_hash' in key)));

    const reset = await fetch(server.origin + '/__mock/reset-calls', { method: 'POST' });
    assert.equal(reset.status, 200);
    assert.equal(server.calls.length, 0);
  });
});
