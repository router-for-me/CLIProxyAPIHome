# Home

[English](README.md) | 中文

Home 是一个为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 设计的全面凭证**管理、调度和分发**服务。

它作为在大规模 CLIProxyAPI 集群部署中管理和调度 API 凭证的中心枢纽。CLIProxyAPI 节点与 Home 通信以获取必要的 API 凭证并上报使用统计数据。

## Home 与 Home Management Center（HMC）的关系

Home 提供后端服务，[Home Management Center（HMC）](https://github.com/router-for-me/Home-Management-Center) 为 Home 提供可视化 Web 界面。**两个开源项目独立维护，HMC 页面内嵌于 Home 的正式发行版中，统一部署和更新。**

### 部署正式发行版

Home 官方二进制和 Docker 镜像已包含 Web 界面，无需单独部署 HMC。启动 Home 后，访问其地址，例如 `http://localhost:8327`，即可打开页面；管理员入口为 `/management.html`，用户入口为 `/user.html`。

管理员登录需要管理密钥，配置方法见[认证说明](docs/management/api_CN.md)。数据库配置与配置导入方式见下文[集群模式](#集群模式)。更新 Web 界面时，升级 Home 二进制或 Docker 镜像即可。

### 本地同时开发两个项目

开发前端时，将两个仓库克隆到同一父目录下，分别在两个终端中运行 Home 和 HMC。环境要求见 Home 的 [go.mod](go.mod) 和 [HMC 开发说明](https://github.com/router-for-me/Home-Management-Center#readme)。

首次在本地搭建且没有 `cluster.yaml` 时，将 Home 的 `config.example.yaml` 复制为 `config.yaml`，填写非空的 `management.secret-key`。先将配置导入本地 SQLite 数据库，再在一个终端中启动 Home：

```bash
cd CLIProxyAPIHome
go run ./cmd/home -config config.yaml -import
go run ./cmd/home -addr 127.0.0.1:8327
```

已有配置好的 Home 后端时，可以直接使用。

在另一个终端中启动 HMC：

```bash
cd Home-Management-Center
bun install --frozen-lockfile
bun run dev
```

打开 `http://localhost:3000/management.html`，将管理端点填写为 `http://127.0.0.1:8327`，使用管理密钥登录。修改前端后即可预览，无需重新编译 Home。

### 前端内嵌构建与发布流程

从源码构建包含 Web 界面的 Home 时，需要先构建 HMC，将完整产物复制到 Home 后再编译。从两个仓库的共同父目录开始执行：

```bash
cd Home-Management-Center
bun install --frozen-lockfile
bun run build:embedded
cp -R dist/. ../CLIProxyAPIHome/internal/managementasset/static/
cd ../CLIProxyAPIHome
go build -o CLIProxyAPIHome ./cmd/home
```

Home 源码仓库不包含生成后的前端产物，`go build` 和 `docker build` 也不会自动构建 HMC。因此，无论构建二进制还是 Docker 镜像，都需要先准备好完整的前端产物。

正式发布会自动完成上述过程：推送版本标签后，发布流程先构建前端，再打包 Home，生成并发布包含 Web 界面的二进制或 Docker 镜像。具体见[二进制发布工作流](.github/workflows/release.yaml)和 [Docker 工作流](.github/workflows/docker-image.yml)。

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

存在 `cluster.yaml` 时，Home 启用集群模式，并使用该文件中声明的数据库后端。可以复制 `cluster.sqlite.example.yaml` 或 `cluster.pgsql.example.yaml` 为 `cluster.yaml` 后再修改。PostgreSQL 既支持拆分连接字段，也支持在 `pgsql.postgres_uri` 中填写连接字符串。SQLite 每个数据库仅支持一个 active Home 进程；多 Home 部署必须使用 PostgreSQL。多个 CPA 节点可以连接同一个使用 SQLite 的 Home。SQLite 使用 `cluster.yaml` 时，请配置 `node.external-ip`，确保 CPA 客户端可以访问 Home。

使用 CPA 自动连接均衡前，请先将所有 Home 节点升级到支持该功能的版本；新旧 Home 混合运行期间不保证自动均衡效果。

可以在所选的 `pgsql` 或 `sqlite` 后端下设置 `slow-query-threshold`，控制慢 SQL 日志阈值。该字段接受 `"500ms"`、`"1s"` 等 Go duration 字符串，默认为 `"200ms"`。

PostgreSQL 连接池可在 `cluster.yaml` 中配置：`pgsql.max-open-conns` 默认为 `0`（不限总连接数），`pgsql.max-idle-conns` 默认为 `2`（受正数总连接上限约束）。显式设置 `max-idle-conns: 0` 表示不保留空闲连接；显式空闲上限不能超过正数总连接上限。这些限制按 Home 进程独立计算，修改后需重启。

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

`-export` / `-import` 保留旧版交换格式。API key 只会导出实际 key 值，因此 Home 数据库专属的 API key 展示名称、用户归属、channel/model group 绑定等元数据不构成完整备份。完整数据库备份和恢复应使用 `-db-export <snapshot.zip>` 与 `-db-import <snapshot.zip>`。数据库快照 v4 包含 API key 展示名称，旧版 Home 无法导入；升级前应保留数据库备份或旧版本兼容快照，以便回滚。

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

凭证并发策略的配置接口与启用前提见 [Management API 文档](docs/management/api_CN.md#credential-concurrency-policy-和-limiter-路由)。

## 贡献

欢迎提交问题、功能建议和 Pull Request，请根据涉及的组件选择对应仓库：

- **前端 / Web UI**：页面显示、交互等问题请提交到 [HMC Issues](https://github.com/router-for-me/Home-Management-Center/issues)，前端代码修改提交至 HMC 仓库。
- **Home 后端**：服务运行、凭证调度、数据管理等问题请提交到 [CLIProxyAPIHome Issues](https://github.com/router-for-me/CLIProxyAPIHome/issues)，后端代码修改提交至本仓库。

同时涉及两个项目的改动，请关联对应的 Issue 或 Pull Request；接口变更还需同步更新相关的 [Management API](docs/management/api_CN.md) 或 [User API](docs/user/api_CN.md) 文档。

1. Fork 仓库
2. 创建您的功能分支（`git checkout -b feature/amazing-feature`）
3. 提交您的更改（`git commit -m 'Add some amazing feature'`）
4. 推送到分支（`git push origin feature/amazing-feature`）
5. 打开 Pull Request

## 许可证

此项目根据 MIT 许可证授权 - 有关详细信息，请参阅 [LICENSE](LICENSE) 文件。
