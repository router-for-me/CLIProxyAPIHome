# CLIProxyAPIHome Cluster Management API 文档

本文档描述 CLIProxyAPIHome 当前 DB-backed Management API。Home 启动时会初始化 runtime database，并注册 Home runtime 使用的 database-backed management route set。

基础路径：

```text
http://<host>:<port>/v0/management
```

可选管理面板：

```text
GET /
GET /index.html
GET /management.html
GET /user.html
GET /assets/*
```

Panel assets 会在构建时内嵌到二进制中。

Home 示例端口通常为 `8327`。实际监听地址来自 runtime config、`cluster.yaml` 或 `-addr` 的最终值。

## Runtime 模型

Home management state 存储在 database-backed cluster repository 中。如果存在 `cluster.yaml`，repository 使用其中配置的后端，例如 PostgreSQL 或 SQLite。如果不存在 cluster config，Home 仍会打开本地 SQLite runtime database，并使用同一套 DB-backed management handlers。

下面的 route 清单是 `cmd/home` 通过 `WithDatabaseManagement` 注册的 database-backed route set。

## 认证

所有 `/v0/management/*` route 都需要 management key。

支持的请求头：

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <MANAGEMENT_KEY>` 或原始 `<MANAGEMENT_KEY>` |
| `X-Management-Key` | `<MANAGEMENT_KEY>` |

访问规则：

| 规则 | 行为 |
| --- | --- |
| 本地请求 | 仍需要有效 management key。 |
| 远程请求 | 需要启用远程管理，例如 `remote-management.allow-remote: true`，或者内部 override。 |
| API 未启用 | 未设置 `remote-management.secret-key` 且未设置 `MANAGEMENT_PASSWORD` 时，Management API route 通常返回 `404`。 |
| 失败封禁 | 同一客户端 IP 连续 5 次失败后封禁 30 分钟；封禁期间即使 key 正确也会失败。 |

常见认证错误：

```json
{ "error": "missing management key" }
{ "error": "invalid management key" }
{ "error": "remote management disabled" }
{ "error": "remote management key not set" }
{ "error": "IP banned due to too many failed attempts. Try again in 29m59s" }
```

Home 管理接口会额外写入以下响应头：

| Header | 说明 |
| --- | --- |
| `x-cpa-home-version` | Home 构建版本。 |
| `x-cpa-home-commit` | Home 构建 commit。 |
| `x-cpa-home-build-date` | Home 构建日期。 |
| `X-CPA-SUPPORT-PLUGIN` | `1` 表示当前二进制在启用 CGO 的情况下构建；否则为 `0`。语义与 CPA 管理接口一致。 |

## 通用响应

多数写入接口成功时返回：

```json
{ "status": "ok" }
```

完整配置替换成功时返回：

```json
{ "ok": true, "changed": ["config"] }
```

DB-backed handler 通常同时返回机器可读 `error` 和可读 `message`：

```json
{ "error": "invalid body", "message": "username is required" }
```

其他常见错误结构：

```json
{ "error": "invalid body" }
{ "error": "invalid_config", "message": "validation detail" }
```

## 已注册 Routes

以下清单来自 `internal/managementhttp/server.go` 为 `cmd/home` 构建的最终 Home route registry。

| Method | Path |
| --- | --- |
| `GET` | `/anthropic-auth-url` |
| `GET` | `/antigravity-auth-url` |
| `POST` | `/api-call` |
| `GET` | `/api-key-usage` |
| `GET` | `/capabilities` |
| `GET` | `/quota/credentials` |
| `GET` | `/quota/credentials/:credential_id` |
| `DELETE` | `/api-keys` |
| `GET` | `/api-keys` |
| `PATCH` | `/api-keys` |
| `POST` | `/api-keys` |
| `PUT` | `/api-keys` |
| `GET` | `/billing/overview` |
| `GET` | `/billing/charges` |
| `GET` | `/billing/balance-records` |
| `POST` | `/billing/balance-records/recharge` |
| `POST` | `/billing/balance-records/deduct` |
| `GET` | `/billing/model-prices` |
| `POST` | `/billing/model-prices` |
| `PATCH` | `/billing/model-prices/:id` |
| `DELETE` | `/billing/model-prices/:id` |
| `POST` | `/billing/model-prices/import/preview` |
| `POST` | `/billing/model-prices/import/apply` |
| `GET` | `/billing/model-prices/import/operations/:id` |
| `GET` | `/billing/settings` |
| `PATCH` | `/billing/settings` |
| `GET` | `/billing/settings/diagnostics` |
| `GET` | `/usage/overview` |
| `GET` | `/usage/records` |
| `GET` | `/usage/records/:id` |
| `GET` | `/usage/aggregates` |
| `GET` | `/usage/export` |
| `GET` | `/usage/realtime` |
| `GET` | `/usage/health/providers` |
| `GET` | `/usage/health/credentials` |
| `GET` | `/request-events` |
| `GET` | `/request-events/export` |
| `GET` | `/request-events/filter-options` |
| `GET` | `/request-events/:id` |
| `GET` | `/request-logs` |
| `GET` | `/proxy/proxy-pools` |
| `POST` | `/proxy/proxy-pools` |
| `PATCH` | `/proxy/proxy-pools/:id` |
| `DELETE` | `/proxy/proxy-pools/:id` |
| `POST` | `/proxy/proxy-pools/:id/test` |
| `DELETE` | `/auth-files` |
| `GET` | `/auth-files` |
| `POST` | `/auth-files` |
| `GET` | `/auth-files/download` |
| `PATCH` | `/auth-files/fields` |
| `GET` | `/auth-files/models` |
| `PATCH` | `/auth-files/status` |
| `POST` | `/certificates/clients` |
| `GET` | `/channel-group-details` |
| `POST` | `/channel-group-details` |
| `DELETE` | `/channel-group-details/:id` |
| `GET` | `/channel-group-details/:id` |
| `PATCH` | `/channel-group-details/:id` |
| `PUT` | `/channel-group-details/:id` |
| `GET` | `/channel-groups` |
| `POST` | `/channel-groups` |
| `DELETE` | `/channel-groups/:id` |
| `GET` | `/channel-groups/:id` |
| `PATCH` | `/channel-groups/:id` |
| `PUT` | `/channel-groups/:id` |
| `DELETE` | `/claude-api-key` |
| `GET` | `/claude-api-key` |
| `PATCH` | `/claude-api-key` |
| `PUT` | `/claude-api-key` |
| `DELETE` | `/codex-api-key` |
| `GET` | `/codex-api-key` |
| `PATCH` | `/codex-api-key` |
| `PUT` | `/codex-api-key` |
| `GET` | `/codex-auth-url` |
| `GET` | `/config` |
| `GET` | `/config.yaml` |
| `PUT` | `/config.yaml` |
| `GET` | `/debug` |
| `PATCH` | `/debug` |
| `PUT` | `/debug` |
| `GET` | `/error-logs-max-files` |
| `PATCH` | `/error-logs-max-files` |
| `PUT` | `/error-logs-max-files` |
| `GET` | `/force-model-prefix` |
| `PATCH` | `/force-model-prefix` |
| `PUT` | `/force-model-prefix` |
| `DELETE` | `/gemini-api-key` |
| `GET` | `/gemini-api-key` |
| `PATCH` | `/gemini-api-key` |
| `PUT` | `/gemini-api-key` |
| `GET` | `/get-auth-status` |
| `GET` | `/kimi-auth-url` |
| `GET` | `/latest-version` |
| `GET` | `/logging-to-file` |
| `PATCH` | `/logging-to-file` |
| `PUT` | `/logging-to-file` |
| `DELETE` | `/logs` |
| `GET` | `/logs` |
| `GET` | `/logs-max-total-size-mb` |
| `PATCH` | `/logs-max-total-size-mb` |
| `PUT` | `/logs-max-total-size-mb` |
| `GET` | `/max-retry-interval` |
| `PATCH` | `/max-retry-interval` |
| `PUT` | `/max-retry-interval` |
| `GET` | `/model-definitions/:channel` |
| `GET` | `/model-group-details` |
| `POST` | `/model-group-details` |
| `DELETE` | `/model-group-details/:id` |
| `GET` | `/model-group-details/:id` |
| `PATCH` | `/model-group-details/:id` |
| `PUT` | `/model-group-details/:id` |
| `GET` | `/model-groups` |
| `POST` | `/model-groups` |
| `DELETE` | `/model-groups/:id` |
| `GET` | `/model-groups/:id` |
| `PATCH` | `/model-groups/:id` |
| `PUT` | `/model-groups/:id` |
| `GET` | `/models` |
| `GET` | `/nodes` |
| `POST` | `/oauth-callback` |
| `DELETE` | `/oauth-excluded-models` |
| `GET` | `/oauth-excluded-models` |
| `PATCH` | `/oauth-excluded-models` |
| `PUT` | `/oauth-excluded-models` |
| `DELETE` | `/oauth-model-alias` |
| `GET` | `/oauth-model-alias` |
| `PATCH` | `/oauth-model-alias` |
| `PUT` | `/oauth-model-alias` |
| `DELETE` | `/openai-compatibility` |
| `GET` | `/openai-compatibility` |
| `PATCH` | `/openai-compatibility` |
| `PUT` | `/openai-compatibility` |
| `DELETE` | `/payload` |
| `GET` | `/payload` |
| `PATCH` | `/payload` |
| `PUT` | `/payload` |
| `GET` | `/plugins` |
| `GET` | `/plugin-store` |
| `POST` | `/plugin-store/:id/install` |
| `POST` | `/plugin-store/:id/uninstall` |
| `GET` | `/plugin-store-auth` |
| `POST` | `/plugin-store-auth` |
| `GET` | `/plugin-store-auth/:id` |
| `PATCH` | `/plugin-store-auth/:id` |
| `DELETE` | `/plugin-store-auth/:id` |
| `DELETE` | `/proxy-url` |
| `GET` | `/proxy-url` |
| `PATCH` | `/proxy-url` |
| `PUT` | `/proxy-url` |
| `GET` | `/quota-exceeded/switch-preview-model` |
| `PATCH` | `/quota-exceeded/switch-preview-model` |
| `PUT` | `/quota-exceeded/switch-preview-model` |
| `GET` | `/quota-exceeded/switch-project` |
| `PATCH` | `/quota-exceeded/switch-project` |
| `PUT` | `/quota-exceeded/switch-project` |
| `GET` | `/request-error-logs` |
| `GET` | `/request-error-logs/:name` |
| `GET` | `/request-log` |
| `PATCH` | `/request-log` |
| `PUT` | `/request-log` |
| `GET` | `/request-log-by-id/:id` |
| `GET` | `/request-retry` |
| `PATCH` | `/request-retry` |
| `PUT` | `/request-retry` |
| `GET` | `/routing/strategy` |
| `PATCH` | `/routing/strategy` |
| `PUT` | `/routing/strategy` |
| `GET` | `/topology` |
| `GET` | `/usage-queue` |
| `GET` | `/usage-statistics-enabled` |
| `PATCH` | `/usage-statistics-enabled` |
| `PUT` | `/usage-statistics-enabled` |
| `GET` | `/users` |
| `POST` | `/users` |
| `DELETE` | `/users/:id` |
| `GET` | `/users/:id/period-limits` |
| `POST` | `/users/:id/period-limits/reset` |
| `GET` | `/users/:id` |
| `PATCH` | `/users/:id` |
| `PUT` | `/users/:id` |
| `DELETE` | `/vertex-api-key` |
| `GET` | `/vertex-api-key` |
| `PATCH` | `/vertex-api-key` |
| `PUT` | `/vertex-api-key` |
| `POST` | `/vertex/import` |
| `DELETE` | `/xai-api-key` |
| `GET` | `/xai-api-key` |
| `PATCH` | `/xai-api-key` |
| `PUT` | `/xai-api-key` |
| `GET` | `/xai-auth-url` |

## 配置接口

### GET `/config`

返回当前 runtime config JSON。

输入：无。

输出示例：

```json
{
  "proxy-url": "http://127.0.0.1:7890",
  "disable-image-generation": false,
  "force-model-prefix": false,
  "request-log": false,
  "api-keys": ["client-key"],
  "passthrough-headers": false,
  "streaming": {
    "keepalive-seconds": 0,
    "bootstrap-retries": 0
  },
  "nonstream-keepalive-interval": 0,
  "tls": {
    "enable": false,
    "cert": "",
    "key": ""
  },
  "debug": false,
  "pprof": {
    "enable": false,
    "addr": "127.0.0.1:8316"
  },
  "commercial-mode": false,
  "logging-to-file": false,
  "logs-max-total-size-mb": 0,
  "error-logs-max-files": 10,
  "usage-statistics-enabled": false,
  "redis-usage-queue-retention-seconds": 60,
  "disable-cooling": false,
  "auth-auto-refresh-workers": 0,
  "request-retry": 0,
  "max-retry-credentials": 0,
  "max-retry-interval": 0,
  "quota-exceeded": {
    "switch-project": false,
    "switch-preview-model": false,
    "antigravity-credits": false
  },
  "routing": {
    "strategy": "round-robin",
    "claude-code-session-affinity": false,
    "session-affinity": false,
    "session-affinity-ttl": "1h"
  },
  "antigravity-signature-cache-enabled": true,
  "antigravity-signature-bypass-strict": false,
  "gemini-api-key": [],
  "codex-api-key": [],
  "xai-api-key": [],
  "codex-header-defaults": {
    "user-agent": "",
    "beta-features": ""
  },
  "claude-api-key": [],
  "claude-header-defaults": {
    "user-agent": "",
    "package-version": "",
    "runtime-version": "",
    "os": "",
    "arch": "",
    "timeout": "",
    "stabilize-device-profile": true
  },
  "openai-compatibility": [],
  "vertex-api-key": [],
  "oauth-excluded-models": {
    "claude": ["model-id"]
  },
  "oauth-model-alias": {
    "claude": [
      { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
    ]
  },
  "payload": {
    "default": [],
    "default-raw": [],
    "override": [],
    "override-raw": [],
    "filter": []
  }
}
```

带 `json:"-"` 的字段不会出现在响应中。Home 会隐藏 `host`、`port`、`allow-host`、`remote-management` 和 `auth-dir`。

### GET `/config.yaml`

返回当前 YAML 配置。

输入：无。

响应 Content-Type：

```text
application/yaml; charset=utf-8
```

响应会从持久化的 config snapshot 重新生成 YAML，因此不会保留原始 YAML 注释和格式。

### PUT `/config.yaml`

替换完整配置。

输入：请求体为完整 YAML 文档。

Home 会把非 credential roots 持久化到 config snapshot。上传 YAML 中包含的 credential roots 会同步到 DB-backed auth 记录；未提交的 credential roots 会保持不变。如需清空某类 provider-key 记录，请提交该 credential root 的空列表：

```text
auth-dir
gemini-api-key
vertex-api-key
codex-api-key
xai-api-key
claude-api-key
openai-compatibility
```

`auth-dir` 仍然只作为 import/export 路径处理，不会持久化到运行时 config snapshot。

输出示例：

```json
{ "ok": true, "changed": ["config", "auth"] }
```

### 简单配置 Leaf Routes

这些接口会写入 cluster repository 中对应 config root，并 reload Home runtime。

| Method | Path | 输入 | 输出 |
| --- | --- | --- | --- |
| `GET` | `/debug` | 无 | `{ "debug": boolean }` |
| `PUT/PATCH` | `/debug` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/logging-to-file` | 无 | `{ "logging-to-file": boolean }` |
| `PUT/PATCH` | `/logging-to-file` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/logs-max-total-size-mb` | 无 | `{ "logs-max-total-size-mb": number }` |
| `PUT/PATCH` | `/logs-max-total-size-mb` | `{ "value": number }`；负数保存为 `0` | `{ "status": "ok" }` |
| `GET` | `/error-logs-max-files` | 无 | `{ "error-logs-max-files": number }` |
| `PUT/PATCH` | `/error-logs-max-files` | `{ "value": number }`；负数保存为 `10` | `{ "status": "ok" }` |
| `GET` | `/usage-statistics-enabled` | 无 | `{ "usage-statistics-enabled": boolean }` |
| `PUT/PATCH` | `/usage-statistics-enabled` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/proxy-url` | 无 | `{ "proxy-url": string }` |
| `PUT/PATCH` | `/proxy-url` | `{ "value": string }` | `{ "status": "ok" }` |
| `DELETE` | `/proxy-url` | 无 | `{ "status": "ok" }` |
| `GET` | `/request-log` | 无 | `{ "request-log": boolean }` |
| `PUT/PATCH` | `/request-log` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/request-retry` | 无 | `{ "request-retry": number }` |
| `PUT/PATCH` | `/request-retry` | `{ "value": number }` | `{ "status": "ok" }` |
| `GET` | `/max-retry-interval` | 无 | `{ "max-retry-interval": number }` |
| `PUT/PATCH` | `/max-retry-interval` | `{ "value": number }` | `{ "status": "ok" }` |
| `GET` | `/force-model-prefix` | 无 | `{ "force-model-prefix": boolean }` |
| `PUT/PATCH` | `/force-model-prefix` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/routing/strategy` | 无 | `{ "strategy": "round-robin" }` 或 `{ "strategy": "fill-first" }` |
| `PUT/PATCH` | `/routing/strategy` | `{ "value": "round-robin" }`、`roundrobin`、`rr`、`fill-first`、`fillfirst`、`ff` | `{ "status": "ok" }` |
| `GET` | `/quota-exceeded/switch-project` | 无 | `{ "switch-project": boolean }` |
| `PUT/PATCH` | `/quota-exceeded/switch-project` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/quota-exceeded/switch-preview-model` | 无 | `{ "switch-preview-model": boolean }` |
| `PUT/PATCH` | `/quota-exceeded/switch-preview-model` | `{ "value": boolean }` | `{ "status": "ok" }` |

### `/payload` Config Root

`GET /payload` 输出：

```json
{
  "payload": {
    "default": [
      {
        "models": [
          {
            "name": "gpt-*",
            "protocol": "responses",
            "from-protocol": "openai",
            "headers": {
              "X-Client-Tier": "tenant-*"
            },
            "match": [{ "metadata.client": "codex" }],
            "not-match": [{ "metadata.mode": "dev" }],
            "exist": ["tools.#(type==\"web_search\").type"],
            "not-exist": ["metadata.disable_payload"]
          }
        ],
        "params": { "reasoning.effort": "high" }
      }
    ],
    "default-raw": [],
    "override": [],
    "override-raw": [],
    "filter": [
      {
        "models": [{ "name": "*", "protocol": "responses" }],
        "params": ["metadata.debug"]
      }
    ]
  }
}
```

`GET /payload` 返回完整持久化 payload root，包括旧前端暂不识别的高级 model matcher 字段。

`PUT /payload` 接受原始 payload object、`{ "value": <payload> }` 或 `{ "payload": <payload> }`。它会替换完整 `payload` root，并校验完整 schema，不会静默丢弃高级 matcher 字段。

`PATCH /payload` 接受相同 body 形态，并对现有 `payload` root 应用 object merge-patch 语义：提交的 object 字段会递归合并，`null` 删除字段，array 作为整体替换，patch 中未出现的 sibling 字段会保留。这样前端只更新 `filter` 等单个 section 时，不会删除 `default`、`override` 或高级 matcher 字段。

`DELETE /payload` 从 config snapshot 删除该 root。

写入成功返回：

```json
{ "status": "ok" }
```

## 节点、版本和证书

### GET `/nodes`

列出当前连接到 Home 集群的 CPA 节点。多 Home 节点共享数据库时，接口返回所有 live Home 上报的 CPA 连接快照，而不只返回当前处理请求的 Home 进程内连接。

输入：无。

输出示例：

```json
{
  "plugin_report_required": true,
  "plugin_report_statuses": [
    {
      "schema_version": 1,
      "task": "plugin-sync",
      "node_type": "cpa",
      "node_id": "node-1",
      "client_ip": "10.0.0.12",
      "status": "success",
      "phase": "load",
      "ok": true,
      "updated_at": "2026-05-27T10:29:59Z",
      "platform": { "goos": "linux", "goarch": "amd64" },
      "plugins": [
        { "id": "sample-provider", "install_status": "installed", "load_status": "loaded" }
      ]
    }
  ],
  "nodes": [
    {
      "ip": "10.0.0.12",
      "node_id": "node-1",
      "connected_time": "2026-05-27T10:30:00Z",
      "last_seen_at": "2026-05-27T10:30:02Z",
      "client_count": 1,
      "healthy": true,
      "home_id": "10.0.0.10:8327",
      "home_ip": "10.0.0.10",
      "home_port": 8327,
      "plugin_report_state": "reported_ok",
      "plugin_report_statuses": [
        {
          "node_id": "node-1",
          "status": "success",
          "phase": "load",
          "ok": true
        }
      ]
    }
  ]
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `nodes` | array | 当前活跃 CPA 节点列表，聚合自所有 live Home 节点写入共享数据库的连接快照。 |
| `plugin_report_required` | boolean | 当前 Home 配置是否期望 CPA 上报插件状态；至少一个已启用插件带有固定的 store manifest 时为 `true`。 |
| `plugin_report_statuses` | array | 共享数据库中保存的最新插件上报，按上报节点和上报元数据分组；单插件删除上报可以和其他插件保留的状态行同时存在。它们会一直保留到节点再次上报或被显式清理，不按 TTL 自动过期。这是 CPA 自报告的观测信息，不是强可信安装事实。 |
| `nodes[].node_id` | string | 从 Home 客户端证书得到的 CPA node ID。 |
| `nodes[].ip` | string | 节点 IP 地址。 |
| `nodes[].connected_time` | string | 当前活跃节点条目的首次连接时间。 |
| `nodes[].last_seen_at` | string | 该 CPA 节点连接快照最近一次由对应 Home 节点刷新到共享数据库的时间。 |
| `nodes[].client_count` | integer | 当前 IP 下活跃 RESP 订阅连接数。 |
| `nodes[].healthy` | boolean | 节点是否存在活跃 RESP 配置订阅连接；插件上报不会直接让该字段变为不健康。 |
| `nodes[].home_id` | string | 当前服务此 CPA 的 Home 节点身份，格式为 `home_ip:home_port`。 |
| `nodes[].home_ip` | string | 当前服务此 CPA 的 Home 节点 IP 或集群广播身份。 |
| `nodes[].home_port` | integer | 当前服务此 CPA 的 Home 节点 RESP/cluster 端口。 |
| `nodes[].plugin_report_state` | string | 当前已配置插件的观测状态：`not_required`、`missing_report`、`reported_partial`、`reported_failed` 或 `reported_ok`；当前不需要的插件上报失败不会让该状态变为失败。 |
| `nodes[].plugin_report_statuses` | array | 关联到当前活跃节点的插件上报，优先按 node ID 匹配，缺失时按 IP fallback。 |
| `plugin_report_statuses[].node_type` | string | 上报节点类型；CPA 节点上报为 `cpa`，`home` 预留给 Home 节点上报。 |
| `plugin_report_statuses[].node_id` | string | 从 CPA Home 客户端证书得到的节点 ID。 |
| `plugin_report_statuses[].status` | string | 此上报分组的插件任务状态，当前为 `success` 或 `failed`。 |
| `plugin_report_statuses[].phase` | string | 此上报分组的任务阶段，例如 `install`、`load` 或 `delete`。 |
| `plugin_report_statuses[].ok` | boolean | 节点自报告的任务是否成功。 |
| `plugin_report_statuses[].plugins` | array | 属于此上报分组的每个插件安装/加载/删除结果。 |

### GET `/topology`

返回数据库态 Home runtime 的 Home + CPA 拓扑快照。与 `GET /nodes` 不同，此接口是面向拓扑的集群视角：Home 节点来自共享 cluster heartbeat 表，CPA 节点来自每个 Home 写入共享数据库的 CPA 快照表，包括会按已配置 heartbeat timeout 分类的 stale 快照。

输入：无。

输出示例：

```json
{
  "summary": {
    "home_count": 1,
    "healthy_home_count": 1,
    "stale_home_count": 0,
    "unknown_home_count": 0,
    "cpa_count": 1,
    "healthy_cpa_count": 1,
    "stale_cpa_count": 0,
    "unknown_cpa_count": 0,
    "plugin_attention_count": 0,
    "attention_count": 0,
    "missing_master": false,
    "stale_after_seconds": 20,
    "retention_after_seconds": 120
  },
  "management": {
    "home_id": "10.0.0.10:8327",
    "home_ip": "10.0.0.10",
    "home_port": 8327
  },
  "master": {
    "id": "10.0.0.10:8327",
    "ip": "10.0.0.10",
    "port": 8327,
    "role": "master",
    "is_master": true,
    "reported_master": true,
    "health": "healthy",
    "healthy": true,
    "client_count": 1,
    "started_at": "2026-05-27T10:00:00Z",
    "last_seen_at": "2026-05-27T10:30:02Z",
    "cpa_count": 1,
    "healthy_cpa_count": 1,
    "stale_cpa_count": 0,
    "unknown_cpa_count": 0
  },
  "homes": [
    {
      "id": "10.0.0.10:8327",
      "ip": "10.0.0.10",
      "port": 8327,
      "role": "master",
      "is_master": true,
      "reported_master": true,
      "health": "healthy",
      "healthy": true,
      "client_count": 1,
      "started_at": "2026-05-27T10:00:00Z",
      "last_seen_at": "2026-05-27T10:30:02Z",
      "cpa_count": 1,
      "healthy_cpa_count": 1,
      "stale_cpa_count": 0,
      "unknown_cpa_count": 0
    }
  ],
  "cpas": [
    {
      "node_id": "node-1",
      "ip": "192.0.2.10",
      "connected_time": "2026-05-27T10:05:00Z",
      "last_seen_at": "2026-05-27T10:30:02Z",
      "client_count": 1,
      "healthy": true,
      "health": "healthy",
      "home_id": "10.0.0.10:8327",
      "home_ip": "10.0.0.10",
      "home_port": 8327,
      "plugin_report_state": "reported_ok",
      "plugin_report_statuses": []
    }
  ]
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `summary.home_count` | integer | 共享 cluster 表中已知的 Home 节点数量。 |
| `summary.healthy_home_count` | integer | `last_seen_at` 未超过 `stale_after_seconds` 的 Home 节点数量。 |
| `summary.stale_home_count` | integer | 数据库已知但已超过 stale cutoff 的 Home 节点数量。 |
| `summary.unknown_home_count` | integer | 因身份或 heartbeat 数据缺失而无法判断健康状态的 Home 节点数量。 |
| `summary.cpa_count` | integer | 集群已知的 CPA 节点快照数量。 |
| `summary.healthy_cpa_count` | integer | 连接到健康 Home 且自身未超过 stale cutoff 的 CPA 快照数量。 |
| `summary.stale_cpa_count` | integer | Home 或自身心跳已过期的 CPA 快照数量。 |
| `summary.unknown_cpa_count` | integer | 无法判断服务 Home 身份或健康状态的 CPA 快照数量。 |
| `summary.plugin_attention_count` | integer | 插件上报为必需时，插件状态缺失、部分上报或失败的 CPA 节点数量。 |
| `summary.attention_count` | integer | 合计关注数量：stale/unknown Home 节点、需要关注的 CPA 节点各计一次，再加缺失 master。 |
| `summary.missing_master` | boolean | 当前是否无法选出健康 Home master。 |
| `summary.stale_after_seconds` | integer | 拓扑健康判断使用的 heartbeat timeout。 |
| `summary.retention_after_seconds` | integer | 拓扑快照保留窗口；早于该窗口的记录不会出现在 `homes[]` 和 `cpas[]` 中。 |
| `management.home_id` | string | 当前 Management runtime 的 Home 身份，格式为 `home_ip:home_port`。 |
| `management.home_ip` | string | 当前 Management runtime 的 Home IP 或集群广播身份。 |
| `management.home_port` | integer | 当前 Management runtime 的 Home 端口。 |
| `master` | object/null | 当前选出的健康 Home master；没有健康 master 时为 `null`。 |
| `homes[]` | array | Home 节点一等拓扑资源。 |
| `homes[].id` | string | Home 身份，格式为 `ip:port`。 |
| `homes[].ip` | string | Home IP 或集群广播身份。 |
| `homes[].port` | integer | Home cluster/RESP 端口。 |
| `homes[].role` | string | `master`、`follower` 或 `unknown`。 |
| `homes[].is_master` | boolean | 此 Home 是否为当前选出的健康 master；stale Home 不会被标记为当前 master。 |
| `homes[].reported_master` | boolean | 此 Home 最近一次 heartbeat 上报的 master 标记。 |
| `homes[].health` | string | `healthy`、`stale` 或 `unknown`。 |
| `homes[].healthy` | boolean | `homes[].health` 是否为 `healthy`。 |
| `homes[].client_count` | integer | 此 Home 上报的活跃 CPA config subscription 总数。 |
| `homes[].started_at` | string | Home 进程启动时间。 |
| `homes[].last_seen_at` | string | 共享数据库中保存的最后一次 Home heartbeat 时间。 |
| `homes[].cpa_count` | integer | 当前关联到此 Home 的 CPA 快照数量。 |
| `homes[].healthy_cpa_count` | integer | 当前关联到此 Home 的健康 CPA 快照数量。 |
| `homes[].stale_cpa_count` | integer | 当前关联到此 Home 的 stale CPA 快照数量。 |
| `homes[].unknown_cpa_count` | integer | 当前关联到此 Home 的未知健康状态 CPA 快照数量。 |
| `cpas[]` | array | CPA 节点快照，并包含服务它的 Home 身份。 |
| `cpas[].node_id` | string | 从客户端证书得到的 CPA node ID。 |
| `cpas[].ip` | string | 服务它的 Home 观测到的 CPA 节点 IP。 |
| `cpas[].connected_time` | string | 此 CPA 快照在服务它的 Home 上首次观测到活跃连接的时间。 |
| `cpas[].last_seen_at` | string | 服务它的 Home 最近一次刷新此 CPA 快照的时间。 |
| `cpas[].client_count` | integer | 此 CPA 快照代表的活跃 RESP 订阅数。 |
| `cpas[].healthy` | boolean | `cpas[].health` 是否为 `healthy`。 |
| `cpas[].health` | string | `healthy`、`stale` 或 `unknown`。 |
| `cpas[].home_id` | string | 服务此 CPA 的 Home 身份，格式为 `home_ip:home_port`。 |
| `cpas[].home_ip` | string | 服务此 CPA 的 Home IP 或集群广播身份。 |
| `cpas[].home_port` | integer | 服务此 CPA 的 Home cluster/RESP 端口。 |
| `cpas[].plugin_report_state` | string | 与 `nodes[].plugin_report_state` 语义相同。 |
| `cpas[].plugin_report_statuses` | array | 关联到此 CPA 节点的插件上报，优先按 node ID 匹配，缺失时按 IP fallback。 |

### GET `/latest-version`

通过 GitHub release API 获取 CLIProxyAPIHome 最新版本；配置 `proxy-url` 时会使用该代理。

输入：无。

输出示例：

```json
{ "latest-version": "v7.0.0" }
```

常见错误码：

```json
{ "error": "request_create_failed", "message": "detail" }
{ "error": "request_failed", "message": "detail" }
{ "error": "unexpected_status", "message": "status 502: detail" }
{ "error": "decode_failed", "message": "detail" }
{ "error": "invalid_response", "message": "missing release version" }
```

## Plugin Management

### GET `/plugins`

列出 Home 进程可见的插件条目。响应包含 Home 配置中的插件条目，以及 Home 进程已经加载并完成 runtime registration 的插件。通过 store 安装的插件只有在配置中显式允许 Home 加载时才会在 Home 注册，例如 `plugins.configs.<pluginID>.load-in-home: true`。

输入：无。

输出示例：

```json
{
  "plugins_enabled": true,
  "plugins_dir": "plugins",
  "plugins": [
    {
      "id": "sample-provider",
      "path": "",
      "configured": true,
      "registered": true,
      "enabled": true,
      "effective_enabled": true,
      "supports_oauth": true,
      "oauth_provider": "sample-provider",
      "logo": "/v0/resource/plugins/sample-provider/logo.png",
      "config_fields": [],
      "menus": [],
      "metadata": {
        "name": "Sample Provider",
        "version": "0.2.0",
        "author": "author-name",
        "github_repository": "https://github.com/author-name/sample-provider",
        "logo": "/v0/resource/plugins/sample-provider/logo.png",
        "config_fields": []
      }
    }
  ]
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `plugins_enabled` | boolean | 当前全局 `plugins.enabled` 值。 |
| `plugins_dir` | string | Home 和 CPA 节点配置的本地插件产物目录。 |
| `plugins[].configured` | boolean | Home 配置中是否存在 `plugins.configs.<id>`。 |
| `plugins[].registered` | boolean | Home 进程是否已经加载该插件并收到 runtime registration。 |
| `plugins[].effective_enabled` | boolean | 全局 plugins、单插件 enabled、runtime registration 都生效时为 true。 |
| `plugins[].supports_oauth` | boolean | runtime registration 中是否包含 auth provider 登录能力。 |
| `plugins[].oauth_provider` | string | OAuth UI 和 `GET /<provider>-auth-url` 使用的 provider key。 |
| `plugins[].menus` | array | 预留给插件资源菜单。Home 当前不会暴露插件资源路由，因此这里返回空列表。 |
| `plugins[].metadata` | object | runtime registration 返回的插件 metadata，包括展示字段和配置字段描述。 |

## Plugin Store

插件商店接口用于列出 registry 中的插件，并把选中的插件写入数据库驱动的 Home 配置。安装接口写入的是 `plugins.configs.<pluginID>.store` 固定 manifest。GitHub release 安装会固定 repository、version 和 release tag；direct 安装会固定 version 和来源 registry URL，Home-mode CPA 节点随后在应用配置时从该 registry 解析当前平台的 artifact URL 与 SHA-256。通过 store 安装的插件默认不会被 Home 进程下载或加载；只有可信的 provider/auth 插件确实需要在 Home 内运行时，才显式设置 `plugins.configs.<pluginID>.load-in-home: true`。

### GET `/plugin-store`

列出内置官方 registry 和 `plugins.store-sources` 配置的额外 registry。

输入：无。

输出示例：

```json
{
  "plugins_enabled": true,
  "plugins_dir": "plugins",
  "sources": [
    {
      "id": "official",
      "name": "Official",
      "url": "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json"
    }
  ],
  "plugins": [
    {
      "store_id": "official/sample-provider",
      "source_id": "official",
      "source_name": "Official",
      "source_url": "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json",
      "id": "sample-provider",
      "name": "Sample Provider",
      "description": "Adds sample provider support.",
      "author": "author-name",
      "version": "0.2.0",
      "repository": "https://github.com/author-name/sample-provider",
      "install_type": "github-release",
      "auth_required": false,
      "installed": true,
      "installed_version": "0.2.0",
      "configured": true,
      "registered": false,
      "enabled": true,
      "effective_enabled": true,
      "update_available": false
    }
  ]
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `plugins_enabled` | boolean | 当前全局 `plugins.enabled` 值。 |
| `plugins_dir` | string | 各节点本地插件产物目录。 |
| `sources` | array | 本次查询使用的插件 registry 来源。 |
| `source_errors` | array | 部分 registry 查询失败时的来源级错误。 |
| `plugins[].install_type` | string | registry 安装类型，目前为 `github-release` 或 `direct`。 |
| `plugins[].auth_required` | boolean | registry 声明该插件来源可能需要认证。 |
| `plugins[].platforms` | array | direct registry 条目声明的可用平台；GitHub release 条目为空。 |
| `plugins[].installed` | boolean | 当前配置中是否存在该插件的 store manifest。 |
| `plugins[].installed_version` | string | 当前配置 manifest 固定的版本。 |
| `plugins[].enabled` | boolean | `plugins.configs.<id>.enabled` 值。 |
| `plugins[].effective_enabled` | boolean | 全局 plugins 和单插件 enabled 同时为 true 时为 true。 |
| `plugins[].update_available` | boolean | registry 版本高于当前 manifest 版本时为 true。 |

常见错误：

```json
{ "error": "plugin_store_source_invalid", "message": "detail" }
{ "error": "plugin_store_registry_failed", "message": "detail" }
```

### POST `/plugin-store/:id/install`

从 registry 条目安装插件配置 manifest。如果多个来源包含同一插件 ID，传入 `?source=<source_id>` 指定来源。`github-release` 条目默认安装 GitHub 最新 release；传入 `version` 可固定安装指定 release tag，例如 `1.0.3` 或 `v1.0.3`。`direct` 条目会写入 source-backed v2 manifest；如果传入 `version`，必须匹配 registry 条目的顶层版本或 `versions[]` 中的某个版本。

输入 body：可选 JSON。

```json
{ "version": "1.0.3" }
```

Query：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `source` | string | 否 | 插件 ID 在多个 registry 中有歧义时指定来源 ID。 |
| `version` | string | 否 | 要安装的插件版本；支持带或不带前导 `v`。 |

输出示例：

```json
{
  "status": "installed",
  "source_id": "official",
  "source_name": "Official",
  "source_url": "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI-Plugins-Store/main/registry.json",
  "id": "sample-provider",
  "version": "0.2.0",
  "install_type": "github-release",
  "path": "",
  "plugins_enabled": true,
  "restart_required": false
}
```

常见错误：

```json
{ "error": "plugin_not_found", "message": "plugin not found in registry" }
{ "error": "plugin_store_source_required", "message": "multiple plugin store sources contain this plugin id; specify source" }
{ "error": "plugin_release_failed", "message": "detail" }
{ "error": "plugin_release_invalid", "message": "detail" }
{ "error": "plugin_manifest_invalid", "message": "detail" }
{ "error": "invalid_config", "message": "detail" }
```

### POST `/plugin-store/:id/uninstall`

从整个 Home/CPA 集群卸载插件。接口会从共享 Home 配置中移除该插件的 store manifest，并为所有 CPA 节点创建删除任务；活跃 Home 节点在应用配置变化时也会删除本机当前平台的插件文件。

输入 body/query：无。

输出示例：

```json
{
  "status": "uninstalled",
  "id": "sample-provider",
  "configured_removed": true,
  "target_node_type": "all",
  "restart_required": false,
  "task": {
    "id": 12,
    "operation": "delete",
    "plugin_id": "sample-provider",
    "target_node_type": "all"
  }
}
```

常见错误：

```json
{ "error": "invalid_plugin_id", "message": "invalid plugin id" }
{ "error": "plugin_task_create_failed", "message": "detail" }
{ "error": "invalid_config", "message": "detail" }
```

### 插件商店凭证接口

Home 将插件商店凭证加密保存在共享数据库中。Secret 为只写字段：任何响应都不会返回明文凭证或密文。创建、实际更新或删除规则会记录集群事件，供后续下游同步使用。规则按数据库创建顺序匹配，第一条匹配规则生效。`match` 必须是无 userinfo、query 和 fragment 的绝对 HTTPS URL。

请求 body 上限为 64 KiB，必须只包含一个 JSON object，并且不允许未知字段。超出上限返回 `413`，格式错误返回 `400`。

接口：

- `GET /plugin-store-auth`：返回 `200` 和 `{ "items": [...] }`。
- `POST /plugin-store-auth`：创建规则并返回 `201`。
- `GET /plugin-store-auth/:id`：返回单条规则和 `200`。
- `PATCH /plugin-store-auth/:id`：部分更新并返回 `200`；省略或设为 `null` 的 secret 字段保留原值。
- `DELETE /plugin-store-auth/:id`：返回 `200` 和 `{ "status": "ok" }`。

创建示例：

```json
{
  "name": "Private artifacts",
  "match": "https://downloads.example/private/",
  "apply_to": ["artifact"],
  "auth_type": "bearer",
  "token": "write-only-token",
  "enabled": true
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `name` | 是 | 非空显示名称。 |
| `match` | 是 | 绝对 HTTPS 匹配前缀。 |
| `apply_to` | 否 | 可包含 `registry`、`metadata`、`artifact`；空数组表示适用于全部请求类型。 |
| `auth_type` | 否 | `none`（默认）、`bearer`、`basic`、`header` 或 `github-token`。 |
| `token` | 按类型 | `bearer` 和 `github-token` 必填。 |
| `username`、`password` | 按类型 | `basic` 必须同时提供。 |
| `header_name`、`header_value` | 按类型 | `header` 必须同时提供，且名称和值必须是合法 HTTP header 数据。 |
| `enabled` | 否 | 默认为 `true`。 |

响应包含 `id`、`name`、`match`、`apply_to`、`auth_type`、可选的 `header_name`、`enabled`、`version` 和 `credentials_configured`。常见错误包括 `400 invalid_request`、`404 plugin_store_auth_*_failed`、更新冲突时的 `409 plugin_store_auth_*_failed`、`413 invalid_request` 和 `422 plugin_store_auth_invalid`。

### POST `/certificates/clients`

创建 pending client certificate enrollment record，并返回节点完成证书注册所需的 Home JWT。

输入：无。

输出示例：

```json
{
  "id": "cert-uuid",
  "home_jwt": "eyJhbGciOi..."
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | Pending client certificate ID。 |
| `home_jwt` | string | 包含 Home target 信息和 enrollment secret 的注册 JWT。 |

常见错误：

```json
{ "error": "cluster_unavailable", "message": "cluster_unavailable" }
{ "error": "certificate_jwt_target_invalid", "message": "certificate_jwt_target_invalid" }
{ "error": "certificate_create_failed", "message": "detail" }
{ "error": "certificate_jwt_failed", "message": "detail" }
```

## Users

User records 存储在 cluster repository 中。

### GET `/users`

列出用户。

输入：无。

输出示例：

```json
{
  "users": [
    {
      "id": 1,
      "username": "alice",
      "password_set": true,
      "credits": 10.5,
      "credits_unlimited": false,
      "mfa": { "enabled": true },
      "passkey": [{ "id": "credential-id" }],
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/users/:id`

按数字 ID 读取单个用户。

Path 参数：

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | integer | 是 | `user.id`；必须大于 `0`。 |

输出示例：

```json
{
  "user": {
    "id": 1,
    "username": "alice",
    "password_set": true,
    "credits": 10.5,
    "credits_unlimited": false,
    "mfa": { "enabled": true },
    "passkey": [{ "id": "credential-id" }],
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/users`

创建用户。

输入示例：

```json
{
  "username": "alice",
  "password": "plaintext-password",
  "credits": 10.5,
  "mfa": { "enabled": true },
  "passkey": [{ "id": "credential-id" }]
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `username` | string | 是 | 用户名；也接受 `user_name`、`user-name`。 |
| `password` | string | 否 | 非空明文会以 bcrypt hash 存储。已有合法 bcrypt hash 会原样保留，便于迁移。响应不返回密码材料，只返回 `password_set`。 |
| `credits` | number | 否 | 用户点数余额；默认为 `0`。当客户端 API key 绑定到该用户且 credits `<= 0` 时，RESP `RPOP auth` 返回 `user_credits_insufficient`。对于计费工作流，优先使用 `/billing/balance-records/recharge` 和 `/billing/balance-records/deduct`，以便余额变更拥有分类账记录。 |
| `credits_unlimited` | boolean | 否 | 为 `true` 时，派发忽略总 `credits` 余额，计费扣费也不减少余额；周期限额仍生效。默认 `false`。 |
| `timezone` | string | 否 | 自然日/周/月使用的 IANA 时区；默认 `Asia/Shanghai`。 |
| `limit_5h_credits` | number/null | 否 | 5 小时 credits 限额。`null` 清除/关闭。`0` 立即不可用。 |
| `window_mode_5h` | string | 否 | `first_use`（默认；别名 `fixed`）或 `sliding`。 |
| `limit_1d_credits` | number/null | 否 | 1 天 credits 限额。 |
| `window_mode_1d` | string | 否 | `first_use`、`sliding`（别名 `rolling`）或 `calendar`（默认 `first_use`）。 |
| `limit_7d_credits` | number/null | 否 | 7 天 credits 限额。 |
| `window_mode_7d` | string | 否 | `first_use`、`sliding`（别名 `rolling`）或 `calendar`（默认 `first_use`）。 |
| `week_reset_day` | integer | 否 | 自然周起点，`1=周一` .. `7=周日`（默认 `1`）。 |
| `week_reset_hour` | integer | 否 | 自然周起点小时 `0-23`（默认 `0`）。 |
| `limit_30d_credits` | number/null | 否 | 30 天/自然月 credits 限额。 |
| `window_mode_30d` | string | 否 | `first_use`、`sliding`（别名 `rolling`）或 `calendar`（默认 `first_use`）。`calendar` 为自然月。 |
| `mfa` | any valid JSON | 否 | 存入 `user.mfa`。 |
| `passkey` | any valid JSON | 否 | 存入 `user.passkey`。 |

输出：与 `GET /users/:id` 相同。

### PUT/PATCH `/users/:id`

更新用户。`PUT` 和 `PATCH` 当前都是局部更新语义：只修改请求体中出现的字段。

输入示例：

```json
{
  "username": "alice-updated",
  "password": "new-plaintext-password",
  "credits": 20,
  "mfa": { "enabled": false },
  "passkey": []
}
```

所有字段均可选；如果出现 `username`，则不能为空。`credits` 如果出现，会替换用户当前点数余额。对于计费工作流，优先使用 `/billing/balance-records/recharge` 和 `/billing/balance-records/deduct`，以便余额变更拥有分类账记录。
当用户需要无限总余额、但仍受独立周期限额约束时，将 `credits_unlimited` 设为 `true`。

输出：与 `GET /users/:id` 相同。


### GET `/users/:id/period-limits`

返回用户周期限额配置与当前用量。

响应包含 `timezone`、`credits`、`credits_unlimited` 和 `windows[]`：

| 字段 | 说明 |
| --- | --- |
| `id` | `5h`、`1d`、`7d` 或 `30d`。 |
| `enabled` | 已配置限额时为 `true`（`limit != null`）。 |
| `limit` | 窗口 credits 上限；`null` 表示关闭。 |
| `used` | 当前窗口已消费 credits（`SUM(billing_charge.amount)`）。 |
| `remaining` | 启用时为 `max(limit - used, 0)`。 |
| `mode` | 规范值为 `first_use`、`sliding` 或 `calendar`（`calendar` 仅 `1d`/`7d`/`30d`）。别名 `fixed`→`first_use`，`rolling`→`sliding`。 |
| `window_start` / `window_end` / `reset_at` | 活跃窗口边界。 |
| `usage_epoch` | 软重置标记；只统计该时间之后的扣费。 |

`5h` 支持 `first_use`（默认，**首次成功计费扣费**开 5h 窗；兼容别名 `fixed`）或 `sliding`（滚动 5h）。`1d`/`7d`/`30d` 支持 `first_use`（默认，首次计费起算时长；兼容别名 `fixed`）、`sliding`（滚动时长）或 `calendar`（自然日/周/月）。派发探测不会打开 `first_use` 窗口。自然周期使用 `timezone`（默认 `Asia/Shanghai`）。自然周使用 `week_reset_day`（1=周一..7=周日）和 `week_reset_hour`。自然 `30d` 按自然月计算。

产品对齐（Claude Code / Codex）：
- 短窗 `5h` 默认 `first_use`：首次成功计费开启 5 小时会话窗，窗内是 credits 预算（不是“能连续工作 5 小时”）。
- 长窗 `7d`/`30d` 可独立叠加；两层同时生效（AND）。
- 需要自然日/周/月刷新时，对 `1d`/`7d`/`30d` 选 `calendar`。
- 需要“随时间滚动恢复”时选 `sliding`（别名 `rolling`）。

### POST `/users/:id/period-limits/reset`

软重置周期计数，不删除账单历史。

请求体：

```json
{ "windows": ["5h", "1d"], "mode": "counter" }
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `windows` | string[] | 否 | `5h`/`1d`/`7d`/`30d` 子集。空/省略表示全部。 |
| `mode` | string | 否 | `counter`（默认）：对选中窗口设置 `usage_epoch_* = now` 并清除对应 `period_window_start_*`。`window_only`：清除 `period_window_start_*`；对 `sliding`/`calendar` 窗口同时写 `usage_epoch_*`（否则无法真正清零 used）。 |

响应：

```json
{
  "status": "ok",
  "user_id": 1,
  "reset": { "mode": "counter", "windows": ["5h", "1d"], "at": "2026-07-09T12:00:00Z" },
  "limits": { "user_id": 1, "windows": [] }
}
```

周期限额在派发时对用户名下所有 API key 生效（`user_credits_insufficient` 与 `user_period_limit_exceeded`）。当 `credits_unlimited=true` 时跳过总余额检查，但已启用的周期窗口仍会拦截。

执行模型为 **soft limit**：费用在请求完成后才记账，因此允许尾笔/并发 in-flight 请求把 used 顶过 limit；下一笔派发会被拦截。`first_use` 在首次 **billable charge** 开窗，不在 dispatch 探活时开窗。

### DELETE `/users/:id`

软删除用户。

输入：无 body。

输出示例：

```json
{ "status": "ok" }
```

常见错误：

```json
{ "error": "not_found", "message": "record not found" }
{ "error": "invalid body", "message": "username is required" }
```

## 客户端 API Keys

### GET `/api-keys`

返回 Home 接受的客户端 API keys。

输入：无。

输出示例：

```json
{
  "api-keys": ["client-key-1"],
  "items": [
    {
      "id": 1,
      "api_key_id": 1,
      "api-key": "client-key-1",
      "api_key": "client-key-1",
      "user-id": 1,
      "user_id": 1,
      "channels": [1],
      "model_groups": [2]
    }
  ],
  "api_key_entries": [
    {
      "id": 1,
      "api_key_id": 1,
      "api-key": "client-key-1",
      "api_key": "client-key-1",
      "user-id": 1,
      "user_id": 1,
      "channels": [1],
      "model_groups": [2]
    }
  ]
}
```

字段说明：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `api-keys` | array of string | 兼容旧客户端的原始 key 列表。 |
| `items` | array of `APIKeyEntry` | 结构化 API key records。 |
| `api_key_entries` | array of `APIKeyEntry` | `items` 的 alias。 |
| `APIKeyEntry.id` | integer | API key 的稳定数据库主键。 |
| `APIKeyEntry.api_key_id` | integer | `id` 的 alias。 |
| `APIKeyEntry.api-key` | string | 客户端 API key。 |
| `APIKeyEntry.api_key` | string | `api-key` 的 alias。 |
| `APIKeyEntry.user-id` | integer or null | 绑定的 `user.id`；`null` 表示未绑定。 |
| `APIKeyEntry.user_id` | integer or null | `user-id` 的 alias。 |
| `APIKeyEntry.channels` | array of integer | 绑定的 channel group IDs；空数组表示不限制。 |
| `APIKeyEntry.model_groups` | array of integer | 绑定的 model group IDs；空数组表示不限制。 |

### POST `/api-keys`

原子创建一个客户端 API key，不替换现有列表。

```json
{
  "api_key": "client-key-1",
  "user_id": 1,
  "channels": [1],
  "model_groups": [2]
}
```

密钥字段也接受 `api-key`、`key` 或 `value`；`user-id` 和 `model-groups` 作为别名。

新的密钥值会获得新的稳定标识。如果重新添加与历史软删除记录完全相同的密钥值，则恢复原记录并复用原标识。创建已存在的活跃密钥会返回 `409 api_key_exists`。

成功输出：

```json
{
  "api_key": {
    "id": 1,
    "api_key_id": 1,
    "api-key": "client-key-1",
    "api_key": "client-key-1",
    "user-id": 1,
    "user_id": 1,
    "channels": [1],
    "model_groups": [2]
  }
}
```

### PUT `/api-keys`

整体替换客户端 API key 列表。

输入可以是原始字符串数组：

```json
["client-key-1", "client-key-2"]
```

或：

```json
{ "items": ["client-key-1", "client-key-2"] }
```

也接受结构化 entry。Wrapper key 可以是 `items`、`api-keys`、`api_keys` 或 `api_key_entries`：

```json
{
  "api_key_entries": [
    {
      "api_key": "client-key-1",
      "user_id": 1,
      "channels": [1],
      "model_groups": [2]
    }
  ]
}
```

Entry 字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `api_key` | string | 条件必填 | 客户端 API key；也接受 `api-key`、`key`、`value`。 |
| `user_id` | integer | 否 | 绑定的 `user.id`；也接受 `user-id`。 |
| `channels` | array of integer | 否 | Channel group IDs。 |
| `model_groups` | array of integer | 否 | Model group IDs；也接受 `model-groups`。 |

如果 `user_id` 引用不存在的用户，接口返回 `404 user_not_found`。

`PUT` 是显式的完整列表替换操作，采用 last-write-wins 语义。对于此操作，`id` 和 `api_key_id` 仅用于响应，在结构化输入中会被忽略。未改变的密钥值保留原标识，被移除的值会软删除，新值会获得新标识；如果新值恢复了完全相同的软删除记录，则复用原标识。

成功输出：

```json
{ "status": "ok" }
```

### PATCH `/api-keys`

更新一个客户端 API key。优先使用稳定的 `id` / `api_key_id` 定位。旧的 `index`、`old/new` 和原始 key selector 继续作为兼容方式，后端会先将其解析到一条数据库记录，再执行定向更新。使用 `old/new` 时，如果旧值不存在，会原子创建 `new`。此接口还可以更新已有 API key 的 `user_id`、`channels` 和 `model_groups`。

按 ID 更新：

```json
{
  "id": 1,
  "value": {
    "api_key": "new-key",
    "user_id": 1,
    "channels": [1],
    "model_groups": [2]
  }
}
```

仅更新绑定时，可以按 ID 定位而不重复发送密钥值：

```json
{ "api_key_id": 1, "channels": [1] }
```

按下标更新：

```json
{ "index": 0, "value": "new-key" }
```

按 old/new 更新：

```json
{ "old": "old-key", "new": "new-key" }
```

绑定更新：

```json
{
  "api_key": "client-key-1",
  "user_id": 1,
  "channels": [1],
  "model_groups": [2]
}
```

清空 user 绑定：

```json
{ "api_key": "client-key-1", "user_id": 0 }
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | integer | 条件必填 | 推荐使用的稳定 API key 标识；别名：`api_key_id`、`api-key-id`。 |
| `index` | integer | 条件必填 | 从 0 开始的下标。 |
| `value` | string or `APIKeyEntry` | 条件必填 | 和 `id` 或 `index` 配套的新值；支持结构化 entry。 |
| `old` | string | 条件必填 | 要查找的旧 key。 |
| `new` | string | 条件必填 | 新 key；旧值不存在时追加。 |
| `api_key` | string | 条件必填 | 旧版原始 key selector；和 `id` 同时提交时必须匹配该记录；也接受 `api-key`、`key`。 |
| `user_id` | integer | 否 | 绑定的 `user.id`；也接受 `user-id`；传 `0` 清空绑定。 |
| `channels` | array of integer | 否 | Channel group IDs。 |
| `model_groups` | array of integer | 否 | Model group IDs；也接受 `model-groups`。 |

成功输出：

```json
{
  "api_key": {
    "id": 1,
    "api_key_id": 1,
    "api-key": "client-key-1",
    "api_key": "client-key-1",
    "user-id": 1,
    "user_id": 1,
    "channels": [1],
    "model_groups": [2]
  }
}
```

### DELETE `/api-keys`

删除一个客户端 API key。优先使用稳定 ID；下标和值 selector 继续作为兼容方式。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `id` | integer | 推荐使用的稳定 API key 标识；别名：`api_key_id`、`api-key-id`。 |
| `index` | integer | 删除指定从 0 开始下标的 key。 |
| `value` | string | 删除 trim 后匹配的 key。 |
| `api_key` | string | `value` 的 alias。 |
| `api-key` | string | `value` 的 alias。 |
| `key` | string | `value` 的 alias。 |

输出示例：

```json
{ "status": "ok" }
```

未知 ID 返回 `404 api_key_not_found`。如果同时提交的 ID 和 key selector 指向不同记录，接口返回 `400 invalid_api_key_selector`。

## Billing

本节所有路径都相对于 Management API 基础 URL，例如 `/v0/management/billing/overview` 或 `/v0/management/proxy/proxy-pools`。这些不是 `/user` 路由，调用时需要管理密钥。

只有 `/billing/overview`、`/billing/charges` 和 `/billing/balance-records` 会将 `from` 和 `to` 解析为 `YYYY-MM-DD`、RFC3339 或 Unix 秒。三个路由统一使用半开区间 `[from,to)`：包含 `from`，不包含 `to`。可选的 `timezone` 参数是报表时区覆盖，并且必须是 IANA 时区名称。未提供时，路由使用 `/billing/settings.report_timezone`，该设置默认为 `UTC`。纯日期值使用实际报表时区中的日历日期，纯日期形式的 `to` 会规范化为下一个本地零点，因此即使跨越 DST，也会完整包含结束日期。显式时间戳是精确的排他上界，不会因报表时区被移动或扩展。`/billing/overview` 还使用实际报表时区生成 `range` 日历日期和 `daily_trend` 分桶，因此一个自然日不会在 UTC 午夜被拆成两天。报表时区只控制查询边界和报表分组，不会重新计算不可变 charge、修改价格快照或改变用户余额。分页参数 `limit` 和 `offset` 仅适用于 `/billing/charges` 和 `/billing/balance-records`；这些路由的 `limit` 默认值为 `50`，最大值为 `200`，负数 `offset` 会规范化为 `0`。`/billing/model-prices` 仅支持 `provider`、`model` 和 `enabled` 查询参数。`/proxy/proxy-pools` 当前不解析查询参数。

不支持的时区名称返回 `400 invalid_timezone`。`from` 晚于 `to` 时返回 `400 invalid_time_range`。

### GET `/billing/overview`

返回管理员计费摘要。

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `from` | string | 可选开始时间：`YYYY-MM-DD`、RFC3339 或 Unix 秒。 |
| `to` | string | 可选排他结束时间；纯日期通过使用下一个本地零点，完整包含 `timezone` 中的结束日期。 |
| `timezone` | string | 可选的 IANA 报表时区覆盖，用于纯日期边界、响应范围日期和每日趋势分桶；默认使用 `/billing/settings.report_timezone`。 |
| `user` | string | 可选用户名、用户文本或用户 ID 过滤；别名：`user_text`、`username`。 |
| `user_id` | integer | 可选精确用户 ID 过滤；别名：`uid`。 |
| `provider` | string | 可选 provider 过滤。 |
| `model` | string | 可选 model 过滤。 |

响应字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `range` | object | 实际使用的日历 `from`/`to`、精确 UTC `from_at`/`to_at_exclusive` 和 `timezone`；未传对应查询边界时，精确边界为 `null`。 |
| `total_charge_amount` | number | 总扣费金额。 |
| `total_recharge_amount` | number | 总充值金额。 |
| `total_deduct_amount` | number | 总手工扣减金额。 |
| `total_balance` | number | 当前用户总余额。 |
| `request_count` | integer | 扣费请求数量。 |
| `input_tokens` | integer | input token 总数。 |
| `output_tokens` | integer | output token 总数。 |
| `cache_tokens` | integer | cache token 总数。 |
| `active_user_count` | integer | 范围内有扣费记录的用户数量。 |
| `daily_trend[]` | array | 按 `range.timezone` 分组的每日扣费金额和请求数量。 |
| `top_users[]` | array | 用户排行，字段为 `id`、`label`、`amount`、`request_count`。 |
| `top_models[]` | array | 模型排行，字段为 `id`、`label`、`amount`、`request_count`。 |
| `top_providers[]` | array | Provider 排行，字段为 `id`、`label`、`amount`、`request_count`。 |

### GET `/billing/charges`

列出扣费记录，并返回管理员上下文。响应会暴露用户 ID、脱敏 API-key 元数据、价格快照、匹配的价格规则、request ID、endpoint，以及 `balance_before`/`balance_after`。计费扣费响应永不暴露原始 API key。

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `from` | string | 可选开始时间：`YYYY-MM-DD`、RFC3339 或 Unix 秒。 |
| `to` | string | 可选排他结束时间；纯日期通过使用下一个本地零点，完整包含 `timezone` 中的结束日期。 |
| `timezone` | string | 可选的 IANA 报表时区覆盖，用于纯日期边界；默认使用 `/billing/settings.report_timezone`。 |
| `user` | string | 可选用户名、用户文本或用户 ID 过滤；别名：`user_text`、`username`。 |
| `user_id` | integer | 可选精确用户 ID 过滤；别名：`uid`。 |
| `provider` | string | 可选 provider 过滤。 |
| `model` | string | 可选 model 过滤。 |
| `limit` | integer | 可选分页大小；默认 `50`，最大 `200`。 |
| `offset` | integer | 可选分页偏移；负数会规范化为 `0`。 |

响应结构：

```json
{
  "items": [
    {
      "id": "charge_xxx",
      "created_at": "2026-06-10T10:00:00Z",
      "user_id": 1,
      "api_key_label": "Alice key",
      "api_key_masked": "cpa_...abcd",
      "provider": "openai",
      "model": "gpt-4.1-mini",
      "original_model": "gpt-4.1-mini",
      "actual_model": "gpt-4.1-mini",
      "input_tokens": 1000,
      "output_tokens": 500,
      "cache_tokens": 0,
      "amount": 1.25,
      "balance_before": 20,
      "balance_after": 18.75,
      "request_id": "req_xxx",
      "endpoint": "/v1/chat/completions",
      "matched_price_rule": "openai:gpt-5.5:priority:272001",
      "price_snapshot": { "request_price": 0, "input_price_per_million": 2.5, "matched_service_tier": "priority", "min_input_tokens": 272001, "requested_service_tier": "priority", "response_service_tier": "default", "service_tier_source": "request", "effective_service_tier": "priority", "response_tier_fallback": false }
    }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

### GET `/billing/balance-records`

列出管理员充值和扣费分类账记录。

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `from` | string | 可选开始时间：`YYYY-MM-DD`、RFC3339 或 Unix 秒。 |
| `to` | string | 可选排他结束时间；纯日期通过使用下一个本地零点，完整包含 `timezone` 中的结束日期。 |
| `timezone` | string | 可选的 IANA 报表时区覆盖，用于纯日期边界；默认使用 `/billing/settings.report_timezone`。 |
| `user` | string | 可选用户名、用户文本或用户 ID 过滤；别名：`user_text`、`username`。 |
| `user_id` | integer | 可选精确用户 ID 过滤；别名：`uid`。 |
| `limit` | integer | 可选分页大小；默认 `50`，最大 `200`。 |
| `offset` | integer | 可选分页偏移；负数会规范化为 `0`。 |

响应结构：

```json
{
  "items": [
    {
      "id": "balance_xxx",
      "user_id": 1,
      "type": "recharge",
      "amount": 50,
      "balance_before": 0,
      "balance_after": 50,
      "operator": "admin",
      "note": "manual recharge",
      "created_at": "2026-06-10T10:00:00Z"
    }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

### POST `/billing/balance-records/recharge`

新增充值分类账记录，并更新用户 `credits`。

请求体：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `user_id` | integer | 是 | 目标用户 ID。 |
| `amount` | number | 是 | 正数充值金额。 |
| `note` | string | 否 | 可选操作备注。 |

管理密钥操作当前的 `operator` 为 `admin`。

响应：

```json
{ "status": "ok", "balance_record": { "id": "balance_xxx", "type": "recharge" } }
```

### POST `/billing/balance-records/deduct`

新增扣减分类账记录，并更新用户 `credits`。

请求体：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `user_id` | integer | 是 | 目标用户 ID。 |
| `amount` | number | 是 | 正数扣减金额。 |
| `note` | string | 是 | 扣减原因，必填。 |

管理密钥操作当前的 `operator` 为 `admin`。

响应：

```json
{ "status": "ok", "balance_record": { "id": "balance_xxx", "type": "deduct" } }
```

### GET `/billing/model-prices`

列出模型价格规则。

响应始终包含 `price_rule_schema_version: 2`，即使 `items` 为空。规则先匹配规范化后的精确 service tier，再回退到兼容通配符 `*`，然后选择不大于 usage 原始 input-token 数量的最大 `min_input_tokens`。

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `provider` | string | 可选 provider 过滤。 |
| `model` | string | 可选 model 过滤。 |
| `enabled` | boolean | 可选 enabled 过滤。 |

模型价格字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 模型价格记录 ID。 |
| `provider` | string | Provider 名称。 |
| `model` | string | Model 名称。 |
| `service_tier` | string | 规范化后的 service tier，或兼容通配符 `*`。 |
| `min_input_tokens` | integer | 上下文分段的包含式下界。 |
| `input_price_per_million` | number | Input-token 价格。 |
| `output_price_per_million` | number | Output-token 价格。 |
| `cache_read_price_per_million` | number | Cache-read token 价格。 |
| `cache_write_price_per_million` | number | Cache-write token 价格。 |
| `request_price` | number | 每请求价格。 |
| `source` | string | 价格来源。 |
| `enabled` | boolean | 规则是否启用。 |
| `note` | string | 操作备注。 |
| `created_at` | string | 创建时间。 |
| `updated_at` | string | 最近更新时间。 |
| `revision` | integer | 用于导入并发冲突检测的单调递增规则版本。 |

### POST `/billing/model-prices`

创建模型价格规则。省略的价格字段默认为 `0`，`enabled` 默认为 `true`。

请求体字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `provider` | string | 是 | Provider 名称。 |
| `model` | string | 是 | Model 名称。 |
| `service_tier` | string | 否 | 精确 tier 或 `*`，默认 `*`；`auto`、`default` 和 `standard` 都归一为本地 `standard` tier。 |
| `min_input_tokens` | integer | 否 | 非负、包含式上下文分段下界，默认 `0`。 |
| `input_price_per_million` | number | 否 | 非负 input-token 价格。 |
| `output_price_per_million` | number | 否 | 非负 output-token 价格。 |
| `cache_read_price_per_million` | number | 否 | 非负 cache-read token 价格。 |
| `cache_write_price_per_million` | number | 否 | 非负 cache-write token 价格。 |
| `request_price` | number | 否 | 非负每请求价格。 |
| `source` | string | 否 | 价格来源，例如 `manual`。 |
| `enabled` | boolean | 否 | 规则是否启用；默认 `true`。 |
| `note` | string | 否 | 操作备注。 |

响应：

```json
{ "status": "ok", "model_price": { "id": "price_xxx", "provider": "openai", "model": "gpt-4.1-mini" } }
```

### PATCH `/billing/model-prices/:id`

局部更新模型价格规则，并保留未指定字段。请求体接受与 `POST /billing/model-prices` 相同的字段。

响应：

```json
{ "status": "ok", "model_price": { "id": "price_xxx", "enabled": false } }
```

### DELETE `/billing/model-prices/:id`

软删除模型价格规则。

输入：无 body。

响应：

```json
{ "status": "ok" }
```

### GET `/billing/settings`

返回 DB-backed 计费策略。`service_tier_source` 默认为 `request`，允许值为 `request` 或 `response`。`report_timezone` 必须是受支持的 IANA 时区，默认值为 `UTC`；当 Management Billing 请求未提供 `timezone` 覆盖时，它作为报表默认日历时区。修改该设置只影响后续报表的边界和分组，不会重新计算历史 charge、重写价格快照或改变余额。无论哪种 tier-source 模式，`auto`、`default` 和 `standard` 都按本地 Standard 价格 tier 匹配。

```json
{ "service_tier_source": "request", "report_timezone": "UTC" }
```

### PATCH `/billing/settings`

局部更新计费设置。两个字段均为可选。在 `response` 模式下，如果 response tier 缺失，会回退到 request tier，并在扣费价格快照中记录该回退。非法 tier-source 值或不支持的时区名称返回 `400 invalid_body`。

```json
{ "service_tier_source": "response", "report_timezone": "Asia/Shanghai" }
```

OpenAI 规定未传 `service_tier` 时为 `auto`；`auto` 是 Home 的内部表示，不能作为字面量直接发给 Codex backend。参见 [OpenAI 定价页](https://developers.openai.com/api/docs/pricing) 和 [Responses Create 参考](https://developers.openai.com/api/reference/resources/responses/methods/create)。Home 保存请求 `service_tier` 和可选上游 `response_service_tier`，方便后续切换计费来源而无需重新灌入 usage。默认 `request` 来源按 `service_tier`（客户端请求 tier）计费。在 `response` 模式下优先使用 `response_service_tier`，上游未返回时回退请求 tier。`auto`、`default` 和 `standard` 都映射到本地 Standard 规则；使用 `flex` 或 `priority` 时请配置相应规则。

扣费 `price_snapshot` 审计数据包含 `requested_service_tier`、可选的 `response_service_tier`、`service_tier_source`、`effective_service_tier`、`response_tier_fallback`、`matched_service_tier` 和 `min_input_tokens`。上下文分段使用原始 input-token 总数。在 OpenAI Responses 协议中，`input_tokens` 已包含 cache-read 和 cache-write token。Home 会先从普通 input 中扣除 cache-read token，再应用 cache-read 价格；当 `cache_write_price_per_million` 大于零时，也会先从普通 input 中扣除 cache-write token，再应用独立的 cache-write 价格；价格为零或未提供时，这些 cache-write token 仍按普通 input 计费。在 Anthropic Messages 协议中，`input_tokens`、`cache_read_input_tokens` 和 `cache_creation_input_tokens` 是相互独立的 bucket，因此 Home 会分别计费，不会从 input 中扣除任何 cache bucket。cache 字段兼容回填只更新 usage 计数；已有不可变 `billing_charge` 快照和余额不会自动重算，历史修正需要显式、可审计的余额调整。

### POST `/billing/model-prices/import/preview`

创建服务端的不可变 `models.dev` 导入预览。服务端拉取并固定来源快照；客户端提供目标、匹配策略、别名、行倍率和可选的来源匹配覆盖。客户端可控的输入无效时返回 `422 invalid_import_preview`，目录拉取失败时返回 `502 models_dev_fetch_failed`，内部 preview 持久化失败时返回 `500 billing_import_preview_failed`。成功响应包含 `preview_id`、`preview_revision`、来源信息、`generated_at`、`expires_at`、明确的 `atomic: true`、行和精确汇总。

当前 preview target 只描述通配符 base 规则（`service_tier: "*"`、`min_input_tokens: 0`）；其他 target scope 会被拒绝，不会被静默改写。已匹配行会包含官方价格、倍率后的最终价格、精确的 `write_rule`、带 `revision` 的完整可选 `existing_rule` 快照，以及机器可读的原因。models.dev 上下文分段会生成不同包含式下界的通配符行；`row_multipliers` 按返回的精确 row key 生效，包含 context-band 行。cache-read 和 cache-write 价格从固定的 catalog snapshot 导入。当 cost 对象包含 input 价格但省略 `cache_read` 或 `cache_write` 时，Home 分别按 `input * 0.1` 或 `input * 1.25` 派生缺失价格；显式值（包括零）保持不变。出现不支持的价格维度、格式错误或无效的价格/分段、重复分段，或 tier 缺少 base 已配置价格维度时，整个 target 都不可应用；服务端不会导入可能低估计费的子集。

`policy.overwrite_mode` 可为 `missing`、`sync` 或 `all`。`missing` 只创建缺失规则，`sync` 可更新已有 `source=sync` 规则，`all` 还可覆盖 manual/default 规则。`overwrite` 行在 apply 时必须确认。

### POST `/billing/model-prices/import/apply`

在一个数据库事务中应用 preview 的选中行。请求体包含 `preview_id`、`preview_revision`、非空且唯一的 `selected_keys`、`confirm_overwrite` 和 `idempotency_key`；相同 key 也可放在 `Idempotency-Key` 请求头中，两者同时存在时必须一致。

预览过期返回 `410`，预览版本不符返回 `412`，已有规则已变化（包括同一身份被并发创建）返回 `409`；无效选择、未确认覆盖或用不同请求复用幂等 key 返回 `422`。相同 key 的等价重放返回原始不可变 operation，且不会再次写入。成功时同步返回 `200`，包含 `operation_id`、`preview_id`、`status: "applied"`、`atomic: true`、`applied_at`、汇总和每个选中行的结果。每个成功行都包含非空 `resource_id`。过期 preview 最多保留 24 小时用于诊断，完成的 operation 最多保留 30 天；后续创建 preview 时会清理它们。

### GET `/billing/model-prices/import/operations/:id`

返回持久化的不可变 apply operation 结果。未知 operation ID 返回 `404`。当前 apply 为同步执行，因此终态是 `applied`，而不是 `pending` 或 `running`。

### GET `/billing/settings/diagnostics`

返回基于存储 usage 的 tier 证据：`supported`、`window_start`、`window_end`、`eligible_requests`、`response_tier_requests`、`fallback_requests` 和可选的 `last_response_tier_at`。eligible request 指近期带 request service tier 的记录；fallback request 指其中没有 response service tier 的记录。该接口只报告实际观测到的 payload 数据，不推断 response-tier 覆盖率。

### GET `/proxy/proxy-pools`

列出代理池记录。

代理池记录在此版本中只会被存储和测试。它们不会改变运行时代理优先级、认证选择、分发或出站流量路由。唯一支持的 `scope` 是 `global`。

响应：

```json
{
  "items": [
    {
      "id": "proxy_xxx",
      "name": "Primary proxy",
      "proxy_url": "http://127.0.0.1:18080",
      "enabled": true,
      "scope": "global",
      "priority": 10,
      "last_tested_at": "2026-06-10T10:00:00Z",
      "last_test_result": "passed",
      "note": "manual entry",
      "updated_at": "2026-06-10T10:00:00Z"
    }
  ]
}
```

### POST `/proxy/proxy-pools`

创建代理池记录。省略 `enabled` 时默认 `true`。`scope` 仅支持 `global`。

请求体字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | 展示名称。 |
| `proxy_url` | string | 是 | 需要存储和测试的代理 URL。 |
| `enabled` | boolean | 否 | 记录是否启用；默认 `true`。 |
| `scope` | string | 否 | 仅支持 `global`。 |
| `priority` | integer | 否 | 存储的优先级值；本版本不影响运行时路由。 |
| `note` | string | 否 | 操作备注。 |

响应：

```json
{ "status": "ok", "proxy_pool": { "id": "proxy_xxx", "scope": "global", "enabled": true } }
```

### PATCH `/proxy/proxy-pools/:id`

局部更新代理池记录，并保留未指定字段。

请求体：`POST /proxy/proxy-pools` 字段的任意子集。

响应：

```json
{ "status": "ok", "proxy_pool": { "id": "proxy_xxx", "enabled": false } }
```

缺失记录返回：

```json
{ "error": "proxy_pool_not_found", "message": "record not found" }
```

### DELETE `/proxy/proxy-pools/:id`

删除代理池记录。

输入：无 body。

响应：

```json
{ "status": "ok" }
```

缺失记录返回 `proxy_pool_not_found`。

### POST `/proxy/proxy-pools/:id/test`

测试已存储的代理池记录。当条目存在且测试完成时，接口返回 `200`，`result` 为 `"passed"` 或 `"failed"`，并更新该记录的 `last_tested_at` 和 `last_test_result`。

输入：无 body。

响应：

```json
{
  "status": "ok",
  "result": "passed",
  "message": "proxy test returned HTTP 204"
}
```

缺失记录返回：

```json
{ "error": "proxy_pool_not_found", "message": "record not found" }
```

## Provider API Key Routes

以下 route 管理上游 API-key 凭证：

```text
GET    /gemini-api-key
PUT    /gemini-api-key
PATCH  /gemini-api-key
DELETE /gemini-api-key

GET    /claude-api-key
PUT    /claude-api-key
PATCH  /claude-api-key
DELETE /claude-api-key

GET    /codex-api-key
PUT    /codex-api-key
PATCH  /codex-api-key
DELETE /codex-api-key

GET    /xai-api-key
PUT    /xai-api-key
PATCH  /xai-api-key
DELETE /xai-api-key

GET    /vertex-api-key
PUT    /vertex-api-key
PATCH  /vertex-api-key
DELETE /vertex-api-key

GET    /openai-compatibility
PUT    /openai-compatibility
PATCH  /openai-compatibility
DELETE /openai-compatibility
```

Home 会从这些 config-like payload 合成 DB auth records。xAI API-key usage 会以 `provider=xai` 和 API-key credential type 进入通用 usage 管线，因此可用于 usage records、provider/credential aggregates、billing，并会出现在旧版 `/api-key-usage` 的 `xai` provider bucket 中。

### 凭证字段结构

`GeminiKey`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `api-key` | string | 上游 Gemini API key。 |
| `priority` | integer | 凭证选择优先级，值越大越优先。 |
| `prefix` | string | 可选模型命名空间前缀。 |
| `base-url` | string | 可选 Gemini API base URL override。 |
| `proxy-url` | string | 可选 per-key 出站代理。 |
| `models` | array of `ModelAlias` | 可选上游模型 alias。 |
| `headers` | object string to string | 额外上游请求头。 |
| `excluded-models` | array of string | 该 key 排除的模型 ID。 |
| `disable-cooling` | boolean | 对该凭证禁用 quota cooldown 调度。 |
| `auth-index` | string | Compatibility credential identifier。 |
| `auth_index`, `id`, `uuid` | string | DB auth identifier aliases。 |
| `disabled` | boolean | 只读 DB auth disabled flag；请使用 `PATCH /auth-files/status` 修改。 |

`ClaudeKey`、`CodexKey`、`XAIKey` 和 `VertexCompatKey` 使用相同通用字段。`XAIKey` 使用原生 xAI executor，并要求提供 `base-url`（通常为 `https://api.x.ai/v1`）。额外字段如下：

| 字段 | 适用范围 | 说明 |
| --- | --- | --- |
| `cloak` | Claude | 可选请求 cloaking 配置。 |
| `experimental-cch-signing` | Claude | 为 cloaked Claude 请求启用实验性 CCH signing。 |
| `websockets` | Codex、xAI | 启用 Responses API websocket transport。 |
| `api-key` | Vertex | 作为 `x-goog-api-key` 发送。 |

`OpenAICompatibility`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | Provider 名称。 |
| `priority` | integer | Provider 选择优先级。 |
| `disabled` | boolean | 为 `true` 时禁用该 provider。 |
| `prefix` | string | 可选模型命名空间前缀。 |
| `base-url` | string | OpenAI-compatible API base URL。 |
| `api-key-entries` | array of `OpenAICompatibilityAPIKey` | Provider API keys 和可选代理。 |
| `models` | array of `OpenAICompatibilityModel` | 模型定义和 alias。 |
| `headers` | object string to string | 额外上游 headers。 |
| `disable-cooling` | boolean | 对该 provider 禁用 quota cooldown 调度。 |
| `auth-index` | string | 无 per-key entries 时的 compatibility credential identifier。 |
| `auth_index`, `id`, `uuid` | string | DB auth identifier aliases。 |

共享嵌套结构：

```json
{
  "ModelAlias": {
    "name": "upstream-model",
    "alias": "client-visible-model",
    "display-name": "Catalog name",
    "force-mapping": true
  },
  "OpenAICompatibilityAPIKey": {
    "api-key": "provider-key",
    "proxy-url": "http://127.0.0.1:7890"
  },
  "OpenAICompatibilityModel": {
    "name": "upstream-model",
    "alias": "client-visible-model",
    "thinking": {
      "min": 0,
      "max": 24576,
      "zero_allowed": true,
      "dynamic_allowed": true,
      "levels": ["low", "medium", "high"]
    }
  },
  "CloakConfig": {
    "mode": "auto",
    "strict-mode": false,
    "sensitive-words": ["word"],
    "cache-user-id": true
  }
}
```

### GET Provider Key Routes

输入：无。

输出示例：

```json
{
  "gemini-api-key": [
    {
      "auth_index": "auth-db-id",
      "id": "auth-db-id",
      "uuid": "auth-db-id",
      "api-key": "AIza...",
      "base-url": "https://generativelanguage.googleapis.com",
      "prefix": "team-a",
      "proxy-url": "",
      "disabled": false,
      "priority": 10,
      "headers": { "X-Test": "1" },
      "models": [
        { "name": "gemini-upstream", "alias": "gemini-alias" }
      ]
    }
  ]
}
```

### PUT Provider Key Routes

整体替换对应 provider 的完整列表。

输入可以是数组：

```json
[
  {
    "api-key": "provider-key",
    "base-url": "https://api.example.com",
    "models": [
      { "name": "upstream-model", "alias": "alias-model" }
    ]
  }
]
```

也可以是 wrapper：

```json
{ "items": [ { "api-key": "provider-key" } ] }
```

Home 还接受 `{ "<route-key>": [...] }`、`{ "list": [...] }`、`{ "data": [...] }` 或单个 entry object。

成功输出：

```json
{ "status": "ok" }
```

### PATCH Provider Key Routes

更新单条 provider credential。

输入示例：

```json
{
  "index": 0,
  "match": "old-api-key",
  "name": "openai-provider-name",
  "value": {
    "api-key": "new-api-key",
    "base-url": "https://api.example.com",
    "proxy-url": "",
    "headers": { "X-Test": "1" },
    "excluded-models": ["model-a"]
  }
}
```

Selector 字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `index` | integer | provider 过滤后列表中的从 0 开始下标。 |
| `match` | string | 匹配 API-key 值。 |
| `name` | string | OpenAI-compatible provider name 或 auth label。 |
| `id` | string | DB auth ID。 |
| `uuid` | string | `id` 的 alias。 |
| query `base-url` | string | 可选 base URL，用于消除 API-key 匹配歧义。 |

`PATCH` 不使用 body 中的 `auth_index` 作为 DB ID selector；按 ID patch 请使用 `id` 或 `uuid`。

成功输出：

```json
{ "status": "ok" }
```

### DELETE Provider Key Routes

删除单条 provider credential。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | DB auth ID。 |
| `uuid` | string | `id` 的 alias。 |
| `auth_index` | string | DB auth ID 或 runtime index。 |
| `index` | integer | provider 过滤后列表中的从 0 开始下标。 |
| `api-key` | string | API-key 值。 |
| `api_key` | string | `api-key` 的 alias。 |
| `match` | string | `api-key` 的 alias。 |
| `base-url` | string | 可选 base URL，用于消除歧义。 |
| `base_url` | string | `base-url` 的 alias。 |
| `name` | string | Provider 或 compatibility name。 |

成功输出：

```json
{ "status": "ok" }
```

## Auth Files 和 OAuth

### GET `/auth-files`

列出 OAuth/file-backed credentials。

输入：无。

输出示例：

```json
{
  "files": [
    {
      "id": "auth-db-id",
      "auth_index": "auth-db-id",
      "name": "auth-db-id.json",
      "file_name": "auth-db-id.json",
      "type": "codex",
      "provider": "codex",
      "label": "user@example.com",
      "status": "active",
      "status_message": "",
      "disabled": false,
      "unavailable": false,
      "runtime_only": false,
      "source": "db",
      "email": "user@example.com",
      "priority": 10,
      "note": "operator note",
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "modtime": "2026-05-27T10:00:00Z"
    }
  ]
}
```

### GET `/auth-files/models?name=<name-or-id>`

返回指定 auth file 或 auth ID 对应的模型。

Query 参数：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | Auth filename、auth ID、display name 或 runtime index。 |

输出示例：

```json
{
  "models": [
    {
      "id": "gpt-5.5",
      "display_name": "GPT-5.5",
      "type": "codex",
      "owned_by": "openai"
    }
  ]
}
```

### GET `/auth-files/download`

下载一个 credential JSON。

Query 参数：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 条件必填 | Filename 或 display name。 |
| `file` | string | 条件必填 | Filename alias。 |
| `filename` | string | 条件必填 | Filename alias。 |
| `id` | string | 条件必填 | DB auth ID。 |
| `uuid` | string | 条件必填 | `id` 的 alias。 |
| `auth_index` | string | 条件必填 | Auth ID 或 runtime index。 |
| `index` | integer | 条件必填 | OAuth auth 的从 0 开始下标。 |

输出：`application/json; charset=utf-8` 附件。

### POST `/auth-files`

上传一个或多个 credential JSON payload。

Multipart 输入：

| Form field | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| 任意 file field | file | 是 | 一个或多个 `.json` credential 文件。 |

Raw JSON 输入：请求体是 credential JSON payload；`name` 不是必填，Home 会推导或分配 UUID-backed 文件名。

输出示例：

```json
{ "status": "ok" }
```

```json
{ "status": "ok", "uploaded": 2, "files": ["a.json", "b.json"] }
```

Raw JSON 输出：

```json
{ "status": "ok", "name": "uuid.json" }
```

### DELETE `/auth-files`

删除 credential records 或文件。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | Filename 或 display name。 |
| `file` | string | Filename alias。 |
| `filename` | string | Filename alias。 |
| `id` | string | DB auth ID。 |
| `uuid` | string | `id` 的 alias。 |
| `auth_index` | string | Auth ID 或 runtime index。 |
| `index` | integer | OAuth auth 的从 0 开始下标。 |
| `all` | `true`、`1` 或 `*` | 删除全部 OAuth/file-backed credentials。 |

输出示例：

```json
{ "status": "ok" }
```

`all` 输出：

```json
{ "status": "ok", "deleted": 2 }
```

### PATCH `/auth-files/status`

启用或禁用 OAuth/file-backed auth。

输入示例：

```json
{
  "name": "codex-user.json",
  "disabled": true
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `name` | string | 是 | Filename、DB auth ID、runtime auth index 或 display name。 |
| `disabled` | boolean | 是 | `true` 禁用，`false` 启用。 |

该接口当前只从 `name` 读取 selector；不会读取 body 中独立的 `id`、`uuid`、`auth_index` 或 `index` 字段。

输出示例：

```json
{ "status": "ok", "disabled": true }
```

### PATCH `/auth-files/fields`

更新可编辑 auth metadata。

输入示例：

```json
{
  "name": "codex-user.json",
  "id": "auth-db-id",
  "uuid": "auth-db-id",
  "auth_index": "auth-db-id",
  "prefix": "team-a",
  "proxy_url": "http://127.0.0.1:7890",
  "proxy-url": "http://127.0.0.1:7890",
  "headers": { "X-Test": "1" },
  "priority": 10,
  "note": "operator note",
  "websockets": true,
  "disabled": false
}
```

Selector 字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name`, `file`, `filename` | string | Filename 或 display name。 |
| `id`, `uuid`, `auth_index` | string | DB auth ID selector。 |
| query `index` | integer | OAuth auth 从 0 开始下标 selector。 |

可编辑字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `prefix` | string | 模型命名空间前缀；空值清空。 |
| `proxy_url` | string | Per-auth proxy URL；空值清空。 |
| `proxy-url` | string | `proxy_url` 的 alias。 |
| `headers` | object string to string | 额外上游 headers。空字符串会删除单个 header。 |
| `priority` | integer or numeric string | 凭证选择优先级。 |
| `note` | string | 操作备注；空值清空。 |
| `websockets` | boolean or string bool | 支持的 auth 的 runtime websocket flag。 |
| `disabled` | boolean or string bool | 更新 auth disabled state 和 status。 |
| 任意 nested path | any valid JSON | 可以设置任意 metadata path，例如 `token.access_token`。 |

输出示例：

```json
{ "status": "ok" }
```

### OAuth 启动路由

以下 route 创建 provider 登录 URL 或 device-flow session：

```text
GET /anthropic-auth-url
GET /codex-auth-url
GET /antigravity-auth-url
GET /kimi-auth-url
GET /xai-auth-url
GET /<plugin-provider>-auth-url
```

通用输出：

```json
{
  "status": "ok",
  "url": "https://provider.example/oauth/authorize?...",
  "state": "oauth-state"
}
```

`GET /kimi-auth-url` 会启动 device flow 并返回验证 URL，Home 在后台等待完成。

`GET /<plugin-provider>-auth-url` 可用于 `GET /plugins` 返回的 Home-loaded 插件 provider；对应条目需要 `supports_oauth: true`、`effective_enabled: true`，并且 `oauth_provider` 非空。provider 路径段会规范化为小写，且只能包含字母、数字和连字符。

### GET `/get-auth-status`

返回当前 OAuth session 状态。

Query 参数：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `state` | string | 否 | OAuth state token。 |

输出示例：

```json
{ "status": "ok" }
{ "status": "wait" }
{ "status": "error", "error": "Authentication failed" }
{ "status": "error", "error": "unknown or expired state" }
```

未知或已过期的 state token 会返回错误，不再被视为已完成。已完成 session 会作为短期 tombstone 保留，使最后一次轮询能够返回 `{ "status": "ok" }`；tombstone 过期后，同一 state 会按未知状态处理。

对于插件 OAuth session，该接口会轮询 Home 已加载的插件。插件返回 success 后，Home 会把插件返回的 auth data 转成 DB-backed auth records，注册该 auth 的模型，完成 OAuth session，然后返回 `{ "status": "ok" }`。

### POST `/oauth-callback`

处理 provider OAuth callback metadata。

输入示例：

```json
{
  "provider": "codex",
  "redirect_url": "http://localhost/callback?code=CODE&state=STATE",
  "code": "CODE",
  "state": "STATE",
  "error": ""
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `provider` | string | 是 | 内置 provider alias：`anthropic`/`claude`、`codex`/`openai`、`antigravity`/`anti-gravity`、`xai`/`x-ai`/`grok`。插件 OAuth session 传入插件的 `oauth_provider` key。`kimi` 不通过该 route 完成。 |
| `redirect_url` | string | 否 | 完整 callback URL；缺失的 `code`、`state` 或 `error` 可以从中提取。 |
| `code` | string | 条件必填 | OAuth authorization code；除非提供 `error`，否则必填。 |
| `state` | string | 是 | OAuth state token。 |
| `error` | string | 条件必填 | Provider error；缺少 `code` 时必填。 |

Home 会从 DB-backed OAuth session 中读取 session data。内置 OAuth session 会在后台 exchange code，并把得到的 auth records 写入 DB。插件 OAuth session 会先把 callback metadata 写入 session；随后 `/get-auth-status` 轮询插件，并持久化插件返回的 auth records。

输出示例：

```json
{ "status": "ok" }
```

常见错误：

```json
{ "status": "error", "error": "invalid body" }
{ "status": "error", "error": "unsupported provider" }
{ "status": "error", "error": "unknown or expired state" }
{ "status": "error", "error": "oauth flow is not pending" }
{ "status": "error", "error": "provider does not match state" }
```

### POST `/vertex/import`

上传 Vertex service account JSON 并创建 Vertex OAuth/file-backed credential。

输入：

| Form field or query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| form `file` | file | 是 | Vertex service account JSON。 |
| form/query `location` | string | 否 | Vertex location；默认 `us-central1`。 |

输出示例：

```json
{
  "status": "ok",
  "auth-file": "vertex-project-id.json",
  "project_id": "project-id",
  "email": "service-account@example.iam.gserviceaccount.com",
  "location": "us-central1"
}
```

Home 会把生成的 credential 作为 DB-backed OAuth auth records 存储，并在 `auth-file` 中返回生成的 `<uuid>.json` 名称。

## API Call Proxy

### POST `/api-call`

从 Home server 发起任意 HTTP 请求。该 route 本身受 Management API 认证保护。

输入示例：

```json
{
  "auth_index": "auth-index",
  "authIndex": "auth-index",
  "AuthIndex": "auth-index",
  "method": "GET",
  "url": "https://api.example.com/v1/ping",
  "header": {
    "Authorization": "Bearer $TOKEN$",
    "Content-Type": "application/json",
    "Host": "api.example.com"
  },
  "data": "{}"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `auth_index`, `authIndex`, `AuthIndex` | string | 否 | 来自 `GET /auth-files` 或 provider-key routes 的 credential index，用于选择代理和替换 `$TOKEN$`。 |
| `method` | string | 是 | HTTP method；会转成大写。 |
| `url` | string | 是 | 带 scheme 和 host 的绝对 URL。 |
| `header` | object string to string | 否 | 请求头；包含 `$TOKEN$` 的 header value 会替换为选中 auth token。`Host` 会设置 request host override。 |
| `data` | string | 否 | 原始请求体字符串。 |

Token 替换更严格：只要任意 header 包含 `$TOKEN$`，`auth_index` 就必须解析到 DB auth 或 runtime auth，否则返回：

```json
{ "error": "auth not found" }
```

代理优先级：

1. 选中 credential 的 proxy。
2. 全局 `proxy-url`。
3. 禁用环境代理的直连 transport。

输出示例：

```json
{
  "status_code": 200,
  "header": {
    "Content-Type": ["application/json"]
  },
  "body": "{\"ok\":true}"
}
```

## 用量和日志

### GET `/api-key-usage`

返回内存中的 API-key usage，按 provider 和 `<base_url>|<api_key>` 分组。

输入：无。

输出示例：

```json
{
  "gemini": {
    "https://generativelanguage.googleapis.com|AIza...": {
      "success": 10,
      "failed": 1,
      "recent_requests": [
        { "time": "10:00-10:10", "success": 8, "failed": 0 }
      ]
    }
  }
}
```

### GET `/capabilities`

返回当前 Home Management API 暴露的前端能力开关和构建信息。该接口用于管理面板判断是否可以启用用量总览、请求明细、聚合排行、导出、实时诊断、健康归因和 request log index。

输出字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `capabilities.usage` | boolean | 是否支持旧版 `GET /api-key-usage` 能力。 |
| `capabilities.quota_snapshots` | boolean | 是否支持 DB-backed `GET /quota/credentials` 额度快照列表。 |
| `capabilities.quota_snapshot_details` | boolean | 是否支持 `GET /quota/credentials/:credential_id`。 |
| `capabilities.usage_overview` | boolean | 是否支持 `GET /usage/overview`。 |
| `capabilities.usage_records` | boolean | 是否支持 `GET /usage/records`。 |
| `capabilities.usage_record_details` | boolean | 是否支持 `GET /usage/records/:id`。 |
| `capabilities.usage_aggregates` | boolean | 是否支持 `GET /usage/aggregates`。 |
| `capabilities.usage_export` | boolean | 是否支持 `GET /usage/export`。 |
| `capabilities.usage_provider_health` | boolean | 是否支持 `GET /usage/health/providers`。 |
| `capabilities.usage_credential_health` | boolean | 是否支持 `GET /usage/health/credentials`。 |
| `capabilities.usage_realtime` | boolean | 是否支持 `GET /usage/realtime`。 |
| `capabilities.request_log_index` | boolean | 是否支持 `GET /request-logs`。 |
| `capabilities.request_events` / `capabilities.requestEvents` | boolean | 是否支持 `GET /request-events`。 |
| `capabilities.request_event_details` / `capabilities.requestEventDetails` | boolean | 是否支持 `GET /request-events/:id`。 |
| `capabilities.request_event_export` / `capabilities.requestEventExport` | boolean | 是否支持 `GET /request-events/export`。 |
| `capabilities.request_event_filters` / `capabilities.requestEventFilters` | boolean | 是否支持 `GET /request-events/filter-options`。 |
| `capabilities.oauth_usage` | boolean | OAuth/file-backed credential usage 归因是否可靠。 |
| `capabilities.logs` | boolean | 是否支持应用日志接口。 |
| `capabilities.request_error_logs` | boolean | 是否支持 request error log file list/download。 |
| `capabilities.topology` | boolean | 是否支持 `GET /topology` Home + CPA 集群拓扑接口。 |
| `server_info.home_version` | string | Home 构建版本。 |
| `server_info.home_commit` | string | Home 构建 commit。 |
| `server_info.home_build_date` | string | Home 构建时间。 |

### 额度快照通用约定

额度快照接口是纯只读 DB 视图。读取不会请求上游 Provider、刷新 OAuth token、改变调度优先级或消费队列。凭证存在但从未产生快照且从未尝试采集时返回 `quota_status=unknown`、`freshness=never`、空窗口和 HTTP `200`。如果第一次采集在获得任何可用额度事实前失败，则返回 `quota_status=error`、`freshness=never` 和 `collection_status=failed`。Provider 明确不在当前采集规划内时返回 `unsupported`。凭证删除后不再可见，对应额度表记录同时删除；凭证 Provider 或凭证类型发生变化时，也会清除旧身份的额度快照。

所有时间为 RFC3339 UTC 或 `null`；比例为 `[0,1]`；数量字段允许 `null`；无限额度使用 `is_unlimited=true`。不同 Provider 周期始终保留为独立窗口。快照仍为 fresh 时，单独到期的合并窗口会从当前结果中移除且不再参与状态/source 汇总；快照整体变为 stale 后，详情仍保留最后已知窗口用于诊断。`earliest_reset_at` 是该凭证项所代表的完整内部窗口集合中，全部非空 `reset_at` 的最小值；stale 的最后已知数据可能返回过去时间。

当前被动采集从 CPA usage 事件的 `response_headers` 中提取受限 `quota_headers`。Home 只保留 Codex `X-Codex-*` 额度 Header allowlist，以及通过语法校验且不具有 secret 特征的 upstream request ID，并在 usage payload 入库前删除 raw `response_headers`。入库前会按 active auth UUID、runtime index、ID 依次解析上报的 `auth_index`，快照始终使用稳定 UUID。Codex Header 观测与 usage record 在同一事务中归一化并 upsert；非法额度元数据会被隔离，不能回滚核心 usage 或 billing 写入。比 Home 接收时间超前五分钟以上的时间戳会归一化为接收时间。迟到事件不能覆盖更新快照，首次并发写入也遵守该规则。更新的 Header 观测会使正在运行的主动探测租约失效；部分 Header 更新只保留仍有效的旧窗口，已过期窗口不会参与新快照状态汇总。

Home 同时为 Claude、Antigravity、Codex、Kimi、xAI 的 OAuth/file credential 运行固定目标主动 collector。Codex 读取官方 usage endpoint；Claude 查询 usage 和 profile，额度成功但 profile 元数据失败时返回 `partial`；Antigravity 携带 credential `project_id` 按固定候选端点顺序尝试；Kimi 查询 coding usage，保留账号 usage 汇总和每个 limit 窗口，并兼容数字或字符串形式的数值字段；xAI 查询 billing，并把 Provider cents 转成显式 USD currency 数值。无法使用这些 OAuth collector 的 Provider API-key credential 返回 `unsupported`。

collector 直接读取 DB 凭证，不接受 HMC 提交 URL。探测前会重新解析最新 DB 凭证，并在 runtime 刷新策略判定到期时刷新 OAuth 状态；全局代理使用热更新后的当前配置，凭证级代理仍优先。统一使用 20 秒 timeout、PostgreSQL 下每个 Provider 并发上限 3（SQLite 下全局为 1）、单凭证 DB 租约，以及从 5 分钟开始、带凭证级 jitter、约 1 小时封顶的指数退避；`Retry-After` 可延后下次尝试。禁用凭证以及 retry deadline 仍在未来的凭证不会被主动探测；deadline 过期后，持久化的 unavailable/error 状态不再永久阻止恢复探测。成功快照默认 30 分钟有效。探测失败时保留最后已知窗口，只写结构化脱敏错误。

凭证核心字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `credential_id` | string | Home 稳定 auth UUID，也是详情路径 ID。 |
| `auth_index` | string/null | 当前 runtime auth index。 |
| `provider` | string | 规范化 Provider ID。 |
| `credential_type` | string | `oauth`、`provider_api_key`、`file_auth`、`vertex` 或 `unknown`。 |
| `label` | string | 安全展示标签，无标签时提供稳定 fallback。 |
| `account`、`project` | string/null | 已脱敏的账号和项目展示元数据；搜索使用同一展示值。 |
| `credential_status` | string | `enabled`、`disabled`、`unavailable`、`cooldown` 或 `unknown`。 |
| `quota_status` | string | `healthy`、`low`、`exhausted`、`unknown`、`error` 或 `unsupported`。 |
| `freshness` | string | `fresh`、`stale` 或 `never`；当前时间达到 `expires_at` 后动态变为 `stale`，旧数据存在观测时间但缺少有效期时也视为 `stale`。 |
| `collection_status` | string | `idle`、`collecting`、`success`、`partial`、`failed` 或 `unsupported`。 |
| `source` | string/null | `response_header`、`active_probe`、`mixed` 或 `null`。 |
| `observed_at`、`expires_at` | string/null | 最近有效观测与新鲜度截止时间。 |
| `earliest_reset_at` | string/null | 全部有效窗口中的最早重置时间，包含未进入 `primary_windows` 的窗口；stale 的最后已知值可能是过去时间。 |
| `last_attempt_at`、`last_success_at`、`next_probe_at` | string/null | 采集调度元数据。 |
| `consecutive_failures` | integer | 连续采集失败次数。 |
| `primary_windows` | array | 稳定选出的最多两个当前窗口。 |
| `window_count` | integer | 全部当前窗口数。 |
| `error` | object/null | 已脱敏采集错误，message 最多 500 bytes。 |
| `runtime` | object/null | Home 和 CPA 归属元数据。 |

额度窗口包含稳定 `id`、可选 `label/scope_id/currency`、`scope`、`mode`、窗口 `status`、显式 `unit`、可空 `used/remaining/limit`、`[0,1]` 比例、`is_unlimited`、可空 `reset_at/window_seconds`、结构化 `period_unit/period_value`、`source` 和实际 `observed_at`。`period_unit` 可为 `minute`、`hour`、`day`、`week`、`month` 或 `unknown`；已知周期的 `period_value` 必须为正数，`unknown` 时为 `null`。

### GET `/quota/credentials`

返回筛选、分页后的当前凭证额度快照。

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `limit` | integer | `50` | 1 到 200。 |
| `offset` | integer | `0` | 非负偏移。 |
| `search` | string | 无 | 对 label、account、project、auth index 和 provider 做不区分大小写包含匹配。 |
| `provider` | string/CSV | 无 | 一个或多个 Provider。 |
| `quota_status` | string/CSV | 无 | 一个或多个额度状态。 |
| `freshness` | string/CSV | 无 | `fresh`、`stale`、`never`。 |
| `source` | string/CSV | 无 | `response_header`、`active_probe`、`mixed` 或 `none`。 |
| `credential_status` | string/CSV | 无 | 一个或多个凭证状态。 |
| `collection_status` | string/CSV | 无 | 一个或多个采集状态。 |
| `sort` | string | `risk_desc` | `risk_desc`、`observed_at_desc`、`observed_at_asc`、`reset_at_asc`、`provider_asc`、`label_asc`。`reset_at_asc` 使用响应中的 `earliest_reset_at`，`null` 排在最后。 |

`summary` 和 `facets` 基于完整筛选结果计算，不受分页影响；`global_summary` 基于全部可见凭证且不受列表筛选影响。`needs_attention` 对非 healthy、非 fresh、partial 或 failed 凭证按凭证去重计数。

```json
{
  "items": [],
  "total": 0,
  "limit": 50,
  "offset": 0,
  "sort": "risk_desc",
  "generated_at": "2026-07-16T01:00:00Z",
  "summary": {
    "total_credentials": 0,
    "healthy": 0,
    "low": 0,
    "exhausted": 0,
    "unknown": 0,
    "error": 0,
    "unsupported": 0,
    "stale": 0,
    "never": 0,
    "collecting": 0,
    "needs_attention": 0,
    "last_observed_at": null
  },
  "global_summary": {
    "total_credentials": 0,
    "healthy": 0,
    "low": 0,
    "exhausted": 0,
    "unknown": 0,
    "error": 0,
    "unsupported": 0,
    "stale": 0,
    "never": 0,
    "collecting": 0,
    "needs_attention": 0,
    "last_observed_at": null
  },
  "facets": {
    "providers": [],
    "quota_statuses": [],
    "freshness": [],
    "sources": [],
    "credential_statuses": [],
    "collection_statuses": []
  }
}
```

### GET `/quota/credentials/:credential_id`

返回与列表相同的 credential 核心对象、全部当前窗口、collection 元数据和 `generated_at`。当前凭证为 unknown 或 unsupported 时返回 HTTP `200`；不存在、已删除或不可见凭证返回 `404`。

```json
{
  "credential": {
    "credential_id": "auth-db-id",
    "provider": "codex",
    "quota_status": "low",
    "earliest_reset_at": "2026-07-16T01:10:00Z",
    "primary_windows": [],
    "window_count": 1
  },
  "windows": [],
  "collection": {
    "source": "response_header",
    "freshness": "fresh",
    "status": "success",
    "observed_at": "2026-07-16T01:00:00Z",
    "expires_at": "2026-07-16T01:30:00Z",
    "last_attempt_at": "2026-07-16T01:00:00Z",
    "last_success_at": "2026-07-16T01:00:00Z",
    "next_probe_at": "2026-07-16T01:30:00Z",
    "consecutive_failures": 0,
    "error": null
  },
  "generated_at": "2026-07-16T01:01:00Z"
}
```

非法筛选、排序或分页返回 `400`，错误体为 `{"error":{"code":"INVALID_FILTER","message":"...","request_id":"","retryable":false}}`；凭证不存在返回 `404`；临时数据库/上下文不可用返回 `503`，其他数据库读取失败返回 `500`。

### 用量观测接口通用约定

这些接口读取持久化 `usage`、`billing_charge`、`api_key`、`user` 和 `auth` 数据。响应不会返回 raw client access key、provider API key、OAuth token、cookie、authorization header、完整 payload 或完整失败 body。允许返回 `api_key_masked`、脱敏 `body_preview` 和 payload summary。

汇总范围参数适用于 `/usage/overview`、`/usage/aggregates`、`/usage/realtime`、`/usage/health/providers`、`/usage/health/credentials`，也作为 `/usage/records` 和 `/usage/export` 的基础范围。所有 usage 范围统一采用半开区间 `[from,to)`：包含 `from`，不包含 `to`。仅日期形式的 `to` 会归一化为下一个本地午夜，因此即使跨越 DST，也会完整包含所选日历日。`/usage/overview` 和 `/usage/aggregates` 在缺少 `from` 或 `to` 时会自动补齐最近 24 小时窗口；`/usage/records` 和 `/usage/export` 不自动补齐时间范围。

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `from` | string | `/usage/overview` 和 `/usage/aggregates` 默认为 `to - 24h`；其他接口无 | 起始时间，支持 `YYYY-MM-DD`、RFC3339 或 Unix 秒；只有日期时按 `timezone` 解释为当天 00:00:00。 |
| `to` | string | `/usage/overview` 和 `/usage/aggregates` 默认为当前时间；其他接口无 | 不包含的结束时间，支持 `YYYY-MM-DD`、RFC3339 或 Unix 秒；只有日期时使用下一个本地午夜作为排他边界，从而按 `timezone` 完整包含当天。 |
| `timezone` | string | `UTC` | 用于日期型 `from`/`to` 和 `day`/`week` 趋势桶的统计时区。 |
| `provider` | string | 无 | Provider 精确筛选。 |
| `model` | string | 无 | 模型模糊筛选。 |
| `credential_type` | string | 无 | 执行凭证类型：`provider_api_key`、`oauth`、`file_auth`、`vertex`、`unknown`。 |
| `home_ip` | string | 无 | Home 节点 IP。 |
| `endpoint` | string | 无 | endpoint 模糊筛选。 |

金额字段使用当前 Home billing 系统中的 credits/points 口径。启用 billing 且 usage 可归因到 `billing_charge` 时，`amount` 或 `total_amount` 返回数值，`currency` 返回 `credits`，`billing_basis` 返回 `billing_charge`；无法可靠归因时金额返回 `null`，不会伪造估算金额。

`UsageRecordSummary` 关键字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `upstream_request_id` | string/null | payload 中可解析到的上游 request ID。 |
| `event_type` | string/null | 事件类型，来自 payload 字段或由 endpoint 派生。 |
| `upstream_status_code` | integer/null | 从结构化 usage 列或 payload 字段解析出的上游状态码。 |
| `source` | string/null | usage payload 的来源。 |
| `service_tier` | string/null | usage payload 的 service tier。 |
| `reasoning_effort` | string/null | usage payload 的 reasoning effort。 |
| `client.client_ip` | string/null | payload 中可关联的调用方 IP；不会被当作 CPA 节点 IP。 |
| `credential.api_key_preview` | string/null | 仅 provider API key 可用时返回脱敏 preview；不会返回 raw key。 |
| `billing.balance_before` / `billing.balance_after` | number/null | 关联 `billing_charge` 时的扣款前后余额。 |
| `runtime.home_ip` / `runtime.home_port` | string/integer/null | 写入 usage 的 Home 节点标识。 |
| `runtime.cpa_node_id` / `runtime.cpa_ip` / `runtime.cpa_port` / `runtime.cpa_label` | mixed | CPA 归属信息；CPA payload 未上报时，Home 会从可信 RESP/mTLS 运行时身份补齐 CPA node ID/IP。 |
| `runtime.request_log_available` | boolean | request log 是否已存在于本地，或可通过已配置的集群转发下载。远端可用表示路由可达，不保证远端文件一定存在。 |
| `runtime.log_home_ip_required` | boolean | 下载 request log 时是否必须带上 Home IP。 |

### GET `/usage/overview`

返回请求用量总览，包括范围、短窗口 live snapshot、总量、趋势、默认 top groups 和 activity 桶。

Query 参数除汇总范围参数外还支持：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `interval` | string | `auto` | `minute`、`hour`、`day`、`week` 或 `auto`。`day` 和 `week` 会按 `timezone` 切桶，响应时间仍为 UTC RFC3339。响应最多包含 10,000 个趋势桶；`auto` 会在需要时自动提升粒度，显式 interval 超出限制时返回 `400 invalid_interval_range`。 |

输出顶层字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `range` | object | 应用后的时间范围、timezone 和 interval。 |
| `live` | object | 最近短窗口 RPM、TPM、错误率和延迟。 |
| `totals` | object | 请求数、成功/失败数、token、金额、延迟和活跃主体数量。 |
| `trend` | array | 与已应用半开区间相交的连续 `interval` 趋势桶，包含请求数为零的时间桶；首尾桶可能是不完整桶。 |
| `cost_breakdown` | array | 当前不伪造不可拆分的费用明细，无法可靠拆分时为空数组。 |
| `model_efficiency` | array | 按总 token 排序的模型效率列表。 |
| `top` | object | `users`、`client_keys`、`credentials`、`providers`、`models`、`endpoints` 和 `errors`。 |
| `activity` | array | 与连续趋势桶一一对齐的健康活动序列。请求数为零时 `status` 为 `empty`；错误率低于 5% 为 `healthy`；5%（含）至 50%（不含）为 `degraded`；达到 50% 为 `unavailable`。 |

### GET `/usage/records`

返回请求明细表，服务端分页、筛选和排序。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `limit` | integer | `50` | 最大 `200`。 |
| `offset` | integer | `0` | page offset。 |
| `sort` | string | `timestamp_desc` | 支持 `timestamp_desc`、`timestamp_asc`、`tokens_desc`、`tokens_asc`、`cost_desc`、`cost_asc`、`latency_desc`、`latency_asc`、`failed_first`。 |
| `search` | string | 无 | request ID、provider、model、endpoint、Home IP、CPA node ID/IP/label、username、masked key、credential label 的宽松搜索。 |
| `status` | string | 无 | `success` 或 `failed`。 |
| `status_code` | integer | 无 | HTTP/失败状态码；2xx/3xx 会匹配成功请求，其他值匹配 `fail_status_code`。 |
| `request_id` | string | 无 | request ID 精确筛选。 |
| `event_type` | string | 无 | 事件类型筛选，常见值为 `completion`、`response`、`message`、`embedding`、`stream`。 |
| `cpa_node` | string | 无 | 按结构化 CPA node ID、CPA IP、CPA label、CPA port 做模糊筛选。 |
| `user` / `user_id` | string / integer | 无 | 用户名或用户 ID。 |
| `client_key` / `client_key_id` | string / integer | 无 | client access key 的 masked/label/ID 筛选。 |
| `credential_id` / `auth_index` | string | 无 | 执行凭证筛选。 |
| `executor_type` | string | 无 | executor type 精确筛选。 |
| `min_latency_ms` / `max_latency_ms` | integer | 无 | 延迟范围。 |
| `min_amount` / `max_amount` | number | 无 | billing amount 范围。 |

响应包含 `items`、`total`、`limit`、`offset`、`sort` 和 `sortable_fields`。`items[]` 为脱敏后的请求摘要，包含 `tokens`、`performance`、`client`、`credential`、`billing`、`runtime` 和可选 `error`。

### GET `/usage/records/:id`

返回单条 usage 详情。`id` 为 usage ID。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `include_payload` | boolean | `false` | 返回脱敏 payload summary。 |
| `include_logs` | boolean | `false` | 找到本地 request log 时返回最多 20 行脱敏日志片段；远端节点或文件不存在时返回空数组。 |

响应包含 `record`、`payload_summary`、`log_excerpt` 和 `related`。`payload_summary` 只包含 `method`、`stream`、`message_count`、`tool_count`，不会返回原始 payload。`related.request_log` 包含 `request_id`、`home_ip`、`home_port`、`available` 和 `download_url`，本地文件与远端转发的可用性语义与请求事件接口一致。

### GET `/usage/aggregates`

返回服务端全量排序后的聚合排行。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `group_by` | string | 必填 | `user`、`client_key`、`credential`、`provider`、`model`、`endpoint`、`home_ip`、`executor_type`、`status_code`。 |
| `metric` | string | `request_count` | `request_count`、`total_tokens`、`total_amount`、`failed_count`、`avg_latency_ms`、`p95_latency_ms`。 |
| `direction` | string | `desc` | `desc` 或 `asc`。 |
| `limit` | integer | `20` | 最大 `100`。 |
| `offset` | integer | `0` | page offset。 |

响应包含 `group_by`、`metric`、`direction`、`items`、`total`、`limit`、`offset` 和 `sortable_metrics`。

### GET `/usage/export`

按当前 records 筛选导出脱敏后的请求记录。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `format` | string | `csv` | `csv` 或 `jsonl`。 |
| records filters | mixed | 无 | 与 `GET /usage/records` 相同。未显式传 `limit` 时默认最多导出 `10000` 条；显式 `limit` 也最多 `10000` 条。 |

响应为 attachment。CSV 使用 `text/csv; charset=utf-8`，JSONL 使用 `application/x-ndjson`。

导出字段是展平后的脱敏摘要，除 records 响应中的核心字段外，还包含 `error_status_code`、`error_message`、`error_body_preview`、`request_log_available` 和 `log_home_ip_required`。

### GET `/request-events`

返回面向管理界面的请求事件列表。该接口是 DB-backed、非破坏性只读接口，数据来源为持久化 usage observability records，不读取也不消费 `/usage-queue`。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `from` / `to` / `timezone` | string | 无 / `UTC` | 与 `/usage/records` 相同。 |
| `limit` / `offset` | integer | `50` / `0` | 服务端分页，`limit` 最大 `200`。 |
| `sort` | string | `timestamp_desc` | 支持 `timestamp_desc`、`timestamp_asc`、`latency_desc`、`latency_asc`、`tokens_desc`、`tokens_asc`、`cost_desc`、`cost_asc`、`failed_first`。 |
| `search` | string | 无 | request ID、provider、model、endpoint、Home IP、username、masked key、credential label 的宽松搜索。 |
| `request_id` | string | 无 | request ID 精确筛选。 |
| `event_type` | string | 无 | 事件类型筛选。当前由 payload 中的 `event_type`/`type` 或 endpoint 派生，常见值为 `completion`、`response`、`message`、`embedding`、`stream`。 |
| `status` / `status_code` | string / integer | 无 | `success`、`failed` 或状态码筛选。 |
| `provider` / `model` | string | 无 | Provider 精确筛选，model 模糊筛选。 |
| `home_ip` | string | 无 | Home 节点筛选。 |
| `cpa_node` | string | 无 | 按结构化 CPA node ID、CPA IP、CPA label、CPA port 做模糊筛选；CPA payload 未上报时，Home 会尽量从可信 RESP/mTLS 运行时身份补齐。 |
| `credential_id` / `auth_index` | string | 无 | 执行凭证筛选。 |
| `user` | string | 无 | 用户名或用户 ID 搜索。 |
| `client_key` | string | 无 | client access key 的 masked/label/ID 搜索。 |
| `min_latency_ms` / `max_latency_ms` | integer | 无 | 延迟范围。 |

响应包含 `items`、`total`、`limit`、`offset` 和 `sort`。`items[]` 为请求事件对象，关键字段包括：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 稳定事件 ID，格式为 `evt_<usage_id>`。 |
| `event_type` | string | 事件类型，优先来自 payload，缺失时由 endpoint 派生。 |
| `status` / `failed` / `status_code` / `upstream_status_code` | mixed | 请求成功/失败和 HTTP 状态。成功请求默认 `status_code=200`。 |
| `provider` / `model` / `original_model` / `model_alias` / `endpoint` | mixed | 模型和路由信息。 |
| `runtime.home_ip` / `runtime.home_port` / `runtime.home_id` | mixed | 写入 usage 的 Home 节点；有端口时 `home_id` 为 `home_ip:home_port`。 |
| `runtime.cpa_node_id` / `runtime.cpa_ip` / `runtime.cpa_port` / `runtime.cpa_label` | mixed | CPA 归属信息；CPA payload 未上报时，Home 会从可信 RESP/mTLS 运行时身份补齐 CPA node ID/IP。 |
| `credential` | object | 执行凭证类型、ID、auth index、provider、label、source 和脱敏 `api_key_preview`。 |
| `client` | object | 用户、client key ID/label、脱敏 `client_key_masked` 和 client IP。 |
| `error` | object | 脱敏错误状态、上游状态、原因、消息和 body preview。 |
| `tokens` / `performance` / `billing` | object | token、latency/TTFT/TPS 和 billing 关联信息。 |
| `related.request_log` | object | request log 关联信息；可用时包含 `home_ip` 和 `home_port`。本地可用性按文件系统精确检查；远端 Home 在集群转发已配置时返回 `available=true` 和 download URL，表示可以尝试下载。 |

### GET `/request-events/filter-options`

返回请求事件筛选 UI 所需的紧凑选项列表。该接口接受与 `GET /request-events` 相同的筛选参数，忽略分页参数，并从筛选后的结果集中返回去重值。

响应字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `event_types` | array | 去重后的事件类型。 |
| `providers` | array | 去重后的 provider。 |
| `models` | array | 去重后的 model。 |
| `home_ips` | array | 去重后的 Home IP。 |
| `cpa_nodes` | array | 去重后的 CPA label、node ID 或 IP。 |
| `status_codes` | array | 去重后的 HTTP/上游状态码，以字符串返回，便于前端 select 控件使用。 |

### GET `/request-events/:id`

返回单条请求事件详情。`id` 接受 `evt_<usage_id>` 或原始 usage ID。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `include_payload` | boolean | `false` | 返回脱敏 payload summary；不会返回原始 payload。 |
| `include_logs` | boolean | `false` | 找到本地 request log 时返回最多 20 行脱敏日志片段。 |
| `include_related` | boolean | `false` | 兼容参数；事件对象始终包含 `related`。 |

响应包含 `event`、`payload_summary` 和 `log_excerpt`。`event` 与列表项同形；`payload_summary.body_preview` 当前固定为 `null`，避免暴露 request body。日志片段只读取本地文件；远端日志应通过 `related.request_log.download_url` 下载。

### GET `/request-events/export`

按当前筛选条件导出请求事件。支持 `format=csv` 和 `format=jsonl`，响应为 attachment，文件名分别为 `request-events.csv` 或 `request-events.jsonl`。

该接口接受与 `GET /request-events` 相同的筛选和排序参数，但忽略分页参数，最多导出 `10000` 条。导出字段是列表事件对象的展平脱敏摘要，包含 Home/CPA、credential、client、error、tokens、performance、billing 和 request log 关联字段。

### GET `/usage/realtime`

返回短窗口实时快照，适合管理面板短轮询。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `window_seconds` | integer | `900` | 统计窗口。 |
| `bucket_seconds` | integer | `60` | velocity bucket 大小。 |
| `group_by` | string | `model` | `model`、`provider`、`client_key`、`credential`。 |

除上述参数外支持汇总范围参数。响应包含 `velocity`、`latency_distribution` 和按 `group_by` 聚合的 `current_usage`。

### GET `/usage/health/providers`

返回 provider 维度最近窗口健康状态。支持 `window_seconds` 和汇总范围参数。

`items[]` 包含 `id`、`label`、`status`、`provider`、`recent_success_count`、`recent_failed_count`、`recent_error_rate`、`last_error_at`、`last_error_status`、`last_error_message`、`next_retry_at`、`avg_latency_ms` 和 `p95_latency_ms`。`next_retry_at` 来自执行凭证的 retry/cooldown 元数据，无法归因时为 `null`。`status` 为 `healthy`、`degraded`、`unavailable` 或 `unknown`。

### GET `/usage/health/credentials`

返回执行凭证维度最近窗口健康状态。参数与 provider health 相同，响应 `subject` 为 `credential`，`items[].credential_type` 来自 usage/auth 元数据。凭证 metadata 标记为 `disabled` 或 `unavailable` 时，`status` 优先返回该状态。

### GET `/request-logs`

返回 request log index。索引基于 usage 记录生成；本地记录在当前 Home 文件系统中检查，远端记录在集群转发已配置时标记为可路由。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `request_id` | string | 无 | request ID 筛选。 |
| `home_ip` | string | 无 | Home 节点筛选。 |
| `from` / `to` | string | 无 | 时间范围。 |
| `provider` / `model` | string | 无 | Provider/model 筛选。 |
| `status` / `status_code` | string / integer | 无 | 状态筛选。 |
| `limit` / `offset` | integer | `50` / `0` | 分页。 |
| `search` | string | 无 | 对 request ID、model、provider 和 status 做 DB 侧宽松搜索；纯数字时间戳或 `.log` 文件名搜索会在最多 `10000` 条基础记录内匹配本地文件名。 |

`items[]` 包含 `id`、`request_id`、`timestamp`、`home_ip`、`home_port`、`file_name`、`size_bytes`、`available`、`provider`、`model`、`status` 和 `download_url`。本地文件会返回精确的可用性、文件名和大小。远端记录在集群转发已配置时返回 `available=true` 和非空 `download_url`；当前 Home 不读取远端文件系统，因此 `file_name` 和 `size_bytes` 可以为 `null`。实际下载使用 `GET /request-log-by-id/:id`，有 `home_port` 时 URL 会同时携带 `home_ip` 和 `home_port`。下载请求仍是最终结果：远端文件已删除时可返回 `404`，目标 Home 不可用时可返回 `502`。

### GET `/usage-queue`

弹出最早排队的 usage records。

Query 参数：

| Query | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `count` | positive integer | `1` | 要弹出的记录数量。 |

输出示例：

```json
[
  {
    "request_id": "req-1",
    "executor_type": "CodexWebsocketsExecutor",
    "model": "gpt-5.5",
    "endpoint": "/v1/responses",
    "failed": false
  }
]
```

### GET `/logs`

返回数据库 `log` 表中的应用日志记录。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `home_ip` | string | 可选 Home node IP 过滤条件。 |
| `client_ip` | string | 可选 CPA client IP 过滤条件。 |
| `request_id` | string | 可选 request ID 过滤条件。 |
| `level` | string | 可选日志级别过滤条件。 |
| `after` | integer 或 RFC3339 | 可选 timestamp 下界。 |
| `before` | integer 或 RFC3339 | 可选 timestamp 上界。 |
| `limit` | integer | 最大返回记录数；默认 `100`，最大 `1000`。 |
| `offset` | integer | 分页偏移量；默认 `0`。 |

输出示例：

```json
{
  "logs": [
    {
      "id": 1,
      "timestamp": "2026-05-29T01:02:03Z",
      "client_ip": "10.0.0.5",
      "request_id": "req-1",
      "home_ip": "192.0.2.10",
      "level": "warn",
      "line": "[2026-05-29 09:02:03] [req-1] [warn] message",
      "created_at": "2026-05-29T01:02:04Z"
    }
  ],
  "total": 1,
  "limit": 100,
  "offset": 0
}
```

### DELETE `/logs`

删除轮转日志文件，并截断当前活跃日志。

输入：无。

输出示例：

```json
{
  "success": true,
  "message": "Logs cleared successfully",
  "removed": 3
}
```

### GET `/request-error-logs`

当详细 request logging 禁用时列出 `error-*.log` 文件；详细 request logging 启用时返回空列表。

输入：无。

输出示例：

```json
{
  "files": [
    {
      "name": "error-2026-05-27.log",
      "size": 2048,
      "modified": 1779876000
    }
  ]
}
```

### GET `/request-error-logs/:name`

下载 request error log 文件。

Path 参数：

| Path | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | 文件名必须以 `error-` 开头并以 `.log` 结尾；拒绝 slash。 |

输出：文件附件。

### GET `/request-log-by-id/:id`

从对应 Home 本机 `logs` 目录下载 request log 文件。`home_ip` 用来指明文件属于哪台 Home，可选 `home_port` 用于区分共享同一 IP 的多个 Home 节点；当目标不是当前 Home 时，当前 Home 会通过内部 mTLS-only cluster route 转发到目标 Home。文件按 request ID 匹配，文件系统仍是事实来源，所以文件已被删除时返回 `404`。

Path 参数：

| Path | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | Request ID；拒绝 slash。 |

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `home_ip` | string | 必填 request log 所属 Home node IP。 |
| `home_port` | integer | 可选 Home node 端口。多个 Home 可能共享同一 IP 时建议传入。 |

输出：文件附件。

## 模型

### GET `/models?scope=available|static`

从当前 runtime registry 或静态模型目录返回模型定义。

每个模型都包含 `providers`，其值是 `usage.provider` 和 Billing 模型价格规则使用的权威
provider 标识。客户端必须按每个 `(provider, model)` 组合生成一个候选项，不能从 `type`、
`owned_by` 或静态目录分组名称推导 Billing provider。
如果模型记录缺少 `providers` 或数组为空，客户端必须忽略该记录，因为它没有安全的 Billing
匹配标识。

Provider 实现必须把所有可能写入 `usage.provider` 的 executor 标识，以相同的归一化 provider
key 注册到 runtime registry。新增或重命名 executor 标识时，必须同步更新 registry 映射和本模型
契约。

Query 参数：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `scope` | string | 否 | `available` 返回当前 active credentials 已注册的模型；`static` 返回静态模型定义。默认：`available`。Alias：`source`、`mode`、`type`。 |
| `channel` | string | 否 | 仅用于静态模型，筛选单个 channel。Alias：`provider`。 |

支持的 `scope` alias：

| 值 | 行为 |
| --- | --- |
| `available`, `active`, `current` | 返回当前 runtime 可用模型。 |
| `static`, `all-static`, `definitions` | 返回静态模型定义。 |

可用模型输出示例：

```json
{
  "scope": "available",
  "models": [
    {
      "id": "gpt-5.5",
      "object": "model",
      "created": 1704067200,
      "owned_by": "openai",
      "type": "openai",
      "providers": ["codex"],
      "display_name": "GPT-5.5"
    }
  ]
}
```

静态模型输出示例：

```json
{
  "scope": "static",
  "models": {
    "codex-pro": [
      {
        "id": "gpt-5.5",
        "object": "model",
        "created": 1704067200,
        "owned_by": "openai",
        "type": "openai",
        "providers": ["codex"],
        "display_name": "GPT-5.5"
      }
    ]
  }
}
```

### GET `/model-definitions/:channel`

返回指定 channel 的静态模型 metadata。
返回的模型记录同样包含上文所述的权威 `providers` 字段。

支持的 channel：

```text
claude
gemini
vertex
codex
codex-free
codex-team
codex-plus
codex-pro
kimi
antigravity
xai
x-ai
grok
```

Path 或 query 参数：

| Path/query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel` | string | 是 | Channel 名称。`x-ai` 和 `grok` 是 `xai` 的 alias。 |

输出示例：

```json
{
  "channel": "codex",
  "models": [
    {
      "id": "gpt-5.5",
      "object": "model",
      "created": 1704067200,
      "owned_by": "openai",
      "type": "openai",
      "display_name": "GPT-5.5",
      "name": "gpt-5.5",
      "version": "gpt-5.5",
      "description": "",
      "inputTokenLimit": 0,
      "outputTokenLimit": 0,
      "supportedGenerationMethods": [],
      "context_length": 0,
      "max_completion_tokens": 0,
      "supported_parameters": [],
      "supportedInputModalities": ["TEXT"],
      "supportedOutputModalities": ["TEXT"],
      "thinking": {
        "min": 0,
        "max": 24576,
        "zero_allowed": true,
        "dynamic_allowed": true,
        "levels": ["low", "medium", "high"]
      }
    }
  ]
}
```

未知 channel 输出：

```json
{ "error": "unknown channel", "channel": "bad-channel" }
```

## Channel Groups

Channel groups 限制某个客户端 API key 可以使用哪些 auth records。如果客户端 API key 的 `channels` 是空数组，则不应用 channel-group 过滤。

### GET `/channel-groups`

输出示例：

```json
{
  "channel_groups": [
    {
      "id": 1,
      "channel_name": "team-a",
      "disabled": false,
      "enabled": true,
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/channel-groups/:id`

返回单个 channel group：

```json
{
  "channel_group": {
    "id": 1,
    "channel_name": "team-a",
    "disabled": false,
    "enabled": true,
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/channel-groups`

创建 channel group。

输入示例：

```json
{
  "channel_name": "team-a",
  "disabled": false
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel_name` | string | 是 | Group 名称；也接受 `name`。 |
| `disabled` | boolean | 否 | 禁用状态，默认 `false`。 |
| `enabled` | boolean | 否 | `disabled` 的反向 alias；如果同时传入，二者必须一致。 |

输出：`{ "channel_group": ... }`。

### PUT/PATCH `/channel-groups/:id`

更新 channel group。请求字段同 `POST /channel-groups`，所有字段均可选。

输出：`{ "channel_group": ... }`。

### DELETE `/channel-groups/:id`

软删除 group 及其 details。

输出：

```json
{ "status": "ok" }
```

### GET `/channel-group-details`

列出 channel group detail rows。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `channel_group_id` | integer | 按 group ID 过滤；也接受 `channel-group-id`、`group_id`、`group-id`。 |
| `auth_id` | string | 按 auth ID 过滤；也接受 `auth-id`。 |

输出示例：

```json
{
  "channel_group_details": [
    {
      "id": 10,
      "channel_group_id": 1,
      "auth_id": "auth-db-id",
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/channel-group-details/:id`

返回单个 detail row：

```json
{
  "channel_group_detail": {
    "id": 10,
    "channel_group_id": 1,
    "auth_id": "auth-db-id",
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/channel-group-details`

创建 channel group 到 auth 的绑定。

输入示例：

```json
{
  "channel_group_id": 1,
  "auth_id": "auth-db-id"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel_group_id` | integer | 是 | 已存在的 channel group ID。 |
| `auth_id` | string | 是 | Auth record ID。 |

输出：`{ "channel_group_detail": ... }`。

### PUT/PATCH `/channel-group-details/:id`

更新 detail row。

输入示例：

```json
{
  "channel_group_id": 2,
  "auth_id": "other-auth-id"
}
```

所有字段均可选；如果传入 `channel_group_id`，必须大于 `0`。

输出：`{ "channel_group_detail": ... }`。

### DELETE `/channel-group-details/:id`

软删除 detail row。

输出：

```json
{ "status": "ok" }
```

## Model Groups

Model groups 限制某个客户端 API key 可以使用哪些模型 ID。如果客户端 API key 的 `model_groups` 是空数组，则不应用模型过滤。

### GET `/model-groups`

输出示例：

```json
{
  "model_groups": [
    {
      "id": 1,
      "group_name": "premium-models",
      "disabled": false,
      "enabled": true,
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/model-groups/:id`

返回单个 model group：

```json
{
  "model_group": {
    "id": 1,
    "group_name": "premium-models",
    "disabled": false,
    "enabled": true,
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/model-groups`

创建 model group。

输入示例：

```json
{
  "group_name": "premium-models",
  "disabled": false
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `group_name` | string | 是 | Group 名称；也接受 `name`。 |
| `disabled` | boolean | 否 | 禁用状态，默认 `false`。 |
| `enabled` | boolean | 否 | `disabled` 的反向 alias；如果同时传入，二者必须一致。 |

输出：`{ "model_group": ... }`。

### PUT/PATCH `/model-groups/:id`

更新 model group。请求字段同 `POST /model-groups`，所有字段均可选。

输出：`{ "model_group": ... }`。

### DELETE `/model-groups/:id`

软删除 model group 及其 details。

输出：

```json
{ "status": "ok" }
```

### GET `/model-group-details`

列出 model group detail rows。

Query 参数：

| Query | 类型 | 说明 |
| --- | --- | --- |
| `model_group_id` | integer | 按 model group ID 过滤；也接受 `model-group-id`、`group_id`、`group-id`。 |
| `model_id` | string | 按 model ID 过滤；也接受 `model-id`。 |

输出示例：

```json
{
  "model_group_details": [
    {
      "id": 10,
      "model_group_id": 1,
      "model_id": "gpt-5.5",
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/model-group-details/:id`

返回单个 detail row：

```json
{
  "model_group_detail": {
    "id": 10,
    "model_group_id": 1,
    "model_id": "gpt-5.5",
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/model-group-details`

创建 model group 到 model ID 的绑定。

输入示例：

```json
{
  "model_group_id": 1,
  "model_id": "gpt-5.5"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `model_group_id` | integer | 是 | 已存在的 model group ID。 |
| `model_id` | string | 是 | 该 group 允许的模型 ID。 |

输出：`{ "model_group_detail": ... }`。

### PUT/PATCH `/model-group-details/:id`

更新 detail row。

输入示例：

```json
{
  "model_group_id": 2,
  "model_id": "gpt-5.5-mini"
}
```

所有字段均可选；如果传入 `model_group_id`，必须大于 `0`。

输出：`{ "model_group_detail": ... }`。

### DELETE `/model-group-details/:id`

软删除 detail row。

输出：

```json
{ "status": "ok" }
```

## OAuth 模型规则

### `/oauth-excluded-models`

GET 输出：

```json
{
  "oauth-excluded-models": {
    "claude": ["claude-opus-4.5"]
  }
}
```

PUT 输入：

```json
{
  "claude": ["claude-opus-4.5"]
}
```

或：

```json
{
  "items": {
    "claude": ["claude-opus-4.5"]
  }
}
```

PATCH 输入：

```json
{
  "provider": "claude",
  "models": ["claude-opus-4.5"]
}
```

DELETE query：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `provider` | string | 是 | 要移除的 provider key。 |

写入成功返回 `{ "status": "ok" }`。

### `/oauth-model-alias`

GET 输出：

```json
{
  "oauth-model-alias": {
    "claude": [
      { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
    ]
  }
}
```

PUT 输入：

```json
{
  "claude": [
    { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
  ]
}
```

或：

```json
{
  "items": {
    "claude": [
      { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
    ]
  }
}
```

PATCH 输入：

```json
{
  "channel": "claude",
  "provider": "claude",
  "aliases": [
    { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
  ]
}
```

DELETE query：

| Query | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel` | string | 条件必填 | 要移除的 alias channel。 |
| `provider` | string | 条件必填 | `channel` 的 alias。 |

写入成功返回 `{ "status": "ok" }`。

## 配置字段参考

以下字段可被 Home YAML config 接受。`PUT /config.yaml` 接受非 credential roots；credential roots 应使用 provider-key 和 auth-file route 管理。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `host` | string | 服务绑定 host/interface。 |
| `port` | integer | 服务监听端口。 |
| `allow-host` | array of string | RESP client IP allowlist；空列表表示允许所有 host。 |
| `tls.enable` | boolean | 启用 HTTPS。 |
| `tls.cert` | string | TLS certificate 路径。 |
| `tls.key` | string | TLS private key 路径。 |
| `remote-management.allow-remote` | boolean | 为 `true` 时允许非 localhost Management API 请求。 |
| `remote-management.secret-key` | string | Management key；本地配置模式下明文会在启动时 hash。 |
| `remote-management.disable-control-panel` | boolean | 禁用内嵌 panel routes：`/`、`/index.html`、`/management.html`、`/user.html`、`/assets/*`。 |
| `remote-management.disable-auto-update-panel` | boolean | 兼容旧配置的字段；内嵌 panel assets 不会在运行时更新。 |
| `remote-management.panel-github-repository` | string | 兼容旧配置的内嵌 panel 源仓库字段。 |
| `auth-dir` | string | 本地 auth token 目录。 |
| `proxy-url` | string | 全局出站代理 URL。 |
| `disable-image-generation` | boolean or `"chat"` | `false` 启用图像生成；`true` 全局禁用；`"chat"` 只对非 image endpoints 禁用注入。 |
| `force-model-prefix` | boolean | 对带 prefix 的凭证要求显式模型 prefix。 |
| `request-log` | boolean | 启用详细 request logging。 |
| `api-keys` | array of string | Home 接受的客户端 API keys。 |
| `passthrough-headers` | boolean | 将上游响应头透传给下游客户端。 |
| `streaming.keepalive-seconds` | integer | SSE heartbeat 间隔秒数；`<=0` 禁用。 |
| `streaming.bootstrap-retries` | integer | Streaming 首字节前重试次数；`<=0` 禁用。 |
| `nonstream-keepalive-interval` | integer | 非 streaming 响应的空行 keepalive 间隔秒数。 |
| `debug` | boolean | 启用 debug logging/features。 |
| `pprof.enable` | boolean | 启用 pprof server。 |
| `pprof.addr` | string | pprof listen address。 |
| `commercial-mode` | boolean | 高并发下减少高开销 middleware 行为。 |
| `logging-to-file` | boolean | 将 app logs 写入文件而非 stdout。 |
| `logs-max-total-size-mb` | integer | 日志文件总大小上限；`0` 禁用清理。 |
| `error-logs-max-files` | integer | request error log 文件保留数量。 |
| `plugins.enabled` | boolean | 在 Home 和下游 CPA 节点启用受信任的进程内插件。 |
| `plugins.dir` | string | 每个节点本地插件产物目录。 |
| `plugins.store-sources` | array of string | 额外插件商店 registry URL；内置官方 registry 始终包含。 |
| `plugins.configs` | object | 以插件 ID 为 key 的单插件配置。插件商店安装会在插件条目下写入固定 `store` manifest；Home-mode CPA 节点根据该 manifest 下载产物，Home 仅在显式设置 `load-in-home: true` 时下载并加载。 |
| `usage-statistics-enabled` | boolean | 启用内存 usage aggregation。Home 会向下游 CPA 强制为 `true`，并拒绝通过 Management API 关闭。 |
| `redis-usage-queue-retention-seconds` | integer | Usage queue 保留窗口；默认 `60`，最大 `3600`。 |
| `disable-cooling` | boolean | 全局禁用 quota cooldown scheduling。Home 会向下游 CPA 强制为 `true`。 |
| `auth-auto-refresh-workers` | integer | 覆盖 auth auto-refresh worker 数量。 |
| `request-retry` | integer | 失败请求重试次数。 |
| `max-retry-credentials` | integer | 一个失败请求最多尝试的凭证数量；`<=0` 表示所有可用凭证。 |
| `max-retry-interval` | integer | 重试 cooled-down credentials 前的最大等待秒数。 |
| `quota-exceeded.switch-project` | boolean | Gemini quota error 时切换 project。 |
| `quota-exceeded.switch-preview-model` | boolean | Quota error 时切换到 preview model。 |
| `quota-exceeded.antigravity-credits` | boolean | Claude 最后兜底使用 Antigravity credits。 |
| `routing.strategy` | string | `round-robin` 或 `fill-first`。 |
| `routing.claude-code-session-affinity` | boolean | 已废弃的 Claude Code session affinity flag。 |
| `routing.session-affinity` | boolean | 通用 session-sticky credential routing。 |
| `routing.session-affinity-ttl` | string | Session-to-auth binding 持续时间。 |
| `antigravity-signature-cache-enabled` | boolean pointer | 启用 Antigravity thinking signature cache validation。 |
| `antigravity-signature-bypass-strict` | boolean pointer | 控制 Antigravity signature bypass 严格程度。 |
| `gemini-api-key` | array of `GeminiKey` | Gemini API-key credentials；应使用 provider-key routes。 |
| `codex-api-key` | array of `CodexKey` | Codex API-key credentials；应使用 provider-key routes。 |
| `xai-api-key` | array of `XAIKey` | 原生 xAI API-key credentials；应使用 provider-key routes。 |
| `codex-header-defaults.user-agent` | string | 默认 Codex User-Agent。 |
| `codex-header-defaults.beta-features` | string | 默认 Codex websocket beta features header。 |
| `claude-api-key` | array of `ClaudeKey` | Claude API-key credentials；应使用 provider-key routes。 |
| `claude-header-defaults.user-agent` | string | 默认 Claude User-Agent。 |
| `claude-header-defaults.package-version` | string | 默认 Claude package version。 |
| `claude-header-defaults.runtime-version` | string | 默认 Claude runtime version。 |
| `claude-header-defaults.os` | string | 默认 Claude OS fingerprint。 |
| `claude-header-defaults.arch` | string | 默认 Claude architecture fingerprint。 |
| `claude-header-defaults.timeout` | string | 默认 Claude timeout header。 |
| `claude-header-defaults.stabilize-device-profile` | boolean pointer | 启用固定 Claude device profile baseline。 |
| `openai-compatibility` | array of `OpenAICompatibility` | OpenAI-compatible providers；应使用 provider-key routes。 |
| `vertex-api-key` | array of `VertexCompatKey` | Vertex-compatible API-key credentials；应使用 provider-key routes。 |
| `oauth-excluded-models` | object string to array of string | 每个 provider 的 OAuth/file-backed auth 排除模型。 |
| `oauth-model-alias` | object string to array of `OAuthModelAlias` | 每个 channel 的 OAuth model aliases。 |
| `oauth-model-alias.*[].force-mapping` | boolean | 为 `true` 时，响应中的 model 字段使用映射后的上游 model name。 |
| `payload.default` | array of `PayloadRule` | 设置缺失的 JSON payload params。 |
| `payload.default-raw` | array of `PayloadRule` | 设置缺失的 raw JSON payload params。 |
| `payload.override` | array of `PayloadRule` | 覆盖 JSON payload params。 |
| `payload.override-raw` | array of `PayloadRule` | 覆盖 raw JSON payload params。 |
| `payload.filter` | array of `PayloadFilterRule` | 移除 JSON payload paths。 |

Payload 嵌套结构：

```json
{
  "PayloadRule": {
    "models": [
      { "name": "gpt-*", "protocol": "responses" }
    ],
    "params": {
      "reasoning.effort": "high"
    }
  },
  "PayloadFilterRule": {
    "models": [
      { "name": "gpt-*", "protocol": "responses" }
    ],
    "params": ["metadata.debug"]
  },
  "PayloadModelRule": {
    "name": "model pattern or wildcard",
    "protocol": "translator protocol",
    "from-protocol": "source protocol",
    "headers": {
      "Header-Name": "wildcard value"
    },
    "match": [
      { "json.path": "required value" }
    ],
    "not-match": [
      { "json.path": "disallowed value" }
    ],
    "exist": ["json.path"],
    "not-exist": ["json.path"]
  }
}
```

`PayloadModelRule` 字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `name` | string | 目标 model name 或通配模式。 |
| `protocol` | string | 当前 translator protocol/provider format 匹配条件，例如 `openai`、`responses`、`gemini`、`claude`、`codex` 或 `antigravity`。 |
| `from-protocol` | string | 来源协议匹配条件，用于请求从其他协议转换而来的场景。 |
| `headers` | object string to string | 请求 header 匹配条件；配置的每个 header 都必须存在，且值需要匹配对应通配模式。 |
| `match` | array of object | payload JSON path/value 必须匹配的条件；path 使用与 payload params 相同的 gjson/sjson 风格路径语法。 |
| `not-match` | array of object | payload JSON path/value 必须不匹配的条件。 |
| `exist` | array of string | 指定 payload JSON path 必须存在且不是 `null`。 |
| `not-exist` | array of string | 指定 payload JSON path 必须不存在或为 `null`。 |
