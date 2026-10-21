# Observer 当前开发实现说明

本文件描述 v0.9.0 当前实现；`REFACTOR-next.md` 是历史需求设计，
v0.6/v0.7/v0.8 发布说明保留各版本历史行为。当前采用最小非独占认证共存，
详见 [认证与模型权限](model-policy.md)，不接管 CPA 的原生凭据或实时令牌。

## 能力与可复用验收

| 需求 | 当前实现 | 主要回归入口 |
| --- | --- | --- |
| R01 全宽 | 三页占满 iframe 安全可用宽度，表格局部横滚 | `internal/web/browser/run.cjs` 官方壳、多宽度、侧栏切换与截图 |
| R02 记住登录 | Observer 自有存储、明确偏好、结构化 401/退出清理 | `web/src/App.test.tsx`、官方壳重载/401 |
| R03 稳定刷新 | 仅请求页单在途定时器、失败退避、原位保留旧行 | `Requests.test.tsx` 的暂停/迟到响应/失败测试、浏览器 12 次实际 5 秒刷新 |
| R04 UTC 日期 | 草稿快捷范围，包含结束日，今天截止查询时刻 | `statistics_test.go`、`management_query_test.go`、`Requests.test.tsx` |
| R05 平铺筛选 | 查询/Enter 才应用，历史维度及下线模型可选 | `ui.test.tsx`、`Requests.test.tsx`、真实宿主 `rebuild_test.go` |
| R06 细分统计 | 事务性小时/日汇总、一致视图、四种分组、样本覆盖 | `statistics*_test.go`、`load_acceptance_test.go`、真实升级测试 |
| R07 终态删除 | 墓碑清哈希，busy/幂等/410，保留账本 | `key_lifecycle_test.go`、`rebuild_test.go`、浏览器删除流程 |
| R08 ID/密钥 | `sk-` + 48 字母数字，内部 ID 保留关联；当前明文持久展示/复制 | `key_material_test.go`、`custom_keys_preview_test.go`、Key 页面测试 |
| R09 可解释计费 | 共用解析器、完整身份、候选和显式映射、历史报价固定 | `internal/pricing/*test.go`、`upgrade_test.go`、设置页草稿隔离测试 |
| R10 内容生命周期 | 三模式、独立 TTL、单 gzip、索引 FIFO/到期、清理/清空/整理 | `capture*_test.go`、`rebuild_test.go`、`acceptance_test.go`、设置页测试 |
| R11 认证共存 | 原生凭据保留 CPA 行为；Observer 只限制、记账自己支持的调用 | `native_scope_test.go`、`native_extensions_test.go`、`gemini_test.go` |

测试文件位于 `internal/engine`、`internal/plugin`、`internal/pricing`、
`web/src` 和 `tests/integration`。测试覆盖能力不等于任意部署保证；
实际候选、运行环境、结果与未覆盖项须一并保留。Mock 浏览器负责交互/布局，
引擎/真实宿主负责账本、认证和内容语义，不相互冒充证据。

## 认证范围与 SDK 限制

- SDK 锁定 `CLIProxyAPI/v8 v8.0.30`，不修改 CPA 核心。
- 识别的生成操作：POST `/v1/chat/completions`、`/v1/completions`、`/v1/messages`、
  `/v1/responses`、`/backend-api/codex/responses`，以及 Gemini
  `/v1beta/models/{model}:generateContent|streamGenerateContent`。
- 目录读取：GET `/v1/models`、`/v1beta/models` 及合法模型详情。
  count：POST `/v1/messages/count_tokens`、Gemini `:countTokens`。
  目录/count 不准入、不预留、不采集、不记账；count 仍校验自有 Key 的模型权限。
- request.before 只处理由 Observer 认证并记录的自有生成/计数操作；
  原生/未知/缺失 caller scope 直接透传，不访问 Observer 存储。
  自有操作校验精确请求模型，不匹配返回 403；生成还检查额度和并发。
  Gemini 的 before-hook 是通配模板，使用认证时记下的真实操作，不猜正文；
  真生成也不能用 `Generate=false` 绕过准入。
- 已认证的自有生成在存储暂停、重配置、记账故障或维护时 fail-closed；
  不能把无关流量一起拒绝，也不能一律返回认证成功。
- 图片/音频/视频、Alpha Search、Live/Realtime、Responses WebSocket 等接口
  不在 Observer Key 的认证白名单；即使 Key 配置全部模型也不通用放行。
  CPA 原生凭据继续按宿主行为访问这些接口，执行不纳入 Observer 账本。
  CPA 原生临时令牌保留签发、有效期、模型/会话绑定和通话归属；Observer 不复制
  令牌存储或另造替代协议。目录能力仍只授权一次 GET /v1/models。
- **无原生 Key 的匿名模式**：CPA 不安装认证 provider 时可能匿名返回 200；
  安装非独占 provider 后全链弃权会返回 401。当前 SDK 没有安全的按路径原生
  委派接口，因此不承诺这类配置的透明共存。真实宿主测试锁定该已知限制。
- **同明文双重身份/混合来源**：原生与 Observer provider 各自认证。
  同一明文也在 CPA 原生 Key 列表时，原生认证先胜出便按原生调用执行，
  不受 Observer 模型/额度/启停限制，也不作为 Observer 请求计账。
  跨 query/header 的混合凭据亦可能由原生 provider 接管。
  不读取宿主密钥库、不改变 provider 顺序抢认证；部署时必须使用不同明文并只发
  一套一致凭据。客户端得到 200 不等于已受 Observer 额度管理。
- 插件被宿主卸载/停用后不能拦截请求，CPA 的非持久化用量队列也不能保证
  热替换/崩溃时所有事件交付。停流升级、检查待记账与 unknown 标记仍然必要。

## 新 API（同源、CPA 管理认证）

前缀 `/v0/management/plugins/cliproxyapi-observer`。仍是静态注册路由，
ID 用 query，响应 `no-store`，错误不回显凭据或底层敏感异常。

### 请求视图

```http
GET /requests/view?from_date=2026-10-01&to_date=2026-10-20&view=model&page=1&page_size=25
```

UTC 日期包含结束日，今天截到本次查询时刻；不接受混入 `from/to`。
过滤参数仍为 `key_id/model/provider/auth_index/status`，状态 `success|failed`；
view 为 `requests|model|key|account|provider`，page_size 最多 100。
未知、重复、畸形参数被拒绝。

返回：

```text
range      {from, to, as_of, timezone:"UTC"}
summary    Aggregate
series     {granularity:"hour"|"day", buckets:[Bucket]}
records    [Record]                  # 仅 requests 视图
groups     [Aggregate + 分组身份]     # 其余视图
pagination {page, page_size, has_more}
coverage   {raw_from, daily_from, detailed_requests, requests, backfill_pending}
```

一次 DB 读事务保持上述部分一致；`as_of` 不是跨请求快照令牌。Aggregate 在原四项
`requests/failures/tokens/cost_micro_usd` 上增加：

```text
input_tokens/output_tokens/cache_read_tokens/cache_write_tokens/reasoning_tokens
complete_requests/detailed_requests/unpriced_requests
latency_ms/latency_samples/ttft_ms/ttft_samples
```

总延迟/TTFT 按各自有效样本数计算平均，细分 Token/缓存命中率只用完整样本集合的合计计算，
reasoning 已在 output 内。每条 TPS 仅用于完整、有效流式样本，不提供平均 TPS。
旧日汇总没有细分时 `detailed_requests=0`，不能把缺失视为已知零。
可用原始明细只在查询范围/不足一小时的边界解码，完整小时复用小时汇总；
跨日图表仍按天合并，不把小时直接塞进日图。

```http
GET /requests/facets?field=model&search=claude&limit=100&cursor=...
```

field 为 `model|key_id|provider|auth_index`，返回
`{items:[{value,label}],next_cursor?}`。来自保留范围内历史元数据和 Key 身份，
不是当前页、凭据文件或正文扫描。前端合并当前可用目录并保留已选择的下线值。
旧 `/requests`、`/summary` 保留 RFC3339 `[from,to)` 语义。

### Key 与内容

- `POST /keys/delete?id=...` → 204，重复幂等。活跃/待记账 → 409 busy；
  终态 Key 再编辑/启用/轮换/重置/恢复 → 410。
  `GET /keys?include_deleted=true` 包含墓碑，默认排除。
  旧 `DELETE /keys` 仍为可轮换后再用的撤销，不改成终态删除。
- Key 管理响应增加 `secret` 当前明文字段，可在列表和详情反复复制，重启后仍可读取。
  创建/轮换仍保留顶层 `secret`。禁用保留明文，撤销/删除清空明文和认证哈希。
  旧 hash-only 行无法恢复，显示未保存并提示重新生成；也可备份后使用新库重新创建。
  客户端明文不写浏览器持久存储/URL/日志，不加入请求、汇总、facets 或健康响应。
- 设置增加 `capture_mode=off|headers|headers_and_body` 和
  `capture_retention_seconds`（60..raw_retention_hours×3600）。
  旧客户端只发 bool 时：false→off，true 只在原来 off 时启用 body，
  否则保留已经启用的较窄模式。两字段都显式提交必须一致；
  省略新增字段不会抹掉已确认的 TTL/模式/价格映射。
- `POST /captures/cleanup` 空 body → `{removed_records,removed_bytes}`。
  `POST /captures/purge` 必须 `{"confirm":true}`，返回相同结构。
  两者均不删元数据/汇总、不改额度/费用；purge 的事务代次阻止旧队列回写。
- detail 增加 `headers_status/body_status/capture_reason`、
  `captured_at/expires_at/original_bytes/compressed_bytes/price_quote`。
  保存的到期时间只会收紧，排队时亦固定上界；过期、清空或淘汰后不恢复。
  off 限制全部历史内容读取；headers 模式从不返回旧正文。
- `/health` 增加内容预算/压缩字节数、可回收空间、按原因丢弃、计数起点、
  最近维护结果和维护错误。诊断丢弃不等于账单丢失。
  `/compact` 无活跃/待记账/待采集时执行；维护中新受管生成可重试 503。
  物理文件回收不等于磁盘安全擦除，不承诺替换后同步失败可以自动回滚。

### 可解释价格

`pricing.mappings` 是 `{model,provider,catalog_model}` 数组，按完整公开 ID 生效，
只影响未来计费，不改路由。规则 > 精确手动价 > 显式/可信自动来源 > 未定价 $0。
preview、Snapshot、Quote 使用同一个 resolver；原因/候选/缺失费率/目录身份
由服务端提供。未知显示 `—`，确实为免费才显示 0。目录故障保留最后有效缓存，
入库报价固定，之后改映射/目录/手动价都不重算历史。

## 升级、失败恢复与回退

1. 安排停流，等待活跃/待记账归零，正常停止 CPA。不要把一次 HTTP 响应结束
   当作用量回调已落盘。
2. 备份**整个数据目录**与匹配旧插件二进制，并核实可读取；
   保留中断标记等关联文件。不要在线普通复制正在写入的 bbolt 文件。
3. 确认采用认证共存策略，且 CPA/Observer 使用不同凭据。数据可继续使用原 schema2 库，也可按用户允许
   在停流备份后显式换用全新空库、重新创建客户端 Key；不自动删除旧文件。
4. 检查 Key 身份/累计、旧费用、内容模式和健康。旧 schema2 增量补建索引，
   按 1000 条明细一批回填新增细分字段及小时汇总；事务 checkpoint 可续接，
   不再次累加旧四项汇总、重计额度或重新报价。已不在库中的细分不可重建。
5. 新写入功能标记为 `observer_features=2`；高于 2 的库会在
   可写打开前拒绝；旧无 schema2 的非空异构库仍拒绝，不就地迁移。
6. 如需回退，停止新版本，恢复第 2 步的一致备份及对应旧二进制。
   **不能直接让旧版本写新库**：v0.8 支持 feature1，会拒绝 feature2；
   v0.7 不认识功能/内容索引标记，也未保证拒绝。
   恢复旧备份意味着放弃升级后数据，必须先导出/保全所需证据并明确接受该代价。

`TestUpgradeFromActualV070PreservesLedgerAndCredentials` 从固定旧 commit
`37a2468f055c698158e50cde9b1758a8417e487e` 构建旧插件，在隔离宿主用实际旧 writer
生成 Key/付费记录/双层内容，再切换候选并重启。它验证旧密钥仍有效、原费用不变、
新费率仅用于新请求、回填重启不双计。不是对生产库做演练。

## 验证命令与边界

```sh
make test
make race
make load
npm --prefix web audit
make frontend
CPA_MANAGEMENT_HTML=/absolute/temp/management.html make browser
OBSERVER_BROWSER=1 CPA_BINARY=/absolute/temp/cpa-v8.0.30 \
  CPA_TEST_ARTIFACTS=/absolute/temp/report-30 make integration
# 换成 cpa-v8.0.33 与独立 report-33 再执行；不沿用旧发布证据。
```

`load` 使用 1 万/10 万真实 gzip 元数据、36500 个日维度、并发准入，
及 1/8/32 MiB 三档接近内容预算，输出查询/准入/Store 锁等待 p50/p95、
字节数和 FIFO 丢弃。CPU、Go、文件系统、样本数必须随报告保存；
这些固定本机基准不是公网延迟或任意规模 SLA。

官方壳自动检查 390/640/768/1280/1440/1920/2560px，多主题和侧栏状态，
截图需人工复核。额外使用独立临时 Chrome profile，完全关闭并重开浏览器验证登录恢复；
通过该临时 profile 的 Chrome 原生设置验证 200% 缩放，核对实际 devicePixelRatio
与 CSS 视口，再走官方壳三页/弹窗流程。不是 CSS zoom 或仅修改 viewport 冒充。
测试不读取用户已有 profile、宿主凭据文件或管理中心保存的认证内容。
共享系统无障碍模式/读屏器等未测环境也不由这些截图保证。

发布、push、打 tag 或部署均需要独立授权。
