'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');

// Local, fake identities only. Exercise key filtering, dimension switching and
// filter-aware summary through authenticated mocks.
async function main() {
  const out = process.argv[2];
  if (!out) throw new Error('output directory required');
  fs.mkdirSync(out, { recursive: true });
  const server = await mock.startServer();
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true });
  const errors = [];
  const calls = [];
  const results = [];
  const metric = (page, key) => page.locator(`[data-metric="${key}"] .metric-value`).textContent();
  const waitMetric = (page, key, value) => page.waitForFunction(
    (args) => {
      const el = document.querySelector(`[data-metric="${args.key}"] .metric-value`);
      return el && el.textContent.trim() === args.value;
    },
    { key, value }
  );
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    page.on('pageerror', (e) => errors.push(e.message));
    page.on('request', (r) => {
      if (!r.url().includes('/management/')) return;
      const url = new URL(r.url());
      assert.equal(url.origin, server.origin);
      assert.equal(r.headers().authorization, 'Bearer ' + mock.SECRET);
      assert.equal(url.href.includes(mock.SECRET), false);
      calls.push(url);
    });
    await page.goto(server.origin + '/proxy/v8/resource/plugins/' + mock.PLUGIN_ID + '/ui');
    await page.locator('#mgmt-key').fill(mock.SECRET);
    await page.locator('#connect').click();
    await page.locator('#identity-apply').waitFor();
    await page.waitForFunction(() => !document.getElementById('identity-apply').disabled);

    // 1. 上游凭据展示 CPA 标签与认证文件名，缓存命中率带分子分母并显式标注。
    assert.match(await page.locator('#requests-body').textContent(), /工作账户（fake-file-a\.json）/);
    assert.match(await page.locator('#requests-body').textContent(), /备用账户（fake-file-b\.json）/);
    assert.match(await page.locator('[data-metric="cache"]').textContent(), /请求命中率.*812\/1,234/);
    results.push('current CPA labels with auth file names and explicit request-level cache ratio');

    // 2. 请求表点击完整指纹：只显示 12 位前缀，筛选使用完整 64 位。
    const button = page.locator('#requests-body button[title]').first();
    const fingerprint = await button.getAttribute('title');
    assert.equal(fingerprint.length, 64);
    assert.equal((await button.textContent()).length, 13);
    await button.click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 26);
    assert.equal(await page.locator('#filter-client-key').inputValue(), fingerprint);
    assert.ok(calls.some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === fingerprint && url.searchParams.get('offset') === '0'));
    assert.equal(await page.locator('#requests-next').isDisabled(), true);
    // summary 必须带上 key 筛选并被概览采纳（而非只影响请求列表）。
    assert.ok(calls.some((url) => url.pathname.endsWith('/summary') &&
      url.searchParams.get('client_key_id') === fingerprint));
    await waitMetric(page, 'requests', '700');
    assert.equal(await metric(page, 'requests'), '700');
    results.push('full fingerprint filtering, not a truncated prefix');

    // 3. 上游凭据按「标签（文件名）」选择，使用非机密 auth_index 过滤，概览同步变化。
    assert.match(await page.locator('#filter-auth').textContent(), /备用账户（fake-file-b\.json） · 2222222222222222/);
    await page.locator('#filter-client-key').fill('');
    await page.locator('#filter-auth').selectOption('2'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => {
      const rows = Array.from(document.querySelectorAll('#requests-body tr'));
      return rows.length === 25 && rows.every((row) => row.textContent.includes('备用账户（fake-file-b.json）'));
    });
    await waitMetric(page, 'requests', '530');
    results.push('auth file selection filters by nonsecret auth_index');

    // 4. 未归属桶：空 id 表示旧记录；key 筛选必须传播到 summary。
    await page.locator('#filter-client-key').fill('unknown');
    await page.locator('#filter-auth').selectOption('unknown');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 1);
    assert.match(await page.locator('#requests-body').textContent(), /未归属/);
    await waitMetric(page, 'requests', '4');
    results.push('legacy unknown bucket: key filters reach the summary');

    // 5. 应用筛选重置 offset 与分页。
    await page.locator('#filter-client-key').fill('');
    await page.locator('#filter-auth').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 50);
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.includes('第 2 页'));
    await page.locator('#filter-auth').selectOption('1'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() =>
      document.getElementById('requests-page').textContent.includes('第 1 页') &&
      document.querySelectorAll('#requests-body tr').length === 26);
    assert.ok(calls.some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('auth_index') === '1'.repeat(16) && url.searchParams.get('offset') === '0'));
    results.push('applying a filter resets offset and page one');

    // 6. Key 分组：客户端维度 3 行（含未归属）；点击整行按完整指纹过滤。
    await page.locator('#filter-auth').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#keys-body tr').length === 3);
    const groupText = await page.locator('#keys-body').textContent();
    assert.match(groupText, /未归属/);
    assert.match(groupText, /700/);
    await page.locator('#keys-body tr').filter({ hasText: '未归属' }).locator('button').click();
    await page.waitForFunction(() => document.getElementById('filter-client-key').value === 'unknown');
    assert.equal(await page.locator('#filter-client-key').inputValue(), 'unknown');

    // 清除筛选后切换到上游凭据维度，凭据名称来自 CPA，可再次点击过滤。
    await page.locator('#filter-client-key').fill('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#keys-body tr').length === 3);
    await page.locator('#key-group-kind').selectOption('auth');
    await page.waitForFunction(() => document.querySelectorAll('#keys-body tr').length === 3);
    assert.match(await page.locator('#keys-body').textContent(), /工作账户（fake-file-a\.json）/);
    await page.locator('#keys-body tr').filter({ hasText: '工作账户' }).locator('button').click();
    await page.waitForFunction(() => document.getElementById('filter-auth').value === '1'.repeat(16));
    assert.equal(await page.locator('#filter-auth').inputValue(), '1'.repeat(16));
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 26);
    results.push('group table click filters full id and dimension switching works');

    // 7. 同一标签的不同认证文件仍各自可选，并以不同 auth_index 独立全局筛选。
    server.controls.credentialFiles = [
      { auth_index: '1'.repeat(16), name: 'dup-one.json', label: '同名' },
      { auth_index: '2'.repeat(16), name: 'dup-two.json', label: '同名' }
    ];
    await page.locator('#refresh').click();
    await page.waitForFunction(() => {
      const opts = Array.from(document.querySelectorAll('#filter-auth option')).map((o) => o.textContent);
      return opts.includes('同名（dup-one.json） · ' + '1'.repeat(16)) &&
        opts.includes('同名（dup-two.json） · ' + '2'.repeat(16));
    });
    await page.locator('#filter-auth').selectOption('2'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => {
      const rows = Array.from(document.querySelectorAll('#requests-body tr'));
      return rows.length === 25 && rows.every((row) => row.textContent.includes('同名（dup-two.json）'));
    });
    await waitMetric(page, 'requests', '530');
    results.push('same-label auth files stay distinguishable and filter independently');

    // 8. 凭据名称缺失时回落索引，仍可按 auth_index 精确筛选且统计一致。
    await page.locator('#filter-auth').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 50);
    server.controls.credentialFiles = [];
    await page.locator('#refresh').click();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('#filter-auth option'))
      .some((o) => o.textContent === '索引 ' + '1'.repeat(16) + ' · ' + '1'.repeat(16)));
    assert.equal((await page.locator('#credential-status').textContent()).trim(), '');
    await page.locator('#filter-auth').selectOption('1'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => {
      const rows = Array.from(document.querySelectorAll('#requests-body tr'));
      return rows.length === 26 && rows.every((row) => row.textContent.includes('索引 ' + '1'.repeat(16)));
    });
    await waitMetric(page, 'requests', '700');
    results.push('missing credential names fall back to the nonsecret index');

    // 9. 恶意文件名/标签只作文本渲染，不产生元素、脚本或弹窗。
    let dialogMessage = null;
    page.on('dialog', (d) => { dialogMessage = d.message(); d.dismiss().catch(() => {}); });
    server.controls.credentialFiles = [
      { auth_index: '1'.repeat(16), name: '<img src=x onerror=alert(1)>.json', label: '<script>evil</script>' }
    ];
    await page.locator('#refresh').click();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('#filter-auth option'))
      .some((o) => o.textContent.includes('onerror=alert(1)')));
    await page.locator('#filter-auth').selectOption('1'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => {
      const rows = Array.from(document.querySelectorAll('#requests-body tr'));
      return rows.length === 26 && rows.some((row) => row.textContent.includes('onerror=alert(1)'));
    });
    assert.match(await page.locator('#requests-body').textContent(),
      /<script>evil<\/script>（<img src=x onerror=alert\(1\)>\.json）/);
    assert.equal(await page.locator('#requests-body img, #requests-body script, #keys-body img, #keys-body script').count(), 0);
    assert.equal(dialogMessage, null);
    results.push('hostile file names and labels render as text only');

    // 恢复默认假认证文件，使响应式截图展示「标签（文件名）」。
    server.controls.credentialFiles = [
      { auth_index: mock.AUTH_INDEX_A, name: 'fake-file-a.json', label: '工作账户' },
      { auth_index: mock.AUTH_INDEX_B, name: 'fake-file-b.json', label: '备用账户' }
    ];
    await page.locator('#refresh').click();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('#filter-auth option'))
      .some((o) => o.textContent === '工作账户（fake-file-a.json） · ' + '1'.repeat(16)));
    await page.locator('#filter-auth').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 50);

    // 10. 响应式与安全：移动端无横向溢出、纯文本渲染、无页面错误。
    await page.setViewportSize({ width: 390, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    assert.equal(await page.locator('#requests-body img, #requests-body script').count(), 0);
    assert.equal(await page.locator('#keys-body img, #keys-body script').count(), 0);
    assert.deepEqual(errors, []);
    await page.screenshot({ path: path.join(out, 'keys-390.png'), fullPage: true });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.screenshot({ path: path.join(out, 'keys-1280.png'), fullPage: true });
    results.push('mobile layout, text-only rendering and error-free interactions');
  } finally {
    await browser.close();
    await server.close();
  }
  fs.writeFileSync(path.join(out, 'key-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}

main().catch((err) => { console.error(err); process.exitCode = 1; });
