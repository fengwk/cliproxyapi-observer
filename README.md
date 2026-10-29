# Observer v1.0.4

认证采用最小侵入的非独占共存：CPA 凭据保留原生行为，Observer 仅管理自己支持的
调用，不是全路由安全网关。范围见 [认证与模型权限](docs/model-policy.md)。

CLIProxyAPI（CPA）独立原生插件，稳定 ID `cliproxyapi-observer`。提供**专属客户端 Key
认证、美元额度、可解释用量成本记账与请求日志**，不修改 CPA 核心、请求正文/头、上游选择、
模型路由、重试或会话。最低 CPA **v8.0.30 / 插件 schema 6**，SDK 锁定 v8.0.30；
更高 v8 稳定版由 CI 验证，不承诺未验证版本或 v9 兼容。

**当前使用 schema3 和 `observer_features=3`，保存客户端密钥明文。**
v1.0.4 沿用 v1.0.0 的数据库结构，**从 v1.0.0–v1.0.3 升级无需重建数据库**。
默认库仍为 `data/cliproxyapi-observer-v2.db`；小时汇总与内容索引继续保留。
不提供 schema2 及更早版本的旧价格、旧汇总或旧配置的迁移/双读路径；打开这些旧库
会明确报不兼容且不会就地改写。仅从这些旧版本升级时，需停流备份后使用新库重建 Key 与价格配置。
旧库只存哈希的密钥无法还原明文，重新生成后才可在列表查看；不会拿内部 ID 冒充。
升级前请备份，安排停流维护；发布不会自动修改生产配置或删除旧数据库。
当前能力、API、SDK 限制和安全升级/回退步骤见 [重构说明](docs/reconstruction.md)。
删除记账异常封锁与人工恢复机制见 [v1.0.4 发布说明](docs/release-notes-v1.0.4.md)；
请求日志筛选与离开确认修复见 [v1.0.3 发布说明](docs/release-notes-v1.0.3.md)；
Gemini 目录价格与诊断修复见 [v1.0.2 发布说明](docs/release-notes-v1.0.2.md)；
紧凑价格布局、真实账户名称与显示修复见 [v1.0.1 发布说明](docs/release-notes-v1.0.1.md)；
v1.0.0 定价、账本与 schema 不兼容变更见 [v1.0.0 发布说明](docs/release-notes-v1.0.0.md)；
此前搜索焦点修复见 [v0.9.3 发布说明](docs/release-notes-v0.9.3.md)；
请求调试、显示与自定义 provider 用量修复见 [v0.9.2 发布说明](docs/release-notes-v0.9.2.md)；
Key 名称样式修正见 [v0.9.1 发布说明](docs/release-notes-v0.9.1.md)；
明文 Key 升级与认证边界见 [v0.9.0 发布说明](docs/release-notes-v0.9.0.md)。

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
“记住登录”默认勾选，成功后将管理凭据保存在 **Observer 自己的 localStorage**；
取消后仅保存在标签页 sessionStorage/内存。退出或确定的结构化 401 清除两处副本，
普通网络错误不清除。存储不可用时明确提示。它不是客户端 Key，不应放到 URL，
也不会读取 CPA 保存的凭据。共享设备请取消记住并在使用后退出。
页面只有三个：

- **请求日志**：平铺筛选草稿、按浏览器本地日历且包含结束日的日期范围、六项概览、
  趋势、宽表，以及模型/Key/Key × 模型/账户/Provider 分组。只有点击查询/Enter 才应用
  筛选；默认浏览器本地当日，筛选栏显示实际 IANA 时区。
  **仅本页自动刷新**（关闭/5/10/30 秒），不重建现有行；编辑、阅读详情、
  文本选择、隐藏、非首页分页或历史范围暂停，失败退避且保留旧数据。
- **Key 管理**：随机/自定义密钥、可逆启停、到期、并发、额度、模型限制、轮换、
  周期重置和不可逆删除。内部 ID 可查历史，不是可用于推理的密钥。
- **设置**：三种内容采集模式、独立保留期、可解释价格/显式计费映射、手动定价，
  以及过期清理/清空内容/物理压缩；退出 Observer 放在设置页底部。Key 和设置页没有周期轮询。

列表、详情、趋势、Key 生命周期与设置中的时间均按浏览器当前时区显示，存储仍为 UTC。
“今天”和近 7/30 天使用本地日历，夏令时日可以是 23/25 小时；重复小时以时差区分。
Key 到期输入也按本地时区解释，未改动时保留原始精度；不存在或有歧义的新输入会明确报错。
定价规则自身的 IANA 时区、UTC 日历日期条件属于计费语义，不随查看者改变；日期条件会
额外预览对应的本地生效时刻。

账户分组和账户筛选通过 CPA 的只读 `host.auth.list` 元数据显示账户名称
（`name` → `label` → 原 `AuthIndex`）。长名称单行省略，悬浮可查看完整名称与 index；
名称只用于显示/搜索，同名账户仍按原 index 独立分组、筛选，不写入历史账本。
CPA v8.0.30 不会在该接口暴露所有配置型 API Key 账户；宿主未提供元数据或账户已删除时
保留 index，不读取宿主配置或凭据 JSON 补齐名称。

创建 Key 时可以随机生成，也可以输入自己的密钥；自定义密钥必须为 **8–256 个
可见 ASCII 字符**，不能含空格或换行，不会自动去除首尾字符。建议至少 32 字符。
重复密钥返回 HTTP 409，失败的轮换不影响旧密钥。**密钥明文保存于本机数据库，**
在管理列表“密钥”列和详情中可反复查看、复制，关闭成功弹窗、刷新或重启后仍可查看。
内部 ID 只用于操作和账本关联。认证仍用 SHA-256 哈希索引；客户端密钥不进入浏览器
持久存储、URL 或日志。拥有管理权限或数据库/备份读取权限的人可以获得客户端密钥。
随机密钥采用 `sk-` + 48 位无偏字母数字；内部 ID 仍为 64 位十六进制，旧密钥仍有效。
UI 的“禁用”可重新启用；`DELETE /keys` 仍是撤销，销毁哈希，之后必须轮换。
新的 `POST /keys/delete` 写入终态墓碑，不能启用/轮换，保留原 ID、历史费用与额度；
活跃请求未结束时返回 busy，不强删。

**认证非独占共存**：CPA 原生 Key、CPA 签发的 Realtime 临时令牌继续由宿主管理，
不自动导入 Observer，也不进入 Observer 账本。Observer Key 仅用于明确支持的生成、
计数和模型目录接口；自有生成保留精确模型授权、额度、并发和记账。
目录/count 不占额度、并发槽或内容采集；自有 count 仍检查 Key 的模型授权。
图片、音频、视频、Alpha Search、Live/Realtime 和 WebSocket 等未纳管入口
保留 CPA 原逻辑，但不会因此向 Observer Key 开放；“全部模型”也不等于“全部接口”。
详见 [认证与模型权限](docs/model-policy.md)。
**不要把同一明文同时设为 CPA 原生 Key 与 Observer Key**：原生认证先胜出时，
Observer 的禁用、删除、模型限制和额度不能撤销那份独立原生凭据。
SDK 在完全没有原生 Key 的匿名配置中，
注册认证 provider 会改变匿名行为（可能从 200 变为 401）；本版不承诺透明匿名模式，
也不通过一律认证成功掩盖该限制。混合凭据来源亦由宿主 provider 顺序决定。
卸载/停用插件后不能期待 Observer 继续保护请求；管理密钥也不用于推理。

模型限制默认**全部模型**；勾选后仅允许精确的请求模型 ID。选择器可搜索、多选，保留
暂不可用的已选 ID，也允许手填完整 ID。可用模型通过管理认证
`POST /models/token` 取得一次性短期能力，再以请求头调用同源只读 `GET /v1/models`；
能力不能用于推理，不入 URL 或存储。目录失败不会把受限 Key 改成无限制。

## 额度与记账

额度为 **5 小时 / 周 / 月 / 总额**，`0` 表示不限额，并发 `0` 表示不限；
空模型列表表示全部。周期从 Key 创建开始，过期后在准入时从当前时间重新开始，
周为 7×24 小时，月为 UTC 日历月（月底夹紧），不是 30 天。总额不自动重置。
可选择重置周期；重置不删除历史账单，轮换不改变累计金额。

金额与单价均为 int64 **micro-USD**：1 USD = 1,000,000 micro-USD，单价按百万 token。
UI 以美元显示。输入 JSON 必须为非负安全整数，最大 9,000,000,000,000,000。
使用检查溢出的整数运算，每次执行各计费桶相加后只向上取整一次，不做浮点账单。
已准入的并发请求可能使额度超出；没有预扣或推测性预留。每个不同执行（包括重试）
独立计费，重放不重复扣款；去重窗口 7 天，过旧且未知事件不入账，不影响后续请求。
周期额度按**入库时间**计入，历史记录日期按请求时间归属。
count-tokens 不占额度或并发槽，但仍检查启用、有效期与模型权限。

### 定价

价格来源固定为 **models.dev v2（默认）或 v1**，用户在设置中选择；两套外部协议
分别由独立适配器读取，内部只保留一套完整价格定义，永不自动回退或混合来源。
目录使用按来源隔离的磁盘/内存快照与 single-flight 刷新；失败保留该来源上次成功
快照及真实获取时间，来源切换后的迟到响应直接丢弃。推理请求不联网查价。
匹配保留完整公开模型身份与可信 Provider；歧义提供候选而不猜测。
**任何缺价都是真实零价**，不阻断推理，也不产生“未计价”状态或补价流程。

人工价按完整 `(model, provider)` 身份覆盖；空 Provider 只匹配实际空 Provider，
不存在模型级通配。价格定义为 `{base, overrides, multipliers}`：基础费率采用
int64 micro-USD/百万 token，`reasoning=null` 表示继承输出，显式 0 表示独立免费；
`cache_write_by_ttl` 以正整数秒精确覆盖缓存写入价，TTL 未知时使用通用写价。
覆盖规则按数组顺序应用所有匹配项，未提及字段继承、显式 0 可覆盖；
倍率为十进制字符串并作用到指定价格字段，全程精确有理运算，最终请求金额只向上
取整一次。条件支持完整上下文的 `gt/gte/lt/lte`、最终 `serviceTier`、UTC 日期、
IANA 时区/星期/跨午夜时段以及 `all/any/not`；缺少完成计算所需的用量或条件事实时
本次费用为 0，非法配置、溢出和存储故障仍报错。外部不支持的 token 计价条件拒绝
导入并显示支持问题，不以部分价格冒充完整定义。非 token 费用只列入支持问题。
reasoning 属于输出子集，同输出时不重复计费，独立定价时按子集拆账。
自动价解析支持经核实的 `commandcode/`、`opencode-go/` 外层命名空间，
不任意删除斜杠、版本、大小写或 `:free`。设置页可保存 `pricing.mappings`
的显式目录来源，**只改变未来计费，不改变 CPA 路由**。
预览与真实报价使用同一解析器和完整定义；详情保留入库时的费率、定义、
目录身份与匹配诊断。已知 provider 沿用固定 SDK 的 token 语义；
未知语义不按 provider 名称猜测：仅当原始插件计数全部非负、显式 total 为正且至少一个
输入/输出为正时，才用固定 SDK 构造器尝试 subset / independent / separate-reasoning。
只有唯一的完整结果（或多个完全相同的完整结果）与显式 total 守恒才恢复细分，否则保留
unclassified 与原始计数。`CachedTokens` 是无读/写拆分的旧聚合，不覆盖显式缓存桶；
无法可靠解释时仍为未知。历史记录不重算、不迁移，缺失数据继续显示 `—`。
Observer 只能使用 CPA 已上报的用量计数，不读取响应原文；若缓存字段在上游或协议转换时
已丢失，不能由总 Token 数反推出真实缓存命中数。
**成本、单价与来源在入库时固定**；以后改价/刷新不会重算历史。

## 数据、隐私与故障边界

内容采集默认 `off`，可选 `headers`（请求头副本）或 `headers_and_body`（加 JSON 正文），
**不保存响应内容，也不采集上游发送的请求头或上游凭据**。采集的是认证前的客户端
入站请求头副本：除 Authorization/Proxy-Authorization、Cookie/Set-Cookie、API Key、
Token、Secret/Password/Credential/Private Key 及认证相关头外，其余头名称和多值均保留，
字段名大小写不敏感；敏感头整体省略，不复制、不落盘。头名称与保留值累计受 **16KiB**
上限约束，超限丢弃整组请求头并标记为超限；仅被排除的敏感头不占预算。
正文递归脱敏敏感 JSON 字段和 URL 查询凭据，普通 `max_tokens` 等计数保留。
内容单层 gzip 压缩、有界非阻塞队列；新 schema 不读取旧库双层压缩格式。
独立内容保留期精确到秒，范围 60 秒至原始明细保留上限；关闭/缩小模式限制历史读取，
但不等于删除。延长 TTL 不恢复已过期内容，排队任务亦不能恢复已清空内容。
默认原始明细 **24h**、
历史汇总 **365 天**；读取立即执行过期检查。单条正文默认 1MiB、最多 **8MiB**，
正文存储默认 256MiB、最多 **1GiB**。配额不等于整个数据库硬容量上限。
到达总内容预算时按采集时间 FIFO 淘汰最旧内容，事务维护压缩字节数和到期索引，
不在每次写入时解压全部正文。头/体独立声明可用、未开启、超限、不支持、过期、
淘汰或已清空等状态。采集丢弃按原因分组，并声明本进程计数起点；
诊断内容丢弃与用量入账相互独立，不能从诊断丢弃数量推断账单是否缺失。

字段名脱敏不能识别普通 prompt/代码中的所有秘密，请保护整个数据目录、备份与磁盘。
清理不等于安全擦除；物理压缩可回收文件空间，需要同目录临时副本和额外磁盘空间。
过期清理只删已到期内容；清空全部内容需明确确认，二者均不改变费用/额度。
挂载整个目录而不是单个数据库文件。物理压缩仅在无活跃请求且内容队列空闲时执行，
忙时返回 busy；短暂维护期间新受管生成可能收到可重试 503，
CPA 原生流量仍按宿主行为处理，Observer 自有生成不会借此绕过准入检查。
复制/替换前失败保留原文件，替换后同步失败会如实报告，不能承诺在线零停顿。
原始明细过期不清空 Key 独立累计额度。UTC 分钟汇总与历史窗口一同保留
（原始窗口更长时至少保留至原始窗口），使历史仍可按浏览器本地整日精确查询；
不延长原始明细或正文保留期。更细的汇总会增加数据库空间，不受正文预算限制。
历史趋势按本地日合并，响应声明实际粒度，不伪造已过期的请求明细。
仍存在的旧明细启动时分批、可续接地补全细分汇总，已过期明细无法重建。
界面声明详细/完整样本覆盖数，不把未知 Token、TTFT、缓存命中率填成已知 0；
reasoning 是输出子集，不再次相加；TPS 只显示有效流式单条样本，不杜撰聚合 TPS。
缓存命中率仅使用完整样本，分母为未缓存输入+缓存读+缓存写，统一显示两位小数。

**可用性优先：漏记或记账失败不封锁 Key，也不要求人工恢复。**
用量写入仍是原子事务；真正的报价错误、溢出或写失败会报告错误，失败事件不保留重试，
不补造费用、不修改已有账单。缺价仍按 $0 正常记录。认证、启停、到期、模型权限、
额度和并发限制照常执行；数据库确实不可访问时，不能假装已经完成这些检查。

CPA 宿主用量队列**不持久化且没有可靠的热替换前排空保证**。崩溃或容器重启可能漏掉
尚未落盘的记录；漏了就保留缺失，重启后已有 Key 可直接继续使用。旧 schema3 数据库
中的记账封锁字段及库旁的旧中断文件不再被读取或用于限制请求，无需逐个点击恢复，
也无需重建数据库；旧文件不会被程序自动删除。已保存的金额、历史与配置不变。
安排停流备份可以减少升级时的记录丢失，但不会因未完成记账而产生持久封锁。

## 管理 API

所有数据接口均在 `/v0/management/plugins/cliproxyapi-observer` 下，要求宿主管理认证，
直接返回 JSON；错误为 `{"error":{"code":"...","message":"..."}}`，无 CORS，
`Cache-Control: no-store`。仅使用以下静态路由，ID 用查询参数：

| 方法 | 路径 |
| --- | --- |
| GET | `/health`、`/keys`、`/requests`、`/requests/view`、`/requests/facets`、`/summary`、`/settings`、`/prices` |
| POST | `/models/token`、`/keys`、`/prices/preview`、`/prices/refresh`、`/compact` |
| PUT / DELETE | `/keys?id=<id>`（编辑 / 撤销） |
| POST | `/keys/rotate?id=<id>`、`/keys/reset?id=<id>` |
| POST | `/keys/delete?id=<id>`、`/captures/cleanup`、`/captures/purge` |
| GET | `/requests/detail?id=<id>` |
| PUT | `/settings` |

`reset` 输入 `{"windows":["five_hour","week","month","total"]}`（选择所需周期）。
旧 `/requests`、`/summary` 查询保留 RFC3339 `from/to`、UTC `[from,to)`；
新 `/requests/view` 使用 `from_date/to_date=YYYY-MM-DD`（包含结束日）与
`timezone=<IANA 名称>`，浏览器总是携带当前时区；直接 API 调用省略时区时默认 UTC。
服务端按当地日界换算 UTC 瞬间，禁止混用旧范围参数。
`view=requests|model|key|key_model|account|provider`，
一次一致 DB 读取返回范围/as_of、概览、趋势和选定明细或分组及覆盖情况；
as_of 不是下一次查询可复用的快照令牌。过滤参数
`key_id/model/provider/auth_index/status`，分页 `page`（默认1）、
`page_size`（默认25，最多100）。旧 `/requests` 返回 `items/page/page_size/has_more`；
新 `/requests/view` 返回 `records` 或 `groups`，分页位于 `pagination` 对象。
未知/畸形参数、未知字段、尾随 JSON 和不支持的方法拒绝；输入 JSON 上限1MiB。
`/requests/facets?field=model|key_id|provider|auth_index&search=&cursor=&limit=`
读取保留范围内历史维度，limit 最多 100。`GET /keys?include_deleted=true` 查询墓碑；
清理接口为空 body，清空接口必须为 `{"confirm":true}`。
新增结构与兼容设置语义见 [重构说明](docs/reconstruction.md)；
[DESIGN.md](DESIGN.md) 与 [v0.7 增补](docs/REFACTOR-v0.7.0.md) 保留基础契约。
创建 Key 的平铺 KeySpec 可选附加 `secret`；轮换允许空 body（随机生成）或
`{"secret":"..."}`，显式空字符串/`null` 被拒绝，编辑接口不接受密钥。
价格预览接受 `{"models":[{"model":"exact-id","provider":""}]}`（最多 4096 项），
返回基础费率、完整 `definition`、来源/候选与支持问题，不按虚构用量求值条件，
也不是对实际请求的预测账单。`GET /prices` 额外返回当前选中的 `source`。

## 开发验证与交付

```bash
npm ci --prefix web             # 开发/测试工具；安装插件时无需执行
npm audit --prefix web          # 包括前端构建工具
make frontend                   # 从源码更新嵌入资源
make test                       # Go vet/unit + 前端类型/行为测试 + mock API
make race                       # 全量 Go race
make load                       # 1万/10万 gzip 元数据及接近内容预算负载验收
CHROME_BIN=/usr/bin/google-chrome-stable make browser
# 可指定 BROWSER_REPORT_DIR 保存 report.json / summary.txt / screenshots
bash scripts/build-cpa-host.sh v8.0.30 /absolute/temp/cli-proxy-api
CPA_BINARY=/absolute/path/to/cli-proxy-api make integration
# 原生宿主浏览器验收；官方 Management Center v1.26.5 shell 需从固定地址下载并核对
# SHA-256 57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700
OBSERVER_BROWSER=1 CHROME_BIN=/usr/bin/google-chrome-stable \
  CPA_MANAGEMENT_HTML=/absolute/temp/management.html \
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
非空升级测试会从 Git 中固定的 v0.7.0 commit 导出源码并在临时目录构建旧插件，
用真实旧 writer 生成数据后再切换候选；因此原生验收需要包含基线历史的 checkout
（CI 已配置 `fetch-depth:0`），不下载未核实二进制或接触生产数据。
浏览器入口见 [现有说明](internal/web/browser/README.md)，使用本地假数据和实际 Chrome，
CI 上传报告及截图。`make build` 验证本机 c-shared；发布矩阵仍构建三个平台。
版本通过 `internal/plugin.Version` ldflags 注入，正式版本来自 tag，开发构建为
`1.0.4-dev+<commit>`，不取 SDK 版本。

现有依赖维护策略保留：仅白名单 Go/官方 Actions 纯版本升级可经完整 CI 后自动合并，
普通功能合并不会触发自动发版；不可变发布器校验源码、三个平台归档与校验和，
不覆盖公开 Release。发布前必须完成最终合并审查与全量验收并获得发布授权；
正式 GitHub Release 包含本版不兼容变更说明。

MIT；SDK、bbolt 与内嵌前端依赖的版权和许可证见 [NOTICE](NOTICE)。
独立实现，非 cpa-plugin-tokens-statistic 的 fork。
