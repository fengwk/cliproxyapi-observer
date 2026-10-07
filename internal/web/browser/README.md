# Observer 面板浏览器验证

本目录包含 `internal/web` 面板的浏览器验证夹具与脚本。它们只用于本地验证，
不参与 `go:embed`，也不会被插件打包进二进制。

- `mock-server.cjs`：同源 mock 宿主，同时提供
  - 公开资源路由 `/v0/resource/plugins/cliproxyapi-observer/{ui,ui.js,ui.css}`（带 CSP）；
  - 管理 API `/v0/management/plugins/cliproxyapi-observer/{summary,requests,settings,health,body}`
    （要求 `Authorization: Bearer observer-test-secret`）；
  - 模拟 management-center 的嵌入宿主页 `/embed`，用于验证父级 `data-theme` 跟随。
- `capture.cjs`：Playwright 验证脚本，输出截图与 `results.json`。

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
