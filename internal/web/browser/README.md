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
    1280-white-request-detail.png
    390-dark-...png
    ...             # 1280/390 × white/light/dark/auto × 5 个界面 = 40 张
```

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
| PUT/DELETE | `/keys/{id}` | 更新 / 撤销（204） |
| POST | `/keys/{id}/rotate` | `{key,secret}` |
| POST | `/keys/{id}/reset` | `{windows:[...]}` |
| GET | `/requests` | 分页、RFC3339 `from/to` 与六个筛选参数 |
| GET | `/requests/{id}` | 详情；只应由日志行点击触发 |
| GET | `/summary` | 汇总与 `granularity/buckets` |
| GET/PUT | `/settings` | `DESIGN.md` 全字段 |
| GET | `/prices` | pricing Snapshot |
| POST | `/prices/refresh` | 更新 `last_sync` 并计数 |
| POST | `/compact` | Health 并计数 |
| GET | 资源/管理前缀下的 `/ui`, `/ui/`, `/ui.html`, `/ui.js`, `/ui.css` | 公开资源，带 CSP；`ui.js` 是 Go `Asset` 同款 bundle |

资源同时通过
`/v0/resource/plugins/cliproxyapi-observer/...` 与
`/v0/management/plugins/cliproxyapi-observer/...` 提供。管理数据始终要求鉴权；
资源外壳公开。与 Go 的 `Asset("ui.js")` 一致，固定 `/ui.js` 响应体是
`settings.js + "\n" + ui.js`；`/settings.js` 不作为独立资源暴露。mock 只保存假
Key 的 SHA-256，明文 secret 仅在创建/轮换响应中出现。

假数据同时覆盖：

- 成功/失败请求、完整/unknown 核算质量、hour 桶与筛选联动；
- 合法 `HH:mm-HH:mm` 价格规则（含跨夜边界 `00:00-23:59`）、手工 0 费率与
  reasoning 可选字段；
- 恶意模型名、Provider、auth_index、Key 标签、header 与请求体（用于注入回归）；
- `Authorization: Bearer [redacted]` 和 `sk-live-DO-NOT-LEAK` 对应的脱敏断言；
- 启用/停用/过期 Key、额度与并发字段、周期开始/重置时间。

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
   资产与 session 检查，最后切回资源 `/ui` 执行原 40 张截图矩阵。
9. **请求日志**：`#filters` 必须提供 `from/to/key_id/model/provider/auth_index/status`；
   查询按钮把 `model/status` 带到第 1 页；恶意模型名只作为文本，不生成
   `img/script/iframe/[onerror]/[onload]`，`window.__observerXss` 保持 undefined。
10. **详情按需**：列表渲染后 `GET /requests/{id}` 计数为 0；点击日志表的模型
    `button` 后恰好新增一次详情读取；详情对话框有可访问名称、位于视口内、
    显示脱敏数据且不泄漏假密钥。
11. **Key 管理**：列表、标签文本化、新建表单标签（标签/到期时间/并发上限/
    允许模型/5小时额度/周额度/月额度/总额度）、保存、编辑、启用/停用、轮换、
    重置、撤销；分别断言 `POST /keys`、`PUT /keys/{id}`、
    `POST /keys/{id}/rotate`、`POST /keys/{id}/reset`、`DELETE /keys/{id}`。
    创建/轮换后页面出现一次性 `observer_sk_...` 明文，URL 中绝不出现。
12. **设置**：捕获/保留/价格/压缩模块标签可见；保存发送完整 `PUT /settings`；
    价格刷新与物理压缩分别发送 `POST /prices/refresh`、`POST /compact`。
    general save 必须使用实际字段值，并原样保留已确认的 `settings.pricing`。
13. **价格编辑器**：加载 seed 手工价与规则；新增手工价覆盖全 0 费率、reasoning
    留空与显式 `0` 的差异；新增规则写入 ID/模型/Token 阈值/UTC 区间/全部费率，
    上移后按新顺序保存。`24:00` 区间与 7 位小数必须在前端拒绝且不发送 PUT。
    保存 payload 只能是 settings 字段，pricing 位于 `settings.pricing` 且保留
    general 配置；general save 必须保留已保存 pricing，不能写入未保存的价格草稿。
    字段通过实际 `id` 后缀与 `data-testid` 定位。
14. **可访问性**：可见 input/select/textarea/button 均有可访问名称；对话框有
    名称；表格有表头；`<html lang>` 与 `document.title` 存在。
15. **安全与错误**：无 HTML 注入、无 `console.error`/页面异常；CSP 安全日志
    （包括内联 style 违规）按错误处理。刻意 401 的网络诊断单独记录在
    `expected_diagnostics`；破坏性操作的原生 `confirm` 会被接受并记录在
    `native_dialogs`，随后仍断言对应 API 调用。
16. **布局**：每个视口/主题/页面断言
    `scrollWidth <= innerWidth + 1`；Key 表单与详情对话框不超出视口。
17. **截图矩阵**：1280×900 与 390×844；white/light/dark/auto；请求日志、
    Key 管理、设置、Key 新建表单、请求详情。页面使用整页截图，对话框使用
    视口截图。`auto` 通过 CDP 模拟 `prefers-color-scheme: dark`。

## 对主 UI 的契约假设

测试按产品契约定位元素；主 UI 需要保持：

- `#login` 内的 `input[type=password]` 与文本“登录”按钮；
- 导航按钮文本“请求日志”“Key 管理”“设置”；
- `#theme`、`#interval`、`#refresh`、`#content`；
- 日志表中模型为 `button`，按钮文本等于模型标识；
- `#filters` 内 `from/to/key_id/model/provider/auth_index/status`；
- Key 新建/编辑使用对话框，字段标签如上，保存按钮文本“保存”，
  操作按钮文本“编辑/启用/停用/撤销/轮换/重置”；
- 设置模块提供捕获、保留、价格、压缩相关标签与按钮。

如果这些名称/结构发生变化，测试会明确失败并打印缺失项，而不是静默跳过。
测试不会修改主 UI、不会启动真实 CPA，也不会读取浏览器用户 profile。
