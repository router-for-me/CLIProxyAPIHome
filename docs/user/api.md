# CLIProxyAPIHome User API

This document describes the current DB-backed User API exposed by CLIProxyAPIHome. The User API is separate from the Management API and does not use the Management API secret key.

Base URL:

```text
http://<host>:<port>/user
```

Home examples usually use port `8327`. The effective listen address comes from runtime config, `cluster.yaml`, or the final `-addr` value.

## Runtime Model

User records, optional email state, one-time security tokens, mail jobs, API keys, TOTP settings, and passkey entries are stored in the database-backed cluster repository. The `/user/*` route group is registered only when Home is running with the database-backed runtime route set.

API key changes made through the User API update the same `api_key` table used by Home dispatch and publish a config refresh event.

## User Email Configuration

Verified email and password recovery are optional and disabled by default. A usable configuration enables all three capability flags returned by `GET /capabilities`.

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

Configuration rules:

- `public-user-url` must be an absolute HTTPS URL. Plain HTTP is accepted only for `localhost` or a loopback IP.
- `from-address` must be one mailbox without a display name.
- The first sender implementation is SMTP. Every non-loopback SMTP host must use `starttls: true`; Home requires TLS 1.2 or newer and does not support implicit TLS on port 465. If `username` is configured, `password-env` must name a non-empty environment variable; the password is not stored in `config.yaml`.
- `verification-token-ttl` and `reset-token-ttl` must be positive Go duration strings.
- Missing or invalid settings keep the email capability disabled without preventing Home from starting.
- `user-email` is Home-only configuration and is not forwarded to downstream CPA nodes.
- Home ignores forwarded client-IP headers by default. When an explicit reverse proxy fronts Home, list only that proxy's exact IP addresses or CIDRs in top-level `trusted-proxies`; trust-all networks are rejected, and changes require a restart. This keeps IP-based registration and recovery limits correct without allowing direct clients to spoof their address.
- Disabling the feature keeps existing email state in the database but stops new email changes, verification requests, and recovery requests. Previously issued verify/reset tokens can still be consumed until they expire or are invalidated.

## Authentication

Public routes:

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/capabilities` | Reports optional User API capabilities. |
| `POST` | `/register` | Creates a user and returns a bearer token. |
| `POST` | `/login` | Password login for users without passkey and without TOTP. |
| `POST` | `/login/totp` | Password plus TOTP login for users with TOTP enabled. |
| `POST` | `/login/passkey` | Passkey login. |
| `POST` | `/email/verify` | Consumes an email-verification token. |
| `POST` | `/password/forgot` | Accepts a generic password-recovery request. |
| `POST` | `/password/reset` | Consumes a reset token and sets a new password. |

All other `/user/*` routes require a bearer token returned by a successful register or login response.
The bearer token is an RS256 JWT signed with the cluster root CA private key and verified with the cluster root CA public key. Replacing the cluster root CA invalidates previously issued User API tokens.
Bearer tokens also carry the user's current session version. A successful password change, password reset, or Management API password update increments that version and invalidates older bearer tokens. Authenticated password changes return a replacement bearer session for the current client; password reset does not log the user in.
Password values are hashed and verified exactly as provided; leading and trailing whitespace is not trimmed. New bcrypt passwords are limited to 72 UTF-8 bytes.
User API JSON request bodies are limited to 1 MiB.

Supported request headers:

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <USER_TOKEN>` |

Login priority:

| State | Behavior |
| --- | --- |
| Passkey enabled with TOTP enabled | Plain password login returns `401 passkey_required`; `/login/passkey` and `/login/totp` are both accepted. |
| Passkey enabled without TOTP | Plain password login returns `401 passkey_required`; use `/login/passkey`. |
| TOTP enabled without passkey | Plain password login returns `401 totp_required`; use `/login/totp`. |
| No passkey and no TOTP | Plain password login returns a bearer token. |

Home also adds these response headers on User API routes:

| Header | Description |
| --- | --- |
| `x-cpa-home-version` | Home build version. |
| `x-cpa-home-commit` | Home build commit. |
| `x-cpa-home-build-date` | Home build date. |

## Response Conventions

Most successful delete or simple write operations return:

```json
{ "status": "ok" }
```

Successful login and register responses return:

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

User API handlers usually return both a machine-readable `error` code and a human-readable `message`:

```json
{ "error": "invalid_credentials", "message": "invalid credentials" }
```

Validation and authentication failures keep specific safe messages. Server-side `5xx` failures retain their machine-readable code but use the generic message `internal server error` so database and implementation details are not exposed publicly.

Common errors:

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_required", "message": "totp code is required" }
{ "error": "invalid_body", "message": "username is required" }
```

## Registered Routes

The table below is extracted from the User API route group registered by `internal/userapi/handler.go`.

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

## Account

### GET `/capabilities`

Reports whether the current Home configuration supports optional email registration, email verification, and password recovery. This route does not expose SMTP settings or user data.

Example response:

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

The three email flags are `false` when `user-email.enabled` is false or the mail configuration is incomplete or invalid. Older Home versions may return `404` because this route does not exist.

`model_catalog` is always `true` on a Home build that serves `/user/models`; it is absent on older builds. Clients should read it instead of calling the catalog route and treating a `404` as an outage.

### POST `/register`

Creates a user account, stores a bcrypt password hash, and returns a bearer token.

Example request:

```json
{
  "username": "alice",
  "password": "secret",
  "email": "alice@example.com"
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `username` | string | yes | Username. Aliases: `user_name`, `user-name`. |
| `password` | string | yes | Plaintext password used to create the stored password hash. |
| `email` | string | no | Optional email. Accepted only when `email_registration` is true. It is normalized and starts unverified; it does not reserve ownership until verification succeeds. |

Response: login response shape. When an email is supplied, Home applies verification-delivery limits and attempts to queue a message asynchronously; an already-owned address or a limited request is accepted without sending. Registration success does not mean the email is verified or ready for recovery.

All registration attempts are limited per source IP and globally, including username-only registration. Verification delivery has its own target-email limits. A limited response is `429 registration_rate_limited` with `Retry-After`.

Common errors:

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

Logs in with username and password when the user has no passkey and no TOTP.

Example request:

```json
{
  "username": "alice",
  "password": "secret"
}
```

Response: login response shape.

Common errors:

```json
{ "error": "invalid_credentials", "message": "invalid credentials" }
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_required", "message": "totp code is required" }
```

### POST `/login/totp`

Logs in with username, password, and TOTP code. This route is accepted when TOTP is enabled, even if the user also has passkeys. For passkey-only users, this route returns `401 passkey_required`.

Example request:

```json
{
  "username": "alice",
  "password": "secret",
  "totp_code": "123456"
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `username` | string | yes | Username. Aliases: `user_name`, `user-name`. |
| `password` | string | yes | User password. |
| `totp_code` | string | yes | TOTP code. Aliases: `totp-code`, `totp`, `code`. |

Response: login response shape.

Common errors:

```json
{ "error": "passkey_required", "message": "passkey is required" }
{ "error": "totp_not_enabled", "message": "totp is not enabled" }
{ "error": "invalid_totp", "message": "invalid totp code" }
```

### POST `/login/passkey`

Logs in with a passkey entry stored in `user.passkey`.

Example request:

```json
{
  "username": "alice",
  "passkey_id": "pk_xxx",
  "passkey_secret": "one-time-returned-secret"
}
```

The route can also compare an opaque JSON `credential` field if the passkey was created with a `credential` payload.

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `username` | string | yes | Username. Aliases: `user_name`, `user-name`. |
| `passkey_id` | string | yes | Passkey ID. Alias: `passkey-id`; `id` is also accepted. |
| `passkey_secret` | string | conditionally | Secret returned when the passkey was created. Alias: `secret`. |
| `credential` | object | conditionally | Opaque credential JSON used for exact comparison when no secret hash is stored. |

Response: login response shape.

Common errors:

```json
{ "error": "invalid_passkey", "message": "invalid passkey" }
{ "error": "invalid_body", "message": "username and passkey_id are required" }
```

### GET `/me`

Returns the authenticated user's profile from the bearer token.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example response:

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

Common errors:

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
```

### POST/PATCH `/password`

Changes the authenticated user's password.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example request:

```json
{
  "new_password": "new-secret"
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `new_password` | string | yes | New plaintext password. Alias: `new-password`. |

Response: login response shape with a new `token` and `expires_at`. The password update increments the user session version, so the bearer token used for this request and all other older bearer tokens become invalid. The client must replace its current session atomically with the returned session.

TOTP and passkey data are not changed.

## Email Verification and Password Recovery

Email addresses are optional. Home stores a normalized lowercase address, but User API responses expose only `email_status`:

Only a verified email is a unique ownership claim. Multiple active accounts may temporarily hold the same unverified address, so an unauthenticated registration cannot permanently squat somebody else's email and registration/update responses do not reveal whether another account already owns it. Home returns the normal accepted result but suppresses verification mail when another active account already owns the address. The first account that verifies an unclaimed address owns it atomically; later conflicting verification links receive the same generic invalid-or-expired response.

| Field | Type | Description |
| --- | --- | --- |
| `configured` | boolean | An email is currently stored. |
| `verified` | boolean | Ownership of the current email version has been verified. |
| `masked` | string | Privacy-safe display value such as `a***@example.com`; empty when no email is configured. |
| `recovery_ready` | boolean | The email is verified and the current mail configuration is usable. |

Changing the email immediately clears verification, increments the email version, invalidates outstanding verify/reset tokens, and supersedes pending mail jobs. Submitting the exact same normalized address is idempotent and preserves verification.

### PUT `/email`

Adds or replaces the authenticated user's email. Requires a bearer token and an enabled email capability.

```json
{ "email": "new@example.com" }
```

Success returns `200` with `{ "status": "ok", "user": ... }`. The returned user contains the new unverified `email_status`. This route stores the address but does not itself queue a message; call `/email/verification` next.

Common errors:

```json
{ "error": "invalid_email", "message": "email is invalid" }
{ "error": "email_feature_unavailable", "message": "email feature is unavailable" }
{ "error": "email_update_rate_limited", "message": "email update rate limit exceeded" }
```

### DELETE `/email`

Removes the authenticated user's stored email. This route remains available when the email capability is disabled so a user can always remove optional personal data. Removing an email increments the email version, invalidates outstanding verify/reset tokens, supersedes pending mail jobs, and releases the address for another active user.

Success returns `200 { "status": "ok", "user": ... }`. Repeating the request when no email is configured is idempotent.

### POST `/email/verification`

Queues a verification message for the authenticated user's current unverified email. No request body is required.

Success:

```http
HTTP/1.1 202 Accepted
```

```json
{ "status": "accepted" }
```

An already verified address returns an idempotent `200 { "status": "ok" }`. Registration's initial verification message and manual resends share the same limiter. Requests have a true 60-second cooldown and are additionally limited per user, target email, source IP, and globally. A limited manual resend returns `429 verification_rate_limited` with an exact `Retry-After` value.

```json
{ "error": "email_not_configured", "message": "email is not configured" }
{ "error": "verification_rate_limited", "message": "verification request rate limit exceeded" }
```

### POST `/email/verify`

Consumes a short-lived, single-use email-verification token. The emailed UI link should first show an explicit confirmation screen and only call this POST after the user confirms, so link scanners do not consume the token.

```json
{ "token": "base64url-token" }
```

Success: `200 { "status": "ok" }`.

Expired, used, unknown, superseded, wrong-email-version, or already-owned-address tokens all use the same response:

```json
{ "error": "invalid_or_expired_token", "message": "verification link is invalid or expired" }
```

### POST `/password/forgot`

Accepts an email address and, when an eligible verified account matches, queues a reset message. Once email capability is enabled, every parseable request receives the same status and body whether the address is missing, invalid, unknown, unverified, rate-limited, or temporarily unable to enqueue mail.

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

Home applies a five-minute target-email cooldown plus per-IP, per-email-hourly, and global limits before enqueueing recovery mail. It also pads accepted responses to a minimum processing duration to reduce timing differences between eligible and ineligible accounts. Clients must still show the same confirmation UI for every accepted response. When the global email capability is disabled, this route returns `404 email_feature_unavailable`.

### POST `/password/reset`

Consumes a short-lived, single-use reset token and sets a new password.

```json
{
  "token": "base64url-token",
  "new_password": "new-secret"
}
```

`new-password` is accepted as an alias. Success returns `200 { "status": "ok" }`, increments the session version, and invalidates all older User API bearer tokens, outstanding reset tokens, and queued reset-mail jobs. Any authenticated or Management API password update performs the same reset-token and queued-job revocation. Password reset does not return a login session. TOTP and passkeys remain unchanged and continue to apply on the next login.

Invalid, expired, used, superseded, or wrong-email-version tokens all return:

```json
{ "error": "invalid_or_expired_token", "message": "password reset link is invalid or expired" }
```

## TOTP

### GET `/totp`

Returns TOTP setup data for the authenticated user. If TOTP is already enabled and `regenerate` is not true, the existing secret is returned.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `issuer` | string | Optional issuer for the otpauth URL. |
| `regenerate` | boolean | Generate a new setup secret instead of returning the current secret. |

Example response:

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

POST alias of `GET /totp`. Accepts the same bearer token and optional JSON fields.

Example request:

```json
{
  "issuer": "CLIProxyAPIHome",
  "regenerate": true
}
```

Response: same shape as `GET /totp`.

### POST `/totp`

Verifies and stores a TOTP secret for the authenticated user.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example request:

```json
{
  "secret": "BASE32SECRET",
  "code": "123456"
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `secret` | string | yes | Base32 TOTP secret from `/totp`. |
| `code` | string | yes | Current TOTP code. Aliases: `totp_code`, `totp-code`, `totp`. |
| `issuer` | string | no | Issuer stored in TOTP metadata. Defaults to `CLIProxyAPIHome`. |

Response: same shape as `POST/PATCH /password`.

Common errors:

```json
{ "error": "invalid_totp", "message": "invalid totp code" }
{ "error": "invalid_body", "message": "secret and code are required" }
```

### POST `/totp/bind`

Alias of `POST /totp`.

### DELETE `/totp`

Deletes the authenticated user's TOTP configuration.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Request body: none.

Response: same shape as `POST/PATCH /password`; `user.totp_enabled` is `false` after a successful delete.

Common errors:

```json
{ "error": "bearer_token_required", "message": "bearer token is required" }
{ "error": "invalid_token", "message": "invalid token" }
```

## Model Catalog

Model catalog routes are under the `/user` base path, so the full paths are `/user/models` and `/user/models/accessible`. `/user/models` is unauthenticated; `/user/models/accessible` requires the bearer token returned by `/user/register` or `/user/login`. Neither route accepts or requires a Management Key.

The two routes answer different questions. `/user/models` answers "what can this cluster serve", which a visitor may ask before holding an account. `/user/models/accessible` answers "what can I call, at what price, and how has it been behaving", which is specific to the caller's API keys and commercial terms.

Neither response includes Management API data, credential identities, node identities, routing details, price rule identifiers, price rule sources, price rule notes, or any other user's data.

### Three-state reporting

Model metadata is reported as an explicit state rather than an empty value, because a model nobody described must not be presented as a model that lacks the capability:

| State | Meaning |
| --- | --- |
| `known` / `supported` | The upstream provider or cluster configuration stated this. |
| `unsupported` | The model published its parameter list and this capability was not in it. |
| `unknown` | Nothing has described this model. Render it as "not published", not as absence of the capability. |

The same rule governs price and availability:

- A model with no enabled price rule is `unpublished`. It is not free, and it is not priced at zero.
- A model with fewer observations than the window's minimum is `insufficient_data`. It is not 100% available.

### GET `/models`

Returns the public catalog: every model the cluster can currently serve.

No headers are required.

Example response:

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

Model fields:

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | The literal model identifier to send in an API request. Never translated or reformatted. |
| `display_name` | string | Optional human-readable name. Absent when the upstream never supplied one. |
| `description` | string | Optional short description supplied by the upstream. |
| `version` | string | Optional upstream version string. |
| `owned_by` | string | Optional upstream owner. |
| `type` | string | Optional model type, for example `chat`. |
| `providers[]` | array | Provider identifiers that can serve this model. These are the same identifiers used on usage records and price rules. |
| `context_length` | number | Maximum input tokens. Omitted when unknown; `context_length` and `inputTokenLimit` upstream spellings are normalized into this one field. |
| `max_output_tokens` | number | Maximum output tokens. Omitted when unknown; normalizes `max_completion_tokens` and `outputTokenLimit`. |
| `modalities` | object | See below. |
| `capabilities` | object | See below. |

`modalities`:

| Field | Type | Description |
| --- | --- | --- |
| `status` | string | `known` or `unknown`. |
| `input[]` | array | Accepted modalities, for example `text`, `image`, `audio`. Present only when `status` is `known`. |
| `output[]` | array | Produced modalities. Present only when `status` is `known`. |

The published vocabulary is `text`, `image`, `audio` and `video`. Document formats are not modalities and are not reported here: several providers list PDF as an accepted input type and the rest do not describe their inputs that way, so publishing it would compare providers on a distinction only some of them make.

Modalities come from the model catalog (`models.json`), which is curated by hand rather than probed from upstream. Values are taken from provider documentation, or from a first-party manifest where the provider publishes one — the Codex entries are filled from `codex_client_models.json`, which ships alongside the catalog and carries OpenAI's own `input_modalities`. A model the catalog does not describe reports `status: "unknown"` — never an empty `input` array, which a client would be entitled to read as "text only". Values are lower-cased and de-duplicated before they are returned.

Leaving a model undescribed is a normal outcome, not a gap to be filled in later with a guess. A handful of models whose vendor documents context length and speed but never input types are expected to report `unknown` indefinitely; that is the catalog working as intended, not a backlog item.

`capabilities`:

| Field | Type | Description |
| --- | --- | --- |
| `reasoning.status` | string | `supported`, `unsupported`, or `unknown`. |
| `reasoning.levels[]` | array | Optional reasoning effort levels the model accepts. |
| `reasoning.budget` | object | Optional thinking budget with `min`, `max`, `can_disable`, `dynamic`. Individual keys are omitted when the model did not state them. |
| `tool_calling.status` | string | `supported`, `unsupported`, or `unknown`. |
| `structured_output.status` | string | `supported`, `unsupported`, or `unknown`. Whether the model can be constrained to a caller-supplied schema. Resolved server-side from the parameter list; clients must not re-derive it. |
| `parameters[]` | array | Optional request parameters the model accepts. |
| `generation_methods[]` | array | Optional upstream generation methods. |

This route never includes `pricing` or `availability`. Their absence is the contract, not a missing value: an anonymous visitor is not entitled to the operator's commercial terms.

### GET `/models/accessible`

Returns the subset of the catalog the authenticated user's own API keys are allowed to call, with price and observed availability attached.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example response:

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

`access`:

| Field | Type | Description |
| --- | --- | --- |
| `restricted` | boolean | `true` when every one of the user's API keys is scoped to model groups. |
| `api_key_count` | number | How many API keys the user holds. |
| `reason` | string | `unrestricted`, `model_groups`, or `no_api_keys`. Lets a client explain an empty list instead of showing a bare zero state. |

Access is a union across the user's keys, not an intersection: holding one unscoped key makes the whole catalog accessible. Holding no key makes nothing accessible, because an account without a credential cannot call the cluster.

`pricing` is a ladder, not a scalar, because a charge resolves by service tier and then by input size:

| Field | Type | Description |
| --- | --- | --- |
| `status` | string | `published` or `unpublished`. |
| `providers[]` | array | One entry per provider that can both serve the model and has an enabled price rule for it. Rules naming a provider that cannot serve the model are excluded, because they are not offers. |
| `providers[].provider` | string | Provider identifier. |
| `providers[].tiers[]` | array | One entry per service tier. |
| `providers[].tiers[].service_tier` | string | Tier name, or `*` for the wildcard tier. |
| `providers[].tiers[].is_default` | boolean | Present and `true` on the `*` tier, which prices requests that name no tier or a tier nobody priced. |
| `providers[].tiers[].rungs[]` | array | Ascending by `min_input_tokens`. A request is priced by the last rung whose threshold it clears. |
| `rungs[].min_input_tokens` | number | Input token count at which this rung takes effect. |
| `rungs[].input_price_per_million` | number | Price per million input tokens. |
| `rungs[].output_price_per_million` | number | Price per million output tokens. |
| `rungs[].cache_read_price_per_million` | number | Price per million cache-read tokens. |
| `rungs[].cache_write_price_per_million` | number | Price per million cache-write tokens. |
| `rungs[].request_price` | number | Flat per-request price. |

A zero price component is emitted rather than omitted: a provider that does not bill for cache reads has stated that, and dropping the field would make it indistinguishable from a component nobody priced. A whole model with no enabled rule reports `"status": "unpublished"` and no `providers`.

Discount metadata is not returned in this version. Clients must not derive, infer, or display a discount from these fields.

`availability` summarizes observed behaviour over a rolling window:

| Field | Type | Description |
| --- | --- | --- |
| `status` | string | `observed` or `insufficient_data`. |
| `window.from` / `window.to` | string | RFC3339 bounds of the observation window, half-open `[from,to)`. |
| `window.hours` | number | Window length in hours. Currently `168`. |
| `window.min_samples` | number | Observations required before a rate is published. Currently `10`. |
| `sample_count` | number | Observations found in the window. Always present, including when it is `0`. |
| `success_count` / `failed_count` | number | Present only when `status` is `observed`. |
| `availability_rate` | number | Successes over samples, rounded to four decimals. **Present only when `status` is `observed`.** |
| `avg_latency_ms` | number | Mean end-to-end latency of measured successful requests. Absent when nothing was measurable. |
| `avg_ttft_ms` | number | Mean time to first token. Only streamed responses report it, so it can be absent while `avg_latency_ms` is present. |
| `output_tokens_per_second` | number | Total output tokens over total generation time, which weights long generations the way a caller experiences them. |
| `first_observed_at` / `last_observed_at` | string | RFC3339 bounds of the observations actually found, which can be much narrower than the window. `last_observed_at` may also appear on an `insufficient_data` model. |

When `status` is `insufficient_data`, no rate, latency, or throughput is published. Clients must render this as "not enough data", never as full availability. The summary is cluster-wide model health and is cached briefly in-process; it contains no per-user request data.

## Billing

User billing routes are under the `/user` base path, so the full paths are `/user/billing/overview` and `/user/billing/charges`. Both routes require the existing bearer token returned by `/user/register` or `/user/login`, and responses are scoped to the authenticated bearer user only.

User billing responses do not include admin notes, global totals, model price management data, proxy-pool data, raw API keys, masked API keys, price snapshots, matched price rules, endpoint, `balance_before`, or other users' data.

User billing `from` and `to` query parameters accept `YYYY-MM-DD`, RFC3339, or Unix seconds and use the half-open interval `[from,to)`. Unix-second values must be between `2000-01-01T00:00:00Z` and `9999-12-31T23:59:59Z`; millisecond timestamps are rejected. A date-only `to` becomes the next UTC midnight so the whole ending UTC day is included. Explicit timestamp `to` values are exact exclusive boundaries and are not expanded. Clients that need a full natural day outside UTC should send RFC3339 boundaries from local midnight to the next local midnight, for example `2026-06-10T00:00:00+08:00` through `2026-06-11T00:00:00+08:00`.

### GET `/billing/overview`

Returns the authenticated user's billing overview.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional exclusive end time. Date-only values include the full UTC day by using the next UTC midnight; explicit timestamps are preserved exactly. |

Example response:

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

Overview fields:

| Field | Type | Description |
| --- | --- | --- |
| `current_balance` | number | Current authenticated user balance. |
| `today_spend` | number | Spend value returned by the current billing overview query. |
| `month_spend` | number | Spend value returned by the current billing overview query. |
| `top_models[]` | array | Model spend entries with `id`, `label`, `amount`, and `request_count`. |

### GET `/billing/charges`

Lists the authenticated user's billing charges.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Query parameters:

| Parameter | Type | Description |
| --- | --- | --- |
| `from` | string | Optional start time: `YYYY-MM-DD`, RFC3339, or Unix seconds. |
| `to` | string | Optional exclusive end time. Date-only values include the full UTC day by using the next UTC midnight; explicit timestamps are preserved exactly. |
| `limit` | integer | Optional page size. Default `50`, max `200`. Invalid non-positive or non-integer values return `400`. |
| `offset` | integer | Optional page offset. Default `0`. Negative or non-integer values return `400`. |

Response shape:

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

Charge item fields:

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Charge record ID. |
| `created_at` | string | Charge creation time. |
| `provider` | string | Provider name. |
| `model` | string | Model name. |
| `input_tokens` | integer | Input tokens. |
| `output_tokens` | integer | Output tokens. |
| `amount` | number | Charged amount. |
| `balance_after` | number | Authenticated user's balance after the charge. |
| `request_id` | string | Request ID associated with the charge. |

Common errors:

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

API key routes operate only on API keys bound to the authenticated `user.id`.

### GET `/api-keys`

Lists API keys owned by the authenticated user.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example response:

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

Creates an API key owned by the authenticated user. If `api_key` is omitted, Home generates one.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example request:

```json
{
  "api_key": "client-key",
  "channels": [1],
  "model_groups": [2]
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `api_key` | string | no | Client API key. Aliases: `api-key`, `key`, `value`. |
| `channels` | array of integer | no | Channel group IDs. Empty or omitted means non-restrictive. |
| `model_groups` | array of integer | no | Model group IDs. Alias: `model-groups`; empty or omitted means non-restrictive. |

Example response:

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

Alias of `POST /api-keys`.

### PATCH `/api-keys`

Updates an API key owned by the authenticated user. The target can be selected by `id` or by API key value.

Example request:

```json
{
  "id": 1,
  "api_key": "new-client-key",
  "channels": [],
  "model_groups": []
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | integer | conditionally | API key record ID. |
| `api_key` | string | conditionally | New API key when `id` is provided; target API key when no `id` is provided. Aliases: `api-key`, `key`, `value`. |
| `old` | string | conditionally | Target API key value. |
| `new` | string | no | New API key value. |
| `new_api_key` | string | no | New API key value. Alias: `new-api-key`. |
| `channels` | array of integer | no | Replacement channel group IDs. |
| `model_groups` | array of integer | no | Replacement model group IDs. Alias: `model-groups`. |

Response: same shape as `POST /api-keys`.

Common errors:

```json
{ "error": "not_found", "message": "record not found" }
{ "error": "invalid_body", "message": "api key id or value is required" }
{ "error": "api_key_exists", "message": "api key already exists" }
```

### PATCH `/api-key`

Alias of `PATCH /api-keys`.

### PATCH `/api-keys/:id`

Updates an API key owned by the authenticated user by numeric record ID.

Path parameters:

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | integer | yes | `api_key.id`; must be greater than `0`. |

Response: same shape as `POST /api-keys`.

### PATCH `/api-key/:id`

Alias of `PATCH /api-keys/:id`.

### DELETE `/api-keys`

Deletes an API key owned by the authenticated user. The target can be selected by `id` or by API key value.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `id` | integer | API key record ID. |
| `api_key` | string | API key value. |
| `api-key` | string | Alias of `api_key`. |
| `key` | string | Alias of `api_key`. |
| `value` | string | Alias of `api_key`. |

Example response:

```json
{ "status": "ok" }
```

### DELETE `/api-key`

Alias of `DELETE /api-keys`.

### DELETE `/api-keys/:id`

Deletes an API key owned by the authenticated user by numeric record ID.

Path parameters:

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | integer | yes | `api_key.id`; must be greater than `0`. |

Response:

```json
{ "status": "ok" }
```

### DELETE `/api-key/:id`

Alias of `DELETE /api-keys/:id`.

## Passkeys

Passkey routes operate on entries stored in `user.passkey`.

### POST `/passkeys`

Creates a passkey entry for the authenticated user. If no `id` is provided, Home generates one. If no `secret` or `credential` is provided, Home generates a one-time secret and returns it only in this response.

Headers:

```http
Authorization: Bearer user.jwt.token
```

Example request:

```json
{
  "name": "Laptop"
}
```

Fields:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | string | no | Passkey ID. Aliases: `passkey_id`, `passkey-id`. |
| `name` | string | no | Display name. |
| `secret` | string | no | Secret to hash and store for passkey login. Alias: `passkey_secret`. |
| `credential` | object | no | Opaque credential JSON to store and compare during passkey login. |

Example response:

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

Common errors:

```json
{ "error": "passkey_exists", "message": "passkey already exists" }
```

### POST `/passkey`

Alias of `POST /passkeys`.

### DELETE `/passkeys`

Deletes a passkey entry for the authenticated user.

Query parameters:

| Query | Type | Description |
| --- | --- | --- |
| `id` | string | Passkey ID. |

Example response:

```json
{ "status": "ok" }
```

Common errors:

```json
{ "error": "not_found", "message": "passkey not found" }
{ "error": "invalid_body", "message": "passkey id is required" }
```

### DELETE `/passkey`

Alias of `DELETE /passkeys`.

### DELETE `/passkeys/:id`

Deletes a passkey entry for the authenticated user by ID.

Path parameters:

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | string | yes | Passkey ID. |

Response:

```json
{ "status": "ok" }
```

### DELETE `/passkey/:id`

Alias of `DELETE /passkeys/:id`.
