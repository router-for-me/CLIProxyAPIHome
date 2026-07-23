# 严格凭证并发限制部署

[English](README.md)

本运行手册用于部署由 Home 管理的严格凭证并发限制。限制由 Home 数据库中的策略和权威 `admitted_in_flight` 计数器执行。启用任何凭证策略前必须遵循本手册。

## 支持的拓扑和前置条件

- SQLite 部署仅在**一个 active Home** 时支持严格限制。任何 limit 可能处于 active 状态时，不得让多个 Home 进程连接同一个 SQLite 数据库。
- 多个 active Home 的部署必须使用 PostgreSQL。SQLite cluster mode 不是受支持的严格限流拓扑。
- 每个 Home 进程和每个 CPA node 都必须使用独立的 mTLS certificate 和 private key。不得在节点间复制 certificate，也不得在替换节点后复用 identity。certificate identity 是 membership 和 capability attribution 的一部分。
- 每个 live Home 的 Management API 必须可访问。启用策略前，经认证的 `GET /v0/management/capabilities` 响应必须包含 `capabilities.credential_concurrency_limits_v2: true`。
- 使用 `GET /v0/management/topology` 和 Home/CPA membership 记录识别所有 live 或 stale incarnation。不能因为 dashboard 未显示旧进程就认定它已停止。

## 发布步骤

必须按顺序执行以下步骤。policy 指 `max_in_flight` 或 `max_in_flight_by_model` 中任一项处于 active 状态。

1. **移除 policy activation。** 保持所有 credential policy 为 unlimited：删除 total 和 per-model limit，或不提供 policy 字段。此阶段不得启用 limit。
2. **升级 Home。** 升级并重启连接目标数据库的全部 Home。SQLite 只能保留一个 active Home；PostgreSQL 必须完成所有 active Home 的升级。确认旧 Home 进程已停止，且其 membership/incarnation 不再 live。每个替换后的 Home 必须使用独立 certificate。
3. **升级并验证 CPA。** 升级全部 CPA，使其重新加载 Home synthesized config。确认每个 live CPA membership 都使用 protocol version `1`。对**每个** live Home 查询 `GET /v0/management/capabilities`，确认 `credential_concurrency_limits_v2` 为 `true`。必须处理或移除所有 legacy CPA membership，以及每个旧的或缺少 capability 的 Home incarnation；不得等待它们自然恢复。
4. **启用 policy。** 只有前三步全部通过后，才能使用 Management policy API 写入目标 limit。先进行小范围 policy 变更，验证 policy response 和权威 `admitted_in_flight` state，再继续发布。

## 混合版本和故障处理

该部署采用 fail-closed。只要仍有旧 Home、缺少 concurrency capability 的 Home，或 protocol 不是 `1` 的 CPA membership 处于 live 状态，active policy 就不受支持。不得通过直接修改数据库 policy、共享 certificate，或允许 legacy 进程重新连接来绕过检查。

如果 activation 后发现旧 Home 或 legacy CPA，必须立即将其移出服务，并移除或 fence 其 live membership/incarnation，然后才能修改或继续使用 policy。如果拓扑为 SQLite 且存在多个 active Home，必须先将其缩减为一个 active Home 再 activation。如果部署无法满足这些条件，必须移除所有 limit，并从步骤 1 重新发布。

Home 会为 CPA node synthesize limiter config。本地 CPA limiter override 不会改变 Home-authoritative policy enforcement。

## 历史表维护

运行时不读取历史 `in_flight_lease` 表，migration 也绝不会自动删除它。清理该表与 policy activation 无关。

只有 DBA 在确认没有任何 prior deployment 仍依赖该表、已完成 backup 且已批准 maintenance change 后，才能执行以下语句：

```sql
DROP TABLE IF EXISTS in_flight_lease;
```

不得将该语句放入 application migration 或普通 deployment script。
