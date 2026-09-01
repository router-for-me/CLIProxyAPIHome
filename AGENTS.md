# AGENTS.md

CLIProxyAPIHome is the database-backed Home control plane for CLIProxyAPI (CPA). It manages credentials, scheduling, clustering, RESP distribution, usage ingestion, request-log ingestion, and Management API access for CPA nodes.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPIHome
- Related CPA repository: https://github.com/router-for-me/CLIProxyAPI

## Commands
```bash
gofmt -w . # Format Go code after Go changes
go build -o CLIProxyAPIHome ./cmd/home # Build
go run ./cmd/home # Run dev server
go test ./... # Run all tests only when explicitly needed
go test -v -run TestName ./path/to/pkg # Run a focused test only when explicitly needed
go build -o test-output ./cmd/home && rm test-output # Verify compile after Go changes
```
- Common flags: `-config <path>`, `-addr <host:port>`, `-sqlite-path <path>`, `-import`, `-export`, `-export-dir <path>`, `-auth-dir <path>`.
- Normal verification after Go edits is the `test-output` compile command above. Do not leave `test-output` in the worktree.

## Config And Runtime
- Default config file: `config.yaml` (template: `config.example.yaml`).
- Home runtime state is database-backed. Without `cluster.yaml`, Home uses local SQLite (`home.db` by default, or `-sqlite-path`). With `cluster.yaml`, Home uses the configured cluster database; PostgreSQL is preferred for multi-node deployments.
- `config.yaml` and `auth-dir` are import/export exchange formats only. Use `-import` to import local files into the database and `-export` to write database state back to files.
- RESP password authentication has been removed. Home RESP access is mTLS-only; `allow-host` is only an IP allowlist.
- The Management API is controlled by `remote-management.secret-key`; an empty key disables Management routes.

## Architecture
- `cmd/home/` - Home command entrypoint, flags, runtime startup, cluster wiring, and graceful shutdown.
- `internal/home/` - Runtime scheduling, credential selection, config hot reload, usage queue, request handling, and CPA-facing behavior.
- `internal/cluster/` - Database-backed state, migrations, repository access, coordinator, certificates, import/export, usage storage, users, API keys, channel groups, and model groups.
- `internal/cluster/management/` - DB-backed Management API handlers.
- `internal/managementhttp/` - Management HTTP server construction, route registration, metadata proxy storage, and control-panel integration.
- `internal/respserver/` - RESP protocol handlers for config/model reads, token/config writes, dispatch pops, usage pushes, request-log pushes, and subscriptions.
- `internal/protocolmux/` - Shared listener protocol detection for HTTP and RESP traffic.
- `internal/config/` - Config schema and SDK config conversion.
- `internal/cliproxy/auth/` - Credential management, scheduling, refresh, filtering, and dispatch selection helpers.
- `internal/auth/` - Provider OAuth/token helpers.
- `internal/registry/` - Model registry and model definition updates.
- `internal/watcher/` - Config synthesis, file watching, and config-diff helpers.
- `internal/access/`, `internal/node/`, `internal/logging/` - Access providers, node registry helpers, and logging/request ID support.
- `docs/management/` - Home Management API documentation.

## CPA Contract
- Home is the center scheduler/aggregator for CPA nodes.
- CPA nodes communicate with Home over RESP/mTLS to retrieve config, obtain dispatchable credentials, report usage, and push request logs.
- When changing a CPA/Home payload shape, verify both repositories so older payloads remain backward compatible unless the task explicitly requires a breaking change.
- Keep Home-specific implementation DB-backed. Do not reintroduce file-backed runtime writes for Management API changes.

## Documentation
- When Management API behavior, routes, request fields, or response fields change, update `docs/management/api.md` and the matching language-specific document when applicable.
- Derive Management API docs from current Home route registration and handlers, not from stale narrative docs.
- New Markdown documents should be English unless the file is explicitly language-specific.

## Code Conventions
- Keep changes small and simple (KISS).
- Comments in code must be English. If editing code that already contains non-English comments, translate those comments to English.
- For user-visible strings, keep the existing language used in that file or feature area.
- Follow `gofmt`; keep imports goimports-style.
- Do not use `log.Fatal` or `log.Fatalf`; return errors and log through logrus instead.
- Shadowed variables must use a method-name suffix, for example `errStart := server.Start()`.
- For deferred calls that return errors, wrap them in a closure and log the returned error.
- Use logrus structured logging where helpful, and never leak secrets, tokens, API keys, or certificate material in logs.
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes.
- Keep network timeout behavior consistent with the existing runtime. Do not add broad upstream timeouts after an upstream connection is established unless the existing code path already has an intentional exception.
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
