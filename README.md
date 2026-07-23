# Home

English | [中文](README_CN.md)

Home is a comprehensive credential **management, scheduling, and distribution** service designed for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

It serves as a centralized hub for managing and scheduling API credentials in large-scale CLIProxyAPI cluster deployments. CLIProxyAPI nodes communicate with Home to retrieve necessary API credentials and report usage statistics.

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

When `cluster.yaml` exists, Home enables cluster mode and uses the database backend declared in that file. Copy `cluster.sqlite.example.yaml` or `cluster.pgsql.example.yaml` to `cluster.yaml` as a starting point. PostgreSQL is recommended for multi-node deployments. SQLite is also supported, but cluster SQLite requires `node.external-ip` so other nodes and CPA clients can reach this node.

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

Before enabling credential concurrency policies, follow the [strict credential concurrency limits deployment runbook](docs/README.md). It defines the required SQLite and PostgreSQL topologies, certificate identity requirements, capability checks, and fail-closed mixed-version rollout.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository.
2. Create your feature branch (`git checkout -b feature/amazing-feature`).
3. Commit your changes (`git commit -m 'Add some amazing feature'`).
4. Push to the branch (`git push origin feature/amazing-feature`).
5. Open a Pull Request.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
