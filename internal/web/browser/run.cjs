#!/usr/bin/env node
'use strict';

/*
 * Observer v0.6.0 真实 Chrome 浏览器验收（零 npm 依赖，Node >= 22）。
 *
 * 用法：
 *   node internal/web/browser/run.cjs --report-dir /absolute/report/dir
 *
 * 可选：
 *   --chrome /usr/bin/google-chrome-stable
 *   --report-dir  截图、report.json、summary.txt 的输出目录
 *   --keep-profile 保留临时 Chrome profile（排障用）
 *
 * 覆盖：
 *   - DESIGN.md 管理 API 的完整 mock 合约（鉴权、输入拒绝、CRUD、筛选/分页/汇总）；
 *   - 主 UI：登录门禁、请求日志/Key 管理/设置三页、主题与刷新间隔；
 *   - #filters 查询参数、详情只在点击日志行后读取、Key 全动作、设置保存/价格刷新/压缩；
 *   - 可访问性（可见控件名称、对话框名称、表头）、无 HTML 注入、无 console/page 错误；
 *   - 1280/390 × white/light/dark/auto × 三页 + Key 表单 + 请求详情截图矩阵；
 *   - 每个视口/主题/页面无横向溢出。
 *
 * 只使用 127.0.0.1 上的假数据；不读取用户 profile、不访问真实 CPA 或上游。
 */

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const mock = require('./mock-server.cjs');

const DEFAULT_CHROME = '/usr/bin/google-chrome-stable';
const WORKTREE_ROOT = path.resolve(__dirname, '../../..');
const DEFAULT_REPORT_DIR = path.resolve(WORKTREE_ROOT, '..', 'browser-report');
const THEMES = ['white', 'light', 'dark', 'auto'];
const SIZES = [
  { width: 1280, height: 900, mobile: false, scale: 1 },
  // 固定 390 CSS px 布局宽度。CDP 的 mobile=true 会叠加视口缩放，导致
  // innerWidth 不是 390；验收按 DESIGN 要求的 390px 响应式宽度取证。
  { width: 390, height: 844, mobile: false, scale: 1 }
];
const PAGES = [
  { key: 'requests', nav: '请求日志' },
  { key: 'keys', nav: 'Key 管理' },
  { key: 'settings', nav: '设置' }
];

const results = [];
const consoleErrors = [];
const expectedDiagnostics = [];
const nativeDialogs = [];
const screenshots = [];
let failed = 0;
let chromeInfo = null;
let reportDir = DEFAULT_REPORT_DIR;
let chromePath = process.env.CHROME_BIN || DEFAULT_CHROME;
let keepProfile = false;

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function check(name, ok, detail) {
  const passed = Boolean(ok);
  if (!passed) failed += 1;
  const entry = {
    name,
    ok: passed,
    detail: detail === undefined ? '' : String(detail)
  };
  results.push(entry);
  process.stdout.write(
    (passed ? 'PASS  ' : 'FAIL  ') + name + (passed || detail === undefined ? '' : ' :: ' + entry.detail) + '\n'
  );
  return passed;
}

async function step(name, fn) {
  const started = Date.now();
  const failuresBefore = failed;
  try {
    const detail = await fn();
    const innerFailed = failed > failuresBefore;
    if (!results.some((entry) => entry.name === name)) {
      check(name, !innerFailed, innerFailed ? '内部断言失败（见上）' : detail);
    }
    return !innerFailed;
  } catch (err) {
    check(name, false, (err && err.message) || String(err));
    return false;
  } finally {
    const entry = results[results.length - 1];
    if (entry && entry.name === name) entry.duration_ms = Date.now() - started;
  }
}

function parseArgs(argv) {
  const args = { reportDir: DEFAULT_REPORT_DIR, chrome: process.env.CHROME_BIN || DEFAULT_CHROME, keepProfile: false };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--report-dir') {
      args.reportDir = path.resolve(argv[++i] || '');
    } else if (arg.startsWith('--report-dir=')) {
      args.reportDir = path.resolve(arg.slice('--report-dir='.length));
    } else if (arg === '--chrome') {
      args.chrome = argv[++i] || DEFAULT_CHROME;
    } else if (arg.startsWith('--chrome=')) {
      args.chrome = arg.slice('--chrome='.length);
    } else if (arg === '--keep-profile') {
      args.keepProfile = true;
    } else if (arg === '--help' || arg === '-h') {
      process.stdout.write(
        'Usage: node internal/web/browser/run.cjs [--report-dir ABS] [--chrome PATH] [--keep-profile]\n' +
          'Default report dir: ' +
          DEFAULT_REPORT_DIR +
          '\n'
      );
      process.exit(0);
    } else {
      throw new Error('unknown argument: ' + arg);
    }
  }
  return args;
}

/* -------------------------------------------------------------------------- */
/* CDP                                                                        */
/* -------------------------------------------------------------------------- */

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.nextId = 0;
    this.pending = new Map();
    this.listeners = new Map();
    ws.addEventListener('message', (event) => {
      let message;
      try {
        message = JSON.parse(event.data);
      } catch (err) {
        return;
      }
      if (message.id && this.pending.has(message.id)) {
        const pending = this.pending.get(message.id);
        this.pending.delete(message.id);
        if (message.error) pending.reject(new Error(JSON.stringify(message.error)));
        else pending.resolve(message.result || {});
        return;
      }
      if (message.method) {
        const handlers = this.listeners.get(message.method) || [];
        for (const handler of handlers) handler(message.params || {});
      }
    });
  }

  on(method, handler) {
    if (!this.listeners.has(method)) this.listeners.set(method, []);
    this.listeners.get(method).push(handler);
  }

  send(method, params = {}, timeoutMs = 30000) {
    const id = ++this.nextId;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error('CDP timeout: ' + method));
        }
      }, timeoutMs);
      this.pending.set(id, {
        resolve: (value) => {
          clearTimeout(timer);
          resolve(value);
        },
        reject: (err) => {
          clearTimeout(timer);
          reject(err);
        }
      });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  async evaluate(expression, timeoutMs = 30000) {
    const result = await this.send(
      'Runtime.evaluate',
      { expression, awaitPromise: true, returnByValue: true, userGesture: true },
      timeoutMs
    );
    if (result.exceptionDetails) {
      const description =
        (result.exceptionDetails.exception && result.exceptionDetails.exception.description) ||
        result.exceptionDetails.text ||
        'page evaluation failed';
      throw new Error('页面执行异常: ' + description);
    }
    return result.result ? result.result.value : undefined;
  }

  async waitFor(expression, timeoutMs = 15000, label = expression) {
    const deadline = Date.now() + timeoutMs;
    let lastError = null;
    while (Date.now() < deadline) {
      try {
        if (await this.evaluate('Boolean(' + expression + ')')) return true;
      } catch (err) {
        lastError = err;
      }
      await sleep(100);
    }
    throw new Error('等待条件超时: ' + label + (lastError ? ' (' + lastError.message + ')' : ''));
  }
}

function connectWebSocket(url) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    const timer = setTimeout(() => {
      try {
        ws.close();
      } catch (err) {
        /* ignore */
      }
      reject(new Error('WebSocket connection timeout'));
    }, 10000);
    ws.addEventListener('open', () => {
      clearTimeout(timer);
      resolve(new CDP(ws));
    });
    ws.addEventListener('error', () => {
      clearTimeout(timer);
      reject(new Error('WebSocket connection failed'));
    });
  });
}

async function launchChrome(executable, profileDir, width, height) {
  if (!fs.existsSync(executable)) {
    throw new Error('Chrome executable not found: ' + executable);
  }
  const args = [
    '--headless=new',
    '--remote-debugging-port=0',
    '--remote-allow-origins=*',
    '--user-data-dir=' + profileDir,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-extensions',
    '--disable-gpu',
    '--disable-dev-shm-usage',
    '--disable-background-networking',
    '--disable-sync',
    '--no-sandbox',
    '--hide-scrollbars',
    '--window-size=' + width + ',' + height,
    'about:blank'
  ];
  const child = spawn(executable, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', (chunk) => {
    stderr += chunk.toString();
    if (stderr.length > 12000) stderr = stderr.slice(-12000);
  });
  chromeInfo = { executable, pid: child.pid, args };

  const activePort = path.join(profileDir, 'DevToolsActivePort');
  const deadline = Date.now() + 25000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error('Chrome exited early (' + child.exitCode + '): ' + stderr.slice(-2000));
    }
    if (fs.existsSync(activePort)) {
      const lines = fs.readFileSync(activePort, 'utf8').split('\n');
      const port = Number(lines[0]);
      if (port) {
        try {
          const list = await (await fetch('http://127.0.0.1:' + port + '/json/list')).json();
          const page = list.find((target) => target.type === 'page' && target.webSocketDebuggerUrl);
          if (page) {
            return { child, port, wsUrl: page.webSocketDebuggerUrl, stderr: () => stderr };
          }
        } catch (err) {
          /* Chrome 尚未就绪 */
        }
      }
    }
    await sleep(200);
  }
  child.kill('SIGKILL');
  throw new Error('Chrome DevTools endpoint unavailable: ' + stderr.slice(-2000));
}

async function stopChrome(context) {
  if (!context) return;
  try {
    await context.cdp.send('Browser.close', {}, 5000);
  } catch (err) {
    /* ignore */
  }
  const child = context.child;
  if (!child || child.exitCode !== null) return;
  await Promise.race([
    new Promise((resolve) => child.once('exit', resolve)),
    sleep(3000).then(() => {
      if (child.exitCode === null) child.kill('SIGKILL');
    })
  ]);
}

/* -------------------------------------------------------------------------- */
/* Page helpers                                                               */
/* -------------------------------------------------------------------------- */

const js = (value) => JSON.stringify(value);

function pageFn(body, ...args) {
  // 使用普通函数而不是箭头函数：帮助片段通过 arguments[n] 读取注入参数。
  return '(function(){' + body + '})(' + args.map((arg) => JSON.stringify(arg)).join(',') + ')';
}

const VISIBLE_SNIPPET =
  'const visible=(el)=>{if(!el)return false;const r=el.getBoundingClientRect();' +
  'const s=getComputedStyle(el);return r.width>0&&r.height>0&&s.display!=="none"&&' +
  's.visibility!=="hidden"&&s.opacity!=="0";};';

const NAME_SNIPPET =
  'const accName=(el)=>{if(!el)return "";const aria=(el.getAttribute("aria-label")||"").trim();' +
  'if(aria)return aria;const labelled=(el.getAttribute("aria-labelledby")||"").trim();' +
  'if(labelled){const t=labelled.split(/\\s+/).map((id)=>{const n=document.getElementById(id);' +
  'return n?n.innerText:"";}).join(" ").trim();if(t)return t;}' +
  'if(el.id){const l=document.querySelector(\'label[for="\'+CSS.escape(el.id)+\'"]\');' +
  'if(l){const t=l.innerText.trim();if(t)return t;}}' +
  'const wrap=el.closest("label");if(wrap){const t=wrap.innerText.trim();if(t)return t;}' +
  'if(el.tagName==="BUTTON"||el.getAttribute("role")==="button"){const t=(el.innerText||"").trim();if(t)return t;}' +
  'const ph=(el.getAttribute("placeholder")||"").trim();if(ph)return ph;' +
  'const title=(el.getAttribute("title")||"").trim();if(title)return title;' +
  'const alt=(el.getAttribute("alt")||"").trim();if(alt)return alt;return "";};';

async function visible(cdp, selector) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const el=document.querySelector(arguments[0]);return visible(el);',
      selector
    )
  );
}

async function waitVisible(cdp, selector, timeout = 15000, label = selector) {
  await cdp.waitFor(
    '(() => {' + VISIBLE_SNIPPET + 'return visible(document.querySelector(' + js(selector) + '));})()',
    timeout,
    label
  );
}

async function text(cdp, selector) {
  return cdp.evaluate(pageFn('const el=document.querySelector(arguments[0]);return el?el.innerText:"";', selector));
}

async function count(cdp, selector) {
  return cdp.evaluate(pageFn('return document.querySelectorAll(arguments[0]).length;', selector));
}

async function clickSelector(cdp, selector) {
  const ok = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const el=document.querySelector(arguments[0]);if(!visible(el))return false;' +
        'if(el.disabled)return false;el.click();return true;',
      selector
    )
  );
  if (!ok) throw new Error('找不到可点击元素: ' + selector);
  await sleep(60);
}

async function clickText(cdp, target, options = {}) {
  const selector = options.selector || 'button';
  const root = options.root || 'document';
  const exact = Boolean(options.exact);
  const result = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const root=arguments[2]==="document"?document:document.querySelector(arguments[2]);' +
        'if(!root)return {ok:false,reason:"root"};' +
        'const exact=Boolean(arguments[3]);' +
        'const target=(arguments[0]||"").replace(/\\s+/g," ").trim();' +
        'const targetCompact=target.replace(/\\s+/g,"");' +
        'const nodes=[...root.querySelectorAll(arguments[1])].filter(visible).filter((el)=>!el.disabled);' +
        'const norm=(s)=>(s||"").replace(/\\s+/g," ").trim();' +
        'const compact=(s)=>norm(s).replace(/\\s+/g,"");' +
        'let el=nodes.find((n)=>exact?norm(n.innerText)===target:norm(n.innerText)===target);' +
        'if(!el)el=nodes.find((n)=>compact(n.innerText)===targetCompact);' +
        'if(!el&&!exact)el=nodes.find((n)=>norm(n.innerText).includes(target)||compact(n.innerText).includes(targetCompact));' +
        'if(!el)return {ok:false,reason:"missing",count:nodes.length};' +
        'el.click();return {ok:true,text:norm(el.innerText)};',
      target,
      selector,
      root,
      exact
    )
  );
  if (!result || !result.ok) throw new Error('找不到文本为「' + target + '」的 ' + selector);
  await sleep(80);
  return result.text;
}

async function clickAnyText(cdp, labels, options = {}) {
  let lastError = null;
  for (const label of labels) {
    try {
      return await clickText(cdp, label, options);
    } catch (err) {
      lastError = err;
    }
  }
  throw lastError || new Error('找不到候选文本: ' + labels.join('/'));
}

async function setValue(cdp, selector, value) {
  const result = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const el=document.querySelector(arguments[0]);if(!visible(el))return {ok:false,reason:"missing"};' +
        'if(!(el instanceof HTMLInputElement)&&!(el instanceof HTMLTextAreaElement)&&!(el instanceof HTMLSelectElement))' +
        'return {ok:false,reason:"not-editable"};' +
        'el.focus();const proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:' +
        'el instanceof HTMLSelectElement?HTMLSelectElement.prototype:HTMLInputElement.prototype;' +
        'const setter=Object.getOwnPropertyDescriptor(proto,"value").set;setter.call(el,String(arguments[1]));' +
        'el.dispatchEvent(new Event("input",{bubbles:true}));' +
        'el.dispatchEvent(new Event("change",{bubbles:true}));' +
        'return {ok:true,tag:el.tagName,value:el.value};',
      selector,
      value
    )
  );
  if (!result || !result.ok) throw new Error('无法写入输入控件: ' + selector + ' (' + (result && result.reason) + ')');
  await sleep(40);
  return result;
}

async function selectValue(cdp, selector, value) {
  const result = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const el=document.querySelector(arguments[0]);if(!visible(el))return {ok:false,reason:"missing"};' +
        'if(!(el instanceof HTMLSelectElement))return {ok:false,reason:"not-select"};' +
        'let target=arguments[1];const options=[...el.options];' +
        'let option=options.find((o)=>o.value===target);' +
        'if(!option)option=options.find((o)=>(o.textContent||"").trim()===target);' +
        'if(!option)return {ok:false,reason:"no-option",values:options.map((o)=>o.value)};' +
        'el.focus();el.value=option.value;' +
        'el.dispatchEvent(new Event("input",{bubbles:true}));' +
        'el.dispatchEvent(new Event("change",{bubbles:true}));' +
        'return {ok:true,value:el.value,text:(option.textContent||"").trim()};',
      selector,
      value
    )
  );
  if (!result || !result.ok) {
    throw new Error(
      '无法选择 ' + selector + '=' + value + ' (' + (result && result.reason) + (result && result.values ? ': ' + result.values.join(',') : '') + ')'
    );
  }
  await sleep(80);
  return result;
}

async function setFieldByLabel(cdp, labelText, value, rootSelector = 'document') {
  const result = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const root=arguments[2]==="document"?document:document.querySelector(arguments[2]);' +
        'if(!root)return {ok:false,reason:"root"};' +
        'const target=(arguments[0]||"").replace(/\\s+/g," ").trim();' +
        'const compact=(s)=>(s||"").replace(/\\s+/g,"");' +
        'const targetCompact=compact(target);' +
        'const norm=(s)=>(s||"").replace(/\\s+/g," ").trim();' +
        'const labels=[...root.querySelectorAll("label")].filter(visible);' +
        'const label=labels.find((l)=>compact(l.innerText).includes(targetCompact));' +
        'let el=null;' +
        'if(label){el=label.control||null;' +
        'if(!el&&label.htmlFor){el=document.getElementById(label.htmlFor);}' +
        'if(!el)el=label.querySelector("input,select,textarea");}' +
        'if(!el){const candidates=[...root.querySelectorAll("input,select,textarea")].filter(visible);' +
        'el=candidates.find((c)=>{const n=(c.getAttribute("aria-label")||c.getAttribute("placeholder")||c.name||c.id||"");' +
        'return norm(n).includes(target);})||null;}' +
        'if(!el)return {ok:false,reason:"missing"};' +
        'if(el instanceof HTMLSelectElement){const option=[...el.options].find((o)=>o.value===arguments[1]);' +
        'if(!option)return {ok:false,reason:"no-option"};el.value=option.value;}' +
        'else{const proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;' +
        'Object.getOwnPropertyDescriptor(proto,"value").set.call(el,String(arguments[1]));}' +
        'el.focus();el.dispatchEvent(new Event("input",{bubbles:true}));' +
        'el.dispatchEvent(new Event("change",{bubbles:true}));' +
        'return {ok:true,tag:el.tagName,type:el.type||"",name:el.name||el.id||""};',
      labelText,
      value,
      rootSelector
    )
  );
  if (!result || !result.ok) throw new Error('找不到标签「' + labelText + '」对应控件 (' + (result && result.reason) + ')');
  await sleep(40);
  return result;
}

async function waitForSecret(cdp, timeout = 15000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await cdp.evaluate(
      pageFn(
        VISIBLE_SNIPPET +
          'const inputs=[...document.querySelectorAll("input")].filter(visible);' +
          'const hit=inputs.map((i)=>String(i.value||"")).find((v)=>/^observer_sk_[0-9a-f]{64}$/.test(v));' +
          'return hit||null;'
      )
    );
    if (value) return value;
    await sleep(100);
  }
  throw new Error('等待一次性 secret 超时');
}

async function secretAbsent(cdp) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const inputs=[...document.querySelectorAll("input")];' +
        'return !inputs.some((i)=>visible(i)&&/^observer_sk_[0-9a-f]{64}$/.test(String(i.value||""))) &&' +
        '!/observer_sk_[0-9a-f]{64}/.test(document.body.innerText);'
    )
  );
}

async function dialogSelector(cdp) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const list=[...document.querySelectorAll("dialog,[role=\\"dialog\\"],[aria-modal=\\"true\\"]")]' +
        '.filter(visible);return list.length?list[list.length-1]:null;'
    )
  );
}

async function dialogInfo(cdp) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        NAME_SNIPPET +
        'const list=[...document.querySelectorAll("dialog,[role=\\"dialog\\"],[aria-modal=\\"true\\"]")].filter(visible);' +
        'const el=list[list.length-1];if(!el)return {present:false};' +
        'const r=el.getBoundingClientRect();' +
        'const heading=[...el.querySelectorAll("h1,h2,h3,[role=heading]")].map((n)=>n.innerText.trim()).find(Boolean)||"";' +
        'return {present:true,tag:el.tagName,role:el.getAttribute("role")||"",name:accName(el)||heading,' +
        'text:el.innerText,rect:{left:r.left,top:r.top,right:r.right,bottom:r.bottom,width:r.width,height:r.height},' +
        'vw:window.innerWidth,vh:window.innerHeight,buttons:[...el.querySelectorAll("button,[role=button]")].filter(visible).map((b)=>b.innerText.trim())};'
    )
  );
}

async function waitDialog(cdp, timeout = 10000) {
  await cdp.waitFor(
    '(() => {' + VISIBLE_SNIPPET + 'return [...document.querySelectorAll("dialog,[role=\\"dialog\\"],[aria-modal=\\"true\\"]")].some(visible);})()',
    timeout,
    '对话框出现'
  );
  await sleep(100);
  return dialogInfo(cdp);
}

async function closeDialog(cdp) {
  const info = await dialogInfo(cdp);
  if (!info.present) return false;
  const labels = ['关闭', '取消', '知道了', '完成', '返回'];
  for (const label of labels) {
    try {
      await clickText(cdp, label, { selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button' });
      return true;
    } catch (err) {
      /* 尝试下一个 */
    }
  }
  await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const list=[...document.querySelectorAll("dialog,[role=\\"dialog\\"],[aria-modal=\\"true\\"]")].filter(visible);' +
        'const el=list[list.length-1];if(!el)return false;if(typeof el.close==="function")el.close();' +
        'else el.remove();return true;'
    )
  );
  await sleep(100);
  return true;
}

async function acceptConfirm(cdp, actionWords = []) {
  await sleep(250);
  const info = await dialogInfo(cdp);
  if (!info.present) return '';
  const words = ['确认', '确定', '继续', '保存', '是'].concat(actionWords);
  for (const word of words) {
    try {
      return await clickText(cdp, word, {
        selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button',
        exact: false
      });
    } catch (err) {
      /* 尝试下一个 */
    }
  }
  const buttons = info.buttons.filter((label) => !/取消|关闭|返回|否/.test(label));
  if (buttons.length === 1) {
    await clickText(cdp, buttons[0], {
      selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button',
      exact: true
    });
    return buttons[0];
  }
  return '';
}

async function clickInRow(cdp, rowText, actionText) {
  const result = await cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const norm=(s)=>(s||"").replace(/\\s+/g," ").trim();' +
        'const target=norm(arguments[0]);const action=norm(arguments[1]);' +
        'const rows=[...document.querySelectorAll("tr,[role=row],[data-key-id],.key-row,.card")].filter(visible)' +
        '.filter((row)=>norm(row.innerText).includes(target));' +
        'rows.sort((a,b)=>{const ar=a.getBoundingClientRect(),br=b.getBoundingClientRect();return ar.width*ar.height-br.width*br.height;});' +
        'for(const row of rows){const buttons=[...row.querySelectorAll("button,[role=button]")].filter(visible);' +
        'let el=buttons.find((b)=>norm(b.innerText)===action)||buttons.find((b)=>norm(b.innerText).includes(action));' +
        'if(el&&!el.disabled){el.click();return {ok:true,scope:row.tagName};}}' +
        'for(const row of rows){const opener=[...row.querySelectorAll("button,[role=button]")].filter(visible)' +
        '.find((b)=>/操作|更多|菜单/.test(norm(b.innerText)));if(opener){opener.click();return {ok:false,opened:true};}}' +
        'return {ok:false,opened:false,rows:rows.length};',
      rowText,
      actionText
    )
  );
  if (result && result.ok) {
    await sleep(120);
    return result;
  }
  if (result && result.opened) {
    await sleep(150);
    return clickText(cdp, actionText, { selector: 'button,[role=button]' });
  }
  throw new Error('找不到行「' + rowText + '」的操作「' + actionText + '」');
}

async function overflowAudit(cdp) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const doc=document.documentElement;const body=document.body;' +
        'const clipped=(el)=>{let p=el.parentElement;' +
        'while(p&&p!==document.body){const s=getComputedStyle(p);' +
        'if(/auto|scroll|hidden/.test(s.overflowX)){const pr=p.getBoundingClientRect();' +
        'if(pr.right<=window.innerWidth+1)return true;}p=p.parentElement;}return false;};' +
        'const offenders=[...document.querySelectorAll("body *")].filter(visible).filter((el)=>!clipped(el))' +
        '.map((el)=>{const r=el.getBoundingClientRect();return {tag:el.tagName,id:el.id||"",' +
        'cls:(typeof el.className==="string"?el.className:"").slice(0,80),left:Math.round(r.left),' +
        'right:Math.round(r.right),width:Math.round(r.width)};}).filter((x)=>x.right>window.innerWidth+1)' +
        '.sort((a,b)=>b.right-a.right).slice(0,12);' +
        'return {vw:window.innerWidth,docSW:doc?doc.scrollWidth:0,bodySW:body?body.scrollWidth:0,' +
        'overflow:Math.max(doc?doc.scrollWidth:0,body?body.scrollWidth:0)-window.innerWidth,offenders};'
    )
  );
}

async function injectionAudit(cdp) {
  return cdp.evaluate(
    pageFn(
      'const selectors=[\'img[src="x"]\',\'iframe[srcdoc*="observerXss"]\',\'svg[onload]\',' +
        '\'[onerror*="observerXss"]\',\'[onload*="observerXss"]\'];' +
        'let injected=[];for(const sel of selectors){injected.push(...document.querySelectorAll(sel));}' +
        'return {flag:typeof window.__observerXss==="undefined"?null:window.__observerXss,' +
        'injected:injected.length,' +
        'literalModel:document.body.innerText.includes(arguments[0]),' +
        'literalLabel:document.body.innerText.includes(arguments[1])};',
      mock.XSS.model,
      mock.XSS.label
    )
  );
}

async function accessibilityAudit(cdp, scopeSelector = 'body') {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        NAME_SNIPPET +
        'const scope=arguments[0]==="body"?document.body:' +
        '([...document.querySelectorAll(arguments[0])].filter(visible).pop()||null);' +
        'if(!scope)return {missing:true};' +
        'const controls=[...scope.querySelectorAll("input,select,textarea,button,[role=button]")].filter(visible)' +
        '.filter((el)=>!(el instanceof HTMLInputElement&&el.type==="hidden"));' +
        'const unnamed=controls.filter((el)=>!accName(el)).map((el)=>({tag:el.tagName,' +
        'type:el.getAttribute("type")||"",id:el.id||"",name:el.getAttribute("name")||""}));' +
        'const dialogs=[...document.querySelectorAll("dialog,[role=dialog],[aria-modal=true]")].filter(visible)' +
        '.map((el)=>({tag:el.tagName,role:el.getAttribute("role")||"",name:accName(el)||' +
        '([...el.querySelectorAll("h1,h2,h3,[role=heading]")].map((n)=>n.innerText.trim()).find(Boolean)||"")}));' +
        'const tables=[...scope.querySelectorAll("table")].filter(visible).map((t)=>({' +
        'headers:[...t.querySelectorAll("th,[role=columnheader]")].length,rows:t.querySelectorAll("tbody tr").length}));' +
        'return {missing:false,lang:document.documentElement.lang||"",title:document.title||"",' +
        'controlCount:controls.length,unnamed,dialogs,tables};',
      scopeSelector
    )
  );
}

async function settle(cdp) {
  try {
    await cdp.evaluate(
      'new Promise((resolve)=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve(true))))'
    );
  } catch (err) {
    /* ignore */
  }
  try {
    await cdp.waitFor('document.getAnimations().every((a)=>a.playState!=="running")', 2500, '动画结束');
  } catch (err) {
    /* 动画不是验收阻塞项 */
  }
}

async function capture(cdp, file, fullPage) {
  const params = { format: 'png', fromSurface: true };
  if (fullPage) {
    const metrics = await cdp.send('Page.getLayoutMetrics');
    const size = metrics.cssContentSize || metrics.contentSize;
    const viewportWidth = await cdp.evaluate(pageFn('return window.innerWidth;'));
    // 矩阵按视口宽度取证；页面横向溢出由 overflow 断言单独失败，截图只截
    // 可见视口宽度，避免把 390 的图悄悄扩成 467。
    const width = Math.max(1, Math.min(1600, Math.min(Math.ceil(size.width || viewportWidth), viewportWidth)));
    const height = Math.max(1, Math.min(16384, Math.ceil(size.height || 900)));
    params.captureBeyondViewport = true;
    params.clip = { x: 0, y: 0, width, height, scale: 1 };
  }
  const shot = await cdp.send('Page.captureScreenshot', params, 60000);
  const buffer = Buffer.from(shot.data, 'base64');
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, buffer);
  screenshots.push({ file: path.relative(reportDir, file), bytes: buffer.length });
  return buffer.length;
}

/* -------------------------------------------------------------------------- */
/* Navigation / page readiness                                                */
/* -------------------------------------------------------------------------- */

async function gotoPage(cdp, key) {
  const page = PAGES.find((item) => item.key === key);
  if (!page) throw new Error('unknown page: ' + key);
  await clickText(cdp, page.nav, { selector: 'button, a, [role="tab"], [role="button"]' });
  if (key === 'requests') {
    await cdp.waitFor(
      '(() => {const c=document.querySelector("#content");if(!c)return false;' +
        'return Boolean(c.querySelector("#filters")) || /请求日志|暂无请求|请求记录/.test(c.innerText);})()',
      15000,
      '请求日志页面'
    );
  } else if (key === 'keys') {
    await cdp.waitFor(
      '(() => {const c=document.querySelector("#content");return !!c && /新建\\s*Key/.test(c.innerText);})()',
      15000,
      'Key 管理页面'
    );
  } else {
    await cdp.waitFor(
      '(() => {const c=document.querySelector("#content");return !!c && /捕获|保留|价格|压缩|设置/.test(c.innerText);})()',
      15000,
      '设置页面'
    );
  }
  await sleep(120);
}

async function waitUiRestored(cdp, timeout = 20000) {
  await cdp.waitFor(
    '(() => {' + VISIBLE_SNIPPET + 'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
      'return !!l&&!!c&&!visible(l)&&visible(c)&&!!document.querySelector("#filters");})()',
    timeout,
    '会话恢复并显示请求日志'
  );
  await waitRequestsLoaded(cdp);
}

async function waitRequestsLoaded(cdp) {
  await cdp.waitFor(
    '(() => {const c=document.querySelector("#content");if(!c)return false;' +
      'return c.querySelectorAll("tbody tr").length>0 || /暂无|没有.*请求|空/.test(c.innerText);})()',
    15000,
    '请求列表渲染'
  );
}

async function findModelButton(cdp, model, click) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        'const target=arguments[0];const click=arguments[1];' +
        'const root=document.querySelector("#content")||document;' +
        'const buttons=[...root.querySelectorAll("table button,table [role=button],td button,td [role=button]")].filter(visible);' +
        'let el=buttons.find((b)=>(b.innerText||"").trim()===target)||' +
        'buttons.find((b)=>(b.innerText||"").includes(target));' +
        'if(!el)return {ok:false,count:buttons.length};' +
        'if(click)el.click();return {ok:true,text:(el.innerText||"").trim()};',
      model,
      Boolean(click)
    )
  );
}

async function applyTheme(cdp, theme) {
  if (theme === 'auto') {
    await cdp.send('Emulation.setEmulatedMedia', {
      features: [{ name: 'prefers-color-scheme', value: 'dark' }]
    });
  } else {
    await cdp.send('Emulation.setEmulatedMedia', { features: [] });
  }
  await selectValue(cdp, '#theme', theme);
  await settle(cdp);
  const value = await cdp.evaluate(pageFn('const el=document.querySelector("#theme");return el?el.value:null;'));
  if (value !== theme) throw new Error('#theme 实际值为 ' + value + '，期望 ' + theme);
}

async function setViewport(cdp, size) {
  await cdp.send('Emulation.setDeviceMetricsOverride', {
    width: size.width,
    height: size.height,
    deviceScaleFactor: size.scale,
    mobile: size.mobile,
    screenWidth: size.width,
    screenHeight: size.height
  });
  await sleep(150);
}

/* -------------------------------------------------------------------------- */
/* Polling / fetch gates                                                      */
/* -------------------------------------------------------------------------- */

async function installTimerProbe(cdp) {
  await cdp.evaluate(
    pageFn(
      'if(window.__pollLog)return true;' +
        'const origSet=window.setTimeout.bind(window);' +
        'const origClear=window.clearTimeout.bind(window);' +
        'window.__nativeSetTimeout=origSet;window.__nativeClearTimeout=origClear;' +
        'window.__pollLog={set:[],clears:[]};' +
        'window.setTimeout=function(fn,delay){window.__pollLog.set.push({delay:Number(delay)||0,at:performance.now()});' +
        'return origSet.apply(null,arguments);};' +
        'window.clearTimeout=function(id){window.__pollLog.clears.push({at:performance.now()});' +
        'return origClear.apply(null,arguments);};' +
        'return true;'
    )
  );
}

async function resetTimerProbe(cdp) {
  await cdp.evaluate(
    pageFn('if(window.__pollLog){window.__pollLog.set=[];window.__pollLog.clears=[];}return true;')
  );
}

async function timerStats(cdp) {
  return cdp.evaluate(
    pageFn(
      'const log=window.__pollLog||{set:[],clears:[]};' +
        'return {set:log.set.length,clears:log.clears.length,' +
        'longSet:log.set.filter((entry)=>entry.delay>=1000).length,' +
        'delays:log.set.map((entry)=>entry.delay)};'
    )
  );
}

async function waitForLongTimer(cdp, timeout = 6000) {
  await cdp.waitFor(
    'window.__pollLog && window.__pollLog.set.some((entry)=>entry.delay>=1000)',
    timeout,
    '长轮询定时器'
  );
}

async function setDocumentHidden(cdp, hidden) {
  await cdp.evaluate(
    pageFn(
      'const value=arguments[0];' +
        'Object.defineProperty(document,"hidden",{configurable:true,get:function(){return value;}});' +
        'document.dispatchEvent(new Event("visibilitychange"));return value;',
      hidden
    )
  );
}

async function restoreDocumentHidden(cdp) {
  await cdp.evaluate(
    pageFn('delete document.hidden;document.dispatchEvent(new Event("visibilitychange"));return document.hidden;')
  );
}

async function installFetchGate(cdp, options) {
  const match = options.match;
  const query = options.query || '';
  const delay = Number(options.delay) || 0;
  const honorAbort = options.honorAbort === true;
  const trackAbort = options.trackAbort === true;
  const detachSignal = options.detachSignal === true;
  await cdp.evaluate(
    pageFn(
      'if(window.__restoreFetch)window.__restoreFetch();' +
        'const orig=window.fetch.bind(window);' +
        'const nset=window.__nativeSetTimeout||window.setTimeout.bind(window);' +
        'const nclear=window.__nativeClearTimeout||window.clearTimeout.bind(window);' +
        'const gateMatch=arguments[0];const gateQuery=arguments[1];' +
        'const gateDelay=arguments[2];const gateAbort=arguments[3];' +
        'const gateTrack=arguments[4];const gateDetach=arguments[5];' +
        'window.__fetchLog={matched:0,aborts:0,released:0};' +
        'window.__staleDone=new Promise(function(resolve){window.__staleSettle=resolve;});' +
        'window.__restoreFetch=function(){window.fetch=orig;window.__restoreFetch=null;};' +
        'window.fetch=function(url,requestOptions){' +
        'const opts=requestOptions||{};const u=String(url);' +
        'if(!u.includes(gateMatch)||(gateQuery&&!u.includes(gateQuery)))return orig(url,requestOptions);' +
        'window.__fetchLog.matched++;' +
        'const fetchOptions=gateDetach&&requestOptions?Object.assign({},requestOptions):requestOptions;' +
        'if(gateDetach&&fetchOptions)delete fetchOptions.signal;' +
        'const settled=orig(url,fetchOptions).then(function(value){return {ok:true,value:value};},' +
        'function(error){return {ok:false,value:error};});' +
        'const delayMs=gateDelay;const shouldAbort=gateAbort;' +
        'return new Promise(function(resolve,reject){' +
        'let finished=false;' +
        'const finish=function(fn,value){if(finished)return;finished=true;window.__fetchLog.released++;window.__staleSettle(true);fn(value);};' +
        'const timer=nset(function(){settled.then(function(out){finish(out.ok?resolve:reject,out.value);});},Number(delayMs)||0);' +
        'if((shouldAbort||gateTrack)&&opts.signal){opts.signal.addEventListener("abort",function(){' +
        'window.__fetchLog.aborts++;' +
        'if(shouldAbort){nclear(timer);' +
        'finish(reject,new DOMException("Aborted","AbortError"));}' +
        '},{once:true});}' +
        '});' +
        '};',
      match,
      query,
      delay,
      honorAbort,
      trackAbort,
      detachSignal
    )
  );
}

async function installFetch401(cdp, match) {
  await cdp.evaluate(
    pageFn(
      'if(window.__restoreFetch)window.__restoreFetch();' +
        'const orig=window.fetch.bind(window);' +
        'const faultMatch=arguments[0];' +
        'window.__faultLog={hits:0};' +
        'window.__restoreFetch=function(){window.fetch=orig;window.__restoreFetch=null;};' +
        'window.fetch=function(url,requestOptions){' +
        'const u=String(url);if(!u.includes(faultMatch))return orig(url,requestOptions);' +
        'window.__faultLog.hits++;' +
        'return Promise.resolve(new Response(JSON.stringify({error:{code:"unauthorized",message:"invalid management key"}}),' +
        '{status:401,headers:{"Content-Type":"application/json"}}));' +
        '};',
      match
    )
  );
}

async function restoreFetch(cdp) {
  await cdp.evaluate(pageFn('if(window.__restoreFetch)window.__restoreFetch();return true;'));
}

/* -------------------------------------------------------------------------- */
/* API contract                                                               */
/* -------------------------------------------------------------------------- */

async function apiRequest(server, method, suffix, options = {}) {
  const headers = {};
  if (options.auth !== false) headers.Authorization = 'Bearer ' + (options.key || mock.DEFAULT_MANAGEMENT_KEY);
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(server.origin + mock.MANAGEMENT_BASE + suffix, {
    method,
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body)
  });
  const raw = await response.text();
  let body = null;
  if (raw) {
    try {
      body = JSON.parse(raw);
    } catch (err) {
      body = raw;
    }
  }
  return { status: response.status, headers: response.headers, body };
}

async function runApiContract(server) {
  await step('mock API: 资源公开且带 CSP/no-store', async () => {
    const response = await fetch(server.origin + mock.RESOURCE_BASE + '/ui');
    check('资源 /ui 返回 HTML', response.status === 200 && /text\/html/.test(response.headers.get('content-type') || ''), response.status);
    check('资源带 CSP 且 no-store', response.headers.get('content-security-policy') === mock.ASSET_CSP && response.headers.get('cache-control') === 'no-store');
    check('无 CORS 响应头', response.headers.get('access-control-allow-origin') === null);
    return 'resource=' + response.status;
  });

  await step('mock API: 管理鉴权与标准错误', async () => {
    const unauthorized = await apiRequest(server, 'GET', '/health', { auth: false });
    check(
      '未认证返回 401 + {error:{code,message}}',
      unauthorized.status === 401 && unauthorized.body && unauthorized.body.error && unauthorized.body.error.code === 'unauthorized',
      JSON.stringify(unauthorized.body)
    );
    const health = await apiRequest(server, 'GET', '/health');
    check(
      'GET /health 返回版本与 schema_version=2',
      health.status === 200 && health.body.schema_version === 2 && typeof health.body.version === 'string',
      JSON.stringify(health.body)
    );
    return 'health=' + health.status;
  });

  await step('mock API: 输入拒绝与列表形状', async () => {
    const list = await apiRequest(server, 'GET', '/requests');
    check(
      'GET /requests 返回 page/page_size/has_more/items',
      list.status === 200 && Array.isArray(list.body.items) && typeof list.body.has_more === 'boolean' && list.body.page === 1,
      JSON.stringify(list.body && { page: list.body.page, size: list.body.page_size, items: list.body.items.length })
    );
    const unknown = await apiRequest(server, 'GET', '/requests?unknown=1');
    check('未知查询参数被拒绝', unknown.status === 400 && unknown.body.error && unknown.body.error.code === 'invalid_query');
    const malformed = await apiRequest(server, 'GET', '/requests?from=nope');
    check('非法 RFC3339 被拒绝', malformed.status === 400);
    const tooLarge = await apiRequest(server, 'GET', '/requests?page_size=101');
    check('page_size > 100 被拒绝', tooLarge.status === 400);
    const wrongMethod = await apiRequest(server, 'POST', '/health');
    check('不支持的方法返回 405', wrongMethod.status === 405 && wrongMethod.body.error);
    return 'validation=ok';
  });

  await step('mock API: 详情仅在显式请求时读取', async () => {
    const before = server.counters.detailReads;
    const detail = await apiRequest(server, 'GET', '/requests/req-002');
    check(
      'GET /requests/{id} 返回 redacted 详情',
      detail.status === 200 &&
        detail.body.capture_status === 'redacted' &&
        detail.body.headers.Authorization[0] === 'Bearer [redacted]' &&
        detail.body.body.prompt === mock.XSS.bodyPrompt,
      detail.status
    );
    check('详情读取计数器 +1', server.counters.detailReads === before + 1, server.counters.detailReads);
    const summary = await apiRequest(server, 'GET', '/summary');
    check(
      'GET /summary 返回 granularity/buckets',
      summary.status === 200 && Array.isArray(summary.body.buckets) && typeof summary.body.granularity === 'string',
      JSON.stringify(summary.body && { granularity: summary.body.granularity, buckets: summary.body.buckets.length })
    );
    return 'detail/summary=ok';
  });

  await step('mock API: Key CRUD 与只在创建/轮换返回明文', async () => {
    const created = await apiRequest(server, 'POST', '/keys', {
      body: {
        label: '契约测试 Key',
        enabled: true,
        expires_at: null,
        max_concurrent: 2,
        allowed_models: ['gpt-5-codex'],
        limits: { five_hour: 0, week: 0, month: 0, total: 0 }
      }
    });
    check(
      'POST /keys 返回 201 + key + 明文 secret',
      created.status === 201 && created.body.key && /^observer_sk_[0-9a-f]{64}$/.test(created.body.secret),
      created.status
    );
    const id = created.body.key.id;
    const rotated = await apiRequest(server, 'POST', '/keys/' + encodeURIComponent(id) + '/rotate');
    check('POST rotate 返回新 secret', rotated.status === 200 && rotated.body.secret !== created.body.secret);
    const reset = await apiRequest(server, 'POST', '/keys/' + encodeURIComponent(id) + '/reset', {
      body: { windows: ['five_hour', 'total'] }
    });
    check('POST reset 清零所选窗口', reset.status === 200 && reset.body.used.five_hour === 0 && reset.body.used.total === 0);
    const revoked = await apiRequest(server, 'DELETE', '/keys/' + encodeURIComponent(id));
    check('DELETE /keys/{id} 返回 204', revoked.status === 204);
    return 'keys=ok';
  });

  await step('mock API: settings/prices/compact 全路由', async () => {
    const settings = await apiRequest(server, 'GET', '/settings');
    check(
      'GET /settings 返回 DESIGN 字段',
      settings.status === 200 &&
        typeof settings.body.capture_enabled === 'boolean' &&
        typeof settings.body.max_body_bytes === 'number' &&
        typeof settings.body.raw_retention_hours === 'number' &&
        settings.body.pricing &&
        Array.isArray(settings.body.pricing.prices),
      JSON.stringify(settings.body && Object.keys(settings.body))
    );
    settings.body.capture_enabled = true;
    const saved = await apiRequest(server, 'PUT', '/settings', { body: settings.body });
    check('PUT /settings 回写成功', saved.status === 200 && saved.body.capture_enabled === true, saved.status);
    const prices = await apiRequest(server, 'GET', '/prices');
    check('GET /prices 返回 Snapshot', prices.status === 200 && Array.isArray(prices.body.prices) && typeof prices.body.last_sync === 'string');
    const refreshed = await apiRequest(server, 'POST', '/prices/refresh');
    check('POST /prices/refresh 返回 Snapshot', refreshed.status === 200 && server.counters.priceRefreshes === 1);
    const compact = await apiRequest(server, 'POST', '/compact');
    check('POST /compact 返回 Health', compact.status === 200 && server.counters.compactions === 1);
    return 'settings=ok';
  });

  // 契约测试会改变状态；UI 测试从干净 fixture 开始。
  server.resetState();
  server.resetCalls();
}

/* -------------------------------------------------------------------------- */
/* Browser flow                                                               */
/* -------------------------------------------------------------------------- */

function managementCalls(server) {
  return server.calls.filter((call) => call.kind === 'management');
}

function suffixOf(call) {
  const index = call.path.indexOf(mock.MANAGEMENT_BASE);
  return index >= 0 ? call.path.slice(index + mock.MANAGEMENT_BASE.length) : call.path;
}

function callPattern(call) {
  const suffix = suffixOf(call);
  if (/^\/keys\/[^/]+\/rotate$/.test(suffix)) return 'POST /keys/{id}/rotate';
  if (/^\/keys\/[^/]+\/reset$/.test(suffix)) return 'POST /keys/{id}/reset';
  if (/^\/keys\/[^/]+$/.test(suffix)) return call.method + ' /keys/{id}';
  if (/^\/requests\/[^/]+$/.test(suffix)) return 'GET /requests/{id}';
  return call.method + ' ' + (suffix || '/');
}

async function clearRequestFilters(cdp) {
  const cleared = await cdp.evaluate(
    pageFn(
      'const form=document.querySelector("#filters");if(!form)return false;' +
        'for(const el of form.querySelectorAll("input,select")){' +
        'if(el.tagName==="SELECT"){const blank=[...el.options].find((option)=>option.value==="")||el.options[0];' +
        'if(blank)el.value=blank.value;}else{el.value="";}' +
        'el.dispatchEvent(new Event("input",{bubbles:true}));' +
        'el.dispatchEvent(new Event("change",{bubbles:true}));}' +
        'return true;'
    )
  );
  if (!cleared) throw new Error('找不到请求筛选表单');
}

async function clickSettingsTestId(cdp, testId) {
  const result = await cdp.evaluate(
    pageFn(
      `
      const id=arguments[0];
      const el=document.querySelector('[data-testid="'+id+'"]');
      if(!el)return {ok:false,reason:'missing'};
      el.click();
      return {ok:true,id:el.id||''};
      `,
      testId
    )
  );
  if (!result || !result.ok) throw new Error('找不到设置控件 data-testid=' + testId);
  await sleep(40);
  return result;
}

async function pricingRowCount(cdp, kind) {
  return cdp.evaluate(
    pageFn(
      `
      return document.querySelectorAll('[data-testid="observer-settings"] [data-kind="'+arguments[0]+'"]').length;
      `,
      kind
    )
  );
}

async function waitPricingRowCount(cdp, kind, expected, timeout = 5000) {
  await cdp.waitFor(
    '(() => {return document.querySelectorAll(\'[data-testid="observer-settings"] [data-kind="' +
      kind +
      '"]\').length===' +
      expected +
      ';})()',
    timeout,
    '价格行数量 ' + kind + '=' + expected
  );
}

async function addPricingRow(cdp, kind) {
  const before = await pricingRowCount(cdp, kind);
  await clickSettingsTestId(cdp, kind === 'manual' ? 'manual-price-add' : 'rule-add');
  await waitPricingRowCount(cdp, kind, before + 1);
  return before + 1;
}

async function settingsFirstManualId(cdp) {
  return cdp.evaluate(
    pageFn(
      `
      const el=document.querySelector('[data-testid="observer-settings"] [data-kind="manual"] input[id*="-manual-model-"]');
      return el?el.id:null;
      `
    )
  );
}

async function waitSettingsReloaded(cdp, before, timeout = 15000) {
  await cdp.waitFor(
    '(() => {const el=document.querySelector(\'[data-testid="observer-settings"] [data-kind="manual"] input[id*="-manual-model-"]\');' +
      'return !!el&&el.id!==' +
      js(before) +
      ';})()',
    timeout,
    '设置页重新加载'
  );
  await settle(cdp);
}

async function pricingEditorState(cdp) {
  return cdp.evaluate(
    pageFn(
      `
      const root=document.querySelector('[data-testid="observer-settings"]');
      if(!root)return {present:false};
      const read=function(el){return el?{id:el.id,value:String(el.value)}:null;};
      const field=function(row,selector){return read(row.querySelector(selector));};
      const rate=function(row,key){return read(row.querySelector('input[id$="-'+key+'"]'));};
      const rates=function(row){const out={};for(const key of ['input','output','cache_read','cache_write','reasoning'])out[key]=rate(row,key);return out;};
      const auto=root.querySelector('[data-testid="settings-pricing-auto"]');
      return {
        present:true,
        auto:!!auto&&auto.checked,
        prices:[...root.querySelectorAll('[data-kind="manual"]')].map(function(row){
          return {model:field(row,'input[id*="-manual-model-"]'),rates:rates(row)};
        }),
        rules:[...root.querySelectorAll('[data-kind="rule"]')].map(function(row){
          return {id:field(row,'input[id*="-rule-id-"]'),model:field(row,'input[id*="-rule-model-"]'),
            threshold:field(row,'input[id*="-rule-threshold-"]'),range:field(row,'input[id*="-rule-range-"]'),
            rates:rates(row)};
        })
      };
      `
    )
  );
}

async function setPricingRow(cdp, kind, index, patch) {
  const result = await cdp.evaluate(
    pageFn(
      `
      const kind=arguments[0],index=arguments[1],patch=arguments[2];
      const rows=[...document.querySelectorAll('[data-testid="observer-settings"] [data-kind="'+kind+'"]')];
      const row=rows[index];
      if(!row)return {ok:false,reason:'missing-row',count:rows.length};
      const errors=[];
      const set=function(selector,value){
        const el=row.querySelector(selector);
        if(!el){errors.push('missing '+selector);return;}
        el.value=String(value);
        el.dispatchEvent(new Event('input',{bubbles:true}));
        el.dispatchEvent(new Event('change',{bubbles:true}));
      };
      if(Object.prototype.hasOwnProperty.call(patch,'model')){
        set(kind==='rule'?'input[id*="-rule-model-"]':'input[id*="-manual-model-"]',patch.model);
      }
      if(kind==='rule'){
        if(patch.id!==undefined)set('input[id*="-rule-id-"]',patch.id);
        if(patch.threshold!==undefined)set('input[id*="-rule-threshold-"]',patch.threshold);
        if(patch.range!==undefined)set('input[id*="-rule-range-"]',patch.range);
      }
      if(patch.rates){
        for(const key of ['input','output','cache_read','cache_write','reasoning']){
          if(Object.prototype.hasOwnProperty.call(patch.rates,key))set('input[id$="-'+key+'"]',patch.rates[key]);
        }
      }
      return {ok:errors.length===0,errors:errors,count:rows.length};
      `,
      kind,
      index,
      patch
    )
  );
  if (!result || !result.ok) throw new Error('设置价格字段失败: ' + JSON.stringify(result));
  await sleep(40);
  return result;
}

async function moveLastRuleUp(cdp) {
  const ok = await cdp.evaluate(
    pageFn(
      `
      const rows=[...document.querySelectorAll('[data-testid="observer-settings"] [data-kind="rule"]')];
      const row=rows[rows.length-1];if(!row)return false;
      const up=row.querySelector('[data-testid="rule-up"]');
      if(!up||up.disabled)return false;
      up.click();return true;
      `
    )
  );
  if (!ok) throw new Error('无法上移最后一条规则');
  await sleep(40);
}

async function waitPricingStatus(cdp, pattern, label, timeout = 5000) {
  await cdp.waitFor(
    '(() => {const el=document.querySelector(\'[id$="-pricing-status"]\');' +
      'return !!el&&' +
      pattern +
      '.test(el.innerText);})()',
    timeout,
    label
  );
}

async function settingsGeneralState(cdp) {
  return cdp.evaluate(
    pageFn(
      `
      const value=function(suffix){const el=document.querySelector('input[id$="'+suffix+'"]');return el?String(el.value):null;};
      const capture=document.querySelector('[data-testid="settings-capture-enabled"]');
      return {capture:!!capture&&capture.checked,
        max_body_bytes:value('-max-body-bytes'),
        max_body_storage_bytes:value('-max-body-storage-bytes'),
        raw_retention_hours:value('-raw-retention-hours'),
        daily_retention_days:value('-daily-retention-days')};
      `
    )
  );
}

async function setGeneralSettings(cdp, values) {
  await cdp.evaluate(
    pageFn(
      `
      const values=arguments[0];
      const capture=document.querySelector('[data-testid="settings-capture-enabled"]');
      if(!capture)throw new Error('missing capture');
      capture.checked=!!values.capture;
      capture.dispatchEvent(new Event('change',{bubbles:true}));
      for(const pair of [['-max-body-bytes','max_body_bytes'],['-max-body-storage-bytes','max_body_storage_bytes'],
        ['-raw-retention-hours','raw_retention_hours'],['-daily-retention-days','daily_retention_days']]){
        const el=document.querySelector('input[id$="'+pair[0]+'"]');
        if(!el)throw new Error('missing '+pair[0]);
        el.value=String(values[pair[1]]);
        el.dispatchEvent(new Event('input',{bubbles:true}));
        el.dispatchEvent(new Event('change',{bubbles:true}));
      }
      return true;
      `,
      values
    )
  );
  await sleep(40);
}

function stableJson(value) {
  if (Array.isArray(value)) return '[' + value.map((item) => stableJson(item)).join(',') + ']';
  if (value && typeof value === 'object') {
    return '{' + Object.keys(value).sort().map((key) => JSON.stringify(key) + ':' + stableJson(value[key])).join(',') + '}';
  }
  return JSON.stringify(value);
}

function fixedAssetAudit(server, since) {
  const assets = server.calls.filter((call) => call.seq > since && call.kind === 'asset');
  const js = assets.find((call) => call.path === mock.RESOURCE_BASE + '/ui.js');
  const css = assets.find((call) => call.path === mock.RESOURCE_BASE + '/ui.css');
  const failed = assets.filter((call) => call.status >= 400);
  const relative = assets.filter((call) => /\/ui\/(ui\.js|ui\.css)$/.test(call.path));
  return {
    ok: Boolean(js && css && js.status === 200 && css.status === 200 && failed.length === 0 && relative.length === 0),
    js: js ? { path: js.path, status: js.status } : null,
    css: css ? { path: css.path, status: css.status } : null,
    failed: failed.map((call) => call.path + ':' + call.status),
    relative: relative.map((call) => call.path),
    all: assets.map((call) => call.path + ':' + call.status)
  };
}

function settingsPutCount(server) {
  return managementCalls(server).filter((call) => call.method === 'PUT' && suffixOf(call) === '/settings').length;
}

async function waitForCall(server, predicate, timeout = 15000, label = 'API 调用') {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const call = managementCalls(server).find(predicate);
    if (call) return call;
    await sleep(100);
  }
  throw new Error('等待超时: ' + label + '；已记录 ' + managementCalls(server).map(callPattern).join(' | '));
}

function absorbExpectedAuthDiagnostics(since) {
  const phaseErrors = consoleErrors.splice(since);
  const expected = phaseErrors.filter((entry) =>
    /401|unauthorized|invalid management key|登录失败|Failed to load resource/i.test(entry.text)
  );
  expectedDiagnostics.push(...expected);
  consoleErrors.push(...phaseErrors.filter((entry) => !expected.includes(entry)));
}

async function login(cdp, password) {
  await waitVisible(cdp, '#login', 15000, '登录表单');
  const passwordSelector = '#login input[type="password"], #login input[name="password"]';
  await setValue(cdp, passwordSelector, password);
  await clickText(cdp, '登录', { selector: '#login button, #login [role="button"], button[type="submit"]' });
}

async function runBrowserSuite(server, cdp) {
  let requestsReady = false;
  let keysReady = false;
  let settingsReady = false;

  await step('登录门禁: 初始只有登录界面', async () => {
    await cdp.send('Page.navigate', { url: server.resourceURL });
    await waitVisible(cdp, '#login', 15000, '登录表单');
    check('登录界面可见', true);
    check(
      '主内容未登录时不可见或为空',
      await cdp.evaluate(
        pageFn(
          VISIBLE_SNIPPET +
            'const c=document.querySelector("#content");return !visible(c)||!c.innerText.trim();'
        )
      )
    );
    const dataCalls = managementCalls(server).filter((call) => /^\/(requests|keys|summary|settings)/.test(suffixOf(call)));
    check('登录前没有读取业务数据', dataCalls.length === 0, dataCalls.map(callPattern).join(','));
    const loginAudit = await accessibilityAudit(cdp, '#login');
    check(
      '登录可访问性: 密码框与按钮有名称',
      !loginAudit.missing && loginAudit.unnamed.length === 0,
      JSON.stringify(loginAudit.unnamed)
    );
    return 'login visible';
  });

  await step('登录错误密钥: 401 且停留在登录界面', async () => {
    const errorStart = consoleErrors.length;
    await login(cdp, 'wrong-browser-secret');
    await sleep(700);
    const unauthorized = server.calls.find((call) => call.kind === 'management' && call.status === 401);
    check('错误密钥触发 401', Boolean(unauthorized), JSON.stringify(server.calls.slice(-3).map(callPattern)));
    check('错误密钥后仍在登录界面', await visible(cdp, '#login'));
    const visibleText = await text(cdp, 'body');
    check('界面显示登录失败而不是静默', /失败|错误|无效|401|invalid/i.test(visibleText), visibleText.slice(0, 200));
    const loginMessage = await text(cdp, '#message');
    check('认证错误不附加历史查询提示', !/历史|partial|边界/i.test(loginMessage), loginMessage);
    // 401 是刻意注入的负向场景；浏览器会记录网络错误。只豁免这次鉴权失败相关
    // 的诊断，其它 console/page 错误仍必须为零。
    const phaseErrors = consoleErrors.splice(errorStart);
    const expected = phaseErrors.filter((entry) => /401|unauthorized|invalid management key|登录失败|Failed to load resource/i.test(entry.text));
    expectedDiagnostics.push(...expected);
    consoleErrors.push(...phaseErrors.filter((entry) => !expected.includes(entry)));
    return '401 expected';
  });

  await step('登录成功并进入请求日志', async () => {
    await login(cdp, mock.DEFAULT_MANAGEMENT_KEY);
    await cdp.waitFor(
      '(() => {' + VISIBLE_SNIPPET + 'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
        'return !visible(l)&&visible(c);})()',
      20000,
      '登录后主界面'
    );
    check('登录后主界面可见', true);
    const authenticated = managementCalls(server).filter((call) => call.status !== 401);
    check(
      '登录后管理请求使用正确密钥',
      authenticated.length > 0 && authenticated.every((call) => call.authorized),
      authenticated.map((call) => call.status + ':' + call.authorized).join(',')
    );
    check('管理密钥不出现在 URL', server.calls.every((call) => !call.path.includes(mock.DEFAULT_MANAGEMENT_KEY) && !JSON.stringify(call.query).includes(mock.DEFAULT_MANAGEMENT_KEY)));
    await waitRequestsLoaded(cdp);
    requestsReady = true;
    return 'authenticated';
  });

  if (requestsReady) {
    await step('轮询: 默认 5 秒真实请求，关闭后不再调度', async () => {
      await installTimerProbe(cdp);
      const defaultInterval = await cdp.evaluate(
        pageFn('const el=document.querySelector("#interval");return el?el.value:null;')
      );
      check('默认 #interval=5', defaultInterval === '5', String(defaultInterval));

      const startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      const started = Date.now();
      const pollCall = await waitForCall(
        server,
        (call) => call.seq > startSeq && call.method === 'GET' && suffixOf(call) === '/requests',
        8500,
        '默认 5 秒轮询'
      );
      const elapsed = Date.now() - started;
      check(
        '默认 5 秒轮询产生真实 GET /requests',
        elapsed >= 3500 && elapsed <= 9000,
        elapsed + 'ms seq=' + pollCall.seq
      );

      await selectValue(cdp, '#interval', '0');
      await resetTimerProbe(cdp);
      const offSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      await clickSelector(cdp, '#refresh');
      await waitForCall(
        server,
        (call) => call.seq > offSeq && call.method === 'GET' && suffixOf(call) === '/requests',
        15000,
        '关闭轮询后的手动刷新'
      );
      const stats = await timerStats(cdp);
      check(
        '关闭自动刷新后不再调度长定时器',
        stats.longSet === 0,
        JSON.stringify(stats)
      );
      return 'default=' + defaultInterval + ', elapsed=' + elapsed + 'ms';
    });

    await step('顶栏: 主题/刷新间隔/刷新控件', async () => {
      const themes = await cdp.evaluate(
        pageFn('const el=document.querySelector("#theme");return el?[...el.options].map((o)=>o.value):[];')
      );
      check(
        '#theme 提供 white/light/dark/auto',
        THEMES.every((theme) => themes.includes(theme)),
        JSON.stringify(themes)
      );
      const intervals = await cdp.evaluate(
        pageFn('const el=document.querySelector("#interval");return el?[...el.options].map((o)=>o.value):[];')
      );
      check(
        '#interval 提供 0/5/15/30/60',
        ['0', '5', '15', '30', '60'].every((value) => intervals.includes(value)),
        JSON.stringify(intervals)
      );
      await selectValue(cdp, '#interval', '0');
      const before = managementCalls(server).filter((call) => call.method === 'GET' && suffixOf(call) === '/requests').length;
      const startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      await clickSelector(cdp, '#refresh');
      const refreshed = await waitForCall(
        server,
        (call) => call.seq > startSeq && call.method === 'GET' && suffixOf(call) === '/requests',
        15000,
        '刷新请求'
      );
      check('刷新后请求计数增加', managementCalls(server).filter((call) => call.method === 'GET' && suffixOf(call) === '/requests').length > before, refreshed.seq);
      check('#refresh 触发新的 GET /requests', true);
      const topAudit = await accessibilityAudit(cdp, 'body');
      check(
        '顶栏控件均有可访问名称',
        topAudit.unnamed.length === 0,
        JSON.stringify(topAudit.unnamed.slice(0, 8))
      );
      check('文档语言与标题存在', Boolean(topAudit.lang && topAudit.title), JSON.stringify({ lang: topAudit.lang, title: topAudit.title }));
      return 'interval=' + intervals.join('/');
    });

    await step('请求日志: 筛选参数与安全文本', async () => {
      await gotoPage(cdp, 'requests');
      await waitRequestsLoaded(cdp);
      const controls = await cdp.evaluate(
        pageFn(
          'const root=document.querySelector("#filters");if(!root)return null;' +
            'return [...root.querySelectorAll("input,select,textarea")].map((el)=>({' +
            'name:el.getAttribute("name")||"",id:el.id||"",type:el.type||""}));'
        )
      );
      check('#filters 存在', Boolean(controls));
      const names = (controls || []).map((control) => control.name || control.id);
      const required = ['from', 'to', 'key_id', 'model', 'provider', 'auth_index', 'status'];
      check(
        '#filters 含 from/to/key_id/model/provider/auth_index/status',
        required.every((name) => names.includes(name)),
        JSON.stringify(names)
      );

      const audit = await accessibilityAudit(cdp, '#filters');
      check('筛选控件均有可访问名称', audit.unnamed.length === 0, JSON.stringify(audit.unnamed));
      const contentAudit = await accessibilityAudit(cdp, '#content');
      check(
        '请求表存在表头',
        contentAudit.tables.length > 0 && contentAudit.tables.every((table) => table.headers > 0),
        JSON.stringify(contentAudit.tables)
      );

      const modelInput = await cdp.evaluate(
        pageFn(
          'const root=document.querySelector("#filters");if(!root)return null;' +
            'const el=root.querySelector(\'[name="model"],#model,[data-filter="model"]\');return el?{name:el.name||el.id}:null;'
        )
      );
      const statusInput = await cdp.evaluate(
        pageFn(
          'const root=document.querySelector("#filters");if(!root)return null;' +
            'const el=root.querySelector(\'[name="status"],#status,[data-filter="status"]\');' +
            'return el?{name:el.name||el.id,tag:el.tagName}:null;'
        )
      );
      check('模型筛选控件存在', Boolean(modelInput));
      check('状态筛选控件存在', Boolean(statusInput));
      if (modelInput) await setValue(cdp, '#filters [name="model"], #filters #model, #filters [data-filter="model"]', 'gpt-5-codex');
      if (statusInput) {
        const selector = '#filters [name="status"], #filters #status, #filters [data-filter="status"]';
        if (statusInput.tag === 'SELECT') await selectValue(cdp, selector, 'failed');
        else await setValue(cdp, selector, 'failed');
      }
      await clickAnyText(cdp, ['筛选', '查询', '应用', '搜索', '过滤'], { selector: '#filters button, button' });
      const filtered = await waitForCall(
        server,
        (call) =>
          call.method === 'GET' &&
          suffixOf(call) === '/requests' &&
          call.query.model === 'gpt-5-codex' &&
          call.query.status === 'failed' &&
          (call.query.page === undefined || call.query.page === '1'),
        15000,
        '筛选后的请求列表'
      );
      check(
        '查询按钮重置到第 1 页并带 model/status',
        filtered.query.model === 'gpt-5-codex' && filtered.query.status === 'failed',
        JSON.stringify(filtered.query)
      );

      // 恢复为全部，避免影响后续截图矩阵。
      if (modelInput) await setValue(cdp, '#filters [name="model"], #filters #model, #filters [data-filter="model"]', '');
      if (statusInput) {
        const selector = '#filters [name="status"], #filters #status, #filters [data-filter="status"]';
        if (statusInput.tag === 'SELECT') await selectValue(cdp, selector, '');
        else await setValue(cdp, selector, '');
      }
      await clickAnyText(cdp, ['筛选', '查询', '应用', '搜索', '过滤'], { selector: '#filters button, button' });
      await sleep(400);

      const injection = await injectionAudit(cdp);
      check('恶意模型只作为文本存在', injection.flag === null && injection.injected === 0, JSON.stringify(injection));
      check('页面保留恶意模型字面量作为文本', injection.literalModel, JSON.stringify(injection));
      return JSON.stringify(names);
    });

    await step('请求日志: 详情只在点击行后读取', async () => {
      await gotoPage(cdp, 'requests');
      await waitRequestsLoaded(cdp);
      const before = server.counters.detailReads;
      const beforeCalls = managementCalls(server).filter((call) => /^\/requests\/[^/]+$/.test(suffixOf(call))).length;
      check('列表渲染后未读取详情', beforeCalls === 0 && before === 0, 'detailCalls=' + beforeCalls);

      const clicked = await findModelButton(cdp, mock.XSS.model, true);
      check('日志表恶意模型也是可点击按钮', clicked && clicked.ok, JSON.stringify(clicked));
      const dialog = await waitDialog(cdp, 15000);
      check('点击后出现详情对话框', dialog.present, JSON.stringify(dialog.buttons));
      const afterCalls = managementCalls(server).filter((call) => /^\/requests\/[^/]+$/.test(suffixOf(call))).length;
      check('点击后恰好新增一次详情读取', afterCalls === beforeCalls + 1, 'detailCalls=' + afterCalls);
      const detailCall = managementCalls(server).filter((call) => /^\/requests\/[^/]+$/.test(suffixOf(call))).pop();
      check('详情读取的是被点击的恶意记录', detailCall && /\/requests\/req-002$/.test(detailCall.path), detailCall && detailCall.path);
      check(
        '详情对话框有可访问名称',
        Boolean(dialog.name),
        JSON.stringify({ tag: dialog.tag, role: dialog.role, name: dialog.name })
      );
      check('详情对话框不泄漏假密钥', !dialog.text.includes(mock.XSS.leakedSecret), dialog.text.slice(0, 240));
      check(
        '详情显示脱敏标记或省略敏感 header',
        /redacted|已脱敏|脱敏/.test(dialog.text) || !/Authorization/i.test(dialog.text),
        dialog.text.slice(0, 240)
      );
      const injection = await injectionAudit(cdp);
      check('详情恶意 header/body 不生成元素或脚本', injection.flag === null && injection.injected === 0, JSON.stringify(injection));
      check('详情明文脚本只作为文本', dialog.text.includes(mock.XSS.bodyPrompt), dialog.text.slice(0, 240));
      const rectOk = dialog.rect.left >= -1 && dialog.rect.top >= -1 && dialog.rect.right <= dialog.vw + 1;
      check('详情对话框位于视口内', rectOk, JSON.stringify(dialog.rect));
      await closeDialog(cdp);
      return 'detail=' + afterCalls;
    });
  }

  await step('Key 管理: 列表/创建/编辑/启停/轮换/重置/撤销', async () => {
    await gotoPage(cdp, 'keys');
    keysReady = true;
    await cdp.waitFor(
      '(() => {const c=document.querySelector("#content");return !!c && c.querySelectorAll("tbody tr,[data-key-id],.key-row,.card").length>0;})()',
      15000,
      'Key 列表'
    );
    const pageText = await text(cdp, '#content');
    check('Key 列表展示假 Key', /生产 Key|已过期 Key/.test(pageText), pageText.slice(0, 200));

    const injection = await injectionAudit(cdp);
    check('恶意 Key 标签只作为文本', injection.flag === null && injection.injected === 0, JSON.stringify(injection));
    check('页面保留恶意 Key 标签字面量', pageText.includes(mock.XSS.label), pageText.slice(0, 200));

    const listAudit = await accessibilityAudit(cdp, '#content');
    check('Key 页面控件均有可访问名称', listAudit.unnamed.length === 0, JSON.stringify(listAudit.unnamed));

    await clickText(cdp, '新建 Key', { selector: '#content button, button' });
    const form = await waitDialog(cdp);
    check('新建 Key 打开对话框', form.present);
    const formAudit = await accessibilityAudit(cdp, 'dialog, [role="dialog"], [aria-modal="true"]');
    check('Key 表单控件均有可访问名称', formAudit.unnamed.length === 0, JSON.stringify(formAudit.unnamed));
    check('Key 表单对话框有可访问名称', Boolean(form.name), JSON.stringify({ tag: form.tag, role: form.role, name: form.name }));
    for (const label of ['标签', '到期时间', '并发上限', '允许模型', '5小时额度', '周额度', '月额度', '总额度']) {
      const compactText = form.text.replace(/\s+/g, '');
      check('Key 表单含标签「' + label + '」', compactText.includes(label.replace(/\s+/g, '')), form.text.slice(0, 300));
    }

    const createdLabel = '自动化验收 Key';
    await setFieldByLabel(cdp, '标签', createdLabel, 'dialog, [role="dialog"], [aria-modal="true"]');
    await setFieldByLabel(cdp, '并发上限', '3', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    await setFieldByLabel(cdp, '允许模型', 'gpt-5-codex\nclaude-opus-4-1', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    await setFieldByLabel(cdp, '5小时额度', '5', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    await setFieldByLabel(cdp, '周额度', '20', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    await setFieldByLabel(cdp, '月额度', '50', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    await setFieldByLabel(cdp, '总额度', '100', 'dialog, [role="dialog"], [aria-modal="true"]').catch(() => {});
    const createBefore = server.state.counters.keyCreates;
    await clickText(cdp, '保存', { selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button' });
    const createdCall = await waitForCall(
      server,
      (call) => call.method === 'POST' && suffixOf(call) === '/keys' && call.request_body && call.request_body.label === createdLabel,
      20000,
      'POST /keys'
    );
    check('保存创建 Key 发送 POST /keys', createdCall.status === 201 && server.state.counters.keyCreates === createBefore + 1);
    const key = server.state.keys.find((item) => item.label === createdLabel);
    check('mock 创建了 Key', Boolean(key), server.state.keys.map((item) => item.label).join('|'));
    await sleep(300);
    const createdSecret = await waitForSecret(cdp);
    check('创建后一次性展示假 secret', /^observer_sk_[0-9a-f]{64}$/.test(createdSecret), createdSecret.slice(0, 16) + '…');
    check('secret 不出现在 URL', server.calls.every((call) => !call.path.includes('observer_sk_')));
    await closeDialog(cdp).catch(() => {});
    await sleep(150);
    check('关闭后 secret 从页面清除', await secretAbsent(cdp));

    if (key) {
      const editedLabel = '自动化验收 Key（编辑）';
      await cdp.waitFor(
        '(() => {' + VISIBLE_SNIPPET + 'return document.querySelector("#content").innerText.includes(' + js(createdLabel) + ');})()',
        15000,
        '新 Key 出现在列表'
      );
      await clickInRow(cdp, createdLabel, '编辑');
      const editDialog = await waitDialog(cdp);
      check('编辑打开对话框', editDialog.present);
      await setFieldByLabel(cdp, '标签', editedLabel, 'dialog, [role="dialog"], [aria-modal="true"]');
      await clickText(cdp, '保存', { selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button' });
      const editCall = await waitForCall(
        server,
        (call) => call.method === 'PUT' && suffixOf(call) === '/keys/' + key.id && call.request_body && call.request_body.label === editedLabel,
        20000,
        'PUT /keys/{id}'
      );
      check('编辑发送 PUT /keys/{id}', editCall.status === 200);
      await sleep(300);
      await closeDialog(cdp).catch(() => {});

      const toggle = async (action, enabled) => {
        await cdp.waitFor(
          '(() => {' + VISIBLE_SNIPPET +
            'const norm=(s)=>(s||"").replace(/\\s+/g," ").trim();' +
            'const rows=[...document.querySelectorAll("tr,[role=row],[data-key-id],.key-row,.card")].filter(visible)' +
            '.filter((row)=>norm(row.innerText).includes(' + js(editedLabel) + '));' +
            'return rows.some((row)=>[...row.querySelectorAll("button,[role=button]")].filter(visible)' +
            '.some((b)=>{const t=norm(b.innerText);return t.includes(' + js(action) + ')||/操作|更多|菜单/.test(t);}));})()',
          15000,
          '编辑后的 Key 出现操作按钮'
        );
        const startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
        await clickInRow(cdp, editedLabel, action);
        await acceptConfirm(cdp, [action]);
        const call = await waitForCall(
          server,
          (item) =>
            item.seq > startSeq &&
            item.method === 'PUT' &&
            suffixOf(item) === '/keys/' + key.id &&
            item.request_body &&
            item.request_body.enabled === enabled,
          15000,
          action + ' -> PUT /keys/{id}'
        );
        return call;
      };
      const disabledCall = await toggle('停用', false);
      check('停用发送 enabled=false 的 PUT', disabledCall.request_body.enabled === false);
      const enabledCall = await toggle('启用', true);
      check('启用发送 enabled=true 的 PUT', enabledCall.request_body.enabled === true);

      await clickInRow(cdp, editedLabel, '轮换');
      await acceptConfirm(cdp, ['轮换']);
      const rotateCall = await waitForCall(
        server,
        (call) => call.method === 'POST' && suffixOf(call) === '/keys/' + key.id + '/rotate',
        15000,
        'POST rotate'
      );
      check('轮换发送 POST /keys/{id}/rotate', rotateCall.status === 200);
      await sleep(250);
      const rotatedSecret = await waitForSecret(cdp);
      check('轮换后再次展示假 secret', /^observer_sk_[0-9a-f]{64}$/.test(rotatedSecret), rotatedSecret.slice(0, 16) + '…');
      await closeDialog(cdp).catch(() => {});
      await sleep(150);
      check('关闭轮换 secret 后从页面清除', await secretAbsent(cdp));

      await clickInRow(cdp, editedLabel, '重置');
      await sleep(300);
      const resetDialog = await dialogInfo(cdp);
      if (resetDialog.present) {
        await cdp.evaluate(
          pageFn(
            VISIBLE_SNIPPET +
              'const d=[...document.querySelectorAll("dialog,[role=dialog],[aria-modal=true]")].filter(visible).pop();' +
              'if(!d)return 0;const boxes=[...d.querySelectorAll(\'input[type="checkbox"]\')].filter(visible);' +
              'boxes.forEach((b)=>{if(!b.checked)b.click();});return boxes.length;'
          )
        );
        await acceptConfirm(cdp, ['重置']);
      }
      const resetCall = await waitForCall(
        server,
        (call) => call.method === 'POST' && suffixOf(call) === '/keys/' + key.id + '/reset',
        15000,
        'POST reset'
      );
      check(
        '重置发送 POST /keys/{id}/reset 且包含窗口',
        resetCall.status === 200 && resetCall.request_body && Array.isArray(resetCall.request_body.windows) && resetCall.request_body.windows.length > 0,
        JSON.stringify(resetCall.request_body)
      );
      await closeDialog(cdp).catch(() => {});

      await clickInRow(cdp, editedLabel, '撤销');
      await acceptConfirm(cdp, ['撤销', '删除']);
      const revokeCall = await waitForCall(
        server,
        (call) => call.method === 'DELETE' && suffixOf(call) === '/keys/' + key.id,
        15000,
        'DELETE /keys/{id}'
      );
      check('撤销发送 DELETE /keys/{id}', revokeCall.status === 204);
    }
    return 'key actions';
  }).then((ok) => {
    keysReady = keysReady || ok;
  });

  await step('设置: 卡片标签/保存/价格刷新/物理压缩', async () => {
    await gotoPage(cdp, 'settings');
    settingsReady = true;
    const pageText = await text(cdp, '#content');
    const modules = ['捕获', '保留', '价格', '压缩'].filter((word) => pageText.includes(word));
    check('设置页包含捕获/保留/价格/压缩模块标签', modules.length >= 3, pageText.slice(0, 300));
    check('设置页显示容量/保留字段', /MiB|MiB|字节|小时|天/.test(pageText), pageText.slice(0, 300));

    const pageAudit = await accessibilityAudit(cdp, '#content');
    check('设置控件均有可访问名称', pageAudit.unnamed.length === 0, JSON.stringify(pageAudit.unnamed));
    check('设置对话框（如有）有名称', pageAudit.dialogs.every((dialog) => Boolean(dialog.name)), JSON.stringify(pageAudit.dialogs));

    const settingsBefore = JSON.stringify(server.state.settings);
    const pricingBefore = JSON.parse(JSON.stringify(server.state.settings.pricing));
    const manualIdBefore = await settingsFirstManualId(cdp);
    await setGeneralSettings(cdp, {
      capture: true,
      max_body_bytes: 2097152,
      max_body_storage_bytes: 536870912,
      raw_retention_hours: 48,
      daily_retention_days: 730
    });
    await clickSettingsTestId(cdp, 'settings-general-save');
    const saveCall = await waitForCall(
      server,
      (call) => call.method === 'PUT' && suffixOf(call) === '/settings',
      20000,
      'PUT /settings'
    );
    check(
      '保存设置发送 PUT /settings',
      saveCall.status === 200 && saveCall.request_body && typeof saveCall.request_body.capture_enabled === 'boolean',
      JSON.stringify(saveCall.request_body && Object.keys(saveCall.request_body))
    );
    check(
      '设置保存包含 DESIGN 全字段',
      saveCall.request_body &&
        ['capture_enabled', 'max_body_bytes', 'max_body_storage_bytes', 'raw_retention_hours', 'daily_retention_days', 'pricing'].every(
          (field) => Object.prototype.hasOwnProperty.call(saveCall.request_body, field)
        ),
      JSON.stringify(saveCall.request_body && Object.keys(saveCall.request_body))
    );
    check(
      'general save 使用实际字段值',
      saveCall.request_body &&
        saveCall.request_body.capture_enabled === true &&
        saveCall.request_body.max_body_bytes === 2097152 &&
        saveCall.request_body.max_body_storage_bytes === 536870912 &&
        saveCall.request_body.raw_retention_hours === 48 &&
        saveCall.request_body.daily_retention_days === 730,
      JSON.stringify(saveCall.request_body)
    );
    check(
      'general save 原样保留 pricing',
      stableJson(saveCall.request_body && saveCall.request_body.pricing) === stableJson(pricingBefore),
      JSON.stringify(saveCall.request_body && saveCall.request_body.pricing)
    );
    check('保存前记录了捕获开关变更', JSON.stringify(server.state.settings) !== settingsBefore);
    await waitSettingsReloaded(cdp, manualIdBefore);
    const generalReloaded = await settingsGeneralState(cdp);
    check(
      'general save 后表单回显已保存值',
      generalReloaded.capture === true &&
        generalReloaded.max_body_bytes === '2097152' &&
        generalReloaded.max_body_storage_bytes === '536870912' &&
        generalReloaded.raw_retention_hours === '48' &&
        generalReloaded.daily_retention_days === '730',
      JSON.stringify(generalReloaded)
    );

    const refreshButton = await cdp.evaluate(
      pageFn(
        VISIBLE_SNIPPET +
          'const root=document.querySelector("#content");if(!root)return null;' +
          'const buttons=[...root.querySelectorAll("button,[role=button]")].filter(visible);' +
          'const el=buttons.find((b)=>/刷新.*价格|价格.*刷新/.test(b.innerText))||' +
          'buttons.find((b)=>/刷新价格|更新价格|刷新|更新/.test(b.innerText));return el?el.innerText.trim():null;'
      )
    );
    check('设置页提供价格刷新按钮', Boolean(refreshButton), String(refreshButton));
    if (refreshButton) {
      await clickText(cdp, refreshButton, { selector: '#content button, button' });
      await acceptConfirm(cdp, ['刷新']);
      const refreshCall = await waitForCall(
        server,
        (call) => call.method === 'POST' && suffixOf(call) === '/prices/refresh',
        20000,
        'POST /prices/refresh'
      );
      check('价格刷新发送 POST /prices/refresh', refreshCall.status === 200);
    }

    const compactButton = await cdp.evaluate(
      pageFn(
        VISIBLE_SNIPPET +
          'const root=document.querySelector("#content");if(!root)return null;' +
          'const buttons=[...root.querySelectorAll("button,[role=button]")].filter(visible);' +
          'const el=buttons.find((b)=>/物理压缩|压缩|整理/.test(b.innerText));return el?el.innerText.trim():null;'
      )
    );
    check('设置页提供物理压缩按钮', Boolean(compactButton), String(compactButton));
    if (compactButton) {
      await clickText(cdp, compactButton, { selector: '#content button, button' });
      await acceptConfirm(cdp, ['压缩', '确定']);
      const compactCall = await waitForCall(
        server,
        (call) => call.method === 'POST' && suffixOf(call) === '/compact',
        20000,
        'POST /compact'
      );
      check('物理压缩发送 POST /compact', compactCall.status === 200);
    }
    return modules.join('/');
  }).then((ok) => {
    settingsReady = settingsReady || ok;
  });

  await step('设置: pricing editor 手工价/规则/严格校验/保存', async () => {
    await gotoPage(cdp, 'settings');
    await waitVisible(cdp, '[data-testid="settings-pricing-save"]', 15000, '价格编辑器');

    const initial = await pricingEditorState(cdp);
    check(
      'pricing editor 加载 seed 手工价与规则',
      initial.present && initial.prices.length === 1 && initial.rules.length === 1,
      JSON.stringify({ present: initial.present, prices: initial.prices.length, rules: initial.rules.length })
    );
    check(
      'pricing 字段通过实际 id 读取',
      initial.prices[0] && initial.prices[0].model.id.includes('-manual-model-') &&
        initial.prices[0].rates.reasoning.id.includes('-manual-1-reasoning') &&
        initial.rules[0].range.id.includes('-rule-range-'),
      JSON.stringify({
        model: initial.prices[0] && initial.prices[0].model.id,
        reasoning: initial.prices[0] && initial.prices[0].rates.reasoning.id,
        range: initial.rules[0] && initial.rules[0].range.id
      })
    );
    check(
      'seed 手工价含零值与 reasoning 空',
      initial.prices[0].model.value === 'gpt-5-codex' &&
        initial.prices[0].rates.input.value === '1.25' &&
        initial.prices[0].rates.output.value === '10' &&
        initial.prices[0].rates.cache_read.value === '0.125' &&
        initial.prices[0].rates.cache_write.value === '1.25' &&
        initial.prices[0].rates.reasoning.value === '',
      JSON.stringify(initial.prices[0])
    );
    check(
      'seed rule 使用合法 HH:mm 区间',
      initial.rules[0].id.value === 'rule-night' &&
        initial.rules[0].model.value === 'claude-opus-4-1' &&
        initial.rules[0].threshold.value === '1000' &&
        initial.rules[0].range.value === '00:00-23:59' &&
        initial.rules[0].rates.reasoning.value === '',
      JSON.stringify(initial.rules[0])
    );

    await addPricingRow(cdp, 'manual');
    await setPricingRow(cdp, 'manual', 1, {
      model: 'zero-rate-model',
      rates: { input: '0', output: '0', cache_read: '0', cache_write: '0', reasoning: '' }
    });
    await addPricingRow(cdp, 'manual');
    await setPricingRow(cdp, 'manual', 2, {
      model: 'reasoning-zero-model',
      rates: { input: '0.5', output: '1.5', cache_read: '0.1', cache_write: '0.2', reasoning: '0' }
    });
    await addPricingRow(cdp, 'rule');
    await setPricingRow(cdp, 'rule', 1, {
      id: 'rule-day',
      model: 'gpt-5-codex',
      threshold: '2000',
      range: '06:00-18:30',
      rates: { input: '2', output: '8', cache_read: '0', cache_write: '0.5', reasoning: '' }
    });
    await moveLastRuleUp(cdp);
    await clickSettingsTestId(cdp, 'settings-pricing-auto');

    const edited = await pricingEditorState(cdp);
    check(
      '手工价保留 0 值',
      edited.prices[1] && edited.prices[1].model.value === 'zero-rate-model' &&
        edited.prices[1].rates.input.value === '0' &&
        edited.prices[1].rates.output.value === '0' &&
        edited.prices[1].rates.cache_read.value === '0' &&
        edited.prices[1].rates.cache_write.value === '0',
      JSON.stringify(edited.prices[1])
    );
    check(
      'reasoning 空与 0 在表单中可区分',
      edited.prices[1].rates.reasoning.value === '' && edited.prices[2].rates.reasoning.value === '0',
      JSON.stringify({ empty: edited.prices[1].rates.reasoning, zero: edited.prices[2].rates.reasoning })
    );
    check(
      '新规则各字段已写入',
      edited.rules[0].id.value === 'rule-day' &&
        edited.rules[0].model.value === 'gpt-5-codex' &&
        edited.rules[0].threshold.value === '2000' &&
        edited.rules[0].range.value === '06:00-18:30' &&
        edited.rules[0].rates.input.value === '2' &&
        edited.rules[0].rates.output.value === '8' &&
        edited.rules[0].rates.cache_read.value === '0' &&
        edited.rules[0].rates.cache_write.value === '0.5' &&
        edited.rules[0].rates.reasoning.value === '',
      JSON.stringify(edited.rules[0])
    );
    check(
      '规则重排序后 DOM 顺序变化',
      edited.rules.map((rule) => rule.id.value).join(',') === 'rule-day,rule-night',
      JSON.stringify(edited.rules.map((rule) => rule.id.value))
    );

    await setPricingRow(cdp, 'rule', 0, { range: '00:00-24:00' });
    const putsBeforeRange = settingsPutCount(server);
    await clickSettingsTestId(cdp, 'settings-pricing-save');
    await waitPricingStatus(cdp, '/HH:mm|时间区间/', '24:00 时间区间被拒绝');
    check('非法 24:00 时间区间不发送 PUT', settingsPutCount(server) === putsBeforeRange, 'PUT 数量未变');

    await setPricingRow(cdp, 'rule', 0, { range: '06:00-18:30' });
    await setPricingRow(cdp, 'manual', 2, { rates: { input: '1.1234567' } });
    const putsBeforeDecimal = settingsPutCount(server);
    await clickSettingsTestId(cdp, 'settings-pricing-save');
    await waitPricingStatus(cdp, '/最多 6 位小数|USD 金额/', '7 位小数被拒绝');
    check('严格 decimal 拒绝时不发送 PUT', settingsPutCount(server) === putsBeforeDecimal, 'PUT 数量未变');
    await setPricingRow(cdp, 'manual', 2, { rates: { input: '0.5' } });

    const expectedPricing = {
      auto: false,
      prices: [
        {
          model: 'gpt-5-codex',
          rates: { input: 1250000, output: 10000000, cache_read: 125000, cache_write: 1250000 },
          source: 'manual'
        },
        {
          model: 'zero-rate-model',
          rates: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
          source: 'manual'
        },
        {
          model: 'reasoning-zero-model',
          rates: { input: 500000, output: 1500000, cache_read: 100000, cache_write: 200000, reasoning: 0 },
          source: 'manual'
        }
      ],
      rules: [
        {
          id: 'rule-day',
          model: 'gpt-5-codex',
          rates: { input: 2000000, output: 8000000, cache_read: 0, cache_write: 500000 },
          input_tokens_gt: 2000,
          time_range: '06:00-18:30'
        },
        {
          id: 'rule-night',
          model: 'claude-opus-4-1',
          rates: { input: 3000000, output: 15000000, cache_read: 300000, cache_write: 3000000 },
          input_tokens_gt: 1000,
          time_range: '00:00-23:59'
        }
      ]
    };
    const generalConfirmed = JSON.parse(JSON.stringify(server.state.settings));
    const idBeforePricingSave = await settingsFirstManualId(cdp);
    let startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSettingsTestId(cdp, 'settings-pricing-save');
    const pricingCall = await waitForCall(
      server,
      (call) => call.seq > startSeq && call.method === 'PUT' && suffixOf(call) === '/settings',
      20000,
      'pricing PUT /settings'
    );
    const pricingBody = pricingCall.request_body || {};
    check('pricing 保存成功', pricingCall.status === 200, String(pricingCall.status));
    check(
      'pricing 保存 payload 顶层只有 settings 字段',
      Object.keys(pricingBody).sort().join(',') ===
        'capture_enabled,daily_retention_days,max_body_bytes,max_body_storage_bytes,pricing,raw_retention_hours' &&
        !Object.prototype.hasOwnProperty.call(pricingBody, 'prices') &&
        !Object.prototype.hasOwnProperty.call(pricingBody, 'rules'),
      JSON.stringify(Object.keys(pricingBody))
    );
    check(
      'pricing payload 位于 settings.pricing',
      stableJson(pricingBody.pricing) === stableJson(expectedPricing),
      JSON.stringify(pricingBody.pricing)
    );
    check(
      'pricing save 保留 general 配置',
      ['capture_enabled', 'max_body_bytes', 'max_body_storage_bytes', 'raw_retention_hours', 'daily_retention_days'].every(
        (field) => pricingBody[field] === generalConfirmed[field]
      ),
      JSON.stringify({ payload: pricingBody, confirmed: generalConfirmed })
    );
    check(
      'pricing save 服务端持久化 0 与 reasoning 差异',
      stableJson(server.state.settings.pricing) === stableJson(expectedPricing),
      JSON.stringify(server.state.settings.pricing)
    );

    await waitSettingsReloaded(cdp, idBeforePricingSave);
    const persisted = await pricingEditorState(cdp);
    check(
      'pricing 保存后回显顺序与字段',
      persisted.auto === false &&
        persisted.prices.map((price) => price.model.value).join(',') ===
          'gpt-5-codex,zero-rate-model,reasoning-zero-model' &&
        persisted.prices[1].rates.input.value === '0' &&
        persisted.prices[1].rates.reasoning.value === '' &&
        persisted.prices[2].rates.reasoning.value === '0' &&
        persisted.rules.map((rule) => rule.id.value).join(',') === 'rule-day,rule-night' &&
        persisted.rules[0].range.value === '06:00-18:30',
      JSON.stringify(persisted)
    );

    const confirmedPricing = JSON.parse(JSON.stringify(server.state.settings.pricing));
    await clickSettingsTestId(cdp, 'settings-pricing-auto');
    const idBeforeGeneralSave = await settingsFirstManualId(cdp);
    await setGeneralSettings(cdp, {
      capture: false,
      max_body_bytes: 3145728,
      max_body_storage_bytes: 1073741824,
      raw_retention_hours: 72,
      daily_retention_days: 900
    });
    startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSettingsTestId(cdp, 'settings-general-save');
    const generalCall = await waitForCall(
      server,
      (call) => call.seq > startSeq && call.method === 'PUT' && suffixOf(call) === '/settings',
      20000,
      'general PUT /settings'
    );
    const generalBody = generalCall.request_body || {};
    check(
      'general save 保留已保存 pricing 而非未保存草稿',
      generalCall.status === 200 &&
        stableJson(generalBody.pricing) === stableJson(confirmedPricing) &&
        generalBody.pricing.auto === false,
      JSON.stringify(generalBody.pricing)
    );
    check(
      'general save 使用新 general 字段',
      generalBody.capture_enabled === false &&
        generalBody.max_body_bytes === 3145728 &&
        generalBody.max_body_storage_bytes === 1073741824 &&
        generalBody.raw_retention_hours === 72 &&
        generalBody.daily_retention_days === 900,
      JSON.stringify(generalBody)
    );
    await waitSettingsReloaded(cdp, idBeforeGeneralSave);
    const finalState = await settingsGeneralState(cdp);
    const finalPricing = await pricingEditorState(cdp);
    check(
      'general save 后 pricing 仍为已保存值',
      finalPricing.auto === false &&
        stableJson(finalPricing.prices.map((price) => price.model.value)) === stableJson(['gpt-5-codex', 'zero-rate-model', 'reasoning-zero-model']) &&
        finalPricing.rules.map((rule) => rule.id.value).join(',') === 'rule-day,rule-night',
      JSON.stringify(finalPricing)
    );
    check(
      'general save 后 general 回显',
      finalState.capture === false &&
        finalState.max_body_bytes === '3145728' &&
        finalState.max_body_storage_bytes === '1073741824' &&
        finalState.raw_retention_hours === '72' &&
        finalState.daily_retention_days === '900',
      JSON.stringify(finalState)
    );
    return 'pricing-editor';
  });

  await step('轮询暂停: 模态/编辑脏状态/隐藏标签页', async () => {
    await gotoPage(cdp, 'requests');
    await waitRequestsLoaded(cdp);
    await selectValue(cdp, '#interval', '5');
    await resetTimerProbe(cdp);
    let startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > startSeq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      '暂停测试的首次刷新'
    );
    await waitForLongTimer(cdp, 6000);
    check('间隔 5 秒会调度长定时器', true);

    await resetTimerProbe(cdp);
    await findModelButton(cdp, mock.XSS.model, true);
    const dialog = await waitDialog(cdp);
    let stats = await timerStats(cdp);
    check(
      '打开详情对话框暂停轮询',
      dialog.present && stats.clears >= 1 && stats.longSet === 0,
      JSON.stringify(stats)
    );
    await closeDialog(cdp);
    await waitForLongTimer(cdp, 6000);
    check('关闭对话框恢复轮询', true);

    await resetTimerProbe(cdp);
    await setDocumentHidden(cdp, true);
    stats = await timerStats(cdp);
    check(
      '隐藏标签页暂停轮询',
      stats.clears >= 1 && stats.longSet === 0,
      JSON.stringify(stats)
    );
    await resetTimerProbe(cdp);
    await setDocumentHidden(cdp, false);
    await waitForLongTimer(cdp, 6000);
    check('恢复可见后恢复轮询', true);

    await resetTimerProbe(cdp);
    await setValue(cdp, '#filters [name=model]', 'gpt-5-codex');
    stats = await timerStats(cdp);
    check(
      '编辑筛选（脏状态）清除轮询定时器',
      stats.clears >= 1 && stats.longSet === 0,
      JSON.stringify(stats)
    );
    await resetTimerProbe(cdp);
    await setDocumentHidden(cdp, true);
    await setDocumentHidden(cdp, false);
    stats = await timerStats(cdp);
    check('脏状态恢复可见也不调度轮询', stats.longSet === 0, JSON.stringify(stats));
    await restoreDocumentHidden(cdp);

    await setValue(cdp, '#filters [name=model]', '');
    await selectValue(cdp, '#interval', '0');
    await resetTimerProbe(cdp);
    startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > startSeq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      '暂停测试后的刷新'
    );
    stats = await timerStats(cdp);
    check('暂停测试后回到无轮询状态', stats.longSet === 0, JSON.stringify(stats));
    return 'modal+hidden+dirty';
  });

  await step('重叠刷新: AbortController 中止与 generation 丢弃陈旧响应', async () => {
    await gotoPage(cdp, 'requests');
    await waitRequestsLoaded(cdp);
    await selectValue(cdp, '#interval', '0');

    const runScenario = async (honorAbort, label) => {
      await installFetchGate(cdp, {
        match: '/requests?',
        query: 'model=gemini-2.5-pro',
        delay: 1500,
        honorAbort
      });
      await setValue(cdp, '#filters [name=model]', 'gemini-2.5-pro');
      let seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      await clickSelector(cdp, '#refresh');
      await waitForCall(
        server,
        (call) =>
          call.seq > seq &&
          call.method === 'GET' &&
          suffixOf(call) === '/requests' &&
          call.query.model === 'gemini-2.5-pro',
        15000,
        label + ': 旧请求已发出'
      );

      await setValue(cdp, '#filters [name=model]', 'claude-opus-4-1');
      seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      await clickSelector(cdp, '#refresh');
      await waitForCall(
        server,
        (call) =>
          call.seq > seq &&
          call.method === 'GET' &&
          suffixOf(call) === '/requests' &&
          call.query.model === 'claude-opus-4-1',
        15000,
        label + ': 新请求已发出'
      );
      await cdp.waitFor(
        '(() => {const b=document.querySelector("#content tbody");return !!b && b.innerText.includes("claude-opus-4-1");})()',
        10000,
        label + ': 新结果渲染'
      );
      await cdp.evaluate(pageFn('return window.__staleDone;'), 8000);
      await settle(cdp);

      const bodyText = await text(cdp, '#content tbody');
      const stats = await cdp.evaluate(pageFn('return window.__fetchLog||{matched:0,aborts:0,released:0};'));
      check(label + ': 新旧请求确实重叠', stats.matched >= 1 && stats.released >= 1, JSON.stringify(stats));
      check(
        label + ': 旧结果未覆盖新结果',
        !bodyText.includes('gemini-2.5-pro') && bodyText.includes('claude-opus-4-1'),
        bodyText.slice(0, 240)
      );
      if (honorAbort) {
        check('停止轮询会 Abort 在途请求', stats.aborts >= 1, JSON.stringify(stats));
      } else {
        check('忽略 Abort 时 generation 仍丢弃陈旧响应', stats.aborts === 0, JSON.stringify(stats));
      }

      await restoreFetch(cdp);
      await setValue(cdp, '#filters [name=model]', '');
      seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
      await clickSelector(cdp, '#refresh');
      await waitForCall(
        server,
        (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
        15000,
        label + ': 清理筛选'
      );
    };

    await runScenario(true, 'abort');
    await runScenario(false, 'generation');
    return 'abort+generation';
  });

  await step('刷新 401: 停止轮询、清 session、回登录，再登录', async () => {
    await gotoPage(cdp, 'requests');
    await waitRequestsLoaded(cdp);
    await selectValue(cdp, '#interval', '5');
    await resetTimerProbe(cdp);
    let seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      '401 测试前刷新'
    );
    await waitForLongTimer(cdp, 6000);

    await installFetch401(cdp, '/requests?');
    await resetTimerProbe(cdp);
    const errorStart = consoleErrors.length;
    await clickSelector(cdp, '#refresh');
    await waitVisible(cdp, '#login', 10000, '401 后返回登录界面');

    const state = await cdp.evaluate(
      pageFn(
        VISIBLE_SNIPPET +
          'const nav=document.querySelector("nav");const content=document.querySelector("#content");' +
          'const tools=document.querySelector("#tools");const msg=document.querySelector("#message");' +
          'return {session:sessionStorage.getItem("observer.management-key"),' +
          'navHidden:!!nav&&nav.hidden,contentEmpty:!!content&&content.children.length===0,' +
          'toolsEmpty:!!tools&&tools.children.length===0,message:msg?msg.innerText:""};'
      )
    );
    const stats = await timerStats(cdp);
    const fault = await cdp.evaluate(pageFn('return window.__faultLog||{hits:0};'));
    check('认证刷新 401 清除 sessionStorage', state.session === null, String(state.session));
    check('认证刷新 401 隐藏导航并清空内容', state.navHidden && state.contentEmpty && state.toolsEmpty, JSON.stringify(state));
    check('认证刷新 401 不附加历史查询提示', !/历史|partial|边界/i.test(state.message), state.message);
    check('认证刷新 401 后不再调度轮询', stats.longSet === 0, JSON.stringify(stats));
    check('401 确实命中管理请求', fault.hits >= 1, JSON.stringify(fault));
    absorbExpectedAuthDiagnostics(errorStart);

    await restoreFetch(cdp);
    await login(cdp, mock.DEFAULT_MANAGEMENT_KEY);
    await cdp.waitFor(
      '(() => {' + VISIBLE_SNIPPET + 'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
        'return !visible(l)&&visible(c);})()',
      20000,
      '401 后重新登录'
    );
    await waitRequestsLoaded(cdp);
    await clearRequestFilters(cdp);
    seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      '401 后重置筛选'
    );
    await selectValue(cdp, '#interval', '0');
    check('401 后重新登录成功', true);
    return 'logout→login';
  });

  await step('在途刷新: 输入筛选后迟到响应不覆盖草稿', async () => {
    await gotoPage(cdp, 'requests');
    await waitRequestsLoaded(cdp);
    await selectValue(cdp, '#interval', '0');

    await installFetchGate(cdp, {
      match: '/requests?',
      query: 'model=gpt-5-codex',
      delay: 1200,
      honorAbort: false,
      trackAbort: true,
      detachSignal: true
    });
    await setValue(cdp, '#filters [name=model]', 'gpt-5-codex');
    let seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) =>
        call.seq > seq &&
        call.method === 'GET' &&
        suffixOf(call) === '/requests' &&
        call.query.model === 'gpt-5-codex',
      15000,
      '在途刷新已发出'
    );

    const draft = await cdp.evaluate(
      pageFn(
        `
        const el=document.querySelector('#filters [name=model]');
        if(!el)return null;
        el.focus();
        el.value='draft-filter';
        el.dispatchEvent(new Event('input',{bubbles:true}));
        return {value:el.value,active:document.activeElement===el};
        `
      )
    );
    check(
      '在途刷新期间 focus/input 触发停止',
      draft && draft.value === 'draft-filter' && draft.active === true,
      JSON.stringify(draft)
    );
    await cdp.waitFor(
      'window.__fetchLog && window.__fetchLog.aborts>=1',
      5000,
      'focus/input 触发 Abort'
    );
    await cdp.evaluate(pageFn('return window.__staleDone;'), 8000);
    await settle(cdp);

    const after = await cdp.evaluate(
      pageFn(
        `
        const el=document.querySelector('#filters [name=model]');
        return {value:el?el.value:null,active:document.activeElement===el,
          text:document.querySelector('#content')?document.querySelector('#content').innerText.slice(0,120):''};
        `
      )
    );
    const stats = await cdp.evaluate(pageFn('return window.__fetchLog||{matched:0,aborts:0,released:0};'));
    check(
      '迟到响应释放后筛选草稿不被覆盖',
      after.value === 'draft-filter' && after.active === true,
      JSON.stringify(after)
    );
    check(
      '在途刷新确实被 abort 且迟到响应已释放',
      stats.matched >= 1 && stats.aborts >= 1 && stats.released >= 1,
      JSON.stringify(stats)
    );

    await restoreFetch(cdp);
    await clearRequestFilters(cdp);
    seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      '在途刷新测试后恢复'
    );
    return 'draft-preserved';
  });

  await step('同源 iframe: 继承 session 与 auto 跟随父主题', async () => {
    await selectValue(cdp, '#theme', 'dark');
    await cdp.waitFor('document.documentElement.dataset.theme==="dark"', 5000, '父页面 dark');
    const inserted = await cdp.evaluate(
      pageFn(
        `
        const old=document.getElementById('observer-iframe-probe');
        if(old)old.remove();
        const f=document.createElement('iframe');
        f.id='observer-iframe-probe';
        f.title='Observer iframe probe';
        f.src=arguments[0];
        document.body.appendChild(f);
        return f.src;
        `,
        server.resourceURL
      )
    );
    check('插入同源资源 iframe', inserted === server.resourceURL, String(inserted));

    await cdp.waitFor(
      '(() => {const f=document.getElementById("observer-iframe-probe");' +
        'const d=f&&f.contentDocument;return !!d&&d.readyState==="complete"&&!!d.querySelector("#filters");})()',
      20000,
      'iframe 直接进入请求日志'
    );
    const initial = await cdp.evaluate(
      pageFn(
        `
        const f=document.getElementById('observer-iframe-probe');
        const d=f.contentDocument;const w=f.contentWindow;
        const theme=d.querySelector('#theme');
        return {session:w.sessionStorage.getItem('observer.management-key'),
          loginHidden:!!d.querySelector('#login')&&d.querySelector('#login').hidden,
          content:!!d.querySelector('#content')&&d.querySelector('#content').innerText.trim().length>0,
          auto:theme?theme.value:null,theme:d.documentElement.dataset.theme,
          parentTheme:document.documentElement.dataset.theme};
        `
      )
    );
    check(
      'iframe 继承本标签 sessionStorage 且无需再次登录',
      initial.session === mock.DEFAULT_MANAGEMENT_KEY && initial.loginHidden && initial.content,
      JSON.stringify(initial)
    );
    check(
      'iframe auto 初始跟随父页面 dark',
      initial.auto === 'auto' && initial.theme === 'dark' && initial.parentTheme === 'dark',
      JSON.stringify(initial)
    );

    await selectValue(cdp, '#theme', 'white');
    await cdp.waitFor('document.documentElement.dataset.theme==="white"', 5000, '父页面 white');
    await cdp.waitFor(
      '(() => {const f=document.getElementById("observer-iframe-probe");' +
        'const d=f&&f.contentDocument;return !!d&&d.documentElement.dataset.theme==="white";})()',
      5000,
      'iframe 跟随 white'
    );
    check('父页面切 white 后 iframe 自动跟随', true);

    await selectValue(cdp, '#theme', 'dark');
    await cdp.waitFor('document.documentElement.dataset.theme==="dark"', 5000, '父页面再次 dark');
    await cdp.waitFor(
      '(() => {const f=document.getElementById("observer-iframe-probe");' +
        'const d=f&&f.contentDocument;return !!d&&d.documentElement.dataset.theme==="dark";})()',
      5000,
      'iframe 跟随 dark'
    );
    check('父页面切回 dark 后 iframe 自动跟随', true);

    const removed = await cdp.evaluate(
      pageFn(
        `
        const f=document.getElementById('observer-iframe-probe');
        if(f)f.remove();
        return !document.getElementById('observer-iframe-probe');
        `
      )
    );
    check('移除探针 iframe', removed === true);
    let seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickSelector(cdp, '#refresh');
    await waitForCall(
      server,
      (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
      15000,
      'iframe 移除后主页面刷新'
    );
    check('iframe 移除后主页面继续可用', true);
    return 'iframe-session+theme';
  });

  await step('路由 smoke: /ui/ 与 management /ui 入口', async () => {
    const resourceSlashURL = server.resourceURL + '/';
    let since = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await cdp.send('Page.navigate', { url: resourceSlashURL });
    await waitUiRestored(cdp);
    const slashState = await cdp.evaluate(
      pageFn(
        `
        return {path:location.pathname,key:sessionStorage.getItem('observer.management-key'),
          loginHidden:!!document.querySelector('#login')&&document.querySelector('#login').hidden,
          filters:!!document.querySelector('#filters')};
        `
      )
    );
    check(
      '/ui/ 入口恢复 session 并直接显示请求日志',
      slashState.path === new URL(resourceSlashURL).pathname &&
        slashState.key === mock.DEFAULT_MANAGEMENT_KEY &&
        slashState.loginHidden === true &&
        slashState.filters === true,
      JSON.stringify(slashState)
    );
    let audit = fixedAssetAudit(server, since);
    check(
      '/ui/ 资产使用固定资源路径且无 404',
      audit.ok,
      JSON.stringify(audit)
    );
    check('/ui/ 无 ui/ui.js 或 ui/ui.css 相对请求', audit.relative.length === 0, JSON.stringify(audit.relative));
    await selectValue(cdp, '#interval', '0');

    const managementUI = server.managementURL + '/ui';
    since = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await cdp.send('Page.navigate', { url: managementUI });
    await waitUiRestored(cdp);
    const managementState = await cdp.evaluate(
      pageFn(
        `
        return {path:location.pathname,key:sessionStorage.getItem('observer.management-key'),
          loginHidden:!!document.querySelector('#login')&&document.querySelector('#login').hidden,
          filters:!!document.querySelector('#filters')};
        `
      )
    );
    check(
      'management /ui 入口恢复 session 并显示请求日志',
      managementState.path === new URL(managementUI).pathname &&
        managementState.key === mock.DEFAULT_MANAGEMENT_KEY &&
        managementState.loginHidden === true &&
        managementState.filters === true,
      JSON.stringify(managementState)
    );
    audit = fixedAssetAudit(server, since);
    check(
      'management /ui 资产仍走固定资源路径且无 404',
      audit.ok,
      JSON.stringify(audit)
    );
    await selectValue(cdp, '#interval', '0');

    since = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await cdp.send('Page.navigate', { url: server.resourceURL });
    await waitUiRestored(cdp);
    const resourceState = await cdp.evaluate(
      pageFn(
        `
        return {path:location.pathname,key:sessionStorage.getItem('observer.management-key'),
          loginHidden:!!document.querySelector('#login')&&document.querySelector('#login').hidden,
          filters:!!document.querySelector('#filters')};
        `
      )
    );
    check(
      '切回资源 route 后恢复 session',
      resourceState.path === new URL(server.resourceURL).pathname &&
        resourceState.key === mock.DEFAULT_MANAGEMENT_KEY &&
        resourceState.loginHidden === true &&
        resourceState.filters === true,
      JSON.stringify(resourceState)
    );
    audit = fixedAssetAudit(server, since);
    check('资源 route 资产仍无 404', audit.ok, JSON.stringify(audit));
    await selectValue(cdp, '#interval', '0');
    return 'ui/ + management/ui';
  });

  if (!requestsReady || !keysReady || !settingsReady) {
    check('三页均可访问', false, JSON.stringify({ requestsReady, keysReady, settingsReady }));
  } else {
    check('三页均可访问', true);
  }

  return { requestsReady, keysReady, settingsReady };
}

/* -------------------------------------------------------------------------- */
/* Screenshot matrix                                                          */
/* -------------------------------------------------------------------------- */

async function runScreenshotMatrix(server, cdp) {
  const shotsDir = path.join(reportDir, 'screenshots');
  let expected = 0;
  for (const size of SIZES) {
    await setViewport(cdp, size);
    const viewportWidth = await cdp.evaluate(pageFn('return window.innerWidth;'));
    check('截图视口宽度为 ' + size.width, viewportWidth === size.width, 'innerWidth=' + viewportWidth);
    for (const theme of THEMES) {
      try {
        await applyTheme(cdp, theme);
      } catch (err) {
        check('主题切换 ' + size.width + '/' + theme, false, err.message);
        continue;
      }

      for (const page of PAGES) {
        expected += 1;
        await step('截图 ' + size.width + '/' + theme + '/' + page.key, async () => {
          await gotoPage(cdp, page.key);
          if (page.key === 'requests') await waitRequestsLoaded(cdp);
          await settle(cdp);
          const overflow = await overflowAudit(cdp);
          check(
            '无横向溢出 ' + size.width + '/' + theme + '/' + page.key,
            overflow.overflow <= 1,
            JSON.stringify(overflow)
          );
          const bytes = await capture(
            cdp,
            path.join(shotsDir, size.width + '-' + theme + '-' + page.key + '.png'),
            true
          );
          return bytes + ' bytes';
        });
      }

      expected += 1;
      await step('截图 ' + size.width + '/' + theme + '/key-form', async () => {
        await gotoPage(cdp, 'keys');
        await clickText(cdp, '新建 Key', { selector: '#content button, button' });
        const dialog = await waitDialog(cdp);
        await settle(cdp);
        const overflow = await overflowAudit(cdp);
        check(
          'Key 表单无横向溢出 ' + size.width + '/' + theme,
          overflow.overflow <= 1,
          JSON.stringify(overflow)
        );
        check(
          'Key 表单在视口内 ' + size.width + '/' + theme,
          dialog.present && dialog.rect.left >= -1 && dialog.rect.right <= dialog.vw + 1,
          JSON.stringify(dialog.present ? dialog.rect : dialog)
        );
        const bytes = await capture(
          cdp,
          path.join(shotsDir, size.width + '-' + theme + '-key-form.png'),
          false
        );
        await closeDialog(cdp);
        return bytes + ' bytes';
      });

      expected += 1;
      await step('截图 ' + size.width + '/' + theme + '/request-detail', async () => {
        await gotoPage(cdp, 'requests');
        await waitRequestsLoaded(cdp);
        const clicked = await findModelButton(cdp, 'gpt-5-codex', true);
        if (!clicked || !clicked.ok) throw new Error('找不到可点击的模型按钮');
        const dialog = await waitDialog(cdp);
        await settle(cdp);
        const overflow = await overflowAudit(cdp);
        check(
          '详情无横向溢出 ' + size.width + '/' + theme,
          overflow.overflow <= 1,
          JSON.stringify(overflow)
        );
        check(
          '详情在视口内 ' + size.width + '/' + theme,
          dialog.present && dialog.rect.left >= -1 && dialog.rect.right <= dialog.vw + 1,
          JSON.stringify(dialog.present ? dialog.rect : dialog)
        );
        const bytes = await capture(
          cdp,
          path.join(shotsDir, size.width + '-' + theme + '-request-detail.png'),
          false
        );
        await closeDialog(cdp);
        return bytes + ' bytes';
      });
    }
  }
  await cdp.send('Emulation.clearDeviceMetricsOverride');
  await cdp.send('Emulation.setEmulatedMedia', { features: [] });
  check('截图矩阵数量为 ' + expected, screenshots.length === expected, 'screenshots=' + screenshots.length + ', expected=' + expected);
  return expected;
}

/* -------------------------------------------------------------------------- */
/* Main                                                                       */
/* -------------------------------------------------------------------------- */

async function main() {
  const args = parseArgs(process.argv.slice(2));
  reportDir = args.reportDir;
  chromePath = args.chrome;
  keepProfile = args.keepProfile;
  fs.mkdirSync(reportDir, { recursive: true });
  // 只清理本脚本拥有的截图目录，避免上一轮失败留下的旧矩阵冒充本轮结果。
  fs.rmSync(path.join(reportDir, 'screenshots'), { recursive: true, force: true });
  fs.mkdirSync(path.join(reportDir, 'screenshots'), { recursive: true });

  const server = await mock.startServer();
  let chrome = null;
  let profileDir = null;
  const started = Date.now();
  let fatal = null;

  try {
    await runApiContract(server);
    server.resetState();
    server.resetCalls();

    profileDir = fs.mkdtempSync(path.join(os.tmpdir(), 'observer-browser-'));
    const launched = await launchChrome(chromePath, profileDir, 1280, 900);
    const cdp = await connectWebSocket(launched.wsUrl);
    chrome = { child: launched.child, cdp, stderr: launched.stderr };

    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Log.enable');
    await cdp.send('Network.enable');

    cdp.on('Runtime.exceptionThrown', (params) => {
      const description =
        (params.exceptionDetails && params.exceptionDetails.exception && params.exceptionDetails.exception.description) ||
        (params.exceptionDetails && params.exceptionDetails.text) ||
        'uncaught exception';
      consoleErrors.push({ type: 'exception', text: description });
    });
    cdp.on('Runtime.consoleAPICalled', (params) => {
      if (params.type !== 'error' && params.type !== 'assert') return;
      const message = (params.args || [])
        .map((arg) => (arg.value !== undefined ? String(arg.value) : arg.description || arg.type || ''))
        .join(' ');
      consoleErrors.push({ type: 'console', text: message });
    });
    cdp.on('Log.entryAdded', (params) => {
      const entry = params.entry || {};
      if (entry.level !== 'error') return;
      consoleErrors.push({
        type: 'log',
        source: entry.source || '',
        text: entry.text || '',
        url: entry.url || '',
        line: entry.lineNumber,
        stack: entry.stackTrace || null
      });
    });
    cdp.on('Page.javascriptDialogOpening', (params) => {
      // UI 用原生 confirm 做破坏性操作确认；验收接受确认并继续断言随后 API
      // 调用，不把预期确认当成 console 错误。alert 同样接受；prompt 使用空值。
      const dialog = { type: params.type || '', message: params.message || '' };
      nativeDialogs.push(dialog);
      const reply = { accept: true };
      if (dialog.type === 'prompt') reply.promptText = '';
      cdp.send('Page.handleJavaScriptDialog', reply).catch(() => {});
    });

    await setViewport(cdp, SIZES[0]);
    const pages = await runBrowserSuite(server, cdp);

    // 401 用例会主动退出登录；若该步骤中途失败，矩阵前恢复登录。
    try {
      await restoreFetch(cdp);
      await restoreDocumentHidden(cdp);
      if (await visible(cdp, '#login')) {
        await login(cdp, mock.DEFAULT_MANAGEMENT_KEY);
        await cdp.waitFor(
          '(() => {' + VISIBLE_SNIPPET + 'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
            'return !visible(l)&&visible(c);})()',
          20000,
          '矩阵前恢复登录'
        );
        await waitRequestsLoaded(cdp);
        await clearRequestFilters(cdp);
        const seq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
        await clickSelector(cdp, '#refresh');
        await waitForCall(
          server,
          (call) => call.seq > seq && call.method === 'GET' && suffixOf(call) === '/requests',
          15000,
          '矩阵前重置筛选'
        );
        await selectValue(cdp, '#interval', '0');
        check('矩阵前恢复登录', true);
      }
    } catch (err) {
      check('矩阵前恢复登录', false, (err && err.message) || String(err));
    }

    // 正常流程的 console/page 错误必须为零；负向 401 诊断已单独保存。
    check('正常流程无 console/page 错误', consoleErrors.length === 0, JSON.stringify(consoleErrors.slice(0, 8)));

    const calls = managementCalls(server);
    const patterns = new Set(calls.map(callPattern));
    const expectedPatterns = [
      'GET /requests',
      'GET /requests/{id}',
      'GET /summary',
      'GET /keys',
      'POST /keys',
      'PUT /keys/{id}',
      'POST /keys/{id}/rotate',
      'POST /keys/{id}/reset',
      'DELETE /keys/{id}',
      'GET /settings',
      'PUT /settings',
      'POST /prices/refresh',
      'POST /compact'
    ];
    const missing = expectedPatterns.filter((pattern) => !patterns.has(pattern));
    check('UI 调用了预期管理 API', missing.length === 0, 'missing=' + JSON.stringify(missing) + '; seen=' + JSON.stringify([...patterns].sort()));

    const unauthorized = calls.filter((call) => call.status !== 401 && !call.authorized);
    check('除刻意 401 外所有管理请求均带控制台密钥鉴权', unauthorized.length === 0, JSON.stringify(unauthorized.map(callPattern)));
    check(
      '管理密钥不出现在任何 URL',
      server.calls.every((call) => !call.path.includes(mock.DEFAULT_MANAGEMENT_KEY) && !JSON.stringify(call.query).includes(mock.DEFAULT_MANAGEMENT_KEY))
    );
    check(
      '假生产密钥未泄漏到页面',
      !(await cdp.evaluate(pageFn('return document.body.innerText.includes(arguments[0]);', mock.XSS.leakedSecret)))
    );

    if (pages.requestsReady && pages.keysReady && pages.settingsReady) {
      await runScreenshotMatrix(server, cdp);
    } else {
      check('截图矩阵', false, '三页未全部就绪，跳过矩阵');
    }

    check('截图文件全部非空', screenshots.length > 0 && screenshots.every((shot) => shot.bytes > 1000), JSON.stringify(screenshots.slice(0, 5)));
    check('最终无 console/page 错误', consoleErrors.length === 0, JSON.stringify(consoleErrors.slice(0, 8)));
  } catch (err) {
    fatal = err;
    check('浏览器验收执行完成', false, err && err.stack ? err.stack.split('\n').slice(0, 4).join(' | ') : String(err));
  } finally {
    if (chrome) await stopChrome({ child: chrome.child, cdp: chrome.cdp });
    if (profileDir && !keepProfile) {
      try {
        fs.rmSync(profileDir, { recursive: true, force: true });
      } catch (err) {
        /* ignore */
      }
    }
    await server.close();
  }

  const durationMs = Date.now() - started;
  const report = {
    generated_at: new Date().toISOString(),
    outcome: failed === 0 ? 'completed' : 'failed',
    duration_ms: durationMs,
    chrome: chromeInfo,
    chrome_path: chromePath,
    report_dir: reportDir,
    resource_url: server.resourceURL,
    summary: {
      total: results.length,
      passed: results.length - failed,
      failed,
      screenshots: screenshots.length
    },
    results,
    screenshots,
    console_errors: consoleErrors,
    expected_diagnostics: expectedDiagnostics,
    native_dialogs: nativeDialogs,
    api_calls: server.calls.map((call) => ({
      seq: call.seq,
      method: call.method,
      path: call.path,
      query: call.query,
      kind: call.kind,
      authorized: call.authorized,
      status: call.status
    })),
    fatal: fatal ? String(fatal.message || fatal) : null
  };
  fs.writeFileSync(path.join(reportDir, 'report.json'), JSON.stringify(report, null, 2));
  const summary = [
    'Observer browser acceptance',
    'outcome: ' + report.outcome,
    'passed: ' + report.summary.passed + '/' + report.summary.total,
    'screenshots: ' + report.summary.screenshots,
    'duration_ms: ' + durationMs,
    'report: ' + path.join(reportDir, 'report.json'),
    fatal ? 'fatal: ' + report.fatal : '',
    ''
  ].join('\n');
  fs.writeFileSync(path.join(reportDir, 'summary.txt'), summary);
  process.stdout.write('\n' + summary);

  if (failed > 0) process.exitCode = 1;
}

if (require.main === module) {
  main().catch((err) => {
    process.stderr.write('FATAL: ' + (err && err.stack ? err.stack : String(err)) + '\n');
    process.exitCode = 2;
  });
}

module.exports = { main };
