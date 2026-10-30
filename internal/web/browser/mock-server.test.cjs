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
      for (const suffix of ['/ui', '/ui/', '/ui.html', '/ui.js', '/ui.css', '/theme.js']) {
        const res = await fetch(server.origin + prefix + suffix);
        assert.equal(res.status, 200, prefix + suffix);
        assert.equal(res.headers.get('content-security-policy'), mock.ASSET_CSP);
        assert.equal(res.headers.get('cache-control'), 'no-store');
        assert.equal(res.headers.get('access-control-allow-origin'), null);
      }
    }
    const html = await (await fetch(server.origin + mock.RESOURCE_BASE + '/ui')).text();
    assert.match(html, /<html/i);

    // Compare the exact compiled bytes embedded in Go, not minified internals.
    for (const name of ['ui.js', 'ui.css', 'theme.js']) {
      const body = await (await fetch(server.origin + mock.RESOURCE_BASE + '/' + name)).text();
      assert.equal(body, fs.readFileSync(path.resolve(__dirname, '../dist', name), 'utf8'));
    }
    for (const hiddenName of ['settings.js', 'models.js']) {
      const hidden = await fetch(server.origin + mock.RESOURCE_BASE + '/' + hiddenName);
      assert.equal(hidden.status, 404, hiddenName);
    }
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
    assert.equal(ok.body.schema_version, 3);
    assert.equal(typeof ok.body.version, 'string');
    for (const field of ['database_bytes', 'dropped_captures', 'active_requests']) {
      assert.equal(typeof ok.body[field], 'number', field);
    }
    assert.equal('pending_usage' in ok.body, false);
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
    // 同名账户仍由各自 index 关联，未知名称不伪造名称。
    const named = first.body.items.filter(record => record.account_name === '生产账户');
    assert.equal(new Set(named.map(record => record.auth_index)).size, 2);
    assert.equal(first.body.items.find(record => record.id === 'req-007').account_name, '');

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

    // 64 位十六进制 Key ID 与 unclassified 质量夹具只增加覆盖，不改变列表形状。
    const keyidFixture = first.body.items.find((record) => record.id === 'req-007');
    assert.ok(keyidFixture, '64-hex key_id fixture present');
    assert.match(keyidFixture.key_id, /^[0-9a-f]{64}$/);
    assert.equal(keyidFixture.accounting_quality, 'complete');
    const qualityFixture = first.body.items.find((record) => record.id === 'req-008');
    assert.ok(qualityFixture, 'unclassified accounting fixture present');
    assert.equal(qualityFixture.accounting_quality, 'unclassified');
    const exactKey = await json(server, '/requests?key_id=' + encodeURIComponent(mock.LONG_KEY_ID));
    assert.deepEqual(exactKey.body.items.map((record) => record.id), ['req-007']);
    assert.equal(exactKey.body.items[0].key_id, mock.LONG_KEY_ID);

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
      (call) => call.method === 'GET' && call.path.endsWith('/requests/detail')
    );
    assert.equal(detailCalls.length, 0, '列表读取不得顺带读取详情');
  });
});

test('request view: key_model groups by Key/provider/model with the Key label', async () => {
  await withServer(async (server) => {
    const date = new Date().toISOString().slice(0, 10);
    const view = await json(server, '/requests/view?from_date=' + date + '&to_date=' + date + '&view=key_model&page_size=100');
    assert.equal(view.res.status, 200);
    const group = view.body.groups.find((item) =>
      item.key_id === 'key-alpha' && item.provider === 'openai' && item.model === 'gpt-5-codex');
    assert.ok(group, 'composite group present');
    assert.equal(group.key, JSON.stringify(['key-alpha', 'openai', 'gpt-5-codex']));
    assert.equal(group.label, '生产 Key（自动化）');
    assert.equal(group.requests, 3);
    assert.ok(view.body.groups.every((item) => Array.isArray(JSON.parse(item.key)) && JSON.parse(item.key).length === 3));
  });
});

test('request view: IANA timezone interprets inclusive local calendar dates', async () => {
  await withServer(async (server) => {
    const base = server.state.records[0];
    const recordAt = (id, time) => ({ ...base, id, time });

    server.state.records = [
      recordAt('sh-before', '2026-10-19T15:30:00.000Z'), // Oct 19 23:30 in Shanghai
      recordAt('sh-first', '2026-10-19T16:30:00.000Z'),  // Oct 20 00:30
      recordAt('sh-last', '2026-10-20T15:30:00.000Z'),   // Oct 20 23:30
      recordAt('sh-after', '2026-10-20T16:30:00.000Z'),  // Oct 21 00:30
    ];
    const shanghai = await json(server,
      '/requests/view?from_date=2026-10-20&to_date=2026-10-20&timezone=Asia%2FShanghai&page_size=100');
    assert.equal(shanghai.res.status, 200);
    assert.equal(shanghai.body.range.timezone, 'Asia/Shanghai');
    assert.equal(shanghai.body.range.from, '2026-10-19T16:00:00.000Z');
    assert.equal(shanghai.body.range.to, '2026-10-20T16:00:00.000Z');
    assert.deepEqual(shanghai.body.records.map((item) => item.id).sort(), ['sh-first', 'sh-last']);

    server.state.records = [
      recordAt('ny-before', '2026-10-20T03:30:00.000Z'), // Oct 19 23:30 in New York
      recordAt('ny-first', '2026-10-20T04:30:00.000Z'),  // Oct 20 00:30 EDT
      recordAt('ny-last', '2026-10-21T03:30:00.000Z'),   // Oct 20 23:30
      recordAt('ny-after', '2026-10-21T04:30:00.000Z'),  // Oct 21 00:30
    ];
    const newYork = await json(server,
      '/requests/view?from_date=2026-10-20&to_date=2026-10-20&timezone=America%2FNew_York&page_size=100');
    assert.equal(newYork.body.range.timezone, 'America/New_York');
    assert.equal(newYork.body.range.from, '2026-10-20T04:00:00.000Z');
    assert.equal(newYork.body.range.to, '2026-10-21T04:00:00.000Z');
    assert.deepEqual(newYork.body.records.map((item) => item.id).sort(), ['ny-first', 'ny-last']);

    for (const zone of ['Local', 'Not/A_Zone']) {
      const bad = await json(server,
        '/requests/view?from_date=2026-10-20&to_date=2026-10-20&timezone=' + encodeURIComponent(zone));
      assert.equal(bad.res.status, 400, zone);
      assert.ok(errorShape(bad.body), zone);
    }
  });
});

test('request view: DST local days and elapsed hours remain exact', async () => {
  await withServer(async (server) => {
    const base = server.state.records[0];
    const recordAt = (id, time) => ({ ...base, id, time });
    server.state.records = [
      recordAt('fall-early', '2025-11-02T05:30:00.000Z'), // 01:30 EDT
      recordAt('fall-late', '2025-11-02T06:30:00.000Z'),  // 01:30 EST
      recordAt('spring', '2026-03-08T07:30:00.000Z'),     // 03:30 EDT
    ];

    const fall = await json(server,
      '/requests/view?from_date=2025-11-02&to_date=2025-11-02&timezone=America%2FNew_York&page_size=100');
    assert.equal(fall.body.range.from, '2025-11-02T04:00:00.000Z');
    assert.equal(fall.body.range.to, '2025-11-03T05:00:00.000Z');
    assert.equal(fall.body.series.granularity, 'hour');
    assert.equal(fall.body.series.buckets.length, 25);
    assert.equal(fall.body.series.buckets[0].time, '2025-11-02T04:00:00.000Z');
    assert.equal(fall.body.series.buckets[24].time, '2025-11-03T04:00:00.000Z');
    assert.deepEqual(fall.body.records.map((item) => item.id).sort(), ['fall-early', 'fall-late']);

    const spring = await json(server,
      '/requests/view?from_date=2026-03-08&to_date=2026-03-08&timezone=America%2FNew_York&page_size=100');
    assert.equal(spring.body.range.from, '2026-03-08T05:00:00.000Z');
    assert.equal(spring.body.range.to, '2026-03-09T04:00:00.000Z');
    assert.equal(spring.body.series.buckets.length, 23);
    assert.deepEqual(spring.body.records.map((item) => item.id), ['spring']);

    const days = await json(server,
      '/requests/view?from_date=2025-11-01&to_date=2025-11-03&timezone=America%2FNew_York&page_size=100');
    assert.equal(days.body.series.granularity, 'day');
    assert.deepEqual(days.body.series.buckets.map((bucket) => bucket.time), [
      '2025-11-01T04:00:00.000Z',
      '2025-11-02T04:00:00.000Z',
      '2025-11-03T05:00:00.000Z',
    ]);

    // Empty local days must keep distinct keys while the timezone offset changes.
    server.state.records = [];
    const empty = await json(server,
      '/requests/view?from_date=2025-10-30&to_date=2025-11-03&timezone=America%2FNew_York');
    assert.deepEqual(empty.body.series.buckets.map((bucket) => bucket.time), [
      '2025-10-30T04:00:00.000Z',
      '2025-10-31T04:00:00.000Z',
      '2025-11-01T04:00:00.000Z',
      '2025-11-02T04:00:00.000Z',
      '2025-11-03T05:00:00.000Z',
    ]);
    const skipped = await json(server,
      '/requests/view?from_date=2011-12-30&to_date=2011-12-30&timezone=Pacific%2FApia');
    assert.equal(skipped.res.status, 400);
  });
});

test('request view: midnight DST gap starts at the first representable instant', async () => {
  await withServer(async (server) => {
    // America/Santiago 2025-09-07: local 00:00 jumps to 01:00, so the local day
    // is 23 elapsed hours and must start at 04:00Z rather than drift to the
    // previous day or carry the next day's boundary to 01:00.
    const day = await json(server,
      '/requests/view?from_date=2025-09-07&to_date=2025-09-07&timezone=America%2FSantiago&page_size=100');
    assert.equal(day.res.status, 200);
    assert.equal(day.body.range.from, '2025-09-07T04:00:00.000Z');
    assert.equal(day.body.range.to, '2025-09-08T03:00:00.000Z');
    assert.equal(day.body.series.granularity, 'hour');
    assert.equal(day.body.series.buckets.length, 23);
    assert.equal(day.body.series.buckets[0].time, '2025-09-07T04:00:00.000Z');
    assert.equal(day.body.series.buckets[22].time, '2025-09-08T02:00:00.000Z');
  });
});

test('details: query-ID route only, fake redacted headers/body, XSS payload stays data', async () => {
  await withServer(async (server) => {
    const missing = await json(server, '/requests/detail?id=does-not-exist');
    assert.equal(missing.res.status, 404);

    const detail = await json(server, '/requests/detail?id=req-002');
    assert.equal(detail.res.status, 200);
    assert.equal(detail.body.record.id, 'req-002');
    assert.equal(detail.body.capture_status, 'redacted');
    assert.equal(detail.body.headers.Authorization[0], 'Bearer [redacted]');
    assert.equal(detail.body.headers[mock.XSS.headerName][0], mock.XSS.headerValue);
    assert.equal(detail.body.body.prompt, mock.XSS.bodyPrompt);
    assert.equal(detail.body.body.leaked, '[redacted]');
    assert.equal(server.counters.detailReads, 1);
    assert.equal(
      server.calls.filter((call) => call.path.endsWith('/requests/detail') && call.query.id === 'req-002').length,
      1
    );

    const disabled = await json(server, '/requests/detail?id=req-004');
    assert.equal(disabled.body.capture_status, 'disabled');
    assert.equal(disabled.body.body, null);

    // 旧动态路由不再注册；负测试同时确认它没有产生副作用。
    const before = server.counters.detailReads;
    for (const legacy of ['/requests/req-001', '/requests/req-002']) {
      const old = await json(server, legacy);
      assert.equal(old.res.status, 404, legacy);
      assert.ok(errorShape(old.body), legacy);
    }
    assert.equal(server.counters.detailReads, before);
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

test('keys: fixed query-ID create/update/rotate/reset/revoke with persistent plaintext secrets', async () => {
  await withServer(async (server) => {
    const list = await json(server, '/keys');
    assert.equal(list.res.status, 200);
    assert.ok(Array.isArray(list.body.items));
    assert.ok(list.body.items.some((key) => key.label === mock.XSS.label));
    assert.ok(list.body.items.every((key) => !('secret_hash' in key)));
    assert.equal(list.body.items.find((key) => key.id === 'key-alpha').secret, 'sk-key-alpha');
    assert.equal(list.body.items.find((key) => key.id === 'key-legacy').secret, undefined);
    assert.ok(list.body.items.some((key) => key.id === 'key-legacy' && key.allowed_models.includes('retired-model-v0')));
    assert.ok(list.body.items.every((key) => !('blocked_reason' in key)));

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
    assert.match(created.body.secret, /^sk-[A-Za-z0-9]{48}$/);
    assert.equal(created.body.key.label, '合约测试 Key');
    assert.equal(created.body.key.secret, created.body.secret);
    const id = created.body.key.id;
    assert.ok(server.state.secrets.has(id), 'mock 保存用于认证的 secret 哈希');
    assert.notEqual(server.state.secrets.get(id), created.body.secret);
    assert.equal(server.state.secretValues.get(id), created.body.secret, 'mock 保存可再次读取的当前明文');

    const updated = await json(server, '/keys?id=' + encodeURIComponent(id), {
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
    assert.equal(updated.body.secret, created.body.secret, '禁用/编辑保留当前明文');
    assert.deepEqual(updated.body.allowed_models, []);

    const rotated = await json(server, '/keys/rotate?id=' + encodeURIComponent(id), { method: 'POST' });
    assert.equal(rotated.res.status, 200);
    assert.match(rotated.body.secret, /^sk-[A-Za-z0-9]{48}$/);
    assert.notEqual(rotated.body.secret, created.body.secret);
    assert.equal(rotated.body.key.secret, rotated.body.secret, '轮转响应中的 key 也返回最新明文');
    assert.equal(server.state.secretValues.get(id), rotated.body.secret);

    const reset = await json(server, '/keys/reset?id=' + encodeURIComponent(id), {
      method: 'POST',
      body: { windows: ['five_hour', 'total'] }
    });
    assert.equal(reset.res.status, 200);
    assert.equal(reset.body.used.five_hour, 0);
    assert.equal(reset.body.used.total, 0);
    assert.equal(reset.body.secret, rotated.body.secret);

    const revoked = await fetch(server.origin + BASE + '/keys?id=' + encodeURIComponent(id), {
      method: 'DELETE',
      headers: AUTH
    });
    assert.equal(revoked.status, 204);
    assert.equal(server.state.counters.keyRevokes, 1);
    assert.equal(server.state.secrets.has(id), false);
    assert.equal(server.state.secretValues.has(id), false);
    const afterRevoke = await json(server, '/keys');
    const revokedKey = afterRevoke.body.items.find((key) => key.id === id);
    assert.equal(revokedKey.secret, undefined, '撤销后不得返回旧明文');
    assert.equal(revokedKey.revoked, true);
    assert.equal('blocked_reason' in revokedKey, false);

    // Revocation is determined by the missing credential: rotation restores it,
    // but does not enable the Key until an explicit update.
    const enableSpec = {
      label: revokedKey.label, enabled: true, expires_at: revokedKey.expires_at,
      max_concurrent: revokedKey.max_concurrent, allowed_models: revokedKey.allowed_models,
      limits: revokedKey.limits
    };
    const prematureEnable = await json(server, '/keys?id=' + encodeURIComponent(id), {
      method: 'PUT', body: enableSpec
    });
    assert.equal(prematureEnable.res.status, 409);
    const rotateRevoked = await json(server, '/keys/rotate?id=' + encodeURIComponent(id), { method: 'POST' });
    assert.equal(rotateRevoked.res.status, 200);
    assert.equal(rotateRevoked.body.key.revoked, false);
    assert.equal(rotateRevoked.body.key.enabled, false);
    const enabled = await json(server, '/keys?id=' + encodeURIComponent(id), {
      method: 'PUT', body: enableSpec
    });
    assert.equal(enabled.res.status, 200);
    assert.equal(enabled.body.enabled, true);
    assert.equal(enabled.body.secret, rotateRevoked.body.secret);

    const deleted = await json(server, '/keys/delete?id=' + encodeURIComponent(id), { method: 'POST' });
    assert.equal(deleted.res.status, 204);
    const afterDelete = await json(server, '/keys?include_deleted=true');
    assert.equal(afterDelete.body.items.find((key) => key.id === id).secret, undefined, '删除后不得返回旧明文');

    const invalid = await json(server, '/keys', {
      method: 'POST',
      body: {
        label: 'bad',
        unknown: true,
        limits: { five_hour: 0, week: 0, month: 0, total: 0 }
      }
    });
    assert.equal(invalid.res.status, 400);

    // 已移除的路由必须是 404，且不能改变状态；只有负测试可以引用它们。
    const snapshot = JSON.stringify(server.state.keys);
    const revokes = server.counters.keyRevokes;
    for (const legacy of [
      { method: 'PUT', path: '/keys/key-alpha' },
      { method: 'DELETE', path: '/keys/key-alpha' },
      { method: 'POST', path: '/keys/key-alpha/rotate' },
      { method: 'POST', path: '/keys/key-alpha/reset' },
      { method: 'POST', path: '/keys/key-alpha/recover' },
      { method: 'POST', path: '/keys/recover?id=key-alpha', body: { acknowledge: true } }
    ]) {
      const old = await json(server, legacy.path, { method: legacy.method, body: legacy.body });
      assert.equal(old.res.status, 404, legacy.method + ' ' + legacy.path);
      assert.ok(errorShape(old.body), legacy.method + ' ' + legacy.path);
    }
    assert.equal(JSON.stringify(server.state.keys), snapshot);
    assert.equal(server.counters.keyRevokes, revokes);

    const missingId = await json(server, '/keys', { method: 'PUT', body: {} });
    assert.equal(missingId.res.status, 400);
    const duplicateId = await json(server, '/keys?id=key-alpha&id=key-beta', { method: 'DELETE' });
    assert.equal(duplicateId.res.status, 400);
  });
});

async function issueToken(server, options) {
  const opts = Object.assign({ method: 'POST', headers: Object.assign({}, AUTH) }, options || {});
  const response = await fetch(server.origin + BASE + '/models/token', opts);
  const text = await response.text();
  return { response, body: text ? JSON.parse(text) : null };
}

async function getCatalog(server, token, options) {
  const opts = Object.assign({}, options || {});
  opts.headers = Object.assign({}, opts.headers || {});
  if (token) opts.headers.Authorization = 'Bearer ' + token;
  const response = await fetch(server.origin + mock.CATALOG_PATH, opts);
  const text = await response.text();
  return { response, body: text ? JSON.parse(text) : null };
}

test('catalog capability: management bearer, empty request, 30s single-use and bounded IDs', async () => {
  await withServer(async (server) => {
    const unauthorized = await fetch(server.origin + BASE + '/models/token', { method: 'POST' });
    assert.equal(unauthorized.status, 401);
    assert.ok(errorShape(await unauthorized.json()));

    const wrongMethod = await json(server, '/models/token');
    assert.equal(wrongMethod.res.status, 405);

    const queryRejected = await fetch(server.origin + BASE + '/models/token?x=1', {
      method: 'POST',
      headers: AUTH
    });
    assert.equal(queryRejected.status, 400);
    const bodyRejected = await fetch(server.origin + BASE + '/models/token', {
      method: 'POST',
      headers: Object.assign({ 'Content-Type': 'application/json' }, AUTH),
      body: '{}'
    });
    assert.equal(bodyRejected.status, 400);

    const query = await issueToken(server, {});
    assert.equal(query.response.status, 200);
    const token = query.body.token;
    assert.match(token, /^[A-Za-z0-9_-]{43}$/, '>=256-bit random capability');
    const ttl = Date.parse(query.body.expires_at) - Date.now();
    assert.ok(ttl > 28_000 && ttl <= 30_500, 'expires_at is 30s: ' + ttl);

    const tokenCall = server.calls
      .filter((call) => call.method === 'POST' && call.path.endsWith('/models/token') && call.status === 200)
      .pop();
    assert.equal(tokenCall.authorized, true);
    assert.deepEqual(tokenCall.query, {});
    assert.equal(tokenCall.request_body, undefined);
    assert.equal(tokenCall.body_bytes, 0);
    assert.equal(tokenCall.capability_stage, 'minted');
    assert.ok(!JSON.stringify(server.calls).includes(token), 'call log must not contain capability token');

    // capability 只认证精确的 /v1/models；管理密钥本身不能读取目录。
    const withManagement = await getCatalog(server, KEY);
    assert.equal(withManagement.response.status, 401);
    const withoutAuth = await getCatalog(server, '');
    assert.equal(withoutAuth.response.status, 401);

    const catalog = await getCatalog(server, token);
    assert.equal(catalog.response.status, 200);
    assert.equal(catalog.response.headers.get('cache-control'), 'no-store');
    assert.ok(Array.isArray(catalog.body.data));
    assert.equal(catalog.body.data.length, mock.MAX_CATALOG_MODELS, 'response is bounded');
    assert.deepEqual(Object.keys(catalog.body.data[0]), ['id']);
    const ids = catalog.body.data.map((item) => item.id);
    assert.ok(ids.includes('gpt-5-codex'));
    assert.ok(ids.includes('claude-opus-4-1'));
    assert.ok(ids.includes('gemini-2.5-pro'));
    assert.ok(!JSON.stringify(catalog.body).match(/secret|DO-NOT-LEAK|api_key/i));

    const catalogCall = server.calls.filter((call) => call.path === mock.CATALOG_PATH).pop();
    assert.equal(catalogCall.kind, 'catalog');
    assert.equal(catalogCall.authorized, true);
    assert.equal(catalogCall.capability_stage, 'used');
    assert.deepEqual(catalogCall.query, {});
    assert.equal(catalogCall.has_extra_auth_header, false);
    assert.ok(!JSON.stringify(catalogCall).includes(token));

    // 一次性：同一 capability 不能再次读取目录。
    const replay = await getCatalog(server, token);
    assert.equal(replay.response.status, 401);
    const replayCall = server.calls.filter((call) => call.path === mock.CATALOG_PATH).pop();
    assert.equal(replayCall.authorized, false);
    assert.equal(replayCall.capability_stage, 'rejected');

    // 精确 namespace：不得出现 auth-files 回退或插件前缀下的 /v1/models。
    const authFiles = await fetch(server.origin + '/v0/management/auth-files', { headers: AUTH });
    assert.equal(authFiles.status, 404);
    const prefixed = await fetch(server.origin + BASE + '/v1/models', { headers: AUTH });
    assert.equal(prefixed.status, 404);
    const queryCatalog = await getCatalog(server, await issueToken(server).then((issued) => issued.body.token), {
      headers: {}
    });
    assert.equal(queryCatalog.response.status, 200);
    const withQuery = await fetch(server.origin + mock.CATALOG_PATH + '?x=1');
    assert.equal(withQuery.status, 400);
    const postCatalog = await fetch(server.origin + mock.CATALOG_PATH, { method: 'POST' });
    assert.equal(postCatalog.status, 405);

    const state = await (await fetch(server.origin + '/__mock/state')).json();
    assert.equal(state.outstanding_capabilities, 0, 'all issued capabilities were consumed');
    assert.ok(!JSON.stringify(state).includes(token));
  });
});

test('catalog capability: bounded outstanding allocation and failure/delay controls', async () => {
  await withServer(async (server) => {
    const issued = [];
    for (let i = 0; i < mock.MAX_OUTSTANDING_CAPABILITIES; i += 1) {
      const token = await issueToken(server);
      assert.equal(token.response.status, 200, 'allocation ' + i);
      issued.push(token.body.token);
    }
    const overflow = await issueToken(server);
    assert.equal(overflow.response.status, 429);
    assert.ok(errorShape(overflow.body));

    // 使用一个后即可再签发一个，保持有界且不误伤已消费 capability。
    const first = await getCatalog(server, issued[0]);
    assert.equal(first.response.status, 200);
    const replacement = await issueToken(server);
    assert.equal(replacement.response.status, 200);

    // 后续失败/延迟用例不继承 32 个未消费 capability。
    await fetch(server.origin + '/__mock/reset', { method: 'POST' });
    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_token: true } })
    });
    const tokenFailed = await issueToken(server);
    assert.equal(tokenFailed.response.status, 502);

    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_token: false, fail_models: true } })
    });
    const token = await issueToken(server);
    const modelsFailed = await getCatalog(server, token.body.token);
    assert.equal(modelsFailed.response.status, 502);
    const consumed = await getCatalog(server, token.body.token);
    assert.equal(consumed.response.status, 401, 'failed catalog use still consumes the capability');

    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_models: false, token_delay_ms: 120, models_delay_ms: 120 } })
    });
    const started = Date.now();
    const delayedToken = await issueToken(server);
    assert.equal(delayedToken.response.status, 200);
    assert.ok(Date.now() - started >= 100, 'token delay is measurable');
    const delayedStart = Date.now();
    const delayedCatalog = await getCatalog(server, delayedToken.body.token);
    assert.equal(delayedCatalog.response.status, 200);
    assert.ok(Date.now() - delayedStart >= 100, 'catalog delay is measurable');

    const state = await (await fetch(server.origin + '/__mock/state')).json();
    assert.deepEqual(state.catalog_control, {
      token_delay_ms: 120,
      models_delay_ms: 120,
      fail_token: false,
      fail_models: false
    });
    assert.ok(server.calls.every((call) => !('authorization' in call)));
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
    assert.equal(settings.body.pricing.source, 'models_dev_v2');
    assert.ok(Array.isArray(settings.body.pricing.prices));
    assert.ok(Array.isArray(settings.body.pricing.mappings));
    const seed = settings.body.pricing.prices[0];
    assert.ok(seed.definition);
    assert.equal(seed.definition.base.reasoning, null);
    assert.equal(seed.definition.overrides[0].when.context.gt, 200000);
    assert.equal(seed.definition.multipliers[0].factors.input, '0.5');

    settings.body.capture_enabled = true;
    settings.body.capture_mode = 'headers_and_body';
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

    const invalidSource = await json(server, '/settings', {
      method: 'PUT',
      body: { ...saved.body, pricing: { ...saved.body.pricing, source: 'models_dev_v0' } }
    });
    assert.equal(invalidSource.res.status, 400);

    const invalidDefinition = await json(server, '/settings', {
      method: 'PUT',
      body: {
        ...saved.body,
        pricing: {
          ...saved.body.pricing,
          prices: [{
            model: 'broken',
            definition: {
              base: { input: 1, output: 1, cache_read: 0, cache_write: 0, reasoning: null },
              overrides: [{ id: 'bad', when: { time: { start: '08:00', end: '08:00', timezone: 'UTC' } }, rates: {} }],
              multipliers: []
            }
          }]
        }
      }
    });
    assert.equal(invalidDefinition.res.status, 400);

    const preview = await json(server, '/prices/preview', {
      method: 'POST',
      body: { models: [{ model: 'gpt-5-codex', provider: '' }, { model: 'gpt-5-mini', provider: '' }] }
    });
    assert.equal(preview.res.status, 200);
    assert.equal(preview.body.items[0].source, 'manual');
    assert.equal(preview.body.items[0].definition.base.input, 1_250_000);
    assert.equal(preview.body.items[0].pricing_supported, true);
    assert.equal(preview.body.items[1].source, 'auto');
    assert.ok(!('has_rules' in preview.body.items[0]));
    assert.ok(!('missing_rates' in preview.body.items[0]));

    // mock 与引擎同步 1 GiB 总存储上限：exact 1 GiB 可保存，+1 字节拒绝。
    const exactStorage = await json(server, '/settings', {
      method: 'PUT',
      body: Object.assign({}, saved.body, { max_body_storage_bytes: mock.MAX_BODY_STORAGE_BYTES })
    });
    assert.equal(exactStorage.res.status, 200);
    assert.equal(exactStorage.body.max_body_storage_bytes, mock.MAX_BODY_STORAGE_BYTES);
    const overflowStorage = await json(server, '/settings', {
      method: 'PUT',
      body: Object.assign({}, saved.body, { max_body_storage_bytes: mock.MAX_BODY_STORAGE_BYTES + 1 })
    });
    assert.equal(overflowStorage.res.status, 400);
    assert.ok(errorShape(overflowStorage.body));
    assert.equal(server.state.settings.max_body_storage_bytes, mock.MAX_BODY_STORAGE_BYTES);

    const prices = await json(server, '/prices');
    assert.equal(prices.res.status, 200);
    assert.ok(Array.isArray(prices.body.prices));
    assert.equal(typeof prices.body.last_sync, 'string');
    const dualProvider = prices.body.prices.filter(
      (price) => price.model === 'gpt-5-codex' && price.source === 'auto'
    );
    assert.deepEqual(
      dualProvider.map((price) => price.provider).sort(),
      ['azure', 'openai']
    );
    assert.notDeepEqual(dualProvider[0].rates, dualProvider[1].rates);
    assert.ok(
      prices.body.prices.some(
        (price) =>
          price.model === 'gpt-5-codex' &&
          price.source === 'manual' &&
          !('provider' in price) &&
          price.definition.base.input === 1_250_000
      ),
      'manual price must use the complete empty-provider identity'
    );
    assert.equal(prices.body.source, 'models_dev_v2');
    assert.ok(prices.body.prices.every((price) => price.definition && price.rates && price.pricing_supported === true));

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

test('settings: exact requestBody number survives PUT and GET without rounding', async () => {
  await withServer(async (server) => {
    const exact = '9007199254740993';
    // Send raw text: parsing this request in the test first would round the
    // number before it ever reaches the mock.
    const payload = `{
      "capture_enabled": false,
      "capture_mode": "off",
      "capture_retention_seconds": 86400,
      "max_body_bytes": 1048576,
      "max_body_storage_bytes": 268435456,
      "raw_retention_hours": 24,
      "daily_retention_days": 365,
      "pricing": {
        "auto": false,
        "source": "models_dev_v2",
        "prices": [{
          "model": "exact-number",
          "provider": "fixture",
          "definition": {
            "base": { "input": 1, "output": 1, "cache_read": 0, "cache_write": 0, "reasoning": null },
            "overrides": [{
              "id": "exact",
              "when": { "requestBody": { "path": "/n", "equals": ${exact} } },
              "rates": { "input": 2 }
            }],
            "multipliers": []
          }
        }],
        "mappings": []
      }
    }`;
    const headers = Object.assign({ 'Content-Type': 'application/json' }, AUTH);
    const put = await fetch(server.origin + BASE + '/settings', { method: 'PUT', headers, body: payload });
    assert.equal(put.status, 200);
    const putText = await put.text();
    assert.match(putText, new RegExp('"equals":' + exact + '(?=[,}])'));
    assert.doesNotMatch(putText, /9007199254740992/);

    const get = await fetch(server.origin + BASE + '/settings', { headers: AUTH });
    assert.equal(get.status, 200);
    const getText = await get.text();
    assert.match(getText, new RegExp('"equals":' + exact + '(?=[,}])'));
    assert.doesNotMatch(getText, /9007199254740992/);

    // A native raw JSON object is exact; an arbitrary lookalike object is not.
    const lookalike = payload.replace(
      `"equals": ${exact}`,
      '"equals": { "rawJSON": "9007199254740993" }'
    );
    const rejected = await fetch(server.origin + BASE + '/settings', { method: 'PUT', headers, body: lookalike });
    assert.equal(rejected.status, 400);
    const rejectedBody = await rejected.json();
    assert.ok(errorShape(rejectedBody));
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
