'use strict';

// 真实 Chromium：时间草稿只在显式应用后查询，缓存列按核算质量展示，
// CPA 风格下拉通过可见触发器提交原生 change。只连接本机假数据。

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');

async function main() {
  const out = process.argv[2];
  if (!out) throw new Error('output directory required');
  fs.mkdirSync(out, { recursive: true });
  const server = await mock.startServer();
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true });
  const results = [];
  const calls = [];
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('request', (request) => {
      if (request.url().includes('/management/')) calls.push(new URL(request.url()));
    });
    await page.goto(server.resourceURL);
    await page.locator('#mgmt-key').fill(mock.SECRET);
    const before = calls.length;
    await page.locator('#connect').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const initial = calls.slice(before).filter((url) => url.pathname.endsWith('/summary'));
    assert.equal(initial.length > 0, true);
    const from = initial[0].searchParams.get('from');
    const to = initial[0].searchParams.get('to');
    assert.equal(Date.parse(to) - Date.parse(from), 24 * 60 * 60 * 1000);
    assert.match(await page.locator('#range-label').textContent(), /已应用范围 20\d{2}-/);
    assert.match(await page.locator('#groups-body tr').first().textContent(), /%|—/);
    results.push('connect applies the initial 24h draft');

    const mark = calls.length;
    await page.locator('.seg-btn[data-range="7d"]').click();
    await page.waitForTimeout(200);
    assert.equal(calls.length, mark, 'preset must not query by itself');
    assert.match(await page.locator('#range-label').textContent(), /时间草稿未应用/);
    const shownFrom = new URL(calls.filter((url) => url.pathname.endsWith('/summary')).at(-1)).searchParams.get('from');
    assert.equal(shownFrom, from);
    await page.locator('#time-apply').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const applied = calls.slice(mark).find((url) => url.pathname.endsWith('/requests'));
    assert.equal(Date.parse(applied.searchParams.get('to')) - Date.parse(applied.searchParams.get('from')), 7 * 24 * 60 * 60 * 1000);
    assert.equal(applied.searchParams.get('offset'), '0');
    results.push('preset fills draft and explicit apply queries page one');

    const fixed = applied.searchParams.get('from');
    await page.locator('#time-start').fill('2020-01-01T00:00');
    await page.locator('#refresh').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const refreshed = calls.filter((url) => url.pathname.endsWith('/summary')).at(-1);
    assert.equal(refreshed.searchParams.get('from'), fixed, 'refresh keeps the applied range');
    await page.locator('#time-end').fill('2020-01-01T00:00');
    const invalidMark = calls.length;
    await page.locator('#time-apply').click();
    assert.match(await page.locator('#time-error').textContent(), /早于/);
    assert.equal(calls.length, invalidMark, 'invalid draft must not fetch');
    results.push('refresh keeps applied range and invalid draft does not fetch');

    await page.locator('.seg-btn[data-range="24h"]').click();
    await page.locator('#time-apply').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const rows = await page.locator('#requests-body tr').evaluateAll((nodes) => nodes.map((node) => ({
      text: node.textContent,
      title: node.querySelectorAll('td')[6] ? node.querySelectorAll('td')[6].title : ''
    })));
    assert.equal(rows.some((row) => row.text.includes('0.0%') && row.title.includes('cache_read_tokens / input_tokens')), true);
    assert.equal(rows.some((row) => row.text.includes('—') && row.title.includes('不支持')), true);
    results.push('request cache share distinguishes complete zero and unclassified');

    const trigger = page.locator('#filter-provider-trigger');
    await trigger.click();
    const popup = page.locator('.select-popup');
    await popup.waitFor();
    assert.equal(await popup.evaluate((el) => getComputedStyle(el).position), 'fixed');
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    assert.equal(await page.locator('#filter-provider').inputValue(), 'anthropic');
    assert.equal(calls.filter((url) => url.pathname.endsWith('/summary')).at(-1).searchParams.get('provider'), 'anthropic');
    await page.locator('#filter-provider').evaluate((el) => { el.disabled = true; });
    await page.evaluate(() => window.__observerSelects.refresh());
    assert.equal(await page.locator('#filter-provider-trigger').isDisabled(), true);
    await page.locator('#filter-provider').evaluate((el) => { el.disabled = false; });
    await page.evaluate(() => window.__observerSelects.refresh());
    await page.locator('#connect').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    assert.equal(await page.locator('.select-popup').count(), 0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    assert.deepEqual(errors, []);
    await page.screenshot({ path: path.join(out, 'ui-polish-1280.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.screenshot({ path: path.join(out, 'ui-polish-390.png'), fullPage: true });
    results.push('dropdown keyboard commit, disabled source and reconnect cleanup');
  } finally {
    await browser.close();
    await server.close();
  }
  fs.writeFileSync(path.join(out, 'ui-polish-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
