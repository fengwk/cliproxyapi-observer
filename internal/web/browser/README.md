# Observer 面板浏览器验证

本目录包含 `internal/web` 面板的浏览器验证夹具与脚本。它们只用于本地验证，
不参与 `go:embed`，也不会被插件打包进二进制。

- `mock-server.cjs`：同源 mock 宿主，同时提供
  - 公开资源路由 `/v0/resource/plugins/cliproxyapi-observer/{ui,ui.js,ui.css}`（带 CSP）；
  - 管理 API `/v0/management/plugins/cliproxyapi-observer/{summary,requests,settings,health,body}`
    （要求 `Authorization: Bearer observer-test-secret`）；
  - 上游凭据名称 `GET /v8/management/credentials`（返回假 `files[].auth_index/label/name`，默认
    `fake-file-a.json`/`fake-file-b.json`，可改写 `controls.credentialFiles` 验证「同标签不同文件可区分、
    名称缺失回落索引、恶意文件名仅作文本」）；UI 只保留非机密展示字段，按 `label（文件名）` 展示并以 `auth_index` 精确筛选。
  - 模拟 management-center 的嵌入宿主页 `/embed`，用于验证父级 `data-theme` 跟随。
  - summary 真实按 `client_key_id` / `auth_index` / `provider` / `model` 过滤，并返回
    `client_keys` / `credentials` 有序身份数组（未归属桶 id 为空字符串）；定价按
    `prices` map 与有序 `price-rules`（first-match、阈值严格大于、UTC 区间）真实计算。
- `capture.cjs`：Playwright 验证脚本，输出截图与 `results.json`。
- `settings.cjs`：真实 Chromium 设置保存、价格增删、刷新保留草稿、错误/超时/过期连接、
  503 重配置确认与宿主浮层命中测试，输出截图与 `settings-results.json`。
- `keys.cjs`：真实 Chromium 的客户端指纹 / 上游凭据筛选、概览与分组联动、分组维度切换
  与整行点击过滤，并校验统一筛选工具栏（单一卡片、桌面一行、窄屏自然换行、无表单序列化），
  输出三主题筛选区近景截图与 `key-results.json`。
- `price-rules.cjs`：真实 Chromium 的有序条件定价场景：阈值严格大于、first-match、
  重排持久化、UTC/跨夜/24:00 边界、删除/清空、后端拒绝保留草稿，并输出
  light/white/dark × 1280/390 截图与 `price-rules-results.json`。
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
NODE_PATH="$tmp/node_modules" CHROMIUM_PATH=/usr/bin/chromium \
node internal/web/browser/keys.cjs "$tmp/keys-evidence"
NODE_PATH="$tmp/node_modules" CHROMIUM_PATH=/usr/bin/chromium \
node internal/web/browser/price-rules.cjs "$tmp/price-rules-evidence"
node --test internal/web/ui.test.cjs internal/web/browser/mock-server.test.cjs
```

截图与 `results.json` 写入指定的输出目录（仓库之外）。脚本在任一项失败时以非零码退出。

## 分页设计与权衡

- **offset / limit 语义**：请求列表采用 `offset`（`0..2147483647`）与 `limit`（默认 50，后端上限 100，UI 固定每页请求 50）切页，返回 `{ items, offset, limit, has_more }`，废弃所有游标 `cursor` 字段。
- **权衡考量**：
  - **优势**：不为总页数额外全库计数；翻页采用单页替换（固定最多 50 行），避免无限列表 DOM/内存累加。
  - **开销与偏移漂移**：深页需跳过前面的匹配记录，无过滤时开销随 `offset + limit` 增长，有过滤时可能扫描更多区间记录。翻页固定 from/to，不查询总条数或总页数；保留清理或迟到写入仍可能造成偏移、重复/遗漏。因此刷新、筛选或重连会重置查询窗口与第 1 页（`offset=0`）。
- **动态查询定价**：历史请求明细与 summary 总成本均在查询时按当前生效价格（`effectiveSettings.prices`）动态计算；保存配置更新价格后立即反映于历史明细，无需快照。

## 弹窗与表单合约

- **异步确认弹窗（#confirmation-dialog）**：
  - 彻底杜绝浏览器原生 `window.confirm` / `alert` 弹窗（Playwright `page.on('dialog')` 守卫直接断言失败）。
  - 保存操作在调用 `editor.read()` 前必经 `#confirmation-dialog` 二次确认；丢弃草稿（`#settings-revert`）仅在脏状态下弹窗确认，干净状态直接重载。
  - 具备可访问性属性（`aria-labelledby="confirmation-title"` / `aria-describedby="confirmation-desc"`）；默认初始焦点在 `#confirmation-cancel`；`Escape` 键或遮罩点击取消；关闭后焦点恢复至触发按钮。
  - 取消确认绝不发起 `/validate` 或 `/config` 写请求（0 计数），保留草稿；连接断开、鉴权失败或重连立即失效并关闭弹窗，陈旧确认绝不作用于新连接。
  - 同源长 iframe 内仅读取框架与宿主视口几何尺寸，将弹窗置于可见区域；不读取宿主内容或凭据。跨源宿主回退浏览器默认居中。
- **无障碍定制复选框**：
  - `#setting-capture-bodies` 为原生 `<input type="checkbox">`，采用 `appearance: none` 定制三套主题（`light`、`white`、`dark`）及高对比度样式。
  - 支持 `<label>` 区域点击与获得焦点时 `Space` 键切换；禁用状态（断开连接/保存中）静默忽略交互。

## 覆盖范围

| 场景 | 断言 |
| --- | --- |
| 嵌入宿主浅色/纯白/深色 | iframe 内 `data-theme` 跟随父级，`--bg-secondary` 与 CPA 主题一致 |
| 原生滚动条与控件主题 | 根元素、表格滚动区、弹窗与 select 的 `color-scheme` 为浅色 `light` / 深色 `dark`；覆盖嵌入、独立打开、宽窄屏与实时主题切换 |
| 实时主题切换 | 父级切换 `data-theme` 后 iframe 立即同步（MutationObserver） |
| 390px 窄屏 | 无横向溢出，表格内部滚动，次要列隐藏 |
| 独立打开截图 | 3 主题（light/white/dark）在 1280px 与 390px 下独立渲染与截图输出 |
| XSS | 恶意模型名/请求体只作为文本存在，不产生 `img`/`script` 元素 |
| 请求体安全与生命周期 | 列表刷新不触发读取；需二次确认后懒加载；关闭弹窗清空；重连/403 立即失效并关闭查看器 |
| 分页翻页与替换 | `#requests-prev` / `#requests-next` / `#requests-page` 双向翻页；每页 50 条替换渲染，无游标无累加；空页与末页禁用导航；筛选变更重置第 1 页 |
| 刷新取消在途翻页 | 点击刷新立即中止在途翻页请求并失效令牌，重置为第 1 页并丢弃旧响应 |
| 动态生效定价与小计 | Summary 与历史请求均按当前价格动态映射，小计与未定价分类明确展示 |
| 客户端 key / 上游凭据筛选 | 概览、趋势、分组与请求记录同源过滤；概览数值随身份变化；`offset` 重置为 0 |
| 统一筛选工具栏 | 时间范围 / Provider / 模型 / 客户端 key / 上游凭据 / 应用按钮同处一张 `筛选条件` 卡片与同一控件容器；桌面 1280px 单行底部对齐，768/390px 自然换行无横向溢出；无 `form` 与命名序列化入口 |
| 完整指纹与凭据名称 | 客户端 key 为下拉选择（全部 / 未归属 / 已出现指纹，只显示 12 位前缀，无需手填），内部按完整 64 位指纹过滤，点击请求行或分组身份同样按完整指纹过滤；上游凭据显示 CPA 标签/名称并以非机密 `auth_index` 过滤 |
| Key 分组与维度切换 | `client_keys` / `credentials` 身份数组，未归属 id 为空；点击整行按完整 id 过滤，切换维度立即反映 |
| 有序条件定价 | `price-rules` 顺序、阈值（含缓存、严格大于）、UTC 区间（起含终不含、跨夜、24:00）与 first-match；旧 `prices` map 作为无条件回退 |
| 价格规则编辑 | 上移/下移/删除、响应式整行控制、危险色删除、可访问性标签、重排与删除标记脏草稿 |
| 安全防护 | 无 localStorage/sessionStorage/cookie；管理密钥不进 URL，仅经 `Authorization` 头；严格禁止重定向 |
| 异步确认弹窗完整生命周期 | 取消/Escape 保留草稿且 0 请求；重复点击不重复弹窗；断开/重连使在途弹窗失效；干净草稿不弹窗 |
| 无障碍主题复选框 | `appearance: none`、label 文本点击与 Space 键切换、禁用状态防篡改 |
| 宿主浮层命中与穿透保护 | 1280px / 390px 下宿主 overlay 绝不遮挡密钥、连接、刷新与状态控件 |
| 共享按钮契约 | 所有按钮带 `.btn`；常规 46px/16px/600/10×14、紧凑 `btn-sm` 39px/14px/600/8×10、段控复用 `.btn` 基础（同字号字重 + 拼接圆角）；危险删除按钮 error 语义色；禁用不透明度/光标与键盘焦点轮廓；三主题一致，无页面横向溢出 |

## 说明

- 管理密钥与全部数据均为假值；服务只监听 `127.0.0.1`，不会访问任何真实 CPA 或上游。
- 主题切换会触发按钮/边框 150ms 过渡；截图脚本在切换主题后等待文档与同源 iframe 内动画结束（`settleTransitions`），避免截到中间灰阶/低对比帧。
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
- `prices: {}` 为规范保存的一部分：编辑器始终写入空的旧价格表加有序 `price-rules`
  列表（先显式规则，后由旧 map 行转换的无条件规则），因此旧 `prices` map 向后兼容且
  不会与规则双写。价格在查询时按当前生效设置（first-match 规则，未命中回落 `prices`
  map）动态计算，不保留快照；更新规则后历史请求与 summary 成本立即反映。容量表单单位为
  MiB，转换结果必须为精确整数 bytes；不隐式裁剪越界数值。

mock 控制可直接使用 `startServer()` 返回的 `controls` / `counters`，或本地
`POST /__mock/control` 与 `GET /__mock/counters`。支持校验失败、写入失败、配置回读失败、
生效永不更新，以及 settings GET 暂时 503（`reconfigureTemporary503Count`）。
mock 配置在同一服务进程中跨页面刷新持久保留；它并不证明真实宿主的文件写入或重启持久性。
