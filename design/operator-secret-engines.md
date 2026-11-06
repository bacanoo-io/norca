<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Operator Secret Engines

## 1. Summary and Decisions

Dynamic secrets today have one built-in backend, NATS, compiled into the
servers (`norca/secretengine`). Every new kind of credential would need a Norca
change and its format in HCL. This design replaces the built-in backends with
one generic backend, `type = "operator"`: an operator is a service that creates
credentials on request. The lease machinery stays; only the part that talks to
the target system moves out.

Decisions (2026-11-01):

1. **Lease engine stays in Norca.** Lease, renewal, `max_ttl`, revocation, the
   lease watcher, workload identity and audit are unchanged.
2. **Credentials are not custom objects.** An object `status` is stored in Raft,
   in the backups and in the event log. Credentials pass through the leader in
   memory and reach the client in the RPC reply, as they do today.
3. **The answer is a named item list,** with no schema per service: the job maps
   items with the existing `file` and `env` blocks.
4. **The leader reaches the operator like a type-bound webhook:** a service of
   the catalog, called over mTLS with a workload certificate.
5. **The operator keeps its own admin credential** as an ordinary secret, read
   through its job's `secret` block. Norca never handles it.
6. **The built-in NATS backend is removed,** with `rotate-root`, the
   `credential_path` of engines and the default file of a credential. Nothing
   used it; a NATS operator is written when something needs it. The system bus
   and the Consul KV `nats` backend are not related.
7. **One lease per task, regional engines, 10 concurrent calls per engine by
   default** (`operator_max_concurrent` in the server configuration; see
   section 10).
8. **`fallback`** names a static secret used only when a task starts and the
   operator can't issue. The static secret is left untouched; rotating one
   shared credential is a different model, covered by rotation jobs.

## 2. Job specification

```hcl
secret "db" {
  engine   = "postgres"
  role     = "app"
  fallback = "app/db-static"

  file {
    destination = "secrets/db.env"
    format      = "env"
  }
  env {
    DB_USER     = "username"
    DB_PASSWORD = "password"
  }
}
```

A certificate adds `params` and one `file` block per item; the existing
`raw:<item>` format selects the item, and secret files are always written with
mode 0400:

```hcl
secret "tls" {
  engine = "internal-pki"
  role   = "api-client"
  params = { common_name = "api.apps.internal" }

  file {
    destination = "secrets/tls.crt"
    format      = "raw:cert"
  }
  file {
    destination = "secrets/tls.key"
    format      = "raw:key"
  }

  change_mode   = "signal"
  change_signal = "SIGHUP"
}
```

New in the `secret` block: `fallback`, `fallback_recover`,
`fallback_keep_on_restart` and `params`; `file`
can be repeated. The params are read from the job on the servers, not from the
request of the client.

## 3. Engine object

```hcl
name = "postgres"
type = "operator"

operator {
  service   = "postgres-operator"   # in the namespace of the engine
  namespace = "platform"            # optional
}

role "app" {
  ttl     = "1h"
  max_ttl = "24h"
  config  = { database = "app", grants = "readwrite" }   # opaque to Norca
  params  = ["common_name"]                              # allowed request keys
}
```

`config` is passed to the operator unchanged. `params` lists the keys a job may
send; any other key is refused when the lease is requested, and the operator
validates the values against the role. A task can't ask for an arbitrary `common_name`.

## 4. Operator contract

JSON over HTTPS. The leader authenticates with the server certificate of the
region; the operator with its workload certificate, whose SPIFFE ID must match
`operator.service`.

| Call | Request | Answer |
| --- | --- | --- |
| `POST /issue` | engine, role, role config, params, lease ID, task identity, `max_expire`, optional `csr` or `public_key` | `items`, `ref`, `expires`, `renewable` |
| `POST /renew` | `ref`, lease ID, ttl | `expires`, optional new `items` |
| `POST /revoke` | namespace, engine, `revoked`: every revoked credential that has not expired, `ref` to revocation time | status |
| `POST /health` | | status, supported features |

```json
{
  "items": { "username": "v-app-7f3", "password": "..." },
  "ref": "v-app-7f3",
  "expires": "2026-11-01T18:00:00Z",
  "renewable": true
}
```

- `items` values are strings or base64 (`{"b64": "..."}`) for binary content.
  The total size is bounded (64 KiB): every answer goes through the leader.
- `ref` is not secret and is the only part stored, as `BackendRef` of the lease.
- `renew` may return new `items`. That is how certificates and cloud sessions
  renew: the client rewrites the files and applies `change_mode`.
- `revoke` is one call with the whole list, the `Revoked` of the existing
  `Backend.Sync`: it is sent on each change and periodically until the
  credentials expire, so it must be idempotent and accept unknown references.
  A lease whose issue did not finish is listed by its lease ID.
- `lease ID` makes `issue` idempotent for a retried request.
- Operators hold no state Norca needs: after a failure the leases in Raft are
  the truth.

Private keys: with `csr` or `public_key` the key is created on the client and
never leaves the node. A role can require it (`require_csr = true`).

## 5. Flow

```
task start -> client: SecretLease.Create (workload identity)
           -> leader: ACL, role, params
           -> leader: operator_max_concurrent slot, no engine lock
           -> leader: Raft: lease, BackendRef = lease ID
           -> operator: POST /issue
           -> leader: Raft: BackendRef, expiry
           -> client: items, written to secrets/ and env
task stop  -> client: SecretLease.Revoke
           -> leader: Raft: revoked -> operator: POST /revoke (retried)
```

The lease is written before the issue, so a credential created during a leader
change can still be revoked by the new leader (issue is idempotent by lease ID).
This is a change from the NATS backend, where issuing needs no outside call.

`Backend.Sync` stays and carries the revocation list. The admin key is the
operator's business. `norca secret engine
rotate-root` is removed; an operator rotates its admin credential through the
normal `change_mode` of its secret.

## 6. Failures

| Situation | Behaviour |
| --- | --- |
| `issue` fails at task start, with `fallback` | The task starts with the static secret; no lease; task event `provider fallback`, audit record, metric `nomad.secrets.engine.fallback` |
| `issue` fails, no `fallback` | The task waits and retries with a backoff, as for a missing secret |
| `renew` fails on a running task | Retry until the lease ends, never switch to the fallback. A short outage must not move every task to the shared credential |
| Lease expires without renewal | `change_mode` of the block applies: restart by default |
| `revoke` fails | The watcher retries; `max_ttl` is the upper bound |
| The operator is back, a task runs on the fallback | At its next restart it asks for a credential, and starts on the fallback again if that fails. `fallback_keep_on_restart = true` keeps it on the fallback, also across a client restart. With `fallback_recover = "signal"` it asks every `fallback_recover_interval` (default 10s) while it runs and applies `change_mode` |

The fallback secret stays valid all the time, so it should have fewer rights
than the dynamic ones. The operator status page and `norca secret lease` show
the tasks running on a fallback.

## 7. Admin credentials

The operator job reads its admin credential as an ordinary secret:

```hcl
secret "admin" {
  path = "operators/postgres/admin"
  env { PGADMIN_PASSWORD = "password" }
  change_mode = "signal"
}
```

Reads are audited, backups include it with the secret store, and one operator
has one admin secret. A compromised operator exposes that credential, not the
jobs or the servers.

## 8. Backups

The engine and the leases stay in Raft. A selective restore doesn't touch them.
After a full restore a lease issued after the backup is lost: the preview warns,
and the credential stays valid until it expires. Revocations that the watcher
sent after the backup are not undone at the operator; a restored lease whose
allocation is gone is revoked again, which `revoke` must accept.

## 9. Plan

1. Contract: `operator` engine type, `secretengine.Operator` client, mock
   operator in tests, the `Create` flow of section 5.
2. Secret block: `fallback`, `params`, `item`, `perms`, renewal with new items.
3. SDK: `norca/controller/secrets`, an HTTP server for the contract.
4. Remove the built-in NATS backend and `rotate-root` (done).
5. `postgres-operator` as the first real user of the contract (done:
   `operators/postgres` in norca-registry, tested on PostgreSQL 14 and 17, no
   pack); a certificate operator for the `csr` path.
6. Documentation, web UI (engine status, leases on a fallback).

## 10. Limits and scope

- **One lease per task.** A lease belongs to a task, also for a task group with
  several tasks, and `/issue` carries the task identity. Credentials are not
  shared between the tasks of an allocation.
- **Concurrent calls.** `server { secrets { operator_max_concurrent = 10 } }`
  bounds the calls in flight to the operator of one engine, per leader
  (default 10). Further requests wait, within the timeout of the lease request,
  so a job starting many instances doesn't overrun the operator.
- **Timeout.** `server { secrets { operator_timeout = "10s" } }` bounds a request
  to an operator, with the wait for its turn. A task waits for it when it starts.
- **Regional.** An engine and its operator belong to one region, like the
  leases: the database of a region is independent of the database of another
  one. A job in another region uses the engine of that region.
