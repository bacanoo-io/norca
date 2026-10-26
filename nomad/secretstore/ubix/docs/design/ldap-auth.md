# Design note: LDAP / Active Directory auth — and the dependency question

> **Status:** Accepted — **option A chosen** (add `go-ldap/ldap/v3`) · 2026-09-02
> · ADR [D-018](../DECISIONS.md). Relates to `docs/ROADMAP.md` "Toward Vault
> Community parity" and the minimal-dependency posture (D-009/D-010/D-014).

## What LDAP auth is

Log in with an LDAP/Active-Directory username and password: the vault **binds**
to the directory as that user (proving the password), optionally **searches**
for the user's group memberships, and maps the user and groups to policies —
naturally via the identity layer's external groups (D-016, phase 3), with the
LDAP groups fed in exactly like an OIDC `groups` claim.

It is the one remaining Vault-**Community** auth method uBixVault lacks. For
organizations running AD/LDAP without an OIDC bridge, it is the auth method they
actually use.

## The tension

Every other engine and auth method in uBixVault is stdlib-only or nearly so —
the project has **exactly one** direct dependency (`go-sql-driver/mysql`) and
sells that "readable in an afternoon, essentially no dependencies" posture as a
feature. LDAP is the first capability that does not have a clean stdlib path:
**Go's standard library has no LDAP client**, and LDAP is ASN.1 **BER** over TCP
(TLS). `encoding/asn1` speaks DER (a BER subset) and does not cover the BER
constructs LDAP uses, so there is no free ride.

So the decision is not *how* to write LDAP — it is *whether* LDAP is worth
changing the dependency story, and if so, by how much.

## Options

### A. Add `github.com/go-ldap/ldap/v3` (the standard Go LDAP client)

The de-facto library; mature, widely used, handles bind/search/StartTLS/controls.

- **Cost:** the project's **second** direct dependency, and it pulls a small
  transitive graph — its ASN.1-BER codec (`go-asn1-ber/asn1-ber`) plus a couple
  of small helpers (NTLM/SSPI, a UUID package). All small and well-scoped, but it
  is more than one new module, in the auth path.
- **Benefit:** LDAP done right, quickly, with none of the protocol risk on us.
  The BER/ASN.1 wire handling — the genuinely error-prone part — is battle-tested
  code, not something we hand-roll in a security-critical login flow.

### B. Hand-roll a minimal LDAP client (stdlib only)

Implement just simple **bind** + **search** over `crypto/tls`, encoding the BER
messages by hand.

- **Cost:** we write ASN.1-BER encoding/decoding for the LDAP PDUs ourselves —
  security-sensitive parser code in the login path, precisely the kind of thing
  the project has *avoided* hand-rolling elsewhere (it uses stdlib crypto, and
  reached for the MySQL driver rather than hand-rolling the MySQL protocol,
  D-010). Real risk of subtle bugs; ongoing maintenance burden.
- **Benefit:** keeps the one-dependency count. Preserves the purity claim.
- **Assessment:** the purity is not worth the risk here. Hand-rolled BER in an
  auth path is a worse security posture than a vetted library — the opposite of
  what the minimal-dependency ethos is *for*.

### C. External-command bind helper (delegate, like the KMS/HSM seal, D-015)

The vault shells out to an operator-supplied command that performs the LDAP
bind/search and returns the result.

- **Cost:** user credentials transit the command's stdin on every login (a login
  is far hotter and more sensitive than the seal's once-per-boot master-key wrap);
  operators must supply and secure the helper; awkward in the shell-less
  distroless image. The seal case worked because it is rare and the payload is a
  fixed 32-byte key — neither holds for interactive logins.
- **Assessment:** the exec-delegation trick that fit the seal does **not** fit a
  login flow. Not recommended.

### D. Don't add LDAP; steer to OIDC

Many directories are already fronted by an OIDC provider (Keycloak, Entra ID,
Okta, Authentik, Dex-over-LDAP), which uBixVault already supports — with group
mapping via the phase-3 external groups.

- **Cost:** organizations with *only* raw LDAP/AD and no OIDC bridge are
  unserved; the Community-parity checklist keeps one unchecked box.
- **Benefit:** zero dependency change; the ethos is untouched. A documented
  "front LDAP with OIDC" recipe covers a large share of real deployments.

## Recommendation

**A if we want LDAP at all; otherwise D — and explicitly not B or C.**

The choice is really a product/philosophy call that is yours to make:

- If LDAP-without-OIDC is a target audience, take **A**. Adding one vetted,
  widely-used library for a genuinely protocol-heavy, security-sensitive feature
  is exactly when a dependency earns its place — the same reasoning that admitted
  the MySQL driver (D-010). Record it as such; keep the count honest (two
  direct dependencies) and scope LDAP to that one library.
- If the "essentially no dependencies" story is worth more than serving raw-LDAP
  shops, take **D**, document the OIDC-fronting recipe, and leave the box
  unchecked deliberately.

Either is defensible. **B is not** (hand-rolled BER in the auth path is the wrong
risk) and **C is not** (credential-over-exec per login).

## If A is chosen — implementation sketch (for a follow-up PR)

- New `internal/ldapauth` method mirroring the other auth methods: `Configure`
  (URL, StartTLS/LDAPS, bind DN + password or anonymous, user search base +
  filter, group search base + filter), `WriteRole`/login by username+password.
  TLS is verified against the system roots, or — for directories with their own
  CA (FreeIPA's Dogtag, an AD enterprise CA) — against a PEM bundle in
  `certificate` (Vault's field name), which then replaces the system roots for
  that connection. `insecure_tls` turns verification off and cannot be combined
  with `certificate`.
- Login: dial with `crypto/tls` (or StartTLS), bind, search the user, optionally
  bind-as-user to verify the password, search groups, then
  `tokens.CreateWithTTLAndAlias(..., "ldap", username, groups)` — the asserted
  groups flow straight into identity external groups, no new plumbing.
- API `/v1/auth/ldap/*`; a CHANGELOG entry; the ADR moved to Accepted with the
  dependency recorded.
