# Design note: cloud-KMS / HSM auto-unseal via an external command

> **Status:** Proposed · 2026-08-26 · relates to ADR [D-015](../DECISIONS.md) and
> `docs/ROADMAP.md` Tier 2.

## Problem

uBixVault auto-unseals by wrapping its master key behind a `Seal`
(`internal/seal`). Two seals exist today:

- **StaticKEK** (`type: auto`) — a 32-byte key-encryption key held locally
  (`-auto-unseal-key`). Simple, but the KEK lives on the host.
- **Transit** (`type: transit`) — wraps the master key via a remote
  Vault-compatible Transit engine, so the wrapping key never touches this host.

The remaining production gate on the roadmap is *"the KEK is supplied directly."*
The Transit seal already removes that **if** you run a Vault-compatible transit
engine. What is missing is reaching a **cloud-native KMS** (AWS KMS, GCP Cloud
KMS, Azure Key Vault) or a **hardware HSM (PKCS#11)** — the key custody most
organizations actually have — without the master key ever sitting on the host.

The design question is not *whether* to support KMS/HSM, but *how* to do it
**without adding a dependency**. uBixVault has exactly one third-party library
(the MySQL driver); pulling in the AWS/GCP/Azure SDKs — each a large transitive
graph — would blow a hole in the "readable in an afternoon" posture (D-009,
D-010, D-014) precisely in the security-critical seal path.

## Decision, in one line

Add a generic **external-command (exec) seal**: uBixVault pipes the master key
to an operator-supplied command that performs the wrap/unwrap against whatever
KMS or HSM the operator uses. The KMS-specific code and credentials live in that
command, **not in uBixVault** — so any KMS or HSM is reachable with zero new
dependency in the vault.

## Why an exec seal, not native SDK seals

Alternatives, and why they lose:

- **Native cloud-KMS SDK seals** (import `aws-sdk-go`, `cloud.google.com/go/kms`,
  Azure SDK). This is what HashiCorp Vault does. It is the most turnkey, but each
  SDK is a heavy transitive dependency, and we would need three of them to cover
  the field — directly against the project's defining constraint. Rejected as the
  default; could be added later as an optional, build-tagged integration, its own
  ADR, with the dependency recorded there.
- **KMS over raw HTTP, in the vault.** Avoids the SDK but requires hand-rolling
  each provider's request signing (AWS SigV4, GCP/Azure OAuth) and error handling
  — a large, per-provider, security-sensitive surface to write and maintain.
  Rejected.
- **PKCS#11 for HSMs, in the vault.** Needs a CGo PKCS#11 binding, which adds a
  dependency *and* breaks the static, distroless, CGo-free build. Rejected.
- **"Just use the Transit seal."** Excellent when you already run a
  Vault-compatible transit engine (another uBixVault, HashiCorp Vault, OpenBao),
  and it stays the recommended path there. But it does not reach cloud-native KMS
  or a local HSM directly. The exec seal complements it.

The exec seal wins because the provider-specific logic — the part that would be a
dependency — is **delegated to a process the operator already trusts and
controls**, reached through the tiny, universal contract below. One mechanism
covers AWS, GCP, Azure, PKCS#11 HSMs, Vault transit, and anything bespoke.

## The contract

A new seal `type: external`. The operator configures a command; the vault invokes
it in two modes:

```
<command> wrap     # reads plaintext master key on stdin, writes wrapped blob to stdout
<command> unwrap   # reads wrapped blob on stdin, writes plaintext master key to stdout
```

- **stdin/stdout carry raw bytes** (the master key is 32 bytes; the wrapped blob
  is whatever the KMS returns, e.g. an AWS KMS `CiphertextBlob`). No encoding is
  imposed on the wire; the command owns the format of its own wrapped output.
- **Exit status 0 = success.** Any non-zero exit, or a write to stderr, is a
  failure; the wrapped/plaintext bytes are taken only from stdout on success.
- **The wrapped blob is opaque to the core**, exactly like the other seals — the
  core stores it and passes it back verbatim to `unwrap`. No format assumptions.
- **A configurable timeout** bounds each invocation; on timeout or non-zero exit
  the operation fails, and (as with the Transit seal) an unreachable KMS at
  startup leaves the vault **sealed** — fail-safe, never fail-open.

This maps directly onto the existing `Seal` interface — `Wrap`/`Unwrap` shell out
— so nothing above the seal changes. `isAutoSeal` gains the new type; recovery
keys and root regeneration already cover every auto-seal uniformly.

### Direct wrap, not envelope encryption

The master key is 32 bytes — comfortably under every KMS's direct-encrypt limit
(AWS KMS allows 4 KB). So the command KMS-encrypts the master key directly; no
envelope/data-key indirection is needed. (Envelope encryption matters when
wrapping large payloads, which this is not.) This keeps the reference wrappers
one-liners.

## Reference wrappers (documentation, not vault code)

The whole point is that these live *outside* the binary. The deployment docs will
ship small examples the operator drops in:

- **AWS KMS:** `aws kms encrypt --key-id "$KEY" --plaintext fileb:///dev/stdin --query CiphertextBlob --output text | base64 -d` for wrap; `aws kms decrypt` for unwrap.
- **GCP Cloud KMS:** `gcloud kms encrypt --key "$KEY" --plaintext-file - --ciphertext-file -` and the matching `decrypt`.
- **Azure Key Vault, PKCS#11 HSM (`pkcs11-tool` / `openssl pkeyutl -engine pkcs11`), or a bespoke service** — the same shape.

Each is a ~5-line script. Its cloud credentials come from the ambient environment
(instance role, workload identity, a mounted key), never from the vault.

## Configuration & wiring

- Server flags: `-seal-external-command <path>` plus optional `-seal-external-arg`
  (repeatable) and `-seal-external-timeout`. Mutually exclusive with the other
  seal flags (the CLI already rejects combining seal modes).
- The command inherits a controlled environment; provider credentials are the
  operator's to supply to it (env, instance metadata, mounted secret).
- Helm: a `sealExternal` block — the command is delivered via a `ConfigMap`
  (script) or an image that already contains the provider CLI, mounted and
  referenced. This is the one wrinkle: the distroless vault image has no shell or
  cloud CLI, so the wrap command must be provided as a sidecar-less mounted
  binary/script the vault can exec, or the deployment uses an image variant that
  bundles it. The docs will spell this out; it does not change the vault.

## Security considerations

- **The master key transits the command's stdin/stdout.** It is 32 bytes, lives
  only in an in-memory pipe and the child process for the duration of an
  unseal, and the child is in the **same trust domain** as the vault (the operator
  already trusts the vault binary and its host). This is a real but bounded
  exposure — no worse than the StaticKEK seal holding the KEK in the same process,
  and strictly better than a KEK persisted on the host, since the wrapping key
  lives in the KMS/HSM. The wrap command must be **minimal and trusted**; the docs
  will say so plainly.
- **Fail-safe:** a missing, failing, or slow command means the vault stays sealed.
  It never falls back to an unprotected key.
- **No new attack surface in the vault:** the vault runs one configured executable
  with a fixed argument vector (no shell interpolation of untrusted data), passes
  bytes on stdin, and reads bytes from stdout. Provider auth, network calls, and
  parsing all live in the operator's command.
- **Provenance:** pairs with the signed-image/SBOM supply chain (D-… image
  signing) — operators should likewise pin and trust the wrap command.

## Testing plan

- Unit-test the exec seal against a **stub command** (a tiny script, or a Go test
  binary) that implements the wrap/unwrap contract with a local reversible
  transform — asserting round-trip (`Unwrap(Wrap(x)) == x`), that a wrong/failing
  command surfaces an error (not a panic, not fail-open), and that a timeout is
  enforced.
- Reuse the existing seal test shape (`internal/seal`), which already exercises
  StaticKEK and Transit round-trips.
- An end-to-end core test: init an auto-seal vault with the stub exec seal, seal,
  and confirm it auto-unseals — the same path the Transit seal test uses.

## Non-goals

- **Native cloud SDK seals** (in-vault AWS/GCP/Azure). Possible later as an
  optional, build-tagged integration behind the same interface — its own ADR, its
  own recorded dependency — only if the exec seal proves insufficient.
- **In-vault PKCS#11.** Reached via the operator's command instead of a CGo binding.
- **Changing the recovery-key / root-regeneration flow.** The exec seal is just
  another auto-seal; that machinery already handles all auto-seals uniformly.

## Rollout

Ship behind the new `-seal-external-*` flags (all existing seals unchanged and
still default), land it as its own beta with the reference wrapper docs, and
validate on the maintainer's cluster against a real cloud KMS before recommending
it — per the roadmap's adoption guidance.
