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
| `DELETE` | `/api-keys` |
| `GET` | `/api-keys` |
| `PATCH` | `/api-keys` |
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
| `GET` | `/usage-queue` |
| `GET` | `/usage-statistics-enabled` |
| `PATCH` | `/usage-statistics-enabled` |
| `PUT` | `/usage-statistics-enabled` |
| `GET` | `/users` |
| `POST` | `/users` |
| `DELETE` | `/users/:id` |
| `GET` | `/users/:id` |
| `PATCH` | `/users/:id` |
| `PUT` | `/users/:id` |
| `DELETE` | `/vertex-api-key` |
| `GET` | `/vertex-api-key` |
| `PATCH` | `/vertex-api-key` |
| `PUT` | `/vertex-api-key` |
| `POST` | `/vertex/import` |
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
claude-api-key
openai-compatibility
```

`auth-dir` is still treated as an import/export path and is not persisted into the runtime config snapshot.

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
        "models": [{ "name": "gpt-*", "protocol": "responses" }],
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

`PUT /payload` and `PATCH /payload` accept either a raw payload object, `{ "value": <payload> }`, or `{ "payload": <payload> }`.

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
      "client_count": 1,
      "healthy": true,
      "home_ip": "10.0.0.10",
      "home_port": 8327,
      "last_seen_at": "2026-05-27T10:30:05Z",
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
| `nodes` | array | Active CPA node list aggregated from connection snapshots written by all live Home nodes into the shared database. |
| `plugin_report_required` | boolean | Whether the current Home config expects CPA plugin reports because at least one enabled plugin has a pinned store manifest. |
| `plugin_report_statuses` | array | Latest plugin reports stored in the shared database, grouped by reporting node and report metadata. Delete reports for one plugin can coexist with preserved status rows for other plugins. These are retained until the node reports again or is explicitly cleaned up; they do not expire by TTL and are self-reported observations, not authoritative install proof. |
| `nodes[].node_id` | string | CPA node ID derived from the Home client certificate when available. |
| `nodes[].ip` | string | Node IP address. |
| `nodes[].connected_time` | string | First connection time for the active node entry. |
| `nodes[].client_count` | integer | Active RESP subscription connection count from this IP. |
| `nodes[].healthy` | boolean | Whether the node has an active RESP subscription connection. Plugin reports do not make this field unhealthy. |
| `nodes[].home_ip` | string | Home node address currently serving this CPA node. |
| `nodes[].home_port` | integer | Home node port currently serving this CPA node. |
| `nodes[].last_seen_at` | string | Time when the serving Home node last refreshed this CPA connection snapshot in the shared database. |
| `nodes[].plugin_report_state` | string | Current configured plugin observation state: `not_required`, `missing_report`, `reported_partial`, `reported_failed`, or `reported_ok`. Failed reports for plugins that are not currently required do not make this state failed. |
| `nodes[].plugin_report_statuses` | array | Plugin reports associated with this active node, matched by node ID when possible and IP as a fallback. |
| `plugin_report_statuses[].node_type` | string | Reporting node type, currently `cpa` for CPA node reports and reserved for `home` reports. |
| `plugin_report_statuses[].node_id` | string | CPA node ID derived from its Home client certificate. |
| `plugin_report_statuses[].status` | string | Reported plugin task status for this report group, currently `success` or `failed`. |
| `plugin_report_statuses[].phase` | string | Reported task phase for this report group, such as `install`, `load`, or `delete`. |
| `plugin_report_statuses[].ok` | boolean | Whether the node reported the task as successful. |
| `plugin_report_statuses[].plugins` | array | Per-plugin install/load/delete results belonging to this report group. |

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
      "auth_configured": false,
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
| `plugins[].auth_configured` | boolean | True when `plugins.store-auth` has a matching rule whose referenced environment variables are present. |
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
    "mfa": { "enabled": true },
    "passkey": [{ "id": "credential-id" }],
    "created_at": "2026-05-27T10:00:00Z",
    "updated_at": "2026-05-27T10:00:00Z",
    "deleted_at": null
  }
}
```

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
| `mfa` | any valid JSON | no | Stored in `user.mfa`. |
| `passkey` | any valid JSON | no | Stored in `user.passkey`. |

Response: same shape as `GET /users/:id`.

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

Response: same shape as `GET /users/:id`.

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
| `APIKeyEntry.api-key` | string | Client API key. |
| `APIKeyEntry.api_key` | string | Alias of `api-key`. |
| `APIKeyEntry.user-id` | integer or null | Bound `user.id`; `null` means unbound. |
| `APIKeyEntry.user_id` | integer or null | Alias of `user-id`. |
| `APIKeyEntry.channels` | array of integer | Bound channel group IDs. An empty array is non-restrictive. |
| `APIKeyEntry.model_groups` | array of integer | Bound model group IDs. An empty array is non-restrictive. |

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

Successful response:

```json
{ "status": "ok" }
```

### PATCH `/api-keys`

Updates one client API key by index or by old value. When `old/new` is used and the old value does not exist, `new` is appended. This route can also update `user_id`, `channels`, and `model_groups` for an existing API key.

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
| `index` | integer | conditionally | Zero-based index. |
| `value` | string or `APIKeyEntry` | conditionally | New value paired with `index`. Structured entries are accepted. |
| `old` | string | conditionally | Old key to find. |
| `new` | string | conditionally | New key; appended when `old` is not found. |
| `api_key` | string | conditionally | Direct-binding target. Aliases: `api-key`, `key`. |
| `user_id` | integer | no | Bound `user.id`. Alias: `user-id`; `0` clears the binding. |
| `channels` | array of integer | no | Channel group IDs. |
| `model_groups` | array of integer | no | Model group IDs. Alias: `model-groups`. |

Normal response:

```json
{ "status": "ok" }
```

Direct binding update response:

```json
{
  "api_key": {
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

Deletes a client API key by index or value.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `index` | integer | Delete the key at this zero-based index. |
| `value` | string | Delete the key whose trimmed value matches. |
| `api_key` | string | Alias of `value`. |
| `api-key` | string | Alias of `value`. |
| `key` | string | Alias of `value`. |

Example response:

```json
{ "status": "ok" }
```

## Billing

All paths in this section are relative to the Management API base URL, for example `/v0/management/billing/overview` or `/v0/management/proxy/proxy-pools`. They are not `/user` routes and require the management key.

Only `/billing/overview`, `/billing/charges`, and `/billing/balance-records` parse `from` and `to` as `YYYY-MM-DD`, RFC3339, or Unix seconds. A date-only `to` value includes the whole ending UTC day. Pagination with `limit` and `offset` applies only to `/billing/charges` and `/billing/balance-records`; those routes use `limit` default `50`, max `200`, and normalize negative `offset` values to `0`. `/billing/model-prices` supports only `provider`, `model`, and `enabled` query parameters. `/proxy/proxy-pools` currently does not parse query parameters.

### GET `/billing/overview`

Returns an administrator billing summary.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional end time. Date-only values include the full UTC day. |
| `user` | string | Optional username, user text, or user ID filter. Aliases: `user_text`, `username`. |
| `user_id` | integer | Optional exact user ID filter. Alias: `uid`. |
| `provider` | string | Optional provider filter. |
| `model` | string | Optional model filter. |

Response fields:

| Field | Type | Description |
| --- | --- | --- |
| `range` | object | Applied `from` and `to` range. |
| `total_charge_amount` | number | Total charged amount. |
| `total_recharge_amount` | number | Total recharge amount. |
| `total_deduct_amount` | number | Total manual deduction amount. |
| `total_balance` | number | Current total user balance. |
| `request_count` | integer | Number of charged requests. |
| `input_tokens` | integer | Total input tokens. |
| `output_tokens` | integer | Total output tokens. |
| `cache_tokens` | integer | Total cache tokens. |
| `active_user_count` | integer | Number of users with charges in the range. |
| `daily_trend[]` | array | Daily charge amount and request count. |
| `top_users[]` | array | Top users with `id`, `label`, `amount`, and `request_count`. |
| `top_models[]` | array | Top models with `id`, `label`, `amount`, and `request_count`. |
| `top_providers[]` | array | Top providers with `id`, `label`, `amount`, and `request_count`. |

### GET `/billing/charges`

Lists billing charge records with administrator context. Responses expose user ID, masked API-key metadata, price snapshot, matched price rule, request ID, endpoint, and `balance_before`/`balance_after`. Billing charge responses never expose raw API keys.

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional end time. Date-only values include the full UTC day. |
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
      "matched_price_rule": "price_xxx",
      "price_snapshot": { "request_price": 0, "input_price_per_million": 1.25 }
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
| `to` | string | Optional end time. Date-only values include the full UTC day. |
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

### POST `/billing/model-prices`

Creates a model price rule. Omitted price values default to `0`, and `enabled` defaults to `true`.

Request body fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `provider` | string | yes | Provider name. |
| `model` | string | yes | Model name. |
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

GET    /vertex-api-key
PUT    /vertex-api-key
PATCH  /vertex-api-key
DELETE /vertex-api-key

GET    /openai-compatibility
PUT    /openai-compatibility
PATCH  /openai-compatibility
DELETE /openai-compatibility
```

Home synthesizes DB auth records from these config-like payloads.

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
| `auth_index`, `id`, `uuid` | string | DB auth identifier aliases. |
| `disabled` | boolean | DB auth disabled flag. |

`ClaudeKey`, `CodexKey`, and `VertexCompatKey` use the same common fields. Additional notable fields:

| Field | Applies to | Description |
| --- | --- | --- |
| `cloak` | Claude | Optional request cloaking config. |
| `experimental-cch-signing` | Claude | Enables experimental CCH signing for cloaked Claude requests. |
| `websockets` | Codex | Enables Responses API websocket transport. |
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
| `auth-index` | string | Compatibility credential identifier when no per-key entries exist. |
| `auth_index`, `id`, `uuid` | string | DB auth identifier aliases. |

Shared nested structures:

```json
{
  "ModelAlias": {
    "name": "upstream-model",
    "alias": "client-visible-model"
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
```

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

### GET `/capabilities`

Returns the frontend capability flags and build metadata exposed by the current Home Management API. The management panel uses this endpoint to decide whether to enable usage overview, request records, aggregate rankings, export, realtime diagnostics, health attribution, request events, and request log indexes.

Response fields:

| Field | Type | Description |
| --- | --- | --- |
| `capabilities.usage` | boolean | Whether the legacy `GET /api-key-usage` capability is available. |
| `capabilities.usage_overview` | boolean | Whether `GET /usage/overview` is available. |
| `capabilities.usage_records` | boolean | Whether `GET /usage/records` is available. |
| `capabilities.usage_record_details` | boolean | Whether `GET /usage/records/:id` is available. |
| `capabilities.usage_aggregates` | boolean | Whether `GET /usage/aggregates` is available. |
| `capabilities.usage_export` | boolean | Whether `GET /usage/export` is available. |
| `capabilities.usage_provider_health` | boolean | Whether `GET /usage/health/providers` is available. |
| `capabilities.usage_credential_health` | boolean | Whether `GET /usage/health/credentials` is available. |
| `capabilities.usage_realtime` | boolean | Whether `GET /usage/realtime` is available. |
| `capabilities.request_log_index` | boolean | Whether `GET /request-logs` is available. |
| `capabilities.request_events` / `capabilities.requestEvents` | boolean | Whether `GET /request-events` is available. |
| `capabilities.request_event_details` / `capabilities.requestEventDetails` | boolean | Whether `GET /request-events/:id` is available. |
| `capabilities.request_event_export` / `capabilities.requestEventExport` | boolean | Whether `GET /request-events/export` is available. |
| `capabilities.request_event_filters` / `capabilities.requestEventFilters` | boolean | Whether `GET /request-events/filter-options` is available. |
| `capabilities.oauth_usage` | boolean | Whether OAuth/file-backed credential usage attribution is reliable. |
| `capabilities.logs` | boolean | Whether application log APIs are available. |
| `capabilities.request_error_logs` | boolean | Whether request error log file list/download APIs are available. |
| `server_info.home_version` | string | Home build version. |
| `server_info.home_commit` | string | Home build commit. |
| `server_info.home_build_date` | string | Home build time. |

### Usage Observability Conventions

These endpoints read persisted `usage`, `billing_charge`, `api_key`, `user`, and `auth` data. Responses never return raw client access keys, provider API keys, OAuth tokens, cookies, authorization headers, complete payloads, or complete failure bodies. They may return `api_key_masked`, redacted `body_preview`, and payload summaries.

The aggregate range parameters apply to `/usage/overview`, `/usage/aggregates`, `/usage/realtime`, `/usage/health/providers`, and `/usage/health/credentials`, and they also act as the base range for `/usage/records` and `/usage/export`. `/usage/overview` and `/usage/aggregates` automatically fill a recent 24-hour window when `from` or `to` is missing; `/usage/records` and `/usage/export` do not fill a time range automatically.

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `from` | string | `to - 24h` for `/usage/overview` and `/usage/aggregates`; none for other endpoints | Start time. Supports `YYYY-MM-DD`, RFC3339, or Unix seconds. Date-only values are interpreted as 00:00:00 in `timezone`. |
| `to` | string | current time for `/usage/overview` and `/usage/aggregates`; none for other endpoints | End time. Supports `YYYY-MM-DD`, RFC3339, or Unix seconds. Date-only values include the full day in `timezone`. |
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
| `client.client_ip` | string/null | Caller IP associated with the usage payload. This is not treated as the CPA node IP. |
| `credential.api_key_preview` | string/null | Redacted provider API key preview when available; raw keys are never returned. |
| `billing.balance_before` / `billing.balance_after` | number/null | Balance before and after the charge when linked to `billing_charge`. |
| `runtime.home_ip` / `runtime.home_port` | string/integer/null | Home node identity that persisted the usage record. |
| `runtime.cpa_node_id` / `runtime.cpa_ip` / `runtime.cpa_port` / `runtime.cpa_label` | mixed | CPA ownership fields. Home fills missing CPA node ID/IP from the trusted RESP/mTLS runtime identity when the CPA payload does not report them. |
| `runtime.request_log_available` | boolean | Whether the current Home node can find a downloadable local request log file. |
| `runtime.log_home_ip_required` | boolean | Whether request log download requires the Home IP. |

### GET `/usage/overview`

Returns a usage overview with the applied range, short-window live snapshot, totals, trends, default top groups, and activity buckets.

Query parameters in addition to aggregate range parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `interval` | string | `auto` | `minute`, `hour`, `day`, `week`, or `auto`. `day` and `week` buckets use `timezone`; response timestamps remain UTC RFC3339. |

Top-level response fields:

| Field | Type | Description |
| --- | --- | --- |
| `range` | object | Applied time range, timezone, and interval. |
| `live` | object | Recent short-window RPM, TPM, error rate, and latency. |
| `totals` | object | Request counts, success/failure counts, tokens, amount, latency, and active subject counts. |
| `trend` | array | Trend buckets aggregated by `interval`. |
| `cost_breakdown` | array | Empty when reliable cost splitting is unavailable; the API does not fabricate indivisible cost details. |
| `model_efficiency` | array | Model efficiency list sorted by total tokens. |
| `top` | object | `users`, `client_keys`, `credentials`, `providers`, `models`, `endpoints`, and `errors`. |
| `activity` | array | Health activity series aligned with the trend buckets. |

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

The response contains `items`, `total`, `limit`, `offset`, `sort`, and `sortable_fields`. `items[]` is a redacted request summary with `tokens`, `performance`, `client`, `credential`, `billing`, `runtime`, and optional `error`.

### GET `/usage/records/:id`

Returns one usage detail. `id` is the usage ID.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `include_payload` | boolean | `false` | Return a redacted payload summary. |
| `include_logs` | boolean | `false` | Return up to 20 redacted log lines when a local request log is found. Remote nodes or missing files return an empty array. |

The response contains `record`, `payload_summary`, `log_excerpt`, and `related`. `payload_summary` only contains `method`, `stream`, `message_count`, and `tool_count`; raw payloads are never returned.

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

The response contains `group_by`, `metric`, `direction`, `items`, `total`, `limit`, `offset`, and `sortable_metrics`.

### GET `/usage/export`

Exports redacted request records for the current records filters.

Query parameters:

| Query | Type | Default | Description |
| --- | --- | --- | --- |
| `format` | string | `csv` | `csv` or `jsonl`. |
| records filters | mixed | none | Same as `GET /usage/records`. When `limit` is omitted, the endpoint exports at most `10000` rows by default; explicit `limit` values are also capped at `10000`. |

Responses are attachments. CSV uses `text/csv; charset=utf-8`; JSONL uses `application/x-ndjson`.

Export fields are flattened redacted summaries. In addition to core record response fields, they include `error_status_code`, `error_message`, `error_body_preview`, `request_log_available`, and `log_home_ip_required`.

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
| `related.request_log` | object | Request log link metadata. `available=true` and `download_url` are returned only when the current Home can find the local log file. |

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

The response contains `event`, `payload_summary`, and `log_excerpt`. `event` has the same shape as list items. `payload_summary.body_preview` is always `null` to avoid exposing request bodies.

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

Returns the request log index. The index is generated from usage records and checks the current Home node for matching local request log files.

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

`items[]` contains `id`, `request_id`, `timestamp`, `home_ip`, `file_name`, `size_bytes`, `available`, `provider`, `model`, `status`, and `download_url`. `available=true` and a non-empty `download_url` are returned only when the current node can find the local file. Missing files or files owned by remote nodes return `available=false`. Actual downloads still use the existing `GET /request-log-by-id/:id`.

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

Deletes rotated log files and truncates the active log.

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

Downloads a Home request log file from that Home's local `logs` directory. `home_ip` identifies which Home owns the file; when it is not the current Home, the current Home forwards the request to the target Home over an internal mTLS-only cluster route. Files are matched by request ID, and the file system remains the source of truth, so deleted files return `404`.

Path parameters:

| Path | Type | Description |
| --- | --- | --- |
| `id` | string | Request ID; slashes are rejected. |

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `home_ip` | string | Required Home node IP that owns the request log. |

Response: file attachment.

## Models

### GET `/models?scope=available|static`

Returns model definitions from either the current runtime registry or the static model catalog.

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
        "display_name": "GPT-5.5"
      }
    ]
  }
}
```

### GET `/model-definitions/:channel`

Returns static model metadata for one channel.

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
| `model_id` | string | Filter by model ID. Alias: `model-id`. |

Example response:

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

Returns one detail row:

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

Creates a model-group-to-model binding.

Example request:

```json
{
  "model_group_id": 1,
  "model_id": "gpt-5.5"
}
```

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `model_group_id` | integer | yes | Existing model group ID. |
| `model_id` | string | yes | Model ID allowed by this group. |

Response: `{ "model_group_detail": ... }`.

### PUT/PATCH `/model-group-details/:id`

Updates a detail row.

Example request:

```json
{
  "model_group_id": 2,
  "model_id": "gpt-5.5-mini"
}
```

All fields are optional, but a supplied `model_group_id` must be greater than `0`.

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
| `remote-management.allow-remote` | boolean | Allows non-localhost Management API requests when true. |
| `remote-management.secret-key` | string | Management key. In local config mode, plaintext is hashed at startup. |
| `remote-management.disable-control-panel` | boolean | Disables the embedded panel routes: `/`, `/index.html`, `/management.html`, `/user.html`, and `/assets/*`. |
| `remote-management.disable-auto-update-panel` | boolean | Legacy compatibility flag; embedded panel assets are not updated at runtime. |
| `remote-management.panel-github-repository` | string | Legacy compatibility field for the embedded panel source repository. |
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
| `plugins.store-auth` | array | Optional auth rules for plugin store `registry`, `metadata`, and `artifact` requests. Rules reference environment variable names only; token values are never stored in manifests. |
| `plugins.configs` | object | Per-plugin config keyed by plugin ID. Store installs write a pinned `store` manifest under each plugin entry. Home-mode CPA nodes download store entries from that manifest; Home downloads and loads them only when `load-in-home: true` is explicitly set. |
| `usage-statistics-enabled` | boolean | Enables in-memory usage aggregation. Home forces this to `true` for downstream CPA nodes and rejects disabling it through Management API updates. |
| `redis-usage-queue-retention-seconds` | integer | Usage queue retention window. Default `60`, max `3600`. |
| `disable-cooling` | boolean | Globally disables quota cooldown scheduling. Home forces this to `true` for downstream CPA nodes. |
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
    "protocol": "translator protocol"
  }
}
```
