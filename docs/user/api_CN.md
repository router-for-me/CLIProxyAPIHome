# CLIProxyAPIHome User API 文档

本文档描述 CLIProxyAPIHome 当前 DB-backed User API。User API 与 Management API 分离，不使用 Management API secret key。

基础路径：

```text
http://<host>:<port>/user
```

Home 示例端口通常为 `8327`。实际监听地址来自 runtime config、`cluster.yaml` 或 `-addr` 的最终值。

## Runtime 模型

用户记录、可选邮箱状态、一次性安全 token、邮件任务、API key、TOTP 配置和 passkey entries 存储在 database-backed cluster repository 中。`/user/*` route group 只会在 Home 使用 database-backed runtime route set 时注册。

通过 User API 修改 API key 会更新 Home dispatch 使用的同一张 `api_key` 表，并发布 config refresh event。

## 用户邮箱配置

邮箱验证与密码找回是可选能力，默认关闭。只有配置完整且可用时，`GET /capabilities` 返回的三个 capability flag 才会启用。

```yaml
user-email:
  enabled: true
  public-user-url: "https://home.example.com/user.html"
  from-address: "no-reply@example.com"
  from-name: "CLIProxyAPIHome"
  sender:
    type: "smtp"
    smtp:
      host: "smtp.example.com"
      port: 587
      username: "smtp-user"
      password-env: "HOME_USER_EMAIL_SMTP_PASSWORD"
      starttls: true
  verification-token-ttl: "24h"
  reset-token-ttl: "30m"
```

配置规则：

- `public-user-url` 必须是绝对 HTTPS URL；只有 `localhost` 或回环 IP 可以使用明文 HTTP。
- `from-address` 必须是单个邮箱地址，不能带 display name。
- 首个 sender 实现为 SMTP。所有非回环 SMTP host 都必须配置 `starttls: true`；Home 要求 TLS 1.2 或更高版本，暂不支持 465 端口的 implicit TLS。配置 `username` 时，`password-env` 必须指向非空环境变量；密码不会写入 `config.yaml`。
- `verification-token-ttl` 与 `reset-token-ttl` 必须是正数 Go duration 字符串。
- 配置缺失或无效时，邮箱 capability 保持关闭，但不会阻止 Home 启动。
- `user-email` 只属于 Home，不会下发到 CPA 节点。
- Home 默认忽略转发的客户端 IP header。若 Home 前面有明确的反向代理，只在顶层 `trusted-proxies` 中填写该 Nginx/Caddy/负载均衡器的精确 IP 或 CIDR；系统拒绝 trust-all 网段，修改后需要重启。这样既能让注册与找回限流识别真实来源，又不会允许直连客户端伪造地址。
- 关闭功能会保留数据库中的邮箱状态，但停止新的邮箱修改、验证请求和找回请求。此前签发的 verify/reset token 在过期或被撤销前仍可消费。

## 认证

公开 routes：

| Method | Path | 说明 |
| --- | --- | --- |
| `GET` | `/capabilities` | 返回可选 User API capability。 |
| `GET` | `/models` | 返回公开模型目录。 |
| `POST` | `/register` | 创建用户并返回 bearer token。 |
| `POST` | `/login` | 用户未启用 passkey 和 TOTP 时的密码登录。 |
| `POST` | `/login/totp` | 用户启用 TOTP 时的密码 + TOTP 登录。 |
| `POST` | `/login/passkey` | Passkey 登录。 |
| `POST` | `/email/verify` | 消费邮箱验证 token。 |
| `POST` | `/password/forgot` | 接收通用密码找回请求。 |
| `POST` | `/password/reset` | 消费重置 token 并设置新密码。 |

其他所有 `/user/*` route 都需要注册或登录成功后返回的 bearer token。
Bearer token 是使用集群根 CA 私钥签名、并使用集群根 CA 公钥验证的 RS256 JWT。替换集群根 CA 后，之前签发的 User API token 会失效。
Bearer token 还包含用户当前 session version。密码修改、密码重置或 Management API 管理员更新密码成功后都会递增该版本，使旧 bearer token 失效。已认证修改密码会为当前客户端返回替换 session；密码重置不会自动登录。
密码会按照原始输入值进行 hash 和校验，不会去除首尾空白字符。新 bcrypt 密码最多为 72 个 UTF-8 字节。
User API JSON 请求体最大为 1 MiB。

支持的请求头：

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <USER_TOKEN>` |

登录优先级：

| 状态 | 行为 |
| --- | --- |
| 已启用 passkey 且已启用 TOTP | 普通密码登录返回 `401 passkey_required`；`/login/passkey` 和 `/login/totp` 都可登录。 |
| 已启用 passkey 但未启用 TOTP | 普通密码登录返回 `401 passkey_required`；使用 `/login/passkey`。 |
| 未启用 passkey 但已启用 TOTP | 普通密码登录返回 `401 totp_required`；使用 `/login/totp`。 |
| 未启用 passkey 且未启用 TOTP | 普通密码登录返回 bearer token。 |

Home User API 会额外写入以下响应头：

| Header | 说明 |
| --- | --- |
| `x-cpa-home-version` | Home 构建版本。 |
| `x-cpa-home-commit` | Home 构建 commit。 |
| `x-cpa-home-build-date` | Home 构建日期。 |

## 通用响应

多数删除或简单写入接口成功时返回：

```json
{ "status": "ok" }
```

注册和登录成功时返回：

```json
{
  "token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",
  "expires_at": "2026-06-02T10:00:00Z",
  "user": {
    "id": 1,
    "username": "alice",
    "credits": 0,
    "totp_enabled": false,
    "passkey_count": 0,
    "email_status": {
      "configured": true,
      "verified": false,
      "masked": "a***@example.com",
      "recovery_ready": false
    },
    "created_at": "2026-06-02T10:00:00Z",
    "updated_at": "2026-06-02T10:00:00Z"
  }
}
```

User API handler 通常同时返回机器可读 `error` 和可读 `message`：

```json
{ "error": "invalid_credentials", "message": "invalid credentials" }
```

校验与认证失败会保留具体且安全的提示；服务端 `5xx` 错误仍保留机器可读 code，但统一使用 `internal server error`，避免向公网暴露数据库或实现细节。

常见错误：

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_required", "message": "totp code is required" }
{ "error": "invalid_body", "message": "username is required" }
```

## 已注册 Routes

以下清单来自 `internal/userapi/handler.go` 注册的 User API route group。

| Method | Path |
| --- | --- |
| `GET` | `/capabilities` |
| `GET` | `/models` |
| `GET` | `/models/accessible` |
| `POST` | `/register` |
| `POST` | `/login` |
| `POST` | `/login/passkey/begin` |
| `POST` | `/login/passkey/options` |
| `POST` | `/login/totp` |
| `POST` | `/login/passkey` |
| `GET` | `/me` |
| `PATCH` | `/password` |
| `POST` | `/password` |
| `POST` | `/password/forgot` |
| `POST` | `/password/reset` |
| `PUT` | `/email` |
| `DELETE` | `/email` |
| `POST` | `/email/verification` |
| `POST` | `/email/verify` |
| `GET` | `/totp` |
| `POST` | `/totp` |
| `POST` | `/totp/show` |
| `POST` | `/totp/bind` |
| `DELETE` | `/totp` |
| `GET` | `/billing/overview` |
| `GET` | `/billing/charges` |
| `GET` | `/api-keys` |
| `POST` | `/api-keys` |
| `POST` | `/api-key` |
| `PATCH` | `/api-keys` |
| `PATCH` | `/api-key` |
| `PATCH` | `/api-keys/:id` |
| `PATCH` | `/api-key/:id` |
| `DELETE` | `/api-keys` |
| `DELETE` | `/api-key` |
| `DELETE` | `/api-keys/:id` |
| `DELETE` | `/api-key/:id` |
| `POST` | `/passkeys/begin` |
| `POST` | `/passkey/begin` |
| `POST` | `/passkeys/options` |
| `POST` | `/passkey/options` |
| `POST` | `/passkeys` |
| `POST` | `/passkey` |
| `DELETE` | `/passkeys` |
| `DELETE` | `/passkey` |
| `DELETE` | `/passkeys/:id` |
| `DELETE` | `/passkey/:id` |

## 账号

### GET `/capabilities`

返回当前 Home 配置是否支持可选邮箱注册、邮箱验证与密码找回。该 route 不暴露 SMTP 配置或用户数据。

响应示例：

```json
{
  "capabilities": {
    "email_registration": true,
    "email_verification": true,
    "password_recovery": true,
    "model_catalog": true
  },
  "server_info": {
    "home_version": "v1.2.3",
    "home_commit": "abcdef0",
    "home_build_date": "2026-07-20"
  }
}
```

当 `user-email.enabled` 为 false，或邮件配置不完整/无效时，三个邮箱相关 flag 都为 `false`。旧版 Home 可能因没有该 route 而返回 `404`。

在提供 `/user/models` 的 Home 版本上，`model_catalog` 恒为 `true`；旧版本不会返回该字段。客户端应读取该 flag 来决定是否展示模型目录，而不是直接调用目录 route 并把 `404` 当作故障处理。

### POST `/register`

创建用户账号，保存 bcrypt password hash，并返回 bearer token。

示例请求：

```json
{
  "username": "alice",
  "password": "secret",
  "email": "alice@example.com"
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `username` | string | yes | 用户名。Aliases: `user_name`, `user-name`。 |
| `password` | string | yes | 用于生成存储密码 hash 的明文密码。 |
| `email` | string | no | 可选邮箱。仅在 `email_registration` 为 true 时接受；会先归一化并以未验证状态保存，验证成功前不会占用唯一所有权。 |

响应：登录响应结构。提供邮箱时，Home 会先执行验证投递限流，再尝试异步排队；地址已被拥有或请求已受限时仍接受注册，但不会发送邮件。注册成功不代表邮箱已验证或已可用于找回。

所有注册尝试（包括不带邮箱的 username-only 注册）都会按来源 IP 和全局维度限流；验证邮件投递另有目标邮箱维度限制。被限制时返回 `429 registration_rate_limited` 与 `Retry-After`。

常见错误：

```json
{ "error": "user_exists", "message": "user already exists" }
{ "error": "email_feature_unavailable", "message": "email feature is unavailable" }
{ "error": "invalid_email", "message": "email is invalid" }
{ "error": "invalid_body", "message": "password is required" }
{ "error": "invalid_password", "message": "password exceeds the 72-byte bcrypt limit" }
{ "error": "request_too_large", "message": "request body exceeds 1048576 bytes" }
{ "error": "registration_rate_limited", "message": "registration rate limit exceeded" }
```

### POST `/login`

当用户未启用 passkey 且未启用 TOTP 时，使用用户名和密码登录。

示例请求：

```json
{
  "username": "alice",
  "password": "secret"
}
```

响应：登录响应结构。

常见错误：

```json
{ "error": "invalid_credentials", "message": "invalid credentials" }
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_required", "message": "totp code is required" }
```

### POST `/login/totp`

使用用户名、密码和 TOTP code 登录。只要用户启用了 TOTP，即使同时有 passkey，也可以使用此 route 登录。仅启用 passkey 的用户调用此 route 会返回 `401 passkey_required`。

示例请求：

```json
{
  "username": "alice",
  "password": "secret",
  "totp_code": "123456"
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `username` | string | yes | 用户名。Aliases: `user_name`, `user-name`。 |
| `password` | string | yes | 用户密码。 |
| `totp_code` | string | yes | TOTP code。Aliases: `totp-code`, `totp`, `code`。 |

响应：登录响应结构。

常见错误：

```json
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_not_enabled", "message": "totp is not enabled" }
{ "error": "invalid_totp", "message": "invalid totp code" }
```

### POST `/login/passkey`

使用存储在 `user.passkey` 中的 passkey entry 登录。

示例请求：

```json
{
  "username": "alice",
  "passkey_id": "pk_xxx",
  "passkey_secret": "one-time-returned-secret"
}
```

如果 passkey 是用 `credential` payload 创建的，此 route 也可以对比 opaque JSON `credential` 字段。

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `username` | string | yes | 用户名。Aliases: `user_name`, `user-name`。 |
| `passkey_id` | string | yes | Passkey ID。Alias: `passkey-id`；也接受 `id`。 |
| `passkey_secret` | string | conditionally | 创建 passkey 时返回的 secret。Alias: `secret`。 |
| `credential` | object | conditionally | 未存储 secret hash 时用于精确比较的 opaque credential JSON。 |

响应：登录响应结构。

常见错误：

```json
{ "error": "invalid_passkey", "message": "invalid passkey" }
{ "error": "invalid_body", "message": "username and passkey_id are required" }
```

### GET `/me`

根据 bearer token 返回当前认证用户的信息。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例响应：

```json
{
  "status": "ok",
  "user": {
    "id": 1,
    "username": "alice",
    "credits": 0,
    "totp_enabled": false,
    "passkey_count": 1,
    "email_status": {
      "configured": true,
      "verified": true,
      "masked": "a***@example.com",
      "recovery_ready": true
    },
    "passkeys": [
      {
        "id": "passkey-1",
        "name": "MacBook Touch ID",
        "created_at": "2026-06-02T10:05:00Z",
        "updated_at": "2026-06-02T10:05:00Z"
      }
    ],
    "created_at": "2026-06-02T10:00:00Z",
    "updated_at": "2026-06-02T10:10:00Z"
  }
}
```

常见错误：

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
```

### POST/PATCH `/password`

修改当前认证用户的密码。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例请求：

```json
{
  "new_password": "new-secret"
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `new_password` | string | yes | 新明文密码。Alias: `new-password`。 |

响应：登录响应结构，包含新的 `token` 与 `expires_at`。密码更新会递增用户 session version，因此本次请求使用的 bearer token 和其他所有旧 bearer token 都会失效；客户端必须原子替换为返回的新 session。

TOTP 与 passkey 数据不会被修改。

## 邮箱验证与密码找回

邮箱是可选字段。Home 保存归一化后的小写地址，但 User API 响应只暴露 `email_status`：

只有已验证邮箱才构成唯一所有权声明。多个 active account 可以暂时保存同一个未验证地址，因此匿名注册不能永久抢占他人的邮箱，注册/更新响应也不会泄露该地址是否已被其他账号拥有。若地址已由其他 active account 拥有，Home 仍返回正常 accepted 结果，但不会发送验证邮件。首个成功验证尚未被占用地址的账号会原子取得所有权；之后发生冲突的验证链接统一返回“无效或已过期”。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `configured` | boolean | 当前是否已保存邮箱。 |
| `verified` | boolean | 当前 email version 是否已完成所有权验证。 |
| `masked` | string | 例如 `a***@example.com` 的隐私安全展示值；未配置时为空。 |
| `recovery_ready` | boolean | 邮箱已验证且当前邮件配置可用。 |

修改邮箱会立即清除验证状态、递增 email version、撤销未完成的 verify/reset token，并将待处理邮件任务标记为 superseded。提交完全相同的归一化地址是幂等操作，不会清除验证状态。

### PUT `/email`

添加或替换当前认证用户的邮箱。需要 bearer token，并要求邮箱 capability 已启用。

```json
{ "email": "new@example.com" }
```

成功返回 `200` 和 `{ "status": "ok", "user": ... }`。返回用户中的新邮箱状态为未验证。该 route 只保存地址，不直接排队发送邮件；随后调用 `/email/verification`。

常见错误：

```json
{ "error": "invalid_email", "message": "email is invalid" }
{ "error": "email_feature_unavailable", "message": "email feature is unavailable" }
{ "error": "email_update_rate_limited", "message": "email update rate limit exceeded" }
```

### DELETE `/email`

删除当前认证用户保存的邮箱。即使邮箱 capability 已关闭，该 route 仍保持可用，确保用户始终可以删除可选个人信息。删除邮箱会递增 email version、撤销未完成的 verify/reset token、将待处理邮件任务标记为 superseded，并释放该地址供其他 active user 使用。

成功返回 `200 { "status": "ok", "user": ... }`。未配置邮箱时重复调用是幂等操作。

### POST `/email/verification`

为当前认证用户的未验证邮箱排队发送验证邮件。无需请求体。

成功：

```http
HTTP/1.1 202 Accepted
```

```json
{ "status": "accepted" }
```

邮箱已经验证时，幂等返回 `200 { "status": "ok" }`。注册时的首次验证邮件与手动重发共用同一组限流：严格 60 秒冷却，并同时按 user、目标邮箱、来源 IP 和全局维度限制。手动重发被限制时返回 `429 verification_rate_limited`，`Retry-After` 为实际剩余秒数。

```json
{ "error": "email_not_configured", "message": "email is not configured" }
{ "error": "verification_rate_limited", "message": "verification request rate limit exceeded" }
```

### POST `/email/verify`

消费短时、单次使用的邮箱验证 token。邮件中的 UI 链接应先展示明确确认页面，只有用户确认后才调用此 POST，避免邮件安全扫描器直接消费 token。

```json
{ "token": "base64url-token" }
```

成功：`200 { "status": "ok" }`。

过期、已用、未知、已被替代、email version 不匹配，或目标邮箱已由其他账号拥有的 token 都统一返回：

```json
{ "error": "invalid_or_expired_token", "message": "verification link is invalid or expired" }
```

### POST `/password/forgot`

接收邮箱地址；如果存在符合条件且已验证的账号，则排队发送重置邮件。邮箱 capability 启用后，所有可解析请求都返回完全相同的状态与响应体，无论邮箱缺失、非法、未知、未验证、已限流，还是邮件暂时无法入队。

```json
{ "email": "alice@example.com" }
```

```http
HTTP/1.1 202 Accepted
```

```json
{
  "status": "accepted",
  "message": "If an eligible account matches, password reset instructions will be sent."
}
```

Home 会在入队前执行目标邮箱五分钟冷却，以及 IP、邮箱小时级和全局限流；同时将 accepted 响应补齐到最短处理时长，以降低符合条件与不符合条件账号之间的耗时差异。客户端仍必须对所有 accepted 响应展示同一确认 UI。全局邮箱 capability 关闭时，该 route 返回 `404 email_feature_unavailable`。

### POST `/password/reset`

消费短时、单次使用的 reset token 并设置新密码。

```json
{
  "token": "base64url-token",
  "new_password": "new-secret"
}
```

也接受 `new-password` alias。成功返回 `200 { "status": "ok" }`，递增 session version，并撤销所有旧 User API bearer token、其他未使用 reset token 与排队中的密码重置邮件任务。认证用户修改密码或 Management API 管理员更新密码时也会执行相同的 reset token 与队列任务撤销。密码重置不会返回登录 session。TOTP 与 passkey 保持不变，下次登录时仍继续生效。

无效、过期、已用、已被替代或 email version 不匹配统一返回：

```json
{ "error": "invalid_or_expired_token", "message": "password reset link is invalid or expired" }
```

## TOTP

### GET `/totp`

返回当前认证用户的 TOTP setup data。如果 TOTP 已启用且 `regenerate` 不是 true，则返回现有 secret。

请求头：

```http
Authorization: Bearer user.jwt.token
```

Query 参数：

| Query | Type | 说明 |
| --- | --- | --- |
| `issuer` | string | otpauth URL 的可选 issuer。 |
| `regenerate` | boolean | 生成新的 setup secret，而不是返回当前 secret。 |

示例响应：

```json
{
  "secret": "BASE32SECRET",
  "otp_auth_url": "otpauth://totp/CLIProxyAPIHome:alice?...",
  "issuer": "CLIProxyAPIHome",
  "period": 30,
  "digits": 6,
  "algorithm": "SHA1",
  "enabled": false
}
```

### POST `/totp/show`

`GET /totp` 的 POST alias。接受同样的 bearer token 和可选 JSON 字段。

示例请求：

```json
{
  "issuer": "CLIProxyAPIHome",
  "regenerate": true
}
```

响应：同 `GET /totp`。

### POST `/totp`

校验并保存当前认证用户的 TOTP secret。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例请求：

```json
{
  "secret": "BASE32SECRET",
  "code": "123456"
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `secret` | string | yes | 来自 `/totp` 的 Base32 TOTP secret。 |
| `code` | string | yes | 当前 TOTP code。Aliases: `totp_code`, `totp-code`, `totp`。 |
| `issuer` | string | no | 存入 TOTP metadata 的 issuer。默认为 `CLIProxyAPIHome`。 |

响应：同 `POST/PATCH /password`。

常见错误：

```json
{ "error": "invalid_totp", "message": "invalid totp code" }
{ "error": "invalid_body", "message": "secret and code are required" }
```

### POST `/totp/bind`

`POST /totp` 的 alias。

### DELETE `/totp`

删除当前认证用户的 TOTP 配置。

请求头：

```http
Authorization: Bearer user.jwt.token
```

请求体：无。

响应：同 `POST/PATCH /password`；成功删除后 `user.totp_enabled` 为 `false`。

常见错误：

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
```

## 模型目录

模型目录路由位于 `/user` 基础路径下，因此完整路径是 `/user/models` 和 `/user/models/accessible`。`/user/models` 无需认证；`/user/models/accessible` 需要 `/user/register` 或 `/user/login` 返回的 Bearer token。两个路由都不接受也不需要 Management Key。

两个路由回答的是不同的问题。`/user/models` 回答"这个集群能提供什么模型"，访客在拥有账号之前就可以查询。`/user/models/accessible` 回答"我能调用什么、价格是多少、最近表现如何"，答案取决于调用方自己的 API keys 与商务条款。

两个响应都不包含 Management API 数据、凭据身份、节点身份、路由细节、价格规则 ID、价格规则来源、价格规则备注，以及任何其他用户的数据。

### 三态语义

模型元数据以显式状态而非空值返回，因为"没有人描述过这个模型"绝不能被呈现成"这个模型不具备该能力"：

| 状态 | 含义 |
| --- | --- |
| `known` / `supported` | 上游 provider 或集群配置明确声明了该能力。 |
| `unsupported` | 模型公布了参数列表，而该能力不在其中。 |
| `unknown` | 没有任何来源描述过该模型。应渲染为"未公布"，而不是"不支持"。 |

价格与可用性遵循同一规则：

- 没有启用价格规则的模型是 `unpublished`。它不是免费的，也不是价格为 0。
- 观测样本少于窗口最小值的模型是 `insufficient_data`。它不是 100% 可用。

### GET `/models`

返回公开目录：集群当前能够提供的全部模型。

无需任何 header。

响应示例：

```json
{
  "models": [
    {
      "id": "gpt-4.1-mini",
      "display_name": "GPT-4.1 mini",
      "description": "Fast general purpose model.",
      "type": "chat",
      "providers": ["openai"],
      "context_length": 128000,
      "max_output_tokens": 16384,
      "modalities": {
        "status": "known",
        "input": ["text", "image"],
        "output": ["text"]
      },
      "capabilities": {
        "reasoning": { "status": "supported", "levels": ["low", "medium", "high"] },
        "tool_calling": { "status": "supported" },
        "structured_output": { "status": "supported" },
        "parameters": ["tools", "temperature", "reasoning_effort", "response_format"]
      }
    }
  ],
  "total": 1
}
```

模型字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 发起 API 请求时使用的字面量模型标识符。不得翻译或改写。 |
| `display_name` | string | 可选的可读名称。上游未提供时不返回。 |
| `description` | string | 可选的上游简介。 |
| `version` | string | 可选的上游版本号。 |
| `owned_by` | string | 可选的上游归属方。 |
| `type` | string | 可选的模型类型，例如 `chat`。 |
| `providers[]` | array | 能够提供该模型的 provider 标识符，与用量记录和价格规则中使用的标识符一致。 |
| `context_length` | number | 最大输入 token 数。未知时不返回；上游的 `context_length` 与 `inputTokenLimit` 两种写法在此归一为同一字段。 |
| `max_output_tokens` | number | 最大输出 token 数。未知时不返回；归一 `max_completion_tokens` 与 `outputTokenLimit`。 |
| `modalities` | object | 见下。 |
| `capabilities` | object | 见下。 |

`modalities`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `status` | string | `known` 或 `unknown`。 |
| `input[]` | array | 支持的输入模态，例如 `text`、`image`、`audio`。仅在 `status` 为 `known` 时出现。 |
| `output[]` | array | 支持的输出模态。仅在 `status` 为 `known` 时出现。 |

对外发布的取值范围是 `text`、`image`、`audio`、`video`。文档格式不属于模态，因此不在此返回：部分厂商会把 PDF 列为输入类型，其余厂商并不这样描述输入，若一并发布，就会拿只有部分厂商作出的区分去横向比较所有厂商。

模态数据来自人工维护的模型目录（`models.json`），由人整理而非从上游探测。取值来自厂商文档；厂商自己发布清单的，则以清单为准——Codex 系列取自与目录同仓的 `codex_client_models.json`，其中带有 OpenAI 自己声明的 `input_modalities`。目录未描述的模型返回 `status: "unknown"`，绝不返回空的 `input` 数组——客户端有理由把空数组理解为“仅支持文本”。返回前会做小写化与去重。

某个模型没有被描述是正常结果，不是待补的缺口，更不该事后用推测填上。少数只公开了上下文与速度、从未说明输入类型的模型预计将长期返回 `unknown`；这是目录按设计工作，不是待办事项。

`capabilities`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `reasoning.status` | string | `supported`、`unsupported` 或 `unknown`。 |
| `reasoning.levels[]` | array | 可选的推理强度级别。 |
| `reasoning.budget` | object | 可选的思考预算，含 `min`、`max`、`can_disable`、`dynamic`。模型未声明的键不会出现。 |
| `tool_calling.status` | string | `supported`、`unsupported` 或 `unknown`。 |
| `structured_output.status` | string | `supported`、`unsupported` 或 `unknown`。表示模型能否被约束为调用方给定的 schema。该判定在服务端根据参数列表完成，客户端不得自行推导。 |
| `parameters[]` | array | 可选的请求参数列表。 |
| `generation_methods[]` | array | 可选的上游生成方法。 |

该 route 永远不返回 `pricing` 与 `availability`。它们的缺失本身就是契约，而不是数据缺失：匿名访客无权获取运营方的商务条款。

### GET `/models/accessible`

返回当前认证用户的 API keys 实际可以调用的模型子集，并附带价格与观测到的可用性。

Headers：

```http
Authorization: Bearer user.jwt.token
```

响应示例：

```json
{
  "models": [
    {
      "id": "gpt-4.1-mini",
      "display_name": "GPT-4.1 mini",
      "providers": ["openai"],
      "context_length": 128000,
      "max_output_tokens": 16384,
      "modalities": { "status": "known", "input": ["text", "image"], "output": ["text"] },
      "capabilities": { "reasoning": { "status": "supported" }, "tool_calling": { "status": "supported" } },
      "pricing": {
        "status": "published",
        "providers": [
          {
            "provider": "openai",
            "tiers": [
              {
                "service_tier": "*",
                "is_default": true,
                "rungs": [
                  {
                    "min_input_tokens": 0,
                    "input_price_per_million": 0.4,
                    "output_price_per_million": 1.6,
                    "cache_read_price_per_million": 0.1,
                    "cache_write_price_per_million": 0.5,
                    "request_price": 0
                  },
                  {
                    "min_input_tokens": 128000,
                    "input_price_per_million": 0.8,
                    "output_price_per_million": 3.2,
                    "cache_read_price_per_million": 0.2,
                    "cache_write_price_per_million": 1,
                    "request_price": 0
                  }
                ]
              }
            ]
          }
        ]
      },
      "availability": {
        "status": "observed",
        "window": {
          "from": "2026-07-25T00:00:00Z",
          "to": "2026-08-01T00:00:00Z",
          "hours": 168,
          "min_samples": 10
        },
        "sample_count": 240,
        "success_count": 236,
        "failed_count": 4,
        "availability_rate": 0.9833,
        "avg_latency_ms": 2140.5,
        "avg_ttft_ms": 318.2,
        "output_tokens_per_second": 42.17,
        "first_observed_at": "2026-07-25T04:11:02Z",
        "last_observed_at": "2026-08-01T09:52:44Z"
      }
    }
  ],
  "total": 1,
  "access": {
    "restricted": false,
    "api_key_count": 2,
    "reason": "unrestricted"
  }
}
```

`access`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `restricted` | boolean | 当用户的每一个 API key 都被限定在 model groups 内时为 `true`。 |
| `api_key_count` | number | 用户持有的 API key 数量。 |
| `reason` | string | `unrestricted`、`model_groups` 或 `no_api_keys`。用于向用户解释列表为什么是空的，而不是只显示一个空态。 |

访问范围是用户各个 key 的并集而非交集：只要持有一个未限定范围的 key，整个目录都可访问。一个 key 都没有则任何模型都不可访问，因为没有凭据的账号无法调用集群。

`pricing` 是价格阶梯而不是标量，因为计费先按 service tier 再按输入规模解析：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `status` | string | `published` 或 `unpublished`。 |
| `providers[]` | array | 每个既能提供该模型、又存在启用价格规则的 provider 一项。指向无法提供该模型的 provider 的规则会被排除，因为它们不构成报价。 |
| `providers[].provider` | string | Provider 标识符。 |
| `providers[].tiers[]` | array | 每个 service tier 一项。 |
| `providers[].tiers[].service_tier` | string | Tier 名称，通配 tier 为 `*`。 |
| `providers[].tiers[].is_default` | boolean | 仅在 `*` tier 上出现且为 `true`。请求未指定 tier、或指定了没有定价的 tier 时按该 tier 计费。 |
| `providers[].tiers[].rungs[]` | array | 按 `min_input_tokens` 升序。请求按其满足的最后一个阶梯计价。 |
| `rungs[].min_input_tokens` | number | 该阶梯生效的输入 token 阈值。 |
| `rungs[].input_price_per_million` | number | 每百万输入 token 价格。 |
| `rungs[].output_price_per_million` | number | 每百万输出 token 价格。 |
| `rungs[].cache_read_price_per_million` | number | 每百万 cache read token 价格。 |
| `rungs[].cache_write_price_per_million` | number | 每百万 cache write token 价格。 |
| `rungs[].request_price` | number | 每次请求的固定价格。 |

价格分量为 0 时会显式返回而不是省略：provider 不对 cache read 计费本身就是一种声明，省略字段会让它与"没有人为该分量定价"无法区分。整个模型没有启用规则时返回 `"status": "unpublished"` 且不带 `providers`。

本版本不返回折扣元数据。客户端不得从上述字段推导、推断或展示折扣。

`availability` 汇总滚动窗口内观测到的实际表现：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `status` | string | `observed` 或 `insufficient_data`。 |
| `window.from` / `window.to` | string | 观测窗口的 RFC3339 边界，半开区间 `[from,to)`。 |
| `window.hours` | number | 窗口长度（小时）。当前为 `168`。 |
| `window.min_samples` | number | 发布可用率所需的最小观测数。当前为 `10`。 |
| `sample_count` | number | 窗口内的观测数。始终返回，包括为 `0` 时。 |
| `success_count` / `failed_count` | number | 仅在 `status` 为 `observed` 时出现。 |
| `availability_rate` | number | 成功数除以观测数，保留四位小数。**仅在 `status` 为 `observed` 时出现。** |
| `avg_latency_ms` | number | 可测量的成功请求的端到端平均延迟。无可测量样本时不返回。 |
| `avg_ttft_ms` | number | 平均首 token 时间。仅流式响应上报，因此可能在 `avg_latency_ms` 存在时缺失。 |
| `output_tokens_per_second` | number | 总输出 token 除以总生成耗时，长生成的权重更高，与调用方的实际体感一致。 |
| `first_observed_at` / `last_observed_at` | string | 实际观测的 RFC3339 时间边界，可能远窄于窗口。`insufficient_data` 的模型也可能带有 `last_observed_at`。 |

`status` 为 `insufficient_data` 时不返回可用率、延迟与吞吐。客户端必须渲染为"数据不足"，绝不能渲染为完全可用。该汇总是集群级的模型健康度，在进程内做短时缓存，不包含任何单个用户的请求数据。

## Billing

用户计费路由位于 `/user` 基础路径下，因此完整路径是 `/user/billing/overview` 和 `/user/billing/charges`。两个路由都需要 `/user/register` 或 `/user/login` 返回的现有 Bearer token，响应只包含当前认证 Bearer 用户的数据。

用户计费响应不包含管理员备注、全局汇总、模型价格管理数据、代理池数据、原始 API keys、脱敏 API keys、价格快照、匹配的价格规则、endpoint、`balance_before` 或其他用户的数据。

用户计费的 `from` 和 `to` 查询参数接受 `YYYY-MM-DD`、RFC3339 或 Unix 秒，并统一使用半开区间 `[from,to)`。Unix 秒值必须位于 `2000-01-01T00:00:00Z` 到 `9999-12-31T23:59:59Z` 之间；毫秒时间戳会被拒绝。只有日期的 `to` 会转换为下一个 UTC 零点，从而完整包含结束 UTC 日期。显式时间戳形式的 `to` 是精确的排他上界，不会自动扩展。需要查询完整非 UTC 自然日的客户端应发送从本地零点到下一个本地零点的 RFC3339 边界，例如 `2026-06-10T00:00:00+08:00` 到 `2026-06-11T00:00:00+08:00`。

### GET `/billing/overview`

返回当前认证用户的计费概览。

请求头：

```http
Authorization: Bearer user.jwt.token
```

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `from` | string | 可选开始时间：`YYYY-MM-DD`、RFC3339 或 Unix 秒。 |
| `to` | string | 可选排他结束时间；纯日期通过使用下一个 UTC 零点完整包含当天，显式时间戳精确保留。 |

响应示例：

```json
{
  "overview": {
    "current_balance": 18.75,
    "today_spend": 1.25,
    "month_spend": 1.25,
    "top_models": [
      {
        "id": "openai/gpt-4.1-mini",
        "label": "gpt-4.1-mini",
        "amount": 1.25,
        "request_count": 1
      }
    ]
  }
}
```

概览字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `current_balance` | number | 当前认证用户余额。 |
| `today_spend` | number | 当前计费概览查询返回的消费值。 |
| `month_spend` | number | 当前计费概览查询返回的消费值。 |
| `top_models[]` | array | 模型消费条目，字段为 `id`、`label`、`amount`、`request_count`。 |

### GET `/billing/charges`

列出当前认证用户的扣费记录。

请求头：

```http
Authorization: Bearer user.jwt.token
```

查询参数：

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `from` | string | 可选开始时间：`YYYY-MM-DD`、RFC3339 或 Unix 秒。 |
| `to` | string | 可选排他结束时间；纯日期通过使用下一个 UTC 零点完整包含当天，显式时间戳精确保留。 |
| `limit` | integer | 可选分页大小；默认 `50`，最大 `200`。非法的非正数或非整数会返回 `400`。 |
| `offset` | integer | 可选分页偏移；默认 `0`。负数或非整数会返回 `400`。 |

响应结构：

```json
{
  "items": [
    {
      "id": "charge_xxx",
      "created_at": "2026-06-10T10:00:00Z",
      "provider": "openai",
      "model": "gpt-4.1-mini",
      "input_tokens": 1000,
      "output_tokens": 500,
      "amount": 1.25,
      "balance_after": 18.75,
      "request_id": "req_xxx"
    }
  ],
  "total": 1,
  "limit": 50,
  "offset": 0
}
```

扣费条目字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 扣费记录 ID。 |
| `created_at` | string | 扣费创建时间。 |
| `provider` | string | Provider 名称。 |
| `model` | string | Model 名称。 |
| `input_tokens` | integer | 输入 tokens。 |
| `output_tokens` | integer | 输出 tokens。 |
| `amount` | number | 扣费金额。 |
| `balance_after` | number | 当前认证用户扣费后的余额。 |
| `request_id` | string | 与扣费关联的 request ID。 |

常见错误：

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
{ "error": "invalid_from", "message": "from must be YYYY-MM-DD, RFC3339, or unix seconds" }
{ "error": "invalid_to", "message": "to must be YYYY-MM-DD, RFC3339, or unix seconds" }
{ "error": "invalid_time_range", "message": "from must not be after to" }
{ "error": "invalid_limit", "message": "limit must be a positive integer" }
{ "error": "invalid_offset", "message": "offset must be a non-negative integer" }
```

## API Keys

API key routes 只操作绑定到当前认证 `user.id` 的 API key。

### GET `/api-keys`

列出当前认证用户拥有的 API key。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例响应：

```json
{
  "api_keys": [
    {
      "id": 1,
      "api-key": "client-key",
      "api_key": "client-key",
      "channels": [1],
      "model_groups": [2],
      "created_at": "2026-06-02T10:00:00Z",
      "updated_at": "2026-06-02T10:00:00Z"
    }
  ],
  "items": [
    {
      "id": 1,
      "api-key": "client-key",
      "api_key": "client-key",
      "channels": [1],
      "model_groups": [2],
      "created_at": "2026-06-02T10:00:00Z",
      "updated_at": "2026-06-02T10:00:00Z"
    }
  ]
}
```

### POST `/api-keys`

创建归属于当前认证用户的 API key。如果省略 `api_key`，Home 会自动生成一个。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例请求：

```json
{
  "api_key": "client-key",
  "channels": [1],
  "model_groups": [2]
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `api_key` | string | no | Client API key。Aliases: `api-key`, `key`, `value`。 |
| `channels` | array of integer | no | Channel group IDs。空数组或省略表示不限制。 |
| `model_groups` | array of integer | no | Model group IDs。Alias: `model-groups`；空数组或省略表示不限制。 |

示例响应：

```json
{
  "api_key": {
    "id": 1,
    "api-key": "client-key",
    "api_key": "client-key",
    "channels": [1],
    "model_groups": [2],
    "created_at": "2026-06-02T10:00:00Z",
    "updated_at": "2026-06-02T10:00:00Z"
  }
}
```

### POST `/api-key`

`POST /api-keys` 的 alias。

### PATCH `/api-keys`

修改当前认证用户拥有的 API key。目标可以通过 `id` 或 API key value 指定。

示例请求：

```json
{
  "id": 1,
  "api_key": "new-client-key",
  "channels": [],
  "model_groups": []
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `id` | integer | conditionally | API key record ID。 |
| `api_key` | string | conditionally | 提供 `id` 时表示新的 API key；没有 `id` 时表示目标 API key。Aliases: `api-key`, `key`, `value`。 |
| `old` | string | conditionally | 目标 API key value。 |
| `new` | string | no | 新 API key value。 |
| `new_api_key` | string | no | 新 API key value。Alias: `new-api-key`。 |
| `channels` | array of integer | no | 替换 channel group IDs。 |
| `model_groups` | array of integer | no | 替换 model group IDs。Alias: `model-groups`。 |

响应：同 `POST /api-keys`。

常见错误：

```json
{ "error": "not_found", "message": "record not found" }
{ "error": "invalid_body", "message": "api key id or value is required" }
{ "error": "api_key_exists", "message": "api key already exists" }
```

### PATCH `/api-key`

`PATCH /api-keys` 的 alias。

### PATCH `/api-keys/:id`

按 numeric record ID 修改当前认证用户拥有的 API key。

Path 参数：

| Parameter | Type | Required | 说明 |
| --- | --- | --- | --- |
| `id` | integer | yes | `api_key.id`；必须大于 `0`。 |

响应：同 `POST /api-keys`。

### PATCH `/api-key/:id`

`PATCH /api-keys/:id` 的 alias。

### DELETE `/api-keys`

删除当前认证用户拥有的 API key。目标可以通过 `id` 或 API key value 指定。

Query 参数：

| Query | Type | 说明 |
| --- | --- | --- |
| `id` | integer | API key record ID。 |
| `api_key` | string | API key value。 |
| `api-key` | string | `api_key` 的 alias。 |
| `key` | string | `api_key` 的 alias。 |
| `value` | string | `api_key` 的 alias。 |

示例响应：

```json
{ "status": "ok" }
```

### DELETE `/api-key`

`DELETE /api-keys` 的 alias。

### DELETE `/api-keys/:id`

按 numeric record ID 删除当前认证用户拥有的 API key。

Path 参数：

| Parameter | Type | Required | 说明 |
| --- | --- | --- | --- |
| `id` | integer | yes | `api_key.id`；必须大于 `0`。 |

响应：

```json
{ "status": "ok" }
```

### DELETE `/api-key/:id`

`DELETE /api-keys/:id` 的 alias。

## Passkeys

Passkey routes 操作存储在 `user.passkey` 中的 entries。

### POST `/passkeys`

为当前认证用户创建 passkey entry。如果没有提供 `id`，Home 会自动生成。如果没有提供 `secret` 或 `credential`，Home 会生成一次性 secret，并且只在本次响应中返回。

请求头：

```http
Authorization: Bearer user.jwt.token
```

示例请求：

```json
{
  "name": "Laptop"
}
```

字段：

| Field | Type | Required | 说明 |
| --- | --- | --- | --- |
| `id` | string | no | Passkey ID。Aliases: `passkey_id`, `passkey-id`。 |
| `name` | string | no | 显示名称。 |
| `secret` | string | no | 用于 hash 并存储的 passkey 登录 secret。Alias: `passkey_secret`。 |
| `credential` | object | no | 用于 passkey 登录比较的 opaque credential JSON。 |

示例响应：

```json
{
  "passkey": {
    "id": "pk_xxx",
    "name": "Laptop",
    "created_at": "2026-06-02T10:00:00Z"
  },
  "secret": "one-time-returned-secret"
}
```

常见错误：

```json
{ "error": "passkey_exists", "message": "passkey already exists" }
```

### POST `/passkey`

`POST /passkeys` 的 alias。

### DELETE `/passkeys`

删除当前认证用户的 passkey entry。

Query 参数：

| Query | Type | 说明 |
| --- | --- | --- |
| `id` | string | Passkey ID。 |

示例响应：

```json
{ "status": "ok" }
```

常见错误：

```json
{ "error": "not_found", "message": "passkey not found" }
{ "error": "invalid_body", "message": "passkey id is required" }
```

### DELETE `/passkey`

`DELETE /passkeys` 的 alias。

### DELETE `/passkeys/:id`

按 ID 删除当前认证用户的 passkey entry。

Path 参数：

| Parameter | Type | Required | 说明 |
| --- | --- | --- | --- |
| `id` | string | yes | Passkey ID。 |

响应：

```json
{ "status": "ok" }
```

### DELETE `/passkey/:id`

`DELETE /passkeys/:id` 的 alias。
