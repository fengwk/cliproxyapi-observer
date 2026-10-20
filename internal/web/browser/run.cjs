#!/usr/bin/env node
'use strict';

// Behavioral acceptance against the verified, unmodified CPA management shell.
// The mock only replaces HTTP data; CPA owns the sidebar, toolbar and iframe.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const { chromium } = require('../../../web/node_modules/playwright');
const mock = require('./mock-server.cjs');
const { ensureManagementHTML } = require('./shell-helper.cjs');

const args = { chrome: process.env.CHROME_BIN || '/usr/bin/google-chrome-stable',
  reportDir: path.resolve(__dirname, '../../../../browser-report'), baseUrl: '' };
for (let i = 2; i < process.argv.length; i++) {
  const name = process.argv[i];
  if (name === '--report-dir') args.reportDir = path.resolve(process.argv[++i]);
  else if (name === '--base-url') args.baseUrl = process.argv[++i];
  else if (name === '--chrome') args.chrome = process.argv[++i];
  else throw new Error('Unknown argument: ' + name);
}
const real = Boolean(args.baseUrl);
if (real) {
  let url;
  try { url = new URL(args.baseUrl); } catch { throw new Error('--base-url must be an HTTP(S) origin or resource URL'); }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new Error('--base-url must not contain credentials, a query or a fragment');
  }
}
const managementKey = real ? process.env.OBSERVER_MANAGEMENT_KEY : mock.DEFAULT_MANAGEMENT_KEY;
if (!managementKey) throw new Error('OBSERVER_MANAGEMENT_KEY is required for --base-url');
const report = {
  mode: real ? 'realhost-readonly' : 'mock-shell', readonly: real, fetch_interception: true,
  ui_url: args.baseUrl, outcome: 'in-progress', fatal: '', results: [], screenshots: [],
  api_requests: [], blocked_requests: [], console_errors: [], expected_diagnostics: [],
};
const secrets = new Set([managementKey]);
let activeEnvironment;
const sanitize = text => {
  let output = String(text);
  for (const secret of secrets) output = output.split(secret).join('[credential]');
  return output;
};
function check(name, condition, detail = '') {
  report.results.push({ name, ok: Boolean(condition), detail: sanitize(detail) });
  console.log(`${condition ? 'PASS' : 'FAIL'} ${name}${condition ? '' : ': ' + sanitize(detail)}`);
}
async function step(name, action) {
  try { await action(); check(name, true); }
  catch (error) {
    if (activeEnvironment) {
      await shot(activeEnvironment.page, 'failure');
      await fs.writeFile(path.join(args.reportDir, 'failure.txt'), sanitize(
        await activeEnvironment.frame.locator('body').innerText() + '\n' + activeEnvironment.diagnostics.join('\n')));
    }
    check(name, false, error.stack || error);
    throw error;
  }
}
async function shot(page, name) {
  const file = path.join('screenshots', name + '.png');
  if (name.startsWith('native-zoom-200')) {
    // Playwright's CSS-sized default screenshot clips the physical surface at
    // native browser zoom. Capture the actual layout viewport in physical
    // coordinates; do not scale up an already clipped screenshot.
    const session = await page.context().newCDPSession(page);
    try {
      const metrics = await session.send('Page.getLayoutMetrics');
      const viewport = metrics.layoutViewport;
      const { data } = await session.send('Page.captureScreenshot', {
        format: 'png', fromSurface: true, captureBeyondViewport: false,
        clip: { x: viewport.pageX, y: viewport.pageY, width: viewport.clientWidth, height: viewport.clientHeight, scale: 1 },
      });
      const image = Buffer.from(data, 'base64');
      check(name + ': full physical zoom screenshot',
        image.readUInt32BE(16) === viewport.clientWidth && image.readUInt32BE(20) === viewport.clientHeight);
      await fs.writeFile(path.join(args.reportDir, file), image);
    } finally { await session.detach(); }
  } else {
    await page.screenshot({ path: path.join(args.reportDir, file), fullPage: true });
  }
  const info = await fs.stat(path.join(args.reportDir, file));
  report.screenshots.push({ file, bytes: info.size });
}
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const keyTab = frame => frame.getByRole('button', { name: 'Key 管理', exact: true });
const requestsTab = frame => frame.getByRole('button', { name: '请求日志', exact: true });
const settingsTab = frame => frame.getByRole('button', { name: '设置', exact: true });

function readonlyAllowed(request, origin) {
  const url = new URL(request.url());
  if (url.origin !== origin) return false;
  if (['GET', 'HEAD', 'OPTIONS'].includes(request.method())) return true;
  if (request.method() !== 'POST' || url.search) return false;
  if (url.pathname === mock.MANAGEMENT_BASE + '/models/token') return !request.postData();
  if (url.pathname !== mock.MANAGEMENT_BASE + '/prices/preview') return false;
  try {
    const body = request.postDataJSON();
    return Object.keys(body).length === 1 && Array.isArray(body.models) && body.models.length <= 4096;
  } catch { return false; }
}

async function openShell(browser, origin, shell, size, theme, server, suppliedContext) {
  const context = suppliedContext || await browser.newContext({
    viewport: size, colorScheme: theme === 'auto' || theme === 'dark' ? 'dark' : 'light',
    reducedMotion: 'reduce',
  });
  // Set only the host's documented theme/language preferences before startup.
  // Never seed/read host authentication storage or alter the host DOM.
  await context.addInitScript(({ theme }) => {
    localStorage.setItem('cli-proxy-theme', JSON.stringify({ state: { theme }, version: 0 }));
    localStorage.setItem('cli-proxy-language', JSON.stringify({ state: { language: 'zh-CN' }, version: 0 }));
    window.__paintThemes = [];
    const sample = () => {
      if (document.body && location.pathname.includes('/resource/plugins/')) {
        window.__paintThemes.push({
          theme: document.documentElement.getAttribute('data-theme'),
          color: getComputedStyle(document.body).backgroundColor,
        });
      }
      if (window.__paintThemes.length < 10) requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
  }, { theme });
  const page = await context.newPage();
  const diagnostics = [];
  page.on('pageerror', error => diagnostics.push('page: ' + error.message));
  page.on('console', message => {
    if (message.type() === 'error') diagnostics.push('console: ' + message.text());
  });
  page.on('response', response => {
    if (response.status() >= 400) diagnostics.push(`HTTP ${response.status()} ${new URL(response.url()).pathname}`);
  });
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url());
    if (real && url.origin === origin && url.pathname === '/management.html') {
      return route.fulfill({ status: 200, contentType: 'text/html', body: shell.content });
    }
    // Do not silently filter console errors. Negative test responses are
    // classified below by their exact path/status while the action is active.
    if (real && !readonlyAllowed(request, origin)) {
      report.blocked_requests.push({ method: request.method(), url: url.href });
      return route.abort('blockedbyclient');
    }
    if (url.pathname.includes('/management/') || url.pathname === '/v1/models') {
      report.api_requests.push({ method: request.method(), url: url.href });
    }
    return route.continue();
  });
  await page.goto(origin + '/management.html#/plugin-pages/' + mock.PLUGIN_ID + '/0');
  await page.locator('input[type="password"]').fill(managementKey);
  await page.getByRole('button', { name: /连接|登录|Connect|Login/i }).last().click();
  // Login may legitimately land on the dashboard. Navigate with the host's
  // router only after it has accepted the supplied fake management credential.
  await page.waitForFunction(() => !location.hash.includes('/login'), { timeout: 20_000 });
  await page.goto(origin + '/management.html#/plugin-pages/' + mock.PLUGIN_ID + '/0');
  try {
    await page.waitForSelector('iframe[title="Observer"]', { timeout: 20_000 });
  } catch (error) {
    await shot(page, 'shell-failure');
    await fs.writeFile(path.join(args.reportDir, 'shell-failure.txt'),
      sanitize(page.url() + '\n' + await page.locator('body').innerText() + '\n' + diagnostics.join('\n')));
    throw error;
  }
  const element = await page.$('iframe[title="Observer"]');
  const frame = await element.contentFrame();
  await frame.locator('#login-key, [data-testid="requests-filters"]').first().waitFor();
  return { context, page, frame, diagnostics, server, expectedDiagnosticIndices: new Set() };
}

async function verifyThemeAndLayout(env, name) {
  const { page, frame } = env;
  const host = await page.evaluate(() => ({
    theme: document.documentElement.getAttribute('data-theme') || 'light',
    bg: getComputedStyle(document.documentElement).getPropertyValue('--bg-secondary').trim(),
  }));
  await frame.waitForFunction(theme => document.documentElement.getAttribute('data-theme') === theme, host.theme);
  const child = await frame.evaluate(() => ({
    bg: getComputedStyle(document.documentElement).getPropertyValue('--bg-secondary').trim(),
    themeControls: [...document.querySelectorAll('.cpa-theme-control')].some(el => getComputedStyle(el).display !== 'none'),
    paints: window.__paintThemes,
  }));
  check(name + ': real parent tokens before login', child.bg === host.bg && !child.themeControls, JSON.stringify(child));
  if (host.theme === 'dark' && await frame.locator('#login-key').count()) {
    check(name + ': no white unauthenticated paint', child.paints.length > 0 &&
      child.paints.every(sample => sample.theme === 'dark' && sample.color !== 'rgb(255, 255, 255)'), JSON.stringify(child.paints));
  }
}

async function verifySafeRects(env, name, dialog = false) {
  const { page, frame } = env;
  const toolbar = await page.locator('.header-actions.floating-actions').boundingBox();
  const iframe = await page.locator('iframe[title="Observer"]').boundingBox();
  const target = await frame.locator(dialog ? '[role="dialog"]' : '.cpa-header').boundingBox();
  assert(toolbar && iframe && target, 'official toolbar/iframe/child rectangle missing');
  // Playwright boundingBox() is in main-frame coordinates, including for
  // iframe descendants. Compare all rectangles in that same coordinate space.
  check(name + ': host actions do not cover controls',
    target.y >= toolbar.y + toolbar.height - 1, JSON.stringify({ toolbar, iframe, target }));
  if (dialog) {
    check(name + ': dialog stays inside the iframe viewport', target.x >= iframe.x - 1 && target.y >= iframe.y - 1 &&
      target.x + target.width <= iframe.x + iframe.width + 1 && target.y + target.height <= iframe.y + iframe.height + 1,
    JSON.stringify({ iframe, target }));
  }
  const overflow = await frame.evaluate(() => ({
    width: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth,
    local: document.querySelector('.cpa-app-layout')?.scrollWidth,
  }));
  check(name + ': viewport is not stretched by tables', overflow.document <= overflow.width + 1 &&
    overflow.body <= overflow.width + 1, JSON.stringify(overflow));
}

async function loginObserver(frame) {
  await frame.locator('#login-key').fill(managementKey);
  await frame.getByRole('button', { name: '登录', exact: true }).click();
  await frame.getByTestId('requests-filters').waitFor();
  await frame.locator('.requests-table tbody tr').first().waitFor();
}
async function selectOption(frame, label, choice) {
  await frame.getByRole('combobox', { name: label, exact: true }).click();
  await frame.getByRole('option', { name: choice, exact: true }).click();
}
async function closeModal(frame) {
  await frame.getByRole('dialog').getByRole('button', { name: '关闭', exact: true }).first().click();
  await frame.getByRole('dialog').waitFor({ state: 'detached' });
}

async function readonlyPages(env, name) {
  const { page, frame, server } = env;
  await verifySafeRects(env, name);
  await shot(page, name + '-requests');
  const width = await frame.evaluate(() => {
    const page = document.querySelector('.requests-page').getBoundingClientRect();
    return { actual: page.width, viewport: innerWidth };
  });
  check(name + ': request page uses full available width', width.actual >= width.viewport - 70, JSON.stringify(width));
  const railToggle = page.locator('.sidebar-toggle-floating');
  if (await railToggle.isVisible()) {
    await railToggle.click();
    await delay(300);
    await verifySafeRects(env, name + '-collapsed-sidebar');
    const available = await frame.evaluate(() => ({
      page: document.querySelector('.requests-page').getBoundingClientRect().width, viewport: innerWidth,
    }));
    check(name + ': full width follows collapsed sidebar', available.page >= available.viewport - 70, JSON.stringify(available));
    await railToggle.click();
    await delay(300);
  }
  await frame.getByTestId('requests-table-section').scrollIntoViewIfNeeded();
  await shot(page, name + '-request-table');
  await frame.locator('.cpa-app-layout').evaluate(el => { el.scrollTop = 0; });
  // Details must be fetched on demand, safely rendered as text and never turn
  // malicious model/header/JSON strings into DOM or script.
  if (server) check(name + ': details initially lazy', server.counters.detailReads === 0);
  const detailButton = server
    ? frame.locator('.requests-model-link').filter({ hasText: 'gpt-5-codex' }).first()
    : frame.locator('.requests-model-link').first();
  await detailButton.click();
  await frame.getByRole('dialog').waitFor();
  await frame.getByRole('tab', { name: '请求头', exact: true }).click();
  await frame.locator('.requests-headers-table').waitFor();
  const headers = await frame.locator('.requests-headers-table').innerText();
  // The native capture may remove sensitive headers altogether rather than
  // retain a placeholder. Both are safe; fake-fixture headers exercise rendering.
  check(name + ': headers redacted', server ? headers.includes('[redacted]') :
    ![managementKey, 'fake-client-key', 'fake-upstream-key'].some(secret => headers.includes(secret)));
  await frame.getByRole('tab', { name: '请求体 (JSON)', exact: true }).click();
  await frame.locator('.requests-json-body').waitFor();
  const body = await frame.locator('.requests-json-body').innerText();
  check(name + ': detail tab selection agrees with the displayed pane',
    await frame.getByRole('tab', { name: '请求体 (JSON)', exact: true }).getAttribute('aria-selected') === 'true');
  check(name + ': formatted JSON', body.includes('\n  "') && JSON.parse(body));
  await delay(200); // allow active-tab border transition to settle for visual review
  await verifySafeRects(env, name + '-detail', true);
  await shot(page, name + '-request-detail-host');
  await closeModal(frame);
  if (server) {
    await frame.locator('.requests-model-link').filter({ hasText: mock.XSS.model }).click();
    await frame.getByRole('tab', { name: '请求头', exact: true }).click();
    await frame.locator('.requests-headers-table').waitFor();
    check(name + ': malicious header remains literal', (await frame.locator('.requests-headers-table').innerText()).includes(mock.XSS.headerValue));
    await frame.getByRole('tab', { name: '请求体 (JSON)', exact: true }).click();
    await frame.locator('.requests-json-body').waitFor();
    check(name + ': malicious JSON remains literal', (await frame.locator('.requests-json-body').innerText()).includes('__observerXss'));
    check(name + ': malicious detail cannot create DOM', await frame.evaluate(() =>
      !window.__observerXss && !document.querySelector('.cpa-modal-body script,.cpa-modal-body img,.cpa-modal-body iframe')));
    await closeModal(frame);
  }
  await keyTab(frame).click();
  await frame.locator('table[aria-label="Key 列表"] tbody tr').first().waitFor();
  await verifySafeRects(env, name + '-keys');
  await shot(page, name + '-keys');
  // Opening/closing the editor is read-only and verifies mobile portal sizing.
  await frame.getByRole('button', { name: '+ 新建 Key', exact: true }).click();
  await verifySafeRects(env, name + '-key-editor', true);
  await shot(page, name + '-key-editor');
  await closeModal(frame);
  await settingsTab(frame).click();
  await frame.getByTestId('observer-settings').waitFor();
  await frame.locator('.settings-table tbody tr').first().waitFor();
  await verifySafeRects(env, name + '-settings');
  const capacity = await frame.locator('#settings-max-body-amount').boundingBox();
  check(name + ': capacity input has readable width', capacity && capacity.width >= (name.startsWith('390') ? 75 : 100), JSON.stringify(capacity));
  await frame.locator('.cpa-app-layout').evaluate(el => { el.scrollTop = 400; });
  // Scroll safe viewport retains the top gutter; page controls cannot slide
  // underneath host actions even though the child document is scrollable.
  const root = await frame.locator('#root').boundingBox();
  const scroller = await frame.locator('.cpa-app-layout').boundingBox();
  check(name + ': safe viewport survives scrolling', scroller.y > root.y + 20);
  await shot(page, name + '-settings');
  check(name + ': XSS strings stayed text', await frame.evaluate(() => !window.__observerXss &&
    !document.querySelector('#content script,#content img[onerror],#content iframe')));
}

async function interactions(env) {
  const { page, frame, server } = env;
  await requestsTab(frame).click();
  await frame.locator('.requests-table tbody tr').first().waitFor();
  const viewReads = () => server.calls.filter(call => call.path === mock.MANAGEMENT_BASE + '/requests/view').length;
  const readsBefore = viewReads();
  await frame.locator('.requests-table tbody tr').first().evaluate(el => { el.dataset.stableNode = 'retained'; });
  await frame.locator('.table-wrap').evaluate(el => { el.scrollLeft = 100; });
  await frame.getByRole('button', { name: '刷新', exact: true }).focus();
  const stableLayout = () => frame.evaluate(() => {
    const boxes = ['.requests-header', '[data-testid="requests-filters"]', '.requests-metrics-section',
      '.requests-trend-container', '.requests-table thead'].flatMap(selector => {
        const rect = document.querySelector(selector).getBoundingClientRect();
        return [rect.x, rect.y, rect.width, rect.height];
      });
    return { boxes, scroll: document.querySelector('.table-wrap').scrollLeft,
      focused: document.activeElement?.textContent };
  });
  const beforeLayout = await stableLayout();
  let maxDrift = 0, stableFocusAndScroll = true;
  // Twelve actual five-second timer cycles, not artificial manual refreshes.
  for (let round = 0; round < 12; round++) {
    await delay(5200);
    const after = await stableLayout();
    maxDrift = Math.max(maxDrift, ...after.boxes.map((value, index) => Math.abs(value - beforeLayout.boxes[index])));
    stableFocusAndScroll &&= after.scroll === beforeLayout.scroll && after.focused === beforeLayout.focused;
  }
  check('12 request-only timer cycles retain rows, geometry, focus and local scrolling',
    viewReads() >= readsBefore + 12 && maxDrift <= 1 && stableFocusAndScroll &&
    await frame.locator('.requests-table tbody tr').first().getAttribute('data-stable-node') === 'retained',
    JSON.stringify({ cycles: viewReads() - readsBefore, maxDrift, stableFocusAndScroll }));
  // Management login may persist only by explicit Observer preference; client
  // credentials are still never retained. Verify a real document reload.
  await frame.goto(frame.url());
  await frame.locator('.requests-table tbody tr').first().waitFor();
  check('remembered management login survives reload', await frame.locator('#login-key').count() === 0);
  await keyTab(frame).click();
  await frame.getByRole('button', { name: '+ 新建 Key', exact: true }).click();
  await frame.getByLabel('Key 名称', { exact: true }).fill('browser-custom');
  await frame.getByRole('radio', { name: '自定义密钥', exact: true }).check();
  const custom = 'browser-custom-secret-exact-123456!';
  secrets.add(custom);
  await frame.locator('#create-custom-secret').fill(' invalid-secret');
  await frame.getByRole('button', { name: '确认创建', exact: true }).click();
  await frame.getByText('自定义密钥只能包含可见 ASCII 字符且不能有空格或换行', { exact: true }).waitFor();
  await frame.locator('#create-custom-secret').fill(custom);
  await frame.getByRole('switch', { name: '启用5小时限额', exact: true }).click();
  await frame.getByRole('button', { name: '确认创建', exact: true }).click();
  await frame.getByText(/金额不能为空且必须大于 0/).waitFor();
  await frame.getByRole('textbox', { name: '5小时额度', exact: true }).fill('0.000001');
  await frame.getByRole('switch', { name: '仅允许指定模型', exact: true }).click();
  await frame.getByRole('button', { name: '确认创建', exact: true }).click();
  await frame.getByText('开启限制模型访问时，必须至少选择或添加一个模型', { exact: true }).waitFor();
  await frame.getByLabel('手动添加精确模型 ID', { exact: true }).fill('exact-model-v1');
  await frame.getByRole('button', { name: '添加', exact: true }).click();
  await verifySafeRects(env, 'custom-editor', true);
  await shot(page, 'custom-key-editor-host');
  await frame.getByRole('button', { name: '确认创建', exact: true }).click();
  await frame.getByRole('dialog', { name: 'Key 创建成功', exact: true }).waitFor();
  assert.equal(await frame.locator('.cpa-secret-box').innerText().then(t => t.includes(custom)), true);
  const created = server.state.keys.find(key => key.label === 'browser-custom');
  check('custom credential created with exact quota/model', created && created.limits.five_hour === 1 &&
    created.allowed_models.join() === 'exact-model-v1');
  const storage = await frame.evaluate(() => JSON.stringify({ ...sessionStorage, ...localStorage }));
  check('client secret not persisted in browser storage', !storage.includes(custom));
  await frame.getByRole('button', { name: '我已安全保存密钥，关闭', exact: true }).click();
  check('one-time client secret cleared on close', !(await frame.locator('body').innerText()).includes(custom));
  const row = frame.getByRole('row').filter({ hasText: 'browser-custom' });
  await row.getByRole('button', { name: '轮转', exact: true }).click();
  await frame.getByRole('radio', { name: '自定义新密钥', exact: true }).check();
  const next = 'browser-rotated-secret-exact-123456!';
  secrets.add(next);
  await frame.locator('#rotate-custom-secret').fill(next);
  await frame.getByRole('button', { name: '确认轮转', exact: true }).click();
  await frame.getByRole('dialog', { name: 'Key 轮转成功', exact: true }).waitFor();
  check('custom rotation retains stable identity', server.state.keys.find(k => k.label === 'browser-custom').id === created.id &&
    server.counters.keyRotates === 1);
  await frame.getByRole('button', { name: '我已安全保存密钥，关闭', exact: true }).click();
  const retainedHash = server.state.secrets.get(created.id);
  assert.match(retainedHash, /^[a-f0-9]{64}$/);
  await row.getByRole('button', { name: '禁用', exact: true }).click();
  await frame.getByRole('button', { name: '确认禁用', exact: true }).click();
  await row.getByRole('button', { name: '启用', exact: true }).waitFor();
  check('disable preserves the credential hash', !created.enabled && server.state.secrets.get(created.id) === retainedHash);
  await row.getByRole('button', { name: '启用', exact: true }).click();
  await frame.getByRole('button', { name: '确认启用', exact: true }).click();
  await row.getByRole('button', { name: '禁用', exact: true }).waitFor();
  check('enable reuses the same credential without rotation', created.enabled && server.state.secrets.get(created.id) === retainedHash);
  await row.getByRole('button', { name: '删除', exact: true }).click();
  await frame.getByRole('button', { name: '确认永久删除', exact: true }).click();
  await row.waitFor({ state: 'detached' });
  check('terminal deletion removes authentication but preserves accounting identity',
    Boolean(created.deleted_at) && !created.enabled && !server.state.secrets.has(created.id) &&
    server.state.keys.some(key => key.id === created.id));
  // Editing the name must not truncate an unchanged nanosecond expiry.
  const nano = frame.getByRole('row').filter({ hasText: '纳秒到期 Key' });
  await nano.getByRole('button', { name: '编辑', exact: true }).click();
  await frame.getByLabel('Key 名称', { exact: true }).fill('纳秒到期 Key updated');
  const keyReads = () => server.calls.filter(call => call.path === mock.MANAGEMENT_BASE + '/keys' && call.method === 'GET').length;
  const readsDuringDraft = keyReads();
  await delay(5500);
  check('Key editor does not trigger periodic list reloads', keyReads() === readsDuringDraft);
  assert.equal(await frame.getByLabel('Key 名称', { exact: true }).inputValue(), '纳秒到期 Key updated');
  await frame.getByRole('button', { name: '保存修改', exact: true }).click();
  await frame.getByRole('dialog').waitFor({ state: 'detached' });
  check('nanosecond expiry survives unrelated edit', server.state.keys.find(k => k.id === mock.NANO_KEY_ID).expires_at === mock.NANO_EXPIRY);
  await settingsTab(frame).click();
  await frame.getByTestId('observer-settings').waitFor();
  await frame.locator('.settings-table tbody tr').first().waitFor();
  const originalPricing = JSON.stringify(server.state.settings.pricing);
  await frame.getByRole('switch', { name: '自动使用 models.dev 公共价格目录', exact: true }).click();
  await frame.getByRole('switch', { name: '保存请求内容', exact: true }).click();
  await frame.getByTestId('settings-general-save').click();
  await frame.getByText('请求记录与存储限制设置已保存', { exact: true }).waitFor();
  check('general save excludes unsaved pricing draft', JSON.stringify(server.state.settings.pricing) === originalPricing);
  assert.equal(await frame.getByRole('switch', { name: '自动使用 models.dev 公共价格目录', exact: true }).getAttribute('aria-checked'), 'false');
  await frame.getByTestId('settings-pricing-save').click();
  await frame.getByText('价格设置已保存', { exact: true }).waitFor();
  check('pricing save preserves confirmed capture settings', server.state.settings.capture_enabled && !server.state.settings.pricing.auto);
  server.state.pricesError = 'test catalog offline';
  const sync = server.state.pricesLastSync;
  await frame.getByTestId('prices-refresh').click();
  await frame.getByText(/价格目录刷新失败，已保留有效缓存/).waitFor();
  check('refresh failure keeps cache and never claims success', server.state.pricesLastSync === sync);
  await shot(page, 'settings-refresh-failure-host');
  // Changing the host theme through its actual UI exercises the observer bridge.
  await page.locator('.theme-menu > button').click();
  await page.getByRole('menuitemradio', { name: /暗色|Dark/ }).click();
  await verifyThemeAndLayout(env, 'live-theme-switch');
  await keyTab(frame).click();
  await frame.locator('table[aria-label="Key 列表"] tbody tr').first().waitFor();
  const readsWithoutDraft = keyReads();
  await delay(5500);
  check('Key page has no periodic reloads when clean either', keyReads() === readsWithoutDraft);

  // A deliberately injected, single 401 is the only accepted negative HTTP
  // diagnostic. The exact response and its browser console message remain in
  // report.expected_diagnostics; no generic error suppression is permitted.
  const start = env.diagnostics.length;
  let injected = false;
  await env.context.route('**/v0/management/plugins/cliproxyapi-observer/keys', async route => {
    if (!injected && route.request().method() === 'GET' && route.request().frame() === frame) {
      injected = true;
      return route.fulfill({ status: 401, contentType: 'application/json',
        body: JSON.stringify({ error: { code: 'unauthorized', message: 'test session expired' } }) });
    }
    return route.continue();
  });
  await frame.getByRole('button', { name: '刷新', exact: true }).click();
  await frame.locator('#login-key').waitFor();
  assert(injected);
  const readsAfter401 = server.calls.length;
  await delay(5500);
  check('401 returns to login and stops polling', server.calls.length === readsAfter401 &&
    !await frame.evaluate(key => JSON.stringify({ ...sessionStorage, ...localStorage }).includes(key), managementKey));
  const negative = env.diagnostics.slice(start);
  const expectedHTTP = 'HTTP 401 ' + mock.MANAGEMENT_BASE + '/keys';
  assert.equal(negative.filter(message => message === expectedHTTP).length, 1);
  assert(negative.every(message => message === expectedHTTP ||
    /^console: Failed to load resource: the server responded with a status of 401 \((Unauthorized)?\)$/.test(message)),
  'Unexpected error during the single intentional 401: ' + negative.join('\n'));
  negative.forEach((message, offset) => {
    env.expectedDiagnosticIndices.add(start + offset);
    report.expected_diagnostics.push({ action: 'deliberate-session-expiry', message });
  });
}

async function persistentLoginAndNativeZoom(origin, shell, server) {
  // Use a disposable Chrome profile owned only by this test. Never inspect a
  // host/user profile, credential file or management-center auth storage.
  const profile = await fs.mkdtemp(path.join(os.tmpdir(), 'observer-browser-profile-'));
  let context;
  const launch = () => chromium.launchPersistentContext(profile, {
    executablePath: args.chrome, headless: true, viewport: { width: 1280, height: 900 },
    reducedMotion: 'reduce', args: ['--no-sandbox'],
  });
  try {
    server.resetState();
    context = await launch();
    const first = await context.newPage();
    await first.goto(server.resourceURL);
    await loginObserver(first);
    await context.close();
    context = await launch(); // a new browser process, not storageState injection
    const reopened = await context.newPage();
    await reopened.goto(server.resourceURL);
    await reopened.locator('.requests-table tbody tr').first().waitFor();
    check('remembered login survives complete Chrome profile restart', await reopened.locator('#login-key').count() === 0);
    await shot(reopened, 'remember-reopened-browser');
    // Chrome's own setting changes real browser zoom, not CSS zoom or emulated
    // viewport scale. This preference is confined to the disposable profile.
    const preferences = await context.newPage();
    await preferences.goto('chrome://settings');
    await preferences.evaluate(() => new Promise(resolve => chrome.settingsPrivate.setDefaultZoom(2, resolve)));
    const zoom = await preferences.evaluate(() => new Promise(resolve => chrome.settingsPrivate.getDefaultZoom(resolve)));
    await preferences.close();
    const env = await openShell(null, origin, shell, { width: 1280, height: 900 }, 'light', server, context);
    await env.frame.locator('.requests-table tbody tr').first().waitFor();
    const actual = await env.page.evaluate(() => ({ ratio: devicePixelRatio, width: innerWidth, outer: outerWidth }));
    check('official CPA shell uses native 200% Chrome zoom', zoom === 2 && actual.ratio === 2 && actual.width === 640, JSON.stringify(actual));
    await readonlyPages(env, 'native-zoom-200');
    check('native 200% zoom has no console/page/CSP errors', env.diagnostics.length === 0, env.diagnostics.join('\n'));
    report.console_errors.push(...env.diagnostics);
  } finally {
    await context?.close();
    await fs.rm(profile, { recursive: true, force: true });
  }
}

async function main() {
  let browser, server;
  try {
    await fs.mkdir(path.join(args.reportDir, 'screenshots'), { recursive: true });
    const shell = await ensureManagementHTML();
    report.management_shell_sha256 = shell.sha256;
    if (!real) server = await mock.startServer({ shellHTML: shell.content });
    const origin = real ? new URL(args.baseUrl).origin : server.origin;
    if (!real) report.ui_url = server.resourceURL;
    browser = await chromium.launch({ executablePath: args.chrome, headless: true, args: ['--no-sandbox'] });
    const cases = real
      ? [{ width: 1280, height: 900, theme: 'dark' }, { width: 390, height: 844, theme: 'white' }]
      : [1280, 390].flatMap(width => ['light', 'white', 'dark', 'auto'].map(theme => ({ width, height: width === 390 ? 844 : 900, theme })))
        .concat([640, 768, 1440, 1920, 2560].map(width => ({ width, height: 900, theme: 'light' })));
    for (const item of cases) {
      if (server) server.resetState();
      const name = `${real ? 'realhost-' : ''}${item.width}-${item.theme}`;
      const env = await openShell(browser, origin, shell, { width: item.width, height: item.height }, item.theme, server);
      activeEnvironment = env;
      try {
        await step(name + ': login theme', () => verifyThemeAndLayout(env, name));
        await shot(env.page, name + '-login');
        await loginObserver(env.frame);
        await step(name + ': readonly pages', () => readonlyPages(env, name));
        if (!real && item.width === 1280 && item.theme === 'light') {
          await step('full management interactions', () => interactions(env));
        }
        if (real && item.width === 1280) await shot(env.page, 'realhost-smoke');
        const unexpected = env.diagnostics.filter((_, index) => !env.expectedDiagnosticIndices.has(index));
        check(name + ': no unexpected console/page/CSP errors', unexpected.length === 0, unexpected.join('\n'));
        report.console_errors.push(...unexpected);
      } finally { await env.context.close(); }
    }
    if (!real) await persistentLoginAndNativeZoom(origin, shell, server);
    report.outcome = report.results.every(result => result.ok) ? 'completed' : 'failed';
  } catch (error) {
    report.fatal = sanitize(error.stack || error);
    report.outcome = 'failed';
  } finally {
    await browser?.close();
    await server?.close();
    report.summary = {
      total: report.results.length, passed: report.results.filter(result => result.ok).length,
      failed: report.results.filter(result => !result.ok).length + (report.fatal ? 1 : 0),
    };
    await fs.mkdir(args.reportDir, { recursive: true });
    await fs.writeFile(path.join(args.reportDir, 'report.json'), sanitize(JSON.stringify(report, null, 2)));
    await fs.writeFile(path.join(args.reportDir, 'summary.txt'),
      `${report.outcome}: ${report.summary.passed}/${report.summary.total} passed\n${report.fatal}\n`);
    if (report.outcome !== 'completed') {
      console.error(report.fatal);
      process.exitCode = 1;
    }
  }
}
main();
