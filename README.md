# Observer v0.7.0

CLIProxyAPI（CPA）独立原生插件，稳定 ID `cliproxyapi-observer`。提供**专属客户端 Key
认证、美元额度、固定成本记账与请求日志**，不修改 CPA 核心、请求正文/头、上游选择、
模型路由、重试或会话。最低 CPA **v8.0.30 / 插件 schema 6**，SDK 锁定 v8.0.30；
更高 v8 稳定版由 CI 验证，不承诺未验证版本或 v9 兼容。

**v0.7.0 保留 v0.6.0 的数据库、Key 和历史费用，无需迁移或重新发放 Key。**
默认库仍为 `data/cliproxyapi-observer-v2.db`。从 v0.6.0 之前的旧实现升级时，
仍须使用新数据库与新 Key；非空且无 schema2 标记的旧库会被拒绝，不会就地改写。
升级前请备份，安排停流维护；发布不会自动修改生产配置或删除旧数据库。
参见 [本版发布说明](docs/release-notes-v0.7.0.md)、
[基础设计契约](DESIGN.md) 与 [v0.7.0 增补契约](docs/REFACTOR-v0.7.0.md)。

## 安装与首次启动

将 [config.example.yaml](config.example.yaml) 的片段**合入**现有 CPA 配置，不要覆盖它：

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  store-sources:
    - "https://raw.githubusercontent.com/fengwk/cliproxyapi-observer/main/registry.json"
  configs:
    cliproxyapi-observer:
      enabled: true
      db: "data/cliproxyapi-observer-v2.db"
```

在 CPA 插件商店刷新、搜索 **Observer** 并安装。源不固定版本，安装器读取正式 GitHub
Release 并校验归档；正式平台为 **Linux amd64、Linux arm64、macOS arm64**。
Linux 产物不适用于 musl/Alpine。手动构建需 Go 1.26+、C 编译器和 Make：
`make build`，动态库位于 `dist/<goos>/<goarch>/`，复制至
`<plugins.dir>/<goos>/<goarch>/`。它是 c-shared 原生库，不是 Go plugin。
React/TypeScript 界面已经编译并嵌入动态库，安装与运行不需要 Node、npm 或 CDN。
仅修改前端源码时，需 Node 22.12+，先 `npm ci --prefix web`，再 `make frontend`；
提交 `internal/web/dist` 的生成资源后再构建动态库。

启动后打开 CPA 管理面板的 Observer，或同源资源地址
`/v0/resource/plugins/cliproxyapi-observer/ui`。用 **CPA 管理密钥**登录一次；
管理凭据仅在当前标签页 sessionStorage 中保存，不是客户端 Key，不应放到 URL。
页面只有三个：

- **请求日志**：筛选、趋势、分页及按需展开的请求详情。默认查询 UTC 当日零点至现在。
- **Key 管理**：随机/自定义密钥、可逆启停、到期、并发、额度、模型限制、轮换与周期重置。
- **设置**：请求内容采集、保留期、可用模型价格匹配、手动定价、健康与物理压缩。

创建 Key 时可以随机生成，也可以输入自己的密钥；自定义密钥必须为 **8–256 个
可见 ASCII 字符**，不能含空格或换行，不会自动去除首尾字符。建议至少 32 字符。
重复密钥返回 HTTP 409，失败的轮换不影响旧密钥。明文仅在成功创建/轮换后显示一次，
关闭即从界面清除；客户端密钥不进入浏览器存储，数据库只存 SHA-256 哈希。
UI 的“禁用”保留密钥，可重新启用；管理 API 的 `DELETE /keys` 是撤销，销毁认证哈希，
保留身份与历史，之后必须轮换才能重新使用。Observer 能力保持注册期间
**独占前端认证**：原有 CPA `api-keys` 不能复用为 Observer Key，存储故障或插件内部
暂停也不回退内置认证。**不要在宿主中停用或卸载插件后，仍期待 Observer 保护请求**：
宿主移除认证能力后可恢复自身认证，未加载的插件无法拦截。管理密钥也不用于推理。

模型限制默认**全部模型**；勾选后仅允许精确的请求模型 ID。选择器可搜索、多选，保留
暂不可用的已选 ID，也允许手填完整 ID。可用模型通过管理认证
`POST /models/token` 取得一次性短期能力，再以请求头调用同源只读 `GET /v1/models`；
能力不能用于推理，不入 URL 或存储。目录失败不会把受限 Key 改成无限制。

## 额度与记账

额度为 **5 小时 / 周 / 月 / 总额**，`0` 表示不限额，并发 `0` 表示不限；
空模型列表表示全部。周期从 Key 创建开始，过期后在准入时从当前时间重新开始，
周为 7×24 小时，月为 UTC 日历月（月底夹紧），不是 30 天。总额不自动重置。
可选择重置周期；重置/轮换不会解除记账故障。

金额与单价均为 int64 **micro-USD**：1 USD = 1,000,000 micro-USD，单价按百万 token。
UI 以美元显示。输入 JSON 必须为非负安全整数，最大 9,000,000,000,000,000。
使用检查溢出的整数运算，每次执行各计费桶相加后只向上取整一次，不做浮点账单。
已准入的并发请求可能使额度超出；没有预扣或推测性预留。每个不同执行（包括重试）
独立计费，重放不重复扣款；去重窗口 7 天，过旧且未知事件会保守阻断其 Key。
周期额度按**入库时间**计入，历史记录日期按请求时间归属。
count-tokens 不占额度或并发槽，但仍检查启用、有效期与模型权限。

### 定价

默认自动从 HTTPS **models.dev** 公共目录读取基础价格，使用有界本地缓存；
网络失败保留最后有效缓存，推理请求不联网查价。匹配保留提供商及完整模型身份，
歧义不猜测，**缺价成本为 0** 且标明来源，不阻断推理。它不是供应商最终账单保证。
设置页在第一次推理前即可预览 CPA 可用模型的基础价格；不会把 `owned_by` 猜成
计价 Provider，也不会虚构 token 数量、写入用量或把预览模型变成历史观测。
公共目录条目数与已使用模型数量分开显示；条件规则是否生效仍取决于实际请求。

设置页支持精确模型手动覆盖与有序条件规则，优先级：
**首条匹配规则 > 手动价 > 自动价 > 缺价零成本**。
上下文阈值 `input_tokens_gt` 严格大于完整输入（未缓存+缓存读+缓存写），
不是仅未缓存输入；同条条件 AND。时间为每日 UTC `HH:mm-HH:mm` 半开区间，支持跨午夜。
reasoning 属于输出子集，不重复计费。未知用量语义保留原始 SDK 计数，不虚构重叠桶。
**成本、单价与来源在入库时固定**；以后改价/刷新不会重算历史，失败重试仍使用原报价。

## 数据、隐私与故障边界

正文采集默认关闭，设置页显式启用后才保存**请求头和请求体，不保存响应内容**。
请求详情/正文 gzip 压缩，有界队列，递归脱敏敏感 JSON 字段及请求头/查询凭据。
默认原始明细 **24h**、
每日汇总 **365 天**；读取立即执行过期检查。单条正文默认 1MiB、最多 **8MiB**，
正文存储默认 256MiB、最多 **1GiB**。配额不等于整个数据库硬容量上限。
采集丢弃计数可见；诊断采集可以丢弃，不能以此策略静默丢弃金额。

字段名脱敏不能识别普通 prompt/代码中的所有秘密，请保护整个数据目录、备份与磁盘。
清理不等于安全擦除；物理压缩可回收文件空间，需要同目录临时副本和额外磁盘空间。
挂载整个目录而不是单个数据库文件。压缩可能返回 busy，请在低负载时操作。
原始明细过期不清空 Key 独立累计额度；历史仅剩日汇总时必须按 UTC 整日查询，
不伪造小时精度；响应声明实际粒度。

记账写失败会保留原报价重试，并阻止该 Key 后续准入，健康状态暴露待处理量。
存储正常且重试结清后，若存在崩溃/丢失事件导致的未知用量阻断，管理员必须
**明确确认未知费用风险**才可恢复该 Key；确认不重建缺失成本，也不修改已有计数、
历史费用或限额。不要用重置/轮换代替恢复。

CPA 宿主用量队列**不持久化且没有可靠的热替换前排空保证**。Observer 可标记已检测的
中断，但无法恢复宿主从未交付的事件，因此不能承诺端到端恰好一次。
避免繁忙时热更新；安排停流维护并检查健康/未结算状态，不要把发布等同于部署。

## 管理 API

所有数据接口均在 `/v0/management/plugins/cliproxyapi-observer` 下，要求宿主管理认证，
直接返回 JSON；错误为 `{"error":{"code":"...","message":"..."}}`，无 CORS，
`Cache-Control: no-store`。仅使用以下静态路由，ID 用查询参数：

| 方法 | 路径 |
| --- | --- |
| GET | `/health`、`/keys`、`/requests`、`/summary`、`/settings`、`/prices` |
| POST | `/models/token`、`/keys`、`/prices/preview`、`/prices/refresh`、`/compact` |
| PUT / DELETE | `/keys?id=<id>`（编辑 / 撤销） |
| POST | `/keys/rotate?id=<id>`、`/keys/reset?id=<id>`、`/keys/recover?id=<id>` |
| GET | `/requests/detail?id=<id>` |
| PUT | `/settings` |

`reset` 输入 `{"windows":["five_hour","week","month","total"]}`（选择所需周期）；
`recover` 输入 `{"acknowledge":true}`，必须先理解上述风险。
查询使用 RFC3339 `from/to`、UTC `[from,to)`，过滤参数
`key_id/model/provider/auth_index/status`，分页 `page`（默认1）、
`page_size`（默认25，最多100），返回 `items/page/page_size/has_more`。
未知/畸形参数、未知字段、尾随 JSON 和不支持的方法拒绝；输入 JSON 上限1MiB。
完整 KeySpec、Settings 与响应结构以 [DESIGN.md](DESIGN.md) 为准。
创建 Key 的平铺 KeySpec 可选附加 `secret`；轮换允许空 body（随机生成）或
`{"secret":"..."}`，显式空字符串/`null` 被拒绝，编辑接口不接受密钥。
价格预览接受 `{"models":[{"model":"exact-id","provider":""}]}`（最多 4096 项），
只返回基础费率、来源和 `has_rules`，不是对实际请求的预测账单。

## 开发验证与交付

```bash
npm ci --prefix web             # 开发/测试工具；安装插件时无需执行
npm audit --prefix web          # 包括前端构建工具
make frontend                   # 从源码更新嵌入资源
make test                       # Go vet/unit + 前端类型/行为测试 + mock API
make race                       # 全量 Go race
CHROME_BIN=/usr/bin/google-chrome-stable make browser
# 可指定 BROWSER_REPORT_DIR 保存 report.json / summary.txt / screenshots
bash scripts/build-cpa-host.sh v8.0.30 /absolute/temp/cli-proxy-api
CPA_BINARY=/absolute/path/to/cli-proxy-api make integration
# 原生宿主浏览器验收
OBSERVER_BROWSER=1 CHROME_BIN=/usr/bin/google-chrome-stable \
  CPA_TEST_ARTIFACTS=/absolute/temp/native-report \
  CPA_BINARY=/absolute/path/to/cli-proxy-api make integration
bash scripts/test-automerge.sh
bash scripts/test-compatibility-report.sh
bash scripts/test-package-plugin.sh
python3 scripts/test_dependency_policy.py
python3 scripts/test_auto_release.py
python3 scripts/test_publish_release.py
```

`integration` 必须提供真实宿主，不会缺省伪通过；CI 基线 v8.0.30 + 最新稳定 v8，
两条宿主检查均启用实际 Chrome。
浏览器入口见 [现有说明](internal/web/browser/README.md)，使用本地假数据和实际 Chrome，
CI 上传报告及截图。`make build` 验证本机 c-shared；发布矩阵仍构建三个平台。
版本通过 `internal/plugin.Version` ldflags 注入，正式版本来自 tag，开发构建为
`0.7.0-dev+<commit>`，不取 SDK 版本。

现有依赖维护策略保留：仅白名单 Go/官方 Actions 纯版本升级可经完整 CI 后自动合并，
普通功能合并不会触发自动发版；不可变发布器校验源码、三个平台归档与校验和，
不覆盖公开 Release。发布前必须完成最终合并审查与全量验收并获得发布授权；
正式 GitHub Release 包含本版不兼容变更说明。

MIT；SDK、bbolt 与内嵌前端依赖的版权和许可证见 [NOTICE](NOTICE)。
独立实现，非 cpa-plugin-tokens-statistic 的 fork。
