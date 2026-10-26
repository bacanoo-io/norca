# Design note: SQL storage backend

> **Status:** Proposed · 2026-08-06 · relates to ADR [D-014](../DECISIONS.md) and
> `docs/ROADMAP.md` Tier 1.

## Problem

uBixVault persists everything through a `storage.Backend` — a flat key/value
blob store over `/`-separated paths (`internal/storage/storage.go`). Today the
only production backend is `FileBackend`: a single directory on one node's local
disk. That is the project's architectural ceiling for real use:

- **Durability** rests on one disk. If it is lost, so is the vault (short of a
  recent snapshot).
- **The node is not replaceable.** Rescheduling the pod means the data has to
  move with it (a `ReadWriteOnce` PVC pinned to a node), or it is gone.

The goal of this note is a backend that turns *"single node with a local disk"*
into *"replaceable node over managed, replicated storage"*: the vault process
can die and restart — on another node — pointing at the same durable database,
and the database's own replication/HA handles durability. For most single-org
production this is *sufficient* availability; true multi-writer HA is a separate,
later step (see [Non-goals](#non-goals)).

## Why SQL (MySQL/MariaDB), and why now

- **No new dependency.** The project already depends on exactly one third-party
  library, `github.com/go-sql-driver/mysql` (D-010), for the dynamic-database
  secrets engine. A MySQL/MariaDB storage backend reuses it. Postgres would add a
  second driver; Consul/etcd/Raft add heavy dependencies and operational surface.
  Reusing the sole dependency keeps the "readable in an afternoon" posture (D-009,
  D-011, D-012).
- **It is the pragmatic durability step before HA.** Full integrated storage
  (Raft) is the eventual HA answer but is months of the hardest possible code. A
  SQL backend gives durability and a replaceable node now, and de-risks any future
  Raft work. This is the ordering committed in `docs/ROADMAP.md`.
- **The trust model is unchanged.** The barrier encrypts every value with
  AES-256-GCM *before* it reaches any backend, so the database stores **only
  ciphertext** — it never sees plaintext, exactly like the file backend
  (DESIGN §3.2). A database compromise yields ciphertext, not secrets; a restore
  still requires the master key. This is what makes an external database an
  acceptable home for a secrets manager's state.

## The interface needs no change

`storage.Backend` is already the clean seam — its package doc anticipates
alternative backends ("in-memory, file, and later Raft … substituted freely").
The SQL backend is a drop-in implementation of the same four methods:

```go
Get(ctx, key)    (*Entry, error)   // (nil, nil) if absent
Put(ctx, entry)  error             // overwrite
Delete(ctx, key) error             // no-op if absent
List(ctx, prefix) ([]string, error) // immediate children; leaves as "seg", subtrees as "seg/"
```

Because the contract is unchanged, the SQL backend must pass the **exact same
conformance suite** every backend passes (`runBackendConformance` in
`internal/storage/conformance_test.go`). That suite is the correctness gate.

## Schema

A single table — `Backend` is a flat KV, so one table suffices.

```sql
CREATE TABLE IF NOT EXISTS ubixvault_kv (
    vault_key  VARBINARY(768) NOT NULL,
    value      LONGBLOB       NOT NULL,
    PRIMARY KEY (vault_key)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC;
```

Design points:

- **`VARBINARY`, not `VARCHAR`.** MySQL/MariaDB default collations are
  *case-insensitive*, which would silently make `secret/App` and `secret/app` the
  same row — a correctness landmine for an exact-match KV. A binary column
  compares byte-for-byte, matching the file/memory backends. This is the single
  most important schema decision.
- **`VARBINARY(768)` primary key.** With `ROW_FORMAT=DYNAMIC`/`COMPRESSED` and
  `innodb_large_prefix` (default on modern MariaDB/MySQL 5.7+/8.0), an index prefix
  up to 3072 bytes is allowed; 768 comfortably covers realistic vault paths. The
  backend enforces a **max key length** of 768 bytes and rejects longer keys with
  a storage error (keys are paths, so this is generous).
- **`LONGBLOB` value.** Barrier ciphertext is small, but LONGBLOB avoids any cap.
- No `updated_at`/versioning column: the `Backend` contract is last-writer-wins
  overwrite with no history (the KV **engine** provides versioning above the
  barrier). Keeping the table minimal keeps the surface auditable.

A tiny `ubixvault_schema` table (a single `version` row) is created alongside it
so the schema can evolve later without guesswork.

## Operation mapping

All statements use **bound parameters** (`?`) — no string interpolation of keys
or values ever. Table/column names are compile-time constants.

| Method | SQL |
|--------|-----|
| `Get` | `SELECT value FROM ubixvault_kv WHERE vault_key = ?` — no row → `(nil, nil)`. |
| `Put` | `INSERT INTO ubixvault_kv (vault_key, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)` — atomic upsert. |
| `Delete` | `DELETE FROM ubixvault_kv WHERE vault_key = ?` — 0 rows affected is fine. |
| `List` | range-scan under the prefix, then reduce to immediate children in Go (below). |

### `List` — the only non-trivial one

`Backend.List` returns *immediate* children: leaves as their final segment,
subtrees as `"segment/"`, sorted and de-duplicated. The plan:

1. **Range-scan the prefix** on the primary-key index:
   ```sql
   SELECT vault_key FROM ubixvault_kv WHERE vault_key >= ? AND vault_key < ?
   ```
   with lower bound = `prefix` and upper bound = `prefix` with its last byte
   incremented (the byte-wise successor). A range predicate avoids `LIKE`
   metacharacter escaping (`%`, `_`) entirely and uses the PK index left-anchored.
   For the root (`prefix == ""`) it is an unbounded scan of all keys.
2. **Reduce in Go** with the existing `childUnder` helper: map each full key to
   its immediate child under the prefix (leaf or `"seg/"`), collect into a set,
   sort. This is byte-for-byte the same reduction the file/memory backends use, so
   `List` semantics are identical by construction — which is why the conformance
   suite passes.

Reducing in Go reads every key under the prefix into memory. For a secrets store
this is bounded and fine; pushing distinct-first-segment into SQL
(`SUBSTRING_INDEX` + `DISTINCT`) is a possible later optimization but is
DB-dialect-specific and harder to prove correct, so v1 favors the simple,
provably-correct reduction.

## Concurrency, consistency, and the single-writer rule

- `*sql.DB` is a connection pool that is safe for concurrent use, satisfying the
  `Backend` "safe for concurrent goroutines" requirement. Each method is a single
  statement (implicitly atomic under InnoDB); the contract needs no multi-key
  transactions.
- **One active vault process per database.** This is the critical operational
  constraint and must be documented loudly. The core is single-node: barrier key,
  lease state, and in-flight unseal progress live in memory. Two vault processes
  pointed at the same database would not be HA — they would race on writes
  (last-writer-wins per key) and hold divergent in-memory state. So `replicaCount`
  stays **1**; the SQL backend's win is that the *storage* survives node loss, not
  that multiple nodes can serve. Multi-active HA requires leader election /
  Raft-style coordination, which is the deferred step.
- Startup **fails fast** if the database is unreachable (like the transit seal),
  rather than coming up in a broken state.

## Migration from the file backend

No new bespoke tooling is required — the snapshot machinery already round-trips a
whole encrypted store through the `Backend` interface:

1. `operator snapshot save` from the file-backed vault → a ciphertext snapshot.
2. `operator snapshot restore` into a **SQL-backed** target (restore writes
   through whatever `Backend` it is given).

The same unseal shares / KEK then work, because the ciphertext and the key
hierarchy are unchanged — only the physical store moved. A convenience
`operator migrate -from <dir>` that streams `storage.Walk(fileBackend)` into the
SQL backend can be added, but is optional given the above.

## Configuration & wiring

- Server flags: `-storage <file|mysql>` (default `file`, preserving current
  behavior) and `-storage-mysql-dsn` (or `$UBIXVAULT_STORAGE_DSN`), a
  go-sql-driver DSN. The DSN carries credentials and TLS parameters
  (`tls=true`/custom CA) so ciphertext-in-transit to the database is protected —
  defense in depth even though it is already ciphertext.
- Helm: a `storage.type` / `storage.mysql.dsnSecret` block; the DSN lives in a
  Secret. When SQL storage is used, the data PVC is unnecessary (the store is the
  database).
- The database credentials become a new operational secret to manage; this is the
  main new burden and is called out in the deployment docs.

## Security summary

- **Confidentiality is unchanged.** The database holds only barrier ciphertext;
  it is in the TCB for *availability*, not *confidentiality*.
- **No SQL injection surface:** every key/value is a bound parameter; identifiers
  are constants.
- **Case-exact keys** via `VARBINARY` prevent silent key aliasing.
- **TLS to the database** protects ciphertext in transit.
- New **credential-management** burden (the DSN) — documented, stored in a Secret.

## Testing plan

1. **Conformance:** run `runBackendConformance` against a real MariaDB — the same
   suite the file and memory backends pass. This is the primary correctness gate.
   CI already has a MariaDB service for the dynamic-database engine; reuse it.
2. **Backend specifics:** upsert overwrite; binary/large values; max-key-length
   rejection; `List` reduction across deep/wide trees; missing-key `(nil,nil)` and
   delete-no-op; concurrent readers/writers.
3. **Round-trip:** init → write secrets → snapshot save (file) → snapshot restore
   (SQL) → unseal with the same shares → read the secrets back.
4. **Failure modes:** database unreachable at startup (fail fast); connection drop
   mid-operation surfaces an error the layers above handle.

## Non-goals (explicitly out of scope for this backend)

- **Multi-writer HA / leader election.** One active writer only; HA is the later
  Raft/leader-lease step.
- **Postgres and other engines.** Viable behind the same interface later; MySQL
  first because it reuses the existing dependency.
- **Cross-key transactions.** The `Backend` contract does not require them and the
  engines above do not assume them.

## Rollout

Ship behind the `-storage` flag (default `file`, so nothing changes for existing
deployments), land it as its own beta, and validate on the maintainer's cluster
against a managed MariaDB **alongside** the file-backed instance before moving any
real data (per the roadmap's adoption guidance).
