# Strict Credential Concurrency Limits Deployment

[中文](CN.md)

This runbook deploys Home-managed strict credential concurrency limits. Limits are enforced by the Home database policy and authoritative `admitted_in_flight` counter. Follow this runbook before activating any credential policy.

## Supported topology and prerequisites

- A SQLite deployment supports strict limits only with **one active Home**. Do not run multiple Home processes against the same SQLite database while any limit can be active.
- A deployment with multiple active Homes requires PostgreSQL. SQLite cluster mode is not a supported strict-limiting topology.
- Give every Home process and every CPA node its own mTLS certificate and private key. Do not copy a certificate between nodes or reuse an identity after replacing a node. Certificate identity is part of membership and capability attribution.
- Ensure the Management API is reachable on every live Home. The authenticated `GET /v0/management/capabilities` response must expose `capabilities.credential_concurrency_limits_v2: true` before policy activation.
- Use `GET /v0/management/topology` and the Home/CPA membership records to identify every live or stale incarnation. Do not treat a missing dashboard entry as evidence that an old process has stopped.

## Rollout

Perform these steps in order. A policy means either `max_in_flight` or `max_in_flight_by_model` is active.

1. **Remove policy activation.** Keep every credential policy unlimited: remove total and per-model limits, or leave the policy fields absent. Do not activate a limit during this stage.
2. **Upgrade Homes.** Upgrade and restart every Home against the target database. For SQLite, leave exactly one active Home. For PostgreSQL, complete the rollout for every active Home. Verify the old Home processes are stopped and their memberships/incarnations are no longer live. Each replacement Home must use its own certificate.
3. **Upgrade and verify CPAs.** Upgrade every CPA so it reloads Home's synthesized configuration. Verify every live CPA membership uses protocol version `1`. Query `GET /v0/management/capabilities` on **each** live Home and verify `credential_concurrency_limits_v2` is `true`. Resolve or remove every legacy CPA membership and every old or capability-missing Home incarnation; do not wait for them to recover naturally.
4. **Activate policies.** Only after all prior checks pass, use the Management policy API to set the intended limits. Start with a small policy change, verify the policy response and authoritative `admitted_in_flight` state, then continue the rollout.

## Mixed-version and failure handling

The deployment is fail-closed. An active policy is unsupported while an old Home, a Home without the concurrency capability, or a CPA membership with a protocol other than `1` remains live. Do not bypass the check by changing a policy directly in the database, sharing certificates, or allowing a legacy process to reconnect.

If an old Home or legacy CPA appears after activation, immediately remove it from service and remove/fence its live membership or incarnation before changing or continuing policies. If the topology is SQLite and more than one active Home is present, reduce it to one active Home before activation. If the deployment cannot meet these conditions, remove all limits and repeat the rollout from step 1.

Home synthesizes limiter configuration for CPA nodes. Local CPA limiter overrides do not change Home-authoritative policy enforcement.

## Historical-table maintenance

The runtime does not read the historical `in_flight_lease` table, and migrations never delete it automatically. Its removal is unrelated to policy activation.

Only a DBA may run the following statement after confirming that no prior deployment depends on the table, taking a backup, and completing the approved maintenance change:

```sql
DROP TABLE IF EXISTS in_flight_lease;
```

Do not put this statement in an application migration or ordinary deployment script.
