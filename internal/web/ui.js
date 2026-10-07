/*
 * Observer 管理面板逻辑。
 *
 * 设计约束（见 CONTRACT.md「Observer frontend slice」）：
 * - 无构建步骤、无第三方依赖；纯同源 fetch。
 * - 管理密钥只存在于 password 输入框与内存，绝不写入任何浏览器持久化存储、URL 或控制台。
 * - 所有服务端文本一律通过 textContent 渲染，禁止任何 HTML 字符串注入与动态代码求值。
 * - 列表刷新绝不触发请求体读取；请求体按需懒加载并要求二次确认，关闭时清空。
 * - 主题跟随同源父级 documentElement 的 data-theme，跨域或独立打开时回退系统偏好。
 */
(function () {
  'use strict';

  var PLUGIN_ID = 'cliproxyapi-observer';
  var DEFAULT_API_BASE = '/v0/management/plugins/' + PLUGIN_ID;
  var ASSET_SUFFIX = /\/(?:ui(?:\.html)?|ui\.js|ui\.css)\/?$/i;
  var SEGMENT_RESOURCE = '/resource/plugins/';
  var SEGMENT_MANAGEMENT = '/management/plugins/';

  var REQUEST_PAGE_LIMIT = 50;
  var RANGE_MS = {
    '24h': 24 * 60 * 60 * 1000,
    '7d': 7 * 24 * 60 * 60 * 1000,
    '30d': 30 * 24 * 60 * 60 * 1000
  };
  var DEFAULT_RANGE = '24h';
  var EXACT_ACCOUNTING = 'exact';

  // 主题令牌白名单：仅同步这些视觉变量，绝不读取父级凭据 / DOM 内容。
  var THEME_TOKENS = [
    '--bg-secondary', '--bg-primary', '--bg-tertiary', '--bg-hover', '--bg-quinary',
    '--text-primary', '--text-secondary', '--text-tertiary',
    '--border-color', '--border-primary', '--border-hover',
    '--primary-color', '--primary-hover', '--primary-active', '--primary-contrast',
    '--success-color', '--warning-color', '--error-color',
    '--warning-bg', '--warning-border',
    '--success-badge-bg', '--success-badge-text',
    '--failure-badge-bg', '--failure-badge-text',
    '--count-badge-bg', '--count-badge-text',
    '--shadow', '--shadow-lg', '--viz-success', '--viz-failure'
  ];

  // ---------------------------------------------------------------------------
  // 纯函数层（可被 Node 测试直接引用）
  // ---------------------------------------------------------------------------

  function toStr(value) {
    return value === null || value === undefined ? '' : String(value);
  }

  function counter(source, key) {
    if (!source || typeof source !== 'object') return 0;
    var n = Number(source[key]);
    return isFinite(n) ? n : 0;
  }

  function normalizeTheme(value) {
    var v = toStr(value).trim().toLowerCase();
    if (v === 'dark') return 'dark';
    if (v === 'white') return 'white';
    return '';
  }

  // 主题来源：同源嵌入时严格跟随父级 data-theme，否则回退系统偏好。
  function resolveThemeSource(input) {
    var i = input || {};
    if (i.framed && i.sameOrigin) return normalizeTheme(i.parentTheme);
    return i.prefersDark ? 'dark' : '';
  }

  function isLoopbackHost(host) {
    var h = toStr(host).trim().toLowerCase();
    return h === 'localhost' || h === '127.0.0.1' || h === '::1' || h === '[::1]';
  }

  // 管理密钥只允许经 HTTPS 或本机回环 HTTP 发送。
  function isAllowedTransport(loc) {
    var l = loc || {};
    var protocol = toStr(l.protocol).toLowerCase();
    if (protocol === 'https:') return true;
    return protocol === 'http:' && isLoopbackHost(l.hostname);
  }

  // 从当前资源路径推导管理 API 前缀：保留反向代理前缀与 v0/v8 版本段。
  function deriveApiBase(pathname) {
    var p = toStr(pathname);
    if (!p) return DEFAULT_API_BASE;
    p = p.replace(ASSET_SUFFIX, '');
    p = p.replace(/\/+$/, '');
    var idx = p.indexOf(SEGMENT_RESOURCE);
    if (idx >= 0) {
      p = p.slice(0, idx) + SEGMENT_MANAGEMENT + p.slice(idx + SEGMENT_RESOURCE.length);
    }
    if (p.charAt(0) !== '/' || p.indexOf(SEGMENT_MANAGEMENT) < 0) return DEFAULT_API_BASE;
    return p;
  }

  // 构造同源相对请求地址；拒绝绝对 URL / 协议相对 URL 以防 SSRF 式跳转。
  function buildApiUrl(base, path, params) {
    var b = toStr(base).trim();
    if (!b || b.charAt(0) !== '/' || b.indexOf('//') === 0) return '';
    if (/^[a-z][a-z0-9+.-]*:/i.test(b)) return '';
    var suffix = toStr(path).replace(/^\/+/, '');
    if (!suffix) return '';
    var url = b.replace(/\/+$/, '') + '/' + suffix;
    var query = [];
    if (params) {
      var keys = Object.keys(params);
      for (var i = 0; i < keys.length; i++) {
        var value = params[keys[i]];
        if (value === undefined || value === null || value === '') continue;
        query.push(encodeURIComponent(keys[i]) + '=' + encodeURIComponent(String(value)));
      }
    }
    return query.length ? url + '?' + query.join('&') : url;
  }

  // RFC3339 的整分钟边界，与服务端分钟对齐聚合一致。
  function rangeBounds(kind, nowMs) {
    var ms = Object.prototype.hasOwnProperty.call(RANGE_MS, kind) ? RANGE_MS[kind] : RANGE_MS[DEFAULT_RANGE];
    var now = typeof nowMs === 'number' && isFinite(nowMs) ? nowMs : Date.now();
    var to = Math.floor(now / 60000) * 60000;
    var from = to - ms;
    return { from: new Date(from).toISOString(), to: new Date(to).toISOString() };
  }

  function formatInt(value) {
    if (value === null || value === undefined || value === '') return '—';
    var n = Number(value);
    if (!isFinite(n)) return '—';
    return Math.round(n).toLocaleString('zh-CN');
  }

  function formatCompact(value) {
    if (value === null || value === undefined || value === '') return '—';
    var n = Number(value);
    if (!isFinite(n)) return '—';
    var abs = Math.abs(n);
    if (abs >= 1e9) return (n / 1e9).toFixed(2) + 'B';
    if (abs >= 1e6) return (n / 1e6).toFixed(2) + 'M';
    if (abs >= 1e4) return (n / 1e3).toFixed(1) + 'K';
    return String(Math.round(n));
  }

  function formatCost(value) {
    if (value === null || value === undefined || value === '') return '未定价';
    var n = Number(value);
    if (!isFinite(n)) return '未定价';
    if (n === 0) return '$0.0000';
    if (Math.abs(n) < 0.0001) return '<$0.0001';
    return '$' + n.toFixed(4);
  }

  function formatLatency(ns) {
    var n = Number(ns);
    if (!isFinite(n) || n <= 0) return '—';
    var ms = n / 1e6;
    if (ms >= 1000) return (ms / 1000).toFixed(2) + ' s';
    if (ms >= 100) return Math.round(ms) + ' ms';
    return ms.toFixed(1) + ' ms';
  }

  function formatTps(value) {
    var n = Number(value);
    if (value === null || value === undefined || !isFinite(n) || n <= 0) return '—';
    return n.toFixed(1) + ' tok/s';
  }

  function formatRate(value) {
    var n = Number(value);
    if (!isFinite(n)) return '—';
    return (n * 100).toFixed(1) + '%';
  }

  function formatBytes(value) {
    var n = Number(value);
    if (!isFinite(n) || n < 0) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
  }

  function formatDuration(seconds) {
    var n = Number(seconds);
    if (!isFinite(n) || n <= 0) return '—';
    if (n % 86400 === 0) return n / 86400 + ' 天';
    if (n % 3600 === 0) return n / 3600 + ' 小时';
    if (n % 60 === 0) return n / 60 + ' 分钟';
    return n + ' 秒';
  }

  function formatTime(value) {
    if (!value) return '—';
    var d = value instanceof Date ? value : new Date(value);
    if (isNaN(d.getTime())) return '—';
    var pad = function (x) { return x < 10 ? '0' + x : String(x); };
    return (
      pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds())
    );
  }

  function nullCost(source) {
    if (!source || typeof source !== 'object') return null;
    var v = source.cost_usd;
    if (v === undefined || v === null || v === '') return null;
    var n = Number(v);
    return isFinite(n) ? n : null;
  }

  function buildOverview(source) {
    var requests = counter(source, 'requests');
    var failed = counter(source, 'failed_requests');
    var input = counter(source, 'input_tokens');
    var output = counter(source, 'output_tokens');
    var total = counter(source, 'total_tokens');
    var cacheRead = counter(source, 'cache_read_tokens');
    var cacheCreation = counter(source, 'cache_creation_tokens');
    var cacheHits = counter(source, 'cache_hits');
    var latency = counter(source, 'latency_ns');
    var latencySamples = counter(source, 'latency_samples');
    var ttft = counter(source, 'ttft_ns');
    var ttftSamples = counter(source, 'ttft_samples');
    var unpriced = counter(source, 'unpriced_requests');
    var cost = nullCost(source);
    var hasData = source !== null && source !== undefined;

    return [
      {
        key: 'requests',
        label: '请求数',
        value: hasData ? formatInt(requests) : '—',
        sub: hasData ? '区间内累计' : ''
      },
      {
        key: 'failed',
        label: '失败请求',
        value: hasData ? formatInt(failed) : '—',
        sub: hasData ? '失败率 ' + (requests > 0 ? formatRate(failed / requests) : '—') : '',
        warn: failed > 0
      },
      {
        key: 'tokens',
        label: 'Token 总量',
        value: hasData ? formatCompact(total) : '—',
        sub: hasData ? '输入 ' + formatCompact(input) + ' / 输出 ' + formatCompact(output) : ''
      },
      {
        key: 'cache',
        label: '缓存 Token',
        value: hasData ? formatCompact(cacheRead + cacheCreation) : '—',
        sub: hasData
          ? '读 ' + formatCompact(cacheRead) + ' / 写 ' + formatCompact(cacheCreation) + ' · 命中 ' + formatInt(cacheHits)
          : ''
      },
      {
        key: 'latency',
        label: '平均延迟',
        value: hasData && latencySamples > 0 ? formatLatency(latency / latencySamples) : '—',
        sub: hasData ? (latencySamples > 0 ? formatInt(latencySamples) + ' 次采样' : '无采样') : ''
      },
      {
        key: 'ttft',
        label: '平均 TTFT',
        value: hasData && ttftSamples > 0 ? formatLatency(ttft / ttftSamples) : '—',
        sub: hasData ? (ttftSamples > 0 ? formatInt(ttftSamples) + ' 次采样' : '无采样') : ''
      },
      {
        key: 'cost',
        label: '成本 (USD)',
        value: hasData ? formatCost(cost) : '—',
        sub: hasData
          ? (unpriced > 0
              ? '未定价 ' + formatInt(unpriced) + ' 条'
              : (cost === null ? '缺少价格配置' : '按已记录价格'))
          : '',
        warn: hasData && (unpriced > 0 || cost === null)
      }
    ];
  }

  function buildGroupRows(groups) {
    var list = Array.isArray(groups) ? groups.slice() : [];
    list.sort(function (a, b) { return counter(b, 'requests') - counter(a, 'requests'); });
    var rows = [];
    for (var i = 0; i < list.length; i++) {
      var g = list[i];
      if (!g || typeof g !== 'object' || Array.isArray(g)) continue;
      rows.push({
        provider: toStr(g.provider) || '—',
        model: toStr(g.model) || '—',
        requests: formatInt(counter(g, 'requests')),
        failed: formatInt(counter(g, 'failed_requests')),
        input: formatCompact(counter(g, 'input_tokens')),
        output: formatCompact(counter(g, 'output_tokens')),
        cacheRead: formatCompact(counter(g, 'cache_read_tokens')),
        cacheCreation: formatCompact(counter(g, 'cache_creation_tokens')),
        cost: formatCost(nullCost(g)),
        costUnpriced: nullCost(g) === null && counter(g, 'requests') > 0,
        failedRaw: counter(g, 'failed_requests')
      });
    }
    return rows;
  }

  function buildRequestRows(items) {
    var list = Array.isArray(items) ? items : [];
    var rows = [];
    for (var i = 0; i < list.length; i++) {
      var it = list[i] || {};
      var cacheRead = counter(it, 'cache_read_tokens');
      var cacheCreation = counter(it, 'cache_creation_tokens');
      var cost = nullCost(it);
      var quality = toStr(it.accounting_quality).trim();
      var tps = it.tps === null || it.tps === undefined ? null : Number(it.tps);
      rows.push({
        requestId: toStr(it.request_id),
        time: formatTime(it.time),
        model: toStr(it.model) || '—',
        alias: toStr(it.alias),
        provider: toStr(it.provider) || '—',
        tokens:
          formatCompact(counter(it, 'total_tokens')) +
          '（入 ' + formatCompact(counter(it, 'input_tokens')) +
          ' / 出 ' + formatCompact(counter(it, 'output_tokens')) +
          ' / 缓存 ' + formatCompact(cacheRead + cacheCreation) + '）',
        latency: formatLatency(it.latency_ns),
        ttft: formatLatency(it.ttft_ns),
        tps: formatTps(tps),
        cost: formatCost(cost),
        costUnpriced: cost === null,
        failed: it.failed === true,
        statusText: it.failed === true
          ? ('失败' + (it.failure_status ? ' ' + it.failure_status : ''))
          : '成功',
        bodyAvailable: it.body_available === true,
        stream: it.stream === true,
        // 仅在核算结果含糊（无 TPS / 无价格）时暴露原始核算质量，避免误导。
        quality: quality && quality !== EXACT_ACCOUNTING && (tps === null || cost === null) ? quality : ''
      });
    }
    return rows;
  }

  function normalizeSettings(raw) {
    var s = raw && typeof raw === 'object' ? raw : {};
    return {
      captureBodies: s.capture_bodies === true,
      bodyRetentionSeconds: Number(s.body_retention_seconds),
      requestRetentionSeconds: Number(s.request_retention_seconds),
      statsRetentionDays: Number(s.stats_retention_days),
      maxBodyBytes: Number(s.max_body_bytes),
      maxBodyStorageBytes: Number(s.max_body_storage_bytes)
    };
  }

  function buildHealthWarning(status) {
    if (!status || typeof status !== 'object') return '';
    var droppedUsage = counter(status, 'dropped_usage');
    var droppedBodies = counter(status, 'dropped_bodies');
    var writeErrors = counter(status, 'write_errors');
    if (droppedUsage === 0 && droppedBodies === 0 && writeErrors === 0) return '';
    return (
      '观测数据存在丢失：丢弃用量 ' + droppedUsage + ' 条、丢弃请求体 ' + droppedBodies +
      ' 条、写入失败 ' + writeErrors + ' 次。可能因写入队列已满或磁盘异常，请检查运行环境。'
    );
  }

  var BODY_IDLE = { phase: 'idle', requestId: '', content: '', redacted: false, meta: null, error: '' };

  // 请求体弹窗状态机：关闭即清空，过期/错配响应直接丢弃。
  function bodyViewerReduce(state, action) {
    var s = state || BODY_IDLE;
    var a = action || {};
    switch (a.type) {
      case 'open':
        return { phase: 'confirm', requestId: toStr(a.requestId), content: '', redacted: false, meta: a.meta || null, error: '' };
      case 'confirm':
        if (s.phase !== 'confirm' || toStr(a.requestId) !== s.requestId) return s;
        return { phase: 'loading', requestId: s.requestId, content: '', redacted: false, meta: s.meta, error: '' };
      case 'loaded':
        if (s.phase !== 'loading' || toStr(a.requestId) !== s.requestId) return s;
        return {
          phase: 'loaded',
          requestId: s.requestId,
          content: toStr(a.content),
          redacted: a.redacted === true,
          meta: a.meta || s.meta,
          error: ''
        };
      case 'error':
        if (s.phase !== 'loading' || toStr(a.requestId) !== s.requestId) return s;
        return { phase: 'error', requestId: s.requestId, content: '', redacted: false, meta: s.meta, error: toStr(a.message) };
      case 'close':
        return BODY_IDLE;
      default:
        return s;
    }
  }

  // 单调递增令牌：只有最新一次请求的结果会被采纳（过期响应守卫）。
  function createGate() {
    var seq = 0;
    return {
      begin: function () { seq += 1; return seq; },
      current: function (token) { return token === seq; },
      invalidate: function () { seq += 1; }
    };
  }

  // ---------------------------------------------------------------------------
  // DOM 渲染层（只用 textContent / createElement，绝不拼接 HTML 字符串）
  // ---------------------------------------------------------------------------

  function setText(node, value) {
    if (!node) return;
    node.textContent = value === null || value === undefined ? '' : String(value);
  }

  function clearChildren(node) {
    if (!node) return;
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function cell(doc, cls, text) {
    var td = doc.createElement('td');
    if (cls) td.className = cls;
    setText(td, text);
    return td;
  }

  function emptyRow(doc, colSpan, message) {
    var tr = doc.createElement('tr');
    tr.className = 'empty-row';
    var td = doc.createElement('td');
    td.colSpan = colSpan;
    setText(td, message);
    tr.appendChild(td);
    return tr;
  }

  function renderOverview(doc, container, cards) {
    clearChildren(container);
    var list = Array.isArray(cards) ? cards : [];
    for (var i = 0; i < list.length; i++) {
      var c = list[i];
      var tile = doc.createElement('div');
      tile.className = 'metric';
      tile.setAttribute('data-metric', toStr(c.key));
      var label = doc.createElement('div');
      label.className = 'metric-label';
      setText(label, c.label);
      var value = doc.createElement('div');
      value.className = 'metric-value';
      setText(value, c.value);
      tile.appendChild(label);
      tile.appendChild(value);
      if (c.sub) {
        var sub = doc.createElement('div');
        sub.className = 'metric-sub' + (c.warn ? ' is-warning' : '');
        setText(sub, c.sub);
        tile.appendChild(sub);
      }
      container.appendChild(tile);
    }
  }

  function renderGroupsTable(doc, tbody, groups) {
    clearChildren(tbody);
    var rows = buildGroupRows(groups);
    if (!rows.length) {
      tbody.appendChild(emptyRow(doc, 9, '暂无数据'));
      return rows;
    }
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var tr = doc.createElement('tr');
      tr.appendChild(cell(doc, '', r.provider));
      tr.appendChild(cell(doc, '', r.model));
      tr.appendChild(cell(doc, 'num', r.requests));
      tr.appendChild(cell(doc, 'num col-secondary', r.failed));
      tr.appendChild(cell(doc, 'num col-secondary', r.input));
      tr.appendChild(cell(doc, 'num col-secondary', r.output));
      tr.appendChild(cell(doc, 'num col-secondary', r.cacheRead));
      tr.appendChild(cell(doc, 'num col-secondary', r.cacheCreation));
      tr.appendChild(cell(doc, 'num' + (r.costUnpriced ? ' warn' : ''), r.cost));
      tbody.appendChild(tr);
    }
    return rows;
  }

  function bindView(button, row, onView) {
    if (typeof button.addEventListener !== 'function' || typeof onView !== 'function') return;
    button.addEventListener('click', function () {
      onView(row.requestId, { requestId: row.requestId, model: row.model, time: row.time });
    });
  }

  function renderRequestsTable(doc, tbody, items, onView) {
    clearChildren(tbody);
    var rows = buildRequestRows(items);
    if (!rows.length) {
      tbody.appendChild(emptyRow(doc, 10, '暂无数据'));
      return rows;
    }
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var tr = doc.createElement('tr');
      tr.appendChild(cell(doc, '', r.time));

      var modelCell = doc.createElement('td');
      var wrap = doc.createElement('div');
      wrap.className = 'model-cell';
      var name = doc.createElement('span');
      setText(name, r.model);
      wrap.appendChild(name);
      if (r.alias) {
        var alias = doc.createElement('span');
        alias.className = 'model-alias';
        setText(alias, '别名 ' + r.alias);
        wrap.appendChild(alias);
      }
      if (r.quality) {
        var quality = doc.createElement('span');
        quality.className = 'model-alias';
        setText(quality, '核算 ' + r.quality);
        wrap.appendChild(quality);
      }
      if (r.stream) {
        var stream = doc.createElement('span');
        stream.className = 'model-alias';
        setText(stream, '流式');
        wrap.appendChild(stream);
      }
      modelCell.appendChild(wrap);
      tr.appendChild(modelCell);

      tr.appendChild(cell(doc, 'col-secondary', r.provider));
      tr.appendChild(cell(doc, 'num col-secondary', r.tokens));
      tr.appendChild(cell(doc, 'num', r.latency));
      tr.appendChild(cell(doc, 'num col-secondary', r.ttft));
      tr.appendChild(cell(doc, 'num col-secondary', r.tps));
      tr.appendChild(cell(doc, 'num' + (r.costUnpriced ? ' warn' : ''), r.cost));

      var statusCell = doc.createElement('td');
      var badge = doc.createElement('span');
      badge.className = 'badge ' + (r.failed ? 'badge-fail' : 'badge-ok');
      setText(badge, r.statusText);
      statusCell.appendChild(badge);
      tr.appendChild(statusCell);

      var bodyCell = doc.createElement('td');
      if (r.bodyAvailable) {
        var button = doc.createElement('button');
        button.type = 'button';
        button.className = 'btn btn-secondary btn-sm';
        button.setAttribute('data-request-id', r.requestId);
        setText(button, '查看');
        bindView(button, r, onView);
        bodyCell.appendChild(button);
      } else {
        var empty = doc.createElement('span');
        empty.className = 'muted';
        setText(empty, '未捕获');
        bodyCell.appendChild(empty);
      }
      tr.appendChild(bodyCell);

      tbody.appendChild(tr);
    }
    return rows;
  }

  function renderSettings(doc, container, settings) {
    clearChildren(container);
    var s = normalizeSettings(settings);
    var items = [
      { label: '请求体捕获', value: s.captureBodies ? '已开启' : '已关闭' },
      { label: '请求体保留', value: formatDuration(s.bodyRetentionSeconds) },
      { label: '请求记录保留', value: formatDuration(s.requestRetentionSeconds) },
      { label: '统计数据保留', value: isFinite(s.statsRetentionDays) && s.statsRetentionDays > 0 ? s.statsRetentionDays + ' 天' : '—' },
      { label: '单条请求体上限', value: formatBytes(s.maxBodyBytes) },
      { label: '请求体存储上限', value: formatBytes(s.maxBodyStorageBytes) }
    ];

    var dl = doc.createElement('dl');
    dl.className = 'settings-grid';
    for (var i = 0; i < items.length; i++) {
      var item = doc.createElement('div');
      item.className = 'settings-item';
      var dt = doc.createElement('dt');
      setText(dt, items[i].label);
      var dd = doc.createElement('dd');
      setText(dd, items[i].value);
      item.appendChild(dt);
      item.appendChild(dd);
      dl.appendChild(item);
    }
    container.appendChild(dl);

    var note = doc.createElement('p');
    note.className = 'settings-note';
    setText(
      note,
      s.captureBodies
        ? '请求体捕获已开启：最多保存 ' + formatDuration(s.bodyRetentionSeconds) + '，查看时必须二次确认，关闭弹窗即清空。'
        : '请求体捕获已关闭（capture-bodies: false）：列表仅显示元数据，请求体不会写入磁盘。如需查看，请在 config.yaml 中设置 capture-bodies: true 后重启。'
    );
    container.appendChild(note);
  }

  function renderChart(canvas, series) {
    if (!canvas || typeof canvas.getContext !== 'function') return;
    var ctx = canvas.getContext('2d');
    if (!ctx) return;

    var dpr = (typeof window !== 'undefined' && window.devicePixelRatio) || 1;
    var rect = typeof canvas.getBoundingClientRect === 'function'
      ? canvas.getBoundingClientRect()
      : { width: canvas.clientWidth || 720, height: canvas.clientHeight || 200 };
    var cssW = Math.max(180, Math.round(rect.width || canvas.clientWidth || 720));
    var cssH = Math.max(120, Math.round(rect.height || canvas.clientHeight || 200));
    canvas.width = Math.round(cssW * dpr);
    canvas.height = Math.round(cssH * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    var style = typeof getComputedStyle === 'function' ? getComputedStyle(canvas) : null;
    var read = function (name, fallback) {
      if (!style) return fallback;
      var v = style.getPropertyValue(name);
      return v && v.trim() ? v.trim() : fallback;
    };
    var borderColor = read('--border-color', '#e3e1db');
    var labelColor = read('--text-tertiary', '#a29c95');
    var okColor = read('--viz-success', '#10b981');
    var failColor = read('--viz-failure', '#c65746');

    var left = 46;
    var right = 12;
    var top = 12;
    var bottom = 22;
    var plotW = cssW - left - right;
    var plotH = cssH - top - bottom;

    var points = [];
    var list = Array.isArray(series) ? series : [];
    for (var i = 0; i < list.length; i++) {
      var p = list[i] || {};
      var t = new Date(toStr(p.time)).getTime();
      if (!isFinite(t)) continue;
      points.push({ t: t, req: counter(p, 'requests'), fail: counter(p, 'failed_requests') });
    }

    ctx.font = '11px ' + (style ? read('--font-family', 'sans-serif') : 'sans-serif');
    ctx.textBaseline = 'middle';

    if (!points.length) {
      ctx.fillStyle = labelColor;
      ctx.textAlign = 'center';
      ctx.fillText('暂无趋势数据', left + plotW / 2, top + plotH / 2);
      canvas.setAttribute('aria-label', '请求趋势图：暂无数据');
      return;
    }

    var minT = points[0].t;
    var maxT = points[0].t;
    var maxY = 0;
    var totalReq = 0;
    var totalFail = 0;
    for (var j = 0; j < points.length; j++) {
      if (points[j].t < minT) minT = points[j].t;
      if (points[j].t > maxT) maxT = points[j].t;
      if (points[j].req > maxY) maxY = points[j].req;
      if (points[j].fail > maxY) maxY = points[j].fail;
      totalReq += points[j].req;
      totalFail += points[j].fail;
    }
    if (maxT === minT) maxT = minT + 60000;
    if (maxY <= 0) maxY = 1;
    maxY = maxY * 1.1;

    var xOf = function (t) { return left + ((t - minT) / (maxT - minT)) * plotW; };
    var yOf = function (v) { return top + (1 - v / maxY) * plotH; };

    ctx.strokeStyle = borderColor;
    ctx.fillStyle = labelColor;
    ctx.lineWidth = 1;
    ctx.textAlign = 'right';
    for (var g = 0; g <= 4; g++) {
      var value = (maxY * g) / 4;
      var y = yOf(value);
      ctx.beginPath();
      ctx.moveTo(left, Math.round(y) + 0.5);
      ctx.lineTo(left + plotW, Math.round(y) + 0.5);
      ctx.stroke();
      ctx.fillText(formatCompact(value), left - 6, y);
    }

    ctx.textAlign = 'center';
    var xLabels = [points[0]];
    if (points.length > 2) xLabels.push(points[Math.floor(points.length / 2)]);
    xLabels.push(points[points.length - 1]);
    for (var k = 0; k < xLabels.length; k++) {
      var d = new Date(xLabels[k].t);
      var pad = function (x) { return x < 10 ? '0' + x : String(x); };
      ctx.fillText(pad(d.getHours()) + ':' + pad(d.getMinutes()), xOf(xLabels[k].t), top + plotH + 12);
    }

    var drawSeries = function (key, color, fillAlpha) {
      ctx.beginPath();
      for (var m = 0; m < points.length; m++) {
        var x = xOf(points[m].t);
        var y = yOf(points[m][key]);
        if (m === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      }
      if (fillAlpha) {
        ctx.save();
        ctx.lineTo(xOf(points[points.length - 1].t), top + plotH);
        ctx.lineTo(xOf(points[0].t), top + plotH);
        ctx.closePath();
        ctx.globalAlpha = fillAlpha;
        ctx.fillStyle = color;
        ctx.fill();
        ctx.restore();
        ctx.beginPath();
        for (var n = 0; n < points.length; n++) {
          var nx = xOf(points[n].t);
          var ny = yOf(points[n][key]);
          if (n === 0) ctx.moveTo(nx, ny);
          else ctx.lineTo(nx, ny);
        }
      }
      ctx.strokeStyle = color;
      ctx.lineWidth = 2;
      ctx.stroke();
    };

    drawSeries('req', okColor, 0.12);
    drawSeries('fail', failColor, 0);

    canvas.setAttribute(
      'aria-label',
      '请求趋势图：共 ' + Math.round(totalReq) + ' 次请求，' + Math.round(totalFail) +
        ' 次失败，' + points.length + ' 个采样点'
    );
  }

  // ---------------------------------------------------------------------------
  // 应用层
  // ---------------------------------------------------------------------------

  function isAbortError(err) {
    if (!err) return false;
    return err.name === 'AbortError' || err.code === 20 || err === 'aborted';
  }

  function assign(target) {
    for (var i = 1; i < arguments.length; i++) {
      var src = arguments[i];
      if (!src) continue;
      var keys = Object.keys(src);
      for (var j = 0; j < keys.length; j++) target[keys[j]] = src[keys[j]];
    }
    return target;
  }

  function optionEl(doc, value, label) {
    var option = doc.createElement('option');
    option.value = value;
    setText(option, label);
    return option;
  }

  function createThemeController(win, doc) {
    var observer = null;

    function parentDocument() {
      try {
        if (!win.parent || win.parent === win) return null;
        var pd = win.parent.document;
        return pd && pd.documentElement ? pd : null;
      } catch (err) {
        return null; // 跨域：拒绝访问父级，回退系统偏好
      }
    }

    function prefersDark() {
      try {
        return !!(win.matchMedia && win.matchMedia('(prefers-color-scheme: dark)').matches);
      } catch (err) {
        return false;
      }
    }

    // 仅同步白名单视觉令牌；绝不读取父级凭据或业务数据。
    function syncTokens(pd) {
      if (!pd || typeof win.getComputedStyle !== 'function') return;
      try {
        var parentStyle = win.getComputedStyle(pd.documentElement);
        var own = doc.documentElement.style;
        for (var i = 0; i < THEME_TOKENS.length; i++) {
          var name = THEME_TOKENS[i];
          var value = parentStyle.getPropertyValue(name);
          if (value && value.trim()) own.setProperty(name, value.trim());
          else own.removeProperty(name);
        }
      } catch (err) {
        /* 父级样式不可读时静默退回本地主题 */
      }
    }

    function apply() {
      var pd = parentDocument();
      var framed = !!(win.parent && win.parent !== win);
      var theme = resolveThemeSource({
        framed: framed,
        sameOrigin: !!pd,
        parentTheme: pd ? pd.documentElement.getAttribute('data-theme') : '',
        prefersDark: prefersDark()
      });
      if (theme) doc.documentElement.setAttribute('data-theme', theme);
      else doc.documentElement.removeAttribute('data-theme');
      syncTokens(pd);
    }

    return {
      start: function () {
        apply();
        var pd = parentDocument();
        if (pd && typeof win.MutationObserver === 'function') {
          observer = new win.MutationObserver(apply);
          observer.observe(pd.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
        }
        try {
          var media = win.matchMedia && win.matchMedia('(prefers-color-scheme: dark)');
          if (media) {
            var listener = function () { if (!parentDocument()) apply(); };
            if (media.addEventListener) media.addEventListener('change', listener);
            else if (media.addListener) media.addListener(listener);
          }
        } catch (err) {
          /* 无 matchMedia 时忽略 */
        }
      },
      refresh: apply
    };
  }

  function startDashboard(win, doc) {
    var els = {
      key: doc.getElementById('mgmt-key'),
      connect: doc.getElementById('connect'),
      refresh: doc.getElementById('refresh'),
      status: doc.getElementById('conn-status'),
      rangeLabel: doc.getElementById('range-label'),
      banner: doc.getElementById('banner'),
      provider: doc.getElementById('filter-provider'),
      model: doc.getElementById('filter-model'),
      cards: doc.getElementById('cards'),
      chart: doc.getElementById('chart'),
      trendNote: doc.getElementById('trend-note'),
      groupsBody: doc.getElementById('groups-body'),
      groupsNote: doc.getElementById('groups-note'),
      requestsBody: doc.getElementById('requests-body'),
      requestsNote: doc.getElementById('requests-note'),
      loadMore: doc.getElementById('load-more'),
      settingsBody: doc.getElementById('settings-body'),
      dialog: doc.getElementById('body-dialog'),
      bodyMeta: doc.getElementById('body-meta'),
      bodyConfirm: doc.getElementById('body-confirm'),
      bodyView: doc.getElementById('body-view'),
      bodyContent: doc.getElementById('body-content'),
      bodyReveal: doc.getElementById('body-reveal'),
      bodyClose: doc.getElementById('body-close')
    };
    if (!els.connect || !els.dialog) return;

    var loc = win.location || {};
    var apiBase = deriveApiBase(loc.pathname);
    var theme = createThemeController(win, doc);
    theme.start();

    var state = {
      key: '',
      connected: false,
      range: DEFAULT_RANGE,
      provider: '',
      model: '',
      cursor: '',
      items: [],
      hasMore: false,
      loadingMore: false,
      bounds: null,
      series: [],
      providers: {},
      models: {}
    };
    var queryGate = createGate();
    var moreGate = createGate();
    var bodyGate = createGate();
    var queryAbort = null;
    var bodyAbort = null;
    var bodyState = BODY_IDLE;

    function setStatus(text, kind) {
      setText(els.status, text);
      els.status.className = 'conn-status' + (kind ? ' is-' + kind : '');
    }

    function setBanner(message) {
      if (!els.banner) return;
      if (message) {
        setText(els.banner, message);
        els.banner.hidden = false;
      } else {
        setText(els.banner, '');
        els.banner.hidden = true;
      }
    }

    function abortQuery() {
      if (queryAbort && typeof queryAbort.abort === 'function') queryAbort.abort();
      queryAbort = null;
    }

    function abortBody() {
      if (bodyAbort && typeof bodyAbort.abort === 'function') bodyAbort.abort();
      bodyAbort = null;
    }

    function newController() {
      try {
        return typeof win.AbortController === 'function' ? new win.AbortController() : null;
      } catch (err) {
        return null;
      }
    }

    function handleAuthFailure(err) {
      state.connected = false;
      els.refresh.disabled = true;
      els.loadMore.hidden = true;
      setStatus((err && err.message) || '管理密钥无效', 'error');
      setBanner('认证失败：请确认管理密钥正确、远程管理已开启，或当前客户端在允许范围内。');
    }

    function fetchJson(path, params, signal) {
      var url = buildApiUrl(apiBase, path, params);
      if (!url) return Promise.reject(new Error('无效的接口地址'));
      if (typeof win.fetch !== 'function') return Promise.reject(new Error('浏览器不支持 fetch'));
      var headers = {};
      if (state.key) headers.Authorization = 'Bearer ' + state.key;
      var init = { method: 'GET', headers: headers, credentials: 'same-origin', cache: 'no-store' };
      if (signal) init.signal = signal;
      return win.fetch(url, init).then(function (res) {
        return res.text().then(function (text) {
          if (res.status === 401 || res.status === 403) {
            var authErr = new Error('管理密钥无效或未被授权');
            authErr.auth = true;
            authErr.status = res.status;
            throw authErr;
          }
          if (!res.ok) {
            var httpErr = new Error('请求失败 (' + res.status + ')');
            httpErr.status = res.status;
            throw httpErr;
          }
          if (!text) return {};
          try {
            return JSON.parse(text);
          } catch (parseErr) {
            var badErr = new Error('响应解析失败');
            badErr.status = res.status;
            throw badErr;
          }
        });
      });
    }

    function updateFacets(groups) {
      var list = Array.isArray(groups) ? groups : [];
      var providers = [];
      var models = [];
      for (var i = 0; i < list.length; i++) {
        var g = list[i] || {};
        var p = toStr(g.provider);
        var m = toStr(g.model);
        if (p && !state.providers[p]) { state.providers[p] = true; providers.push(p); }
        if (m && !state.models[m]) { state.models[m] = true; models.push(m); }
      }
      providers.sort();
      models.sort();
      for (var a = 0; a < providers.length; a++) els.provider.appendChild(optionEl(doc, providers[a], providers[a]));
      for (var b = 0; b < models.length; b++) els.model.appendChild(optionEl(doc, models[b], models[b]));
    }

    function applySummary(summary) {
      var s = summary && typeof summary === 'object' ? summary : {};
      renderOverview(doc, els.cards, buildOverview(s.totals));
      var count = renderGroupsTable(doc, els.groupsBody, s.groups).length;
      setText(els.groupsNote, count ? count + ' 个分组' : '');
      var series = Array.isArray(s.series) ? s.series : [];
      state.series = series;
      renderChart(els.chart, series);
      setText(els.trendNote, series.length ? series.length + ' 个采样点' : '');
      updateFacets(s.groups);
      if (s.from && s.to) {
        setText(els.rangeLabel, '范围 ' + formatTime(s.from) + ' ~ ' + formatTime(s.to));
      }
    }

    function applyRequestsPage(page, append) {
      var p = page && typeof page === 'object' ? page : {};
      var items = Array.isArray(p.items) ? p.items : [];
      state.items = append ? state.items.concat(items) : items;
      state.cursor = typeof p.next_cursor === 'string' ? p.next_cursor : '';
      state.hasMore = p.has_more === true && !!state.cursor;
      renderRequestsTable(doc, els.requestsBody, state.items, openBody);
      setText(els.requestsNote, state.items.length ? '已加载 ' + state.items.length + ' 条' : '');
      els.loadMore.hidden = !state.hasMore;
      els.loadMore.disabled = false;
    }

    function applySettings(settings) {
      renderSettings(doc, els.settingsBody, settings);
    }

    function applyHealth(status) {
      setBanner(buildHealthWarning(status));
    }

    function refreshAll() {
      if (!state.connected) return;
      abortQuery();
      moreGate.invalidate();
      var controller = newController();
      queryAbort = controller;
      var token = queryGate.begin();
      var bounds = rangeBounds(state.range, Date.now());
      state.bounds = bounds;
      state.cursor = '';
      state.hasMore = false;
      els.loadMore.hidden = true;
      setText(els.rangeLabel, '范围 ' + formatTime(bounds.from) + ' ~ ' + formatTime(bounds.to));
      setStatus('加载中…', '');

      var common = { from: bounds.from, to: bounds.to, provider: state.provider, model: state.model };
      var signal = controller ? controller.signal : undefined;
      var tasks = [
        fetchJson('summary', common, signal).then(function (d) { return { kind: 'summary', data: d }; }),
        fetchJson('requests', assign({}, common, { limit: REQUEST_PAGE_LIMIT }), signal).then(function (d) { return { kind: 'requests', data: d }; }),
        fetchJson('settings', null, signal).then(function (d) { return { kind: 'settings', data: d }; }),
        fetchJson('health', null, signal).then(function (d) { return { kind: 'health', data: d }; })
      ];

      Promise.all(tasks)
        .then(function (results) {
          if (!queryGate.current(token)) return;
          var byKind = {};
          for (var i = 0; i < results.length; i++) byKind[results[i].kind] = results[i].data;
          applySummary(byKind.summary);
          applyRequestsPage(byKind.requests, false);
          applySettings(byKind.settings);
          setStatus('已更新 ' + formatTime(new Date().toISOString()), 'ok');
          applyHealth(byKind.health);
        })
        .catch(function (err) {
          if (isAbortError(err) || !queryGate.current(token)) return;
          if (err && err.auth) { handleAuthFailure(err); return; }
          setStatus('加载失败：' + ((err && err.message) || '未知错误'), 'error');
        });
    }

    function loadMore() {
      if (!state.connected || state.loadingMore || !state.hasMore || !state.cursor || !state.bounds) return;
      state.loadingMore = true;
      els.loadMore.disabled = true;
      var token = moreGate.begin();
      var controller = newController();
      var params = {
        from: state.bounds.from,
        to: state.bounds.to,
        provider: state.provider,
        model: state.model,
        limit: REQUEST_PAGE_LIMIT,
        cursor: state.cursor
      };
      fetchJson('requests', params, controller ? controller.signal : undefined)
        .then(function (page) {
          if (!moreGate.current(token)) return;
          applyRequestsPage(page, true);
        })
        .catch(function (err) {
          if (isAbortError(err) || !moreGate.current(token)) return;
          if (err && err.auth) { handleAuthFailure(err); return; }
          setStatus('加载更多失败：' + ((err && err.message) || '未知错误'), 'error');
        })
        .then(function () {
          if (!moreGate.current(token)) return;
          state.loadingMore = false;
          els.loadMore.disabled = false;
        });
    }

    // ---- 请求体弹窗 ----

    function appendMeta(label, value) {
      var span = doc.createElement('span');
      setText(span, label + '：' + toStr(value));
      els.bodyMeta.appendChild(span);
    }

    function renderBodyDialog() {
      var s = bodyState;
      clearChildren(els.bodyMeta);
      if (s.meta && s.meta.requestId) {
        appendMeta('请求 ID', s.meta.requestId);
        if (s.meta.model) appendMeta('模型', s.meta.model);
        if (s.meta.time) appendMeta('时间', s.meta.time);
        if (s.meta.sizeBytes !== undefined && s.meta.sizeBytes !== null) appendMeta('大小', formatBytes(s.meta.sizeBytes));
        if (s.meta.expiresAt) appendMeta('过期', s.meta.expiresAt);
      } else if (s.requestId) {
        appendMeta('请求 ID', s.requestId);
      }
      if (s.redacted) appendMeta('脱敏', '是');

      var confirming = s.phase === 'confirm' || s.phase === 'loading';
      var showing = s.phase === 'loaded' || s.phase === 'error';
      els.bodyConfirm.hidden = !confirming;
      els.bodyView.hidden = !showing;
      els.bodyReveal.disabled = s.phase === 'loading';
      setText(els.bodyReveal, s.phase === 'loading' ? '加载中…' : '确认查看');

      if (s.phase === 'loaded') {
        els.bodyContent.className = 'body-content';
        setText(els.bodyContent, s.content);
      } else if (s.phase === 'error') {
        els.bodyContent.className = 'body-content dialog-error';
        setText(els.bodyContent, s.error);
      } else {
        els.bodyContent.className = 'body-content';
        setText(els.bodyContent, '');
      }
    }

    function openBody(requestId, meta) {
      if (!requestId) return;
      bodyGate.invalidate();
      abortBody();
      bodyState = bodyViewerReduce(bodyState, { type: 'open', requestId: requestId, meta: meta });
      renderBodyDialog();
      try {
        if (typeof els.dialog.showModal === 'function') {
          if (!els.dialog.open) els.dialog.showModal();
        } else if (typeof els.dialog.show === 'function') {
          els.dialog.show();
        }
      } catch (err) {
        /* 已打开的对话框保持现状 */
      }
    }

    function revealBody() {
      if (bodyState.phase !== 'confirm') return;
      var requestId = bodyState.requestId;
      bodyState = bodyViewerReduce(bodyState, { type: 'confirm', requestId: requestId });
      var token = bodyGate.begin();
      var controller = newController();
      bodyAbort = controller;
      renderBodyDialog();
      fetchJson('body', { request_id: requestId }, controller ? controller.signal : undefined)
        .then(function (detail) {
          if (!bodyGate.current(token)) return;
          var d = detail && typeof detail === 'object' ? detail : {};
          bodyState = bodyViewerReduce(bodyState, {
            type: 'loaded',
            requestId: requestId,
            content: d.body,
            redacted: d.redacted === true,
            meta: {
              requestId: toStr(d.request_id) || requestId,
              model: bodyState.meta ? bodyState.meta.model : '',
              time: bodyState.meta ? bodyState.meta.time : '',
              expiresAt: formatTime(d.expires_at),
              sizeBytes: d.size_bytes
            }
          });
          renderBodyDialog();
        })
        .catch(function (err) {
          if (isAbortError(err) || !bodyGate.current(token)) return;
          var message = '加载失败：' + ((err && err.message) || '未知错误');
          if (err && err.status === 404) message = '请求体不存在或已过期';
          if (err && err.auth) {
            handleAuthFailure(err);
            message = '管理密钥无效，无法读取请求体';
          }
          bodyState = bodyViewerReduce(bodyState, { type: 'error', requestId: requestId, message: message });
          renderBodyDialog();
        });
    }

    function clearBodyViewer() {
      bodyGate.invalidate();
      abortBody();
      bodyState = bodyViewerReduce(bodyState, { type: 'close' });
      els.bodyConfirm.hidden = false;
      els.bodyView.hidden = true;
      setText(els.bodyContent, '');
      clearChildren(els.bodyMeta);
    }

    function closeBody() {
      if (els.dialog.open && typeof els.dialog.close === 'function') {
        els.dialog.close(); // 触发 close 事件 → clearBodyViewer
      } else {
        clearBodyViewer();
      }
    }

    function connect() {
      var key = els.key && els.key.value ? els.key.value.trim() : '';
      if (!isAllowedTransport(loc)) {
        setStatus('仅允许 HTTPS 或本机回环地址发送管理密钥', 'error');
        return;
      }
      if (!key) {
        setStatus('请输入管理密钥', 'error');
        if (els.key && typeof els.key.focus === 'function') els.key.focus();
        return;
      }
      state.key = key;
      state.connected = true;
      els.refresh.disabled = false;
      setBanner('');
      refreshAll();
    }

    // ---- 事件绑定 ----

    els.connect.addEventListener('click', connect);
    els.refresh.addEventListener('click', function () { refreshAll(); });
    els.loadMore.addEventListener('click', loadMore);
    els.bodyReveal.addEventListener('click', revealBody);
    els.bodyClose.addEventListener('click', closeBody);
    els.dialog.addEventListener('close', clearBodyViewer);
    els.dialog.addEventListener('cancel', clearBodyViewer);

    var segButtons = doc.querySelectorAll ? doc.querySelectorAll('.seg-btn') : [];
    var bindSeg = function (button) {
      button.addEventListener('click', function () {
        var range = button.getAttribute('data-range');
        if (!range) return;
        state.range = range;
        for (var i = 0; i < segButtons.length; i++) {
          if (segButtons[i] === button) segButtons[i].className = 'seg-btn is-active';
          else segButtons[i].className = 'seg-btn';
        }
        if (state.connected) refreshAll();
      });
    };
    for (var s = 0; s < segButtons.length; s++) bindSeg(segButtons[s]);

    var onFilterChange = function () {
      state.provider = els.provider.value || '';
      state.model = els.model.value || '';
      if (state.connected) refreshAll();
    };
    els.provider.addEventListener('change', onFilterChange);
    els.model.addEventListener('change', onFilterChange);

    var resizeTimer = null;
    win.addEventListener('resize', function () {
      if (resizeTimer) win.clearTimeout(resizeTimer);
      resizeTimer = win.setTimeout(function () {
        if (state.connected) renderChart(els.chart, state.series || []);
      }, 150);
    });

    win.addEventListener('beforeunload', function () {
      abortQuery();
      abortBody();
    });

    if (!isAllowedTransport(loc)) {
      els.connect.disabled = true;
      els.refresh.disabled = true;
      setStatus('当前连接不安全：请使用 HTTPS 或本机回环地址', 'error');
      setBanner('出于安全考虑，管理密钥仅允许通过 HTTPS 或本机回环 HTTP 发送。请通过 HTTPS 或 127.0.0.1 访问本面板。');
    } else {
      setStatus('未连接', '');
    }

    renderChart(els.chart, []);
  }

  var api = {
    PLUGIN_ID: PLUGIN_ID,
    DEFAULT_API_BASE: DEFAULT_API_BASE,
    RANGE_MS: RANGE_MS,
    DEFAULT_RANGE: DEFAULT_RANGE,
    REQUEST_PAGE_LIMIT: REQUEST_PAGE_LIMIT,
    deriveApiBase: deriveApiBase,
    buildApiUrl: buildApiUrl,
    isAllowedTransport: isAllowedTransport,
    isLoopbackHost: isLoopbackHost,
    normalizeTheme: normalizeTheme,
    resolveThemeSource: resolveThemeSource,
    rangeBounds: rangeBounds,
    formatInt: formatInt,
    formatCompact: formatCompact,
    formatCost: formatCost,
    formatLatency: formatLatency,
    formatTps: formatTps,
    formatRate: formatRate,
    formatBytes: formatBytes,
    formatDuration: formatDuration,
    formatTime: formatTime,
    counter: counter,
    buildOverview: buildOverview,
    buildGroupRows: buildGroupRows,
    buildRequestRows: buildRequestRows,
    buildHealthWarning: buildHealthWarning,
    normalizeSettings: normalizeSettings,
    bodyViewerReduce: bodyViewerReduce,
    createGate: createGate,
    renderOverview: renderOverview,
    renderGroupsTable: renderGroupsTable,
    renderRequestsTable: renderRequestsTable,
    renderSettings: renderSettings,
    renderChart: renderChart,
    setText: setText,
    clearChildren: clearChildren
  };

  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof window !== 'undefined') window.ObserverUI = api;
  if (typeof document !== 'undefined' && document.getElementById) {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function () { startDashboard(window, document); });
    } else {
      startDashboard(window, document);
    }
  }
})();
