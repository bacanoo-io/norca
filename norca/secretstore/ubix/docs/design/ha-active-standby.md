# Design note: High availability — active/standby over shared storage

> **Status:** Accepted · 2026-09-24 · ADR: D-021 (supersedes the "`replicaCount`
> stays 1" clause of D-014). HA itself is not built yet; prerequisites are marked as they land.

## Goal

Run **more than one uBixVault replica** against the same MySQL/MariaDB database so
that planned maintenance — node drains, kernel patching, cluster upgrades, and our
own rolling upgrades — does not take the vault offline, and an unplanned pod or node
loss is covered by a replica that is already unsealed.

Exactly **one** replica is *active* and serves requests. The others are *standbys*:
unsealed, holding the barrier key, and ready to take over the moment the active
replica lets go of — or loses — the lock. This is HashiCorp Vault's HA model
(Vault Community, `ha_enabled` storage backends), and it keeps the wire API and
client behaviour Vault-compatible (D-003).

**Not a goal:** multi-active serving, performance standbys that answer reads, or
cross-cluster replication. Those are the Enterprise replication item on the
roadmap and would build on this, not replace it.

## Why now

Today's model (D-014) is "replaceable node over replicated storage": the database
survives node loss, but the vault process is a single pod. Every planned node drain
is a vault outage lasting a reschedule plus an unseal — and every restart costs an
unseal (see `CLAUDE.md`). For a system that sits in front of every other workload's
credentials, the operations teams who own the nodes must be able to take one
offline without scheduling around the vault. That is the requirement; D-014's
"sufficient HA" was sufficient for crash recovery, not for routine maintenance.

## What already makes this cheap

An inventory of in-memory state (2026-09-24) found that **almost nothing is cached**:
policies, tokens, identity, KV, transit keys, PKI, AppRole, userpass, cubbyhole and
wrapping all read storage on every request. There is no mount table (mounts are
static), no barrier keyring with rotating terms (one barrier key that never changes
while unsealed), and no in-memory lease heap. So a standby's view of the data can
never be stale in the ways that make Vault's takeover path heavy — "load state on
takeover" is mostly "reset four small caches".

The state that *is* held in memory, and what HA must do about each:

| State | Where | HA handling |
| --- | --- | --- |
| Barrier key | `barrier.Barrier.key` | Loaded at unseal on every replica. Never changes while unsealed; `Rekey` only re-wraps it. Nothing to do. |
| Unseal / generate-root / rekey progress | `core.Core.{progress,rootAttempt,rekey}` | Per-process by nature. Unseal is per replica (as in Vault). Generate-root and rekey are **forwarded to the active** so there is one attempt. |
| Quota specs + loaded flag | `quota.Manager` | Stale after another replica writes. **Reset on becoming active.** Rate buckets stay per-node (already documented as local, D-020). |
| DB plugin connection pool | `database.Engine.ready`, `mariadb.Plugin.db` | Never re-read once ready. **Reset (and close the old pool) on becoming active.** |
| TokenReview client | `kubeauth.Method.reviewer` | Never re-read once set. **Reset on becoming active.** |
| JWKS / OIDC discovery cache | `jwtauth.Method` | Keyed by URL, refetches on failure. **Reset on becoming active** for simplicity. |
| Audit HMAC key | `audit.FileDevice.hmacKey` | Was random per process; now kept in the barrier and loaded after unseal, so every replica HMACs a token the same way (prerequisite 4, done). |
| Per-client rate buckets, metrics | `ratelimit`, `metrics` | Per-node by design. Nothing to do. |

None of these four caches is reset on **seal** today either, which is a latent bug
on a single node too; the same reset hook fixes both.

## Why a single writer, when the data is read-through

Because read-through storage is not the same as safe concurrent writes. The MySQL
backend is plain `Get`/`Put`/`Delete` with no transactions or compare-and-swap, so
every read-modify-write in the codebase is last-writer-wins. With two serving
replicas:

- **Response wrapping** could be unwrapped twice (Get then Delete) — a single-use
  guarantee broken.
- **Transit `Rotate`** (load → increment → save) could lose a key version.
- **Identity `ResolveAlias`** could create two entities for one alias; its mutex is
  per process.
- **The lease sweeper** would run on every replica and race to `DROP USER`.
- **Lease-count quotas** (count, then create) would overshoot.

Active/standby makes all of these single-writer again without touching any engine.
Multi-active would require compare-and-swap in the storage interface and a pass over
every engine; that is the replication item, not this one.

## Architecture

### 1. An `HABackend` interface with fencing, in `internal/storage`

```go
// HABackend is a Backend that can also elect a single active writer.
type HABackend interface {
    Backend
    // Lock returns a handle for the named lock, identified to other replicas by
    // holderID and advertising advertiseAddr (the URL standbys forward to).
    Lock(name, holderID, advertiseAddr string) Lock
}

type Lock interface {
    // Acquire blocks until the lock is held or ctx ends. The returned channel is
    // closed when the lock is lost (renewal failed or was superseded).
    Acquire(ctx context.Context) (lost <-chan struct{}, err error)
    Release(ctx context.Context) error
    // Holder reports the current holder's ID and advertise address.
    Holder(ctx context.Context) (holderID, advertiseAddr string, err error)
}
```

**Core only sees this interface.** Nothing in `internal/core` knows the lock is a
MySQL row — which is what keeps integrated storage (Raft) possible later: Raft
leadership would implement the same interface, exactly as it does in Vault.

**Fencing lives behind the interface too.** Once a replica holds the lock, the
backend it hands to the barrier **refuses every write** unless that replica still
holds the current lock generation. That is the split-brain guard: a replica that is
paused (GC, CPU starvation, network partition) past its lock TTL, and resumes still
believing it is active, cannot write — its writes fail with `storage.ErrFenced`
(surfaced as a 503, and it steps down). Timing assumptions decide *when* a replica
steps down; fencing makes a wrong timing assumption harmless instead of silently
corrupting data.

### 2. The MySQL implementation

A new table, created by `ensureSchema` alongside `ubixvault_kv` (schema version 2):

```sql
CREATE TABLE IF NOT EXISTS ubixvault_lock (
  name        VARBINARY(64)   NOT NULL PRIMARY KEY,
  holder_id   VARBINARY(255)  NOT NULL,   -- '' when nobody holds it
  advertise   VARBINARY(1024) NOT NULL,
  generation  BIGINT UNSIGNED NOT NULL,   -- the fencing token
  expires_at  DATETIME(6)     NOT NULL
) ENGINE=InnoDB;
```

- **Clock:** every expiry comparison uses the **database's** `NOW(6)`, never a
  replica's wall clock, so clock skew between nodes cannot hand the lock to two
  holders.
- **Acquire:** `UPDATE … SET holder_id=?, advertise=?, generation=generation+1,
  expires_at=NOW(6)+INTERVAL ? MICROSECOND WHERE name=? AND expires_at < NOW(6)`
  (plus an `INSERT IGNORE` of the row on first use). One affected row = acquired;
  the new generation is read back in the same transaction. Standbys retry every
  `ha_retry_interval` (default 500ms).
- **Renew:** `UPDATE … SET expires_at=… WHERE name=? AND holder_id=? AND
  generation=?` every `ha_lock_ttl/3`. Zero rows = superseded → lost. If no renewal
  has *succeeded* within `ha_lock_ttl − margin` measured on the replica's monotonic
  clock, the replica steps down on its own without waiting for the database.
- **Release:** on graceful shutdown (`SIGTERM`, i.e. every drain and rolling
  upgrade), clear `holder_id` and set `expires_at` to the past for our
  generation. Clearing the holder is what fences the releasing replica's own
  later writes, since the generation does not change until someone acquires. A standby picks the lock
  up on its next retry — the planned-maintenance handoff is **~one retry interval**,
  not a TTL.
- **Fenced writes:** each `Put`/`Delete` from the active replica runs in a
  transaction that first does `SELECT generation, holder_id FROM ubixvault_lock
  WHERE name=? LOCK IN SHARE MODE` (MySQL 5.7+ and MariaDB; `FOR SHARE` is MySQL-8-only) and aborts with `ErrFenced` on a mismatch. The shared row
  lock makes an acquirer's generation bump wait for in-flight writes to commit, and
  every write after the bump fails — so the old and new active can never interleave
  writes. Writes are low-volume (reads dominate a secrets manager), so one extra
  indexed read per write is an acceptable cost; it is measured in slice 5.
- **Defaults:** `ha_lock_ttl` 15s, renew every 5s, retry every 500ms (was 2s
  in the first draft; a standby's retry is a handful of cheap statements, and
  it bounds the planned-handoff gap). Unplanned failover (crash, node loss) is
  therefore ≤ ~16s; planned handoff ≤ ~0.5s, and standbys hold requests across
  it rather than failing them (§4).

The existing `RetryBackend` keeps wrapping the MySQL backend (D-019 transient-error
classification is unchanged). `ErrFenced` is **permanent**, never retried.

The file backend does not implement `HABackend`. HA requires `-storage mysql`; with
file storage the server behaves exactly as today.

### 3. Core: active and standby

New states alongside sealed/unsealed: **standby** and **active**.

1. Every replica starts, opens storage, and unseals (Shamir shares per replica, or
   auto-unseal). Unsealing does **not** make a replica serve.
2. An unsealed replica runs `Lock.Acquire` in a loop. While waiting it is a standby.
3. On acquiring, `becomeActive`: reset the four caches above, start the lease
   sweeper, mark active. Only now do non-`sys` requests get handled locally.
4. On `lost` closing, on `sys/step-down`, on seal, or on shutdown, `stepDown`: mark
   standby, stop the sweeper, release the lock if still held, and return to step 2
   (or stop, on seal/shutdown). A step-down caused by lost lock or `ErrFenced` is
   logged at WARNING and counted in metrics.

Background writes (today: only the lease sweeper, `RunLeaseSweeper`) run on the
active replica only. Anything added later that writes on a timer must hook the same
start/stop.

**Initialize** requires the lock: on an uninitialized HA cluster, the replica that
receives `sys/init` acquires the lock first, so two replicas cannot initialize the
same database concurrently.

### 4. Requests on a standby: forward over the cluster listener

A standby **forwards** every request it does not answer itself to the active
replica and returns the active's response unchanged. Clients — ESO, the PHP
resolver, `curl`, the console — need no changes and no redirect handling, and a
plain Kubernetes Service over all ready pods works.

Answered locally by a standby: `sys/health`, `sys/livez`, `sys/seal-status`,
`sys/leader`, `sys/unseal`, `sys/seal`, `sys/init`, `sys/metrics`, `/ui` assets.
Everything else is forwarded, including `sys/step-down`, `sys/rekey/*` and
`sys/generate-root/*`, so each lands where it can act and there is one attempt,
held by the active.

**Transport — a dedicated cluster listener with mutual TLS** (owner decision,
2026-09-24; implemented in `internal/cluster`). Replicas do not forward over the
API listener: the API's certificate is the operator's (in production a
`*.ubixsys.com` wildcard), which cannot verify a pod address. Instead, as in
HashiCorp Vault, each replica runs a second listener (`-ha-cluster-listen`,
default port 8201) that speaks only TLS 1.3 and requires a client certificate:

- The **active replica creates a cluster CA** (ECDSA P-256, 10 years) in the
  barrier at `sys/ha/cluster-ca` on becoming active, retrying in the background
  if storage fails, before it serves as active.
- **Every unsealed replica issues itself a 24-hour leaf** from that CA, held in
  memory only, reissued at half-life, dropped on seal. All leaves carry one fixed
  name; replicas verify each other by CA, not by address.
- So **only a process that unsealed the same vault can connect** — no operator
  certificates, no cert-manager, no shared secret to distribute.

The rest follows from that authentication:

- **Client address:** the standby sends the original client address in a header
  the active honours only on the cluster listener (a client sending it to the
  API is ignored, and the standby overwrites it). `X-Forwarded-For` is passed
  through unchanged, so `-rate-limit-trust-forwarded` behaves the same on every
  replica.
- **Audit and quotas** run on the active, under the original client's address.
  The standby neither audits nor rate-limits a forwarded request.
- **Handoffs without errors:** while no replica is active (a planned handoff
  takes up to one retry interval) the standby holds the request for up to 5s
  rather than answering 503; if the replica it waited on turns out to be itself,
  it serves the request locally once active. A request without a body is also
  re-sent when the leader it reached has just gone (connection refused, or a
  "no longer active" answer from the old leader's cluster listener). A request
  with a body is never sent twice. In testing, a client pinned to one replica
  through a step-down and a SIGTERM of the new active saw no failed requests.
- **No loops:** a forwarded request reaching a replica that is not active is
  refused (503 with a marker header), never forwarded again.
- **Unplanned failover** (the active dies without releasing the lock) still
  costs up to the lock TTL; held requests time out after 5s with 503, which
  clients retry.

Redirect (`307` to the active, Vault's legacy standby behaviour) was considered
and rejected: it exposes pod addresses to clients and `curl`/PHP do not follow it
by default.

### 5. API surface (Vault-compatible)

- **`GET /v1/sys/health`** gains Vault's query parameters: `standbyok`,
  `standbycode` (default **429**), `activecode` (200), `sealedcode` (503),
  `uninitcode` (501), `perfstandbyok` (accepted, no-op). Body gains `standby`.
  Behaviour without parameters is unchanged for a single node.
- **`GET /v1/sys/leader`** — unauthenticated: `ha_enabled`, `is_self`,
  `leader_address`, `leader_cluster_address`, `active_time` (on the active).
- **`PUT /v1/sys/step-down`** — authenticated (ACL on `sys/step-down`); the
  active releases the lock and becomes a standby (for moving the active off a
  specific pod by hand). It then stays out of the election for 10s so another
  replica takes over — ending early if the lock stays free for two retry
  intervals, so a step-down can never leave the vault with no active replica.
- **`/v1/sys/livez`** stays storage-independent and lock-independent. A standby is
  alive.

### 6. Server flags

`-ha` (enable; requires `-storage mysql`), `-ha-advertise-addr` (this replica's
API URL, default derived from `POD_IP`/hostname), `-ha-cluster-listen` (default
`-listen`'s host, port 8201), `-ha-cluster-addr` (URL other replicas forward to,
default `https://$POD_IP:8201`), `-ha-lock-ttl`, `-ha-retry-interval`.

### 7. Helm chart

- `ha.enabled` (requires `storage.type: mysql`). With it, the `replicaCount > 1`
  guard is lifted; without it, it stays.
- Readiness probe → `/v1/sys/health?standbyok=true`, so unsealed standbys are ready
  and a StatefulSet rolling update can proceed pod by pod. The main Service keeps
  selecting all pods; standbys forward.
- No new Service: replicas advertise their pod IP (`POD_IP` via the downward
  API) and reach each other directly on the cluster port 8201, so neither DNS
  nor certificates change (the cluster listener brings its own identity, §4).
  (A headless Service was the first plan; it was dropped because a StatefulSet's
  `serviceName` — like `podManagementPolicy` — cannot change on an existing
  release, and pod IPs make it unnecessary.)
- `PodDisruptionBudget` with `maxUnavailable: 1`, and default pod anti-affinity
  (preferred) across nodes — a drain can never take two replicas at once.
- `terminationGracePeriodSeconds` long enough for a clean `Release`.
- Recommended default for HA: `replicaCount: 3` (survives one planned drain plus one
  unplanned loss); `2` is valid.

## Prerequisites (independent of HA, fix first)

1. **Done (MR !11).** **Response-wrapping unwrap is not single-use under concurrency — even on one
   node today.** `wrapping.Unwrap` is Get → Delete with no lock
   (`internal/wrapping/wrapping.go:109-127`); two concurrent unwraps of the same
   token can both return the payload. Serialize it in-process (a mutex, or a
   delete-that-reports-existence on the backend). Security fix, separate `fix/` MR.
2. **Done (MR !12).** **Auto-unseal is a single attempt at startup** (`cmd/ubixvault/main.go:175-184`).
   If the KMS or storage is briefly unreachable, the process stays sealed until
   restarted. A standby that silently stays sealed is no standby. Retry with
   backoff (consistent with D-019).
3. **Done (`fix/seal-reset-audit-hmac`).** **Reset caches on seal** (the four above), so a seal/unseal cycle in one process
   does not keep stale config. HA reuses the same hook.
4. **Done (`fix/seal-reset-audit-hmac`).** **Persist the audit HMAC key** in the barrier, so a token's HMAC is the same on
   every replica and across restarts — otherwise audit logs cannot be correlated
   after a failover.

## Delivery plan (one MR each, in order)

0. This design note + ADR D-021.
1. Prerequisites 1–4.
2. **Done (`feat/ha-lock`).** `HABackend` + MySQL lock + fencing, with conformance tests and MySQL
   integration tests that simulate a paused holder and a partition. No runtime
   change (nothing uses it yet).
3. **Done (`feat/ha-core`).** Core active/standby state machine, `becomeActive`/`stepDown`, sweeper gating,
   `-ha` flags.
4. **Done (`feat/ha-forward`).** Standby forwarding over the mutual-TLS cluster listener, `sys/leader`, `sys/step-down` (the `sys/health` parameters shipped with slice 3).
5. **Done (`feat/ha-chart`).** Chart (`ha.enabled`, PDB, soft/hard anti-affinity, `POD_IP`, cluster port, `standbyok` readiness, grace period) and the rolling-upgrade / enabling-HA procedure in `docs/DEPLOYMENT.md` § Upgrades.
6. **Done (`feat/ha-e2e`).** Failover test on kind: kill the active, drain its node, rolling upgrade under
   load — measure the gap, assert no write lands from a fenced replica.
7. Release (MINOR — additive API and flags), README/POSITIONING/DEPLOYMENT, then
   `ubixsys-web`. Roll out to `ubixvault-dev` at 3 replicas before prod (a
   `kubernetes` repo MR).

## Future: integrated storage (Raft)

Not now. Raft brings a local storage engine per replica, snapshots and log
compaction, membership changes, quorum-loss recovery, peer TLS and bootstrap, and
either several new dependencies (`hashicorp/raft`, `raft-boltdb`, bbolt…) or an
in-house consensus implementation. It is only worth doing to remove the external
database, and MySQL is already run as an HA service here.

It stays possible because the parts built here are storage-agnostic: standby mode,
takeover, forwarding, health codes and the chart all carry over. Raft would
implement `HABackend` (leadership = lock, term = fencing generation) and replace
the MySQL `Backend`; migration is a data copy, which snapshot/restore mostly
covers already.

## Open questions

- ~~**Forwarding credential**~~ — resolved: the cluster listener's mutual TLS
  (§4) authenticates replicas, so no separate credential exists.
- **Standby reads:** the data model would allow standbys to answer reads locally
  (performance standbys). Deliberately out of scope; revisit with the replication
  item.
- **Shamir operations across replicas:** with Shamir seal, operators must unseal
  every replica. Acceptable (Vault does the same); auto-unseal is the recommended
  mode for HA and is what both deployed instances use.
