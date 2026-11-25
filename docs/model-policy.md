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

握手只认领带 WebSocket Upgrade 的 `GET /v1/responses`，不认领 `GET /backend-api/codex/responses`
或其他 WebSocket 路径。执行阶段使用 CPA 的执行会话元数据识别轮次；HTTP 请求带 Upgrade 头
也不会被当成 WebSocket。

每个独立轮次沿用原有模型授权、额度、并发和计费。同一连接共享 trace ID，但每轮单独记用量。
`requestBody` 价格条件只保留有界的匹配结果，不保留原文；结果缺失时按既有 `missing_facts` 处理。
WebSocket 传输标记独立持久化，不受条件结果过期或丢失影响。无论采集是否开启，记录带
`websocket: true`，详情的 `capture_status`、`headers_status` 和 `body_status` 都是 `unsupported`，
`capture_reason` 是 `websocket`，`headers` 和 `body` 分别返回 `{}` 和 `null`。

Codex `response-steering` 会让追加帧共用原请求的生命周期，Observer 无法逐帧单独准入。
这些后续业务帧会收到 `status: 400` 的 WebSocket 错误帧，不会转发。请使用顺序 Responses 请求，
并在 CPA 保持 `upstream.codex.response-steering` 关闭（默认）；上游不必改用 SSE。

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

SDK 锁定 `CLIProxyAPI/v8 v8.1.4`。HTTP 最低支持 CPA v8.0.30，插件仍注册 schema 6。
WebSocket 仅在宿主报告 schema ≥7 时开放（首个符合条件的版本是 CPA v8.0.44）。
v8.0.39 起已有逐轮认证，但与更旧宿主同报 schema 6，插件无法安全区分。管理 `/health` 的
`responses_websocket` 表明当前宿主是否允许 Observer Key 握手。

Observer Key 额度耗尽时，HTTP 返回 429，错误 `type` 和 `code` 均为 `insufficient_quota`。
CPA v8.1.4 在 WebSocket 上发送 `status: 429` 的错误帧后断开。两种传输都不会把拒绝的轮次
转发到上游。原生 Key 及上游凭据的冷却、重试和选择仍由 CPA 管理。

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
