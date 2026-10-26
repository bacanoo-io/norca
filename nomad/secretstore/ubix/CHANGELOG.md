# Changelog

All notable changes to uBixVault are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.5.0] — 2026-09-30

**The console runs a vault's whole lifecycle, and delegated tokens form a tree.**
Initialize and unseal from `/ui/`, now covered by a headless-Chrome end-to-end test
in CI; revoking a token revokes the tokens it delegated; and every auth method
takes a per-role `token_max_ttl`. Backward-compatible with the 1.x API; no storage
or chart-values changes.

### Upgrade notes

- **Delegated tokens now die with their parent.** A token created by a caller that
  is neither root nor holds `sudo` on `auth/token/create` is revoked when that
  caller's token is revoked or expires. Tokens created by root or a `sudo` holder,
  and auth-method logins, are unaffected (orphans). Tokens that existed before the
  upgrade have no parent and behave as orphans.
- **Expired tokens are removed by the sweeper**, within 10 minutes, instead of by
  the lookup that noticed them. They are still refused immediately.

### Added

- **Revoking a token revokes the tokens it created** (ADR D-022 follow-up). A token
  created by a non-root, non-`sudo` caller is that caller's child: `revoke-self` —
  and expiry — revoke it and its own children in turn, destroying each one's
  cubbyhole and dynamic-database leases. Tokens created by root or a `sudo` holder,
  and auth-method logins, are **orphans** and unaffected, so revoking an operator's
  token does not revoke what they issued. `lookup-self` reports `orphan`.
- **Per-role `token_max_ttl`** on every auth method (userpass users, AppRole,
  TLS cert, JWT/OIDC and Kubernetes roles, and the LDAP config), as in Vault. It
  lowers the ceiling of the tokens that role issues: their expiry and any renewal
  stop at login + `token_max_ttl`. It never raises the ceiling above the system
  maximum. A `token_ttl` larger than `token_max_ttl` is refused with a `400`.
  Returned on read.
- **Console: initialize and unseal.** A new vault can be initialized from `/ui/`
  (choose key shares and threshold); the shares — or recovery keys, with
  auto-unseal — and the root token are shown once, each with a Copy button, and
  stay on screen until you confirm you have stored them. A sealed vault shows an
  Unseal panel that takes key shares one at a time with a live progress count; a
  share is never kept in the page. An auto-unseal vault that stays sealed explains
  that shares cannot help and to check the seal.
- **Console end-to-end test in CI** (`test/e2e/console/`, `make console-e2e`): drives
  the real console in headless Chrome — initialize, unseal share by share, sign in
  with a token and with userpass, sign out — and checks the server side (a pasted
  token survives "Forget token"; a signed-out token is really revoked).

### Changed

- An expired token is still refused at once, but its record is now removed by the
  background sweeper (within 10 minutes) rather than by the lookup that noticed it,
  so the child cascade always runs.

## [1.4.0] — 2026-09-29

**Works with HashiCorp's own clients.** The `vault` CLI, External Secrets
Operator and anything else built on `github.com/hashicorp/vault/api` now work
against uBixVault — checked end to end with the official client. Also: LDAP
directories with a private CA can be verified (`certificate`), expired tokens are
cleaned up in the background, and Kubernetes roles' `ttl` is finally applied.

### Upgrade notes

- **Policy reads changed shape:** `GET /v1/sys/policies/acl/<name>` returns
  `data.policy` as a **string**, as Vault does. If you parse that response
  yourself, parse the string (it is the policy's JSON text).
- **JSON policies with unknown keys are now refused** (`400`) instead of being
  stored without them. Check any automation that writes JSON policies with keys
  other than `path` / `capabilities`.
- **Kubernetes roles' `ttl` now takes effect.** If a role already sets one,
  logins through it get that lifetime from now on instead of 32 days.
- **Consider a short `ttl` on Kubernetes roles** used by apps that log in per
  request or per process — each token then stops being a valid credential soon
  after it is used, and the new sweeper removes it.

### Added

- **Expired tokens are deleted in the background** (every 10 minutes, on the
  active replica, up to 20,000 per pass), along with what each one owned — its
  cubbyhole and its dynamic-database leases — as revoke-self does. Before, an
  expired token was only removed if something looked it up, so a client that
  logs in on every request left every token behind; a dev deployment had
  accumulated 80,811 of them in five weeks, ~45 rows of real data among them.
- **LDAP auth: `certificate`** on `auth/ldap/config` — a PEM bundle of the CA
  certificates that sign the directory's TLS certificate, as in Vault. Directories
  that issue their own certificates (FreeIPA's Dogtag CA, Active Directory
  enterprise CAs) can now be verified instead of needing `insecure_tls`, which
  sent the bind password and every user's password to an unverified peer. When
  set, only those CAs are trusted for the connection. Validated on write: it must
  contain only certificates (a pasted private key is refused, not stored), and it
  cannot be combined with `insecure_tls`. Returned on config read, like Vault.

### Changed

- **`GET /v1/sys/policies/acl/<name>` returns `data.policy` as a string**, as
  Vault does (the policy's canonical JSON text, which can be written back
  unchanged). It was a JSON object, which Vault's own client could not read — its
  `GetPolicy` panicked — and neither can Terraform's Vault provider. **If you
  parse this response yourself, parse the string.** The console is updated.
- **JSON policy documents reject unknown keys** (`400`). A misspelled `path`
  used to be stored as an empty policy, and restrictions uBixVault does not
  implement (e.g. `allowed_parameters`) were silently dropped, so the policy
  granted more than written. HCL policies were already strict.

### Fixed

- **Writing a policy the way Vault's clients do stored an empty policy.** Vault's
  clients and Terraform send `{"policy": "<HCL or JSON text>"}`; uBixVault took
  that body as the policy document itself, found no paths, stored a policy with
  no rules, and answered `204`. It fails closed (an empty policy grants nothing)
  but reported success. The `{"policy": ...}` shape is now accepted; a bare
  document still works.
- **Kubernetes auth ignored the role's `ttl`.** It was stored and shown on read
  but not applied, so every login got the 32-day default. Logins now get the
  role's `ttl` when set. If your clients log in per request or per process, set
  a short `ttl` on their role (minutes): each login's token then stops being a
  valid credential almost as soon as it is used.
- **HashiCorp's own Vault clients now work.** Checked end to end with the
  official Go client (`github.com/hashicorp/vault/api`), which the `vault` CLI
  and External Secrets Operator are built on. It failed at the first call before
  this; now init, unseal, KV v2, listing, policies, token create/lookup/renew/
  revoke and userpass login all work:
  - **Every write route accepts both `PUT` and `POST`**, as in Vault. 41 routes
    accepted only one, so e.g. `vault operator init`/`unseal` and the Go client's
    `PUT auth/kubernetes/login` (what ESO sends) got `405`.
  - **`GET …?list=true` is a list**, as in Vault (the Go client lists this way);
    only the `LIST` method worked, and listing the top of the KV store failed.
  - **Durations accept a number of seconds** (`3600`, `"3600"`) as well as
    `"1h"`, on every TTL/increment field, as in Vault.
  - **`sys/init`, `sys/unseal` and `auth/token/create` accept the fields Vault's
    client always sends.** Fields uBixVault does not implement are accepted only
    at their zero value and otherwise refused with a clear error — never silently
    ignored where that would change the result (e.g. `pgp_keys`, which would
    otherwise return plaintext shares, or `num_uses`). With auto-unseal,
    `recovery_shares`/`recovery_threshold` configure the recovery keys, as in
    Vault.

## [1.3.0] — 2026-09-29

**Token scoping and console sign-in.** Fixes a critical privilege escalation in
token creation (see Security), caps token renewal, and lets the web console sign
in with userpass or LDAP. Backward-compatible with the 1.x API; no storage or
chart-values changes.

### Upgrade notes

- **Read the Security section first** and upgrade promptly if any non-root policy
  grants `auth/token/create`.
- **Token creation is now scoped.** A caller that is neither root nor holds `sudo`
  on `auth/token/create` gets `400` if it requests policies it does not hold, and
  its child tokens cannot outlive it. If something relies on delegated token
  creation with wider policies, give that caller `sudo` on `auth/token/create`
  deliberately. Root-issued tokens, including long-lived CI tokens, are unchanged.
- **Renewal is capped** at `-max-token-ttl` (default `768h`) for tokens whose own
  TTL is shorter. Existing tokens are never shortened.

### Added

- **Console sign-in.** The `/ui/` console signs in with a username and password
  (userpass) or LDAP, as well as a pasted token, and shows who you are: policies,
  identity policies, and when the token expires. **Sign out** revokes a token the
  console obtained; a pasted token (which may be root or shared) is only
  forgotten in that tab, never revoked. **Renew** appears for expiring tokens.
- **`GET /v1/auth/token/lookup-self`**, in Vault's response shape: `id`,
  `policies`, `identity_policies`, `entity_id`, `creation_time`, `issue_time`,
  `expire_time` (null when it never expires), `ttl`, `renewable`, `type`.
- **`-max-token-ttl`** (default `768h`, Vault's default `max_lease_ttl`): how far
  renewal may extend a token whose own TTL is shorter. See Security below.

### Changed

- **Every token may look itself up, renew itself, and revoke itself** without an
  ACL grant, as under Vault's built-in `default` policy. Each acts only on the
  calling token, and renewal is now capped (below), so none extends its access.

### Fixed

- The console header still read "uBix Vault"; it now reads uBixVault.

### Security

- **Critical — token creation could escalate to any policy, including `root`.**
  `auth/token/create` passed the requested policies and TTL straight through, so
  a token whose only grant was `auth/token/create` could mint a `root` token (or
  any other policy) with any lifetime. A caller that is neither root nor holds
  `sudo` on `auth/token/create` may now only give a child token policies it holds
  itself (else `400 child policies must be subset of parent`), and the child
  cannot outlive the caller's own maximum lifetime. Root and `sudo` callers are
  unaffected, so operator workflows that mint long-lived tokens (e.g. 1-year CI
  tokens) keep working. Affects every earlier version, in any deployment where a
  non-root policy grants `auth/token/create`. ADR D-022.
- **Token renewal is capped.** `renew-self` used to set the expiry to now plus
  any requested increment, so a token allowed to renew could keep itself alive
  indefinitely. Every expiring token now has a ceiling fixed at creation — the
  later of its own expiry and creation time plus `-max-token-ttl` — and renewal
  never passes it. Tokens issued before the upgrade are never shortened: their
  ceiling is the later of their current expiry and created time plus the
  maximum.

## [1.2.1] — 2026-09-28

A patch release: a log flood behind Kubernetes ingress, and naming and console
touch-ups. No API, storage, or chart-values changes; upgrade in place.

### Changed

- **Name is written `uBixVault`** — one word, in the console, docs, chart, and
  startup output. Past entries below keep the spelling they shipped with.
- **Console footer no longer says "read-only"** — the console has written KV,
  policies, tokens, and PKI since 0.2.0-beta.3. It stays marked beta until
  Transit and the newer auth methods are in `/ui/`.

### Fixed

- **Aborted TLS handshakes no longer flood the log.** Ingress controllers and
  service meshes health-check backends with bare TCP connects; against a TLS
  listener each one aborted the handshake and logged a line — one per probe, per
  replica, which was 99.96% of the log on a real 3-replica cluster. Handshakes
  (and HTTP/2 prefaces) the peer abandons by closing or resetting the connection
  are now counted and summarized every 5 minutes. Every other handshake failure is
  still logged as it happens. New flag `-log-tls-handshake-aborts` restores the
  per-line output — set it if anything alerts on those lines, since the summary
  carries a count but no client addresses.

## [1.2.0] — 2026-09-24

**High availability.** uBix Vault runs as several replicas over MySQL/MariaDB
storage: every replica unseals, one holds a fenced lock in the database and
serves, and the rest are standbys that forward to it — HashiCorp Vault's
Community HA model (ADR D-021, `docs/design/ha-active-standby.md`). Additive and
backward-compatible with the 1.x API; single-replica deployments behave as
before.

### Upgrade notes

- **MySQL schema goes to version 2** (a new lock table), applied automatically on
  first start. Schema migrations are forward-only and a binary now refuses a
  database migrated by a newer one, so **take a snapshot before upgrading**:
  rolling back across this release means restoring it.
- **Enabling HA on an existing install:** upgrade to 1.2.0 at one replica first,
  then set `ha.enabled`, then scale. A pre-1.2 replica takes no lock and must
  never run against the database alongside HA replicas. See
  `docs/DEPLOYMENT.md` § Upgrades.
- **Audit `token_hmac` values change once**: the HMAC key now lives in the
  barrier, so HMACs from before the upgrade will not match those after it.

### Added

- **Active/standby HA** (`-ha`, requires `-storage mysql`). The active replica
  holds a lease-based lock row in the database; expiry is judged by the
  database's clock. **Every write is fenced** to the lock generation the replica
  acquired, inside its own transaction, so a replica that lost the lock without
  noticing (a pause, a partition) cannot write. A replica that is shut down —
  every drain and rolling restart — hands the lock over and a standby is active
  within `-ha-retry-interval` (500ms); a replica that dies is replaced within
  `-ha-lock-ttl` (15s). Two replicas cannot initialize the same database.
- **Standby forwarding**, so clients can use any replica (e.g. one Kubernetes
  Service). Replicas talk over a dedicated cluster listener (`-ha-cluster-listen`,
  port 8201; `-ha-cluster-addr`) with mutual TLS from a CA the vault keeps in its
  own barrier — no operator certificates, and only replicas that unsealed the
  same vault can connect. The active replica audits and rate-limits forwarded
  requests under the original client's address. During a handoff standbys hold
  requests (up to 5s) rather than failing them, and re-send a request without a
  body if the leader they reached had just gone; a request with a body is never
  sent twice.
- **`sys/leader`** (unauthenticated) and **`sys/step-down`** (authenticated),
  Vault-compatible. A replica that steps down stays out of the election for 10s
  so another takes over — unless none does, in which case it takes the lock back
  rather than leave the vault with no active replica.
- **`sys/health` query parameters** as in Vault: `standbyok`, `activecode`,
  `standbycode` (default `429`), `sealedcode`, `uninitcode`; `perfstandbyok` is
  accepted and ignored. The body gains `standby`. Without parameters a single
  replica answers exactly as before.
- **`-shutdown-delay`**: on SIGTERM an HA replica hands the active role over at
  once, then keeps serving (as a forwarding standby) for this long before closing
  its listeners, while Kubernetes stops routing to the terminating pod.
- **Helm chart `ha.enabled`** (chart 0.1.16): `replicaCount` replicas (3
  recommended) with `-ha`, the pod IP from the downward API, the cluster port, a
  `standbyok` readiness probe, a PodDisruptionBudget (`maxUnavailable: 1`), pod
  anti-affinity (`ha.antiAffinity`: `soft`/`hard`/`none`) and
  `ha.shutdownDelay` (5s). `replicaCount > 1` without HA, and HA without MySQL,
  are still refused. New `terminationGracePeriodSeconds` (30, Kubernetes'
  default). Single-replica renders are otherwise unchanged.
- **HA failover test** in CI (`.github/workflows/ha-e2e.yml`, `test/e2e/ha/`):
  three replicas on kind with a client reading through the Service throughout.
  Deleting the active pod, draining its node, a rolling restart, `sys/step-down`
  and a force-delete must fail zero requests (first run: 0 of 1,512); freezing
  the active pod's node must fail over within the lock TTL (17s) and the thawed
  former active must step down.
- `docs/DEPLOYMENT.md`: an HA guide, the rolling-upgrade procedure, and the order
  for enabling HA on an existing install.

### Changed

- **The MySQL schema is versioned and checked at startup.** The backend used to
  record a schema version it never read. It now reads it first: a current schema
  needs no lock and no DDL (so replicas restarting together don't queue); a
  behind one is migrated in order under a MySQL named lock, with its own
  generous time limit; a newer one is refused, naming both versions.
- **Audit token HMACs are stable across restarts and replicas**: the key is kept
  in the barrier (`sys/audit/hmac-key`) instead of being random per process.
  Entries written while sealed omit `token_hmac`.
- `make lint` installs and runs golangci-lint v2.12.2, the version CI uses, and
  CI runs on pushes to every branch mirrored from GitLab.

### Fixed

- **Auto-unseal retries instead of giving up.** A KMS, transit vault, external
  seal command or database briefly unreachable at startup used to leave the
  server sealed until restarted; it now retries with backoff (1s to 1m) while the
  API is already up (`livez` 200, `health` 503 until unsealed).
- **Sealing drops cached state**: loaded quotas, the database engine's connection
  pool, the Kubernetes auth TokenReview client and cached JWKS keys no longer
  survive a seal/unseal; reconfiguring the database engine no longer leaks the
  previous connection pool.

### Security

- **Response-wrapping tokens are single-use under concurrency.** Simultaneous
  `sys/wrapping/unwrap` calls with the same token could each return the payload;
  unwrap is now serialized, so exactly one does. Affected single-node
  deployments of every earlier version.
- The security review brief covers HA's new surface (the cluster listener and its
  CA, the forwarded client address, fencing, and forwarding/replay).

## [1.1.0] — 2026-09-18

First post-1.0 feature release: **resource quotas**, the first step toward
HashiCorp Vault **Enterprise** feature parity. Additive and backward-compatible
with the 1.0 API.

### Added

- **Resource quotas** — the first Vault-Enterprise-parity feature (design:
  `docs/design/resource-quotas.md`, ADR D-020). Vault-compatible, root/ACL-gated,
  barrier-persisted, loaded at unseal.
  - **Rate-limit quotas** (`sys/quotas/rate-limit/:name`, LIST/GET/POST/DELETE):
    path-scoped per-client token buckets over a logical API path prefix; the most
    specific matching quota (longest prefix) is enforced in the request middleware
    with `429` + `Retry-After`. A **default (global) quota** is set via
    `sys/quotas/config` (`default_rate`/`default_burst`) and the `-rate-limit` flag
    now seeds it — the default applies to any path with no named quota, even while
    sealed (so init/unseal can't be brute-forced), while named quotas take effect
    after unseal.
  - **Lease-count quotas** (`sys/quotas/lease-count/:name`): cap the number of
    active leases under a path prefix; a new dynamic DB credential is refused with
    `429` at the cap, before any credential is created.
  - Denials increment `ubixvault_quota_exceeded_total{quota}`. Finer role/mount
    scope is a later phase.

### Fixed

- **Helm chart `appVersion` tracks the release.** The chart's `appVersion` was
  stuck at `0.2.0-beta.11` while `values.yaml` defaults `image.tag` to
  `.Chart.AppVersion` when empty — so a `helm upgrade` without an explicit
  `--set image.tag` deployed beta.11. Bumped to `1.0.0` (chart `0.1.13` →
  `0.1.14`) so the default now matches the current release.

## [1.0.0] — 2026-09-12

**First stable release.** The public interface — the Vault-compatible HTTP API,
the `ubixvault` server/operator CLI, the on-disk/SQL storage format, and the Helm
chart values — is declared **stable under [SemVer](https://semver.org/)** from
here. This is an **API-stability and feature-completeness** milestone; it is
**not** a claim that the cryptography has been independently audited — that
assurance is an open, actively-pursued milestone, tracked separately and
deliberately **not** a version gate (`docs/VERSIONING.md`, `docs/ROADMAP.md`).
Until it lands, the "not yet independently audited" status stands.

1.0.0 is the code validated through `1.0.0-rc.1`…`rc.3` (identical to `rc.3`),
soaked in the reference Kubernetes deployment. It rolls up the full journey from
the `0.1.0` MVP and the `0.2.0-beta` line: the encryption barrier, in-house
Shamir seal/unseal, KV v2, Transit (incl. key derivation & convergent
encryption), dynamic database credentials, PKI, cubbyhole, a full identity layer
(entities, aliases, internal + external groups, policy templating), the complete
Vault-Community auth-method set (token, AppRole, Kubernetes, userpass, JWT/OIDC,
TLS client-certificate, LDAP/AD), response wrapping, fail-closed audit, three
seal modes incl. an external-command KMS/HSM seal, a MySQL/MariaDB storage
backend, rekey, snapshots, metrics/health, and a read-only web console — over a
Vault-compatible API, with essentially two dependencies (the MySQL driver and
go-ldap).

### Fixed

- **Survive brief storage outages.** A transient MySQL/MariaDB outage (a network
  blip, a dropped connection) no longer fails every request: storage operations
  retry transient errors — network failures, `driver.ErrBadConn`, MySQL
  server-gone/shutdown codes — with bounded exponential backoff, classifying
  retryable vs permanent (a malformed key still fails immediately). A sustained
  outage fails fast as a new `storage.ErrUnavailable`, which `/v1/sys/health`
  maps to **503** (readiness drops, traffic stops, the vault recovers on its own
  when storage returns) instead of a 500. `/v1/sys/livez` is (and remains)
  storage-independent, so a liveness probe cannot kill the process during a
  storage outage. A storage outage does **not** reseal the vault (ADR D-019).
  Hardening prompted by `docs/bugs/2026-09-12-storage-outage-crashloop.md`;
  investigation there found the production restarts that surfaced this were a
  node/runtime-level cause, not storage, but the missing retry/degradation was a
  real defect worth fixing regardless.

## [1.0.0-rc.2] — 2026-09-12

Second release candidate for **1.0.0**. Identical in behavior to `rc.1`; it exists
because `rc.1`'s container image never built.

### Fixed

- **Container image build.** The Dockerfile pinned `golang:1.24-alpine`, but the
  `go-ldap` dependency added in `beta.12` raised the `go.mod` directive to
  `go 1.25.0`, so `go mod download` failed inside the image (the `Build & Test`
  CI job uses Go 1.26 and stayed green, masking it — only the tag/`main` image
  build broke). Bumped the build stage to `golang:1.26-alpine`. This also
  restores the `:edge` image published on `main`, broken since `beta.12`.

## [1.0.0-rc.1] — 2026-09-12

Release candidate for **1.0.0**. No functional changes since `0.2.0-beta.12` —
this candidate declares the public interface (the Vault-compatible HTTP API, the
`ubixvault` server/operator CLI, the storage format, and the Helm chart values)
**stable under [SemVer](https://semver.org/)** and rolls up the accumulated beta
work into the first `1.0` line.

### Changed

- **Version and security assurance are now decoupled.** `1.0` is an API-stability
  and feature-completeness milestone; it is **not** a claim that the cryptography
  has been independently audited. An external security review is tracked as an
  open **assurance milestone, not a version gate** (`docs/VERSIONING.md`,
  `docs/ROADMAP.md`). Until it lands, the README and `SECURITY.md` carry an
  explicit "not yet independently audited" status. New `docs/VERSIONING.md`
  states the policy; the roadmap and README were reframed accordingly.

## [0.2.0-beta.12] — 2026-09-04

Twelfth beta: the identity layer and LDAP close the last two Vault-Community
gaps, plus TLS-cert auth, OIDC discovery, cubbyhole, and transit key
derivation. This completes the Vault-Community auth-method set (token, AppRole,
Kubernetes, userpass, JWT/OIDC, TLS cert, LDAP).

### Added

- **Transit — derived keys & convergent encryption** — a transit key created
  with `"derived":true` derives a per-context subkey (HKDF-SHA256) from a
  caller-supplied `context` on every encrypt/decrypt, so one key yields
  independent keys per context; the context is bound into the ciphertext (a wrong
  context fails to decrypt). `"convergent":true` (implies derived) makes
  encryption deterministic — the same plaintext and context produce the same
  ciphertext, enabling equality/dedup checks without decrypting — via a nonce
  derived (HMAC-SHA256) from the plaintext. `context` (base64) is accepted on
  `/encrypt`, `/decrypt`, and `/rewrap`; a derived key requires it and a
  non-derived key rejects it. Stdlib `crypto/hkdf` — zero new dependency.

- **LDAP / Active Directory auth method** — log in with a directory username and
  password: the vault binds to the directory to verify them, reads the user's
  group memberships, and issues a token whose policies come from a group→policy
  map (`/v1/auth/ldap/groups/{name}`) and/or identity external groups (the LDAP
  groups are asserted through the same seam as an OIDC groups claim). Configure
  via `/v1/auth/ldap/config` (URL, StartTLS/LDAPS, service bind DN, user/group
  search bases and attributes); log in at `/v1/auth/ldap/login/{username}`. This
  is the project's **second** dependency — `github.com/go-ldap/ldap/v3` (ADR
  D-018) — confined to one adapter behind a `Connector` seam; the method logic is
  stdlib. Completes the Vault-Community auth-method set (token, AppRole,
  Kubernetes, userpass, JWT/OIDC, TLS cert, LDAP).

- **Identity — policy templating (phase 4)** — ACL policy paths may now contain
  `{{identity.entity.id}}`, `{{identity.entity.name}}`, and
  `{{identity.entity.metadata.<key>}}` placeholders, expanded against the
  requesting token's entity when the request is authorized. One policy —
  `secret/data/users/{{identity.entity.name}}/*` — thus gives every user their
  own subtree, with no per-user policy. A placeholder that does not resolve drops
  that rule (fail-closed), so a templated grant is worthless to a token without
  the value. Completes the identity layer (D-016); design in
  `docs/design/identity-templating.md`, ADR D-017. Zero new dependency.

- **Identity — external groups (phase 3)** — groups whose membership is asserted
  by an auth method rather than listed by hand. Create a group with
  `"type":"external"`, a `mount_type`, and a `group_name`; an entity is a member
  when a login through that method asserted that group. The JWT/OIDC method reads
  the caller's groups from a configurable `groups_claim` and records them on the
  entity's alias, refreshed each login — so gaining or losing an IdP group takes
  effect on next login. External groups nest inside internal ones like any other.
  Zero new dependency. (Identity templating in ACL paths is the remaining phase.)

- **Identity — internal groups (phase 2)** — collect entities into **groups**
  that carry their own policies; a member entity's tokens pick up the group's
  policies (unioned per-request, like entity policies). Groups nest —
  `member_group_ids` names child groups whose members inherit this group's
  policies, to any depth (cycles tolerated). Manage via `/v1/identity/group`
  (create/update by name, or update by `id`), `/group/id/{id}`,
  `/group/name/{name}`, `LIST /group/id`. Zero new dependency. External
  (IdP-asserted) groups and identity templating are still to come.

- **Identity — entities & aliases (phase 1)** — a subject layer over the auth
  methods. Every login now resolves its `(method, login-name)` to an **entity**,
  auto-creating one on first sight, and the issued token carries the entity's ID
  (returned as `entity_id`). Policies attached to an entity are unioned with the
  token's own **on each request** — so a policy added to a subject takes effect
  for tokens already issued, and applies across every method that subject logs in
  through. Manage via `/v1/identity/entity` (create/update by name, or update by
  `id`), `/v1/identity/entity/id/{id}`, `/v1/identity/entity/name/{name}`, `LIST
  /v1/identity/entity/id`, and `/v1/identity/entity-alias` (bind another
  method's login to an existing entity). Groups, external-group mapping, and
  identity templating are later phases (design: `docs/design/identity-entities-groups.md`,
  ADR D-016). Zero new dependency.

- **Cubbyhole secrets engine** — per-token private storage at `/v1/cubbyhole/`.
  Each token gets its own namespace: `POST`/`GET`/`LIST`/`DELETE
  /v1/cubbyhole/{path}` read and write plain JSON objects (no versioning), and
  the data is visible only to the token that wrote it — no ACL grant, not even a
  root token, reaches another token's cubbyhole, because the scoping is
  structural (the storage path derives from the token). A token's cubbyhole is
  destroyed when the token is revoked, so its private data never outlives the
  credential. Zero new dependency.

- **JWT/OIDC discovery** — the JWT auth method can now resolve its JWKS URL from
  an OIDC issuer: set `oidc_discovery_url` on the config and the vault fetches
  `<issuer>/.well-known/openid-configuration` to find `jwks_uri` (cached), instead
  of configuring `jwks_url` directly. Zero new dependency.

- **TLS client-certificate auth method** — authenticate by presenting an mTLS
  client certificate. Define cert roles under `/v1/auth/cert/certs/{name}` (a
  trusted CA or a specific certificate, a policy set, optional
  `allowed_common_names`, token TTL), then `POST /v1/auth/cert/login` while
  presenting a certificate that chains to (or equals) the role's trust anchor and
  matches its name constraints — no request body needed. The server requests a
  client certificate on the TLS handshake but does not verify it; the auth method
  verifies it against each role's configured CA. Zero new dependency (stdlib
  `crypto/x509`). Joins the token, AppRole, Kubernetes, userpass, and JWT/OIDC
  methods.

## [0.2.0-beta.11] — 2026-08-27

Eleventh beta: cloud-KMS / HSM auto-unseal — the last 1.0 engineering gate.

### Added

- **External-command (KMS/HSM) auto-unseal seal** — reach any cloud KMS
  (AWS/GCP/Azure) or hardware HSM without a provider SDK. `-seal-external-command`
  configures a command the vault invokes as `<cmd> [args] wrap|unwrap` over
  stdin/stdout to wrap/unwrap the master key; the provider-specific logic and
  credentials live in that command, so the vault adds no new dependency (ADR
  D-015). `-seal-external-arg` (repeatable) and `-seal-external-timeout` tune it;
  a failing or slow command leaves the vault sealed (fail-safe). Joins the static
  KEK and transit seals behind the same interface.

[1.5.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.5.0
[1.4.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.4.0
[1.3.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.3.0
[1.2.1]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.2.1
[1.2.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.2.0
[1.1.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.1.0
[1.0.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.0.0
[1.0.0-rc.2]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.0.0-rc.2
[1.0.0-rc.1]: https://github.com/cwolsen7905/ubixvault/releases/tag/v1.0.0-rc.1
[0.2.0-beta.12]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.12
[0.2.0-beta.11]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.11

## [0.2.0-beta.10] — 2026-08-19

Tenth beta: a dedicated Kubernetes liveness endpoint.

### Added

- **`GET /v1/sys/livez`** — a liveness endpoint that returns `200` whenever the
  HTTP server is serving, regardless of init/seal state. Unlike `/v1/sys/health`
  (whose status encodes *readiness* — `501` uninitialized, `503` sealed), `livez`
  is safe as a Kubernetes liveness probe: it never fails during the normal sealed
  window, so it will not crash-loop a sealed vault, yet it still verifies the HTTP
  server actually responds (which a bare TCP probe cannot). Unauthenticated and
  audit/rate-limit-exempt, like `/v1/sys/health`.

### Changed

- **Helm chart 0.1.11** — the `livenessProbe` and `startupProbe` now `httpGet`
  `/v1/sys/livez` instead of a bare `tcpSocket` check. This removes the recurring
  `TLS handshake error … EOF` log noise (a TCP probe against the TLS port never
  completes a handshake) and detects a hung-but-listening server. Readiness
  continues to use `/v1/sys/health`. The chart now requires **>= 0.2.0-beta.10**;
  do not deploy chart 0.1.11 against an older image (the probe would 404).

## [0.2.0-beta.9] — 2026-08-11

Ninth beta: production-oriented storage & operations — durable database storage,
live unseal-share rotation, and scheduled backups.

### Added

- **MySQL/MariaDB storage backend** — run the vault against a database instead of
  a local disk with `-storage mysql` (`-storage-mysql-dsn` or
  `$UBIXVAULT_STORAGE_DSN`); `file` remains the default. The node becomes
  replaceable: it can die and restart against the same durable, replicated
  database. The vault creates its tables automatically. Values stay barrier
  ciphertext — the database never sees plaintext, so a database or DSN compromise
  yields ciphertext, not secrets. Single active writer (durability, not multi-writer
  HA). Helm: `storage.type=mysql` with the DSN supplied via a Secret
  (`storage.mysql.dsnSecret`), never a chart value; chart bumped to 0.1.10. No new
  dependency (reuses the MySQL driver). See ADR D-014 and
  `docs/design/sql-storage-backend.md`.

- **Rekey** — rotate the Shamir unseal shares without downtime or data
  re-encryption. `POST /v1/sys/rekey/init` starts an attempt (new share
  count/threshold), `POST /v1/sys/rekey/update` feeds a quorum of the *current*
  shares, and on completion a fresh set of unseal shares is returned once; the
  old shares stop working. Internally the master key is regenerated and the
  barrier keyring re-wrapped under it — the barrier key and all data are
  untouched, so the vault keeps serving throughout. Shamir-unseal vaults only.
  Driveable from the CLI: `ubixvault operator rekey init | update | status | cancel`.
- **Scheduled backups (Helm chart)** — an opt-in `backup.enabled` CronJob runs
  `operator snapshot save` against the running vault on a schedule and writes the
  snapshot to a separate PVC (put it on network-backed storage for off-node
  durability), using a least-privilege `sys/snapshot` token. Chart bumped to
  0.1.9; the README documents the token/policy setup and the restore procedure.

[0.2.0-beta.9]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.9

## [0.2.0-beta.8] — 2026-08-06

Eighth beta: Transit grows into full crypto-as-a-service, plus response wrapping.

### Added

- **Response wrapping** — `POST /v1/sys/wrapping/wrap` stores a JSON payload
  behind a fresh single-use token (TTL from the `X-Vault-Wrap-TTL` header,
  default 5m, max 24h), and `POST /v1/sys/wrapping/unwrap` returns it exactly
  once before destroying the token. Wrapped payloads are barrier-encrypted and
  indexed by the token's hash. This is the secure-introduction pattern: hand a
  consumer a short-lived token instead of the secret itself.

- **Transit rewrap & data keys** — `POST /v1/transit/rewrap/{name}` re-encrypts a
  ciphertext under a key's latest version without exposing the plaintext, so old
  key versions can be retired after a rotation. `POST /v1/transit/datakey/{plaintext|wrapped}/{name}`
  generates a random 128/256/512-bit data key wrapped under the named key — for
  envelope encryption where the caller encrypts bulk data locally and stores only
  the wrapped key.
- **Transit HMAC** — `POST /v1/transit/hmac/{name}` computes an HMAC over the
  input using the key's latest version (sha2-256/384/512; default sha2-256), and
  `POST /v1/transit/verify/{name}` checks one in constant time. The MAC is
  version-tagged so it keeps verifying across key rotations.
- **Transit signing keys** — Transit keys can now be asymmetric signing keys
  (`ecdsa-p256/384/521`, `ed25519`), selected with `{"type":...}` at creation.
  `POST /v1/transit/sign/{name}` signs input (ECDSA hashes with `sha2-256/384/512`;
  Ed25519 signs directly) and `POST /v1/transit/verify/{name}` checks a
  `signature` (or an `hmac`). A key's PEM public keys are returned per version so
  signatures can be verified without the vault. Symmetric-only operations
  (encrypt/decrypt/hmac) reject signing keys and vice versa.

[0.2.0-beta.8]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.8

## [0.2.0-beta.7] — 2026-07-30

Seventh beta: JWT/OIDC login, completing the auth-method set.

### Added

- **JWT/OIDC auth method** — exchange a signed JWT for a token
  (`POST /v1/auth/jwt/login`). Signatures are verified with the standard library
  (RS256/384/512 and ES256/384/512) against static PEM public keys and/or a
  fetched JWKS — no new dependency. Configure validation under
  `/v1/auth/jwt/config` (JWKS URL, validation public keys, bound issuer) and
  define roles under `/v1/auth/jwt/role/{name}` that bind audiences and claims to
  a policy set and token TTL. Login validates `exp`/`nbf`, the bound issuer,
  audiences, and per-claim bindings before issuing the token.

[0.2.0-beta.7]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.7

## [0.2.0-beta.6] — 2026-07-30

Sixth beta: human login (userpass) and certificate management from the console.

### Added

- **userpass auth method** — human login with a username and password
  (`POST /v1/auth/userpass/login/{username}`) returning a token with the user's
  policies. Passwords are stored only as a PBKDF2-HMAC-SHA256 hash (600k
  iterations, per-user salt) via the Go 1.24 stdlib `crypto/pbkdf2` — no new
  dependency; login compares in constant time and equalizes timing for unknown
  users. User management under `/v1/auth/userpass/users/*`.
- **PKI in the web console** — the `/ui/` console gains a PKI panel: generate or
  view the root CA, manage roles (allowed domains, subdomains, max TTL, key
  type), and issue certificates. Issued cert/key/CA render as copy-able PEM
  blocks, with a "shown once" warning on the private key.

[0.2.0-beta.6]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.6

## [0.2.0-beta.5] — 2026-07-29

Fifth beta: an internal certificate authority (PKI), self-hosted transit
auto-unseal, and security hardening.

### Security

- Internal security-review pass. **Hardened:** `500` responses now return a
  generic message and log the detail server-side, instead of echoing internal
  error strings to clients. Documented that seal secrets should be passed by
  environment variable rather than a CLI flag (visible in `ps`). Verified no ACL
  bypass via path traversal, no weak randomness, path-bound AEAD with random
  nonces, constant-time recovery-key checks, hash-indexed tokens, and
  fail-closed audit.

### Added

- **PKI secrets engine** — an internal certificate authority: generate a
  self-signed root CA (its key never leaves the vault), define roles that
  constrain issuance (allowed domains, subdomains, max TTL, key type), and issue
  short-lived leaf certificates. Vault-compatible paths under `/v1/pki/*`
  (`root/generate/internal`, `roles/{name}`, `issue/{role}`, `ca`). Built on
  `crypto/x509`; no new dependency. (Intermediate CAs and CRL are future work.)
- **Transit auto-unseal seal** — unseal by wrapping the master key via another
  Vault-compatible Transit engine (`-seal-transit-address/-key/-token`), so no
  KEK lives on this host. Introduces a `Seal` interface (`internal/seal`) with
  static-KEK and Transit implementations (ADR D-013); recovery keys work in both
  modes. Chart: `sealTransit.*` (mutually exclusive with `autoUnseal`). No new
  dependency — the Transit paths are Vault-compatible.

[0.2.0-beta.5]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.5

## [0.2.0-beta.4] — 2026-07-29

Fourth beta: machine authentication and API hardening.

### Added

- **AppRole auth method** — machine clients present a stable `role_id` and a
  `secret_id` (password-equivalent, stored only as a hash) to
  `POST /v1/auth/approle/login` and receive a token carrying the role's policies.
  Roles set policies, token TTL, and an optional secret-id TTL; role/secret-id
  management is authenticated, login is not. Vault-compatible paths under
  `/v1/auth/approle/*`.
- **Rate limiting** — optional per-client token-bucket throttling of the API
  (`-rate-limit`, `-rate-limit-burst`, `-rate-limit-trust-forwarded`), in-house
  with no new dependency. Keyed by client IP (or `X-Forwarded-For` behind a
  trusted proxy); health, metrics, and the console are exempt; over-limit
  requests get `429` with `Retry-After`. Exposed in the Helm chart via
  `rateLimit.*`.

[0.2.0-beta.4]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.4

## [0.2.0-beta.3] — 2026-07-25

Third beta: the web console gains full read/write management.

### Added

- **Web console — write operations & management** — the `/ui/` console now goes
  well beyond read-only:
  - **KV v2:** create/edit secrets (key/value editor → new version), version
    **history**, read a specific version, and per-version soft-delete / undelete
    / **destroy** (with an inline confirm).
  - **ACL policies:** list, read, create/edit (JSON or HCL), and delete.
  - **Tokens:** mint a child token scoped to policies with an optional TTL.

  Still token-in-header (no cookies/CSRF), strict-CSP, secret values rendered via
  `textContent`, and every write is an audited `/v1` call requiring the token's
  capabilities.

[0.2.0-beta.3]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.3

## [0.2.0-beta.2] — 2026-07-25

Second beta: Kubernetes deployment (Helm chart, image publishing, ingress,
Prometheus metrics, a read-only web console) plus an auto-unseal recovery-key
fix.

### Added

- **Prometheus metrics** — `GET /v1/sys/metrics` exposes operational series
  (build info, seal state, uptime, HTTP request counts) in Prometheus text
  format, via an in-house exporter (no new dependency, ADR D-012). The Helm
  chart can create a `ServiceMonitor` (`metrics.serviceMonitor.enabled`).
- **Auto-unseal recovery keys** — auto-unseal `init` now generates *k-of-n*
  recovery keys (default 5/3) and returns them once. The KEK still unseals the
  vault automatically; the recovery keys authorize **root-token regeneration**,
  closing a lockout gap where a lost root token was unrecoverable under
  auto-unseal. `POST /v1/sys/generate-root/*` accepts recovery keys in
  auto-unseal mode. (Vaults initialized before this have no recovery keys.)
- **Helm chart** (`deploy/charts/ubixvault`), multi-arch image publishing to
  ghcr.io, an optional chart Ingress, and TLS options for the operator CLI
  (`-ca-cert`, `-tls-skip-verify`).
- **Web console (read-only)** — an embedded, self-contained admin console at
  `/ui/` (with `/` redirecting to it). It shows the vault's seal state and
  reads/lists KV v2 secrets with a token the operator supplies in the browser.
  Vanilla HTML/JS/CSS served from the binary under a strict CSP (no external
  assets, no new dependency).

[0.2.0-beta.2]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.2

## [0.2.0-beta.1] — 2026-07-23

Beta: hardening and completeness on top of the MVP. uBix Vault is now usable for
real workloads (see `docs/DEPLOYMENT.md`), though it has not had an external
security review.

### Added

- **Token TTLs, expiry, and renewal** — tokens now expire (default TTL; explicit
  TTL on create; root non-expiring). The auth middleware rejects expired tokens;
  `POST /v1/auth/token/renew-self` extends a token.
- **Auto-unseal** — protect the master key with a 32-byte KEK instead of Shamir
  shares, so a restarted server unseals itself (`-auto-unseal-key`).
- **Health/readiness endpoint** — `GET /v1/sys/health`, with a readiness status
  code (200/503/501) for load balancers and probes.
- **Backup / restore** — consistent, encrypted snapshots via
  `POST /v1/sys/snapshot` and `operator snapshot save`/`restore`.
- **Root-token regeneration** — recover a new root token from a quorum of unseal
  shares (`/v1/sys/generate-root/*`).
- **Lease renewal, lookup, and cascading revocation** — dynamic-database leases
  can be renewed/looked up, and revoking a token (`revoke-self`) revokes the
  credentials it created.
- **Kubernetes auth method** — pods exchange a ServiceAccount token for a
  policy-scoped token (`/v1/auth/kubernetes/*`), validated via the TokenReview API.
- **HCL policy documents** — accept HashiCorp-style HCL policies in addition to
  JSON (auto-detected), via an in-house parser (no new dependency).
- **Deployment guide** (`docs/DEPLOYMENT.md`).

### Changed

- **TLS hardening** — without TLS the server binds to loopback only; serving
  plaintext HTTP on a non-loopback address is refused unless `-dev-no-tls` is set.
- Seal status now reports the seal `type` (`shamir` or `auto`).

### Known limitations

- Not production-hardened; no external security review.
- Auto-unseal takes the KEK directly (a pluggable cloud-KMS/HSM seal is future
  work); root regeneration is Shamir-only.
- The in-house HCL parser is a policy-grammar subset, not full HCL.
- Cascading revocation and lease renewal cover dynamic-database leases.

[0.2.0-beta.1]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.2.0-beta.1

## [0.1.0] — 2026-07-19

First release: the complete MVP core (see `docs/DESIGN.md` §6). uBix Vault can be
initialized, unsealed, and used to store and generate secrets over an
authenticated, authorized, audited HTTP API.

> **Status:** working MVP, not yet production-hardened. No external security
> review or operational hardening yet.

### Added

- **Storage** — a durable key/value backend interface with file and in-memory
  implementations, path-traversal-safe and covered by a shared conformance suite.
- **Barrier** — AES-256-GCM encryption at rest; the storage path is bound into
  the ciphertext (AEAD additional data) so blobs cannot be relocated between
  paths.
- **Shamir seal/unseal** — in-house Shamir Secret Sharing over GF(2⁸) with
  constant-time field arithmetic, validated against FIPS-197 vectors. The master
  key is split into k-of-n unseal shares.
- **Core** — initialization and the seal/unseal lifecycle, issuing the initial
  root token at init.
- **Token authentication** — tokens indexed by a hash of their value (never
  stored in the clear); root and scoped tokens.
- **ACL policies** — JSON policy documents, default-deny with deny-override,
  exact and prefix path matching.
- **KV v2 secrets engine** — versioned secrets with soft-delete, undelete,
  destroy, and max-versions aging.
- **Transit secrets engine** — encryption-as-a-service; versioned keys that never
  leave the vault and rotate without breaking existing ciphertext.
- **Dynamic database secrets engine** — short-lived credentials via a
  `DatabasePlugin` interface, with a MariaDB reference plugin; leases are revoked
  on expiry by a background sweeper.
- **Audit logging** — a fail-closed file device recording who accessed what,
  with the client token HMAC'd (never logged in the clear).
- **HTTP API** — Vault-path-compatible endpoints for the above, behind an
  authentication + ACL middleware.
- **Server & operator CLI** — `ubixvault server` runs the API (with optional TLS
  and audit logging); `ubixvault operator init/unseal/seal-status/seal` drives
  the lifecycle over the API.
- **Engineering** — CI running build, race tests, `golangci-lint` (incl. gosec),
  `govulncheck`, and a MariaDB integration job; design docs, decision records
  (ADRs), and a threat model in `docs/`.

### Known limitations

- Not production-hardened; no external security review.
- The lease manager covers dynamic-secret leases (TTL + revoke + expiry sweep);
  general renew and cross-type cascading revocation are future work.
- Policies are JSON only (HCL parity pending); Transit sign/HMAC and asymmetric
  keys are not yet implemented.
- The in-memory unseal progress and the audit device's HMAC salt are
  per-process; cross-restart correlation of audit entries is future work.

[0.1.0]: https://github.com/cwolsen7905/ubixvault/releases/tag/v0.1.0
