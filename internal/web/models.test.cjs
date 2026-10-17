"use strict";

/*
 * Node 单元测试：`node --test internal/web/models.test.cjs`
 *
 * 覆盖目标：
 *  - 目录加载只接受 {ids} 或字符串数组，五分钟缓存与 force 刷新。
 *  - abort 不写入迟到结果；错误统一且不泄漏底层 secret。
 *  - 默认不限、限制模式无选择抛错、目录外/手动 ID 保留、最多 100 可见。
 *  - 模型 ID 的 provider/大小写/斜杠命名空间精确保留；DOM hook 稳定。
 *
 * 组件 DOM 由真实浏览器验收负责，这里保持纯 Node、零依赖。
 */

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const models = require("./models.js");
const source = fs.readFileSync(path.join(__dirname, "models.js"), "utf8");

function abortError() {
  return new DOMException("aborted", "AbortError");
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function delay(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal && signal.aborted) {
      reject(abortError());
      return;
    }
    const timer = setTimeout(resolve, ms);
    if (signal) {
      signal.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          reject(abortError());
        },
        { once: true },
      );
    }
  });
}

// ---------------------------------------------------------------------------
// 导出面
// ---------------------------------------------------------------------------

test("exports the component, cache controls, loader and selection helpers", () => {
  for (const name of [
    "ObserverModels",
    "clearCache",
    "loadIDs",
    "createCache",
    "parseIDs",
    "initialState",
    "mergeAvailable",
    "toggleSelection",
    "addExactModel",
    "readAllowed",
    "filterModels",
  ]) {
    assert.equal(typeof models[name], "function", name);
  }
  assert.equal(typeof models.ObserverModels.clearCache, "function");
  assert.equal(models.CACHE_TTL_MS, 5 * 60 * 1000);
  assert.equal(models.MAX_VISIBLE, 100);
});

// ---------------------------------------------------------------------------
// 选择语义
// ---------------------------------------------------------------------------

test("selection defaults to unrestricted and explicit restrictions fail closed", () => {
  // 空数组是“不限模型”；非空（即使内容无效）必须进入显式限制。
  assert.deepEqual(models.initialState([]), {
    restricted: false,
    selected: [],
  });
  assert.deepEqual(models.initialState([" a ", "a", "b"]), {
    restricted: true,
    selected: ["a", "b"],
  });
  assert.deepEqual(models.initialState(["  ", null]), {
    restricted: true,
    selected: [],
  });

  assert.deepEqual(models.readAllowed(false, ["a"]), []);
  assert.deepEqual(models.readAllowed(true, ["a", "b"]), ["a", "b"]);
  assert.throws(() => models.readAllowed(true, []), /至少选择一个模型/);
  assert.throws(
    () => models.readAllowed(true, ["  ", null]),
    /至少选择一个模型/,
  );
});

test("toggling two IDs preserves order and directory-missing IDs survive refresh merge", () => {
  let selected = models.toggleSelection([], "a", true);
  selected = models.toggleSelection(selected, "b", true);
  assert.deepEqual(selected, ["a", "b"]);
  assert.deepEqual(models.readAllowed(true, selected), ["a", "b"]);

  selected = models.toggleSelection(selected, "a", false);
  assert.deepEqual(selected, ["b"]);
  assert.deepEqual(models.toggleSelection(selected, "missing", false), ["b"]);

  // 初始未列出 ID 与手动 ID 排在目录结果之前，刷新后不丢失。
  assert.deepEqual(
    models.mergeAvailable(["new", "shared"], ["old", "shared"]),
    ["old", "shared", "new"],
  );
});

test("manual exact IDs are trimmed, deduplicated, auto-selected and searched case-insensitively", () => {
  assert.deepEqual(models.addExactModel(["a"], ["a"], " openai/gpt-4o "), {
    id: "openai/gpt-4o",
    available: ["a", "openai/gpt-4o"],
    selected: ["a", "openai/gpt-4o"],
  });
  assert.throws(() => models.addExactModel([], [], "   "), /请输入精确模型 ID/);
  assert.deepEqual(models.filterModels(["OpenAI/GPT-4o", "claude-3"], "gpt"), [
    "OpenAI/GPT-4o",
  ]);
  assert.deepEqual(models.filterModels(["a", "b"], "  "), ["a", "b"]);
});

// ---------------------------------------------------------------------------
// 模型 ID 命名空间
// ---------------------------------------------------------------------------

test("model namespaces are preserved exactly while trimming only outer whitespace", () => {
  const direct = models.parseIDs([
    " OpenAI/GPT-4o ",
    "openai/gpt-4o",
    "a/b",
    "a/b",
    "",
    null,
    7,
  ]);
  assert.deepEqual(direct, ["OpenAI/GPT-4o", "openai/gpt-4o", "a/b"]);
  assert.deepEqual(models.parseIDs({ ids: ["x:y/z"] }), ["x:y/z"]);

  // 不接受旧 auth-files / models 兼容形状，避免把无效响应当成目录。
  assert.throws(
    () => models.parseIDs({ files: [] }),
    (error) => {
      return (
        error.code === "MODEL_CATALOG_FAILED" &&
        error.message === "模型目录加载失败"
      );
    },
  );
  assert.throws(() => models.parseIDs({ models: ["a"] }), /模型目录加载失败/);
});

// ---------------------------------------------------------------------------
// 缓存、force 与 clearCache
// ---------------------------------------------------------------------------

test("ids are cached for five minutes, copied on read, and force bypasses the cache", async () => {
  const cache = models.createCache(() => now);
  let now = 1_000_000;
  let calls = 0;
  async function loadCatalog() {
    calls++;
    return { ids: ["openai/GPT-4o", "a/b"] };
  }

  const first = await models.loadIDs(loadCatalog, { cache });
  assert.deepEqual(first, ["openai/GPT-4o", "a/b"]);
  assert.equal(calls, 1);

  // 缓存命中不调用 loader；返回值被修改也不能污染缓存。
  first.push("mutated");
  assert.deepEqual(await models.loadIDs(loadCatalog, { cache }), [
    "openai/GPT-4o",
    "a/b",
  ]);
  assert.equal(calls, 1);

  now += models.CACHE_TTL_MS - 1;
  await models.loadIDs(loadCatalog, { cache });
  assert.equal(calls, 1);

  now += 1;
  await models.loadIDs(loadCatalog, { cache });
  assert.equal(calls, 2);

  await models.loadIDs(loadCatalog, { cache, force: true });
  assert.equal(calls, 3);

  // 缓存只保存字符串 ID，不保留 loader 响应的其他字段。
  cache.set(["public/model"]);
  assert.deepEqual(cache.get(), ["public/model"]);
});

test("clearCache and ObserverModels.clearCache both reset the shared cache", async () => {
  let calls = 0;
  async function loadCatalog() {
    calls++;
    return ["shared/model"];
  }

  models.clearCache();
  await models.loadIDs(loadCatalog);
  await models.loadIDs(loadCatalog);
  assert.equal(calls, 1);

  models.ObserverModels.clearCache();
  await models.loadIDs(loadCatalog);
  assert.equal(calls, 2);

  models.clearCache();
  await models.loadIDs(loadCatalog);
  assert.equal(calls, 3);
});

// ---------------------------------------------------------------------------
// 取消与错误
// ---------------------------------------------------------------------------

// Logout must invalidate pending cache writers, even if transport ignores abort.
test("clearing the session cache detaches in-flight catalog results", async () => {
  models.clearCache();
  let release;
  const pending = models.loadIDs(
    () =>
      new Promise((resolve) => {
        release = resolve;
      }),
  );
  await Promise.resolve();
  models.clearCache();
  release(["old-session/model"]);
  await pending;
  let calls = 0;
  const current = await models.loadIDs(async () => {
    calls++;
    return ["new-session/model"];
  });
  assert.equal(calls, 1);
  assert.deepEqual(current, ["new-session/model"]);
  models.clearCache();
});

test("an already-aborted signal never calls the loader", async () => {
  const controller = new AbortController();
  controller.abort();
  let calls = 0;

  await assert.rejects(
    models.loadIDs(
      async () => {
        calls++;
        return [];
      },
      { signal: controller.signal, cache: models.createCache() },
    ),
    (error) => error.name === "AbortError",
  );
  assert.equal(calls, 0);
});

test("abort during load rejects and late results are never cached", async () => {
  const controller = new AbortController();
  const cache = models.createCache();
  let calls = 0;

  // loader 故意忽略 signal，验证模块自身在恢复后仍拒绝并跳过缓存写入。
  let seenSignal = null;
  async function loadCatalog(options) {
    calls++;
    seenSignal = options.signal;
    await wait(30);
    return ["late/model"];
  }

  const pending = models.loadIDs(loadCatalog, {
    cache,
    signal: controller.signal,
  });
  await wait(0);
  controller.abort();
  await assert.rejects(pending, (error) => error.name === "AbortError");
  await wait(40);

  assert.equal(seenSignal, controller.signal);
  assert.equal(calls, 1);
  assert.equal(cache.get(), null);
});

test("loader failures are generic and never leak response bodies or secrets", async () => {
  const cache = models.createCache();
  await assert.rejects(
    models.loadIDs(
      async () => {
        throw Error("upstream sk-live-DO-NOT-LEAK");
      },
      { cache },
    ),
    (error) =>
      error.code === "MODEL_CATALOG_FAILED" &&
      error.message === "模型目录加载失败" &&
      !/DO-NOT-LEAK|upstream/.test(error.message),
  );
  assert.equal(cache.get(), null);

  await assert.rejects(
    models.loadIDs(async () => ({ models: [{ id: "x" }] }), { cache }),
    (error) => error.code === "MODEL_CATALOG_FAILED",
  );
  await assert.rejects(
    models.loadIDs(undefined, { cache }),
    (error) => error.code === "MODEL_CATALOG_FAILED",
  );
});

// ---------------------------------------------------------------------------
// 静态契约：DOM hooks 与已移除的旧发现路径
// ---------------------------------------------------------------------------

test("models.js keeps DOM hooks and no longer contains auth-files, limiters or credentials", () => {
  assert.match(source, /window\.ObserverModels\s*=\s*ObserverModels/);
  assert.match(source, /ObserverModels\.clearCache\s*=/);
  assert.match(source, /slice\(0, MAX_VISIBLE\)/);

  for (const hook of [
    "models-root",
    "models-mode",
    "models-restricted",
    "models-list",
    "models-search-field",
    "models-item",
    "models-empty",
    "models-actions",
    "models-select-all",
    "models-clear",
    "models-refresh",
    "models-manual",
    "models-add",
    "models-note",
    "models-hint",
    "models-status",
    "models-warning",
    "models-missing",
    'role: "status"',
    'role: "alert"',
    "aria-live",
    "aria-label",
  ]) {
    assert.ok(source.includes(hook), "missing DOM hook " + hook);
  }

  for (const removed of [
    "auth-files",
    "createLimiter",
    "MAX_CONCURRENCY",
    "credential",
  ]) {
    assert.equal(source.includes(removed), false, "must not keep " + removed);
  }
  for (const forbidden of [
    "innerHTML",
    "outerHTML",
    "insertAdjacentHTML",
    "document.write",
    "eval(",
    "new Function",
    "window.open",
    "require(",
  ]) {
    assert.equal(
      source.includes(forbidden),
      false,
      "must not use " + forbidden,
    );
  }
});
