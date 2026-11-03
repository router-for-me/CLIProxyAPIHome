# Home

English | [中文](README_CN.md)

Home is a comprehensive credential **management, scheduling, and distribution** service designed for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

It serves as a centralized hub for managing and scheduling API credentials in large-scale CLIProxyAPI cluster deployments. CLIProxyAPI nodes communicate with Home to retrieve necessary API credentials and report usage statistics.

## Home and Home Management Center (HMC)

Home provides the backend service, and [Home Management Center (HMC)](https://github.com/router-for-me/Home-Management-Center) provides a visual Web interface for Home. **The two open-source projects are maintained separately. Official Home releases embed HMC so they can be deployed and updated together.**

### Deploying an official release

Official Home binaries and Docker images include the Web interface, so HMC does not need to be deployed separately. After starting Home, open its address, such as `http://localhost:8327`. The administrator console is at `/management.html`, and the user workspace is at `/user.html`.

Administrator access requires a management key; see the [authentication instructions](docs/management/api.md#authentication). Database setup and configuration import are covered under [Cluster Mode](#cluster-mode). Update the Home binary or Docker image to update the bundled Web interface.

### Developing both projects locally

For frontend development, clone both repositories into sibling directories and run Home and HMC in separate terminals. Environment requirements are listed in Home's [go.mod](go.mod) and the [HMC README](https://github.com/router-for-me/Home-Management-Center#readme).

For a new local setup without `cluster.yaml`, copy Home's `config.example.yaml` to `config.yaml` and set a non-empty `management.secret-key`. Import that configuration into the local SQLite database, then start Home in one terminal:

```bash
cd CLIProxyAPIHome
go run ./cmd/home -config config.yaml -import
go run ./cmd/home -addr 127.0.0.1:8327
```

An existing configured Home backend can be used directly.

Start HMC in another terminal:

```bash
cd Home-Management-Center
bun install --frozen-lockfile
bun run dev
```

Open `http://localhost:3000/management.html`, enter `http://127.0.0.1:8327` as the management endpoint, and sign in with the management key. Frontend changes can be previewed without rebuilding Home.

### Building and releasing the bundled frontend

To build Home with the Web interface from source, first build HMC, then copy the complete output into Home before compiling. Starting from the parent of the two sibling repositories:

```bash
cd Home-Management-Center
bun install --frozen-lockfile
bun run build:embedded
cp -R dist/. ../CLIProxyAPIHome/internal/managementasset/static/
cd ../CLIProxyAPIHome
go build -o CLIProxyAPIHome ./cmd/home
```

Home's source checkout does not include the generated frontend. Neither `go build` nor `docker build` builds HMC automatically, so prepare the complete frontend output before building either a binary or a Docker image.

Official releases automate this process: a version tag triggers the frontend build, followed by Home packaging and publication of binaries or Docker images. See the [binary release workflow](.github/workflows/release.yaml) and [Docker workflow](.github/workflows/docker-image.yml) for details.

## Key Features

- **Scalable Node Management**: Supports connecting an unlimited number of CLIProxyAPI nodes for credential distribution, ideal for large-scale deployments.
- **Multi-Provider OAuth2 Support**: Manages OAuth2 credentials for various AI model providers, including OpenAI, Claude, Codex, Antigravity, Kimi, xAI, and more.
- **High Availability**: Supports multi-account management and round-robin load balancing to ensure optimal performance and reliability.
- **Secure Access**: Provides a streamlined CLI authentication flow to ensure secure access to APIs.
- **Plugin Support**: Supports installing and enabling CLIProxyAPI-compatible plugins through Home Management.
- **Flexible Upstream Integration**: Easily integrate upstream providers via configuration, including:
  - OpenAI-compatible providers (e.g., OpenRouter)
  - OpenAI Responses protocol providers
  - Anthropic Messages protocol providers
  - Google Gemini protocol providers

## Cluster Mode

Home now always runs from a database-backed runtime. When `cluster.yaml` is absent, Home runs the same scheduler, node discovery, leader election, event watcher, management, and RESP logic on a local SQLite database. The default SQLite path is `home.db`; use `-sqlite-path` to override it.

When `cluster.yaml` exists, Home enables cluster mode and uses the database backend declared in that file. Copy `cluster.sqlite.example.yaml` or `cluster.pgsql.example.yaml` to `cluster.yaml` as a starting point. PostgreSQL accepts either individual connection fields or a connection string in `pgsql.postgres_uri`. SQLite supports only one active Home process per database; deployments with multiple active Homes must use PostgreSQL. Multiple CPA nodes may connect to a single SQLite-backed Home. When using SQLite with `cluster.yaml`, configure `node.external-ip` so CPA clients can reach the Home.

Before using automatic CPA connection rebalancing, upgrade all Home nodes to a version that supports it. Automatic rebalancing is not guaranteed while old and new Home versions coexist.

Set `slow-query-threshold` under the selected `pgsql` or `sqlite` backend to control slow SQL logging. It accepts Go duration strings such as `"500ms"` or `"1s"` and defaults to `"200ms"`.

PostgreSQL pool limits are configured in `cluster.yaml`: `pgsql.max-open-conns` defaults to `0` (unlimited), and `pgsql.max-idle-conns` defaults to `2` (capped by a positive total limit). Explicit `max-idle-conns: 0` disables idle connection retention; an explicit idle limit must not exceed a positive total limit. Limits apply per Home process and require a restart.

`config.yaml` and `auth-dir` are no longer runtime storage. They are import/export exchange formats only. To migrate an existing local setup into the database, run:

```bash
./CLIProxyAPIHome -import
```

By default, import reads `./config.yaml` and then resolves credentials from the `auth-dir` value inside that config. Use `-config <path>` to select another config file, `-auth-dir <path>` to override credential discovery, and `-sqlite-path <path>` when importing into a non-default SQLite database. Import is idempotent, so rerunning it with the same files does not duplicate records.

To export config and credentials back to local files, run:

```bash
./CLIProxyAPIHome -export
```

Export writes `./config.yaml` and credential files under `~/.cli-proxy-api/` by default. Use `-export-dir <path>` to write `<path>/config.yaml` and credential files under `<path>/auths/`. It refuses to overwrite an existing `config.yaml` or a non-empty credential directory at the selected target.

`-export` / `-import` preserve the legacy exchange format. API keys are exported only as their actual key values, so Home database-only metadata such as API key display names, user ownership, and channel/model-group bindings is not a complete backup. Use `-db-export <snapshot.zip>` and `-db-import <snapshot.zip>` for full database backup and restore. Database snapshot format v4 includes API key display names and cannot be imported by older Home versions. Before upgrading, retain a database backup or an older-version-compatible snapshot for rollback.

In cluster mode, the Management API operates directly on database data. The default listen port comes from `node.port` in `cluster.yaml`; the startup `-addr` flag can override the listen address. Set `node.external-port` when a reverse proxy changes the port that clients must use; when omitted, the advertised cluster node port follows the final listen port.

RESP password authentication has been removed. Home RESP access only accepts mTLS-authenticated clients; CPA should connect with `-home-jwt` or configured TLS material. `allow-host` is only an IP allowlist and is not a password mechanism.

Docker Compose uses `docker-compose.sqlite.yml` or `docker-compose.pgsql.yml`. Copy the matching `cluster.sqlite.example.yaml` or `cluster.pgsql.example.yaml` to `cluster.yaml`, then use `/CLIProxyAPIHome/data/home.db` for SQLite or `postgres` as the PostgreSQL host. Select either database type when importing existing local state:

```bash
COMPOSE_FILE=docker-compose.sqlite.yml
# PostgreSQL: COMPOSE_FILE=docker-compose.pgsql.yml
docker compose -f "$COMPOSE_FILE" run --rm \
  -v "$PWD/config.yaml:/CLIProxyAPIHome/config.yaml:ro" \
  -v "$PWD/auths:/root/.cli-proxy-api" \
  home ./CLIProxyAPIHome -import
```

## Strict Credential Concurrency Limits

See the [Management API documentation](docs/management/api.md#credential-concurrency-policy-and-limiter-routes) for credential concurrency policy configuration and activation prerequisites.

## Contributing

Issues, feature requests, and pull requests are welcome. Choose the repository that maintains the affected component:

- **Frontend / Web UI**: Report page display and interaction issues in [HMC Issues](https://github.com/router-for-me/Home-Management-Center/issues), and submit frontend changes to HMC.
- **Home backend**: Report service, scheduling, and data management issues in [CLIProxyAPIHome Issues](https://github.com/router-for-me/CLIProxyAPIHome/issues), and submit backend changes here.

For changes spanning both projects, link the related issues or pull requests. Update the relevant [Management API](docs/management/api.md) or [User API](docs/user/api.md) documentation when an interface changes.

1. Fork the repository.
2. Create your feature branch (`git checkout -b feature/amazing-feature`).
3. Commit your changes (`git commit -m 'Add some amazing feature'`).
4. Push to the branch (`git push origin feature/amazing-feature`).
5. Open a Pull Request.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
