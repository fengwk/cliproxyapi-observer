'use strict';

/*
 * Node 单元测试：`node --test internal/web/ui.test.cjs`
 *
 * 覆盖目标：
 *  - ui.js 只导出 settings.js 需要的纯助手：micro / usd / base / query。
 *  - 金额解析严格：六位小数、非负、精确 micro-USD 且不超过安全上限，绝不静默截断。
 *  - 反向代理前缀下的 UI → management 路由推导。
 *  - 日期筛选统一转换为 UTC 日边界，并固定分页参数。
 *  - 静态安全契约：文本只走 textContent/createTextNode，鉴权与取消控件不做隐式写入。
 */

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ui = require('./ui.js');
const readAsset = (name) => fs.readFileSync(path.join(__dirname, name), 'utf8');

const html = readAsset('ui.html');
const css = readAsset('ui.css');
const uiSource = readAsset('ui.js');
const settingsSource = readAsset('settings.js');
const servedUISource = settingsSource + '\n' + uiSource;

// ---------------------------------------------------------------------------
// 共享助手导出
// ---------------------------------------------------------------------------

test('ui.js exports exactly micro, usd, base and query for settings.js', () => {
  assert.deepEqual(Object.keys(ui).sort(), ['base', 'micro', 'query', 'usd']);
  for (const name of ['micro', 'usd', 'base', 'query']) {
    assert.equal(typeof ui[name], 'function', name + ' should be exported as a function');
  }
});

// ---------------------------------------------------------------------------
// 严格金额换算
// ---------------------------------------------------------------------------

test('micro converts USD to exact integer micro-USD, including six-digit fractions', () => {
  const cases = [
    ['0', 0],
    ['1', 1_000_000],
    ['1.25', 1_250_000],
    ['0.000001', 1],
    ['12.345678', 12_345_678],
    [' 12.345678 ', 12_345_678]
  ];
  for (const [input, want] of cases) {
    const got = ui.micro(input);
    assert.equal(got, want, input);
    assert.equal(Number.isSafeInteger(got), true, input + ' must stay a safe integer');
  }
});

test('micro accepts the exact safe upper bound and rejects one micro above it', () => {
  const max = ui.micro('9000000000');
  assert.equal(max, 9_000_000_000_000_000);
  assert.equal(Number.isSafeInteger(max), true);
  assert.equal(ui.micro('9000000000.000000'), max);

  assert.throws(() => ui.micro('9000000000.000001'), /金额超过安全上限/);
  assert.throws(() => ui.micro('9000000001'), /金额超过安全上限/);
});

test('micro rejects malformed, negative and over-precise input without clamping', () => {
  for (const input of ['', ' ', '-1', '-0.000001', '+1', '.5', '1.', '1.0000001', '1e3', 'NaN', 'Infinity', '0x10', null, undefined, {}]) {
    assert.throws(() => ui.micro(input), /请输入非负美元金额，最多六位小数/, String(input));
  }
});

test('usd renders integer micro-USD without losing fractional micros', () => {
  const cases = [
    [0, '0'],
    [1, '0.000001'],
    [999_999, '0.999999'],
    [1_000_000, '1'],
    [1_000_001, '1.000001'],
    [1_000_100, '1.0001'],
    [1_250_000, '1.25'],
    [1_500_000, '1.5'],
    [9_000_000_000_000_000, '9000000000']
  ];
  for (const [input, want] of cases) {
    assert.equal(ui.usd(input), want);
  }
  assert.equal(ui.usd(null), '0');
  assert.equal(ui.usd(undefined), '0');
  assert.equal(ui.usd(ui.micro('12.345678')), '12.345678');
});

// ---------------------------------------------------------------------------
// 路由与查询
// ---------------------------------------------------------------------------

test('base keeps reverse-proxy prefixes and maps resource URLs to management routes', () => {
  const cases = [
    ['/ui', ''],
    ['/ui/', ''],
    ['/ui.html', ''],
    ['/v0/resource/plugins/observer/ui', '/v0/management/plugins/observer'],
    ['/v0/resource/plugins/observer/ui/', '/v0/management/plugins/observer'],
    ['/v0/resource/plugins/observer/ui.html', '/v0/management/plugins/observer'],
    ['/proxy/v0/resource/plugins/observer/ui/', '/proxy/v0/management/plugins/observer'],
    ['/v8/management/plugins/observer/ui.html', '/v8/management/plugins/observer'],
    ['/proxy/v0/management/plugins/observer/ui/', '/proxy/v0/management/plugins/observer']
  ];
  for (const [input, want] of cases) {
    assert.equal(ui.base(input), want, input);
  }
});

test('query pins pagination and converts date-only filters to UTC day boundaries', () => {
  assert.equal(ui.query({}), '?page=1&page_size=25');
  assert.equal(
    ui.query({ from: '2026-10-18', to: '2026-10-19' }, 2),
    '?page=2&page_size=25&from=2026-10-18T00%3A00%3A00Z&to=2026-10-19T00%3A00%3A00Z'
  );
});

test('query drops empty filters and URL-encodes remaining values', () => {
  assert.equal(
    ui.query({ from: '', to: null, key_id: '', model: undefined, provider: 0, auth_index: 'auth-1', status: false }),
    '?page=1&page_size=25&auth_index=auth-1'
  );
  assert.equal(
    ui.query({ model: 'a&b c' }),
    '?page=1&page_size=25&model=a%26b+c'
  );
});

// ---------------------------------------------------------------------------
// 静态安全 / 鉴权 / 取消控件
// ---------------------------------------------------------------------------

const externalReference = /https?:\/\//i;
const externalWithoutSVGNamespace = (source) =>
  source.replaceAll('http://www.w3.org/2000/svg', '');
// 静态契约只看代码语义：去掉空白与引号后比较，避免 prettier 等格式调整造成误报。
const compactSource = (source) => source.replace(/["']/g, '').replace(/\s+/g, '');
const includesCode = (source, snippet) => compactSource(source).includes(compactSource(snippet));

test('the fixed ui.js bundle keeps settings.js before the UI runtime', () => {
  const settingsAt = compactSource(servedUISource).indexOf(
    compactSource('window.ObserverSettings = ObserverSettings')
  );
  const uiAt = compactSource(servedUISource).indexOf(compactSource('window.ObserverUI = {'));
  assert.ok(settingsAt >= 0, 'settings.js must define ObserverSettings in the bundle');
  assert.ok(uiAt > settingsAt, 'settings.js must execute before ui.js in the bundle');
  assert.match(uiSource, /window\.ObserverSettings\s*\(/);

  // ui.html only needs the fixed ui.js endpoint; settings.js is not a separate asset.
  const scripts = [...html.matchAll(/<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["']/gi)].map(
    (match) => match[1]
  );
  assert.ok(scripts.includes('/v0/resource/plugins/cliproxyapi-observer/ui.js'), 'ui.html must load ui.js');
});

const forbiddenMarkupAPIs = [
  'innerHTML',
  'outerHTML',
  'insertAdjacentHTML',
  'document.write',
  'eval(',
  'new Function',
  'window.open'
];

test('dynamic UI text never uses markup injection APIs', () => {
  for (const [name, source] of [['ui.js', uiSource], ['settings.js', settingsSource]]) {
    for (const needle of forbiddenMarkupAPIs) {
      assert.equal(source.includes(needle), false, name + ' must not use ' + needle);
    }
    assert.match(source, /textContent/, name + ' must render dynamic text with textContent');
  }
  assert.match(uiSource, /createTextNode/, 'table cells must use text nodes for untrusted values');
});

test('assets stay self-contained and CSP-compatible', () => {
  assert.equal(externalReference.test(externalWithoutSVGNamespace(html)), false);
  assert.equal(externalReference.test(externalWithoutSVGNamespace(css)), false);
  assert.equal(externalReference.test(externalWithoutSVGNamespace(uiSource)), false);
  assert.equal(externalReference.test(externalWithoutSVGNamespace(settingsSource)), false);

  assert.match(html, /default-src 'self'/);
  assert.match(html, /script-src 'self'/);
  assert.match(html, /style-src 'self'/);
  assert.match(html, /connect-src 'self'/);
  assert.equal(/@import/i.test(css), false, 'ui.css must not import external styles');
  assert.equal(/\son[a-z]+\s*=/i.test(html), false, 'ui.html must not use inline event handlers');
  assert.equal(/\sstyle\s*=/i.test(html), false, 'ui.html must not use inline style attributes');

  for (const tag of html.match(/<script\b[^>]*>/gi) || []) {
    assert.match(tag, /\bsrc=/i, 'every script must be same-origin src: ' + tag);
  }
});

test('login and API calls keep the management key in memory and the tab session only', () => {
  for (const control of [
    'id="login"',
    'type="password"',
    'autocomplete="off"',
    'required',
    '>登录</button>'
  ]) {
    assert.ok(includesCode(html, control), 'ui.html missing auth control ' + control);
  }

  for (const guard of [
    'Authorization: "Bearer " + key',
    'sessionStorage.getItem("observer.management-key")',
    'sessionStorage.setItem("observer.management-key", key)',
    'sessionStorage.removeItem("observer.management-key")',
    'login.querySelector("input").value = ""',
    'r.status === 401',
    'function logout()',
    'key = ""'
  ]) {
    assert.ok(includesCode(uiSource, guard), 'ui.js missing auth guard ' + guard);
  }
  assert.equal(/\blocalStorage\b/.test(uiSource + settingsSource), false);
  assert.equal(/document\.cookie/.test(uiSource + settingsSource), false);
});

test('cancels and destructive actions stay explicit before any write', () => {
  for (const control of [
    'd.addEventListener("cancel"',
    'd.addEventListener("close"',
    'd.replaceChildren()',
    'd.remove()',
    'function confirmed(',
    'confirm(',
    'button("退出", logout)'
  ]) {
    assert.ok(includesCode(uiSource, control), 'ui.js missing cancel/cleanup control ' + control);
  }

  for (const control of ['重新加载 / 放弃草稿', 'win.confirm', '已取消压缩']) {
    assert.ok(includesCode(settingsSource, control), 'settings.js missing cancel control ' + control);
  }
});
