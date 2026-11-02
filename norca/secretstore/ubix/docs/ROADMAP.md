# uBixVault — Roadmap

> **Status:** Active · Last updated 2026-09-17 · Current release **`v1.0.0`**
> (shipped — the official 1.0). The external security review is an assurance
> milestone, **not** a version gate, and does **not** hold back v1 or later
> releases (see `docs/VERSIONING.md`).

uBixVault is a **self-hosted secrets manager for a single organization**, built on a
minimal-dependency, fully-auditable ethos: the security-critical code — the encryption
barrier, Shamir seal/unseal, all cryptography — is standard-library Go written and tested
in-house, so the entire trust path can be read in an afternoon. It speaks a
Vault-compatible HTTP API so existing clients work unchanged.

The **feature core is complete** and the interface is stable, so uBixVault has
reached **`1.0` — an API-stability and feature-completeness milestone
([SemVer](https://semver.org/)):** the HTTP API, CLI, storage format, and chart
values are stable and we commit to SemVer compatibility rules from here. All the
engineering that got here landed through the betas — **durable storage** (a
MySQL/MariaDB backend), **hardened correctness** (parser fuzzing, crypto property
tests, a crash-recovery fix, signed images + SBOM), the **cloud-KMS/HSM seal**
(ADR D-015), and the full identity/auth surface.

`1.0` is **not** a claim that the cryptography has been audited — that is a
separate axis. An **independent external security review** is an open,
actively-pursued **assurance milestone**, tracked below and in
[`docs/VERSIONING.md`](VERSIONING.md); it is deliberately **not a version gate**,
because SemVer versions the interface, not the audit status.

## Honest positioning (read before deploying)

uBixVault matches HashiCorp Vault's *core feature surface* but **not** its *assurance*:
it has **not** had an independent third-party security audit. Regardless of the version
number, until that review lands it is best suited for sandbox, dev, and
internal/low-blast-radius use, not as a drop-in replacement for an audited secrets
manager holding critical secrets.

**Recommended adoption path** for anyone (including the maintainer) moving real workloads
onto it:
1. Run it **alongside** an existing audited secrets manager, not as a big-bang cutover.
2. Start with **low-blast-radius** secrets (a dev environment, one non-critical service).
3. Keep the incumbent for anything whose loss or compromise is serious until durable
   storage **and** an external review are in place.
4. Make the risk **visible** to whoever owns security — a solo, unreviewed system in the
   secrets path is a shared-risk decision.

---

## Where it is now (through `v0.2.0-beta.10`)

Core — complete, tested, documented:

- [x] Storage backend interface + **file** and **in-memory** backends.
- [x] **Barrier** — AES-256-GCM at rest, path-bound AAD, barrier-key hierarchy.
- [x] **Seal / unseal** — in-house Shamir k-of-n; init, unseal, seal-status.
- [x] **Root regeneration** (`generate-root`) + **recovery keys** for auto-unseal mode.
- [x] **Tokens** (TTL/expiry, lookup-self, renew-self, revoke-self) + **ACL policies** (JSON *and* HCL, default-deny).
- [x] **KV v2** (versioned secrets).
- [x] **Transit** — full crypto-as-a-service: encrypt/decrypt, rotate, **rewrap**, **data keys**, **HMAC**, **sign/verify** (ECDSA + Ed25519).
- [x] **Dynamic database credentials** — `DatabasePlugin` interface + MariaDB plugin, lease-scoped, auto-revoked.
- [x] **PKI** — internal CA, role-constrained short-lived certificate issuance.
- [x] **Auth methods** — token, AppRole, Kubernetes, userpass, JWT/OIDC.
- [x] **Seals** — Shamir, static KEK, and transit seal (unwrap via another vault).
- [x] **Response wrapping** — single-use, TTL'd secure-introduction tokens.
- [x] **Audit** — fail-closed file device, HMAC'd sensitive fields.
- [x] **Leases** — renew/lookup + cascading revocation.
- [x] **Web console** (`/ui/`) — KV, policies, tokens, PKI.
- [x] **Operations** — Prometheus metrics, rate limiting, health/readiness, encrypted snapshot/restore.
- [x] **Delivery** — `ubixvault server` + `operator` CLI, single-node Helm chart, multi-arch GHCR images, CI (build/test/lint/govulncheck/MariaDB integration, and since 1.2 a kind HA failover test).
- [x] **Rekey** — live rotation of the Shamir unseal shares (`sys/rekey`, `operator rekey`), no downtime.
- [x] **MySQL/MariaDB storage backend** — durable, replaceable-node storage (`-storage mysql`); the DB holds only ciphertext (ADR D-014).
- [x] **Scheduled backups** — opt-in chart CronJob snapshotting to an off-node PVC.
- [x] **Signed releases** — keyless cosign signatures + SPDX SBOM attestation on every published image.
- [x] **Dedicated liveness endpoint** (`/v1/sys/livez`), used by the chart's probes.

---

## Engineering gates for 1.0 — done

These were the work that made uBixVault safe to *run* — sequenced by what actually
reduces risk, not by what is most fun to build. The guiding rule: **durability without
correctness is a trap** — HA on top of un-hardened crypto is just a reliable way to lose
or leak secrets — so the hardening ran *alongside* the storage work, not after it. **All
of it has landed**, which is what makes the `1.0` API-stability milestone honest.

### Found in the field — done

- [x] **Survive a storage outage instead of crash-looping.** Reported from
      production 2026-09-12; **fixed and shipped in 1.0.0.** Transient storage
      errors (dial failures, `driver.ErrBadConn`, specific MySQL codes) are now
      classified and retried with bounded backoff in `internal/storage/retry.go`
      (`RetryBackend`), a sustained outage surfaces as `storage.ErrUnavailable` →
      HTTP 503 on `/v1/sys/health` instead of a crash, and `/v1/sys/livez` is
      storage-free so Kubernetes no longer kills (and re-unseals) the pod during a
      blip. No reseal on outage (ADR D-019). Report + resolution:
      [`docs/bugs/2026-09-12-storage-outage-crashloop.md`](bugs/2026-09-12-storage-outage-crashloop.md).

### Tier 0 — production safety (do first; small) — **done**
- [x] **Automated, off-node backups.** Chart CronJob runs `snapshot save` to a separate
      (network-backed) PVC. *Follow-up:* timestamped history + direct object-storage upload
      (needs an uploader image alongside the shell-less vault image).
- [x] **Rekey** — live rotation of the Shamir unseal shares (`sys/rekey`, `operator rekey`),
      no downtime, no data re-encryption.

### Tier 1 — durable storage (the real production unlock) — **done**
- [x] **SQL storage backend** over the existing MySQL/MariaDB driver (no new dependency).
      Turns "single node + local disk" into "**replaceable node over managed, replicated
      storage**": the node can die and reschedule pointing at the same durable database,
      and the database's own HA handles durability. For most single-org production this is
      *sufficient HA*; it also de-risks any future Raft work.
      Design: [`docs/design/sql-storage-backend.md`](design/sql-storage-backend.md) · ADR D-014.

### Continuous — trust & correctness (rides alongside Tier 0–1)
- [x] **Fuzz** the parsers — HCL policy, JWT/JWS, transit ciphertext, snapshot restore.
- [x] **Property tests** for Shamir (split/combine round-trips) and the barrier (round-trip + encryption-at-rest).
- [~] **Race / chaos tests** — crash-mid-write recovery done (found + fixed a real temp-file bug);
      broader concurrent-load testing still open.
- [x] **Supply-chain hygiene** — keyless cosign signatures + SPDX SBOM attestation on every image.
      *Follow-up:* reproducible builds.

### Tier 2 — pre-production trust gates (before real secrets land)
- [x] **Pluggable cloud-KMS/HSM seal** — an external-command seal (`-seal-external-command`)
      wraps/unwraps the master key via an operator-supplied command, so any cloud KMS or HSM
      works with no provider SDK in the vault ([`docs/design/kms-hsm-seal.md`](design/kms-hsm-seal.md) ·
      ADR D-015). *Follow-up:* Helm `sealExternal` chart wiring.
- [x] **`SECURITY.md`** + coordinated-disclosure policy, supported-versions, and scope;
      threat-model refresh in `docs/DESIGN.md` §5.

### Assurance — the open milestone (NOT a version gate)
- [ ] **External security review.** The single thing that separates "carefully engineered"
      from "independently trusted," and the one item on this roadmap that is **not ours to
      build**. Full paid audit (Trail of Bits / NCC / Cure53 class) when feasible, or a
      funded/coordinated audit via a program such as OSTIF or NLnet, or at minimum a scoped
      independent-researcher pass. It is tracked as an **assurance milestone, not a `1.0`
      gate** — SemVer versions the interface, not the audit status
      ([`docs/VERSIONING.md`](VERSIONING.md)). When it lands, the assurance badge/disclaimer
      flips in the README, `SECURITY.md`, and release notes; no version bump is implied.

### Optional — only if the external database should go away
- [ ] **Integrated Storage (Raft)** — HA with no external database. Deliberately **last**:
      active/standby HA over the SQL backend (D-021) covers replicas and maintenance, so
      Raft only earns its place if MySQL itself is to be removed. If pursued, it implements
      the same `HABackend` interface (leadership = lock, term = fencing generation) and
      starts with a design doc + an ADR on in-house Raft vs. `hashicorp/raft` (which would
      add several dependencies to a tree that has two).

---

## Beyond 1.0 — the catch-up backlog (not committed scope)

The long path toward broader Vault parity — **the standing goal is to close the gap
to HashiCorp Vault Enterprise's feature set** — recorded so the gap is explicit and
can be chipped away at. **1.0 already shipped;** none of this gated it, and none of
it gates future releases — the external review is a separate *assurance* milestone,
never a version gate. These are undertaken as they earn their place, never at the
expense of the finished core, and each larger item gets its own design note + ADR
first (as the SQL backend and the KMS seal did), with any new dependency recorded
there. Rough priority: Community-parity gaps (small, quick wins) interleaved with
the Enterprise push below.

**Enterprise push — suggested build order** (each design-note + ADR first; revisit
as we learn): **1.** Resource quotas (self-contained, extends the existing rate
limiter — a good first Enterprise slice). **2.** Namespaces (foundational — quotas,
policy scoping, and replication all key off it). **3.** Policy-as-code / ABAC
(Sentinel-style). **4.** Control Groups (M-of-N approval, step-up MFA). **5.**
Transform engine (tokenization / FPE / masking). **6.** Managed Keys + Key
Management engine (cloud-KMS offload). **7.** KMIP server engine. **8.** Replication
(performance / DR / standbys — largest). **9.** FIPS 140-3 compliance builds (gated
on a validated crypto module — hardest for a stdlib-only tree). Seal-wrap of
individual entries and object-storage snapshot upload fold in alongside as the two
partial analogs are completed.

### Toward Vault Community parity (free-tier gaps)
- [x] **HA: active/standby replicas** (1.2) — multiple replicas over the MySQL backend, one active holding a fenced lock, standbys forwarding and taking over on drain or failure (Vault Community HA). Design: [`docs/design/ha-active-standby.md`](design/ha-active-standby.md) · ADR D-021.
- [ ] **Integrated Storage (Raft)** — HA without an external database (also in "Path to 1.0 · Optional"; builds on the D-021 `HABackend` interface, and may never be needed).
- [x] **TLS client-certificate auth** — mTLS cert roles (CA- or pinned-cert trust, name constraints).
- [x] **LDAP / Active Directory auth** — bind + group search via `go-ldap/ldap/v3` (D-018, the project's second dependency); LDAP groups feed a group→policy map and identity external groups.
- [ ] **More dynamic secrets** — PostgreSQL / MySQL / Mongo / MSSQL DB plugins; cloud IAM (AWS/GCP/Azure).
- [x] **Identity** — entities, aliases, and groups, so multiple auth logins map to one subject, with policy templating. Design: [`docs/design/identity-entities-groups.md`](design/identity-entities-groups.md) + [`identity-templating.md`](design/identity-templating.md) (ADRs D-016, D-017). All four phases shipped: entities + aliases + entity policies; internal groups (nestable); external/IdP-asserted groups (JWT `groups_claim`); `{{identity.*}}` templating in ACL paths. Request-time policy union.
- [~] **Console breadth** — sign-in (userpass, LDAP, token), who-am-I, sign-out, and **initialize + unseal** shipped, covered by a headless-Chrome end-to-end test in CI; still to do: Transit, and managing the auth methods' roles and users in `/ui/`.
- [x] **Token scoping and max TTL** (ADR D-022) — child tokens only get the parent's policies (unless root/`sudo`) and cannot outlive it; renewal is capped by `-max-token-ttl`; every token may renew itself. A token hierarchy followed: delegated children are revoked with their parent (root/`sudo`-issued tokens are orphans). Per-role `token_max_ttl` on every auth method followed too.
- [~] **Transit extras** — key derivation + convergent encryption **shipped** (stdlib HKDF); BYOK import still to do.
- [x] **Cubbyhole** — per-token private storage, destroyed on token revoke.
- [x] **OIDC discovery** — resolve the JWKS URL from `.well-known/openid-configuration`.

### Toward Vault Enterprise parity (Enterprise-differentiated features)
Mostly large and compliance-oriented; listed so the gap is explicit (see
`docs/POSITIONING.md` for the side-by-side). Of ~15, two already have partial analogs:
- [~] **HSM / cloud-KMS auto-unseal** — done via the external-command seal (D-015); **seal-wrap** of individual entries is not.
- [~] **Automated snapshots to object storage** — the scheduled backup CronJob exists (to a PVC); direct object-storage upload + retention remains.
- [ ] **Namespaces** — in-vault administrative multi-tenancy.
- [ ] **Replication** — Performance replication, Disaster-Recovery replication, performance standby nodes.
- [ ] **Policy-as-code (Sentinel-style)** — ABAC / endpoint- & role-governing policies beyond ACL.
- [ ] **Control Groups** — M-of-N approval to access a secret; login-enforced step-up MFA.
- [ ] **Managed Keys** — offload crypto operations to an external HSM/KMS.
- [ ] **Key Management secrets engine** — distribute/manage keys in cloud KMS (AWS/Azure/GCP).
- [ ] **KMIP secrets engine** — act as a KMIP server.
- [ ] **Transform secrets engine** — tokenization, format-preserving encryption, data masking.
- [x] **Resource quotas** (1.1) — path-scoped rate-limit and lease-count quotas (ADR D-020).
- [ ] **Compliance builds** — FIPS 140-3 validated crypto, entropy augmentation.

---

## Sequencing principles

- **Interfaces first** (storage, seal, auth, engine, audit) so every addition is additive, never a rewrite — this is what lets the SQL backend and KMS seal slot in without touching the core.
- **Durability before HA, correctness before durability.** Backups and hardening are cheap insurance that make everything after them safer.
- **Zero-new-dependency by default.** Each capability is built from the standard library unless there is no reasonable alternative; the dependency graph staying readable is a security feature.
- **Ship in small, reviewed, CI-green slices**; cut a beta when a coherent set lands.
