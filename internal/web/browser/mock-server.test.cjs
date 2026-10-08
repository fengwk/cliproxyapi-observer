'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const mock = require('./mock-server.cjs');
const ui = require('../ui.js');

const AUTH_HEADER = {
  Authorization: 'Bearer ' + mock.SECRET,
  'Content-Type': 'application/json'
};

test('生产 CSP：从 internal/plugin/management.go 动态读取并注入资源响应头', async () => {
  const goPath = path.resolve(__dirname, '../../plugin/management.go');
  const goContent = fs.readFileSync(goPath, 'utf8');
  const match = goContent.match(/cspPolicy\s*=\s*"([^"]+)"/);
  assert.ok(match && match[1], 'Go 源文件必须包含 cspPolicy 常量');
  assert.equal(mock.ASSET_CSP, match[1], 'mock.ASSET_CSP 必须精确等于 Go 常量');

  const server = await mock.startServer();
  try {
    for (const file of ['ui', 'ui.html', 'ui.js', 'ui.css']) {
      const res = await fetch(server.origin + mock.RESOURCE_BASE + '/' + file);
      assert.equal(res.status, 200);
      assert.equal(res.headers.get('content-security-policy'), mock.ASSET_CSP);
    }
  } finally {
    await server.close();
  }
});

test('config 路由：仅限 v0 路径，保留反向代理前缀', async () => {
  const server = await mock.startServer();
  try {
    const defaultUrl = server.origin + mock.MANAGEMENT_BASE + '/config';
    const proxyUrl = server.origin + '/reverse-proxy/v0/management/plugins/' + mock.PLUGIN_ID + '/config';
    const v8Url = server.origin + '/v8/management/plugins/' + mock.PLUGIN_ID + '/config';
    const proxyV8Url = server.origin + '/reverse-proxy/v8/management/plugins/' + mock.PLUGIN_ID + '/config';

    // 默认 v0 访问 GET/PATCH 成功
    const resGet = await fetch(defaultUrl, { headers: AUTH_HEADER });
    assert.equal(resGet.status, 200);
    const cfg = await resGet.json();
    assert.equal(cfg.db, 'data/cliproxyapi-observer.db');

    // 反向代理 v0 访问成功
    const resProxyGet = await fetch(proxyUrl, { headers: AUTH_HEADER });
    assert.equal(resProxyGet.status, 200);

    const settingsRes = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const patch = ui.settingsToPatch(await settingsRes.json());
    const resProxyPatch = await fetch(proxyUrl, {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resProxyPatch.status, 200);
    assert.deepEqual(await resProxyPatch.json(), { status: 'ok' });

    // v8 路径必须 404（无论有无反向代理）
    const resV8Get = await fetch(v8Url, { headers: AUTH_HEADER });
    assert.equal(resV8Get.status, 404);

    const resProxyV8Get = await fetch(proxyV8Url, { headers: AUTH_HEADER });
    assert.equal(resProxyV8Get.status, 404);

    const resV8Patch = await fetch(v8Url, {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resV8Patch.status, 404);
  } finally {
    await server.close();
  }
});

test('验证失败不写：非法 patch 被拒绝且不修改配置，累加计数器', async () => {
  const server = await mock.startServer();
  try {
    const cfgBefore = JSON.parse(JSON.stringify(server.getConfig()));

    // 1. POST /validate 非法 patch
    const resVal = await fetch(server.origin + mock.MANAGEMENT_BASE + '/validate', {
      method: 'POST',
      headers: AUTH_HEADER,
      body: JSON.stringify({ 'request-retention': 'invalid-duration' })
    });
    assert.equal(resVal.status, 400);
    assert.equal(server.counters.validations, 1);
    assert.equal(server.counters.validateFailures, 1);

    // 2. PATCH /config 非法 patch
    const resPatch = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify({ 'stats-retention-days': 99999 })
    });
    assert.equal(resPatch.status, 400);
    assert.equal(server.counters.configWrites, 1);
    assert.equal(server.counters.writeFailures, 1);

    // 配置未被写入
    assert.deepEqual(server.getConfig(), cfgBefore);
  } finally {
    await server.close();
  }
});

test('shallow prices 删除与 store metadata 保留', async () => {
  const server = await mock.startServer();
  try {
    const settingsRes = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const patch = ui.settingsToPatch(await settingsRes.json());

    // 替换为全新的 prices 字典（删除原 gpt-5.1-codex，增加 new-model）
    patch.prices = {
      'new-model': {
        input: 2.0,
        output: 8.0,
        'cache-read': 0.5,
        'cache-creation': 2.0
      }
    };
    patch['stats-retention-days'] = 120;

    const resPatch = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resPatch.status, 200);

    const cfg = server.getConfig();
    // store metadata 保留
    assert.equal(cfg.db, 'data/cliproxyapi-observer.db');
    assert.equal(cfg['max-body-bytes'], 1048576);
    assert.equal(cfg['stats-retention-days'], 120);
    assert.equal(cfg.enabled, true);
    assert.deepEqual(cfg.store, { fixture: 'preserve-me' });
    assert.equal(cfg.flush, '1s');

    // prices 浅层被覆盖替换：原模型已不存在，新模型生效
    assert.equal(cfg.prices['gpt-5.1-codex'], undefined);
    assert.deepEqual(cfg.prices['new-model'], {
      input: 2.0,
      output: 8.0,
      'cache-read': 0.5,
      'cache-creation': 2.0
    });

    // settings 转换反映新 prices
    const settingsResAfter = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const settingsAfter = await settingsResAfter.json();
    assert.equal(settingsAfter.stats_retention_days, 120);
    assert.equal(settingsAfter.prices['gpt-5.1-codex'], undefined);
    assert.deepEqual(settingsAfter.prices['new-model'], {
      input: 2.0,
      output: 8.0,
      cache_read: 0.5,
      cache_creation: 2.0
    });
  } finally {
    await server.close();
  }
});

test('temporary503 GET effective：在 settings 读取时返回 503 且计数', async () => {
  const server = await mock.startServer();
  try {
    server.controls.reconfigureTemporary503Count = 1;

    // 第一次读取 settings 返回 503
    const res503 = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    assert.equal(res503.status, 503);
    assert.equal(server.counters.reconfigure503s, 1);
    assert.equal(server.counters.settingsReads, 1);

    // 计数耗尽后自动恢复正常 200
    const res200 = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    assert.equal(res200.status, 200);
    assert.equal(server.counters.reconfigure503s, 1);
    assert.equal(server.counters.settingsReads, 2);
  } finally {
    await server.close();
  }
});

test('永不生效（simulateEffectiveNeverUpdates）：PATCH 成功但 settings 不更新', async () => {
  const server = await mock.startServer();
  try {
    const settingsResBefore = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const settingsBefore = await settingsResBefore.json();
    const patch = ui.settingsToPatch(settingsBefore);
    patch['stats-retention-days'] = 60;

    server.controls.simulateEffectiveNeverUpdates = true;

    const resPatch = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resPatch.status, 200);

    // config 已更新为 60
    const cfgRes = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', { headers: AUTH_HEADER });
    const cfg = await cfgRes.json();
    assert.equal(cfg['stats-retention-days'], 60);

    // settings 依然返回更新前的旧值 365
    const settingsResAfter = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const settingsAfter = await settingsResAfter.json();
    assert.equal(settingsAfter.stats_retention_days, 365);
  } finally {
    await server.close();
  }
});

test('write / read 错误模拟与计数器', async () => {
  const server = await mock.startServer();
  try {
    // 1. read 失败
    server.controls.simulateConfigReadFail = true;
    const resReadFail = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', { headers: AUTH_HEADER });
    assert.equal(resReadFail.status, 500);
    assert.equal(server.counters.configReads, 1);
    assert.equal(server.counters.readFailures, 1);

    server.controls.simulateConfigReadFail = false;
    const resReadOk = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', { headers: AUTH_HEADER });
    assert.equal(resReadOk.status, 200);
    assert.equal(server.counters.configReads, 2);
    assert.equal(server.counters.readFailures, 1);

    // 2. write 失败
    const settingsRes = await fetch(server.origin + mock.MANAGEMENT_BASE + '/settings', { headers: AUTH_HEADER });
    const patch = ui.settingsToPatch(await settingsRes.json());

    server.controls.simulateWriteFailure = true;
    const resWriteFail = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resWriteFail.status, 500);
    assert.equal(server.counters.configWrites, 1);
    assert.equal(server.counters.writeFailures, 1);

    server.controls.simulateWriteFailure = false;
    const resWriteOk = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify(patch)
    });
    assert.equal(resWriteOk.status, 200);
    assert.equal(server.counters.configWrites, 2);
    assert.equal(server.counters.writeFailures, 1);
  } finally {
    await server.close();
  }
});
