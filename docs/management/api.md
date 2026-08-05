# CLIProxyAPIHome Cluster Management API

This document describes the current DB-backed Management API exposed by CLIProxyAPIHome. Home startup initializes a runtime database and registers the database-backed management route set used by the Home runtime.

Base URL:

```text
http://<host>:<port>/v0/management
```

Optional management panel:

```text
GET /
GET /index.html
GET /management.html
GET /user.html
GET /assets/*
```

The panel assets are embedded into the binary at build time.

Home examples usually use port `8327`. The effective listen address comes from runtime config, `cluster.yaml`, or the final `-addr` value.

## Runtime Model

Home management state is stored in the database-backed cluster repository. When `cluster.yaml` is present, the repository uses the configured backend, such as PostgreSQL or SQLite. When no cluster config is present, Home still opens a local SQLite runtime database and uses the same DB-backed management handlers.

The route list below is the database-backed route set registered by `cmd/home` through `WithDatabaseManagement`.

## Authentication

Every `/v0/management/*` route requires a management key.

Supported request headers:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <MANAGEMENT_KEY>` or raw `<MANAGEMENT_KEY>` |
| `X-Management-Key` | `<MANAGEMENT_KEY>` |

Access rules:

| Rule | Behavior |
| --- | --- |
| Local requests | Still require a valid management key. |
| Remote requests | Require remote management to be enabled, such as `remote-management.allow-remote: true`, or an internal override. |
| API disabled | If neither `remote-management.secret-key` nor `MANAGEMENT_PASSWORD` is set, Management API routes normally return `404`. |
| Failed-auth ban | The same client IP is banned for 30 minutes after 5 consecutive failed attempts. During the ban, a correct key still fails. |

Common auth errors:

```json
{ "error": "missing management key" }
{ "error": "invalid management key" }
{ "error": "remote management disabled" }
{ "error": "remote management key not set" }
{ "error": "IP banned due to too many failed attempts. Try again in 29m59s" }
```

Home also adds these response headers on management routes:

| Header | Description |
| --- | --- |
| `x-cpa-home-version` | Home build version. |
| `x-cpa-home-commit` | Home build commit. |
| `x-cpa-home-build-date` | Home build date. |
| `X-CPA-SUPPORT-PLUGIN` | `1` when the current binary was built with CGO enabled; `0` otherwise. Same semantics as CPA management API. |

## Response Conventions

Most successful write operations return:

```json
{ "status": "ok" }
```

Full config replacement returns:

```json
{ "ok": true, "changed": ["config"] }
```

DB-backed handlers usually return both a machine-readable `error` code and a human-readable `message`:

```json
{ "error": "invalid body", "message": "username is required" }
```

Other common error shapes:

```json
{ "error": "invalid body" }
{ "error": "invalid_config", "message": "validation detail" }
```

## Registered Routes

The table below is extracted from the final Home route registry built by `internal/managementhttp/server.go` for `cmd/home`.

| Method | Path |
| --- | --- |
| `GET` | `/anthropic-auth-url` |
| `GET` | `/antigravity-auth-url` |
| `POST` | `/api-call` |
| `GET` | `/api-key-usage` |
| `GET` | `/capabilities` |
| `GET` | `/quota/credentials` |
| `GET` | `/quota/credentials/:credential_id` |
| `POST` | `/quota/collect` |
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
| `GET` | `/credentials/in-flight` |
| `GET` | `/credentials/in-flight/summary` |
| `GET` | `/credentials/concurrency-policies` |
| `GET` | `/credentials/concurrency` |
| `GET` | `/credentials/:credential_id/concurrency-policy` |
| `PATCH` | `/credentials/:credential_id/concurrency-policy` |
| `DELETE` | `/credentials/:credential_id/cooldown` |
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

## Config APIs

### GET `/config`

Returns the current runtime config as JSON.

Input: none.

Example response:

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

Fields with `json:"-"` are not returned. Home hides `host`, `port`, `allow-host`, `remote-management`, and `auth-dir` from this JSON response.

#### Credential concurrency lifecycle fields

The `credential-concurrency` object is included in both config responses. Home owns `lifecycle-config-revision` and `observation-barrier-revision`: clients may send them, but Home derives both values from singleton records. The observation barrier starts at `0`, increases monotonically when policies change, and Home publishes its current value. Neither revision can be changed through YAML import or Management API updates.

| Field | Default | Description |
| --- | --- | --- |
| `lifecycle-config-revision` | `1` on initialization | Home-owned monotonic lifecycle revision. |
| `observation-barrier-revision` | `0` on initialization | Home-owned monotonic policy observation barrier revision. |
| `cpa-heartbeat-timeout` | `3s` | CPA heartbeat timeout. |
| `cpa-cancel-bound` | `5s` | Maximum CPA cancellation bound. |
| `reclaim-grace` | `5s` | Grace period before reclaim. |
| `cleanup-interval` | `5s` | Lifecycle cleanup interval. |

All durations must be positive. Home also requires `node.heartbeat-timeout + reclaim-grace > cpa-heartbeat-timeout + cpa-cancel-bound`, rejecting duration-sum overflow as invalid. While any CPA membership is `active` or `canceling`, changes to `cpa-heartbeat-timeout`, `cpa-cancel-bound`, `reclaim-grace`, or `cleanup-interval` are rejected with `lifecycle configuration is in use`. An identical configuration is accepted as unchanged, and non-safety tuning fields may still be updated.

### GET `/config.yaml`

Returns the current YAML config.

Input: none.

Response content type:

```text
application/yaml; charset=utf-8
```

The response is reconstructed from the persisted config snapshot, so original YAML comments and formatting are not preserved.

### PUT `/config.yaml`

Replaces the full config.

Input: a complete YAML document in the request body.

Home persists non-credential roots into the config snapshot. Credential roots included in the uploaded YAML are synchronized into DB-backed auth records, while omitted credential roots are left unchanged. Send an empty list for a credential root to clear the corresponding provider-key records:

```text
auth-dir
gemini-api-key
vertex-api-key
codex-api-key
xai-api-key
claude-api-key
openai-compatibility
```

`auth-dir` is still treated as an import/export path and is not persisted into the runtime config snapshot.

A supplied `credential-concurrency` object updates the Home-owned lifecycle configuration. If it changes `cpa-heartbeat-timeout`, `cpa-cancel-bound`, `reclaim-grace`, or `cleanup-interval`, no CPA membership may be `active` or `canceling`; identical configurations and non-safety tuning changes remain allowed. Lifecycle, credential reconciliation, config snapshot replacement, and plugin task creation (when applicable) are committed atomically; any failure rolls all of them back.

Example response:

```json
{ "ok": true, "changed": ["config", "auth"] }
```

### Simple Config Leaf Routes

These routes write the corresponding config root into the cluster repository and reload the Home runtime.

| Method | Path | Input | Output |
| --- | --- | --- | --- |
| `GET` | `/debug` | none | `{ "debug": boolean }` |
| `PUT/PATCH` | `/debug` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/logging-to-file` | none | `{ "logging-to-file": boolean }` |
| `PUT/PATCH` | `/logging-to-file` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/logs-max-total-size-mb` | none | `{ "logs-max-total-size-mb": number }` |
| `PUT/PATCH` | `/logs-max-total-size-mb` | `{ "value": number }`; negative values are saved as `0` | `{ "status": "ok" }` |
| `GET` | `/error-logs-max-files` | none | `{ "error-logs-max-files": number }` |
| `PUT/PATCH` | `/error-logs-max-files` | `{ "value": number }`; negative values are saved as `10` | `{ "status": "ok" }` |
| `GET` | `/usage-statistics-enabled` | none | `{ "usage-statistics-enabled": boolean }` |
| `PUT/PATCH` | `/usage-statistics-enabled` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/proxy-url` | none | `{ "proxy-url": string }` |
| `PUT/PATCH` | `/proxy-url` | `{ "value": string }` | `{ "status": "ok" }` |
| `DELETE` | `/proxy-url` | none | `{ "status": "ok" }` |
| `GET` | `/request-log` | none | `{ "request-log": boolean }` |
| `PUT/PATCH` | `/request-log` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/request-retry` | none | `{ "request-retry": number }` |
| `PUT/PATCH` | `/request-retry` | `{ "value": number }` | `{ "status": "ok" }` |
| `GET` | `/max-retry-interval` | none | `{ "max-retry-interval": number }` |
| `PUT/PATCH` | `/max-retry-interval` | `{ "value": number }` | `{ "status": "ok" }` |
| `GET` | `/force-model-prefix` | none | `{ "force-model-prefix": boolean }` |
| `PUT/PATCH` | `/force-model-prefix` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/routing/strategy` | none | `{ "strategy": "round-robin" }` or `{ "strategy": "fill-first" }` |
| `PUT/PATCH` | `/routing/strategy` | `{ "value": "round-robin" }`, `roundrobin`, `rr`, `fill-first`, `fillfirst`, or `ff` | `{ "status": "ok" }` |
| `GET` | `/quota-exceeded/switch-project` | none | `{ "switch-project": boolean }` |
| `PUT/PATCH` | `/quota-exceeded/switch-project` | `{ "value": boolean }` | `{ "status": "ok" }` |
| `GET` | `/quota-exceeded/switch-preview-model` | none | `{ "switch-preview-model": boolean }` |
| `PUT/PATCH` | `/quota-exceeded/switch-preview-model` | `{ "value": boolean }` | `{ "status": "ok" }` |

### `/payload` Config Root

`GET /payload` returns:

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

`GET /payload` returns the complete persisted payload root, including advanced model matcher fields that older frontends may not recognize.

`PUT /payload` accepts either a raw payload object, `{ "value": <payload> }`, or `{ "payload": <payload> }`. It replaces the complete `payload` root and validates the full schema without dropping advanced matcher fields.

`PATCH /payload` accepts the same body shapes and applies object merge-patch semantics to the existing `payload` root: submitted object fields are merged recursively, `null` removes a field, arrays are replaced as whole values, and sibling fields not present in the patch are preserved. This lets clients update one section, such as `filter`, without deleting `default`, `override`, or advanced matcher fields.

`DELETE /payload` removes the root from the config snapshot.

Successful writes return:

```json
{ "status": "ok" }
```

## Nodes, Version, and Certificates

### GET `/nodes`

Lists CPA nodes currently connected to the Home cluster. When multiple Home nodes share a database, the route returns CPA connection snapshots reported by every live Home node instead of only the in-process connections on the Home node handling the request.

Input: none.

Example response:

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

| Field | Type | Description |
| --- | --- | --- |
| `nodes` | array | Authoritative active CPA list. A snapshot is returned only when it has a config subscription and its certificate fingerprint and complete Home incarnation match an active `cpa_node_membership` owner. Draining and revision-only snapshots are excluded. |
| `plugin_report_required` | boolean | Whether the current Home config expects CPA plugin reports because at least one enabled plugin has a pinned store manifest. |
| `plugin_report_statuses` | array | Latest plugin reports stored in the shared database, grouped by reporting node and report metadata. Delete reports for one plugin can coexist with preserved status rows for other plugins. These are retained until the node reports again or is explicitly cleaned up; they do not expire by TTL and are self-reported observations, not authoritative install proof. |
| `nodes[].node_id` | string | CPA node ID derived from the Home client certificate when available. |
| `nodes[].ip` | string | Node IP address. |
| `nodes[].connected_time` | string | First connection time for the active node entry. |
| `nodes[].last_seen_at` | string | Time when the serving Home last refreshed the derived `cpa_node` snapshot. |
| `nodes[].client_count` | integer | Active RESP subscription connection count from this IP. |
| `nodes[].healthy` | boolean | Whether the CPA membership heartbeat and its exact serving Home incarnation are both healthy. Plugin reports do not affect this field. |
| `nodes[].home_id` | string | Home node identity serving this CPA node, formatted as `home_ip:home_port`. |
| `nodes[].home_ip` | string | Home node IP or advertised cluster identity serving this CPA node. |
| `nodes[].home_port` | integer | Home node RESP/cluster port serving this CPA node. |
| `nodes[].plugin_report_state` | string | Current configured plugin observation state: `not_required`, `missing_report`, `reported_partial`, `reported_failed`, or `reported_ok`. Failed reports for plugins that are not currently required do not make this state failed. |
| `nodes[].plugin_report_statuses` | array | Plugin reports associated with this active node, matched by node ID when possible and IP as a fallback. |
| `plugin_report_statuses[].node_type` | string | Reporting node type, currently `cpa` for CPA node reports and reserved for `home` reports. |
| `plugin_report_statuses[].node_id` | string | CPA node ID derived from its Home client certificate. |
| `plugin_report_statuses[].status` | string | Reported plugin task status for this report group, currently `success` or `failed`. |
| `plugin_report_statuses[].phase` | string | Reported task phase for this report group, such as `install`, `load`, or `delete`. |
| `plugin_report_statuses[].ok` | boolean | Whether the node reported the task as successful. |
| `plugin_report_statuses[].plugins` | array | Per-plugin install/load/delete results belonging to this report group. |

### GET `/topology`

Returns a Home + CPA topology snapshot for the database-backed Home runtime. Active ownership comes from `cpa_node_membership`; `cpa_node` is a derived diagnostic snapshot. Subscription snapshots that do not match the membership fingerprint and complete `(HomeIP, HomePort, HomeStartedAt)` owner are shown as `draining`, not as active CPAs.

Input: none.

Example response:

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
      "state": "active",
      "health": "healthy",
      "healthy": true,
      "home_id": "10.0.0.10:8327",
      "home_ip": "10.0.0.10",
      "home_port": 8327,
      "plugin_report_state": "reported_ok",
      "plugin_report_statuses": []
    }
  ]
}
```

| Field | Type | Description |
| --- | --- | --- |
| `summary.home_count` | integer | Number of known Home nodes in the shared cluster table. |
| `summary.healthy_home_count` | integer | Home nodes whose `last_seen_at` is within `stale_after_seconds`. |
| `summary.stale_home_count` | integer | Home nodes known to the database but past the stale cutoff. |
| `summary.unknown_home_count` | integer | Home nodes whose health cannot be determined because required identity or heartbeat data is missing. |
| `summary.cpa_count` | integer | Logical CPA count, deduplicated by certificate fingerprint across active and draining snapshots. |
| `summary.healthy_cpa_count` | integer | Active CPAs whose membership heartbeat and exact Home incarnation are healthy. |
| `summary.stale_cpa_count` | integer | Active CPAs whose membership heartbeat or exact Home incarnation is stale. |
| `summary.unknown_cpa_count` | integer | Active CPAs whose exact serving Home incarnation cannot be determined. |
| `summary.plugin_attention_count` | integer | CPA nodes with missing, partial, or failed plugin reports when plugin reports are required. |
| `summary.attention_count` | integer | Combined operational attention count: stale/unknown Home nodes, CPA nodes needing attention counted once each, plus missing master. |
| `summary.missing_master` | boolean | Whether no healthy Home can currently be selected as master. |
| `summary.stale_after_seconds` | integer | Home heartbeat timeout used to classify Home health and select the current master. CPA health additionally uses each membership's CPA heartbeat timeout. |
| `summary.retention_after_seconds` | integer | Topology snapshot retention window. Records older than this are omitted from `homes[]` and `cpas[]`. |
| `management.home_id` | string | Current Management runtime Home identity, formatted as `home_ip:home_port`. |
| `management.home_ip` | string | Current Management runtime Home IP or advertised cluster identity. |
| `management.home_port` | integer | Current Management runtime Home port. |
| `master` | object/null | Currently selected healthy Home master, or `null` if no healthy master is available. |
| `homes[]` | array | Home nodes as first-class topology resources. |
| `homes[].id` | string | Home identity formatted as `ip:port`. |
| `homes[].ip` | string | Home IP or advertised cluster identity. |
| `homes[].port` | integer | Home cluster/RESP port. |
| `homes[].role` | string | `master`, `follower`, or `unknown`. |
| `homes[].is_master` | boolean | Whether this Home is the currently selected healthy master. Stale Home nodes are never marked current master. |
| `homes[].reported_master` | boolean | Last master flag reported by that Home heartbeat. |
| `homes[].health` | string | `healthy`, `stale`, or `unknown`. |
| `homes[].healthy` | boolean | Whether `homes[].health` is `healthy`. |
| `homes[].client_count` | integer | Total active CPA config subscriptions reported by that Home. |
| `homes[].started_at` | string | Home process start time. |
| `homes[].last_seen_at` | string | Last Home heartbeat stored in the shared database. |
| `homes[].cpa_count` | integer | Active CPA count owned by this exact Home incarnation. |
| `homes[].healthy_cpa_count` | integer | Healthy active CPAs owned by this Home incarnation. |
| `homes[].stale_cpa_count` | integer | Stale active CPAs owned by this Home incarnation. |
| `homes[].unknown_cpa_count` | integer | Unknown-health active CPAs owned by this Home incarnation. |
| `cpas[]` | array | Active and draining CPA diagnostic snapshots. Revision-only snapshots are omitted. |
| `cpas[].node_id` | string | CPA node ID derived from the client certificate when available. |
| `cpas[].ip` | string | CPA node IP address observed by its serving Home. |
| `cpas[].connected_time` | string | First observed active connection time for this CPA snapshot on its serving Home. |
| `cpas[].last_seen_at` | string | Last time the serving Home refreshed this derived snapshot. |
| `cpas[].client_count` | integer | Active RESP subscription count represented by this CPA snapshot. |
| `cpas[].state` | string | `active` when the subscription and authoritative membership owner match; otherwise `draining`. |
| `cpas[].health` | string | `healthy`, `stale`, or `unknown`. |
| `cpas[].healthy` | boolean | Whether `cpas[].health` is `healthy`; retained for backward compatibility. |
| `cpas[].home_id` | string | Serving Home identity, formatted as `home_ip:home_port`. |
| `cpas[].home_ip` | string | Serving Home IP or advertised cluster identity. |
| `cpas[].home_port` | integer | Serving Home cluster/RESP port. |
| `cpas[].plugin_report_state` | string | Same semantics as `nodes[].plugin_report_state`. |
| `cpas[].plugin_report_statuses` | array | Plugin reports associated with this CPA node, matched by node ID when possible and IP as a fallback. |

### GET `/latest-version`

Fetches the latest CLIProxyAPIHome release from GitHub. If `proxy-url` is configured, the request uses that proxy.

Input: none.

Example response:

```json
{ "latest-version": "v7.0.0" }
```

Common error codes:

```json
{ "error": "request_create_failed", "message": "detail" }
{ "error": "request_failed", "message": "detail" }
{ "error": "unexpected_status", "message": "status 502: detail" }
{ "error": "decode_failed", "message": "detail" }
{ "error": "invalid_response", "message": "missing release version" }
```

## Plugin Management

### GET `/plugins`

Lists plugin entries visible to the Home process. The response includes configured plugin entries and plugins currently registered by Home-loaded runtime plugins. Store-installed plugins only become registered in Home when their config explicitly enables Home loading, such as `plugins.configs.<pluginID>.load-in-home: true`.

Input: none.

Example response:

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

| Field | Type | Description |
| --- | --- | --- |
| `plugins_enabled` | boolean | Current global `plugins.enabled` value. |
| `plugins_dir` | string | Local plugin artifact directory configured for Home and CPA nodes. |
| `plugins[].configured` | boolean | True when `plugins.configs.<id>` exists in the Home config. |
| `plugins[].registered` | boolean | True when the Home process has loaded the plugin and received its runtime registration. |
| `plugins[].effective_enabled` | boolean | True only when global plugins, per-plugin enabled, and runtime registration are all active. |
| `plugins[].supports_oauth` | boolean | True when the runtime plugin registration includes an auth provider login capability. |
| `plugins[].oauth_provider` | string | Provider key used by OAuth UI and `GET /<provider>-auth-url`. |
| `plugins[].menus` | array | Reserved for plugin resource menus. Home currently returns an empty list because plugin resource routes are not exposed by Home. |
| `plugins[].metadata` | object | Plugin metadata returned by runtime registration, including display fields and config field descriptors. |

## Plugin Store

Plugin store routes list registry entries and install a selected plugin into the DB-backed Home config. Install writes `plugins.configs.<pluginID>.store` with a pinned manifest. GitHub-release installs pin the repository, version, and exact release tag; direct installs pin the version and source registry URL, then Home-mode CPA nodes resolve the current-platform artifact URL and SHA-256 from that registry during runtime config application. Store-installed plugins are not downloaded or loaded by the Home process by default; set `plugins.configs.<pluginID>.load-in-home: true` only for trusted provider/auth plugins that must run inside Home.

### GET `/plugin-store`

Lists plugin entries from the built-in official registry plus any configured `plugins.store-sources`.

Input: none.

Example response:

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

| Field | Type | Description |
| --- | --- | --- |
| `plugins_enabled` | boolean | Current global `plugins.enabled` value. |
| `plugins_dir` | string | Local plugin artifact directory configured for each node. |
| `sources` | array | Plugin store registry sources queried for the response. |
| `source_errors` | array | Per-source registry fetch errors when some sources fail. |
| `plugins[].install_type` | string | Registry install type, currently `github-release` or `direct`. |
| `plugins[].auth_required` | boolean | Registry-declared hint that this plugin source may need authentication. |
| `plugins[].platforms` | array | Platforms declared by a direct registry entry. Empty for GitHub-release entries. |
| `plugins[].installed` | boolean | True when config contains a store manifest for this plugin ID. |
| `plugins[].installed_version` | string | Version pinned in the configured manifest. |
| `plugins[].enabled` | boolean | Per-plugin `plugins.configs.<id>.enabled` value. |
| `plugins[].effective_enabled` | boolean | True only when both global plugins and this plugin are enabled. |
| `plugins[].update_available` | boolean | True when the registry version is newer than the configured manifest version. |

Common errors:

```json
{ "error": "plugin_store_source_invalid", "message": "detail" }
{ "error": "plugin_store_registry_failed", "message": "detail" }
```

### POST `/plugin-store/:id/install`

Installs a plugin config manifest from a registry entry. If multiple configured sources contain the same plugin ID, pass `?source=<source_id>`. `github-release` entries install the latest GitHub release by default; pass `version` to pin a specific release tag such as `1.0.3` or `v1.0.3`. `direct` entries write a source-backed v2 manifest; when `version` is supplied it must match either the registry entry version or an item in `versions[]`.

Input body: optional JSON.

```json
{ "version": "1.0.3" }
```

Query:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `source` | string | no | Registry source ID when the plugin ID is ambiguous across sources. |
| `version` | string | no | Plugin version to install. Values with or without a leading `v` are accepted. |

Example response:

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

Common errors:

```json
{ "error": "plugin_not_found", "message": "plugin not found in registry" }
{ "error": "plugin_store_source_required", "message": "multiple plugin store sources contain this plugin id; specify source" }
{ "error": "plugin_release_failed", "message": "detail" }
{ "error": "plugin_release_invalid", "message": "detail" }
{ "error": "plugin_manifest_invalid", "message": "detail" }
{ "error": "invalid_config", "message": "detail" }
```

### POST `/plugin-store/:id/uninstall`

Uninstalls a plugin from the whole Home/CPA cluster. The route removes the plugin store manifest from the shared Home config and creates a delete task for all CPA nodes; active Home nodes also delete their local current-platform artifact when they apply the config change.

Input body/query: none.

Example response:

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

Common errors:

```json
{ "error": "invalid_plugin_id", "message": "invalid plugin id" }
{ "error": "plugin_task_create_failed", "message": "detail" }
{ "error": "invalid_config", "message": "detail" }
```

### Plugin store credential routes

Home stores plugin store credentials encrypted in the shared database. Secrets are write-only: responses never include plaintext credentials or encrypted payloads. Creating, materially updating, or deleting a rule records a cluster event for downstream synchronization. Rules are evaluated in database creation order; the first matching rule wins. Matches must be absolute HTTPS URLs without user information, query, or fragment.

Request bodies are limited to 64 KiB, must contain exactly one JSON object, and reject unknown fields. Oversized bodies return `413`; malformed bodies return `400`.

Routes:

- `GET /plugin-store-auth` returns `200` with `{ "items": [...] }`.
- `POST /plugin-store-auth` creates a rule and returns it with `201`.
- `GET /plugin-store-auth/:id` returns one rule with `200`.
- `PATCH /plugin-store-auth/:id` partially updates a rule and returns it with `200`. Omitted or `null` secret fields retain their current value.
- `DELETE /plugin-store-auth/:id` returns `{ "status": "ok" }` with `200`.

Create example:

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

| Field | Required | Description |
| --- | --- | --- |
| `name` | yes | Non-empty display name. |
| `match` | yes | Absolute HTTPS match prefix. |
| `apply_to` | no | Any of `registry`, `metadata`, and `artifact`; empty applies to all request kinds. |
| `auth_type` | no | `none` (default), `bearer`, `basic`, `header`, or `github-token`. |
| `token` | by type | Required by `bearer` and `github-token`. |
| `username`, `password` | by type | Both required by `basic`. |
| `header_name`, `header_value` | by type | Both required by `header`; the name and value must be valid HTTP header data. |
| `enabled` | no | Defaults to `true`. |

Responses include `id`, `name`, `match`, `apply_to`, `auth_type`, optional `header_name`, `enabled`, `version`, and `credentials_configured`. Common errors are `400 invalid_request`, `404 plugin_store_auth_*_failed`, `409 plugin_store_auth_*_failed` for an update conflict, `413 invalid_request`, and `422 plugin_store_auth_invalid`.

### POST `/certificates/clients`

Creates a pending client certificate enrollment record and returns a Home JWT that a node can use to finish client-certificate enrollment.

Input: none.

Example response:

```json
{
  "id": "cert-uuid",
  "home_jwt": "eyJhbGciOi..."
}
```

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Pending client certificate ID. |
| `home_jwt` | string | Enrollment JWT containing Home target information and enrollment secret. |

Common errors:

```json
{ "error": "cluster_unavailable", "message": "cluster_unavailable" }
{ "error": "certificate_jwt_target_invalid", "message": "certificate_jwt_target_invalid" }
{ "error": "certificate_create_failed", "message": "detail" }
{ "error": "certificate_jwt_failed", "message": "detail" }
```

## Users

User records are stored in the cluster repository.

### GET `/users`

Lists users.

Input: none.

Example response:

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

Reads one user by numeric ID.

Path parameters:

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | integer | yes | `user.id`; must be greater than `0`. |

Example response:

```json
{
  "user": {
    "id": 1,
    "username": "alice",
    "password_set": true,
    "credits": 10.5,
    "credits_unlimited": false,
    "period_limits_summary": {
      "enabled_windows": ["5h", "1d"],
      "zero_limit_windows": []
    },
    "mfa": { "enabled": true },
    "passkey": [{ "id": "credential-id" }],
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

User records include `period_limits_summary`, a lightweight overview derived from the record itself (no usage queries): `enabled_windows` lists the windows whose limit is configured (`5h`/`1d`/`7d`/`30d`), and `zero_limit_windows` lists enabled windows with a `0` limit (immediately blocking). Use `GET /users/:id/period-limits` for live used/remaining data.

### POST `/users`

Creates a user.

Example request:

```json
{
  "username": "alice",
  "password": "plaintext-password",
  "credits": 10.5,
  "mfa": { "enabled": true },
  "passkey": [{ "id": "credential-id" }]
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `username` | string | yes | Username. Aliases: `user_name`, `user-name`. |
| `password` | string | no | Non-empty plaintext is stored as a bcrypt hash. Existing valid bcrypt hashes are preserved for migration. Responses do not return password material; they return `password_set`. |
| `credits` | number | no | User credit balance. Defaults to `0`. When a client API key is bound to this user and credits are `<= 0`, RESP `RPOP auth` returns `user_credits_insufficient`. For billing workflows, prefer `/billing/balance-records/recharge` and `/billing/balance-records/deduct` so balance changes have ledger records. |
| `credits_unlimited` | boolean | no | When `true`, dispatch ignores the total `credits` balance and billing charges do not deduct it. Period limits still apply. Defaults to `false`. |
| `timezone` | string | no | IANA timezone for calendar windows; default `Asia/Shanghai`. |
| `limit_5h_credits` | number/null | no | 5-hour credits limit. `null` clears/disables. `0` blocks immediately. |
| `window_mode_5h` | string | no | `first_use` (default; alias `fixed`) or `sliding`. |
| `limit_1d_credits` | number/null | no | 1-day credits limit. |
| `window_mode_1d` | string | no | `first_use`, `sliding` (alias `rolling`), or `calendar` (default `first_use`). |
| `limit_7d_credits` | number/null | no | 7-day credits limit. |
| `window_mode_7d` | string | no | `first_use`, `sliding` (alias `rolling`), or `calendar` (default `first_use`). |
| `week_reset_day` | integer | no | Calendar week start day, `1=Mon` .. `7=Sun` (default `1`). |
| `week_reset_hour` | integer | no | Calendar week start hour `0-23` (default `0`). |
| `limit_30d_credits` | number/null | no | 30-day / month credits limit. |
| `window_mode_30d` | string | no | `first_use`, `sliding` (alias `rolling`), or `calendar` (default `first_use`). Calendar uses calendar months. |
| `mfa` | any valid JSON | no | Stored in `user.mfa`. |
| `passkey` | any valid JSON | no | Stored in `user.passkey`. |

Response: same shape as `GET /users/:id`.

Semantic validation failures and identifiable type mismatches in structurally valid JSON return HTTP `400` with a structured `field_errors` array so clients can localize and anchor errors without parsing message strings:

```json
{
  "error": "invalid body",
  "message": "window_mode_5h: window mode must be \"first_use\" or \"sliding\"",
  "field_errors": [{ "field": "window_mode_5h", "code": "invalid_window_mode" }]
}
```

| `field_errors[].code` | `field_errors[].field` | Meaning |
| --- | --- | --- |
| `required` | `username` | Username is missing or empty. |
| `invalid_type` | `credits_unlimited` | Value is not a boolean or `null`. |
| `invalid_timezone` | `timezone` | Invalid IANA timezone, or a value whose type is not string/`null`. |
| `invalid_limit` | `limit_5h_credits`, `limit_1d_credits`, `limit_7d_credits`, `limit_30d_credits` | Limit is negative, not finite, or not a number/`null`. |
| `invalid_window_mode` | `window_mode_5h`, `window_mode_1d`, `window_mode_7d`, `window_mode_30d` | Unsupported mode or a value other than a string/`null` (`calendar` is rejected for `5h`). |
| `invalid_week_reset_day` | `week_reset_day` | Outside `1`–`7` or not an integer/`null`. |
| `invalid_week_reset_hour` | `week_reset_hour` | Outside `0`–`23` or not an integer/`null`. |

Malformed JSON, or JSON that cannot be decoded far enough to identify a specific field, still returns `400` with `error: "invalid body"` but may omit `field_errors`.

### PUT/PATCH `/users/:id`

Updates a user. `PUT` and `PATCH` currently have the same partial-update behavior: only fields present in the body are modified.

Example request:

```json
{
  "username": "alice-updated",
  "password": "new-plaintext-password",
  "credits": 20,
  "mfa": { "enabled": false },
  "passkey": []
}
```

All request fields are optional, but `username`, if present, must not be empty. `credits`, if present, replaces the user's current credit balance. For billing workflows, prefer `/billing/balance-records/recharge` and `/billing/balance-records/deduct` so balance changes have ledger records.
Set `credits_unlimited` to `true` when the user should have unlimited total balance but still be constrained by configured period limits.
When `password` is present, a successful update increments the user's session version and invalidates all previously issued User API bearer tokens. The Management API does not issue a replacement user session.

Validation failures use the same `field_errors` contract as `POST /users`.

Response: same shape as `GET /users/:id`.


### GET `/users/:id/period-limits`

Returns the user's period-limit configuration and current usage.

Response fields include `timezone`, `credits`, `credits_unlimited`, and `windows[]` with:

| Field | Description |
| --- | --- |
| `id` | `5h`, `1d`, `7d`, or `30d`. |
| `enabled` | `true` when the limit is configured (`limit != null`). |
| `limit` | Credits limit for the window; `null` means disabled. |
| `used` | Credits spent in the current window (`SUM(billing_charge.amount)`). |
| `remaining` | `max(limit - used, 0)` when enabled. |
| `mode` | `first_use`, `sliding`, or `calendar` (`calendar` only for `1d`/`7d`/`30d`). |
| `window_start` / `window_end` / `reset_at` | Current window bounds when active. |
| `usage_epoch` | Soft-reset marker; usage only counts charges at/after this time. |

`5h` supports `first_use` (default, **first billable charge** opens a 5h window; legacy alias `fixed`) or `sliding` (rolling 5h). `1d`/`7d`/`30d` support `first_use` (default, first-charge duration; legacy alias `fixed`), `sliding` (rolling duration), or `calendar` (natural day/week/month). Dispatch probes do not open `first_use` windows. Calendar mode uses `timezone` (default `Asia/Shanghai`). Calendar `7d` uses `week_reset_day` (1=Mon..7=Sun) and `week_reset_hour`. Calendar `30d` is a calendar month, not a rolling 30-day span.

Product alignment (Claude Code / Codex):
- Short window `5h` defaults to `first_use`: the first billable charge opens a 5-hour session; the limit is a credits budget inside that window, not five hours of continuous work.
- Longer windows (`7d` / `30d`) stack independently; all enabled windows are enforced with AND.
- Use `calendar` on `1d` / `7d` / `30d` for natural day / week / month resets.
- Use `sliding` (alias `rolling`) when usage should recover continuously as older spend ages out.

### POST `/users/:id/period-limits/reset`

Soft-resets period counters without deleting billing history.

Request body:

```json
{ "windows": ["5h", "1d"], "mode": "counter" }
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `windows` | string[] | no | Subset of `5h`/`1d`/`7d`/`30d`. Empty/omitted resets all windows. |
| `mode` | string | no | `counter` (default): for each selected window set `usage_epoch_* = now` and clear the matching `period_window_start_*`. `window_only`: clear `period_window_start_*`; for `sliding`/`calendar` also set `usage_epoch_*` so used actually resets. |

An empty request body, omitted `windows`, `windows: []`, or `windows: null` always targets all four windows in stable order (`5h`, `1d`, `7d`, `30d`), including windows that are currently disabled or inactive.

Response:

```json
{
  "status": "ok",
  "user_id": 1,
  "reset": { "mode": "counter", "windows": ["5h", "1d"], "at": "2026-07-09T12:00:00Z" },
  "limits": {
    "user_id": 1,
    "timezone": "Asia/Shanghai",
    "credits": 10.5,
    "credits_unlimited": false,
    "windows": [
      {
        "id": "5h",
        "enabled": true,
        "limit": 5,
        "used": 0,
        "remaining": 5,
        "mode": "first_use",
        "active": false,
        "window_start": null,
        "window_end": null,
        "reset_at": null,
        "usage_epoch": "2026-07-09T12:00:00Z"
      }
    ]
  }
}
```

`limits` is the complete post-reset status, rebuilt inside the same transaction; it is identical in shape to `GET /users/:id/period-limits`, so clients can update their local state directly from the response without an extra read.

Validation and identifiable JSON type failures return HTTP `400` with the `field_errors` contract described in the user write sections, using codes `invalid_reset_mode` (field `mode`) and `invalid_reset_windows` (field `windows`). Malformed JSON may omit `field_errors`.

Period limits are enforced at dispatch for every API key owned by the user (`user_credits_insufficient` and `user_period_limit_exceeded`). When `credits_unlimited=true`, the total-balance check is skipped, but enabled period windows are still enforced.

Enforcement is a **soft limit**: cost is known only after the request, so a final/in-flight request may push `used` past `limit`; the next dispatch is blocked. `first_use` opens on the first **billable charge**, not on dispatch probes.

### DELETE `/users/:id`

Soft-deletes a user.

Input: no body.

Example response:

```json
{ "status": "ok" }
```

Common errors:

```json
{ "error": "not_found", "message": "record not found" }
{ "error": "invalid body", "message": "username is required" }
```

## Client API Keys

### GET `/api-keys`

Returns client API keys accepted by Home.

Input: none.

Example response:

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

Fields:

| Field | Type | Description |
| --- | --- | --- |
| `api-keys` | array of string | Compatibility list of raw client keys. |
| `items` | array of `APIKeyEntry` | Structured API key records. |
| `api_key_entries` | array of `APIKeyEntry` | Alias of `items`. |
| `APIKeyEntry.id` | integer | Stable database primary key of the API key. |
| `APIKeyEntry.api_key_id` | integer | Alias of `id`. |
| `APIKeyEntry.api-key` | string | Client API key. |
| `APIKeyEntry.api_key` | string | Alias of `api-key`. |
| `APIKeyEntry.user-id` | integer or null | Bound `user.id`; `null` means unbound. |
| `APIKeyEntry.user_id` | integer or null | Alias of `user-id`. |
| `APIKeyEntry.channels` | array of integer | Bound channel group IDs. An empty array is non-restrictive. |
| `APIKeyEntry.model_groups` | array of integer | Bound model group IDs. An empty array is non-restrictive. |

### POST `/api-keys`

Atomically creates one client API key without replacing the existing list.

```json
{
  "api_key": "client-key-1",
  "user_id": 1,
  "channels": [1],
  "model_groups": [2]
}
```

The request also accepts `api-key`, `key`, or `value` as the key field. `user-id` and `model-groups` are accepted as aliases.

A new key value receives a new stable identifier. Re-adding an identical soft-deleted key restores the previous record and reuses its identifier. Creating an active duplicate returns `409 api_key_exists`.

Successful response:

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

Replaces the complete client API key list.

Input can be a raw string array:

```json
["client-key-1", "client-key-2"]
```

or:

```json
{ "items": ["client-key-1", "client-key-2"] }
```

Structured entries are also accepted. Wrapper keys can be `items`, `api-keys`, `api_keys`, or `api_key_entries`:

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

Entry fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `api_key` | string | conditionally | Client API key. Aliases: `api-key`, `key`, `value`. |
| `user_id` | integer | no | Bound `user.id`. Alias: `user-id`. |
| `channels` | array of integer | no | Channel group IDs. |
| `model_groups` | array of integer | no | Model group IDs. Alias: `model-groups`. |

If `user_id` references a missing user, the API returns `404 user_not_found`.

`PUT` is an explicit complete-list replacement operation with last-write-wins semantics. `id` and `api_key_id` are response-only for this operation and are ignored in structured input. Unchanged key values preserve their identifiers, removed values are soft-deleted, and new values receive new identifiers unless they restore an identical soft-deleted value.

Successful response:

```json
{ "status": "ok" }
```

### PATCH `/api-keys`

Updates one client API key. Stable `id` / `api_key_id` selection is preferred. Legacy `index`, `old/new`, and raw-key selectors remain available for compatibility and are resolved to one database record before the update is applied. When `old/new` is used and the old value does not exist, `new` is created atomically. This route can also update `user_id`, `channels`, and `model_groups` for an existing API key.

ID update:

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

Binding-only updates can select the record by ID without resending the key value:

```json
{ "api_key_id": 1, "channels": [1] }
```

Index update:

```json
{ "index": 0, "value": "new-key" }
```

Old/new update:

```json
{ "old": "old-key", "new": "new-key" }
```

Binding update:

```json
{
  "api_key": "client-key-1",
  "user_id": 1,
  "channels": [1],
  "model_groups": [2]
}
```

Clear user binding:

```json
{ "api_key": "client-key-1", "user_id": 0 }
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | integer | conditionally | Preferred stable API key identifier. Alias: `api_key_id`, `api-key-id`. |
| `index` | integer | conditionally | Zero-based index. |
| `value` | string or `APIKeyEntry` | conditionally | New value paired with `id` or `index`. Structured entries are accepted. |
| `old` | string | conditionally | Old key to find. |
| `new` | string | conditionally | New key; appended when `old` is not found. |
| `api_key` | string | conditionally | Legacy raw-key target. When supplied with `id`, it must match the selected record. Aliases: `api-key`, `key`. |
| `user_id` | integer | no | Bound `user.id`. Alias: `user-id`; `0` clears the binding. |
| `channels` | array of integer | no | Channel group IDs. |
| `model_groups` | array of integer | no | Model group IDs. Alias: `model-groups`. |

Successful response:

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

Deletes one client API key. Stable ID selection is preferred; index and value selectors remain available for compatibility.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `id` | integer | Preferred stable API key identifier. Aliases: `api_key_id`, `api-key-id`. |
| `index` | integer | Delete the key at this zero-based index. |
| `value` | string | Delete the key whose trimmed value matches. |
| `api_key` | string | Alias of `value`. |
| `api-key` | string | Alias of `value`. |
| `key` | string | Alias of `value`. |

Example response:

```json
{ "status": "ok" }
```

Unknown IDs return `404 api_key_not_found`. If an ID and key selector are both supplied but identify different records, the API returns `400 invalid_api_key_selector`.

## Billing

All paths in this section are relative to the Management API base URL, for example `/v0/management/billing/overview` or `/v0/management/proxy/proxy-pools`. They are not `/user` routes and require the management key.

Only `/billing/overview`, `/billing/charges`, and `/billing/balance-records` parse `from` and `to` as `YYYY-MM-DD`, RFC3339, or Unix seconds. All three routes use the half-open interval `[from,to)`: `from` is included and `to` is excluded. The optional `timezone` parameter is a reporting-timezone override and must be an IANA timezone name. When omitted, the routes use `/billing/settings.report_timezone`, which defaults to `UTC`. Date-only values use calendar dates in the applied reporting timezone, and a date-only `to` is normalized to the next local midnight so the whole ending day remains included across DST transitions. Explicit timestamps are exact exclusive boundaries and are not shifted or expanded by the reporting timezone. `/billing/overview` also uses the applied reporting timezone for `range` calendar dates and `daily_trend` buckets, so one natural day is not split at UTC midnight. The reporting timezone only controls query boundaries and report grouping; it never reprices immutable charges, changes price snapshots, or mutates user balances. Pagination with `limit` and `offset` applies only to `/billing/charges` and `/billing/balance-records`; those routes use `limit` default `50`, max `200`, and normalize negative `offset` values to `0`. `/billing/model-prices` supports only `provider`, `model`, and `enabled` query parameters. `/proxy/proxy-pools` currently does not parse query parameters.

Unsupported timezone names return `400 invalid_timezone`. A `from` value after `to` returns `400 invalid_time_range`.

### GET `/billing/overview`

Returns an administrator billing summary.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional exclusive end time. Date-only values include the full ending day in `timezone` by using the next local midnight. |
| `timezone` | string | Optional IANA reporting-timezone override for date-only boundaries, response range dates, and daily trend buckets. Defaults to `/billing/settings.report_timezone`. |
| `user` | string | Optional username, user text, or user ID filter. Aliases: `user_text`, `username`. |
| `user_id` | integer | Optional exact user ID filter. Alias: `uid`. |
| `provider` | string | Optional provider filter. |
| `model` | string | Optional model filter. |

Response fields:

| Field | Type | Description |
| --- | --- | --- |
| `range` | object | Applied calendar `from`/`to`, exact UTC `from_at`/`to_at_exclusive`, and `timezone`. Exact boundaries are `null` when the corresponding query boundary is omitted. |
| `total_charge_amount` | number | Total charged amount. |
| `total_recharge_amount` | number | Total recharge amount. |
| `total_deduct_amount` | number | Total manual deduction amount. |
| `total_balance` | number | Current total user balance. |
| `request_count` | integer | Number of charged requests. |
| `input_tokens` | integer | Total input tokens. |
| `output_tokens` | integer | Total output tokens. |
| `cache_tokens` | integer | Total cache tokens. |
| `active_user_count` | integer | Number of users with charges in the range. |
| `daily_trend[]` | array | Daily charge amount and request count grouped in `range.timezone`. |
| `top_users[]` | array | Top users with `id`, `label`, `amount`, and `request_count`. |
| `top_models[]` | array | Top models with `id`, `label`, `amount`, and `request_count`. |
| `top_providers[]` | array | Top providers with `id`, `label`, `amount`, and `request_count`. |

### GET `/billing/charges`

Lists billing charge records with administrator context. Responses expose user ID, masked API-key metadata, price snapshot, matched price rule, request ID, endpoint, and `balance_before`/`balance_after`. Billing charge responses never expose raw API keys.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional exclusive end time. Date-only values include the full ending day in `timezone` by using the next local midnight. |
| `timezone` | string | Optional IANA reporting-timezone override for date-only boundaries. Defaults to `/billing/settings.report_timezone`. |
| `user` | string | Optional username, user text, or user ID filter. Aliases: `user_text`, `username`. |
| `user_id` | integer | Optional exact user ID filter. Alias: `uid`. |
| `provider` | string | Optional provider filter. |
| `model` | string | Optional model filter. |
| `limit` | integer | Optional page size. Default `50`, max `200`. |
| `offset` | integer | Optional page offset. Negative values normalize to `0`. |

Response shape:

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

Lists administrator recharge and deduction ledger records.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional exclusive end time. Date-only values include the full ending day in `timezone` by using the next local midnight. |
| `timezone` | string | Optional IANA reporting-timezone override for date-only boundaries. Defaults to `/billing/settings.report_timezone`. |
| `user` | string | Optional username, user text, or user ID filter. Aliases: `user_text`, `username`. |
| `user_id` | integer | Optional exact user ID filter. Alias: `uid`. |
| `limit` | integer | Optional page size. Default `50`, max `200`. |
| `offset` | integer | Optional page offset. Negative values normalize to `0`. |

Response shape:

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

Adds a recharge ledger record and updates the user's `credits`.

Request body:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `user_id` | integer | yes | Target user ID. |
| `amount` | number | yes | Positive recharge amount. |
| `note` | string | no | Optional operator note. |

The current operator for management-key operations is `admin`.

Response:

```json
{ "status": "ok", "balance_record": { "id": "balance_xxx", "type": "recharge" } }
```

### POST `/billing/balance-records/deduct`

Adds a deduction ledger record and updates the user's `credits`.

Request body:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `user_id` | integer | yes | Target user ID. |
| `amount` | number | yes | Positive deduction amount. |
| `note` | string | yes | Required reason for the deduction. |

The current operator for management-key operations is `admin`.

Response:

```json
{ "status": "ok", "balance_record": { "id": "balance_xxx", "type": "deduct" } }
```

### GET `/billing/model-prices`

Lists model price rules.

The response always includes `price_rule_schema_version: 2`, including when `items` is empty. Rules match exact normalized service tiers before the `*` compatibility wildcard, then select the greatest `min_input_tokens` not exceeding the usage record's original input-token count.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `provider` | string | Optional provider filter. |
| `model` | string | Optional model filter. |
| `enabled` | boolean | Optional enabled filter. |

Model price fields:

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Model price record ID. |
| `provider` | string | Provider name. |
| `model` | string | Model name. |
| `service_tier` | string | Normalized service tier, or `*` as a compatibility wildcard. |
| `min_input_tokens` | integer | Inclusive lower bound of the context band. |
| `input_price_per_million` | number | Input-token price. |
| `output_price_per_million` | number | Output-token price. |
| `cache_read_price_per_million` | number | Cache-read token price. |
| `cache_write_price_per_million` | number | Cache-write token price. |
| `request_price` | number | Per-request price. |
| `source` | string | Price source. |
| `enabled` | boolean | Whether the rule is active. |
| `note` | string | Operator note. |
| `created_at` | string | Creation time. |
| `updated_at` | string | Last update time. |
| `revision` | integer | Monotonic rule revision used by import conflict detection. |

### POST `/billing/model-prices`

Creates a model price rule. Omitted price values default to `0`, and `enabled` defaults to `true`.

Request body fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `provider` | string | yes | Provider name. |
| `model` | string | yes | Model name. |
| `service_tier` | string | no | Exact tier or `*`; defaults to `*`. `auto`, `default`, and `standard` normalize to the local `standard` tier. |
| `min_input_tokens` | integer | no | Non-negative inclusive context-band lower bound; defaults to `0`. |
| `input_price_per_million` | number | no | Non-negative input-token price. |
| `output_price_per_million` | number | no | Non-negative output-token price. |
| `cache_read_price_per_million` | number | no | Non-negative cache-read token price. |
| `cache_write_price_per_million` | number | no | Non-negative cache-write token price. |
| `request_price` | number | no | Non-negative per-request price. |
| `source` | string | no | Price source such as `manual`. |
| `enabled` | boolean | no | Whether the rule is active. Defaults to `true`. |
| `note` | string | no | Operator note. |

Response:

```json
{ "status": "ok", "model_price": { "id": "price_xxx", "provider": "openai", "model": "gpt-4.1-mini" } }
```

### PATCH `/billing/model-prices/:id`

Partially updates a model price rule and preserves unspecified fields. The request body accepts the same fields as `POST /billing/model-prices`.

Response:

```json
{ "status": "ok", "model_price": { "id": "price_xxx", "enabled": false } }
```

### DELETE `/billing/model-prices/:id`

Soft-deletes a model price rule.

Input: no body.

Response:

```json
{ "status": "ok" }
```

### GET `/billing/settings`

Returns the DB-backed billing policy. `service_tier_source` is `request` by default and may be `request` or `response`. `report_timezone` is a supported IANA timezone and defaults to `UTC`; it is the default calendar timezone for Management Billing reports when a request does not provide a `timezone` override. Changing it affects future report boundaries and grouping only. It does not reprice historical charges, rewrite price snapshots, or change balances. In either tier-source mode, `auto`, `default`, and `standard` match the local Standard price tier.

```json
{ "service_tier_source": "request", "report_timezone": "UTC" }
```

### PATCH `/billing/settings`

Partially updates billing settings. Both fields are optional. In `response` mode, a missing response tier falls back to the request tier and records that fallback in the charge price snapshot. Invalid tier-source values or unsupported timezone names return `400 invalid_body`.

```json
{ "service_tier_source": "response", "report_timezone": "Asia/Shanghai" }
```

OpenAI defines an omitted `service_tier` as `auto`; `auto` is represented internally by Home and is not a literal Codex backend request value. See the [OpenAI pricing page](https://developers.openai.com/api/docs/pricing) and [Responses Create reference](https://developers.openai.com/api/reference/resources/responses/methods/create). Home stores the request `service_tier` and optional upstream `response_service_tier` so later billing policy can switch sources without re-ingesting usage. The default `request` source bills from `service_tier` (client-requested tier). In `response` mode, Home uses `response_service_tier` when present and falls back to the request tier when the upstream omits it. `auto`, `default`, and `standard` map to the local Standard price tier; configure `flex` or `priority` rules when those tiers are used.

Charge `price_snapshot` audit data includes `requested_service_tier`, optional `response_service_tier`, `service_tier_source`, `effective_service_tier`, `response_tier_fallback`, `matched_service_tier`, and `min_input_tokens`. Context-band selection uses the original total input count. In the OpenAI Responses protocol, `input_tokens` includes both cache-read and cache-write tokens. Home removes cache-read tokens from ordinary input before applying the cache-read price. When `cache_write_price_per_million` is positive, Home also removes cache-write tokens from ordinary input before applying that separate price; when the price is zero or omitted, those cache-write tokens remain billed as ordinary input. In the Anthropic Messages protocol, `input_tokens`, `cache_read_input_tokens`, and `cache_creation_input_tokens` are independent buckets, so Home prices them independently without subtracting either cache bucket from input. Cache-field compatibility backfills update usage counters only; existing immutable `billing_charge` snapshots and balances are not repriced automatically, so historical corrections require an explicit audited balance adjustment.

### POST `/billing/model-prices/import/preview`

Creates a server-side, immutable `models.dev` import preview. The server fetches and pins the source snapshot; clients provide targets, matching policy, aliases, row multipliers, and optional source-match overrides. Invalid request-controlled input returns `422 invalid_import_preview`, a catalog fetch failure returns `502 models_dev_fetch_failed`, and an internal preview persistence failure returns `500 billing_import_preview_failed`. A successful response contains `preview_id`, `preview_revision`, source provenance, `generated_at`, `expires_at`, explicit `atomic: true`, rows, and an exact summary.

Preview targets currently describe only the wildcard base rule (`service_tier: "*"`, `min_input_tokens: 0`); other target scopes are rejected rather than silently rewritten. A matched row includes official prices, final multiplied prices, the exact `write_rule`, optional complete `existing_rule` snapshot with `revision`, and a machine-readable reason. Models.dev context bands create distinct wildcard rows at their inclusive lower bounds. `row_multipliers` apply to the exact returned row key, including a context-band key. Cache-read and cache-write prices are imported from the pinned catalog snapshot. When a cost object has an input price but omits `cache_read` or `cache_write`, Home derives the missing price as `input * 0.1` or `input * 1.25`, respectively; explicit values, including zero, are preserved. Unsupported dimensions, malformed or invalid prices/bands, duplicate bands, or a tier that omits a price dimension configured by its base rule make the whole target non-applicable; the server never imports a potentially undercharged subset.

`policy.overwrite_mode` is `missing`, `sync`, or `all`. `missing` creates only absent rules, `sync` may update prior `source=sync` rules, and `all` may overwrite manual/default rules. Preview rows requiring an overwrite use action `overwrite` and require confirmation on apply.

### POST `/billing/model-prices/import/apply`

Applies selected rows from a preview in one database transaction. The body contains `preview_id`, `preview_revision`, non-empty unique `selected_keys`, `confirm_overwrite`, and `idempotency_key`; the same key may also be sent in the `Idempotency-Key` header and must match when both are present.

The server rejects an expired preview with `410`, a revision mismatch with `412`, changed existing rules (including a concurrent create of the same identity) with `409`, and invalid selections, policy confirmation, or idempotency-key reuse with a different request with `422`. Replaying an equivalent request with the same idempotency key returns the original immutable operation result without additional writes. A successful synchronous response is `200` with `operation_id`, `preview_id`, `status: "applied"`, `atomic: true`, `applied_at`, summary, and a result for every selected row. Every successful row includes its non-empty `resource_id`. Expired previews are retained for up to 24 hours for diagnostics, and completed operation results for up to 30 days; both are cleaned during later preview creation.

### GET `/billing/model-prices/import/operations/:id`

Returns the persisted immutable operation result for an import apply. Unknown operation IDs return `404`. The current implementation completes apply synchronously; therefore returned terminal status is `applied` rather than `pending` or `running`.

### GET `/billing/settings/diagnostics`

Returns bounded billing-tier evidence derived from stored usage: `supported`, `window_start`, `window_end`, `eligible_requests`, `response_tier_requests`, `fallback_requests`, and optional `last_response_tier_at`. Eligible requests are the recent records that contain a request service tier; fallback requests are eligible records without a response service tier. This endpoint reports observed payload data only; it does not infer response-tier coverage.

### GET `/proxy/proxy-pools`

Lists proxy pool records.

Proxy pool records are stored and tested only in this release. They do not change runtime proxy priority, auth selection, dispatch, or outbound traffic routing. The only supported `scope` is `global`.

Response:

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

Creates a proxy pool record. `enabled` defaults to `true` when omitted. `scope` is only `global`.

Request body fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Display name. |
| `proxy_url` | string | yes | Proxy URL to store and test. |
| `enabled` | boolean | no | Whether the record is enabled. Defaults to `true`. |
| `scope` | string | no | Only `global` is supported. |
| `priority` | integer | no | Stored priority value. It does not affect runtime routing in this release. |
| `note` | string | no | Operator note. |

Response:

```json
{ "status": "ok", "proxy_pool": { "id": "proxy_xxx", "scope": "global", "enabled": true } }
```

### PATCH `/proxy/proxy-pools/:id`

Partially updates a proxy pool record and preserves unspecified fields.

Request body: any subset of the `POST /proxy/proxy-pools` fields.

Response:

```json
{ "status": "ok", "proxy_pool": { "id": "proxy_xxx", "enabled": false } }
```

Missing records return:

```json
{ "error": "proxy_pool_not_found", "message": "record not found" }
```

### DELETE `/proxy/proxy-pools/:id`

Deletes a proxy pool record.

Input: no body.

Response:

```json
{ "status": "ok" }
```

Missing records return `proxy_pool_not_found`.

### POST `/proxy/proxy-pools/:id/test`

Tests a stored proxy pool record. When the item exists and the test completes, the endpoint returns `200` with `result: "passed"` or `result: "failed"` and updates `last_tested_at` and `last_test_result` on the record.

Input: no body.

Response:

```json
{
  "status": "ok",
  "result": "passed",
  "message": "proxy test returned HTTP 204"
}
```

Missing records return:

```json
{ "error": "proxy_pool_not_found", "message": "record not found" }
```

## Provider API Key Routes

These routes manage upstream API-key credentials:

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

Home synthesizes DB auth records from these config-like payloads. xAI API-key usage is ingested through the normal usage pipeline with `provider=xai` and an API-key credential type, so it is available in usage records, provider/credential aggregates, billing, and legacy `/api-key-usage` output under the `xai` provider bucket.

### Credential Field Structures

`GeminiKey`:

| Field | Type | Description |
| --- | --- | --- |
| `api-key` | string | Upstream Gemini API key. |
| `priority` | integer | Higher priority credentials are selected first. |
| `prefix` | string | Optional model namespace prefix. |
| `base-url` | string | Optional Gemini API base URL override. |
| `proxy-url` | string | Optional per-key outbound proxy. |
| `models` | array of `ModelAlias` | Optional upstream model aliases. |
| `headers` | object string to string | Extra upstream request headers. |
| `excluded-models` | array of string | Model IDs excluded from this key. |
| `disable-cooling` | boolean | Disable quota cooldown scheduling for this credential. |
| `auth-index` | string | Compatibility credential identifier. |
| `id` | string | Canonical immutable credential UUID. Responses and exports use this field. |
| `uuid` | string | Legacy input-only alias for `id`; it is normalized to `id` and never returned or exported. |
| `disabled` | boolean | Read-only DB auth disabled flag. Use `PATCH /auth-files/status` to change it. |

`ClaudeKey`, `CodexKey`, `XAIKey`, and `VertexCompatKey` use the same common fields. `XAIKey` uses the native xAI executor and requires `base-url` (normally `https://api.x.ai/v1`). Additional notable fields:

| Field | Applies to | Description |
| --- | --- | --- |
| `cloak` | Claude | Optional request cloaking config. |
| `experimental-cch-signing` | Claude | Enables experimental CCH signing for cloaked Claude requests. |
| `websockets` | Codex, xAI | Enables Responses API websocket transport. |
| `alpha-search` | Codex | Allows this API key to serve `/v1/alpha/search` through `base-url` plus `/alpha/search`. Defaults to `false`. |
| `api-key` | Vertex | Sent as `x-goog-api-key`. |

`OpenAICompatibility`:

| Field | Type | Description |
| --- | --- | --- |
| `name` | string | Provider name. |
| `priority` | integer | Higher priority providers are selected first. |
| `disabled` | boolean | Disables this provider when true. |
| `prefix` | string | Optional model namespace prefix. |
| `base-url` | string | OpenAI-compatible API base URL. |
| `api-key-entries` | array of `OpenAICompatibilityAPIKey` | Provider API keys and optional proxies. |
| `models` | array of `OpenAICompatibilityModel` | Model definitions and aliases. |
| `headers` | object string to string | Extra upstream headers. |
| `disable-cooling` | boolean | Disable quota cooldown scheduling for this provider. |
| `id` | string | Canonical immutable UUID of the fallback credential when `api-key-entries` is empty. Responses and exports use this field. |
| `uuid` | string | Legacy input-only alias for the fallback `id`; it is normalized to `id` and never returned or exported. |

Shared nested structures:

```json
{
  "ModelAlias": {
    "name": "upstream-model",
    "alias": "client-visible-model",
    "display-name": "Catalog name",
    "force-mapping": true
  },
  "OpenAICompatibilityAPIKey": {
    "id": "canonical-credential-uuid",
    "uuid": "legacy-input-only-credential-uuid",
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

For each `OpenAICompatibilityAPIKey`, `uuid` is accepted as a legacy input-only alias for `id`. It is normalized to `id` and is never returned or exported.

### GET Provider Key Routes

Input: none.

Example response:

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

Replaces the full list for the route provider.

Input can be an array:

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

or a wrapper:

```json
{ "items": [ { "api-key": "provider-key" } ] }
```

Home also accepts `{ "<route-key>": [...] }`, `{ "list": [...] }`, `{ "data": [...] }`, or a single entry object.

Successful response:

```json
{ "status": "ok" }
```

### PATCH Provider Key Routes

Updates one provider credential.

Example request:

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

Selector fields:

| Field | Type | Description |
| --- | --- | --- |
| `index` | integer | Zero-based index in the filtered provider list. |
| `match` | string | API-key value to match. |
| `name` | string | OpenAI-compatible provider name or auth label. |
| `id` | string | DB auth ID. |
| `uuid` | string | Alias of `id`. |
| query `base-url` | string | Optional base URL to disambiguate API-key matches. |

`PATCH` does not use body `auth_index` as the DB ID selector. Use `id` or `uuid` for ID-based patching.

Successful response:

```json
{ "status": "ok" }
```

### DELETE Provider Key Routes

Deletes one provider credential.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `id` | string | DB auth ID. |
| `uuid` | string | Alias of `id`. |
| `auth_index` | string | DB auth ID or runtime index. |
| `index` | integer | Zero-based index in the filtered provider list. |
| `api-key` | string | API-key value. |
| `api_key` | string | Alias of `api-key`. |
| `match` | string | Alias of `api-key`. |
| `base-url` | string | Optional base URL to disambiguate. |
| `base_url` | string | Alias of `base-url`. |
| `name` | string | Provider or compatibility name. |

Successful response:

```json
{ "status": "ok" }
```

## Auth Files and OAuth

### GET `/auth-files`

Lists OAuth/file-backed credentials.

Input: none.

Example response:

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
      "prefix": "team-a",
      "proxy_url": "socks5://127.0.0.1:1080",
      "priority": 10,
      "note": "operator note",
      "websockets": true,
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "modtime": "2026-05-27T10:00:00Z"
    }
  ]
}
```

Editable metadata is projected with each list item so management clients can display and update
the current value without downloading the credential JSON:

| Field | Type | Description |
| --- | --- | --- |
| `prefix` | string | Model namespace prefix; empty when unset. |
| `proxy_url` | string | Per-auth proxy URL; empty when unset. |
| `priority` | integer | Credential selection priority; omitted when unset. |
| `note` | string | Operator note; omitted when empty. |
| `websockets` | boolean | Effective runtime websocket flag. |

### GET `/auth-files/models?name=<name-or-id>`

Returns models associated with an auth file or auth ID.

Query parameters:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Auth filename, auth ID, display name, or runtime index. |

Example response:

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

Downloads one credential JSON.

Query parameters:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | conditionally | Filename or display name. |
| `file` | string | conditionally | Alias for filename. |
| `filename` | string | conditionally | Alias for filename. |
| `id` | string | conditionally | DB auth ID. |
| `uuid` | string | conditionally | Alias of `id`. |
| `auth_index` | string | conditionally | Auth ID or runtime index. |
| `index` | integer | conditionally | Zero-based OAuth auth index. |

Response: `application/json; charset=utf-8` attachment.

### POST `/auth-files`

Uploads one or more credential JSON payloads.

Multipart input:

| Form field | Type | Required | Description |
| --- | --- | --- | --- |
| any file field | file | yes | One or more `.json` credential files. |

Raw JSON input: the request body is the credential JSON payload. `name` is not required; Home derives or allocates a UUID-backed filename.

Example responses:

```json
{ "status": "ok" }
```

```json
{ "status": "ok", "uploaded": 2, "files": ["a.json", "b.json"] }
```

Raw JSON response:

```json
{ "status": "ok", "name": "uuid.json" }
```

### DELETE `/auth-files`

Deletes credential records or files.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `name` | string | Filename or display name. |
| `file` | string | Alias for filename. |
| `filename` | string | Alias for filename. |
| `id` | string | DB auth ID. |
| `uuid` | string | Alias of `id`. |
| `auth_index` | string | Auth ID or runtime index. |
| `index` | integer | Zero-based OAuth auth index. |
| `all` | `true`, `1`, or `*` | Delete all OAuth/file-backed credentials. |

Example responses:

```json
{ "status": "ok" }
```

`all` response:

```json
{ "status": "ok", "deleted": 2 }
```

### PATCH `/auth-files/status`

Enables or disables an OAuth/file-backed auth.

Example request:

```json
{
  "name": "codex-user.json",
  "disabled": true
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Filename, DB auth ID, runtime auth index, or display name. |
| `disabled` | boolean | yes | `true` disables the auth; `false` enables it. |

This route currently reads the selector from `name`; it does not read separate body `id`, `uuid`, `auth_index`, or `index` fields for this endpoint.

Example response:

```json
{ "status": "ok", "disabled": true }
```

### PATCH `/auth-files/fields`

Updates editable auth metadata.

Example request:

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

Selector fields:

| Field | Type | Description |
| --- | --- | --- |
| `name`, `file`, `filename` | string | Filename or display name. |
| `id`, `uuid`, `auth_index` | string | DB auth ID selector. |
| query `index` | integer | Zero-based OAuth auth index selector. |

Editable fields:

| Field | Type | Description |
| --- | --- | --- |
| `prefix` | string | Model namespace prefix; empty value clears it. |
| `proxy_url` | string | Per-auth proxy URL; empty value clears it. |
| `proxy-url` | string | Alias for `proxy_url`. |
| `headers` | object string to string | Extra upstream headers. Empty string deletes a single header. |
| `priority` | integer or numeric string | Credential selection priority. |
| `note` | string | Operator note; empty value clears it. |
| `websockets` | boolean or string bool | Runtime websocket flag for supported auths. |
| `disabled` | boolean or string bool | Updates auth disabled state and status. |
| any nested path | any valid JSON | Sets arbitrary metadata paths such as `token.access_token`. |

Example response:

```json
{ "status": "ok" }
```

### OAuth Start Routes

These routes create provider login URLs or device-flow sessions:

```text
GET /anthropic-auth-url
GET /codex-auth-url
GET /antigravity-auth-url
GET /kimi-auth-url
GET /xai-auth-url
GET /<plugin-provider>-auth-url
```

Common response:

```json
{
  "status": "ok",
  "url": "https://provider.example/oauth/authorize?...",
  "state": "oauth-state"
}
```

`GET /kimi-auth-url` starts a device flow and returns the verification URL. Completion is handled by Home in the background.

`GET /<plugin-provider>-auth-url` is available for Home-loaded plugin providers returned by `GET /plugins` with `supports_oauth: true`, `effective_enabled: true`, and a non-empty `oauth_provider`. The provider segment is normalized to lowercase and must contain only letters, numbers, or hyphens.

### GET `/get-auth-status`

Returns the current OAuth session status.

Query parameters:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `state` | string | no | OAuth state token. |

Example responses:

```json
{ "status": "ok" }
{ "status": "wait" }
{ "status": "error", "error": "Authentication failed" }
{ "status": "error", "error": "unknown or expired state" }
```

Unknown or expired state tokens return an error instead of being treated as completed. Completed sessions remain available as short-lived tombstones so the final poll can return `{ "status": "ok" }`; after the tombstone expires, the same state is treated as unknown.

For plugin OAuth sessions, this route polls the Home-loaded plugin. When the plugin returns success, Home converts the returned auth data into DB-backed auth records, registers models for the auths, completes the OAuth session, and then returns `{ "status": "ok" }`.

### POST `/oauth-callback`

Processes provider OAuth callback metadata.

Example request:

```json
{
  "provider": "codex",
  "redirect_url": "http://localhost/callback?code=CODE&state=STATE",
  "code": "CODE",
  "state": "STATE",
  "error": ""
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `provider` | string | yes | Built-in aliases: `anthropic`/`claude`, `codex`/`openai`, `antigravity`/`anti-gravity`, and `xai`/`x-ai`/`grok`. For plugin OAuth sessions, pass the plugin `oauth_provider` key. `kimi` is not completed through this route. |
| `redirect_url` | string | no | Full callback URL. Missing `code`, `state`, or `error` values can be extracted from it. |
| `code` | string | conditionally | OAuth authorization code; required unless `error` is supplied. |
| `state` | string | yes | OAuth state token. |
| `error` | string | conditionally | Provider error; required when `code` is absent. |

Home reads session data from the DB-backed OAuth session. Built-in OAuth sessions exchange the code in the background and store resulting auth records in the DB. Plugin OAuth sessions store callback metadata in the session; `/get-auth-status` then polls the plugin and persists the auth records returned by the plugin.

Example response:

```json
{ "status": "ok" }
```

Common errors:

```json
{ "status": "error", "error": "invalid body" }
{ "status": "error", "error": "unsupported provider" }
{ "status": "error", "error": "unknown or expired state" }
{ "status": "error", "error": "oauth flow is not pending" }
{ "status": "error", "error": "provider does not match state" }
```

### POST `/vertex/import`

Uploads a Vertex service account JSON and creates a Vertex OAuth/file-backed credential.

Input:

| Form field or query | Type | Required | Description |
| --- | --- | --- | --- |
| form `file` | file | yes | Vertex service account JSON. |
| form/query `location` | string | no | Vertex location. Default: `us-central1`. |

Example response:

```json
{
  "status": "ok",
  "auth-file": "vertex-project-id.json",
  "project_id": "project-id",
  "email": "service-account@example.iam.gserviceaccount.com",
  "location": "us-central1"
}
```

Home stores the resulting credential as DB-backed OAuth auth records and returns the generated `<uuid>.json` name in `auth-file`.

## API Call Proxy

### POST `/api-call`

Sends an arbitrary HTTP request from the Home server. The route itself is protected by Management API authentication.

Example request:

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

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `auth_index`, `authIndex`, `AuthIndex` | string | no | Credential index from `GET /auth-files` or provider-key routes. Used for proxy selection and `$TOKEN$` replacement. |
| `method` | string | yes | HTTP method; normalized to uppercase. |
| `url` | string | yes | Absolute URL with scheme and host. |
| `header` | object string to string | no | Request headers. Header values containing `$TOKEN$` are replaced with the selected auth token. `Host` sets request host override. |
| `data` | string | no | Raw request body string. |

Token replacement is strict: if any header contains `$TOKEN$`, `auth_index` must resolve to a DB auth or runtime auth. Otherwise the endpoint returns:

```json
{ "error": "auth not found" }
```

Proxy priority:

1. Selected credential proxy.
2. Global `proxy-url`.
3. Direct transport with environment proxy disabled.

Example response:

```json
{
  "status_code": 200,
  "header": {
    "Content-Type": ["application/json"]
  },
  "body": "{\"ok\":true}"
}
```

## Usage and Logs

### GET `/api-key-usage`

Returns in-memory API-key usage grouped by provider and by `<base_url>|<api_key>`.

Input: none.

Example response:

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

### DELETE `/credentials/:credential_id/cooldown`

Clears Home-owned execution quota cooldown state for one credential. Without a query parameter, the operation clears quota cooldowns for every model. With `?model=<model>`, request-option suffixes such as `(high)` are removed and credential-specific public aliases or prefixes are resolved to the canonical upstream model before lookup. The compatibility route-model key is also cleared when it differs from the upstream key.

Execution cooldowns are always scoped to a credential and canonical model pair; Home never creates a credential-wide execution cooldown. CPA execution results without a canonical model are ignored by the cooldown state machine. HTTP 429 results use the capped model-level exponential backoff and do not use `Retry-After` or provider `retryDelay` hints for scheduling.

This operation is idempotent. It only clears cooldown state created by quota-exceeded/HTTP 429 results: quota flags, retry deadline, and backoff level. It does not enable a manually disabled credential, clear model-level 401/403/404/5xx state, change refresh scheduling, modify provider quota snapshots, or consume provider reset credits.

Examples:

```http
DELETE /credentials/credential-db-id/cooldown
DELETE /credentials/credential-db-id/cooldown?model=gpt-5
```

Example model-scoped response:

```json
{
  "status": "ok",
  "credential_id": "credential-db-id",
  "scope": "model",
  "model": "gpt-5",
  "cleared": true,
  "cleared_models": ["gpt-5"]
}
```

For an all-model request, `scope` is `all` and `model` is omitted. `cleared` is `false` when the credential exists but the requested scope has no quota cooldown. An unknown credential returns HTTP `404` with `error: "credential_not_found"`; an explicitly empty `model` returns HTTP `400` with `error: "model_required"`; an unavailable atomic state backend returns HTTP `503` with `error: "cooldown_reset_unavailable"`.

### Credential in-flight observation

`GET /credentials/in-flight` returns bounded request details derived only from the latest complete CPA snapshots. It accepts optional `credential_id` and `model` exact-match filters plus `limit` and `offset`. `limit` defaults to 100 and is bounded to 1 through 1000; `offset` defaults to 0. Each item contains `request_id`, `credential_id`, normalized upstream `model`, `request_kind`, and `started_at`. `request_id` is a display and correlation field, not a row-uniqueness guarantee; clients must preserve distinct returned rows even when request IDs repeat. A credential with a policy also includes its authoritative `limiter` state. It never exposes CPA identity, tokens, or request payloads. Every response also returns `total`, nullable `next_offset`, nullable `snapshot_cursor`, nullable `snapshot_cursor_read_at`, and nullable `snapshot_expires_at`. When `offset + returned item count < total`, `next_offset` equals that sum; otherwise `next_offset` is `null`.

Clients with `capabilities.credential_in_flight_snapshot_cursor=true` can request stable multi-page reads. Send `stable_snapshot=true` on the first page. When another page exists, Home stores the filtered observation and joined limiter states as a short-lived immutable database view, then returns an opaque `snapshot_cursor`, RFC 3339 `snapshot_cursor_read_at`, and RFC 3339 `snapshot_expires_at`. The read and expiry timestamps come from the same database clock, so clients can derive a relative remaining lifetime without comparing the database timestamp directly to the browser or Home process clock. The cursor remains valid for 2 minutes and can be read by any Home instance sharing the database. Every later page must send the same `credential_id` and `model` values plus that `snapshot_cursor`; `offset` should use the preceding `next_offset`, while `limit` may change. Clients must also keep `total` and `snapshot_expires_at` identical across all pages; `snapshot_cursor_read_at` is refreshed on each successful page read. All three cursor fields are `null` when the first response has no next page. Items and limiter values remain pinned to the stored view even if a newer CPA snapshot is ingested. Freshness diagnostics are not frozen: their deadline remains based on the original CPA snapshots' Home database ingestion timestamps, so creating a cursor never grants a new freshness interval. A page read after that deadline returns `stale=true` and `coverage_complete=false`.

An invalid, expired, schema-incompatible, or filter-mismatched cursor returns HTTP `409` with `error: "in_flight_snapshot_cursor_expired"`; clients must discard all accumulated pages and restart from offset 0. Failure to persist a first-page cursor returns HTTP `500` with `error: "in_flight_snapshot_cursor_store_failed"`. Home stores each cursor as relational header, request-item, observed-credential, limiter-state, and limiter-model rows written in bounded batches; later requests read only the requested item page and the projections for credentials on that page. The view is therefore not constrained by a single JSON payload size. Storage pressure or another database write failure still returns `in_flight_snapshot_cursor_store_failed`, so clients may fall back to one bounded non-stable request. Missing or inconsistent stored rows return HTTP `500` with `error: "in_flight_snapshot_cursor_load_failed"`. Expired cursor relations are removed opportunistically with a bounded relation-row budget when new cursors are created. Reading an expired cursor does not perform cleanup, and large expired views may require multiple later cursor creations to finish removal. All cursor tables are transient migration-only data: they are auto-migrated for runtime use but excluded from database snapshot export and import.

`GET /credentials/in-flight/summary` returns eventually consistent observed totals. For a credential with a concurrency policy, it also joins authoritative `max_in_flight`, admitted counters, remaining capacity, saturation, and a `limiter` object. The observed `in_flight` value remains snapshot-derived; `observed` keeps accounted and unaccounted snapshot totals separate from authoritative limiter state. Credentials without a policy retain the compatible `null`/`false` limiter fields.

Both responses include `observed_at`, `stale`, `coverage_complete`, `aggregates_complete`, `protocol_coverage_complete`, `minimum_processed_barrier_revision`, and `details_truncated`. The minimum barrier is the lowest processed barrier from the exact active membership lifetimes with visible snapshots. Freshness is calculated from Home database ingestion time, not CPA wall-clock time. Only complete multipart revisions are visible. Detail truncation does not make aggregates incomplete; a canceling membership, an active-lifetime mismatch, an incomplete/newer attempt, or a non-v1 active protocol makes diagnostics incomplete.

`GET /auth-files` joins observations and authoritative limiter state by the stable DB credential UUID (`id`/`auth_index`). It always exposes the compatible in-flight fields: `in_flight`, `max_in_flight`, `max_in_flight_by_model`, `remaining`, `total_saturated`, `saturated_model_count`, and `admitted_in_flight`, plus `observed` and `limiter`. Credentials without a matching observation have `in_flight` and `observed` set to `null`; authoritative limiter counters remain available when a policy exists. Observation and limiter reads are independent optional joins: failure of either leaves only that projection `null` and still returns the auth files.

`GET /credentials/concurrency-policies` lists stored policies. `GET` and `PATCH /credentials/:credential_id/concurrency-policy` read or replace the presence-aware `max_in_flight` and `max_in_flight_by_model` fields. `PATCH` accepts an optional policy `version`; a stale version returns HTTP `409`. `GET /credentials/concurrency` consumes only policy and admitted-counter state; it never reads observations, always returns `observed: null`, and returns `fully_enforced: "unknown"`.

When an observation is joined to a policy in `/credentials/in-flight`, `/credentials/in-flight/summary`, or `/auth-files`, `fully_enforced` is a diagnostic only: `"unknown"` for absent, stale, canceling, incomplete, non-v1-protocol, or barrier-behind observations; `"false"` when all coverage and barrier checks are known and the credential has unaccounted observed work; and `"true"` when those checks are known and unaccounted work is zero. A policy barrier is acknowledged only when the minimum processed active-lifetime observation barrier is at least the policy barrier. If the optional limiter read fails, the in-flight summary and details still return their observation with `limiter: null`.

`PATCH /auth-files/fields` accepts the same limiter fields and optional `version`. Auth metadata and policy changes are one database transaction, so either both persist or neither does. Limiter fields are not written into auth JSON.

### Credential concurrency policy and limiter routes

Before activating a policy, follow the [strict credential concurrency limits deployment runbook](../README.md). It defines the required topology, capability checks, certificate identities, and fail-closed mixed-version rollout.

The policy route groups are `GET /credentials/concurrency-policies`, `GET /credentials/:credential_id/concurrency-policy` plus `PATCH /credentials/:credential_id/concurrency-policy`, and `GET /credentials/concurrency`. The first two read stored policy; the patch replaces only supplied fields; the final route reads policy plus the authoritative admitted-counter state. `PATCH /auth-files/fields` is the auth-files compatibility form of the same policy patch.

A patch body must be valid JSON and supply at least one of `max_in_flight` or `max_in_flight_by_model`. A supplied total or model limit is `0` to remove that limit, or an integer from `1` through `credential-concurrency.max-limit`; model keys must be valid canonical model keys and cannot collide after canonicalization. `null` clears a supplied total or model map. `version` is optional optimistic concurrency control: the response includes the current `version`, and a stale submitted version is rejected rather than overwriting a newer policy.

`admitted_in_flight` is the authoritative counter, not a reconstructed observation. `GET /credentials/concurrency` never joins snapshots, so every item has `observed: null` and `fully_enforced: "unknown"`. The compatible `/auth-files` fields remain `in_flight`, `max_in_flight`, `max_in_flight_by_model`, `remaining`, `total_saturated`, `saturated_model_count`, `admitted_in_flight`, `observed`, and `limiter`; absent observations do not hide an available authoritative counter.

The Management capability `credential_concurrency_limits_v2` declares policy and admitted-counter support. Policy errors always use the envelope `{ "error": "<code>", "message": "<message>" }`. Clients must branch on the exact HTTP status and `error` code below; decoder and database detail in `message` is not stable.

| Condition | HTTP status | `error` code | `message` on the wire |
| --- | --- | --- | --- |
| PATCH body is not valid JSON or cannot be decoded | `400` | `invalid body` | The JSON decoder detail. |
| PATCH has neither `max_in_flight` nor `max_in_flight_by_model` | `400` | `no fields to update` | `no fields to update` |
| A supplied limit or model is invalid, or canonical model keys collide | `400` | `invalid_concurrency_policy` | The validation detail, such as `invalid credential concurrency limit`, `invalid credential concurrency model`, or `duplicate credential concurrency model: <model>`. |
| Credential does not exist | `404` | `not_found` | `credential concurrency policy credential not found` |
| Submitted `version` is stale | `409` | `concurrency_policy_version_conflict` | `credential concurrency policy version conflict` |
| Activating a policy is blocked by a legacy CPA, a capability-missing Home, or multiple active Homes on SQLite | `500` | `concurrency_policy_write_failed` | Respectively `legacy CPA membership is active`, `active Home lacks credential concurrency limiter capability`, or `active concurrency limits require a single SQLite Home`. |
| Policy list/read-after-patch cannot be loaded | `500` | `concurrency_policy_load_failed` | Database error detail. |
| Authoritative limiter state cannot be loaded | `500` | `concurrency_state_load_failed` | Database error detail. |
| Another policy write failure occurs | `500` | `concurrency_policy_write_failed` | Underlying write error detail. |

The `credential_in_flight_snapshots` capability is independent from `credential_concurrency_limits_v2`. The latter declares Management concurrency policy and authoritative counter support. `credential_in_flight_snapshot_cursor` independently declares the database-backed stable pagination contract for `GET /credentials/in-flight`. Snapshot availability, freshness, barrier acknowledgement, staging, overflow, and cleanup never participate in admission or release. Observation failures are best effort and do not gate traffic.

The observation configuration is database-backed for Home-managed CPA nodes. `credential-in-flight.snapshot-interval` defaults to `2s`; `stale-after` defaults to `10s` and must be at least three snapshot intervals; `max-part-bytes`, `max-part-count`, `max-revision-bytes`, `max-aggregate-groups`, `max-details`, and `max-string-bytes` default to `262144`, `64`, `16777216`, `100000`, `10000`, and `256`; `staging-retention` defaults to `1m`. Cleanup uses Home database time. Closed lifetimes are removed with their exact fingerprint and `membership_connected_at`; stale staging never removes an active or canceling lifetime's watermark or highest-revision parts.

Request details are limited to the documented fields. Headers, bodies, credentials, API keys, tokens, and certificate material are prohibited.

Historical `in_flight_lease` tables are not used by this feature and automatic migration does not delete them. Operators may remove those legacy tables only through an explicit, separately planned maintenance procedure after confirming that no prior deployment still depends on them.

### GET `/capabilities`

Returns the frontend capability flags and build metadata exposed by the current Home Management API. The management panel uses this endpoint to decide whether to enable usage overview, request records, aggregate rankings, export, realtime diagnostics, health attribution, request events, and request log indexes.

Response fields:

| Field | Type | Description |
| --- | --- | --- |
| `capabilities.usage` | boolean | Whether the legacy `GET /api-key-usage` capability is available. |
| `capabilities.quota_snapshots` | boolean | Whether the DB-backed `GET /quota/credentials` snapshot list is available. |
| `capabilities.quota_snapshot_details` | boolean | Whether `GET /quota/credentials/:credential_id` is available. |
| `capabilities.quota_recollect` | boolean | Whether `POST /quota/collect` on-demand collection is available. |
| `capabilities.usage_overview` | boolean | Whether `GET /usage/overview` is available. |
| `capabilities.usage_records` | boolean | Whether `GET /usage/records` is available. |
| `capabilities.usage_record_details` | boolean | Whether `GET /usage/records/:id` is available. |
| `capabilities.usage_aggregates` | boolean | Whether `GET /usage/aggregates` is available. |
| `capabilities.usage_export` | boolean | Whether `GET /usage/export` is available. |
| `capabilities.usage_provider_health` | boolean | Whether `GET /usage/health/providers` is available. |
| `capabilities.usage_credential_health` | boolean | Whether `GET /usage/health/credentials` is available. |
| `capabilities.usage_realtime` | boolean | Whether `GET /usage/realtime` is available. |
| `capabilities.usage_token_breakdown_v2` | boolean | Whether every persisted usage row has the canonical token-accounting v2 breakdown. It remains `false` while the resumable historical backfill is pending. |
| `capabilities.request_log_index` | boolean | Whether `GET /request-logs` is available. |
| `capabilities.request_events` / `capabilities.requestEvents` | boolean | Whether `GET /request-events` is available. |
| `capabilities.request_event_details` / `capabilities.requestEventDetails` | boolean | Whether `GET /request-events/:id` is available. |
| `capabilities.request_event_export` / `capabilities.requestEventExport` | boolean | Whether `GET /request-events/export` is available. |
| `capabilities.request_event_filters` / `capabilities.requestEventFilters` | boolean | Whether `GET /request-events/filter-options` is available. |
| `capabilities.oauth_usage` | boolean | Whether OAuth/file-backed credential usage attribution is reliable. |
| `capabilities.logs` | boolean | Whether application log APIs are available. |
| `capabilities.request_error_logs` | boolean | Whether request error log file list/download APIs are available. |
| `capabilities.model_channel_bindings` | boolean | Whether model group details support model-specific channel group bindings through `channels`. |
| `capabilities.topology` | boolean | Whether `GET /topology` is available for Home + CPA cluster topology. |
| `capabilities.users` | boolean | Whether the `/users` user-management routes are available. |
| `capabilities.access_groups` | boolean | Whether the `/channel-groups` and `/model-groups` access-scope routes are available. |
| `capabilities.user_period_limits` | boolean | Whether user period-limit configuration fields plus `GET /users/:id/period-limits` and `POST /users/:id/period-limits/reset` are available. |
| `capabilities.credential_in_flight_snapshots` | boolean | Whether the independent CPA in-flight observation snapshot APIs are available. |
| `capabilities.credential_in_flight_snapshot_cursor` | boolean | Whether `GET /credentials/in-flight` supports database-backed stable snapshot cursors for multi-page reads. |
| `capabilities.credential_concurrency_limits_v2` | boolean | Whether concurrency policy and authoritative admitted-counter APIs are available. |
| `capabilities.credential_cooldown_reset` | boolean | Whether `DELETE /credentials/:credential_id/cooldown` supports all-model and model-scoped quota cooldown resets. |
| `server_info.home_version` | string | Home build version. |
| `server_info.home_commit` | string | Home build commit. |
| `server_info.home_build_date` | string | Home build time. |

### Quota Snapshot Conventions

Quota snapshot endpoints are read-only DB views. Reading them does not call an upstream provider, refresh OAuth tokens, change scheduler priority, or consume a queue. A credential that exists but has never produced a snapshot or collection attempt is returned with `quota_status=unknown`, `freshness=never`, an empty window list, and HTTP `200`. If its first collection attempt fails before any usable quota fact exists, it is returned as `quota_status=error`, `freshness=never`, and `collection_status=failed`. A credential whose provider is explicitly outside the current collector plan is returned as `unsupported`. Deleted credentials are not visible and their quota rows are deleted with the credential. Changing a credential's provider or credential type also discards snapshots from the previous identity.

All timestamps are RFC3339 UTC values or `null`. Ratios are numbers in `[0,1]`. Quantity fields may be `null`; unlimited quota uses `is_unlimited=true`. Different provider periods remain separate windows. While a snapshot is fresh, an individually expired merged window is omitted and no longer contributes to status/source. Once the snapshot itself is stale, the detail view retains its last-known windows for diagnosis. `earliest_reset_at` is the minimum non-null `reset_at` across the same complete internal window set represented by the credential item. It may be in the past for stale last-known data.

Current passive collection extracts a bounded `quota_headers` object from the CPA usage event `response_headers`. Home preserves only the Codex `X-Codex-*` quota allowlist plus a syntax-validated, non-secret-like upstream request ID, and removes the raw `response_headers` object before writing the usage payload. The reported `auth_index` is resolved against the active auth UUID, runtime index, and ID before the snapshot is stored under the stable UUID. Codex Header observations are normalized and upserted in the same database transaction as the usage record; invalid quota metadata is isolated so it cannot roll back the core usage or billing write. Timestamps more than five minutes ahead of Home's receive time are normalized to the receive time. Older observations cannot replace a newer snapshot or window, including concurrent first writes. Codex Header observations are always treated as sparse rolling updates: they merge by stable limit identity and period, retain still-valid windows and metadata learned by the authoritative active probe, do not clear an in-flight probe lease, and do not postpone the next active probe or bypass its retry backoff. Unprefixed Primary/Secondary windows are accepted only when `X-Codex-Active-Limit` contains a valid limit identity: `codex` and `premium` map to the default account family, `codex_bengalfox` maps to the Spark model family, and other valid identities remain isolated model families. Missing or invalid active-limit metadata causes only the unprefixed windows to be ignored; explicitly grouped windows remain usable. If the active family is also present as an explicit group, matching identity-and-period windows collapse to one entry, the unprefixed observation supplies the quota values, and the explicit group may supply its label. Repeated Primary/Secondary observations for the same limit period also collapse to one window. Expired windows cannot make a new snapshot appear healthy or exhausted.

Home also runs fixed-target active collectors for Claude, Antigravity, Codex, Kimi, and xAI OAuth/file credentials. Codex reads the official usage endpoint, uses `metered_feature` as the stable identity for additional limits, and derives normalized plan metadata. It queries the reset-credit detail endpoint independently of whether the usage summary contains `rate_limit_reset_credits`. If that detail request fails while the usage summary reports a positive current count, the collection is `partial` and stores the latest count with an empty detail list instead of carrying forward older quantities or expired credits. If neither endpoint provides reliable reset-credit information, older reset-credit data is cleared without degrading otherwise usable quota windows. Codex `primary_windows` keeps one representative from the default account family and one from `codex_bengalfox`, preferring the longest (normally weekly) window in each family, so the default 5-hour window or code-review limits cannot hide Spark. Existing Codex v1 snapshots that use positional `*-primary`/`*-secondary` IDs trigger one immediate v2 active recollection even when freshness or `next_probe_at` would normally defer it. The upgrade attempt is recorded before the request, so a failure returns to the normal retry backoff instead of bypassing it repeatedly. Legacy raw windows remain stored for diagnosis, but the API reports them as `unknown`/`stale` before a successful upgrade and `error`/`stale` after a failed upgrade; a successful v2 probe atomically replaces the old IDs, and later passive Header observations cannot downgrade the snapshot version. No reset-credit consume operation is exposed through the Management API. Claude reads usage and profile, reporting `partial` when quota succeeds but profile metadata fails. Antigravity calls the grouped quota-summary endpoint with the credential `project_id` and maps only `gemini-5h`, `gemini-weekly`, `3p-5h`, and `3p-weekly` into stable `gemini` and `third-party` model scopes. Numeric fractions, numeric strings, and percentage strings are accepted; disabled, unknown, or malformed buckets are omitted independently. Both weekly buckets are required before a response can replace the last-known snapshot, while either 5-hour bucket remains independently optional. Primary selection keeps one window per stable scope and prefers that scope's 5-hour bucket when present. Existing model-ID Antigravity v1 snapshots trigger an immediate v2 recollection; they are reported as `unknown`/`stale` while upgrade is pending and `error`/`stale` after a failed upgrade, with normal retry backoff. A successful v2 probe atomically replaces the legacy model windows. Kimi reads coding usage and preserves both the account usage summary and each returned limit window while accepting numeric fields encoded as numbers or strings. Kimi provider limits take primary-window priority over its aggregate summary so weekly and duration limits remain visible in list views. xAI calls the Grok CLI billing endpoint with the CLI token-auth, client-version, user-agent, and optional user-ID headers. It accepts camelCase or snake_case billing fields and `{ "val": ... }`, numeric, or string cent values. `monthlyLimit=15000` maps to SuperGrok and `monthlyLimit=150000` maps to SuperGrok Heavy. Positive `onDemandCap` plus explicit or derived `onDemandUsed` values produce the `xai-on-demand` monthly USD window; a missing or zero cap means pay-as-you-go is disabled and no such window is emitted. Provider API-key credentials that cannot use these OAuth collectors are returned as `unsupported`.

Collectors read DB credentials directly and never accept a URL from HMC. Before probing, they resolve the latest DB credential and refresh OAuth state when the runtime refresh policy says it is due. They use the current hot-reloaded global proxy unless the credential has its own proxy. They use a 20-second request timeout, a per-provider concurrency limit of 3 on PostgreSQL and a global limit of 1 on SQLite, a per-credential DB lease, and a five-minute exponential retry backoff with per-credential jitter capped near one hour. `Retry-After` can extend the next attempt. Disabled credentials and credentials whose retry deadline is still in the future are not actively probed; persisted unavailable/error state no longer blocks recovery after that deadline. Successful snapshots are fresh for 30 minutes. Failures preserve last-known windows and store only structured, redacted error metadata.

Credential fields:

| Field | Type | Description |
| --- | --- | --- |
| `credential_id` | string | Stable Home auth UUID used by the detail path. |
| `auth_index` | string/null | Current runtime auth index. |
| `provider` | string | Normalized provider ID. |
| `credential_type` | string | `oauth`, `provider_api_key`, `file_auth`, `vertex`, or `unknown`. |
| `label` | string | Safe display label with a stable fallback. |
| `account`, `project` | string/null | Masked display account/project metadata when present. Search uses these displayed values. |
| `credential_status` | string | `enabled`, `disabled`, `unavailable`, `cooldown`, or `unknown`. |
| `quota_status` | string | `healthy`, `low`, `exhausted`, `unknown`, `error`, or `unsupported`. |
| `freshness` | string | `fresh`, `stale`, or `never`. `stale` is computed when current time reaches `expires_at`; a legacy observation without an expiry is also stale. |
| `collection_status` | string | `idle`, `collecting`, `success`, `partial`, `failed`, or `unsupported`. |
| `source` | string/null | `response_header`, `active_probe`, `mixed`, or `null`. |
| `observed_at`, `expires_at` | string/null | Latest valid observation and its freshness deadline. |
| `earliest_reset_at` | string/null | Earliest reset across all effective windows, including windows omitted from `primary_windows`. Stale last-known values may be in the past. |
| `last_attempt_at`, `last_success_at`, `next_probe_at` | string/null | Collection scheduling metadata. |
| `consecutive_failures` | integer | Consecutive collection failures. |
| `primary_windows` | array | Provider-aware stable selection of at most two current windows. Codex returns one longest representative for the default account family and one for `codex_bengalfox` when present. |
| `window_count` | integer | Number of all current windows. |
| `error` | object/null | Redacted collection error. Messages are capped at 500 bytes. |
| `runtime` | object/null | Home and CPA ownership metadata. |
| `plan` | object/null | Provider subscription plan metadata derived by the collector (for example Codex `Pro 20x` or xAI `SuperGrok`). Shape: `{"name": "Pro 20x", "premium": true}`. A successful authoritative probe clears an older plan when the current payload no longer maps to one. |

Quota window fields:

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Stable per-credential window ID. |
| `label`, `scope_id`, `currency` | string/null | Optional display metadata. |
| `scope` | string | `account`, `project`, `model`, `organization`, or `unknown`. |
| `mode` | string | `rolling`, `fixed`, `balance`, or `unknown`. |
| `status` | string | Window-level quota status. |
| `unit` | string | `requests`, `tokens`, `credits`, `currency`, `percentage`, or `unknown`. |
| `used`, `remaining`, `limit` | number/null | Normalized quantities. |
| `used_ratio`, `remaining_ratio` | number/null | Normalized ratios in `[0,1]`. |
| `is_unlimited` | boolean | Whether this window has no finite limit. |
| `reset_at` | string/null | Next reset boundary. |
| `window_seconds` | integer/null | Duration when it has a safe seconds representation. |
| `period_unit`, `period_value` | mixed | Structured display period. Unit is `minute`, `hour`, `day`, `week`, `month`, or `unknown`; `period_value` is positive for known units and `null` for `unknown`. |
| `source` | string | Window collection source. |
| `observed_at` | string | Actual window observation time. |

### GET `/quota/credentials`

Returns filtered, paginated current credential snapshots.

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `limit` | integer | `50` | Page size from 1 to 200. |
| `offset` | integer | `0` | Non-negative result offset. |
| `search` | string | none | Case-insensitive contains match over label, account, project, auth index, and provider. |
| `ids` | string/CSV | none | One or more exact, case-sensitive credential IDs for direct batch joins. ID-filtered reads load quota windows only for the requested credentials while `global_summary` remains unfiltered. |
| `provider` | string/CSV | none | One or more provider IDs. |
| `quota_status` | string/CSV | none | One or more quota statuses. |
| `freshness` | string/CSV | none | `fresh`, `stale`, or `never`. |
| `source` | string/CSV | none | `response_header`, `active_probe`, `mixed`, or `none`. |
| `credential_status` | string/CSV | none | One or more credential statuses. |
| `collection_status` | string/CSV | none | One or more collection statuses. |
| `sort` | string | `risk_desc` | `risk_desc`, `observed_at_desc`, `observed_at_asc`, `reset_at_asc`, `provider_asc`, or `label_asc`. `reset_at_asc` uses the returned `earliest_reset_at`, with `null` values last. |

`summary` and `facets` cover the complete filtered result before pagination. `global_summary` covers all visible credentials without list filters. `needs_attention` counts each credential once when it is non-healthy, non-fresh, partial, or failed.

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

Returns the same credential core object, every current window in stable order, optional provider-specific read-only detail metadata, collection metadata, and `generated_at`. Unknown or unsupported current credentials return HTTP `200`; missing, deleted, or invisible credentials return `404`.

```json
{
  "credential": {
    "credential_id": "auth-db-id",
    "provider": "codex",
    "quota_status": "low",
    "plan": {"name": "Pro 20x", "premium": true},
    "earliest_reset_at": "2026-07-16T01:10:00Z",
    "primary_windows": [],
    "window_count": 1
  },
  "windows": [],
  "reset_credits": {
    "available_count": 3,
    "observed_at": "2026-07-16T01:00:00Z",
    "credits": [
      {
        "status": "available",
        "granted_at": "2026-07-15T01:00:00Z",
        "expires_at": "2026-07-27T07:40:51Z"
      }
    ]
  },
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

`reset_credits` is `null` when the provider does not report this capability or no reliable observation is available. It is detail-only and is not included in `GET /quota/credentials` list items. `available_count` is the latest provider-reported number of currently available credits, `observed_at` is the time of that reset-credit observation, and `credits` contains the bounded, expiry-sorted list of currently available Codex rate-limit reset credits. When the usage summary supplies a positive count but the independent detail request fails, `available_count` remains current while `credits` is empty and `collection_status=partial`; older quantities and expired detail rows are not presented as current. Credit entries expose only status and timing metadata; provider reset-credit identifiers are not returned. This endpoint is read-only and does not consume a credit.

### POST `/quota/collect`

Starts an asynchronous on-demand quota collection round and returns how many credentials were accepted into the local collector queue. This is the only quota endpoint that is not read-only. On-demand jobs share the same process-wide provider concurrency controls as scheduled collection and are deduplicated per credential while queued or running.

Request body (all fields optional; an empty body collects every eligible credential):

| Field | Type | Description |
| --- | --- | --- |
| `credential_ids` | string array | Only these credential IDs are collected. |
| `providers` | string array | Only these providers are collected: `claude`, `antigravity`, `codex`, `kimi`, `xai`; `grok` is accepted as an alias for `xai`. Other values return `400`. Both filters combine with AND semantics. |

Response `202`:

```json
{
  "accepted": 2,
  "running": true
}
```

`accepted` counts eligible credentials newly queued by this request. Disabled, execution-cooldown, collector-unsupported, and already queued/running local credentials are skipped. When a queued job reaches a concurrency slot it force-claims the DB probe lease: an active lease is still respected, but snapshot freshness, `next_probe_at`, and quota retry backoff do not suppress the requested attempt. The collection round runs in the background; read updated snapshots through `GET /quota/credentials` after it finishes. When the runtime has no quota collector wired, the route returns `404` with `QUOTA_RECOLLECT_UNSUPPORTED` and `capabilities.quota_recollect` is `false`.


Quota endpoint validation errors use:

```json
{
  "error": {
    "code": "INVALID_FILTER",
    "message": "quota_status contains an unsupported value",
    "request_id": "",
    "retryable": false
  }
}
```

Invalid filters, sorts, or pagination return `400`; missing credentials return `404`; temporary database/context unavailability returns `503`; other database read failures return `500`.

### Usage Observability Conventions

These endpoints read persisted `usage`, `billing_charge`, `api_key`, `user`, and `auth` data. Responses never return raw client access keys, provider API keys, OAuth tokens, cookies, authorization headers, complete payloads, or complete failure bodies. They may return `api_key_masked`, redacted `body_preview`, and payload summaries.

Canonical token accounting is available only when `capabilities.usage_token_breakdown_v2=true`. Clients must read that declared capability and must not infer support by probing a usage route or rebuilding a breakdown from the legacy token counters. While the capability is `false`, the server may already return `token_breakdown` objects for newly normalized rows, but the selected range can still contain historical rows awaiting backfill. The legacy `input_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens`, `cache_read_tokens`, `cache_creation_tokens`, and `total_tokens` fields remain available for diagnostics and backward compatibility; they are not mutually exclusive and must not be stacked into a total.

When enabled, `totals`, every `trend[]` bucket, every usage record, and every aggregate item include this schema:

```json
{
  "token_breakdown": {
    "schema_version": 2,
    "quality": "complete",
    "total_tokens": 39346,
    "input": {
      "total_tokens": 39320,
      "uncached_tokens": 6040,
      "cache_read_tokens": 33280,
      "cache_write_tokens": 0
    },
    "output": {
      "total_tokens": 26,
      "non_reasoning_tokens": 26,
      "reasoning_tokens": 0
    },
    "unclassified_tokens": 0
  }
}
```

All values are non-negative and the buckets are mutually exclusive. The enforced invariants are `input.total_tokens = uncached_tokens + cache_read_tokens + cache_write_tokens`, `output.total_tokens = non_reasoning_tokens + reasoning_tokens`, and `total_tokens = input.total_tokens + output.total_tokens + unclassified_tokens`. `quality` is `complete`, `unclassified`, or `inconsistent`. Contradictory upstream totals are not silently repaired: the authoritative total is placed in `unclassified_tokens` and quality becomes `inconsistent`. Legacy rows from known protocol families are normalized during ingestion or the resumable background backfill; unknown provider semantics remain `unclassified`. The backfill uses a persisted high-water cursor and PostgreSQL advisory lock, rechecks for late pre-v2 rows during mixed-version operation, does not modify historical billing charges, and enables the capability only after no pre-v2 rows remain.

The aggregate range parameters apply to `/usage/overview`, `/usage/aggregates`, `/usage/realtime`, `/usage/health/providers`, and `/usage/health/credentials`, and they also act as the base range for `/usage/records` and `/usage/export`. All usage ranges use the half-open interval `[from,to)`: `from` is included and `to` is excluded. A date-only `to` value is normalized to the next local midnight, so the selected calendar day remains fully included across DST transitions. `/usage/overview` and `/usage/aggregates` automatically fill a recent 24-hour window when `from` or `to` is missing; `/usage/records` and `/usage/export` do not fill a time range automatically.

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `from` | string | `to - 24h` for `/usage/overview` and `/usage/aggregates`; none for other endpoints | Start time. Supports `YYYY-MM-DD`, RFC3339, or Unix seconds. Date-only values are interpreted as 00:00:00 in `timezone`. |
| `to` | string | current time for `/usage/overview` and `/usage/aggregates`; none for other endpoints | Exclusive end time. Supports `YYYY-MM-DD`, RFC3339, or Unix seconds. Date-only values include the full day in `timezone` by using the next local midnight as the exclusive boundary. |
| `timezone` | string | `UTC` | Statistics timezone for date-only `from`/`to` and `day`/`week` trend buckets. |
| `provider` | string | none | Exact provider filter. |
| `model` | string | none | Fuzzy model filter. |
| `credential_type` | string | none | Execution credential type: `provider_api_key`, `oauth`, `file_auth`, `vertex`, or `unknown`. |
| `home_ip` | string | none | Home node IP. |
| `endpoint` | string | none | Fuzzy endpoint filter. |

Amount fields use the current Home billing credit/point unit. When billing is enabled and usage can be attributed to `billing_charge`, `amount` or `total_amount` returns a number, `currency` returns `credits`, and `billing_basis` returns `billing_charge`. When attribution is not reliable, amount fields return `null`; the API does not fabricate estimated charges.

Key `UsageRecordSummary` fields:

| Field | Type | Description |
| --- | --- | --- |
| `upstream_request_id` | string/null | Upstream request ID parsed from the payload. |
| `event_type` | string/null | Normalized event type, parsed from payload fields or derived from the endpoint. |
| `upstream_status_code` | integer/null | Upstream status code parsed from structured usage columns or payload fields. |
| `source` | string/null | Usage payload source. |
| `service_tier` | string/null | Usage payload service tier. |
| `reasoning_effort` | string/null | Usage payload reasoning effort. |
| `token_breakdown` | object | Canonical v2 mutually exclusive input/output/unclassified token buckets. Consume only when `capabilities.usage_token_breakdown_v2=true`. |
| `client.client_ip` | string/null | Caller IP associated with the usage payload. This is not treated as the CPA node IP. |
| `credential.api_key_preview` | string/null | Redacted provider API key preview when available; raw keys are never returned. |
| `billing.balance_before` / `billing.balance_after` | number/null | Balance before and after the charge when linked to `billing_charge`. |
| `runtime.home_ip` / `runtime.home_port` | string/integer/null | Home node identity that persisted the usage record. |
| `runtime.cpa_node_id` / `runtime.cpa_ip` / `runtime.cpa_port` / `runtime.cpa_label` | mixed | CPA ownership fields. Home fills missing CPA node ID/IP from the trusted RESP/mTLS runtime identity when the CPA payload does not report them. |
| `runtime.request_log_available` | boolean | Whether the request log is locally present or can be downloaded through configured cluster forwarding. Remote availability is routability, not a remote file-existence guarantee. |
| `runtime.log_home_ip_required` | boolean | Whether request log download requires the Home IP. |

### GET `/usage/overview`

Returns a usage overview with the applied range, short-window live snapshot, totals, trends, default top groups, and activity buckets.

Query parameters in addition to aggregate range parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `interval` | string | `auto` | `minute`, `hour`, `day`, `week`, or `auto`. `day` and `week` buckets use `timezone`; response timestamps remain UTC RFC3339. A response is limited to 10,000 trend buckets. `auto` promotes to a coarser interval when needed; an explicit interval that exceeds the limit returns `400 invalid_interval_range`. |

Top-level response fields:

| Field | Type | Description |
| --- | --- | --- |
| `range` | object | Applied time range, timezone, and interval. |
| `live` | object | Recent short-window RPM, TPM, error rate, and latency. |
| `totals` | object | Request counts, success/failure counts, tokens, `token_breakdown`, amount, latency, and active subject counts. |
| `trend` | array | Contiguous trend buckets intersecting the applied half-open range at `interval`, including zero-request buckets. Every bucket contains `token_breakdown`; the first and last buckets may be partial. |
| `cost_breakdown` | array | Empty when reliable cost splitting is unavailable; the API does not fabricate indivisible cost details. |
| `model_efficiency` | array | Model efficiency list sorted by total tokens. |
| `top` | object | `users`, `client_keys`, `credentials`, `providers`, `models`, `endpoints`, and `errors`. |
| `activity` | array | Health activity series aligned one-for-one with the contiguous trend buckets. `status` is `empty` for zero requests, `healthy` below 5% errors, `degraded` from 5% to below 50% errors, and `unavailable` from 50% errors. |

### GET `/usage/records`

Returns the request record table with server-side pagination, filters, and sorting.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `limit` | integer | `50` | Maximum `200`. |
| `offset` | integer | `0` | Page offset. |
| `sort` | string | `timestamp_desc` | Supports `timestamp_desc`, `timestamp_asc`, `tokens_desc`, `tokens_asc`, `cost_desc`, `cost_asc`, `latency_desc`, `latency_asc`, and `failed_first`. |
| `search` | string | none | Fuzzy search across request ID, provider, model, endpoint, Home IP, CPA node ID/IP/label, username, masked key, and credential label. |
| `status` | string | none | `success` or `failed`. |
| `status_code` | integer | none | HTTP/failure status code. 2xx/3xx values match successful requests; other values match `fail_status_code`. |
| `request_id` | string | none | Exact request ID filter. |
| `event_type` | string | none | Normalized event type filter. Common values include `completion`, `response`, `message`, `embedding`, and `stream`. |
| `cpa_node` | string | none | Fuzzy filter across structured CPA node ID, CPA IP, CPA label, and CPA port. |
| `user` / `user_id` | string / integer | none | Username or user ID. |
| `client_key` / `client_key_id` | string / integer | none | Client access key masked value, label, or ID. |
| `credential_id` / `auth_index` | string | none | Execution credential filter. |
| `executor_type` | string | none | Exact executor type filter. |
| `min_latency_ms` / `max_latency_ms` | integer | none | Latency range. |
| `min_amount` / `max_amount` | number | none | Billing amount range. |

The response contains `items`, `total`, `limit`, `offset`, `sort`, and `sortable_fields`. `items[]` is a redacted request summary with legacy `tokens`, canonical `token_breakdown`, `performance`, `client`, `credential`, `billing`, `runtime`, and optional `error`.

### GET `/usage/records/:id`

Returns one usage detail. `id` is the usage ID.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `include_payload` | boolean | `false` | Return a redacted payload summary. |
| `include_logs` | boolean | `false` | Return up to 20 redacted log lines when a local request log is found. Remote nodes or missing files return an empty array. |

The response contains `record`, `payload_summary`, `log_excerpt`, and `related`. `payload_summary` only contains `method`, `stream`, `message_count`, and `tool_count`; raw payloads are never returned. `related.request_log` contains `request_id`, `home_ip`, `home_port`, `available`, and `download_url` with the same local-file and remote-forwarding availability semantics as the request event APIs.

### GET `/usage/aggregates`

Returns server-side aggregate rankings after full-result sorting.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `group_by` | string | required | `user`, `client_key`, `credential`, `provider`, `model`, `endpoint`, `home_ip`, `executor_type`, or `status_code`. |
| `metric` | string | `request_count` | `request_count`、`total_tokens`、`total_amount`、`failed_count`、`avg_latency_ms`、`p95_latency_ms`。 |
| `direction` | string | `desc` | `desc` or `asc`. |
| `limit` | integer | `20` | Maximum `100`. |
| `offset` | integer | `0` | Page offset. |

The response contains `group_by`, `metric`, `direction`, `items`, `total`, `limit`, `offset`, and `sortable_metrics`. Every `items[]` entry includes canonical `token_breakdown` in addition to the legacy token counters.

### GET `/usage/export`

Exports redacted request records for the current records filters.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `format` | string | `csv` | `csv` or `jsonl`. |
| records filters | mixed | none | Same as `GET /usage/records`. When `limit` is omitted, the endpoint exports at most `10000` rows by default; explicit `limit` values are also capped at `10000`. |

Responses are attachments. CSV uses `text/csv; charset=utf-8`; JSONL uses `application/x-ndjson`.

Export fields are flattened redacted summaries. In addition to core record response fields, they include `error_status_code`, `error_message`, `error_body_preview`, `request_log_available`, and `log_home_ip_required`. Token accounting v2 is exported as `token_breakdown_schema_version`, `token_breakdown_quality`, `token_breakdown_total_tokens`, the four `token_breakdown_input_*` fields, the three `token_breakdown_output_*` fields, and `token_breakdown_unclassified_tokens`.

### GET `/request-events`

Returns the request event list for the management UI. This endpoint is DB-backed and read-only. It reads persisted usage observability records and does not read or consume `/usage-queue`.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `from` / `to` / `timezone` | string | none / `UTC` | Same as `/usage/records`. |
| `limit` / `offset` | integer | `50` / `0` | Server-side pagination. `limit` is capped at `200`. |
| `sort` | string | `timestamp_desc` | Supports `timestamp_desc`, `timestamp_asc`, `latency_desc`, `latency_asc`, `tokens_desc`, `tokens_asc`, `cost_desc`, `cost_asc`, and `failed_first`. |
| `search` | string | none | Fuzzy search across request ID, provider, model, endpoint, Home IP, CPA node ID/IP/label, username, masked key, and credential label. |
| `request_id` | string | none | Exact request ID filter. |
| `event_type` | string | none | Event type filter. The value is parsed from `event_type`/`type` payload fields or derived from the endpoint. Common values are `completion`, `response`, `message`, `embedding`, and `stream`. |
| `status` / `status_code` | string / integer | none | `success`, `failed`, or status code filter. |
| `provider` / `model` | string | none | Exact provider filter and fuzzy model filter. |
| `home_ip` | string | none | Home node filter. |
| `cpa_node` | string | none | Fuzzy filter across structured CPA node ID, CPA IP, CPA label, and CPA port. Home fills missing CPA node ID/IP from the trusted RESP/mTLS runtime identity when available. |
| `credential_id` / `auth_index` | string | none | Execution credential filter. |
| `user` | string | none | Username or user ID search. |
| `client_key` | string | none | Client access key masked value, label, or ID search. |
| `min_latency_ms` / `max_latency_ms` | integer | none | Latency range. |

The response contains `items`, `total`, `limit`, `offset`, and `sort`. `items[]` is a request event object with these key fields:

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Stable event ID in the `evt_<usage_id>` format. |
| `event_type` | string | Event type, parsed from the payload first and derived from the endpoint when absent. |
| `status` / `failed` / `status_code` / `upstream_status_code` | mixed | Request success/failure state and HTTP status. Successful requests default to `status_code=200` when no explicit status is available. |
| `provider` / `model` / `original_model` / `model_alias` / `endpoint` | mixed | Model and routing metadata. |
| `runtime.home_ip` / `runtime.home_port` / `runtime.home_id` | mixed | Home node that persisted the usage record. `home_id` is `home_ip:home_port` when the port is available. |
| `runtime.cpa_node_id` / `runtime.cpa_ip` / `runtime.cpa_port` / `runtime.cpa_label` | mixed | CPA ownership metadata. Home fills missing CPA node ID/IP from the trusted RESP/mTLS runtime identity when the CPA payload does not report them. |
| `credential` | object | Execution credential type, ID, auth index, provider, label, source, and redacted `api_key_preview`. |
| `client` | object | User, client key ID/label, redacted `client_key_masked`, and caller client IP. |
| `error` | object | Redacted error status, upstream status, reason, message, and body preview. |
| `tokens` / `performance` / `billing` | object | Token, latency/TTFT/TPS, and billing metadata. |
| `related.request_log` | object | Request log link metadata, including `home_ip` and `home_port` when available. Local availability is checked against the filesystem. For a remote Home, `available=true` and `download_url` mean cluster forwarding is configured and the download can be attempted. |

### GET `/request-events/filter-options`

Returns compact option lists for the request event filter UI. It accepts the same filters as `GET /request-events`, ignores pagination parameters, and returns distinct values from the filtered result set.

Response fields:

| Field | Type | Description |
| --- | --- | --- |
| `event_types` | array | Distinct normalized event types. |
| `providers` | array | Distinct providers. |
| `models` | array | Distinct models. |
| `home_ips` | array | Distinct Home IPs. |
| `cpa_nodes` | array | Distinct CPA labels, node IDs, or IPs. |
| `status_codes` | array | Distinct HTTP/upstream status codes encoded as strings for UI select controls. |

### GET `/request-events/:id`

Returns a single request event. `id` accepts either `evt_<usage_id>` or the raw usage ID.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `include_payload` | boolean | `false` | Return a redacted payload summary. Raw payloads are never returned. |
| `include_logs` | boolean | `false` | Return up to 20 redacted log lines when a local request log is found. |
| `include_related` | boolean | `false` | Compatibility parameter. The event object always includes `related`. |

The response contains `event`, `payload_summary`, and `log_excerpt`. `event` has the same shape as list items. `payload_summary.body_preview` is always `null` to avoid exposing request bodies. Log excerpts are local-only; remote logs are downloaded through `related.request_log.download_url` instead.

### GET `/request-events/export`

Exports request events for the current filters. Supports `format=csv` and `format=jsonl`; responses are attachments named `request-events.csv` or `request-events.jsonl`.

This endpoint accepts the same filters and sort parameter as `GET /request-events`, ignores pagination parameters, and exports at most `10000` rows. Export fields are flattened redacted summaries of list event objects, including Home/CPA, credential, client, error, token, performance, billing, and request log link fields.

### GET `/usage/realtime`

Returns a short-window realtime snapshot suitable for management panel polling.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `window_seconds` | integer | `900` | Statistics window. |
| `bucket_seconds` | integer | `60` | Velocity bucket size. |
| `group_by` | string | `model` | `model`、`provider`、`client_key`、`credential`。 |

Aggregate range parameters are also supported. The response contains `velocity`, `latency_distribution`, and `current_usage` grouped by `group_by`.

### GET `/usage/health/providers`

Returns recent-window health status by provider. Supports `window_seconds` and aggregate range parameters.

`items[]` contains `id`, `label`, `status`, `provider`, `recent_success_count`, `recent_failed_count`, `recent_error_rate`, `last_error_at`, `last_error_status`, `last_error_message`, `next_retry_at`, `avg_latency_ms`, and `p95_latency_ms`. `next_retry_at` comes from execution credential retry/cooldown metadata and is `null` when attribution is unavailable. `status` is `healthy`, `degraded`, `unavailable`, or `unknown`.

### GET `/usage/health/credentials`

Returns recent-window health status by execution credential. Parameters are the same as provider health. The response `subject` is `credential`, and `items[].credential_type` comes from usage/auth metadata. When credential metadata marks a credential as `disabled` or `unavailable`, `status` returns that state first.

### GET `/request-logs`

Returns the request log index. The index is generated from usage records. Local records are checked against the current Home filesystem; remote records are marked routable when cluster forwarding is configured.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `request_id` | string | none | Request ID filter. |
| `home_ip` | string | none | Home node filter. |
| `from` / `to` | string | none | Time range. |
| `provider` / `model` | string | none | Provider/model filter. |
| `status` / `status_code` | string / integer | none | Status filter. |
| `limit` / `offset` | integer | `50` / `0` | Pagination. |
| `search` | string | none | DB-side fuzzy search across request ID, model, provider, and status. Numeric timestamps or `.log` file name searches are matched against local file names within at most `10000` base records. |

`items[]` contains `id`, `request_id`, `timestamp`, `home_ip`, `home_port`, `file_name`, `size_bytes`, `available`, `provider`, `model`, `status`, and `download_url`. Local files return exact availability, file name, and size. Remote records return `available=true` and a non-empty `download_url` when cluster forwarding is configured; `file_name` and `size_bytes` can be `null` because the current Home does not inspect the remote filesystem. Actual downloads use `GET /request-log-by-id/:id`, and generated URLs include both `home_ip` and `home_port` when available. The download remains authoritative and can return `404` if a remote file was deleted or `502` if the target Home is unavailable.

### GET `/usage-queue`

Pops the oldest queued usage records.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `count` | positive integer | `1` | Number of records to pop. |

Example response:

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

Returns application log records from the database `log` table.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `home_ip` | string | Optional Home node IP filter. |
| `client_ip` | string | Optional CPA client IP filter. |
| `request_id` | string | Optional request ID filter. |
| `level` | string | Optional log level filter. |
| `after` | integer or RFC3339 | Optional timestamp lower bound. |
| `before` | integer or RFC3339 | Optional timestamp upper bound. |
| `limit` | integer | Maximum returned record count. Default is `100`, maximum is `1000`. |
| `offset` | integer | Pagination offset. Default is `0`. |

Example response:

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

Deletes all application log records from the shared database `log` table. In a cluster deployment, this clears records for every Home and CPA node; it does not delete or truncate local log files. The `removed` response field is the number of deleted database rows.

Input: none.

Example response:

```json
{
  "success": true,
  "message": "Logs cleared successfully",
  "removed": 3
}
```

### GET `/request-error-logs`

Lists `error-*.log` files when detailed request logging is disabled. Returns an empty list when detailed request logging is enabled.

Input: none.

Example response:

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

Downloads a request error log file.

Path parameters:

| Path | Type | Description |
| --- | --- | --- |
| `name` | string | Filename that starts with `error-` and ends with `.log`; slashes are rejected. |

Response: file attachment.

### GET `/request-log-by-id/:id`

Downloads a Home request log file from that Home's local `logs` directory. `home_ip` identifies which Home owns the file, and optional `home_port` disambiguates Home nodes that share the same IP. When the target is not the current Home, the current Home forwards the request to the target Home over an internal mTLS-only cluster route. Files are matched by request ID, and the file system remains the source of truth, so deleted files return `404`.

Path parameters:

| Path | Type | Description |
| --- | --- | --- |
| `id` | string | Request ID; slashes are rejected. |

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `home_ip` | string | Required Home node IP that owns the request log. |
| `home_port` | integer | Optional Home node port. Recommended when multiple Home nodes can share the same IP. |

Response: file attachment.

## Models

### GET `/models?scope=available|static`

Returns model definitions from either the current runtime registry or the static model catalog.

Each model includes `providers`, the authoritative provider identifiers used by `usage.provider`
and Billing model-price rules. Clients must create one candidate per `(provider, model)` pair and
must not derive Billing providers from `type`, `owned_by`, or a static catalog group name.
Clients must omit model records whose `providers` field is absent or empty because they do not have
a safe Billing identity.

Provider implementations must register every executor identifier that can appear in
`usage.provider` under the same normalized runtime registry provider key. Adding or renaming an
executor identifier requires updating the registry mapping and this model contract together.

Query parameters:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `scope` | string | no | `available` returns models currently registered by active credentials. `static` returns static model definitions. Default: `available`. Aliases: `source`, `mode`, `type`. |
| `channel` | string | no | Static-only filter for one channel. Alias: `provider`. |

Supported `scope` aliases:

| Value | Behavior |
| --- | --- |
| `available`, `active`, `current` | Return currently available runtime models. |
| `static`, `all-static`, `definitions` | Return static model definitions. |

Example available response:

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

Example static response:

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

Returns static model metadata for one channel.
Returned model entries include the same authoritative `providers` field described above.

Supported channels:

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

Path or query parameters:

| Path/query | Type | Required | Description |
| --- | --- | --- | --- |
| `channel` | string | yes | Channel name. `x-ai` and `grok` are aliases for `xai`. |

Example response:

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

Unknown channel response:

```json
{ "error": "unknown channel", "channel": "bad-channel" }
```

## Channel Groups

Channel groups restrict which auth records a client API key may use. If a client API key has an empty `channels` array, channel-group filtering is not applied.

### GET `/channel-groups`

Example response:

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

Returns one channel group:

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

Creates a channel group.

Example request:

```json
{
  "channel_name": "team-a",
  "disabled": false
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `channel_name` | string | yes | Group name. Alias: `name`. |
| `disabled` | boolean | no | Disabled state. Default: `false`. |
| `enabled` | boolean | no | Inverse alias of `disabled`. If both are present, they must agree. |

Response: `{ "channel_group": ... }`.

### PUT/PATCH `/channel-groups/:id`

Updates a channel group. The request fields are the same as `POST /channel-groups`; all fields are optional.

Response: `{ "channel_group": ... }`.

### DELETE `/channel-groups/:id`

Soft-deletes the group and its details.

Response:

```json
{ "status": "ok" }
```

### GET `/channel-group-details`

Lists channel group detail rows.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `channel_group_id` | integer | Filter by group ID. Aliases: `channel-group-id`, `group_id`, `group-id`. |
| `auth_id` | string | Filter by auth ID. Alias: `auth-id`. |

Example response:

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

Returns one detail row:

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

Creates a channel-group-to-auth binding.

Example request:

```json
{
  "channel_group_id": 1,
  "auth_id": "auth-db-id"
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `channel_group_id` | integer | yes | Existing channel group ID. |
| `auth_id` | string | yes | Auth record ID. |

Response: `{ "channel_group_detail": ... }`.

### PUT/PATCH `/channel-group-details/:id`

Updates a detail row.

Example request:

```json
{
  "channel_group_id": 2,
  "auth_id": "other-auth-id"
}
```

All fields are optional, but a supplied `channel_group_id` must be greater than `0`.

Response: `{ "channel_group_detail": ... }`.

### DELETE `/channel-group-details/:id`

Soft-deletes the detail row.

Response:

```json
{ "status": "ok" }
```

## Model Groups

Model groups restrict which model IDs a client API key may use. If a client API key has an empty `model_groups` array, model filtering is not applied.

Each model group detail may also contain `channels`, an array of channel group IDs that limits which credentials can execute that model. Empty or omitted `channels` inherits the client API key's existing credential scope and preserves legacy behavior. A non-empty value resolves the enabled credentials in those channel groups and intersects them by auth ID with the API key's own `channels` scope. If the API key does not restrict channels, the model-specific scope applies directly. If the resulting credential set is empty, dispatch fails closed rather than falling back to unrestricted credentials.

When the same model occurs in multiple model groups selected by an API key, Home unions all non-empty model-specific `channels` bindings. Duplicate details with empty `channels` do not cancel an explicit restriction. Disabled or deleted model/channel groups do not contribute eligible credentials.

### GET `/model-groups`

Example response:

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

Returns one model group:

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

Creates a model group.

Example request:

```json
{
  "group_name": "premium-models",
  "disabled": false
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `group_name` | string | yes | Group name. Alias: `name`. |
| `disabled` | boolean | no | Disabled state. Default: `false`. |
| `enabled` | boolean | no | Inverse alias of `disabled`. If both are present, they must agree. |

Response: `{ "model_group": ... }`.

### PUT/PATCH `/model-groups/:id`

Updates a model group. The request fields are the same as `POST /model-groups`; all fields are optional.

Response: `{ "model_group": ... }`.

### DELETE `/model-groups/:id`

Soft-deletes the model group and its details.

Response:

```json
{ "status": "ok" }
```

### GET `/model-group-details`

Lists model group detail rows.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `model_group_id` | integer | Filter by model group ID. Aliases: `model-group-id`, `group_id`, `group-id`. |
| `model_id` | string | Filter by canonical model ID. Alias: `model-id`. Matching is case-insensitive and ignores a trailing request-option suffix on either the query or a legacy stored record. |

Example response:

```json
{
  "model_group_details": [
    {
      "id": 10,
      "model_group_id": 1,
      "model_id": "gpt-5.5",
      "channels": [2, 3],
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z",
      "deleted_at": null
    }
  ]
}
```

### GET `/model-group-details/:id`

Returns one detail row:

```json
{
  "model_group_detail": {
    "id": 10,
    "model_group_id": 1,
    "model_id": "gpt-5.5",
    "channels": [2, 3],
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

### POST `/model-group-details`

Creates a model-group-to-model binding.

Example request:

```json
{
  "model_group_id": 1,
  "model_id": "gpt-5.5",
  "channels": [2, 3]
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `model_group_id` | integer | yes | Existing model group ID. |
| `model_id` | string | yes | Canonical model ID allowed by this group. A trailing request-option suffix such as `(high)` is stripped on write and is not part of the model identity. |
| `channels` | array of positive integer | no | Channel group IDs allowed to execute this model. ID `0` is rejected with HTTP `400`. Empty or omitted inherits the API key credential scope. |

Response: `{ "model_group_detail": ... }`.

### PUT/PATCH `/model-group-details/:id`

Updates a detail row.

Example request:

```json
{
  "model_group_id": 2,
  "model_id": "gpt-5.5-mini",
  "channels": [4]
}
```

All fields are optional, but a supplied `model_group_id` must be greater than `0`. Supplying `channels: []` removes the model-specific credential restriction and restores inherited behavior.

Response: `{ "model_group_detail": ... }`.

### DELETE `/model-group-details/:id`

Soft-deletes the detail row.

Response:

```json
{ "status": "ok" }
```

## OAuth Model Rules

### `/oauth-excluded-models`

GET response:

```json
{
  "oauth-excluded-models": {
    "claude": ["claude-opus-4.5"]
  }
}
```

PUT input:

```json
{
  "claude": ["claude-opus-4.5"]
}
```

or:

```json
{
  "items": {
    "claude": ["claude-opus-4.5"]
  }
}
```

PATCH input:

```json
{
  "provider": "claude",
  "models": ["claude-opus-4.5"]
}
```

DELETE query:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `provider` | string | yes | Provider key to remove. |

Successful writes return `{ "status": "ok" }`.

### `/oauth-model-alias`

GET response:

```json
{
  "oauth-model-alias": {
    "claude": [
      { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
    ]
  }
}
```

PUT input:

```json
{
  "claude": [
    { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
  ]
}
```

or:

```json
{
  "items": {
    "claude": [
      { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
    ]
  }
}
```

PATCH input:

```json
{
  "channel": "claude",
  "provider": "claude",
  "aliases": [
    { "name": "claude-sonnet-4", "alias": "sonnet", "fork": true, "force-mapping": true }
  ]
}
```

DELETE query:

| Query | Type | Required | Description |
| --- | --- | --- | --- |
| `channel` | string | conditionally | Alias channel to remove. |
| `provider` | string | conditionally | Alias of `channel`. |

Successful writes return `{ "status": "ok" }`.

## Config Field Reference

These fields are accepted by Home YAML config. `PUT /config.yaml` accepts non-credential roots; use provider-key and auth-file routes for credential roots.

| Field | Type | Description |
| --- | --- | --- |
| `host` | string | Service bind host/interface. |
| `port` | integer | Service listen port. |
| `allow-host` | array of string | RESP client IP allowlist. Empty list allows all hosts. |
| `tls.enable` | boolean | Enable HTTPS. |
| `tls.cert` | string | TLS certificate path. |
| `tls.key` | string | TLS private key path. |
| `trusted-proxies` | array of string | Explicit reverse-proxy IP/CIDR allowlist used for forwarded client addresses. Empty trusts none; trust-all networks are rejected; restart after changes. |
| `remote-management.allow-remote` | boolean | Allows non-localhost Management API requests when true. |
| `remote-management.secret-key` | string | Management key. In local config mode, plaintext is hashed at startup. |
| `remote-management.disable-control-panel` | boolean | Disables the embedded panel routes: `/`, `/index.html`, `/management.html`, `/user.html`, and `/assets/*`. |
| `remote-management.disable-auto-update-panel` | boolean | Legacy compatibility flag; embedded panel assets are not updated at runtime. |
| `remote-management.panel-github-repository` | string | Legacy compatibility field for the embedded panel source repository. |
| `user-email.enabled` | boolean | Enables verified-email registration and password recovery when all mail settings are valid. |
| `user-email.public-user-url` | string | Absolute public user-panel URL used in verify/reset links; production requires HTTPS. |
| `user-email.from-address` | string | SMTP envelope/header mailbox without a display name. |
| `user-email.from-name` | string | Optional safe display name for user email. |
| `user-email.sender.type` | string | Mail sender type; currently only `smtp`. |
| `user-email.sender.smtp.host` | string | SMTP host. Non-loopback hosts require STARTTLS. |
| `user-email.sender.smtp.port` | integer | SMTP port; implicit TLS port `465` is unsupported. |
| `user-email.sender.smtp.username` | string | Optional SMTP username. |
| `user-email.sender.smtp.password-env` | string | Environment variable containing the SMTP password; the secret is not stored in config. |
| `user-email.sender.smtp.starttls` | boolean | Requires STARTTLS with TLS 1.2 or newer. |
| `user-email.verification-token-ttl` | string | Positive Go duration for verification tokens. |
| `user-email.reset-token-ttl` | string | Positive Go duration for password-reset tokens. |
| `auth-dir` | string | Local auth token directory. |
| `proxy-url` | string | Global outbound proxy URL. |
| `disable-image-generation` | boolean or `"chat"` | `false` enables image generation; `true` disables it globally; `"chat"` disables injection for non-image endpoints. |
| `force-model-prefix` | boolean | Requires explicit model prefixes for prefixed credentials. |
| `request-log` | boolean | Enables detailed request logging. |
| `api-keys` | array of string | Client API keys accepted by Home. |
| `passthrough-headers` | boolean | Passes upstream response headers to downstream clients. |
| `streaming.keepalive-seconds` | integer | SSE heartbeat interval in seconds; `<=0` disables it. |
| `streaming.bootstrap-retries` | integer | Streaming retries before first byte; `<=0` disables it. |
| `nonstream-keepalive-interval` | integer | Blank-line keepalive interval for non-streaming responses. |
| `debug` | boolean | Enables debug logging/features. |
| `pprof.enable` | boolean | Enables pprof server. |
| `pprof.addr` | string | pprof listen address. |
| `commercial-mode` | boolean | Reduces high-overhead middleware behavior under high concurrency. |
| `logging-to-file` | boolean | Writes app logs to files instead of stdout. |
| `logs-max-total-size-mb` | integer | Total log file size limit in MB; `0` disables cleanup. |
| `error-logs-max-files` | integer | Retained request error log file count. |
| `plugins.enabled` | boolean | Enables trusted in-process plugins on Home and downstream CPA nodes. |
| `plugins.dir` | string | Local plugin artifact directory used by each node. |
| `plugins.store-sources` | array of string | Additional plugin store registry URLs. The built-in official registry is always included. |
| `plugins.configs` | object | Per-plugin config keyed by plugin ID. Store installs write a pinned `store` manifest under each plugin entry. Home-mode CPA nodes download store entries from that manifest; Home downloads and loads them only when `load-in-home: true` is explicitly set. |
| `usage-statistics-enabled` | boolean | Enables in-memory usage aggregation. Home forces this to `true` for downstream CPA nodes and rejects disabling it through Management API updates. |
| `redis-usage-queue-retention-seconds` | integer | Usage queue retention window. Default `60`, max `3600`. |
| `disable-cooling` | boolean | Compatibility field. Home is the sole credential scheduler and always normalizes this to `false` so central quota cooldown remains enabled. Config sent to downstream CPA nodes is independently forced to `true`, disabling only CPA-local cooldown. |
| `auth-auto-refresh-workers` | integer | Overrides auth auto-refresh worker count. |
| `request-retry` | integer | Failed request retry count. |
| `max-retry-credentials` | integer | Max credentials to try per failed request; `<=0` means all available. |
| `max-retry-interval` | integer | Max wait seconds before retrying cooled-down credentials. |
| `quota-exceeded.switch-project` | boolean | Switches Gemini project on quota errors. |
| `quota-exceeded.switch-preview-model` | boolean | Switches to preview model on quota errors. |
| `quota-exceeded.antigravity-credits` | boolean | Uses Antigravity credits as last-resort Claude fallback. |
| `routing.strategy` | string | `round-robin` or `fill-first`. |
| `routing.claude-code-session-affinity` | boolean | Deprecated Claude Code session affinity flag. |
| `routing.session-affinity` | boolean | Universal session-sticky credential routing. |
| `routing.session-affinity-ttl` | string | Session-to-auth binding duration. |
| `antigravity-signature-cache-enabled` | boolean pointer | Enables Antigravity thinking signature cache validation. |
| `antigravity-signature-bypass-strict` | boolean pointer | Controls strictness of Antigravity signature bypass. |
| `gemini-api-key` | array of `GeminiKey` | Gemini API-key credentials; use provider-key routes. |
| `codex-api-key` | array of `CodexKey` | Codex API-key credentials; use provider-key routes. |
| `xai-api-key` | array of `XAIKey` | Native xAI API-key credentials; use provider-key routes. |
| `codex-header-defaults.user-agent` | string | Default Codex User-Agent. |
| `codex-header-defaults.beta-features` | string | Default Codex websocket beta features header. |
| `claude-api-key` | array of `ClaudeKey` | Claude API-key credentials; use provider-key routes. |
| `claude-header-defaults.user-agent` | string | Default Claude User-Agent. |
| `claude-header-defaults.package-version` | string | Default Claude package version. |
| `claude-header-defaults.runtime-version` | string | Default Claude runtime version. |
| `claude-header-defaults.os` | string | Default Claude OS fingerprint. |
| `claude-header-defaults.arch` | string | Default Claude architecture fingerprint. |
| `claude-header-defaults.timeout` | string | Default Claude timeout header. |
| `claude-header-defaults.stabilize-device-profile` | boolean pointer | Enables stable Claude device profile baseline. |
| `openai-compatibility` | array of `OpenAICompatibility` | OpenAI-compatible providers; use provider-key routes. |
| `vertex-api-key` | array of `VertexCompatKey` | Vertex-compatible API-key credentials; use provider-key routes. |
| `oauth-excluded-models` | object string to array of string | Per-provider OAuth/file-backed auth excluded models. |
| `oauth-model-alias` | object string to array of `OAuthModelAlias` | Per-channel OAuth model aliases. |
| `oauth-model-alias.*[].force-mapping` | boolean | When true, response model fields use the mapped upstream model name. |
| `payload.default` | array of `PayloadRule` | Sets missing JSON payload params. |
| `payload.default-raw` | array of `PayloadRule` | Sets missing raw JSON payload params. |
| `payload.override` | array of `PayloadRule` | Overrides JSON payload params. |
| `payload.override-raw` | array of `PayloadRule` | Overrides raw JSON payload params. |
| `payload.filter` | array of `PayloadFilterRule` | Removes JSON payload paths. |

Payload nested structure:

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

`PayloadModelRule` fields:

| Field | Type | Description |
| --- | --- | --- |
| `name` | string | Target model name or wildcard pattern. |
| `protocol` | string | Current translator protocol/provider format matcher, such as `openai`, `responses`, `gemini`, `claude`, `codex`, or `antigravity`. |
| `from-protocol` | string | Source protocol matcher used when a request was translated from another protocol. |
| `headers` | object string to string | Request header matchers. Every configured header must be present and its value must match the configured wildcard pattern. |
| `match` | array of object | Payload JSON path/value conditions that must match. Paths use the same gjson/sjson-style path syntax as payload params. |
| `not-match` | array of object | Payload JSON path/value conditions that must not match. |
| `exist` | array of string | Payload JSON paths that must exist and not be `null`. |
| `not-exist` | array of string | Payload JSON paths that must be missing or `null`. |
