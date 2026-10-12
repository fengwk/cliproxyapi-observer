'use strict';

// Local mock only: verify weighted cache rates, coverage, invalid/zero input
// fallbacks and three-theme desktop/mobile layout in real Chromium.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');

const OUT = process.argv[2];
if (!OUT) throw new Error('output directory required');
const CHROMIUM_PATH = process.env.CHROMIUM_PATH || '/usr/bin/chromium';

const results = [];
const browserErrors = [];

function record(name, ok, detail) {
  results.push({ name, ok, detail: detail || '' });
  const mark = ok ? 'PASS' : 'FAIL';
  console.log(`[${mark}] ${name}${detail ? ' — ' + detail : ''}`);
}

async function withStep(name, fn) {
  try {
    const detail = await fn();
    record(name, true, detail);
  } catch (err) {
    record(name, false, err && err.message ? err.message : String(err));
    throw err;
  }
}

function watchErrors(page) {
  page.on('dialog', (d) => { browserErrors.push('Forbidden native dialog: ' + d.message()); });
  page.on('pageerror', (err) => { browserErrors.push('Pageerror: ' + err.message); });
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      browserErrors.push('Console error: ' + msg.text());
    }
  });
}

async function connect(page, secret) {
  await page.locator('#mgmt-key').fill(secret);
  await page.locator('#connect').click();
  await page.waitForFunction(() => {
    const el = document.getElementById('conn-status');
    return el && el.textContent.startsWith('已更新');
  }, null, { timeout: 15000 });
}

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const server = await mock.startServer();
  const browser = await chromium.launch({
    executablePath: CHROMIUM_PATH,
    args: ['--no-sandbox', '--disable-dev-shm-usage']
  });

  try {
    // -------------------------------------------------------------------------
    // 阶段 1：真实 mock 数据下的指标渲染精确核对
    // -------------------------------------------------------------------------
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    watchErrors(page);
    await page.goto(server.resourceURL, { waitUntil: 'load' });
    await connect(page, mock.SECRET);

    await withStep('概览主卡：Token 缓存命中率与完整核算覆盖', async () => {
      const card = page.locator('[data-metric="cache"]');
      const label = await card.locator('.metric-label').textContent();
      assert.equal(label.trim(), '缓存命中率（Token）');

      const value = await card.locator('.metric-value').textContent();
      // 真实 token 比率: 2,100,000 / 4,130,000 = 50.8%
      assert.equal(value.trim(), '50.8%');

      const sub = await card.locator('.metric-sub').textContent();
      assert.match(sub, /读 2\.35M \/ 写 234\.6K/);
      assert.match(sub, /完整核算 1,082\/1,234 请求/);

      const title = await card.getAttribute('title');
      assert.match(title, /缓存命中率（Token）= Σ完整cache_read \/ Σ完整input/);
      assert.match(title, /2,100,000\/4,130,000/);
      return `value=${value.trim()}, sub=${sub.trim()}`;
    });

    await withStep('模型分组表：主 rate 为 token 公式且换行显示完整核算覆盖', async () => {
      // 表头检查
      const header = page.locator('table').first().locator('th').nth(8);
      assert.equal((await header.textContent()).trim(), '缓存命中率（Token）');
      assert.match(await header.getAttribute('title'), /Σ完整cache_read \/ Σ完整input/);

      const rows = page.locator('#groups-body tr');
      const rowCount = await rows.count();
      assert.equal(rowCount, 4);

      // gpt-5.1-codex (640/640 complete): 1,200,000 / 2,520,000 = 47.6%
      const gptRow = rows.nth(0);
      const gptCell = gptRow.locator('td').nth(8);
      assert.match(await gptCell.textContent(), /47\.6%/);
      assert.match(await gptCell.locator('.muted').textContent(), /完整核算 640\/640 请求/);

      // claude-opus-4-1 (322/322 complete): 900,000 / 1,490,000 = 60.4%
      const claudeRow = rows.nth(1);
      const claudeCell = claudeRow.locator('td').nth(8);
      assert.match(await claudeCell.textContent(), /60\.4%/);
      assert.match(await claudeCell.locator('.muted').textContent(), /完整核算 322\/322 请求/);

      // unknown-vendor (120/120 complete, read 0, input 120,000): 0.0%
      const unknownRow = rows.nth(3);
      const unknownCell = unknownRow.locator('td').nth(8);
      assert.match(await unknownCell.textContent(), /0\.0%/);
      assert.match(await unknownCell.locator('.muted').textContent(), /完整核算 120\/120 请求/);

      // gemini-3-pro (unclassified): complete=0, input=0, read=0 => 显示“—”，绝非 0.0%
      const geminiRow = rows.nth(2);
      const geminiCell = geminiRow.locator('td').nth(8);
      const geminiRate = (await geminiCell.locator('span').first().textContent()).trim();
      assert.equal(geminiRate, '—');
      assert.match(await geminiCell.locator('.muted').textContent(), /完整核算 0\/152 请求/);
      assert.match(await geminiCell.getAttribute('title'), /无输入 Token/);
      return '4 模型行 rate 与可见覆盖全部正确匹配';
    });

    await withStep('Key 分组表：切换客户端 key / auth，可见换行覆盖与公式 title', async () => {
      // 客户端 key 维度
      const keyHeader = page.locator('#keys-heading').locator('..').locator('..').locator('th').nth(4);
      assert.match(await keyHeader.textContent(), /缓存读 \/ 写 · 缓存命中率（Token）/);

      const clientRows = page.locator('#keys-body tr');
      const clientCount = await clientRows.count();
      assert.equal(clientCount, 3);
      const keyCell = clientRows.nth(0).locator('td').nth(4);
      assert.match(await keyCell.textContent(), /50\.8%/);
      assert.match(await keyCell.locator('.muted').textContent(), /完整核算 614\/700 请求/);
      assert.match(await keyCell.getAttribute('title'), /Σ完整cache_read \/ Σ完整input/);

      // 切换到 auth 维度
      await page.locator('#key-group-kind').selectOption('auth');
      await page.waitForFunction(() => document.querySelectorAll('#keys-body tr').length === 3);
      const authCell = page.locator('#keys-body tr').nth(0).locator('td').nth(4);
      assert.match(await authCell.textContent(), /50\.8%/);
      assert.match(await authCell.locator('.muted').textContent(), /完整核算 614\/700 请求/);
      return '客户端 key 与 auth 两维度聚合 rate 与覆盖结构一致';
    });

    await withStep('请求明细表：单条缓存读占比语义与公式 tooltip', async () => {
      const th = page.locator('#requests-body').locator('..').locator('th').nth(6);
      assert.equal((await th.textContent()).trim(), '缓存读占比');
      assert.match(await th.getAttribute('title'), /cache_read_tokens \/ input_tokens/);

      // 第 0 条为 unclassified 记录：展示“—”且 title 标明核算质量不支持
      const row0Share = page.locator('#requests-body tr').nth(0).locator('td').nth(6);
      assert.equal((await row0Share.textContent()).trim(), '—');
      assert.equal(await row0Share.getAttribute('title'), '核算质量不支持缓存读占比');

      // 第 1 条为 complete 记录：展示具体百分比且 title 说明单条公式
      const row1Share = page.locator('#requests-body tr').nth(1).locator('td').nth(6);
      assert.match(await row1Share.getAttribute('title'), /cache_read_tokens \/ input_tokens/);
      return '请求表头与单元格语义统一，单条完整与未分类分别正确显示';
    });

    await context.close();

    // -------------------------------------------------------------------------
    // 阶段 2：Route Override 模拟边界：zero input / invalid / 部分核算污染
    // -------------------------------------------------------------------------
    const edgeContext = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const edgePage = await edgeContext.newPage();
    watchErrors(edgePage);

    await withStep('边界 1：zero input => ratio 为“—”，不当作 0% 未命中', async () => {
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.totals.cache_accounting = { requests: 5, input_tokens: 0, cache_read_tokens: 0 };
        json.groups[0].cache_accounting = { requests: 5, input_tokens: 0, cache_read_tokens: 0 };
        await route.fulfill({ json });
      });

      await edgePage.goto(server.resourceURL, { waitUntil: 'load' });
      await connect(edgePage, mock.SECRET);

      const card = edgePage.locator('[data-metric="cache"]');
      const value = (await card.locator('.metric-value').textContent()).trim();
      assert.equal(value, '—');
      const sub = await card.locator('.metric-sub').textContent();
      assert.match(sub, /完整核算 5\/1,234 请求/);

      const groupRate = (await edgePage.locator('#groups-body tr td').nth(8).locator('span').first().textContent()).trim();
      assert.equal(groupRate, '—');
      return 'zero input 严格降级为“—”且保留覆盖分子分母';
    });

    await withStep('边界 2：cache_accounting 为 null / missing / 不一致 => 降级为未知', async () => {
      // 场景 2a: null
      await edgePage.unroute('**/summary*');
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.totals.cache_accounting = null;
        await route.fulfill({ json });
      });
      await edgePage.locator('#refresh').click();
      await edgePage.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));

      const cardNull = edgePage.locator('[data-metric="cache"]');
      assert.equal((await cardNull.locator('.metric-value').textContent()).trim(), '—');
      assert.match(await cardNull.locator('.metric-sub').textContent(), /完整核算未知/);
      assert.match(await cardNull.getAttribute('title'), /完整核算数据缺失或不一致/);

      // 场景 2b: read > input 违反契约
      await edgePage.unroute('**/summary*');
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.totals.cache_accounting = { requests: 10, input_tokens: 50, cache_read_tokens: 100 };
        await route.fulfill({ json });
      });
      await edgePage.locator('#refresh').click();
      await edgePage.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
      const cardReadGt = edgePage.locator('[data-metric="cache"]');
      assert.equal((await cardReadGt.locator('.metric-value').textContent()).trim(), '—');
      assert.match(await cardReadGt.locator('.metric-sub').textContent(), /完整核算未知/);

      // 场景 2c: completeRequests == 0 但存在非零 tokens 违反契约
      await edgePage.unroute('**/summary*');
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.totals.cache_accounting = { requests: 0, input_tokens: 100, cache_read_tokens: 0 };
        await route.fulfill({ json });
      });
      await edgePage.locator('#refresh').click();
      await edgePage.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
      const cardZeroReqTokens = edgePage.locator('[data-metric="cache"]');
      assert.equal((await cardZeroReqTokens.locator('.metric-value').textContent()).trim(), '—');
      assert.match(await cardZeroReqTokens.locator('.metric-sub').textContent(), /完整核算未知/);

      // 场景 2d: completeRequests > source.requests
      await edgePage.unroute('**/summary*');
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        json.totals.cache_accounting = { requests: 9999, input_tokens: 1000, cache_read_tokens: 100 };
        await route.fulfill({ json });
      });
      await edgePage.locator('#refresh').click();
      await edgePage.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
      const cardBadCoverage = edgePage.locator('[data-metric="cache"]');
      assert.equal((await cardBadCoverage.locator('.metric-value').textContent()).trim(), '—');
      assert.match(await cardBadCoverage.locator('.metric-sub').textContent(), /完整核算未知/);
      return '4 类畸形/不一致数据均拒绝并安全降级为未知';
    });

    await withStep('边界 3：部分核算且原始 read 污染 => 仅使用 complete partition', async () => {
      await edgePage.unroute('**/summary*');
      await edgePage.route('**/summary*', async (route) => {
        const response = await route.fetch();
        const json = await response.json();
        // 原始字段污染：原始 cache_read 占 90%
        json.totals.input_tokens = 10000000;
        json.totals.cache_read_tokens = 9000000;
        // 实际 self-consistent 完整核算集合：80 / 1000 = 8.0%
        json.totals.cache_accounting = { requests: 2, input_tokens: 1000, cache_read_tokens: 80 };
        await route.fulfill({ json });
      });
      await edgePage.locator('#refresh').click();
      await edgePage.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));

      const card = edgePage.locator('[data-metric="cache"]');
      const val = (await card.locator('.metric-value').textContent()).trim();
      assert.equal(val, '8.0%', '必须准确计算 80/1000=8.0%，绝不能使用被污染的 90% 原始字段');
      const sub = await card.locator('.metric-sub').textContent();
      assert.match(sub, /完整核算 2\/1,234 请求/);
      return '部分核算精确隔离未分类记录污染';
    });

    await edgeContext.close();

    // -------------------------------------------------------------------------
    // 阶段 3：多主题 (light / white / dark) × 1280px & 390px 截图与溢出检查
    // -------------------------------------------------------------------------
    for (const width of [1280, 390]) {
      for (const theme of ['light', 'white', 'dark']) {
        await withStep(`视觉验证与截图：${theme} ${width}px`, async () => {
          const colorScheme = theme === 'dark' ? 'dark' : 'light';
          const ctx = await browser.newContext({
            viewport: { width, height: 900 },
            colorScheme
          });
          const p = await ctx.newPage();
          watchErrors(p);
          await p.goto(server.resourceURL, { waitUntil: 'load' });
          await connect(p, mock.SECRET);

          if (theme === 'dark') {
            await p.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
          } else if (theme === 'white') {
            await p.evaluate(() => document.documentElement.setAttribute('data-theme', 'white'));
          } else {
            await p.evaluate(() => document.documentElement.removeAttribute('data-theme'));
          }

          // 等待主题过渡和布局稳定，避免截图落在颜色的中间帧。
          await p.evaluate(() => new Promise((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(resolve))));
          await p.waitForFunction(() => document.getAnimations().every((a) => a.playState !== 'running'));

          // 严格检测页面横向溢出
          const overflow = await p.evaluate(() =>
            document.documentElement.scrollWidth - document.documentElement.clientWidth);
          assert.ok(overflow <= 1, `${width}px 下产生横向溢出 ${overflow}px`);

          const shotName = `cache-rate-${theme}-${width}.png`;
          const shotPath = path.join(OUT, shotName);
          await p.screenshot({ path: shotPath, fullPage: true });

          await ctx.close();
          return `已生成 ${shotName} (overflow=${overflow})`;
        });
      }
    }

    // -------------------------------------------------------------------------
    // 阶段 4：确认全程 0 异常
    // -------------------------------------------------------------------------
    assert.equal(browserErrors.length, 0, '浏览器捕获到错误：\n' + browserErrors.join('\n'));

    const summaryReport = {
      passed: results.filter((r) => r.ok).length,
      total: results.length,
      evidenceDirectory: OUT,
      results
    };
    fs.writeFileSync(path.join(OUT, 'cache-rate-results.json'), JSON.stringify(summaryReport, null, 2));

    console.log(`\n全部 ${summaryReport.passed}/${summaryReport.total} 项测试通过！证据目录：${OUT}`);
  } finally {
    await browser.close();
    await server.close();
  }
}

main().catch((err) => {
  console.error('Test execution failed:', err);
  process.exit(1);
});
