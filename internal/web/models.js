/*
 * Observer 模型选择组件（零依赖）。
 *
 * 浏览器入口：
 *   window.ObserverModels(host, initialAllowedModels, discover, signal)
 *     -> { read(): string[], dispose(): void }
 *
 * discover(path, {signal}) 由主 UI 提供并完成鉴权。本组件只会请求固定的
 * /v0/management/auth-files 与 /v0/management/auth-files/models?name=...。
 * 组件同步创建 DOM，目录发现异步进行；缓存只保留公开的账户名与模型 ID
 * 字符串，不保留任何响应对象或凭据。
 *
 * read()：默认“不限模型”返回 []；显式限制返回已选精确 ID；显式限制但
 * 未选择时抛错，主 UI 必须展示该错误并禁止提交。刷新按钮会取消旧请求。
 *
 * Node 侧额外导出发现与选择纯助手，便于不依赖浏览器的单元测试。
 */
(function () {
  "use strict";

  var AUTH_FILES_PATH = "/v0/management/auth-files";
  var CACHE_TTL_MS = 5 * 60 * 1000;
  var MAX_CONCURRENCY = 4;
  var defaultCache = new Map();
  var sharedLimiter = createLimiter(MAX_CONCURRENCY);
  var componentSeq = 0;

  function abortError() {
    try {
      return new DOMException("模型目录加载已取消", "AbortError");
    } catch (_) {
      var error = new Error("模型目录加载已取消");
      error.name = "AbortError";
      return error;
    }
  }

  function isAbort(error) {
    return !!error && error.name === "AbortError";
  }

  // 外层空白不属于模型身份；其余字符（包括 provider 前缀、斜杠、版本号）原样保留。
  function normalizeIDs(values) {
    var result = [];
    var seen = new Set();
    if (!Array.isArray(values)) return result;
    for (var i = 0; i < values.length; i++) {
      var raw = values[i];
      if (typeof raw !== "string") continue;
      var id = raw.trim();
      if (!id || seen.has(id)) continue;
      seen.add(id);
      result.push(id);
    }
    return result;
  }

  function filterModels(models, query) {
    var ids = normalizeIDs(models);
    var needle = typeof query === "string" ? query.trim().toLowerCase() : "";
    if (!needle) return ids;
    return ids.filter(function (id) {
      return id.toLowerCase().indexOf(needle) >= 0;
    });
  }

  function parseFiles(payload) {
    var items;
    if (Array.isArray(payload)) items = payload;
    else if (payload && Array.isArray(payload.files)) items = payload.files;
    else throw Error("auth-files 响应格式无效");
    var names = [];
    for (var i = 0; i < items.length; i++) {
      var item = items[i];
      var name = typeof item === "string" ? item : item && item.name;
      if (typeof name === "string" && name.trim()) names.push(name);
    }
    return normalizeIDs(names);
  }

  function parseModels(payload) {
    var items;
    if (Array.isArray(payload)) items = payload;
    else if (payload && Array.isArray(payload.models)) items = payload.models;
    else throw Error("auth-files models 响应格式无效");
    var ids = [];
    for (var i = 0; i < items.length; i++) {
      var item = items[i];
      var id = typeof item === "string" ? item : item && item.id;
      if (typeof id === "string" && id.trim()) ids.push(id);
    }
    return normalizeIDs(ids);
  }

  function modelsPath(name) {
    return AUTH_FILES_PATH + "/models?name=" + encodeURIComponent(String(name));
  }

  // 空数组是“不限模型”；任何非空输入都进入显式限制模式。
  // 即使条目无效也 fail closed，避免把异常初始值误解释为不限制。
  function initialState(allowedModels) {
    var values = Array.isArray(allowedModels) ? allowedModels : [];
    var selected = normalizeIDs(values);
    return {
      restricted: values.length > 0,
      selected: selected,
      available: selected.slice(),
    };
  }

  // 保留已选但目录中不再出现的 ID；已保留项排在发现结果之前。
  function mergeAvailable(discovered, preserved) {
    return normalizeIDs(
      normalizeIDs(preserved).concat(normalizeIDs(discovered)),
    );
  }

  function toggleSelection(selected, id, checked) {
    var next = normalizeIDs(selected);
    var value = typeof id === "string" ? id.trim() : "";
    if (!value) return next;
    var at = next.indexOf(value);
    if (checked) {
      if (at < 0) next.push(value);
    } else if (at >= 0) {
      next.splice(at, 1);
    }
    return next;
  }

  function addExactModel(available, selected, raw) {
    var id = typeof raw === "string" ? raw.trim() : "";
    if (!id) throw Error("请输入精确模型 ID");
    return {
      id: id,
      available: normalizeIDs(normalizeIDs(available).concat([id])),
      selected: toggleSelection(selected, id, true),
    };
  }

  function readAllowed(restricted, selected) {
    if (!restricted) return [];
    var ids = normalizeIDs(selected);
    if (!ids.length) throw Error("限制模式至少选择一个模型");
    return ids;
  }

  // 所有目录请求共享同一个限流器，保证跨组件也最多四个网络请求并行。
  function createLimiter(max) {
    var requested = Math.floor(Number(max));
    var limit = Number.isFinite(requested)
      ? Math.min(MAX_CONCURRENCY, Math.max(1, requested))
      : MAX_CONCURRENCY;
    var queue = [];
    var active = 0;

    function cleanup(task) {
      if (task.signal && task.onAbort) {
        task.signal.removeEventListener("abort", task.onAbort);
        task.onAbort = null;
      }
    }

    function settle(task, value, failed) {
      cleanup(task);
      active--;
      if (failed) task.reject(value);
      else task.resolve(value);
      pump();
    }

    function pump() {
      while (active < limit && queue.length) {
        var task = queue.shift();
        if (task.signal && task.signal.aborted) {
          cleanup(task);
          task.reject(abortError());
          continue;
        }
        active++;
        var pending;
        try {
          pending = task.fn();
        } catch (error) {
          cleanup(task);
          active--;
          task.reject(error);
          continue;
        }
        Promise.resolve(pending).then(
          function (value) {
            settle(task, value, false);
          },
          function (error) {
            settle(task, error, true);
          },
        );
      }
    }

    function run(fn, signal) {
      return new Promise(function (resolve, reject) {
        var task = {
          fn: fn,
          signal: signal,
          resolve: resolve,
          reject: reject,
          onAbort: null,
        };
        if (signal) {
          if (signal.aborted) {
            reject(abortError());
            return;
          }
          task.onAbort = function () {
            var at = queue.indexOf(task);
            if (at < 0) return; // 已开始执行，由底层请求响应 abort。
            queue.splice(at, 1);
            cleanup(task);
            reject(abortError());
          };
          signal.addEventListener("abort", task.onAbort, { once: true });
        }
        queue.push(task);
        pump();
      });
    }

    return {
      run: run,
      get active() {
        return active;
      },
      get queued() {
        return queue.length;
      },
      get limit() {
        return limit;
      },
    };
  }

  function createCache() {
    return new Map();
  }

  function cacheGet(cache, key, at) {
    var entry = cache.get(key);
    if (!entry) return null;
    if (at - entry.at >= CACHE_TTL_MS) {
      cache.delete(key);
      return null;
    }
    return Array.isArray(entry.ids) ? entry.ids.slice() : null;
  }

  function cacheSet(cache, key, ids, at) {
    cache.set(key, { at: at, ids: normalizeIDs(ids) });
  }

  function discoveryFailure() {
    var error = Error("auth-files 模型目录加载失败");
    error.code = "MODEL_DISCOVERY_FAILED";
    return error;
  }

  function callDiscover(discover, path, signal) {
    return Promise.resolve()
      .then(function () {
        if (signal && signal.aborted) throw abortError();
        return discover(path, { signal: signal });
      })
      .then(function (payload) {
        if (payload && typeof payload.json === "function")
          return payload.json();
        return payload;
      });
  }

  async function loadFileNames(discover, signal, limiter, cache, now, force) {
    if (signal && signal.aborted) throw abortError();
    if (!force) {
      var cached = cacheGet(cache, AUTH_FILES_PATH, now());
      if (cached) return cached;
    }
    var payload;
    try {
      payload = await limiter.run(function () {
        return callDiscover(discover, AUTH_FILES_PATH, signal);
      }, signal);
    } catch (error) {
      if (isAbort(error)) throw error;
      throw discoveryFailure();
    }
    if (signal && signal.aborted) throw abortError();
    var names;
    try {
      names = parseFiles(payload);
    } catch (_) {
      throw discoveryFailure();
    }
    if (signal && signal.aborted) throw abortError();
    cacheSet(cache, AUTH_FILES_PATH, names, now());
    return names;
  }

  async function loadModelIDs(
    discover,
    name,
    signal,
    limiter,
    cache,
    now,
    force,
  ) {
    if (signal && signal.aborted) throw abortError();
    var path = modelsPath(name);
    if (!force) {
      var cached = cacheGet(cache, path, now());
      if (cached) return cached;
    }
    var payload;
    try {
      payload = await limiter.run(function () {
        return callDiscover(discover, path, signal);
      }, signal);
    } catch (error) {
      if (isAbort(error)) throw error;
      throw discoveryFailure();
    }
    if (signal && signal.aborted) throw abortError();
    var ids;
    try {
      ids = parseModels(payload);
    } catch (_) {
      throw discoveryFailure();
    }
    if (signal && signal.aborted) throw abortError();
    cacheSet(cache, path, ids, now());
    return ids;
  }

  /*
   * 发现并合并所有 auth-file 的模型 ID。
   *
   * options:
   *   signal       AbortSignal，取消队列中与在途请求，并禁止写入缓存
   *   cache        Map，默认跨组件共享；只保存字符串数组
   *   now          () => 毫秒时间戳，便于测试缓存 TTL
   *   force        跳过缓存读取（刷新按钮使用）
   *   limiter      自定义限流器；默认 4 并发
   *   concurrency  未传 limiter 时的并发上限，最大 4
   *   onProgress   (done, total, failures) => void
   *
   * 返回 {models, failures, total}。单个账户失败只进入 failures；
   * 根目录请求或响应格式失败则抛错，调用方必须保留原选择与模式。
   */
  async function discoverModels(discover, options) {
    options = options || {};
    if (typeof discover !== "function") throw Error("缺少模型目录查询函数");
    var signal = options.signal;
    if (signal && signal.aborted) throw abortError();
    var cache = options.cache || defaultCache;
    var now = typeof options.now === "function" ? options.now : Date.now;
    var force = !!options.force;
    var limiter =
      options.limiter ||
      (options.concurrency
        ? createLimiter(options.concurrency)
        : sharedLimiter);
    var report =
      typeof options.onProgress === "function" ? options.onProgress : null;

    var names = await loadFileNames(
      discover,
      signal,
      limiter,
      cache,
      now,
      force,
    );
    if (signal && signal.aborted) throw abortError();

    var total = names.length;
    var done = 0;
    var failureCount = 0;
    if (report) {
      try {
        report(0, total, 0);
      } catch (_) {}
    }

    var rows = await Promise.all(
      names.map(function (name) {
        return (async function () {
          var ids = [];
          var failure = null;
          try {
            ids = await loadModelIDs(
              discover,
              name,
              signal,
              limiter,
              cache,
              now,
              force,
            );
          } catch (error) {
            if (isAbort(error)) throw error;
            failure = error;
          }
          done++;
          if (failure) failureCount++;
          if (report) {
            try {
              report(done, total, failureCount);
            } catch (_) {}
          }
          return { name: name, ids: ids, failure: failure };
        })();
      }),
    );

    if (signal && signal.aborted) throw abortError();

    var flattened = [];
    rows.forEach(function (row) {
      flattened = flattened.concat(row.ids);
    });
    return {
      models: normalizeIDs(flattened),
      failures: rows
        .filter(function (row) {
          return !!row.failure;
        })
        .map(function (row) {
          // 不把底层错误正文带入 UI 或缓存。
          return { name: row.name, message: "模型目录加载失败" };
        }),
      total: total,
    };
  }

  function ObserverModels(host, initialAllowedModels, discover, signal) {
    if (!host || typeof host.appendChild !== "function")
      throw Error("ObserverModels 需要挂载容器");
    var UI = typeof window !== "undefined" ? window.ObserverUI : null;
    if (!UI || typeof UI.el !== "function" || typeof UI.field !== "function")
      throw Error("ObserverModels 需要先加载 ObserverUI");
    if (typeof discover !== "function")
      throw Error("ObserverModels 需要模型目录查询函数");
    if (signal && typeof signal.addEventListener !== "function")
      throw Error("signal 必须是 AbortSignal");

    var el = UI.el;
    var field = UI.field;
    var initial = initialState(initialAllowedModels);
    var restricted = initial.restricted;
    var selected = initial.selected.slice();
    var custom = initial.available.slice();
    var available = initial.available.slice();
    var discovered = [];
    var directoryLoaded = false;
    var disposed = false;
    var generation = 0;
    var controller = null;
    var hostAbort = null;
    var radioName = "models-mode-" + ++componentSeq;

    var root = el("section", null, {
      class: "models-root",
      "aria-label": "允许模型",
    });
    var mode = el("fieldset", null, { class: "models-mode" });
    mode.append(el("legend", "模型限制"));
    var allRadio = el("input", null, {
      type: "radio",
      name: radioName,
      value: "all",
    });
    var restrictedRadio = el("input", null, {
      type: "radio",
      name: radioName,
      value: "restricted",
    });
    var allOption = field("不限模型（默认）", allRadio);
    allOption.className = "models-mode-option";
    var restrictedOption = field("仅允许所选模型", restrictedRadio);
    restrictedOption.className = "models-mode-option";
    mode.append(allOption, restrictedOption);
    root.append(mode);

    var note = el("p", null, { class: "models-note" });
    root.append(note);

    var panel = el("div", null, { class: "models-restricted" });
    var list = el("div", null, {
      class: "models-list",
      role: "group",
      "aria-label": "可用模型",
    });
    list.id = "models-list-" + componentSeq;
    var search = el("input", null, {
      type: "search",
      placeholder: "按模型 ID 搜索",
      autocomplete: "off",
    });
    search.setAttribute("aria-controls", list.id);
    var searchField = field("搜索模型", search);
    searchField.className = "models-search-field";
    var hint = el("p", null, { class: "models-hint" });
    var status = el("p", null, {
      class: "models-status",
      role: "status",
      "aria-live": "polite",
    });
    var warning = el("p", null, { class: "models-warning", role: "alert" });
    warning.hidden = true;
    var actions = el("div", null, { class: "models-actions" });
    var selectAll = el("button", "全选", {
      type: "button",
      class: "models-select-all",
    });
    var clear = el("button", "清空", { type: "button", class: "models-clear" });
    var refresh = el("button", "刷新模型列表", {
      type: "button",
      class: "models-refresh",
    });
    actions.append(selectAll, clear);
    var manual = el("input", null, {
      type: "text",
      placeholder: "例如 provider/model",
      autocomplete: "off",
    });
    var manualField = field("手动添加精确模型 ID", manual);
    manualField.className = "models-manual-field";
    var add = el("button", "添加精确 ID", {
      type: "button",
      class: "models-add",
    });
    var manualRow = el("div", null, { class: "models-manual" });
    manualRow.append(manualField, add);
    var manualNote = el(
      "p",
      "手动添加会作为精确模型 ID 保存，适用于目录不可用或未列出的模型。",
      { class: "models-manual-note" },
    );
    panel.append(searchField, list, actions, manualRow, manualNote, hint);
    // 进度、警告与刷新放在限制面板之外，默认“不限模型”时也可用。
    root.append(panel, refresh, status, warning);

    function updateHint() {
      if (!restricted) {
        hint.textContent = "";
        return;
      }
      hint.textContent = selected.length
        ? "已选择 " + selected.length + " 个模型。"
        : "限制模式至少选择一个模型；也可手动添加精确 ID。";
    }

    function refreshMode() {
      allRadio.checked = !restricted;
      restrictedRadio.checked = restricted;
      panel.hidden = !restricted;
      note.textContent = restricted
        ? "仅允许所勾选的精确模型 ID；目录加载失败时仍可手动添加。"
        : "默认不限模型。切换到“仅允许所选模型”后可搜索、勾选或手动添加。";
      updateHint();
    }

    function renderList() {
      var shown = filterModels(available, search.value);
      list.replaceChildren();
      if (!shown.length) {
        list.append(
          el(
            "p",
            available.length
              ? "没有匹配的模型。"
              : "暂无可用模型，请刷新目录或手动添加精确 ID。",
            { class: "models-empty" },
          ),
        );
        return;
      }
      var fragment = list.ownerDocument.createDocumentFragment();
      shown.forEach(function (id) {
        var checkbox = el("input", null, { type: "checkbox" });
        checkbox.checked = selected.indexOf(id) >= 0;
        checkbox.addEventListener("change", function () {
          selected = toggleSelection(selected, id, checkbox.checked);
          updateHint();
        });
        var option = field(id, checkbox);
        option.className = "models-item";
        if (directoryLoaded && discovered.indexOf(id) < 0) {
          option.append(
            el("span", "（目录未列出）", { class: "models-missing" }),
          );
        }
        fragment.append(option);
      });
      list.append(fragment);
    }

    function addManual() {
      var result;
      try {
        result = addExactModel(available, selected, manual.value);
      } catch (error) {
        hint.textContent = error.message;
        manual.focus();
        return;
      }
      available = result.available;
      selected = result.selected;
      custom = normalizeIDs(custom.concat([result.id]));
      manual.value = "";
      if (!restricted) {
        restricted = true;
        refreshMode();
      }
      if (search.value && filterModels([result.id], search.value).length === 0)
        search.value = "";
      renderList();
      updateHint();
      manual.focus();
    }

    allRadio.addEventListener("change", function () {
      if (!allRadio.checked) return;
      restricted = false;
      refreshMode();
    });
    restrictedRadio.addEventListener("change", function () {
      if (!restrictedRadio.checked) return;
      restricted = true;
      refreshMode();
    });
    search.addEventListener("input", renderList);
    selectAll.addEventListener("click", function () {
      filterModels(available, search.value).forEach(function (id) {
        if (selected.indexOf(id) < 0) selected.push(id);
      });
      selected = normalizeIDs(selected);
      renderList();
      updateHint();
    });
    clear.addEventListener("click", function () {
      selected = [];
      renderList();
      updateHint();
    });
    refresh.addEventListener("click", function () {
      load(true);
    });
    add.addEventListener("click", addManual);
    manual.addEventListener("keydown", function (event) {
      if (event.key !== "Enter") return;
      event.preventDefault();
      addManual();
    });

    function cancelLoad() {
      generation++;
      if (controller) {
        controller.abort();
        controller = null;
      }
    }

    function load(force) {
      if (disposed) return;
      if (signal && signal.aborted) {
        status.textContent = "模型目录加载已取消。";
        return;
      }
      cancelLoad();
      var current = generation;
      var local = new AbortController();
      controller = local;
      root.setAttribute("aria-busy", "true");
      warning.hidden = true;
      warning.textContent = "";
      status.textContent = "正在加载模型目录…";

      discoverModels(discover, {
        signal: local.signal,
        force: !!force,
        limiter: sharedLimiter,
        onProgress: function (done, total, failures) {
          if (disposed || current !== generation || local.signal.aborted)
            return;
          if (!total) {
            status.textContent = "未发现 auth-files 模型目录。";
            return;
          }
          status.textContent =
            "模型目录进度 " +
            done +
            "/" +
            total +
            (failures ? "，失败 " + failures : "") +
            "…";
        },
      })
        .then(function (result) {
          if (disposed || current !== generation || local.signal.aborted)
            return;
          discovered = result.models.slice();
          directoryLoaded = true;
          available = mergeAvailable(
            discovered,
            normalizeIDs(custom.concat(selected)),
          );
          renderList();
          updateHint();
          if (result.failures.length) {
            warning.hidden = false;
            warning.textContent =
              "部分账户模型目录加载失败（" +
              result.failures.length +
              "/" +
              result.total +
              "）。已显示成功加载的模型；可刷新或手动添加精确 ID。";
          }
          status.textContent = result.total
            ? "已从 " +
              result.total +
              " 个账户加载 " +
              result.models.length +
              " 个模型。"
            : "未发现 auth-files 模型目录；可手动添加精确 ID。";
          root.setAttribute("aria-busy", "false");
        })
        .catch(function (error) {
          if (
            disposed ||
            current !== generation ||
            isAbort(error) ||
            local.signal.aborted
          )
            return;
          warning.hidden = false;
          warning.textContent =
            "模型目录加载失败。当前模式与已选模型保持不变；可手动添加精确 ID 或稍后刷新。";
          status.textContent = "";
          root.setAttribute("aria-busy", "false");
        });
    }

    function onHostAbort() {
      if (disposed) return;
      cancelLoad();
      status.textContent = "模型目录加载已取消。";
      root.setAttribute("aria-busy", "false");
    }

    if (signal) {
      if (signal.aborted) onHostAbort();
      else {
        hostAbort = onHostAbort;
        signal.addEventListener("abort", hostAbort, { once: true });
      }
    }

    function dispose() {
      if (disposed) return;
      disposed = true;
      cancelLoad();
      if (signal && hostAbort) signal.removeEventListener("abort", hostAbort);
      hostAbort = null;
      if (root.parentNode) root.parentNode.removeChild(root);
    }

    refreshMode();
    renderList();
    host.appendChild(root);
    load(false);

    return {
      read: function () {
        return readAllowed(restricted, selected);
      },
      dispose: dispose,
    };
  }

  var api = {
    ObserverModels: ObserverModels,
    discoverModels: discoverModels,
    createLimiter: createLimiter,
    createCache: createCache,
    parseFiles: parseFiles,
    parseModels: parseModels,
    initialState: initialState,
    mergeAvailable: mergeAvailable,
    toggleSelection: toggleSelection,
    addExactModel: addExactModel,
    readAllowed: readAllowed,
    filterModels: filterModels,
    modelsPath: modelsPath,
    AUTH_FILES_PATH: AUTH_FILES_PATH,
    CACHE_TTL_MS: CACHE_TTL_MS,
    MAX_CONCURRENCY: MAX_CONCURRENCY,
  };

  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ObserverModels = ObserverModels;
})();
