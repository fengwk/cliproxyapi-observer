'use strict';

/*
 * Node 单元测试：`node --test internal/web/models.test.cjs`
 *
 * 覆盖目标（组件 DOM 由主任务的真实浏览器验收负责）：
 *  - 发现只请求固定的 auth-files 路由，最多四个请求并行。
 *  - 缓存五分钟、只保存公开字符串、刷新可强制绕过，部分失败不吞掉成功结果。
 *  - abort 会取消排队/在途请求，迟到结果不得写入缓存。
 *  - 选择助手实现“默认不限、显式非空、保留目录外 ID、手动精确 ID”语义。
 *  - 源码保持零依赖、纯文本渲染、可访问标签与 models-* 样式钩子。
 */

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const models = require('./models.js');
const source = fs.readFileSync(path.join(__dirname, 'models.js'), 'utf8');

const FILES = models.AUTH_FILES_PATH;

function abortError() {
  return new DOMException('aborted', 'AbortError');
}

function wait(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal && signal.aborted) {
      reject(abortError());
      return;
    }
    const timer = setTimeout(() => {
      cleanup();
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      cleanup();
      reject(abortError());
    };
    function cleanup() {
      if (signal) signal.removeEventListener('abort', onAbort);
    }
    if (signal) signal.addEventListener('abort', onAbort, { once: true });
  });
}

function modelName(url) {
  return new URL('http://local' + url).searchParams.get('name');
}

// ---------------------------------------------------------------------------
// 导出面
// ---------------------------------------------------------------------------

test('exports the browser component plus pure discovery and selection helpers', () => {
  assert.equal(typeof models.ObserverModels, 'function');
  for (const name of [
    'discoverModels',
    'createLimiter',
    'createCache',
    'parseFiles',
    'parseModels',
    'initialState',
    'mergeAvailable',
    'toggleSelection',
    'addExactModel',
    'readAllowed',
    'filterModels',
    'modelsPath'
  ]) {
    assert.equal(typeof models[name], 'function', name);
  }
  assert.equal(models.AUTH_FILES_PATH, '/v0/management/auth-files');
  assert.equal(models.CACHE_TTL_MS, 5 * 60 * 1000);
  assert.equal(models.MAX_CONCURRENCY, 4);
});

// ---------------------------------------------------------------------------
// 响应解析与模型身份
// ---------------------------------------------------------------------------

test('parses auth-file and model payloads defensively while preserving exact identities', () => {
  // 无效条目被忽略，重复项去重，外层空白归一；provider 前缀等内部字符不改变。
  assert.deepEqual(
    models.parseFiles({
      files: [{ name: 'a' }, { name: ' a ' }, { name: '' }, { name: 42 }, null, {}]
    }),
    ['a']
  );
  assert.deepEqual(models.parseFiles([{ name: 'x' }, 'y']), ['x', 'y']);
  assert.deepEqual(models.parseFiles({ files: [] }), []);
  assert.throws(() => models.parseFiles({}), /响应格式无效/);

  assert.deepEqual(
    models.parseModels({
      models: [
        { id: 'openai/gpt-4o' },
        { id: 'openai/gpt-4o' },
        { id: '  anthropic/claude-3.5-sonnet ' },
        { id: '' },
        { id: 7 },
        null
      ]
    }),
    ['openai/gpt-4o', 'anthropic/claude-3.5-sonnet']
  );
  assert.deepEqual(models.parseModels([{ id: 'x' }, 'y']), ['x', 'y']);
  assert.throws(() => models.parseModels({ models: {} }), /响应格式无效/);
  assert.equal(
    models.modelsPath('a b&c'),
    '/v0/management/auth-files/models?name=a%20b%26c'
  );
});

// ---------------------------------------------------------------------------
// 选择语义
// ---------------------------------------------------------------------------

test('selection helpers default to unrestricted and enforce nonempty explicit restrictions', () => {
  // 默认 all 必须映射为 read() === []；非空初始值进入显式限制。
  assert.deepEqual(models.initialState([]), {
    restricted: false,
    selected: [],
    available: []
  });
  assert.deepEqual(models.initialState([' a ', 'a', 'b']), {
    restricted: true,
    selected: ['a', 'b'],
    available: ['a', 'b']
  });
  // 非空但无效的初始值也必须 fail closed，不能退化成“不限模型”。
  assert.deepEqual(models.initialState(['  ', null]), {
    restricted: true,
    selected: [],
    available: []
  });
  assert.deepEqual(models.readAllowed(false, ['a']), []);
  assert.deepEqual(models.readAllowed(true, ['a', 'b']), ['a', 'b']);
  assert.throws(() => models.readAllowed(true, []), /至少选择一个模型/);
  assert.throws(() => models.readAllowed(true, ['  ', null]), /至少选择一个模型/);
});

test('toggling two exact IDs yields both, unchecking removes one, and preserved IDs survive merge', () => {
  // “选 2”场景：两个精确 ID 按选择顺序返回。
  let selected = models.toggleSelection([], 'a', true);
  selected = models.toggleSelection(selected, 'b', true);
  assert.deepEqual(selected, ['a', 'b']);
  assert.deepEqual(models.readAllowed(true, selected), ['a', 'b']);

  selected = models.toggleSelection(selected, 'a', false);
  assert.deepEqual(selected, ['b']);
  assert.deepEqual(models.toggleSelection(selected, 'missing', false), ['b']);

  // 已保留项（初始值或手动项）优先，即使目录刷新后不再出现也不会丢失。
  assert.deepEqual(
    models.mergeAvailable(['new', 'shared'], ['old', 'shared']),
    ['old', 'shared', 'new']
  );
});

test('manual exact IDs are trimmed, deduplicated, auto-selected, and search is case-insensitive', () => {
  assert.deepEqual(models.addExactModel(['a'], ['a'], ' openai/gpt-4o '), {
    id: 'openai/gpt-4o',
    available: ['a', 'openai/gpt-4o'],
    selected: ['a', 'openai/gpt-4o']
  });
  assert.throws(() => models.addExactModel([], [], '   '), /请输入精确模型 ID/);

  assert.deepEqual(models.filterModels(['OpenAI/GPT-4o', 'claude-3'], 'gpt'), [
    'OpenAI/GPT-4o'
  ]);
  assert.deepEqual(models.filterModels(['a', 'b'], '  '), ['a', 'b']);
});

// ---------------------------------------------------------------------------
// 发现、并发与缓存
// ---------------------------------------------------------------------------

test('discovery uses fixed routes, deduplicates IDs, bounds parallel calls to four, and reports progress', async () => {
  const files = ['a', 'b', 'c', 'd', 'e', 'f'];
  const paths = [];
  const progress = [];
  let active = 0;
  let peak = 0;

  async function discover(url, { signal }) {
    paths.push(url);
    active++;
    peak = Math.max(peak, active);
    try {
      await wait(10, signal);
      if (url === FILES) {
        return { files: files.map((name) => ({ name, secret: 'ignored' })) };
      }
      const name = modelName(url);
      return {
        models: [{ id: 'model/' + name }, { id: 'shared' }],
        secret: 'ignored'
      };
    } finally {
      active--;
    }
  }

  const result = await models.discoverModels(discover, {
    limiter: models.createLimiter(4),
    onProgress: (...args) => progress.push(args)
  });

  // 根目录请求完成后才并发 6 个账户，限流器必须把峰值压在 4。
  assert.equal(peak, 4);
  assert.equal(paths[0], FILES);
  assert.equal(paths.filter((url) => url === FILES).length, 1);
  assert.equal(paths.length, 7);
  assert.ok(
    paths
      .slice(1)
      .every((url) => url.startsWith(FILES + '/models?name='))
  );

  // 跨账户重复 ID 只保留一次，完整模型身份（含斜杠）不截断。
  assert.equal(result.total, 6);
  assert.equal(result.failures.length, 0);
  assert.equal(result.models.length, 7);
  assert.equal(result.models.filter((id) => id === 'shared').length, 1);
  assert.ok(result.models.includes('model/f'));

  // 进度从 0/N 到 N/N，每次账户完成都推进一次。
  assert.deepEqual(progress[0], [0, 6, 0]);
  assert.deepEqual(progress[progress.length - 1], [6, 6, 0]);
  assert.equal(progress.length, 7);
});

test('successful discovery is cached for five minutes, refreshes bypass it, and only public strings are stored', async () => {
  const cache = models.createCache();
  let now = 1_000_000;
  let calls = 0;

  async function discover(url, { signal }) {
    calls++;
    await wait(0, signal);
    if (url === FILES) {
      return {
        files: [{ name: 'account', token: 'secret-root' }],
        token: 'secret-root'
      };
    }
    return {
      models: [
        { id: 'model-a', token: 'secret-model' },
        { id: 'model-b' }
      ],
      token: 'secret-model'
    };
  }

  const first = await models.discoverModels(discover, {
    cache,
    now: () => now
  });
  assert.deepEqual(first.models, ['model-a', 'model-b']);
  assert.equal(calls, 2);

  // 差一毫秒仍命中缓存。
  now += models.CACHE_TTL_MS - 1;
  const second = await models.discoverModels(discover, {
    cache,
    now: () => now
  });
  assert.deepEqual(second.models, ['model-a', 'model-b']);
  assert.equal(calls, 2);

  // 达到 TTL 后失效并重新发现。
  now += 1;
  const third = await models.discoverModels(discover, {
    cache,
    now: () => now
  });
  assert.deepEqual(third.models, ['model-a', 'model-b']);
  assert.equal(calls, 4);

  // 刷新按钮使用 force，必须绕过缓存。
  const forced = await models.discoverModels(discover, {
    cache,
    now: () => now,
    force: true
  });
  assert.deepEqual(forced.models, ['model-a', 'model-b']);
  assert.equal(calls, 6);

  // 缓存条目只能是公开 ID 数组，不能出现任何响应字段或凭据。
  for (const [key, entry] of cache) {
    assert.ok(
      key === FILES || key.startsWith(FILES + '/models?name='),
      'cached key must be a fixed route'
    );
    assert.ok(Array.isArray(entry.ids));
    for (const id of entry.ids) assert.equal(typeof id, 'string');
    assert.equal(JSON.stringify(entry).includes('secret'), false);
  }
});

test('partial account failures keep successful models and return generic warnings', async () => {
  const cache = models.createCache();

  async function discover(url, { signal }) {
    await wait(0, signal);
    if (url === FILES) {
      return { files: [{ name: 'ok' }, { name: 'broken' }, { name: 'empty' }] };
    }
    const name = modelName(url);
    if (name === 'broken') throw Error('sk-live-DO-NOT-LEAK');
    if (name === 'empty') return { models: [] };
    return { models: [{ id: 'model/ok' }, { id: 'shared' }] };
  }

  const result = await models.discoverModels(discover, { cache });
  assert.deepEqual(result.models, ['model/ok', 'shared']);
  assert.equal(result.total, 3);
  assert.deepEqual(
    result.failures.map((failure) => failure.name),
    ['broken']
  );
  assert.equal(result.failures[0].message, '模型目录加载失败');
  assert.equal(JSON.stringify(result).includes('DO-NOT-LEAK'), false);

  for (const entry of cache.values()) {
    assert.equal(JSON.stringify(entry).includes('DO-NOT-LEAK'), false);
  }
});

test('a failed account is retried next time while successful account results stay cached', async () => {
  const cache = models.createCache();
  let flakyDown = true;
  const calls = [];

  async function discover(url, { signal }) {
    calls.push(url);
    await wait(0, signal);
    if (url === FILES) {
      return { files: [{ name: 'ok' }, { name: 'flaky' }] };
    }
    const name = modelName(url);
    if (name === 'flaky' && flakyDown) throw Error('temporary outage');
    return { models: [{ id: 'model/' + name }] };
  }

  const first = await models.discoverModels(discover, { cache });
  assert.equal(first.failures.length, 1);
  const firstCalls = calls.length;

  flakyDown = false;
  const second = await models.discoverModels(discover, { cache });
  assert.deepEqual(second.failures, []);
  assert.deepEqual(second.models, ['model/ok', 'model/flaky']);

  // 根目录与成功账户来自缓存，只重试上次失败的账户。
  assert.equal(calls.length, firstCalls + 1);
});

test('root discovery failures throw sanitized errors and never fabricate a directory', async () => {
  await assert.rejects(
    models.discoverModels(
      async () => {
        throw Error('upstream-body-secret');
      },
      { cache: models.createCache() }
    ),
    (error) =>
      error.code === 'MODEL_DISCOVERY_FAILED' &&
      /auth-files/.test(error.message) &&
      !/secret/.test(error.message)
  );

  await assert.rejects(
    models.discoverModels(async () => ({ files: {} }), {
      cache: models.createCache()
    }),
    (error) => error.code === 'MODEL_DISCOVERY_FAILED'
  );
});

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

test('an already-aborted signal performs no discovery', async () => {
  const controller = new AbortController();
  controller.abort();
  let calls = 0;

  await assert.rejects(
    models.discoverModels(
      async () => {
        calls++;
        return {};
      },
      { signal: controller.signal }
    ),
    (error) => error.name === 'AbortError'
  );
  assert.equal(calls, 0);
});

test('abort cancels queued and in-flight work without caching late model results', async () => {
  const controller = new AbortController();
  const cache = models.createCache();
  let modelCalls = 0;

  async function discover(url, { signal }) {
    if (url === FILES) return { files: [{ name: 'a' }, { name: 'b' }, { name: 'c' }] };
    modelCalls++;
    await new Promise((resolve, reject) => {
      const timer = setTimeout(resolve, 200);
      const onAbort = () => {
        clearTimeout(timer);
        reject(abortError());
      };
      if (signal) signal.addEventListener('abort', onAbort, { once: true });
    });
    return { models: [{ id: 'late' }] };
  }

  const pending = models.discoverModels(discover, {
    cache,
    signal: controller.signal,
    concurrency: 2,
    onProgress: (done) => {
      if (done === 0) setImmediate(() => controller.abort());
    }
  });

  await assert.rejects(pending, (error) => error.name === 'AbortError');
  // 并发上限为 2，第三个账户仍在队列时被 abort 直接拒绝。
  assert.equal(modelCalls, 2);
  assert.equal(
    [...cache.keys()].some((key) => key.startsWith(FILES + '/models?name=')),
    false
  );
});

// ---------------------------------------------------------------------------
// 静态契约
// ---------------------------------------------------------------------------

test('models.js is dependency-free, text-only, and keeps accessibility/style hooks', () => {
  // 组件不得引入第三方依赖，也不得用标记注入渲染不可信模型 ID。
  assert.equal(/https?:\/\//i.test(source), false);
  for (const forbidden of [
    'innerHTML',
    'outerHTML',
    'insertAdjacentHTML',
    'document.write',
    'eval(',
    'new Function',
    'window.open',
    'textarea',
    'require('
  ]) {
    assert.equal(source.includes(forbidden), false, 'must not use ' + forbidden);
  }

  assert.match(source, /window\.ObserverModels\s*=/);
  assert.match(source, /ObserverUI/);
  assert.match(source, /typeof UI\.el/);
  assert.match(source, /typeof UI\.field/);
  assert.match(source, /\/v0\/management\/auth-files/);
  assert.match(source, /models-root/);
  assert.match(source, /models-list/);
  assert.match(source, /role: "status"/);
  assert.match(source, /role: "alert"/);
  assert.match(source, /aria-live/);
  assert.match(source, /aria-label/);
});
