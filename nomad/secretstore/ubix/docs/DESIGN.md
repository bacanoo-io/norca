# uBixVault — Design Document

> **Status:** Active · Last updated 2026-07-18

## 1. What uBixVault is

uBixVault is an open-source, self-hosted **secrets management and data-protection**
system — an alternative to HashiCorp Vault. It originated to provide secrets management for
[uBixCore](https://github.com/cwolsen7905/uBixCore), but it is **framework-agnostic**: it exposes
a standard HTTP API and works with any application, language, or platform. It is the central
authority that stores, generates, and controls access to secrets: API keys, database
credentials, certificates, and encryption keys.

**Design goals**

1. **Secure by default** — default-deny access, encryption at rest, audited, memory-safe implementation.
2. **API-compatible with HashiCorp Vault** at the wire level, so existing Vault client libraries, SDKs, and Terraform providers can be reused without building new tooling.
3. **Single static binary**, no external dependencies to run a node.
4. **Pluggable everywhere** — storage, auth methods, secrets engines, and audit devices are all interfaces.
5. **Operationally predictable** — observable, easy to run, upgrade, and reason about.

**Non-goals (for now):** being a general database, a full IAM/SSO product, or a
KMS-replacement for cloud-native workloads that already have one.

## 2. Technology choice

**Language: Go.**

Rationale (full comparison in `docs/DECISIONS.md`):

- HashiCorp Vault and the entire cloud-native secrets/infra ecosystem (Kubernetes, Consul, etcd, cert-manager, SPIFFE/SPIRE) are Go — largest pool of contributors who can be productive in this codebase, and easiest integration path.
- First-class domain libraries: `crypto/*` stdlib (constant-time-aware), `x/crypto`, `hashicorp/raft`, gRPC/protobuf, HTTP/TLS.
- Memory safety eliminates an entire class of CVEs — non-negotiable for a security product.
- Single static binary, trivial cross-compilation, plugin model over gRPC (`go-plugin`).

**Known Go weakness we accept and mitigate:** the GC can copy/relocate memory, so we
cannot guarantee a secret is wiped and never duplicated in RAM. Mitigations: `mlock`
the process, minimize secret lifetime in memory, zero buffers where the runtime allows.
If an extreme threat model (nation-state memory forensics, FIPS 140-3 physical) ever
becomes a first-order requirement, the crypto core is isolated behind an interface so a
Rust/C++ module could be swapped in.

## 3. Core architecture

uBixVault is layered. Everything rests on the **barrier**; the barrier rests on the
**storage backend**.

```
            ┌─────────────────────────────────────────────┐
  clients → │  HTTP API  ·  CLI (ubixvault)                │
            ├─────────────────────────────────────────────┤
            │  Auth Methods  →  Token & Lease Manager      │
            │  Policy (ACL) Engine  ·  Identity            │
            ├─────────────────────────────────────────────┤
            │  Secrets Engines (KV v2, Transit, DB dynamic)│
            ├─────────────────────────────────────────────┤
            │  Audit Devices  (broker → file/syslog)       │
            ├─────────────────────────────────────────────┤
            │  BARRIER  (AES-256-GCM encryption at rest)   │
            │  Seal / Unseal  (Shamir k-of-n)              │
            ├─────────────────────────────────────────────┤
            │  Storage Backend  (file → Raft later)        │
            └─────────────────────────────────────────────┘
```

### 3.1 Barrier & Seal/Unseal
- All data written to storage is encrypted by the **barrier** with a symmetric key (AES-256-GCM). Storage never sees plaintext. Each entry is `version || nonce || ciphertext+tag`.
- The **storage path and the format version are bound as GCM additional authenticated data.** Binding the path means a ciphertext only decrypts at the exact location it was written to, so an attacker with storage access cannot relocate a valid blob to another path; binding the version prevents silent format downgrade.
- The 96-bit nonce is random per write. This carries a birthday bound (~2³² writes per key before collision risk becomes non-negligible), addressed by barrier-key rotation (§8.2).
- On startup the node is **sealed**: it holds ciphertext but not the master key.
- **Unsealing** reconstructs the master key. MVP uses **Shamir's Secret Sharing** (e.g. 3-of-5 key shares). The master key decrypts the barrier key; the barrier key decrypts data.
- Key hierarchy: `unseal shares → master key → barrier encryption key → data`. Supports key rotation of the barrier key without re-sharing.
- Auto-unseal (cloud KMS) is a post-MVP seal type behind the same interface.

### 3.2 Storage backend
- Interface: `Get/Put/Delete/List(prefix)` over an encrypted blob store.
- MVP: single-node **file backend** (and an in-memory backend for tests).
- Post-MVP: **Integrated Storage (Raft)** — embedded consensus, no external dependency, provides HA. The interface is designed so Raft slots in without touching engines.

### 3.3 Auth methods → tokens
- An auth method verifies an external identity and issues a **token** with attached policies + TTL.
- MVP: **token** auth (root token on init, child token creation). AppRole is the first post-MVP add (machine-to-machine).
- Post-MVP: Kubernetes, JWT/OIDC, LDAP, TLS cert, cloud IAM, userpass.

### 3.4 Tokens & leases
- Everything issued (a login token, a dynamic secret) is a **lease** with a TTL.
- Leases can be **renewed** and **revoked**; revocation cascades to child leases.
- The lease manager tracks expiry and triggers revocation (e.g. dropping a dynamic DB user).

### 3.5 Policy (ACL) engine
- Path-based ACLs, **default-deny**, capabilities: `create / read / update / delete / list / sudo`.
- Policies authored in HCL/JSON, Vault-compatible semantics.
- Token → policies → capability check on every request.

### 3.6 Secrets engines (mounted at paths)
MVP ships three:
- **KV v2** — versioned static key-value secrets (the baseline).
- **Transit** — encryption-as-a-service: encrypt/decrypt/sign/HMAC over the API; key never leaves the vault. High-value, self-contained.
- **Database dynamic** — generates on-demand, short-lived DB credentials with a TTL and auto-revokes them on lease expiry. This exercises the dynamic-secret model — on-demand generation with lease-bound revocation — which is the defining capability of a secrets manager. Built against a **`DatabasePlugin` interface**; **MariaDB** is the MVP reference implementation, with PostgreSQL/MySQL as follow-on plugins (only the driver and credential SQL differ — the lease lifecycle is database-agnostic).

### 3.7 Audit devices

Every request and response passes through an **audit broker** that fans out to one or more
**audit devices**. The audit log is the record of *who accessed which secret or key, when,
and whether it succeeded* — the primary tool for answering "who is using what."

**What each entry records:**

| Field | Example | Purpose |
|---|---|---|
| Timestamp | `2026-07-18T14:03:22Z` | when the request occurred |
| Identity (*who*) | token **accessor** (not the token), auth method, entity/alias, attached policies, remote address | who performed the action |
| Operation | `read` / `create` / `update` / `delete` | the action taken |
| Path (*what*) | `transit/encrypt/orders-key`, `secret/data/db-creds` | which key / secret / mount |
| Outcome | success, or error + code | whether it was allowed and succeeded |
| Request/response data | **HMAC'd**, never plaintext | correlation without exposure |

So a call to `transit/encrypt/orders-key` yields an entry stating *identity X invoked
`encrypt` on key `orders-key` at time Z — success*, with no plaintext, ciphertext, or key
material in the log.

**Security properties:**
- **Token accessors, not tokens** — the log references a token by its accessor handle, so the audit trail never contains a usable credential.
- **HMAC'd sensitive fields** — request/response values are HMAC'd with a key kept in the barrier (one per vault, stable across restarts), so identical inputs yield identical hashes; you can ask "was this value seen elsewhere?" without storing the value.

**Where entries go — pluggable devices (`AuditDevice` interface):**
- **MVP: file device** — structured **JSON, one object per request and one per response**, appended to an operator-configured path (e.g. `/var/log/ubixvault/audit.log`).
- **Post-MVP: syslog and socket devices** — stream to a SIEM / log aggregator (Splunk, ELK, …) over TCP/UDP/unix socket.
- **Multiple devices** may be enabled at once. Auditing is **fail-closed**: if a device marked mandatory cannot record an entry, the request is refused rather than served unaudited — auditing cannot be bypassed by breaking the log sink. (This creates an availability tension — see §8.)
- *Tamper-evidence* (append-only, hash-chained entries) is noted as an optional extension; the MVP does not claim it.

### 3.8 Supporting glue (mostly post-MVP)
- **Identity** — entities + groups unifying multiple auth aliases under one identity.
- **Response wrapping** — hand a client a single-use token that unwraps to the real secret; secure secret delivery.
- **Cubbyhole** — per-token private storage.

## 4. API surface

- REST/HTTP, JSON, versioned under `/v1/`, **path-compatible with HashiCorp Vault** for the subsystems we implement (`/v1/sys/*`, `/v1/auth/*`, `/v1/secret/*`, `/v1/transit/*`, `/v1/database/*`).
- Token passed via `X-Vault-Token` (and an `X-uBix-Token` alias).
- `ubixvault` CLI mirrors the `vault` CLI verbs (`operator init/unseal`, `login`, `kv`, `policy`, `token`, `secrets enable`, `transit`, ...).
- Goal: existing Vault client libraries and Terraform providers work by changing only the address. This is **wire/API compatibility per subsystem** — not feature-complete parity with Vault. Compatibility is verified per subsystem in CI, not promised globally.

### 4.1 How workloads consume secrets

The API *is* the delivery mechanism. The intended pattern — and the one designed for first —
is that a workload **calls the vault API directly** and holds secrets in memory:

- On Kubernetes, a pod authenticates with its ServiceAccount token via the (post-MVP)
  Kubernetes auth method, receives a policy-scoped token, and reads its secrets over the API.
- Pods in **different Kubernetes namespaces can share access to the same secret** by binding
  their ServiceAccounts to a common ACL policy — no per-namespace deployment and no in-vault
  namespace is required (Kubernetes namespaces are unrelated to in-vault namespaces; see §3.5
  and `docs/ROADMAP.md`).
- Secrets never touch disk or etcd, never appear as environment variables, and can be
  re-fetched when they rotate — which matters for the short-lived dynamic credentials the
  database engine issues.

This is the recommended approach for workloads we control (uBixCore's own apps). A separate
**operator** that syncs secrets into native Kubernetes `Secret` objects for
environment-variable injection is an *optional* extension, warranted only for third-party
apps that cannot be modified to call the API; it is less secure and cannot deliver rotating
secrets to env vars. See `docs/DECISIONS.md` (D-008) and `docs/ROADMAP.md`.

## 5. Threat model

The model an external review should start from — what is protected, the boundaries
it is protected across, which adversary positions are in scope, and what is
explicitly not defended. Reflects the implementation as of `v0.2.0-beta.11`.

### Assets

- **Secrets** — plaintext KV values, generated dynamic credentials, and the
  plaintext a caller submits to Transit. The thing being protected.
- **The key hierarchy** — unseal shares / recovery keys → master key → barrier key
  → data. Compromise of a key compromises everything below it.
- **The auto-unseal wrapping key** — a local KEK, a transit token, or a KMS/HSM key
  reached through the external seal — which recovers the master key on restart.
- **Tokens** and their ACL policies (the credentials that authorize requests).
- **Audit-log integrity** — a tamper-evident record of access.
- **Availability** of the vault.

### Trust boundaries

- **Client ↔ API** — the network. Mandatory TLS (§2); the server refuses
  non-loopback plaintext without an explicit dev override.
- **Barrier ↔ storage** — the file or MySQL backend. The barrier AES-256-GCM-encrypts
  every value, path- and version-bound, *before* it reaches storage, so the storage
  layer (a local disk or a shared/managed database) sees **only ciphertext** and is in
  the TCB for *availability*, not *confidentiality* (§3.1–3.2, D-014).
- **Vault ↔ seal** — for auto-unseal the wrapping key lives outside the vault: in
  another transit engine (D-013) or a KMS/HSM reached via the external-command seal
  (D-015). The external seal pipes the 32-byte master key through a child process's
  stdio during an unseal.
- **Vault ↔ managed database** — the dynamic-secrets engine holds admin credentials
  to mint short-lived users.
- **Release ↔ operator** — published images are keyless-signed with an SBOM
  attestation, so an operator can verify provenance before deploying.

### Adversary positions (in scope) and mitigations

- **Reads storage** (steals the disk, dumps the database, or leaks the MySQL DSN):
  gets **ciphertext only** — still needs the master key, which never touches storage.
- **Steals or stops a sealed node:** useless — nothing is readable until unsealed,
  and unsealing needs a share quorum, the KEK, or the external seal.
- **On the network:** blocked by TLS; secrets never transit in plaintext, and the
  barrier's path binding stops a ciphertext being relocated to another path.
- **Holds a leaked token:** limited to that token's default-deny ACL grants; tokens
  are short-lived and revocable, and dynamic DB creds auto-revoke on lease expiry.
- **Reads the audit log:** sensitive fields (including the client token) are HMAC'd,
  and the log is fail-closed (unwritable → the vault stops serving).
- **Tampers with a published image:** detectable via the cosign signature + SBOM.
- **Supplies a malicious external-seal command or KMS response:** a wrong unwrap
  yields a wrong master key, which fails barrier authentication → the vault stays
  **sealed** (fail-safe), never fail-open.

### Residual risks (in scope, imperfectly mitigated)

- **Secrets in memory.** Plaintext secrets and the barrier key live in RAM while in
  use. Mitigated only by short lifetimes and minimized copies; **`mlock` is not
  currently used**, and the Go GC may relocate or retain copies — so a
  memory-forensics adversary on a running, unsealed host can recover secrets. Known
  limitation (D-001); memory-locking is possible future hardening.
- **A compromised unsealed host is game over** — code execution on the unsealed
  process has the master key in memory. No secrets manager defends this fully;
  mitigation is operational (host hardening, minimizing the unsealed blast radius).
- **The external-seal key path** — the master key transits a child process's stdio
  during unseal: bounded (in-memory pipe, operator-trusted child, no worse than a
  local KEK) but a real exposure (D-015).
- **"Secret zero"** — auto-unseal and the MySQL backend each need one bootstrap
  credential (a KEK, a transit token, or a DSN). It can be shrunk and protected
  (Secrets, KMS, mTLS/IAM) but not eliminated.
- **No external security review yet** — the most significant residual risk. See
  `SECURITY.md`.

### Explicitly out of scope

- **Multi-active writers and multi-tenant isolation** — one active writer per store
  (active/standby HA elects it and fences the rest, D-021; there is no multi-active
  mode); in-vault namespaces are not implemented.
- **Approval workflows (M-of-N control groups)** and login-enforced MFA.
- **Nation-state memory forensics, physical extraction, and FIPS-140 physical
  requirements** — D-001 notes the escape hatch (the crypto core sits behind an
  interface a hardened module could replace).

## 6. MVP definition of done

A single-node uBixVault that can: initialize, seal/unseal via Shamir, authenticate with
tokens, enforce ACL policies, store versioned KV secrets, do encryption-as-a-service via
Transit, issue and auto-revoke dynamic MariaDB credentials (via the `DatabasePlugin`
interface), and write an HMAC'd audit log — all reachable over a Vault-API-compatible HTTP
interface and the `ubixvault` CLI.

See `docs/ROADMAP.md` for phased milestones beyond the MVP.

## 7. Engineering principles & standards

These are the standards the codebase is held to. They are not aspirational — each maps to a
concrete structural decision, and code review checks against them.

### 7.1 Idiomatic Go first, with SOLID as a complementary lens

The primary bar is **idiomatic Go**, not a language-agnostic checklist. Go is not
class-based, so the principles that carry the most weight here are the language's own
(see §7.3 for the canonical references):

- **Accept interfaces, return structs** — functions depend on the narrow behavior they need; constructors return concrete types.
- **Consumer-defined, minimal interfaces** — an interface belongs in the package that *uses* it and stays as small as possible (often one or two methods, in the spirit of `io.Reader`).
- **Composition over inheritance** — behavior is assembled from embedding and small interfaces; Go has no inheritance by design.
- **Useful zero values, errors as values, and "clear is better than clever."**

**SOLID** remains a useful shared vocabulary and maps cleanly onto those idioms, so we
use it as a complementary lens — not the definition of quality:

- **S — Single Responsibility.** Each package/type owns one concern: the *barrier* only encrypts/decrypts at rest, the *seal* only manages unseal state, the *lease manager* only tracks TTLs and revocation. No catch-all types.
- **O — Open/Closed.** The core is extended by *adding implementations*, never editing it: new storage backends, auth methods, secrets engines, audit devices, and database plugins plug in through interfaces.
- **L — Liskov Substitution.** Any implementation of an interface is fully substitutable, and **shared conformance tests enforce it** — e.g. the in-memory and file storage backends pass one common suite. Contracts (error semantics, idempotency of revoke) are part of the interface.
- **I — Interface Segregation.** Small, focused interfaces over broad ones — the Go norm. `DatabasePlugin` exposes only `Initialize/CreateUser/RevokeUser/RotateRoot`.
- **D — Dependency Inversion.** The core depends on *interfaces*; concrete implementations are injected at startup. Nothing in the core imports a concrete driver, cloud SDK, or storage engine.

**The key interfaces (the seams the whole design rests on):** `storage.Backend`,
`Barrier`, `Seal`, `AuthMethod`, `SecretsEngine`, `DatabasePlugin`, `AuditDevice`.
Names follow Go convention — package-qualified and stutter-free (`storage.Backend`, not
`storage.StorageBackend`). Defining these seams first (in the v1.0 core) is what makes
every later extension additive.

### 7.2 Security engineering standards

- **Default-deny** everywhere; **least privilege** via short-lived tokens and dynamic secrets.
- **Fail closed** — if a mandatory audit device can't record, the request is refused; if the barrier can't verify, the node stays sealed.
- **No secrets in logs or errors** — sensitive values are HMAC'd in audit, never logged in plaintext; error messages never leak key material.
- **Constant-time** comparisons for tokens/HMACs (`crypto/subtle`); vetted stdlib crypto only — **no hand-rolled cryptography**. The sole, deliberate exception is Shamir secret sharing (a secret-sharing scheme, not a cipher), implemented in-house with constant-time field arithmetic and test vectors — see `docs/DECISIONS.md` (D-009). Ciphers, hashes, and primitives remain standard-library only.
- **Defense in depth** — barrier encryption + sealed-at-rest + TLS-in-transit + ACLs are independent layers.
- Track guidance from **OWASP ASVS** and the **OWASP Secrets Management Cheat Sheet** as the review checklist for the API and secret handling.

### 7.3 Go & general engineering standards

- **Idiomatic Go**: formatting is canonical and tool-enforced by `gofmt`/`goimports` — not a matter of style or debate. Conventions follow **Effective Go**, the **Go Code Review Comments**, and the **Google Go Style Guide**; `go vet`, `staticcheck`, and `golangci-lint` run clean in CI. Errors are wrapped with `%w`; `context.Context` is threaded through request paths.
- **Testing**: table-driven unit tests; the crypto core and seal/unseal state machine held to high coverage; integration tests for the DB plugin against a real MariaDB (containerized in CI).
- **API**: documented with an **OpenAPI** spec; Vault path-compatibility validated per subsystem against real Vault client libraries.
- **Config**: 12-factor style — config via file + env, no secrets baked into images.
- **Versioning & history**: **Semantic Versioning**; **Conventional Commits** for a readable, tooling-friendly history.
- **Observability**: structured logging, Prometheus metrics (post-MVP), health/readiness endpoints.
- **Supply chain**: pinned dependencies, `govulncheck` in CI; reproducible static builds.

See `docs/DECISIONS.md` (D-007) for the rationale behind adopting these as hard standards.

## 8. Operational & security considerations

Concerns a production secrets manager must address. Each is marked **[MVP]** (in the v1.0
core) or **[Later]** (roadmap). Listing them here — even when deferred — is deliberate: it
records that they were considered, not overlooked.

### 8.1 Initialization & secret-zero distribution — [MVP]
`operator init` is the most sensitive moment: it produces the **unseal key shares** and the
initial **root token**. Shares must not all land in one place (that defeats Shamir). The MVP
supports splitting shares to separate recipients and treating the root token as a
bootstrap-only credential to be revoked after initial policy setup. Optional PGP-encryption
of individual shares (so each holder can decrypt only their own) is a natural extension.

### 8.2 Key rotation & rekeying — [MVP for barrier; versioning for Transit]
Three distinct rotations, deliberately separated:
- **Barrier key rotation** — issue a new barrier encryption key going forward without re-sharing unseal keys (supported by the key hierarchy in §3.1). **[MVP]**
- **Transit key versioning** — keys have versions; encrypt uses the latest, decrypt accepts configured older versions, with `rewrap` to re-encrypt to the newest and a minimum-decryption-version to retire old ones. **[MVP]**
- **Master-key rekey** — regenerate the master key and reissue unseal shares (e.g. when a share holder leaves). **[Later]**

### 8.3 Root regeneration & disaster recovery — [partly MVP]
- **Regenerate-root** — reconstruct a new root token from a quorum of unseal shares when the original is lost, without unsealing plaintext. **[MVP]**
- **Recovery keys** — when auto-unseal (KMS/HSM) is used, the barrier is unsealed automatically, so a separate set of *recovery* keys guards privileged operations. **[Later, with auto-unseal]**
- **Irrecoverability risk** — losing a quorum of unseal shares means the data is unrecoverable by design. This must be documented prominently for operators.
- **Backup & restore** — the encrypted storage must be backupable and restorable; for Raft, via consistent snapshots. **[Later, with Raft]**

### 8.4 Privileged credentials held by engines — [MVP]
The dynamic database engine holds a **privileged MariaDB account** in order to create and
drop users. This is a concentrated blast radius and is treated as such: the credential is
stored inside the barrier, its permissions are scoped to user management, and the engine
supports **root-credential rotation** (`RotateRoot` on the `DatabasePlugin` interface) so the
bootstrap password can be rotated out immediately after configuration.

### 8.5 Availability vs. fail-closed auditing — [MVP]
Fail-closed auditing (§3.7) means an unavailable or full audit sink will **stop the vault
from serving** — availability is traded for the guarantee that nothing goes unaudited. This
is intentional but must be operationalized: log rotation, disk-space monitoring, and (where
appropriate) multiple audit devices so a single sink failure does not halt service.

### 8.6 Entropy & randomness — [MVP]
All key and token generation uses the platform CSPRNG (`crypto/rand`). Generation fails
closed if the entropy source is unavailable rather than falling back to a weak source.

### 8.7 Rate limiting & resource quotas — [Later]
API rate limiting and **lease/dynamic-secret quotas** bound abuse and prevent resource
exhaustion — e.g. an unbounded stream of dynamic-credential requests exhausting users on the
target database. Deferred, but the lease manager is designed with quota hooks in mind.

### 8.8 Transport security — [MVP]
TLS is required for the API (configurable minimum version and cipher suites). Optional
mutual TLS for clients and a documented server-certificate lifecycle are extensions.

### 8.9 Storage format versioning & upgrades — [MVP-aware]
On-disk/barrier entries carry a format version so future releases can migrate data forward
compatibly. Designing this in from v1.0 avoids a painful breaking migration later.

### 8.10 Restart & availability model — [MVP]
A node restarts **sealed** and must be unsealed before serving — a deliberate security
property with an availability cost. Single-node deployments therefore have a manual-unseal
recovery step; HA (Raft) and auto-unseal address this later.
