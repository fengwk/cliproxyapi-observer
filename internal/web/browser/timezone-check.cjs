#!/usr/bin/env node
'use strict';

// Focused real-Chrome acceptance for browser-local time display and the
// timezone-aware RequestView contract. It loads the compiled plugin UI
// directly (no simplified parent shell) and drives it with fake mock data.

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium } = require('../../../web/node_modules/playwright');
const mock = require('./mock-server.cjs');

const args = {
  chrome: process.env.CHROME_BIN || '/usr/bin/google-chrome-stable',
  reportDir: path.resolve(__dirname, '../../../../timezone-browser-report'),
};
for (let i = 2; i < process.argv.length; i++) {
  const name = process.argv[i];
  if (name === '--report-dir') args.reportDir = path.resolve(process.argv[++i]);
  else if (name === '--chrome') args.chrome = process.argv[++i];
  else throw new Error('Unknown argument: ' + name);
}

const results = [];
const screenshots = [];
const DST_SECOND_OCCURRENCE = '2026-11-01T06:30:00.123456789Z';
function check(name, condition, detail = '') {
  results.push({ name, ok: Boolean(condition), detail: String(detail) });
  console.log(`${condition ? 'PASS' : 'FAIL'} ${name}${condition ? '' : ': ' + detail}`);
  assert.ok(condition, name + (detail ? ': ' + detail : ''));
}

function partsAt(ms, timeZone) {
  const out = {};
  const formatter = new Intl.DateTimeFormat('en-US', {
    timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
  });
  for (const part of formatter.formatToParts(new Date(ms))) {
    if (part.type !== 'literal') out[part.type] = part.value;
  }
  return out;
}

function localDate(ms, timeZone) {
  const p = partsAt(ms, timeZone);
  return `${p.year}-${p.month}-${p.day}`;
}

function localDateTuple(value) {
  return value.split('-').map(Number);
}

// Local wall-clock at a UTC instant, formatted exactly as the UI does.
function localInput(iso, timeZone) {
  const p = partsAt(Date.parse(iso), timeZone);
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`;
}

function offsetLabel(ms, timeZone) {
  const p = partsAt(ms, timeZone);
  const asUTC = Date.UTC(Number(p.year), Number(p.month) - 1, Number(p.day),
    Number(p.hour), Number(p.minute), Number(p.second));
  const offset = Math.round((asUTC - ms) / 60000);
  if (offset === 0) return 'UTC';
  const abs = Math.abs(offset);
  return `UTC${offset > 0 ? '+' : '-'}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
}

function localDisplay(iso, timeZone) {
  const p = partsAt(Date.parse(iso), timeZone);
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second} ${offsetLabel(Date.parse(iso), timeZone)}`;
}

function fixture(base, id, model, time) {
  return {
    ...base,
    id,
    trace_id: 'trace-' + id,
    key_id: 'key-alpha',
    model,
    provider: 'local-zone-mock',
    auth_index: 'local-zone-account',
    time,
    failed: false,
    status_code: 200,
    tokens: { input: 10, output: 5, cache_read: 0, cache_write: 0, reasoning: 0, total: 15 },
    cost_micro_usd: 1000,
    price_source: 'auto',
    accounting_quality: 'complete',
  };
}

async function shot(page, name) {
  const file = path.join('screenshots', name + '.png');
  await page.screenshot({ path: path.join(args.reportDir, file), fullPage: true });
  const info = await fs.stat(path.join(args.reportDir, file));
  screenshots.push({ file, bytes: info.size });
}

async function main() {
  await fs.mkdir(path.join(args.reportDir, 'screenshots'), { recursive: true });
  const server = await mock.startServer();
  const browser = await chromium.launch({ executablePath: args.chrome, headless: true });
  const report = { mode: 'timezone-focused', results, screenshots, zones: {} };
  try {
    for (const zone of ['Asia/Shanghai', 'America/New_York']) {
      server.resetState();
      server.resetCalls();
      const base = server.state.records[0];
      // Saved pricing date conditions stay UTC-calendar rules; the UI only
      // previews their local instant and must not rewrite the wire value.
      server.state.settings.pricing.prices[0].definition.overrides[0].when.date = {
        gte: '2026-10-20',
      };
      server.state.settings.pricing.prices[0].definition.overrides[0].when.time = {
        start: '08:00',
        end: '18:00',
        timezone: 'UTC',
      };
      server.state.records = [
        // UTC Oct 19: only Shanghai's Oct 20 includes the first row.
        fixture(base, 'zone-early', 'zone-boundary-early', '2026-10-19T16:30:00.000Z'),
        fixture(base, 'zone-both', 'zone-boundary-both', '2026-10-20T04:30:00.000Z'),
        // UTC Oct 21: only New York's Oct 20 includes the last row.
        fixture(base, 'zone-late', 'zone-boundary-late', '2026-10-21T03:30:00.000Z'),
        fixture(base, 'zone-outside', 'zone-outside', '2026-10-19T15:30:00.000Z'),
      ];
      if (zone === 'America/New_York') {
        // Stored instant for the second 01:30 on the fall-back day (EST).
        const overlapKey = mock.createKey('key-dst-overlap', 'DST 重复时刻 Key', {
          allowed_models: ['gpt-5-codex'],
          max_concurrent: 1,
          active: 0,
          expires_at: DST_SECOND_OCCURRENCE,
        });
        server.state.keys.push(overlapKey);
        // A directly inserted enabled fixture needs the same credential state
        // as other live Keys; a missing hash correctly requires rotation.
        const secret = mock.SECRET_PREFIX + overlapKey.id;
        server.state.secrets.set(overlapKey.id, crypto.createHash('sha256').update(secret).digest('hex'));
        server.state.secretValues.set(overlapKey.id, secret);
      }

      const context = await browser.newContext({
        viewport: { width: 1280, height: 900 },
        timezoneId: zone,
        reducedMotion: 'reduce',
      });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', error => errors.push('page: ' + error.message));
      page.on('console', message => {
        if (message.type() === 'error') errors.push('console: ' + message.text());
      });
      try {
        await page.goto(server.resourceURL);
        await page.locator('#login-key').fill(mock.DEFAULT_MANAGEMENT_KEY);
        await page.getByRole('button', { name: '登录', exact: true }).click();
        await page.getByTestId('requests-filters').waitFor();

        const zoneInfo = { zone, checks: results.length };
        report.zones[zone] = zoneInfo;
        const viewCalls = () => server.calls.filter(call => call.path === mock.MANAGEMENT_BASE + '/requests/view');
        for (let i = 0; i < 100 && viewCalls().length === 0; i++) {
          await new Promise(resolve => setTimeout(resolve, 50));
        }
        const initial = viewCalls().at(-1);
        const today = localDate(Date.now(), zone);
        check(`${zone}: initial query carries the browser IANA timezone`,
          initial && initial.query.timezone === zone, JSON.stringify(initial?.query));
        check(`${zone}: initial dates use the browser-local today`,
          initial && initial.query.from_date === today && initial.query.to_date === today,
          JSON.stringify({ today, query: initial?.query }));
        check(`${zone}: filter bar names the active zone`,
          await page.getByText(`本地时区：${zone}`, { exact: true }).count() === 1);

        await page.getByLabel('开始日期').fill('2026-10-20');
        await page.getByLabel('结束日期').fill('2026-10-20');
        const responsePromise = page.waitForResponse(response => {
          const url = new URL(response.url());
          return url.pathname.endsWith('/requests/view') && url.searchParams.get('from_date') === '2026-10-20';
        });
        await page.getByRole('button', { name: '查询', exact: true }).click();
        const response = await responsePromise;
        const payload = await response.json();
        check(`${zone}: response echoes the requested zone`, payload.range.timezone === zone,
          JSON.stringify(payload.range));

        const query = viewCalls().at(-1).query;
        check(`${zone}: applied query passes local inclusive dates and zone`,
          query.from_date === '2026-10-20' && query.to_date === '2026-10-20' && query.timezone === zone,
          JSON.stringify(query));

        const expectedIds = zone === 'Asia/Shanghai'
          ? ['zone-early', 'zone-both']
          : ['zone-both', 'zone-late'];
        const excludedId = zone === 'Asia/Shanghai' ? 'zone-late' : 'zone-early';
        await page.getByTestId('record-row-' + expectedIds[0]).waitFor();
        check(`${zone}: local date boundaries include only the local Oct 20 rows`,
          await page.getByTestId('record-row-' + expectedIds[1]).count() === 1 &&
          await page.getByTestId('record-row-' + excludedId).count() === 0,
          JSON.stringify({ expectedIds, excludedId }));

        const crossBoundary = zone === 'Asia/Shanghai' ? 'zone-early' : 'zone-late';
        const crossIso = zone === 'Asia/Shanghai'
          ? '2026-10-19T16:30:00.000Z'
          : '2026-10-21T03:30:00.000Z';
        const rowText = await page.getByTestId('record-row-' + crossBoundary).innerText();
        check(`${zone}: cross-UTC-day row renders browser-local time with explicit offset`,
          rowText.includes(localDisplay(crossIso, zone)) && !rowText.includes(' UTC\n'),
          rowText);
        const axisText = await page.getByTestId('requests-trend-axis').innerText();
        check(`${zone}: chart axis uses local offsets instead of a bare UTC label`,
          axisText.includes(offsetLabel(Date.parse(crossIso), zone)) && !/ \d{2}:\d{2} UTC(?:\s|$)/.test(axisText),
          axisText);
        await shot(page, `${zone.replace('/', '-')}-requests`);

        // Key editing starts from the stored RFC3339 nanosecond expiry and must
        // show browser-local wall time without changing the untouched value.
        await page.getByRole('button', { name: 'Key 管理', exact: true }).click();
        const keyRow = page.getByRole('row').filter({ hasText: '纳秒到期 Key' });
        await keyRow.waitFor();
        await keyRow.getByRole('button', { name: '编辑', exact: true }).click();
        const dialog = page.getByRole('dialog');
        await dialog.waitFor();
        const expectedInput = localInput(mock.NANO_EXPIRY, zone);
        check(`${zone}: Key editor shows the stored expiry in browser-local time`,
          await dialog.getByLabel('到期时间').inputValue() === expectedInput,
          await dialog.getByLabel('到期时间').inputValue());
        check(`${zone}: Key editor names the local timezone semantics`,
          await dialog.getByText(new RegExp('按浏览器本地时区 ' + zone.replace(/[/.]/g, '\\$&'))).count() === 1);
        await shot(page, `${zone.replace('/', '-')}-key-editor`);
        const putPromise = page.waitForResponse(res =>
          res.url().includes('/keys?id=key-nano') && res.request().method() === 'PUT');
        await dialog.getByRole('button', { name: '保存修改', exact: true }).click();
        await putPromise;
        await dialog.waitFor({ state: 'detached' });
        const put = server.calls.filter(call =>
          call.method === 'PUT' && call.path === mock.MANAGEMENT_BASE + '/keys' &&
          call.query.id === 'key-nano').at(-1);
        check(`${zone}: untouched expiry keeps the exact original RFC3339 nanoseconds`,
          put && put.request_body && put.request_body.expires_at === mock.NANO_EXPIRY,
          JSON.stringify(put?.request_body));

        if (zone === 'America/New_York') {
          // Drive the actual compiled form logic through the Key editor. DST
          // gap/overlap values must be rejected without any API write.
          const nanoPutsBefore = server.calls.filter(call =>
            call.method === 'PUT' && call.path === mock.MANAGEMENT_BASE + '/keys' &&
            call.query.id === 'key-nano').length;
          await keyRow.getByRole('button', { name: '编辑', exact: true }).click();
          await dialog.waitFor();
          const dtInput = dialog.getByLabel('到期时间');
          await dtInput.fill('2026-03-08T02:30');
          await dialog.getByRole('button', { name: '保存修改', exact: true }).click();
          await dialog.getByText('该本地时间在时区切换时不存在，请选择其他时间', { exact: true }).waitFor();
          check('America/New_York: Key editor rejects a spring-forward gap time',
            true);
          await dtInput.fill('2026-11-01T01:30');
          await dialog.getByRole('button', { name: '保存修改', exact: true }).click();
          await dialog.getByText('该本地时间在时区切换时重复，请避开重复时段', { exact: true }).waitFor();
          const nanoPutsAfterErrors = server.calls.filter(call =>
            call.method === 'PUT' && call.path === mock.MANAGEMENT_BASE + '/keys' &&
            call.query.id === 'key-nano').length;
          check('America/New_York: Key editor rejects a fall-back overlap time without writing',
            nanoPutsAfterErrors === nanoPutsBefore,
            JSON.stringify({ nanoPutsBefore, nanoPutsAfterErrors }));

          await dtInput.fill('2026-03-08T03:30');
          const validPutPromise = page.waitForResponse(res =>
            res.url().includes('/keys?id=key-nano') && res.request().method() === 'PUT');
          await dialog.getByRole('button', { name: '保存修改', exact: true }).click();
          await validPutPromise;
          await dialog.waitFor({ state: 'detached' });
          const validPut = server.calls.filter(call =>
            call.method === 'PUT' && call.path === mock.MANAGEMENT_BASE + '/keys' &&
            call.query.id === 'key-nano').at(-1);
          check('America/New_York: valid post-gap local time saves the correct UTC instant',
            validPut && validPut.request_body.expires_at === '2026-03-08T07:30:00.000Z',
            JSON.stringify(validPut?.request_body));

          // The stored instant is the second (EST) 01:30. The unchanged edit
          // must preserve that instant and its exact nanoseconds, not the EDT one.
          const overlapRow = page.getByRole('row').filter({ hasText: 'DST 重复时刻 Key' });
          await overlapRow.waitFor();
          await overlapRow.getByRole('button', { name: '编辑', exact: true }).click();
          await dialog.waitFor();
          check('America/New_York: second-occurrence expiry displays as local 01:30',
            await dialog.getByLabel('到期时间').inputValue() === '2026-11-01T01:30',
            await dialog.getByLabel('到期时间').inputValue());
          const overlapPutPromise = page.waitForResponse(res =>
            res.url().includes('/keys?id=key-dst-overlap') && res.request().method() === 'PUT');
          await dialog.getByRole('button', { name: '保存修改', exact: true }).click();
          const overlapResponse = await overlapPutPromise;
          check('America/New_York: enabled second-occurrence Key saves successfully',
            overlapResponse.status() === 200, `HTTP ${overlapResponse.status()}`);
          await dialog.waitFor({ state: 'detached' });
          const overlapPut = server.calls.filter(call =>
            call.method === 'PUT' && call.path === mock.MANAGEMENT_BASE + '/keys' &&
            call.query.id === 'key-dst-overlap').at(-1);
          check('America/New_York: unchanged second-occurrence edit preserves exact nanoseconds',
            overlapPut && overlapPut.request_body.expires_at === DST_SECOND_OCCURRENCE,
            JSON.stringify(overlapPut?.request_body));
        }

        await page.getByRole('button', { name: '设置', exact: true }).click();
        await page.locator('#settings-pricing-heading').waitFor();
        await page.locator('#settings-pricing-heading').scrollIntoViewIfNeeded();
        await page.locator('.settings-definition').first().waitFor();
        const boundaryLocal = await page.evaluate((iso) => new Intl.DateTimeFormat(undefined, {
          year: 'numeric', month: '2-digit', day: '2-digit',
          hour: '2-digit', minute: '2-digit', second: '2-digit',
          timeZoneName: 'short', hourCycle: 'h23',
        }).format(new Date(iso)), '2026-10-20T00:00:00Z');
        const boundaryHint = await page.getByText(/本地生效：从/).first().innerText();
        check(`${zone}: UTC pricing date rule shows a browser-local boundary preview`,
          boundaryHint.includes(`本地生效：从 ${boundaryLocal} 起`) &&
          boundaryHint.includes('UTC 日历'),
          boundaryHint);
        const ruleZone = await page.locator('input[aria-label$="时区"]').first().inputValue();
        check(`${zone}: saved pricing rule keeps its explicit UTC timezone`, ruleZone === 'UTC', ruleZone);
        await shot(page, `${zone.replace('/', '-')}-pricing-boundary`);

        if (zone === 'America/New_York') {
          // Fall-back day has two distinct instants at local 01:30. The table
          // must show both with different numeric offsets rather than merging.
          server.state.records = [
            fixture(base, 'dst-early', 'dst-fold-early', '2025-11-02T05:30:00.000Z'),
            fixture(base, 'dst-late', 'dst-fold-late', '2025-11-02T06:30:00.000Z'),
          ];
          await page.getByRole('button', { name: '请求日志', exact: true }).click();
          await page.getByTestId('requests-filters').waitFor();
          await page.getByLabel('开始日期').fill('2025-11-02');
          await page.getByLabel('结束日期').fill('2025-11-02');
          const dstResponse = page.waitForResponse(res => {
            const url = new URL(res.url());
            return url.pathname.endsWith('/requests/view') && url.searchParams.get('from_date') === '2025-11-02';
          });
          await page.getByRole('button', { name: '查询', exact: true }).click();
          const dstPayload = await (await dstResponse).json();
          await page.getByTestId('record-row-dst-early').waitFor();
          const early = await page.getByTestId('record-row-dst-early').innerText();
          const late = await page.getByTestId('record-row-dst-late').innerText();
          check('America/New_York: DST-repeated local hour is distinguished by numeric offset',
            early.includes('2025-11-02 01:30:00 UTC-04:00') &&
            late.includes('2025-11-02 01:30:00 UTC-05:00'),
            JSON.stringify({ early, late }));
          check('America/New_York: fall-back local day projects 25 elapsed hour buckets',
            dstPayload.series.granularity === 'hour' && dstPayload.series.buckets.length === 25,
            JSON.stringify({ granularity: dstPayload.series.granularity, buckets: dstPayload.series.buckets.length }));
          await shot(page, 'America-New_York-dst');
        }

        check(`${zone}: no console or page errors`, errors.length === 0, errors.join('\n'));
      } finally {
        await context.close();
      }
    }
    report.outcome = 'passed';
    await fs.writeFile(path.join(args.reportDir, 'report.json'), JSON.stringify(report, null, 2));
    console.log(`\n${results.length} checks passed. Report: ${args.reportDir}`);
  } catch (error) {
    report.outcome = 'failed';
    report.error = error.stack || String(error);
    await fs.writeFile(path.join(args.reportDir, 'report.json'), JSON.stringify(report, null, 2));
    throw error;
  } finally {
    await browser.close();
    await server.close();
  }
}

main().catch(error => {
  console.error(error.stack || error);
  process.exitCode = 1;
});
