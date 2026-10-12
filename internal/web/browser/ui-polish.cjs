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
    assert.match(await page.locator('#range-label').textContent(), /范围 20\d{2}-/);
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

    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#filter-provider-trigger').click();
    await page.locator('.select-option').filter({ hasText: '全部' }).click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    await page.locator('#requests-next').click();
    await page.locator('#requests-page').filter({ hasText: '第 2 页' }).waitFor();
    const oldPage = await page.locator('#requests-page').textContent();
    const oldRows = await page.locator('#requests-body').textContent();
    const oldFrom = calls.filter((url) => url.pathname.endsWith('/requests')).at(-1).searchParams.get('from');
    await page.locator('.seg-btn[data-range="30d"]').click();
    await page.route('**/summary**', (route) => route.fulfill({ status: 500, json: {} }));
    await page.locator('#time-apply').click();
    await page.locator('#conn-status').filter({ hasText: '加载失败' }).waitFor();
    assert.equal(await page.locator('#requests-page').textContent(), oldPage);
    assert.equal(await page.locator('#requests-body').textContent(), oldRows);
    await page.unroute('**/summary**');
    const pageCalls = calls.length;
    await page.locator('#requests-prev').click();
    await page.locator('#requests-page').filter({ hasText: '第 1 页' }).waitFor();
    assert.equal(calls.slice(pageCalls).find((url) => url.pathname.endsWith('/requests')).searchParams.get('from'), oldFrom);
    await page.locator('#refresh').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const recovered = calls.filter((url) => url.pathname.endsWith('/summary')).at(-1);
    assert.equal(Date.parse(recovered.searchParams.get('to')) - Date.parse(recovered.searchParams.get('from')), 30 * 24 * 60 * 60 * 1000);
    results.push('failed refresh keeps the loaded page and its successful window');

    await page.locator('#time-start').fill('2026-01-01T08:00:00.123');
    await page.locator('#time-end').fill('2026-01-01T09:00:00.456');
    await page.locator('#time-apply').click();
    await page.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    const manual = calls.filter((url) => url.pathname.endsWith('/summary')).at(-1);
    const startValue = await page.locator('#time-start').inputValue();
    const endValue = await page.locator('#time-end').inputValue();
    const converted = await page.evaluate(([start, end]) => {
      const ui = window.ObserverUI;
      return [ui.localInputToIso(start), ui.localInputToIso(end)];
    }, [startValue, endValue]);
    assert.equal(manual.searchParams.get('from'), converted[0]);
    assert.equal(manual.searchParams.get('to'), converted[1]);
    assert.notEqual(new Date(converted[0]).getUTCHours(), 8);
    results.push('manual datetime-local values convert exactly to the requested UTC bounds');

    const embed = await browser.newPage({ viewport: { width: 1280, height: 500 } });
    await embed.goto(server.embedURL);
    const frame = embed.frames().find((item) => item.url().includes('/ui'));
    await frame.locator('#mgmt-key').fill(mock.SECRET);
    await frame.locator('#connect').click();
    await frame.locator('#conn-status').filter({ hasText: '已更新' }).waitFor();
    await embed.evaluate(() => { document.documentElement.scrollTop = 180; });
    await frame.locator('#filter-provider-trigger').click();
    const menu = frame.locator('.select-popup');
    await menu.waitFor();
    const geometry = await frame.locator('.select-popup').evaluate((el) => {
      const rect = el.getBoundingClientRect();
      const frameRect = window.frameElement.getBoundingClientRect();
      const visibleBottom = Math.min(window.innerHeight, window.parent.innerHeight - frameRect.top);
      return rect.bottom <= visibleBottom + 1;
    });
    assert.equal(geometry, true);
    await embed.mouse.click(8, 8);
    await frame.locator('.select-popup').waitFor({ state: 'hidden' });
    results.push('iframe menu stays within the parent-visible area and closes on outside click');
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
