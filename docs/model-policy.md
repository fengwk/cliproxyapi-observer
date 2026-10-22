# 认证共存与模型权限

## 最小原则：各管各的

使用 CPA 已有的非独占 `FrontendAuthProvider`，不改 CPA 核心、不另造令牌系统。
CPA 原生凭据保留原生行为；Observer 只管理自己认证且明确支持的调用。
本轮删除候选中的“所有模型执行必须使用 Observer Key”守卫及其拒绝码、路径分类表、
专属 WebSocket 测试框架，不将其替换成新的全路由拦截层。

## 凭据与接口范围

| 凭据 / 入口 | 行为 |
| --- | --- |
| CPA 原生 Key | 宿主原生认证、权限和执行；不自动导入 Observer、不纳入 Observer 账本 |
| CPA 签发的 Realtime `ek_…` | CPA 负责签发、有效期、模型/会话绑定、签发者身份与通话归属；Observer 不复制或读取其令牌存储 |
| Observer Key：POST Chat / Completions / Messages / Responses / Codex Responses | 检查 Key 状态、精确请求模型、额度和并发；支持的执行按 Observer 规则记账 |
| Observer Key：Gemini generateContent / streamGenerateContent | 认证时保留真实动作，在通配执行边界按自有操作进行授权、准入和记账 |
| Observer Key：Claude / Gemini countTokens | 检查 Key 状态及精确请求模型，不计费、不占并发、不采集 |
| Observer Key：支持的 GET 模型目录 / 详情 | 只读，不准入、不计账 |
| Observer Key：图片、音频、视频、Responses compact、Interactions、Alpha Search、Live/Realtime、WebSocket 等其他入口 | 不在认证白名单，不因“全部模型”或有效 Key 而通用放行 |
| Observer 目录能力 | 管理认证后签发的单次、30 秒令牌；仅 Bearer `GET /v1/models`，不能推理、计数或管理 |
| CPA 管理密钥 | 管理认证；不是 Observer 客户端 Key，Observer 不将客户端 Key 升格为管理凭据 |

没有原生凭据兜底时，不支持的 Observer 请求由宿主拒绝认证；在配置原生客户端 Key 的
真实宿主中验证为 401，而不是放行后漏记账。CPA 对这些接口自身的支持状况不由插件决定。

## 认证、模型授权、准入与记账不是同一件事

- `Authenticate` 检查密钥哈希、启用、到期等状态，不因认证成功就授权任意模型。
- 自有生成 / count 在执行前用完整请求模型 ID 检查 `AllowedModels`。
  空列表表示这些支持接口内不限制模型，不表示获得其他接口权限。
- 生成再检查额度、并发及记账故障；count / 目录不消耗生成额度或并发。
- `request.before` 先判断自有 caller scope 与操作，不属于 Observer 的请求直接透传，
  不访问 Observer 存储。自有生成在存储不可用时仍拒绝准入。
- 不通过 `generate` 正文字段猜测操作，不修改请求正文、头、路由、重试或会话。

**限制只属于相应凭据，不是进程级模型策略。** 同一明文同时配置到 CPA 原生和 Observer，
或 query / header 混入独立原生凭据时，宿主的原生 provider 可能先认证成功。
这种请求按 CPA 原生规则执行，不受 Observer Key 的模型、额度、禁用或删除控制，
也不进入 Observer 账本。请使用不同凭据及单一一致的来源；不能声称删除 Observer Key
会同时撤销 CPA 中那份独立凭据。

## 宿主边界与验证

SDK 基线仍为 `CLIProxyAPI/v8 v8.0.30`，最低插件 schema 6。
Alpha Search、Live/Realtime 等直接转发链没有完整的 Observer 执行授权和会话用量钩子；
本次不为 Observer Key 开放这些接口，也不宣称其具有 Observer 实时计费或额度控制。
CPA 原生请求保持原有语义，不需要为认证共存改造宿主。

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

v1.0.0 使用 **schema3 / `observer_features=3`**。schema2 及更早数据库会在可写打开前
被明确拒绝，不迁移、不双读、不就地改写；回退需一致备份和匹配旧二进制。
停流备份后，请由你安排新的数据库路径并重新创建 Key 与价格配置。
旧库中只存认证哈希的 Key 无法还原明文，也不显示内部 ID 冒充密钥；可重新生成。
发布或升级过程不自动删除、覆盖或修改任何生产文件。

管理 API 认证、request capture 脱敏、客户端密钥不写浏览器持久存储/日志/URL等继续保留。
数据库和备份现在包含可用客户端明文，按内部凭据资料管理。
