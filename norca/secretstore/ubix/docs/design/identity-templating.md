# Design note: identity templating in ACL policy paths

> **Status:** Accepted · shipped 2026-09-02 · phase 4 of ADR
> [D-016](../DECISIONS.md) (identity); recorded as ADR [D-017](../DECISIONS.md).
> Builds on `docs/design/identity-entities-groups.md`.

## Problem

Identity (D-016) gives every token a subject — an entity. But a policy still
names concrete paths, so "let each user read and write their own KV subtree"
takes one policy per user:

```json
{ "path": { "secret/data/users/alice/*": { "capabilities": ["create","read","update","delete"] } } }
```

Multiply that by every user and it is unmanageable — the exact toil identity was
meant to remove. HashiCorp Vault solves it with **policy templating**: a policy
path may contain `{{identity.entity.id}}` (and similar), resolved against the
requesting token's entity at evaluation time. One policy then serves everyone:

```json
{ "path": { "secret/data/users/{{identity.entity.name}}/*": { "capabilities": ["create","read","update","delete"] } } }
```

## Decision, in one line

At authorize time, expand `{{identity.*}}` placeholders in each rule's path
against the requesting token's entity, then evaluate the ACL as usual. A
placeholder that cannot be resolved drops that rule (fail-closed), so a templated
grant is worthless to a token with no matching identity value.

## Supported placeholders (phase 4)

- `{{identity.entity.id}}` — the entity's stable ID.
- `{{identity.entity.name}}` — the entity's name.
- `{{identity.entity.metadata.<key>}}` — a metadata value on the entity.

These cover the common "own subtree" and "per-team prefix" patterns. Alias- and
group-scoped placeholders (`{{identity.entity.aliases.<mount>.name}}`,
`{{identity.groups.names}}`) are a later addition if wanted; the expansion
mechanism below already accommodates them — only the value map grows.

## Where it happens

The policy engine stays identity-agnostic. It gains a **generic** expander:

```go
// Policy.Templated returns a copy with {{...}} placeholders in rule paths
// resolved via resolve; a rule whose placeholders don't all resolve is dropped.
// A policy with no placeholders is returned unchanged (the common case, no cost).
func (p *Policy) Templated(resolve func(key string) (string, bool)) *Policy
```

`authorize()` (in `internal/api`) already loads the token's policies (its own
plus entity and group policies — D-016) and builds a `policy.ACL`. It now, when
the token has an entity, asks the identity engine for the entity's **template
values** (a flat `map[string]string` of the placeholders above) and runs each
policy through `Templated` before merging. The identity engine supplies the
values; the policy engine does the string substitution; neither depends on the
other's internals.

Non-entity tokens (root, a bare `auth/token/create` token) have no template
values, so every templated rule drops — root is unaffected because it bypasses
the ACL entirely, and a plain token simply gains nothing from a templated policy.

## Fail-closed, and the injection caveat

- **Unresolved ⇒ dropped.** If a path references `{{identity.entity.metadata.team}}`
  and the entity has no `team` metadata, the rule is removed, not left with an
  empty segment that might match something unintended.
- **Values are inserted literally.** `entity.id` is a hex string and `entity.name`
  is slash-safe by construction, but **metadata values are operator-set and go in
  verbatim** — a metadata value of `*` would widen a prefix match. Templating from
  metadata is therefore only as safe as who can write that metadata (entity writes
  are root/ACL-gated, same as policy writes). Documented, not sanitized, matching
  Vault's model: template from `id`/`name` freely; treat metadata as trusted input.

## Alternatives considered

- **Pre-expand at policy-write time.** Impossible — the value is per-request
  (per-entity); a stored policy serves every subject.
- **Teach the policy package the identity schema.** Couples the two packages for
  no gain; the `resolve` callback keeps the expander generic and testable in
  isolation.

## Zero-dependency confirmation

Placeholder expansion is a small string scan in the existing `policy` package;
the value map comes from the existing `identity` engine. No new dependency.
