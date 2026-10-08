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

test('有序条件定价：阈值严格大于、UTC 跨夜/24:00 边界与旧 map 回退', () => {
  const at = (h, m) => Date.UTC(2026, 0, 2, h, m);
  // UTC 时间区间：起点包含 / 终点不包含；支持跨夜与 24:00。
  assert.equal(mock.timeInRange('22:00-06:00', at(22, 0)), true);
  assert.equal(mock.timeInRange('22:00-06:00', at(23, 30)), true);
  assert.equal(mock.timeInRange('22:00-06:00', at(5, 59)), true);
  assert.equal(mock.timeInRange('22:00-06:00', at(6, 0)), false);
  assert.equal(mock.timeInRange('22:00-06:00', at(12, 0)), false);
  assert.equal(mock.timeInRange('08:00-09:00', at(8, 0)), true);
  assert.equal(mock.timeInRange('08:00-09:00', at(9, 0)), false);
  assert.equal(mock.timeInRange('00:00-24:00', at(13, 0)), true);
  assert.equal(mock.timeInRange('08:00-08:00', at(8, 0)), false);

  const pricing = {
    prices: { fallback: { input: 9, output: 9, 'cache-read': 9, 'cache-creation': 9 } },
    rules: [
      { model: 'm', 'input-tokens-gt': 1000, price: { input: 1, output: 1, 'cache-read': 1, 'cache-creation': 1 } },
      { model: 'm', price: { input: 2, output: 2, 'cache-read': 2, 'cache-creation': 2 } }
    ]
  };
  // 阈值严格大于：1001 命中第一条，恰好 1000 不命中而落到第二条。
  assert.equal(mock.resolveModelPrice('m', 1001, at(0, 0), pricing).input, 1);
  assert.equal(mock.resolveModelPrice('m', 1000, at(0, 0), pricing).input, 2);
  assert.equal(mock.resolveModelPrice('m', 500, at(0, 0), pricing).input, 2);
  // 无规则命中时回退旧的无条件 prices map，未配置模型返回 null。
  assert.equal(mock.resolveModelPrice('fallback', 1, at(0, 0), pricing).input, 9);
  assert.equal(mock.resolveModelPrice('absent', 1, at(0, 0), pricing), null);
});

test('价格规则经 config 持久化，并以 first-match 影响请求与 summary 成本', async () => {
  const server = await mock.startServer();
  try {
    const cfgUrl = server.origin + mock.MANAGEMENT_BASE + '/config';
    const settingsUrl = server.origin + mock.MANAGEMENT_BASE + '/settings';
    const reqUrl = server.origin + mock.MANAGEMENT_BASE + '/requests?offset=0&limit=52';
    const sumUrl = server.origin + mock.MANAGEMENT_BASE + '/summary';
    // 阈值 115：输入 110 不命中落到默认高价，输入 120 命中第一条零价。
    const patch = Object.assign(ui.settingsToPatch(await (await fetch(settingsUrl, { headers: AUTH_HEADER })).json()), {
      prices: {},
      'price-rules': [
        { model: 'gpt-5.1-codex', 'input-tokens-gt': 115, price: { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 } },
        { model: 'gpt-5.1-codex', price: { input: 1000000, output: 1000000, 'cache-read': 1000000, 'cache-creation': 1000000 } }
      ]
    });
    const res = await fetch(cfgUrl, { method: 'PATCH', headers: AUTH_HEADER, body: JSON.stringify(patch) });
    assert.equal(res.status, 200);
    assert.deepEqual(server.getConfig().prices, {});
    assert.equal(server.getConfig()['price-rules'].length, 2);

    const items = (await (await fetch(reqUrl, { headers: AUTH_HEADER })).json()).items;
    const gpt = items.filter((it) => it.model === 'gpt-5.1-codex' && it.accounting_quality === 'complete');
    const total = (it) => it.uncached_input_tokens + it.cache_read_tokens + it.cache_creation_tokens;
    const low = gpt.find((it) => total(it) === 110);
    const high = gpt.find((it) => total(it) === 120);
    assert.ok(low && high, 'fixture 必须提供 110/120 输入 token 的请求');
    assert.ok(low.cost_usd > 0, '低于阈值应落到默认高价');
    assert.equal(high.cost_usd, 0, '高于阈值应命中第一条零价');

    const summary = await (await fetch(sumUrl, { headers: AUTH_HEADER })).json();
    const gptGroup = summary.groups.find((g) => g.model === 'gpt-5.1-codex');
    assert.equal(gptGroup.cost_usd, 0, '聚合输入远超阈值时命中第一条');

    // 清空规则并恢复旧 map：无条件回退生效。
    const legacy = Object.assign(ui.settingsToPatch(server.getSettings()), {
      prices: { 'gpt-5.1-codex': { input: 2, output: 2, 'cache-read': 2, 'cache-creation': 2 } },
      'price-rules': []
    });
    const res2 = await fetch(cfgUrl, { method: 'PATCH', headers: AUTH_HEADER, body: JSON.stringify(legacy) });
    assert.equal(res2.status, 200);
    const gpt2 = (await (await fetch(reqUrl, { headers: AUTH_HEADER })).json()).items
      .filter((it) => it.model === 'gpt-5.1-codex' && it.accounting_quality === 'complete');
    assert.ok(gpt2.length > 0);
    assert.ok(gpt2.every((it) => it.cost_usd !== null && it.cost_usd > 0));
  } finally {
    await server.close();
  }
});

test('后端拒绝畸形价格规则、不写入配置', async () => {
  const server = await mock.startServer();
  try {
    const cfgBefore = JSON.parse(JSON.stringify(server.getConfig()));
    const malformed = [
      { model: 'm', price: { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 }, 'time-range': '9:00-10:00' },
      { model: 'm', price: { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 }, 'time-range': '08:00-08:00' },
      { model: 'm', price: { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 }, 'input-tokens-gt': Number.MAX_SAFE_INTEGER + 1 },
      { model: '', price: { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 } }
    ];
    for (const rule of malformed) {
      const base = ui.settingsToPatch(server.getSettings());
      const patch = Object.assign(base, { prices: {}, 'price-rules': [rule] });
      const validated = await fetch(server.origin + mock.MANAGEMENT_BASE + '/validate', {
        method: 'POST', headers: AUTH_HEADER, body: JSON.stringify(patch)
      });
      assert.equal(validated.status, 400, 'validate 必须拒绝 ' + JSON.stringify(rule));
      const patched = await fetch(server.origin + mock.MANAGEMENT_BASE + '/config', {
        method: 'PATCH', headers: AUTH_HEADER, body: JSON.stringify(patch)
      });
      assert.equal(patched.status, 400, 'config 必须拒绝 ' + JSON.stringify(rule));
    }
    assert.deepEqual(server.getConfig(), cfgBefore);
  } finally {
    await server.close();
  }
});

test('summary 按客户端 key / 凭据 / provider 真实过滤并暴露内嵌计数器', async () => {
  const server = await mock.startServer();
  try {
    const sum = async (query) => (await (await fetch(
      server.origin + mock.MANAGEMENT_BASE + '/summary' + (query || ''), { headers: AUTH_HEADER }
    )).json());

    const all = await sum('');
    assert.equal(all.totals.requests, 1234);
    assert.equal(all.totals.cache_hits, 812);
    assert.equal(all.groups.length, 4);
    // 未归属桶 id 为空字符串，计数器内嵌于各身份条目。
    assert.deepEqual(all.client_keys.map((g) => g.id), [mock.CLIENT_KEY_A, mock.CLIENT_KEY_B, '']);
    assert.deepEqual(all.credentials.map((g) => g.id), [mock.AUTH_INDEX_A, mock.AUTH_INDEX_B, '']);
    assert.equal(all.client_keys[0].requests, 700);
    assert.equal(all.client_keys[2].requests, 4);
    const gpt = all.groups.find((g) => g.model === 'gpt-5.1-codex');
    assert.equal(gpt.total_tokens, gpt.input_tokens + gpt.output_tokens);
    assert.ok(gpt.cost_usd !== null);

    const byKey = await sum('?client_key_id=' + mock.CLIENT_KEY_A);
    assert.equal(byKey.totals.requests, 700);
    assert.equal(byKey.totals.cache_hits, 480);
    assert.deepEqual(byKey.client_keys.map((g) => g.id), [mock.CLIENT_KEY_A]);
    assert.deepEqual(byKey.credentials.map((g) => g.id), [mock.AUTH_INDEX_A]);

    const unknown = await sum('?auth_index=unknown');
    assert.equal(unknown.totals.requests, 4);
    assert.deepEqual(unknown.credentials.map((g) => g.id), ['']);

    const missing = await sum('?client_key_id=' + 'f'.repeat(64));
    assert.equal(missing.totals.requests, 0);
    assert.deepEqual(missing.client_keys, []);
    assert.deepEqual(missing.groups, []);

    const gemini = await sum('?provider=gemini');
    assert.equal(gemini.totals.requests, 152);
    assert.deepEqual(gemini.groups.map((g) => g.model), ['gemini-3-pro']);
    assert.equal(gemini.groups[0].cost_usd, null);
    assert.equal(gemini.groups[0].unpriced_requests, 152);
  } finally {
    await server.close();
  }
});
