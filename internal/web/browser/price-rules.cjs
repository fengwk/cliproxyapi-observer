'use strict';

// Ordered conditional pricing browser scenario. Real Chromium against the local
// fake host only: proves UI reordering/validation persist first-match rules and
// that the effective summary cost follows thresholds and UTC windows, then
// captures light/dark/mobile pricing screenshots.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');
const OUT = process.argv[2];
if (!OUT) throw new Error('Provide an external evidence directory');

const results = [];
const ready = (frame) => frame.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
async function connect(frame) {
  await frame.locator('#mgmt-key').fill(mock.SECRET);
  await frame.locator('#connect').click();
  await ready(frame);
}
async function save(frame, accept = true) {
  await frame.locator('#settings-save').click();
  const dialog = frame.locator('#confirmation-dialog');
  await dialog.waitFor({ state: 'visible' });
  if (accept) await frame.locator('#confirmation-confirm').click();
  else await frame.locator('#confirmation-cancel').click();
  await dialog.waitFor({ state: 'hidden' });
}
const saved = (frame) => frame.locator('#settings-status').filter({ hasText: '保存成功' }).waitFor();
const waitCost = (frame, value) => frame.waitForFunction((expected) => {
  const el = document.querySelector('[data-metric="cost"] .metric-value');
  return el && el.textContent.trim() === expected;
}, value);

// 默认 prices map 已含 gpt-5.1-codex 一行；该行即第一条规则。
async function fillRule(row, { model, prices, threshold, range }) {
  await row.locator('[data-price="model"]').fill(model || '');
  for (const key of ['input', 'output', 'cache-read', 'cache-creation']) {
    await row.locator('[data-price="' + key + '"]').fill(String(prices[key]));
  }
  await row.locator('[data-price="input-tokens-gt"]').fill(threshold === undefined ? '' : String(threshold));
  await row.locator('[data-price="time-range"]').fill(range === undefined ? '' : range);
}

const UNIT = { input: 1, output: 1, 'cache-read': 1, 'cache-creation': 1 };
const ZERO = { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 };

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true,
    args: ['--no-sandbox']
  });
  async function run(name, fn, options = {}) {
    const server = await mock.startServer({ captureBodies: false });
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, ...options });
    const page = await context.newPage();
    page.on('dialog', (d) => { throw new Error('严禁原生弹窗：' + d.message()); });
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    try {
      await fn({ server, page });
      assert.deepEqual(errors, []);
      results.push({ name, ok: true });
      console.log('PASS ' + name);
    } catch (e) {
      results.push({ name, ok: false, error: e.message });
      console.error('FAIL ' + name + ': ' + e.message);
      await page.screenshot({ path: path.join(OUT, 'failure-' + results.length + '.png'), fullPage: true }).catch(() => {});
    } finally {
      await context.close(); await server.close();
    }
  }

  try {
    await run('Threshold is strict greater-than and first match wins for the whole request', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 0 });
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(1), { model: 'gpt-5.1-codex', prices: ZERO });
      await save(page); await saved(page);
      // 阈值 0 且严格大于：每个身份的 gpt 输入都命中首条 1x 单价 => 3.32 USD。
      await waitCost(page, '$3.3200');
      const rules = server.getConfig()['price-rules'];
      assert.deepEqual(rules.map((r) => r['input-tokens-gt']), [0, undefined]);
      assert.deepEqual(server.getConfig().prices, {});

      // 阈值超过所有身份输入 => 落到默认 0 价。
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 9000000 });
      await save(page); await saved(page);
      await waitCost(page, '$0.0000');

      // 恰好等于最小身份的 gpt 输入（8169）不算命中（严格大于）=> 该身份回落默认价。
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 8169 });
      await save(page); await saved(page);
      await waitCost(page, '$3.3092');

      // 低 1 即重新命中全部身份。
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 8168 });
      await save(page); await saved(page);
      await waitCost(page, '$3.3200');
    });

    await run('Reordering rules changes first-match outcome and persists across reload', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 0 });
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(1), { model: 'gpt-5.1-codex', prices: ZERO });
      await save(page); await saved(page);
      await waitCost(page, '$3.3200');

      // 上移默认规则到首位：其余不变，但 first-match 变为 0 价，且草稿变脏。
      await page.locator('.price-row').nth(1).getByRole('button', { name: '上移' }).click();
      assert.match(await page.locator('#settings-status').textContent(), /未保存的草稿/);
      await save(page); await saved(page);
      await waitCost(page, '$0.0000');
      assert.deepEqual(server.getConfig()['price-rules'].map((r) => r.price.input), [0, 1]);

      // 重新加载后顺序保持（第一条为 0 价默认规则）。
      await page.reload(); await connect(page);
      assert.equal(await page.locator('.price-row').nth(0).locator('[data-price="input"]').inputValue(), '0');
      assert.equal(await page.locator('.price-row').nth(1).locator('[data-price="input"]').inputValue(), '1');
    });

    await run('UTC windows are inclusive-start, exclusive-end, overnight and 24:00 aware', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      // summary 参考时刻为 2026-01-02T12:00Z。
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, range: '11:00-13:00' });
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(1), { model: 'gpt-5.1-codex', prices: ZERO });
      await save(page); await saved(page);
      await waitCost(page, '$3.3200');

      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, range: '13:00-24:00' });
      await save(page); await saved(page);
      await waitCost(page, '$0.0000');

      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, range: '22:00-06:00' });
      await save(page); await saved(page);
      await waitCost(page, '$0.0000');

      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, range: '00:00-24:00' });
      await save(page); await saved(page);
      await waitCost(page, '$3.3200');
      assert.equal(server.getConfig()['price-rules'][0]['time-range'], '00:00-24:00');
    });

    await run('Deletion and empty rule list persist as prices:{} + price-rules:[]', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(1), { model: 'claude-opus-4-1', prices: UNIT });
      await save(page); await saved(page);
      assert.ok(server.getConfig()['price-rules'].length >= 2);
      // 删除所有规则并保存。
      while (await page.locator('.price-row').count() > 0) {
        await page.locator('.price-row').last().getByRole('button', { name: '删除' }).click();
      }
      await save(page); await saved(page);
      assert.deepEqual(server.getConfig().prices, {});
      assert.deepEqual(server.getConfig()['price-rules'], []);
      await page.reload(); await connect(page);
      assert.equal(await page.locator('.price-row').count(), 0);
    });

    await run('Malformed rules are rejected before write; backend rejection keeps the draft', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const row = page.locator('.price-row').nth(0);
      await fillRule(row, { model: 'gpt-5.1-codex', prices: UNIT, range: '9:00-10:00' });
      await save(page);
      await page.locator('#settings-status').filter({ hasText: '时间区间' }).waitFor();
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);

      // 合法草稿 + 后端校验失败：不写入、不宣称成功、草稿保留。
      await fillRule(row, { model: 'gpt-5.1-codex', prices: UNIT, threshold: 1000000 });
      server.controls.simulateValidateFailure = true;
      await save(page);
      await page.locator('#settings-status').filter({ hasText: '保存未确认' }).waitFor();
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await row.locator('[data-price="input-tokens-gt"]').inputValue(), '1000000');
      server.controls.simulateValidateFailure = false;
      await save(page); await saved(page);
      assert.equal(server.counters.configWrites, 1);
      assert.deepEqual(server.getConfig()['price-rules'][0]['input-tokens-gt'], 1000000);
    });

    await run('Pricing editor screenshots: light/white/dark 1280px and light 390px', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await fillRule(page.locator('.price-row').nth(0), { model: 'gpt-5.1-codex', prices: UNIT, threshold: 1000000, range: '22:00-06:00' });
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(1), { model: 'claude-opus-4-1', prices: { input: 3, output: 15, 'cache-read': 0.75, 'cache-creation': 3 } });
      await page.locator('#price-add').click();
      await fillRule(page.locator('.price-row').nth(2), { model: 'example-model', prices: { input: 0.5, output: 1, 'cache-read': 0.1, 'cache-creation': 0.5 } });

      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 900 });
        for (const theme of ['', 'white', 'dark']) {
          await page.evaluate((t) => {
            if (t) document.documentElement.setAttribute('data-theme', t);
            else document.documentElement.removeAttribute('data-theme');
          }, theme);
          const tName = theme || 'light';
          await page.locator('#settings-body').scrollIntoViewIfNeeded();
          await page.screenshot({ path: path.join(OUT, 'price-rules-' + tName + '-' + width + '.png'), fullPage: true });
        }
      }
      // 390px 不得横向溢出。
      await page.setViewportSize({ width: 390, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth) <= 1);
      assert.equal(await page.locator('#settings-body img').count(), 0);
    });
  } finally { await browser.close(); }
  fs.writeFileSync(path.join(OUT, 'price-rules-results.json'), JSON.stringify(results, null, 2));
  console.log(`${results.filter((r) => r.ok).length}/${results.length} passed`);
  if (results.some((r) => !r.ok)) process.exitCode = 1;
}

main().catch((e) => { console.error(e); process.exitCode = 1; });
