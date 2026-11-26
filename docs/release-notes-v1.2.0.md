# Observer v1.2.0 发布说明

## Responses WebSocket

- Observer Key 可握手 `GET /v1/responses`。同一连接每轮重新认证，分别检查模型权限、额度和并发，再按用量独立记账。其他 WebSocket 入口不开放。
- 即使开启采集，Observer 也不保存 WebSocket 请求头或正文。请求详情显示「WebSocket 请求不支持采集」，带 `requestBody` 条件的手工价格仍按轮匹配。Observer 只在内存中保存用于定价的条件结果，不保存请求原文。
- 普通 Codex 上游 WebSocket 可用；同一次执行内追加输入的 Codex `response-steering` 不支持，请在 CPA 保持 `upstream.codex.response-steering` 关闭（默认）。
- Observer Key 额度耗尽时，HTTP 仍为 429，但错误的 `type`、`code` 现在均为 `insufficient_quota`；若客户端匹配旧的 `admission_denied`，请更新判断。在 CPA v8.1.4 上，WebSocket 收到 `status: 429` 的错误帧后断开。被拒的轮次不转发到上游；上游账号的冷却、重试和换号仍由 CPA 管理。

## 兼容与升级

- SDK 锁定 **CLIProxyAPI/v8 v8.1.4**。HTTP 最低仍为 CPA **v8.0.30**；Observer WebSocket 要求 CPA **v8.0.44** 起的宿主 schema 7，插件注册仍使用 schema 6。
- 延续 **schema3 / `observer_features=4`**。从 v1.1.x 升级无需重建数据库，不改历史费用。升级前请停流、正常停止 CPA，并备份整个数据目录和旧插件二进制。较早版本的数据库兼容和回退要求见 [v1.1.0 发布说明](https://github.com/fengwk/cliproxyapi-observer/blob/v1.1.0/docs/release-notes-v1.1.0.md)。
- 不修改 CPA 核心，不替换原生 Key，也不改变路由或重试策略。安装或更新插件后刷新管理页面；发布不会自动部署。
