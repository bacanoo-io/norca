# Versioning & assurance policy

uBixVault tracks **two independent things** that are easy to conflate. Keeping
them separate is deliberate.

## 1. Version — what the number means

uBixVault follows [Semantic Versioning](https://semver.org/). The version
communicates exactly one thing: **the stability of the public interface** — the
Vault-compatible HTTP API, the `ubixvault` server/operator CLI flags, the storage
format, and the Helm chart values.

- **`MAJOR`** (e.g. `1.x` → `2.0`) — a backward-incompatible change to that
  interface.
- **`MINOR`** (e.g. `1.0` → `1.1`) — new, backward-compatible capability.
- **`PATCH`** (e.g. `1.0.0` → `1.0.1`) — backward-compatible fixes.
- **Pre-release** (`1.0.0-rc.1`, `-beta.N`) — a candidate for the version that
  follows it; lower precedence than the final release.

**`1.0.0` therefore means: the interface is stable and the feature set is
complete, and we commit to the SemVer compatibility rules from here on.** It is
an API-stability contract. It is **not** a security certification, a
production-readiness stamp, or a claim that the cryptography has been audited.
SemVer says nothing about any of those — they are the *assurance* axis below.

## 2. Assurance — what the number does *not* mean

Assurance is tracked **separately from the version**, because it moves on its own
schedule and is not something a maintainer can grant themselves.

**Current assurance status: NOT independently audited.**

The security-critical code — the encryption barrier, Shamir seal/unseal, and all
cryptography — is standard-library Go, written and tested in-house, fuzzed, and
property-tested (see `docs/DESIGN.md` §5 and the roadmap). That is careful
engineering, and it is not the same as an outside review. A secrets manager's
crypto cannot be meaningfully certified by the person who wrote it; the value of
an audit is precisely that someone who is *not* the author, and not invested in
the author being right, tries to break it.

An **independent external security review is an open milestone** (`docs/ROADMAP.md`),
pursued actively — but it is **not a version gate**. When it lands, we flip the
assurance status here, in `SECURITY.md`, and in the release notes; no version
bump is required or implied by it.

Why not hold `1.0` hostage to the audit? Because that would overload the version
number with something SemVer doesn't carry, mislead tooling that parses versions
for compatibility, and indefinitely withhold an honest signal (the API *is*
stable) waiting on an event with no fixed date. Decoupling lets each axis tell the
truth.

## The disclaimer block

Reuse this verbatim in release notes, downstream docs, and anywhere the security
posture needs stating. Keep it current as the assurance status changes.

> **Security assurance:** uBixVault has **not** yet undergone an independent
> third-party security audit. Its cryptography and trust path are standard-library
> Go, written in-house, fuzzed, and property-tested — but that is not a substitute
> for outside review. Until an audit lands, weigh that before placing
> high-blast-radius secrets in it: prefer running it alongside an audited store,
> starting with low-criticality secrets, and making the risk visible to whoever
> owns security. See [`SECURITY.md`](../SECURITY.md) and
> [`docs/ROADMAP.md`](ROADMAP.md).
