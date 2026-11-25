# 认证共存与模型权限

## 最小原则：各管各的

使用 CPA 已有的非独占 `FrontendAuthProvider`，不改 CPA 核心、不另造令牌系统。
CPA 原生凭据保留原生行为；Observer 只管理自己认证且明确支持的调用。
不使用“所有模型执行必须使用 Observer Key”守卫或全路由拦截层；仅对明确支持的
Observer 调用进行认证与准入，Responses WebSocket 也只限精确路径 `GET /v1/responses`。

## 凭据与接口范围

| 凭据 / 入口 | 行为 |
| --- | --- |
| CPA 原生 Key | 宿主原生认证、权限和执行；不自动导入 Observer、不纳入 Observer 账本 |
| CPA 签发的 Realtime `ek_…` | CPA 负责签发、有效期、模型/会话绑定、签发者身份与通话归属；Observer 不复制或读取其令牌存储 |
| Observer Key：POST Chat / Completions / Messages / Responses / Codex Responses | 检查 Key 状态、精确请求模型、额度和并发；支持的执行按 Observer 规则记账 |
| Observer Key：Responses WebSocket（精确路径 `GET /v1/responses`） | 握手按生成操作认证；每轮按现有模型授权、额度和并发准入并记账 |
| Observer Key：Gemini generateContent / streamGenerateContent | 认证时保留真实动作，在通配执行边界按自有操作进行授权、准入和记账 |
| Observer Key：Claude / Gemini countTokens | 检查 Key 状态及精确请求模型，不计费、不占并发、不采集 |
| Observer Key：支持的 GET 模型目录 / 详情 | 只读，不准入、不计账 |
| Observer Key：图片、音频、视频、Responses compact、Interactions、Alpha Search、Live/Realtime 等其他入口 | 不在认证白名单，不因“全部模型”或有效 Key 而通用放行 |
| Observer 目录能力 | 管理认证后签发的单次、30 秒令牌；仅 Bearer `GET /v1/models`，不能推理、计数或管理 |
| CPA 管理密钥 | 管理认证；不是 Observer 客户端 Key，Observer 不将客户端 Key 升格为管理凭据 |

没有原生凭据兜底时，不支持的 Observer 请求由宿主拒绝认证；在配置原生客户端 Key 的
真实宿主中验证为 401，而不是放行后漏记账。CPA 对这些接口自身的支持状况不由插件决定。

Responses WebSocket 仅支持精确 `GET /v1/responses` 握手，不扩大到其他 GET 路由或其他
WebSocket 入口。能力门槛取自宿主 lifecycle 传入的 `schema_version`，不是插件注册声明：
插件仍注册 schema 6，只有宿主 schema version ≥7 才启用 WebSocket（首个支持版本 CPA
v8.0.44）。v8.0.30–v8.0.43 虽然同样报告 schema 6，仍只支持 HTTP；在这些旧宿主上，
Observer WebSocket 握手不被认领，拒绝升级，不回退成放行。握手还必须是精确路径，并符合
WebSocket Upgrade/Connection 头条件；执行阶段依据宿主可信的执行 session metadata 判断实际
WebSocket 轮次，而不以客户端伪造的 Upgrade 头绕过 HTTP 采集。运维健康信息提供
`responses_websocket` 布尔能力标记。

每个独立轮次沿用原有模型授权、额度、并发和计费；同一连接各轮使用相同 Trace，但有各自的
请求记录。包含 `requestBody` 条件的手工价格按轮次请求帧事实匹配，因此各轮分别定价；条件
facts 仅有界保存在内存，重启、裁剪、过期或事实缺失时按既有 `missing_facts` 语义处理，不猜价；
HTTP 同 Trace 的歧义语义保持不变。用量记录另依据持久化的 WebSocket 传输标记识别连接轮次，
因此事实快照丢失不影响 WebSocket 记录的传输标记。WebSocket 轮次跳过内容采集，不入采集队列；
记录带 `websocket: true`，详情的 capture、headers、body 状态均为 `unsupported`，原因是
`websocket`，且与采集模式无关。详情 JSON 的空内容表示为 `headers: {}`、`body: null`，字段
不会省略，也不包含采集内容。

首版不支持上游 Codex response-steering 在同一执行中通过后续 `response.create`、`append` 或
`steer` 帧加入/改写业务输入。宿主对这些 follow-up 使用原生命周期请求 ID 回调，无法形成独立准入；
对已确定属于 Observer 的 steering follow-up 明确返回 HTTP 400，不重跑同一请求 ID 冒充新轮次，
不会新增准入或采集。请改用顺序 Responses 请求，并在需要时于 CPA 宿主关闭 response-steering；
这不要求上游改用 SSE。正常 CPA 自动重试、HTTP 语义及原生 Key 行为保持宿主原有处理。HTTP 请求单独
附带伪造 Upgrade 头仍按 HTTP 处理，不会被误判为 WebSocket 轮次。

## 认证、模型授权、准入与记账不是同一件事

- `Authenticate` 检查密钥哈希、启用、到期等状态，不因认证成功就授权任意模型。
- 自有生成 / count 在执行前用完整请求模型 ID 检查 `AllowedModels`。
  空列表表示这些支持接口内不限制模型，不表示获得其他接口权限。
- 生成再检查额度和并发；count / 目录不消耗生成额度或并发。
- 记账错误或漏记不作为后续准入条件，崩溃重启不自动封锁已有 Key。
- `request.before` 先判断自有 caller scope 与操作，不属于 Observer 的请求直接透传，
  不访问 Observer 存储。自有生成在存储不可用时仍拒绝准入。
- 不通过 `generate` 正文字段猜测操作，不修改请求正文、头、路由、重试或会话。

**限制只属于相应凭据，不是进程级模型策略。** 同一明文同时配置到 CPA 原生和 Observer，
或 query / header 混入独立原生凭据时，宿主的原生 provider 可能先认证成功。
这种请求按 CPA 原生规则执行，不受 Observer Key 的模型、额度、禁用或删除控制，
也不进入 Observer 账本。请使用不同凭据及单一一致的来源；不能声称删除 Observer Key
会同时撤销 CPA 中那份独立凭据。

## 宿主边界与验证

SDK 锁定 `CLIProxyAPI/v8 v8.1.4`。HTTP 功能最低宿主仍为 CPA v8.0.30 / 插件 schema 6；
Responses WebSocket 仅在宿主生命周期传入 `schema_version >= 7` 时启用，首个符合条件的版本是
CPA v8.0.44。不能仅凭 v8.0.39 起始的逐轮认证能力判断支持，因为 v8.0.30–v8.0.43
仍报告 schema 6。插件注册保持 schema 6 和现有能力，不启用新版正文省略或正文修改能力。
管理健康信息的 `responses_websocket` 字段可用于宿主能力分支。

Observer 自有生成继续使用已有模型授权、额度、并发和用量记账；额度拒绝 HTTP 响应保持 429，
错误 `type` 与 `code` 均为 `insufficient_quota`，其他 HTTP 拒绝行为不变。支持的 CPA v8.1.4
WebSocket 每轮额度拒绝会先向客户端发送 status 429 的 error 帧再断开；v8.0.30 及其他 schema 6
旧宿主不支持 Observer WebSocket 握手。手工价格的轮次条件事实有界保存在内存；重启或保留上限
裁剪后缺失时沿用 `missing_facts`，费用不猜测、不借用其他请求事实。Alpha Search、Live/Realtime
等直接转发链没有完整的 Observer 执行授权和会话用量钩子；本次不为 Observer Key 开放这些接口，
也不宣称其具有 Observer 实时计费或额度控制。CPA 原生请求保持原有语义，不需要为认证共存改造宿主。

`native_scope_test.go` 对比启用 / 停用插件的原生生成、计数、目录与管理行为，
核对发往假上游的正文和请求头一致，且没有 Observer 账目。
`native_extensions_test.go` 验证原生 Realtime 令牌签发与两种 Alpha Search 别名继续工作，
受限 Observer Key 不能借这些入口绕过模型权限。所有上游均为隔离回环测试服务。
模型授权、额度、并发、Gemini 操作区分和目录能力边界继续由相应测试验证。

这是独立插件，不是进程外强制网关。宿主可能在插件卸载、熔断或钩子失败时跳过插件；
无原生 Key 的匿名配置注册 provider 后也可能从匿名成功变为 401。
本次不更改这些宿主边界，不以全局独占认证或伪造认证成功来掩盖。

## Key 明文与数据

按用户对内部系统的明确要求，新建/自定义/轮换后的当前密钥明文写入本机数据库，
在管理列表“密钥”列和详情中可以反复查看、复制，关闭弹窗、刷新、重启不再丢失。
仍保持稳定内部 ID、认证哈希索引、重复冲突原子性、固定账本和额度规则。
禁用保留当前明文；撤销和终态删除清空当前明文/哈希，但不承诺擦除 bbolt 历史页。

v1.1.0 使用 **schema3 / `observer_features=4`**。从 v1.0.0–v1.0.4 升级无需重建数据库，
但新版打开后旧版会拒绝该库；回退需升级前的一致备份和匹配旧二进制。
schema2 及更早数据库会在可写打开前被明确拒绝，不迁移、不双读、不就地改写；
仅从这些旧版本升级时，需停流备份后安排新的数据库路径并重新创建 Key 与价格配置。
旧库中只存认证哈希的 Key 无法还原明文，也不显示内部 ID 冒充密钥；可重新生成。
发布或升级过程不自动删除、覆盖或修改任何生产文件。

管理 API 认证、request capture 脱敏、客户端密钥不写浏览器持久存储/日志/URL等继续保留。
数据库和备份现在包含可用客户端明文，按内部凭据资料管理。
