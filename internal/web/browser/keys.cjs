'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');

// 主题切换会触发按钮/边框 150ms 过渡；截图前先跨越两个渲染帧让过渡启动，再等待
// 文档与同源 iframe 内的动画全部结束，避免截到中间灰阶/低对比帧。
async function settleTransitions(page) {
  await page.evaluate(() => new Promise((resolve) =>
    requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await page.waitForFunction(() => {
    const docs = [document];
    for (const frame of document.querySelectorAll('iframe')) {
      try { if (frame.contentDocument) docs.push(frame.contentDocument); } catch (err) { /* cross-origin */ }
    }
    return docs.every((doc) => doc.getAnimations().every((a) => a.playState !== 'running'));
  });
}

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
  // identity-apply 在刷新/翻页/保存中禁用；等待其重新可用可避免与在途刷新竞态。
  const idle = (page) => page.waitForFunction(() => !document.getElementById('identity-apply').disabled);
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

    // 1a. 客户端 key 是原生下拉：连接后默认「全部」并含「未归属」，不再有手填 input/datalist。
    assert.equal(await page.locator('#filter-client-key').evaluate((el) => el.tagName), 'SELECT');
    assert.equal(await page.locator('input#filter-client-key, datalist').count(), 0);
    assert.equal(await page.locator('#filter-client-key').inputValue(), '');
    assert.deepEqual(
      (await page.locator('#filter-client-key option').evaluateAll((opts) => opts.map((o) => [o.value, o.textContent]))).slice(0, 2),
      [['', '全部'], ['unknown', '未归属（含旧记录）']]
    );

    // 1. 上游凭据展示短名称截断，title 暴露完整凭据名称；缓存命中率带分子分母并显式标注。
    assert.match(await page.locator('#requests-body').textContent(), /工作账户（fake-fi…/);
    assert.match(await page.locator('#requests-body').textContent(), /备用账户（fake-fi…/);
    assert.ok(await page.locator('#requests-body td[title="工作账户（fake-file-a.json）"]').count() > 0);
    assert.ok(await page.locator('#requests-body td[title="备用账户（fake-file-b.json）"]').count() > 0);
    assert.match(await page.locator('[data-metric="cache"]').textContent(), /请求命中率.*812\/1,234/);
    results.push('current CPA labels with auth file names and explicit request-level cache ratio');

    // 2. 请求表点击完整指纹：只显示 12 位前缀，筛选使用完整 64 位。
    const keyCell = page.locator('#requests-body td.cell-clickable').first();
    const fingerprint = await keyCell.getAttribute('title');
    assert.equal(fingerprint.length, 64);
    assert.equal((await keyCell.textContent()).length, 13);
    await keyCell.click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 26);
    assert.equal(await page.locator('#filter-client-key').inputValue(), fingerprint);
    // 下拉只显示 12 位前缀，但选中项的 value 与 title 都是请求实际使用的完整 64 位指纹。
    assert.equal(await page.locator('#filter-client-key option:checked').textContent(), fingerprint.slice(0, 12) + '…');
    assert.equal(await page.locator('#filter-client-key').evaluate((el) => el.selectedOptions[0].title), fingerprint);
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
    await page.locator('#filter-client-key').selectOption('');
    await page.locator('#filter-auth').selectOption('2'.repeat(16));
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => {
      const rows = Array.from(document.querySelectorAll('#requests-body tr'));
      return rows.length === 25 && rows.every((row) => row.children[4]?.title === '备用账户（fake-file-b.json）' &&
        row.children[4]?.textContent === '备用账户（fake-fi…');
    });
    await waitMetric(page, 'requests', '530');
    results.push('auth file selection filters by nonsecret auth_index');

    // 4. 未归属桶：空 id 表示旧记录；key 筛选必须传播到 summary。
    await page.locator('#filter-client-key').selectOption('unknown');
    await page.locator('#filter-auth').selectOption('unknown');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 1);
    assert.match(await page.locator('#requests-body').textContent(), /未归属/);
    await waitMetric(page, 'requests', '4');
    results.push('legacy unknown bucket: key filters reach the summary');

    // 5. 应用筛选重置 offset 与分页。
    await page.locator('#filter-client-key').selectOption('');
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
    await page.locator('#filter-client-key').selectOption('');
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
      return rows.length === 25 && rows.every((row) => row.children[4]?.title === '同名（dup-two.json）' &&
        row.children[4]?.textContent === '同名（dup-two.j…');
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
      return rows.length === 26 && rows.every((row) => row.children[4]?.title === '索引 ' + '1'.repeat(16) &&
        row.children[4]?.textContent === '索引 ' + '1'.repeat(9) + '…');
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
      return rows.length === 26 && rows.some((row) => row.textContent.includes('<script>evil…'));
    });
    assert.match(await page.locator('#requests-body').textContent(),
      /<script>evil…/);
    assert.ok(await page.locator('#requests-body td[title="<script>evil</script>（<img src=x onerror=alert(1)>.json）"]').count() > 0);
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

    // 10. 统一筛选工具栏：单一卡片、同一控件容器；桌面一行底部对齐；无表单/命名序列化入口。
    assert.equal(await page.locator('section[aria-label="筛选条件"]').count(), 1);
    assert.equal(await page.locator('section[aria-label="Key 筛选"]').count(), 0);
    assert.equal((await page.locator('body').textContent()).includes('Key 筛选影响概览'), false);
    assert.equal(await page.evaluate(() => {
      const container = document.querySelector('section[aria-label="筛选条件"] .controls');
      const seg = document.querySelector('section[aria-label="筛选条件"] .seg');
      const members = ['filter-provider', 'filter-model', 'filter-client-key', 'filter-auth', 'identity-apply'];
      return !!container && !!seg && seg.closest('.controls') === container &&
        members.every((id) => document.getElementById(id)?.closest('.controls') === container);
    }), true);
    assert.equal(await page.locator('form').count(), 0);
    assert.equal(await page.locator('input[type="password"]').count(), 1);
    assert.equal(await page.locator('[name="client_key_id"], [name="auth_index"]').count(), 0);
    const bottomEdges = () => page.evaluate(() => Array.from(
      document.querySelector('section[aria-label="筛选条件"] .controls').children
    ).map((el) => Math.round(el.getBoundingClientRect().bottom)));
    const desktopBottoms = await bottomEdges();
    assert.ok(Math.max(...desktopBottoms) - Math.min(...desktopBottoms) <= 1,
      'desktop controls should align on a single row');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    results.push('single consolidated filter toolbar, one aligned desktop row, no form serialization');

    // 10b. 共享按钮契约：所有按钮带 .btn；常规控件 46px/16px/600/10×14/半径 8，
    // 紧凑 btn-sm 39px/14px/600/8×10；段控保留 .btn 基础（同字号字重、拼接圆角）；
    // 禁用态与键盘焦点可见轮廓符合 Management Center 设计语言。
    const contract = await page.evaluate(() => {
      const round = (v) => Math.round(parseFloat(v));
      const snap = (el) => {
        const s = getComputedStyle(el);
        const r = el.getBoundingClientRect();
        return {
          hasBtn: el.classList.contains('btn'),
          fontSize: round(s.fontSize),
          fontWeight: s.fontWeight,
          padTop: round(s.paddingTop),
          padRight: round(s.paddingRight),
          radius: round(s.borderTopLeftRadius),
          height: Math.round(r.height),
          opacity: s.opacity,
          cursor: s.cursor
        };
      };
      const normals = ['#connect', '#refresh', '#identity-apply', '#settings-save',
        '#settings-revert', '#requests-prev', '#requests-next']
        .map((sel) => ({ sel, ...snap(document.querySelector(sel)) }));
      const smalls = Array.from(document.querySelectorAll('#requests-body button, #keys-body button'))
        .slice(0, 4).map((el) => ({ text: el.textContent, ...snap(el) }));
      const segments = Array.from(document.querySelectorAll('.seg-btn')).map((el) => ({
        cls: el.className,
        groupHeight: Math.round(el.parentElement.getBoundingClientRect().height),
        ...snap(el)
      }));
      const prev = document.querySelector('#requests-prev');
      return { normals, smalls, segments, disabled: { flag: prev.disabled, ...snap(prev) } };
    });
    for (const b of contract.normals) {
      assert.equal(b.hasBtn, true, b.sel + ' 必须带 .btn');
      assert.equal(b.fontSize, 16, b.sel + ' 常规字号 16px');
      assert.equal(b.fontWeight, '600', b.sel + ' 常规字重 600');
      assert.equal(b.padTop, 10, b.sel + ' 常规纵向内边距 10px');
      assert.equal(b.padRight, 14, b.sel + ' 常规横向内边距 14px');
      assert.equal(b.radius, 8, b.sel + ' 圆角 8px');
      assert.equal(b.height, 46, b.sel + ' 常规高度 46px');
    }
    assert.ok(contract.smalls.length > 0, '应存在紧凑操作按钮');
    for (const b of contract.smalls) {
      assert.equal(b.hasBtn, true, '紧凑按钮必须带 .btn');
      assert.equal(b.fontSize, 14);
      assert.equal(b.fontWeight, '600');
      assert.equal(b.padTop, 8);
      assert.equal(b.padRight, 10);
      assert.equal(b.radius, 8);
      assert.equal(b.height, 39);
    }
    assert.equal(contract.segments.length, 3);
    for (const b of contract.segments) {
      assert.equal(b.hasBtn, true, '段控必须保留 .btn');
      assert.equal(b.fontSize, 16, '段控字号与 .btn 一致');
      assert.equal(b.fontWeight, '600', '段控字重与 .btn 一致');
      assert.equal(b.padTop, 10);
      assert.equal(b.padRight, 14);
      assert.equal(b.radius, 0, '段控拼接圆角');
      assert.equal(b.groupHeight, 46, '段控组高 46px');
    }
    assert.equal(contract.disabled.flag, true, '#requests-prev 第一页应为禁用');
    assert.equal(contract.disabled.opacity, '0.6', '禁用态降低不透明度');
    assert.equal(contract.disabled.cursor, 'not-allowed', '禁用态使用 not-allowed 光标');

    // 键盘焦点必须显示可见轮廓（按 .btn:focus-visible 契约）。
    await page.locator('#filter-auth').focus();
    await page.keyboard.press('Tab');
    const focusState = await page.evaluate(() => {
      const el = document.activeElement;
      const s = getComputedStyle(el);
      return { id: el.id, visible: el.matches(':focus-visible'),
        width: s.outlineWidth, style: s.outlineStyle, offset: s.outlineOffset, color: s.outlineColor };
    });
    assert.equal(focusState.id, 'identity-apply', 'Tab 应落到下一个控件');
    assert.equal(focusState.visible, true, '按钮键盘焦点可见');
    assert.equal(focusState.width, '2px');
    assert.equal(focusState.style, 'solid');
    assert.equal(focusState.offset, '3px');
    assert.notEqual(focusState.color, 'rgba(0, 0, 0, 0)', '焦点轮廓必须可见');

    // 主按钮悬停改变背景（语义色 hover 令牌生效）。
    const hoverBefore = await page.locator('#connect').evaluate((el) => getComputedStyle(el).backgroundColor);
    await page.locator('#connect').hover();
    await page.waitForFunction((before) =>
      getComputedStyle(document.getElementById('connect')).backgroundColor !== before, hoverBefore);
    const hoverAfter = await page.locator('#connect').evaluate((el) => getComputedStyle(el).backgroundColor);
    assert.notEqual(hoverAfter, hoverBefore, '主按钮悬停背景应变化');
    await page.mouse.move(0, 0);
    results.push('shared .btn contract: normal/small/segment geometry, disabled and focus states');

    // 11. 超长认证文件名与超长模型选项不得撑破布局或页面横向溢出。
    server.controls.credentialFiles = [
      { auth_index: mock.AUTH_INDEX_A, name: 'fake-file-' + 'x'.repeat(48) + '.json', label: '长文件名账户' },
      { auth_index: mock.AUTH_INDEX_B, name: 'fake-file-b.json', label: '备用账户' }
    ];
    await page.locator('#refresh').click();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('#filter-auth option'))
      .some((o) => o.textContent.startsWith('长文件名账户（fake-file-')));
    await page.locator('#filter-auth').selectOption(mock.AUTH_INDEX_A);
    await page.evaluate(() => {
      const model = document.getElementById('filter-model');
      const opt = document.createElement('option');
      opt.value = 'm'.repeat(72);
      opt.textContent = 'm'.repeat(72);
      model.appendChild(opt);
      model.value = opt.value;
    });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.evaluate(() => {
      const model = document.getElementById('filter-model');
      model.value = '';
      Array.from(model.options).forEach((o) => { if (o.value.length > 64) o.remove(); });
    });
    // 恢复默认假认证文件与全量结果，供后续响应式与截图使用。
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
    results.push('long auth filename and long model option keep the layout overflow-free');

    // 11b. 空结果：key=A 与 auth=B 交集为空，summary/requests 真实为空；已选完整指纹仍须保留。
    let stepCalls = calls.length;
    await page.locator('#filter-client-key').selectOption(mock.CLIENT_KEY_A);
    await page.locator('#filter-auth').selectOption(mock.AUTH_INDEX_B);
    await page.locator('#identity-apply').click();
    await idle(page);
    await waitMetric(page, 'requests', '0');
    assert.equal(await page.locator('#requests-body tr.empty-row').count(), 1);
    assert.equal((await page.locator('#requests-body tr.empty-row').textContent()).trim(), '暂无数据');
    const emptyCalls = calls.slice(stepCalls);
    assert.equal(emptyCalls.some((url) => url.pathname.endsWith('/summary') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_A &&
      url.searchParams.get('auth_index') === mock.AUTH_INDEX_B), true);
    assert.equal(emptyCalls.some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_A &&
      url.searchParams.get('auth_index') === mock.AUTH_INDEX_B && url.searchParams.get('offset') === '0'), true);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_A);
    assert.equal(await page.locator('#filter-client-key option[value="' + mock.CLIENT_KEY_A + '"]').count(), 1);
    assert.equal(await page.locator('#filter-client-key option:checked').textContent(), mock.CLIENT_KEY_A.slice(0, 12) + '…');

    // 11c. 刷新与时间范围切换后保持同一选中指纹，且仍按该完整指纹查询。
    stepCalls = calls.length;
    await page.locator('#refresh').click();
    await idle(page);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_A);
    await page.locator('#filter-auth').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 26);
    await idle(page);
    stepCalls = calls.length;
    await page.locator('.seg-btn[data-range="7d"]').click();
    await idle(page);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_A);
    assert.equal(calls.slice(stepCalls).some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_A), true);
    results.push('empty result, refresh and time range keep the selected full fingerprint and issue the applied query');

    // 11d. 从已应用的 A 直接切到 B：同一连接已发现的候选保留，query 用完整 B 指纹。
    assert.equal(await page.locator('#filter-client-key option[value="' + mock.CLIENT_KEY_B + '"]').count(), 1);
    stepCalls = calls.length;
    await page.locator('#filter-client-key').selectOption(mock.CLIENT_KEY_B);
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 25);
    await idle(page);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_B);
    assert.equal(await page.locator('#filter-client-key option[value="' + mock.CLIENT_KEY_B + '"]').count(), 1);
    const switchCalls = calls.slice(stepCalls);
    assert.equal(switchCalls.some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_B && url.searchParams.get('offset') === '0'), true);
    assert.equal(switchCalls.some((url) => url.pathname.endsWith('/summary') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_B), true);
    results.push('switching from key A to key B keeps both candidates and filters by the full B id');

    // 11e. 未应用的新选择在翻页重建选项后仍保留，而查询仍用已应用的过滤条件。
    stepCalls = calls.length;
    await page.locator('#filter-client-key').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 50);
    await idle(page);
    await page.locator('#filter-client-key').selectOption(mock.CLIENT_KEY_B);
    stepCalls = calls.length;
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.includes('第 2 页'));
    await idle(page);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_B);
    assert.equal(calls.slice(stepCalls).some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === null && url.searchParams.get('offset') === '50'), true);
    await page.locator('#requests-prev').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.includes('第 1 页'));
    await idle(page);
    await page.locator('#filter-client-key').selectOption('');
    await page.locator('#identity-apply').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 50);
    await idle(page);
    results.push('pagination keeps the pending selection while the query keeps the applied filter');

    // 11f. 人为移除某指纹的 option，点击请求行身份必须补全并选中完整值，并发出新的完整指纹查询。
    await page.evaluate((key) => {
      const opt = document.querySelector('#filter-client-key option[value="' + key + '"]');
      if (opt) opt.remove();
    }, mock.CLIENT_KEY_A);
    assert.equal(await page.locator('#filter-client-key option[value="' + mock.CLIENT_KEY_A + '"]').count(), 0);
    stepCalls = calls.length;
    await page.locator(`#requests-body td[title="${mock.CLIENT_KEY_A}"]`).first().click();
    await idle(page);
    assert.equal(await page.locator('#filter-client-key').inputValue(), mock.CLIENT_KEY_A);
    assert.equal(await page.locator('#filter-client-key option[value="' + mock.CLIENT_KEY_A + '"]').count(), 1);
    assert.equal(calls.slice(stepCalls).some((url) => url.pathname.endsWith('/requests') &&
      url.searchParams.get('client_key_id') === mock.CLIENT_KEY_A && url.searchParams.get('offset') === '0'), true);
    results.push('clicking a request identity restores a missing option and filters by the full fingerprint');

    // 11g. 同一连接新发现的候选（DOM 注入的合法 c 指纹）在刷新后保留，但重连后必须清除。
    await page.evaluate((key) => {
      const sel = document.getElementById('filter-client-key');
      const opt = document.createElement('option');
      opt.value = key;
      opt.textContent = key.slice(0, 12) + '…';
      sel.appendChild(opt);
    }, 'c'.repeat(64));
    await page.locator('#refresh').click();
    await idle(page);
    assert.equal(await page.locator('#filter-client-key option[value="' + 'c'.repeat(64) + '"]').count(), 1);
    await page.locator('#connect').click();
    await idle(page);
    assert.equal(await page.locator('#filter-client-key option[value="' + 'c'.repeat(64) + '"]').count(), 0);
    assert.equal(await page.locator('#filter-client-key').inputValue(), '');
    assert.equal(await page.locator('#filter-auth').inputValue(), '');
    assert.equal(await page.locator('#filter-client-key option').first().textContent(), '全部');
    // 恢复时间范围为默认 24h，供后续响应式与截图使用。
    await page.locator('.seg-btn[data-range="24h"]').click();
    await idle(page);
    results.push('discovered candidates persist within a connection but are cleared on reconnect');

    // 12. 响应式与安全：中等宽度与 390px 自然换行、无横向溢出、纯文本渲染、无页面错误。
    await page.setViewportSize({ width: 768, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.setViewportSize({ width: 390, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    assert.ok(new Set(await bottomEdges()).size > 1, 'narrow layout should wrap naturally into multiple rows');
    assert.equal(await page.locator('#requests-body img, #requests-body script').count(), 0);
    assert.equal(await page.locator('#keys-body img, #keys-body script').count(), 0);
    assert.deepEqual(errors, []);
    await settleTransitions(page);
    await page.screenshot({ path: path.join(out, 'keys-390.png'), fullPage: true });

    // 13. 三套主题在桌面与移动端的筛选区近景截图，逐一确认无横向溢出。
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const theme of ['light', 'white', 'dark']) {
        await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
        await page.waitForFunction((t) => document.documentElement.getAttribute('data-theme') === t, theme);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
        await settleTransitions(page);
        await page.locator('section[aria-label="筛选条件"]').screenshot({ path: path.join(out, 'filter-' + theme + '-' + width + '.png') });
      }
    }
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.evaluate(() => document.documentElement.removeAttribute('data-theme'));
    await settleTransitions(page);
    await page.screenshot({ path: path.join(out, 'keys-1280.png'), fullPage: true });
    results.push('theme close-ups and mobile layout, text-only rendering and error-free interactions');
  } finally {
    await browser.close();
    await server.close();
  }
  fs.writeFileSync(path.join(out, 'key-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}

main().catch((err) => { console.error(err); process.exitCode = 1; });
