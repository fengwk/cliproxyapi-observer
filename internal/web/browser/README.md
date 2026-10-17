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

# 2) 真实 Chrome 浏览器验收（mock 数据）
node internal/web/browser/run.cjs \
  --report-dir /home/fengwk/proj/cliproxyapi-observer/.workspace/observer-web/browser-report

# 3) 真实 host 只读 smoke
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
- `--base-url` 执行真实 host smoke；管理密钥只从
  `OBSERVER_MANAGEMENT_KEY` 环境变量读取，CLI 不接受 `--management-key`，
  避免密钥进入 shell history/进程列表。未提供密钥时只验证公开 UI 外壳。
- `--base-url` 可传完整 `/ui` URL、插件 management/resource 前缀，或仅 host；
  但拒绝 userinfo、query、hash，避免把凭据或 token 写入 `report.json`。
  smoke 会规范化为固定 `/ui` 入口。
- real smoke 在 CDP `Fetch.requestPaused` 的 Request 阶段检查方法：仅放行
  `GET/HEAD/OPTIONS` 和**唯一**的精确
  `POST /v0/management/plugins/cliproxyapi-observer/models/token`
  （必须同源、空 query、空 body）。其他写请求立即 `Fetch.failRequest`，
  **请求不会真正发出**，并计入 `blocked_requests`。

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
| POST | `/models/token` | 管理鉴权、空 body/query，返回 30 秒有效的单次目录 capability |
| GET | `/requests` | 分页、RFC3339 `from/to` 与六个筛选参数 |
| GET | `/requests/detail?id={id}` | 详情；只应由日志行点击触发 |
| GET | `/summary` | 汇总与 `granularity/buckets` |
| GET/PUT | `/settings` | `DESIGN.md` 全字段 |
| GET | `/prices` | pricing Snapshot；同一模型可含多个 provider 的独立 auto 费率 |
| POST | `/prices/refresh` | 更新 `last_sync` 并计数 |
| POST | `/compact` | Health 并计数 |
| GET | `/v1/models` | **仅**接受 minted capability（不接受管理密钥），`{data:[{id}]}`，最多 100 条 |
| GET | 资源/管理前缀下的 `/ui`, `/ui/`, `/ui.html`, `/ui.js`, `/ui.css` | 公开资源，带 CSP；`ui.js` 是 Go `Asset` 同款 bundle |

模型目录契约：

- 打开 Key 对话框才按需调用 `POST /models/token`；请求必须使用管理 Bearer，
  body/query 为空。
- 组件立即用返回的 capability 请求精确的 `/v1/models`；token 只放在
  `Authorization: Bearer`，不进入 URL、storage 或调用日志。
- capability 单次使用、30 秒过期，mock 最多保留 32 个未消费 capability；
  使用后（包括目录请求失败）立即失效。
- `GET /v1/models` 不接受管理密钥、不经过插件前缀，也不回退到
  auth-files、pricing、config 或凭证目录；`/v0/management/auth-files` 返回 404。
- 响应有界（100 条），主模型 ID 精确保留 provider/大小写/斜杠命名空间。
- mock 调用记录只保存 `authorized`、`capability_stage`、空 body 字节数等
  布尔/阶段信息，不保存 token 或 Authorization 内容。

旧动态路由 `/keys/{id}`、`/keys/{id}/rotate|reset|recover`、`/requests/{id}`
**不再注册**；脚本只在负测试中确认它们返回 404 且没有副作用。

资源同时通过
`/v0/resource/plugins/cliproxyapi-observer/...` 与
`/v0/management/plugins/cliproxyapi-observer/...` 提供。管理数据始终要求鉴权；
资源外壳公开。与 Go 的 `Asset("ui.js")` 一致，固定 `/ui.js` 响应体是
`settings.js + "\n" + models.js + "\n" + ui.js`；`settings.js` / `models.js`
不作为独立资源暴露。mock 只保存假 Key 的 SHA-256，明文 secret 仅在创建/轮换
响应中出现。

mock 控制端点 `/__mock/control` 可注入目录失败/延迟：

```json
{"catalog":{"token_delay_ms":1200,"models_delay_ms":0,"fail_token":false,"fail_models":false}}
```

`/__mock/state` 只暴露 `catalog_control`、`catalog_ids`、
`outstanding_capabilities` 与计数器，不暴露 token 或 Authorization。

假数据同时覆盖：

- 成功/失败请求、完整/unknown 核算质量、hour 桶与筛选联动；
- 合法 `HH:mm-HH:mm` 价格规则（含跨夜边界 `00:00-23:59`）、手工 0 费率与
  reasoning 可选字段；
- 同一模型 `gpt-5-codex` 的 `openai` / `azure` 两条 auto 快照费率，以及无
  provider 的 model-wide 手工价；
- 恶意模型名、Provider、auth_index、Key 标签、header 与请求体（用于注入回归）；
- `Authorization: Bearer [redacted]` 和 `sk-live-DO-NOT-LEAK` 对应的脱敏断言；
- 启用/停用/过期/受限/待恢复 Key、额度与并发字段、周期开始/重置时间；
- 目录外历史模型 ID（`retired-model-v0`）；
- 服务端目录响应上限、token 失败/延迟与目录失败/延迟注入。

## 浏览器断言

`run.cjs` 首先自检 mock 合约，再启动真实 Chrome：

1. **登录门禁**：初始只有 `#login`；错误密码得到 401 且停留登录页；正确密码后
   主界面出现，密钥不出现在 URL，除刻意 401 外所有管理请求都带正确 Bearer。
2. **顶栏**：`#theme` 必须包含 `white/light/dark/auto`；`#interval` 必须包含
   `0/5/15/30/60`；`#refresh` 触发新的 `GET /requests`。
3. **轮询时序**：登录后默认 5 秒必须产生真实 `GET /requests`；切到 `0` 后
   不得再调度长定时器。打开详情对话框、编辑筛选产生脏状态、隐藏标签页都会
   清理轮询定时器；关闭对话框/恢复可见后按 5 秒恢复，脏状态恢复可见仍不调度。
4. **重叠刷新**：用页面内 `fetch` gate 延迟旧请求；断言旧请求被
   `AbortController` 中止，迟到响应不会覆盖新结果。
5. **认证刷新 401**：刷新中注入 401 后必须停止轮询、清空
   `sessionStorage`、隐藏导航/清空内容并回登录页；随后重新登录并清空筛选。
6. **在途输入**：延迟旧刷新并在其未完成时 focus/修改筛选草稿；断言旧响应
   不覆盖输入值、焦点和表格。
7. **同源 iframe**：iframe 直接继承本标签 `sessionStorage`、不出现第二次登录；
   `auto` 主题跟随父页面 light/dark；只读展示，不读取其它 CPA 存储。
8. **路由 smoke**：真实 Chrome 打开资源前缀 `/ui/`（尾斜杠），断言固定走
   `/v0/resource/plugins/cliproxyapi-observer/` 资产且状态均为 200；再打开
   management `/ui` 入口，最后切回资源 `/ui` 执行 48 张截图矩阵。
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
12. **Key 模型选择器**：
    - 默认 `all` 提交 `allowed_models:[]`；切到 restricted 后可搜索、勾选两个
      精确模型；搜索词在勾选后保留；编辑回显选择；
    - 目录外历史 ID 保留并标记“目录未列出”；可手动追加精确 ID；
    - 打开对话框时先 `POST /models/token`（管理 Bearer、空 body/query），再
      `GET /v1/models`（仅 capability header、无 query/额外 secret）；
    - 目录响应有界，五分钟缓存不重复签发；手动刷新强制重新签发；
    - `fail_models` 显示通用警告、保留缓存/已选且不改变 all 模式；
    - 慢目录中关闭对话框会 abort，迟到响应不写入 UI 且不泄漏凭据；
    - logout 调用 `ObserverModels.clearCache()`，重新登录打开对话框必须重新签发；
    - 模型发现不得访问 auth-files/pricing/config 目录。
13. **设置**：捕获/保留/价格/压缩模块标签可见；保存发送完整 `PUT /settings`；
    价格刷新与物理压缩分别发送 `POST /prices/refresh`、`POST /compact`。
    general save 必须使用实际字段值，并原样保留已确认的 `settings.pricing`。
    `max_body_bytes` 超过 8 MiB 时必须被原生约束和提交层双重拒绝，且不得发送 PUT。
14. **价格编辑器**：seed 手工价与规则；同一模型 `gpt-5-codex` 的
    `openai` / `azure` provider 费率可区分，手工价仍为 model-wide。帮助文本
    说明完整提示词阈值包含未缓存输入 + 缓存读 + 缓存写；支持缓存读/写费率、
    reasoning 留空与显式 `0` 的差异。`24:00` 与 7 位小数在前端拒绝且不发 PUT。
15. **可访问性**：可见 input/select/textarea/button 均有可访问名称；对话框有
    名称；表格有表头；`<html lang>` 与 `document.title` 存在。
16. **安全与错误**：无 HTML 注入、无 `console.error`/页面异常；CSP 安全日志
    （包括内联 style 违规）按错误处理；破坏性操作的原生 `confirm` 被接受并
    记录在 `native_dialogs`，随后仍断言对应 API 调用。
17. **布局**：每个视口/主题/页面断言
    `scrollWidth <= innerWidth + 1`；Key 表单、模型选择器与详情对话框不超出视口。
18. **截图矩阵**：1280×900 与 390×844；white/light/dark/auto；请求日志、
    Key 管理、设置、Key 新建表单、Key 模型选择器、请求详情。页面使用整页截图，
    对话框使用视口截图。`auto` 通过 CDP 模拟 `prefers-color-scheme: dark`。
    共 **48 张**。

## 真实 host smoke

真实 host 使用 native integration seed 的至少一个假 Key 和请求日志：

- 登录后请求日志必须至少有一行；
- 点击日志行前 `GET /requests/detail` 必须为 0，点击后恰好新增一次；
- 打开 Key 新建对话框，断言真实权威目录至少返回一个模型；只读关闭，
  **不填写、不保存、不轮换、不重置、不撤销、不刷新价格、不压缩**；
- 生成 1280/390 × light/dark × 请求日志/Key 管理/设置，共 **12 张**
  `screenshots/realhost-<width>-<theme>-<page>.png`，并额外保存代表截图
  `screenshots/realhost-smoke.png`（native browser_test 固定要求存在）；登录
  成功时截图总数为 **13**；
- 唯一允许的写请求是精确插件路径、空 query、空 body 的
  `POST /models/token`；其他写请求在 CDP Fetch Request 阶段被阻断，计入
  `blocked_requests`，正常流程必须为 0；
- `report.json` 标记 `mode:"realhost-readonly"`、`readonly:true`、
  `fetch_interception:true`，并列出 `api_requests`、`allowed_token_posts`、
  `blocked_requests` 与 `readonly_exception`；不记录管理密钥或 capability token。
- **唯一诊断豁免**：浏览器自动请求同源根路径 `/favicon.ico` 且网络状态为 404
  时，归入 `expected_diagnostics`（`reason` 明确标记为非应用资源请求），
  `favicon_404_exempted:true`。仅此精确情形被豁免；其他 404、CSP 违规、
  应用资源 404（包括 `/ui/favicon.ico`）仍按失败处理。报告 URL 只保留
  origin+path，不写入 query/hash。

## 对主 UI 的契约假设

测试按产品契约定位元素；主 UI 需要保持：

- `#login` 内的 `input[type=password]` 与文本“登录”按钮；
- 导航按钮文本“请求日志”“Key 管理”“设置”，以及顶栏“退出”；
- `#theme`、`#interval`、`#refresh`、`#content`；
- 日志表中模型为 `button`，按钮文本等于模型标识；
- `#filters` 内 `from/to/key_id/model/provider/auth_index/status`；
- Key 新建/编辑使用对话框，字段标签如上，保存按钮文本“保存”，
  操作按钮文本“编辑/启用/停用/撤销/轮换/重置”，阻塞 Key 另提供“核对后恢复”；
- 模型选择器挂载 `.models-root`，提供 `all/restricted` 单选、模型复选框、
  搜索框、手动精确 ID 与通用失败警告；
- 设置模块提供捕获、保留、价格、压缩相关标签与按钮。

如果这些名称/结构发生变化，测试会明确失败并打印缺失项，而不是静默跳过。
测试不会修改主 UI、不会对真实 host 做保存操作，也不会读取浏览器用户 profile。
