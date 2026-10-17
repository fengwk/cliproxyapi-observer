/*
 * Observer 模型选择组件（零依赖）。
 *
 *   window.ObserverModels(host, initialAllowedModels, loadCatalog, signal)
 *     -> { read(): string[], dispose(): void }
 *
 * loadCatalog({signal}) 由主 UI 实现（取一次性目录令牌并请求权威模型目录），
 * 返回 {ids: string[]} 或直接返回 string[]。组件只缓存模型 ID 字符串五分钟，
 * 刷新时 force 绕过缓存；取消由 AbortController + generation 双重拦截。
 *
 * read()：默认返回 []（不限模型）；显式限制返回已选精确 ID；限制模式下
 * 没有选择时抛错，主 UI 必须展示并禁止提交。初始限制中目录未列出的 ID 与
 * 手动添加的精确 ID 始终保留。
 */
(function () {
  "use strict";

  var CACHE_TTL_MS = 5 * 60 * 1000;
  var MAX_VISIBLE = 100;
  var componentSeq = 0;

  function abortError() {
    try {
      return new DOMException("模型目录加载已取消", "AbortError");
    } catch (_) {
      var error = Error("模型目录加载已取消");
      error.name = "AbortError";
      return error;
    }
  }

  function isAbort(error) {
    return !!error && error.name === "AbortError";
  }

  function catalogFailure() {
    var error = Error("模型目录加载失败");
    error.code = "MODEL_CATALOG_FAILED";
    return error;
  }

  // 去掉外层空白，但完整保留 provider 前缀、大小写、斜杠等模型身份。
  function normalizeIDs(values) {
    var result = [];
    var seen = new Set();
    if (!Array.isArray(values)) return result;
    for (var i = 0; i < values.length; i++) {
      var value = values[i];
      if (typeof value !== "string") continue;
      var id = value.trim();
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

  // 主 UI 可以返回 {ids} 或直接返回字符串数组；其余形状一律失败。
  function parseIDs(payload) {
    var items = Array.isArray(payload)
      ? payload
      : payload && Array.isArray(payload.ids)
        ? payload.ids
        : null;
    if (!items) throw catalogFailure();
    return normalizeIDs(items);
  }

  // 空数组代表“不限模型”；非空初始值即使内容无效也 fail closed。
  function initialState(initialAllowedModels) {
    var values = Array.isArray(initialAllowedModels)
      ? initialAllowedModels
      : [];
    var selected = normalizeIDs(values);
    return {
      restricted: values.length > 0,
      selected: selected,
    };
  }

  // 初始/手动的目录外 ID 排在发现结果之前，且不会被刷新丢弃。
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

  function createCache(now) {
    var clock = typeof now === "function" ? now : Date.now;
    var entry = null;
    return {
      get: function () {
        if (!entry) return null;
        if (clock() - entry.at >= CACHE_TTL_MS) {
          entry = null;
          return null;
        }
        return entry.ids.slice();
      },
      set: function (ids) {
        entry = { at: clock(), ids: normalizeIDs(ids) };
      },
      clear: function () {
        entry = null;
      },
    };
  }

  var sharedCache = createCache();

  function clearCache() {
    // Detach in-flight loaders too: a late old-session result may only write
    // its abandoned cache object, never the next session's catalog.
    sharedCache = createCache();
  }

  /*
   * 加载并缓存权威目录 ID。force 跳过缓存；abort 后不得渲染或写入缓存；
   * 任何底层错误只暴露通用错误，避免响应体或凭据进入 UI。
   */
  async function loadIDs(loadCatalog, options) {
    options = options || {};
    var signal = options.signal;
    var cache = options.cache || sharedCache;
    var force = !!options.force;
    if (signal && signal.aborted) throw abortError();
    if (typeof loadCatalog !== "function") throw catalogFailure();
    if (!force) {
      var cached = cache.get();
      if (cached) return cached;
    }

    var payload;
    try {
      payload = await Promise.resolve().then(function () {
        return loadCatalog({ signal: signal });
      });
    } catch (error) {
      if (isAbort(error)) throw error;
      throw catalogFailure();
    }
    if (signal && signal.aborted) throw abortError();

    var ids;
    try {
      ids = parseIDs(payload);
    } catch (_) {
      throw catalogFailure();
    }
    if (signal && signal.aborted) throw abortError();
    cache.set(ids);
    return ids.slice();
  }

  function ObserverModels(host, initialAllowedModels, loadCatalog, signal) {
    if (!host || typeof host.appendChild !== "function")
      throw Error("ObserverModels 需要挂载容器");
    var UI = typeof window !== "undefined" ? window.ObserverUI : null;
    if (!UI || typeof UI.el !== "function" || typeof UI.field !== "function")
      throw Error("ObserverModels 需要先加载 ObserverUI");
    if (typeof loadCatalog !== "function")
      throw Error("ObserverModels 需要模型目录加载函数");
    if (signal && typeof signal.addEventListener !== "function")
      throw Error("signal 必须是 AbortSignal");

    var el = UI.el;
    var field = UI.field;
    var initial = initialState(initialAllowedModels);
    var restricted = initial.restricted;
    var selected = initial.selected.slice();
    var preserved = initial.selected.slice();
    var available = initial.selected.slice();
    var discovered = [];
    var catalogLoaded = false;
    var disposed = false;
    var generation = 0;
    var controller = null;
    var hostAbort = null;
    var radioName = "models-mode-" + ++componentSeq;

    var root = el("section", null, {
      class: "models-root",
      "aria-label": "允许模型",
      "aria-busy": "false",
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
    root.append(panel, refresh, status, warning);

    function updateHint() {
      if (!restricted) {
        hint.textContent = "";
        return;
      }
      var matching = filterModels(available, search.value);
      var parts = selected.length
        ? ["已选择 " + selected.length + " 个模型。"]
        : ["限制模式至少选择一个模型；也可手动添加精确 ID。"];
      if (matching.length > MAX_VISIBLE) {
        parts.push(
          "当前显示前 " +
            MAX_VISIBLE +
            " 个，共 " +
            matching.length +
            " 个，请用搜索缩小范围。",
        );
      } else if (matching.length) {
        parts.push("当前显示 " + matching.length + " 个。");
      }
      hint.textContent = parts.join(" ");
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
      var visible = shown.slice(0, MAX_VISIBLE);
      list.replaceChildren();
      if (!visible.length) {
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
      visible.forEach(function (id) {
        var checkbox = el("input", null, { type: "checkbox" });
        checkbox.checked = selected.indexOf(id) >= 0;
        checkbox.addEventListener("change", function () {
          selected = toggleSelection(selected, id, checkbox.checked);
          updateHint();
        });
        var option = field(id, checkbox);
        option.className = "models-item";
        if (catalogLoaded && discovered.indexOf(id) < 0) {
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
      preserved = normalizeIDs(preserved.concat([result.id]));
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

    function cancelLoad() {
      generation++;
      if (controller) {
        controller.abort();
        controller = null;
      }
    }

    function finishLoad() {
      root.setAttribute("aria-busy", "false");
    }

    function load(force) {
      if (disposed) return;
      if (signal && signal.aborted) {
        status.textContent = "模型目录加载已取消。";
        finishLoad();
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

      loadIDs(loadCatalog, { signal: local.signal, force: !!force })
        .then(function (ids) {
          if (disposed || current !== generation || local.signal.aborted)
            return;
          controller = null;
          discovered = ids.slice();
          catalogLoaded = true;
          available = mergeAvailable(
            discovered,
            normalizeIDs(preserved.concat(selected)),
          );
          renderList();
          updateHint();
          warning.hidden = true;
          warning.textContent = "";
          status.textContent = "已加载 " + discovered.length + " 个模型。";
          finishLoad();
        })
        .catch(function (error) {
          if (
            disposed ||
            current !== generation ||
            isAbort(error) ||
            local.signal.aborted
          )
            return;
          controller = null;
          warning.hidden = false;
          warning.textContent =
            "模型目录加载失败。当前模式与已选模型保持不变；可手动添加精确 ID 或稍后刷新。";
          status.textContent = "";
          finishLoad();
        });
    }

    function onHostAbort() {
      if (disposed) return;
      cancelLoad();
      status.textContent = "模型目录加载已取消。";
      finishLoad();
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
    search.addEventListener("input", function () {
      renderList();
      updateHint();
    });
    selectAll.addEventListener("click", function () {
      filterModels(available, search.value)
        .slice(0, MAX_VISIBLE)
        .forEach(function (id) {
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

  ObserverModels.clearCache = clearCache;

  var api = {
    ObserverModels: ObserverModels,
    clearCache: clearCache,
    loadIDs: loadIDs,
    createCache: createCache,
    parseIDs: parseIDs,
    initialState: initialState,
    mergeAvailable: mergeAvailable,
    toggleSelection: toggleSelection,
    addExactModel: addExactModel,
    readAllowed: readAllowed,
    filterModels: filterModels,
    CACHE_TTL_MS: CACHE_TTL_MS,
    MAX_VISIBLE: MAX_VISIBLE,
  };

  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ObserverModels = ObserverModels;
})();
