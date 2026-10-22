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
const measureContrast = require('./contrast.cjs');

const args = { chrome: process.env.CHROME_BIN || '/usr/bin/google-chrome-stable',
  reportDir: path.resolve(__dirname, '../../../../browser-report'), baseUrl: '' };
for (let i = 2; i < process.argv.length; i++) {
  const name = process.argv[i];
  if (name === '--report-dir') args.reportDir = path.resolve(process.argv[++i]);
  else if (name === '--base-url') args.baseUrl = process.argv[++i];
  else if (name === '--chrome') args.chrome = process.argv[++i];
  else if (name === '--account-display') args.accountDisplay = true;
  else if (name === '--ui-layout') args.uiLayout = true;
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
  const env = activeEnvironment?.page === page ? activeEnvironment : null;
  // Radix can have mounted the dialog before its first fade-in frame, even
  // with reduced motion. Require the final rendered state, not an empty scan.
  const settledDialog = () => env?.frame.waitForFunction(() => {
    const dialog = document.querySelector('[role="dialog"]');
    return !dialog || getComputedStyle(dialog).opacity === '1';
  });
  await settledDialog();
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
  if (env) {
    await settledDialog();
    const contrast = await measureContrast(env.frame);
    check(name + ': rendered text and control contrast',
      contrast.texts > 0 && contrast.controls > 0 && contrast.failures.length === 0, JSON.stringify(contrast));
  }
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

async function tabButtonsBox(frame, iframe) {
  // The <nav> spans the full content width; only its buttons are interactive.
  // Union the actual tab buttons (not the decorative full-width underline).
  const boxes = await frame.locator('.cpa-tab-item').evaluateAll(elements => elements.map(element => {
    const rect = element.getBoundingClientRect();
    return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
  }));
  assert(boxes.length > 0, 'tab buttons missing');
  assert(iframe, 'observer iframe rectangle missing');
  const left = Math.min(...boxes.map(box => box.x));
  const top = Math.min(...boxes.map(box => box.y));
  const right = Math.max(...boxes.map(box => box.x + box.width));
  const bottom = Math.max(...boxes.map(box => box.y + box.height));
  return { x: iframe.x + left, y: iframe.y + top, width: right - left, height: bottom - top };
}

async function verifySafeRects(env, name, dialog = false) {
  const { page, frame } = env;
  const toolbar = await page.locator('.header-actions.floating-actions').boundingBox();
  const iframe = await page.locator('iframe[title="Observer"]').boundingBox();
  const target = dialog
    ? await frame.locator('[role="dialog"]').boundingBox()
    : await tabButtonsBox(frame, iframe);
  assert(toolbar && iframe && target, 'official toolbar/iframe/child rectangle missing');
  // Playwright boundingBox() is in main-frame coordinates, including for
  // iframe descendants. Compare all rectangles in that same coordinate space.
  const intersects = target.x < toolbar.x + toolbar.width && target.x + target.width > toolbar.x &&
    target.y < toolbar.y + toolbar.height && target.y + target.height > toolbar.y;
  check(name + ': host actions do not intersect child controls', !intersects,
    JSON.stringify({ toolbar, iframe, target }));
  if (dialog) {
    check(name + ': dialog stays inside the iframe viewport', target.x >= iframe.x - 1 && target.y >= iframe.y - 1 &&
      target.x + target.width <= iframe.x + iframe.width + 1 && target.y + target.height <= iframe.y + iframe.height + 1,
    JSON.stringify({ iframe, target }));
    check(name + ': dialog stays below host actions',
      target.y >= toolbar.y + toolbar.height - 1, JSON.stringify({ toolbar, target }));
    const footer = await frame.locator('[role="dialog"] .cpa-modal-footer').boundingBox().catch(() => null);
    if (footer) {
      check(name + ': dialog footer stays inside the iframe viewport',
        footer.y + footer.height <= iframe.y + iframe.height + 1, JSON.stringify({ iframe, footer }));
    }
  }
  const overflow = await frame.evaluate(() => ({
    width: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth,
    local: document.querySelector('.cpa-app-layout')?.scrollWidth,
  }));
  check(name + ': viewport is not stretched by tables', overflow.document <= overflow.width + 1 &&
    overflow.body <= overflow.width + 1, JSON.stringify(overflow));
}

const rectsIntersect = (a, b) => a.x < b.x + b.width && a.x + a.width > b.x &&
  a.y < b.y + b.height && a.y + a.height > b.y;

async function hostLayout(env) {
  const { page, frame } = env;
  const [toolbar, iframe, content, tabs, root] = await Promise.all([
    page.locator('.header-actions.floating-actions').boundingBox(),
    page.locator('iframe[title="Observer"]').boundingBox(),
    frame.locator('#content').boundingBox(),
    frame.locator('.cpa-tabs').boundingBox(),
    frame.locator('#root').boundingBox(),
  ]);
  assert(toolbar && iframe && content && tabs && root, 'official toolbar/iframe/content/tabs rectangle missing');
  const scroll = await frame.evaluate(() => {
    const element = document.querySelector('#content');
    return {
      scrollTop: element.scrollTop,
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
      overflowY: getComputedStyle(element).overflowY,
      rootPaddingTop: getComputedStyle(document.querySelector('#root')).paddingTop,
      viewport: innerWidth,
    };
  });
  return { toolbar, iframe, content, tabs, root, scroll };
}

// The scroller viewport itself must start at or below the host action line.
// Content inside may be partially clipped while scrolled; #content is the clip.
async function verifyContentPane(env, name) {
  const layout = await hostLayout(env);
  check(name + ': content viewport stays below host actions',
    layout.content.y >= layout.toolbar.y + layout.toolbar.height - 1, JSON.stringify(layout));
  check(name + ': authenticated content is a clipped flex scroller',
    layout.scroll.overflowY === 'auto' && layout.scroll.rootPaddingTop === '0px' &&
    layout.scroll.clientHeight <= layout.scroll.scrollHeight, JSON.stringify(layout));
  if (layout.scroll.viewport > 768) {
    check(name + ': tabs stay at frame top on desktop',
      Math.abs(layout.tabs.y - layout.iframe.y) <= 1, JSON.stringify(layout));
  }
  return layout;
}

async function verifyControlClear(env, name, selector) {
  const { page, frame } = env;
  const toolbar = await page.locator('.header-actions.floating-actions').boundingBox();
  const control = await frame.locator(selector).first().boundingBox();
  assert(toolbar && control, selector + ' or host toolbar rectangle missing');
  check(name + ': first-row control does not intersect host actions',
    !rectsIntersect(control, toolbar), JSON.stringify({ toolbar, control }));
}

// Native checkbox glyph and text must share a vertical center at 100% and at
// Chrome's real 200% zoom; flex alignment is what this guards.
async function verifyRememberCheckbox(env, name) {
  const input = env.frame.locator('#remember-login');
  if (await input.count() === 0) return;
  const metrics = await env.frame.evaluate(() => {
    const box = document.querySelector('#remember-login');
    const label = box?.closest('label');
    const text = label?.querySelector('span');
    if (!box || !label || !text) return null;
    const a = box.getBoundingClientRect(), b = text.getBoundingClientRect(), l = label.getBoundingClientRect();
    return {
      centerDelta: (a.top + a.height / 2) - (b.top + b.height / 2),
      labelDisplay: getComputedStyle(label).display,
      inputInside: a.top >= l.top - 1 && a.bottom <= l.bottom + 1,
    };
  });
  check(name + ': remember checkbox is vertically centered',
    metrics && Math.abs(metrics.centerDelta) <= 1.5 && metrics.labelDisplay.includes('flex') && metrics.inputInside,
    JSON.stringify(metrics));
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
  const paneTop = await verifyContentPane(env, name);
  await verifyControlClear(env, name + ': requests poll/refresh', '.requests-header');
  const topGeometry = await frame.evaluate(() => {
    const root = document.querySelector('#root');
    const app = document.querySelector('.cpa-app-layout');
    const tabs = document.querySelector('.cpa-tabs');
    return {
      rootPaddingTop: getComputedStyle(root).paddingTop,
      rootTop: root.getBoundingClientRect().top,
      appTop: app.getBoundingClientRect().top,
      tabsTop: tabs.getBoundingClientRect().top,
      tabsBottom: tabs.getBoundingClientRect().bottom,
      viewport: innerWidth,
    };
  });
  check(name + ': embedded content has no global safe-top reservation',
    topGeometry.rootPaddingTop === '0px' && Math.abs(topGeometry.appTop - topGeometry.rootTop) <= 1,
    JSON.stringify(topGeometry));
  check(name + ': top tabs stay at the embedded frame top / mobile clearance',
    topGeometry.tabsTop - topGeometry.rootTop >= 0 && topGeometry.tabsTop - topGeometry.rootTop <= 80 &&
    topGeometry.tabsBottom <= topGeometry.rootTop + 140, JSON.stringify({ topGeometry, paneTop }));
  await requestsTab(frame).focus();
  check(name + ': top tabs accept keyboard focus', await frame.evaluate(() => {
    const active = document.activeElement;
    return Boolean(active && active.classList.contains('cpa-tab-item') && active.getBoundingClientRect().width > 0);
  }));
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
  if (server) await verifyAccountLayout(env, name);
  await frame.locator('#content').evaluate(el => { el.scrollTop = 0; });
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
  check(name + ': key table displays credentials rather than internal IDs',
    await frame.getByRole('columnheader', { name: '密钥', exact: true }).count() === 1 &&
    await frame.getByRole('columnheader', { name: '内部 ID', exact: true }).count() === 0);
  // Clickable names must not lose the old flex-item ellipsis and paint over
  // the secret column. Measure actual text/button/status bounds, not just CSS.
  const names = await frame.locator('table[aria-label="Key 列表"] tbody tr').evaluateAll(rows => rows.map(row => {
    const cell = row.cells[0];
    const button = cell.querySelector('button[title="查看 Key 详情"]');
    const label = button.querySelector('strong');
    const status = button.nextElementSibling;
    const bounds = cell.getBoundingClientRect();
    const inside = element => {
      const rect = element.getBoundingClientRect();
      return rect.left >= bounds.left && rect.right <= bounds.right;
    };
    return {
      inside: [button, label, status].every(inside),
      clipped: label.scrollWidth > label.clientWidth,
      ellipsis: getComputedStyle(label).textOverflow === 'ellipsis' &&
        getComputedStyle(label).overflow === 'hidden',
    };
  }));
  check(name + ': key names and status stay in their own column',
    names.length > 0 && names.every(item => item.inside), JSON.stringify(names));
  if (server) {
    check(name + ': long key names are visibly ellipsized',
      names.some(item => item.clipped && item.ellipsis), JSON.stringify(names));
    const managed = frame.getByRole('row').filter({ hasText: 'sk-key-alpha' });
    check(name + ': current client secret is readable and copyable in the list',
      await managed.count() === 1 && await managed.getByRole('button', { name: '复制密钥', exact: true }).count() === 1);
    const legacy = frame.getByRole('row').filter({ hasText: '未保存明文，请重新生成' });
    check(name + ': unavailable historical material is not replaced by an ID',
      await legacy.count() === 1 && await legacy.getByRole('button', { name: '复制密钥', exact: true }).count() === 0);
  }
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
  await verifyPricingControls(env, name);
  await frame.locator('#content').evaluate(el => { el.scrollTop = 0; });
  await verifySafeRects(env, name + '-settings');
  await verifyContentPane(env, name + ': settings top');
  await verifyControlClear(env, name + ': settings refresh', '.settings-toolbar');
  await shot(page, name + '-settings-top');
  if (name === '390-light') {
    await frame.locator('#settings-pricing-heading').scrollIntoViewIfNeeded();
    await verifyPricingLayout(env, name + ': pricing');
    check(name + ': zero price is shown as a normal amount, not unpriced',
      await frame.getByText('零价格', { exact: true }).count() > 0 &&
      await frame.getByText(/未定价|missing_rates/).count() === 0);
    await frame.locator('.settings-definition').first().scrollIntoViewIfNeeded();
    await shot(page, name + '-settings-pricing');
  }
  const capacity = await frame.locator('#settings-max-body-amount').boundingBox();
  check(name + ': capacity input has readable width', capacity && capacity.width >= (name.startsWith('390') ? 75 : 100), JSON.stringify(capacity));
  // Scroll the capture Select to a partially clipped position. Its bounding
  // rect may extend above the pane, but hit testing must show the clipped part
  // is not interactive and the visible part is.
  const clippedSelect = await frame.evaluate(() => {
    const content = document.querySelector('#content');
    const select = document.querySelector('#settings-capture-mode');
    const offset = select.getBoundingClientRect().top - content.getBoundingClientRect().top + content.scrollTop;
    content.scrollTop = Math.max(0, offset + 8);
    const pane = content.getBoundingClientRect(), rect = select.getBoundingClientRect();
    const x = rect.left + Math.min(12, rect.width / 2);
    const clippedHit = document.elementFromPoint(x, pane.top - 2);
    const visibleHit = document.elementFromPoint(x, Math.min(pane.top + 8, rect.bottom - 1));
    return {
      paneTop: pane.top, selectTop: rect.top, selectBottom: rect.bottom, scrollTop: content.scrollTop,
      clippedHitsSelect: Boolean(clippedHit && select.contains(clippedHit)),
      visibleHitsSelect: Boolean(visibleHit && select.contains(visibleHit)),
    };
  });
  check(name + ': capture Select is clipped by the content pane, not the host toolbar',
    clippedSelect.selectTop < clippedSelect.paneTop && clippedSelect.scrollTop > 0 &&
    !clippedSelect.clippedHitsSelect && clippedSelect.visibleHitsSelect, JSON.stringify(clippedSelect));
  await verifyContentPane(env, name + ': settings scrolled');
  await frame.locator('#content').evaluate(el => { el.scrollTop = el.scrollHeight; });
  await delay(200);
  const paneBottom = await verifyContentPane(env, name + ': settings bottom');
  const logoutBox = await frame.getByTestId('settings-logout').boundingBox();
  check(name + ': logout is reachable at the settings bottom',
    logoutBox && logoutBox.y >= paneBottom.content.y - 1 &&
    logoutBox.y + logoutBox.height <= paneBottom.content.y + paneBottom.content.height + 1 &&
    !rectsIntersect(logoutBox, paneBottom.toolbar), JSON.stringify({ logoutBox, paneBottom }));
  await shot(page, name + '-settings-bottom');
  check(name + ': XSS strings stayed text', await frame.evaluate(() => !window.__observerXss &&
    !document.querySelector('#content script,#content img[onerror],#content iframe')));
}

// Assert real clipping and full-name hover text, not just a CSS class.
async function verifyAccountEllipsis(frame, name) {
  const metrics = await frame.getByTitle(mock.LONG_ACCOUNT_NAME, { exact: true }).evaluate(element => {
    const style = getComputedStyle(element), rect = element.getBoundingClientRect();
    const cell = element.closest('td') || element.closest('.requests-detail-item');
    const bounds = cell.getBoundingClientRect();
    return {
      text: element.textContent, title: element.title, width: rect.width,
      clipped: element.scrollWidth > element.clientWidth,
      ellipsis: style.textOverflow === 'ellipsis' && style.overflow === 'hidden' && style.whiteSpace === 'nowrap',
      inside: rect.left >= bounds.left && rect.right <= bounds.right,
    };
  });
  check(name + ': long account name is ellipsized with its complete tooltip',
    metrics.text === mock.LONG_ACCOUNT_NAME && metrics.title === mock.LONG_ACCOUNT_NAME &&
    metrics.width > 0 && metrics.width <= 220 && metrics.clipped && metrics.ellipsis && metrics.inside,
    JSON.stringify(metrics));
}

async function verifyAccountLayout(env, name) {
  const { frame } = env;
  await verifyAccountEllipsis(frame, name + ' requests');
  await frame.getByRole('tab', { name: '账户分组', exact: true }).click();
  await frame.getByRole('columnheader', { name: '账户', exact: true }).waitFor();
  await verifyAccountEllipsis(frame, name + ' account groups');
  await frame.getByRole('tab', { name: '请求明细', exact: true }).click();
  await frame.getByRole('columnheader', { name: '提供商 / 账户', exact: true }).waitFor();
}

async function verifyPricingControls(env, name) {
  const { frame } = env;
  // Status and search stay on one row, including narrow and zoomed viewports.
  const previewStatus = await frame.getByRole('combobox', { name: '价格匹配状态', exact: true }).boundingBox();
  const previewSearch = await frame.getByRole('textbox', { name: '搜索可用模型', exact: true }).boundingBox();
  check(name + ': pricing status and search share one row',
    previewStatus && previewSearch &&
    Math.abs(previewStatus.y - previewSearch.y) <= 2 &&
    previewStatus.x + previewStatus.width <= previewSearch.x &&
    previewSearch.width > 0, JSON.stringify({ previewStatus, previewSearch }));
  const controls = await frame.evaluate(() => {
    const box = selector => {
      const rect = document.querySelector(selector).getBoundingClientRect();
      return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
    };
    return {
      row: box('.settings-pricing-controls'),
      autoLabel: box('label[for="settings-pricing-auto"]'),
      toggle: box('#settings-pricing-auto'),
      sourceLabel: box('label[for="settings-pricing-source"]'),
      source: box('#settings-pricing-source'),
    };
  });
  const center = box => box.y + box.height / 2;
  const gap = (left, right) => right.x - left.x - left.width;
  check(name + ': public-price toggle stays beside its label',
    Math.abs(center(controls.autoLabel) - center(controls.toggle)) <= 2 &&
    gap(controls.autoLabel, controls.toggle) >= 0 && gap(controls.autoLabel, controls.toggle) <= 16,
    JSON.stringify(controls));
  check(name + ': catalog source is bounded and shares its label row',
    controls.source.width > 0 && controls.source.width <= 240 &&
    Math.abs(center(controls.sourceLabel) - center(controls.source)) <= 2 &&
    gap(controls.sourceLabel, controls.source) >= 0 && gap(controls.sourceLabel, controls.source) <= 12,
    JSON.stringify(controls));
  if (controls.row.width >= 600) {
    check(name + ': desktop pricing controls share one compact row',
      Math.abs(center(controls.toggle) - center(controls.source)) <= 2, JSON.stringify(controls));
  }
}

async function verifyPricingLayout(env, name) {
  const { frame } = env;
  const layout = await frame.evaluate(() => {
    const content = document.querySelector('#content');
    const section = document.querySelector('[aria-labelledby="settings-pricing-heading"]');
    const definition = section?.querySelector('.settings-definition');
    const contentRect = content?.getBoundingClientRect();
    const definitionRect = definition?.getBoundingClientRect();
    return {
      viewport: innerWidth,
      document: document.documentElement.scrollWidth,
      body: document.body.scrollWidth,
      contentClient: content?.clientWidth,
      contentScroll: content?.scrollWidth,
      sectionClient: section?.clientWidth,
      sectionScroll: section?.scrollWidth,
      definitionClient: definition?.clientWidth,
      definitionScroll: definition?.scrollWidth,
      definitionRight: definitionRect?.right,
      contentRight: contentRect?.right,
    };
  });
  check(name + ': pricing section does not stretch the viewport',
    layout.document <= layout.viewport + 1 && layout.body <= layout.viewport + 1 &&
    layout.contentScroll <= layout.contentClient + 1 &&
    layout.sectionScroll <= layout.sectionClient + 1, JSON.stringify(layout));
  check(name + ': pricing definition stays inside the content pane',
    layout.definitionScroll <= layout.definitionClient + 1 &&
    layout.definitionRight <= layout.contentRight + 1, JSON.stringify(layout));
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
  // Management login persists by explicit preference; client credentials are
  // reloaded from the management API, never from browser persistent storage.
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
  await frame.getByRole('button', { name: '关闭并返回列表', exact: true }).click();
  await frame.getByRole('dialog').waitFor({ state: 'detached' });
  const row = frame.getByRole('row').filter({ hasText: 'browser-custom' });
  await row.getByText(custom, { exact: true }).waitFor();
  check('client secret remains available after closing the success modal',
    (await row.innerText()).includes(custom) && !await frame.getByRole('dialog').count());
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(frame.url()).origin });
  await row.getByRole('button', { name: '复制密钥', exact: true }).click();
  check('copy from the list writes the actual secret, not the ID',
    await frame.evaluate(() => navigator.clipboard.readText()) === custom);
  await shot(page, 'persistent-client-key-list');
  await frame.goto(frame.url());
  await frame.locator('.requests-table tbody tr').first().waitFor();
  await keyTab(frame).click();
  await row.getByText(custom, { exact: true }).waitFor();
  check('client secret survives a real document reload through the management API',
    (await row.innerText()).includes(custom));
  await row.getByRole('button', { name: '轮转', exact: true }).click();
  await frame.getByRole('radio', { name: '自定义新密钥', exact: true }).check();
  const next = 'browser-rotated-secret-exact-123456!';
  secrets.add(next);
  await frame.locator('#rotate-custom-secret').fill(next);
  await frame.getByRole('button', { name: '确认轮转', exact: true }).click();
  await frame.getByRole('dialog', { name: 'Key 轮转成功', exact: true }).waitFor();
  check('custom rotation retains stable identity', server.state.keys.find(k => k.label === 'browser-custom').id === created.id &&
    server.counters.keyRotates === 1);
  await frame.getByRole('button', { name: '关闭并返回列表', exact: true }).click();
  await row.getByText(next, { exact: true }).waitFor();
  check('rotation replaces the plaintext shown in the list',
    !(await row.innerText()).includes(custom) && server.state.secretValues.get(created.id) === next);
  const retainedHash = server.state.secrets.get(created.id);
  assert.match(retainedHash, /^[a-f0-9]{64}$/);
  await row.getByRole('button', { name: '禁用', exact: true }).click();
  await frame.getByRole('button', { name: '确认禁用', exact: true }).click();
  await row.getByRole('button', { name: '启用', exact: true }).waitFor();
  check('disable preserves the credential hash and displayed plaintext', !created.enabled &&
    server.state.secrets.get(created.id) === retainedHash && (await row.innerText()).includes(next));
  await row.getByRole('button', { name: '启用', exact: true }).click();
  await frame.getByRole('button', { name: '确认启用', exact: true }).click();
  await row.getByRole('button', { name: '禁用', exact: true }).waitFor();
  check('enable reuses the same credential without rotation', created.enabled && server.state.secrets.get(created.id) === retainedHash);
  await row.getByRole('button', { name: '删除', exact: true }).click();
  await frame.getByRole('button', { name: '确认永久删除', exact: true }).click();
  await row.waitFor({ state: 'detached' });
  check('terminal deletion removes authentication but preserves accounting identity',
    Boolean(created.deleted_at) && !created.enabled && !server.state.secrets.has(created.id) &&
    !server.state.secretValues.has(created.id) && server.state.keys.some(key => key.id === created.id));
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
  await frame.locator('#settings-pricing-heading').scrollIntoViewIfNeeded();
  await verifyPricingLayout(env, 'settings-pricing');
  const sourceSelect = frame.getByRole('combobox', { name: '价格目录来源', exact: true });
  check('pricing UI defaults to models.dev v2',
    (await sourceSelect.innerText()).includes('models.dev v2'));
  check('manual price keeps the complete model/provider identity',
    await frame.getByLabel('第 1 条手工价格模型 ID', { exact: true }).inputValue() === 'gpt-5-codex' &&
    await frame.getByLabel('第 1 条手工价格 Provider', { exact: true }).inputValue() === '');
  check('structured definition renders base rates and reasoning inheritance',
    await frame.getByLabel('第 1 条手工价格 输入价格', { exact: true }).inputValue() === '1.25' &&
    await frame.getByLabel('第 1 条手工价格 输出价格', { exact: true }).inputValue() === '10' &&
    (await frame.getByLabel('第 1 条手工价格 思考计价方式', { exact: true }).innerText()).includes('同输出'));
  const definitionParts = {
    overrides: await frame.locator('[data-testid="definition-override"]').count(),
    multipliers: await frame.locator('[data-testid="definition-multiplier"]').count(),
    ttlSummaries: await frame.locator('.settings-details > summary').filter({ hasText: '缓存写入 TTL 覆盖' }).count(),
  };
  check('structured definition renders overrides, multipliers and TTL section',
    definitionParts.overrides === 1 && definitionParts.multipliers === 1 &&
    definitionParts.ttlSummaries >= 1, JSON.stringify(definitionParts));
  check('preview marks manual and zero prices with rule counts, never unpriced',
    await frame.getByText('手工价格', { exact: true }).count() > 0 &&
    await frame.getByText('零价格', { exact: true }).count() > 0 &&
    await frame.getByText('2 条条件规则', { exact: true }).count() > 0 &&
    await frame.getByText(/未定价|missing_rates/).count() === 0);
  await shot(page, 'settings-pricing-host');
  await sourceSelect.click();
  await frame.getByRole('option', { name: 'models.dev v1', exact: true }).click();
  await frame.getByLabel('第 1 条手工价格 Provider', { exact: true }).fill('openai');
  await frame.getByTestId('settings-pricing-save').click();
  await frame.getByText('价格设置已保存', { exact: true }).waitFor();
  const savedPrice = server.state.settings.pricing.prices.find(price => price.model === 'gpt-5-codex');
  check('pricing save persists v1 source and exact provider pair',
    server.state.settings.pricing.source === 'models_dev_v1' && savedPrice?.provider === 'openai' &&
    savedPrice?.definition.base.input === 1_250_000);
  check('pricing save preserves confirmed capture settings', server.state.settings.capture_enabled && !server.state.settings.pricing.auto);
  await shot(page, 'settings-pricing-saved-host');
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
    activeEnvironment = { page: reopened, frame: reopened, diagnostics: [] };
    await shot(reopened, 'remember-reopened-browser');
    // Chrome's own setting changes real browser zoom, not CSS zoom or emulated
    // viewport scale. This preference is confined to the disposable profile.
    const preferences = await context.newPage();
    await preferences.goto('chrome://settings');
    await preferences.evaluate(() => new Promise(resolve => chrome.settingsPrivate.setDefaultZoom(2, resolve)));
    const zoom = await preferences.evaluate(() => new Promise(resolve => chrome.settingsPrivate.getDefaultZoom(resolve)));
    await preferences.close();
    const env = await openShell(null, origin, shell, { width: 1280, height: 900 }, 'light', server, context);
    activeEnvironment = env;
    await env.frame.locator('.requests-table tbody tr').first().waitFor();
    const actual = await env.page.evaluate(() => ({ ratio: devicePixelRatio, width: innerWidth, outer: outerWidth }));
    check('official CPA shell uses native 200% Chrome zoom', zoom === 2 && actual.ratio === 2 && actual.width === 640, JSON.stringify(actual));
    await readonlyPages(env, 'native-zoom-200');
    check('native 200% zoom has no console/page/CSP errors', env.diagnostics.length === 0, env.diagnostics.join('\n'));
    report.console_errors.push(...env.diagnostics);
    // The persistent profile is already logged in. Open the standalone login
    // once, clear only this mock origin's storage, and verify the native
    // checkbox at the same real 200% zoom used above.
    const zoomedLogin = await context.newPage();
    await zoomedLogin.goto(server.resourceURL);
    await zoomedLogin.evaluate(() => { localStorage.clear(); sessionStorage.clear(); });
    await zoomedLogin.reload();
    await zoomedLogin.locator('#login-key').waitFor();
    const zoomLogin = await zoomedLogin.evaluate(() => ({ ratio: devicePixelRatio, width: innerWidth }));
    check('native 200% zoom login uses physical zoom', zoomLogin.ratio === 2, JSON.stringify(zoomLogin));
    const loginEnv = { page: zoomedLogin, frame: zoomedLogin, diagnostics: [] };
    activeEnvironment = loginEnv;
    await verifyRememberCheckbox(loginEnv, 'native-zoom-200');
    await shot(zoomedLogin, 'native-zoom-200-login');
    await zoomedLogin.close();
  } finally {
    await context?.close();
    await fs.rm(profile, { recursive: true, force: true });
  }
}

async function main() {
  let browser, server;
  try {
    await fs.mkdir(path.join(args.reportDir, 'screenshots'), { recursive: true });
    if (args.accountDisplay) {
      server = await mock.startServer();
      browser = await chromium.launch({ executablePath: args.chrome, headless: true, args: ['--no-sandbox'] });
      for (const width of [390, 1440]) {
        const page = await browser.newPage({ viewport: { width, height: 900 } });
        await page.goto(server.resourceURL);
        await loginObserver(page);
        const rows = page.locator('.requests-table tbody tr');
        await rows.first().waitFor();
        assert.ok(await rows.getByText('生产账户', { exact: true }).count());
        assert.ok(await rows.getByText('账户名称不可用', { exact: true }).count());
        assert.ok(await rows.getByText('Key 名称不可用', { exact: true }).count());
        assert.doesNotMatch(await page.locator('.requests-table tbody').innerText(), /auth-openai-1|auth-anthropic-1|auth-mock-keyid|0123456789abcdef/);
        assert.equal(await page.locator('[title*="AuthIndex"]').count(), 0);
        await page.screenshot({ path: path.join(args.reportDir, `records-${width}.png`), fullPage: true });
        await rows.filter({ hasText: '生产账户' }).filter({ hasText: 'openai' }).first().getByRole('button', { name: '查看请求详情' }).click();
        const dialog = page.getByRole('dialog');
        await dialog.getByRole('heading', { name: '请求详情' }).waitFor();
        await page.waitForFunction(() => getComputedStyle(document.querySelector('[role="dialog"]')).opacity === '1');
        const technical = dialog.locator('details');
        assert.equal(await technical.getAttribute('open'), null);
        assert.ok(await dialog.getByText('生产账户', { exact: true }).count());
        await page.screenshot({ path: path.join(args.reportDir, `detail-${width}.png`), fullPage: true });
        await technical.locator('summary').click();
        assert.match(await technical.innerText(), /auth-openai-1/);
        await page.keyboard.press('Escape');
        await page.getByRole('tab', { name: '账户分组', exact: true }).click();
        await page.locator('.requests-account-name').first().waitFor();
        assert.equal(await page.locator('.requests-account-name').filter({ hasText: '生产账户' }).count(), 2);
        await page.screenshot({ path: path.join(args.reportDir, `groups-${width}.png`), fullPage: true });
        for (let index = 0; index < 2; index++) {
          await page.getByRole('combobox', { name: '账户', exact: true }).click();
          await page.getByRole('option', { name: '生产账户', exact: true }).nth(index).click();
          const response = page.waitForResponse(r => r.url().includes('/requests/view?') && r.url().includes('auth_index='));
          await page.getByRole('button', { name: '查询', exact: true }).click();
          const received = await response;
          assert.equal(new URL(received.url()).searchParams.get('auth_index'), index === 0 ? 'auth-anthropic-1' : 'auth-openai-1');
        }
        await page.screenshot({ path: path.join(args.reportDir, `filter-${width}.png`), fullPage: true });
        check(`${width}: account display, collapsed diagnostics and exact filter identity`, true);
        await page.close();
      }
      report.outcome = 'completed';
      return;
    }
    const shell = await ensureManagementHTML();
    report.management_shell_sha256 = shell.sha256;
    if (!real) server = await mock.startServer({ shellHTML: shell.content });
    const origin = real ? new URL(args.baseUrl).origin : server.origin;
    if (!real) report.ui_url = server.resourceURL;
    browser = await chromium.launch({ executablePath: args.chrome, headless: true, args: ['--no-sandbox'] });
    if (args.uiLayout) {
      assert(!real, '--ui-layout uses fake data only');
      for (const width of [320, 390, 640, 1440]) {
        server.resetState();
        const env = await openShell(browser, origin, shell, { width, height: 900 }, 'light', server);
        activeEnvironment = env;
        try {
          await loginObserver(env.frame);
          await verifyAccountLayout(env, `${width}`);
          const account = env.frame.getByTitle(mock.LONG_ACCOUNT_NAME, { exact: true });
          await account.scrollIntoViewIfNeeded();
          await shot(env.page, `${width}-request-account`);
          await account.locator('xpath=ancestor::tr').getByRole('button', { name: '查看请求详情' }).click();
          await env.frame.getByRole('dialog').getByTitle(mock.LONG_ACCOUNT_NAME, { exact: true }).waitFor();
          await verifyAccountEllipsis(env.frame.getByRole('dialog'), `${width} detail`);
          await closeModal(env.frame);
          await settingsTab(env.frame).click();
          await env.frame.locator('.settings-table tbody tr').first().waitFor();
          await env.frame.locator('#content').evaluate(content => {
            const section = document.querySelector('[aria-labelledby="settings-pricing-heading"]');
            content.scrollTop += section.getBoundingClientRect().top - content.getBoundingClientRect().top;
          });
          await verifyPricingControls(env, `${width}`);
          await verifyPricingLayout(env, `${width}`);
          await shot(env.page, `${width}-pricing-controls`);
          const help = env.frame.locator('.settings-pricing-help');
          assert.equal(await help.getAttribute('open'), null);
          await help.locator('summary').click();
          await help.locator('p').waitFor({ state: 'visible' });
          await shot(env.page, `${width}-pricing-help`);
          check(`${width}: no console/page/CSP errors`, env.diagnostics.length === 0, env.diagnostics.join('\n'));
        } finally { await env.context.close(); }
      }
      report.outcome = report.results.every(result => result.ok) ? 'completed' : 'failed';
      return;
    }
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
        await verifyRememberCheckbox(env, name);
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
