# Observer

CLIProxyAPI (CPA) 的原生观测插件。独立仓库、独立动态库，**不修改、不 fork CPA 核心**。
插件展示名为 **Observer**，稳定 ID 为 `cliproxyapi-observer`，版本由
`internal/plugin.Version` 注入，仓库自带发布版本 `0.1.0`。

插件只做一件事：在请求生命周期中**只读观测**，把用量与（可选的）请求正文写入本地
bbolt 数据库，并通过 CPA 管理 API 与内置中文页面提供查询。它不代理上游、不改写
请求体/请求头/模型路由/重试/会话，也不参与推理决策。

## 功能

- **请求观测**：注册 CPA 的 `UsagePlugin`、`RequestInterceptor` 与 `ManagementAPI`。
  请求前回调只入队，用量回调记录执行信息；即便存储关闭/已满/不可用或观测数据非法，
  也**不修改请求**、不阻塞推理。
- **用量统计**：只归一化一次并持久化记账质量（accounting quality）与 TPS。按分钟/提供商/模型
  事务性预聚合，汇总直接读取预聚合区间并降采样到 ≤300 个点，不解码全量请求。
- **游标分页**：请求列表使用时间戳+序列号的游标分页，不扫描精确总数；提供商/模型过滤
  只扫描选定区间。
- **静态定价**：按**精确、完整**的模型 ID 配置 USD/百万 token 单价（输入/输出/读缓存/
  写缓存）。
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
- **脱敏**：对敏感 JSON 字段名（`authorization`、`api_key`、`api-key`、`password`、
  `secret`、`access_token`、`refresh_token`，大小写不敏感）做脱敏，正常 prompt/代码
  字符串保留。不保存请求头、认证对象、`Failure.Body` 或响应内容。
- **无凭据泄漏**：错误信息不返回数据库路径或请求载荷；管理路由响应带
  `Cache-Control: no-store` 与 `nosniff`；内嵌页面 CSP 仅允许同源，禁止内联代码与
  任意站点嵌入。

24 小时限制是插件的访问期限与逻辑删除策略，**不等同于磁盘安全擦除**。bbolt 删除后
会复用空闲页，数据库文件不会自动缩小；正文容量限制的是有效正文载荷，不是整个
数据库的物理文件大小。启用采集时，请同时保护数据目录、磁盘与备份，并按敏感数据
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
```

### 3. 打开插件页

管理面板左侧「插件」分组点击 **Observer**，或直接打开插件资源地址：

```text
http://127.0.0.1:8317/v0/resource/plugins/cliproxyapi-observer/ui
```

页面仅允许同源管理面板 iframe 嵌入，并跟随父级主题；请勿移除 CSP 或允许任意站点嵌入。
连接时输入 CPA **管理密钥**（仅存内存、只作请求头）。页面提供周期 24h/7d/30d、
模型/提供商过滤，概览、趋势、分组表、游标请求表，以及需二次确认才展开的正文详情。

### 4. 使用管理 API

均需 CPA 管理密钥，且 `/v0/management` 与 `/v8/management` 两个前缀都可用：

```text
GET /v0/management/plugins/cliproxyapi-observer/summary
GET /v0/management/plugins/cliproxyapi-observer/requests
GET /v0/management/plugins/cliproxyapi-observer/body?request_id=<id>
GET /v0/management/plugins/cliproxyapi-observer/settings
GET /v0/management/plugins/cliproxyapi-observer/health
```

`from`/`to` 为 RFC3339，默认最近 24 小时；`requests` 的 `limit` 取值 1..100（默认 50）
并使用 `cursor`。正文缺失、过期或采集关闭时返回 404；畸形参数、越界保留期或非法游标
返回 400。`settings` 返回 `capture_bodies`、`body_retention_seconds`、
`request_retention_seconds`、`stats_retention_days`、`max_body_bytes`、
`max_body_storage_bytes`；`health` 来自运行状态。

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
| `prices` | 空 | 精确完整模型 ID 到 USD/百万 token 单价的映射 |

`prices` 每项包含 `input`、`output`、`cache-read`、`cache-creation`。定价为**静态配置**，
不再向上游查询；历史成本在记录时快照，不会因之后改配置而回算。

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
