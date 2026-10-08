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

test('requests 接口：offset/limit 校验与分页返回，拒绝 legacy cursor', async () => {
  const server = await mock.startServer();
  try {
    const base = server.origin + mock.MANAGEMENT_BASE + '/requests';

    // 1. 默认 offset=0, limit=50
    const resPage1 = await fetch(base, { headers: AUTH_HEADER });
    assert.equal(resPage1.status, 200);
    const page1 = await resPage1.json();
    assert.equal(page1.offset, 0);
    assert.equal(page1.limit, 50);
    assert.equal(page1.has_more, true);
    assert.equal(page1.items.length, 50);
    assert.equal(page1.items[0].sequence, 1);
    assert.equal(page1.next_cursor, undefined);

    // 2. 第二页 offset=50, limit=50
    const resPage2 = await fetch(base + '?offset=50&limit=50', { headers: AUTH_HEADER });
    assert.equal(resPage2.status, 200);
    const page2 = await resPage2.json();
    assert.equal(page2.offset, 50);
    assert.equal(page2.limit, 50);
    assert.equal(page2.has_more, false);
    assert.equal(page2.items.length, 2);
    assert.equal(page2.items[0].sequence, 51);

    // 3. 超出范围页 offset=100
    const resPage3 = await fetch(base + '?offset=100&limit=50', { headers: AUTH_HEADER });
    assert.equal(resPage3.status, 200);
    const page3 = await resPage3.json();
    assert.equal(page3.offset, 100);
    assert.equal(page3.has_more, false);
    assert.equal(page3.items.length, 0);

    // 4. 拒绝非空 legacy cursor
    const resCursor = await fetch(base + '?cursor=page-2', { headers: AUTH_HEADER });
    assert.equal(resCursor.status, 400);
    const errCursor = await resCursor.json();
    assert.match(errCursor.error, /cursor is not supported/);

    // 5. 允许空 cursor（不报错）
    const resEmptyCursor = await fetch(base + '?cursor=', { headers: AUTH_HEADER });
    assert.equal(resEmptyCursor.status, 200);

    // 6. 拒绝非法 offset（负数、浮点数、非数字、越界）
    for (const badOffset of ['-1', '1.5', 'abc', '2147483648']) {
      const resBad = await fetch(base + '?offset=' + badOffset, { headers: AUTH_HEADER });
      assert.equal(resBad.status, 400, 'offset=' + badOffset + ' 必须返回 400');
    }

    // 7. 拒绝非法 limit（0、负数、超过100、非数字）
    for (const badLimit of ['0', '-10', '101', 'xyz']) {
      const resBad = await fetch(base + '?limit=' + badLimit, { headers: AUTH_HEADER });
      assert.equal(resBad.status, 400, 'limit=' + badLimit + ' 必须返回 400');
    }

    // 8. 错误模拟（simulateRequests500）
    server.controls.simulateRequests500 = true;
    const res500 = await fetch(base, { headers: AUTH_HEADER });
    assert.equal(res500.status, 500);
    server.controls.simulateRequests500 = false;
  } finally {
    await server.close();
  }
});

test('动态定价：当前有效价格映射计算历史请求与 summary 成本，配置变更立即生效而非快照不变', async () => {
  const server = await mock.startServer();
  try {
    const reqUrl = server.origin + mock.MANAGEMENT_BASE + '/requests?offset=0&limit=50';
    const sumUrl = server.origin + mock.MANAGEMENT_BASE + '/summary';
    const cfgUrl = server.origin + mock.MANAGEMENT_BASE + '/config';

    // 1. 默认仅配置 gpt-5.1-codex 价格（input: 1.25, output: 5.0, cache-read: 0.3125, cache-creation: 1.25）
    const r1 = await fetch(reqUrl, { headers: AUTH_HEADER });
    const data1 = await r1.json();
    const gptItems = data1.items.filter((it) => it.model === 'gpt-5.1-codex');
    const claudeItems = data1.items.filter((it) => it.model === 'claude-opus-4-1');

    // complete gpt-5.1-codex 请求具有有效动态成本
    const pricedGpt = gptItems.filter((it) => it.accounting_quality === 'complete');
    assert.ok(pricedGpt.length > 0);
    assert.ok(pricedGpt[0].cost_usd > 0);
    const initialGptCost = pricedGpt[0].cost_usd;
    // Complete fixtures must use the same exclusive token buckets as the API.
    for (const item of data1.items.filter((it) => it.accounting_quality === 'complete')) {
      assert.equal(item.input_tokens, item.uncached_input_tokens + item.cache_read_tokens + item.cache_creation_tokens);
      assert.equal(item.total_tokens, item.input_tokens + item.output_tokens);
    }

    // claude-opus-4-1 未定价，成本为 null
    assert.ok(claudeItems.length > 0);
    assert.equal(claudeItems[0].cost_usd, null);

    // unclassified 请求即使模型已定价也是 null
    const unclassifiedGpt = gptItems.filter((it) => it.accounting_quality === 'unclassified');
    assert.ok(unclassifiedGpt.length > 0);
    assert.equal(unclassifiedGpt[0].cost_usd, null);

    // summary 动态反映默认定价
    const s1 = await fetch(sumUrl, { headers: AUTH_HEADER });
    const summary1 = await s1.json();
    assert.ok(summary1.totals.cost_usd > 0);
    assert.ok(summary1.totals.unpriced_requests > 0);
    const gptGroup = summary1.groups.find((g) => g.model === 'gpt-5.1-codex');
    const claudeGroup = summary1.groups.find((g) => g.model === 'claude-opus-4-1');
    assert.ok(gptGroup.cost_usd > 0);
    assert.equal(gptGroup.total_tokens, gptGroup.input_tokens + gptGroup.output_tokens);
    assert.equal(claudeGroup.total_tokens, claudeGroup.input_tokens + claudeGroup.output_tokens);
    assert.equal(claudeGroup.cost_usd, null);
    assert.equal(claudeGroup.unpriced_requests, claudeGroup.requests);

    // 2. 动态修改价格：为 claude-opus-4-1 配置价格，并将 gpt-5.1-codex 价格翻倍
    const patchRes = await fetch(cfgUrl, {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify({
        prices: {
          'gpt-5.1-codex': {
            input: 2.5,
            output: 10.0,
            'cache-read': 0.625,
            'cache-creation': 2.5
          },
          'claude-opus-4-1': {
            input: 3.0,
            output: 15.0,
            'cache-read': 0.75,
            'cache-creation': 3.0
          }
        }
      })
    });
    assert.equal(patchRes.status, 200);

    // 重新拉取 requests：成本动态更新，无需快照
    const r2 = await fetch(reqUrl, { headers: AUTH_HEADER });
    const data2 = await r2.json();
    const gptItems2 = data2.items.filter((it) => it.model === 'gpt-5.1-codex' && it.accounting_quality === 'complete');
    const claudeItems2 = data2.items.filter((it) => it.model === 'claude-opus-4-1' && it.accounting_quality === 'complete');

    // gpt-5.1-codex 成本动态翻倍
    assert.ok(Math.abs(gptItems2[0].cost_usd - initialGptCost * 2) < 1e-9);
    // claude-opus-4-1 动态产生非空成本
    assert.ok(claudeItems2[0].cost_usd > 0);

    // 重新拉取 summary：totals 与 groups 动态更新
    const s2 = await fetch(sumUrl, { headers: AUTH_HEADER });
    const summary2 = await s2.json();
    const claudeGroup2 = summary2.groups.find((g) => g.model === 'claude-opus-4-1');
    assert.ok(claudeGroup2.cost_usd > 0);
    assert.ok(summary2.totals.cost_usd > summary1.totals.cost_usd);

    // 3. 价格设为 0：有效 0 成本而非 null
    await fetch(cfgUrl, {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify({
        prices: {
          'gpt-5.1-codex': { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 }
        }
      })
    });
    const rZero = await fetch(reqUrl, { headers: AUTH_HEADER });
    const dataZero = await rZero.json();
    const zeroGpt = dataZero.items.find((it) => it.model === 'gpt-5.1-codex' && it.accounting_quality === 'complete');
    assert.equal(zeroGpt.cost_usd, 0);

    // 4. 清空全部价格：全部变为 null
    await fetch(cfgUrl, {
      method: 'PATCH',
      headers: AUTH_HEADER,
      body: JSON.stringify({ prices: {} })
    });
    const rEmpty = await fetch(reqUrl, { headers: AUTH_HEADER });
    const dataEmpty = await rEmpty.json();
    assert.ok(dataEmpty.items.every((it) => it.cost_usd === null));

    const sEmpty = await fetch(sumUrl, { headers: AUTH_HEADER });
    const sumEmpty = await sEmpty.json();
    assert.equal(sumEmpty.totals.cost_usd, null);
    assert.equal(sumEmpty.totals.unpriced_requests, sumEmpty.totals.requests);
  } finally {
    await server.close();
  }
});
