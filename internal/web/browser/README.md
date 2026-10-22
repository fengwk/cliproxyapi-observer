# Observer 浏览器验收

验收使用 Node 22、Playwright 和真实 Chrome。它打开**未经修改的官方 CPA 管理中心
v1.26.5**，由 CPA 自己渲染侧栏、浮动工具栏和插件 iframe，不使用简化父页面。
本目录只用于开发测试，不参与 `go:embed`。

- `run.cjs`：浏览器行为检查、布局矩形检查、截图和错误报告。
- `timezone-check.cjs`：Asia/Shanghai / America/New_York 真实 Chrome 时区验收，
  检查本地日期 query、UTC 日界、DST 重复小时、Key 到期往返与本地时间预览。
- `contrast.cjs`：测量实际渲染文字与共享控件边界的 sRGB 对比度。
- `shell-helper.cjs`：在独立临时目录下载官方 `management.html` 并验证 SHA-256。
- `mock-server.cjs`：仅监听回环地址，提供假宿主管理 API 与已编译的插件资源。
- `mock-server.test.cjs`：无需 Chrome 的 mock API 合约回归。

## 运行

```bash
npm ci --prefix web
make frontend
node --test internal/web/browser/mock-server.test.cjs
CHROME_BIN=/usr/bin/google-chrome-stable \
  BROWSER_REPORT_DIR=/tmp/observer-browser-report make browser
CHROME_BIN=/usr/bin/google-chrome-stable \
  node internal/web/browser/timezone-check.cjs --report-dir /tmp/observer-tz-report

# 仅验收此次价格控件与账户名称布局：320/390/640/1440px，仍使用官方 CPA 外壳。
CHROME_BIN=/usr/bin/google-chrome-stable \
  node internal/web/browser/run.cjs --ui-layout --report-dir /tmp/observer-ui-layout
```

`make build` 使用已提交的 `internal/web/dist`。更改前端源码后必须重新构建，
CI 会检查生成资源与源码一致。浏览器直接加载这些资源，不使用开发服务器或 CDN。
中文截图需要系统中文字体；Ubuntu 可安装 `fonts-noto-cjk`。CI 两种浏览器入口均安装
并用 `fc-match` 验证中文字体，避免方框字形掩盖真实文字尺寸与排版问题。

官方外壳地址：
`https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/download/v1.26.5/management.html`。
SHA-256 固定为：

```text
57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700
```

可设置 `CPA_MANAGEMENT_HTML=/absolute/path/management.html` 复用已下载文件，
**仍须通过同一 SHA-256 校验**。没有配置本地文件时，自动下载到系统临时目录。
不会修改、复制到产品资源目录或运行其他版本来绕过校验。

## 真实宿主只读验收

应使用独立测试 CPA 配置、假 Key 和本地 mock upstream，不访问生产环境：

```bash
OBSERVER_MANAGEMENT_KEY='<fake-management-key>' \
  node internal/web/browser/run.cjs \
  --base-url http://127.0.0.1:8317/v0/resource/plugins/cliproxyapi-observer/ui \
  --report-dir /tmp/observer-realhost-report

# 推荐：由 Go harness 自动创建测试配置、数据和假 upstream，
# 运行完整原生集成测试，并比较浏览器运行前后的管理状态。
OBSERVER_BROWSER=1 CHROME_BIN=/usr/bin/google-chrome-stable \
  CPA_TEST_ARTIFACTS=/absolute/temp/native-report \
  CPA_BINARY=/absolute/path/to/cli-proxy-api make integration
```

真实宿主模式要求 `OBSERVER_MANAGEMENT_KEY`，不提供明文命令行参数，也不读取宿主
认证存储。`--base-url` 不得包含 userinfo、query 或 fragment；仅取同源 origin，
实际导航固定的 `/management.html` 插件路由。管理外壳仍使用固定官方版本，
不会依赖宿主自动更新下载到哪个前端版本。

请求拦截在请求发送前只允许同源 `GET/HEAD/OPTIONS`，另允许两项只读 POST：

1. 精确插件路径 `/models/token`：空 query、空 body，只签发一次性目录能力。
2. 精确插件路径 `/prices/preview`：空 query、仅包含最多 4096 项的 `models` 数组。

其余写请求被中止并记录在 `blocked_requests`，正常流程必须为零。真实模式不会创建、
保存、轮换、重置、撤销 Key，不刷新价格或压缩数据库。Go harness 在浏览器前后
对比 Key、设置与汇总，确认没有变更。目录能力仅放请求头，不进入 URL/报告或存储。

## 检查范围与证据

Mock 模式覆盖桌面 1280、手机 390 与暖灰/纯白/深色/跟随系统四种主题，
额外覆盖 640/768/1440/1920/2560 的暖灰主题和可见侧栏展开/收起；
使用一次性 Chrome profile 验证关闭/重开后的登录恢复及原生 200% 缩放，
不读取用户 profile，不用 CSS zoom 替代浏览器缩放；
真实宿主模式覆盖桌面深色、手机纯白。自动检查：

- **登录前主题**：继承实际父页面设计 token，深色模式没有白色未认证首帧，
  嵌入模式隐藏重复主题控件；通过宿主 UI 切换主题后跟随更新。
- **宿主遮挡**：读取官方浮动工具栏、iframe、标签行、内容滚动视口和弹窗的实际矩形。
  普通页面不再为整个 iframe 预留顶部安全区；标签行位于 iframe 顶部（窄屏下移），
  认证内容的 `#content` 单独滚动且其视口顶部位于宿主操作安全线之下，滚动前后
  都不会让轮询、刷新或表单控件进入工具栏区域。弹窗保留安全顶部并位于工具栏
  下方，底部操作区不越出 iframe。表格不拉宽文档。
- **登录控件**：原生“记住登录”复选框与文字在 100% 和原生 200% 缩放下垂直
  居中；滚动条采用 CPA v1.26.5 的 8px 轨道/滑块 token。
- **对比度**：截图时检查可见文字（普通文字至少 4.5:1，大字至少 3:1）
  与共享控件边界（至少 3:1），合成透明祖先背景；禁用项按规范排除。
  这不是完整 WCAG 审计，不覆盖图片或原生控件内部绘制。
  原生 200% 缩放截图按物理视口捕获并校验 PNG 尺寸，防止默认 CSS 尺寸截图裁切。
- **日志与注入**：详情按需加载；请求头脱敏、JSON 格式化；恶意模型名、请求头、
  JSON 内容必须保留为文字，不创建可执行 DOM。
- **Key**：自定义创建/轮换、可逆启停、确认终态删除、严格字符校验、最小美元额度、受限模型必选、
  不变的内部 ID、原始纳秒有效期；明文在列表反复展示/复制，关闭弹窗和重载后仍从
  管理接口读取，轮换替换、禁用保留、删除清除，不进入浏览器持久存储。
  长名称实际省略，名称按钮、文本和状态不越出名称列或遮挡密钥列。
- **设置**：通用设置和价格草稿分别保存；刷新失败保留缓存并明确告知失败。
- **轮询/认证**：仅请求页定时刷新，至少 12 个真实 5 秒周期检查行节点、几何、
  焦点与表内滚动；Key 页编辑/空闲都没有定时读。明确记住的登录跨 document reload
  恢复，刻意注入一次 401 后清空两类存储并停止请求。
  组件/网络单测另覆盖可见性、取消、迟到响应、暂停和失败退避。

`report.json` 包含结果、外壳 SHA-256、API 请求、被阻止请求、截图列表与错误；
`summary.txt` 提供总计，`screenshots/` 保留完整 CPA 外壳和 iframe 弹窗截图。
真实宿主还生成 `realhost-smoke.png`。`CHROME_BIN` 或 `--chrome` 可指定 Chrome。
报告目录应位于仓库之外，缺省是工作树的同级 `browser-report`。

所有非预期 HTTP、console、page 和 CSP 错误均使验收失败。唯一负向诊断分类是
会话过期测试**刻意注入的单个 `GET /keys` 401**及对应的精确资源 console 消息；
它们保留在 `expected_diagnostics`。不使用通配规则忽略真实认证失败或 CSP 错误。
管理/客户端密钥从文本报告中脱敏；截图仅用于假数据测试。

## 资源与 mock 合约

资源从 `internal/web/dist/{ui.html,ui.js,ui.css,theme.js}` 读取，与 Go 嵌入资源一致；
Vite 构建结束时将入口 `index.html` 重命名为 `ui.html`。
公开固定路由为资源/管理前缀下的 `/ui`、`/ui/`、`/ui.js`、`/ui.css`、`/theme.js`，
携带相同 CSP、内容类型和 `no-store`。管理数据必须通过 Bearer 管理认证。
不存在单独的 `settings.js` / `models.js` 或任意文件目录服务。

当前管理 API 形状以根目录 README 和 `docs/reconstruction.md` 为准；
`DESIGN.md`、`docs/REFACTOR-v0.7.0.md` 保留原基础接口。
所有 Key 与详情操作使用查询 ID，不使用动态 `/keys/{id}`。Mock 调用日志只保留
认证布尔值和能力阶段，不保留 Authorization 内容；假客户端密钥按新策略保存
当前明文及认证哈希，控制状态接口不暴露二者。
控制端点只在回环测试服务器存在，用于目录延迟/失败、调用计数与状态检查。
