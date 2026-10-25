# Design note: Resource Quotas (rate-limit + lease-count)

> **Status:** Phases 1 (rate-limit) and 2 (lease-count) **implemented** · 2026-09-17
> · first slice of the Vault-**Enterprise**-parity push (`docs/ROADMAP.md`). ADR: D-020.

## Goal

Give operators **path-scoped, API-managed limits** on two resources a single-org
vault can exhaust or have abused:

1. **Rate-limit quotas** — cap requests/second under an API **path prefix** (Vault:
   `sys/quotas/rate-limit`).
2. **Lease-count quotas** — cap the number of **active leases** (dynamic DB creds,
   tokens, PKI, etc.) under a path prefix (Vault: `sys/quotas/lease-count`).

This is the first Enterprise-parity feature because it is **self-contained** — it
extends machinery that already exists (`internal/ratelimit` and the lease/expiration
subsystem) rather than touching the barrier or the request path shape — and it
establishes the cadence (design note → ADR → small reviewed slices) for the larger
Enterprise items that follow (namespaces, ABAC, replication…).

## Why (and why now)

Today rate limiting is a single **global**, flag-configured token bucket
(`--rate-limit`, `internal/ratelimit`): one knob for the whole process, not
manageable at runtime, not scoped. There is **no** cap on active leases at all — a
misbehaving client can open dynamic DB creds without bound until the backing DB or
the vault's own storage is stressed. Both are real single-org failure modes and
both are Vault features clients/operators expect. Quotas are also a prerequisite
shaped by **namespaces** later (per-namespace quotas), so getting the model right
now pays forward.

## Model

A **quota** is a small stored config object, Vault-compatible:

| Field | Meaning |
| --- | --- |
| `name` | unique within its type |
| `type` | `rate-limit` \| `lease-count` |
| `path` | API path **prefix** it applies to (`""` = global) |
| `rate` / `burst` | rate-limit only: requests/sec + burst |
| `max_leases` | lease-count only: cap on active leases |
| `role` / `auth_mount` | optional finer scope (later phase) |
| `block_interval` | rate-limit only: optional penalty window after a violation |

**Matching:** longest-prefix wins; a request/lease is checked against the single
most-specific matching quota (Vault semantics), plus the global default if set. A
`sys/quotas/config` object holds defaults (e.g. a default rate-limit, and
`enable_rate_limit_response_headers`).

**Storage:** quotas live under a `sys/quotas/…` barrier path (ciphertext like
everything else), loaded into an in-memory manager at unseal and refreshed on each
API write. No new dependency — same pattern as policies and mounts.

## Enforcement points

- **Rate-limit quotas** — evaluated in the HTTP middleware, **before** auth (as
  Vault does: an unauthenticated flood must be shed cheaply), after resolving the
  request path. Generalize `ratelimit.Limiter` so a key is `(quotaName, clientID)`
  and the manager picks the limiter for the matched quota; the existing global
  flag becomes the built-in default quota. Over-limit → **HTTP 429** with
  `Retry-After` and (opt-in) `X-RateLimit-*` headers.
- **Lease-count quotas** — checked at **lease creation** (dynamic secrets, token
  creation) in the lease/expiration subsystem: the manager keeps a live count per
  matched quota; at/over `max_leases` the create is refused with a `429`-class
  error and a `Quota` event, and the count is decremented on revoke/expiry. The
  count is rebuilt from the lease store at unseal so it survives restarts.

## API surface (Vault-compatible)

- `sys/quotas/config` — GET/POST global defaults.
- `sys/quotas/rate-limit` — LIST; `sys/quotas/rate-limit/:name` — GET/POST/DELETE.
- `sys/quotas/lease-count` — LIST; `sys/quotas/lease-count/:name` — GET/POST/DELETE.

All are **sys** endpoints, root/`sudo`-gated by ACL (default-deny already applies).
Existing Vault CLI/tooling (`vault write sys/quotas/rate-limit/...`) works unchanged
per the API-compatibility ADR (D-003).

## Observability

- Counter `ubixvault_quota_exceeded_total{name,type}` (mirrors Vault's
  `quota.rate_limit.violation` / `quota.lease_count.violation`).
- Gauge `ubixvault_lease_count{quota}` for lease-count quotas.
- A `Quota` audit/event line on each rejection, so a client hitting a wall is
  visible rather than inferred.

## Phasing (small reviewed slices)

1. **Rate-limit quotas** — ✅ **shipped, complete.** Quota manager (`internal/quota`)
   + barrier storage + `sys/quotas/rate-limit*` API (`internal/api/quota.go`) +
   middleware enforcement (`internal/api/audit.go`, longest-prefix match, 429 +
   `Retry-After`). The global `-rate-limit` flag is now **folded in as the default
   quota** (via the manager), overridable at runtime by **`sys/quotas/config`**
   (`default_rate`/`default_burst`); the default applies even while sealed (so
   init/unseal can't be brute-forced), named quotas take effect after unseal. A
   named quota is the most-specific match and governs its path alone. Denials
   increment **`ubixvault_quota_exceeded_total{quota}`** (`"default"` for the
   default) and surface as 429s in the request-status metric + audit log.
2. **Lease-count quotas** — ✅ **shipped.** `sys/quotas/lease-count/*` CRUD
   (barrier-persisted, loaded at unseal) + longest-prefix `MatchLeaseCount`.
   Enforced at DB-credential issuance (`dbCredentials`): the matched quota's path
   is counted live via `database.CountLeasesUnder` (leases mapped to their issuing
   path `database/creds/<role>`) and a new lease is refused with `429` at the cap,
   before any credential is created. Counting is on-demand (correct, no drift); a
   cached counter is a later optimization. Currently covers DB leases — the only
   lease type today. Denials increment `ubixvault_quota_exceeded_total{quota}`.
3. **Finer scope** — `role` / `auth_mount` scoping and `block_interval` penalty.

Each phase is independently shippable and CI-green.

## Non-goals / deferred

- **Per-namespace quotas** — folds in when namespaces land (the quota's scope gains
  a namespace segment; the manager is already prefix-based, so this is additive).
- **Distributed rate limiting** across replicas — the single-node/SQL-backed model
  counts locally; a shared counter is only meaningful once replication exists.

## Decision

Adopt Vault-compatible resource quotas, built in-house on the existing rate limiter
and lease subsystem with **no new dependency**, phased rate-limit-first. Recorded as
**ADR D-020**.
