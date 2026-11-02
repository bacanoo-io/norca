# uBixVault — Positioning & Prior Art

> **Status:** Active · Last updated 2026-09-24
> Why this project exists alongside HashiCorp Vault and OpenBao.

## Prior art (acknowledged up front)

Secrets management is a mature space with strong existing implementations, and it's worth
being clear about them:

- **HashiCorp Vault** — the reference implementation and uBixVault's API-compatibility
  target. Relicensed from MPL 2.0 to the source-available **BUSL** in 2023; now IBM-owned.
- **OpenBao** — the MPL 2.0, Linux Foundation fork of Vault's last open version. A drop-in,
  actively-maintained, fully open secrets manager. For a production deployment that just
  needs an open Vault, OpenBao is the right choice.

## Why build uBixVault

uBixVault is a lightweight, from-scratch secrets manager with full control over its design
and footprint. It originated to provide secrets management for
[uBixCore](https://github.com/cwolsen7905/uBixCore), but it is framework-agnostic and works with
any stack over its HTTP API. The reasons to build rather than adopt:

- **First-class uBixCore support, without coupling.** Purpose-built to integrate cleanly
  with uBixCore, while remaining a general-purpose secrets manager any project can use.
- **Lightweight and opinionated.** A small, focused core — the parts that matter most —
  instead of the full breadth (and operational weight) of Vault/OpenBao.
- **Full design control.** Owning the barrier, seal/unseal, policy, and lease-lifecycle
  design end-to-end, so the system can evolve with its users' needs.

## Scope philosophy

Depth over breadth: a small, finished, tested, well-documented core rather than a partial
clone chasing feature parity. The committed scope and the optional extensions are separated
explicitly in `docs/ROADMAP.md`, with the reasoning in `docs/DECISIONS.md` (D-004).

## What this project is *not*

- Not a drop-in replacement for the full Vault/OpenBao feature set — see the roadmap's
  committed-vs-extension split.
- Not aiming to displace Vault or OpenBao — for general production use, prefer those.

## Where it sits: Vault Community vs. Enterprise

uBixVault deliberately targets a **subset of Vault _Community_** (the free tier), not
Enterprise — and it does not yet have full Community parity either. That's the point of
"depth over breadth." Enterprise-tier features are, with a couple of exceptions, out of
scope; the long-term catch-up backlog is tracked in `docs/ROADMAP.md` ("Beyond 1.0").

The Community/Enterprise line has shifted over Vault's history — cloud-KMS auto-unseal,
login MFA, and rate-limit quotas all moved from Enterprise to Community. Notably, uBix
Vault's **external-command seal already covers cloud-KMS/HSM auto-unseal** — the capability
that was Enterprise-gated (HSM) for years — reached without any provider SDK.

| Vault Enterprise capability | uBixVault |
| --- | --- |
| HSM / cloud-KMS auto-unseal (+ seal-wrap) | 🟡 KMS/HSM unseal via the external-command seal; no seal-wrap |
| Automated snapshots to cloud storage | 🟡 scheduled backup CronJob (to a PVC, not object storage yet) |
| Namespaces (in-vault multi-tenancy) | ❌ |
| Performance + DR replication; performance standbys | ❌ |
| Sentinel policies (ABAC / EGP / RGP) | ❌ — ACL (JSON + HCL) only |
| Control Groups (M-of-N approval) | ❌ |
| Managed Keys; Key Management engine (→ cloud KMS) | ❌ |
| KMIP secrets engine | ❌ |
| Transform engine (tokenization / FPE / masking) | ❌ |
| FIPS 140-2/-3 builds; entropy augmentation | ❌ |
| Lease-count / resource quotas | ✅ rate-limit and lease-count quotas, path-scoped, Vault-compatible (1.1, D-020) |

Of ~15 Enterprise-differentiated capabilities, uBixVault has ~1 and a partial analog of ~2,
and is missing ~12 — **by design.** The gaps that matter more for the project are the
remaining *Community* ones (integrated Raft storage, more auth methods and DB plugins), also
tracked in the roadmap; the real open milestone is an external security review.

**High availability** is a Community feature in Vault, and uBixVault has it since 1.2:
active/standby replicas over MySQL/MariaDB — every replica unseals, one holds a fenced lock
in the database and serves, standbys forward to it over their own mutual-TLS cluster port,
and `sys/leader`, `sys/step-down` and the `sys/health` standby codes behave as Vault's do
(D-021). What it does not have is Vault's *integrated storage* (Raft) — HA here relies on the
database's own availability — nor Enterprise *performance standbys* that answer reads.
