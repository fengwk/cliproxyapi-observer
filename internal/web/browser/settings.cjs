'use strict';

// Real Chromium interactions against local fake CPA only; no production credentials.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');
const OUT = process.argv[2];
if (!OUT) throw new Error('Provide an external evidence directory');

const results = [];
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};
const ready = (frame) => frame.waitForFunction(() =>
  document.getElementById('conn-status').textContent.startsWith('已更新'));
async function connect(frame) {
  await frame.locator('#mgmt-key').fill(mock.SECRET);
  await frame.locator('#connect').click();
  await ready(frame);
}

function resolveTarget(arg1, arg2, arg3) {
  let root = arg1;
  let accept = true;
  if (typeof arg2 === 'boolean') {
    accept = arg2;
  } else if (arg2 && typeof arg2.locator === 'function') {
    root = arg2;
    if (typeof arg3 === 'boolean') {
      accept = arg3;
    }
  }
  return { root, accept };
}

async function save(arg1, arg2, arg3) {
  const { root, accept } = resolveTarget(arg1, arg2, arg3);
  const dialog = root.locator('#confirmation-dialog');
  await root.locator('#settings-save').click();
  await dialog.waitFor({ state: 'visible' });
  assert.equal(await root.locator('#confirmation-title').textContent(), '确认保存设置');
  assert.match(await root.locator('#confirmation-desc').textContent(), /提示词.*永久删除.*价格/);
  if (accept) {
    await root.locator('#confirmation-confirm').click();
  } else {
    await root.locator('#confirmation-cancel').click();
  }
  await dialog.waitFor({ state: 'hidden' });
}

async function revert(arg1, arg2, arg3) {
  const { root, accept } = resolveTarget(arg1, arg2, arg3);
  const dialog = root.locator('#confirmation-dialog');
  await root.locator('#settings-revert').click();
  await dialog.waitFor({ state: 'visible' });
  assert.equal(await root.locator('#confirmation-title').textContent(), '放弃未保存草稿');
  assert.match(await root.locator('#confirmation-desc').textContent(), /放弃未保存草稿/);
  if (accept) {
    await root.locator('#confirmation-confirm').click();
  } else {
    await root.locator('#confirmation-cancel').click();
  }
  await dialog.waitFor({ state: 'hidden' });
}

const saved = (frame) => frame.locator('#settings-status').filter({ hasText: '保存成功' }).waitFor();

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
    page.on('dialog', (d) => {
      throw new Error('严禁原生弹窗：' + d.message());
    });
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    try {
      await fn({ server, context, page });
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
    await run('Capture and pricing CRUD save explicitly, survive reload, preserve metadata and secrets', async ({ server, page }) => {
      const network = [];
      page.on('request', (r) => network.push({ url: r.url(), method: r.method(), body: r.postData(), auth: r.headers().authorization }));
      await page.goto(server.resourceURL); await connect(page);
      assert.equal(await page.locator('#mgmt-key').getAttribute('name'), null);
      assert.equal(await page.locator('#mgmt-key').evaluate((n) => n.closest('form')), null);
      await page.locator('#setting-capture-bodies').check();
      assert.match(await page.locator('.settings-note').allTextContents().then((v) => v.join(' ')), /提示词.*永久删除/);
      await page.locator('#price-add').click();
      const row = page.locator('.price-row').last();
      await row.locator('[data-price="model"]').fill(mock.XSS_MODEL);
      await row.locator('[data-price="input"]').fill('1.25');
      await row.locator('[data-price="output"]').fill('2.5');
      await row.locator('[data-price="cache-read"]').fill('0.2');
      await row.locator('[data-price="cache-creation"]').fill('0.5');
      assert.equal(server.counters.configWrites, 0);
      await page.locator('#refresh').click(); await ready(page);
      assert.equal(await row.locator('[data-price="model"]').inputValue(), mock.XSS_MODEL);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(server.getSettings().capture_bodies, false);
      await save(page, page, false);
      assert.equal(server.counters.validations, 0);
      server.controls.reconfigureTemporary503Count = 2;
      await save(page, page); await saved(page);
      assert.equal(server.counters.configWrites, 1);
      assert.equal(server.counters.reconfigure503s, 2);
      assert.equal(server.getConfig().db, 'data/cliproxyapi-observer.db');
      assert.equal(server.getConfig().flush, '1s');
      assert.deepEqual(server.getConfig().store, { fixture: 'preserve-me' });
      assert.equal(server.getConfig().enabled, true);
      // Canonical save replaces the legacy map with an ordered rule list.
      assert.deepEqual(server.getConfig().prices, {});
      const savedRules = server.getConfig()['price-rules'];
      assert.ok(savedRules.some((r) => r.model === 'gpt-5.1-codex' && r.price.input === 1.25),
        '默认 prices map 行必须转换为无条件规则保留');
      assert.equal(savedRules.find((r) => r.model === mock.XSS_MODEL).price['cache-read'], 0.2);
      assert.equal(await page.locator('#settings-body img').count(), 0);
      await page.reload(); await connect(page);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await page.locator('[data-price="model"]').last().inputValue(), mock.XSS_MODEL);
      await page.locator('.price-row').last().getByRole('button', { name: '删除' }).click();
      await page.locator('.price-row').last().getByRole('button', { name: '删除' }).click();
      await save(page, page); await saved(page);
      assert.deepEqual(server.getConfig().prices, {});
      assert.deepEqual(server.getConfig()['price-rules'], []);
      await page.reload(); await connect(page);
      assert.equal(await page.locator('.price-row').count(), 0);
      assert.equal(await page.evaluate(() => window.__observerXss || 0), 0);
      for (const r of network) {
        assert.ok(!r.url.includes(mock.SECRET));
        assert.ok(!r.body || !r.body.includes(mock.SECRET));
        if (r.url.includes('/management/')) assert.equal(r.auth, 'Bearer ' + mock.SECRET);
        assert.ok(!r.url.includes('/management/config')); // Never fetch full CPA config.
        if (r.method === 'PATCH' || r.method === 'POST') {
          const payload = JSON.parse(r.body);
          for (const key of ['db', 'enabled', 'store', 'flush'])
            assert.equal(Object.hasOwn(payload, key), false);
        }
      }
      assert.deepEqual(await page.evaluate(() => [localStorage.length, sessionStorage.length, document.cookie]), [0, 0, '']);
      await page.screenshot({ path: path.join(OUT, 'standalone-saved.png'), fullPage: true });
    });

    await run('Client bounds and blank/malformed/negative prices never reach persistence', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      for (const [id, value] of [
        ['setting-body-retention', '2'], ['setting-request-retention', '31'],
        ['setting-stats-retention-days', '3651'], ['setting-max-body-bytes', '65'],
        ['setting-max-body-storage-bytes', '0.5'], ['setting-compact-min-bytes', '0.01']
      ]) {
        const control = page.locator('#' + id), old = await control.inputValue();
        await control.fill(value);
        await save(page, page);
        await page.locator('#settings-status').filter({ hasText: '未保存' }).waitFor();
        assert.equal(server.counters.validations, 0); assert.equal(server.counters.configWrites, 0);
        await control.fill(old);
      }
      await page.locator('#price-add').click();
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '未保存' }).waitFor();
      assert.equal(server.counters.validations, 0);
      const row = page.locator('.price-row').last();
      // 有序规则允许多条同模型规则；但畸形 UTC 区间必须先被本地拒绝。
      await row.locator('[data-price="model"]').fill('gpt-5.1-codex');
      await row.locator('[data-price="time-range"]').fill('8:00-9:00');
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '时间区间' }).waitFor();
      assert.equal(server.counters.configWrites, 0);
      assert.equal(server.counters.validations, 0);
      await row.locator('[data-price="time-range"]').fill('');
      await row.locator('[data-price="input"]').fill('-1');
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '未保存' }).waitFor();
      assert.equal(server.counters.configWrites, 0);
      // 恢复合法值：重复模型作为有序规则持久化（first-match 语义）。
      await row.locator('[data-price="input"]').fill('0');
      await save(page, page); await saved(page);
      assert.equal(server.counters.configWrites, 1);
      assert.deepEqual(server.getConfig()['price-rules'].map((r) => r.model), ['gpt-5.1-codex', 'gpt-5.1-codex']);
      assert.equal(await page.locator('.price-row').count(), 2);
      assert.equal(await page.locator('#setting-max-body-bytes').inputValue(), '1');
    });

    await run('Validation/write/read failures preserve dirty drafts and never claim success', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await page.locator('#setting-stats-retention-days').fill('10');
      server.controls.simulateValidateFailure = true;
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '保存未确认' }).waitFor();
      assert.equal(server.counters.configWrites, 0);
      server.controls.simulateValidateFailure = false;
      server.controls.simulateWriteFailure = true;
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '保存未确认' }).waitFor();
      assert.equal(server.getConfig()['stats-retention-days'], 365);
      server.controls.simulateWriteFailure = false;
      server.controls.simulateConfigReadFail = true;
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '未能确认生效' }).waitFor();
      assert.equal(await page.locator('#setting-stats-retention-days').inputValue(), '10');
      await page.locator('#refresh').click(); await ready(page);
      assert.equal(await page.locator('#setting-stats-retention-days').inputValue(), '10');
      assert.ok(!(await page.locator('#settings-status').textContent()).includes('保存成功'));
      await page.screenshot({ path: path.join(OUT, 'save-read-failure.png'), fullPage: true });
      await revert(page, page);
      await page.locator('#settings-status').filter({ hasText: '重新加载失败' }).waitFor();
      assert.equal(await page.locator('#setting-stats-retention-days').inputValue(), '10');
    });

    await run('Effective mismatch fails clearly rather than claiming persisted success', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      server.controls.simulateEffectiveNeverUpdates = true;
      await page.locator('#setting-stats-retention-days').fill('20');
      await save(page, page);
      await page.locator('#settings-status').filter({ hasText: '未能确认生效' }).waitFor({ timeout: 20000 });
      assert.equal(server.getConfig()['stats-retention-days'], 20);
      assert.equal(server.getSettings().stats_retention_days, 365);
      assert.equal(await page.locator('#setting-stats-retention-days').inputValue(), '20');
    });

    await run('Reconnect aborts stale validation before write; duplicate saves are blocked', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const entered = deferred(), release = deferred(), finished = deferred();
      await page.route('**/validate', async (route) => {
        entered.resolve(); await release.promise;
        try { await route.fulfill({ json: { valid: true } }); } catch (_) { /* aborted client */ }
        finished.resolve();
      });
      await page.locator('#setting-capture-bodies').check();
      await save(page, page); await entered.promise;
      assert.equal(await page.locator('#settings-save').isDisabled(), true);
      await page.locator('#refresh').click(); // Must not supersede save or erase draft.
      await page.locator('#connect').click(); await ready(page);
      release.resolve(); await finished.promise;
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), false);
      assert.ok(!(await page.locator('#settings-status').textContent()).includes('保存成功'));
    });

    await run('Hung validation times out, restores controls and cannot write later', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      await page.clock.install();
      const entered = deferred(), release = deferred(), finished = deferred();
      await page.route('**/validate', async (route) => {
        entered.resolve(); await release.promise;
        try { await route.fulfill({ json: { valid: true } }); } catch (_) { /* aborted client */ }
        finished.resolve();
      });
      await page.locator('#setting-capture-bodies').check();
      await save(page, page); await entered.promise;
      await page.clock.fastForward(15001);
      await page.locator('#settings-status').filter({ hasText: '保存确认超时' }).waitFor();
      assert.equal(await page.locator('#settings-save').isDisabled(), false);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      release.resolve(); await finished.promise;
      assert.equal(server.counters.configWrites, 0);
    });

    await run('Async confirmation modal: cancel, Escape, focus restoration, duplicate clicks, and clean vs dirty revert', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const dialog = page.locator('#confirmation-dialog');
      const title = page.locator('#confirmation-title');
      const desc = page.locator('#confirmation-desc');
      const confirmBtn = page.locator('#confirmation-confirm');
      const cancelBtn = page.locator('#confirmation-cancel');
      const saveBtn = page.locator('#settings-save');
      const revertBtn = page.locator('#settings-revert');

      // 1. Accessibility attributes
      assert.equal(await dialog.getAttribute('aria-labelledby'), 'confirmation-title');
      assert.equal(await dialog.getAttribute('aria-describedby'), 'confirmation-desc');
      assert.equal(await dialog.evaluate((el) => el.open), false);

      // 2. Clean revert: not dirty -> no modal opened, reloads directly
      await revertBtn.click();
      assert.equal(await dialog.evaluate((el) => el.open), false);
      await page.locator('#settings-status').filter({ hasText: '已重新加载' }).waitFor();

      // 3. Make dirty
      await page.locator('#setting-capture-bodies').check();
      assert.match(await page.locator('#settings-status').textContent(), /未保存的草稿/);

      // 4. Cancel save via cancel button
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });
      assert.equal(await title.textContent(), '确认保存设置');
      assert.match(await desc.textContent(), /提示词.*永久删除.*价格/);
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'confirmation-cancel');
      await cancelBtn.click();
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'settings-save');

      // 5. Cancel save via Escape key
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });
      await page.keyboard.press('Escape');
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'settings-save');

      // 6. Duplicate clicks do not open multiple modals
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });
      await page.evaluate(() => {
        document.getElementById('settings-save').click();
        document.getElementById('settings-revert').click();
      });
      assert.equal(await page.locator('#confirmation-dialog').count(), 1);
      assert.equal(await dialog.evaluate((el) => el.open), true);
      await cancelBtn.click();
      await dialog.waitFor({ state: 'hidden' });

      // Backdrop cancellation follows the same no-write path.
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });
      await page.mouse.click(5, 5);
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);

      // 7. Dirty revert: shows modal, cancel keeps draft
      await revertBtn.click();
      await dialog.waitFor({ state: 'visible' });
      assert.equal(await title.textContent(), '放弃未保存草稿');
      assert.match(await desc.textContent(), /放弃未保存草稿/);
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'confirmation-cancel');
      await cancelBtn.click();
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'settings-revert');

      // 8. Dirty revert confirm via Space key on confirm button
      await revertBtn.click();
      await dialog.waitFor({ state: 'visible' });
      await confirmBtn.focus();
      await page.keyboard.press('Space');
      await dialog.waitFor({ state: 'hidden' });
      await page.locator('#settings-status').filter({ hasText: '已重新加载' }).waitFor();
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), false);
    });

    // Saving reconfigures the backend; an earlier page response must never win.
    await run('Saving settings cancels pending page and resets derived refresh to page one', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const entered = deferred(), release = deferred(), finished = deferred();
      await page.route('**/requests**', async (route) => {
        if (new URL(route.request().url()).searchParams.get('offset') !== '50') return route.continue();
        entered.resolve(); await release.promise;
        try { await route.fulfill({ json: { items: [{ model: 'STALE-BEFORE-SAVE' }], offset: 50, limit: 50, has_more: false } }); }
        catch (_) { /* client aborted */ }
        finished.resolve();
      });
      await page.locator('#requests-next').click(); await entered.promise;
      await page.locator('#setting-capture-bodies').check();
      await save(page); await saved(page); await ready(page);
      release.resolve(); await finished.promise;
      assert.equal(await page.locator('#requests-body').getByText('STALE-BEFORE-SAVE').count(), 0);
      assert.equal(await page.locator('#requests-page').textContent(), '第 1 页 · 本页 50 条');
      assert.equal(server.counters.configWrites, 1);
    });

    await run('Prices save: visibly updates historical request row cost AND summary cost dynamically in real browser', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);

      // A previously retained complete request acquires a price after settings save.
      const initialClaudeRow = page.locator('#requests-body tr:has-text("claude-opus-4-1")').first();
      assert.ok(await initialClaudeRow.isVisible());
      const initialClaudeCost = await initialClaudeRow.locator('td').nth(9).textContent();
      assert.equal(initialClaudeCost, '未定价');

      // Verify initial summary total cost
      const initialTotalCost = await page.locator('[data-metric="cost"] .metric-value').textContent();

      // Go to settings: add price configuration for claude-opus-4-1
      await page.locator('#price-add').click();
      const row = page.locator('.price-row').last();
      await row.locator('[data-price="model"]').fill('claude-opus-4-1');
      await row.locator('[data-price="input"]').fill('3.0');
      await row.locator('[data-price="output"]').fill('15.0');
      await row.locator('[data-price="cache-read"]').fill('0.75');
      await row.locator('[data-price="cache-creation"]').fill('3.0');

      // Save settings with confirmation modal
      await save(page, page);
      await saved(page);

      // After save succeeds, refreshAll is invoked by UI.
      // Assert historical row cost dynamically displays formatted cost ($...)
      await page.waitForFunction(() => {
        const rows = Array.from(document.querySelectorAll('#requests-body tr'));
        const r = rows.find((el) => el.textContent.includes('claude-opus-4-1'));
        if (!r) return false;
        const c = r.querySelectorAll('td')[9];
        return c && c.textContent.includes('$');
      });
      const updatedClaudeCost = await page.locator('#requests-body tr:has-text("claude-opus-4-1")').first().locator('td').nth(9).textContent();
      assert.match(updatedClaudeCost, /\$/);

      // Assert summary total cost has updated dynamically
      await page.waitForFunction((prev) => {
        const el = document.querySelector('[data-metric="cost"] .metric-value');
        return el && el.textContent !== prev;
      }, initialTotalCost);

      // Assert groups table row for claude-opus-4-1 also shows priced cost ($)
      await page.waitForFunction(() => {
        const rows = Array.from(document.querySelectorAll('#groups-body tr'));
        const r = rows.find((el) => el.textContent.includes('claude-opus-4-1'));
        if (!r) return false;
        const c = r.querySelectorAll('td')[8];
        return c && c.textContent.includes('$');
      });
    });

    await run('Async confirmation modal: Tab focus trap, auth failure closing, and stale confirm clicked after reconnect zero writes', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const dialog = page.locator('#confirmation-dialog');
      const cancelBtn = page.locator('#confirmation-cancel');
      const confirmBtn = page.locator('#confirmation-confirm');
      const saveBtn = page.locator('#settings-save');

      // 1. Tab focus trap inside open modal
      await page.locator('#setting-capture-bodies').check();
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });

      // Initial focus on cancel button
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'confirmation-cancel');

      // Tab moves to confirm button
      await page.keyboard.press('Tab');
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'confirmation-confirm');

      // Shift+Tab moves back to cancel button
      await page.keyboard.press('Shift+Tab');
      assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.id), 'confirmation-cancel');

      // Close modal
      await cancelBtn.click();
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);

      // 2. Auth failure closing modal
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });

      // Route 403 on management endpoints to simulate auth failure
      await page.route('**' + mock.MANAGEMENT_BASE + '/settings**', (route) => {
        return route.fulfill({ status: 403, json: { error: 'invalid key' } });
      });

      // An in-flight refresh failure invalidates the modal, not just reconnect.
      await page.evaluate(() => {
        document.getElementById('refresh').click();
      });
      await dialog.waitFor({ state: 'hidden' });
      assert.equal(await dialog.evaluate((el) => el.open), false);
      await page.locator('#conn-status').filter({ hasText: '管理密钥' }).waitFor();
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);

      // 3. Stale confirm clicked after reconnect causes zero writes
      await page.unroute('**' + mock.MANAGEMENT_BASE + '/settings**');
      await connect(page);
      await page.locator('#setting-capture-bodies').check();
      await saveBtn.click();
      await dialog.waitFor({ state: 'visible' });

      // Reconnect happens while modal is open
      await page.evaluate(() => document.getElementById('connect').click());
      await ready(page);
      await dialog.waitFor({ state: 'hidden' });

      // Simulate stale click on confirm button
      await page.evaluate(() => {
        document.getElementById('confirmation-confirm').click();
      });

      // Zero writes and zero validations must reach the server for the new connection
      assert.equal(server.counters.validations, 0);
      assert.equal(server.counters.configWrites, 0);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), false);
    });

    await run('Themed accessible checkbox: computed checked/disabled/focus/highcontrast styles and modal screenshots across themes', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      const checkbox = page.locator('#setting-capture-bodies');
      const span = page.locator('label:has(#setting-capture-bodies) span');

      // 1. Appearance none
      const cs = await checkbox.evaluate((el) => {
        const s = window.getComputedStyle(el);
        return {
          appearance: s.appearance || s.webkitAppearance,
          display: s.display
        };
      });
      assert.equal(cs.appearance, 'none');
      assert.equal(cs.display, 'grid');

      // 2. Toggle via label span click and computed checked style
      assert.equal(await checkbox.isChecked(), false);
      await span.click();
      assert.equal(await checkbox.isChecked(), true);

      const checkedStyle = await checkbox.evaluate((el) => {
        const s = window.getComputedStyle(el);
        const theme = window.getComputedStyle(document.documentElement);
        const probe = document.createElement('span');
        probe.style.color = theme.getPropertyValue('--primary-color');
        document.body.appendChild(probe);
        const expected = getComputedStyle(probe).color;
        probe.remove();
        return {
          bg: s.backgroundColor,
          border: s.borderColor,
          expected,
          indicator: getComputedStyle(el, '::before').transform
        };
      });
      assert.equal(checkedStyle.bg, checkedStyle.expected);
      assert.equal(checkedStyle.border, checkedStyle.expected);
      assert.notEqual(checkedStyle.indicator, 'matrix(0, 0, 0, 0, 0, 0)');

      // Toggle back
      await span.click();
      assert.equal(await checkbox.isChecked(), false);

      // 3. Toggle via Space key when focused
      await checkbox.focus();
      await page.keyboard.press('Space');
      assert.equal(await checkbox.isChecked(), true);

      // 4. Focus / focus-visible style
      const focusStyle = await checkbox.evaluate((el) => {
        const s = window.getComputedStyle(el);
        return {
          outlineStyle: s.outlineStyle,
          outlineWidth: s.outlineWidth
        };
      });
      assert.notEqual(focusStyle.outlineStyle, 'none');
      assert.equal(focusStyle.outlineWidth, '2px');

      await page.keyboard.press('Space');
      assert.equal(await checkbox.isChecked(), false);

      // 5. Disabled style: disconnect
      await page.evaluate(() => {
        document.getElementById('mgmt-key').value = '';
        document.getElementById('connect').click();
      });
      assert.equal(await checkbox.isDisabled(), true);
      const disabledStyle = await checkbox.evaluate((el) => {
        const s = window.getComputedStyle(el);
        return { opacity: s.opacity, cursor: s.cursor };
      });
      assert.equal(disabledStyle.opacity, '0.55');
      assert.equal(disabledStyle.cursor, 'not-allowed');

      // Disabled checkbox ignores click and Space
      await span.click({ force: true }).catch(() => {});
      assert.equal(await checkbox.isChecked(), false);
      await checkbox.focus().catch(() => {});
      await page.keyboard.press('Space').catch(() => {});
      assert.equal(await checkbox.isChecked(), false);

      // 6. High contrast / forced colors stylesheet rule verification
      const hasForcedColorRules = await page.evaluate(() => {
        let count = 0;
        for (const sheet of document.styleSheets) {
          try {
            for (const rule of sheet.cssRules) {
              if (rule.conditionText && rule.conditionText.includes('forced-colors')) count++;
            }
          } catch (_) {}
        }
        return count > 0;
      });
      assert.equal(hasForcedColorRules, true, 'CSS 必须包含 @media (forced-colors: active) 无障碍高对比度规则');
      await page.emulateMedia({ forcedColors: 'active' });
      assert.equal(await checkbox.evaluate((el) => getComputedStyle(el).forcedColorAdjust), 'none');
      await page.emulateMedia({ forcedColors: 'none' });

      // 7. Standalone page AND modal screenshots across 3 themes at 1280px and 390px
      await connect(page);
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 900 });
        for (const theme of ['', 'white', 'dark']) {
          await page.evaluate((t) => {
            if (t) document.documentElement.setAttribute('data-theme', t);
            else document.documentElement.removeAttribute('data-theme');
          }, theme);
          const tName = theme || 'light';
          // Page screenshot
          await page.screenshot({ path: path.join(OUT, 'standalone-settings-' + tName + '-' + width + '.png'), fullPage: true });

          // Modal screenshot
          await page.locator('#setting-capture-bodies').check();
          await page.locator('#settings-save').click();
          await page.locator('#confirmation-dialog').waitFor({ state: 'visible' });
          await page.screenshot({ path: path.join(OUT, 'standalone-modal-' + tName + '-' + width + '.png') });
          await page.locator('#confirmation-cancel').click();
          await page.locator('#confirmation-dialog').waitFor({ state: 'hidden' });
          await revert(page);
          await page.waitForFunction(() => !document.getElementById('setting-capture-bodies').checked);
        }
      }
    });

    await run('All three themes at wide/390px iframe: host overlay never blocks connection actions/status', async ({ server, page }) => {
      await page.goto(server.embedURL);
      const frame = page.frame({ url: (u) => u.pathname === mock.RESOURCE_BASE + '/ui' });
      await connect(frame);
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 900 });
        for (const theme of ['', 'white', 'dark']) {
          await page.evaluate((t) => window.__setTheme(t), theme);
          await frame.waitForFunction((t) => (document.documentElement.getAttribute('data-theme') || '') === t, theme);
          await page.evaluate(() => window.scrollTo(0, 0));
          await frame.evaluate(() => window.scrollTo(0, 0));
          const overlay = await page.locator('.host-overlay').boundingBox();
          for (const id of ['mgmt-key', 'connect', 'refresh', 'conn-status']) {
            const box = await frame.locator('#' + id).boundingBox();
            assert.ok(box && box.width > 0 && box.height > 0);
            const overlap = box.x < overlay.x + overlay.width && box.x + box.width > overlay.x &&
              box.y < overlay.y + overlay.height && box.y + box.height > overlay.y;
            assert.equal(overlap, false, id + ' overlaps host chrome');
            assert.equal(await page.evaluate((b) => document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2).tagName, box), 'IFRAME');
            assert.equal(await frame.locator('#' + id).evaluate((n) => {
              const b = n.getBoundingClientRect();
              return document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2) === n;
            }), true);
          }
          assert.ok(await frame.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth) <= 1);
          await frame.locator('#refresh').click(); await ready(frame);
          await page.screenshot({ path: path.join(OUT, 'iframe-' + (theme || 'light') + '-' + width + '.png'), fullPage: true });
          await frame.locator('#settings-body').scrollIntoViewIfNeeded();
          await page.evaluate(() => window.scrollTo(0, 0));
          await frame.evaluate(() => {
            const card = document.getElementById('settings-body').parentElement;
            window.scrollTo(0, card.offsetTop - 140);
          });
          await page.screenshot({ path: path.join(OUT, 'settings-' + (theme || 'light') + '-' + width + '.png') });

          // Iframe modal screenshot for each theme and width
          await frame.locator('#setting-capture-bodies').check();
          await frame.locator('#settings-save').click();
          await frame.locator('#confirmation-dialog').waitFor({ state: 'visible' });
          const hostOverlay = await page.locator('.host-overlay').boundingBox();
          const modal = await frame.locator('#confirmation-dialog').boundingBox();
          assert.ok(modal.y >= 0 && modal.y + modal.height <= 900, 'modal fits visible host viewport');
          for (const id of ['confirmation-title', 'confirmation-desc', 'confirmation-cancel', 'confirmation-confirm']) {
            const box = await frame.locator('#' + id).boundingBox();
            assert.equal(box.x < hostOverlay.x + hostOverlay.width && box.x + box.width > hostOverlay.x &&
              box.y < hostOverlay.y + hostOverlay.height && box.y + box.height > hostOverlay.y, false, id + ' overlaps host overlay');
            assert.equal(await page.evaluate((b) => document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2).tagName, box), 'IFRAME');
          }
          await page.screenshot({ path: path.join(OUT, 'iframe-modal-' + (theme || 'light') + '-' + width + '.png') });
          await frame.locator('#confirmation-cancel').click();
          await frame.locator('#confirmation-dialog').waitFor({ state: 'hidden' });
          await revert(frame);
          await frame.waitForFunction(() => !document.getElementById('setting-capture-bodies').checked);
        }
      }
      await frame.locator('#setting-capture-bodies').check();
      await frame.locator('#price-add').click();
      await frame.locator('[data-price="model"]').last().fill('iframe-exact-model');
      await frame.locator('[data-price="input"]').last().fill('0.125');
      await save(page, frame); await saved(frame);
      assert.equal(server.counters.configWrites, 1);
      await page.reload();
      const reloaded = page.frame({ url: (u) => u.pathname === mock.RESOURCE_BASE + '/ui' });
      await connect(reloaded);
      assert.equal(await reloaded.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await reloaded.locator('[data-price="model"]').last().inputValue(), 'iframe-exact-model');
    });

    await run('390px standalone settings wrap, preserve exact MiB bounds and disk health', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth) <= 1);
      assert.match(await page.locator('#disk-status').textContent(), /32\.0 MB.*8\.0 MB.*成功 2 \/ 失败 1/);
      await page.locator('#setting-max-body-bytes').fill(String(1 / 1048576));
      await page.locator('#setting-max-body-storage-bytes').fill('8192');
      await page.locator('#setting-compact-min-bytes').fill('0.0625');
      await save(page, page); await saved(page);
      assert.equal(server.getConfig()['max-body-bytes'], 1);
      assert.equal(server.getConfig()['max-body-storage-bytes'], 8589934592);
      assert.equal(server.getConfig()['compact-min-bytes'], 65536);
      await page.locator('#settings-body').scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(OUT, 'standalone-settings-390.png') });
    }, { viewport: { width: 390, height: 900 } });

    await run('JS-disabled secret input is unnamed, outside forms, with no submit action', async ({ server, page }) => {
      await page.goto(server.resourceURL);
      assert.equal(await page.locator('form').count(), 0);
      assert.equal(await page.locator('#mgmt-key').getAttribute('name'), null);
      for (const id of ['connect', 'refresh', 'settings-save', 'settings-revert'])
        assert.equal(await page.locator('#' + id).getAttribute('type'), 'button');
      await page.locator('#mgmt-key').fill(mock.SECRET);
      await page.locator('#mgmt-key').press('Enter');
      assert.equal(server.counters.configWrites, 0);
    }, { javaScriptEnabled: false });

    await run('v8 resource preserves proxy prefix; plugin/config use v0 and credential names use v8', async ({ server, page }) => {
      const calls = [];
      page.on('request', (r) => { if (r.url().includes('/management/')) calls.push(r.url()); });
      await page.goto(server.origin + '/proxy/v8/resource/plugins/' + mock.PLUGIN_ID + '/ui');
      await connect(page);
      await page.locator('#setting-capture-bodies').check();
      await save(page, page); await saved(page);
      assert.ok(calls.length >= 8);
      assert.ok(calls.includes(server.origin + '/proxy/v8/management/credentials'));
      for (const url of calls) assert.ok(
        url === server.origin + '/proxy/v8/management/credentials' ||
        url.startsWith(server.origin + '/proxy/v0/management/plugins/' + mock.PLUGIN_ID + '/'));
    });
  } finally { await browser.close(); }
  fs.writeFileSync(path.join(OUT, 'settings-results.json'), JSON.stringify(results, null, 2));
  console.log(`${results.filter((r) => r.ok).length}/${results.length} passed`);
  if (results.some((r) => !r.ok)) process.exitCode = 1;
}

main().catch((e) => { console.error(e); process.exitCode = 1; });
