<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Automated, Encrypted Backup and Disaster Recovery Architecture

## 1. Executive Summary & Core Principles

Norca introduces a native, zero-external-dependency backup and disaster recovery subsystem (`norca/norca/backup/`). In contrast to standard Nomad's manual `operator snapshot save` workflow, Norca provides continuous, scheduled, multi-destination backups protected by modern authenticated client-side encryption (`filippo.io/age`), granular retention policies, and cryptographically enforced two-person authorization for full rollbacks.

### Key Architectural Tenets:
1. **Region-Scoped Autonomy**: Because Raft consensus is strictly intra-region, backups capture the complete state of a single region (Raft state machine, keyring, and secret store root key).
2. **Zero-Trust Client-Side Encryption**: Backup archives are encrypted before transmission using modern asymmetric cryptography (`age`). Private decryption keys (`-age-identity`) are supplied ephemerally per request and are never stored on cluster servers.
3. **Multi-Destination Redundancy**: Backups can be concurrently mirrored to S3-compatible object storage (AWS S3, MinIO, Ceph, Cloudflare R2) and POSIX/shared filesystem directories.
4. **Two-Person Rule for Destruction**: Full region rollbacks require dual-management authorization: one administrator requests and previews the rollback; a second administrator must independently inspect and approve it.
5. **Non-Destructive Selective Restores**: Individual workloads, variables, or configurations can be selectively resurrected through standard API writes, respecting namespace ACLs, admission webhooks, quotas, and revision history.

```
+---------------------------------------------------------------------------------------+
|                                NORCA REGION LEADER                                    |
|                                                                                       |
|   +--------------------+     +---------------------+     +------------------------+   |
|   | Raft State Machine |     | Nomad Keyring Keys  |     | uBixVault Core Master  |   |
|   | (helper/snapshot)  |     | (Root/Derived Keys) |     | (Secret Store Root Key)|   |
|   +---------+----------+     +----------+----------+     +-----------+------------+   |
|             |                           |                            |                |
|             +---------------------------+----------------------------+                |
|                                         |                                             |
|                                         v                                             |
|                     +---------------------------------------+                         |
|                     |       Backup Manager (manager.go)     |                         |
|                     |     (Interval / Cron UTC Scheduler)   |                         |
|                     +-------------------+-------------------+                         |
|                                         |                                             |
|                                         v                                             |
|                     +---------------------------------------+                         |
|                     |     Authenticated Encryption (age)    |                         |
|                     |    Recipients: ["age1...", "age1..."] |                         |
|                     +-------------------+-------------------+                         |
+-----------------------------------------|---------------------------------------------+
                                          | Parallel Streaming Upload
                  +-----------------------+-----------------------+
                  |                                               |
                  v                                               v
+-----------------------------------+           +-----------------------------------+
|       S3 OBJECT STORAGE           |           |       DIRECTORY / SHARED MOUNT    |
|   (AWS S3, MinIO, Ceph, R2)       |           |       (NFS, CephFS, Local Path)   |
|                                   |           |                                   |
|   prefix/YYYY/MM/DD/<id>/         |           |   prefix/YYYY/MM/DD/<id>/         |
|     ├── manifest.json (plain)     |           |     ├── manifest.json (plain)     |
|     ├── state.snap.age (enc)      |           |     ├── state.snap.age (enc)      |
|     ├── keyring.age (enc)         |           |     ├── keyring.age (enc)         |
|     └── secret-store-key.age (enc)|           |     └── secret-store-key.age (enc)|
|   latest.json                     |           |   latest.json                     |
+-----------------------------------+           +-----------------------------------+
```

---

## 2. Backup Archive Structure & Objects

Each backup is stored under a timestamped directory hierarchy: `prefix/YYYY/MM/DD/<backup-id>/`. The ID is deterministically derived from the start time and the Raft commit index (`norca/norca/backup/manifest.go:50`):
$$\text{ID} = \text{YYYYMMDDTHHMMSSZ}-\langle\text{RaftIndex}\rangle$$

| Filename | Status | Format | Contents & Purpose |
| :--- | :--- | :--- | :--- |
| **`manifest.json`** | Unencrypted | JSON | Descriptive metadata, timestamp, Raft index/term, Norca/Nomad versions, SHA-256 checksums of encrypted blobs, age recipient fingerprint, and high-level object count summary. **Never contains secret or job payload data.** |
| **`state.snap.age`** | Encrypted | `age` envelope (gzip tar) | Full Raft state machine snapshot (MemDB dump: jobs, allocations, evals, nodes, namespaces, quotas, admission policies, custom objects). |
| **`keyring.age`** | Encrypted | `age` envelope | Nomad root cryptographic keys used for encrypting variables and tokens. Optional (`include_keyring = true`). |
| **`secret-store-key.age`**| Encrypted | `age` envelope | Norca's built-in uBixVault master core key. Enables instant recovery of secret stores without entering Shamir unseal shares. |
| **`latest.json`** | Unencrypted | JSON | Pointer file stored at the root of the destination prefix, referencing the latest verified backup ID and manifest. |

### 2.1 The Unencrypted Manifest Philosophy
The `manifest.json` file is intentionally left unencrypted. It exposes object summaries (e.g. `summary: {"jobs": 42, "allocations": 110, "variables": 25}`) and cryptographic integrity hashes without exposing sensitive payloads. This allows administrative tools (`norca operator backup list`) to inspect, verify, and filter backups across remote object stores without requiring operators to provide their private Age keys.

---

## 3. Storage Destinations & Transport Engine

Norca supports two primary destination drivers via the `Destination` interface (`norca/norca/backup/destination.go:30`).

### 3.1 S3-Compatible Object Storage (`type = "s3"`)
Built on `github.com/minio/minio-go/v7`:
- **Compatibility**: AWS S3, MinIO, Ceph Object Gateway, Cloudflare R2, Scaleway Object Storage, Google Cloud Storage (S3 interoperability).
- **Multipart Streaming**: Backups stream directly to S3 via multipart uploads with configurable top-level `part_size` (default `16MiB`) and `upload_concurrency` (default `4` workers). Large state snapshots never consume memory buffers equal to the backup size.
- **Server-Side Encryption (SSE)**: Supports `sse = "AES256"` or `sse = "aws:kms"` (`kms_key_id = "..."`) on top of client-side Age encryption (`norca/norca/backup/config.go:144`).
- **Pre-signed URLs**: Generates temporary pre-signed HTTP download URLs (`PresignGet`) for external tooling.

### 3.2 Directory Storage (`type = "directory"`)
- **Use Cases**: Local attached storage, network-mounted NFS, CephFS, or CIFS shares.
- **Atomic Commits**: Data is written to temporary files (`.<name>.tmp`) and atomically renamed upon completion to prevent partially uploaded backups from being read.
- **Critical Clustering Requirement**: If directory storage is used in a multi-server cluster, **the directory must reside on a shared network mount** visible to all servers at the same path. Because any server can become the Raft leader, a local disk path on a single node would cause leader transitions to lose visibility of prior backups and fail retention pruning.

### 3.3 Concurrent Multi-Destination Mirroring
When multiple destinations are declared (e.g. `destination "local"` and `destination "s3-offsite"`), the backup manager uploads to all destinations in parallel using goroutines. A failure on one destination (e.g. temporary S3 timeout) does not cancel uploads to the other destinations:
- **`complete`**: All destinations succeeded and verified.
- **`partial`**: At least one destination succeeded; failures are recorded in the status description.
- **`failed`**: All destinations failed; the backup record is marked failed.

---

## 4. Cryptographic Architecture

### 4.1 Asymmetric Envelope Encryption via `age`
All state and key objects are encrypted using `filippo.io/age` (`norca/norca/backup/crypto.go:48`):
- **Recipients**: One or more Age public keys (`age1ql3z7hjy...`) specified in configuration. Multiple recipients allow multiple administrators or disaster recovery teams to decrypt the backup independently.
- **Authenticated Encryption**: ChaCha20-Poly1305 or AES-GCM ensures that any tampering, byte truncation, or bit-flipping during transit or storage causes decryption to fail immediately before data touches the Raft FSM.
- **Memory Hygiene**: Secret store keys and plaintext crypto materials are explicitly wiped from memory (`clear(key)`) as soon as the ciphertext is emitted.

### 4.2 Ephemeral Identity Lifecycle
Private keys (`-age-identity`) are **never stored on the Norca cluster nodes**:
1. When an operator runs `norca operator backup contents`, `diff`, or `restore`, the private key file is read on the operator's local machine.
2. The identity string is transmitted inside the TLS/mTLS RPC payload for that specific call.
3. The server decrypts the target stream in-memory and immediately discards the private key from memory once the RPC completes.

### 4.3 Checksum Verification
During upload, a SHA-256 digest is calculated over the raw ciphertext. If `verify_checksum = true` is enabled, Norca immediately streams the uploaded object back from the storage destination and verifies the checksum against the manifest before marking the destination verified.

---

## 5. Scheduling, Retention, and Lifecycle Management

### 5.1 The Leader Scheduler Loop
Backups are orchestrated exclusively by the active Raft leader (`norca/norca/backup/manager.go:90`):
- **Schedule Syntax**: Supports standard time intervals (`interval = "1h"`) or standard 5-field UTC cron expressions (`schedule = "0 2 * * *"`).
- **Jittering**: Configurable `jitter` (e.g. `"5m"`) applies a deterministic, region-hashed stagger to scheduled backups, preventing hundreds of federated regions from hammering object storage at the top of the hour.
- **Retry Backoff**: If a backup fails, the scheduler waits `retry_interval` (default `10m`) before re-attempting.

### 5.2 Grandfather-Father-Son (GFS) Retention Policy
Pruning is executed automatically after each successful backup run (`norca/norca/backup/retention.go:20`):

$$\text{Keep} = \text{Pinned} \cup \text{MinKeep} \cup \text{Hourly} \cup \text{Daily} \cup \text{Weekly} \cup \text{Monthly}$$

- **`keep_hourly`**: Preserves the newest backup for each of the last $N$ hours.
- **`keep_daily`**: Preserves the newest backup for each of the last $N$ calendar days (UTC).
- **`keep_weekly`**: Preserves the newest backup for each of the last $N$ ISO weeks.
- **`keep_monthly`**: Preserves the newest backup for each of the last $N$ calendar months.
- **`min_keep`**: Safety floor (default 3); the $N$ newest usable backups are unconditionally kept regardless of age.
- **`keep_failed`**: Number of failed backup attempts to retain for troubleshooting before deleting.

### 5.3 Backup Pinning
Operators can pin critical backups (`norca operator backup pin <id>` or `save -pin`):
- Pinned backups have `Pinned = true` in their Raft record.
- Pinned backups are completely excluded from GFS retention pruning and cannot be deleted via `operator backup delete` until explicitly unpinned.

---

## 6. Restore Modes & Workflows

Norca provides three distinct operational restore modes depending on the incident type:

```
                            INCIDENT DETECTED
                                    |
            +-----------------------+-----------------------+
            |                                               |
   Accidental Deletion /                    Total Region Crash / State Corruption
   Granular Rollback                                        |
            |                               +---------------+---------------+
            v                               |                               |
    [ MODE A: SELECTIVE ]            Existing Cluster Live?         Bare-Metal Total Loss?
    - Single Job / Version                  |                               |
    - Single Variable / Quota               v                               v
    - Re-applied via standard       [ MODE B: FULL LIVE ]           [ MODE C: BOOTSTRAP ]
      write RPCs                    - 2-Person Approval             - DR Cluster Bringup
    - Admission & Quota checked     - Automatic safety snapshot     - Local client decrypt
    - Version history bumped        - Full Raft state wipe          - First node bootstrap
```

---

### Mode A: Selective / Item-Level Restore
*Use case: A developer accidentally purged a job, or a variable was overwritten.*
- **Scope**: Re-creates specific objects: `-job <name>[@version]`, `-variable <path>`, `-namespace <name>`, `-quota <name>`, `-policy <name>`.
- **Concurrency Limit**: Up to 100 items per request (`maxBackupRestoreItems = 100`).
- **Mechanism**: The leader decrypts the backup in memory, extracts the requested objects, and **re-submits them via standard internal RPC handlers** on behalf of the calling identity.
- **Security & Traceability**:
  - The caller's ACL tokens and namespace permissions are verified.
  - Admission controller webhooks and CEL policies run.
  - Resource quotas are re-checked.
  - Jobs are created as new versions in the job history (e.g. resurrecting version 2 creates version 4).

---

### Mode B: Full Region Restore (Live Cluster)
*Use case: Catastrophic configuration failure, ransomware, or widespread regional corruption where the running Raft state must roll back.*

To prevent a rogue or compromised administrator from destroying cluster state, full restores enforce the **Two-Person Rule** (`norca/norca/backup_full_restore.go:37`):

```
Administrator 1 (Requester)                         Administrator 2 (Approver)
---------------------------                         --------------------------
1. Request Full Restore:
   norca operator backup restore \
     -full -confirm <region> \
     -age-identity key1.txt <id>
              |
              v
   Server loads backup, generates diff:
   - Allocations stopped: 15
   - ACL tokens revoked: 2
   - Warnings: Region rollback
              |
              v
   Creates PENDING Restore Record (UUID)
   Status: "pending", Expires: now + 1h
              |
              +-----------------------------------> 2. Inspect Pending Request:
                                                       norca operator backup restores <uuid>
                                                    3. Approve & Execute:
                                                       norca operator backup approve \
                                                         -age-identity key2.txt <uuid>
                                                            |
                                                            v
                                                       Server validates:
                                                       Requester != Approver
                                                            |
                                                            v
                                                       [ EXECUTES FULL RESTORE ]
                                                       1. Automatic safety backup taken
                                                       2. Keyring injected to servers
                                                       3. Secret store master key re-wrapped
                                                       4. raft.Restore(snapshot) applied
```

#### Dual-Person Validation Constraints:
1. **ACL Enforcement**: ACLs must be enabled (`acl.enabled = true`). Both administrators must present valid management tokens.
2. **Distinct Identities**: `record.RequestedBy != record.ApprovedBy`. The token accessor of the approver must differ from the requester.
3. **Region Confirmation**: The requester must pass `-confirm <region>` matching the exact region name.
4. **Pre-Restore Safety Snapshot**: Before wiping the state, the leader automatically executes a complete backup of the existing state, guaranteeing an escape hatch if the rollback was mistaken.

---

### Mode C: Restore-Bootstrap (Bare-Metal Disaster Recovery)
*Use case: Complete loss of cloud infrastructure; standing up a brand-new cluster from S3 or local directory backups.*

Command: `norca operator backup restore-bootstrap -from <url> -age-identity <key>`

1. **Client-Side Decryption**: The administrator runs the command on a workstation. The backup archive is pulled from S3 or disk and decrypted locally on the administrator's laptop. S3 credentials and Age keys are never transmitted to the new nodes.
2. **Single-Node Seed**: A single new Norca server is started with `bootstrap_expect = 1` and matching `region`.
3. **Direct State Injection**:
   - The root keyring keys are written into the new server's keyring.
   - The secret store core master key is imported and re-sealed with the new server's unseal configuration.
   - The Raft snapshot is restored into the fresh BoltDB store.
4. **Cluster Expansion**: Once the seed server commits the state, its ACL tokens become active. Subsequent servers join the seed node, replicating the state and keyrings over mTLS Raft transport.

---

## 7. Server Configuration Reference

Backups are configured under the server's `backup` stanza in `norca.hcl`:

```hcl
server {
  enabled = true

  backup {
    enabled        = true
    schedule       = "0 2 * * *"  # Daily at 02:00 UTC (mutually exclusive with interval)
    jitter         = "5m"
    timeout        = "1h"         # Default 1h
    retry_interval = "10m"        # Default 10m

    part_size          = "16MiB"  # Default 16MiB
    upload_concurrency = 4        # Default 4

    include_keyring = true   # Backup variable & token crypto keys (default true)
    verify_checksum = true   # Download back from S3 to verify SHA-256 (default true)

    # Two-Person Rule Configuration
    require_approval = true  # Mandatory; cannot be disabled
    approval_ttl     = "1h"  # Time window for second admin approval (default 1h)

    # Client-Side Encryption
    encryption {
      mode       = "age"
      recipients = [
        "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
        "age1u80v5wv7n8ecl98k9vsq2d73w6x7v7q4m2tq8v7q4m2tq8v7q4m2tq8v7q"
      ]
    }

    # Grandfather-Father-Son Retention (Defaults from DefaultRetention)
    retention {
      keep_hourly  = 24
      keep_daily   = 14
      keep_weekly  = 8
      keep_monthly = 12
      min_keep     = 3
      keep_failed  = 10
    }

    # Primary Destination: AWS S3 or MinIO
    destination "primary-s3" {
      type   = "s3"
      bucket = "company-norca-backups"
      prefix = "clusters/${region}"
      region = "eu-central-1"
      prune  = true

      # Optional S3 Server-Side Encryption ("AES256" or "aws:kms")
      sse = "AES256"
    }

    # Secondary Destination: Mounted Shared Storage
    destination "nfs-backup" {
      type  = "directory"
      path  = "/mnt/shared-backup/norca/${region}"
      prune = true
    }
  }
}
```

---

## 8. CLI Command Cheat-Sheet

All administrative operations fall under `norca operator backup`:

| Command | Action | Key Flags |
| :--- | :--- | :--- |
| `norca operator backup list` | List all backups and schedule status | `-destination`, `-json` |
| `norca operator backup save` | Trigger an immediate manual backup | `-pin`, `-label key=val` |
| `norca operator backup inspect <id>` | View manifest, sizes, hashes, and counts | `-destination` |
| `norca operator backup contents <id>` | Browse objects (jobs, variables) in backup | `-age-identity <key>` |
| `norca operator backup diff <id>` | Compare backup with currently running state | `-age-identity <key>` |
| `norca operator backup pin <id>` | Protect a backup from retention pruning | `-unpin` |
| `norca operator backup delete <id>` | Delete an unpinned backup manually | `-force` |
| `norca operator backup restore <id>` | Perform selective restore of specific items | `-job <id>@<ver>`, `-variable <path>` |
| `norca operator backup restore <id>` | Request a full region rollback (Step 1) | `-full`, `-confirm <region>`, `-dry-run` |
| `norca operator backup restores` | List pending full restore requests | `<restore-id>` |
| `norca operator backup approve <id>`| Approve and execute full rollback (Step 2) | `-age-identity <key>` |
| `norca operator backup restore-bootstrap` | Seed a brand new cluster from a backup | `-from <s3://...>`, `-age-identity <key>` |

---

## 9. Summary for SREs and Security Officers

1. **Private Keys Never Reside on Servers**: The encryption is asymmetric. Cluster servers hold only public Age recipient keys. The compromise of a Nomad server never grants access to decrypt historical backup archives.
2. **Full Restores Leave an Audit Trail**: Every full restore requires two management tokens, records both identities in the Raft state, and logs audit events (`"audit": true`) to the system log.
3. **Always Run Backups with `include_keyring = true`**: Backing up the Raft state without the keyring leaves encrypted Variables and Secret Store entries permanently unreadable upon restoration.
4. **Directory Storage Requires POSIX Locking & Shared Mounts**: Never configure `type = "directory"` pointing to node-local scratch disks in multi-server environments.
