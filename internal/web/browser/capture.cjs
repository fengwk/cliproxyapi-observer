'use strict';

/*
 * Observer 面板浏览器验证脚本（Playwright）。
 *
 * 用法：
 *   node internal/web/browser/capture.cjs <输出目录>
 * 环境变量：
 *   CHROMIUM_PATH  指定 Chromium 可执行文件（默认 /usr/bin/chromium）
 *
 * 覆盖：
 *   - 嵌入宿主（同源 iframe）浅色 / 纯白 / 深色 + 实时主题切换；
 *   - 390px 窄屏布局与无横向溢出；
 *   - 独立打开时的系统偏好回退；
 *   - 恶意模型名 / 请求体 XSS 不落地为元素；
 *   - 请求体懒加载 + 二次确认 + 关闭清空 + 列表刷新不触发请求体读取；
 *   - 切换筛选时的过期响应丢弃（旧响应不得覆盖新结果）；
 *   - 无浏览器密钥持久化、密钥不进 URL、以 Authorization 头发送；
 *   - 捕获关闭时的解释文案。
 *
 * 截图与结果写入输出目录（仓库之外），便于留档审阅。
 */

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const mock = require('./mock-server.cjs');

const OUT = process.argv[2] || path.join(process.env.TMPDIR || '/tmp', 'observer-ui-evidence');
const CHROMIUM_PATH = process.env.CHROMIUM_PATH || '/usr/bin/chromium';

const results = [];
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};
const ready = (page) => page.waitForFunction(
  () => document.getElementById('conn-status').textContent.startsWith('已更新'));
const watched = new WeakSet();
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
  }
}

async function checkColorScheme(frame, theme) {
  const styles = await frame.evaluate(() => Array.from(
    document.querySelectorAll(':root, body, .table-scroll, dialog.dialog, .dialog-body, select'),
    (el) => ({ tag: el.tagName, scheme: getComputedStyle(el).colorScheme })
  ));
  const expected = theme === 'dark' ? 'dark' : 'light';
  for (const style of styles) {
    assert.equal(style.scheme, expected, style.tag + ' native color-scheme');
  }
}

async function appFrame(page, origin) {
  await page.waitForFunction(
    (prefix) => Array.from(document.querySelectorAll('iframe')).some((f) => f.src.startsWith(prefix)),
    origin + mock.RESOURCE_BASE,
    { timeout: 15000 }
  );
  return page.frame({ url: (u) => u.href.startsWith(origin + mock.RESOURCE_BASE) });
}

async function connect(frame, secret) {
  const page = typeof frame.page === 'function' ? frame.page() : frame;
  if (!watched.has(page)) {
    watched.add(page);
    page.on('dialog', () => { browserErrors.push('Forbidden native dialog'); });
    page.on('pageerror', (error) => browserErrors.push(error.message));
  }
  await frame.locator('#mgmt-key').fill(secret);
  await frame.locator('#connect').click();
  await frame.locator('[data-metric="requests"] .metric-value').waitFor({ timeout: 15000 });
  await frame.waitForFunction(
    () => {
      const el = document.querySelector('[data-metric="requests"] .metric-value');
      return el && el.textContent && el.textContent.trim() !== '—';
    },
    null,
    { timeout: 15000 }
  );
}

// Use controllable responses to exercise cancellation and credential boundaries.
async function reviewCases(browser, server) {
  const requests = '**' + mock.MANAGEMENT_BASE + '/requests**';
  const bodies = '**' + mock.MANAGEMENT_BASE + '/body**';
  const fresh = async () => {
    const page = await browser.newPage();
    page.on('dialog', (d) => { throw new Error('Forbidden native browser dialog: ' + d.message()); });
    await page.goto(server.resourceURL);
    await connect(page, mock.SECRET);
    await page.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
    return page;
  };
  const bodyOpen = async (page) => {
    await page.locator('[data-request-id="req-body-xss"]').click();
    await page.locator('#body-reveal').click();
  };

  await withStep('Review: cost subtotal and unknown group are explicit', async () => {
    const page = await fresh();
    assert.match(await page.locator('[data-metric="cost"] .metric-sub').textContent(), /已定价小计；未定价/);
    assert.match(await page.locator('#groups-body tr').filter({ hasText: 'gemini-3-pro' }).textContent(), /未定价/);
    assert.ok(!(await page.locator('#groups-body tr').filter({ hasText: 'gemini-3-pro' }).textContent()).includes('$0.0000'));
    await page.screenshot({ path: path.join(OUT, 'review-cost-retention.png'), fullPage: true });
    await page.close();
  });

  await withStep('Review: current-minute requests are included without future to', async () => {
    const page = await browser.newPage();
    const now = Date.UTC(2026, 9, 8, 12, 0, 45, 123);
    await page.addInitScript((value) => { Date.now = () => value; }, now);
    await page.route(requests, (route) => {
      const url = new URL(route.request().url());
      assert.equal(Date.parse(url.searchParams.get('to')), now);
      assert.equal(Date.parse(url.searchParams.get('from')), now - 86400000);
      assert.equal(url.searchParams.has('cursor'), false, '不得发出 cursor 参数');
      assert.equal(url.searchParams.get('offset'), '0', '初始必须请求 offset=0');
      return route.fulfill({ json: { items: [{
        request_id: 'current', model: 'CURRENT-MINUTE', time: new Date(now - 1000).toISOString()
      }], offset: 0, limit: 50, has_more: false } });
    });
    await page.goto(server.resourceURL);
    await connect(page, mock.SECRET);
    await page.locator('#requests-body').filter({ hasText: 'CURRENT-MINUTE' }).waitFor();
    assert.match(await page.locator('#requests-note').textContent(), /请求明细仅保留 1 天.*统计按所选/);
    await page.close();
  });

  await withStep('Review: refresh cancels pending page transition, resets to page 1 and rejects old page data', async () => {
    const page = await fresh();
    const entered = deferred();
    const release = deferred();
    const finished = deferred();
    let first = true;
    await page.route(requests, async (route) => {
      const u = new URL(route.request().url());
      assert.equal(u.searchParams.has('cursor'), false, '不得发出 cursor 参数');
      if (u.searchParams.get('offset') === '50' && first) {
        first = false;
        entered.resolve();
        await release.promise;
        try {
          await route.fulfill({
            json: {
              items: [{ model: 'OLD-OFFSET-50', time: new Date().toISOString(), provider: 'p', total_tokens: 1 }],
              offset: 50,
              limit: 50,
              has_more: false
            }
          });
        } catch (_) { /* aborted client */ }
        finished.resolve();
      } else await route.continue();
    });
    await page.locator('#requests-next').click();
    await entered.promise;
    await page.locator('#refresh').click();
    await ready(page);
    release.resolve();
    await finished.promise;
    assert.equal(await page.locator('#requests-body tr').count(), 50);
    assert.equal(await page.locator('#requests-body').getByText('OLD-OFFSET-50').count(), 0);
    assert.match(await page.locator('#requests-page').textContent(), /1/);
    await page.close();
  });

  await withStep('Review: pagination replaces table rows and never accumulates beyond 50 rows without cursor', async () => {
    const page = await fresh();
    const seenOffsets = [];
    await page.route(requests, (route) => {
      const u = new URL(route.request().url());
      assert.equal(u.searchParams.has('cursor'), false, '严禁包含 cursor 参数');
      const off = parseInt(u.searchParams.get('offset') || '0', 10);
      seenOffsets.push(off);
      const items = Array.from({ length: 50 }, (_, i) => ({
        request_id: 'p-' + off + '-' + i,
        model: 'model-page-' + off,
        time: new Date().toISOString(),
        provider: 'openai',
        total_tokens: 100
      }));
      return route.fulfill({
        json: {
          items,
          offset: off,
          limit: 50,
          has_more: off < 150
        }
      });
    });

    // 初始第 1 页：50 条
    assert.equal(await page.locator('#requests-body tr').count(), 50);

    // 翻到第 2 页：仍为 50 条（替换，不累加至 100 条）
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => {
      const p = document.getElementById('requests-page');
      return p && p.textContent.includes('2');
    });
    assert.equal(await page.locator('#requests-body tr').count(), 50, '翻页必须替换，行数不得累加为 100');

    // 翻到第 3 页：仍为 50 条（不累加至 150 条）
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => {
      const p = document.getElementById('requests-page');
      return p && p.textContent.includes('3');
    });
    assert.equal(await page.locator('#requests-body tr').count(), 50, '翻页必须替换，行数不得累加为 150');

    // 翻到第 4 页（最后一页）：has_more 为 false，下一页按钮禁用
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => {
      const p = document.getElementById('requests-page');
      return p && p.textContent.includes('4');
    });
    assert.equal(await page.locator('#requests-next').isDisabled(), true, '取尽后下一页按钮应禁用');
    assert.equal(await page.locator('#requests-body tr').count(), 50);

    await page.close();
  });

  await withStep('Review: empty page renders cleanly with navigation disabled', async () => {
    const page = await fresh();
    await page.route(requests, (route) => {
      return route.fulfill({
        json: { items: [], offset: 0, limit: 50, has_more: false }
      });
    });
    await page.locator('#refresh').click();
    await ready(page);
    assert.equal(await page.locator('#requests-prev').isDisabled(), true);
    assert.equal(await page.locator('#requests-next').isDisabled(), true);
    await page.close();
  });

  await withStep('Review: filter change resets pagination to page 1', async () => {
    const page = await fresh();
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => {
      const p = document.getElementById('requests-page');
      return p && p.textContent.includes('2');
    });
    let requestOffsetSeen = null;
    await page.route(requests, (route) => {
      const u = new URL(route.request().url());
      requestOffsetSeen = u.searchParams.get('offset');
      return route.continue();
    });
    await page.locator('[data-range="7d"]').click();
    await page.waitForFunction(() => {
      const p = document.getElementById('requests-page');
      return p && p.textContent.includes('1');
    });
    assert.equal(requestOffsetSeen, '0', '切换筛选必须重置为 offset=0');
    assert.equal(await page.locator('#requests-prev').isDisabled(), true);
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.startsWith('第 2 页'));
    await page.locator('#filter-model').selectOption('gpt-5.1-codex');
    await ready(page);
    assert.equal(requestOffsetSeen, '0');
    assert.equal(await page.locator('#requests-page').textContent(), '第 1 页 · 本页 34 条');
    for (const text of await page.locator('#requests-body .model-cell').allTextContents()) assert.ok(text.includes('gpt-5.1-codex'));
    await page.close();
  });

  await withStep('Review: pending duplicate navigation blocked and failure retains prior page', async () => {
    const page = await fresh();
    const originalRows = await page.locator('#requests-body').textContent();
    const entered = deferred();
    const release = deferred();
    let calls = 0;
    await page.route(requests, async (route) => {
      const u = new URL(route.request().url());
      if (u.searchParams.get('offset') === '50') {
        calls++;
        entered.resolve();
        await release.promise;
        return route.fulfill({ status: 500, json: { error: 'server error' } });
      }
      return route.continue();
    });

    await page.locator('#requests-next').click();
    await entered.promise;

    assert.equal(await page.locator('#requests-next').isDisabled(), true);
    assert.equal(await page.locator('#requests-prev').isDisabled(), true);
    await page.locator('#requests-next').click({ force: true }).catch(() => {});
    assert.equal(calls, 1, '等待中不得重复发起请求');

    release.resolve();
    await page.waitForFunction(() => {
      const btn = document.getElementById('requests-next');
      return btn && !btn.disabled;
    });

    assert.equal(await page.locator('#requests-body tr').count(), 50, '失败必须保留原页数据');
    assert.equal(await page.locator('#requests-body').textContent(), originalRows);
    assert.match(await page.locator('#requests-page').textContent(), /1/, '失败必须保留原页码 1');
    assert.equal(await page.locator('#requests-prev').isDisabled(), true);
    assert.equal(await page.locator('#requests-next').isDisabled(), false);
    await page.unroute(requests);
    await page.locator('#requests-next').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.startsWith('第 2 页'));
    const lastRows = await page.locator('#requests-body').textContent();
    await page.route(requests, (route) => route.fulfill({ status: 500, json: {} }));
    await page.locator('#requests-prev').click();
    await page.waitForFunction(() => !document.getElementById('requests-prev').disabled);
    assert.equal(await page.locator('#requests-body').textContent(), lastRows);
    assert.equal(await page.locator('#requests-page').textContent(), '第 2 页 · 本页 2 条');
    assert.equal(await page.locator('#requests-next').isDisabled(), true);
    await page.unroute(requests);
    await page.locator('#requests-prev').click();
    await page.waitForFunction(() => document.getElementById('requests-page').textContent.startsWith('第 1 页'));
    assert.equal(await page.locator('#requests-body').textContent(), originalRows);
    await page.close();
  });

  await withStep('Review: mismatched and malformed body responses fail closed', async () => {
    const page = await fresh();
    for (const detail of [
      { request_id: 'other', body: 'WRONG-SECRET' },
      { request_id: 'req-body-xss', body: { secret: 'WRONG-SECRET' } }
    ]) {
      await page.route(bodies, (route) => route.fulfill({ json: detail }));
      await bodyOpen(page);
      await page.locator('#body-content').filter({ hasText: '不匹配或格式无效' }).waitFor();
      assert.ok(!(await page.locator('#body-content').textContent()).includes('WRONG-SECRET'));
      await page.locator('#body-close').click();
      await page.unroute(bodies);
    }
    await page.close();
  });

  for (const failure of [false, true]) {
    await withStep('Review: ' + (failure ? 'auth failure' : 'reconnect') + ' invalidates old body and closes viewer', async () => {
      const page = await fresh();
      const entered = deferred();
      const release = deferred();
      const finished = deferred();
      let bodyCalls = 0;
      await page.route(bodies, async (route) => {
        bodyCalls++;
        entered.resolve();
        await release.promise;
        try { await route.fulfill({ json: { request_id: 'req-body-xss', body: 'OLD-AUTH-SECRET' } }); }
        catch (_) { /* aborted client */ }
        finished.resolve();
      });
      await bodyOpen(page);
      await entered.promise;
      if (failure) {
        await page.route('**' + mock.MANAGEMENT_BASE + '/health', (route) => route.fulfill({ status: 403, json: {} }));
        await page.evaluate(() => document.getElementById('refresh').click());
        await page.waitForFunction(() => document.getElementById('conn-status').classList.contains('is-error'));
        // Existing list buttons must not issue requests while disconnected.
        await page.evaluate(() => {
          document.querySelector('[data-request-id="req-body-xss"]').click();
          document.getElementById('body-reveal').click();
        });
        assert.equal(bodyCalls, 1);
        await page.unroute('**' + mock.MANAGEMENT_BASE + '/health');
      }
      await page.evaluate(() => document.getElementById('connect').click());
      await ready(page);
      release.resolve();
      await finished.promise;
      assert.equal(await page.locator('#body-dialog').getAttribute('open'), null);
      assert.equal(await page.locator('#body-content').textContent(), '');
      await page.close();
    });
  }

  await withStep('Review: redirects are rejected before forwarding credentials', async () => {
    const page = await fresh();
    let redirected = 0;
    await page.route('**/redirect-target', (route) => { redirected++; return route.fulfill({ json: {} }); });
    await page.route('**' + mock.MANAGEMENT_BASE + '/summary**', (route) =>
      route.fulfill({ status: 302, headers: { location: '/redirect-target' } }));
    await page.locator('#refresh').click();
    await page.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('加载失败'));
    assert.equal(redirected, 0);
    await page.close();
  });
}

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const server = await mock.startServer({ captureBodies: true });
  const serverOff = await mock.startServer({ captureBodies: false });
  const browser = await chromium.launch({
    executablePath: CHROMIUM_PATH,
    ignoreDefaultArgs: ['--hide-scrollbars'],
    args: ['--no-sandbox', '--disable-dev-shm-usage']
  });

  const shot = async (page, name) => {
    await page.screenshot({ path: path.join(OUT, name + '.png'), fullPage: true });
    return name + '.png';
  };

  try {
    // ---------------- 嵌入宿主：三套主题 + 实时切换 ----------------
    {
      const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
      const page = await context.newPage();
      await page.goto(server.embedURL, { waitUntil: 'load' });
      const frame = await appFrame(page, server.origin);
      assert.ok(frame, 'embed frame found');

      await withStep('嵌入：连接并加载数据', async () => {
        await connect(frame, mock.SECRET);
        const value = await frame.locator('[data-metric="requests"] .metric-value').textContent();
        assert.equal(value.trim(), '1,234');
        return '数据加载 ' + value.trim();
      });

      await withStep('嵌入：浅色（暖灰）主题跟随父级', async () => {
        const theme = await frame.evaluate(() => document.documentElement.getAttribute('data-theme'));
        assert.equal(theme, null);
        const bg = await frame.evaluate(() =>
          getComputedStyle(document.documentElement).getPropertyValue('--bg-secondary').trim()
        );
        await checkColorScheme(frame, '');
        await shot(page, 'embedded-light');
        return 'bg-secondary=' + bg;
      });

      await withStep('嵌入：纯白主题', async () => {
        await page.evaluate(() => window.__setTheme('white'));
        await page.waitForFunction(() => {
          const f = document.getElementById('frame');
          return f && f.contentDocument && f.contentDocument.documentElement.getAttribute('data-theme') === 'white';
        });
        await checkColorScheme(frame, 'white');
        await shot(page, 'embedded-white');
        return 'data-theme=white';
      });

      await withStep('嵌入：深色主题', async () => {
        await page.evaluate(() => window.__setTheme('dark'));
        await page.waitForFunction(() => {
          const f = document.getElementById('frame');
          return f && f.contentDocument && f.contentDocument.documentElement.getAttribute('data-theme') === 'dark';
        });
        await checkColorScheme(frame, 'dark');
        await shot(page, 'embedded-dark');
        return 'data-theme=dark';
      });

      await withStep('嵌入：实时主题切换（白→深→浅）', async () => {
        await page.evaluate(() => window.__setTheme('white'));
        await frame.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'white');
        await checkColorScheme(frame, 'white');

        await page.evaluate(() => window.__setTheme('dark'));
        await frame.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'dark');
        await checkColorScheme(frame, 'dark');

        await page.evaluate(() => window.__setTheme(''));
        await frame.waitForFunction(() => !document.documentElement.getAttribute('data-theme'));
        await checkColorScheme(frame, '');

        const theme = await frame.evaluate(() => document.documentElement.getAttribute('data-theme'));
        assert.equal(theme, null);
        await shot(page, 'embedded-live-switch');
        return 'MutationObserver 同步生效';
      });

      await withStep('分页：上一页/下一页、当前页码与 50 条替换（无累加无游标）', async () => {
        const bounds = [];
        await page.route('**' + mock.MANAGEMENT_BASE + '/requests**', (route) => {
          const url = new URL(route.request().url());
          assert.equal(url.searchParams.has('cursor'), false);
          bounds.push([url.searchParams.get('from'), url.searchParams.get('to')]);
          return route.continue();
        });
        const before = await frame.locator('#requests-body tr').count();
        assert.equal(before, 50, '第一页应显示 50 条');
        assert.equal(await frame.locator('#requests-prev').isDisabled(), true, '第一页时上一页按钮禁用');
        assert.equal(await frame.locator('#requests-next').isDisabled(), false, '仍有下一页时下一页按钮可用');
        assert.match(await frame.locator('#requests-page').textContent(), /1/, '应显示第 1 页');

        // 点击下一页
        await frame.locator('#requests-next').click();
        await frame.waitForFunction(() => {
          const p = document.getElementById('requests-page');
          return p && p.textContent.includes('2');
        });
        const page2Count = await frame.locator('#requests-body tr').count();
        assert.equal(page2Count, 2, '第二页替换为 2 条（不累加）');
        assert.equal(await frame.locator('#requests-prev').isDisabled(), false, '第二页时上一页按钮可用');
        assert.equal(await frame.locator('#requests-next').isDisabled(), true, '最后一页时下一页按钮禁用');

        // 回到上一页（roundtrip）
        await frame.locator('#requests-prev').click();
        await frame.waitForFunction(() => {
          const p = document.getElementById('requests-page');
          return p && p.textContent.includes('1');
        });
        assert.equal(await frame.locator('#requests-body tr').count(), 50, '回到第一页恢复 50 条');
        assert.equal(await frame.locator('#requests-prev').isDisabled(), true);
        assert.deepEqual(bounds[0], bounds[1], '往返页必须保持同一个查询窗口');
        await page.unroute('**' + mock.MANAGEMENT_BASE + '/requests**');
        return '第 1 页 (50 条) ↔ 第 2 页 (2 条)';
      });

      // ---------------- XSS ----------------
      await withStep('XSS：恶意模型名不落地为元素', async () => {
        const images = await frame.locator('#requests-body img').count();
        assert.equal(images, 0, '恶意 <img> 不得被解析');
        const texts = await frame.locator('#requests-body .model-cell').allTextContents();
        assert.ok(texts.some((t) => t.includes(mock.XSS_MODEL)), '恶意模型名应以文本出现');
        const xss = await frame.evaluate(() => window.__observerXss || 0);
        assert.equal(xss, 0, 'onerror 不得执行');
        return 'payload 保持纯文本';
      });

      // ---------------- 请求体懒加载 ----------------
      let bodyFetches = 0;
      await page.route('**' + mock.MANAGEMENT_BASE + '/body**', async (route) => {
        bodyFetches += 1;
        await route.continue();
      });

      await withStep('请求体：列表刷新不触发读取', async () => {
        await frame.locator('#refresh').click();
        await ready(frame);
        assert.equal(bodyFetches, 0, '列表刷新期间不得请求请求体');
        return 'body 请求数 0';
      });

      await withStep('请求体：二次确认后才懒加载并原样渲染', async () => {
        await frame.locator('#requests-body button[data-request-id="req-body-xss"]').click();
        await frame.locator('#body-dialog[open]').waitFor({ timeout: 3000 });
        assert.equal(await frame.locator('#body-confirm').isVisible(), true, '需先展示确认');
        assert.equal(await frame.locator('#body-view').isVisible(), false, '确认前不得展示内容');
        assert.equal(bodyFetches, 0, '确认前不得请求请求体');
        await frame.locator('#body-reveal').click();
        await frame.waitForFunction(() => {
          const el = document.getElementById('body-content');
          return el && el.textContent.includes('prompt');
        });
        assert.equal(bodyFetches, 1);
        assert.equal(await frame.locator('#body-confirm').isVisible(), false, '加载后确认区应隐藏');
        assert.equal(await frame.locator('#body-content script').count(), 0, '不得解析出 <script>');
        const content = await frame.locator('#body-content').textContent();
        assert.ok(content.includes(mock.XSS_PROMPT), '请求体应原样渲染');
        await shot(page, 'body-viewer');
        return '懒加载 ' + bodyFetches + ' 次';
      });

      await withStep('请求体：关闭弹窗清空内容', async () => {
        await frame.locator('#body-close').click();
        await frame.locator('#body-dialog').waitFor({ state: 'hidden' });
        const content = await frame.locator('#body-content').textContent();
        assert.equal(content.trim(), '', '关闭后必须清空');
        return '内容已清空';
      });

      // ---------------- 过期响应守卫 ----------------
      await withStep('筛选切换：旧响应不得覆盖新结果', async () => {
        let armed = false;
        let calls = 0;
        const call1Entered = deferred();
        const call1Release = deferred();
        const call2Finished = deferred();
        await page.route('**' + mock.MANAGEMENT_BASE + '/requests**', async (route) => {
          if (!armed) {
            await route.continue();
            return;
          }
          calls += 1;
          const call = calls;
          const items = [
            call === 1
              ? { request_id: 'stale', model: 'STALE-OLD', provider: 'p', time: '2026-01-02T11:00:00Z', total_tokens: 1 }
              : { request_id: 'fresh', model: 'FRESH-NEW', provider: 'p', time: '2026-01-02T11:00:00Z', total_tokens: 1 }
          ];
          if (call === 1) {
            call1Entered.resolve();
            await call1Release.promise;
          }
          try {
            await route.fulfill({ json: { items, offset: 0, limit: 50, has_more: false } });
          } catch (err) {
            /* 请求可能已被客户端 abort */
          }
          if (call === 2) call2Finished.resolve();
        });

        armed = true;
        calls = 0;
        await frame.locator('[data-range="7d"]').click();
        await call1Entered.promise;
        await frame.locator('[data-range="24h"]').click();
        await call2Finished.promise;
        call1Release.resolve();
        await ready(frame);

        const models = await frame.locator('#requests-body .model-cell').allTextContents();
        assert.ok(!models.some((m) => m.includes('STALE-OLD')), '过期响应必须被丢弃');
        assert.ok(models.some((m) => m.includes('FRESH-NEW')), '最新响应应生效');
        return '旧响应已丢弃';
      });

      // ---------------- 安全 ----------------
      await withStep('安全：无浏览器密钥持久化 / 密钥不入 URL', async () => {
        const storage = await frame.evaluate(() => ({
          ls: (() => { try { return localStorage.length; } catch (e) { return -1; } })(),
          ss: (() => { try { return sessionStorage.length; } catch (e) { return -1; } })(),
          cookie: document.cookie
        }));
        assert.equal(storage.ls, 0);
        assert.equal(storage.ss, 0);
        assert.equal(storage.cookie, '');
        assert.ok(!frame.url().includes(mock.SECRET), '密钥不得出现在 URL');
        return 'localStorage/sessionStorage/cookie 均为空';
      });

      await withStep('安全：密钥以 Authorization 头发送', async () => {
        let seen = null;
        await page.route('**' + mock.MANAGEMENT_BASE + '/summary**', async (route) => {
          seen = route.request().headers().authorization || null;
          await route.continue();
        });
        await frame.locator('#refresh').click();
        await ready(frame);
        assert.equal(seen, 'Bearer ' + mock.SECRET);
        return 'Authorization: Bearer <已隐藏>';
      });

      await withStep('390px：窄屏无横向溢出', async () => {
        await page.setViewportSize({ width: 390, height: 844 });
        await page.evaluate(() => window.__setTheme('dark'));
        await frame.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'dark');
        const overflow = await frame.evaluate(
          () => document.documentElement.scrollWidth - document.documentElement.clientWidth
        );
        assert.ok(overflow <= 1, '页面横向溢出 ' + overflow + 'px');
        await checkColorScheme(frame, 'dark');
        await shot(page, 'embedded-dark-390');
        return 'overflow=' + overflow;
      });

      for (const theme of ['', 'white']) {
        await withStep('390px: ' + (theme || 'light'), async () => {
          await page.evaluate((value) => window.__setTheme(value), theme);
          await frame.waitForFunction((value) =>
            (document.documentElement.getAttribute('data-theme') || '') === value, theme);
          const overflow = await frame.evaluate(
            () => document.documentElement.scrollWidth - document.documentElement.clientWidth);
          assert.ok(overflow <= 1);
          await checkColorScheme(frame, theme);
          await shot(page, 'embedded-' + (theme || 'light') + '-390');
        });
      }

      await context.close();
    }

    // ---------------- 独立打开：系统偏好回退 ----------------
    {
      const darkContext = await browser.newContext({
        viewport: { width: 1100, height: 900 },
        colorScheme: 'dark'
      });
      const page = await darkContext.newPage();
      await page.goto(server.resourceURL, { waitUntil: 'load' });
      await withStep('独立打开：系统深色回退', async () => {
        await connect(page, mock.SECRET);
        const theme = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
        assert.equal(theme, 'dark');
        await checkColorScheme(page, 'dark');
        await page.screenshot({ path: path.join(OUT, 'standalone-system-dark.png'), fullPage: true });
        return 'data-theme=dark';
      });
      await darkContext.close();

      const lightContext = await browser.newContext({
        viewport: { width: 1100, height: 900 },
        colorScheme: 'light'
      });
      const lightPage = await lightContext.newPage();
      await lightPage.goto(server.resourceURL, { waitUntil: 'load' });
      await withStep('独立打开：系统浅色回退', async () => {
        const theme = await lightPage.evaluate(() => document.documentElement.getAttribute('data-theme'));
        assert.equal(theme, 'white');
        await checkColorScheme(lightPage, 'white');
        return 'CPA auto-white';
      });
      await lightContext.close();

      // 独立打开：三套主题在 1280 与 390 视图截图
      for (const width of [1280, 390]) {
        for (const theme of ['', 'white', 'dark']) {
          await withStep('独立打开截图：' + (theme || 'light') + ' ' + width + 'px', async () => {
            const ctx = await browser.newContext({
              viewport: { width, height: 900 },
              colorScheme: theme === 'dark' ? 'dark' : 'light'
            });
            const p = await ctx.newPage();
            p.on('dialog', (d) => { throw new Error('Forbidden native dialog: ' + d.message()); });
            await p.goto(server.resourceURL, { waitUntil: 'load' });
            await connect(p, mock.SECRET);
            if (theme) {
              await p.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
            } else {
              await p.evaluate(() => document.documentElement.removeAttribute('data-theme'));
            }
            await p.waitForFunction(
              (expected) => (document.documentElement.getAttribute('data-theme') || '') === expected,
              theme || ''
            );
            await checkColorScheme(p, theme);
            await shot(p, 'standalone-' + (theme || 'light') + '-' + width);
            await p.screenshot({ path: path.join(OUT, 'standalone-' + (theme || 'light') + '-' + width + '-viewport.png') });
            await ctx.close();
            return 'standalone-' + (theme || 'light') + '-' + width + '.png';
          });
        }
      }
    }

    // ---------------- 捕获关闭 ----------------
    {
      const context = await browser.newContext({ viewport: { width: 1100, height: 1000 } });
      const page = await context.newPage();
      await page.goto(serverOff.resourceURL, { waitUntil: 'load' });
      await withStep('捕获关闭：设置解释且请求体不可查看', async () => {
        await connect(page, mock.SECRET);
        const settings = await page.locator('#settings-body').textContent();
        assert.ok(settings.includes('请求体捕获已关闭'), '应解释捕获关闭');
        assert.ok(settings.includes('保存前不会捕获新请求体'), '应给出显式保存方式');
        const buttons = await page.locator('#requests-body button').count();
        assert.equal(buttons, 0, '捕获关闭时不得出现查看按钮');
        await page.screenshot({ path: path.join(OUT, 'standalone-capture-off.png'), fullPage: true });
        return '无查看按钮';
      });
      await context.close();
    }
    await reviewCases(browser, server);
  } finally {
    await browser.close();
    await server.close();
    await serverOff.close();
  }

  if (browserErrors.length) record('Browser console/runtime errors', false, browserErrors.join('; '));
  fs.writeFileSync(path.join(OUT, 'results.json'), JSON.stringify(results, null, 2));
  const failed = results.filter((r) => !r.ok);
  console.log('\n结果：' + (results.length - failed.length) + '/' + results.length + ' 通过');
  console.log('截图目录：' + OUT);
  if (failed.length) {
    console.error('失败项：' + failed.map((f) => f.name).join('; '));
    process.exitCode = 1;
  }
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
