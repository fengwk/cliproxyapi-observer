/* Dependency-free Observer settings/pricing editor. All dynamic text uses textContent. */
(function () {
  "use strict";

  var MAX_SAFE_INPUT = 9000000000000000;
  // DESIGN caps each captured body at 8 MiB.
  var MAX_BODY_BYTES = 8 * 1024 * 1024;
  var MAX_BODY_STORAGE_BYTES = 1073741824;
  var DEFAULT_MAX_BODY_BYTES = 1048576;
  var DEFAULT_MAX_BODY_STORAGE_BYTES = 268435456;
  var MAX_CONFIG_ENTRIES = 4096;

  var RATE_FIELDS = [
    { key: "input", label: "输入（USD / 百万 Token）", optional: false },
    { key: "output", label: "输出（USD / 百万 Token）", optional: false },
    { key: "cache_read", label: "缓存读（USD / 百万 Token）", optional: false },
    {
      key: "cache_write",
      label: "缓存写（USD / 百万 Token）",
      optional: false,
    },
    {
      key: "reasoning",
      label: "推理（USD / 百万 Token，可选）",
      optional: true,
    },
  ];

  var instanceSequence = 0;
  var DEFAULT_PRICING = { auto: true, prices: [], rules: [] };

  function ObserverSettings(host, data, prices, health, api, reload) {
    if (!host || !host.ownerDocument) {
      throw new Error("ObserverSettings 需要有效的宿主元素");
    }
    var doc = host.ownerDocument;
    var win =
      doc.defaultView || (typeof window !== "undefined" ? window : null);
    var UI = win && win.ObserverUI ? win.ObserverUI : null;
    if (!UI && typeof ObserverUI !== "undefined" && ObserverUI) UI = ObserverUI;
    var uid = "observer-settings-" + ++instanceSequence;
    var manualSequence = 0;
    var ruleSequence = 0;
    var savedRuleSequence = 0;

    var settingsData = unwrapObject(data, "settings");
    var pricingData = unwrapObject(settingsData.pricing, "pricing");
    var snapshotData = unwrapObject(prices, "snapshot");
    var healthData = unwrapObject(health, "health");

    var state = {
      base: normalizeSettings(settingsData),
      snapshot: normalizeSnapshot(snapshotData),
      health: normalizeHealth(healthData),
      manualRows: [],
      ruleRows: [],
      busy: false,
    };
    state.base.pricing = normalizePricing(pricingData);
    var basePricing = clone(state.base.pricing);

    // -----------------------------------------------------------------------
    // Minimal DOM/helper bridge
    // -----------------------------------------------------------------------

    function applyAttrs(node, attrs) {
      if (!attrs) return node;
      Object.keys(attrs).forEach(function (key) {
        var value = attrs[key];
        if (value === null || value === undefined || value === false) return;
        if (key === "class" || key === "className")
          node.className = String(value);
        else if (key === "for" || key === "htmlFor")
          node.htmlFor = String(value);
        else if (key === "value") node.value = String(value);
        else if (key === "checked") node.checked = value === true;
        else node.setAttribute(key, String(value));
      });
      return node;
    }

    function el(tag, text, attrs) {
      if (UI && typeof UI.el === "function") {
        return UI.el(tag, text === undefined ? "" : text, attrs || {});
      }
      var node = doc.createElement(tag);
      if (text !== undefined && text !== null) node.textContent = String(text);
      applyAttrs(node, attrs);
      return node;
    }

    function field(label, input) {
      var wrapper;
      if (UI && typeof UI.field === "function") {
        wrapper = UI.field(label, input);
      }
      if (!wrapper || wrapper.nodeType !== 1) {
        wrapper = doc.createElement("label");
        wrapper.className = "setting-field";
        var caption = doc.createElement("span");
        caption.textContent = String(label);
        wrapper.appendChild(caption);
      }
      if (input && input.parentNode !== wrapper && wrapper.nodeType === 1) {
        wrapper.appendChild(input);
      }
      return wrapper;
    }

    function moneyInput(value) {
      var input;
      if (UI && typeof UI.moneyInput === "function") {
        input = UI.moneyInput(value);
      }
      if (!input || input.nodeType !== 1) {
        input = doc.createElement("input");
        input.type = "number";
        input.value =
          value === null || value === undefined || value === ""
            ? ""
            : String(Number(value) / 1000000);
      }
      input.setAttribute("min", "0");
      input.setAttribute("step", "0.000001");
      input.setAttribute("inputmode", "decimal");
      input.setAttribute("autocomplete", "off");
      return input;
    }

    function addClass(node, className) {
      if (!node || node.nodeType !== 1 || !className) return;
      var current = node.className ? String(node.className).split(/\s+/) : [];
      var additions = String(className).split(/\s+/);
      for (var i = 0; i < additions.length; i++) {
        if (additions[i] && current.indexOf(additions[i]) < 0)
          current.push(additions[i]);
      }
      node.className = current.join(" ").replace(/^\s+|\s+$/g, "");
    }

    function text(node, value) {
      node.textContent =
        value === null || value === undefined ? "" : String(value);
      return node;
    }

    function setStatus(node, message, failed) {
      text(node, message);
      node.className = failed
        ? "settings-note settings-error"
        : "settings-note";
      node.setAttribute("role", failed ? "alert" : "status");
      node.setAttribute("aria-live", failed ? "assertive" : "polite");
    }

    function errorMessage(err) {
      if (!err) return "操作失败";
      if (typeof err === "string") return err;
      if (err.error && typeof err.error.message === "string")
        return err.error.message;
      if (typeof err.message === "string" && err.message) return err.message;
      return "操作失败";
    }

    function clone(value) {
      if (value === undefined) return undefined;
      return JSON.parse(JSON.stringify(value));
    }

    function unwrapObject(value, key) {
      if (value && typeof value === "object") {
        if (
          value[key] &&
          typeof value[key] === "object" &&
          !Array.isArray(value[key])
        ) {
          return value[key];
        }
        return value;
      }
      return {};
    }

    // -----------------------------------------------------------------------
    // Strict conversions
    // -----------------------------------------------------------------------

    // USD per million tokens -> integer micro-USD. Only up to six decimal
    // places are accepted so the stored integer is exact, never a float.
    function strictUSDToMicro(raw, label) {
      var value = String(raw === null || raw === undefined ? "" : raw).replace(
        /^\s+|\s+$/g,
        "",
      );
      var match = /^(\d+)(?:\.(\d{1,6}))?$/.exec(value);
      if (!match) {
        throw new Error(label + "必须是 USD 金额（最多 6 位小数，不能为负数）");
      }
      var whole = Number(match[1]);
      if (
        !Number.isSafeInteger(whole) ||
        whole > Math.floor(MAX_SAFE_INPUT / 1000000)
      ) {
        throw new Error(label + "超出可精确表示的金额范围");
      }
      var fraction = match[2] || "";
      while (fraction.length < 6) fraction += "0";
      var micro = whole * 1000000 + Number(fraction || "0");
      if (!Number.isSafeInteger(micro) || micro < 0 || micro > MAX_SAFE_INPUT) {
        throw new Error(label + "超出可精确表示的金额范围");
      }
      return micro;
    }

    function readMoney(input, label, optional) {
      var raw =
        input && input.value !== null && input.value !== undefined
          ? String(input.value).replace(/^\s+|\s+$/g, "")
          : "";
      if (raw === "") {
        if (optional) return null;
        throw new Error(label + "不能为空");
      }
      var expected = strictUSDToMicro(raw, label);
      var micro = expected;
      if (UI && typeof UI.micro === "function") {
        try {
          micro = UI.micro(raw);
        } catch (err) {
          throw new Error(label + "：" + errorMessage(err));
        }
        if (typeof micro === "bigint") micro = Number(micro);
        else if (typeof micro === "string" && /^\d+$/.test(micro))
          micro = Number(micro);
      }
      if (
        typeof micro !== "number" ||
        !Number.isSafeInteger(micro) ||
        micro < 0 ||
        micro > MAX_SAFE_INPUT
      ) {
        throw new Error(label + "必须转换为整数 micro-USD");
      }
      if (micro !== expected) {
        throw new Error(label + "无法精确换算为整数 micro-USD");
      }
      return micro;
    }

    function readInteger(input, label, min, max, optional) {
      var raw =
        input && input.value !== null && input.value !== undefined
          ? String(input.value).replace(/^\s+|\s+$/g, "")
          : "";
      if (raw === "") {
        if (optional) return null;
        throw new Error(label + "不能为空");
      }
      if (!/^\d+$/.test(raw)) throw new Error(label + "必须是非负整数");
      var value = Number(raw);
      if (!Number.isSafeInteger(value) || value < min || value > max) {
        throw new Error(label + "必须在 " + min + " 至 " + max + " 之间");
      }
      return value;
    }

    function validTimeRange(value) {
      var match = /^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$/.exec(
        value,
      );
      return !!match && value.slice(0, 5) !== value.slice(6);
    }

    // -----------------------------------------------------------------------
    // Data normalization
    // -----------------------------------------------------------------------

    function normalizeSettings(raw) {
      var source =
        raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
      return {
        capture_enabled: source.capture_enabled === true,
        max_body_bytes: integerOr(
          source.max_body_bytes,
          DEFAULT_MAX_BODY_BYTES,
        ),
        max_body_storage_bytes: integerOr(
          source.max_body_storage_bytes,
          DEFAULT_MAX_BODY_STORAGE_BYTES,
        ),
        raw_retention_hours: integerOr(source.raw_retention_hours, 24),
        daily_retention_days: integerOr(source.daily_retention_days, 365),
        pricing: normalizePricing(source.pricing),
      };
    }

    // Merge a server response into the last confirmed settings. Empty or
    // partial responses must not silently reset unrelated fields.
    function mergeSettingsResponse(raw, current) {
      if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
      var next = clone(current);
      if (Object.prototype.hasOwnProperty.call(raw, "capture_enabled")) {
        next.capture_enabled = raw.capture_enabled === true;
      }
      if (Object.prototype.hasOwnProperty.call(raw, "max_body_bytes")) {
        next.max_body_bytes = integerOr(
          raw.max_body_bytes,
          next.max_body_bytes,
        );
      }
      if (Object.prototype.hasOwnProperty.call(raw, "max_body_storage_bytes")) {
        next.max_body_storage_bytes = integerOr(
          raw.max_body_storage_bytes,
          next.max_body_storage_bytes,
        );
      }
      if (Object.prototype.hasOwnProperty.call(raw, "raw_retention_hours")) {
        next.raw_retention_hours = integerOr(
          raw.raw_retention_hours,
          next.raw_retention_hours,
        );
      }
      if (Object.prototype.hasOwnProperty.call(raw, "daily_retention_days")) {
        next.daily_retention_days = integerOr(
          raw.daily_retention_days,
          next.daily_retention_days,
        );
      }
      if (
        raw.pricing &&
        typeof raw.pricing === "object" &&
        !Array.isArray(raw.pricing)
      ) {
        next.pricing = normalizePricing(raw.pricing);
      }
      return next;
    }

    function integerOr(value, fallback) {
      var n = Number(value);
      return isFinite(n) && n >= 0 && Number.isSafeInteger(n) ? n : fallback;
    }

    function normalizePricing(raw) {
      var source =
        raw && typeof raw === "object" && !Array.isArray(raw)
          ? raw
          : DEFAULT_PRICING;
      var prices = Array.isArray(source.prices) ? source.prices : [];
      var rules = Array.isArray(source.rules) ? source.rules : [];
      return {
        auto: source.auto !== false,
        prices: prices.map(function (price) {
          var item = price && typeof price === "object" ? price : {};
          return {
            model: item.model === undefined ? "" : String(item.model),
            rates: normalizeRates(item.rates),
            source: item.source === undefined ? "manual" : String(item.source),
          };
        }),
        rules: rules.map(function (rule) {
          var item = rule && typeof rule === "object" ? rule : {};
          var result = {
            id: item.id === undefined ? "" : String(item.id),
            model: item.model === undefined ? "" : String(item.model),
            rates: normalizeRates(item.rates),
          };
          if (
            item.input_tokens_gt !== undefined &&
            item.input_tokens_gt !== null
          ) {
            result.input_tokens_gt = Number(item.input_tokens_gt);
          }
          if (
            item.time_range !== undefined &&
            item.time_range !== null &&
            item.time_range !== ""
          ) {
            result.time_range = String(item.time_range);
          }
          return result;
        }),
      };
    }

    function normalizeRates(raw) {
      var source = raw && typeof raw === "object" ? raw : {};
      var result = {
        input: integerOr(source.input, 0),
        output: integerOr(source.output, 0),
        cache_read: integerOr(source.cache_read, 0),
        cache_write: integerOr(source.cache_write, 0),
      };
      if (source.reasoning !== undefined && source.reasoning !== null) {
        result.reasoning = integerOr(source.reasoning, 0);
      }
      return result;
    }

    function normalizeSnapshot(raw) {
      var source = raw && typeof raw === "object" ? raw : {};
      return {
        prices: Array.isArray(source.prices)
          ? source.prices.map(normalizeSnapshotPrice)
          : [],
        last_sync: source.last_sync,
        error: source.error ? String(source.error) : "",
        refreshing: source.refreshing === true,
      };
    }

    // Snapshot rows are read-only. Provider is optional and only present for
    // provider-specific catalog rates; it never participates in manual config.
    function normalizeSnapshotPrice(raw) {
      var item =
        raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
      return {
        model: item.model === undefined ? "" : String(item.model),
        provider:
          item.provider === undefined || item.provider === null
            ? ""
            : String(item.provider),
        source: item.source === undefined ? "" : String(item.source),
        rates: normalizeRates(item.rates),
      };
    }

    function normalizeHealth(raw) {
      var source = raw && typeof raw === "object" ? raw : {};
      return {
        database_bytes: Number(source.database_bytes) || 0,
        dropped_captures: Number(source.dropped_captures) || 0,
        pending_usage: Number(source.pending_usage) || 0,
        active_requests: Number(source.active_requests) || 0,
        last_error: source.last_error ? String(source.last_error) : "",
      };
    }

    function formatInt(value) {
      var n = Number(value);
      if (!isFinite(n)) return "—";
      return Math.round(n).toLocaleString("zh-CN");
    }

    function formatBytes(value) {
      var n = Number(value);
      if (!isFinite(n) || n < 0) return "—";
      if (n < 1024) return n + " B";
      if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
      if (n < 1073741824) return (n / 1048576).toFixed(1) + " MB";
      return (n / 1073741824).toFixed(2) + " GB";
    }

    function formatRate(value) {
      var n = Number(value);
      if (!isFinite(n) || n < 0) return "—";
      var usd = (n / 1000000).toFixed(6).replace(/\.?0+$/, "");
      return "$" + (usd || "0");
    }

    function sourceLabel(value) {
      var source = value === null || value === undefined ? "" : String(value);
      if (source === "manual") return "手工";
      if (source === "auto") return "自动目录";
      if (source === "rule") return "规则";
      if (source === "unpriced") return "未定价";
      return source || "—";
    }

    function formatTime(value) {
      if (!value) return "尚未同步";
      var date = new Date(value);
      if (!isFinite(date.getTime()) || date.getUTCFullYear() <= 1)
        return "尚未同步";
      return date
        .toISOString()
        .replace("T", " ")
        .replace(/\.\d+Z$/, " UTC");
    }

    // -----------------------------------------------------------------------
    // Shared UI pieces
    // -----------------------------------------------------------------------

    function section(title, titleId) {
      var wrapper = el("section", "", {
        class: "card settings",
        "aria-labelledby": titleId,
      });
      var header = el("div", "", { class: "card-header" });
      header.appendChild(el("h2", title, { id: titleId }));
      wrapper.appendChild(header);
      return wrapper;
    }

    function numberField(grid, idBase, labelText, value, min, max, hint) {
      var input = el("input", "", {
        type: "number",
        id: uid + "-" + idBase,
        min: String(min),
        max: String(max),
        step: "1",
        inputmode: "numeric",
        autocomplete: "off",
      });
      input.value = String(value);
      var wrapper = field(labelText, input);
      addClass(wrapper, "setting-field");
      if (hint) {
        var hintNode = el("span", hint, {
          class: "muted",
          id: uid + "-" + idBase + "-hint",
        });
        wrapper.appendChild(hintNode);
        input.setAttribute("aria-describedby", uid + "-" + idBase + "-hint");
      }
      grid.appendChild(wrapper);
      return input;
    }

    function actionButton(label, className, testId) {
      return el("button", label, {
        type: "button",
        class: className || "btn btn-secondary",
        "data-testid": testId || "",
      });
    }

    function callApi(path, options) {
      var promise;
      try {
        promise = api(path, options || {});
      } catch (err) {
        return Promise.reject(err);
      }
      return Promise.resolve(promise);
    }

    function invokeReload() {
      if (typeof reload !== "function") return;
      try {
        reload();
      } catch (err) {
        // A refresh failure must not hide the successful write.
      }
    }

    // -----------------------------------------------------------------------
    // General settings: every capture/limit/retention field
    // -----------------------------------------------------------------------

    function renderGeneral() {
      var wrapper = section("采集、限制与保留", uid + "-general-heading");
      var form = el("form", "", {
        class: "settings-form",
        novalidate: "novalidate",
      });

      var captureInput = el("input", "", {
        type: "checkbox",
        id: uid + "-capture-enabled",
        "data-testid": "settings-capture-enabled",
      });
      captureInput.checked = state.base.capture_enabled;
      var captureField = field(
        "显式捕获请求体（可能包含提示词、代码等敏感内容）",
        captureInput,
      );
      addClass(captureField, "setting-capture");
      form.appendChild(captureField);

      var captureNote = el("p", "", {
        class: "settings-note",
        id: uid + "-capture-note",
        role: "status",
        "aria-live": "polite",
      });
      captureInput.setAttribute("aria-describedby", uid + "-capture-note");
      form.appendChild(captureNote);

      function updateCaptureNote() {
        text(
          captureNote,
          captureInput.checked
            ? "请求体捕获已开启：仅对保存后的新请求生效；内容可能包含提示词、代码等敏感信息，并受保留期限制。"
            : "请求体捕获已关闭：只记录元数据，不保存提示词或响应内容。",
        );
      }
      captureInput.addEventListener("change", updateCaptureNote);
      updateCaptureNote();

      var grid = el("div", "", { class: "settings-fields" });
      var maxBodyInput = numberField(
        grid,
        "max-body-bytes",
        "单个请求体上限（字节）",
        state.base.max_body_bytes,
        1,
        MAX_BODY_BYTES,
        "1 MiB = 1,048,576 字节；上限 8 MiB = 8,388,608 字节",
      );
      var maxStorageInput = numberField(
        grid,
        "max-body-storage-bytes",
        "请求体总存储上限（字节）",
        state.base.max_body_storage_bytes,
        1,
        MAX_BODY_STORAGE_BYTES,
        "请求体以 gzip 压缩保存；所有已捕获请求体的总量上限 1 GiB = 1,073,741,824 字节",
      );
      var rawRetentionInput = numberField(
        grid,
        "raw-retention-hours",
        "原始明细保留（小时）",
        state.base.raw_retention_hours,
        1,
        168,
        "1–168 小时；读取时同样执行过期检查",
      );
      var dailyRetentionInput = numberField(
        grid,
        "daily-retention-days",
        "每日汇总保留（天）",
        state.base.daily_retention_days,
        1,
        3650,
        "1–3650 天；不会删除独立 Key 的已用额度",
      );
      form.appendChild(grid);

      var note = el(
        "p",
        "保存只修改采集、限制与保留字段；当前已保存的价格配置会原样保留。缩短保留期会在清理后永久删除旧数据。",
        { class: "settings-note" },
      );
      form.appendChild(note);

      var actions = el("div", "", { class: "settings-actions" });
      var save = el("button", "保存采集与保留", {
        type: "submit",
        class: "btn btn-primary",
        "data-testid": "settings-general-save",
      });
      var reloadButton = actionButton(
        "重新加载 / 放弃草稿",
        "btn btn-secondary",
        "settings-general-reload",
      );
      actions.appendChild(save);
      actions.appendChild(reloadButton);
      var status = el("span", "", {
        class: "settings-note",
        id: uid + "-general-status",
        role: "status",
        "aria-live": "polite",
      });
      actions.appendChild(status);
      form.appendChild(actions);

      function readGeneral() {
        var capture = captureInput.checked === true;
        var maxBody = readInteger(
          maxBodyInput,
          "单个请求体上限",
          1,
          MAX_BODY_BYTES,
          false,
        );
        var maxStorage = readInteger(
          maxStorageInput,
          "请求体总存储上限",
          1,
          MAX_BODY_STORAGE_BYTES,
          false,
        );
        var rawRetention = readInteger(
          rawRetentionInput,
          "原始明细保留",
          1,
          168,
          false,
        );
        var dailyRetention = readInteger(
          dailyRetentionInput,
          "每日汇总保留",
          1,
          3650,
          false,
        );
        if (maxBody > maxStorage) {
          throw new Error("单个请求体上限不能超过请求体总存储上限");
        }
        return {
          capture_enabled: capture,
          max_body_bytes: maxBody,
          max_body_storage_bytes: maxStorage,
          raw_retention_hours: rawRetention,
          daily_retention_days: dailyRetention,
        };
      }

      form.addEventListener("submit", function (event) {
        event.preventDefault();
        var general;
        try {
          general = readGeneral();
        } catch (err) {
          setStatus(status, errorMessage(err), true);
          return;
        }
        var payload = {
          capture_enabled: general.capture_enabled,
          max_body_bytes: general.max_body_bytes,
          max_body_storage_bytes: general.max_body_storage_bytes,
          raw_retention_hours: general.raw_retention_hours,
          daily_retention_days: general.daily_retention_days,
          // Preserve the last server-confirmed pricing configuration.
          pricing: clone(state.base.pricing || basePricing),
        };
        if (state.busy) return;
        state.busy = true;
        save.disabled = true;
        reloadButton.disabled = true;
        setStatus(status, "正在保存…", false);
        callApi("/settings", { method: "PUT", body: payload })
          .then(function (result) {
            var saved = unwrapObject(result, "settings");
            var next = mergeSettingsResponse(saved, state.base);
            if (next) state.base = next;
            setStatus(status, "采集、限制与保留设置已保存", false);
            invokeReload();
          })
          .catch(function (err) {
            setStatus(status, errorMessage(err), true);
          })
          .then(function () {
            state.busy = false;
            save.disabled = false;
            reloadButton.disabled = false;
          });
      });

      reloadButton.addEventListener("click", function () {
        invokeReload();
      });

      wrapper.appendChild(form);
      return wrapper;
    }

    // -----------------------------------------------------------------------
    // Pricing: manual rows, ordered rules, auto and catalog refresh
    // -----------------------------------------------------------------------

    function renderPricing() {
      var wrapper = section("模型价格与匹配规则", uid + "-pricing-heading");
      var autoInput = el("input", "", {
        type: "checkbox",
        id: uid + "-pricing-auto",
        "data-testid": "settings-pricing-auto",
      });
      autoInput.checked = state.base.pricing.auto !== false;
      var autoField = field("自动使用 models.dev 公共价格目录", autoInput);
      addClass(autoField, "setting-capture");
      wrapper.appendChild(autoField);

      var autoNote = el(
        "p",
        "关闭自动目录后只使用下述手工价格与规则。规则按从上到下的顺序匹配，条件同时满足（AND）时第一条命中生效。",
        { class: "settings-note", id: uid + "-pricing-auto-note" },
      );
      autoInput.setAttribute("aria-describedby", uid + "-pricing-auto-note");
      wrapper.appendChild(autoNote);

      var snapshotInfo = el("p", "", {
        class: "settings-note",
        id: uid + "-pricing-snapshot",
        role: "status",
        "aria-live": "polite",
      });
      wrapper.appendChild(snapshotInfo);

      // Read-only effective price snapshot. The table is built lazily so a large
      // catalog stays collapsed and never mixes with the manual/rule editors.
      var catalogDetails = el("details", "", { class: "settings-catalog" });
      var catalogSummary = el("summary", "", {});
      var catalogBody = el("div", "", {});
      catalogDetails.appendChild(catalogSummary);
      catalogDetails.appendChild(catalogBody);
      wrapper.appendChild(catalogDetails);

      function renderCatalog() {
        catalogBody.textContent = "";
        var prices = state.snapshot.prices || [];
        if (!prices.length) {
          catalogBody.appendChild(
            el("p", "尚无生效价格。", { class: "muted" }),
          );
          return;
        }
        var wrap = el("div", "", { class: "table-wrap" });
        var table = el("table");
        var thead = el("thead");
        var headRow = el("tr");
        ["模型", "提供商", "来源", "费率（USD / 百万 Token）"].forEach(
          function (title) {
            headRow.appendChild(el("th", title, { scope: "col" }));
          },
        );
        thead.appendChild(headRow);
        table.appendChild(thead);
        var tbody = el("tbody");
        prices.forEach(function (price) {
          var row = el("tr");
          row.appendChild(el("td", price.model || "—"));
          row.appendChild(el("td", price.provider || "—"));
          row.appendChild(el("td", sourceLabel(price.source)));
          var rates = price.rates || {};
          var cells = [
            "输入 " + formatRate(rates.input),
            "输出 " + formatRate(rates.output),
            "缓存读 " + formatRate(rates.cache_read),
            "缓存写 " + formatRate(rates.cache_write),
          ];
          if (rates.reasoning !== undefined && rates.reasoning !== null) {
            cells.push("推理 " + formatRate(rates.reasoning));
          }
          row.appendChild(el("td", cells.join(" · ")));
          tbody.appendChild(row);
        });
        table.appendChild(tbody);
        wrap.appendChild(table);
        catalogBody.appendChild(wrap);
        catalogBody.appendChild(
          el("p", "只读快照；修改请使用下方手工价格或规则。", {
            class: "settings-note",
          }),
        );
      }

      function renderSnapshot() {
        var snapshot = state.snapshot;
        var parts = [];
        parts.push(
          snapshot.refreshing
            ? "自动价格目录正在刷新…"
            : "上次同步：" + formatTime(snapshot.last_sync),
        );
        parts.push("生效价格条目：" + formatInt(snapshot.prices.length));
        if (snapshot.error) parts.push("目录错误：" + snapshot.error);
        text(snapshotInfo, parts.join(" · "));
        text(
          catalogSummary,
          "生效价格明细（" + formatInt(snapshot.prices.length) + " 条）",
        );
        if (catalogDetails.open) renderCatalog();
        else catalogBody.textContent = "";
      }
      catalogDetails.addEventListener("toggle", function () {
        if (catalogDetails.open) renderCatalog();
      });
      renderSnapshot();

      var refresh = actionButton(
        "立即刷新价格目录",
        "btn btn-secondary",
        "prices-refresh",
      );
      refresh.disabled = state.snapshot.refreshing === true;
      wrapper.appendChild(refresh);

      var zeroNote = el(
        "p",
        "未命中任何规则、手工价格，且自动目录没有该模型价格时，成本按 $0（未定价）记录；系统不会猜测价格，也不会阻断推理。",
        { class: "settings-note" },
      );
      wrapper.appendChild(zeroNote);

      var manualHeading = el("h3", "手工价格（精确模型覆盖）");
      wrapper.appendChild(manualHeading);
      var manualNote = el(
        "p",
        "每行对应一个完整模型 ID；手工价格（包括显式 0）优先于自动目录，但低于规则。新配置仅影响后续请求，不会重新计算历史费用。金额以 USD / 百万 Token 输入，保存时严格换算为整数 micro-USD。",
        { class: "settings-note" },
      );
      wrapper.appendChild(manualNote);
      var manualWrap = el("div", "", {
        class: "settings-rows",
        role: "group",
        "aria-label": "手工模型价格",
      });
      wrapper.appendChild(manualWrap);
      var manualEmpty = el("p", "尚无手工价格。", { class: "muted" });
      wrapper.appendChild(manualEmpty);

      function updateManualEmpty() {
        manualEmpty.hidden = state.manualRows.length > 0;
      }

      function manualAria(record, index) {
        var name = record.model.value.trim() || "未命名模型";
        text(record.remove, "删除");
        record.remove.setAttribute(
          "aria-label",
          "删除第 " + (index + 1) + " 条手工价格（" + name + "）",
        );
        RATE_FIELDS.forEach(function (rate) {
          record.rates[rate.key].setAttribute(
            "aria-label",
            "第 " + (index + 1) + " 条手工价格（" + name + "）" + rate.label,
          );
        });
      }

      function relabelManual() {
        state.manualRows.forEach(manualAria);
        updateManualEmpty();
      }

      function addManualRow(price) {
        manualSequence += 1;
        var row = el("div", "", {
          class: "settings-rule",
          "data-kind": "manual",
        });
        var fields = el("div", "", { class: "settings-fields" });
        row.appendChild(fields);
        var modelInput = el("input", "", {
          type: "text",
          id: uid + "-manual-model-" + manualSequence,
          autocomplete: "off",
          spellcheck: "false",
          placeholder: "例如 provider/model",
        });
        modelInput.value =
          price && price.model !== undefined ? String(price.model) : "";
        var modelField = field("精确完整模型 ID", modelInput);
        addClass(modelField, "setting-field");
        fields.appendChild(modelField);

        var rateInputs = {};
        RATE_FIELDS.forEach(function (rate) {
          var source = price && price.rates ? price.rates[rate.key] : undefined;
          if ((source === undefined || source === null) && !rate.optional)
            source = 0;
          var input = moneyInput(source);
          if (source === undefined || source === null) input.value = "";
          else if (String(input.value).replace(/^\s+|\s+$/g, "") === "") {
            input.value = String(Number(source) / 1000000);
          }
          input.id = uid + "-manual-" + manualSequence + "-" + rate.key;
          var rateField = field(rate.label, input);
          addClass(rateField, "setting-field");
          fields.appendChild(rateField);
          rateInputs[rate.key] = input;
        });

        var actions = el("div", "", { class: "price-actions" });
        var remove = actionButton(
          "删除",
          "btn btn-danger btn-sm",
          "manual-price-remove",
        );
        actions.appendChild(remove);
        row.appendChild(actions);

        var record = {
          row: row,
          model: modelInput,
          rates: rateInputs,
          remove: remove,
          source: price && price.source ? String(price.source) : "manual",
        };
        remove.addEventListener("click", function () {
          var index = state.manualRows.indexOf(record);
          if (index >= 0) state.manualRows.splice(index, 1);
          if (row.parentNode) row.parentNode.removeChild(row);
          relabelManual();
        });
        modelInput.addEventListener("input", relabelManual);
        manualWrap.appendChild(row);
        state.manualRows.push(record);
        relabelManual();
      }

      state.base.pricing.prices.forEach(addManualRow);
      relabelManual();

      var addManual = actionButton(
        "添加手工价格",
        "btn btn-secondary",
        "manual-price-add",
      );
      addManual.addEventListener("click", function () {
        if (state.manualRows.length >= MAX_CONFIG_ENTRIES) {
          setStatus(
            pricingStatus,
            "手工价格不能超过 " + MAX_CONFIG_ENTRIES + " 条",
            true,
          );
          return;
        }
        addManualRow({ model: "", rates: {}, source: "manual" });
      });
      wrapper.appendChild(addManual);

      var rulesHeading = el("h3", "有序匹配规则");
      wrapper.appendChild(rulesHeading);
      var rulesNote = el(
        "p",
        "规则条件：模型 ID 精确匹配；完整提示词 Token 阈值表示“严格大于”，按完整提示词上下文（未缓存输入 + 缓存读 + 缓存写）计算，留空即不限制；时间区间为 UTC HH:mm-HH:mm，起点包含、终点不包含，可跨夜。所有条件需同时满足。",
        { class: "settings-note" },
      );
      wrapper.appendChild(rulesNote);
      var rulesWrap = el("div", "", {
        class: "settings-rows",
        role: "group",
        "aria-label": "有序价格规则",
      });
      wrapper.appendChild(rulesWrap);
      var rulesEmpty = el("p", "尚无规则。", { class: "muted" });
      wrapper.appendChild(rulesEmpty);

      function updateRulesEmpty() {
        rulesEmpty.hidden = state.ruleRows.length > 0;
      }

      function nextRuleID() {
        var used = Object.create(null);
        state.ruleRows.forEach(function (record) {
          used[record.id.value.trim()] = true;
        });
        var id;
        do {
          savedRuleSequence += 1;
          id =
            "ui-" +
            Date.now().toString(36) +
            "-" +
            savedRuleSequence.toString(36);
        } while (used[id]);
        return id;
      }

      function ruleAria(record, index) {
        var name = record.model.value.trim() || "未命名规则";
        record.up.setAttribute(
          "aria-label",
          "上移第 " + (index + 1) + " 条规则（" + name + "）",
        );
        record.down.setAttribute(
          "aria-label",
          "下移第 " + (index + 1) + " 条规则（" + name + "）",
        );
        record.remove.setAttribute(
          "aria-label",
          "删除第 " + (index + 1) + " 条规则（" + name + "）",
        );
        record.up.disabled = index === 0;
        record.down.disabled = index === state.ruleRows.length - 1;
        record.model.setAttribute(
          "aria-label",
          "第 " + (index + 1) + " 条规则模型 ID",
        );
        record.id.setAttribute(
          "aria-label",
          "第 " + (index + 1) + " 条规则 ID",
        );
        record.threshold.setAttribute(
          "aria-label",
          "第 " +
            (index + 1) +
            " 条规则完整提示词 Token 阈值（严格大于，含缓存读/写，可选）",
        );
        record.range.setAttribute(
          "aria-label",
          "第 " + (index + 1) + " 条规则 UTC 时间区间（可选）",
        );
        RATE_FIELDS.forEach(function (rate) {
          record.rates[rate.key].setAttribute(
            "aria-label",
            "第 " + (index + 1) + " 条规则（" + name + "）" + rate.label,
          );
        });
      }

      function relabelRules() {
        state.ruleRows.forEach(ruleAria);
        updateRulesEmpty();
      }

      function moveRule(record, delta) {
        var index = state.ruleRows.indexOf(record);
        var targetIndex = index + delta;
        if (
          index < 0 ||
          targetIndex < 0 ||
          targetIndex >= state.ruleRows.length
        )
          return;
        var target = state.ruleRows[targetIndex];
        if (delta < 0) rulesWrap.insertBefore(record.row, target.row);
        else rulesWrap.insertBefore(target.row, record.row);
        state.ruleRows.splice(index, 1);
        state.ruleRows.splice(targetIndex, 0, record);
        relabelRules();
      }

      function addRuleRow(rule) {
        ruleSequence += 1;
        var row = el("div", "", {
          class: "settings-rule",
          "data-kind": "rule",
        });
        var conditions = el("div", "", { class: "settings-fields" });
        row.appendChild(conditions);

        var idInput = el("input", "", {
          type: "text",
          id: uid + "-rule-id-" + ruleSequence,
          autocomplete: "off",
          spellcheck: "false",
        });
        idInput.value =
          rule && rule.id !== undefined && rule.id !== ""
            ? String(rule.id)
            : nextRuleID();
        var idField = field("规则 ID（唯一）", idInput);
        addClass(idField, "setting-field");
        conditions.appendChild(idField);

        var modelInput = el("input", "", {
          type: "text",
          id: uid + "-rule-model-" + ruleSequence,
          autocomplete: "off",
          spellcheck: "false",
          placeholder: "例如 provider/model",
        });
        modelInput.value =
          rule && rule.model !== undefined ? String(rule.model) : "";
        var modelField = field("精确完整模型 ID", modelInput);
        addClass(modelField, "setting-field");
        conditions.appendChild(modelField);

        var thresholdInput = el("input", "", {
          type: "number",
          id: uid + "-rule-threshold-" + ruleSequence,
          min: "0",
          max: String(MAX_SAFE_INPUT),
          step: "1",
          inputmode: "numeric",
          placeholder: "留空 = 不限制",
        });
        thresholdInput.value =
          rule && rule.input_tokens_gt !== undefined
            ? String(rule.input_tokens_gt)
            : "";
        var thresholdField = field(
          "完整提示词 Token 严格大于（含缓存读/写，可选）",
          thresholdInput,
        );
        addClass(thresholdField, "setting-field");
        conditions.appendChild(thresholdField);

        var rangeInput = el("input", "", {
          type: "text",
          id: uid + "-rule-range-" + ruleSequence,
          autocomplete: "off",
          spellcheck: "false",
          placeholder: "HH:mm-HH:mm（UTC）",
        });
        rangeInput.value =
          rule && rule.time_range ? String(rule.time_range) : "";
        var rangeField = field("UTC 时间区间（可选）", rangeInput);
        addClass(rangeField, "setting-field");
        conditions.appendChild(rangeField);

        var rates = el("div", "", { class: "settings-fields" });
        row.appendChild(rates);
        var rateInputs = {};
        RATE_FIELDS.forEach(function (rate) {
          var source = rule && rule.rates ? rule.rates[rate.key] : undefined;
          if ((source === undefined || source === null) && !rate.optional)
            source = 0;
          var input = moneyInput(source);
          if (source === undefined || source === null) input.value = "";
          else if (String(input.value).replace(/^\s+|\s+$/g, "") === "") {
            input.value = String(Number(source) / 1000000);
          }
          input.id = uid + "-rule-" + ruleSequence + "-" + rate.key;
          var rateField = field(rate.label, input);
          addClass(rateField, "setting-field");
          rates.appendChild(rateField);
          rateInputs[rate.key] = input;
        });

        var actions = el("div", "", { class: "settings-actions" });
        var up = actionButton("上移", "btn btn-secondary btn-sm", "rule-up");
        var down = actionButton(
          "下移",
          "btn btn-secondary btn-sm",
          "rule-down",
        );
        var remove = actionButton(
          "删除",
          "btn btn-danger btn-sm",
          "rule-remove",
        );
        actions.appendChild(up);
        actions.appendChild(down);
        actions.appendChild(remove);
        row.appendChild(actions);

        var record = {
          row: row,
          id: idInput,
          model: modelInput,
          threshold: thresholdInput,
          range: rangeInput,
          rates: rateInputs,
          up: up,
          down: down,
          remove: remove,
        };
        up.addEventListener("click", function () {
          moveRule(record, -1);
        });
        down.addEventListener("click", function () {
          moveRule(record, 1);
        });
        remove.addEventListener("click", function () {
          var index = state.ruleRows.indexOf(record);
          if (index >= 0) state.ruleRows.splice(index, 1);
          if (row.parentNode) row.parentNode.removeChild(row);
          relabelRules();
        });
        modelInput.addEventListener("input", relabelRules);
        rulesWrap.appendChild(row);
        state.ruleRows.push(record);
        relabelRules();
      }

      state.base.pricing.rules.forEach(addRuleRow);
      relabelRules();

      var addRule = actionButton("添加规则", "btn btn-secondary", "rule-add");
      addRule.addEventListener("click", function () {
        if (state.ruleRows.length >= MAX_CONFIG_ENTRIES) {
          setStatus(
            pricingStatus,
            "规则不能超过 " + MAX_CONFIG_ENTRIES + " 条",
            true,
          );
          return;
        }
        addRuleRow({ id: "", model: "", rates: {} });
      });
      wrapper.appendChild(addRule);

      var pricingActions = el("div", "", { class: "settings-actions" });
      var savePricing = el("button", "保存价格设置", {
        type: "button",
        class: "btn btn-primary",
        "data-testid": "settings-pricing-save",
      });
      pricingActions.appendChild(savePricing);
      var pricingStatus = el("span", "", {
        class: "settings-note",
        id: uid + "-pricing-status",
        role: "status",
        "aria-live": "polite",
      });
      pricingActions.appendChild(pricingStatus);
      wrapper.appendChild(pricingActions);

      function readRates(record, label) {
        var result = {};
        RATE_FIELDS.forEach(function (rate) {
          result[rate.key] = readMoney(
            record.rates[rate.key],
            label + rate.label,
            rate.optional,
          );
        });
        if (result.reasoning === null) delete result.reasoning;
        return result;
      }

      function readPricing() {
        if (state.manualRows.length > MAX_CONFIG_ENTRIES) {
          throw new Error("手工价格不能超过 " + MAX_CONFIG_ENTRIES + " 条");
        }
        if (state.ruleRows.length > MAX_CONFIG_ENTRIES) {
          throw new Error("规则不能超过 " + MAX_CONFIG_ENTRIES + " 条");
        }
        var seenModels = Object.create(null);
        var prices = state.manualRows.map(function (record, index) {
          var label = "第 " + (index + 1) + " 条手工价格";
          var model = record.model.value.trim();
          if (!model) throw new Error(label + "的模型 ID 不能为空");
          if (model.length > 1024)
            throw new Error(label + "的模型 ID 不能超过 1024 个字符");
          if (seenModels[model])
            throw new Error("手工价格的模型 ID 重复：" + model);
          seenModels[model] = true;
          var item = {
            model: model,
            rates: readRates(record, label + "（" + model + "）"),
            source: record.source || "manual",
          };
          return item;
        });

        var seenIDs = Object.create(null);
        var rules = state.ruleRows.map(function (record, index) {
          var label = "第 " + (index + 1) + " 条规则";
          var id = record.id.value.trim();
          var model = record.model.value.trim();
          if (!id) throw new Error(label + "的规则 ID 不能为空");
          if (id.length > 1024)
            throw new Error(label + "的规则 ID 不能超过 1024 个字符");
          if (seenIDs[id]) throw new Error("规则 ID 重复：" + id);
          seenIDs[id] = true;
          if (!model) throw new Error(label + "的模型 ID 不能为空");
          if (model.length > 1024)
            throw new Error(label + "的模型 ID 不能超过 1024 个字符");

          var item = {
            id: id,
            model: model,
            rates: readRates(record, label + "（" + model + "）"),
          };
          var threshold = readInteger(
            record.threshold,
            label + "的完整提示词 Token 阈值（含缓存读/写）",
            0,
            MAX_SAFE_INPUT,
            true,
          );
          if (threshold !== null) item.input_tokens_gt = threshold;
          var timeRange = record.range.value.trim();
          if (timeRange) {
            if (!validTimeRange(timeRange)) {
              throw new Error(
                label + "的时间区间必须是 UTC HH:mm-HH:mm，且起点与终点不同",
              );
            }
            item.time_range = timeRange;
          }
          return item;
        });

        return {
          auto: autoInput.checked === true,
          prices: prices,
          rules: rules,
        };
      }

      savePricing.addEventListener("click", function () {
        var pricing;
        try {
          pricing = readPricing();
        } catch (err) {
          setStatus(pricingStatus, errorMessage(err), true);
          return;
        }
        if (state.busy) return;
        state.busy = true;
        savePricing.disabled = true;
        addManual.disabled = true;
        addRule.disabled = true;
        setStatus(pricingStatus, "正在保存…", false);
        var payload = {
          capture_enabled: state.base.capture_enabled === true,
          max_body_bytes: state.base.max_body_bytes,
          max_body_storage_bytes: state.base.max_body_storage_bytes,
          raw_retention_hours: state.base.raw_retention_hours,
          daily_retention_days: state.base.daily_retention_days,
          // General settings come from the last server-confirmed snapshot, so
          // this save does not persist unrelated unsaved form edits.
          pricing: pricing,
        };
        callApi("/settings", { method: "PUT", body: payload })
          .then(function (result) {
            var saved = unwrapObject(result, "settings");
            var next = mergeSettingsResponse(saved, state.base);
            if (next) state.base = next;
            if (!saved || !saved.pricing || typeof saved.pricing !== "object") {
              state.base.pricing = clone(pricing);
            }
            setStatus(pricingStatus, "价格设置已保存", false);
            invokeReload();
          })
          .catch(function (err) {
            setStatus(pricingStatus, errorMessage(err), true);
          })
          .then(function () {
            state.busy = false;
            savePricing.disabled = false;
            addManual.disabled = false;
            addRule.disabled = false;
          });
      });

      refresh.addEventListener("click", function () {
        if (state.busy) return;
        state.busy = true;
        refresh.disabled = true;
        setStatus(pricingStatus, "正在刷新价格目录…", false);
        callApi("/prices/refresh", { method: "POST" })
          .then(function (result) {
            var snapshot = unwrapObject(result, "snapshot");
            state.snapshot = normalizeSnapshot(snapshot);
            renderSnapshot();
            setStatus(
              pricingStatus,
              state.snapshot.error
                ? "刷新失败，已保留最后有效缓存：" + state.snapshot.error
                : "价格目录已刷新",
              Boolean(state.snapshot.error),
            );
          })
          .catch(function (err) {
            setStatus(pricingStatus, errorMessage(err), true);
          })
          .then(function () {
            state.busy = false;
            refresh.disabled = state.snapshot.refreshing === true;
          });
      });

      return wrapper;
    }

    // -----------------------------------------------------------------------
    // Health and physical compaction
    // -----------------------------------------------------------------------

    function renderHealth() {
      var wrapper = section("运行状况与数据库压缩", uid + "-health-heading");
      var grid = el("div", "", { class: "settings-fields" });
      wrapper.appendChild(grid);

      var values = {};
      [
        ["database_bytes", "数据库大小"],
        ["dropped_captures", "丢弃的请求体捕获"],
        ["pending_usage", "待重试计费事件"],
        ["active_requests", "活跃请求"],
      ].forEach(function (entry) {
        var box = el("div", "", { class: "setting-field" });
        box.appendChild(el("span", entry[1], { class: "muted" }));
        values[entry[0]] = el("strong", "", {});
        box.appendChild(values[entry[0]]);
        grid.appendChild(box);
      });

      var errorBox = el("div", "", { class: "setting-field" });
      errorBox.appendChild(el("span", "最近错误", { class: "muted" }));
      var errorValue = el("span", "", { class: "settings-note" });
      errorBox.appendChild(errorValue);
      grid.appendChild(errorBox);

      function updateHealth() {
        text(values.database_bytes, formatBytes(state.health.database_bytes));
        text(values.dropped_captures, formatInt(state.health.dropped_captures));
        text(values.pending_usage, formatInt(state.health.pending_usage));
        text(values.active_requests, formatInt(state.health.active_requests));
        text(errorValue, state.health.last_error || "无");
      }
      updateHealth();

      var note = el(
        "p",
        "压缩会尝试回收数据库空闲空间，需要临时磁盘空间；失败时保留原文件。压缩不会清除独立 Key 的已用额度或未持久化计费事件。",
        { class: "settings-note" },
      );
      wrapper.appendChild(note);

      var actions = el("div", "", { class: "settings-actions" });
      var compact = actionButton(
        "压缩数据库",
        "btn btn-secondary",
        "health-compact",
      );
      actions.appendChild(compact);
      var status = el("span", "", {
        class: "settings-note",
        id: uid + "-health-status",
        role: "status",
        "aria-live": "polite",
      });
      actions.appendChild(status);
      wrapper.appendChild(actions);

      compact.addEventListener("click", function () {
        if (state.busy) return;
        var message =
          "确定要压缩数据库吗？当前大小 " +
          formatBytes(state.health.database_bytes) +
          "。压缩会临时占用额外磁盘空间，且可能需要一些时间；失败时会保留原文件。";
        if (
          !win ||
          typeof win.confirm !== "function" ||
          !win.confirm(message)
        ) {
          setStatus(status, "已取消压缩", false);
          return;
        }
        state.busy = true;
        compact.disabled = true;
        setStatus(status, "正在压缩数据库…", false);
        callApi("/compact", { method: "POST" })
          .then(function (result) {
            var nextHealth = unwrapObject(result, "health");
            if (nextHealth && typeof nextHealth === "object") {
              state.health = normalizeHealth(nextHealth);
              updateHealth();
            }
            setStatus(status, "压缩完成", false);
          })
          .catch(function (err) {
            setStatus(status, errorMessage(err), true);
          })
          .then(function () {
            state.busy = false;
            compact.disabled = false;
          });
      });

      return wrapper;
    }

    // -----------------------------------------------------------------------
    // Mount
    // -----------------------------------------------------------------------

    host.textContent = "";
    var root = el("div", "", {
      class: "settings",
      "data-testid": "observer-settings",
    });
    root.appendChild(renderGeneral());
    root.appendChild(renderPricing());
    root.appendChild(renderHealth());
    host.appendChild(root);
  }

  if (typeof window !== "undefined") {
    window.ObserverSettings = ObserverSettings;
  }
  if (typeof module !== "undefined" && module.exports) {
    module.exports = ObserverSettings;
  }
})();
