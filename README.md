# Observer

CLIProxyAPI (CPA) 的原生观测插件。独立仓库、独立动态库，**不修改、不 fork CPA 核心**。
插件展示名为 **Observer**，稳定 ID 为 `cliproxyapi-observer`，版本由
`internal/plugin.Version` 注入；正式构建从发布 tag 解析版本，开发构建附带提交标识。

插件只做一件事：在请求生命周期中**只读观测**，把用量与（可选的）请求正文写入本地
bbolt 数据库，并通过 CPA 管理 API 与内置中文页面提供查询。它不代理上游、不改写
请求体/请求头/模型路由/重试/会话，也不参与推理决策。

## 功能

- **请求观测**：注册 CPA 的 `UsagePlugin`、`RequestInterceptor` 与 `ManagementAPI`。
  请求前回调只入队，用量回调记录执行信息；即便存储关闭/已满/不可用或观测数据非法，
  也**不修改请求**、不阻塞推理。
- **用量统计**：只归一化一次并持久化记账质量（accounting quality）与 TPS。按分钟/提供商/模型
  事务性预聚合，汇总直接读取预聚合区间并降采样到 ≤300 个点，不解码全量请求。
- **按页查询**：请求列表按时间戳+序列号倒序，以 `offset`/`limit` 翻页，不查询精确
  总数或总页数；提供商/模型过滤只扫描选定区间。
- **查询时定价**：按**精确、完整**的模型 ID 配置 USD/百万 token 单价（输入/输出/
  读缓存/写缓存），支持有序条件规则、输入 token 阈值与每日 UTC 时间区间。
  新记录只保存 token 与执行数据，查询时按当前生效价格计算历史成本。
- **Key 与认证文件维度**：客户端 key 仅保存数据库专属 HMAC 指纹，上游凭据仅保存
  CPA `auth_index`。可筛选概览、趋势、模型分组、请求及 Key 分组；认证文件显示标签与文件名。
- **未分类 token**：无法确定语义的用量保留原始计数与权威总量，标记为 `unclassified`
  质量，并把有歧义的 TPS/成本置空，而不是按模型名猜测协议或价格。
- **本地库**：单文件 bbolt，相对路径按 CPA 工作目录解析（默认
  `data/cliproxyapi-observer.db`）。无硬编码加密密钥、无 API key 存储、无外部 HTTP 调用。

## 职责边界（不修改 CPA）

```text
客户端 -> CPA 的会话识别、亲和、选 key、冷却、重试、路由
       -> Observer 只读观测（用量 / 请求元数据 / 可选正文）-> 本地 bbolt
```

- CPA 仍独占凭据池、故障转移、冷却、会话识别与上游推理，全部行为不变。
- 插件不新增 provider/executor/router/响应或流拦截器，不产生任何请求修改。
- 观测为非阻塞：入队失败或队列/正文内存达到上限时**丢弃观测并计数**，推理照常。
- `Status` 暴露丢弃的用量/正文与写失败次数，便于发现静默丢失。

## 数据、保留与隐私

- **正文采集默认关闭**：`capture-bodies: false`。只有运维显式开启后才可能写入正文，
  且始终通过 CPA 管理认证保护。
- **保留期相互独立**：请求元数据 `request-retention`（默认 24h）、正文
  `body-retention`（默认 24h，可缩短，**不得超过 24h**）与聚合统计
  `stats-retention-days`（默认 365 天）分别过期，互不影响。
- **读取即过期**：正文读取与可用性计算都会强制检查过期，不依赖后台清理，
  因此已过期正文绝不会被返回；缩短配置后的保留期立即约束读取，延长配置不延长原有
  正文已持久化的期限。
- **正文有界**：正文总字节上限 `max-body-storage-bytes`（默认 256 MiB），
  超出时确定性淘汰最旧正文；单条正文上限 `max-body-bytes`（默认 1 MiB），
  非法 JSON 或超限正文被跳过而**不截断成损坏内容**。降低容量后，重开数据库即执行
  淘汰，不需要等待新请求。
- **内容与元数据分离**：正文单独存桶，绝不进入请求记录或聚合记录；正文关联
  `request_id` 与 `trace_id`。
- **关联索引回收**：最后一条关联正文过期或被淘汰时，同时删除 trace 映射与引用计数。
  有多个正文的 trace 保持不可判定，不会在部分正文删除后任意关联剩余正文；启动时
  重建引用计数并清理旧库中的无主映射。
- **脱敏**：对敏感 JSON 字段名（`authorization`、`api_key`、`api-key`、`password`、
  `secret`、`access_token`、`refresh_token`，大小写不敏感）做脱敏，正常 prompt/代码
  字符串保留。不保存请求头、认证对象、`Failure.Body` 或响应内容。
- **无凭据泄漏**：错误信息不返回数据库路径或请求载荷；管理路由响应带
  `Cache-Control: no-store` 与 `nosniff`；内嵌页面 CSP 仅允许同源，禁止内联代码与
  任意站点嵌入。

24 小时限制是插件的访问期限与逻辑删除策略，**不等同于磁盘安全擦除**。bbolt 删除后
会复用空闲页，单次删除不会缩小文件；插件在达到阈值后会自动物理压缩（见下文）。
正文容量限制的是有效正文载荷，不是整个数据库的物理文件大小。
启用采集时，请同时保护数据目录、磁盘与备份，并按敏感数据
处理数据库副本。字段名脱敏不会识别正常 prompt/代码字符串中的任意凭据。

## 安装与使用

要求：支持原生插件的 CPA v8。原生 SDK 基线锁定在 `go.mod`（v8.0.16），最低宿主
v8.0.15；更高 v8 版本由 CI 每日验证，不保证未通过测试的版本或 v9 兼容。
SDK 精确版本由自动维护链路在验证兼容后升级。

### 1. 安装插件

从自定义插件源安装（推荐）。把下面片段合入现有 CPA 配置的 `plugins` 对象：

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  store-sources:
    - "https://raw.githubusercontent.com/fengwk/cliproxyapi-observer/main/registry.json"
```

保存并让 CPA 重载后，在 **插件商店** 刷新、搜索 **Observer** 并安装。商店读取最新正式
GitHub Release，下载当前平台 ZIP、按 `checksums.txt` 校验，安装动态库并启用插件。
自定义源不替换官方源。CPA 运行环境需可访问 GitHub Raw、GitHub API 与 Release 下载地址。

#### 手动构建

本地构建需要 Go 1.26+、C 编译器和 Make：

```bash
git clone https://github.com/fengwk/cliproxyapi-observer.git
cd cliproxyapi-observer
make build
```

产物：`dist/<goos>/<goarch>/cliproxyapi-observer.so`（macOS 为 `.dylib`）。
复制到 CPA 配置的 `<plugins.dir>/<goos>/<goarch>/`，不要将 C 共享库误当成 Go `plugin` 包。
CI 构建 Linux amd64、Linux arm64、macOS arm64 原生库；正式版本的归档与校验和由
`v*` tag 的 release 工作流生成。不支持把 glibc Linux 构建用于 musl/Alpine。

### 2. 配置 CPA

参考 [config.example.yaml](config.example.yaml)，把片段合入现有 CPA v8 配置，
不要覆盖原配置。核心片段：

```yaml
plugins:
  configs:
    cliproxyapi-observer:
      enabled: true
      db: "data/cliproxyapi-observer.db"
      stats-retention-days: 365
      request-retention: "24h"
      body-retention: "24h"
      capture-bodies: false
      max-body-bytes: 1048576
      max-body-storage-bytes: 268435456
      flush: "1s"
      compact-interval: "15m"
      compact-min-bytes: 8388608
```

### 3. 打开插件页

管理面板左侧「插件」分组点击 **Observer**，或直接打开插件资源地址：

```text
http://127.0.0.1:8317/v0/resource/plugins/cliproxyapi-observer/ui
```

页面仅允许同源管理面板 iframe 嵌入，并跟随父级主题；请勿移除 CSP 或允许任意站点嵌入。
连接时输入 CPA **管理密钥**（仅存内存、只作请求头）。页面提供周期 24h/7d/30d、
模型/提供商/客户端 key/上游凭据或认证文件过滤，概览、趋势、分组表、上一页/下一页请求表，
以及需二次确认才展开的正文详情。
连接控件位于内容区的独立卡片，不再占用宿主右上角工具区。
「采集设置」可编辑正文采集、保留期、容量、压缩参数与模型价格；修改只形成草稿，
显式保存且通过校验后才写入 CPA 配置。保存会回读配置并确认已生效；
仪表盘刷新不会覆盖未保存草稿，重新连接会清除旧连接的草稿。
保存与放弃草稿使用跟随主题的异步确认弹窗；可按 Escape 或点击遮罩取消，不产生写入。

### 4. 使用管理 API

均需 CPA 管理密钥。以下是实际宿主验证覆盖的插件自定义管理地址；CPA v8 宿主仍使用
`/v0/management` 挂载这些路由，不能因配置版本为 v8 就直接替换 URL 前缀：

```text
GET /v0/management/plugins/cliproxyapi-observer/summary
GET /v0/management/plugins/cliproxyapi-observer/requests
GET /v0/management/plugins/cliproxyapi-observer/body?request_id=<id>
GET /v0/management/plugins/cliproxyapi-observer/settings
GET /v0/management/plugins/cliproxyapi-observer/health
POST /v0/management/plugins/cliproxyapi-observer/validate
```

`from`/`to` 为 RFC3339，默认最近 24 小时；`requests` 的 `offset` 取值
0..2147483647（默认 0），`limit` 取值 1..100（默认 50），返回
`{"items": [...], "offset": 0, "limit": 50, "has_more": true}`。
非空旧 `cursor`、畸形参数或越界参数返回 400。正文缺失、过期或采集关闭时返回 404。
`settings` 返回 `capture_bodies`、`body_retention_seconds`、
`request_retention_seconds`、`stats_retention_days`、`max_body_bytes`、
`max_body_storage_bytes`、`compact_interval_seconds`、`compact_min_bytes`、`prices`、`price_rules`；
`validate` 用于验证候选配置 JSON patch（支持白名单 keys），成功返回 `{"valid": true}`，
超限 256KiB 返回 413，格式错误或与当前配置冲突返回 400；`health` 来自运行状态。

验证请求体是原始 JSON patch 对象，无包装；只接受 `capture-bodies`、`request-retention`、
`body-retention`、`stats-retention-days`、`max-body-bytes`、`max-body-storage-bytes`、
`compact-interval`、`compact-min-bytes`、`prices` 和 `price-rules`。验证本身不修改配置或数据库。
保存时将同一 patch 发送到 CPA 核心的
`PATCH /v0/management/plugins/cliproxyapi-observer/config`，由核心浅合并、持久化并热重载，
保留未编辑的 `enabled`、`db`、`flush` 等配置。

翻页示例：`requests?offset=50&limit=50&from=<RFC3339>&to=<RFC3339>`。
页面在翻页期间固定时间窗口，刷新、筛选或重连回到第 1 页。过期清理或迟到写入仍可能
导致偏移漂移、重复或遗漏；固定窗口不是数据库快照。深页需跳过前面的匹配记录，
无过滤时开销随 `offset + limit` 增长，有过滤时可能扫描更多区间记录；不会额外全库计数。

## 配置项

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | — | 是否启用观测与写入（由 CPA 插件配置提供） |
| `db` | `data/cliproxyapi-observer.db` | bbolt 路径，相对路径按 CPA 工作目录解析 |
| `stats-retention-days` | `365` | 聚合统计保留天数 |
| `request-retention` | `24h` | 请求元数据保留时长 |
| `body-retention` | `24h` | 正文保留时长，可缩短，不得超过 24h |
| `capture-bodies` | `false` | 是否采集正文（敏感，默认关闭） |
| `max-body-bytes` | `1048576` | 单条正文上限 |
| `max-body-storage-bytes` | `268435456` | 正文总存储上限 |
| `flush` | `1s` | 异步写批次间隔（默认批次 1s / 100 事件） |
| `compact-interval` | `15m` | 自动压缩周期，范围 `1m..24h`（始终启用，无需开关） |
| `compact-min-bytes` | `8388608` | 触发自动压缩的最小可回收页面字节数，范围 `64KiB..8GiB` |
| `prices` | 空 | 精确完整模型 ID 到 USD/百万 token 单价的映射 |
| `price-rules` | 空列表 | 自上而下、首次命中的条件价格规则，未命中再查旧 `prices` 映射 |

`prices` 每项包含 `input`、`output`、`cache-read`、`cache-creation`。单价必须为有限非负数，
trim 后的模型 ID 不得重复。价格来自**当前生效配置**，不向上游查询；修改、增加或删除
价格后，历史请求与聚合统计在下次查询时重新计算。配置为零的价格是有效零成本，
缺少价格或记账质量不完整的请求仍为未定价；旧请求中的金额快照即使存在也不再读取。
JSON settings 使用 `cache_read`、`cache_creation`；patch 与 YAML 同时兼容下划线和连字符，
页面保存使用 `cache-read`、`cache-creation`。

### 有序条件定价

`price-rules` 最多 1000 条，每条包含完整 `model`、`price`，以及两个可选条件：

- `input-tokens-gt`：单次请求的**归一化总输入（含缓存读/写）严格大于**该值，
  取值为 `0..9007199254740991` 的整数；不包含输出 token。
- `time-range`：请求开始时间落在每日 **UTC+0** 的 `HH:mm-HH:mm` 区间，
  起点包含、终点不包含；支持跨午夜，例如 `22:00-06:00`。终点可为 `24:00`，
  起点不能是 `24:00`，两个端点不能相同。UTC 没有夏令时变化。

同条规则的模型和所有条件以 AND 组合；**按列表顺序选第一条匹配规则，并为整个请求
应用该价格**，不是超出部分才加价。无条件默认价应放在该模型的条件规则之后。
UTC+8 的窗口需要减去 8 小时再填写；请按供应商公布的实际时间换算，示例不是实时官方报价。

```yaml
price-rules:
  - model: "example-model"
    input-tokens-gt: 512000
    price: {input: 3, output: 12, cache-read: 0.3, cache-creation: 3}
  - model: "example-model"
    input-tokens-gt: 256000
    price: {input: 2, output: 8, cache-read: 0.2, cache-creation: 2}
  - model: "example-model"
    time-range: "22:00-06:00"
    price: {input: 0.5, output: 2, cache-read: 0.05, cache-creation: 0.5}
  - model: "example-model"
    price: {input: 1, output: 4, cache-read: 0.1, cache-creation: 1}
```

以上示例中，大输入档位优先于夜间规则；希望“大输入且夜间”使用另一价格时，添加一条
同时具有两个条件的规则并上移到这些规则之前。UI 提供上移、下移和紧凑删除操作；
保存时将旧映射转换成无条件规则，持久化 `prices: {}` 和有序 `price-rules`。
settings JSON 的条件键为 `input_tokens_gt` / `time_range`，patch 同时接受下划线和连字符。

```text
cost_usd = (uncached_input * input_price + cache_read * cache_read_price
          + cache_creation * cache_creation_price + output * output_price) / 1,000,000
```

输出使用包含 reasoning 的归一化总数，不重复计费 reasoning。聚合只持久化 token、请求数、
延迟等与价格无关的数据，以及完整记账请求的四个互斥计费桶；明细过期后仍可重算聚合成本。

### 旧统计库升级

首次打开旧库时，在单个事务中升级原有 120 字节统计记录，保留全部流量、token 与延迟总数。
能证明全部请求已完整记账的桶可直接恢复计费分量；其他桶仅从仍保留的完整请求明细恢复。
已过期且无法恢复的部分保持未定价，不猜测成本，也不从短保留期明细重建长期流量总数。
升级有持久标记，后续重开或查询不再扫描全量请求。

首次启用 Key/阶梯统计时，另在一个原子事务中将旧聚合复制到“未归属”维度，不从
短保留期请求猜测历史 key。旧聚合没有单次输入长度；若有可能命中的输入阈值规则，
该部分保持未定价，不擅自套用默认价。仍保留的请求明细可按自身输入长度独立计价。
新聚合按分钟、输入长度、客户端指纹、上游索引、提供商、模型保存 token 计数，
所以明细过期后仍支持条件重定价和 Key 筛选；不保存金额或价格快照。

升级会改变统计存储格式。**升级前应停宿主并备份数据库；旧插件不支持新统计格式，
不可直接用旧版打开升级后的库。** 回滚需恢复升级前的数据库备份。

## API key 维度与 OpenCode Provider

下游客户端 key 与上游凭据/认证文件是两个独立维度：

- `client_key_id`：数据库中随机 32 字节 secret 派生的 HMAC-SHA256（64 位小写十六进制）。
  UI 以下拉选择（全部 / 未归属 / 已出现的指纹，只显示前 12 位），内部按完整指纹传值，
  无需手填完整指纹。CPA 下游 key 没有原生名称。
  secret 随数据库备份、重开和物理压缩保留；不同数据库的同一 key 指纹不同。
- `auth_index`：CPA usage 回调提供的非机密 16 位小写十六进制索引。
  `GET /v8/management/credentials` 暴露当前凭据/认证文件的同一索引、`name` 与 `label`；
  UI 仅保留索引与展示名称，文件显示“标签（文件名）”，相同标签的不同文件仍可区分。
  文件或凭据被删除、名称接口不可用时，历史用量保留且退回显示索引。
  部分 CPA 版本不在此接口列出配置产生的运行时凭据，这些凭据同样按索引展示与筛选。

`summary` 和 `requests` 均接受 `client_key_id` / `auth_index`，两个条件同时填写时取交集，
空字符串表示全部，`unknown` 表示未归属（包括旧记录）。`summary.client_keys` 和
`summary.credentials` 分别返回 `{id, ...counters}` 分组，空 `id` 为未归属；所有分组、
概览和趋势遵循相同筛选。请求日志默认仅保留 24h，7d/30d Key 统计从长期聚合读取。
缓存**请求**命中率为 `cache_hits / requests`，命中指该请求的原始缓存读 token 大于零，
不是缓存 token 占比。

Observer 不保存 `APIKey`、`AuthID`、`Source`、认证文件内容或路径，也不下载认证文件。
CPA 的文件索引基于其凭据身份；更换文件位置或身份可能产生新索引，Observer 不猜测合并。

OpenCode Provider 的凭据绑定执行路径在本地 CPA v8.0.15/v8.0.20 双插件联调中，
流式与非流式请求均进入 Observer，提供商为 `opencode-go`，模型保留
`opencode-go/<model>` 全名；按插件 ID `cliproxyapi-opencode-provider` 过滤会匹配不到这些记录。
当前锁定 SDK 不识别 `opencode-go` 的 token 重叠语义，因此非零用量会标为
`unclassified`：请求数、原始输入/输出/缓存计数仍统计，但不确定的未缓存分量、TPS 与成本
不会猜测，配置价格也不能将未知记账质量变为完整。

如果连请求明细和请求数都没有，应先清空提供商/模型筛选、刷新时间范围，检查两个插件均已
启用，以及 Observer `/health` 的 `dropped_usage`、`write_errors` 和宿主插件加载日志。
上述联调不证明生产环境的缺失原因；解决准确计费需要明确的协议记账语义，不能把整个
多协议 Provider 强行当作 OpenAI。

### 自动物理回收

过期记录由后台清理移除；清理成功后（包括启动时），当 free + pending 页面字节数达到
`compact-min-bytes` 且至少占实际 `.db` 文件的 25%，自动执行 bbolt 压缩。
`compact-interval` 限制两次压缩尝试之间的最短时间，失败也计入间隔。
因此过期后无需管理 API 操作，符合阈值的文件会真正缩小，而不只是内部页面复用。

Linux/macOS 使用同目录 0600 临时数据库、约 1MiB 写事务与分配增量，
完成复制并 Sync 后，仅在临时文件实际小于原文件时原子替换。
数据库文件路径为符号链接时，会替换真实目标文件并保留链接。
大小未减少时保留原库、删除临时库，不计成功压缩，下一次尝试仍受间隔限制。
复制期间原数据库仍可读取，采集保持有界且不阻塞；队列满时按原规则丢弃并计数。
替换前失败保留原库并删除临时文件，失败可在之后重试；压缩不会更改保留的正文、
请求序号或统计。孤立临时文件清理失败也计入压缩错误，不会静默忽略。
其他操作系统不执行不安全的覆盖回写。

`health` 增加缓存的 `database_bytes`、`reclaimable_bytes`、`compactions`、
`compaction_errors`、`last_compaction_unix`（Unix 秒，未发生时为 0）和
`last_compaction_reclaimed_bytes`。这些状态查询不进行磁盘 I/O。

### 小磁盘 VPS 的容量边界

当前没有整个 `.db` 文件的硬容量上限。正文配额只计算脱敏后的正文载荷，不包含
请求元数据、分钟聚合、关联索引和 bbolt 页开销；元数据随请求速率与保留时长增长，
聚合统计随活跃分钟数、输入长度与 Key/模型组合数增长；输入长度高度分散时，
条件价格预聚合的行数可能接近请求数，需要按吞吐量适当缩短统计保留期。
保留期与自动压缩能控制常见场景中的
长期积累，但不能保证高流量下数据库始终小于某个固定大小。

小磁盘可先采用以下保守配置，合入现有插件配置，不覆盖其他字段。平时关闭正文采集，
临时开启后也只保留 1h、最多 16 MiB 的有效正文：

```yaml
plugins:
  configs:
    cliproxyapi-observer:
      capture-bodies: false
      request-retention: "8h"
      stats-retention-days: 30
      body-retention: "1h"
      max-body-bytes: 262144
      max-body-storage-bytes: 16777216
      compact-interval: "5m"
      compact-min-bytes: 2097152
```

压缩过程中原库与新副本同时存在，应预留至少接近当前库大小的额外磁盘空间，
不要等磁盘耗尽再指望压缩。空间不足时保留原库并报告压缩错误，观测写入也可能丢弃。
需要严格磁盘上界时，还需独立文件系统或目录配额作为最后防线，并接受配额耗尽后
无法继续采集。Docker 必须挂载**整个数据目录**，不能只挂单个数据库文件，
否则无法安全执行同目录原子替换。

## 验证

```bash
make test                                 # vet、Go 单测、Node 22+ UI 测试
make race                                 # Go race detector
bash scripts/test-automerge.sh            # 自动合并安全边界
bash scripts/test-compatibility-report.sh # 兼容性失败上报与恢复
bash scripts/test-package-plugin.sh       # 真实 ZIP 布局与校验和
python3 scripts/test_dependency_policy.py # Go / 官方 Actions 纯版本升级策略
python3 scripts/test_auto_release.py      # 自动发版、竞态与恢复
python3 scripts/test_publish_release.py   # 不可变产物与草稿恢复
CPA_BINARY=/absolute/path/to/cpa make integration
make package                              # 需要 zip，输出 dist/pkg/
```

集成测试加载真正的 CPA 二进制与原生插件，使用本地 mock 上游和假密钥，覆盖认证后的
管理与资源访问、未授权拒绝、正文关联、未知模型统计、流式/非流式推理不变、数据库重启
与热替换回滚。

## 自动维护

维护完全无人值守，但**普通功能合并永不触发自动发布**：

1. 每日兼容性 CI 使用**未改动插件 SDK**测试最新稳定 CPA v8 宿主；失败即红，不用成功码
   掩盖，并建立去重 issue 报告，恢复后自动关闭。
2. Dependabot 每日更新 Go 依赖、每周更新官方 GitHub Actions。
3. Go 更新必须保持模块路径、直接依赖集合及 CPA v8 不变；Actions 更新只能改变现有白名单
   官方 action 的版本引用，不得改变命令、权限、工作流结构或其他内容。所有 PR 跑单测、
   race、管理页测试、自动化安全策略、三个平台原生构建及最低/最新真实宿主测试。
   完整 CI 通过后，只对**精确已验证提交**自动 squash 合并；作者未知、分支异常或越界
   改动一律 fail-closed，留给人工。
4. 合并后显式派发主分支完整 CI，不依赖仓库 auto-merge 设置或被 `GITHUB_TOKEN` 抑制的
   push 事件。
5. 主分支 CI 通过后，控制器核对自上一正式版本以来的每个主线提交均来自**已合并的同仓库
   Dependabot PR** 且满足安全策略，然后自动递增 patch 版本、创建绑定该提交的 tag，并
   显式派发三平台 release 工作流。例如 `v0.1.2 -> v0.1.3`，无需人工打 tag。
6. 每 6 小时自动协调未完成发布：缺少主分支 CI 时仅对合格更新重新派发 CI，有活动任务时
   等待，派发或构建失败时重试同一 tag，绝不移动 tag、越过待发布版本或覆盖已公开 Release。
   发布前核对校验和、归档布局、平台与源码提交，所有资源上传完整后才将草稿公开。

普通功能、配置、权限修改与 CPA v9 迁移**不会**被当作依赖更新自动发布；失败保持旧正式
版本可用，不伪装成功。持续失败或越界更新仍需人工诊断；自动化不保证外部 API、权限、
托管 runner 或未来破坏性变更永不需要人工介入。协调器异常会建立去重的 GitHub issue，
仅包含运行链接，不公开日志或凭据。

人工功能发版仍可对已验证提交推送 `vX.Y.Z` tag；CI artifacts 不是正式 Release。
`registry.json` 不固定版本，新 Release 无需修改插件源。发布不等于部署，已安装插件需在
CPA 插件商店中主动更新。也可手动运行 `auto-release` 协调器；人工 tag 的失败发布可从
主分支重试，例如 `gh workflow run release.yml --ref main -f tag=v0.1.2`，产物仍构建自该
tag。仅自动化自有、尚未公开的草稿允许恢复；人工草稿不动，已公开资源不覆盖。

## 安全与许可证

静态资源公开且不含密钥；观测、设置与正文接口由 CPA 管理认证保护。插件不保存管理密钥、
API key 或浏览器凭据，不把密钥放入 URL、日志或浏览器存储，正文仅在用户确认后展开。
本仓库与测试仅使用假凭据和本地 mock 上游，绝不含真实生产配置或流量。

MIT；SDK 与 bbolt 的归属见 [NOTICE](NOTICE)。本插件为独立实现，非
`cpa-plugin-tokens-statistic` 的 fork。
