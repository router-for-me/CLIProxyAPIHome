# Home

[English](README.md) | 中文

Home 是一个为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 设计的全面凭证**管理、调度和分发**服务。

它作为在大规模 CLIProxyAPI 集群部署中管理和调度 API 凭证的中心枢纽。CLIProxyAPI 节点与 Home 通信以获取必要的 API 凭证并上报使用统计数据。

## 核心特性

- **可扩展的节点管理**：支持连接无限数量的 CLIProxyAPI 节点进行凭证分发，非常适合大规模部署。
- **多提供商 OAuth2 支持**：管理包括 OpenAI、Claude、Codex、Antigravity、Kimi、xAI 等在内的各种 AI 模型提供商的 OAuth2 凭证。
- **高可用性**：支持多账号管理和轮询负载均衡，以确保最佳性能和可靠性。
- **安全访问**：提供精简的 CLI 身份验证流程，确保安全访问 API。
- **插件支持**：支持通过 Home Management 安装并启用兼容 CLIProxyAPI 的插件。
- **灵活的上游集成**：通过配置轻松集成上游提供商，包括：
  - OpenAI 兼容提供商（例如 OpenRouter）
  - OpenAI Responses 协议提供商
  - Anthropic Messages 协议提供商
  - Google Gemini 协议提供商

## 集群模式

Home 现在始终使用数据库驱动的运行时。没有 `cluster.yaml` 时，Home 使用本地 SQLite 数据库运行同一套调度、节点发现、选主、事件监听、Management 和 RESP 逻辑。默认 SQLite 路径是 `home.db`，可以通过 `-sqlite-path` 覆盖。

存在 `cluster.yaml` 时，Home 启用集群模式，并使用该文件中声明的数据库后端。可以复制 `cluster.sqlite.example.yaml` 或 `cluster.pgsql.example.yaml` 为 `cluster.yaml` 后再修改。多节点部署推荐使用 PostgreSQL；SQLite 也可以作为集群后端，但必须配置 `node.external-ip`，否则其他 Home 节点和 CPA 客户端无法稳定发现这个节点。

`config.yaml` 和 `auth-dir` 不再是运行时存储，只作为导入/导出的交换格式。迁移已有本地配置时，先运行：

```bash
./CLIProxyAPIHome -import
```

导入默认读取当前目录的 `config.yaml`，再根据该配置里的 `auth-dir` 查找凭证。可以通过 `-config <path>` 指定配置文件，通过 `-auth-dir <path>` 覆盖凭证目录，通过 `-sqlite-path <path>` 导入到非默认 SQLite 数据库。导入是幂等的，同一批文件重复导入不会产生重复记录。

需要将配置和凭证导出为本地文件时，运行：

```bash
./CLIProxyAPIHome -export
```

导出默认会写出当前目录的 `config.yaml`，并把凭证写到 `~/.cli-proxy-api/`。可以通过 `-export-dir <path>` 写出 `<path>/config.yaml`，并把凭证写到 `<path>/auths/`。如果目标 `config.yaml` 已存在，或者目标凭证目录已存在且非空，导出会拒绝覆盖。

集群模式下，Management API 会直接操作数据库中的数据。默认监听端口来自 `cluster.yaml` 的 `node.port`；也可以通过启动参数 `-addr` 覆盖监听地址。如果前置反代改变了 client 实际需要连接的端口，可以设置 `node.external-port`；未设置时，对外发布的集群节点端口仍以最终监听端口为准。

RESP 密码认证已经移除。Home RESP 只接受通过 mTLS 认证的客户端；CPA 应使用 `-home-jwt` 或配置好的 TLS 材料连接。`allow-host` 只是 IP 允许列表，不是密码机制。

Docker Compose 使用 `docker-compose.sqlite.yml` 或 `docker-compose.pgsql.yml`。将对应的 `cluster.sqlite.example.yaml` 或 `cluster.pgsql.example.yaml` 复制为 `cluster.yaml` 后，SQLite 路径使用 `/CLIProxyAPIHome/data/home.db`，PostgreSQL 主机使用 `postgres`。导入已有本地配置时二选一：

```bash
COMPOSE_FILE=docker-compose.sqlite.yml
# PostgreSQL: COMPOSE_FILE=docker-compose.pgsql.yml
docker compose -f "$COMPOSE_FILE" run --rm \
  -v "$PWD/config.yaml:/CLIProxyAPIHome/config.yaml:ro" \
  -v "$PWD/auths:/root/.cli-proxy-api" \
  home ./CLIProxyAPIHome -import
```

## 严格凭证并发限制

启用 credential concurrency policy 前，必须遵循[严格凭证并发限制部署运行手册](docs/CN.md)。手册规定了 SQLite 和 PostgreSQL 所需的拓扑、certificate identity 要求、capability 检查和 fail-closed mixed-version 发布方式。

## 贡献

欢迎贡献！请随时提交 Pull Request。

1. Fork 仓库
2. 创建您的功能分支（`git checkout -b feature/amazing-feature`）
3. 提交您的更改（`git commit -m 'Add some amazing feature'`）
4. 推送到分支（`git push origin feature/amazing-feature`）
5. 打开 Pull Request

## 许可证

此项目根据 MIT 许可证授权 - 有关详细信息，请参阅 [LICENSE](LICENSE) 文件。
