# Embedded secret storage with external providers – plan

**Status:** In progress · 2026-10-25 (phases 0–3 done: uBixVault import, `shares`/`aead` seal, the built-in store, delivery and fallback; seal hardening done: wrapped key distribution, rewrap completion, restore with another seal; the UI page of phase 6 done 2026-10-26) · Starting point: Nomad v1.6.5 (MPL 2.0) and the Norca features built on it (Variables, keyring, workload identities, backup) · Related documents: [nomad-fork-development-plan.md](nomad-fork-development-plan.md), [backup-restore-plan.md](backup-restore-plan.md), [consul-independence-plan.md](consul-independence-plan.md), [package-management-plan.md](package-management-plan.md)

Norca gets a built-in secret store, so that a cluster can keep and hand out passwords, API keys and certificates without running a separate secret server. The **existing external Vault integration stays exactly as it is**, and further external modules (OpenBao, cloud secret managers, plugins) can be added. The rule that ties the two together: **a secret lookup goes to the external store when one is defined for it, and falls back to the built-in store when none is defined.** This also holds for jobs written for Vault: on a cluster without Vault, their `vault` blocks and `secret` template calls are served by the built-in store, unchanged.

## Table of contents

- [Starting point](#starting-point)
- [Goals and non-goals](#goals-and-non-goals)
- [Architecture](#architecture)
- [Implementation basis: uBixVault](#implementation-basis-ubixvault)
- [Key protection (seal)](#key-protection-seal)
- [The built-in store](#the-built-in-store)
- [Provider resolution and fallback](#provider-resolution-and-fallback)
- [Vault compatibility](#vault-compatibility)
- [Delivery to tasks](#delivery-to-tasks)
- [External providers](#external-providers)
- [Permissions](#permissions)
- [Audit](#audit)
- [Rotation and dynamic secrets](#rotation-and-dynamic-secrets)
- [Multi-region operation](#multi-region-operation)
- [Backup and restore](#backup-and-restore)
- [API, CLI and UI](#api-cli-and-ui)
- [Migration](#migration)
- [Implementation and schedule](#implementation-and-schedule)
- [Testing and acceptance](#testing-and-acceptance)
- [Risks](#risks)
- [License](#license)
- [Open questions](#open-questions)

## Starting point

| Item | Today | Location |
| --- | --- | --- |
| Variables | Encrypted key/value entries in Raft (AES-256-GCM with the active root key), at most 64 KiB per entry, ACL by path, admission policies on writes, `Variables` event topic | `nomad/variables_endpoint.go`, `nomad/structs/variables.go` |
| Access from tasks | A task's workload identity can read `nomad/jobs`, `nomad/jobs/<job>`, `.../<group>` and `.../<task>` in its namespace without extra policies; templates read them with `nomadVar` | `nomad/variables_endpoint.go`, `client/allocrunner/taskrunner/template/` |
| Keyring | Root keys in the `root_key_meta` table; the key material is on every server's disk in `keystore/`, wrapped with a key-encrypting key that sits **in the same file** (the code itself calls this "security theatre … a shim for external KMS providers in the future") | `nomad/encrypter.go` (`saveKeyToStore`, `newKMSWrapper`) |
| Vault integration | Server-derived Vault tokens (`vault` block of the agent and the task), `secret` template function; Vault itself is BSL 1.1 since 1.15 and is not shipped by Norca | `nomad/vault.go`, `client/vaultclient/` |
| Secrets in packs | `secret_items` of Variables resources are applied but never stored in the release | `nomad/pack/docs/releases.md` |
| Other key material in Raft | The native mTLS CA key, encrypted with the root key | `nomad/mtls*.go` |
| Backups | Raft snapshot plus the keystore, encrypted with `kms` or `age` | `nomad/backup/` |
| Libraries already in the module | `go-kms-wrapping/v2` (MPL 2.0), `vault/api` (MPL 2.0) | `go.mod` |

Variables are therefore already an encrypted, replicated, access-controlled store. What is missing for real secret management:

1. **The keys are not protected.** Whoever reads a server's disk can decrypt every Variable, every backup made with the cluster's own keys, and the mTLS CA key.
2. **No secret lifecycle:** no versions, no expiry or rotation, no audit of who read a value.
3. **Delivery is template-only:** there is no declarative way to say "put this secret into this file or environment variable", and nothing that works the same way for an external store.
4. **External stores are Vault-only**, with the old token-derivation flow, and Vault itself is no longer open source.

## Goals and non-goals

**Goals**

- Secrets at rest are protected by a key that is **not** on the servers' disks in usable form (cloud KMS, an OpenBao/Vault transit key, or operator-held unseal shares).
- A first-class secret object with versions, metadata, expiry and rotation reminders, separate from configuration Variables, with value redaction everywhere (API listings, events, logs, UI, backups browsing).
- One declarative `secret` block in the jobspec that delivers values as files or environment variables, re-delivers them on change, and works identically for the built-in store and for external providers.
- **The existing Vault integration is unchanged:** the agent's `vault` block, server-derived Vault tokens, the task `vault` block and the `secret` template function keep working against a configured Vault exactly as in Nomad 1.6.5.
- **Fallback:** every secret lookup is resolved to an external store if one is defined for it, otherwise to the built-in store. Jobs written for Vault run without Vault: the built-in store answers their KV reads.
- A **provider interface** with built-in providers (the embedded store, OpenBao/Vault, the cloud secret managers of the supported clouds) and external plugins over the same gRPC plugin mechanism as the autoscaler's external plugins.
- Workload identity as the only credential a task needs; no long-lived tokens in jobspecs or on clients where the provider supports identity federation.
- An audit trail of every value read and write.

**Non-goals (for this plan)**

- Replacing a full Vault/OpenBao: no generic secret-engine marketplace, no SSH CA, no general PKI engine beyond the existing mTLS CA, no transit encryption-as-a-service API in the first phases.
- Secrets larger than the Raft entry budget (the built-in store keeps the 64 KiB limit per secret).
- Storing secrets of external providers in Raft: external values are fetched by the client and never written to the servers.

## Architecture

```mermaid
flowchart LR
  subgraph Servers["Servers (one region)"]
    Seal["Seal: KMS / transit / shares"]
    KR["Keyring (root keys)"]
    Store["Secret store (Raft, encrypted)"]
    Audit["Audit sink + SecretAccess events"]
    Seal --> KR --> Store
    Store --> Audit
  end
  subgraph Client["Client node"]
    Hook["secret hook (task runner)"]
    PNorca["provider: norca"]
    PExt["provider: openbao / vault / aws-sm / gcp-sm / scw-sm / plugin"]
    Hook --> PNorca
    Hook --> PExt
  end
  Task["Task: secrets/ dir (tmpfs), env"]
  PNorca -- "RPC with workload identity" --> Store
  PExt -- "provider API, identity federation" --> Ext[("External store")]
  Hook --> Task
```

- **Servers** own the seal, the keyring and the built-in store. Values are encrypted with the active root key (as Variables are); the root keys are wrapped by the seal.
- **Clients** run a `secret` task-runner hook. For each `secret` block it picks the provider, authenticates with the task's workload identity, fetches the value, writes it into the task's `secrets/` directory or environment, and watches for changes.
- **Providers** are one interface; `norca` is the built-in one. External providers are either compiled in (OpenBao/Vault and the cloud secret managers) or loaded as plugins. The existing Vault integration is wrapped as the `vault` provider without changing its behaviour.
- **Resolution:** the client decides per lookup which provider answers it (see the next section); `norca` is always the last fallback, so a lookup never fails just because no external store is configured.

## Implementation basis: uBixVault

**Decided 2026-10-25:** the built-in store is not written from scratch; its core comes from [uBixVault](https://github.com/cwolsen7905/ubixvault) (BSD-3-Clause, an independent from-scratch Go secrets manager, not derived from Vault; reference clone in the workspace at `C:\orchestrate\ubixvault`).

| uBixVault part | Used for | How |
| --- | --- | --- |
| `barrier` (AES-256-GCM, path bound into the ciphertext), `shamir`, `seal`, the init/unseal part of `core` | Key protection, the `shares` seal, sealed state | Imported; the root of trust for secrets |
| `kv` (KV v2: versions, soft delete, undelete, destroy, max versions) | The `Secret` object's storage and versioning | Imported; addressed through the Norca secret API |
| `transit` | Later: encryption as a service (not in the first phases) | Imported, not exposed until decided |
| `database` (dynamic credentials with leases) | Phase 7: its lease handling is the model for the built-in dynamic engine; the database backends themselves only when needed | The first backend is NATS (written in Norca); MariaDB/MySQL from uBixVault and PostgreSQL later, on demand |
| `audit` | The audit file sink | Imported; records also feed the `SecretAccess` topic |
| HTTP API layer (Vault-compatible shapes) | – | **Not used** (decided 2026-10-25): its single handler hard-wires every auth method, token, policy, identity, PKI and quota package (and go-ldap, the MySQL driver); the client endpoint of phase 4 gets small Norca handlers for the KV read subset instead |
| `storage/raft`, `file`, `mysql` backends, HA lock over MySQL | – | **Not used** (see below) |
| `token`, `policy`, `userpass`, `ldapauth`, `jwtauth`, `kubeauth`, `approle`, `certauth`, `identity` | – | **Not used**: access is decided by Norca ACL policies and workload identities |

- **Storage on Norca's Raft.** uBixVault's core only sees a small storage interface (`Get`, `Put`, `Delete`, `List`) and an HA lock interface. Norca implements both on its own Raft: a new message type and memdb table hold the barrier's ciphertext blobs (keys are storage paths, values stay encrypted), and the HA lock is Nomad leadership. uBixVault's own Raft backend (a second Raft cluster with its own peers and ports) is not used, so there is one consensus system per region, and secrets are automatically replicated, included in the backup snapshots and carried over leader changes.
- **One active instance per region:** the barrier core runs on the Nomad leader (unsealed state per server, as planned); followers forward secret requests over the normal Nomad RPC forwarding.
- **Authorization:** Norca ACL capabilities (`list-secret`, `read-secret`, …) and workload identities are checked before the engines are called; uBixVault tokens and policies are not exposed.
- **Code import:** the needed packages are brought into the Norca repository with `git subtree` (history kept, like Nomad Pack), under `nomad/secretstore/ubix/`, because they are Go `internal` packages and cannot be imported as a module. Upstream is asked whether it would export them as public packages; if it does, the subtree is replaced by a normal module dependency. Local changes are kept small and offered upstream.
- **Version:** v1.6.0 (2026-10-14) is the latest release; per the two-week rule it can be adopted from 2026-10-28, until then v1.5.0 (2026-09-30).
- **Risks taken on:** the project is young (first commit 2026-07-18), maintained essentially by one person, and not independently audited; Norca takes on review and maintenance of the imported code (own security review of the barrier, Shamir and seal paths before release, fuzz and property tests kept and run in Norca's CI).

## Key protection (seal)

A new `keyring` block in the server configuration replaces the key-encrypting key stored next to the key:

```hcl
server {
  keyring {
    seal "awskms" {            # or gcpckms, scwkms, transit, shares, aead (legacy)
      region     = "eu-central-1"
      kms_key_id = "alias/norca-keyring"
    }
    # optional second seal: keys are wrapped by both, either can unwrap
    seal "shares" {
      threshold = 3
      shares    = 5
    }
  }
}
```

| Seal | Wraps the root keys with | Unseal at server start | Fits |
| --- | --- | --- | --- |
| `awskms` | AWS KMS key (server role needs `Encrypt`/`Decrypt` only) | Automatic | AWS regions |
| `gcpckms` | Google Cloud KMS key | Automatic | GCP regions |
| `scwkms` | Scaleway Key Manager key | Automatic | Scaleway regions |
| `transit` | An OpenBao/Vault transit key | Automatic while the transit server is reachable | Any cloud with an existing OpenBao/Vault |
| `shares` | A random unseal key split into Shamir shares held by operators | Manual: `norca operator unseal` with `threshold` shares | Hetzner (no managed KMS), bare metal, highest protection |
| `aead` | Today's behaviour (key next to the key) | Automatic | Development and migration only; the agent warns |

- **Decided 2026-10-25:** the first version implements `shares` (and `aead` for development and migration). The KMS and `transit` seals above are the designed extension points and are integrated later, when someone needs them.
- Hetzner Cloud has no managed KMS; Hetzner regions use `transit` (an OpenBao in another region or on dedicated hosts) or `shares`.
- **Sealed state:** a server whose keys cannot be unwrapped joins Raft and serves everything that needs no decryption, but rejects secret and Variable reads, workload-identity signing and mTLS issuance with a clear `sealed` error until unsealed. `norca operator seal-status` shows it per server.
- **Key distribution between servers** (decided 2026-10-25, the model Vault documents; built): in a region with a real seal (anything but `aead`, in place or as the target of a rewrap), key material never travels between servers in usable form. A stolen server certificate, or a stolen data directory that joins the cluster, therefore yields no key without the unseal shares or the KMS. The server TLS identity itself stays on disk as in Nomad (moving it behind the seal is not planned). With `aead`, the behaviour of Nomad stays. What was built:
  - `Keyring.Get` (keyring replication) answers with `WrappedKey` instead of `Key`: the root key encrypted with a fresh key-encrypting key that is wrapped by the region's non-aead seals only, the same form as a keystore file. The receiving server unwraps it with its own seal; a sealed server neither serves nor requests keys.
  - The fan-out of `Keyring.Restore` (full restore, `restore-bootstrap`) sends the same wrapped form (`WrappedKeys`) to the other servers. Only the operator's request to the first server carries the keys of the backup in the clear (over the API, with a management token).
  - The key hand-over of a rewrap is gone (the `Operator.SealRewrapKey` RPC is removed). The leader puts the key of a new shares seal into the rewrap record in Raft (`SealRewrap.Keys`), wrapped by the active root key of the keyring: every server unsealed with a current seal holds that root key in memory and unwraps the new key there; on disk the root keys exist only wrapped by the seals. A wrap by the `aead` seal was rejected: it would leave the new key usable in the Raft log of every server, and a restarted server would unseal itself from its own log. With the root key, a server whose keystore was already rewritten to the new seal can't recover it from its log. A new region has no root key yet, so its other servers are unsealed with the new shares (as every server of a Vault cluster is unsealed on its own).
  - A server holds the keys of several shares seals at once (old and new during a `shares`→`shares` rekey), and outside a rewrap the keystore and the secret store key never gain an `aead` wrap: a seal configuration that does not match the keystore (the restored one, for the moment between a snapshot restore and the re-applied configuration) can't weaken it.
  - Other key paths checked: the mTLS CA key is in Raft encrypted with a root key and `MTLS.Roots` returns public roots only; workload identity keys are the root keys (JWKS publishes public keys only); the backup keyring export never goes to another server (it is encrypted with `kms`/`age` for the destination).
- **Seal migration** (`norca operator keyring rewrap -to <seal>`) re-wraps all root keys and the secret store key online; both seals stay valid until the rewrap completes. `shares`→`shares` replaces the unseal key (a rekey). **Completion** (decided 2026-10-25, re-examined with the key distribution rule): the rewrap completes once the leader has wrapped its keys and the secret store key with the new seal and every *other* server of the Raft configuration has finished, reported that it is sealed and can't take part (`SealRewrap.Sealed`), or is down (not alive in Serf). A down or sealed server no longer holds the rewrap up. When it returns it is sealed and must be unsealed with the new shares; its keystore files, wrapped by the old seal only, are replaced through the keyring replication (wrapped keys from the other servers). At completion the key wraps leave the state and every server drops the old wraps and the old unseal key: the old shares stop working. Trade-offs: a server that was down needs the new shares (the old ones no longer unseal anything), and the Raft log entries of the rewrap (until compaction) still hold the new key wrapped by the root key, so old shares together with an old copy of a data directory reveal it; a server that was down during a migration away from `aead` keeps its keystore in the clear on disk until it is unsealed with the new shares (rotate the root keys after leaving `aead`, as the CLI advises).
- The existing `Keyring.Restore` and full restore of the backup plan work on wrapped keys: restoring into a cluster requires access to the seal that wrapped them (or the `age` backup copy, see below).

## The built-in store

**Object:** `Secret`, namespace-scoped, stored in a new Raft table `secrets` (not in the `variables` table), encrypted with the keyring exactly like Variables.

| Field | Meaning |
| --- | --- |
| `path` | Hierarchical name, e.g. `prod/db/postgres` |
| `items` | Key/value pairs (the value), at most 64 KiB in total |
| `version` | Monotonic; the last `max_versions` (default 10) are kept, older ones destroyed |
| `metadata` | Free labels, `owner`, `description` – never secret, always visible to readers of metadata |
| `expires_at`, `rotate_every` | Optional; drive warnings, events and the rotation hook |
| `deleted` | Soft delete with `undelete` until the version is destroyed |

- **Why not just Variables:** Variables are widely used for non-secret configuration, are readable by default by every task of a job, and have no versions or audit. Keeping both lets Variables stay simple and compatible while secrets get stricter defaults (no implicit task access, value redaction, audit). A Variable item can still be marked secret by moving it.
- **Values never leave the servers except** through an explicit value read (API/CLI with `read-value`, or the client `secret` hook). Listings, events, the event stream, backup browsing, release drift and the UI only ever show metadata and a value hash.
- **Size and load:** secret writes are rare; reads by clients use blocking queries on metadata and fetch the value only when the version changes.

## Provider resolution and fallback

Every secret lookup – a `secret` block, a `secret`/`nomadSecret` template call, a task `vault` block, or a read through the Vault-compatible endpoint – is resolved by the client in this order; the first step that defines a provider wins:

| Step | Where it is defined | Example |
| --- | --- | --- |
| 1. Explicit | `provider` in the `secret` block, or the `provider:` prefix in a template call | `provider = "aws"`, `{{ secret "bao:kv/data/db" }}` |
| 2. Path mapping | `client { secrets { route } }` rules in the client configuration, matched on the path prefix (longest wins) | `route "kv/" { provider = "vault" }` |
| 3. Job or namespace default | `secrets { default_provider }` in the job's group or the job, else the namespace metadata `secrets.default_provider` | `secrets.default_provider = "bao"` for the `prod` namespace |
| 4. Vault integration | The agent's existing `vault { enabled = true }` block: Vault-style lookups (task `vault` block, `secret` template calls) go to that Vault, exactly as today | unchanged Nomad 1.6.5 behaviour |
| 5. **Fallback** | Nothing above defines a provider | the built-in `norca` store |

- **Where the defaults live** (decided 2026-10-25, built in phase 3):
  - Client routes: `client { secrets { route "<prefix>" { provider = "<name>" } } }` in the agent configuration (next to the server's `server { secrets { } }` block; the prefix is the block label because HCL1 cannot decode repeated unlabeled blocks); the longest matching prefix wins, and a route to a provider the client doesn't have is a configuration error at agent start.
  - Job and group defaults: `secrets { default_provider = "<name>" }` in the job or the group; the group's wins over the job's.
  - Namespace default: the namespace metadata key `secrets.default_provider`. The servers capture it on the job at registration (`Job.Secrets.NamespaceDefaultProvider`, like the namespace's default node pool), so clients resolve step 3 without reading namespaces, admission policies see it, and a changed namespace default applies to jobs (re-)registered afterwards, never silently to running tasks. Order inside step 3: group, job, namespace.
  - Step 4 applies to Vault-style lookups only (phase 4: the task `vault` block and the Vault `secret` template function); `secret` blocks and `nomadSecret` calls are not Vault-style and reach the fallback without a defined provider. The `vault` provider can still be named explicitly, by a route or by a default.
- **Fallback means "not defined", never "failed".** If a provider is defined but unreachable, or denies the request, or the secret does not exist there, the lookup fails with that provider's error; it does **not** silently fall through to the built-in store. A silent fallback could hand a task a stale or different value without anyone noticing.
- The resolved provider of each secret of a task is shown in the task events and the allocation status (`secret db: provider norca (fallback)`), never the value.
- Admission policies can require a provider for a namespace (for example: `prod` must use `bao`), so that a missing configuration fails at job submission instead of falling back.
- Servers use the same order for server-side lookups (for example the pack release `secret_items`).

## Vault compatibility

The existing Vault integration is kept as it is: the agent's `vault` block, the server-derived Vault tokens, the task `vault` block (`policies`, `change_mode`, `env`), `VAULT_TOKEN`/`VAULT_ADDR` injection and the `secret` template function. With a configured Vault (step 4 above) nothing changes.

When **no Vault is configured** (and no other provider is defined for the lookup), Vault-style lookups fall back to the built-in store:

- **Template calls:** `{{ with secret "kv/data/app/db" }}{{ .Data.data.password }}{{ end }}` reads the built-in secret `app/db` and returns it in the KV v2 response shape (`.Data.data`, `.Data.metadata.version`), so existing templates render unchanged. KV v1 paths (`secret/app/db` → `.Data.password`) are supported as well. The mount prefix is mapped by configurable rules (default: `kv/data/<p>`, `secret/data/<p>` and `secret/<p>` → `<p>`).
- **Task `vault` block:** without Vault, the task gets a short-lived, task-scoped token for a **Vault-compatible read endpoint on the local client agent** (`VAULT_ADDR` points to it), so applications that use a Vault SDK directly can read their KV secrets from the built-in store. The endpoint implements only KV v1/v2 reads, `token/lookup-self`, `token/renew-self` and `sys/health`; everything else returns a clear "not supported by the built-in store" error. Access is checked with the task's workload identity and the `read-secret` policy, not with Vault policies; the `policies` list of the `vault` block is used only for a warning if no matching Norca policy exists.
- This mirrors the Consul-compatible read layer of the Consul independence plan: same idea, same "subset plus explicit errors" rule.
- Dynamic Vault engine paths (for example `database/creds/<role>`) fall back to the built-in dynamic engine (see [Rotation and dynamic secrets](#rotation-and-dynamic-secrets)) for the engines it implements; other engines (for example PKI or SSH) fail with a message that a Vault/OpenBao provider is required.
- **Built (phase 4, 2026-10-26):**
  - **Scope of the takeover.** Everything below applies only to agents without a Vault integration (`vault { enabled }` unset or false) whose `client { secrets { vault_compat { enabled } } }` is not false (default true; false keeps Nomad's behaviour without Vault for templates and vault blocks; the test client configuration of the code base sets it false, so Nomad's tests see Nomad's behaviour). With one, the Vault `secret`/`secrets` template functions stay consul-template's, the task `vault` block derives Vault tokens through the servers, and the job registration checks are Nomad's; nothing of this section runs. Decided provisionally: with a Vault integration, client routes and job/namespace defaults do not redirect Vault-style template calls either (consul-template's Vault function can't be wrapped from outside, and "a configured Vault behaves exactly as Nomad 1.6.5" wins); `nomadSecret` and `secret` blocks keep the full resolution order.
  - **Mount mapping** (`client { secrets { vault_kv "<prefix>" { kv_version, path } } }`, `client/config/secrets_vault.go`, `client/secrets/vaultpath.go`): the label is the Vault path prefix (ending in `/`); the rest of the path after it, appended to `path` (default `""`, else a relative prefix ending in `/`), is the built-in secret; `kv_version` (1 or 2; default 2 for prefixes containing `/data/`, else 1) selects the response shape. A KV v2 rule `<mount>/data/<p>` also serves `<mount>/metadata/<p>`. The longest matching prefix wins; configured rules replace the defaults `kv/data/` (v2), `secret/data/` (v2) and `secret/` (v1), all with `path = ""`. Example: `vault_kv "kv/data/" { path = "apps/" }` makes `kv/data/db/pg` read `apps/db/pg`. Unmapped paths fail with "not supported by the built-in store" (listing the mounts), paths of dynamic engines (`<mount>/creds/<role>`, `<mount>/static-creds/<role>`, `<mount>/sts/<role>`) with "requires a Vault/OpenBao provider or the built-in dynamic engine (not available yet)"; neither falls back.
  - **Resolution:** Vault-style lookups (template calls and endpoint reads) are resolved like every lookup with `VaultStyle` set (`client/secrets/vaultstyle.go`, `VaultReader`): explicit `provider:` prefix, client routes (matched on the Vault path), group/job/namespace default, then the fallback. Only when the result is the built-in store is the path mapped; another provider (phase 5) reads the Vault path as it is.
  - **Template calls** (`client/secrets/template.go`, `client/allocrunner/taskrunner/template/`): the task's `TemplateSource` replaces consul-template's `secret` with its own function (external function map) and serves it like `nomadSecret`: first fetch with the timeout, then the metadata watch; a change rewrites the trigger file, so all templates render again with their change modes. The value has the fields of consul-template's Vault secret (`.Data`, `.LeaseDuration`, `.LeaseID`, `.Renewable`, `.Warnings`, `.RequestID`). KV v2: `.Data.data` (items as strings), `.Data.metadata` with `version` (a JSON number), `created_time`, `custom_metadata` (the secret's labels), `deletion_time`, `destroyed`; KV v1: `.Data` are the items, `.LeaseDuration` 2764800 as in Vault; `<mount>/metadata/<p>` returns the KV v2 metadata shape. `?version=n` pins a KV v2 version (not watched). Writes (`secret` with data arguments) and `secrets` (listing) fail with clear errors.
  - **Task `vault` block** (`client/vaultcompat/client.go`, `client/vaultcompat_setup.go`, `client/allocrunner/taskrunner/vault_hook.go`): the client replaces its (disabled) Vault client with the endpoint's client, so the vault hook works unchanged: `VAULT_TOKEN` (with `env = true`), `secrets/vault_token` (unless `disable_file`), `change_mode` on a new token. In addition the hook sets `VAULT_ADDR` to the endpoint (Nomad itself never set it) and emits a `Vault` task event when the block names `policies` and the task has no `secret_access` grant ("Vault policies … are not used by the built-in secret store …").
  - **Token design:** `nvc.<alloc ID>.<task, base64url>.<HMAC-SHA256>`, the MAC over allocation and task with a key derived from the node's secret ID. Live tokens are an in-memory set: a token is accepted from its derivation until its task stops (the vault hook stops the renewal, which revokes it), or until its TTL (`token_ttl`, default 1h) if it is not renewed; while the task runs the client keeps it alive, and `auth/token/renew-self` extends it up to the TTL. Every request also checks that the task still runs on the client. The token is never in the client state: the private-directory copy of Vault tokens is not written, and `VAULT_TOKEN` is removed from the driver handle the client persists. After an agent restart the vault hook renews the token it finds in `secrets/vault_token`; the MAC proves this node derived it, so it is adopted again if its task still runs (a running task keeps working with its token), and a token of a stopped task never is. A token reads only with its own task's workload identity, so it can't reach another task's secrets; the tokens of tasks in bridge mode are additionally accepted only from their allocation's address. Host-network tasks share the loopback address and can't be told apart by address; there the token is the credential, as a Vault token is.
  - **Endpoint** (`client/vaultcompat/server.go`, own handlers): `GET /v1/<mount>/data/<p>` (`?version=n`), `GET /v1/<mount>/<p>` (KV v1), `GET /v1/<mount>/metadata/<p>`, `GET|POST /v1/auth/token/lookup-self`, `POST|PUT /v1/auth/token/renew-self` (increment in seconds or as a duration), `GET /v1/sys/health` (no token; version `<norca>+norca-builtin-store`). Responses use Vault's envelope (`request_id`, `lease_id`, `renewable`, `lease_duration`, `data`, `wrap_info`, `warnings`, `auth`) and the shapes above; the official Go client (`vault/api`: `Logical`, `KVv1`, `KVv2` incl. `GetMetadata`, `Auth().Token()`, `Sys().Health()`) reads them. Errors are `{"errors": [...]}`: 403 for missing, invalid, revoked or foreign tokens and denied reads, 404 with no errors for a missing secret (as Vault), 405 for writes ("read-only"), 501 for unsupported paths, listing and every other API (`sys/*`, `auth/*`, `identity/*`, …: "not supported by the built-in secret store", with the list of what is served), 429 when rate limited, 503 while the store is unavailable.
  - **Binding** (decided provisionally): the endpoint listens only on `127.0.0.1:<port>` (default 8250) and, once a task in bridge networking mode asks for a token, on the bridge gateway (the first address of the client's `bridge_network_subnet`, `172.26.64.1` by default), which the network namespaces of bridge-mode allocations reach; never on other addresses. It starts listening on the first token request, so agents without such tasks open no port. Tasks in other network namespaces (Docker's own bridge, CNI networks) can't reach it; they need `network_mode = "host"` or a group `network { mode = "bridge" }`. TLS is not offered: the traffic never leaves the host. Client configuration: `vault_compat { enabled = true, port = 8250, bridge = true, token_ttl = "1h", rate_limit = 20, rate_burst = 40 }`; the rate limit is per token (requests without a valid token share one limiter).
  - **Audit:** reads of the built-in store are audited by the servers with the task's workload identity (`Secret.Read`, also denials), reads of other providers with the client record (`Secret.RecordAccess`); no duplicates. Requests the endpoint rejects itself (no or invalid token, foreign address, stopped task) are logged by the agent (path, remote address, reason; never the token).
  - **Servers** (`nomad/secrets_vaultcompat.go`): without Vault, `Job.Register`/`Plan` accept jobs with vault blocks (warning "served by the built-in secret store …") instead of failing with "Vault not enabled", once every server runs Norca (`ServersMeetMinimumNorcaVersion`). The implied constraint of their groups is `${attr.norca.secrets.vault_compat} = true`, the node attribute of clients without Vault whose endpoint is enabled, instead of `${attr.vault.version}`. `server { secrets { vault_fallback = false } }` keeps Nomad's rejection; the agent enables it by default (the server library default is off, so Nomad's tests see Nomad's behaviour). No Raft message type was needed.
  - **Admission:** `request.secrets` also lists the Vault-style lookups: `source = "vault"` (a task vault block, `name = "vault"`, no path) and `source = "vault_template"` (a Vault `secret` call of an embedded template, with its Vault path). Their `effective_provider` is the explicit provider, else the default, else `"vault"` when the servers have a Vault integration, else `""` (fallback): `request.secrets.all(s, s.effective_provider != "")` makes a namespace require a provider for them as well.
  - Tested with recorded KV v1/v2 shapes of the Vault API documentation and the `vault/api` client in-process (no Vault or OpenBao binary was used).

## Delivery to tasks

```hcl
task "api" {
  secret "db" {
    provider = "aws"              # optional; omitted = resolution order, fallback to the built-in store
    path     = "prod/db/postgres" # provider-specific path
    version  = 0                  # 0 = latest

    file {
      destination = "secrets/db.env"   # always below secrets/ (tmpfs, 0400)
      format      = "env"              # env | json | raw:<item> | template
    }
    env {                              # optional, items to environment variables
      DB_USER     = "username"
      DB_PASSWORD = "password"
    }

    change_mode   = "restart"          # restart | signal | script | noop
    change_signal = "SIGHUP"
  }
}
```

- The hook runs before the task starts (`prestart`) and blocks the start until every required secret is delivered (`optional = true` makes a secret non-blocking).
- Files are written atomically into `secrets/` (tmpfs on Linux where available) with mode 0400 and the task's user as owner; `env` values go only into the task's environment, never into the allocation API or logs.
- A **template function** `{{ nomadSecret "prod/db/postgres" }}` (optionally with a `provider:` prefix) gives the same data to templates and follows the same resolution order; the existing Vault `secret` function keeps working, against Vault when one is configured and against the built-in store otherwise.
- Values are never written into the job spec, the allocation, client state on disk, or task events. Restarted clients fetch them again.
- Jobs reference secrets by path; admission policies (CEL) can restrict which paths a namespace may reference.
- **Built (phase 3, 2026-10-25):**
  - Block fields: `provider`, `path` (with the `${job}`, `${group}`, `${task}` placeholders; `${job}` is the parent's ID for dispatched and periodic children), `version`, `file { destination, format, template }` (`template` is a Go template over the items, `{{ .password }}`), `env`, `change_mode` (default `restart`), `change_signal`, `change_script` (as for templates), `optional`, `timeout` (default 5m). `engine` and `role` are reserved and rejected until phase 7.
  - The hook (`client/allocrunner/taskrunner/secret_hook.go`) runs after the identity and Vault hooks and before the templates. Each secret is fetched with the task's workload identity (the `vault` provider uses the task's Vault token, so it needs a task `vault` block); unavailable providers and missing secrets are retried until `timeout` with task events, denials and invalid requests fail at once. Task events name the resolution (`secret db: provider norca (fallback)`) and versions, never values.
  - Files are written atomically (temporary file and rename) with mode 0400, owned by the task's user when the agent may change owners (the task's `user`, or `nobody` for the `exec` and `java` drivers that default to it; otherwise the agent's user).
  - `env` values are set on the task's environment builder only: not in the hook's response environment (which the client persists) and removed from the driver handle the client persists. A restarted client fetches every secret again; for a task that is still running this happens in the background, so it keeps its files even if the provider is unreachable.
  - Change detection: the `norca` provider blocks on the secret's metadata (`Secret.ReadMetadata`) and fetches the value only when the current version changes; the `vault` provider reads again every 5 s. A pinned `version` is not watched.
  - `nomadSecret` is an external consul-template function served by the task's `secrets.TemplateSource` with the same resolution; a change rewrites a trigger file in the task's private directory that an internal template of the runner watches, so all templates render again and their own change modes apply. The Vault `secret` function is unchanged with a Vault integration; without one it is served by the same source since phase 4 (see [Vault compatibility](#vault-compatibility)).
  - Admission: the CEL request variable has a `secrets` list (group, task, source `secret`/`template`, name, provider, default_provider, effective_provider, path with placeholders replaced, version, file_format, env, optional). Example rules: `request.secrets.all(s, s.effective_provider == "bao")`, `request.secrets.all(s, size(s.env) == 0)`, `request.secrets.all(s, s.path.startsWith("jobs/" + job.ID + "/"))`. `nomadSecret` calls are found in embedded templates only (a template read from a source file can't be inspected at submission).

## External providers

**Configuration** on clients (and servers, for providers used by server-side features):

```hcl
secrets {
  provider "bao" {
    type    = "openbao"            # Vault-compatible API: openbao | vault
    address = "https://bao.internal:8200"
    auth "jwt" { role = "norca-workloads" }   # login with the task's workload identity
  }
  provider "aws" {
    type   = "aws-secretsmanager"
    region = "eu-central-1"
    auth "web-identity" { role_arn = "arn:aws:iam::123:role/norca-tasks" }
  }
  provider "gcp"  { type = "gcp-secretmanager"      project = "acme-prod" }
  provider "scw"  { type = "scaleway-secretmanager" region  = "fr-par" }
  provider "corp" { type = "plugin" plugin = "corp-secrets" }   # external plugin binary
}
```

| Provider type | Backend | Authentication of the task | Notes |
| --- | --- | --- | --- |
| `norca` | Built-in store | Workload identity, server RPC | Always available; the fallback |
| `vault` (existing integration) | The Vault configured in the agent's `vault` block | Server-derived Vault tokens, as in Nomad 1.6.5 | Unchanged; defined implicitly when `vault { enabled = true }` |
| `openbao`, `vault` (new provider block) | OpenBao or Vault KV v2 (also dynamic secrets engines, read as leases) | JWT auth with the workload identity (no Vault token in the job) | Optional newer path; can run next to the existing integration |
| `aws-secretsmanager` | AWS Secrets Manager | STS `AssumeRoleWithWebIdentity` with the workload identity, or the node's instance role | |
| `gcp-secretmanager` | Google Secret Manager | Workload Identity Federation with the workload identity, or the node's service account | |
| `scaleway-secretmanager` | Scaleway Secret Manager | API key from the client configuration (no identity federation) | |
| `plugin` | Anything | The plugin receives the workload identity token | go-plugin over gRPC, same packaging as the autoscaler's external plugins |

- **Provider interface** (built in phase 3, `client/secrets/provider.go`): `Provider` (`Name`, `Type`, `Capabilities`, `Fetch(ctx, request) → items, version, lease`), the optional `Watcher` (`Watch(ctx, request, version) → new version`) and `LeaseManager` (`Renew`, `Revoke`, for phase 7); `Capabilities` say whether versions, watches and leases are supported, the poll interval otherwise, and whether the provider audits its reads itself. Requests carry the path, version, the reserved `engine`/`role` and the task's identity (workload identity token, job, group, task, allocation, and the Vault token of the existing integration). Errors are classified as unavailable or not found (retried until the timeout) and denied or invalid (final). Built providers: `norca` and `vault` (the existing integration, implicit when the agent's `vault` block is enabled); further providers register in the same `Registry`.
- **Plugin interface** (gRPC): `Configure`, `Fetch(path, version, identity) → items, version, lease`, `Renew(lease)`, `Revoke(lease)`, `Capabilities()`. A plugin can be written without importing Norca's server code (a small SDK in the api module, like the controller SDK).
- **Identity federation** needs the cluster's public OIDC discovery document and JWKS so that cloud IAM and OpenBao can verify workload identity tokens; Nomad 1.6.5 has neither endpoint, so the plan adds `/.well-known/openid-configuration` and `/.well-known/jwks.json` with a configurable issuer and audiences per provider.
- **Cloud SDKs** (Hetzner, Scaleway, AWS, GCP are the supported clouds) are the same official SDKs used by the LoadBalancer providers and the autoscaler's cloud targets, so they are shared dependencies, not one copy per feature. Hetzner has no secret manager; its regions use `norca` or `openbao`.
- **Fallback and caching:** a client caches values only in memory for running tasks; if a provider is unreachable, running tasks keep their delivered files, new tasks wait (with an event) until `timeout`, then fail.

## Permissions

New namespace capabilities for the built-in store, none of them included in the `read`/`write` shorthands:

| Capability | Allows |
| --- | --- |
| `list-secrets` | Paths and metadata |
| `read-secret` | Values (also needed for the `secret` hook through workload identities) |
| `write-secret` | Create, update, soft delete, undelete |
| `destroy-secret` | Destroy versions permanently |

- Path restrictions use the same glob rules as Variables (`secret "prod/db/*" { capabilities = [...] }` inside a namespace rule).
- **No implicit task access:** unlike Variables, a task can read a secret only if a policy bound to the job (`job_acl`) or a workload-identity binding grants `read-secret` on the path. A shorthand binding `secret_access { path = "jobs/${job}/*" }` keeps the common case short.
- **`secret_access` design** (decided 2026-10-25, built in phase 3): a block of a group (for all its tasks) or of a task, repeatable, with a path pattern (globs, `${job}`, `${group}`, and `${task}` in tasks). It is part of the job, so it needs no extra object, Raft type or policy: the servers check `read-secret` and `list-secrets` of a workload identity against the bindings of its allocation's job (`Secret.Read`/`ReadMetadata`), in the job's namespace, and only while the allocation is not terminal. At `Job.Register` and `Job.Plan` the submitter's ACL must hold `read-secret` on each binding's pattern (`acl.AllowSecretGrant`): the pattern must be covered as a whole, and no deny and no rule at least as specific that lacks `read-secret` may overlap it (a conservative check: overlapping rules that can't be ordered deny the grant). `write-secret` alone is not enough, since the binding hands out read access to the current values. A job therefore never grants its tasks access its submitter doesn't have; revert and multiregion registration go through the same check with the caller's token, dispatched and periodic children inherit the parent's bindings (`${job}` is the parent's ID). A task without a grant fails at start with the provider's permission error, and the server's audit records the denied read with the workload identity.
- Seal operations (`unseal`, `rewrap`, seal status) need `operator:write`.

## Audit

- Every value read and every write produces an audit record: time, region, namespace, path, version, operation, accessor (token accessor or workload identity: job, group, task, allocation), client node, result. Never the value.
- Records go to a new event stream topic `SecretAccess` (subscribers need `operator:read`) and, optionally, to an append-only audit file sink on each server (`secrets { audit_file = "/var/log/norca/secrets-audit.log" }`) with rotation. The NATS bridge can forward the topic like any other.
- External providers keep their own audit; the client additionally emits a `SecretAccess` record with `provider = <name>` for every fetch it performs.
- **Built (phase 3):** the client sends the record of each fetch from a provider other than `norca` with `Secret.RecordAccess`, authenticated with the task's workload identity; the leader takes only path, provider, provider version and result from the client, sets the accessor from the identity and records it like its own (event topic and audit file). Fetches from `norca` get no client record: `Secret.Read` already audits every value read on the leader with the same workload identity, so a second record would only duplicate it (metadata watches are not value reads and are not audited).

## Rotation and dynamic secrets

- **Rotation reminders:** `rotate_every` and `expires_at` produce `SecretExpiring`/`SecretExpired` events and metrics; nothing is rotated automatically for static secrets.
- **Rotation hooks:** a secret can name a rotation job (`rotate { job = "rotate-db-password" }`), dispatched as a parameterized job with the secret path; the job writes the new version and the `secret` hooks of consuming tasks re-deliver it (`change_mode`).
- **Dynamic secrets** (database users, cloud credentials created on demand with a lease and revoked when the lease ends) get a **built-in engine** (decided 2026-10-25). The same resolution rule applies as for static secrets: if an external provider is defined for the lookup (for example an OpenBao/Vault with its database engine), it provides the dynamic secret; otherwise the built-in engine does.
- **Built-in engine design:**
  - A `SecretEngine` object per namespace describes a backend connection (`type = "postgresql" | "mysql" | …`, address, the engine's own admin credential stored as a built-in secret) and `role`s (creation and revocation statements or cloud policy, `ttl`, `max_ttl`).
  - A task asks for it with `secret "db" { engine = "pg-prod" role = "readonly" }` (or the Vault-style path `database/creds/readonly` through the fallback); the client's `secret` hook requests a lease from the servers, the leader creates the credential on the backend, stores the lease (not the password) in Raft, and returns the credential to the client only.
  - Leases are renewed by the client while the task runs and revoked by the leader when the allocation stops, when `max_ttl` is reached, or after a lease expires without renewal (also after leader changes: the lease table is in Raft, a lease watcher on the leader revokes overdue leases).
  - Every create, renew and revoke goes to the audit trail; the backend admin credential can be rotated by the engine itself (`rotate-root`).
  - **First backend: NATS** (decided 2026-10-25). A `SecretEngine` of type `nats` holds the account signing key of the NATS deployment (as a built-in secret); a role names the allowed publish/subscribe subjects, `ttl` and `max_ttl`. A lease is a freshly generated NKey user with a JWT signed by the account key and expiring with the lease; revocation adds the user to the account's revocation list. Tasks receive `nats.creds` as a file. Database, cloud and Norca token backends come later, when needed; no external database is required for any part of the secret store.

## Multi-region operation

- Secrets are **region-local** by default; each region has its own keyring and seal (one cloud per region, so each region uses its own cloud's KMS).
- **Replicated paths:** paths marked `replicate = true` in the authoritative region are copied to the other regions. A replicated secret is read-only outside the authoritative region.
- **Transport: the Nomad region RPC** (decided 2026-10-25), the same server-to-server connection over the existing VPN that already carries request forwarding, ACL replication, multiregion deployments and the mTLS CA federation. NATS stays an event and notification bridge only.
  - Each non-authoritative region's leader pulls the replicated paths from the authoritative region with a blocking query on their metadata (like the ACL policy replication), fetches changed versions with a dedicated `Secret.Replicate` RPC that is allowed only for server identities, and writes them through its own Raft, re-encrypted with its own keyring.
  - The RPC runs over the servers' mutual TLS; in addition, the value is encrypted for the target region (each region publishes an X25519 replication public key whose private key is part of that region's keyring, therefore protected by its seal), so a value is never in plaintext outside the two leaders' memory.
  - Replication is idempotent (path plus version); an older version never overwrites a newer one, and deletes are replicated as tombstones. A leader change or an offline period is caught up by the next blocking query, plus a periodic full reconciliation that compares version hashes.
- A task in region B never fetches secrets from region A directly.

## Backup and restore

- Secrets are part of the Raft snapshot (encrypted); the backup subsystem needs no new content type.
- With a KMS or transit seal, the keystore in the backup holds **wrapped** keys; restoring into a new cluster requires access to the same KMS key or transit key. The `age` copy of the backup plan remains the disaster-recovery path that needs nothing from the cloud.
- With the `shares` seal, a restore requires the unseal shares; the backup manifest records which seal wrapped the keys.
- **Restoring a store wrapped by another seal** (decided 2026-10-25: build it; built): when the restored secret store core was wrapped by a seal the region no longer has (for example older shares), it can be recovered in two ways:
  - The backup carries the core master key next to the root keys: a separate object `secret-store-key.age` (`secret_store_key` in the manifest's objects, an additive change), written whenever the keyring is included and encrypted the same way (`kms` or `age`). A full restore re-wraps the restored core key with the region's current seals after the Raft state is restored; `restore-bootstrap` decrypts it on the operator's machine and hands it to the new server (`Operator.SealRecover` with the key, management token). A wrong key is refused: it is checked against the restored barrier keyring first.
  - A dedicated recovery unseal: `norca operator unseal -recover` (`POST /v1/operator/seal/recover`, `operator:write`, the share read from standard input or a hidden prompt, never from argv or the URL) collects the *old* shares on the leader until they reconstruct a key that unwraps the restored core key (a backup records no threshold, so from the second share on all shares entered are combined and tried; a wrong share spoils the attempt until `-reset` discards them); the leader then re-wraps the core key with the current seals.
  - Until then the store stays sealed (the core is sealed even when it runs with the same barrier key, as after a rekey in the same region), secret requests fail with a message pointing at the recovery, `norca operator seal-status` shows "needs recovery" with the seals that wrap the core key and the progress, and the full restore preview says which way the restore takes. Backups taken before this change have no secret store key object: for them only the recovery unseal works, and the preview says so.
  - A full restore and `operator snapshot restore` (thus `restore-bootstrap`) keep the region's seal configuration, as the keystores are not part of the snapshot; a region with the implicit `aead` seal keeps it explicitly.
- Backup browsing and the selective restore show secret paths and metadata, never values.
- **Selective restore of a secret writes a new version** (decided 2026-10-25): `norca operator backup restore -secret <path>[@<check-index>] [-version n] [-with-metadata]` takes one version of the secret from the backup (by default the version that was current at backup time; `-version` picks another version the backup holds, anything else is a clear error), decrypts its items through the leader's core and writes them as a **new version** on top of the current history through the normal write path: `write-secret`, the check-and-set index, the audit record and the `Secret` event. Version numbers only grow, existing versions stay, and `max_versions` ages out the oldest as with any write. A soft deleted secret becomes live again; a destroyed or missing secret is created.
  - **Check index:** an optional guard. Without it the value is always appended; `0` requires that the secret does not exist; any other value must be the secret's current modify index (shown by `-dry-run`).
  - **Metadata:** by default only the value is restored and the current metadata (labels, owner, description, `expires_at`, `rotate_every`, `max_versions`) is kept; with `-with-metadata` the backup's metadata is applied too. A secret that does not exist gets the backup's metadata either way.
  - The audit record (`restore` operation) and the restore result name the backup and the backup's version; the result shows the new version number. The value never appears in the result, events, logs or the audit trail.
  - A backup whose data the current core cannot decrypt (for example from a store wrapped by another seal) is refused with an error that points to the recovery paths above (the `age` copy or the recovery unseal); nothing is written. The restore never copies ciphertext or replaces a secret's history.

## API, CLI and UI

| Operation | HTTP | CLI |
| --- | --- | --- |
| List | `GET /v1/secrets?prefix=` | `norca secret list [prefix]` |
| Metadata | `GET /v1/secret/:path?metadata` | `norca secret info <path>` |
| Read value | `GET /v1/secret/:path?version=` | `norca secret get <path> [-version n] [-item k]` |
| Write | `PUT /v1/secret/:path` (check-and-set index) | `norca secret put <path> k=v k=@file` |
| Delete, undelete, destroy | `DELETE /v1/secret/:path`, `POST …/undelete`, `POST …/destroy?versions=` | `norca secret delete/undelete/destroy` |
| Seal status, unseal, rewrap, recovery | `GET /v1/operator/seal`, `POST /v1/operator/unseal`, `POST /v1/operator/keyring/rewrap`, `POST /v1/operator/seal/recover` | `norca operator seal-status/unseal`, `norca operator keyring rewrap`, `norca operator unseal -recover` |
| Providers on a client | `GET /v1/client/secrets/providers` | `norca node secrets-providers` |

- `norca secret put` never takes values as plain arguments in shell history when `-` or `@file` is used, and refuses to print values unless `-item`/`-format` is given explicitly.
- **UI:** a "Secrets" page per namespace (paths, versions, metadata, expiry, last access from the audit, which jobs reference a path); values are revealed only on explicit click with `read-secret`, and the reveal is audited. Seal status on the cluster overview.
- **UI built (2026-10-26)** (`ui/app/components/secrets/`, Configuration → Secrets): the list per namespace scope (path, current version, state, owner, description, update time, last access) and a page per secret (metadata incl. expiry and rotation interval as stored, the version history with the keyed value hash, delete/undelete/destroy per version, destroy of the secret with the typed path, restore of a kept version as a new version, the jobs that reference the path, recent audit records). Values are read only on an explicit Reveal through `GET /v1/secret/:path?version=` (audited by the leader), kept in the component only (never the URL, the Ember Data store, events or notifications) and dropped on Hide, on another version and on navigation. Writes: a form for a new secret or version (password fields, check index 0 or the modify index, an optional audited fill-in of the current values) and the metadata in the unified editor. Seal types and the per-server seal and store state (`GET /v1/operator/seal`, operator read) on the Secrets page and in the dashboard's federation strip, with warnings for sealed servers and a store that needs recovery; unsealing stays in the CLI. Gaps, as the API provides them today: last access comes from the `SecretAccess` event stream (the events the servers still buffer, then live; operator read), as there is no audit query endpoint; the referencing jobs are found in the UI by reading the job specs of the namespace (secret blocks, `secret_access` bindings, `nomadSecret`/`secret` template calls; the first 50 jobs), as there is no server-side reference index; restore as a new version is a read plus a conditional write (no dedicated endpoint, so it needs `read-secret` and `write-secret`); expiry and rotation are shown as stored, without the warnings and events of phase 6, and replication is not shown.
- **Packs:** `secret_items` of the package plan become `Secret` resources (with `replicate`, `rotate_every`), still never stored in the release.

## Migration

- **Variables → secrets:** `norca secret import -from-variable <path>` moves (or copies) a Variable into the store; a deprecation warning can be enabled for Variables items that look secret (same heuristics as the pack release redaction).
- **Vault jobs:** jobs using `vault {}` and `{{ with secret "kv/data/..." }}` keep working against a configured Vault with no change. To move a cluster off Vault, copy the KV secrets into the built-in store (`norca secret import -from-vault <mount>/<prefix>`, reading through the existing Vault configuration), then remove the agent's `vault` block: the same jobs now resolve to the built-in store through the fallback. Switching to an `openbao`/`vault` provider block with JWT auth is optional and removes the need for server-derived Vault tokens.
- **Built (phase 4, `command/secret_import.go`):** `norca secret import -from-vault <mount>/<prefix> | -from-variable <path>` with `-dry-run`, `-prefix-map <from>=<to>` (repeatable, longest wins), `-overwrite` (default: skip existing secrets; new secrets are written with check index 0, so a secret written meanwhile is never overwritten), `-move` (Variables only: deletes each Variable with its modify index after the copy) and the general `-namespace`. `-from-vault` reads with the standard Vault environment of the user (`VAULT_ADDR`, `VAULT_TOKEN` or the vault CLI's `~/.vault-token`, `VAULT_NAMESPACE`, TLS variables; `-vault-address`/`-vault-namespace` override), finds the mount and its KV version with `sys/internal/ui/mounts` (else the first path segment, `-kv-version` overrides), walks the prefix recursively and writes each secret to its path below the mount, which is what the default `vault_kv` rules read afterwards. Only the latest version is copied; deleted versions are skipped; non-string values are stored as JSON. A dry run reads KV v2 metadata only. Values are never printed: each line names source path and version and target path and version, followed by a summary. A path ending in `/` imports every Variable below it. "Reading through the existing Vault configuration" was taken as the user's Vault environment: the agent's `vault` block holds no token a CLI could use.
- **Keyring:** clusters start with `aead` (today's behaviour) until a `keyring { seal … }` block is added; `norca operator keyring rewrap` then moves to the new seal online.

## Implementation and schedule

| Phase | Content | Main code locations |
| --- | --- | --- |
| 0. Import | uBixVault packages via `git subtree` into `nomad/secretstore/ubix/`, Norca-Raft storage and HA-lock backend (new message type and table), REUSE and notices, Norca CI running its tests | `nomad/secretstore/`, `nomad/fsm.go`, `nomad/state/` |
| 1. Seal | `keyring { seal }` with `shares` (on uBixVault's barrier, Shamir and seal) (and `aead`, today's behaviour, for development and migration); a seal interface that later seals plug into; sealed state; unseal, seal status, rewrap; backup manifest records the seal | `nomad/encrypter.go`, new `nomad/seal/`, `command/operator_unseal.go`, `nomad/backup/` |
| 2. Built-in store | `Secret` object, Raft table, versions, soft delete, ACL capabilities, redaction, `SecretAccess` events, audit file sink, API, CLI | new `nomad/secrets_endpoint.go`, `nomad/structs/secrets.go`, `nomad/state/state_store_secrets.go`, `acl/policy.go`, `api/secrets.go`, `command/secret_*.go` |
| 3. Delivery and fallback (done 2026-10-25) | `secret` jobspec block, task-runner hook, `nomadSecret` template function, change modes, provider interface with the `norca` provider and the existing Vault integration wrapped as the `vault` provider, the resolution order with fallback, admission on referenced paths and required providers | `jobspec2`, `nomad/structs`, `client/allocrunner/taskrunner/secret_hook.go`, `client/secrets/` |
| 4. Vault compatibility | Vault-style template calls and the task `vault` block served by the built-in store when no Vault is configured; Vault-compatible KV read endpoint on the client agent (own handlers: KV v1/v2 read, `token/lookup-self`, `token/renew-self`, `sys/health`); `secret import -from-vault` | `client/allocrunner/taskrunner/vault_hook.go`, `client/allocrunner/taskrunner/template/`, new `client/vaultcompat/` |
| 5. Further external providers (on demand) | `openbao`/`vault` provider blocks (JWT), `aws-secretsmanager`, `gcp-secretmanager`, `scaleway-secretmanager`, plugin mechanism and SDK, OIDC discovery/JWKS; built only when a user needs them | `client/secrets/providers/`, `api/secretsplugin/`, `command/agent/` |
| – Further seals (on demand) | `awskms`, `gcpckms`, `scwkms`, `transit`, behind the phase 1 seal interface; built only when a user needs them | `nomad/seal/` |
| 6. Lifecycle | Expiry and rotation events, rotation jobs, replicated paths over the region RPC (pull by the non-authoritative leaders, encrypted for the target region), UI page (UI page done 2026-10-26) | `nomad/secrets_watcher.go`, new `nomad/secretsrepl/`, `nomad/leader.go`, `ui/` |
| 7. Built-in dynamic secrets | `SecretEngine` objects and roles, lease table and lease watcher on the leader, credential creation and revocation, renewal by clients, `rotate-root`, fallback of Vault-style dynamic paths, the first backend: NATS users (NKey/JWT) | new `nomad/secretengine/`, `nomad/structs/secret_engine.go`, `client/secrets/` |

Phase 0 comes first; phases 1 and 2 build on it and are independent of each other, so they can run in parallel; phase 3 needs 2; phase 4 needs 3; phase 7 needs 2 and 3 (and 4 for the Vault-style dynamic paths). Phase 5 and the further seals are built on demand (decided 2026-10-25); the interfaces of phases 1 and 3 are designed so that they can be added without changing jobs or stored data. The existing Vault integration keeps working throughout; no phase changes its behaviour when a Vault is configured.

## Testing and acceptance

- With `awskms`/`gcpckms`/`scwkms`/`transit`, the keystore files contain no usable key material: copying a server's data directory to another machine does not allow decrypting any Variable, secret or backup.
- With `shares`, a restarted server stays sealed (serving Raft but rejecting decryption) until `threshold` shares are entered; fewer shares do not unseal.
- `rewrap` from `aead` to a KMS seal runs online under write load without failed reads.
- A secret's value never appears in: list responses, the event stream, server or client logs, task events, the allocation API, the job spec, release revisions, backup browsing (tests grep all of these for a canary value).
- A task without a policy granting `read-secret` cannot start if it requires the secret, and the denial is in the audit trail.
- Updating a secret re-delivers it to running tasks according to `change_mode` within 10 seconds.
- The same job with `provider = "norca"` and with `provider = "bao"` (OpenBao test server, JWT auth with the workload identity) yields the same files and environment.
- **Fallback:** a job without any `provider` setting, on a client with no route, no namespace default and no `vault` block, gets its secrets from the built-in store; adding a namespace default makes the same job read from that provider; making that provider unreachable makes the task fail with the provider's error instead of falling back.
- **Vault compatibility:** an unmodified job that uses `vault {}` and `{{ with secret "kv/data/app/db" }}` renders the same file with a real Vault (existing integration, OpenBao/Vault test server) and, after removing the agent's `vault` block, from the built-in store; an application using the Vault Go client with `VAULT_ADDR`/`VAULT_TOKEN` reads the same KV value through the client's compatible endpoint.
- The existing Vault integration tests of Nomad 1.6.5 pass unchanged.
- Each cloud secret-manager provider is tested against the provider's API (integration tests gated on credentials; contract tests with recorded responses in CI).
- A replicated path written in the authoritative region appears in the other regions, re-encrypted, within the replication interval.

## Risks

| Risk | Mitigation |
| --- | --- |
| A KMS outage blocks server restarts (sealed servers) | Running servers keep their unwrapped keys in memory; a second seal (`shares` or another KMS) can be configured; seal status in metrics and alerts |
| Lost unseal shares or a deleted KMS key make all data unrecoverable | Two seals recommended; the `age` backup copy; documented key-deletion protection (KMS deletion windows) |
| Secrets leak through a side channel (logs, events, crash dumps, `env` inspection) | Canary-value tests across all outputs; `file` delivery recommended over `env`; values only in memory on clients |
| Too many secret reads load Raft | Clients watch metadata and fetch values only on version change; reads are served by followers with stale-OK for metadata |
| External provider differences (versions, leases, naming) | Provider capability flags; contract tests per provider; clear errors for unsupported features |
| Confusion between Variables and secrets | Documentation, `secret import`, warnings for secret-looking Variables |
| A job silently reads from the built-in store although an external store was intended (missing configuration) | The resolved provider is shown in task events and the allocation status; admission policies can require a provider per namespace; fallback only when nothing is defined, never on errors |
| The Vault-compatible endpoint is mistaken for a full Vault | Only KV reads and token self-operations; explicit errors for everything else; documented subset |

## License

- The core of the built-in store comes from uBixVault (BSD-3-Clause, on the allowed list): its copyright and license notice are kept in the imported files and in `THIRD_PARTY_NOTICES.md`, and `REUSE.toml` annotates the subtree. Its dependencies used by the imported packages are MPL/MIT/BSD/Apache (for example `go-sql-driver/mysql`, MPL 2.0).

- Everything is built on the Nomad v1.6.5 code (MPL 2.0) with an independent design; no code is taken from Vault or from Nomad/Vault versions under BSL 1.1.
- `hashicorp/go-kms-wrapping/v2` and its `awskms`, `gcpckms` and `transit` wrapper modules are MPL 2.0 (checked 2026-10-25 for awskms v2.0.11, gcpckms v2.0.13, transit v2.0.13; to be re-checked for the versions adopted); the Scaleway Key Manager wrapper is written in Norca on the Scaleway SDK.
- `vault/api` (MPL 2.0) is already a dependency and talks to OpenBao (MPL 2.0) and Vault. Norca never ships Vault; OpenBao is the recommended external server.
- Cloud SDKs: `hcloud-go` (MIT), `scaleway-sdk-go`, `aws-sdk-go-v2`, `google-cloud-go` (Apache 2.0).
- Database drivers for the dynamic engine: for example `jackc/pgx` (MIT) and `go-sql-driver/mysql` (MPL 2.0); the engine's design is independent, no code is taken from Vault's database plugins (BSL).
- Shamir secret sharing: an own implementation or a library under an allowed license (MPL/Apache/MIT/BSD/ISC); not the BSL-licensed Vault code.

## Open questions

- [x] What happens to the existing Vault integration, and how do built-in and external stores relate? Decided 2026-10-25: the existing Vault integration stays as it is; external modules remain possible; secret lookups go to the external store when one is defined and fall back to the built-in store when none is defined.
- [x] Should a defined but failing external provider ever fall back to the built-in store? Decided 2026-10-25: never; fallback only when no provider is defined.
- [x] Should the Vault-compatible read endpoint on the client agent be part of the first version, or only the template fallback? Decided 2026-10-25: part of the first version, KV reads only (as proposed).
- [x] Separate `Secret` object next to Variables, or extend Variables with secret features? Decided 2026-10-25: a separate `Secret` object.
- [x] Which seals are needed first? Decided 2026-10-25: `shares` for now (plus `aead` for development and migration); KMS and `transit` seals are integrated later, when someone needs a more complex setup.
- [x] Which external providers come first? Decided 2026-10-25: none beyond the existing Vault integration for now; further providers (OpenBao/Vault provider blocks, cloud secret managers, plugins) are integrated later, on demand.
- [x] Is a built-in dynamic secrets engine needed at all, or are OpenBao's engines enough? Decided 2026-10-25: a built-in engine is needed; if an external provider is defined for the lookup, the external provider supplies the dynamic secret instead.
- [x] Is the built-in store written from scratch or based on an existing project? Decided 2026-10-25: based on uBixVault (barrier, Shamir, seal, KV, transit, database, audit) imported with `git subtree`, storage on Norca's Raft, access through Norca ACL, the Vault-compatible client endpoint written as small Norca handlers (its HTTP layer is not imported, decided 2026-10-25); see [Implementation basis](#implementation-basis-ubixvault).
- [x] Which backends does the built-in dynamic engine support first? Decided 2026-10-25: NATS (short-lived NATS users as NKey/JWT credentials, limited to the subjects of the role, for the NATS of the `nats` pack of the [Norca pack registry](https://github.com/bacanoo-io/norca-registry)); no external database is installed for secrets. Cloud credentials, Norca ACL tokens and databases (MySQL/MariaDB from uBixVault, PostgreSQL) are added later, only when needed.
- [x] Should secrets be replicable across regions, and over which connection? Decided 2026-10-25: yes, opt-in per path from the authoritative region, over the Nomad region RPC (the server-to-server connection over the existing VPN); NATS was considered and rejected, it stays an event bridge only.
- [x] Is `env` delivery allowed by default, or only `file` delivery unless an admission policy allows `env`? Decided 2026-10-25: `env` is enabled by default; the documentation recommends `file`, and an admission policy can forbid `env` per namespace.
- [x] Where do audit records go besides the event stream? Decided 2026-10-25: an append-only file sink on each server (as proposed); no remote sink for now (the NATS bridge can still forward the `SecretAccess` topic).
