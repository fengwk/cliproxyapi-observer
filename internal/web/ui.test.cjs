'use strict';

/*
 * Node 单元测试：`node --test internal/web/ui.test.cjs`
 *
 * 覆盖目标：
 *  - 反向代理前缀 / v0 / v8 的 API 路径推导与同源 URL 构造（含注入防护）。
 *  - 管理密钥传输安全（HTTPS / 回环）判断。
 *  - 服务端畸形 / 空 / null 数据的格式化与降级。
 *  - 主题解析（父级 data-theme 跟随 vs 系统偏好回退）。
 *  - 过期查询 / 过期请求体响应的丢弃守卫。
 *  - 请求体弹窗关闭清空。
 *  - 渲染层 XSS：恶意文本必须以 textContent 原样落盘，且绝不触碰 innerHTML。
 *  - 源码级防线：无 innerHTML / eval / 浏览器密钥存储。
 */

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const ui = require('./ui.js');

// ---------------------------------------------------------------------------
// 最小 DOM 桩：任何 innerHTML / outerHTML 访问都会抛错，用来证明渲染层只用
// textContent + createElement。
// ---------------------------------------------------------------------------

class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.childNodes = [];
    this.attributes = {};
    this.className = '';
    this.listeners = {};
    this._text = '';
    this.colSpan = 0;
    this.type = '';
    this.value = '';
    this.hidden = false;
    this.disabled = false;
    this.parentNode = null;
  }

  set textContent(value) {
    this._text = value === null || value === undefined ? '' : String(value);
    this.childNodes = [];
  }

  get textContent() {
    if (this.childNodes.length) return this.childNodes.map((c) => c.textContent).join('');
    return this._text;
  }

  appendChild(child) {
    this.childNodes.push(child);
    child.parentNode = this;
    return child;
  }

  removeChild(child) {
    const index = this.childNodes.indexOf(child);
    if (index >= 0) this.childNodes.splice(index, 1);
    return child;
  }

  get firstChild() {
    return this.childNodes[0] || null;
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  getAttribute(name) {
    return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
  }

  addEventListener(type, handler) {
    (this.listeners[type] || (this.listeners[type] = [])).push(handler);
  }

  dispatch(type) {
    (this.listeners[type] || []).forEach((handler) => handler());
  }
}

for (const forbidden of ['innerHTML', 'outerHTML', 'insertAdjacentHTML']) {
  Object.defineProperty(FakeNode.prototype, forbidden, {
    get() {
      throw new Error('forbidden DOM property read: ' + forbidden);
    },
    set() {
      throw new Error('forbidden DOM property write: ' + forbidden);
    }
  });
}

class FakeDocument {
  createElement(tag) {
    return new FakeNode(tag);
  }
}

function collectText(node, out = []) {
  if (node.childNodes.length === 0) {
    out.push(node.textContent);
    return out;
  }
  node.childNodes.forEach((child) => collectText(child, out));
  return out;
}

function findNodes(node, predicate, out = []) {
  if (predicate(node)) out.push(node);
  node.childNodes.forEach((child) => findNodes(child, predicate, out));
  return out;
}

// ---------------------------------------------------------------------------
// API 路径推导
// ---------------------------------------------------------------------------

test('deriveApiBase 保留反向代理前缀并映射 resource→management（v0/v8）', () => {
  assert.equal(
    ui.deriveApiBase('/v0/resource/plugins/cliproxyapi-observer/ui'),
    '/v0/management/plugins/cliproxyapi-observer'
  );
  assert.equal(
    ui.deriveApiBase('/v8/resource/plugins/cliproxyapi-observer/ui.js'),
    '/v8/management/plugins/cliproxyapi-observer'
  );
  assert.equal(
    ui.deriveApiBase('/proxy/v0/resource/plugins/cliproxyapi-observer/ui.css'),
    '/proxy/v0/management/plugins/cliproxyapi-observer'
  );
  // 已处于 management 路径时原样保留。
  assert.equal(
    ui.deriveApiBase('/v0/management/plugins/cliproxyapi-observer/ui'),
    '/v0/management/plugins/cliproxyapi-observer'
  );
});

test('deriveApiBase 对独立打开 / 畸形路径回退默认前缀', () => {
  assert.equal(ui.deriveApiBase('/ui'), ui.DEFAULT_API_BASE);
  assert.equal(ui.deriveApiBase(''), ui.DEFAULT_API_BASE);
  assert.equal(ui.deriveApiBase(null), ui.DEFAULT_API_BASE);
  assert.equal(ui.deriveApiBase('/totally/unrelated/path'), ui.DEFAULT_API_BASE);
});

test('buildApiUrl 编码参数并忽略空值', () => {
  assert.equal(
    ui.buildApiUrl('/v0/management/plugins/x', 'summary', {
      from: '2026-01-01T00:00:00Z',
      to: '2026-01-02T00:00:00Z',
      provider: '',
      model: null,
      limit: 50
    }),
    '/v0/management/plugins/x/summary?from=2026-01-01T00%3A00%3A00Z&to=2026-01-02T00%3A00%3A00Z&limit=50'
  );
  assert.equal(ui.buildApiUrl('/base/', '/body', { request_id: 'a b' }), '/base/body?request_id=a%20b');
  assert.equal(ui.buildApiUrl('/base', 'health', null), '/base/health');
});

test('buildApiUrl 拒绝非相对 / 跨源基址', () => {
  assert.equal(ui.buildApiUrl('', 'summary', null), '');
  assert.equal(ui.buildApiUrl('//evil.example.com/x', 'summary', null), '');
  assert.equal(ui.buildApiUrl('https://evil.example.com/x', 'summary', null), '');
  assert.equal(ui.buildApiUrl('javascript:alert(1)', 'summary', null), '');
  assert.equal(ui.buildApiUrl('/base', '', null), '');
  for (const base of ['/\\evil.test', '//evil.test', '/%5cevil.test', '/%2f%2fevil.test', '/base/../evil']) {
    assert.equal(ui.buildApiUrl(base, 'body'), '');
    assert.equal(ui.deriveApiBase(base + '/resource/plugins/x/ui'), ui.DEFAULT_API_BASE);
  }
  assert.equal(ui.buildApiUrl('/base', '\\evil'), '');
  assert.equal(ui.buildApiUrl('/base', '//evil'), '');
});

test('isAllowedTransport 只放行 HTTPS 与本机回环', () => {
  assert.equal(ui.isAllowedTransport({ protocol: 'https:', hostname: 'cpa.example.com' }), true);
  assert.equal(ui.isAllowedTransport({ protocol: 'http:', hostname: '127.0.0.1' }), true);
  assert.equal(ui.isAllowedTransport({ protocol: 'http:', hostname: 'localhost' }), true);
  assert.equal(ui.isAllowedTransport({ protocol: 'http:', hostname: '::1' }), true);
  assert.equal(ui.isAllowedTransport({ protocol: 'http:', hostname: 'cpa.example.com' }), false);
  assert.equal(ui.isAllowedTransport({ protocol: 'ftp:', hostname: '127.0.0.1' }), false);
  assert.equal(ui.isAllowedTransport({}), false);
});

test('rangeBounds includes the current minute without a future upper bound', () => {
  const now = Date.UTC(2026, 0, 2, 3, 4, 5, 678);
  const bounds = ui.rangeBounds('24h', now);
  assert.equal(bounds.to, '2026-01-02T03:04:05.678Z');
  assert.equal(bounds.from, '2026-01-01T03:04:05.678Z');
  const week = ui.rangeBounds('7d', now);
  assert.equal(week.from, '2025-12-26T03:04:05.678Z');
  const month = ui.rangeBounds('30d', now);
  assert.equal(month.from, '2025-12-03T03:04:05.678Z');
  assert.equal(Date.parse(bounds.to), now);
  // 未知范围回退 24h。
  assert.deepEqual(ui.rangeBounds('bogus', now), bounds);
});

// ---------------------------------------------------------------------------
// 格式化 / 畸形数据
// ---------------------------------------------------------------------------

test('格式化函数对 null / 畸形输入降级为占位符', () => {
  assert.equal(ui.formatInt(null), '—');
  assert.equal(ui.formatInt('abc'), '—');
  assert.equal(ui.formatInt(1234), '1,234');
  assert.equal(ui.formatCompact(undefined), '—');
  assert.equal(ui.formatCompact(25000), '25.0K');
  assert.equal(ui.formatCompact(2500000), '2.50M');
  assert.equal(ui.formatLatency(0), '—');
  assert.equal(ui.formatLatency('x'), '—');
  assert.equal(ui.formatLatency(250000000), '250 ms');
  assert.equal(ui.formatTps(null), '—');
  assert.equal(ui.formatTps(0), '—');
  assert.equal(ui.formatTps(9.87), '9.9 tok/s');
  assert.equal(ui.formatRate('bad'), '—');
  assert.equal(ui.formatRate(0.1234), '12.3%');
  assert.equal(ui.formatBytes(-1), '—');
  assert.equal(ui.formatBytes(1048576), '1.0 MB');
  assert.equal(ui.formatDuration('bad'), '—');
  assert.equal(ui.formatDuration(86400), '1 天');
  assert.equal(ui.formatDuration(3600), '1 小时');
  assert.equal(ui.formatTime('not-a-date'), '—');
  assert.equal(ui.formatTime(''), '—');
});

test('formatCost 明确标注未定价与极小金额', () => {
  assert.equal(ui.formatCost(null), '未定价');
  assert.equal(ui.formatCost(undefined), '未定价');
  assert.equal(ui.formatCost('oops'), '未定价');
  assert.equal(ui.formatCost(0), '$0.0000');
  assert.equal(ui.formatCost(0.00001), '<$0.0001');
  assert.equal(ui.formatCost(2.5), '$2.5000');
});

// 历史成本由响应中的查询结果决定，不依赖前端旧价格或请求快照。
test('历史成本文案使用当前生效价格并采纳重新查询金额', () => {
  assert.equal(ui.buildOverview({ requests: 1, cost_usd: 1 })[6].sub, '按当前生效价格');
  assert.equal(ui.buildRequestRows([{ cost_usd: 2 }])[0].cost, '$2.0000');
  assert.equal(ui.buildRequestRows([{ cost_usd: null }])[0].cost, '未定价');
});

// Zero is an authoritative subtotal, not a price for unknown requests.
test('aggregate costs distinguish all-unknown, partial and priced-zero', () => {
  for (const source of [
    { requests: 5, unpriced_requests: 5, cost_usd: 0 },
    { requests: 5, unpriced_requests: 6, cost_usd: 0 }
  ]) {
    assert.equal(ui.buildOverview(source).find((c) => c.key === 'cost').value, '未定价');
    assert.equal(ui.buildGroupRows([source])[0].cost, '未定价');
  }
  const partial = { requests: 5, unpriced_requests: 2, cost_usd: 0 };
  assert.match(ui.buildOverview(partial).find((c) => c.key === 'cost').sub, /已定价小计；未定价 2 条/);
  assert.match(ui.buildGroupRows([partial])[0].cost, /已定价小计；未知 2 条/);
  assert.equal(ui.buildGroupRows([{ requests: 5, unpriced_requests: 0, cost_usd: 0 }])[0].cost, '$0.0000');
});

test('body responses require exact server identity and string content', () => {
  const valid = { request_id: 'r1', body: '' };
  assert.equal(ui.validateBodyDetail(valid, 'r1'), valid);
  for (const value of [null, {}, { request_id: 'r2', body: 'secret' },
    { request_id: 'r1', body: null }, { request_id: 'r1', body: {} },
    { request_id: 1, body: 'secret' }]) {
    assert.throws(() => ui.validateBodyDetail(value, 'r1'), /不匹配或格式无效/);
  }
});

test('buildOverview 对 null / 缺失计数安全降级', () => {
  const empty = ui.buildOverview(null);
  assert.equal(empty.length, 7);
  assert.deepEqual(
    empty.map((c) => c.key),
    ['requests', 'failed', 'tokens', 'cache', 'latency', 'ttft', 'cost']
  );
  assert.ok(empty.every((c) => c.value === '—'));

  const partial = ui.buildOverview({ requests: 4, failed_requests: 1, total_tokens: 100, latency_ns: 300000000, latency_samples: 2 });
  const byKey = Object.fromEntries(partial.map((c) => [c.key, c]));
  assert.equal(byKey.requests.value, '4');
  assert.equal(byKey.failed.value, '1');
  assert.equal(byKey.failed.sub, '失败率 25.0%');
  assert.equal(byKey.latency.value, '150 ms');
  assert.equal(byKey.cost.value, '未定价');
  assert.equal(byKey.cost.warn, true);
});

test('buildGroupRows 处理畸形数据并按请求量排序', () => {
  const rows = ui.buildGroupRows([
    { provider: 'b', model: 'm2', requests: 1 },
    null,
    { provider: 'a', model: 'm1', requests: 10, cost_usd: null },
    'junk',
    { model: 'm3', requests: 5, cost_usd: 0.5 }
  ]);
  assert.equal(rows.length, 3);
  assert.equal(rows[0].model, 'm1');
  assert.equal(rows[0].cost, '未定价');
  assert.equal(rows[0].costUnpriced, true);
  assert.equal(rows[1].model, 'm3');
  assert.equal(rows[1].costUnpriced, false);
  assert.equal(rows[2].model, 'm2');
});

test('buildRequestRows 标注 null TPS / 未定价 / 未知核算', () => {
  const rows = ui.buildRequestRows([
    {
      request_id: 'r1',
      time: '2026-01-01T10:00:00Z',
      model: 'gpt-x',
      provider: 'openai',
      total_tokens: 10,
      tps: null,
      cost_usd: null,
      accounting_quality: 'unclassified',
      body_available: true
    },
    { request_id: 'r2', model: 'claude', failed: true, failure_status: 500, tps: null, cost_usd: null, accounting_quality: 'complete' }
  ]);
  assert.equal(rows[0].tps, '—');
  assert.equal(rows[0].cost, '未定价');
  assert.equal(rows[0].quality, 'unclassified');
  assert.equal(rows[0].statusText, '成功');
  assert.equal(rows[1].quality, '');
  assert.equal(rows[1].statusText, '失败 500');
  assert.equal(rows[1].failed, true);
  assert.equal(rows[1].bodyAvailable, false);
});

test('buildHealthWarning 只在存在丢失 / 写入失败时告警', () => {
  assert.equal(ui.buildHealthWarning(null), '');
  assert.equal(ui.buildHealthWarning({ dropped_usage: 0, dropped_bodies: 0, write_errors: 0 }), '');
  const warning = ui.buildHealthWarning({ dropped_usage: 3, dropped_bodies: 1, write_errors: 2 });
  assert.match(warning, /丢弃用量 3 条/);
  assert.match(warning, /丢弃请求体 1 条/);
  assert.match(warning, /写入失败 2 次/);
});

test('normalizeSettings 对畸形输入安全降级', () => {
  const s = ui.normalizeSettings(null);
  assert.equal(s.captureBodies, false);
  assert.ok(Number.isNaN(s.bodyRetentionSeconds));
  const real = ui.normalizeSettings({
    capture_bodies: true,
    body_retention_seconds: 3600,
    request_retention_seconds: 86400,
    stats_retention_days: 365,
    max_body_bytes: 1048576,
    max_body_storage_bytes: 268435456
  });
  assert.equal(real.captureBodies, true);
  assert.equal(real.bodyRetentionSeconds, 3600);
  assert.equal(real.statsRetentionDays, 365);
});

// ---------------------------------------------------------------------------
// 过期守卫 / 请求体弹窗状态机
// ---------------------------------------------------------------------------

test('createGate 只在最新令牌时放行（过期查询守卫）', () => {
  const gate = ui.createGate();
  const first = gate.begin();
  assert.equal(gate.current(first), true);
  const second = gate.begin();
  assert.equal(gate.current(first), false);
  assert.equal(gate.current(second), true);
  gate.invalidate();
  assert.equal(gate.current(second), false);
});

test('bodyViewerReduce 丢弃过期请求体响应并在关闭时清空', () => {
  let state = ui.bodyViewerReduce(null, { type: 'open', requestId: 'r1', meta: { model: 'm' } });
  assert.equal(state.phase, 'confirm');
  state = ui.bodyViewerReduce(state, { type: 'confirm', requestId: 'r1' });
  assert.equal(state.phase, 'loading');
  // 错配请求 ID 的响应必须被丢弃。
  const stale = ui.bodyViewerReduce(state, { type: 'loaded', requestId: 'r2', content: 'leak' });
  assert.equal(stale, state);
  // 正常响应被采纳。
  state = ui.bodyViewerReduce(state, { type: 'loaded', requestId: 'r1', content: 'hello', redacted: true });
  assert.equal(state.phase, 'loaded');
  assert.equal(state.content, 'hello');
  assert.equal(state.redacted, true);
  // 关闭清空内容。
  state = ui.bodyViewerReduce(state, { type: 'close' });
  assert.equal(state.phase, 'idle');
  assert.equal(state.content, '');
  // 关闭后的迟到响应被丢弃。
  const late = ui.bodyViewerReduce(state, { type: 'loaded', requestId: 'r1', content: 'late' });
  assert.equal(late, state);
});

test('bodyViewerReduce 忽略未处于加载中的重复确认 / 错误', () => {
  const idle = ui.bodyViewerReduce(null, { type: 'close' });
  assert.equal(ui.bodyViewerReduce(idle, { type: 'confirm', requestId: 'r1' }), idle);
  assert.equal(ui.bodyViewerReduce(idle, { type: 'error', requestId: 'r1', message: 'x' }), idle);
});

// ---------------------------------------------------------------------------
// 主题
// ---------------------------------------------------------------------------

test('normalizeTheme 只接受 white / dark', () => {
  assert.equal(ui.normalizeTheme('dark'), 'dark');
  assert.equal(ui.normalizeTheme(' WHITE '), 'white');
  assert.equal(ui.normalizeTheme(''), '');
  assert.equal(ui.normalizeTheme(null), '');
  assert.equal(ui.normalizeTheme('junk'), '');
});

test('resolveThemeSource 同源嵌入跟随父级，否则回退系统偏好', () => {
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: true, parentTheme: 'dark', prefersDark: false }), 'dark');
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: true, parentTheme: 'white' }), 'white');
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: true, parentTheme: '' }), '');
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: true, parentTheme: 'junk' }), '');
  // 跨域嵌入不能读取父级，回退系统偏好。
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: false, parentTheme: 'dark', prefersDark: true }), 'dark');
  assert.equal(ui.resolveThemeSource({ framed: true, sameOrigin: false, prefersDark: false }), 'white');
  // 独立打开。
  assert.equal(ui.resolveThemeSource({ framed: false, prefersDark: true }), 'dark');
  assert.equal(ui.resolveThemeSource({ framed: false, prefersDark: false }), 'white');
});

// ---------------------------------------------------------------------------
// XSS：恶意文本必须原样落入 textContent
// ---------------------------------------------------------------------------

test('请求表渲染恶意模型名：原样文本、零 innerHTML', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  const payload = '<img src=x onerror="alert(1)">';
  const providerPayload = '<script>evil<\/script>';
  ui.renderRequestsTable(
    doc,
    tbody,
    [{ request_id: 'r1', model: payload, provider: providerPayload, total_tokens: 1, time: 'x' }],
    null
  );
  const texts = collectText(tbody);
  assert.ok(texts.includes(payload), '模型名必须原样渲染为文本节点');
  assert.ok(texts.includes(providerPayload), 'provider 必须原样渲染为文本节点');

  // payload 必须作为纯文本存在，绝不能被解析成元素。
  assert.equal(findNodes(tbody, (n) => n.tagName === 'img' || n.tagName === 'script').length, 0);
  const leafTexts = findNodes(tbody, (n) => n.childNodes.length === 0).map((n) => n.textContent);
  assert.ok(leafTexts.includes(payload), 'payload 应以叶子文本节点存在');
  assert.ok(leafTexts.includes(providerPayload), 'provider payload 应以叶子文本节点存在');
});

test('请求表为可查看请求体提供按钮并回传 request_id', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  const seen = [];
  ui.renderRequestsTable(
    doc,
    tbody,
    [{ request_id: 'req-42', model: 'm', body_available: true, time: 'x' }],
    (id) => seen.push(id)
  );
  const buttons = findNodes(tbody, (n) => n.tagName === 'button');
  assert.equal(buttons.length, 1);
  assert.equal(buttons[0].getAttribute('data-request-id'), 'req-42');
  buttons[0].dispatch('click');
  assert.deepEqual(seen, ['req-42']);
});

test('请求表对未捕获请求体显示提示而非按钮', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  ui.renderRequestsTable(doc, tbody, [{ request_id: 'r1', model: 'm', body_available: false }], null);
  assert.equal(findNodes(tbody, (n) => n.tagName === 'button').length, 0);
});

test('空数据渲染占位行', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  ui.renderRequestsTable(doc, tbody, [], null);
  assert.ok(collectText(tbody).some((t) => t.includes('暂无数据')));
  const tbody2 = doc.createElement('tbody');
  ui.renderGroupsTable(doc, tbody2, null);
  assert.ok(collectText(tbody2).some((t) => t.includes('暂无数据')));
});

test('分组表 / 概览渲染恶意文本不会触碰 innerHTML', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  ui.renderGroupsTable(doc, tbody, [{ provider: '<svg onload=alert(1)>', model: '"><b>', requests: 3 }]);
  const texts = collectText(tbody).join('|');
  assert.ok(texts.includes('<svg onload=alert(1)>'));
  assert.ok(texts.includes('"><b>'));

  const cards = doc.createElement('div');
  ui.renderOverview(doc, cards, ui.buildOverview({ requests: 1 }));
  assert.equal(findNodes(cards, (n) => n.tagName === 'div').length > 0, true);
});

test('设置渲染在捕获关闭时给出解释文案', () => {
  const doc = new FakeDocument();
  const container = doc.createElement('div');
  ui.renderSettings(doc, container, { capture_bodies: false, stats_retention_days: 365 });
  const text = collectText(container).join(' ');
  assert.match(text, /请求体捕获已关闭/);
  assert.match(text, /保存前不会捕获新请求体/);

  const doc2 = new FakeDocument();
  const container2 = doc2.createElement('div');
  ui.renderSettings(doc2, container2, {
    capture_bodies: true,
    body_retention_seconds: 86400,
    request_retention_seconds: 86400,
    max_body_bytes: 1048576,
    max_body_storage_bytes: 268435456
  });
  const text2 = collectText(container2).join(' ');
  assert.match(text2, /请求体捕获已开启/);
  assert.equal(findNodes(container2, (n) => n.id === 'setting-body-retention')[0].value, '1');
  assert.equal(findNodes(container2, (n) => n.id === 'unit-body-retention')[0].value, 'd');
  assert.equal(findNodes(container2, (n) => n.id === 'setting-max-body-bytes')[0].value, '1');
});

// ---------------------------------------------------------------------------
// 源码级安全防线
// ---------------------------------------------------------------------------

test('ui.js 源码不含危险 DOM / 浏览器密钥存储 API', () => {
  const source = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
  const forbidden = [
    'innerHTML',
    'outerHTML',
    'insertAdjacentHTML',
    'document.write',
    'eval(',
    'new Function',
    'localStorage',
    'sessionStorage',
    'document.cookie',
    'win.confirm(', 'win.alert(', 'win.prompt('
  ];
  for (const needle of forbidden) {
    assert.equal(source.includes(needle), false, 'ui.js 不应包含 ' + needle);
  }
});

test('ui.html 不包含内联脚本 / 事件处理器 / 外部资源', () => {
  const html = fs.readFileSync(path.join(__dirname, 'ui.html'), 'utf8');
  assert.equal(/\son[a-z]+\s*=/i.test(html), false, '不得使用内联事件处理器');
  assert.equal(/<script(?![^>]*\ssrc=)/i.test(html), false, '不得包含内联脚本');
  assert.equal(/https?:\/\//i.test(html), false, '不得引用外部资源');
  assert.match(html, /src="ui\.js"/);
  assert.match(html, /href="ui\.css"/);
});

test('ui.css 覆盖三套主题令牌', () => {
  const css = fs.readFileSync(path.join(__dirname, 'ui.css'), 'utf8');
  assert.match(css, /\[data-theme='white'\]/);
  assert.match(css, /\[data-theme='dark'\]/);
  assert.match(css, /--bg-secondary:\s*#faf9f5/);
  assert.equal(/@import|https?:\/\//i.test(css), false, 'CSS 不得引入外部资源');
});

// 共享按钮契约：与 Provider 面板逐条复制同一组声明。所有按钮必须带 .btn，
// 尺寸只来自语义/尺寸变体类，禁止组件或表格专属的字体、内边距、颜色覆盖。
test('ui 按钮遵循共享 Management Center 契约', () => {
  const css = fs.readFileSync(path.join(__dirname, 'ui.css'), 'utf8');
  const html = fs.readFileSync(path.join(__dirname, 'ui.html'), 'utf8');
  const js = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

  const base = css.match(/\.btn\s*\{([^}]*)\}/);
  assert.ok(base, '.btn 基础规则必须存在');
  assert.match(base[1], /display:\s*inline-flex/);
  assert.match(base[1], /gap:\s*8px/);
  assert.match(base[1], /padding:\s*10px 14px/);
  assert.match(base[1], /border-radius:\s*var\(--radius-md\)/);
  assert.match(base[1], /font-size:\s*16px/);
  assert.match(base[1], /font-weight:\s*600/);
  assert.match(base[1], /line-height:\s*1\.5/);
  assert.match(base[1], /white-space:\s*nowrap/);

  const small = css.match(/\.btn-sm\s*\{([^}]*)\}/);
  assert.ok(small, '.btn-sm 规则必须存在');
  assert.match(small[1], /padding:\s*8px 10px/);
  assert.match(small[1], /font-size:\s*14px/);
  assert.equal(/font-weight/.test(small[1]), false, 'btn-sm 不得覆盖字重');

  assert.match(css, /\.btn-danger\s*\{[^}]*background-color:\s*var\(--error-color\)/);
  assert.match(css, /\.btn-danger:hover:not\(:disabled\)/);
  assert.match(css, /\.btn:disabled\s*\{[^}]*opacity:\s*0\.6/);
  assert.match(css, /\.btn:disabled\s*\{[^}]*cursor:\s*not-allowed/);
  assert.match(css, /\.btn:focus-visible\s*\{[^}]*outline:\s*2px solid var\(--text-primary\)/);
  assert.equal(/price-delete/.test(css), false, '不得保留 price-delete 视觉覆盖');

  // 段控复用 .btn 基础，只特化拼接边框/圆角/选中与默认文字色；不得定义字号/字重/内边距。
  const seg = css.match(/\.seg-btn\s*\{([^}]*)\}/);
  assert.ok(seg, '.seg-btn 规则必须存在');
  assert.equal(/font-size|font-weight|padding/.test(seg[1]), false, 'seg-btn 尺寸必须继承 .btn');
  assert.match(seg[1], /border-radius:\s*0/);
  assert.match(css, /\.seg-btn:hover:not\(:disabled\)/);

  // 静态按钮全部带 .btn。
  for (const m of html.matchAll(/<button\b[^>]*>/g)) {
    const classes = m[0].match(/\bclass="([^"]*)"/);
    assert.ok(classes && classes[1].split(/\s+/).includes('btn'), '静态按钮缺少 .btn: ' + m[0]);
  }
  // 动态按钮与段控运行时保留 .btn，删除按钮改用已定义的 btn-danger。
  assert.match(js, /className = 'btn btn-danger btn-sm'/);
  assert.match(js, /className = 'btn seg-btn is-active'/);
  assert.match(js, /className = 'btn seg-btn'/);
  assert.equal(js.includes('price-delete'), false, 'ui.js 不得再引用 price-delete');
});

// 保存只允许同源 v0 核心配置路由；版本转换不能削弱路径防线。
test('config URLs use only v0 while preserving proxy prefix', () => {
  assert.equal(ui.coreApiBase(ui.deriveApiBase('/proxy/v8/resource/plugins/cliproxyapi-observer/ui')),
    '/proxy/v0/management/plugins/cliproxyapi-observer');
  for (const value of ['//evil/v8/management/plugins/cliproxyapi-observer',
    '/proxy/../v8/management/plugins/cliproxyapi-observer', '/%2e/v8/management/plugins/cliproxyapi-observer',
    'https://evil/v0/management/plugins/cliproxyapi-observer']) assert.equal(ui.coreApiBase(value), '');
});

const effectiveSettings = {
  capture_bodies: false, request_retention_seconds: 86400, body_retention_seconds: 86400,
  stats_retention_days: 365, max_body_bytes: 1048576, max_body_storage_bytes: 268435456,
  compact_interval_seconds: 900, compact_min_bytes: 8388608, prices: {}
};

test('settings patch includes only editable keys and exact conversions', () => {
  const patch = ui.settingsToPatch(effectiveSettings);
  assert.equal(ui.validateSettingsPatch(patch), patch);
  assert.equal(ui.durationSeconds('1.5', 'h'), 5400);
  assert.equal(ui.durationSeconds('0.1', 'm'), 6);
  assert.throws(() => ui.durationSeconds('0.001', 'm'));
  assert.equal(patch['request-retention'], '86400s');
  assert.equal(patch['compact-interval'], '900s');
  assert.deepEqual(Object.keys(patch).sort(), ['body-retention', 'capture-bodies', 'compact-interval',
    'compact-min-bytes', 'max-body-bytes', 'max-body-storage-bytes', 'prices',
    'request-retention', 'stats-retention-days'].sort());
});

test('all server bounds reject invalid drafts without silently clamping', () => {
  const patch = ui.settingsToPatch(effectiveSettings);
  for (const [key, value] of [
    ['request-retention', '59s'], ['request-retention', '2592001s'],
    ['body-retention', '86401s'], ['body-retention', '59s'],
    ['stats-retention-days', 0], ['stats-retention-days', 3651], ['stats-retention-days', 1.5],
    ['max-body-bytes', 0], ['max-body-bytes', 67108865],
    ['max-body-storage-bytes', 8589934593], ['max-body-storage-bytes', 1],
    ['compact-interval', '59s'], ['compact-interval', '86401s'],
    ['compact-min-bytes', 65535], ['compact-min-bytes', 8589934593], ['db', 'unexpected']
  ]) assert.throws(() => ui.validateSettingsPatch({ ...patch, [key]: value }), key + ':' + value);
  ui.validateSettingsPatch({
    ...patch, 'request-retention': '2592000s', 'body-retention': '60s',
    'stats-retention-days': 3650, 'max-body-bytes': 1, 'max-body-storage-bytes': 8589934592,
    'compact-interval': '86400s', 'compact-min-bytes': 65536
  });
});

test('prices reject blank normalized duplicate IDs and nonfinite or negative values', () => {
  const patch = ui.settingsToPatch(effectiveSettings);
  const valid = { input: 0, output: 1.2, 'cache-read': 0, 'cache-creation': 0 };
  for (const prices of [
    { ' ': valid }, { m: valid, ' m ': valid },
    { m: { ...valid, input: -1 } }, { m: { ...valid, input: Infinity } },
    { m: { ...valid, input: NaN } }, { m: { ...valid, input: '' } }
  ]) assert.throws(() => ui.validateSettingsPatch({ ...patch, prices }));
  ui.validateSettingsPatch({ ...patch, prices: { '<script>literal</script>': valid } });
  assert.equal(ui.patchesMatch(patch, { ...patch, 'request-retention': '24h' }), true);
  assert.equal(ui.patchesMatch(patch, { ...patch, 'capture-bodies': true }), false);
});

test('editable price rules stay text-only and controls create a draft, never auto-save', () => {
  const doc = new FakeDocument(), container = doc.createElement('div');
  let edits = 0;
  const id = '<img onerror="evil" src=x>';
  const editor = ui.renderSettings(doc, container, {
    ...effectiveSettings, prices: { [id]: { input: 1, output: 2, cache_read: 3, cache_creation: 4 } }
  }, () => edits++);
  assert.equal(edits, 0);
  // Legacy map rows render as the first ordered rule and save canonically.
  const draft = editor.read();
  assert.deepEqual(Object.keys(draft.prices), []);
  assert.equal(draft['price-rules'].length, 1);
  assert.equal(draft['price-rules'][0].model, id);
  assert.equal(draft['price-rules'][0].price['cache-read'], 3);
  assert.equal(findNodes(container, (n) => n.tagName === 'img').length, 0);
  const capture = findNodes(container, (n) => n.id === 'setting-capture-bodies')[0];
  capture.checked = true; capture.dispatch('input');
  assert.equal(edits, 1);
  assert.equal(editor.read()['capture-bodies'], true);
  findNodes(container, (n) => n.tagName === 'button' && n.textContent === '删除')[0].dispatch('click');
  assert.equal(editor.read()['price-rules'].length, 0);
  assert.deepEqual(Object.keys(editor.read().prices), []);
});

test('normalizePriceRules 规范化别名并严格校验阈值与 UTC 区间', () => {
  assert.deepEqual(
    ui.normalizePriceRules([
      { model: ' m ', input_tokens_gt: 0, time_range: '00:00-08:30', price: { input: 1, output: 2, cache_read: 3, cache_creation: 4 } }
    ]),
    [{
      model: 'm',
      price: { input: 1, output: 2, 'cache-read': 3, 'cache-creation': 4 },
      'input-tokens-gt': 0,
      'time-range': '00:00-08:30'
    }]
  );
  const zero = { input: 0, output: 0, 'cache-read': 0, 'cache-creation': 0 };
  // 跨夜与 24:00 终点合法。
  ui.normalizePriceRules([{ model: 'm', price: zero, 'time-range': '22:00-06:00' }]);
  ui.normalizePriceRules([{ model: 'm', price: zero, 'time-range': '00:00-24:00' }]);
  ui.normalizePriceRules([{ model: 'm', price: zero, 'input-tokens-gt': Number.MAX_SAFE_INTEGER }]);
  for (const rule of [
    { model: 'm', price: zero, 'time-range': '08:00-08:00' },
    { model: 'm', price: zero, 'time-range': '8:00-9:00' },
    { model: 'm', price: zero, 'time-range': '24:00-01:00' },
    { model: 'm', price: zero, 'time-range': '25:00-01:00' },
    { model: 'm', price: zero, unknown: 1 },
    { model: 'm', price: { ...zero, extra: 1 } },
    { model: 'm', price: zero, 'input-tokens-gt': Number.MAX_SAFE_INTEGER + 1 },
    { model: 'm', price: zero, 'input-tokens-gt': -1 },
    { model: 'm', price: zero, 'input-tokens-gt': true },
    { model: 'm', price: zero, 'input-tokens-gt': '1' },
    { model: 'm', price: zero, 'input-tokens-gt': 1, input_tokens_gt: 2 },
    { model: 'm', price: zero, 'time-range': null },
    { model: 'm', price: zero, 'time-range': ' ' },
    { model: 'm', price: { ...zero, input: false } },
    { model: 'm', price: { ...zero, cache_read: 1 } },
    { model: 'm', price: { ...zero, input: -1 } },
    { model: '', price: zero }
  ]) assert.throws(() => ui.normalizePriceRules([rule]), JSON.stringify(rule));
  assert.throws(() => ui.normalizePriceRules('nope'));
  assert.equal(ui.normalizePriceRules([{ model: 'm', price: {} }])[0].price.input, 0);
});

test('规则编辑器保持显式规则优先的顺序，并在重排/删除时标记草稿', () => {
  const doc = new FakeDocument(), container = doc.createElement('div');
  let edits = 0;
  const editor = ui.renderSettings(doc, container, {
    ...effectiveSettings,
    price_rules: [{ model: 'cond', price: { input: 1, output: 1, 'cache-read': 1, 'cache-creation': 1 }, 'input-tokens-gt': 100 }],
    prices: { legacy: { input: 2, output: 2, cache_read: 2, cache_creation: 2 } }
  }, () => edits++);
  assert.deepEqual(editor.read()['price-rules'].map((r) => r.model), ['cond', 'legacy']);
  assert.equal(edits, 0);
  // 第二条规则上移，顺序改变且草稿变脏。
  const upButtons = findNodes(container, (n) => n.tagName === 'button' && n.textContent === '上移');
  assert.equal(upButtons.length, 2);
  upButtons[1].dispatch('click');
  assert.deepEqual(editor.read()['price-rules'].map((r) => r.model), ['legacy', 'cond']);
  assert.equal(edits, 1);
  // 删除第一条规则只移除它本身。
  findNodes(container, (n) => n.tagName === 'button' && n.textContent === '删除')[0].dispatch('click');
  assert.deepEqual(editor.read()['price-rules'].map((r) => r.model), ['cond']);
  assert.equal(edits, 2);
  // 移动目标按钮带可达性标签。
  const action = findNodes(container, (n) => n.tagName === 'button' && n.textContent === '下移')[0];
  assert.match(action.getAttribute('aria-label'), /下移 第 1 条/);
});

test('settingsToPatch 同时保留旧 map 与有序规则，压缩后回读等价', () => {
  const readback = ui.settingsToPatch({
    ...effectiveSettings,
    prices: { legacy: { input: 1, output: 1, cache_read: 1, cache_creation: 1 } },
    price_rules: [{ model: 'legacy', price: { input: 1, output: 1, cache_read: 1, cache_creation: 1 } }]
  });
  assert.equal(readback.prices.legacy.input, 1);
  assert.equal(readback['price-rules'].length, 1);
  const saved = {
    'capture-bodies': false, 'request-retention': '86400s', 'body-retention': '86400s',
    'stats-retention-days': 365, 'max-body-bytes': 1048576, 'max-body-storage-bytes': 268435456,
    'compact-interval': '900s', 'compact-min-bytes': 8388608,
    prices: {},
    'price-rules': [{ model: 'm', price: { input: 1, output: 2, 'cache-read': 3, 'cache-creation': 4 }, 'input-tokens-gt': 10 }]
  };
  const canonicalReadback = ui.settingsToPatch({
    ...effectiveSettings,
    prices: {},
    price_rules: [{ model: 'm', price: { input: 1, output: 2, cache_read: 3, cache_creation: 4 }, input_tokens_gt: 10 }]
  });
  assert.equal(ui.patchesMatch(saved, canonicalReadback), true);
  // 空规则数组与缺失在比较时归一。
  assert.equal(ui.patchesMatch({ ...saved, 'price-rules': [] }, { ...saved, 'price-rules': undefined }), true);
});
// The percentage is request-level, not a guessed token-level cache ratio.
test('缓存请求命中率显示分子分母与零请求降级', () => {
  const cache = ui.buildOverview({ requests: 4, cache_hits: 3 }).find((c) => c.key === 'cache');
  assert.match(cache.sub, /75.*%/);
  assert.match(cache.sub, /3\/4.*缓存读 Token > 0/);
  assert.match(ui.buildOverview({ requests: 0 }).find((c) => c.key === 'cache').sub, /请求命中率 —/);
});

// Use current CPA labels without retaining other credential or token fields.
test('凭据名称优先标签并安全降级，保留反向代理前缀', () => {
  const index = '1'.repeat(16);
  const names = ui.credentialNames({ files: [
    { auth_index: index, name: 'filename', label: '<img onerror=evil>\u0000', api_key: 'secret-never-used' },
    { auth_index: 'unsafe-index', name: 'bad' }
  ] });
  assert.equal(names[index], '<img onerror=evil>');
  assert.equal(Object.keys(names).length, 1);
  assert.equal(JSON.stringify(names).includes('secret-never-used'), false);
  assert.equal(ui.credentialsUrl('/proxy/v8/management/plugins/cliproxyapi-observer'), '/proxy/v8/management/credentials');
  assert.equal(ui.credentialsUrl('https://evil/'), '');
  const rows = ui.buildRequestRows([{ auth_index: index, client_key_id: 'a'.repeat(64) }], names);
  assert.equal(rows[0].credential, '<img onerror=evil>');
  assert.equal(rows[0].clientKeyID.length, 64);
  assert.equal(ui.buildRequestRows([{ auth_index: index }])[0].credential, '索引 ' + index);
  assert.equal(ui.buildRequestRows([{}])[0].credential, '未归属');
});

// 客户端 key 现在是原生下拉：只做选择，默认明确「全部」。候选只含合法 64 位指纹，
// 展示前 12 位但筛选值始终完整；已选指纹即使不在新候选中也必须保留，避免 UI 落回
// 「全部」而查询仍按旧 key 执行。
test('clientKeyOptions 提供固定全部/未归属选项并保留已选指纹', () => {
  const a = 'a'.repeat(64);
  const b = 'b'.repeat(64);
  const c = 'c'.repeat(64);
  const groups = [{ id: b }, { id: a }, { id: '' }, { id: 'not-a-fingerprint' }];
  const items = [{ client_key_id: a }, { client_key_id: 'unknown' }, { client_key_id: null }];
  const opts = ui.clientKeyOptions(groups, items, '');
  // 固定选项在前：空值「全部」与未归属；随后是去重排序的完整指纹。
  assert.deepEqual(opts.map((o) => o.value), ['', 'unknown', a, b]);
  assert.equal(opts[0].label, '全部');
  assert.equal(opts[1].label, '未归属（含旧记录）');
  // 候选文本是 12 位前缀，筛选值与 title 始终是完整 64 位指纹。
  assert.equal(opts[2].label, a.slice(0, 12) + '…');
  assert.equal(opts[2].title, a);
  assert.equal(opts[2].value.length, 64);
  // 已选指纹即使不在新候选（空结果或切换其它维度）中也保留。
  assert.deepEqual(ui.clientKeyOptions([], [], c).map((o) => o.value), ['', 'unknown', c]);
  // 非法或固定选中值不注入重复候选；畸形输入仅剩固定选项。
  assert.deepEqual(ui.clientKeyOptions([], [], 'bogus').map((o) => o.value), ['', 'unknown']);
  assert.deepEqual(ui.clientKeyOptions([], [], 'unknown').map((o) => o.value), ['', 'unknown']);
  assert.deepEqual(ui.clientKeyOptions(null, undefined, null).map((o) => o.value), ['', 'unknown']);
  // previousOptions（本次连接已出现的候选）：只接受合法 64 位、去重；空结果时全部保留。
  const previous = [{ value: a }, { value: c }, { value: a }, { value: 'unknown' }, { value: 'bogus' }, { value: '' }];
  assert.deepEqual(ui.clientKeyOptions([], [], '', previous).map((o) => o.value), ['', 'unknown', a, c]);
  assert.deepEqual(ui.clientKeyOptions([], [], b, previous).map((o) => o.value), ['', 'unknown', a, b, c]);
  assert.deepEqual(ui.clientKeyOptions([], [], '', null).map((o) => o.value), ['', 'unknown']);
});

// Auth files show "label（filename）" so a shared label cannot hide which file was used;
// only nonsecret display fields are retained, never source/path/id/secret values.
test('认证文件显示标签与文件名，同标签不重复且不覆盖文件名', () => {
  const a = '3'.repeat(16);
  const b = '4'.repeat(16);
  const same = '5'.repeat(16);
  const bare = '6'.repeat(16);
  const mem = '7'.repeat(16);
  const evil = '8'.repeat(16);
  const names = ui.credentialNames({ files: [
    {
      auth_index: a, name: '/etc/cpa/auth/fake-file-a.json', label: '工作账户',
      source: 'file', path: '/secret/path', id: 'real-id', api_key: 'sk-secret'
    },
    { auth_index: b, name: 'fake-file-b.json', label: '工作账户' },
    { auth_index: same, name: 'plain.json', label: 'plain.json' },
    { auth_index: bare, name: 'no-label.json' },
    { auth_index: mem, name: 'memory-credential', label: '仅标签' },
    { auth_index: evil, name: '<img src=x onerror=alert(1)>.json', label: '<b>粗</b>' },
    { auth_index: 'not-hex', name: 'ignored.json', label: 'x' }
  ] });
  assert.equal(names[a], '工作账户（fake-file-a.json）');
  assert.equal(names[b], '工作账户（fake-file-b.json）');
  assert.notEqual(names[a], names[b]);
  assert.equal(names[same], 'plain.json');
  assert.equal(names[bare], 'no-label.json');
  assert.equal(names[mem], '仅标签');
  assert.equal(names[evil], '<b>粗</b>（<img src=x onerror=alert(1)>.json）');
  assert.equal(Object.keys(names).length, 6);
  const dumped = JSON.stringify(names);
  ['sk-secret', '/secret/path', 'real-id', 'memory-credential'].forEach((leak) => {
    assert.equal(dumped.includes(leak), false);
  });
});

test('truncateText 截断超长字符串并保留短字符串', () => {
  assert.equal(ui.truncateText('short', 12), 'short');
  assert.equal(ui.truncateText('123456789012', 12), '123456789012');
  assert.equal(ui.truncateText('1234567890123', 12), '123456789012…');
  assert.equal(ui.truncateText('2921300b7afb00000000', 12), '2921300b7afb…');
  assert.equal(ui.truncateText('fengwk94@gmail.com（opencode.json）', 12), 'fengwk94@gma…');
  assert.equal(ui.truncateText('', 12), '');
  assert.equal(ui.truncateText(null, 12), '');
});

test('renderRequestsTable 简化模型列并截断 key/auth 且保留 hover title', () => {
  const doc = new FakeDocument();
  const tbody = doc.createElement('tbody');
  const fullKey = '2921300b7afb' + '0'.repeat(52);
  const fullCred = 'fengwk94@gmail.com（opencode-go-0ba95305ffa1ba700d8f63765ced53a2ef95eddecf70577fa0d88f01605f6210.json）';
  const items = [
    {
      request_id: 'r1',
      time: '2026-10-09T16:13:48Z',
      model: 'opencode-go/deepseek-v4.1-flash',
      alias: 'deepseek-v4.1-flash',
      accounting_quality: 'unclassified',
      stream: true,
      provider: 'opencode-go',
      client_key_id: fullKey,
      auth_index: '1'.repeat(16)
    }
  ];
  const credentials = { ['1'.repeat(16)]: fullCred };
  ui.renderRequestsTable(doc, tbody, items, null, credentials, null);

  const row = tbody.childNodes[0];
  const cells = row.childNodes;
  // 1. Model cell: plain model text, no alias/accounting/stream stacking; the title
  // always carries the full model and appends a differing alias on a new line.
  const modelCell = cells[1];
  assert.equal(modelCell.textContent, 'opencode-go/deepseek-v4.1-flash');
  assert.equal(modelCell.title, 'opencode-go/deepseek-v4.1-flash\n别名 deepseek-v4.1-flash');
  assert.ok(!collectText(modelCell).some((t) => t.includes('核算')));
  assert.ok(!collectText(modelCell).some((t) => t.includes('流式')));

  // 2. 客户端 key 列：纯文本展示 12 位前缀 + …，完整 64 位存入 title，无 button 元素
  const keyCell = cells[3];
  assert.equal(keyCell.textContent, '2921300b7afb…');
  assert.equal(keyCell.title, fullKey);
  assert.equal(findNodes(keyCell, (n) => n.tagName === 'button').length, 0);
  assert.ok(keyCell.className.includes('cell-clickable'));
  assert.ok(keyCell.className.includes('cell-truncate'));

  // 3. auth 列：展示 12 位前缀 + …，完整凭据存入 title
  const authCell = cells[4];
  assert.equal(authCell.textContent, 'fengwk94@gma…');
  assert.equal(authCell.title, fullCred);
  assert.ok(authCell.className.includes('cell-truncate'));

  // 3b. Without a differing alias the title is exactly the full model; the cell
  // stays plain text and renders no button.
  const tbodyPlain = doc.createElement('tbody');
  ui.renderRequestsTable(doc, tbodyPlain, [
    { request_id: 'r2', model: 'opencode-go/full-model-with-no-alias', provider: 'opencode-go' }
  ], null, {}, null);
  const plainModelCell = tbodyPlain.childNodes[0].childNodes[1];
  assert.equal(plainModelCell.textContent, 'opencode-go/full-model-with-no-alias');
  assert.equal(plainModelCell.title, 'opencode-go/full-model-with-no-alias');
  assert.equal(findNodes(plainModelCell, (n) => n.tagName === 'button').length, 0);

  // 4. 点击 key 单元格依然触发 onKey 筛选
  let clickedKey = null;
  const tbody2 = doc.createElement('tbody');
  ui.renderRequestsTable(doc, tbody2, items, null, credentials, (k) => { clickedKey = k; });
  tbody2.childNodes[0].childNodes[3].dispatch('click');
  assert.equal(clickedKey, fullKey);
});
