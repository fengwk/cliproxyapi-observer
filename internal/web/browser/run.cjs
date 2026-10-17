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
 *   --base-url URL 真实 host 只读 smoke；管理密钥仅从 OBSERVER_MANAGEMENT_KEY 读取
 *
 * realhost 只允许 GET/HEAD/OPTIONS 和唯一精确的插件
 * POST /models/token（同源、空 query、空 body）；其他写请求在发送前阻断。
 *
 * 覆盖：
 *   - DESIGN.md 管理 API 的完整 mock 合约（鉴权、输入拒绝、CRUD、筛选/分页/汇总）；
 *   - 主 UI：登录门禁、请求日志/Key 管理/设置三页、主题与刷新间隔；
 *   - #filters 查询参数、详情只在点击日志行后读取、Key 全动作、设置保存/价格刷新/压缩；
 *   - 可访问性（可见控件名称、对话框名称、表头）、无 HTML 注入、无 console/page 错误；
 *   - 1280/390 × white/light/dark/auto × 三页 + Key 表单 + 请求详情截图矩阵；
 *   - 每个视口/主题/页面无横向溢出。
 *
 * 默认只使用 127.0.0.1 上的假数据；--base-url 模式只对真实 host 做只读 smoke，
 * 不读取用户 profile，不写 Key/设置，也不访问上游。
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
  const args = {
    reportDir: DEFAULT_REPORT_DIR,
    chrome: process.env.CHROME_BIN || DEFAULT_CHROME,
    keepProfile: false,
    baseUrl: '',
    managementKey: process.env.OBSERVER_MANAGEMENT_KEY || ''
  };
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
    } else if (arg === '--base-url') {
      args.baseUrl = argv[++i] || '';
    } else if (arg.startsWith('--base-url=')) {
      args.baseUrl = arg.slice('--base-url='.length);
    } else if (arg === '--management-key' || arg.startsWith('--management-key=')) {
      throw new Error('--management-key 已移除；请通过 OBSERVER_MANAGEMENT_KEY 环境变量提供管理密钥');
    } else if (arg === '--keep-profile') {
      args.keepProfile = true;
    } else if (arg === '--help' || arg === '-h') {
      process.stdout.write(
        'Usage: node internal/web/browser/run.cjs [--report-dir ABS] [--chrome PATH] [--keep-profile]\n' +
          '       OBSERVER_MANAGEMENT_KEY=... node internal/web/browser/run.cjs --base-url URL\n' +
          'Default report dir: ' +
          DEFAULT_REPORT_DIR +
          '\n' +
          '--base-url runs a read-only smoke against a real host.\n' +
          'Management key is read only from OBSERVER_MANAGEMENT_KEY; CLI key flags are not accepted.\n' +
          'CDP Fetch interception allows only GET/HEAD/OPTIONS plus the exact empty-body/query POST\n' +
          '  /v0/management/plugins/cliproxyapi-observer/models/token; all other writes are blocked.\n'
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

async function keyDialogScrollAudit(cdp) {
  return cdp.evaluate(
    pageFn(
      VISIBLE_SNIPPET +
        `
        const dialogs=[...document.querySelectorAll("${DIALOG_ROOT}")].filter(visible);
        const dialog=dialogs[dialogs.length-1];
        if(!dialog)return {present:false,reason:'missing-dialog'};
        const body=dialog.querySelector('.dialog-body')||dialog;
        const buttons=[...dialog.querySelectorAll('button')].filter(function(button){
          return visible(button)&&/^(保存|取消)$/.test((button.innerText||'').trim());
        });
        const rect=function(el){
          const r=el.getBoundingClientRect();
          return {left:r.left,right:r.right,top:r.top,bottom:r.bottom,width:r.width,height:r.height};
        };
        const dialogRect=rect(dialog);
        const inside=function(r){
          return r.width>0&&r.height>0&&
            r.left>=dialogRect.left-1&&r.right<=dialogRect.right+1&&
            r.top>=dialogRect.top-1&&r.bottom<=dialogRect.bottom+1;
        };
        const snapshot=function(){
          return buttons.map(function(button){
            const r=rect(button);
            return {text:(button.innerText||'').trim(),rect:r,inside:inside(r)};
          });
        };
        body.scrollTop=0;
        const top=snapshot();
        body.scrollTop=body.scrollHeight;
        const bottom=snapshot();
        body.scrollTop=0;
        return {
          present:true,
          dialog:dialogRect,
          body:{clientHeight:body.clientHeight,scrollHeight:body.scrollHeight,scrollTop:body.scrollTop},
          top:top,
          bottom:bottom
        };
        `
    )
  );
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

/* -------------------------------------------------------------------------- */
/* Key model picker (models.js contract)                                      */
/* -------------------------------------------------------------------------- */

const MODEL_ROOT_SELECTOR = '.models-root';
// 不带引号的属性值可直接嵌入双引号包裹的 JS 选择器字符串。
const DIALOG_ROOT = 'dialog, [role=dialog], [aria-modal=true]';

function modelRootExpression(body, ...args) {
  return pageFn(
    VISIBLE_SNIPPET +
      'const dialogs=[...document.querySelectorAll("' + DIALOG_ROOT + '")].filter(visible);' +
      'const host=dialogs.length?dialogs[dialogs.length-1]:document;' +
      'const root=host.querySelector("' + MODEL_ROOT_SELECTOR + '");' +
      (body || 'return root;'),
    ...args
  );
}

async function modelPickerPresent(cdp) {
  return cdp.evaluate(modelRootExpression('return !!root;'));
}

async function waitModelDirectory(cdp, timeout = 15000) {
  await cdp.waitFor(
    '(() => {' + VISIBLE_SNIPPET +
      'const dialogs=[...document.querySelectorAll("' + DIALOG_ROOT + '")].filter(visible);' +
      'const host=dialogs.length?dialogs[dialogs.length-1]:document;' +
      'const root=host.querySelector("' + MODEL_ROOT_SELECTOR + '");' +
      'return !!root && root.getAttribute("aria-busy")==="false";})()',
    timeout,
    '模型目录加载完成'
  );
}

async function modelPickerState(cdp) {
  return cdp.evaluate(
    modelRootExpression(`
      if(!root)return {present:false};
      const one=(selector)=>root.querySelector(selector);
      const items=[...root.querySelectorAll('.models-item')].map(function(item){
        const box=item.querySelector('input[type="checkbox"]');
        const text=(item.innerText||'').replace(/\\s+/g,' ').trim();
        const id=text.replace(/（目录未列出）\\s*$/,'').trim();
        return {id:id,checked:!!box&&box.checked,missing:/目录未列出/.test(text)};
      });
      const warning=one('.models-warning');
      return {
        present:true,
        busy:root.getAttribute('aria-busy'),
        all:!!one('input[value="all"]')&&one('input[value="all"]').checked,
        restricted:!!one('input[value="restricted"]')&&one('input[value="restricted"]').checked,
        panelHidden:!!one('.models-restricted')&&one('.models-restricted').hidden,
        search:one('input[type="search"]')?one('input[type="search"]').value:null,
        status:(one('.models-status')&&one('.models-status').innerText)||'',
        warning:warning?warning.innerText:'',
        warningVisible:!!warning&&!warning.hidden,
        items:items,
        text:(root.innerText||'').slice(0,1200)
      };
    `)
  );
}

async function setModelMode(cdp, mode) {
  const result = await cdp.evaluate(
    modelRootExpression(
      `
      if(!root)return {ok:false,reason:'missing'};
      const radio=root.querySelector('input[type="radio"][value="'+arguments[0]+'"]');
      if(!radio)return {ok:false,reason:'missing-radio'};
      if(!radio.checked)radio.click();
      return {ok:true,value:radio.value,checked:radio.checked};
      `,
      mode
    )
  );
  if (!result || !result.ok) throw new Error('无法切换模型选择模式: ' + JSON.stringify(result));
  await sleep(80);
}

async function setModelSearch(cdp, value) {
  const result = await cdp.evaluate(
    modelRootExpression(
      `
      if(!root)return {ok:false,reason:'missing'};
      const el=root.querySelector('input[type="search"]');
      if(!el)return {ok:false,reason:'missing-search'};
      el.focus();
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(el,String(arguments[0]));
      el.dispatchEvent(new Event('input',{bubbles:true}));
      return {ok:true,value:el.value,active:document.activeElement===el};
      `,
      value
    )
  );
  if (!result || !result.ok) throw new Error('无法写入模型搜索: ' + JSON.stringify(result));
  await sleep(60);
  return result;
}

async function setModelChecked(cdp, id, checked) {
  const result = await cdp.evaluate(
    modelRootExpression(
      `
      if(!root)return {ok:false,reason:'missing'};
      const target=String(arguments[0]);
      const items=[...root.querySelectorAll('.models-item')];
      const item=items.find(function(candidate){
        const text=(candidate.innerText||'').replace(/\\s+/g,' ').replace(/（目录未列出）\\s*$/,'').trim();
        return text===target;
      });
      if(!item)return {ok:false,reason:'missing-model',ids:items.map(function(i){return (i.innerText||'').trim();})};
      const box=item.querySelector('input[type="checkbox"]');
      if(!box)return {ok:false,reason:'missing-checkbox'};
      if(box.checked!==Boolean(arguments[1]))box.click();
      return {ok:true,id:target,checked:box.checked};
      `,
      id,
      Boolean(checked)
    )
  );
  if (!result || !result.ok) throw new Error('无法勾选模型 ' + id + ': ' + JSON.stringify(result));
  await sleep(60);
  return result;
}

async function addManualModel(cdp, id) {
  const result = await cdp.evaluate(
    modelRootExpression(
      `
      if(!root)return {ok:false,reason:'missing'};
      const input=root.querySelector('.models-manual input[type="text"]');
      const button=root.querySelector('.models-add');
      if(!input||!button)return {ok:false,reason:'missing-manual'};
      input.focus();
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,String(arguments[0]));
      input.dispatchEvent(new Event('input',{bubbles:true}));
      button.click();
      return {ok:true,value:input.value};
      `,
      id
    )
  );
  if (!result || !result.ok) throw new Error('无法手动添加模型: ' + JSON.stringify(result));
  await sleep(80);
}

async function refreshModelDirectory(cdp) {
  const result = await cdp.evaluate(
    modelRootExpression(
      `
      if(!root)return {ok:false,reason:'missing'};
      const button=root.querySelector('.models-refresh');
      if(!button)return {ok:false,reason:'missing-refresh'};
      if(button.disabled)return {ok:false,reason:'disabled'};
      button.click();
      return {ok:true};
      `
    )
  );
  if (!result || !result.ok) throw new Error('无法刷新模型目录: ' + JSON.stringify(result));
  await sleep(50);
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
    const detail = await apiRequest(server, 'GET', '/requests/detail?id=req-002');
    check(
      'GET /requests/detail 返回 redacted 详情',
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

  await step('mock API: Key CRUD 使用固定 query-ID 路由且只在创建/轮换返回明文', async () => {
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
    const updated = await apiRequest(server, 'PUT', '/keys?id=' + encodeURIComponent(id), {
      body: {
        label: '契约测试 Key（编辑）',
        enabled: true,
        expires_at: null,
        max_concurrent: 2,
        allowed_models: [],
        limits: { five_hour: 0, week: 0, month: 0, total: 0 }
      }
    });
    check('PUT /keys?id 更新且默认 all 保持 []', updated.status === 200 && Array.isArray(updated.body.allowed_models) && updated.body.allowed_models.length === 0);
    const rotated = await apiRequest(server, 'POST', '/keys/rotate?id=' + encodeURIComponent(id));
    check('POST /keys/rotate?id 返回新 secret', rotated.status === 200 && rotated.body.secret !== created.body.secret);
    const reset = await apiRequest(server, 'POST', '/keys/reset?id=' + encodeURIComponent(id), {
      body: { windows: ['five_hour', 'total'] }
    });
    check('POST /keys/reset?id 清零所选窗口', reset.status === 200 && reset.body.used.five_hour === 0 && reset.body.used.total === 0);
    const blockedBefore = JSON.parse(JSON.stringify(server.state.keys.find((key) => key.id === 'key-blocked')));
    const recovered = await apiRequest(server, 'POST', '/keys/recover?id=key-blocked', {
      body: { acknowledge: true }
    });
    check(
      'POST /keys/recover?id 显式 acknowledge 且不重置计数器',
      recovered.status === 200 &&
        recovered.body.blocked_reason === '' &&
        JSON.stringify(recovered.body.used) === JSON.stringify(blockedBefore.used) &&
        JSON.stringify(recovered.body.starts) === JSON.stringify(blockedBefore.starts) &&
        JSON.stringify(recovered.body.resets) === JSON.stringify(blockedBefore.resets),
      JSON.stringify(recovered.body)
    );
    const revoked = await apiRequest(server, 'DELETE', '/keys?id=' + encodeURIComponent(id));
    check('DELETE /keys?id 返回 204', revoked.status === 204);

    // 旧动态路由只允许作为负测试出现：必须 404 且没有副作用。
    const snapshot = JSON.stringify(server.state.keys);
    for (const legacy of [
      ['PUT', '/keys/key-alpha'],
      ['DELETE', '/keys/key-alpha'],
      ['POST', '/keys/key-alpha/rotate'],
      ['POST', '/keys/key-alpha/reset'],
      ['POST', '/keys/key-alpha/recover'],
      ['GET', '/requests/req-001']
    ]) {
      const old = await apiRequest(server, legacy[0], legacy[1]);
      check('旧动态路由 404: ' + legacy[0] + ' ' + legacy[1], old.status === 404, String(old.status));
    }
    check('旧动态路由没有改变 Key 状态', JSON.stringify(server.state.keys) === snapshot);
    return 'keys=ok';
  });

  await step('mock API: 一次性模型目录 capability（管理鉴权/空请求/精确 namespace/失败）', async () => {
    const unauthorized = await apiRequest(server, 'POST', '/models/token', { auth: false });
    check('POST /models/token 需要管理鉴权', unauthorized.status === 401, String(unauthorized.status));
    const bodyRejected = await apiRequest(server, 'POST', '/models/token', { body: {} });
    check('POST /models/token 拒绝非空 body', bodyRejected.status === 400, String(bodyRejected.status));
    const queryRejected = await fetch(
      server.origin + mock.MANAGEMENT_BASE + '/models/token?x=1',
      { method: 'POST', headers: { Authorization: 'Bearer ' + mock.DEFAULT_MANAGEMENT_KEY } }
    );
    check('POST /models/token 拒绝 query', queryRejected.status === 400, String(queryRejected.status));

    const issued = await apiRequest(server, 'POST', '/models/token');
    check(
      'POST /models/token 返回 token + 30s expires_at',
      issued.status === 200 &&
        typeof issued.body.token === 'string' &&
        /^[A-Za-z0-9_-]{43}$/.test(issued.body.token) &&
        Date.parse(issued.body.expires_at) - Date.now() > 28_000,
      JSON.stringify(issued.body && { ...issued.body, token: '[redacted]' })
    );
    const token = issued.body.token;
    const tokenCall = server.calls.filter(
      (call) => call.method === 'POST' && call.path.endsWith('/models/token') && call.status === 200
    ).pop();
    check(
      'token 只签发一次且请求为空 body/query',
      tokenCall.authorized === true &&
        tokenCall.body_bytes === 0 &&
        Object.keys(tokenCall.query).length === 0 &&
        tokenCall.request_body === undefined &&
        tokenCall.capability_stage === 'minted',
      JSON.stringify(tokenCall)
    );
    check('calls 不记录 capability token', !JSON.stringify(server.calls).includes(token));

    // 管理密钥不能替代 capability；目录只接受精确的 /v1/models。
    const withManagement = await fetch(server.origin + mock.CATALOG_PATH, {
      headers: { Authorization: 'Bearer ' + mock.DEFAULT_MANAGEMENT_KEY }
    });
    check('GET /v1/models 拒绝管理密钥', withManagement.status === 401, String(withManagement.status));
    const catalog = await fetch(server.origin + mock.CATALOG_PATH, {
      headers: { Authorization: 'Bearer ' + token }
    });
    const catalogBody = await catalog.json();
    check(
      'GET /v1/models 仅用 minted token 返回有界 {data:[{id}]}',
      catalog.status === 200 &&
        Array.isArray(catalogBody.data) &&
        catalogBody.data.length === mock.MAX_CATALOG_MODELS &&
        catalogBody.data.every((item) => typeof item.id === 'string' && Object.keys(item).length === 1) &&
        catalogBody.data.some((item) => item.id === 'gpt-5-codex'),
      JSON.stringify({ status: catalog.status, length: catalogBody.data && catalogBody.data.length })
    );
    const catalogCall = server.calls.filter((call) => call.path === mock.CATALOG_PATH).pop();
    check(
      '目录请求只记录 authorized/capability stage，不记录 token 或额外凭据 header',
      catalogCall.authorized === true &&
        catalogCall.capability_stage === 'used' &&
        catalogCall.has_extra_auth_header === false &&
        !('authorization' in catalogCall) &&
        !JSON.stringify(catalogCall).includes(token),
      JSON.stringify(catalogCall)
    );
    const replay = await fetch(server.origin + mock.CATALOG_PATH, {
      headers: { Authorization: 'Bearer ' + token }
    });
    check('capability 单次使用后失效', replay.status === 401, String(replay.status));
    const authFiles = await fetch(server.origin + '/v0/management/auth-files', {
      headers: { Authorization: 'Bearer ' + mock.DEFAULT_MANAGEMENT_KEY }
    });
    check('不存在 auth-files 旧发现回退', authFiles.status === 404, String(authFiles.status));
    const prefixed = await fetch(server.origin + mock.MANAGEMENT_BASE + '/v1/models', {
      headers: { Authorization: 'Bearer ' + mock.DEFAULT_MANAGEMENT_KEY }
    });
    check('目录 namespace 精确为 /v1/models', prefixed.status === 404, String(prefixed.status));

    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_token: true } })
    });
    const tokenFailed = await apiRequest(server, 'POST', '/models/token');
    check('token 签发失败可注入', tokenFailed.status === 502, String(tokenFailed.status));
    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_token: false, fail_models: true } })
    });
    const forModels = await apiRequest(server, 'POST', '/models/token');
    const modelsFailed = await fetch(server.origin + mock.CATALOG_PATH, {
      headers: { Authorization: 'Bearer ' + forModels.body.token }
    });
    check('模型目录失败可注入且消耗 capability', modelsFailed.status === 502, String(modelsFailed.status));
    await fetch(server.origin + '/__mock/control', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ catalog: { fail_models: false } })
    });
    return 'catalog-capability=ok';
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

function tokenCalls(server, since = 0) {
  return server.calls.filter(
    (call) => call.seq > since && call.method === 'POST' && call.path.endsWith('/models/token')
  );
}

function catalogCalls(server, since = 0) {
  return server.calls.filter((call) => call.seq > since && call.path === mock.CATALOG_PATH);
}

async function setCatalogControl(server, patch) {
  const response = await fetch(server.origin + '/__mock/control', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ catalog: patch })
  });
  if (response.status !== 200) throw new Error('catalog control failed: ' + response.status);
}

function suffixOf(call) {
  const index = call.path.indexOf(mock.MANAGEMENT_BASE);
  return index >= 0 ? call.path.slice(index + mock.MANAGEMENT_BASE.length) : call.path;
}

function callPattern(call) {
  const suffix = suffixOf(call);
  if (suffix === '/keys/rotate') return 'POST /keys/rotate';
  if (suffix === '/keys/reset') return 'POST /keys/reset';
  if (suffix === '/keys/recover') return 'POST /keys/recover';
  if (suffix === '/keys' && (call.method === 'PUT' || call.method === 'DELETE')) {
    return call.method + ' /keys';
  }
  if (suffix === '/requests/detail') return 'GET /requests/detail';
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
        manualProviderFields:root.querySelectorAll('[data-kind="manual"] input[id*="provider"], [data-kind="manual"] select[id*="provider"]').length,
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

function pricingCatalogDetailsExpression(action) {
  return pageFn(
    `
    const root=document.querySelector('[data-testid="observer-settings"]');
    if(!root)return {ok:false,reason:'missing-settings'};
    const details=[...root.querySelectorAll('details.settings-catalog')].find(function(el){
      const summary=el.querySelector('summary');
      return !!summary&&/生效价格明细/.test(summary.innerText);
    });
    if(!details)return {ok:false,reason:'missing-catalog'};
    ${action}
    return {ok:true,open:details.open};
    `
  );
}

async function openPricingCatalog(cdp) {
  const result = await cdp.evaluate(pricingCatalogDetailsExpression('details.open=true;'));
  if (!result || !result.ok) throw new Error('无法展开价格明细: ' + JSON.stringify(result));
  await cdp.waitFor(
    `(() => {const root=document.querySelector('[data-testid="observer-settings"]');` +
      `if(!root)return false;const details=[...root.querySelectorAll('details.settings-catalog')].find(function(el){` +
      `const summary=el.querySelector('summary');return !!summary&&/生效价格明细/.test(summary.innerText);});` +
      `return !!details&&details.open&&details.querySelectorAll('tbody tr').length>0;})()`,
    5000,
    '价格明细已展开'
  );
}

async function closePricingCatalog(cdp) {
  await cdp
    .evaluate(pricingCatalogDetailsExpression('details.open=false;'))
    .catch(() => {});
  await sleep(30);
}

async function pricingCatalogState(cdp) {
  return cdp.evaluate(
    pageFn(
      `
      const root=document.querySelector('[data-testid="observer-settings"]');
      if(!root)return {present:false};
      const details=[...root.querySelectorAll('details.settings-catalog')].find(function(el){
        const summary=el.querySelector('summary');return !!summary&&/生效价格明细/.test(summary.innerText);
      });
      if(!details)return {present:false};
      return {
        present:true,
        open:details.open,
        summary:details.querySelector('summary').innerText.trim(),
        rows:[...details.querySelectorAll('tbody tr')].map(function(row){
          return [...row.children].map(function(cell){return cell.innerText.trim();});
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

// 负向目录注入（部分失败/中止）会产生网络诊断；只豁免本次注入相关条目。
function absorbExpectedDiagnostics(since, pattern) {
  const phaseErrors = consoleErrors.splice(since);
  const expected = phaseErrors.filter((entry) => pattern.test((entry.text || '') + ' ' + (entry.url || '')));
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
      const beforeCalls = managementCalls(server).filter(
        (call) => call.method === 'GET' && suffixOf(call) === '/requests/detail'
      ).length;
      check('列表渲染后未读取详情', beforeCalls === 0 && before === 0, 'detailCalls=' + beforeCalls);

      const clicked = await findModelButton(cdp, mock.XSS.model, true);
      check('日志表恶意模型也是可点击按钮', clicked && clicked.ok, JSON.stringify(clicked));
      const dialog = await waitDialog(cdp, 15000);
      check('点击后出现详情对话框', dialog.present, JSON.stringify(dialog.buttons));
      const afterCalls = managementCalls(server).filter(
        (call) => call.method === 'GET' && suffixOf(call) === '/requests/detail'
      ).length;
      check('点击后恰好新增一次详情读取', afterCalls === beforeCalls + 1, 'detailCalls=' + afterCalls);
      const detailCall = managementCalls(server).filter(
        (call) => call.method === 'GET' && suffixOf(call) === '/requests/detail'
      ).pop();
      check(
        '详情读取的是被点击的恶意记录',
        detailCall && detailCall.query && detailCall.query.id === 'req-002',
        detailCall && JSON.stringify(detailCall.query)
      );
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

  await step('Key 管理: 模型选择器/默认 all/丢失 ID/搜索/部分失败/中止/全动作', async () => {
    await gotoPage(cdp, 'keys');
    keysReady = true;
    await cdp.waitFor(
      '(() => {const c=document.querySelector("#content");return !!c && c.querySelectorAll("tbody tr,[data-key-id],.key-row,.card").length>0;})()',
      15000,
      'Key 列表'
    );
    const pageText = await text(cdp, '#content');
    check('Key 列表展示假 Key', /生产 Key|已过期 Key|待恢复计费 Key/.test(pageText), pageText.slice(0, 240));

    const injection = await injectionAudit(cdp);
    check('恶意 Key 标签只作为文本', injection.flag === null && injection.injected === 0, JSON.stringify(injection));
    check('页面保留恶意 Key 标签字面量', pageText.includes(mock.XSS.label), pageText.slice(0, 240));

    const listAudit = await accessibilityAudit(cdp, '#content');
    check('Key 页面控件均有可访问名称', listAudit.unnamed.length === 0, JSON.stringify(listAudit.unnamed));

    // 打开 Key 对话框才按需获取一次性 capability，再读取精确 /v1/models。
    const catalogBaseline = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    const setKeyField = (label, value) => setFieldByLabel(cdp, label, value, DIALOG_ROOT);
    const clickSave = () =>
      clickText(cdp, '保存', {
        selector: 'dialog button, [role="dialog"] button, [aria-modal="true"] button'
      });
    const keyByLabel = (label) => server.state.keys.find((item) => item.label === label);
    const openNew = async () => {
      await gotoPage(cdp, 'keys');
      await clickText(cdp, '新建 Key', { selector: '#content button, button' });
      const form = await waitDialog(cdp);
      check('新建 Key 打开对话框', form.present);
      return form;
    };
    const closeSecret = async (secret, what) => {
      check(what + '后一次性展示假 secret', /^observer_sk_[0-9a-f]{64}$/.test(secret), secret.slice(0, 16) + '…');
      check(what + ' secret 不出现在 URL', server.calls.every((call) => !call.path.includes('observer_sk_')));
      await closeDialog(cdp).catch(() => {});
      await sleep(150);
      check(what + '后 secret 从页面清除', await secretAbsent(cdp));
    };

    const form = await openNew();
    const formAudit = await accessibilityAudit(cdp, DIALOG_ROOT);
    check('Key 表单控件均有可访问名称', formAudit.unnamed.length === 0, JSON.stringify(formAudit.unnamed));
    check('Key 表单对话框有可访问名称', Boolean(form.name), JSON.stringify({ tag: form.tag, role: form.role, name: form.name }));
    const hasModelLabel = await cdp.evaluate(
      modelRootExpression('return !!root && root.getAttribute("aria-label")==="允许模型";')
    );
    for (const label of ['标签', '到期时间', '并发上限', '允许模型', '5小时额度', '周额度', '月额度', '总额度']) {
      const compactText = form.text.replace(/\s+/g, '');
      const present =
        compactText.includes(label.replace(/\s+/g, '')) || (label === '允许模型' && hasModelLabel);
      check('Key 表单含标签「' + label + '」', present, form.text.slice(0, 300));
    }
    check('Key 表单不再使用旧允许模型 textarea', !form.text.includes('每行精确模型'), form.text.slice(0, 300));
    check('Key 表单挂载 models.js 选择器', await modelPickerPresent(cdp));
    await waitModelDirectory(cdp);
    let picker = await modelPickerState(cdp);
    check(
      '新建默认“不限模型”（all）且目录提供至少四个模型',
      picker.present && picker.all === true && picker.restricted === false && picker.items.length >= 4,
      JSON.stringify({ all: picker.all, restricted: picker.restricted, items: picker.items })
    );
    check(
      '目录响应有界且主模型可用',
      picker.items.length === mock.MAX_CATALOG_MODELS &&
        picker.items.some((item) => item.id === 'gpt-5-codex') &&
        picker.items.some((item) => item.id === 'claude-opus-4-1'),
      JSON.stringify({ count: picker.items.length, max: mock.MAX_CATALOG_MODELS })
    );

    const firstTokens = tokenCalls(server, catalogBaseline);
    const firstCatalogs = catalogCalls(server, catalogBaseline);
    check(
      '打开 Key 对话框先 POST /models/token 再 GET /v1/models',
      firstTokens.length === 1 &&
        firstCatalogs.length === 1 &&
        firstTokens[0].seq < firstCatalogs[0].seq &&
        firstTokens[0].status === 200 &&
        firstCatalogs[0].status === 200,
      JSON.stringify({
        tokens: firstTokens.map((call) => ({ seq: call.seq, status: call.status })),
        catalogs: firstCatalogs.map((call) => ({ seq: call.seq, status: call.status }))
      })
    );
    check(
      'token 请求为管理 bearer + 空 body/query',
      firstTokens[0].authorized === true &&
        firstTokens[0].capability_stage === 'minted' &&
        firstTokens[0].body_bytes === 0 &&
        Object.keys(firstTokens[0].query).length === 0 &&
        firstTokens[0].request_body === undefined,
      JSON.stringify(firstTokens[0])
    );
    check(
      '目录请求仅带 capability header，无额外凭据/query',
      firstCatalogs[0].authorized === true &&
        firstCatalogs[0].capability_stage === 'used' &&
        firstCatalogs[0].has_extra_auth_header === false &&
        Object.keys(firstCatalogs[0].query).length === 0 &&
        !JSON.stringify(firstCatalogs[0]).includes(mock.DEFAULT_MANAGEMENT_KEY),
      JSON.stringify(firstCatalogs[0])
    );
    check(
      '模型发现不再访问 auth-files 或 pricing 目录',
      !server.calls.some((call) => call.path.includes('/auth-files')),
      JSON.stringify(server.calls.filter((call) => call.path.includes('/auth-files')))
    );
    check('默认 all 时受限面板隐藏', picker.panelHidden === true, JSON.stringify(picker.panelHidden));

    const defaultLabel = '自动化默认全部 Key';
    await setKeyField('标签', defaultLabel);
    await setKeyField('并发上限', '3');
    await setKeyField('5小时额度', '5');
    await setKeyField('周额度', '20');
    await setKeyField('月额度', '50');
    await setKeyField('总额度', '100');
    const createBefore = server.state.counters.keyCreates;
    await clickSave();
    const defaultCreate = await waitForCall(
      server,
      (call) => call.method === 'POST' && suffixOf(call) === '/keys' && call.request_body && call.request_body.label === defaultLabel,
      20000,
      'POST /keys 默认 all'
    );
    check('默认 all 创建发送 POST /keys', defaultCreate.status === 201 && server.state.counters.keyCreates === createBefore + 1);
    check(
      'default[] 创建保持 allowed_models=[]',
      Array.isArray(defaultCreate.request_body.allowed_models) && defaultCreate.request_body.allowed_models.length === 0,
      JSON.stringify(defaultCreate.request_body)
    );
    const defaultKey = keyByLabel(defaultLabel);
    check('mock 创建了 default[] Key', Boolean(defaultKey), server.state.keys.map((item) => item.label).join('|'));
    await sleep(250);
    await closeSecret(await waitForSecret(cdp), '默认 Key 创建');

    if (defaultKey) {
      await cdp.waitFor(
        '(() => {' + VISIBLE_SNIPPET + 'return document.querySelector("#content").innerText.includes(' + js(defaultLabel) + ');})()',
        15000,
        'default[] Key 出现在列表'
      );
      await clickInRow(cdp, defaultLabel, '编辑');
      const editDialog = await waitDialog(cdp);
      check('编辑 default[] Key 打开对话框', editDialog.present);
      await waitModelDirectory(cdp);
      picker = await modelPickerState(cdp);
      check('编辑 default[] Key 仍为 all', picker.all === true && picker.restricted === false, JSON.stringify(picker));
      check(
        '五分钟目录缓存避免重复签发 capability',
        tokenCalls(server, catalogBaseline).length === 1 &&
          catalogCalls(server, catalogBaseline).length === 1,
        JSON.stringify({
          tokens: tokenCalls(server, catalogBaseline).length,
          catalogs: catalogCalls(server, catalogBaseline).length
        })
      );
      await setKeyField('标签', defaultLabel + '（编辑）');
      await clickSave();
      const defaultEdit = await waitForCall(
        server,
        (call) => call.method === 'PUT' && suffixOf(call) === '/keys' && call.query.id === defaultKey.id,
        20000,
        'PUT /keys?id 默认 all'
      );
      check(
        '编辑 default[] Key 保持 allowed_models=[]',
        defaultEdit.status === 200 && Array.isArray(defaultEdit.request_body.allowed_models) && defaultEdit.request_body.allowed_models.length === 0,
        JSON.stringify(defaultEdit.request_body)
      );
      await closeDialog(cdp).catch(() => {});
    }

    // 旧 textarea 用例改为通过组件勾选两个精确模型，并验证搜索状态不会被勾选重绘破坏。
    await openNew();
    await waitModelDirectory(cdp);
    const restrictedLabel = '自动化验收 Key';
    await setKeyField('标签', restrictedLabel);
    await setKeyField('并发上限', '3');
    await setKeyField('5小时额度', '5');
    await setKeyField('周额度', '20');
    await setKeyField('月额度', '50');
    await setKeyField('总额度', '100');
    await setModelMode(cdp, 'restricted');
    picker = await modelPickerState(cdp);
    check('切换到“仅允许所选模型”显示受限面板', picker.restricted === true && picker.panelHidden === false, JSON.stringify(picker));
    await setModelSearch(cdp, 'gpt');
    let searching = await modelPickerState(cdp);
    check(
      '模型搜索过滤列表并保留输入',
      searching.search === 'gpt' && searching.items.length > 0 && searching.items.every((item) => item.id.toLowerCase().includes('gpt')),
      JSON.stringify(searching.items)
    );
    await setModelChecked(cdp, 'gpt-5-codex', true);
    searching = await modelPickerState(cdp);
    check(
      '勾选模型后搜索词仍保留',
      searching.search === 'gpt' && searching.items.find((item) => item.id === 'gpt-5-codex').checked === true,
      JSON.stringify(searching)
    );
    await setModelSearch(cdp, 'claude');
    await setModelChecked(cdp, 'claude-opus-4-1', true);
    searching = await modelPickerState(cdp);
    check(
      '第二个模型的搜索词保留且已勾选',
      searching.search === 'claude' && searching.items.find((item) => item.id === 'claude-opus-4-1').checked === true,
      JSON.stringify(searching)
    );
    await setModelSearch(cdp, '');
    picker = await modelPickerState(cdp);
    check(
      '组件恰有两个已选精确模型',
      picker.items.filter((item) => item.checked).map((item) => item.id).sort().join(',') === 'claude-opus-4-1,gpt-5-codex',
      JSON.stringify(picker.items.filter((item) => item.checked))
    );
    await clickSave();
    const restrictedCreate = await waitForCall(
      server,
      (call) => call.method === 'POST' && suffixOf(call) === '/keys' && call.request_body && call.request_body.label === restrictedLabel,
      20000,
      'POST /keys 两个模型'
    );
    check(
      '组件选择两个模型后创建 payload 精确匹配',
      JSON.stringify(restrictedCreate.request_body.allowed_models) === JSON.stringify(['gpt-5-codex', 'claude-opus-4-1']),
      JSON.stringify(restrictedCreate.request_body.allowed_models)
    );
    const key = keyByLabel(restrictedLabel);
    check('mock 创建了受限 Key', Boolean(key), server.state.keys.map((item) => item.label).join('|'));
    await sleep(250);
    await closeSecret(await waitForSecret(cdp), '受限 Key 创建');

    if (key) {
      await cdp.waitFor(
        '(() => {' + VISIBLE_SNIPPET + 'return document.querySelector("#content").innerText.includes(' + js(restrictedLabel) + ');})()',
        15000,
        '受限 Key 出现在列表'
      );
      await clickInRow(cdp, restrictedLabel, '编辑');
      await waitDialog(cdp);
      await waitModelDirectory(cdp);
      picker = await modelPickerState(cdp);
      check(
        '编辑受限 Key 回显 restricted + 两个勾选',
        picker.restricted === true &&
          picker.items.filter((item) => item.checked).map((item) => item.id).sort().join(',') === 'claude-opus-4-1,gpt-5-codex',
        JSON.stringify(picker.items)
      );
      await setKeyField('标签', restrictedLabel + '（编辑）');
      await clickSave();
      const restrictedEdit = await waitForCall(
        server,
        (call) => call.method === 'PUT' && suffixOf(call) === '/keys' && call.query.id === key.id,
        20000,
        'PUT /keys?id 受限'
      );
      check(
        '编辑受限 Key 保持两个精确 ID',
        JSON.stringify(restrictedEdit.request_body.allowed_models) === JSON.stringify(['gpt-5-codex', 'claude-opus-4-1']),
        JSON.stringify(restrictedEdit.request_body.allowed_models)
      );
      await closeDialog(cdp).catch(() => {});
    }

    // 目录已不存在的历史 ID 必须保留并标记，手动精确 ID 也可追加。
    await gotoPage(cdp, 'keys');
    await clickInRow(cdp, '历史受限 Key', '编辑');
    await waitDialog(cdp);
    await waitModelDirectory(cdp);
    picker = await modelPickerState(cdp);
    const retired = picker.items.find((item) => item.id === 'retired-model-v0');
    check(
      '编辑保留目录外历史 ID 并标记（目录未列出）',
      picker.restricted === true && retired && retired.checked === true && retired.missing === true,
      JSON.stringify(picker.items)
    );
    check(
      '历史 ID 与目录内 ID 同时保持勾选',
      picker.items.find((item) => item.id === 'gpt-5-codex').checked === true,
      JSON.stringify(picker.items)
    );
    await addManualModel(cdp, 'manual/provider-exact-v9');
    // 目录有 100 条可见上限，手动 ID 追加在末尾；用搜索定位以验证其已勾选。
    await setModelSearch(cdp, 'manual/provider-exact-v9');
    picker = await modelPickerState(cdp);
    check(
      '手动添加精确模型 ID 后自动勾选',
      picker.items.find((item) => item.id === 'manual/provider-exact-v9') &&
        picker.items.find((item) => item.id === 'manual/provider-exact-v9').checked === true,
      JSON.stringify(picker.items)
    );
    await setModelSearch(cdp, '');
    await clickSave();
    const legacyEdit = await waitForCall(
      server,
      (call) => call.method === 'PUT' && suffixOf(call) === '/keys' && call.query.id === 'key-legacy',
      20000,
      'PUT /keys?id 历史'
    );
    check(
      '保存保留丢失 ID 与手动精确 ID',
      JSON.stringify(legacyEdit.request_body.allowed_models) ===
        JSON.stringify(['retired-model-v0', 'gpt-5-codex', 'manual/provider-exact-v9']),
      JSON.stringify(legacyEdit.request_body.allowed_models)
    );
    await closeDialog(cdp).catch(() => {});

    // 目录失败：只显示通用错误，不改变默认 all / 已选模型，也不泄漏凭据。
    const failureBaseline = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await setCatalogControl(server, { fail_models: true });
    await openNew();
    // 目录有 5 分钟缓存；失败用例必须点“刷新模型列表”强制绕过缓存。
    const failureDiagnosticsStart = consoleErrors.length;
    await refreshModelDirectory(cdp);
    await waitModelDirectory(cdp);
    await cdp.waitFor(
      modelRootExpression(
        'return !!root && !!root.querySelector(".models-warning") && !root.querySelector(".models-warning").hidden;'
      ),
      15000,
      '目录失败警告'
    );
    picker = await modelPickerState(cdp);
    check(
      '目录失败显示通用警告且保留缓存模型',
      picker.warningVisible === true &&
        /加载失败|模型目录/.test(picker.warning) &&
        picker.items.some((item) => item.id === 'gpt-5-codex') &&
        picker.items.some((item) => item.id === 'claude-opus-4-1'),
      JSON.stringify({ warning: picker.warning, count: picker.items.length })
    );
    check(
      '目录失败不改变默认 all 模式',
      picker.all === true && picker.restricted === false,
      JSON.stringify(picker)
    );
    check(
      '模型目录 UI 不包含管理凭据',
      !/DO-NOT-LEAK|observer-browser-secret|Bearer\s/i.test(picker.text),
      picker.text
    );
    const failureTokens = tokenCalls(server, failureBaseline);
    const failureCatalogs = catalogCalls(server, failureBaseline);
    check(
      '强制刷新重新签发 capability 并精确请求 /v1/models',
      failureTokens.length === 1 &&
        failureCatalogs.length === 1 &&
        failureTokens[0].seq < failureCatalogs[0].seq &&
        failureCatalogs[0].capability_stage === 'used' &&
        failureCatalogs[0].status === 502,
      JSON.stringify({
        tokens: failureTokens.map((call) => ({ seq: call.seq, status: call.status })),
        catalogs: failureCatalogs.map((call) => ({ seq: call.seq, status: call.status }))
      })
    );
    await closeDialog(cdp).catch(() => {});
    absorbExpectedDiagnostics(failureDiagnosticsStart, /502|v1\/models|catalog/);
    await setCatalogControl(server, { fail_models: false });

    // 慢目录中关闭表单必须 abort；迟到响应不得写入 UI 或泄漏凭据。
    await setCatalogControl(server, { models_delay_ms: 1200 });
    const abortSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await gotoPage(cdp, 'keys');
    await clickText(cdp, '新建 Key', { selector: '#content button, button' });
    await waitDialog(cdp);
    await cdp.waitFor(
      '(() => {' + VISIBLE_SNIPPET +
        'const dialogs=[...document.querySelectorAll("' + DIALOG_ROOT + '")].filter(visible);' +
        'const host=dialogs.length?dialogs[dialogs.length-1]:document;' +
        'return !!host.querySelector("' + MODEL_ROOT_SELECTOR + '");})()',
      5000,
      '慢目录选择器已挂载'
    );
    // 缓存会让打开表单直接完成；强制刷新后立即关闭，才能覆盖真正在途的请求。
    const abortDiagnosticsStart = consoleErrors.length;
    await refreshModelDirectory(cdp);
    await sleep(100);
    await closeDialog(cdp).catch(() => {});
    await sleep(1500);
    const leaked = await cdp.evaluate(
      pageFn(
        VISIBLE_SNIPPET +
          'return {dialog:[...document.querySelectorAll("' + DIALOG_ROOT + '")].some(visible),' +
          'text:document.body.innerText,' +
          'secret:/DO-NOT-LEAK|observer-browser-secret|Bearer\s/i.test(document.body.innerText)};'
      )
    );
    check('中止后无残留模型对话框', leaked.dialog === false, JSON.stringify(leaked.dialog));
    check('中止后迟到目录响应不泄漏凭据', leaked.secret === false, leaked.text.slice(0, 300));
    const abortTokens = tokenCalls(server, abortSeq);
    const abortedCatalogs = catalogCalls(server, abortSeq);
    check(
      '慢目录请求在关闭对话框后被中止且不携带额外凭据',
      abortTokens.length === 1 &&
        abortedCatalogs.length === 1 &&
        abortTokens[0].seq < abortedCatalogs[0].seq &&
        abortedCatalogs[0].authorized === true &&
        abortedCatalogs[0].has_extra_auth_header === false &&
        (abortedCatalogs[0].aborted === true || abortedCatalogs[0].status === 200),
      JSON.stringify({ tokens: abortTokens, catalogs: abortedCatalogs })
    );
    absorbExpectedDiagnostics(abortDiagnosticsStart, /ERR_ABORTED|v1\/models|catalog/);
    await setCatalogControl(server, { models_delay_ms: 0 });
    await openNew();
    await waitModelDirectory(cdp);
    picker = await modelPickerState(cdp);
    check(
      '中止后重新打开沿用既有缓存并恢复 UI',
      picker.present &&
        picker.warningVisible === false &&
        picker.items.length === mock.MAX_CATALOG_MODELS &&
        tokenCalls(server, abortSeq).length === 1,
      JSON.stringify({ items: picker.items.length, tokens: tokenCalls(server, abortSeq).length })
    );
    await refreshModelDirectory(cdp);
    await waitModelDirectory(cdp);
    picker = await modelPickerState(cdp);
    check(
      '中止后强制刷新重新签发 token 并恢复完整目录',
      picker.present &&
        picker.warningVisible === false &&
        picker.items.length === mock.MAX_CATALOG_MODELS &&
        tokenCalls(server, abortSeq).length === 2 &&
        catalogCalls(server, abortSeq).length === 2,
      JSON.stringify({
        items: picker.items.length,
        tokens: tokenCalls(server, abortSeq).length,
        catalogs: catalogCalls(server, abortSeq).length
      })
    );
    await closeDialog(cdp).catch(() => {});

    if (key) {
      const editedLabel = restrictedLabel + '（编辑）';
      const toggle = async (action, enabled) => {
        await cdp.waitFor(
          '(() => {' + VISIBLE_SNIPPET +
            'const norm=(s)=>(s||"").replace(/\\s+/g," ").trim();' +
            'const rows=[...document.querySelectorAll("tr,[role=row],[data-key-id],.key-row,.card")].filter(visible)' +
            '.filter((row)=>norm(row.innerText).includes(' + js(editedLabel) + '));' +
            'return rows.some((row)=>[...row.querySelectorAll("button,[role=button]")].filter(visible)' +
            '.some((b)=>{const t=norm(b.innerText);return t.includes(' + js(action) + ')||/操作|更多|菜单/.test(t);}));})()',
          15000,
          '受限 Key 出现操作按钮'
        );
        const startSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
        await clickInRow(cdp, editedLabel, action);
        await acceptConfirm(cdp, [action]);
        return waitForCall(
          server,
          (item) =>
            item.seq > startSeq &&
            item.method === 'PUT' &&
            suffixOf(item) === '/keys' &&
            item.query.id === key.id &&
            item.request_body &&
            item.request_body.enabled === enabled,
          15000,
          action + ' -> PUT /keys?id'
        );
      };
      await gotoPage(cdp, 'keys');
      const disabledCall = await toggle('停用', false);
      check('停用发送 enabled=false 的 PUT /keys?id', disabledCall.request_body.enabled === false && disabledCall.status === 200);
      const enabledCall = await toggle('启用', true);
      check('启用发送 enabled=true 的 PUT /keys?id', enabledCall.request_body.enabled === true && enabledCall.status === 200);

      await clickInRow(cdp, editedLabel, '轮换');
      await acceptConfirm(cdp, ['轮换']);
      const rotateCall = await waitForCall(
        server,
        (call) => call.method === 'POST' && suffixOf(call) === '/keys/rotate' && call.query.id === key.id,
        15000,
        'POST /keys/rotate?id'
      );
      check('轮换发送 POST /keys/rotate?id', rotateCall.status === 200);
      await sleep(250);
      await closeSecret(await waitForSecret(cdp), '轮换');

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
        (call) => call.method === 'POST' && suffixOf(call) === '/keys/reset' && call.query.id === key.id,
        15000,
        'POST /keys/reset?id'
      );
      check(
        '重置发送 POST /keys/reset?id 且包含窗口',
        resetCall.status === 200 && resetCall.request_body && Array.isArray(resetCall.request_body.windows) && resetCall.request_body.windows.length > 0,
        JSON.stringify(resetCall.request_body)
      );
      await closeDialog(cdp).catch(() => {});

      await clickInRow(cdp, editedLabel, '撤销');
      await acceptConfirm(cdp, ['撤销', '删除']);
      const revokeCall = await waitForCall(
        server,
        (call) => call.method === 'DELETE' && suffixOf(call) === '/keys' && call.query.id === key.id,
        15000,
        'DELETE /keys?id'
      );
      check('撤销发送 DELETE /keys?id', revokeCall.status === 204);
    }

    // 恢复：UI 必须先给出明确警告；确认后才发送 acknowledge:true，且不得重置计数器。
    await gotoPage(cdp, 'keys');
    const blocked = server.state.keys.find((item) => item.id === 'key-blocked');
    const blockedBefore = JSON.parse(JSON.stringify(blocked));
    const recoversBefore = server.counters.keyRecovers;
    const dialogStart = nativeDialogs.length;
    let warningText = '';
    await clickInRow(cdp, '待恢复计费 Key', '恢复');
    await sleep(250);
    const warnDialog = await dialogInfo(cdp);
    if (warnDialog.present) {
      warningText = warnDialog.text || '';
      // UI 要求显式勾选“已核对并处理缺失用量”，然后才启用确认按钮。
      const checked = await cdp.evaluate(
        pageFn(
          VISIBLE_SNIPPET +
            'const d=[...document.querySelectorAll("' + DIALOG_ROOT + '")].filter(visible).pop();' +
            'if(!d)return 0;const boxes=[...d.querySelectorAll(\'input[type="checkbox"]\')].filter(visible);' +
            'boxes.forEach((box)=>{if(!box.checked)box.click();});return boxes.length;'
        )
      );
      check('恢复对话框要求勾选核对声明', checked >= 1, String(checked));
      await acceptConfirm(cdp, ['恢复', '确认', '继续']);
    }
    const newDialogs = nativeDialogs.slice(dialogStart);
    if (!warningText && newDialogs.length) warningText = newDialogs.map((dialog) => dialog.message).join(' | ');
    check(
      '恢复操作先显示未知计费/不可逆警告',
      /未知|计费|不可逆|acknowledge|恢复/.test(warningText),
      warningText || JSON.stringify(newDialogs)
    );
    const recoverCall = await waitForCall(
      server,
      (call) => call.method === 'POST' && suffixOf(call) === '/keys/recover' && call.query.id === 'key-blocked',
      15000,
      'POST /keys/recover?id'
    );
    check(
      '恢复显式发送 acknowledge:true',
      recoverCall.status === 200 && recoverCall.request_body && recoverCall.request_body.acknowledge === true,
      JSON.stringify(recoverCall.request_body)
    );
    check('恢复清除 Key 级阻塞原因', blocked.blocked_reason === '', JSON.stringify(blocked.blocked_reason));
    check(
      '恢复不重置 used/starts/resets/limits',
      JSON.stringify(blocked.used) === JSON.stringify(blockedBefore.used) &&
        JSON.stringify(blocked.starts) === JSON.stringify(blockedBefore.starts) &&
        JSON.stringify(blocked.resets) === JSON.stringify(blockedBefore.resets) &&
        JSON.stringify(blocked.limits) === JSON.stringify(blockedBefore.limits),
      JSON.stringify(blocked)
    );
    check('恢复只增加 keyRecovers 计数', server.counters.keyRecovers === recoversBefore + 1, String(server.counters.keyRecovers));

    // logout 必须清空模型目录缓存；重新登录后打开 Key 对话框要重新签发 capability。
    const logoutSeq = server.calls.reduce((max, call) => Math.max(max, call.seq || 0), 0);
    await clickText(cdp, '退出', { selector: '#content button, button, [role="button"]' });
    await waitVisible(cdp, '#login', 15000, '退出后登录界面');
    await login(cdp, mock.DEFAULT_MANAGEMENT_KEY);
    await cdp.waitFor(
      '(() => {' + VISIBLE_SNIPPET +
        'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
        'return !!l&&!!c&&!visible(l)&&visible(c);})()',
      20000,
      '重新登录'
    );
    await gotoPage(cdp, 'keys');
    await openNew();
    await waitModelDirectory(cdp);
    picker = await modelPickerState(cdp);
    const logoutTokens = tokenCalls(server, logoutSeq);
    const logoutCatalogs = catalogCalls(server, logoutSeq);
    check(
      'logout 清除模型目录缓存并在下次打开时重新签发 token',
      logoutTokens.length === 1 &&
        logoutCatalogs.length === 1 &&
        logoutTokens[0].seq < logoutCatalogs[0].seq &&
        logoutTokens[0].capability_stage === 'minted' &&
        logoutCatalogs[0].capability_stage === 'used',
      JSON.stringify({
        tokens: logoutTokens.map((call) => ({ seq: call.seq, status: call.status })),
        catalogs: logoutCatalogs.map((call) => ({ seq: call.seq, status: call.status }))
      })
    );
    check('logout 后目录仍可用', picker.present && picker.items.length === mock.MAX_CATALOG_MODELS, JSON.stringify({ items: picker.items.length }));
    await closeDialog(cdp).catch(() => {});

    return 'models+key actions';
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

    // 8 MiB 上限：原生约束阻止点击提交；直接触发 submit 也必须被应用层拒绝且不发 PUT。
    const putsBeforeOversize = settingsPutCount(server);
    await setGeneralSettings(cdp, {
      capture: true,
      max_body_bytes: 8388609,
      max_body_storage_bytes: 536870912,
      raw_retention_hours: 48,
      daily_retention_days: 730
    });
    const oversizeValidity = await cdp.evaluate(
      pageFn(
        `
        const input=document.querySelector('input[id$="-max-body-bytes"]');
        return input?{value:String(input.value),max:input.max,rangeOverflow:input.validity.rangeOverflow,valid:input.validity.valid}:null;
        `
      )
    );
    await clickSettingsTestId(cdp, 'settings-general-save');
    await sleep(250);
    check(
      'max_body > 8MiB 被原生约束标记越界',
      oversizeValidity &&
        oversizeValidity.value === '8388609' &&
        oversizeValidity.max === '8388608' &&
        oversizeValidity.rangeOverflow === true &&
        oversizeValidity.valid === false,
      JSON.stringify(oversizeValidity)
    );
    check(
      'max_body > 8MiB 点击保存不发送 PUT',
      settingsPutCount(server) === putsBeforeOversize,
      'PUT 数量未变'
    );
    await cdp.evaluate(
      pageFn(
        `
        const save=document.querySelector('[data-testid="settings-general-save"]');
        if(!save||!save.form)return false;
        save.form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true}));
        return true;
        `
      )
    );
    await cdp.waitFor(
      '(() => {const el=document.querySelector(\'[id$="-general-status"]\');' +
        'return !!el&&/8388608/.test(el.innerText);})()',
      5000,
      '8 MiB 上限应用层错误'
    );
    check(
      'max_body > 8MiB 应用层拒绝且不发送 PUT',
      settingsPutCount(server) === putsBeforeOversize &&
        server.state.settings.max_body_bytes === 1048576,
      JSON.stringify({ puts: settingsPutCount(server), max_body_bytes: server.state.settings.max_body_bytes })
    );

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

    await openPricingCatalog(cdp);
    const catalog = await pricingCatalogState(cdp);
    const gptCatalogRows = (catalog.rows || []).filter((row) => row[0] === 'gpt-5-codex');
    const autoRows = gptCatalogRows.filter((row) => row[2] === '自动目录');
    const manualCatalogRows = gptCatalogRows.filter((row) => row[2] === '手工');
    check(
      '价格明细区分同模型不同 provider',
      catalog.present &&
        catalog.open &&
        autoRows.length === 2 &&
        autoRows.map((row) => row[1]).sort().join(',') === 'azure,openai' &&
        autoRows[0][3] !== autoRows[1][3],
      JSON.stringify({ rows: catalog.rows, autoRows })
    );
    check(
      '价格明细中的手工价仍是 model-wide',
      manualCatalogRows.length === 1 &&
        manualCatalogRows[0][1] === '—' &&
        initial.manualProviderFields === 0,
      JSON.stringify({ manualRows: manualCatalogRows, providerFields: initial.manualProviderFields })
    );
    const pricingHelp = await cdp.evaluate(
      pageFn(
        `
        const root=document.querySelector('[data-testid="observer-settings"]');
        if(!root)return {present:false};
        return {
          present:true,
          text:root.innerText,
          manualLabels:[...root.querySelectorAll('[data-kind="manual"] input')].map(function(el){
            return (el.getAttribute('aria-label')||'')+' '+(el.id||'');
          })
        };
        `
      )
    );
    check(
      '价格帮助说明完整提示词含缓存读/写',
      pricingHelp.present &&
        /完整提示词上下文（未缓存输入 \+ 缓存读 \+ 缓存写）/.test(pricingHelp.text) &&
        /含缓存读\/写/.test(pricingHelp.text),
      JSON.stringify({ present: pricingHelp.present, text: (pricingHelp.text || '').slice(0, 500) })
    );
    check(
      '手工价格提供缓存读/写费率字段',
      /缓存读（USD \/ 百万 Token）/.test(pricingHelp.manualLabels.join(' ')) &&
        /缓存写（USD \/ 百万 Token）/.test(pricingHelp.manualLabels.join(' ')),
      JSON.stringify(pricingHelp.manualLabels)
    );
    await closePricingCatalog(cdp);

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
        if (size.width <= 500) {
          const scrollAudit = await keyDialogScrollAudit(cdp);
          const buttonsInside = (state) =>
            Array.isArray(state) &&
            state.length === 2 &&
            state.every((button) => button.inside === true);
          check(
            '移动端 Key 表单滚到顶部时保存/取消仍在对话框内',
            scrollAudit.present && buttonsInside(scrollAudit.top),
            JSON.stringify(scrollAudit)
          );
          check(
            '移动端 Key 表单滚到底部时保存/取消仍在对话框内',
            scrollAudit.present &&
              buttonsInside(scrollAudit.bottom) &&
              scrollAudit.body &&
              scrollAudit.body.scrollHeight >= scrollAudit.body.clientHeight,
            JSON.stringify(scrollAudit)
          );
        }
        await closeDialog(cdp);
        return bytes + ' bytes';
      });

      expected += 1;
      await step('截图 ' + size.width + '/' + theme + '/key-models', async () => {
        await gotoPage(cdp, 'keys');
        await clickText(cdp, '新建 Key', { selector: '#content button, button' });
        const dialog = await waitDialog(cdp);
        await waitModelDirectory(cdp);
        await setModelMode(cdp, 'restricted');
        await setModelChecked(cdp, 'gpt-5-codex', true);
        await setModelChecked(cdp, 'claude-opus-4-1', true);
        await cdp.evaluate(
          pageFn(
            VISIBLE_SNIPPET +
              'const dialogs=[...document.querySelectorAll("' + DIALOG_ROOT + '")].filter(visible);' +
              'const host=dialogs[dialogs.length-1]||document;const root=host.querySelector("' + MODEL_ROOT_SELECTOR + '");' +
              'if(root)root.scrollIntoView({block:"center"});return !!root;'
          )
        );
        await settle(cdp);
        const overflow = await overflowAudit(cdp);
        check(
          '模型选择器截图无横向溢出 ' + size.width + '/' + theme,
          overflow.overflow <= 1,
          JSON.stringify(overflow)
        );
        check(
          '模型选择器在视口内 ' + size.width + '/' + theme,
          dialog.present && dialog.rect.left >= -1 && dialog.rect.right <= dialog.vw + 1,
          JSON.stringify(dialog.present ? dialog.rect : dialog)
        );
        const bytes = await capture(
          cdp,
          path.join(shotsDir, size.width + '-' + theme + '-key-models.png'),
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
/* Real host read-only smoke                                                  */
/* -------------------------------------------------------------------------- */

function normalizeRealUI(baseUrl) {
  const raw = String(baseUrl || '').trim();
  if (!raw) throw new Error('--base-url 不能为空');
  const url = new URL(raw);
  if (!/^https?:$/.test(url.protocol)) throw new Error('--base-url 只支持 http/https');
  // 报告只保存规范化后的入口；拒绝可能夹带凭据的 URL 形态，而不是悄悄剥离。
  if (url.username || url.password) throw new Error('--base-url 不允许包含 userinfo');
  if (url.search) throw new Error('--base-url 不允许包含 query（避免把 token 写入报告）');
  if (url.hash) throw new Error('--base-url 不允许包含 hash');
  let pathname = url.pathname.replace(/\/+$/, '');
  if (/\/ui(?:\.html)?$/.test(pathname)) return url.origin + pathname;
  if (/\/v0\/(?:management|resource)\/plugins\/[^/]+$/.test(pathname)) {
    return url.origin + pathname + '/ui';
  }
  return url.origin + '/v0/management/plugins/' + mock.PLUGIN_ID + '/ui';
}

async function runRealHostSmoke(args) {
  if (!args.baseUrl) throw new Error('realhost smoke 需要 --base-url');
  reportDir = args.reportDir;
  chromePath = args.chrome;
  keepProfile = args.keepProfile;
  fs.mkdirSync(reportDir, { recursive: true });
  fs.rmSync(path.join(reportDir, 'screenshots'), { recursive: true, force: true });
  fs.mkdirSync(path.join(reportDir, 'screenshots'), { recursive: true });

  const uiURL = normalizeRealUI(args.baseUrl);
  const realOrigin = new URL(uiURL).origin;
  // 唯一允许的写请求：精确插件路径、空 query、空 body 的一次性目录 capability。
  const allowedTokenURL = realOrigin + mock.MANAGEMENT_BASE + '/models/token';
  const started = Date.now();
  const apiRequests = [];
  const blockedRequests = [];
  const allowedTokenPosts = [];
  let fetchInterceptionEnabled = false;
  let chrome = null;
  let profileDir = null;
  let fatal = null;

  // 报告只保留 origin+path，绝不写入 query/hash（避免任何凭据/token 进入报告）。
  function reportURL(raw) {
    try {
      const parsed = new URL(raw);
      return parsed.origin + parsed.pathname;
    } catch (err) {
      return '';
    }
  }

  // 浏览器会自行请求站点根 /favicon.ico；真实 CPA 没有该应用资源。
  // 仅当“同源根路径 + HTTP 404”时归入 expected_diagnostics，其他 404/CSP 仍失败。
  let favicon404Seen = false;
  const pendingFaviconLogs = [];
  function isRootFaviconURL(raw) {
    try {
      const parsed = new URL(raw);
      return parsed.origin === realOrigin && parsed.pathname === '/favicon.ico';
    } catch (err) {
      return false;
    }
  }
  function recordFavicon404(source, text, url) {
    if (favicon404Seen) return;
    favicon404Seen = true;
    expectedDiagnostics.push({
      type: 'expected-network',
      source,
      text,
      url: reportURL(url),
      reason: 'same-origin root /favicon.ico HTTP 404; browser default request, not an application asset'
    });
  }

  function isAllowedCapabilityPost(request) {
    if (String(request.method || '').toUpperCase() !== 'POST') return false;
    let parsed;
    try {
      parsed = new URL(request.url || '');
    } catch (err) {
      return false;
    }
    if (parsed.origin !== realOrigin) return false;
    if (parsed.pathname !== mock.MANAGEMENT_BASE + '/models/token') return false;
    if (parsed.search !== '' || parsed.hash !== '') return false;
    if (request.postData !== undefined && request.postData !== '') return false;
    return true;
  }

  async function writeReport() {
    const durationMs = Date.now() - started;
    const report = {
      generated_at: new Date().toISOString(),
      mode: 'realhost-readonly',
      readonly: true,
      readonly_methods: ['GET', 'HEAD', 'OPTIONS'],
      readonly_exception: {
        method: 'POST',
        path: mock.MANAGEMENT_BASE + '/models/token',
        requirements: 'same origin, exact plugin path, empty query, empty body'
      },
      fetch_interception: fetchInterceptionEnabled,
      outcome: failed === 0 ? 'completed' : 'failed',
      duration_ms: durationMs,
      chrome: chromeInfo,
      chrome_path: chromePath,
      report_dir: reportDir,
      ui_url: uiURL,
      summary: {
        total: results.length,
        passed: results.length - failed,
        failed,
        screenshots: screenshots.length,
        allowed_token_posts: allowedTokenPosts.length
      },
      results,
      screenshots,
      console_errors: consoleErrors,
      expected_diagnostics: expectedDiagnostics,
      expected_diagnostics_policy:
        'Only a same-origin root /favicon.ico HTTP 404 is exempt as a browser default non-application request; all other 404/CSP/network errors fail.',
      favicon_404_exempted: favicon404Seen,
      native_dialogs: nativeDialogs,
      api_requests: apiRequests,
      blocked_requests: blockedRequests,
      allowed_token_posts: allowedTokenPosts,
      fatal: fatal ? String(fatal.message || fatal) : null
    };
    fs.writeFileSync(path.join(reportDir, 'report.json'), JSON.stringify(report, null, 2));
    const summary = [
      'Observer real-host read-only smoke',
      'outcome: ' + report.outcome,
      'passed: ' + report.summary.passed + '/' + report.summary.total,
      'ui: ' + uiURL,
      'readonly: true (GET/HEAD/OPTIONS; only exact empty-body/query POST ' + mock.MANAGEMENT_BASE + '/models/token)',
      'token_posts: ' + allowedTokenPosts.length,
      'screenshots: ' + screenshots.length,
      'favicon_404_exempted: ' + favicon404Seen + ' (same-origin root /favicon.ico only)',
      'duration_ms: ' + durationMs,
      'report: ' + path.join(reportDir, 'report.json'),
      report.fatal ? 'fatal: ' + report.fatal : '',
      ''
    ].join('\n');
    fs.writeFileSync(path.join(reportDir, 'summary.txt'), summary);
    process.stdout.write('\n' + summary);
    if (failed > 0) process.exitCode = 1;
  }

  try {
    profileDir = fs.mkdtempSync(path.join(os.tmpdir(), 'observer-realhost-'));
    const launched = await launchChrome(chromePath, profileDir, 1280, 900);
    const cdp = await connectWebSocket(launched.wsUrl);
    chrome = { child: launched.child, cdp, stderr: launched.stderr };

    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Log.enable');
    await cdp.send('Network.enable');
    // 在 Request 阶段拦截：除精确 token POST 外，任何写请求在真正发出前就被 fail。
    cdp.on('Fetch.requestPaused', (params) => {
      const request = params.request || {};
      const method = String(request.method || '').toUpperCase();
      const url = request.url || '';
      if (/^(GET|HEAD|OPTIONS)$/.test(method) || isAllowedCapabilityPost(request)) {
        if (method === 'POST') {
          allowedTokenPosts.push({
            method,
            path: new URL(url).pathname,
            query_empty: true,
            body_empty: true
          });
        }
        cdp.send('Fetch.continueRequest', { requestId: params.requestId }).catch((err) => {
          fatal = fatal || err;
        });
        return;
      }
      blockedRequests.push({
        method,
        url: reportURL(url),
        type: params.resourceType || ''
      });
      cdp.send('Fetch.failRequest', {
        requestId: params.requestId,
        errorReason: 'BlockedByClient'
      }).catch((err) => {
        fatal = fatal || err;
      });
    });
    await cdp.send('Fetch.enable', {
      patterns: [{ urlPattern: '*', requestStage: 'Request' }]
    });
    fetchInterceptionEnabled = true;
    cdp.on('Runtime.exceptionThrown', (params) => {
      const description =
        (params.exceptionDetails && params.exceptionDetails.exception && params.exceptionDetails.exception.description) ||
        (params.exceptionDetails && params.exceptionDetails.text) ||
        'uncaught exception';
      consoleErrors.push({ type: 'exception', text: description });
    });
    cdp.on('Runtime.consoleAPICalled', (params) => {
      if (params.type !== 'error' && params.type !== 'assert') return;
      consoleErrors.push({
        type: 'console',
        text: (params.args || [])
          .map((arg) => (arg.value !== undefined ? String(arg.value) : arg.description || arg.type || ''))
          .join(' ')
      });
    });
    cdp.on('Log.entryAdded', (params) => {
      const entry = params.entry || {};
      if (entry.level !== 'error') return;
      const url = entry.url || '';
      const text = entry.text || '';
      if (isRootFaviconURL(url)) {
        if (/404|not found/i.test(text)) recordFavicon404('log', text, url);
        else pendingFaviconLogs.push({ type: 'log', source: entry.source || '', text, url });
        return;
      }
      consoleErrors.push({
        type: 'log',
        source: entry.source || '',
        text,
        url
      });
    });
    cdp.on('Network.responseReceived', (params) => {
      const response = params.response || {};
      if (response.status === 404 && isRootFaviconURL(response.url || '')) {
        recordFavicon404('network', 'HTTP 404', response.url || '');
      }
    });
    cdp.on('Network.requestWillBeSent', (params) => {
      const request = params.request || {};
      const url = request.url || '';
      if (!/^https?:/i.test(url)) return;
      apiRequests.push({
        method: request.method || '',
        url: reportURL(url),
        type: params.type || ''
      });
    });
    cdp.on('Page.javascriptDialogOpening', (params) => {
      nativeDialogs.push({ type: params.type || '', message: params.message || '' });
      cdp.send('Page.handleJavaScriptDialog', { accept: false }).catch(() => {});
    });

    await setViewport(cdp, SIZES[0]);
    await cdp.send('Page.navigate', { url: uiURL });
    await cdp.waitFor(
      '(() => {' + VISIBLE_SNIPPET +
        'return visible(document.querySelector("#login"))||visible(document.querySelector("#content"));})()',
      20000,
      '真实 host UI 外壳'
    );
    const shell = await cdp.evaluate(
      pageFn(
        'return {title:document.title||"",lang:document.documentElement.lang||"",' +
          'login:!!document.querySelector("#login"),content:!!document.querySelector("#content")};'
      )
    );
    check('真实 host UI 外壳可访问', Boolean(shell.title && shell.lang && shell.login && shell.content), JSON.stringify(shell));

    let loggedIn = false;
    if (args.managementKey) {
      await login(cdp, args.managementKey);
      await cdp.waitFor(
        '(() => {' + VISIBLE_SNIPPET +
          'const l=document.querySelector("#login");const c=document.querySelector("#content");' +
          'return !!l&&!!c&&!visible(l)&&visible(c);})()',
        20000,
        '真实 host 登录'
      );
      loggedIn = true;
      await waitRequestsLoaded(cdp);

      // native integration 会 seed 至少一条假日志；有日志才能验证“详情只点击后读取”。
      const requestRows = await cdp.evaluate(
        pageFn('return document.querySelectorAll("#content tbody tr").length;')
      );
      check('真实 host 至少有一条 seed 请求日志', requestRows >= 1, String(requestRows));
      const detailBefore = apiRequests.filter(
        (request) => /\/requests\/detail(?:[?#]|$)/.test(request.url)
      ).length;
      check('真实 host 列表渲染后未预取请求详情', detailBefore === 0, String(detailBefore));
      const firstModel = await cdp.evaluate(
        pageFn(
          VISIBLE_SNIPPET +
            'const rows=[...document.querySelectorAll("#content tbody tr")].filter(visible);' +
            'for(const row of rows){const button=[...row.querySelectorAll("button,[role=button]")].filter(visible)' +
            '.find((b)=>(b.innerText||"").trim());if(button){button.click();return (button.innerText||"").trim();}}' +
            'return null;'
        )
      );
      check('真实 host 请求日志存在可点击模型', typeof firstModel === 'string' && firstModel.length > 0, String(firstModel));
      const detailDialog = await waitDialog(cdp);
      const detailAfter = apiRequests.filter(
        (request) => /\/requests\/detail(?:[?#]|$)/.test(request.url)
      ).length;
      check('真实 host 请求详情只在点击行后读取一次', detailDialog.present && detailAfter === detailBefore + 1, JSON.stringify({ detailBefore, detailAfter }));
      await closeDialog(cdp).catch(() => {});

      await gotoPage(cdp, 'keys');
      const keyRows = await cdp.evaluate(
        pageFn(
          'return document.querySelectorAll("#content tbody tr,[data-key-id],.key-row,.card").length;'
        )
      );
      check('真实 host 至少有一个 seed 假 Key', keyRows >= 1, String(keyRows));

      // 打开 Key 对话框只读取真实权威目录；不填写、不保存。
      const tokenPostsBefore = allowedTokenPosts.length;
      await clickText(cdp, '新建 Key', { selector: '#content button, button' });
      const keyDialog = await waitDialog(cdp);
      check('真实 host 可打开 Key 对话框', keyDialog.present);
      await waitModelDirectory(cdp);
      const picker = await modelPickerState(cdp);
      check(
        '真实 host Key 对话框发现真实模型',
        picker.present && picker.warningVisible === false && picker.items.length >= 1,
        JSON.stringify({ present: picker.present, warning: picker.warning, count: picker.items.length, text: picker.text.slice(0, 240) })
      );
      check(
        '真实 host 模型发现使用一次性 capability',
        allowedTokenPosts.length === tokenPostsBefore + 1,
        JSON.stringify(allowedTokenPosts.slice(tokenPostsBefore))
      );
      await closeDialog(cdp).catch(() => {});
      check('真实 host Key 对话框只读关闭且未保存', !blockedRequests.some((request) => /\/keys/.test(request.url)), JSON.stringify(blockedRequests));

      await gotoPage(cdp, 'settings');
      check('真实 host 设置页面可读取', /捕获|保留|价格|压缩|设置/.test(await text(cdp, '#content')));

      // native browser_test 固定要求该代表截图存在；矩阵之前先保存。
      await setViewport(cdp, SIZES[0]);
      await applyTheme(cdp, 'light');
      await gotoPage(cdp, 'requests');
      await waitRequestsLoaded(cdp);
      await settle(cdp);
      const representative = await capture(
        cdp,
        path.join(reportDir, 'screenshots', 'realhost-smoke.png'),
        true
      );
      check('真实 host 代表截图 realhost-smoke.png 非空', representative > 1000, String(representative));

      // 三页 × desktop/mobile × light/dark = 12 张；总计 13 张。
      const themes = ['light', 'dark'];
      for (const size of SIZES) {
        await setViewport(cdp, size);
        for (const theme of themes) {
          await applyTheme(cdp, theme);
          for (const page of PAGES) {
            await gotoPage(cdp, page.key);
            if (page.key === 'requests') await waitRequestsLoaded(cdp);
            await settle(cdp);
            const bytes = await capture(
              cdp,
              path.join(
                reportDir,
                'screenshots',
                'realhost-' + size.width + '-' + theme + '-' + page.key + '.png'
              ),
              true
            );
            check(
              '真实 host 截图 ' + size.width + '/' + theme + '/' + page.key,
              bytes > 1000,
              String(bytes)
            );
          }
        }
      }
      await cdp.send('Emulation.clearDeviceMetricsOverride');
      await cdp.send('Emulation.setEmulatedMedia', { features: [] });
      const matrixShots = screenshots.filter((shot) =>
        /realhost-(1280|390)-(light|dark)-(requests|keys|settings)\.png$/.test(shot.file)
      );
      check(
        '真实 host 截图：12 张矩阵 + realhost-smoke.png = 13',
        screenshots.length === 13 &&
          matrixShots.length === 12 &&
          screenshots.some((shot) => shot.file.endsWith('screenshots/realhost-smoke.png')),
        JSON.stringify(screenshots.map((shot) => shot.file))
      );
    } else {
      check('未提供 OBSERVER_MANAGEMENT_KEY 时只验证公开外壳', true);
      await settle(cdp);
      const bytes = await capture(cdp, path.join(reportDir, 'screenshots', 'realhost-smoke.png'), false);
      check('真实 host smoke 截图非空', bytes > 1000, String(bytes));
    }

    const writes = apiRequests.filter((request) => !/^(GET|HEAD|OPTIONS)$/i.test(request.method));
    const illegalWrites = writes.filter((request) => {
      let parsed;
      try {
        parsed = new URL(request.url);
      } catch (err) {
        return true;
      }
      return !(
        String(request.method).toUpperCase() === 'POST' &&
        parsed.origin === realOrigin &&
        parsed.pathname === mock.MANAGEMENT_BASE + '/models/token' &&
        parsed.search === ''
      );
    });
    check(
      '真实 host 只放行精确的 /models/token POST',
      fetchInterceptionEnabled === true &&
        blockedRequests.length === 0 &&
        illegalWrites.length === 0 &&
        allowedTokenPosts.every(
          (request) =>
            request.method === 'POST' &&
            request.path === mock.MANAGEMENT_BASE + '/models/token' &&
            request.query_empty === true &&
            request.body_empty === true
        ),
      JSON.stringify({ blocked: blockedRequests, illegal: illegalWrites, allowed: allowedTokenPosts })
    );
    check(
      '真实 host 所有其他写请求均在发送前被拦截',
      blockedRequests.length === 0 ? illegalWrites.length === 0 : writes.length === 0,
      JSON.stringify({ blocked: blockedRequests, writes })
    );
    const methods = [...new Set(apiRequests.map((request) => request.method))].sort();
    check(
      '真实 host 方法集合仅为只读 + capability POST',
      methods.every((method) => /^(GET|HEAD|OPTIONS|POST)$/.test(method)) &&
        (loggedIn ? allowedTokenPosts.length > 0 : writes.length === 0),
      JSON.stringify(methods)
    );
    check(
      '真实 host 报告不含管理密钥或 capability token',
      apiRequests.every((request) => !request.url.includes(args.managementKey || '\u0000')) &&
        !JSON.stringify({ apiRequests, allowedTokenPosts, blockedRequests }).includes(args.managementKey || '\u0000'),
      JSON.stringify(apiRequests.slice(0, 5))
    );
    if (!favicon404Seen) consoleErrors.push(...pendingFaviconLogs);
    check('真实 host 正常流程无 console/page 错误', consoleErrors.length === 0, JSON.stringify(consoleErrors.slice(0, 8)));
  } catch (err) {
    fatal = err;
    check('真实 host 只读 smoke 执行完成', false, err && err.stack ? err.stack.split('\n').slice(0, 4).join(' | ') : String(err));
  } finally {
    if (chrome) await stopChrome({ child: chrome.child, cdp: chrome.cdp });
    if (profileDir && !keepProfile) {
      try {
        fs.rmSync(profileDir, { recursive: true, force: true });
      } catch (err) {
        /* ignore */
      }
    }
    await writeReport();
  }
}

/* -------------------------------------------------------------------------- */
/* Main                                                                       */
/* -------------------------------------------------------------------------- */

async function main() {
  const args = parseArgs(process.argv.slice(2));
  if (args.baseUrl) {
    await runRealHostSmoke(args);
    return;
  }
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
      'GET /requests/detail',
      'GET /summary',
      'GET /keys',
      'POST /keys',
      'PUT /keys',
      'POST /keys/rotate',
      'POST /keys/reset',
      'POST /keys/recover',
      'DELETE /keys',
      'GET /settings',
      'PUT /settings',
      'POST /prices/refresh',
      'POST /compact',
      'POST /models/token'
    ];
    const missing = expectedPatterns.filter((pattern) => !patterns.has(pattern));
    check('UI 调用了预期管理 API', missing.length === 0, 'missing=' + JSON.stringify(missing) + '; seen=' + JSON.stringify([...patterns].sort()));

    const catalogCallsAll = server.calls.filter((call) => call.kind === 'catalog');
    const tokenCallsAll = server.calls.filter((call) => call.method === 'POST' && call.path.endsWith('/models/token'));
    check(
      'UI 只通过精确 /v1/models + 一次性 capability 发现模型',
      catalogCallsAll.length > 0 &&
        catalogCallsAll.every(
          (call) => call.path === mock.CATALOG_PATH && call.authorized === true && Object.keys(call.query).length === 0
        ) &&
        !server.calls.some((call) => call.path.includes('/auth-files')),
      JSON.stringify(catalogCallsAll.map((call) => ({ path: call.path, status: call.status, stage: call.capability_stage })))
    );
    check(
      '每次目录读取前都有空 body/query 的 token 签发',
      catalogCallsAll.length > 0 &&
        tokenCallsAll.length >= catalogCallsAll.length &&
        tokenCallsAll.every(
          (call) =>
            call.authorized === true &&
            call.body_bytes === 0 &&
            Object.keys(call.query).length === 0 &&
            call.request_body === undefined
        ) &&
        catalogCallsAll.every((catalog) =>
          tokenCallsAll.some((token) => token.seq < catalog.seq)
        ),
      JSON.stringify({
        tokens: tokenCallsAll.map((call) => ({ seq: call.seq, status: call.status })),
        catalogs: catalogCallsAll.map((call) => ({ seq: call.seq, status: call.status }))
      })
    );
    check(
      '目录/凭据不出现在 URL 或调用日志',
      server.calls.every(
        (call) =>
          !('authorization' in call) &&
          !JSON.stringify(call).includes(mock.DEFAULT_MANAGEMENT_KEY) &&
          !call.path.includes('Bearer')
      ),
      JSON.stringify(catalogCallsAll.slice(0, 3))
    );

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
