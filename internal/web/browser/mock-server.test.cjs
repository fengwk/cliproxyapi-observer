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

test('keys: fixed query-ID create/update/rotate/reset/recover/revoke with hashed-only secrets', async () => {
  await withServer(async (server) => {
    const list = await json(server, '/keys');
    assert.equal(list.res.status, 200);
    assert.ok(Array.isArray(list.body.items));
    assert.ok(list.body.items.some((key) => key.label === mock.XSS.label));
    assert.ok(list.body.items.every((key) => !('secret' in key) && !('secret_hash' in key)));
    assert.ok(list.body.items.some((key) => key.id === 'key-legacy' && key.allowed_models.includes('retired-model-v0')));
    assert.ok(list.body.items.some((key) => key.id === 'key-blocked' && key.blocked_reason));

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
    assert.equal(created.body.key.secret, undefined);
    const id = created.body.key.id;
    assert.ok(server.state.secrets.has(id), 'mock 只保存 secret 哈希');
    assert.notEqual(server.state.secrets.get(id), created.body.secret);

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
    assert.deepEqual(updated.body.allowed_models, []);

    const rotated = await json(server, '/keys/rotate?id=' + encodeURIComponent(id), { method: 'POST' });
    assert.equal(rotated.res.status, 200);
    assert.match(rotated.body.secret, /^sk-[A-Za-z0-9]{48}$/);
    assert.notEqual(rotated.body.secret, created.body.secret);

    const reset = await json(server, '/keys/reset?id=' + encodeURIComponent(id), {
      method: 'POST',
      body: { windows: ['five_hour', 'total'] }
    });
    assert.equal(reset.res.status, 200);
    assert.equal(reset.body.used.five_hour, 0);
    assert.equal(reset.body.used.total, 0);

    const blocked = server.state.keys.find((key) => key.id === 'key-blocked');
    const beforeRecover = JSON.parse(JSON.stringify(blocked));
    const missingAck = await json(server, '/keys/recover?id=key-blocked', {
      method: 'POST',
      body: { acknowledge: false }
    });
    assert.equal(missingAck.res.status, 400);
    assert.ok(blocked.blocked_reason, '失败的 acknowledge 不得清除阻塞');
    assert.equal(server.counters.keyRecovers, 0);

    const recovered = await json(server, '/keys/recover?id=key-blocked', {
      method: 'POST',
      body: { acknowledge: true }
    });
    assert.equal(recovered.res.status, 200);
    assert.equal(recovered.body.blocked_reason, '');
    assert.equal(server.counters.keyRecovers, 1);
    assert.deepEqual(recovered.body.used, beforeRecover.used, '恢复不得重置 used');
    assert.deepEqual(recovered.body.starts, beforeRecover.starts, '恢复不得重置 starts');
    assert.deepEqual(recovered.body.resets, beforeRecover.resets, '恢复不得重置 resets');
    assert.deepEqual(recovered.body.limits, beforeRecover.limits, '恢复不得改动 limits');

    const revoked = await fetch(server.origin + BASE + '/keys?id=' + encodeURIComponent(id), {
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

    // 旧动态路由必须是 404，且不能改变状态；只有负测试可以引用它们。
    const snapshot = JSON.stringify(server.state.keys);
    const revokes = server.counters.keyRevokes;
    for (const legacy of [
      { method: 'PUT', path: '/keys/key-alpha' },
      { method: 'DELETE', path: '/keys/key-alpha' },
      { method: 'POST', path: '/keys/key-alpha/rotate' },
      { method: 'POST', path: '/keys/key-alpha/reset' },
      { method: 'POST', path: '/keys/key-alpha/recover' }
    ]) {
      const old = await json(server, legacy.path, { method: legacy.method });
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
        (price) => price.model === 'gpt-5-codex' && price.source === 'manual' && !('provider' in price)
      ),
      'manual price must stay model-wide without provider'
    );

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
