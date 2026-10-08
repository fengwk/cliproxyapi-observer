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
async function save(page, frame, accept = true) {
  const dialog = new Promise((resolve) => page.once('dialog', async (d) => {
    assert.match(d.message(), /提示词.*永久删除.*新请求/);
    await (accept ? d.accept() : d.dismiss());
    resolve();
  }));
  await frame.locator('#settings-save').click();
  await dialog;
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
      assert.equal(server.getConfig().prices[mock.XSS_MODEL]['cache-read'], 0.2);
      assert.equal(await page.locator('#settings-body img').count(), 0);
      await page.reload(); await connect(page);
      assert.equal(await page.locator('#setting-capture-bodies').isChecked(), true);
      assert.equal(await page.locator('[data-price="model"]').last().inputValue(), mock.XSS_MODEL);
      await page.locator('.price-row').last().getByRole('button', { name: '删除' }).click();
      await page.locator('.price-row').last().getByRole('button', { name: '删除' }).click();
      await save(page, page); await saved(page);
      assert.deepEqual(server.getConfig().prices, {});
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

    await run('Client bounds and duplicate/blank/negative prices never reach persistence', async ({ server, page }) => {
      await page.goto(server.resourceURL); await connect(page);
      for (const [id, value] of [
        ['setting-body-retention', '2'], ['setting-request-retention', '31'],
        ['setting-stats-retention-days', '3651'], ['setting-max-body-bytes', '65'],
        ['setting-max-body-storage-bytes', '0.5'], ['setting-compact-min-bytes', '0.01']
      ]) {
        const control = page.locator('#' + id), old = await control.inputValue();
        await control.fill(value); await page.locator('#settings-save').click();
        await page.locator('#settings-status').filter({ hasText: '未保存' }).waitFor();
        assert.equal(server.counters.validations, 0); assert.equal(server.counters.configWrites, 0);
        await control.fill(old);
      }
      await page.locator('#price-add').click();
      await page.locator('#settings-save').click();
      assert.equal(server.counters.validations, 0);
      const row = page.locator('.price-row').last();
      await row.locator('[data-price="model"]').fill(' gpt-5.1-codex ');
      await page.locator('#settings-save').click();
      assert.match(await page.locator('#settings-status').textContent(), /重复/);
      await row.locator('[data-price="model"]').fill('new-model');
      await row.locator('[data-price="input"]').fill('-1');
      await page.locator('#settings-save').click();
      assert.equal(server.counters.configWrites, 0);
      page.once('dialog', (d) => d.accept());
      await page.locator('#settings-revert').click();
      await page.locator('#settings-status').filter({ hasText: '已重新加载' }).waitFor();
      assert.equal(await page.locator('.price-row').count(), 1);
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
      page.once('dialog', (d) => d.accept());
      await page.locator('#settings-revert').click();
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

    await run('v8 resource preserves proxy prefix but all host native/config calls use v0', async ({ server, page }) => {
      const calls = [];
      page.on('request', (r) => { if (r.url().includes('/management/')) calls.push(r.url()); });
      await page.goto(server.origin + '/proxy/v8/resource/plugins/' + mock.PLUGIN_ID + '/ui');
      await connect(page);
      await page.locator('#setting-capture-bodies').check();
      await save(page, page); await saved(page);
      assert.ok(calls.length >= 8);
      for (const url of calls) assert.ok(url.startsWith(server.origin + '/proxy/v0/management/plugins/' + mock.PLUGIN_ID + '/'));
    });
  } finally { await browser.close(); }
  fs.writeFileSync(path.join(OUT, 'settings-results.json'), JSON.stringify(results, null, 2));
  console.log(`${results.filter((r) => r.ok).length}/${results.length} passed`);
  if (results.some((r) => !r.ok)) process.exitCode = 1;
}
main().catch((e) => { console.error(e); process.exitCode = 1; });
