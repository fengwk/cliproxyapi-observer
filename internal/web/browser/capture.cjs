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

async function appFrame(page, origin) {
  await page.waitForFunction(
    (prefix) => Array.from(document.querySelectorAll('iframe')).some((f) => f.src.startsWith(prefix)),
    origin + mock.RESOURCE_BASE,
    { timeout: 15000 }
  );
  return page.frame({ url: (u) => u.href.startsWith(origin + mock.RESOURCE_BASE) });
}

async function connect(frame, secret) {
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
    await page.goto(server.resourceURL);
    await connect(page, mock.SECRET);
    await page.waitForFunction(() => document.getElementById('conn-status').textContent.startsWith('已更新'));
    return page;
  };
  const deferred = () => {
    let resolve;
    const promise = new Promise((r) => { resolve = r; });
    return { promise, resolve };
  };
  const ready = (page) => page.waitForFunction(
    () => document.getElementById('conn-status').textContent.startsWith('已更新'));
  const bodyOpen = async (page) => {
    await page.locator('[data-request-id="req-body-xss"]').click();
    await page.locator('#body-reveal').click();
  };

  await withStep('Review: cost subtotal and unknown group are explicit', async () => {
    const page = await fresh();
    assert.match(await page.locator('[data-metric="cost"] .metric-sub').textContent(), /已定价小计；未定价 3 条/);
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
      return route.fulfill({ json: { items: [{
        request_id: 'current', model: 'CURRENT-MINUTE', time: new Date(now - 1000).toISOString()
      }], has_more: false, next_cursor: '' } });
    });
    await page.goto(server.resourceURL);
    await connect(page, mock.SECRET);
    await page.locator('#requests-body').filter({ hasText: 'CURRENT-MINUTE' }).waitFor();
    assert.match(await page.locator('#requests-note').textContent(), /请求明细仅保留 1 天.*统计按所选/);
    await page.close();
  });

  await withStep('Review: refresh cancels pending cursor, resets busy and rejects old append', async () => {
    const page = await fresh();
    const entered = deferred();
    const release = deferred();
    const finished = deferred();
    let first = true;
    await page.route(requests, async (route) => {
      if (new URL(route.request().url()).searchParams.has('cursor') && first) {
        first = false;
        entered.resolve();
        await release.promise;
        try { await route.fulfill({ json: { items: [{ model: 'OLD-CURSOR' }], has_more: false } }); }
        catch (_) { /* aborted client */ }
        finished.resolve();
      } else await route.continue();
    });
    await page.locator('#load-more').click();
    await entered.promise;
    await page.locator('#refresh').click();
    await ready(page);
    release.resolve();
    await finished.promise;
    assert.equal(await page.locator('#requests-body tr').count(), 50);
    assert.equal(await page.locator('#requests-body').getByText('OLD-CURSOR').count(), 0);
    await page.locator('#load-more').click();
    await page.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 52);
    await page.close();
  });

  await withStep('Review: request accumulation is capped at 500', async () => {
    const page = await fresh();
    let cursor = 0;
    await page.route(requests, (route) => route.fulfill({ json: {
      items: Array.from({ length: 50 }, (_, i) => ({ request_id: String(cursor) + '-' + i, model: 'bounded' })),
      next_cursor: String(++cursor), has_more: true
    } }));
    for (let count = 100; count <= 500; count += 50) {
      await page.locator('#load-more').click();
      await page.waitForFunction((n) => document.querySelectorAll('#requests-body tr').length === n, count);
    }
    assert.equal(await page.locator('#load-more').isVisible(), false);
    assert.match(await page.locator('#requests-note').textContent(), /最多 500/);
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
        await shot(page, 'embedded-light');
        return 'bg-secondary=' + bg;
      });

      await withStep('嵌入：纯白主题', async () => {
        await page.evaluate(() => window.__setTheme('white'));
        await page.waitForFunction(() => {
          const f = document.getElementById('frame');
          return f && f.contentDocument && f.contentDocument.documentElement.getAttribute('data-theme') === 'white';
        });
        await shot(page, 'embedded-white');
        return 'data-theme=white';
      });

      await withStep('嵌入：深色主题', async () => {
        await page.evaluate(() => window.__setTheme('dark'));
        await page.waitForFunction(() => {
          const f = document.getElementById('frame');
          return f && f.contentDocument && f.contentDocument.documentElement.getAttribute('data-theme') === 'dark';
        });
        await shot(page, 'embedded-dark');
        return 'data-theme=dark';
      });

      await withStep('嵌入：实时主题切换（白→深→浅）', async () => {
        await page.evaluate(() => window.__setTheme('white'));
        await page.waitForTimeout(120);
        await page.evaluate(() => window.__setTheme('dark'));
        await page.waitForTimeout(120);
        await page.evaluate(() => window.__setTheme(''));
        await page.waitForTimeout(200);
        const theme = await frame.evaluate(() => document.documentElement.getAttribute('data-theme'));
        assert.equal(theme, null);
        await shot(page, 'embedded-live-switch');
        return 'MutationObserver 同步生效';
      });

      await withStep('分页：加载更多使用游标并追加', async () => {
        const before = await frame.locator('#requests-body tr').count();
        assert.equal(before, 50);
        assert.equal(await frame.locator('#load-more').isVisible(), true, '仍有下一页时应显示加载更多');
        await frame.locator('#load-more').click();
        await frame.waitForFunction(() => document.querySelectorAll('#requests-body tr').length === 52);
        const note = await frame.locator('#requests-note').textContent();
        assert.match(note, /52/);
        assert.equal(await frame.locator('#load-more').isVisible(), false, '取尽后隐藏加载更多');
        return '50 → 52 条';
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
        await frame.waitForTimeout(400);
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
        await frame.waitForTimeout(150);
        const content = await frame.locator('#body-content').textContent();
        assert.equal(content.trim(), '', '关闭后必须清空');
        return '内容已清空';
      });

      // ---------------- 过期响应守卫 ----------------
      await withStep('筛选切换：旧响应不得覆盖新结果', async () => {
        let armed = false;
        let calls = 0;
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
          if (call === 1) await new Promise((r) => setTimeout(r, 900));
          try {
            await route.fulfill({ json: { items, next_cursor: '', has_more: false } });
          } catch (err) {
            /* 请求可能已被客户端 abort */
          }
        });

        armed = true;
        calls = 0;
        await frame.locator('[data-range="7d"]').click();
        await page.waitForTimeout(80);
        await frame.locator('[data-range="24h"]').click();
        await page.waitForTimeout(1400);

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
        await frame.waitForTimeout(600);
        assert.equal(seen, 'Bearer ' + mock.SECRET);
        return 'Authorization: Bearer <已隐藏>';
      });

      await withStep('390px：窄屏无横向溢出', async () => {
        await page.setViewportSize({ width: 390, height: 844 });
        await page.evaluate(() => window.__setTheme('dark'));
        await page.waitForTimeout(300);
        const overflow = await frame.evaluate(
          () => document.documentElement.scrollWidth - document.documentElement.clientWidth
        );
        assert.ok(overflow <= 1, '页面横向溢出 ' + overflow + 'px');
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
        return 'CPA auto-white';
      });
      await lightContext.close();
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
        assert.ok(settings.includes('capture-bodies: true'), '应给出开启方式');
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
