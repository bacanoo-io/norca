<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Built-in Secret Store and Cryptographic Seal Architecture

## 1. Executive Summary & Core Architectural Principles

Norca introduces a first-class, zero-external-dependency Secret Store and Cryptographic Seal subsystem (`norca/secretstore/`, `norca/norca/seal/`, `norca/client/secrets/`, and `norca/client/vaultcompat/`). Whereas standard Nomad requires deploying, operating, and unsealing an external HashiCorp Vault cluster for versioned secrets, dynamic workload credentials, and at-rest root key protection, Norca embeds the BSD-3-Clause **uBixVault** cryptographic barrier and KV v2 engine directly atop the server's Raft consensus log and unifies its master key lifecycle with the server keyring seal.

### Key Architectural Tenets:
1. **Zero External Vault Requirement & Seamless Vault Compatibility**: Workloads can consume secrets natively via `secret` blocks and `nomadSecret` template functions, or transparently run existing Vault-dependent jobs (`vault` stanzas, `{{ with secret ... }}` templates, and Vault SDK calls via `VAULT_ADDR` / `VAULT_TOKEN`) against Norca's built-in store without modifying application code.
2. **Sibling Key Hierarchy Under a Unified Seal**: A server's on-disk keyring root keys (protecting Variables, Workload Identity JWT signing keys, and mTLS CA keys) and the uBixVault core master key (`core/auto-master`) are siblings wrapped by the same region seal (`shares` via Shamir's Secret Sharing or `aead` for development). Unsealing a server unlocks both subsystems simultaneously.
3. **Zero Plaintext Key Transit**: When a region operates with a non-AEAD seal (`SealConfig.RealSeal() == true`), cryptographic keys never cross the network in usable form. Inter-server keyring replication (`Keyring.Get`), online seal migrations (`Keyring.Rewrap`), and Raft snapshots transmit only seal-wrapped envelopes (`structs.SealWrappedKey`).
4. **Atomic Metadata & Ciphertext Commits**: Secret writes execute on the active Raft leader using an in-memory `secretstore.Batch` context (`norca/secretstore/batch.go:30`). The KV v2 barrier ciphertexts (`secret_store_blobs` MemDB table) and the secret's plaintext-free metadata row (`secrets` MemDB table) are committed in a single Raft log entry (`SecretApplyRequestType` / `SecretPurgeRequestType`), preventing split-brain divergence between metadata indexes and encrypted payloads.
5. **Fail-Closed Leader-Local Audit Trail**: Every secret value read, write, version mutation, and backup restoration—including denied attempts—is logged through a fail-closed `audit.Broker` (`norca/norca/secrets_kv.go:78`) to both the `SecretAccess` event stream topic and an optional size/duration-rotated append-only JSON audit file (`norca/secretstore/audit.go:60`). If an audit record cannot be persisted, the secret operation is aborted before any value is returned.
6. **Privilege Escalation Prevention & Symlink-Safe Delivery**: Jobs can grant workload identities `read-secret` access via `secret_access` stanzas only if the job submitter already possesses `read-secret` across the entire glob pattern (`acl.AllowSecretGrant`). On client nodes, secrets are written atomically via `os.OpenRoot(taskDir)` (`norca/client/allocrunner/taskrunner/secret_hook.go:546`), rejecting any intermediate symlinks or directory junctions and redacting secret environment variables from persisted driver handles.

---

## 2. System Architecture & Data Flow Diagrams

### 2.1 Control Plane Seal, uBixVault Barrier, and Raft Storage Architecture

```
+---------------------------------------------------------------------------------------------------+
|                                      NORCA SERVER (LEADER)                                        |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  |                            SEAL MANAGER (norca/norca/seal_server.go)                        |  |
|  |                                                                                             |  |
|  |   +------------------------------------+         +--------------------------------------+   |  |
|  |   |     seal.Shares (Shamir k-of-n)    |   OR    |      seal.AEAD (Dev / Migration)     |   |  |
|  |   | 256-bit Unseal Key (RAM only)      |         | Random 256-bit KEK stored alongside  |   |  |
|  |   | Verified via SealInfo.KeyCheck     |         | ciphertext in SealWrappedKey.KEK     |   |  |
|  |   +-----------------+------------------+         +------------------+-------------------+   |  |
|  +---------------------|-----------------------------------------------|-----------------------+  |
|                        +-----------------------+-----------------------+                          |
|                                                |                                                  |
|                      Wraps / Unwraps (Sibling Cryptographic Hierarchy)                            |
|                        +-----------------------+-----------------------+                          |
|                        |                                               |                          |
|                        v                                               v                          |
|  +-------------------------------------------+   +---------------------------------------------+  |
|  |   SERVER KEYRING (encrypter_seal.go)      |   |    uBixVault CORE (norca/secret_store.go)   |  |
|  |                                           |   |                                             |  |
|  | - Local Disk: keystore/<keyID>.nks        |   | - Auto-Unseal Adapter: seal.CoreSeal        |  |
|  | - AAD: "norca-keystore:<keyID>"           |   | - Master Key Blob: "core/auto-master"       |  |
|  | - Unlocks:                                |   |   AAD: "norca-secret-store-core-master-key" |  |
|  |   * Nomad Variables Encryption            |   | - Barrier Keyring: "core/keyring"           |  |
|  |   * Workload Identity Ed25519 Signer      |   | - Mounted Engine: kv.New(b, "secrets")      |  |
|  |   * Cluster mTLS Root CA                  |   | - Value Hash Key: "norca/secrets-value-..." |  |
|  +-------------------------------------------+   +----------------------+----------------------+  |
|                                                                         |                         |
|                                                         Batch Context (secretstore.Batch)         |
|                                                                         |                         |
|                                                                         v                         |
|  +---------------------------------------------------------------------------------------------+  |
|  |                       RAFT STORAGE & HA ADAPTER (norca/secretstore/backend.go)              |  |
|  |                                                                                             |  |
|  |  - Implements uBixVault storage.HABackend over Norca Raft                                   |  |
|  |  - HALock tied to establishLeadership / revokeLeadership (Fences stale leaders w/ ErrFenced)|  |
|  |  - Single Atomic Raft Commit: SecretApplyRequestType (109) / SecretPurgeRequestType (110)   |  |
|  +--------------------------------------------------+------------------------------------------+  |
|                                                     |                                             |
|                                                     v                                             |
|  +---------------------------------------------------------------------------------------------+  |
|  |                                  MEMDB STATE STORE (norca/state/)                           |  |
|  |                                                                                             |  |
|  |   +-----------------------------------------+   +---------------------------------------+   |  |
|  |   | Table: "secrets" (Plaintext Metadata)   |   | Table: "secret_store_blobs"           |   |  |
|  |   | - Namespace, Path, CurrentVersion       |   | - Opaque AES-256-GCM Barrier Blobs    |   |  |
|  |   | - ItemKeys, ValueHash (HMAC-SHA256)     |   |   * "core/auto-master"                |   |  |
|  |   | - Versions[] (Deleted, Destroyed, Hash) |   |   * "core/keyring"                    |   |  |
|  |   | - Labels, Owner, ExpiresAt, MaxVersions |   |   * "secrets/meta/<ns>/<path>"        |   |  |
|  |   | - Emits TopicSecret events              |   |   * "secrets/data/<ns>/<path>/<ver>"  |   |  |
|  |   +-----------------------------------------+   +---------------------------------------+   |  |
|  +---------------------------------------------------------------------------------------------+  |
+---------------------------------------------------------------------------------------------------+
```

### 2.2 Client Task Secret Delivery & Vault Compatibility Lifecycle

```
+---------------------------------------------------------------------------------------------------+
|                                         NORCA CLIENT NODE                                         |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  |                        PROVIDER REGISTRY & RESOLVER (client/secrets/resolve.go)             |  |
|  |                                                                                             |  |
|  |  Resolution Order:                                                                          |  |
|  |  [1. Explicit Provider] -> [2. Client Route] -> [3. Group/Job/NS Default]                   |  |
|  |                         -> [4. Vault Integration (if VaultStyle & enabled)]                 |  |
|  |                         -> [5. Fallback: Built-in "norca" Provider (RPC w/ Workload JWT)]   |  |
|  +--------------+-----------------------------------+------------------------------------+-----+  |
|                 |                                   |                                    |        |
|     +-----------+-----------+           +-----------+-----------+            +-----------+-----+  |
|     |                       |           |                       |            |                 |  |
|     v                       v           v                       v            v                 |  |
|  +-----------------------+  +-------------------------------+  +----------------------------+  |  |
|  |   secret_hook.go      |  |    template_secrets.go        |  |   vaultcompat/server.go    |  |  |
|  | (Task "secret" block) |  | (nomadSecret / Vault secret)  |  | (Local HTTP Vault Shim)    |  |  |
|  |                       |  |                               |  |                            |  |  |
|  | - Prestart blocking   |  | - Registers nomadSecret       |  | - Listens on:              |  |  |
|  |   fetch w/ backoff    |  |   in consul-template          |  |   * 127.0.0.1:8250 (host)  |  |  |
|  | - Watches metadata    |  | - Overrides "secret" when     |  |   * 172.26.64.1:8250 (br)  |  |  |
|  |   via blocking RPC    |  |   Vault not enabled           |  | - Issues HMAC Task Token:  |  |  |
|  | - Formats: env, json, |  | - Trigger file watch:         |  |   nvc.<alloc>.<task>.<mac> |  |  |
|  |   raw:<item>, template|  |   private/.nomad-secrets-     |  | - Maps Vault KV v1/v2 via  |  |  |
|  | - Redacts Env from    |  |   trigger                     |  |   vault_kv prefix rules    |  |  |
|  |   TaskHandle state    |  +---------------+---------------+  +-------------+--------------+  |  |
|  +-----------+-----------+                  |                                |                 |  |
|              |                              |                                |                 |  |
|              v                              v                                v                 |  |
|  +------------------------------------------------------------------------------------------+  |  |
|  |                     TASK FILESYSTEM & ENVIRONMENT (Sandbox Isolation)                    |  |  |
|  |                                                                                          |  |  |
|  |  - writeSecretFile() via os.OpenRoot(taskDir):                                           |  |  |
|  |    * Walks path segments with root.Lstat() -> Refuses symlinks & junctions               |  |  |
|  |    * Writes to ".<name>.tmp-<uuid8>" (0600) -> fsync -> chown -> chmod 0400 -> Rename    |  |  |
|  |  - In-Memory Env: SetHookEnv("secret-env", merged) + VAULT_ADDR / secrets/vault_token    |  |  |
|  +------------------------------------------------------------------------------------------+  |  |
+---------------------------------------------------------------------------------------------------+
```

---

## 3. Cryptographic & Seal Architecture (`norca/norca/seal/`)

### 3.1 Dual-Target Sibling Protection Model
Norca's seal subsystem protects two distinct categories of cryptographic root material at rest (`norca/norca/seal/seal.go:8`):
1. **Server Keyring Root Keys (`norca/norca/encrypter_seal.go:18`)**: Stored as individual files in the server's on-disk `keystore/` directory. Each file contains a root key encrypted with a random 256-bit key-encrypting key (KEK), and the KEK is wrapped by every active seal of the region (`structs.KeyEncryptionKeyWrapper.SealedKEKs`). Each wrap is cryptographically bound to its root key ID using Additional Authenticated Data (AAD):
   $$\text{AAD}_{\text{keystore}} = \texttt{"norca-keystore:"} \parallel \text{KeyID}$$
2. **uBixVault Core Master Key (`norca/norca/seal/core.go:17`)**: Stored in the Raft state store (`secret_store_blobs` table) at path `core/auto-master` as a JSON-encoded `seal.CoreEnvelope`. The uBixVault core operates in external auto-unseal mode (`core.SealTypeExternal`) backed by `seal.CoreSeal`, which wraps the 256-bit core master key using the server's `sealManager` with AAD:
   $$\text{AAD}_{\text{core}} = \texttt{"norca-secret-store-core-master-key"}$$

Because the keyring KEKs and the uBixVault master key are **siblings** under the region seal (neither wraps the other), unsealing a server unlocks both the Nomad keyring and the secret store core in a single workflow.

### 3.2 Seal Implementations: `shares` (Shamir) vs. `aead`

All seals implement the `seal.Seal` interface (`norca/norca/seal/seal.go:72`) using AES-256-GCM (`KeySize = 32` bytes, 96-bit random nonce prepended to the ciphertext):

| Property | `shares` Seal (`seal.Shares`) | `aead` Seal (`seal.AEAD`) |
| :--- | :--- | :--- |
| **Config Stanza** | `keyring { seal "shares" { threshold = 3 shares = 5 } }` | `keyring { seal "aead" {} }` (or omitted default) |
| **Cryptographic Mechanism** | Random 256-bit unseal key split via Shamir's Secret Sharing over $\text{GF}(2^8)$ (`norca/secretstore/ubix/internal/shamir/shamir.go`). | Generates a fresh 256-bit random KEK per wrap and stores `KEK` in plaintext inside `SealWrappedKey.KEK`. |
| **Share Format** | `ShareSize = 33` bytes (32 bytes key polynomial evaluation + 1 byte $x$-coordinate), Base64-encoded. | None (no operator shares). |
| **Server Startup State** | Starts **sealed** (`Available() == false`). Rejects Variables, JWT signing, mTLS CA, and Secret Store ops with `structs.ErrSealed`. | Starts **unsealed** (`Available() == true`) automatically. Logs a startup warning that keys are stored next to their KEKs. |
| **Key Verification** | Verifies reconstructed key against `SealInfo.KeyCheck`: AES-256-GCM encryption of `"norca seal key check v1"` with AAD `"norca-seal-key-check:<KeyID>"`. | Authenticated via AES-256-GCM tag during `gcmOpen`. |
| **Inter-Server Key Transit** | `RealSeal() == true`: Keys travel between servers **only** wrapped by the `shares` seal (`wrapKeyForTransfer`). | `RealSeal() == false`: Keys travel with plaintext KEK as in legacy Nomad. |

#### Mathematical Formulation of Shamir's Secret Sharing (`shares`)
When `seal.Generate(shares, threshold)` (`norca/norca/seal/shares.go:41`) is invoked with parameters $(n, k)$ where $2 \le k \le n \le 255$ (defaults: $n = 5, k = 3$):
- A random 32-byte unseal key $K = (b_0, b_1, \dots, b_{31}) \in \text{GF}(2^8)^{32}$ is generated via `crypto/rand`.
- For each byte $b_j$, a random degree-$(k-1)$ polynomial $P_j(x) = b_j + a_{j,1}x + a_{j,2}x^2 + \dots + a_{j,k-1}x^{k-1}$ is constructed over $\text{GF}(2^8)$ with $a_{j,k-1} \ne 0$.
- Each share $i \in \{1, \dots, n\}$ is a 33-byte slice where the first 32 bytes are $(P_0(x_i), P_1(x_i), \dots, P_{31}(x_i))$ and byte 32 is the non-zero coordinate $x_i$.
- When $k$ distinct shares are submitted to `Operator.SealUnseal`, `shamir.Combine` reconstructs each byte $b_j = P_j(0)$ via Lagrange interpolation in $\text{GF}(2^8)$:
  $$P_j(0) = \sum_{m=1}^{k} y_{j,m} \prod_{\substack{1 \le r \le k \\ r \ne m}} \frac{x_r}{x_r \oplus x_m}$$
- The reconstructed key $K$ is verified against `SealInfo.KeyCheck` (or `encrypter.verifySharesKey` if Raft state has not replicated yet). If verification fails, all accumulated shares in `s.progress` are zeroed in memory and discarded (`seal.ErrUnsealFailed`).

### 3.3 Online Keyring Rewrap & Rekey Protocol (`seal_server.go`)

Moving a cluster between seals (e.g., migrating from `aead` to `shares`, rotating Shamir shares via `shares` $\to$ `shares`, or initializing a fresh `shares` region) is performed online via `Keyring.Rewrap` (`norca operator keyring rewrap -to shares`) in two coordinated phases (`norca/norca/seal_server.go:498`):

```
Leader (startRewrap)                                  All Unsealed Servers (reconcile)
--------------------                                  --------------------------------
1. Generate new 256-bit unseal key K_new
   and split into n Shamir shares.
2. Wrap K_new with active Keyring Root Key:
   AAD = "norca-seal-rewrap-key:shares:<keyID>"
   (SealWrapKeyring in SealRewrap.Keys)
3. Apply SealConfigUpsertRequestType (107) ---------> 4. WatchSet wakes sealManager.run()
   with SealConfig.Rewrap = {ID, To, Keys}            5. Unwrap K_new using in-memory active
4. Return Base64 shares to CLI once;                     Root Key (unwrapWithRootKey)
   zero shares in memory.                             6. Add new seal wrap to all local
                                                         keystore files (rewrapKeystore(true))
                                                      7. Leader rewraps "core/auto-master"
                                                         (rewrapSecretCoreKey(ctx, true))
                                                      8. Call Operator.SealRewrapDone RPC
                                                         (SealRewrapDoneRequestType 108)
                                                                      |
9. Leader verifies all alive Raft peers  <----------------------------+
   are in rw.Done (or rw.Sealed / down)
10. Apply SealConfig{Seals: rw.To, Rewrap: nil} ----> 11. Drop old seal wraps from keystore
    (drops SealRewrap.Keys from Raft state)               and "core/auto-master"
                                                      12. Zero & drop old unseal key from RAM
```

- **Why `SealRewrap.Keys` is Safe in Raft**: During an online rekey from an existing seal to a new `shares` seal, the new unseal key $K_{\text{new}}$ is stored temporarily in `SealRewrap.Keys` wrapped by the **active keyring root key** (`SealWrapKeyring`, `norca/norca/encrypter_seal.go:215`). Unsealed servers hold the active root key in RAM and immediately unwrap $K_{\text{new}}$ to rewrap their local keystores, whereas on disk (and in backups) the active root key itself is protected by the region's seal. Once all running unsealed servers report `SealRewrapDone`, the leader commits a new `SealConfig` with `Rewrap = nil`, erasing `SealRewrap.Keys` from the current state.
- **Handling Sealed or Down Servers**: A server that is sealed during a rewrap calls `reportSealed` (`SealRewrapDoneRequest{Sealed: true}`), adding its Raft ID to `SealRewrap.Sealed`. Neither sealed servers nor offline servers block completion of the rewrap (`norca/norca/seal_server.go:864`); when they later return and are unsealed with the **new** shares, they fetch re-wrapped root keys from peers.
- **Anti-Downgrade Guard on Snapshot Restore**: Because server keystores reside on local disk outside the Raft snapshot, restoring a Raft snapshot must not roll back `SealConfig` to `aead` and inadvertently strip `shares` protection. Before a snapshot restore, `keptSealConfig()` (`norca/norca/seal_server.go:929`) captures the current `SealConfig` and `reapplySealConfig()` re-applies it immediately after restoration. Furthermore, outside an explicit `Rewrap` (`add == false`), `keepOrStrengthen()` (`norca/norca/encrypter_seal.go:257`) refuses to ever add an `aead` wrap to a key file that does not already have one.

### 3.4 Secret Store Master Key Recovery (`seal_recover.go`)

When a full backup taken under an older set of Shamir shares (Seal $A$) is restored into a region currently operating under different shares (Seal $B$), the restored `core/auto-master` blob in Raft is wrapped by Seal $A$. `coreNeedsRecovery()` (`norca/norca/seal_server.go:962`) detects that no wrap in `CoreEnvelope.Wraps` matches the region's current seals, sets `ServerSealStatus.SecretCoreRecovery = true`, and keeps the uBixVault core sealed.

Norca provides two recovery paths in `norca/norca/seal_recover.go`:
1. **Automatic Age Backup Key Recovery**: If the backup archive includes `secret-store-key.age`, the full restore (or `restore-bootstrap` via `SealRecoverRequest.CoreKey`, requiring a `management` ACL token) decrypts the raw 32-byte core master key using the operator's ephemeral `-age-identity`, verifies that it unlocks the restored barrier (`verifySecretCoreKey`), and immediately re-wraps `core/auto-master` with the region's active seals (`recoverSecretCoreWithKey`).
2. **Interactive Old-Shares Recovery (`norca operator unseal -recover`)**: If the backup has no `secret-store-key.age` (or during manual recovery), operators submit the old unseal shares from Seal $A$ via `Operator.SealRecover` (`POST /v1/operator/seal/recover`, requiring `operator:write`). Because the old `SealConfig` threshold is not stored in `CoreEnvelope`, `addRecoveryShare()` (`norca/norca/seal_recover.go:99`) collects distinct old shares (up to `maxRecoveryShares = 255`) on the leader and attempts `shamir.Combine(m.recovery)` on every submission of $\ge 2$ shares until `seal.UnwrapCoreKeyWith(unsealKey, w)` succeeds against one of the `CoreEnvelope` wraps and `verifySecretCoreKey` confirms the key unlocks `core/keyring`.

---

## 4. Embedded uBixVault Engine & Raft Storage Backend

### 4.1 Subtree Architecture & Export Facades
The cryptographic engine is derived from **uBixVault** (BSD-3-Clause) maintained under `norca/secretstore/ubix/`:
- `norca/secretstore/ubix/internal/`: Contains the core uBixVault packages (`barrier`, `shamir`, `seal`, `core`, `kv`, `transit`, `database`, `audit`, `storage`).
- `norca/secretstore/ubix/export/`: Generated facade packages that expose strictly the types and constructors used by Norca, enforcing package boundaries.

### 4.2 Raft Physical Storage & HA Lock (`norca/secretstore/backend.go`)
Rather than running a separate storage backend or secondary consensus group, `secretstore.Backend` implements uBixVault's `storage.HABackend` directly over Norca's regional Raft state machine:

- **Physical Ciphertext Table (`secret_store_blobs`)**: Every entry written by the uBixVault security barrier is stored as a `structs.SecretStoreBlob` in the MemDB `secret_store_blobs` table (`norca/norca/state/state_store_secret_store.go:17`), indexed by `Path`. Individual blobs are capped at `DefaultMaxValueSize = 512 KiB` (`norca/secretstore/backend.go:25`), leaving ample headroom for a secret version's `MaxSecretSize = 64 KiB` (`norca/norca/structs/secrets.go:86`) plus metadata and AES-GCM barrier envelopes.
- **Leadership-Bound HA Fencing (`raftLock`)**: Every Norca server instantiates `s.secretStoreCore` at startup (`norca/norca/secret_store.go:51`) and runs `s.secretStoreCore.RunHA()` in a background goroutine. `Backend.HALock` ties lock acquisition directly to `Backend.SetLeader(true)` (called from `establishLeadership`) and `Backend.SetLeader(false)` (called from `revokeLeadership`). When a leader steps down, `b.endLocked()` immediately closes the `lost` channel on its `raftLock`, causing its uBixVault core to step down to standby, and fences any in-flight storage writes with `storage.ErrFenced`.
- **Lazy First-Use Initialization**: The uBixVault core is initialized lazily on the leader the first time a secret read or write calls `s.secretCore()` (`norca/norca/secret_store.go:99`). `initSecretCore()` invokes `c.Initialize(ctx, core.InitConfig{})`, which generates the barrier encryption key (`core/keyring`) and wraps the core master key (`core/auto-master`) via `seal.CoreSeal`. Because Norca authorizes all requests via Nomad ACLs and `secretManager` seals, the root token and recovery keys returned by uBixVault's `Initialize` are immediately zeroed in memory and discarded (`norca/norca/secret_store.go:162`).
- **Continuous Core Auto-Unseal Loop (`runSecretCoreUnseal`)**: Each server runs `runSecretCoreUnseal()` (`norca/norca/secret_store.go:174`) watching both `s.seal.changedCh()` and MemDB WatchSets on `core/keyring` and `core/auto-master`. As soon as a server is unsealed and the core is initialized in Raft, the server calls `c.AutoUnseal(ctx)`. If a snapshot or backup restore replaces `core/keyring` underneath a running server, `runSecretCoreUnseal` detects the byte mismatch, seals the local core (`c.Seal()`), and automatically unseals it against the restored state.

### 4.3 Atomic Batched KV v2 Operations (`norca/norca/secrets_kv.go`)
Secrets are managed by uBixVault's KV v2 engine mounted at `secretKVPrefix = "secrets"` on the leader's barrier (`norca/norca/secrets_kv.go:61`). A single logical write (such as `secretUpsert`) causes the KV v2 engine to read and write multiple barrier paths (`secrets/meta/<ns>/<path>`, `secrets/data/<ns>/<path>/<ver>`, and potentially delete aged-out versions beyond `MaxVersions`).

Applying each barrier write as a separate Raft log entry would create partial-failure hazards and race windows between the encrypted blobs and the queryable metadata table. Norca eliminates this via `secretstore.Batch` (`norca/secretstore/batch.go:30`):

```
Leader: s.secretWrite() (under serverSecrets.writeLock)
  |
  +-- 1. Check CAS index (checkSecretIndex against StateStore.SecretByPath)
  +-- 2. ctx, batch := secretstore.WithBatch(context.Background())
  +-- 3. Ensure 32-byte HMAC key exists at "norca/secrets-value-hash-key"
  |      in barrier (secretValueHashKey); if created, add to CreateBlobs guard
  +-- 4. Update MaxVersions in "secrets/meta/<ns>/<path>" (default 10, max 100)
  +-- 5. eng.Write(ctx, "<ns>/<path>", data)
  |      * Barrier encrypts plaintext items with AES-256-GCM
  |      * Backend.Put intercepts ctx batch -> buffers in batch.puts / batch.deletes
  |      * Subsequent Backend.Get in same op reads uncommitted batch (read-your-writes)
  +-- 6. Compute keyed ValueHash for new version:
  |      HMAC-SHA256(key, ns || 0x00 || path || 0x00 || canonicalJSON(items))
  +-- 7. Build structs.Secret metadata row (no plaintext values!)
  +-- 8. Apply single Raft entry: SecretApplyRequestType (109)
         containing {Secret, ExpectedIndex, Blobs, DeleteBlobs, CreateBlobs}
```

#### Keyed Value Hash Formula
To allow operators, UI views, blocking queries, and event stream consumers to verify whether two secret versions hold identical values—without exposing plaintext or permitting offline dictionary attacks against low-entropy secrets—every version stores a keyed HMAC digest (`norca/norca/secrets_kv.go:200`):
$$\text{ValueHash} = \texttt{"hmac-sha256:"} \parallel \text{Hex}\Big(\text{HMAC-SHA256}\big(K_{\text{hmac}},\; \text{Namespace} \parallel \texttt{0x00} \parallel \text{Path} \parallel \texttt{0x00} \parallel \text{JSON}(Items)\big)\Big)$$
where $K_{\text{hmac}}$ is a 256-bit random key generated once and stored inside the encrypted barrier at `norca/secrets-value-hash-key`.

#### Raft Message Types & MemDB Schema Summary

| Component | Identifier / Name | Value / Index | Purpose & Contents |
| :--- | :--- | :--- | :--- |
| **Raft MsgType** | `SecretStoreBlobsUpsertRequestType` | `105` (Snapshot `105`) | Direct barrier blob upsert (e.g., `core/auto-master`, `core/keyring`). |
| **Raft MsgType** | `SecretStoreBlobsDeleteRequestType` | `106` | Direct barrier blob deletion. |
| **Raft MsgType** | `SealConfigUpsertRequestType` | `107` (Snapshot `107`) | Updates regional `SealConfig` (`Seals`, `Rewrap`, `LastRewrapID`). |
| **Raft MsgType** | `SealRewrapDoneRequestType` | `108` | Records per-server completion or `Sealed` skip during rewrap. |
| **Raft MsgType** | `SecretApplyRequestType` | `109` (Snapshot `109`) | Atomic transaction updating `secrets` row + `secret_store_blobs` puts/deletes. |
| **Raft MsgType** | `SecretPurgeRequestType` | `110` | Atomic transaction deleting `secrets` row + all version blobs (`destroy -all`). |
| **MemDB Table** | `secrets` (`TableSecrets`) | Compound `id`: `(Namespace, Path)` | Plaintext-free metadata rows (`structs.Secret`). Emits `TopicSecret` events. |
| **MemDB Table** | `secret_store_blobs` | Unique `id`: `(Path)` | Opaque barrier ciphertexts (`structs.SecretStoreBlob`). Emits **no** events. |
| **MemDB Table** | `seal_config` (`TableSealConfig`) | Singleton | Regional `structs.SealConfig`. |

### 4.4 Provider Resolution & Vault Compatibility Architecture

When a task resolves a secret (via a `secret` block, `nomadSecret` template call, `{{ with secret ... }}` Vault template call, or the client's local Vault-compatible HTTP endpoint), `secrets.Registry.Resolve` (`norca/client/secrets/resolve.go:143`) evaluates a deterministic 5-step resolution chain:

1. **Step 1 — Explicit Provider (`SourceExplicit`)**: `provider = "..."` on the task's `secret` block or `"<provider>:<path>"` prefix in `nomadSecret "<provider>:<path>"`.
2. **Step 2 — Client Route (`SourceRoute`)**: Longest-prefix match against `client { secrets { route "<prefix>" { provider = "..." } } }` (`norca/client/secrets/resolve.go:156`).
3. **Step 3 — Hierarchy Defaults (`SourceGroupDefault` $\to$ `SourceJobDefault` $\to$ `SourceNamespaceDefault`)**: Checks `group.secrets.default_provider`, then `job.secrets.default_provider`, then the namespace metadata key `secrets.default_provider` (captured onto `job.Secrets.NamespaceDefaultProvider` at job registration by `jobSecretsHook.Mutate` in `norca/norca/job_endpoint_hook_secrets.go:36`).
4. **Step 4 — External Vault Integration (`SourceVaultIntegration`)**: If the lookup is Vault-style (`VaultStyle == true`, i.e., a task `vault` block or `secret` template function) **and** the agent has an external Vault integration enabled (`vault { enabled = true }`), it routes to `VaultProvider`.
5. **Step 5 — Built-in Store Fallback (`SourceFallback`)**: Falls back to `norca` (`NorcaProvider`, `norca/client/secrets/norca.go:27`). Note: if steps 1–3 explicitly name an unconfigured provider, resolution fails immediately with `KindInvalid` rather than falling through.

#### Server & Client Vault Compatibility (`secrets_vaultcompat.go` & `client/vaultcompat/`)
When external Vault is **not** enabled on the servers (`!s.config.VaultConfig.IsEnabled()`) and `server { secrets { vault_fallback = true } }` is active (the default):
- **Server Job Admission (`norca/norca/secrets_vaultcompat.go:26`)**: Instead of rejecting jobs that contain `vault` stanzas or Vault template calls, the server accepts them with warning `errVaultFallback` and injects an implicit placement constraint `${attr.norca.secrets.vault_compat} = "true"` onto task groups using `vault` blocks.
- **Path Mapping (`VaultMapper`, `norca/client/secrets/vaultpath.go:37`)**: When a Vault-style lookup resolves to the built-in `norca` provider, `VaultMapper` strips the Vault mount prefix according to `client { secrets { vault_kv "<prefix>" { ... } } }` rules (sorted longest-prefix first) and formats the response in exact Vault KV v1 (`lease_duration = 2764800` / 32 days) or KV v2 (`{"data": ..., "metadata": ...}`) JSON structure. If no custom `vault_kv` rules are configured, `DefaultVaultKVRules()` (`norca/client/config/secrets_vault.go:45`) provides:
  - `kv/data/<p>` $\to$ built-in secret `<p>` (KV v2 response; also serves `kv/metadata/<p>`)
  - `secret/data/<p>` $\to$ built-in secret `<p>` (KV v2 response; also serves `secret/metadata/<p>`)
  - `secret/<p>` $\to$ built-in secret `<p>` (KV v1 response)
- **Client Local Vault-Compatible HTTP Endpoint (`norca/client/vaultcompat/server.go:106`)**: For tasks with a `vault` block on a client without external Vault, the client starts an on-demand local HTTP server (default port `8250`) bound exclusively to `127.0.0.1` (for host-network tasks) and, when a bridge-network task requests a token, to the bridge gateway IP (e.g., `172.26.64.1:8250`).
  - **HMAC Task Tokens (`norca/client/vaultcompat/tokens.go:38`)**: The `vault` hook derives a task-scoped token of the form:
    $$\text{Token} = \texttt{"nvc."} \parallel \text{AllocID} \parallel \texttt{"."} \parallel \text{Base64URL}(\text{TaskName}) \parallel \texttt{"."} \parallel \text{Base64URL}\big(\text{HMAC-SHA256}(K_{\text{node}}, \text{AllocID} \parallel \texttt{0x00} \parallel \text{TaskName})\big)$$
    where $K_{\text{node}} = \text{HMAC-SHA256}(\text{NodeSecret}, \texttt{"norca vault-compatible endpoint token key"})$.
  - **Network Namespace Isolation & Crash Recovery**: Tokens for bridge-mode tasks are verified against the source IP of the HTTP request (`remote == bridgeIP`), preventing one container from using another allocation's token. Because the token is deterministically MACed with the node's secret, a restarted Norca client agent seamlessly re-adopts tokens recovered from running task directories (`tokens.track()`).
  - **Supported Endpoints**: `GET /v1/<mount>/data/<path>[?version=n]`, `GET /v1/<mount>/metadata/<path>`, `GET /v1/<mount>/<path>`, `GET|POST|PUT /v1/auth/token/lookup-self`, `POST|PUT /v1/auth/token/renew-self`, and `GET|HEAD /v1/sys/health`. Token requests are rate-limited per token (`DefaultVaultCompatRateLimit = 20.0` req/s, burst `40`).

---

## 5. Access Control, Delegation & Audit Model

### 5.1 Namespace Secret Capabilities (`norca/acl/secrets.go`)
Secret permissions are configured inside `namespace` blocks of Nomad ACL policies, either at the namespace level or scoped to glob path patterns via `secret "<glob>"` stanzas (`norca/acl/secrets.go:16`):

| Capability | Implies | Permitted Operations |
| :--- | :--- | :--- |
| **`list-secrets`** | — | `Secret.List` (`GET /v1/secrets`), `Secret.ReadMetadata` (`GET /v1/secret/:path?metadata`), `TopicSecret` event stream. Never exposes values. |
| **`read-secret`** | `list-secrets` | `Secret.Read` (`GET /v1/secret/:path`), task `secret` delivery, `nomadSecret` templates, and delegating `secret_access` in jobs. |
| **`write-secret`** | — | `Secret.Upsert` (`PUT /v1/secret/:path`), `Secret.Delete` (soft-delete), `Secret.Undelete`, and selective backup restore (`restoreSecret`). |
| **`destroy-secret`** | — | `Secret.Destroy` (`POST /v1/secret/:path/destroy`) for permanent cryptographic erasure of versions or entire secrets. |
| **`deny`** | — | Explicitly denies all secret operations on matching paths; overrides any allow rule. |

Unlike Nomad Variables, **workload identities receive zero implicit access** to secrets matching their job/group/task path (`norca/acl/secrets.go:25`). A task can read a secret only if granted by an ACL policy attached to the job or by a validated `secret_access` binding on the task group or task.

### 5.2 `secret_access` Grant Validation & Glob Intersection (`AllowSecretGrant`)
Task groups and tasks can declare `secret_access { path = "prod/${job}/*" }` as a shorthand to grant the task's workload identity `read-secret` (and implied `list-secrets`) within the job's namespace (`norca/norca/structs/secrets_delivery.go:152`).

To prevent privilege escalation, `checkSecretAccessGrants()` (`norca/norca/job_endpoint_hook_secrets.go:88`) runs during `Job.Register` and `Job.Plan` and verifies that **the submitting user's ACL token holds `read-secret` across every possible path matched by the `secret_access` pattern** (`acl.AllowSecretGrant`, `norca/acl/secrets.go:160`):
1. `a.AllowSecretOperation(ns, pattern, "read-secret")` must be true.
2. `AllowSecretGrant` walks both the exact radix tree (`a.secrets`) and wildcard radix tree (`a.wildcardSecrets`) using dynamic programming glob intersection (`globsIntersect(rule, key)`, `norca/acl/secrets.go:232`).
3. If any `deny` rule intersects the pattern, or if any rule lacking `read-secret` that is at least as specific (measured by literal character length `len(rule) - count(rule, "*") >= covering`) intersects the pattern, the job submission is rejected with `403 Forbidden`. Note that holding `write-secret` alone is insufficient to submit a `secret_access` binding—the submitter must hold `read-secret`.

### 5.3 Fail-Closed Audit Pipeline (`norca/secretstore/audit.go`)
Every secret value read (`Secret.Read`), write (`Secret.Upsert`), version lifecycle operation (`Delete`, `Undelete`, `Destroy`), selective backup restore (`restoreSecret`), and external provider fetch reported by a client (`Secret.RecordAccess`) executes a two-phase fail-closed audit sequence via `s.secretAudit(rec, response)` (`norca/norca/secrets_kv.go:596`):
1. **Pre-Execution (`type: "request"`)**: Logged before reading from or writing to the uBixVault barrier.
2. **Post-Execution (`type: "response"`)**: Logged with `rec.Result` (`ok`, `denied`, `not-found`, `conflict`, `unavailable`, `invalid`, `error`) and `rec.Version`. Even authentication and ACL denials (`e.denied(rec)`) emit a `"response"` record.
3. **Fan-Out Devices**:
   - **`secretEventDevice` (`norca/norca/secrets_kv.go:552`)**: Publishes `"response"` records to the in-memory `TopicSecretAccess` (`SecretAccess`) event broker on the executing server (the leader). Audit events are **not** written to Raft, avoiding consensus amplification on read-heavy workloads.
   - **`secretstore.AuditFile` (`norca/secretstore/audit.go:60`)**: Appends one JSON line (`auditLine`) per `"request"` and `"response"` with mode `0600`, rotating automatically to `<path>.<YYYYMMDDTHHMMSS.nnnnnnnnnZ>` by `RotateBytes` and `RotateDuration` and pruning files beyond `RotateMaxFiles`.

---

## 6. Client Task Delivery Lifecycle (`secret_hook.go` & `template_secrets.go`)

### 6.1 Prestart Delivery, Backoff, and Change Modes
On the client node, `secretHook` (`norca/client/allocrunner/taskrunner/secret_hook.go:86`) manages all `secret` blocks declared on a task:
- **Path Interpolation**: Placeholders `${job}` (using the parent job ID for periodic/dispatched jobs via `structs.SecretJobID`), `${group}`, and `${task}` are interpolated before resolution (`norca/norca/structs/secrets_delivery.go:554`).
- **Prestart Blocking & Optional Secrets**: During initial task startup, `Prestart` spawns `h.run(st)` per secret block and blocks until all non-optional secrets are delivered. Recoverable errors (e.g., sealed store, network outage, or missing secret) are retried with exponential backoff (`secretBackoffBaseline = 1s` to `secretBackoffLimit = 30s`) up to `spec.Timeout` (default `5m`). If `optional = true` and `Timeout` expires, `Prestart` unblocks so the task can start, while the background goroutine continues waiting to deliver the secret as a live change once available.
- **Efficient Metadata Watching**: If `spec.Version == 0` (unpinned), `h.watch(st, p)` monitors the secret for rotation. For the built-in `norca` provider, `NorcaProvider.Watch` (`norca/client/secrets/norca.go:161`) issues stale-allowed blocking queries (`Secret.ReadMetadata`, `MaxQueryTime = 1m`) against the `secrets` metadata table. The leader's barrier is only queried (`Secret.Read`) when `CurrentVersion` or `Deleted`/`Destroyed` state actually changes, and the task is only signaled if the SHA-256 content fingerprint (`secrets.Fingerprint(sec.Items)`) differs from the currently delivered value. If a provider becomes unreachable while a task is already running, the task **retains its delivered files** and logs a rate-limited warning every `30s`.
- **Change Modes**: When a secret's content changes, `h.changed(st)` (`norca/client/allocrunner/taskrunner/secret_hook.go:486`) executes:
  - `restart` (default): Restarts the task via `lifecycle.Restart`.
  - `signal`: Sends `change_signal` (e.g., `SIGHUP`) via `lifecycle.Signal`.
  - `script`: Executes `change_script` inside the task container via `DriverExec.Exec`.
  - `noop`: Updates the file on disk without disturbing the process.

### 6.2 Template Trigger Mechanism (`template_secrets.go`)
Tasks can also read secrets inside `template` blocks via `{{ with nomadSecret "prod/db" }}{{ .password }}{{ end }}` (or `{{ with secret "kv/data/prod/db" }}` under Vault fallback). Because `nomadSecret` is registered as an external Go function in `consul-template`, `consul-template` does not natively track its dependencies. Norca solves this cleanly (`norca/client/allocrunner/taskrunner/template/template_secrets.go:77`):
1. When any template in the task uses `nomadSecret` (or `secret` when `ServesVault()` is true), `secretTriggerTemplate` creates an internal trigger file at `<taskDir>/private/.nomad-secrets-trigger` (`0600`) and registers an internal `consul-template` template watching `{{ file ".../private/.nomad-secrets-trigger" }}`.
2. When `TemplateSource` detects a secret rotation on its watch channel (`Changes()`), `relaySecretChanges()` atomically increments and rewrites `.nomad-secrets-trigger`, causing `consul-template`'s file watcher to re-render all templates and fire their configured change modes.

### 6.3 Symlink-Safe Atomic File Writing & State Redaction
Because a running task has write access to its own sandbox and `secrets/` directory, a compromised or malicious process inside the container could plant a symlink or directory junction under `secrets/` pointing to a sensitive host path before a secret rotation occurs. `writeSecretFile()` (`norca/client/allocrunner/taskrunner/secret_hook.go:546`) prevents symlink-traversal attacks via four defenses:
1. **Path Validation**: Rejects any destination not starting with `secrets/`, containing `\`, `:`, `.`, `..`, or matching reserved filenames (`secrets/nomad_token`, `secrets/vault_token`).
2. **OS-Enforced Root Descriptor (`os.OpenRoot`)**: Opens an `os.Root` handle on `taskDir` (`os.OpenRoot(taskDir)`), which the OS kernel/runtime restricts from resolving any path outside `taskDir`.
3. **Directory Walk with `root.Lstat`**: Walks every parent directory segment using `root.Lstat(dir)` and verifies `fi.Mode().Type() == fs.ModeDir`, refusing to follow any symlink or Windows junction.
4. **Atomic Exclusive Temp-File Rename & State Redaction**: Opens `.` + `<filename>.tmp-<uuid8>` with `O_WRONLY|O_CREATE|O_EXCL` (`0600`), writes and `fsync`s the payload, `chown`s to the task user (`secretFileOwner`), `chmod`s to read-only `0400`, and atomically `root.Rename(tmp, dest)` over the destination. For environment-delivered secrets (`env` map), values are injected into `envBuilder.SetHookEnv("secret-env", merged)` in memory only and stripped from the persisted driver handle via `redactSecretEnv()` (`norca/client/allocrunner/taskrunner/secret_hook.go:646`) and `redactVaultCompatToken()` (`norca/client/allocrunner/taskrunner/vault_hook.go:212`) so plaintext secrets are never written to the client's local BoltDB state store.

---

## 7. Server, Client, ACL & Job HCL Configuration Reference

### 7.1 Server & Client Agent Configuration (`norca.hcl`)

```hcl
server {
  enabled          = true
  bootstrap_expect = 3

  # Keyring & Secret Store Seal Configuration (norca/structs/config/keyring.go)
  keyring {
    seal "shares" {
      shares    = 5 # Total Shamir shares (2..255, default 5)
      threshold = 3 # Shares required to unseal (2..shares, default 3)
    }
  }

  # Built-in Secret Store Server Configuration (command/agent/config_secrets.go)
  secrets {
    audit_file             = "/var/log/norca/secrets-audit.json"
    audit_rotate_bytes     = 104857600 # 100 MiB
    audit_rotate_duration  = "24h"
    audit_rotate_max_files = 30
    vault_fallback         = true      # Serve job vault blocks from built-in store when Vault is disabled
  }
}

client {
  enabled = true

  # Client Secret Routing & Vault Compatibility (command/agent/config_client_secrets.go)
  secrets {
    # Optional prefix routing (longest prefix wins)
    route "legacy/" {
      provider = "vault"
    }

    # Map Vault KV paths to built-in store paths when external Vault is disabled
    vault_kv "kv/data/" {
      kv_version = 2
      path       = "apps/" # Reads of "kv/data/db/pg" map to built-in secret "apps/db/pg"
    }

    # Local Vault-compatible HTTP endpoint for tasks with vault {} blocks
    vault_compat {
      enabled    = true  # Exposes ${attr.norca.secrets.vault_compat} = "true"
      port       = 8250  # Binds 127.0.0.1:8250 and bridge gateway (172.26.64.1:8250)
      bridge     = true
      token_ttl  = "1h"
      rate_limit = 20.0
      rate_burst = 40
    }
  }
}
```

### 7.2 ACL Policy & Job Specification HCL (`job.norca.hcl`)

```hcl
# 1. ACL Policy (norca/acl/secrets.go)
namespace "prod" {
  capabilities = ["submit-job", "list-secrets"]

  secret "db/*" {
    capabilities = ["read-secret", "write-secret"]
  }

  secret "db/root-*" {
    capabilities = ["deny"]
  }
}
```

```hcl
# 2. Job Specification (norca/structs/secrets_delivery.go)
job "payment-api" {
  namespace   = "prod"
  datacenters = ["dc1"]

  secrets {
    default_provider = "norca"
  }

  group "backend" {
    # Grant workload identities read-secret on matching paths in "prod"
    # (Requires job submitter to hold read-secret on "db/payment-api/*")
    secret_access {
      path = "db/${job}/*"
    }

    task "api" {
      driver = "docker"

      # Native secret delivery block
      secret "pg-creds" {
        path          = "db/${job}/postgres"
        version       = 0          # 0 = track latest version and auto-rotate
        change_mode   = "signal"   # restart | signal | script | noop
        change_signal = "SIGHUP"
        optional      = false
        timeout       = "5m"

        # Deliver selected items into task environment (redacted from client state)
        env {
          DB_USER = "username"
        }

        # Deliver formatted file into secrets/ (mode 0400, symlink-safe)
        file {
          destination = "secrets/pg.json"
          format      = "json" # env | json | raw:<item> | template
        }
      }

      # Native template function delivery
      template {
        data        = <<EOH
DSN="postgres://{{ with nomadSecret "db/payment-api/postgres" }}{{ .username }}:{{ .password }}{{ end }}@db.internal:5432/app"
EOH
        destination = "secrets/dsn.env"
      }
    }
  }
}
```

---

## 8. CLI & HTTP API Cheat-Sheet

### 8.1 Seal & Keyring Operations (`norca operator ...`)

| CLI Command | HTTP Endpoint | ACL Required | Description & Key Flags |
| :--- | :--- | :--- | :--- |
| `norca operator seal-status` | `GET /v1/operator/seal` | `operator:read` | Inspect region seal config, rewrap progress, server seal states, and uBixVault core status (`-server-id`, `-json`). |
| `norca operator unseal` | `POST /v1/operator/unseal` | `operator:write` | Submit one Shamir share via hidden prompt or stdin (`-server-id <id>`, `-reset`). Never accepts shares as CLI args. |
| `norca operator unseal -recover` | `POST /v1/operator/seal/recover` | `operator:write` | Submit an old Shamir share on the leader to recover a restored secret store (`-recover`, `-reset`). |
| `norca operator keyring rewrap` | `POST /v1/operator/keyring/rewrap` | `operator:write` | Online migration of region keys and secret store master key (`-to shares\|aead`, `-shares 5`, `-threshold 3`). |

### 8.2 Secret Store Operations (`norca secret ...`)

| CLI Command | HTTP Endpoint | ACL Required | Description & Key Flags |
| :--- | :--- | :--- | :--- |
| `norca secret put <path> ...` | `PUT /v1/secret/<path>[?cas=n]` | `write-secret` | Write items (`k=@file`, `k=-`, `-`, `k=v`) and metadata (`-check-index`, `-max-versions`, `-owner`, `-label`, `-expires-at`, `-rotate-every`). |
| `norca secret get <path>` | `GET /v1/secret/<path>[?version=n]` | `read-secret` | Audited value read; requires explicit `-item <k>` or `-format json\|env` (`-version <n>`). |
| `norca secret info <path>` | `GET /v1/secret/<path>?metadata` | `list-secrets` | View metadata and version history with keyed `hmac-sha256:` hashes (never values). |
| `norca secret list [prefix]` | `GET /v1/secrets?prefix=` | `list-secrets` | List secret metadata rows in namespace or `-namespace "*"` (`-filter`, `-per-page`, `-json`). |
| `norca secret delete <path>` | `DELETE /v1/secret/<path>` | `write-secret` | Soft-delete current or specified `-versions 1,2` (`-check-index`). Values become unreadable until undeleted. |
| `norca secret undelete <path>` | `POST /v1/secret/<path>/undelete` | `write-secret` | Restore soft-deleted versions (`-versions 1,2`, `-check-index`). |
| `norca secret destroy <path>` | `POST /v1/secret/<path>/destroy` | `destroy-secret` | Permanently erase ciphertext blobs for `-versions 1,2` or purge the entire secret with `-all`. |
| `norca secret import` | Multiple RPCs | `write-secret` | Bulk migrate from Vault KV v1/v2 (`-from-vault kv/app/`) or Nomad Variables (`-from-variable path/ -move`, `-prefix-map`, `-dry-run`). |

---

## 9. Summary for SREs and Security Officers

1. **Always Migrate Production Clusters to `seal "shares"` and Rotate Root Keys**: A cluster started without a `keyring` block defaults to `seal "aead"` for development convenience. After running `norca operator keyring rewrap -to shares -shares 5 -threshold 3`, always execute `norca operator root keyring rotate -full` so that any historical root keys previously stored on disk or in backups under `aead` are superseded.
2. **Audit Log Availability Is Fail-Closed by Design**: When `server { secrets { audit_file = "..." } }` is configured, the leader will refuse all secret value reads and writes (`500 Internal Server Error: secret store audit failed`) if the disk partition hosting `audit_file` runs out of space or becomes read-only. Monitor disk capacity and configure `audit_rotate_bytes` and `audit_rotate_max_files` on all servers.
3. **Zero Implicit Workload Access**: Unlike Nomad Variables—where tasks implicitly access `norca/jobs/<job>/<group>/<task>`—Norca secrets require explicit authorization via `secret_access` stanzas or job-bound ACL policies, and `secret_access` is cryptographically gated by `acl.AllowSecretGrant` at job submission time so a user with `submit-job` cannot escalate privileges to read secrets they do not already have `read-secret` permission on.
4. **Understand Seal-Wrapped Backups**: Automated backups (`backup { include_keyring = true }`) include `secret-store-key.age` encrypted to your offline Age public keys. Restoring a backup with `-age-identity` automatically re-wraps the secret store master key under the target cluster's current seal; if a backup was taken without `secret-store-key.age` and the cluster has since rekeyed its Shamir shares, use `norca operator unseal -recover` with the historical shares to unlock the restored store.
