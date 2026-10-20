# 模型接口 Observer Key 策略（开发中，发布阻塞）

## 目标与当前状态

用户要求：**模型接口必须使用 Observer Key，非模型接口保持不变**。
这取代 v0.8.0 的原生模型调用透传规则，但不是全局独占认证。

当前实现完成了有限模型执行拦截与明文 Key 管理，**尚未完成全路由强制鉴权**。
仅把路径写进分类表不能让宿主补调用插件。发布不能隐瞒以下 SDK 覆盖缺口。

## 已实现的执行边界

| 入口 | 当前行为 |
| --- | --- |
| POST Chat / Completions / Messages / Responses / Codex Responses | 有效 Observer 认证及正常准入才执行；原生 Key 返回 `403 observer_key_required` |
| Gemini generateContent / streamGenerateContent | Observer 认证时记录真实动作；原生 scope 在通配执行边界拒绝 |
| Claude / Gemini countTokens | 要求 Observer Key，校验状态/模型权限，但不计费、不占并发、不采集 |
| 图片、音频、视频、Responses compact、Interactions 等已进入 before 的未支持执行 | 返回 `403 model_surface_unsupported`，不悄悄透传原生调用 |
| Responses WebSocket | Observer Key 不支持握手；原生 Key 可以握手，但每 turn 的生成在 before 拒绝，v8.0.30 客户端观察为关闭而非 JSON 错误 |
| 模型目录、详情、健康、管理认证、OAuth 等非模型执行 | 不在执行 guard 中接管，保留宿主行为 |

无 Key 或任意无效 Key 仍由宿主认证返回 401。原生 Key 有效不等于获得模型执行权限。
不把原生凭据自动导入 Observer，不读取宿主密钥，也不修改请求头/正文/路由或选路顺序。
同一密钥同时配置到原生和 Observer、或混合 query/header 凭据时，原生认证先胜出
的请求在受管模型边界也会被拒绝；请使用独立 Observer Key 和一套一致凭据。

## 尚未封闭的入口：宿主不经过 before

已核查 SDK 基线 `CLIProxyAPI/v8 v8.0.30`（本地源码提交
`5be2cdab0869cbaa3d401891f740aebf7e15aa99`）：

| 原生入口 | 已追踪的直接上游链（该版本源码） |
| --- | --- |
| POST `/v1/alpha/search`、`/backend-api/codex/alpha/search` | `internal/api/server_routes.go` 的 `codexAlphaSearch` → AuthManager.HttpRequest |
| Live / Realtime HTTP 执行 | `internal/client/codex/live/live.go` 的 Handle → AuthManager.HttpRequest |
| Realtime WebSocket | `internal/client/codex/live/websocket.go` → dialer.DialContext |
| Live / Realtime sideband | `internal/client/codex/live/sideband.go` → dialer.DialContext |
| Realtime 临时 token / session 等家族 | 独立认证/会话链，不属于已经覆盖的 before 模型入口；不作保护保证 |

这些路径不通过 SDK 的 `applyRequestInterceptorsBeforeAuth`。
普通非独占前端 provider 返回 `Authenticated:false` 是“交给下一 provider”，
不是“终止请求”；原生 provider 仍可能认证成功。

不能用以下方式假装补齐：

- 开启全局独占认证会改变非模型接口，不符合用户要求。
- 对所有调用返回认证成功，会把错误凭据认证为有效，不能使用。
- 依赖请求正文 `generate` 判断，无法区分真实生成与计数，也可由客户端操控。
- 抢占调度/改路由不是可靠的请求过滤器；Home 选路路径会绕过插件 Scheduler，
  而 Scheduler 请求也缺少精确 method/path。不能把未覆盖链伪装成已受控。

**阻塞的收尾决策**：在支持路径级终止且覆盖这些入口的宿主能力可用后验证，
或者在部署入口显式封禁未支持的执行路径并禁止直连绕过，再以真实覆盖范围发布。
当前未修改 CPA 核心，也未替用户更改生产代理/防火墙配置。仅源码实现不能替代
部署入口隔离；“已经部署、无法绕过”的说法在此不成立。

此外，插件卸载/熔断/调用错误时宿主仍可能跳过它；这也不是进程外强制网关。

## Key 明文与数据

按用户对内部系统的明确要求，新建/自定义/轮换后的当前密钥明文写入本机数据库，
在管理列表“密钥”列和详情中可以反复查看、复制，关闭弹窗、刷新、重启不再丢失。
仍保持稳定内部 ID、认证哈希索引、重复冲突原子性、固定账本和额度规则。
禁用保留当前明文；撤销和终态删除清空当前明文/哈希，但不承诺擦除 bbolt 历史页。

主 schema 仍为 2，功能标记为 `observer_features=2`。v0.8 的 feature1 writer 会拒绝
直接打开新库，回退需一致备份和匹配旧二进制。旧 hash-only Key 不猜明文，也不显示
内部 ID 作为密钥；可重新生成。用户允许停流备份后显式新建数据库、重新创建所有 Key，
但本次开发不自动删除或覆盖任何生产文件。

管理 API 认证、request capture 脱敏、客户端密钥不写浏览器持久存储/日志/URL等继续保留。
数据库和备份现在包含可用客户端明文，按内部凭据资料管理。
