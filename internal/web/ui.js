/* Dependency-free dashboard. Credentials stay in memory; bodies are revealed on demand. */
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
  var EXACT_ACCOUNTING = 'complete';

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
    return i.prefersDark ? 'dark' : 'white';
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
    if (!p || !safePath(p)) return DEFAULT_API_BASE;
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
    if (!safePath(b)) return '';
    if (/^[a-z][a-z0-9+.-]*:/i.test(b)) return '';
    if (/^\/\/|\\/.test(toStr(path))) return '';
    var suffix = toStr(path).replace(/^\/+/, '');
    if (!/^[a-z]+$/.test(suffix)) return '';
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

  function safePath(value) {
    return /^\/(?!\/)/.test(value) && !/[\\?#\s]/.test(value) &&
      !/%(?:2f|5c|2e)/i.test(value) && !/(?:^|\/)\.\.?(?:\/|$)/.test(value);
  }

  // Keep the current minute visible; summary alignment belongs to the server.
  function rangeBounds(kind, nowMs) {
    var ms = Object.prototype.hasOwnProperty.call(RANGE_MS, kind) ? RANGE_MS[kind] : RANGE_MS[DEFAULT_RANGE];
    var now = typeof nowMs === 'number' && isFinite(nowMs) ? nowMs : Date.now();
    var to = now;
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

  function formatRangeTime(value) {
    if (!value) return '—';
    var d = value instanceof Date ? value : new Date(value);
    if (isNaN(d.getTime())) return '—';
    return d.getFullYear() + '-' + formatTime(d);
  }

  function pad2(value) {
    return value < 10 ? '0' + value : String(value);
  }

  // datetime-local 没有时区；按浏览器本地日历解释，再转成 UTC ISO。
  function localInputToIso(value) {
    var text = toStr(value).trim();
    var match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,3}))?)?$/.exec(text);
    if (!match) return '';
    var year = Number(match[1]);
    var month = Number(match[2]);
    var day = Number(match[3]);
    var hour = Number(match[4]);
    var minute = Number(match[5]);
    var second = match[6] === undefined ? 0 : Number(match[6]);
    var milli = match[7] === undefined ? 0 : Number((match[7] + '00').slice(0, 3));
    var date = new Date(year, month - 1, day, hour, minute, second, milli);
    if (isNaN(date.getTime()) || date.getFullYear() !== year || date.getMonth() !== month - 1 ||
        date.getDate() !== day || date.getHours() !== hour || date.getMinutes() !== minute ||
        date.getSeconds() !== second || date.getMilliseconds() !== milli) return '';
    return date.toISOString();
  }

  function isoToLocalInput(value) {
    var date = value instanceof Date ? value : new Date(value);
    if (isNaN(date.getTime())) return '';
    var base = date.getFullYear() + '-' + pad2(date.getMonth() + 1) + '-' + pad2(date.getDate()) +
      'T' + pad2(date.getHours()) + ':' + pad2(date.getMinutes()) + ':' + pad2(date.getSeconds());
    var fraction = date.getMilliseconds();
    return fraction ? base + '.' + (fraction < 10 ? '00' : fraction < 100 ? '0' : '') + fraction : base;
  }

  // 草稿必须两端都有且严格开始早于结束；非法或被日历归一化的值不产生查询范围。
  function draftRange(startValue, endValue) {
    var from = localInputToIso(startValue);
    var to = localInputToIso(endValue);
    if (!from || !to) return { ok: false, message: '请填写有效的开始和结束时间' };
    if (!(Date.parse(from) < Date.parse(to))) return { ok: false, message: '开始时间必须早于结束时间' };
    return { ok: true, from: from, to: to, message: '' };
  }

  function nonnegativeInteger(value) {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
  }

  // 仅完整且自洽的互斥分桶允许计算缓存读占比；含糊输入不从旧 cached_tokens 推断。
  function cacheReadShare(item) {
    var it = item || {};
    var unsupported = { text: '—', title: '核算质量不支持缓存读占比' };
    if (toStr(it.accounting_quality).trim() !== EXACT_ACCOUNTING) return unsupported;
    var input = it.input_tokens;
    var read = it.cache_read_tokens;
    var write = it.cache_creation_tokens;
    var uncached = it.uncached_input_tokens;
    if (!nonnegativeInteger(input) || !nonnegativeInteger(read) || !nonnegativeInteger(write) ||
        !nonnegativeInteger(uncached) || input <= 0 || input < read || input - read < write ||
        input - read - write !== uncached) return unsupported;
    return {
      text: formatRate(read / input),
      title: '缓存读占比 = cache_read_tokens / input_tokens（' + formatInt(read) + '/' + formatInt(input) + '）'
    };
  }

  function requestHitRate(source) {
    var requests = counter(source, 'requests');
    var hits = counter(source, 'cache_hits');
    if (!(requests > 0)) return { text: '—', title: '请求命中率 = cache_hits / requests（无请求）' };
    return {
      text: formatRate(hits / requests),
      title: '请求命中率 = cache_hits / requests（' + formatInt(hits) + '/' + formatInt(requests) + '；缓存读 Token > 0）'
    };
  }

  function truncateText(value, maxLen) {
    var s = toStr(value);
    var limit = maxLen || 12;
    return s.length > limit ? s.slice(0, limit) + '…' : s;
  }

  function nullCost(source) {
    if (!source || typeof source !== 'object') return null;
    var v = source.cost_usd;
    if (v === undefined || v === null || v === '') return null;
    var n = Number(v);
    return isFinite(n) ? n : null;
  }

  function aggregateCost(source) {
    var requests = counter(source, 'requests');
    var unknown = counter(source, 'unpriced_requests');
    if (requests > 0 && unknown >= requests) return '未定价';
    var value = formatCost(nullCost(source));
    return unknown > 0 ? value + ' 已定价小计；未定价 ' + formatInt(unknown) + ' 条' : value;
  }

  function validateBodyDetail(detail, requestId) {
    if (!detail || detail.request_id !== requestId || typeof detail.body !== 'string') {
      throw new Error('请求体响应不匹配或格式无效');
    }
    return detail;
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
          ? '读 ' + formatCompact(cacheRead) + ' / 写 ' + formatCompact(cacheCreation) +
            ' · 请求命中率 ' + (requests > 0 ? formatRate(cacheHits / requests) : '—') +
            '（' + formatInt(cacheHits) + '/' + formatInt(requests) + '；缓存读 Token > 0）'
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
        value: hasData ? (requests > 0 && unpriced >= requests ? '未定价' : formatCost(cost)) : '—',
        sub: hasData
          ? (unpriced > 0
              ? (unpriced < requests ? '已定价小计；' : '') + '未定价 ' + formatInt(unpriced) + ' 条'
              : (cost === null ? '缺少价格配置' : '按当前生效价格'))
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
        hitRate: requestHitRate(g).text,
        hitTitle: requestHitRate(g).title,
        cost: aggregateCost(g),
        costUnpriced: nullCost(g) === null || counter(g, 'unpriced_requests') > 0,
        failedRaw: counter(g, 'failed_requests')
      });
    }
    return rows;
  }

  function buildRequestRows(items, credentials) {
    var list = Array.isArray(items) ? items : [];
    var rows = [];
    for (var i = 0; i < list.length; i++) {
      var it = list[i] || {};
      var cacheRead = counter(it, 'cache_read_tokens');
      var cacheCreation = counter(it, 'cache_creation_tokens');
      var share = cacheReadShare(it);
      var cost = nullCost(it);
      var quality = toStr(it.accounting_quality).trim();
      var tps = it.tps === null || it.tps === undefined ? null : Number(it.tps);
      rows.push({
        requestId: toStr(it.request_id),
        time: formatTime(it.time),
        model: toStr(it.model) || '—',
        alias: toStr(it.alias),
        provider: toStr(it.provider) || '—',
        clientKeyID: /^[a-f0-9]{64}$/.test(toStr(it.client_key_id)) ? it.client_key_id : '',
        authIndex: /^[a-f0-9]{16}$/.test(toStr(it.auth_index)) ? it.auth_index : '',
        credential: (credentials && credentials[it.auth_index]) ||
          (/^[a-f0-9]{16}$/.test(toStr(it.auth_index)) ? '索引 ' + it.auth_index : '未归属'),
        tokens:
          formatCompact(counter(it, 'total_tokens')) +
          '（入 ' + formatCompact(counter(it, 'input_tokens')) +
          ' / 出 ' + formatCompact(counter(it, 'output_tokens')) +
          ' / 缓存 ' + formatCompact(cacheRead + cacheCreation) + '）',
        cacheShare: share.text,
        cacheShareTitle: share.title,
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
      maxBodyStorageBytes: Number(s.max_body_storage_bytes),
      compactIntervalSeconds: Number(s.compact_interval_seconds === undefined ? 900 : s.compact_interval_seconds),
      compactMinBytes: Number(s.compact_min_bytes === undefined ? 8388608 : s.compact_min_bytes),
      prices: s.prices || {},
      priceRules: Array.isArray(s.price_rules) ? s.price_rules : []
    };
  }

  // Only the version segment changes; never accept another origin or unsafe path.
  function coreApiBase(base) {
    if (!safePath(toStr(base))) return '';
    return /\/v(?:0|8)\/management\/plugins\/cliproxyapi-observer$/.test(base)
      ? base.replace(/\/v(?:0|8)(\/management\/plugins\/cliproxyapi-observer)$/, '/v0$1') : '';
  }

  var MIB = 1048576;
  function credentialsUrl(base) {
    var core = coreApiBase(base);
    return core ? core.replace(/\/v0\/management\/plugins\/cliproxyapi-observer$/, '/v8/management/credentials') : '';
  }

  function filenameBase(value) {
    var s = toStr(value).trim();
    var idx = Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\'));
    return idx >= 0 ? s.slice(idx + 1) : s;
  }

  function safeDisplay(value) {
    return toStr(value).replace(/[\u0000-\u001f\u007f]/g, '').slice(0, 200);
  }

  // Retain only display names and nonsecret host indexes, not the full response.
  // Concrete auth files (.json, path reduced to basename) are shown as
  // "label（filename）" so a shared label can never hide which file was used;
  // source/path/id/secret fields are never read or stored.
  function credentialNames(response) {
    var names = Object.create(null);
    var files = response && Array.isArray(response.files) ? response.files : [];
    files.forEach(function (entry) {
      if (!entry || !/^[a-f0-9]{16}$/.test(toStr(entry.auth_index))) return;
      var label = safeDisplay(toStr(entry.label).trim());
      var raw = toStr(entry.name).trim();
      var display;
      if (/\.json$/i.test(raw)) {
        var file = safeDisplay(filenameBase(raw));
        display = !label || label === file ? file : label + '（' + file + '）';
      } else {
        display = label || safeDisplay(raw) || entry.auth_index;
      }
      names[entry.auth_index] = display;
    });
    return names;
  }
  var SETTING_FIELDS = [
    ['request-retention', '请求记录保留（默认 24 小时）', 'duration', 60, 2592000, 'requestRetentionSeconds'],
    ['body-retention', '请求体保留（最多 24 小时）', 'duration', 60, 86400, 'bodyRetentionSeconds'],
    ['stats-retention-days', '聚合统计保留（天）', 'integer', 1, 3650, 'statsRetentionDays'],
    ['max-body-bytes', '单条请求体上限（MiB）', 'bytes', 1, 64 * MIB, 'maxBodyBytes'],
    ['max-body-storage-bytes', '请求体总存储上限（MiB）', 'bytes', 1, 8192 * MIB, 'maxBodyStorageBytes'],
    ['compact-interval', '压缩检查间隔', 'duration', 60, 86400, 'compactIntervalSeconds'],
    ['compact-min-bytes', '最小可回收空间（MiB）', 'bytes', 65536, 8192 * MIB, 'compactMinBytes']
  ];
  var PRICE_KEYS = ['input', 'output', 'cache-read', 'cache-creation'];

  function boundedNumber(value, min, max, label, integer) {
    if (toStr(value).trim() === '') throw new Error(label + '不能为空');
    var n = Number(value);
    if (!isFinite(n) || n < min || n > max || (integer && !Number.isSafeInteger(n))) {
      throw new Error(label + '必须在 ' + min + ' 至 ' + max + ' 之间' + (integer ? '且转换后为整数' : ''));
    }
    return n;
  }

  function durationSeconds(value, unit) {
    var scale = { s: 1, m: 60, h: 3600, d: 86400 }[unit];
    if (!scale) throw new Error('无效的时长单位');
    return boundedNumber(boundedNumber(value, 0, 2592000, '时长', false) * scale, 0, 2592000, '时长（秒）', true);
  }

  function settingsToPatch(raw) {
    var s = normalizeSettings(raw);
    var patch = { 'capture-bodies': s.captureBodies, prices: Object.create(null) };
    SETTING_FIELDS.forEach(function (field) {
      patch[field[0]] = field[2] === 'duration' ? s[field[5]] + 's' : s[field[5]];
    });
    Object.keys(s.prices).forEach(function (id) {
      var price = s.prices[id] || {};
      patch.prices[id] = {
        input: price.input, output: price.output,
        'cache-read': price.cache_read === undefined ? (price['cache-read'] || 0) : price.cache_read,
        'cache-creation': price.cache_creation === undefined ? (price['cache-creation'] || 0) : price.cache_creation
      };
    });
    if (s.priceRules.length) patch['price-rules'] = normalizePriceRules(s.priceRules);
    return patch;
  }

  // Accepts the backend's underscore aliases but always emits hyphenated canonical keys.
  var RULE_KEYS = ['model', 'price', 'input-tokens-gt', 'input_tokens_gt', 'time-range', 'time_range'];
  var TIME_RANGE = /^(?:[01]\d|2[0-3]):[0-5]\d-(?:(?:[01]\d|2[0-3]):[0-5]\d|24:00)$/;
  function normalizePriceRules(raw) {
    if (!Array.isArray(raw) || raw.length > 1000) throw new Error('价格规则必须是最多 1000 项的列表');
    return raw.map(function (rule) {
      if (!rule || typeof rule !== 'object' || Array.isArray(rule)) throw new Error('价格规则无效');
      var seenRuleKeys = Object.create(null);
      Object.keys(rule).forEach(function (key) {
        if (RULE_KEYS.indexOf(key) < 0) throw new Error('价格规则包含未知字段');
        var canonical = key.replace(/_/g, '-');
        if (seenRuleKeys[canonical]) throw new Error('价格规则包含重复字段');
        seenRuleKeys[canonical] = true;
      });
      if (typeof rule.model !== 'string' || !rule.model.trim()) throw new Error('规则模型 ID 不能为空');
      var result = { model: rule.model.trim(), price: {} };
      var price = rule.price;
      if (!price || typeof price !== 'object' || Array.isArray(price)) throw new Error('价格字段无效');
      var seenPriceKeys = Object.create(null);
      Object.keys(price).forEach(function (key) {
        var canonical = key.replace(/_/g, '-');
        if (PRICE_KEYS.indexOf(canonical) < 0) throw new Error('价格字段无效');
        if (seenPriceKeys[canonical]) throw new Error('价格包含重复字段');
        seenPriceKeys[canonical] = true;
      });
      PRICE_KEYS.forEach(function (key) {
        var alias = key.replace(/-/g, '_');
        var value = price[key] === undefined ? price[alias] : price[key];
        if (value === undefined) value = 0;
        if (typeof value !== 'number') throw new Error('模型价格必须是数字');
        result.price[key] = boundedNumber(value, 0, Number.MAX_VALUE, '模型价格', false);
      });
      var threshold = rule['input-tokens-gt'] === undefined ? rule.input_tokens_gt : rule['input-tokens-gt'];
      if (threshold !== undefined) {
        if (typeof threshold !== 'number') throw new Error('输入 Token 阈值必须是整数');
        result['input-tokens-gt'] = boundedNumber(threshold, 0, Number.MAX_SAFE_INTEGER, '输入 Token 阈值', true);
      }
      var range = rule['time-range'] === undefined ? rule.time_range : rule['time-range'];
      if (range !== undefined && range !== '') {
        // UTC HH:mm-HH:mm; start inclusive, end exclusive, overnight and end 24:00 allowed.
        if (typeof range !== 'string' || !TIME_RANGE.test(range) || range.slice(0, 5) === range.slice(6)) {
          throw new Error('时间区间使用 UTC HH:mm-HH:mm，起点包含终点不包含且不能相同');
        }
        result['time-range'] = range;
      }
      return result;
    });
  }

  function validateSettingsPatch(patch) {
    if (!patch || typeof patch !== 'object' || Array.isArray(patch)) throw new Error('设置必须是对象');
    var allowed = ['capture-bodies', 'prices', 'price-rules'].concat(SETTING_FIELDS.map(function (f) { return f[0]; }));
    Object.keys(patch).forEach(function (key) { if (allowed.indexOf(key) < 0) throw new Error('不支持的设置字段'); });
    if (typeof patch['capture-bodies'] !== 'boolean') throw new Error('捕获开关无效');
    SETTING_FIELDS.forEach(function (f) {
      var value = patch[f[0]];
      if (f[2] === 'duration') {
        var match = /^(\d+(?:\.\d+)?)(s|m|h)$/.exec(toStr(value));
        if (!match) throw new Error(f[1] + '时长无效');
        value = durationSeconds(match[1], match[2]);
      }
      boundedNumber(value, f[3], f[4], f[1], true);
    });
    if (patch['max-body-bytes'] > patch['max-body-storage-bytes']) throw new Error('单条请求体上限不能超过总存储上限');
    var prices = patch.prices;
    if (!prices || typeof prices !== 'object' || Array.isArray(prices)) throw new Error('价格必须是对象');
    var seen = Object.create(null);
    Object.keys(prices).forEach(function (id) {
      var normalized = id.trim();
      if (!normalized || seen[normalized]) throw new Error('模型 ID 为空或重复');
      seen[normalized] = true;
      var price = prices[id];
      if (!price || typeof price !== 'object' || Array.isArray(price) ||
          Object.keys(price).some(function (key) { return PRICE_KEYS.indexOf(key) < 0; })) throw new Error('价格字段无效');
      PRICE_KEYS.forEach(function (key) { boundedNumber(price[key], 0, Number.MAX_VALUE, '模型价格', false); });
    });
    if (patch['price-rules'] !== undefined) normalizePriceRules(patch['price-rules']);
    return patch;
  }

  function patchesMatch(a, b) {
    try {
      validateSettingsPatch(a); validateSettingsPatch(b);
      var normalize = function (p) {
        var result = {};
        SETTING_FIELDS.forEach(function (f) {
          var v = p[f[0]];
          var m = f[2] === 'duration' && /^(\d+(?:\.\d+)?)(s|m|h)$/.exec(v);
          result[f[0]] = m ? durationSeconds(m[1], m[2]) : v;
        });
        result['capture-bodies'] = p['capture-bodies'];
        result.prices = Object.keys(p.prices).sort().map(function (id) {
          return [id, PRICE_KEYS.map(function (key) { return p.prices[id][key]; })];
        });
        result['price-rules'] = normalizePriceRules(p['price-rules'] || []);
        return JSON.stringify(result);
      };
      return normalize(a) === normalize(b);
    } catch (_) { return false; }
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
      tbody.appendChild(emptyRow(doc, 10, '暂无数据'));
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
      var hit = cell(doc, 'num col-secondary', r.hitRate);
      hit.title = r.hitTitle;
      tr.appendChild(hit);
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

  function renderRequestsTable(doc, tbody, items, onView, credentials, onKey) {
    clearChildren(tbody);
    var rows = buildRequestRows(items, credentials);
    if (!rows.length) {
      tbody.appendChild(emptyRow(doc, 13, '暂无数据'));
      return rows;
    }
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var tr = doc.createElement('tr');
      tr.appendChild(cell(doc, '', r.time));

      var modelCell = cell(doc, 'model-cell cell-truncate', r.model);
      // The cell is CSS-truncated, so the hover title must always carry the
      // full model name; a differing alias is appended on a new line.
      modelCell.title = r.alias && r.alias !== r.model
        ? r.model + '\n别名 ' + r.alias
        : r.model;
      tr.appendChild(modelCell);

      tr.appendChild(cell(doc, 'col-secondary', r.provider));

      var keyDisplay = r.clientKeyID ? truncateText(r.clientKeyID, 12) : '未归属';
      var keyClass = 'cell-truncate' + (r.clientKeyID ? ' cell-clickable' : '');
      var keyCell = cell(doc, keyClass, keyDisplay);
      keyCell.title = r.clientKeyID || '未归属';
      if (r.clientKeyID && onKey) {
        (function (id) {
          keyCell.addEventListener('click', function () { onKey(id); });
        })(r.clientKeyID);
      }
      tr.appendChild(keyCell);

      var authDisplay = truncateText(r.credential, 12);
      var authCell = cell(doc, 'cell-truncate', authDisplay);
      authCell.title = r.credential;
      tr.appendChild(authCell);
      tr.appendChild(cell(doc, 'num col-secondary', r.tokens));
      var shareCell = cell(doc, 'num col-secondary', r.cacheShare);
      shareCell.title = r.cacheShareTitle;
      tr.appendChild(shareCell);
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

  function renderSettings(doc, container, settings, onDirty) {
    clearChildren(container);
    var s = normalizeSettings(settings);
    var controls = {};
    var changed = function () { if (onDirty) onDirty(); };
    function input(label, value, type, id) {
      var wrapper = doc.createElement('label');
      wrapper.className = 'setting-field';
      var title = doc.createElement('span');
      setText(title, label);
      wrapper.appendChild(title);
      var node = doc.createElement('input');
      node.type = type;
      node.value = toStr(value);
      if (type === 'number') node.step = 'any';
      if (id) node.id = id;
      node.addEventListener('input', changed);
      wrapper.appendChild(node);
      return { wrapper: wrapper, node: node };
    }
    var capture = input('显式选择捕获请求体（含提示词 / 代码）', '', 'checkbox', 'setting-capture-bodies');
    capture.wrapper.className = 'setting-field setting-capture';
    capture.node.checked = s.captureBodies;
    container.appendChild(capture.wrapper);
    var note = doc.createElement('p');
    note.className = 'settings-note';
    setText(note, s.captureBodies ? '请求体捕获已开启。' : '请求体捕获已关闭：仅显示元数据。勾选只修改草稿，保存前不会捕获新请求体。');
    container.appendChild(note);
    var grid = doc.createElement('div');
    grid.className = 'settings-fields';
    SETTING_FIELDS.forEach(function (f) {
      var value = s[f[5]], unit = 's';
      if (f[2] === 'duration') {
        if (value % 86400 === 0) unit = 'd';
        else if (value % 3600 === 0) unit = 'h';
        else if (value % 60 === 0) unit = 'm';
        value /= { s: 1, m: 60, h: 3600, d: 86400 }[unit];
      } else if (f[2] === 'bytes') value /= MIB;
      var field = input(f[1], value, 'number', 'setting-' + f[0]);
      var group = doc.createElement('div');
      group.className = 'setting-field';
      group.appendChild(field.wrapper);
      if (f[2] !== 'duration') {
        field.node.min = f[3] / (f[2] === 'bytes' ? MIB : 1);
        field.node.max = f[4] / (f[2] === 'bytes' ? MIB : 1);
      }
      var select = null;
      if (f[2] === 'duration') {
        select = doc.createElement('select');
        select.setAttribute('aria-label', f[1] + '单位');
        ['s', 'm', 'h', 'd'].forEach(function (u, i) { select.appendChild(optionEl(doc, u, ['秒', '分钟', '小时', '天'][i])); });
        select.value = unit;
        select.id = 'unit-' + f[0];
        select.addEventListener('change', changed);
        group.appendChild(select);
      }
      controls[f[0]] = { node: field.node, unit: select };
      grid.appendChild(group);
    });
    container.appendChild(grid);
    var heading = doc.createElement('h3');
    setText(heading, '模型价格 · USD / 百万 Token');
    container.appendChild(heading);
    var priceNote = doc.createElement('p');
    priceNote.className = 'settings-note';
    setText(
      priceNote,
      '规则自上而下匹配：模型 ID、输入 Token 阈值（严格大于）与 UTC+0 时间区间（HH:mm-HH:mm，' +
        '起点包含、终点不包含，可跨夜，终点可为 24:00）需同时满足（AND），整个请求采用第一条命中的价格；' +
        '无条件规则即默认价，建议放在最后。'
    );
    container.appendChild(priceNote);
    var rows = doc.createElement('div');
    container.appendChild(rows);
    var priceRows = [];
    // Screen-reader labels follow the current position and model of every rule.
    function relabel() {
      priceRows.forEach(function (r, index) {
        var name = r.model.value.trim() || '未命名规则';
        var prefix = '第 ' + (index + 1) + ' 条（' + name + '）';
        r.up.setAttribute('aria-label', '上移 ' + prefix);
        r.down.setAttribute('aria-label', '下移 ' + prefix);
        r.remove.setAttribute('aria-label', '删除 ' + prefix);
        r.remove.title = '删除 ' + prefix;
      });
    }
    function addPrice(id, price, rule) {
      var row = doc.createElement('div');
      row.className = 'price-row';
      var model = input('精确完整模型 ID', id, 'text');
      model.node.setAttribute('data-price', 'model');
      model.node.addEventListener('input', relabel);
      row.appendChild(model.wrapper);
      var record = { row: row, model: model.node, values: {} };
      PRICE_KEYS.forEach(function (key, i) {
        var p = input(['输入', '输出', '缓存读', '缓存创建'][i], price[key] === undefined ? 0 : price[key], 'number');
        p.node.setAttribute('data-price', key);
        record.values[key] = p.node;
        row.appendChild(p.wrapper);
      });
      var threshold = input('输入 Token >（可选）', rule && rule['input-tokens-gt'] !== undefined ? rule['input-tokens-gt'] : '', 'number');
      threshold.node.step = '1'; threshold.node.min = '0';
      threshold.node.setAttribute('data-price', 'input-tokens-gt');
      record.threshold = threshold.node;
      var daily = input('每日时间 UTC+0（可选）', rule && rule['time-range'] || '', 'text');
      daily.node.placeholder = '00:00-08:30';
      daily.node.setAttribute('data-price', 'time-range');
      record.daily = daily.node;
      row.appendChild(threshold.wrapper); row.appendChild(daily.wrapper);
      var actions = doc.createElement('div');
      actions.className = 'price-actions';
      var up = doc.createElement('button');
      up.type = 'button'; up.className = 'btn btn-secondary btn-sm'; setText(up, '上移');
      var down = doc.createElement('button');
      down.type = 'button'; down.className = 'btn btn-secondary btn-sm'; setText(down, '下移');
      var remove = doc.createElement('button');
      remove.type = 'button'; remove.className = 'btn btn-danger btn-sm'; setText(remove, '删除');
      record.up = up; record.down = down; record.remove = remove;
      function move(delta) {
        var index = priceRows.indexOf(record), target = index + delta;
        if (target < 0 || target >= priceRows.length) return;
        priceRows.splice(index, 1); priceRows.splice(target, 0, record);
        clearChildren(rows);
        priceRows.forEach(function (r) { rows.appendChild(r.row); });
        relabel();
        changed();
        var focus = delta < 0 ? up : down;
        if (typeof focus.focus === 'function') focus.focus();
      }
      up.addEventListener('click', function () { move(-1); });
      down.addEventListener('click', function () { move(1); });
      remove.addEventListener('click', function () {
        rows.removeChild(row); priceRows.splice(priceRows.indexOf(record), 1); relabel(); changed();
      });
      actions.appendChild(up); actions.appendChild(down); actions.appendChild(remove);
      row.appendChild(actions);
      priceRows.push(record); rows.appendChild(row); relabel();
    }
    var currentPatch = settingsToPatch(settings);
    (currentPatch['price-rules'] || []).forEach(function (rule) { addPrice(rule.model, rule.price, rule); });
    var currentPrices = currentPatch.prices;
    Object.keys(currentPrices).forEach(function (id) { addPrice(id, currentPrices[id]); });
    var add = doc.createElement('button');
    add.type = 'button'; add.className = 'btn btn-secondary'; add.id = 'price-add';
    setText(add, '添加价格规则');
    add.addEventListener('click', function () { addPrice('', {}); changed(); });
    container.appendChild(add);
    return {
      read: function () {
        var patch = { 'capture-bodies': capture.node.checked, prices: Object.create(null) };
        SETTING_FIELDS.forEach(function (f) {
          var c = controls[f[0]], v = c.node.value;
          if (toStr(v).trim() === '') throw new Error(f[1] + '不能为空');
          if (f[2] === 'duration') patch[f[0]] = durationSeconds(v, c.unit.value) + 's';
          else patch[f[0]] = Number(v) * (f[2] === 'bytes' ? MIB : 1);
        });
        var rules = [];
        priceRows.forEach(function (r) {
          var id = r.model.value.trim();
          if (!id) throw new Error('模型 ID 不能为空');
          var rule = { model: id, price: {} };
          PRICE_KEYS.forEach(function (key) {
            rule.price[key] = boundedNumber(r.values[key].value, 0, Number.MAX_VALUE, '模型价格', false);
          });
          if (toStr(r.threshold.value).trim()) {
            rule['input-tokens-gt'] = boundedNumber(r.threshold.value, 0, Number.MAX_SAFE_INTEGER, '输入 Token 阈值', true);
          }
          if (toStr(r.daily.value).trim()) rule['time-range'] = r.daily.value.trim();
          rules.push(rule);
        });
        // Canonical save: always replace the legacy map with an empty object and
        // persist every row as an ordered rule. Rows render explicit rules first
        // and converted legacy map rows after them, so their order stays stable.
        patch['price-rules'] = normalizePriceRules(rules);
        return validateSettingsPatch(patch);
      }
    };
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

  // 客户端 key 下拉候选：始终含「全部」「未归属」；只收集合法 64 位指纹并去重排序，
  // 展示前 12 位但 value/title 为完整指纹；当前已选合法指纹与本次连接已出现的候选
  // （previousOptions）即使不在当前结果中也保留。
  function clientKeyOptions(groups, items, selected, previousOptions) {
    var ids = Object.create(null);
    var collect = function (id) { if (/^[a-f0-9]{64}$/.test(toStr(id))) ids[id] = true; };
    (Array.isArray(groups) ? groups : []).forEach(function (group) { collect(group && group.id); });
    (Array.isArray(items) ? items : []).forEach(function (item) { collect(item && item.client_key_id); });
    (Array.isArray(previousOptions) ? previousOptions : []).forEach(function (option) { collect(option && option.value); });
    var chosen = toStr(selected);
    if (chosen && chosen !== 'unknown' && /^[a-f0-9]{64}$/.test(chosen)) ids[chosen] = true;
    var out = [
      { value: '', label: '全部' },
      { value: 'unknown', label: '未归属（含旧记录）' }
    ];
    Object.keys(ids).sort().forEach(function (id) {
      out.push({ value: id, label: id.slice(0, 12) + '…', title: id });
    });
    return out;
  }

  function selectHasOption(select, value) {
    var options = select && select.options;
    if (!options) return false;
    for (var i = 0; i < options.length; i++) {
      if (options[i].value === value) return true;
    }
    return false;
  }

  function createThemeController(win, doc, onChange) {
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
      if (onChange) onChange();
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

  function createSelectController(win, doc) {
    var records = [];
    var popup = null;
    var active = null;
    var highlight = -1;
    var outsideBound = false;
    var viewportBound = false;
    var nextId = 1;

    function optionsOf(select) {
      return select && select.options ? Array.prototype.slice.call(select.options) : [];
    }

    function optionDisabled(option) {
      var group = option && option.parentNode;
      return !option || option.disabled || !!(group && group.tagName === 'OPTGROUP' && group.disabled);
    }

    function enabledIndexes(select) {
      var indexes = [];
      optionsOf(select).forEach(function (option, index) {
        if (!optionDisabled(option)) indexes.push(index);
      });
      return indexes;
    }

    function moveHighlight(select, direction, edge) {
      var indexes = enabledIndexes(select);
      if (!indexes.length) return;
      if (edge) {
        highlight = direction < 0 ? indexes[0] : indexes[indexes.length - 1];
        return;
      }
      var position = indexes.indexOf(highlight);
      if (position < 0) highlight = indexes[0];
      else highlight = indexes[(position + direction + indexes.length) % indexes.length];
    }

    function closePopup(restoreFocus) {
      if (popup && popup.parentNode) popup.parentNode.removeChild(popup);
      popup = null;
      if (active && active.trigger) {
        active.trigger.setAttribute('aria-expanded', 'false');
        active.trigger.removeAttribute('aria-activedescendant');
        active.trigger.removeAttribute('aria-controls');
        if (restoreFocus && typeof active.trigger.focus === 'function') active.trigger.focus();
      }
      active = null;
      highlight = -1;
      if (outsideBound && win.removeEventListener) {
        win.removeEventListener('mousedown', onOutside, true);
        try { if (win.parent && win.parent !== win) win.parent.removeEventListener('mousedown', onOutside, true); } catch (err) {}
        outsideBound = false;
      }
      if (viewportBound && win.removeEventListener) {
        win.removeEventListener('resize', onViewport);
        win.removeEventListener('scroll', onViewport, true);
        try {
          if (win.parent && win.parent !== win) {
            win.parent.removeEventListener('resize', onViewport);
            win.parent.removeEventListener('scroll', onViewport, true);
          }
        } catch (err) {}
        viewportBound = false;
      }
    }

    function visibleViewport() {
      var top = 0;
      var left = 0;
      var right = win.innerWidth;
      var bottom = win.innerHeight;
      try {
        if (win.frameElement && win.parent && win.parent !== win) {
          var frame = win.frameElement.getBoundingClientRect();
          var parentTop = Math.max(0, -frame.top);
          var parentLeft = Math.max(0, -frame.left);
          var parentRight = Math.min(win.innerWidth, win.parent.innerWidth - frame.left);
          var parentBottom = Math.min(win.innerHeight, win.parent.innerHeight - frame.top);
          if (parentRight > parentLeft && parentBottom > parentTop) {
            top = parentTop;
            left = parentLeft;
            right = parentRight;
            bottom = parentBottom;
          }
        }
      } catch (err) { /* 跨源宿主只使用插件自身视口。 */ }
      return { top: top, left: left, right: right, bottom: bottom };
    }

    function placePopup() {
      if (!popup || !active || typeof active.trigger.getBoundingClientRect !== 'function') return;
      var rect = active.trigger.getBoundingClientRect();
      var view = visibleViewport();
      var margin = 8;
      var gap = 6;
      var maxHeight = 240;
      var width = Math.max(0, Math.min(rect.width, view.right - view.left - margin * 2));
      var left = Math.min(Math.max(rect.left, view.left + margin), Math.max(view.left + margin, view.right - width - margin));
      var below = view.bottom - rect.bottom - margin - gap;
      var above = rect.top - view.top - margin - gap;
      popup.style.left = left + 'px';
      popup.style.width = width + 'px';
      popup.style.maxHeight = Math.max(0, Math.min(maxHeight, below >= maxHeight || below >= above ? below : above)) + 'px';
      if (below >= maxHeight || below >= above) {
        popup.style.top = (rect.bottom + gap) + 'px';
        popup.style.bottom = 'auto';
      } else {
        popup.style.top = 'auto';
        popup.style.bottom = (win.innerHeight - rect.top + gap) + 'px';
      }
    }

    function onOutside(event) {
      if (!active || !popup) return;
      var target = event.target;
      if (active.wrap && active.wrap.contains && active.wrap.contains(target)) return;
      if (popup.contains && popup.contains(target)) return;
      closePopup(false);
    }

    function onViewport() {
      if (!active || !active.select || !active.select.isConnected) closePopup(false);
      else placePopup();
    }

    function paintOptions() {
      if (!popup || !active) return;
      clearChildren(popup);
      var select = active.select;
      optionsOf(select).forEach(function (option, index) {
        var item = doc.createElement('button');
        item.type = 'button';
        item.tabIndex = -1;
        item.className = 'select-option' + (option.value === select.value ? ' is-selected' : '') +
          (index === highlight ? ' is-highlighted' : '');
        item.id = select.id + '-option-' + index;
        item.setAttribute('role', 'option');
        item.setAttribute('aria-selected', option.value === select.value ? 'true' : 'false');
        item.disabled = optionDisabled(option);
        setText(item, option.textContent || option.label || option.value || '');
        item.addEventListener('mousedown', function (event) { event.preventDefault(); });
        item.addEventListener('click', function () { commit(index); });
        popup.appendChild(item);
      });
      if (highlight >= 0) {
        active.trigger.setAttribute('aria-activedescendant', select.id + '-option-' + highlight);
        var current = popup.childNodes[highlight];
        if (current) {
          if (current.offsetTop < popup.scrollTop) popup.scrollTop = current.offsetTop;
          else if (current.offsetTop + current.offsetHeight > popup.scrollTop + popup.clientHeight) {
            popup.scrollTop = current.offsetTop + current.offsetHeight - popup.clientHeight;
          }
        }
      } else active.trigger.removeAttribute('aria-activedescendant');
    }

    function commit(index) {
      if (!active) return;
      var select = active.select;
      var option = optionsOf(select)[index];
      if (select.disabled || optionDisabled(option)) return;
      var changed = select.value !== option.value;
      select.value = option.value;
      closePopup(true);
      syncRecord(recordFor(select));
      if (changed) select.dispatchEvent(new win.Event('change', { bubbles: true }));
    }

    function onTriggerKey(event) {
      var record = recordFor(event.currentTarget && event.currentTarget.selectSource);
      if (!record || record.select.disabled) return;
      var key = event.key;
      if (key === 'ArrowDown' || key === 'ArrowUp' || key === 'Home' || key === 'End') {
        event.preventDefault();
        if (!popup) open(record);
        else moveHighlight(record.select, key === 'ArrowUp' || key === 'Home' ? -1 : 1, key === 'Home' || key === 'End');
        paintOptions();
      } else if (key === 'Enter' || key === ' ') {
        event.preventDefault();
        if (popup) commit(highlight);
        else open(record);
      } else if (key === 'Escape' && popup) {
        event.preventDefault();
        closePopup(true);
      } else if (key === 'Tab' && popup) {
        closePopup(false);
      }
    }

    function open(record) {
      if (!record || record.select.disabled) return;
      if (active && active !== record) closePopup(false);
      active = record;
      if (!popup) {
        popup = doc.createElement('div');
        popup.className = 'select-popup';
        popup.setAttribute('role', 'listbox');
        doc.body.appendChild(popup);
      }
      popup.id = record.select.id + '-listbox';
      record.trigger.setAttribute('aria-controls', popup.id);
      record.trigger.setAttribute('aria-expanded', 'true');
      var selected = optionsOf(record.select).findIndex(function (option) { return option.value === record.select.value && !optionDisabled(option); });
      var enabled = enabledIndexes(record.select);
      highlight = selected >= 0 ? selected : (enabled.length ? enabled[0] : -1);
      placePopup();
      paintOptions();
      if (!outsideBound) {
        win.addEventListener('mousedown', onOutside, true);
        try {
          if (win.parent && win.parent !== win) win.parent.addEventListener('mousedown', onOutside, true);
        } catch (err) { /* 跨源宿主不能监听父级点击。 */ }
        outsideBound = true;
      }
      if (!viewportBound) {
        win.addEventListener('resize', onViewport);
        win.addEventListener('scroll', onViewport, true);
        try {
          if (win.parent && win.parent !== win) {
            win.parent.addEventListener('resize', onViewport);
            win.parent.addEventListener('scroll', onViewport, true);
          }
        } catch (err) { /* 跨源宿主不能监听父级几何变化。 */ }
        viewportBound = true;
      }
    }

    function labelFor(select) {
      if (!select.id || !doc.querySelector) return null;
      return doc.querySelector('label[for="' + select.id + '"]');
    }

    function recordFor(select) {
      for (var i = 0; i < records.length; i++) if (records[i].select === select) return records[i];
      return null;
    }

    function syncRecord(record) {
      if (!record) return;
      var select = record.select;
      if (!select.isConnected) {
        if (active === record) closePopup(false);
        if (record.wrap && record.wrap.parentNode) record.wrap.parentNode.removeChild(record.wrap);
        records.splice(records.indexOf(record), 1);
        return;
      }
      if (!record.wrap) {
        record.wrap = doc.createElement('span');
        record.wrap.className = 'select-wrap';
        select.parentNode.insertBefore(record.wrap, select);
        record.wrap.appendChild(select);
        record.trigger = doc.createElement('button');
        record.trigger.type = 'button';
        record.trigger.selectSource = select;
        record.trigger.addEventListener('click', function () {
          if (active === record && popup) closePopup(false);
          else open(record);
        });
        record.trigger.addEventListener('keydown', onTriggerKey);
        var label = doc.createElement('span');
        label.className = 'select-trigger-text';
        var icon = doc.createElement('span');
        icon.className = 'select-chevron';
        icon.setAttribute('aria-hidden', 'true');
        record.trigger.appendChild(label);
        record.trigger.appendChild(icon);
        record.wrap.appendChild(record.trigger);
        select.classList.add('select-native');
        select.tabIndex = -1;
        select.setAttribute('aria-hidden', 'true');
        if (!select.id) select.id = 'observer-select-' + nextId++;
        select.addEventListener('focus', function () {
          if (record.trigger && !select.disabled) record.trigger.focus();
        });
        select.addEventListener('change', function () { syncRecord(record); });
        if (labelFor(select)) {
          labelFor(select).addEventListener('click', function (event) {
            if (select.disabled) return;
            event.preventDefault();
            record.trigger.focus();
          });
        }
      }
      if (active === record && popup) paintOptions();
      var compact = select.id === 'key-group-kind' || (select.id && select.id.indexOf('unit-') === 0);
      record.trigger.className = 'btn select-trigger' + (compact ? ' is-compact' : '');
      record.trigger.disabled = !!select.disabled;
      record.trigger.setAttribute('role', 'combobox');
      record.trigger.setAttribute('aria-haspopup', 'listbox');
      record.trigger.setAttribute('aria-expanded', active === record && popup ? 'true' : 'false');
      if (select.id) record.trigger.id = select.id + '-trigger';
      var labelledBy = select.getAttribute('aria-labelledby');
      var label = labelFor(select);
      if (labelledBy) record.trigger.setAttribute('aria-labelledby', labelledBy);
      else if (label && label.id) record.trigger.setAttribute('aria-labelledby', label.id);
      else if (label) record.trigger.setAttribute('aria-label', label.textContent || '');
      else if (select.getAttribute('aria-label')) record.trigger.setAttribute('aria-label', select.getAttribute('aria-label'));
      var described = select.getAttribute('aria-describedby');
      if (described) record.trigger.setAttribute('aria-describedby', described);
      else record.trigger.removeAttribute('aria-describedby');
      var selected = optionsOf(select).filter(function (option) { return option.value === select.value; })[0];
      var text = selected ? (selected.textContent || selected.label || selected.value || '') : '';
      setText(record.trigger.firstChild, text);
      record.trigger.title = text;
      if (select.disabled && active === record) closePopup(false);
    }

    function refresh() {
      closePopup(false);
      var selects = doc.querySelectorAll ? doc.querySelectorAll('select') : [];
      var seen = [];
      for (var i = 0; i < selects.length; i++) {
        var select = selects[i];
        if (!select.isConnected) continue;
        var record = recordFor(select) || { select: select };
        if (!recordFor(select)) records.push(record);
        seen.push(select);
        syncRecord(record);
      }
      records.slice().forEach(function (record) {
        if (seen.indexOf(record.select) < 0) syncRecord(record);
      });
    }

    return { refresh: refresh, close: function () { closePopup(false); } };
  }

  function startDashboard(win, doc) {
    var els = {
      key: doc.getElementById('mgmt-key'),
      connect: doc.getElementById('connect'),
      refresh: doc.getElementById('refresh'),
      status: doc.getElementById('conn-status'),
      rangeLabel: doc.getElementById('range-label'),
      timeStart: doc.getElementById('time-start'),
      timeEnd: doc.getElementById('time-end'),
      timeApply: doc.getElementById('time-apply'),
      timeError: doc.getElementById('time-error'),
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
      requestsPrev: doc.getElementById('requests-prev'),
      requestsNext: doc.getElementById('requests-next'),
      requestsPage: doc.getElementById('requests-page'),
      clientKey: doc.getElementById('filter-client-key'),
      auth: doc.getElementById('filter-auth'),
      identityApply: doc.getElementById('identity-apply'),
      credentialStatus: doc.getElementById('credential-status'),
      settingsBody: doc.getElementById('settings-body'),
      save: doc.getElementById('settings-save'),
      revert: doc.getElementById('settings-revert'),
      settingsStatus: doc.getElementById('settings-status'),
      diskStatus: doc.getElementById('disk-status'),
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
    var derivedBase = deriveApiBase(loc.pathname);
    var apiBase = coreApiBase(derivedBase) || derivedBase;
    var theme = createThemeController(win, doc, function () {
      if (state) renderChart(els.chart, state.series);
    });
    theme.start();
    var selects = createSelectController(win, doc);
    win.__observerSelects = selects;

    var state = {
      key: '',
      connected: false,
      range: null,
      draftPending: false,
      provider: '',
      model: '',
      offset: 0,
      items: [],
      hasMore: false,
      loadingPage: false,
      refreshing: false,
      bounds: null,
      requestProvider: '',
      requestModel: '',
      clientKeyID: '',
      authIndex: '',
      requestClientKeyID: '',
      requestAuthIndex: '',
      credentials: Object.create(null),
      clientGroups: [],
      authGroups: [],
      series: [],
      providers: {},
      models: {}
    };
    var queryGate = createGate();
    var moreGate = createGate();
    var bodyGate = createGate();
    var queryAbort = null;
    var moreAbort = null;
    var bodyAbort = null;
    var bodyState = BODY_IDLE;
    var connectionGate = createGate();
    var saveAbort = null;
    var saveTimer = null;
    var saving = false;
    var dirty = false;
    var editor = null;
    var confirmation = doc.getElementById('confirmation-dialog');
    var confirmationResolve = null;
    var confirmationFocus = null;

    function positionConfirmation() {
      confirmation.style.removeProperty('top');
      confirmation.style.removeProperty('margin');
      // A tall same-origin iframe may be scrolled behind host chrome. Read only
      // frame/viewport geometry, never parent content or credentials.
      try {
        if (!win.frameElement || win.parent === win) return;
        var frame = win.frameElement.getBoundingClientRect();
        var start = Math.max(0, -frame.top);
        var end = Math.min(win.innerHeight, win.parent.innerHeight - frame.top);
        var height = confirmation.getBoundingClientRect().height;
        if (end - start < height) return;
        confirmation.style.margin = '0 auto';
        confirmation.style.top = (start + (end - start - height) / 2) + 'px';
      } catch (_) { /* Cross-origin hosts keep browser-native centering. */ }
    }

    // A single asynchronous modal; cancellation never enters validation or writes.
    function finishConfirmation(approved) {
      if (!confirmationResolve) return;
      var resolve = confirmationResolve;
      confirmationResolve = null;
      if (confirmation.open) confirmation.close();
      if (confirmationFocus && confirmationFocus.isConnected) confirmationFocus.focus();
      confirmationFocus = null;
      resolve(approved);
    }

    function askConfirmation(title, description) {
      if (confirmationResolve) return Promise.resolve(false);
      setText(doc.getElementById('confirmation-title'), title);
      setText(doc.getElementById('confirmation-desc'), description);
      confirmationFocus = doc.activeElement;
      return new Promise(function (resolve) {
        confirmationResolve = resolve;
        confirmation.showModal();
        positionConfirmation();
        doc.getElementById('confirmation-cancel').focus({ preventScroll: true });
      });
    }
    doc.getElementById('confirmation-confirm').addEventListener('click', function () { finishConfirmation(true); });
    doc.getElementById('confirmation-cancel').addEventListener('click', function () { finishConfirmation(false); });
    confirmation.addEventListener('cancel', function (event) { event.preventDefault(); finishConfirmation(false); });
    confirmation.addEventListener('close', function () { if (!confirmation.open) finishConfirmation(false); });
    confirmation.addEventListener('click', function (event) {
      var rect = confirmation.getBoundingClientRect();
      if (event.target === confirmation && (event.clientX < rect.left || event.clientX > rect.right ||
          event.clientY < rect.top || event.clientY > rect.bottom)) finishConfirmation(false);
    });

    function editorActions() {
      pageActions();
      if (els.save) els.save.disabled = !state.connected || !editor || !dirty || saving;
      if (els.revert) els.revert.disabled = !state.connected || saving;
      if (els.settingsBody.querySelectorAll) {
        var fields = els.settingsBody.querySelectorAll('input, select, button');
        for (var i = 0; i < fields.length; i++) fields[i].disabled = saving || !state.connected;
      }
      selects.refresh();
    }

    function markDirty() {
      dirty = true;
      setText(els.settingsStatus, '有未保存的草稿（刷新不会覆盖）');
      editorActions();
    }

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

    function abortMore() {
      moreGate.invalidate();
      if (moreAbort) moreAbort.abort();
      moreAbort = null;
      state.loadingPage = false;
      pageActions();
    }

    function disconnect() {
      selects.close();
      connectionGate.invalidate();
      finishConfirmation(false);
      if (saveAbort) saveAbort.abort();
      saveAbort = null;
      if (saveTimer) win.clearTimeout(saveTimer);
      saveTimer = null;
      saving = false;
      state.connected = false;
      state.key = '';
      queryGate.invalidate();
      abortQuery();
      abortMore();
      clearBodyViewer();
      closeBody();
      els.refresh.disabled = true;
      state.refreshing = false;
      pageActions();
      editorActions();
    }

    function handleAuthFailure(err) {
      disconnect();
      setStatus((err && err.message) || '管理密钥无效', 'error');
      setBanner('认证失败：请确认管理密钥正确、远程管理已开启，或当前客户端在允许范围内。');
    }

    function fetchJson(path, params, signal, method, payload) {
      if (!state.connected || !state.key) return Promise.reject(new Error('尚未连接'));
      var url = path === 'credentials' ? credentialsUrl(apiBase) :
        buildApiUrl(path === 'config' ? coreApiBase(apiBase) : apiBase, path, params);
      if (!url) return Promise.reject(new Error('无效的接口地址'));
      if (typeof win.fetch !== 'function') return Promise.reject(new Error('浏览器不支持 fetch'));
      var headers = {};
      if (state.key) headers.Authorization = 'Bearer ' + state.key;
      var init = { method: method || 'GET', headers: headers, credentials: 'same-origin', cache: 'no-store', redirect: 'error' };
      if (payload !== undefined) {
        headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(payload);
      }
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
      selects.refresh();
    }

    function applySummary(summary) {
      var s = summary && typeof summary === 'object' ? summary : {};
      state.clientGroups = Array.isArray(s.client_keys) ? s.client_keys : [];
      state.authGroups = Array.isArray(s.credentials) ? s.credentials : [];
      renderOverview(doc, els.cards, buildOverview(s.totals));
      var count = renderGroupsTable(doc, els.groupsBody, s.groups).length;
      setText(els.groupsNote, count ? count + ' 个分组' : '');
      var series = Array.isArray(s.series) ? s.series : [];
      state.series = series;
      renderChart(els.chart, series);
      setText(els.trendNote, series.length ? series.length + ' 个采样点' : '');
      updateFacets(s.groups);
      showRange();
    }

    function showRange() {
      if (!els.rangeLabel) return;
      if (state.bounds) {
        setText(els.rangeLabel, '已应用范围 ' + formatRangeTime(state.bounds.from) + ' ~ ' + formatRangeTime(state.bounds.to) +
          '（请求记录受现有保留时长限制，不会补造更早记录）' +
          (state.draftPending ? ' · 时间草稿未应用' : ''));
        els.rangeLabel.className = 'range-label' + (state.draftPending ? ' is-pending' : '');
      } else if (state.range) {
        setText(els.rangeLabel, '待应用范围 ' + formatRangeTime(state.range.from) + ' ~ ' + formatRangeTime(state.range.to));
        els.rangeLabel.className = 'range-label is-pending';
      } else {
        setText(els.rangeLabel, state.draftPending ? '时间草稿未应用' : '');
      }
    }

    function currentDraft() {
      return draftRange(els.timeStart && els.timeStart.value, els.timeEnd && els.timeEnd.value);
    }

    function fillDraft(bounds, pending) {
      if (els.timeStart) els.timeStart.value = isoToLocalInput(bounds.from);
      if (els.timeEnd) els.timeEnd.value = isoToLocalInput(bounds.to);
      state.draftPending = pending === true;
      if (els.timeError) setText(els.timeError, '');
      showRange();
    }

    function markDraftPending() {
      state.draftPending = true;
      if (els.timeError) setText(els.timeError, '');
      showRange();
    }

    function applyDraft() {
      if (saving) return false;
      var draft = currentDraft();
      if (!draft.ok) {
        if (els.timeError) setText(els.timeError, draft.message);
        state.draftPending = true;
        showRange();
        return false;
      }
      state.range = { from: draft.from, to: draft.to };
      state.draftPending = false;
      if (els.timeError) setText(els.timeError, '');
      showRange();
      return true;
    }

    function pageActions() {
      var pending = !state.connected || state.loadingPage || state.refreshing || saving;
      if (els.timeApply) els.timeApply.disabled = !state.connected || saving;
      els.requestsPrev.disabled = pending || state.offset === 0;
      els.requestsNext.disabled = pending || !state.hasMore || state.offset > 2147483647 - REQUEST_PAGE_LIMIT;
      els.identityApply.disabled = pending;
    }

    function applyRequestsPage(page, offset) {
      var p = page && typeof page === 'object' ? page : {};
      var items = Array.isArray(p.items) ? p.items : [];
      state.items = items.slice(0, REQUEST_PAGE_LIMIT);
      state.offset = offset;
      state.hasMore = p.has_more === true;
      renderRequestsTable(doc, els.requestsBody, state.items, openBody, state.credentials, filterByKey);
      updateIdentityOptions();
      updateRequestsNote();
      pageActions();
    }

    function updateRequestsNote() {
      setText(els.requestsPage, '第 ' + (Math.floor(state.offset / REQUEST_PAGE_LIMIT) + 1) + ' 页 · 本页 ' + state.items.length + ' 条');
      setText(els.requestsNote, '请求明细仅保留 ' + formatDuration(state.retention) + '，统计按所选时间范围展示');
    }

    function updateIdentityOptions() {
      var selected = els.auth.value || '';
      var selectedKey = els.clientKey.value || '';
      var authNames = assign(Object.create(null), state.credentials);
      if (selected && selected !== 'unknown' && !authNames[selected]) authNames[selected] = '索引 ' + selected;
      state.authGroups.forEach(function (group) {
        if (/^[a-f0-9]{16}$/.test(toStr(group.id)) && !authNames[group.id]) authNames[group.id] = '索引 ' + group.id;
      });
      state.items.forEach(function (item) {
        if (/^[a-f0-9]{16}$/.test(toStr(item.auth_index)) && !authNames[item.auth_index]) {
          authNames[item.auth_index] = '索引 ' + item.auth_index;
        }
      });
      clearChildren(els.auth);
      els.auth.appendChild(optionEl(doc, '', '全部'));
      els.auth.appendChild(optionEl(doc, 'unknown', '未归属（含旧记录）'));
      Object.keys(authNames).sort().forEach(function (index) {
        els.auth.appendChild(optionEl(doc, index, authNames[index] + ' · ' + index));
      });
      els.auth.value = selected;
      // 保留本次连接已出现的下拉候选，不随当前结果集变化而移除。
      var previousKeyOptions = els.clientKey.options
        ? Array.prototype.slice.call(els.clientKey.options) : [];
      clearChildren(els.clientKey);
      clientKeyOptions(state.clientGroups, state.items, selectedKey, previousKeyOptions).forEach(function (opt) {
        var option = optionEl(doc, opt.value, opt.label);
        if (opt.title) option.title = opt.title;
        els.clientKey.appendChild(option);
      });
      els.clientKey.value = selectedKey;
      selects.refresh();
    }

    function applyIdentityFilter() {
      if (!state.connected || saving || state.refreshing || state.loadingPage) return;
      var key = els.clientKey.value || '';
      var auth = els.auth.value || '';
      if ((key && key !== 'unknown' && !/^[a-f0-9]{64}$/.test(key)) ||
          (auth && auth !== 'unknown' && !/^[a-f0-9]{16}$/.test(auth))) {
        setStatus('筛选值无效，请从下拉列表中选择客户端 key 或 auth', 'error');
        return;
      }
      state.clientKeyID = key;
      state.authIndex = auth;
      refreshAll();
    }

    function filterByKey(key) {
      if (!state.connected || saving || state.refreshing || state.loadingPage) return;
      var value = key || 'unknown';
      // 点击请求/分组身份时为合法但当前缺失的指纹补足 option，避免静默落回「全部」。
      if (value !== 'unknown' && !/^[a-f0-9]{64}$/.test(value)) return;
      if (!selectHasOption(els.clientKey, value)) {
        var option = optionEl(doc, value, value.slice(0, 12) + '…');
        option.title = value;
        els.clientKey.appendChild(option);
      }
      els.clientKey.value = value;
      applyIdentityFilter();
    }

    function renderKeyGroups() {
      var tbody = doc.getElementById('keys-body');
      clearChildren(tbody);
      var auth = doc.getElementById('key-group-kind').value === 'auth';
      var groups = auth ? state.authGroups : state.clientGroups;
      if (!groups.length) { tbody.appendChild(emptyRow(doc, 8, '暂无数据')); return; }
      groups.slice().sort(function (a, b) { return counter(b, 'requests') - counter(a, 'requests'); }).forEach(function (group) {
        var id = toStr(group.id);
        var tr = doc.createElement('tr');
        var name = !id ? '未归属' : auth ? state.credentials[id] || '索引 ' + id : id.slice(0, 12) + '…';
        var td = cell(doc, '', '');
        var button = doc.createElement('button');
        button.type = 'button'; button.className = 'btn btn-secondary btn-sm';
        button.title = id; setText(button, name);
        button.addEventListener('click', function () {
          if (auth) { els.auth.value = id || 'unknown'; applyIdentityFilter(); }
          else filterByKey(id || 'unknown');
        });
        td.appendChild(button); tr.appendChild(td);
        var requests = counter(group, 'requests');
        var cards = buildOverview(group);
        tr.appendChild(cell(doc, 'num', formatInt(requests)));
        tr.appendChild(cell(doc, 'num', formatInt(counter(group, 'failed_requests')) + ' / ' +
          (requests > 0 ? formatRate(counter(group, 'failed_requests') / requests) : '—')));
        tr.appendChild(cell(doc, 'num', cards[2].value + '（入 ' + formatCompact(counter(group, 'input_tokens')) + ' / 出 ' + formatCompact(counter(group, 'output_tokens')) + '）'));
        tr.appendChild(cell(doc, 'num', formatCompact(counter(group, 'cache_read_tokens')) + ' / ' +
          formatCompact(counter(group, 'cache_creation_tokens')) + ' · ' +
          (requests > 0 ? formatRate(counter(group, 'cache_hits') / requests) : '—')));
        tr.appendChild(cell(doc, 'num', cards[4].value));
        tr.appendChild(cell(doc, 'num', cards[5].value));
        tr.appendChild(cell(doc, 'num', aggregateCost(group)));
        tbody.appendChild(tr);
      });
    }

    function applySettings(settings, preserveStatus) {
      if (!dirty && !saving) {
        editor = renderSettings(doc, els.settingsBody, settings, markDirty);
        if (!preserveStatus) setText(els.settingsStatus, '已加载生效设置');
      }
      editorActions();
      state.retention = normalizeSettings(settings).requestRetentionSeconds;
      updateRequestsNote();
    }

    function applyHealth(status) {
      setBanner(buildHealthWarning(status));
      var optionalBytes = function (key) { return status && status[key] != null ? formatBytes(status[key]) : '—'; };
      setText(els.diskStatus, '数据库磁盘 ' + optionalBytes('database_bytes') + ' · 可能可回收 ' +
        optionalBytes('reclaimable_bytes') + ' · 压缩成功 ' + formatInt(status && status.compactions) +
        ' / 失败 ' + formatInt(status && status.compaction_errors) + ' · 上次压缩 ' +
        (status && status.last_compaction_unix > 0 ? formatTime(new Date(status.last_compaction_unix * 1000)) : '—') +
        ' · 上次回收 ' + optionalBytes('last_compaction_reclaimed_bytes') +
        '（健康快照 ' + formatTime(new Date()) + '；加载失败时可能过期）');
    }

    function refreshAll(preserveSettingsStatus) {
      if (!state.connected || saving || !state.range) return;
      abortQuery();
      abortMore();
      var controller = newController();
      queryAbort = controller;
      var token = queryGate.begin();
      var bounds = { from: state.range.from, to: state.range.to };
      state.refreshing = true;
      pageActions();
      showRange();
      setStatus('加载中…', '');

      var common = { from: bounds.from, to: bounds.to, provider: state.provider, model: state.model };
      common.client_key_id = state.clientKeyID;
      common.auth_index = state.authIndex;
      var signal = controller ? controller.signal : undefined;
      var tasks = [
        fetchJson('summary', common, signal).then(function (d) { return { kind: 'summary', data: d }; }),
        fetchJson('requests', assign({}, common, { offset: 0, limit: REQUEST_PAGE_LIMIT }), signal).then(function (d) { return { kind: 'requests', data: d }; }),
        fetchJson('settings', null, signal).then(function (d) { return { kind: 'settings', data: d }; }),
        fetchJson('health', null, signal).then(function (d) { return { kind: 'health', data: d }; }),
        fetchJson('credentials', null, signal).then(function (d) {
          return { kind: 'credentials', data: credentialNames(d) };
        }).catch(function (err) {
          if (err.auth || isAbortError(err)) throw err;
          return { kind: 'credentials', data: null };
        })
      ];

      Promise.all(tasks)
        .then(function (results) {
          if (!state.connected || !queryGate.current(token)) return;
          var byKind = {};
          for (var i = 0; i < results.length; i++) byKind[results[i].kind] = results[i].data;
          applySummary(byKind.summary);
          state.bounds = bounds;
          showRange();
          state.requestProvider = common.provider;
          state.requestModel = common.model;
          state.requestClientKeyID = state.clientKeyID;
          state.requestAuthIndex = state.authIndex;
          state.credentials = byKind.credentials || Object.create(null);
          setText(els.credentialStatus, byKind.credentials ? '' : 'CPA 凭据名称加载失败，本次仅按索引显示；auth 筛选仍可用。');
          state.refreshing = false;
          applyRequestsPage(byKind.requests, 0);
          renderKeyGroups();
          applySettings(byKind.settings, preserveSettingsStatus);
          setStatus('已更新 ' + formatTime(new Date().toISOString()), 'ok');
          applyHealth(byKind.health);
        })
        .catch(function (err) {
          if (isAbortError(err) || !queryGate.current(token)) return;
          if (err && err.auth) { handleAuthFailure(err); return; }
          state.refreshing = false;
          pageActions();
          showRange();
          setStatus('加载失败：' + ((err && err.message) || '未知错误'), 'error');
        });
    }

    async function saveSettings() {
      if (!state.connected || !editor || saving || confirmationResolve) return;
      var approvalToken = connectionGate.begin();
      if (!await askConfirmation('确认保存设置', '捕获请求体可能保存提示词、代码等敏感内容；缩短保留时间会在清理后永久删除旧数据，无法恢复。历史请求与统计将按当前生效价格重新计算。')) return;
      if (!state.connected || !connectionGate.current(approvalToken)) return;
      var patch;
      try { patch = editor.read(); }
      catch (err) { setText(els.settingsStatus, '未保存：' + err.message); return; }
      abortQuery(); queryGate.invalidate();
      state.refreshing = false; abortMore();
      var token = connectionGate.begin();
      var controller = newController();
      saveAbort = controller;
      var signal = controller ? controller.signal : undefined;
      saving = true; editorActions();
      setText(els.settingsStatus, '正在校验并保存…');
      saveTimer = win.setTimeout(function () {
        if (!connectionGate.current(token)) return;
        if (controller) controller.abort();
        connectionGate.invalidate();
        saving = false; saveAbort = null; saveTimer = null; editorActions();
        setText(els.settingsStatus, '保存确认超时（可能已写入），草稿保留；请重新加载核对。');
      }, 15000);
      var written = false;
      var guard = function () {
        if (!state.connected || !connectionGate.current(token) || (signal && signal.aborted)) {
          var err = new Error('aborted'); err.name = 'AbortError'; throw err;
        }
      };
      function retryPause() {
        return new Promise(function (resolve, reject) {
          if (signal && signal.aborted) { reject({ name: 'AbortError' }); return; }
          var onAbort = function () { win.clearTimeout(timer); reject({ name: 'AbortError' }); };
          var timer = win.setTimeout(function () {
            if (signal) signal.removeEventListener('abort', onAbort);
            resolve();
          }, 500);
          if (signal) signal.addEventListener('abort', onAbort, { once: true });
        });
      }
      function confirmEffective(attempt) {
        guard();
        return Promise.all([fetchJson('config', null, signal), fetchJson('settings', null, signal)])
          .then(function (values) {
            guard();
            var config = {};
            Object.keys(patch).forEach(function (key) { config[key] = values[0][key]; });
            if (patchesMatch(patch, config) && patchesMatch(patch, settingsToPatch(values[1]))) return values[1];
            if (attempt >= 20) throw new Error('回读配置或生效设置仍与保存值不一致');
            return retryPause().then(function () { return confirmEffective(attempt + 1); });
          }, function (err) {
            guard();
            if (err.status === 503 && attempt < 20) return retryPause().then(function () { return confirmEffective(attempt + 1); });
            throw err;
          });
      }
      fetchJson('validate', null, signal, 'POST', patch)
        .then(function (result) {
          guard();
          if (result.valid !== true) throw new Error('服务端未确认校验通过');
          return fetchJson('config', null, signal, 'PATCH', patch);
        })
        .then(function (result) {
          guard();
          if (result.status !== 'ok') throw new Error('服务端未确认写入成功');
          written = true;
          setText(els.settingsStatus, '已提交，正在回读持久配置并等待生效…');
          return confirmEffective(0);
        })
        .then(function (settings) {
          guard(); saving = false; dirty = false;
          applySettings(settings);
          setText(els.settingsStatus, '保存成功：宿主持久配置与生效设置已确认');
          win.clearTimeout(saveTimer); saveTimer = null;
          saveAbort = null;
          refreshAll(true);
        })
        .catch(function (err) {
          if (!connectionGate.current(token) || isAbortError(err)) return;
          saving = false; saveAbort = null; editorActions();
          setText(els.settingsStatus, (written ? '已提交但未能确认生效，请重新加载核对：' : '保存未确认（可能已写入，请重新加载核对；草稿保留）：') + err.message);
          win.clearTimeout(saveTimer); saveTimer = null;
          if (err.auth) handleAuthFailure(err);
        });
    }

    async function reloadSettings() {
      if (!state.connected || saving || confirmationResolve) return;
      var approvalToken = connectionGate.begin();
      if (dirty && !await askConfirmation('放弃未保存草稿', '放弃未保存草稿并重新加载宿主设置？')) return;
      if (!state.connected || !connectionGate.current(approvalToken)) return;
      var token = connectionGate.begin();
      abortQuery(); queryGate.invalidate();
      state.refreshing = false; abortMore();
      saving = true; editorActions();
      saveAbort = newController();
      var signal = saveAbort ? saveAbort.signal : undefined;
      setText(els.settingsStatus, '正在重新加载宿主配置与生效设置…');
      saveTimer = win.setTimeout(function () {
        if (!connectionGate.current(token)) return;
        if (saveAbort) saveAbort.abort();
        connectionGate.invalidate();
        saving = false; saveAbort = null; saveTimer = null; editorActions();
        setText(els.settingsStatus, '重新加载超时（草稿保留），请重试。');
      }, 15000);
      Promise.all([fetchJson('config', null, signal), fetchJson('settings', null, signal)])
        .then(function (values) {
          if (!connectionGate.current(token)) return;
          var loaded = validateSettingsPatch(settingsToPatch(values[1]));
          var savedConfig = {};
          Object.keys(loaded).forEach(function (key) { savedConfig[key] = values[0][key]; });
          win.clearTimeout(saveTimer); saveTimer = null;
          saving = false; dirty = false; saveAbort = null;
          applySettings(values[1]);
          setText(els.settingsStatus, patchesMatch(loaded, savedConfig)
            ? '已重新加载：宿主持久配置与当前生效设置一致'
            : '已重新加载当前生效设置；宿主持久配置与其不一致（或使用默认值），请稍后重新加载核对。');
        })
        .catch(function (err) {
          if (!connectionGate.current(token) || isAbortError(err)) return;
          saving = false; saveAbort = null; editorActions();
          setText(els.settingsStatus, '重新加载失败（草稿保留）：' + err.message);
          win.clearTimeout(saveTimer); saveTimer = null;
          if (err.auth) handleAuthFailure(err);
        });
    }

    function navigatePage(offset) {
      if (!state.connected || saving || state.refreshing || state.loadingPage || !state.bounds ||
          offset < 0 || offset > 2147483647 || (offset > state.offset && !state.hasMore)) return;
      state.loadingPage = true;
      pageActions();
      var token = moreGate.begin();
      var controller = newController();
      moreAbort = controller;
      var params = {
        from: state.bounds.from,
        to: state.bounds.to,
        provider: state.requestProvider,
        model: state.requestModel,
        client_key_id: state.requestClientKeyID,
        auth_index: state.requestAuthIndex,
        limit: REQUEST_PAGE_LIMIT,
        offset: offset
      };
      fetchJson('requests', params, controller ? controller.signal : undefined)
        .then(function (page) {
          if (!state.connected || !moreGate.current(token)) return;
          applyRequestsPage(page, offset);
          setStatus('已更新 ' + formatTime(new Date().toISOString()), 'ok');
        })
        .catch(function (err) {
          if (isAbortError(err) || !moreGate.current(token)) return;
          if (err && err.auth) { handleAuthFailure(err); return; }
          setStatus('翻页失败：' + ((err && err.message) || '未知错误'), 'error');
        })
        .then(function () {
          if (!moreGate.current(token)) return;
          state.loadingPage = false;
          pageActions();
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
      if (!state.connected || !requestId) return;
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
      if (!state.connected || bodyState.phase !== 'confirm') return;
      var requestId = bodyState.requestId;
      bodyState = bodyViewerReduce(bodyState, { type: 'confirm', requestId: requestId });
      var token = bodyGate.begin();
      var controller = newController();
      bodyAbort = controller;
      renderBodyDialog();
      fetchJson('body', { request_id: requestId }, controller ? controller.signal : undefined)
        .then(function (detail) {
          if (!state.connected || !bodyGate.current(token)) return;
          var d = validateBodyDetail(detail, requestId);
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
            return;
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
      clearBodyViewer();
      if (els.dialog.open && typeof els.dialog.close === 'function') {
        els.dialog.close();
      }
    }

    function connect() {
      var key = els.key && els.key.value ? els.key.value.trim() : '';
      if (!isAllowedTransport(loc)) {
        disconnect();
        setStatus('仅允许 HTTPS 或本机回环地址发送管理密钥', 'error');
        return;
      }
      if (!key) {
        disconnect();
        setStatus('请输入管理密钥', 'error');
        if (els.key && typeof els.key.focus === 'function') els.key.focus();
        return;
      }
      if (state.connected && state.range && !state.draftPending) {
        /* 重连沿用当前已验证范围，不在保存中的异步流程里重读可能失焦的输入。 */
      } else if (!applyDraft()) {
        setStatus(currentDraft().message || '请先填写有效时间范围', 'error');
        return;
      }
      var applied = { from: state.range.from, to: state.range.to };
      disconnect();
      state.bounds = null;
      state.range = applied;
      state.offset = 0;
      state.draftPending = false;
      showRange();
      state.key = key;
      state.connected = true;
      state.offset = 0;
      state.items = [];
      state.hasMore = false;
      state.clientKeyID = ''; state.authIndex = '';
      state.credentials = Object.create(null);
      els.clientKey.value = ''; els.auth.value = '';
      state.clientGroups = []; state.authGroups = [];
      // 新连接不得保留上一个连接的候选 key。
      clearChildren(els.clientKey);
      updateIdentityOptions();
      renderRequestsTable(doc, els.requestsBody, [], openBody);
      updateRequestsNote();
      dirty = false; editor = null;
      clearChildren(els.settingsBody);
      setText(els.settingsStatus, '正在加载新连接设置…');
      els.refresh.disabled = false;
      setBanner('');
      refreshAll();
    }

    // ---- 事件绑定 ----

    els.connect.addEventListener('click', connect);
    els.identityApply.addEventListener('click', applyIdentityFilter);
    doc.getElementById('key-group-kind').addEventListener('change', renderKeyGroups);
    if (els.save) els.save.addEventListener('click', saveSettings);
    if (els.revert) els.revert.addEventListener('click', reloadSettings);
    els.refresh.addEventListener('click', function () { refreshAll(); });
    els.requestsPrev.addEventListener('click', function () { navigatePage(state.offset - REQUEST_PAGE_LIMIT); });
    els.requestsNext.addEventListener('click', function () { navigatePage(state.offset + REQUEST_PAGE_LIMIT); });
    els.bodyReveal.addEventListener('click', revealBody);
    els.bodyClose.addEventListener('click', closeBody);
    els.dialog.addEventListener('close', function () { if (!els.dialog.open) clearBodyViewer(); });
    els.dialog.addEventListener('cancel', clearBodyViewer);

    var segButtons = doc.querySelectorAll ? doc.querySelectorAll('.seg-btn') : [];
    var bindSeg = function (button) {
      button.addEventListener('click', function () {
        var range = button.getAttribute('data-range');
        if (!RANGE_MS[range]) return;
        fillDraft(rangeBounds(range, Date.now()), true);
        for (var i = 0; i < segButtons.length; i++) {
          if (segButtons[i] === button) segButtons[i].className = 'btn seg-btn is-active';
          else segButtons[i].className = 'btn seg-btn';
        }
      });
    };
    for (var s = 0; s < segButtons.length; s++) bindSeg(segButtons[s]);
    fillDraft(rangeBounds(DEFAULT_RANGE, Date.now()), false);
    if (els.timeStart) els.timeStart.addEventListener('input', markDraftPending);
    if (els.timeEnd) els.timeEnd.addEventListener('input', markDraftPending);
    if (els.timeApply) els.timeApply.addEventListener('click', function () {
      if (saving || !applyDraft()) return;
      if (state.connected) refreshAll();
    });

    var onFilterChange = function () {
      state.provider = els.provider.value || '';
      state.model = els.model.value || '';
      if (state.connected) refreshAll();
    };
    els.provider.addEventListener('change', onFilterChange);
    els.model.addEventListener('change', onFilterChange);

    var resizeTimer = null;
    win.addEventListener('resize', function () {
      if (confirmation.open) positionConfirmation();
      if (resizeTimer) win.clearTimeout(resizeTimer);
      resizeTimer = win.setTimeout(function () {
        if (state.connected) renderChart(els.chart, state.series || []);
      }, 150);
    });

    win.addEventListener('beforeunload', function () {
      disconnect();
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
    selects.refresh();
  }

  var api = {
    PLUGIN_ID: PLUGIN_ID,
    DEFAULT_API_BASE: DEFAULT_API_BASE,
    RANGE_MS: RANGE_MS,
    DEFAULT_RANGE: DEFAULT_RANGE,
    REQUEST_PAGE_LIMIT: REQUEST_PAGE_LIMIT,
    validateBodyDetail: validateBodyDetail,
    deriveApiBase: deriveApiBase,
    coreApiBase: coreApiBase,
    credentialsUrl: credentialsUrl,
    credentialNames: credentialNames,
    durationSeconds: durationSeconds,
    settingsToPatch: settingsToPatch,
    validateSettingsPatch: validateSettingsPatch,
    patchesMatch: patchesMatch,
    normalizePriceRules: normalizePriceRules,
    buildApiUrl: buildApiUrl,
    isAllowedTransport: isAllowedTransport,
    isLoopbackHost: isLoopbackHost,
    normalizeTheme: normalizeTheme,
    resolveThemeSource: resolveThemeSource,
    rangeBounds: rangeBounds,
    clientKeyOptions: clientKeyOptions,
    formatInt: formatInt,
    formatCompact: formatCompact,
    formatCost: formatCost,
    formatLatency: formatLatency,
    formatTps: formatTps,
    formatRate: formatRate,
    formatBytes: formatBytes,
    formatDuration: formatDuration,
    formatTime: formatTime,
    formatRangeTime: formatRangeTime,
    localInputToIso: localInputToIso,
    isoToLocalInput: isoToLocalInput,
    draftRange: draftRange,
    cacheReadShare: cacheReadShare,
    requestHitRate: requestHitRate,
    createSelectController: createSelectController,
    refreshSelects: function (win, doc) {
      if (win && win.__observerSelects) win.__observerSelects.refresh();
    },
    truncateText: truncateText,
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
