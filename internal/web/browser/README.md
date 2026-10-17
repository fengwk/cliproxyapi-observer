# Observer 浏览器验收（真实 Chrome CDP，零 npm 依赖）

本目录只包含本地验收夹具，不参与 `go:embed`，不会被打包进插件二进制。

- `mock-server.cjs`：`127.0.0.1` 上的假 Observer 宿主，实现 `DESIGN.md`
  管理 API 的完整形状，记录每次调用与状态变更。
- `mock-server.test.cjs`：`node:test` 合约回归，不启动浏览器。
- `run.cjs`：Node 22 + 真实 `/usr/bin/google-chrome-stable` 的 CDP 验收，
  不依赖 Playwright/Puppeteer/npm 包。

## 运行

```bash
# 1) 管理 API / mock 合约（无需 Chrome）
node --test internal/web/browser/mock-server.test.cjs

# 2) 真实 Chrome 浏览器验收
node internal/web/browser/run.cjs \
  --report-dir /home/fengwk/proj/cliproxyapi-observer/.workspace/observer-web/browser-report

# 3) 可选：真实 host 只读 smoke
#    管理密钥只从环境变量读取，不提供 CLI 明文参数。
OBSERVER_MANAGEMENT_KEY=... node internal/web/browser/run.cjs \
  --base-url https://cpa.example/v0/management/plugins/cliproxyapi-observer/ui \
  --report-dir /tmp/observer-realhost-smoke
```

可选参数：

```bash
node internal/web/browser/run.cjs \
  --report-dir /tmp/observer-browser-report \
  --chrome /usr/bin/google-chrome-stable
```

- `--report-dir` 必须指向仓库之外的持久目录；缺省为
  `<worktree>/../browser-report`。
- `CHROME_BIN` 环境变量可覆盖 Chrome 路径。
- `--keep-profile` 保留临时 Chrome profile，仅用于排障。
- `--base-url` 执行真实 host 的只读 smoke；管理密钥只从
  `OBSERVER_MANAGEMENT_KEY` 环境变量读取，CLI 不接受 `--management-key`，
  避免密钥进入 shell history/进程列表。未提供密钥时只验证公开 UI 外壳。
- smoke 在 CDP `Fetch.requestPaused` 的 Request 阶段检查方法：非
  GET/HEAD/OPTIONS 立即 `Fetch.failRequest`，**请求不会真正发出**；同时仍保
  留事后方法集合断言。该模式不打开 Key 表单，不保存/轮换/重置/撤销/刷新
  价格/物理压缩。
- `--base-url` 可传完整 `/ui` URL、插件 management/resource 前缀，或仅 host；
  但拒绝 userinfo、query、hash，避免把带 token 的 URL 写入 `report.json`。
  smoke 会规范化为固定 `/ui` 入口。

脚本在任一断言失败时以非零码退出。输出：

```text
<report-dir>/
  report.json       # 断言、截图、console 错误、全部 API 调用
  summary.txt       # 简短结果
  screenshots/
    1280-white-requests.png
    1280-white-keys.png
    1280-white-settings.png
    1280-white-key-form.png
    1280-white-key-models.png
    1280-white-request-detail.png
    390-dark-...png
    ...             # 1280/390 × white/light/dark/auto × 6 个界面 = 48 张
```

真实 host 只读 smoke 输出 `report.json`、`summary.txt` 与
`screenshots/realhost-smoke.png`，`report.json` 标记
`mode:"realhost-readonly"`、`readonly:true`、`fetch_interception:true`，
并列出 `api_requests`、`blocked_requests` 与允许的 `readonly_methods`。

## mock API

所有管理路由位于
`/v0/management/plugins/cliproxyapi-observer`（同时接受带反向代理前缀的路径），
要求 `Authorization: Bearer <管理密钥>`，错误统一为
`{"error":{"code":"...","message":"..."}}`，响应带 `Cache-Control: no-store`，
不发送任何 CORS 头。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/health` | `Health` + `version` + `schema_version:2` |
| GET/POST | `/keys` | 列表 / 201 `{key,secret}` |
| PUT/DELETE | `/keys?id={id}` | 更新 / 撤销（204） |
| POST | `/keys/rotate?id={id}` | `{key,secret}` |
| POST | `/keys/reset?id={id}` | `{windows:[...]}` |
| POST | `/keys/recover?id={id}` | 要求 `{"acknowledge":true}`；不清 `used/starts/resets/limits` |
| GET | `/requests` | 分页、RFC3339 `from/to` 与六个筛选参数 |
| GET | `/requests/detail?id={id}` | 详情；只应由日志行点击触发 |
| GET | `/summary` | 汇总与 `granularity/buckets` |
| GET/PUT | `/settings` | `DESIGN.md` 全字段 |
| GET | `/prices` | pricing Snapshot；同一模型可含多个 provider 的独立 auto 费率 |
| POST | `/prices/refresh` | 更新 `last_sync` 并计数 |
| POST | `/compact` | Health 并计数 |
| GET | `/v0/management/auth-files` | CPA 模型可用性目录：`{files:[{name,...}]}` |
| GET | `/v0/management/auth-files/models?name=...` | `{models:[{id,...}]}`，每个文件至少 4 个真实模型 |
| GET | 资源/管理前缀下的 `/ui`, `/ui/`, `/ui.html`, `/ui.js`, `/ui.css` | 公开资源，带 CSP；`ui.js` 是 Go `Asset` 同款 bundle |

旧动态路由 `/keys/{id}`、`/keys/{id}/rotate|reset|recover`、`/requests/{id}`
**不再注册**；脚本只在负测试中确认它们返回 404 且没有副作用。

资源同时通过
`/v0/resource/plugins/cliproxyapi-observer/...` 与
`/v0/management/plugins/cliproxyapi-observer/...` 提供。管理数据始终要求鉴权；
资源外壳公开。与 Go 的 `Asset("ui.js")` 一致，固定 `/ui.js` 响应体是
`settings.js + "\n" + models.js + "\n" + ui.js`；`settings.js` / `models.js`
不作为独立资源暴露。mock 只保存假 Key 的 SHA-256，明文 secret 仅在创建/轮换
响应中出现；auth-files 内部凭证不会进入公开响应、日志或 UI。

mock 控制端点 `/__mock/control` 可注入 auth-files 目录的失败/延迟：

```json
{"auth_files":{"delay_ms":1200,"fail_root":false,"fail_models":false,"fail_names":["google-vertex.json"]}}
```


假数据同时覆盖：

- 成功/失败请求、完整/unknown 核算质量、hour 桶与筛选联动；
- 合法 `HH:mm-HH:mm` 价格规则（含跨夜边界 `00:00-23:59`）、手工 0 费率与
  reasoning 可选字段；
- 同一模型 `gpt-5-codex` 的 `openai` / `azure` 两条 auto 快照费率，以及无
  provider 的 model-wide 手工价；
- 恶意模型名、Provider、auth_index、Key 标签、header 与请求体（用于注入回归）；
- `Authorization: Bearer [redacted]` 和 `sk-live-DO-NOT-LEAK` 对应的脱敏断言；
- 启用/停用/过期/受限/待恢复 Key、额度与并发字段、周期开始/重置时间；
- 目录外历史模型 ID（`retired-model-v0`）与 runtime auth-file；
- auth-files 部分失败、整体失败与可测延迟。

## 浏览器断言

`run.cjs` 首先自检 mock 合约，再启动真实 Chrome：

1. **登录门禁**：初始只有 `#login`；错误密码得到 401 且停留登录页；正确密码后
   主界面出现，密钥不出现在 URL，除刻意 401 外所有管理请求都带正确 Bearer。
2. **顶栏**：`#theme` 必须包含 `white/light/dark/auto`；`#interval` 必须包含
   `0/5/15/30/60`；`#refresh` 触发新的 `GET /requests`。
3. **轮询时序**：登录后默认 5 秒必须产生真实 `GET /requests`；切到 `0` 后
   不得再调度长定时器。打开详情对话框、编辑筛选产生脏状态、隐藏标签页都会
   清理轮询定时器；关闭对话框/恢复可见后按 5 秒恢复，脏状态恢复可见仍不调度。
   通过包装 `setTimeout/clearTimeout` 记录调度与清理，不靠固定 sleep 断言。
4. **重叠刷新**：用页面内 `fetch` gate 延迟旧请求。先发 `gemini-2.5-pro`
   再发 `claude-opus-4-1`：断言旧请求被 `AbortController` 中止；再模拟忽略
   Abort 的迟到响应，断言 generation 保护不会让旧结果覆盖新结果。
5. **认证刷新 401**：刷新中注入 401 后必须停止轮询、清空
   `sessionStorage`、隐藏导航/清空内容并回登录页；认证错误不得附加历史查询
   提示（仅历史相关错误追加）；随后重新登录并清空筛选。
6. **在途输入**：延迟一次旧刷新并在其未完成时 focus 筛选输入框、修改草稿。
   断言 focus/input 会 stop/abort 旧刷新；随后释放迟到响应，输入值、焦点和
   表格不被旧响应覆盖。
7. **同源 iframe**：通过页面内 evaluate 插入资源 URL iframe。断言 iframe 直接
   继承本标签 `sessionStorage`、不出现第二次登录；`auto` 主题初始跟随父页面
   dark，父页面切 white/dark 时通过 MutationObserver 动态跟随。移除 iframe
   后主页面仍需可刷新；只读展示，不读取或猜测其它 CPA 存储。
8. **路由 smoke**：真实 Chrome 打开资源前缀 `/ui/`（尾斜杠），断言 HTML 的
   `/ui.css` 与 `/ui.js` 请求固定走 `/v0/resource/plugins/cliproxyapi-observer/`
   且状态均为 200、无 404、无 `/ui/ui.js` 或 `/ui/ui.css` 相对请求；
   `sessionStorage` 恢复后直接显示请求日志。再打开 management `/ui` 入口重复
   资产与 session 检查，最后切回资源 `/ui` 执行 48 张截图矩阵。
9. **请求日志**：`#filters` 必须提供 `from/to/key_id/model/provider/auth_index/status`；
   查询按钮把 `model/status` 带到第 1 页；恶意模型名只作为文本，不生成
   `img/script/iframe/[onerror]/[onload]`，`window.__observerXss` 保持 undefined。
10. **详情按需**：列表渲染后 `GET /requests/detail` 计数为 0；点击日志表的模型
    `button` 后恰好新增一次 `GET /requests/detail?id={id}`；详情对话框有可访问
    名称、位于视口内、显示脱敏数据且不泄漏假密钥。
11. **Key 管理**：列表、标签文本化、新建表单标签、保存、编辑、启用/停用、轮换、
    重置、核对后恢复、撤销；分别断言 `POST /keys`、`PUT /keys?id`、
    `POST /keys/rotate?id`、`POST /keys/reset?id`、`POST /keys/recover?id`、
    `DELETE /keys?id`。创建/轮换后页面出现一次性 `observer_sk_...` 明文，URL
    中绝不出现；恢复先显示未知用量警告，勾选核对声明后才发送
    `{"acknowledge":true}`，并断言 `used/starts/resets/limits` 不变。
12. **Key 模型选择器**：新建/编辑对话框挂载 `models.js` 组件；默认 `all` 提交
    `allowed_models:[]`；切到 restricted 后可搜索、勾选两个精确模型；搜索词在
    勾选后保留；编辑回显选择；目录外历史 ID 保留并标记“目录未列出”；可手动
    追加精确 ID；部分 auth-file 失败显示警告但保留成功模型；慢目录中关闭对话框
    会 abort，迟到响应不写入 UI 且不泄漏凭据。
13. **设置**：捕获/保留/价格/压缩模块标签可见；保存发送完整 `PUT /settings`；
    价格刷新与物理压缩分别发送 `POST /prices/refresh`、`POST /compact`。
    general save 必须使用实际字段值，并原样保留已确认的 `settings.pricing`。
    `max_body_bytes` 超过 8 MiB 时必须被原生约束和提交层双重拒绝，且不得发送 PUT。
14. **价格编辑器**：加载 seed 手工价与规则；展开只读快照后，同一模型
    `gpt-5-codex` 的 `openai` / `azure` 两行 provider 与费率必须可区分，手工价
    仍为无 provider 的 model-wide 行。帮助文本必须说明完整提示词阈值包含
    未缓存输入 + 缓存读 + 缓存写，且手工价提供缓存读/写费率字段。
    新增手工价覆盖全 0 费率、reasoning 留空与显式 `0` 的差异；新增规则写入
    ID/模型/Token 阈值/UTC 区间/全部费率，上移后按新顺序保存。`24:00` 区间与
    7 位小数必须在前端拒绝且不发送 PUT。保存 payload 只能是 settings 字段，
    pricing 位于 `settings.pricing` 且保留 general 配置；general save 必须保留
    已保存 pricing，不能写入未保存的价格草稿。字段通过实际 `id` 后缀与
    `data-testid` 定位。
15. **可访问性**：可见 input/select/textarea/button 均有可访问名称；对话框有
    名称；表格有表头；`<html lang>` 与 `document.title` 存在。
16. **安全与错误**：无 HTML 注入、无 `console.error`/页面异常；CSP 安全日志
    （包括内联 style 违规）按错误处理。real smoke 在 CDP Fetch Request 阶段
    阻断非 GET/HEAD/OPTIONS，写请求不会真正发出；刻意 401 与目录失败注入的
    网络诊断单独记录在 `expected_diagnostics`；破坏性操作的原生 `confirm`
    会被接受并记录在 `native_dialogs`，随后仍断言对应 API 调用。
17. **布局**：每个视口/主题/页面断言
    `scrollWidth <= innerWidth + 1`；Key 表单、模型选择器与详情对话框不超出视口。
    390 宽度下 Key 表单滚到顶部和底部时，“保存”“取消”按钮均必须保持在
    dialog viewport 内。
18. **截图矩阵**：1280×900 与 390×844；white/light/dark/auto；请求日志、
    Key 管理、设置、Key 新建表单、Key 模型选择器、请求详情。页面使用整页截图，
    对话框使用视口截图。`auto` 通过 CDP 模拟 `prefers-color-scheme: dark`。

## 对主 UI 的契约假设

测试按产品契约定位元素；主 UI 需要保持：

- `#login` 内的 `input[type=password]` 与文本“登录”按钮；
- 导航按钮文本“请求日志”“Key 管理”“设置”；
- `#theme`、`#interval`、`#refresh`、`#content`；
- 日志表中模型为 `button`，按钮文本等于模型标识；
- `#filters` 内 `from/to/key_id/model/provider/auth_index/status`；
- Key 新建/编辑使用对话框，字段标签如上，保存按钮文本“保存”，
  操作按钮文本“编辑/启用/停用/撤销/轮换/重置”，阻塞 Key 另提供“核对后恢复”；
- 模型选择器挂载 `.models-root`，提供 `all/restricted` 单选、模型复选框、
  搜索框、手动精确 ID 与部分失败警告；
- 设置模块提供捕获、保留、价格、压缩相关标签与按钮。

如果这些名称/结构发生变化，测试会明确失败并打印缺失项，而不是静默跳过。
测试不会修改主 UI、不会对真实 host 做写操作，也不会读取浏览器用户 profile。
