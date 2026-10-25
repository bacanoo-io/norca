# Design note: identity — entities, aliases, and groups

> **Status:** Accepted · **complete** — all four phases (entities + aliases +
> entity policies; internal groups; external/IdP-asserted groups; policy
> templating) shipped 2026-09-02 · relates to ADR [D-016](../DECISIONS.md), ADR
> [D-017](../DECISIONS.md) (templating, [design note](identity-templating.md)),
> and `docs/ROADMAP.md` "Toward Vault Community parity".

## Problem

uBixVault has six ways to authenticate — token, AppRole, Kubernetes, userpass,
JWT/OIDC, and TLS client certificate. Each is self-contained: a successful login
mints a token carrying exactly the policies named on the role that authorized it.
There is no notion of *who* is behind a login. Concretely:

- The same human or workload that logs in through two methods (say userpass at a
  laptop and JWT/OIDC from CI) gets two unrelated tokens with two independently
  managed policy sets. Nothing ties them to one subject.
- Policy assignment is per-auth-role. To grant "the platform team" a new path you
  edit every role every one of them logs in through, in every method.
- There is no way to say "everyone who presents an OIDC token whose `groups`
  claim contains `platform` gets these policies" — group membership asserted by
  the IdP is discarded once the role's static policy set is attached.

HashiCorp Vault solves this with its **Identity** system: an *entity* is the
canonical subject, *aliases* map each auth-method login to an entity, and
*groups* collect entities (with their own policies), optionally mirroring an
external IdP's groups. Entity and group policies are merged into the token's
policy set at request time. This is the single largest "feels incomplete versus
Vault Community" gap in uBixVault (see `docs/POSITIONING.md`).

## Goals

- One **entity** per subject; multiple auth logins (**aliases**) resolve to it.
- **Entity policies** and **group policies** are added to whatever the auth role
  already grants, evaluated fresh on each request (a group edit takes effect
  without re-issuing tokens).
- **Internal groups** (membership managed in uBixVault) and **external groups**
  (membership asserted by the auth method — e.g. an OIDC `groups` claim, a
  Kubernetes namespace) so IdP-side grouping drives policy.
- **Zero new dependency** — the whole thing is storage records plus a resolver on
  the existing barrier, consistent with D-009/D-010/D-014.

## Non-goals (for the first cut)

- **Identity templating in ACLs** (`{{identity.entity.id}}` / metadata in policy
  paths). Valuable, but it touches the policy engine, not just auth; deferred to a
  follow-up phase so the core subject model can land first.
- **Entity merging / MFA on identity / control groups.** Out of scope; some are
  Enterprise-tier in Vault anyway (`docs/POSITIONING.md`).
- **Cross-cluster identity.** Single node, like everything else today.

## Data model

Three record types, all stored in the barrier under `identity/` (encrypted at
rest, keyed by ID):

```
Entity {
  ID        string            // server-generated, stable
  Name      string            // unique, human-facing
  Policies  []string          // attached directly to the entity
  Metadata  map[string]string
  Disabled  bool              // disabled → contributes no policies (phase 1); hard login-block later
}

Alias {
  ID           string
  EntityID     string
  MountType    string   // the auth method: "userpass", "jwt", "cert", ...
  Name         string   // the method-side login name (username, JWT subject, CN)
  Metadata     map[string]string
}

Group {
  ID              string
  Name            string
  Policies        []string
  MemberEntityIDs []string
  MemberGroupIDs  []string   // groups may nest
  Type            string     // "internal" | "external"
  Metadata        map[string]string
}
```

Indexing (all plain — names are not secrets, and the barrier does not encrypt
keys):

- `identity/entity/<id>` → Entity; `identity/entity-name/<name>` → id (uniqueness).
- `identity/alias/<id>` → Alias; `identity/alias-index/<mountType>/<name>` → id,
  so a login resolves to an alias in one lookup.
- `identity/group/<id>` → Group; `identity/group-name/<name>` → id.

## Integration point

The token gains one field, `EntityID` (empty for tokens with no identity, e.g.
the root token and bare `auth/token/create`). Two touch points:

1. **At login.** Every auth method already ends by calling the token store to
   mint a token. It instead calls an identity resolver with `(mountType, name)`:
   - look up the alias index; if absent, **auto-create** an entity + alias (Vault
     does this too — first login through a method materializes the subject),
     unless the deployment turns auto-creation off;
   - stamp the resolved `EntityID` onto the new token.
   The role's own policies are still attached to the token as today — identity
   *adds*, it never subtracts.

2. **At authorize time.** `authorize()` (in `internal/api/auth.go`) currently
   loads the policies named on the token. It additionally, when the token has an
   `EntityID`: loads the entity's policies, the policies of every group the
   entity belongs to (transitively, via `MemberGroupIDs`), and — for external
   groups — the groups implied by the token's auth metadata. The union is fed to
   the same `policy.NewACL(...)`. Resolving on each request (not snapshotting into
   the token) is what makes a group edit effective immediately; a small
   per-request cache keyed by `EntityID` keeps the cost down.

External-group membership needs the auth method's asserted attributes (the OIDC
`groups` claim, the Kubernetes namespace/service-account) to reach `authorize`.
The cleanest carrier is alias/entity metadata written at login, matched against
each external group's configured `(mountType, metadata-key, value)`. That keeps
`authorize` reading only stored state and avoids threading raw claims through the
request context.

## Phasing

Small, independently shippable PRs, each CI-green, in the established pattern
(engine/store + API handlers + `sys.go` wiring + tests + CHANGELOG):

1. **Entities + aliases + entity policies.** The token `EntityID` field, the
   resolver, login auto-creation, `/v1/identity/entity` and
   `/v1/identity/entity-alias` CRUD, and the `authorize` union. Delivers
   "multiple logins, one subject, shared policies" — the core value.
2. **Internal groups.** `/v1/identity/group` CRUD, transitive membership in the
   resolver.
3. **External groups.** Alias metadata capture in each auth method, external-group
   matching in the resolver. Delivers "IdP groups drive policy."
4. **Identity templating in ACLs** (its own note) — `{{identity.*}}` in policy
   paths, so a single policy grants each subject its own subtree.

## Trade-offs and risks

- **Per-request policy resolution cost.** Entity + group lookups on every
  authenticated call. Mitigated by an in-memory cache invalidated on
  entity/group writes; the barrier read is already the norm for policies.
- **Revocation semantics.** Revoking a token does not disable the entity; a new
  login mints a new token for the same subject. Disabling an entity (or removing
  the alias) blocks future logins but does not retroactively revoke live tokens —
  same model as Vault, and consistent with our TTL-bounded tokens. Worth stating
  explicitly in the docs.
- **Auto-created entity sprawl.** First login through any method materializes an
  entity; over time these accumulate. Vault has the same behavior; entity listing
  + delete and an opt-out flag are enough for the first cut.
- **Policy escalation surface.** Identity can only *add* policies, but that still
  means group membership is now a privilege-granting edge. Group writes are
  root/ACL-gated like policy writes, and the audit log records them.

## Alternatives considered

- **Snapshot merged policies into the token at login** (instead of resolving each
  request). Simpler and cache-free, but a group or entity policy change would not
  reach already-issued tokens until they expire — surprising, and the opposite of
  what operators expect from a group edit. Rejected.
- **Do nothing; keep policy on auth roles.** Leaves the parity gap and the
  per-role policy-duplication toil. Rejected.
- **A dependency on an external identity/OPA service.** Against the
  minimal-dependency, self-contained posture; also just moves the problem.

## Zero-dependency confirmation

Entities, aliases, and groups are JSON records on the existing barrier; the
resolver is Go over the existing `policy` package; external-group matching reads
metadata the auth methods already parse. No third-party library is introduced.
