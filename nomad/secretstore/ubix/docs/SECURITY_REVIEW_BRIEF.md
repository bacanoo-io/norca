# uBixVault — External Security Review Brief (1.0)

> A scoping brief for anyone assessing uBixVault's security — a firm, an
> independent reviewer, or a funded-OSS program. It gives the hard scoping facts,
> a pass/fail definition of the assurance bar, the crown-jewels scope, the specific
> questions we care about, and the working model. uBixVault has reached
> **`1.0.0`** (API stability under SemVer); an independent external review is the
> open **assurance milestone** that the version deliberately does *not* gate on —
> the number communicates interface stability, not audit status
> (`docs/VERSIONING.md`, `docs/ROADMAP.md`). This brief makes it easy to scope and
> kick off.

## At a glance (scoping facts)

| | |
|---|---|
| **Review target** | `v1.0.0` is released; the review runs against `v1.0.0` (or a later frozen tag) · commit `<sha>` · frozen `<YYYY-MM-DD>` at outreach, docs frozen to the same tag. |
| **Language / toolchain** | Go 1.25 (`go.mod`), standard library + **two** direct third-party modules — `github.com/go-ldap/ldap/v3` (LDAP/AD auth) and `github.com/go-sql-driver/mysql` (MySQL storage). go-ldap pulls a small transitive set (`go-asn1-ber/asn1-ber`, `Azure/go-ntlmssp`); all module hashes are pinned in `go.sum`. |
| **Size** | ~13.1k LoC production (67 non-test `.go` files) + ~9.4k LoC tests. |
| **Build** | Single **static** binary, **CGO disabled** (`CGO_ENABLED=0`), multi-arch (amd64/arm64), distroless runtime image. |
| **Entry points** | `cmd/ubixvault/main.go` (server + `operator` CLI); HTTP API under `internal/api` (routes in `internal/api/sys.go`, Vault-compatible `/v1/*` paths). |
| **Attack surface** | The HTTP API; with HA, the **cluster listener** (replica-to-replica, mutual TLS, port 8201); the storage backend (file or MySQL); the auto-unseal seal (KEK, transit, or external command); operator CLI/flags/env. **`net/http/pprof` is not exposed.** |
| **Storage backends** | File (local dir) and **MySQL/MariaDB** (`-storage mysql`); both hold only barrier ciphertext. |

## What uBixVault is

A self-hosted secrets manager in Go (HashiCorp Vault–style), single-node or
active/standby HA over MySQL: an
AES-256-GCM encryption barrier, in-house Shamir seal/unseal, KV v2, Transit
(crypto-as-a-service, incl. HKDF-derived keys and convergent encryption), dynamic
MySQL/MariaDB credentials, PKI, cubbyhole, an **identity** layer (entities,
aliases, internal + external groups, templated policies), **seven auth methods**
(token, userpass, AppRole, JWT/OIDC, Kubernetes, TLS client-certificate, and
LDAP/AD), ACL policies, and a fail-closed audit log, over a Vault-compatible HTTP
API. All cryptography is Go standard-library primitives wrapped in in-house logic;
the only third-party modules are the LDAP and MySQL drivers. Architecture in
`docs/DESIGN.md` §3; decisions in `docs/DECISIONS.md`.

**Status:** **`1.0.0` released** — the public API is stable under SemVer. It has
**not** been independently reviewed or audited; the version communicates interface
stability, not assurance (`docs/VERSIONING.md`). The engineering gates for 1.0
(durable storage, hardening, KMS/HSM seal, identity, the full auth set) have
landed; this external review is the open **assurance milestone**.

## The assurance bar (pass/fail)

A clean review — the assurance milestone — is achieved when, against the frozen
review tag:

- **No open Critical or High findings on the crown-jewels scope** (below).
- The **fail-closed audit** guarantee is verified (an unwritable audit sink stops the vault serving).
- The load-bearing invariant **"storage / DSN compromise yields ciphertext, not secrets"** holds end to end.
- **Medium/Low findings may be accepted** for 1.0 with documented mitigations or a tracked follow-up.

**Severity (our definitions; score with CVSS 3.1):** *Critical* = key disclosure,
unseal without a quorum/KEK, a fail-open seal, or storage/DSN yielding plaintext.
*High* = authentication or authorization bypass, or audit bypass. *Medium/Low* =
everything else, per CVSS.

## Threat model

Full model in `docs/DESIGN.md` §5. In two sentences: **in scope** — a network
attacker, a storage/DSN compromise (reads the backend), a holder of a leaked
token, a malicious client, and a supply-chain tamper of the release image; **out
of scope** — a compromised, *unsealed* host with the master key already in memory,
and multi-writer/multi-tenant concerns. The central trust boundary is the
**barrier**: everything below it is ciphertext.

## Scope — Primary (must-review; catastrophic if broken)

1. **`internal/barrier`** — AES-256-GCM at rest. AEAD construction, **nonce
   uniqueness under crash/retry**, AAD = storage path + format version (relocation
   / downgrade / replay), and the master-key → barrier-key wrapping incl. the
   **rewrap-on-rekey** path.
2. **`internal/shamir` + `internal/core` seal state machine** — the in-house
   GF(2⁸) secret-sharing: field arithmetic and Lagrange interpolation correctness,
   **constant-time** behavior, share validation, and **quorum enforcement**. Can
   the vault be coerced to unseal without a quorum/KEK, or leak a key? Soundness of
   `rekey`, `generate-root`, and the recovery-key flows.
3. **`internal/seal`** — static-KEK, transit, and **external-command** seals (see
   [External seal](#external-seal--process-model) for the process model). Any
   **fail-open** path is Critical.
4a. **`internal/policy` + `internal/token`** — ACL **default-deny** correctness,
   path/**glob** semantics, token hierarchy/scoping. The policy parser is an
   **in-house HCL parser (not `hashicorp/hcl`)** — it needs a parser
   differential / injection review. Default-deny has deliberate built-in
   exceptions in `internal/api/auth.go` `authorize`: every token may use its own
   cubbyhole, and `auth/token/lookup-self`, `renew-self` and `revoke-self`,
   each acting only on the calling token (renewal is capped at the token's
   ceiling). Token creation enforces parent/child scoping (ADR D-022): policies
   must be a subset of the caller's unless it is root or holds `sudo`, and a child
   cannot outlive the caller's ceiling. Such a child is revoked with its parent
   (on `revoke-self` and on expiry, recursively); tokens created by root or a
   `sudo` holder are deliberate orphans. The parent link is stored as a hash in a
   `sys/token-child/` index — reviewers may want to check the cascade cannot be
   skipped (e.g. a failed index write) or turned against unrelated tokens.
4b. **`internal/jwtauth`, `internal/approle`, `internal/userpass`, `internal/kubeauth`, `internal/certauth`, `internal/ldapauth`**
   — JWS/JWT verification (**algorithm confusion**, `kid`/JWKS handling,
   `exp`/`aud`/issuer checks), AppRole secret-id **constant-time** hash compare,
   userpass PBKDF2 parameters + timing, Kubernetes TokenReview handling, TLS
   client-certificate chain / SAN verification, and LDAP/AD bind — the `go-ldap`
   dependency surface, DN / search-filter injection, and referral handling.
4c. **`internal/identity`** — entity/alias resolution, **group → policy**
   attachment (internal and external/auth-mapped groups), and **policy templating**
   (identity data interpolated into ACL paths). An entity mapped to the wrong
   groups, or a template that expands to an over-broad path, is an authorization
   bypass — the alias-matching and the templating expansion are the parts to press.

## Scope — Secondary (important; High if broken)

5. **`internal/transit`** — encrypt/decrypt, sign/verify (ECDSA/Ed25519), HMAC,
   key versioning; correct algorithm/key-type separation.
6. **`internal/storage`** — file backend path-traversal defenses and
   **crash-recovery/temp-file** handling; MySQL backend bound-parameter / injection
   surface, case-exact `VARBINARY` keys, and the single-active-writer assumption.
7. **Snapshot / restore / rekey APIs** — the ciphertext/snapshot parsers,
   **downgrade risk**, and whether a crafted snapshot can corrupt or mislead a restore.
8. **Config / CLI / env handling** — a frequent leak point for the KEK, DSN, or
   transit token (flags vs. env, process args, error messages).
9. **Token lifetime / revocation / lease GC** — can a stale token or lease bypass
   ACL or outlive a revocation?
10. **`internal/audit`** — fail-closed guarantees and HMAC'ing of sensitive fields.
11. **`internal/pki`**, and the **supply chain** (cosign signing + SBOM;
   minimal-dependency posture — two direct modules plus go-ldap's small transitive set).
12. **HA (`internal/cluster`, `internal/core/ha.go`, `internal/storage/mysql_lock.go`;
   D-021)** — new in 1.2:
   - **Cluster listener.** TLS 1.3, client certificate required, from a CA the active
     replica creates in the barrier (`sys/ha/cluster-ca`) and every unsealed replica
     can read; leaves are issued in-process, 24h. Is "only a process that unsealed this
     vault can connect" true? What does holding the CA key (anyone who can read the
     barrier) allow?
   - **Forwarded client address.** A standby passes the original client address in
     `X-Ubixvault-Cluster-Client-Addr`; the active honours it only on the cluster
     listener and audits/rate-limits under it. Can a client reach the API with that
     header honoured, or make a standby forward a spoofed one?
   - **Fencing.** Once a replica has a lock handle, each MySQL write checks holder and
     generation under a shared row lock in its transaction. Can a replica that lost the
     lock (paused, partitioned) still land a write — through the retry wrapper, a
     transaction edge, or a path that writes around `exec`?
   - **Forwarding behaviour.** Requests are held during an election and bodyless ones
     re-sent to a new leader; requests with a body are never re-sent. Any way to replay a
     write, or to loop a request between replicas?

## External seal — process model

The external-command seal (`-seal-external-command`) is how cloud-KMS/HSM
auto-unseal works without a provider SDK. The exact model to review:

- The command and its args are **operator-supplied server configuration** (flags),
  not network input. It is invoked as `<cmd> [args] wrap|unwrap`.
- **`wrap`** reads the 32-byte master key on stdin → writes the wrapped blob to
  stdout at init; **`unwrap`** reverses it at each unseal. The master key transits
  a child process's stdin/stdout (an in-memory pipe) during that call.
- **Timeout:** each call is bounded (`-seal-external-timeout`, default 30s) via a
  context; on timeout or non-zero exit the child is killed and the operation fails.
- **Fail-safe means:** the vault **stays sealed** — it never falls back to an
  unprotected key. A wrong `unwrap` yields a wrong master key that fails barrier
  authentication (so a bad KMS response also just keeps it sealed).
- **Env:** the child inherits the vault's environment (plus optional appended
  vars); KMS credentials are the operator's to supply there.
- **Known residual:** we do not explicitly wipe the pipe buffers after use
  (best-effort only) — please assess. Implementation: `internal/seal/external.go`.

## Explicitly in / out of scope

- **In:** everything above; the honesty of the "ciphertext-only storage" claim.
- **Out:** a compromised *unsealed* process (master key in memory — accepted; with HA
  that is any unsealed replica); multi-active writers and in-vault namespaces (not
  implemented); nation-state memory
  forensics / physical extraction / FIPS-140 physical.
- **DoS / resource exhaustion:** **lower priority.** A per-client token-bucket
  rate limiter exists (opt-in). Note obvious unauthenticated amplification, but a
  full DoS assessment is not the focus.

## Questions we most want answered

- Is the **in-house cryptography** (Shamir, the barrier's AEAD usage, PBKDF2, JWS
  verification) correct and free of timing / side-channel issues?
- Can any path **unseal without a proper quorum/KEK**, disclose a key, or leave a
  seal **fail-open**?
- Any **authorization bypass** — ACL evaluation, glob/path matching, token
  scoping, or auth-method verification (esp. JWT algorithm/`kid` confusion)?
- Any **injection or memory-safety** issue in the parsers (in-house HCL, JWS,
  ciphertext, snapshot) or the storage backends?
- Are there **key-zeroization or logging side-channels** that leak key material via
  error messages, metrics, or (were it enabled) profiling endpoints?
- Is the **fail-closed audit** actually fail-closed, and is
  **"storage compromise = ciphertext only"** true end to end?

## Constant-time & zeroization (what we attempt — please validate)

- **Constant-time:** the in-house Shamir GF(2⁸) multiply, PBKDF2 password compare
  (userpass), AppRole secret-id compare, and recovery-key compare use
  constant-time paths (`crypto/subtle` / branch-free field math). We claim intent;
  please confirm.
- **Zeroization:** the master key, barrier key, and Shamir shares are
  best-effort-overwritten (`zero()`) after use. This is **best-effort only** — the
  Go GC may relocate or retain copies, and `mlock` is not used; assess the real
  residual exposure.

## Supply chain

- **Toolchain:** Go 1.25; two direct modules (`go-ldap/ldap/v3`,
  `go-sql-driver/mysql`) plus their transitive set (`go-asn1-ber/asn1-ber`,
  `Azure/go-ntlmssp`, `google/uuid`, `filippo.io/edwards25519`, `golang.org/x/crypto`).
  `go.sum` pins all module hashes; standard Go module resolution (module proxy),
  **not vendored**. **No `replace` directives, no vendoring, `go.sum` committed.**
  `govulncheck ./...` runs in CI on every change.
- **Releases:** multi-arch images are **keyless-signed with cosign** and carry an
  **SPDX SBOM attestation**; both are stored in the registry (OCI) alongside the
  image and logged to the Rekor transparency log, verifiable via the GitHub OIDC
  identity (see `docs/DEPLOYMENT.md` §9).

## What we provide

- Full source (public, BSD-3-Clause), `docs/DESIGN.md` (architecture, §5 threat
  model), `docs/DECISIONS.md` (ADRs), and this brief — frozen to the review tag.
- **`docs/REVIEWER_QUICKSTART.md`** — build, run (incl. MariaDB via a container),
  and a happy-path API walk-through, plus how to run the tests and fuzz targets.
- The Vault-compatible route list (`internal/api/sys.go`); an OpenAPI spec is not
  yet published — API semantics mirror HashiCorp Vault per subsystem (D-003).
- Our own prior testing, to build on rather than trust: native **fuzz targets**
  (parsers), **property tests** (Shamir, barrier), backend **conformance** tests,
  and `govulncheck` output on request.
- A specific frozen commit/tag, and prompt engagement on questions.

## Working model & deliverable

- **Private disclosure** of unfixed findings via the channel in `SECURITY.md`
  (GitHub private vulnerability reporting) — **not** public issues. NDA available
  on request; a direct comms channel (email / Signal / Slack) set at kickoff.
- **Coordinated disclosure:** a private fix window before publication (see the
  90-day default in `SECURITY.md`). We provide a **fix branch for re-test**.
- Expected deliverable: a **written report with severity-rated findings and a
  methodology note.** After remediation, we intend to **publish the final report**
  (with the reviewer's agreement) — findings and fixes.
- **Reviewer profile sought:** independence (unaffiliated) and relevant expertise
  (applied cryptography + Go / systems security), with a documented methodology.

## Engagement paths & effort

Any of these satisfies the gate, given the profile above:

- **Scoped crown-jewels review** — our preferred first step: roughly a
  **1-week, 1–2-reviewer** pass over Primary scope (§Primary).
- **Full-scope firm audit** (e.g. Trail of Bits, NCC Group, Cure53, Doyensec,
  Latacora) — typically **~2–3 weeks, 2 auditors** for a codebase this size.
- **Funded-OSS program** — e.g. **OSTIF**, which can broker/fund a firm audit.

Budget: we are a solo open-source project **seeking pro-bono / OSTIF / grant
funding or a scoped paid engagement** — happy to right-size scope to fit.

## Contact

- **Vulnerabilities:** GitHub Security tab → *Report a vulnerability*
  ([repo](https://github.com/cwolsen7905/ubixvault)).
- **Scoping / engagement:** `cwolsen@brainchurts.com`.
