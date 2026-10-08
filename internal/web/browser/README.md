# Observer 面板浏览器验证

本目录包含 `internal/web` 面板的浏览器验证夹具与脚本。它们只用于本地验证，
不参与 `go:embed`，也不会被插件打包进二进制。

- `mock-server.cjs`：同源 mock 宿主，同时提供
  - 公开资源路由 `/v0/resource/plugins/cliproxyapi-observer/{ui,ui.js,ui.css}`（带 CSP）；
  - 管理 API `/v0/management/plugins/cliproxyapi-observer/{summary,requests,settings,health,body}`
    （要求 `Authorization: Bearer observer-test-secret`）；
  - 模拟 management-center 的嵌入宿主页 `/embed`，用于验证父级 `data-theme` 跟随。
- `capture.cjs`：Playwright 验证脚本，输出截图与 `results.json`。
- `settings.cjs`：真实 Chromium 设置保存、价格增删、刷新保留草稿、错误/超时/过期连接、
  503 重配置确认与宿主浮层命中测试，输出截图与 `settings-results.json`。
- `mock-server.test.cjs`：Node mock 合约回归。mock CSP 直接读取
  `internal/plugin/management.go` 的 `cspPolicy`，不会使用另一套策略。

## 运行

Playwright 只应安装在仓库之外的专用临时目录：

```bash
tmp=$(mktemp -d)
cd "$tmp"
npm init -y >/dev/null
PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm install playwright@1.63.0

cd -
NODE_PATH="$tmp/node_modules" \
CHROMIUM_PATH=/usr/bin/chromium \
node internal/web/browser/capture.cjs "$tmp/evidence"
NODE_PATH="$tmp/node_modules" CHROMIUM_PATH=/usr/bin/chromium \
node internal/web/browser/settings.cjs "$tmp/settings-evidence"
node --test internal/web/ui.test.cjs internal/web/browser/mock-server.test.cjs
```

截图与 `results.json` 写入指定的输出目录（仓库之外）。脚本在任一项失败时以非零码退出。

## 覆盖范围

| 场景 | 断言 |
| --- | --- |
| 嵌入宿主浅色/纯白/深色 | iframe 内 `data-theme` 跟随父级，`--bg-secondary` 与 CPA 主题一致 |
| 实时主题切换 | 父级切换 `data-theme` 后 iframe 立即同步（MutationObserver） |
| 390px 窄屏 | 无横向溢出，表格内部滚动，次要列隐藏 |
| 独立打开 | 无父级时回退系统 `prefers-color-scheme` |
| XSS | 恶意模型名/请求体只作为文本存在，不产生 `img`/`script` 元素 |
| 请求体 | 列表刷新不触发读取；需二次确认；关闭清空；内容原样渲染 |
| 分页 | `加载更多` 使用 `cursor` 追加，取尽后隐藏 |
| 过期响应 | 快速切换筛选时旧响应被丢弃，不覆盖新结果 |
| 安全 | 无 localStorage/sessionStorage/cookie；密钥不进 URL，仅经 `Authorization` 头 |
| 捕获关闭 | 设置区解释 `capture-bodies: false`，列表无查看按钮 |

## 说明

- 管理密钥与全部数据均为假值；服务只监听 `127.0.0.1`，不会访问任何真实 CPA 或上游。
- CSP 的 `frame-ancestors 'self'` 由宿主响应头提供（`<meta>` 中该指令会被浏览器忽略），
  因此 `ui.html` 的元策略只声明 `default-src/script-src/style-src/img-src/connect-src/base-uri/form-action`。

## 设置保存合约

- 仅发送可编辑白名单原始 JSON：先 POST 插件 `/validate`，再 PATCH 插件 `/config`；
  不获取或编辑 YAML，不获取 CPA 全量 `/config`，不覆盖 `db`、`flush`、`store` 等元数据。
- 核心配置仅使用 `/v0/management/plugins/cliproxyapi-observer/config`，保留同源反代前缀；
  v8 资源入口也使用当前宿主实际提供的 v0 插件管理路由。
- Save 前确认敏感捕获 / 不可逆缩短保留；成功必须 GET 配置回读，并轮询生效设置。
  暂时 503 可重试，最多 20 次间隔 500ms，总保存/重新加载上限 15s。
  写入结果未知、回读失败、仍未生效均保留草稿，明确提示核对，不宣称成功。
- 普通仪表盘刷新不覆盖脏草稿；重新连接会清除旧连接草稿并中止旧保存。
  所有请求拒绝重定向，凭据只在内存 / Authorization 头中使用。
- `prices: {}` 替换整个价格表；价格快照不影响历史请求。容量表单单位为 MiB，
  转换结果必须为精确整数 bytes；不隐式裁剪越界数值。

mock 控制可直接使用 `startServer()` 返回的 `controls` / `counters`，或本地
`POST /__mock/control` 与 `GET /__mock/counters`。支持校验失败、写入失败、配置回读失败、
生效永不更新，以及 settings GET 暂时 503（`reconfigureTemporary503Count`）。
mock 配置在同一服务进程中跨页面刷新持久保留；它并不证明真实宿主的文件写入或重启持久性。
