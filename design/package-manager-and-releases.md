<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Integrated Package Manager and Server-Side Release Architecture

## 1. Executive Summary & Paradigm Shift

In standard HashiCorp Nomad, packaging was historically an afterthought relegated to `nomad-pack`—a separate, external CLI binary that operated strictly client-side. Upstream `nomad-pack` merely rendered text templates into Job HCL specifications and issued raw `job run` API calls. It maintained no cluster-side state, lacked revision tracking, could not roll back failed deployments atomically, had no drift detection, and could not manage auxiliary resources such as namespaces, quotas, ACL policies, encrypted variables, admission controls, or custom objects (CRDs).
   
Norca replaces this paradigm with a **fully integrated, server-side package and release management platform**:

1. **Unified Binary (`norca pack`)**: The package manager is compiled directly into the `norca` core binary, sharing configuration, authentication, connection pooling, and CLI flags.
2. **First-Class Cluster State (`packs.norca.io/Release`)**: Releases are first-class, versioned objects stored in the Raft state machine (`MemDB`). The cluster tracks historical revisions, phase transitions, and ownership of every deployed resource.
3. **Multi-Resource Packaging**: A Norca pack is not merely a job template; it can declare and manage complete application stacks including **Namespaces, Quotas, ACL Policies, Object Kinds, Custom Objects, Admission Policies, Encrypted Variables, and Jobs**.
4. **Deterministic & Atomic Rollback**: Resources are installed in strict topological dependency order and removed in exact reverse order. If a deployment, health check, or lifecycle hook fails, the cluster automatically and atomically restores the prior revision.
5. **Continuous Drift Detection**: Norca continuously hashes live cluster objects against their release manifests, flagging out-of-band modifications without breaking running workloads.
6. **Enterprise Supply-Chain Security**: Built-in support for OCI registries, Cosign-compatible cryptographic signing and verification, immutable dependency lockfiles (`pack.lock`), and air-gapped offline bundles.

```
+---------------------------------------------------------------------------------------+
|                                    DEVELOPER WORKSTATION                              |
|                                                                                       |
|   +-----------------------+     +------------------------+     +------------------+   |
|   | Pack Source Directory |     | Cosign / Age Keypairs  |     | pack.lock        |   |
|   | (Templates, Metadata) |     | (Signing & Encryption) |     | (Pinned Digests) |   |
|   +-----------+-----------+     +-----------+------------+     +--------+---------+   |
|               |                             |                           |             |
|               +-----------------------------+---------------------------+             |
|                                             |                                         |
|                                             v                                         |
|                                  norca pack run / push                                |
|                                (Template Render & Lint)                               |
+---------------------------------------------+-----------------------------------------+
                                              |
                   +--------------------------+--------------------------+
                   | (Push OCI Artifact)                                 | (Server-Side Apply)
                   v                                                     v
+-------------------------------------+               +-------------------------------------+
|        OCI REGISTRY / GHCR          |               |         NORCA SERVER LEADER         |
|   (oci://ghcr.io/org/pack:1.0.0)    |               |                                     |
|                                     |               |  +-------------------------------+  |
|   ├── OCI Manifest & Digest         |               |  | Release Endpoint (RPC)        |  |
|   ├── Cosign Signatures / Attest.   |               |  +---------------+---------------+  |
|   └── Pack Tarball Layer            |               |                  |                  |
+-------------------------------------+               |  +---------------v---------------+  |
                                                      |  | Topological Resource Planner  |  |
                                                      |  | (Namespaces -> Jobs)          |  |
                                                      |  +---------------+---------------+  |
                                                      |                  |                  |
                                                      |  +---------------v---------------+  |
                                                      |  | Atomic Transaction Manager    |  |
                                                      |  | (Pre/Post Hooks, Rollback)    |  |
                                                      |  +---------------+---------------+  |
                                                      |                  |                  |
                                                      |  +---------------v---------------+  |
                                                      |  | Raft FSM State Store          |  |
                                                      |  | (packs.norca.io/Release)      |  |
                                                      |  +-------------------------------+  |
                                                      +-------------------------------------+
```

---

## 2. Pack Anatomy & Multi-Resource Schema

A Norca pack is a structured directory comprising metadata, typed variable definitions, dependency constraints, and resource templates:

```
my-app-pack/
├── metadata.hcl         # Pack name, version, description, and required server features
├── variables.hcl        # Typed schema for user inputs, validations, and defaults
├── pack.lock            # Generated cryptographic lockfile pinning dependency digests
├── templates/
│   ├── _helpers.tpl     # Reusable text/template and Sprig macros
│   ├── namespace.nomad  # Target namespace declaration
│   ├── quota.nomad      # Resource quota specification
│   ├── policy.nomad     # ACL policy granting task access
│   ├── secrets.nomad    # Encrypted Variables entries
│   ├── hooks.nomad      # Pre-install migration batch jobs
│   └── app.nomad        # Core service jobs
└── tests/
    └── test_job.nomad   # Verification jobs executed by 'norca pack release test'
```

### 2.1 Feature Requirements (`requires`)
Packs explicitly specify the cluster capabilities they depend upon in `metadata.hcl`:

```hcl
pack {
  name        = "secure-database"
  version     = "1.4.0"
  description = "HA PostgreSQL with automated backups and network policies"
}

requires {
  norca    = ">= 0.1.0"
  features = [
    "releases",
    "release-hooks",
    "custom-objects",
    "admission-cel",
    "backups",
    "native-service-catalog"
  ]
}
```

If a target cluster lacks any required capability (e.g. older server versions in a mixed cluster), the release is rejected at pre-flight before any resources are applied.

### 2.2 Supported Resource Types
Unlike upstream Nomad which only recognizes `Job` objects, Norca's release engine (`norca/norca/release_resources.go:52`) manages eight distinct resource kinds:

| Resource Kind Constant | Struct Type | Function in Application Stack |
| :--- | :--- | :--- |
| `ReleaseResourceNamespace` | `*structs.Namespace` | Isolated execution boundary for multi-tenancy. |
| `ReleaseResourceQuota` | `*structs.QuotaSpec` | CPU, Memory, and Disk usage limits enforced across the namespace. |
| `ReleaseResourceACLPolicy` | `*structs.ACLPolicy` | Granular capability rules required by the pack's workloads. |
| `ReleaseResourceObjectKind`| `*structs.ObjectKind` | Custom Resource Definitions (CRDs) for operators. |
| `ReleaseResourceAdmissionPolicy` | `*structs.AdmissionPolicy` | CEL or webhook policies validating workloads in the namespace. |
| `ReleaseResourceVariable` | `*structs.VariableDecrypted`| Config data and secrets written to Norca's encrypted variable store. |
| `ReleaseResourceObject` | `*structs.CustomObject` | Instances of custom resources (e.g. backup schedules, TLS certs). |
| `ReleaseResourceJob` | `*structs.Job` | Nomad task groups, allocations, and batch workloads. |

---

## 3. The Server-Side Release Engine (`packs.norca.io/Release`)

### 3.1 Release Object Model & Revisions
Releases are persisted in Raft memory as custom objects of kind `packs.norca.io/Release` (`norca/norca/release_endpoint.go:21`). Every operation—initial installation, variable update, version upgrade, or rollback—generates an immutable **Revision**:

```json
{
  "name": "production-redis",
  "namespace": "databases",
  "spec": {
    "pack": {
      "name": "redis",
      "version": "7.2.1",
      "registry": "oci://ghcr.io/bacanoo-io/norca-registry",
      "digest": "sha256:7a8b...9c",
      "trusted_signer": "backystart@gmail.com"
    },
    "revision": 3,
    "atomic": true,
    "max_history": 10,
    "values_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
  },
  "status": {
    "phase": "Deployed",
    "revision": 3,
    "conditions": [
      {"type": "Ready", "status": "True", "reason": "DeploymentsHealthy"},
      {"type": "Drifted", "status": "False", "reason": "StateInSync"}
    ],
    "resources": [
      {"type": "namespace", "name": "databases"},
      {"type": "variable", "name": "redis/config"},
      {"type": "job", "name": "redis-cluster"}
    ]
  }
}
```

### 3.2 Deterministic Topological Order
To prevent race conditions (e.g. a Job attempting to run in a Namespace that has not yet been registered, or accessing a Variable that does not exist), Norca strictly enforces an 8-stage forward execution sequence (`norca/norca/structs/release.go:137`):

```
+---------------------------------------------------------------------------------------+
|                       DETERMINISTIC APPLICATION ORDER (FORWARD)                       |
+---------------------------------------------------------------------------------------+
|  Stage 1: Namespaces           (Create isolation boundary)                            |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 2: Quotas               (Establish resource limits before allocations start)   |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 3: ACL Policies         (Establish security capabilities)                      |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 4: Encrypted Variables  (Seed configuration, credentials, and connection URI)  |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 5: Object Kinds (CRDs)  (Register schemas for custom operators)                |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 6: Admission Policies   (Install governance webhooks/CEL checks)               |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 7: Custom Objects       (Instantiate operator objects)                         |
|     │                                                                                 |
|     ▼                                                                                 |
|  Stage 8: Workload Jobs        (Schedule batch/service tasks and evaluate)            |
+---------------------------------------------------------------------------------------+
|                        DELETION / ROLLBACK ORDER (EXACT REVERSE)                      |
+---------------------------------------------------------------------------------------+
```

When upgrading or uninstalling, resources are pruned in **exact reverse order** (Jobs stopped first, followed by Custom Objects, Admission Policies, Object Kinds, Variables, and finally Namespaces).

### 3.3 Atomic Rollback & State Restoration
By default in production namespaces (or when `--atomic` is set), operations are transactional:
1. **Change Tracking**: As each resource is applied, Norca captures the pre-existing state (`appliedChange` in `release_apply.go:71`), including the previous modify index and content hash.
2. **Failure Detection**: If any job deployment fails health checks, a lifecycle hook fails, or the operation times out (`--timeout=5m`), the server aborts the sequence.
3. **Rollback Execution**: The server unwinds all applied changes in reverse order, restoring the previous revision's live state without manual intervention.

---

## 4. Lifecycle Hooks & Automated Verification

Packs can coordinate complex initialization tasks (database migrations, schema seeding, cache pre-warming, or external deregistration) using **Lifecycle Hooks** (`norca/norca/release_hooks.go:16`).

```
[Release Install / Upgrade Initiated]
                │
                ▼
      Run "pre-install" Hooks (sorted by hook_weight)
                │
                ├── (Fail) ──> [Abort & Rollback]
                │
                ▼ (Success)
      Apply Application Resources in Deterministic Order
                │
                ▼
      Wait for Deployment Health (Allocations Running & Passing Health Checks)
                │
                ├── (Fail) ──> [Abort & Rollback]
                │
                ▼ (Success)
      Run "post-install" Hooks (sorted by hook_weight)
                │
                ├── (Fail) ──> [Abort & Rollback]
                │
                ▼ (Success)
      [Release Marked "Deployed"]
```

### Hook Definition Syntax
Hooks are standard Nomad batch jobs annotated with metadata:
```hcl
job "db-migration" {
  type = "batch"

  meta {
    "pack.hook"        = "pre-install,pre-upgrade"
    "pack.hook_weight" = "10"
    "pack.hook_delete" = "on-success"
  }

  group "migrate" {
    task "run" {
      driver = "docker"
      config {
        image = "postgres-migrator:v2.1"
      }
    }
  }
}
```

### Release Testing
Packs can ship validation suites under `tests/` (or batch jobs marked with `pack.hook = "test"`). Running `norca pack test <release-name>` triggers test jobs in the target namespace, asserting that end-to-end endpoints are functional, and reports success or failure without mutating application state.

---

## 5. Continuous Drift Detection

Norca addresses configuration drift caused by out-of-band manual edits (`norca/norca/release_drift.go:21`):

1. **Content Hashing**: During release application, Norca calculates a canonical SHA-256 hash of the user-controlled configuration fields for every managed resource (`releaseResourceHash`). Internal volatile fields (modify index, timestamps, status) are excluded from the hash.
2. **Drift Evaluation**: When `norca pack status` is invoked, or during automated audit sweeps, the leader computes `computeDrift` against current `MemDB` state:
   - **`missing`**: A managed resource was deleted directly via CLI or API outside of `norca pack`.
   - **`modified`**: A managed resource's live content hash differs from the release record.
3. **Passive Reporting**: Norca updates the `Drifted` condition to `True` and publishes a `ReleaseDrifted` event on the event stream. It **never automatically overwrites** live drift, avoiding accidental restarts of modified production systems until an operator explicitly runs `norca pack run`, `norca pack upgrade`, or `norca pack rollback`.

---

## 6. Multi-Region Release Coordination

When applications must be deployed globally across federated regions, Norca's package manager coordinates multi-region releases natively (`norca/norca/release_multiregion.go:16`):

```
                                  COORDINATING REGION (europe-west)
                                                │
                 ┌──────────────────────────────┴──────────────────────────────┐
                 ▼                                                             ▼
     [ Authoritative Region ]                                      [ Satellite Region ]
          (europe-west)                                                 (us-east)
                 │                                                             │
  1. Applies Global Resources:                                  3. Waits for Global Replication:
     - Namespaces                                                  - Replicates Namespaces
     - Quotas                                                      - Replicates ACL Policies
     - ACL Policies                                                            │
                 │                                                             ▼
                 v                                              4. Applies Regional Resources:
  2. Applies Regional Resources:                                   - Local Service Jobs
     - Local Service Jobs                                          - Local Variables
     - Local Variables                                                         │
                 │                                                             ▼
                 ▼                                              5. Coordinates Multiregion
  6. Coordinates Multiregion Rollout via Cross-Region RPC          Deployment Watcher
```

1. **Global vs. Regional Resource Division**:
   - **Global Resources** (Namespaces, Quotas, ACL Policies) are applied exclusively in the federation's Authoritative Region and replicated across regions via Nomad's core state replication.
   - **Regional Resources** (Variables, Custom Objects, Jobs) are dispatched to each target region's leader via cross-region RPC.
2. **Failure Isolation**: If a multi-region deployment fails in `us-east` due to capacity exhaustion, only `us-east` rolls back atomically; healthy regions like `europe-west` remain deployed.

---

## 7. OCI Distribution & Supply-Chain Security

Norca standardizes pack distribution on Open Container Initiative (OCI) registries (`norca/pack/internal/cli/oci_commands.go:82`), replacing unauthenticated Git clones with enterprise image registries.

### 7.1 OCI Artifact Publishing
```bash
# Package, tag, and publish to GitHub Packages (GHCR) with Cosign key signing
norca pack push ./my-pack oci://ghcr.io/bacanoo-io/norca-registry/my-pack:1.4.0 \
  --sign-key /secrets/cosign.key
```
- **OCI Media Types**: Packs are packaged as standard OCI container layers with media type `application/vnd.norca.pack.layer.v1+tar`.
- **Cosign Compatibility**: Pack digests can be cryptographically signed using Cosign keys (`.key` / `.pub`).

### 7.2 Immutable Lockfiles (`pack.lock`)
Similar to `package-lock.json` or `go.sum`, running `norca pack run` generates or enforces `pack.lock`:
```hcl
pack "redis" {
  version = "7.2.1"
  digest  = "sha256:8f4c28b7...910a"
}

dependency "consul-connect" {
  source  = "oci://ghcr.io/bacanoo-io/norca-registry/consul-connect"
  version = "1.16.0"
  digest  = "sha256:1a2b3c...4d"
}
```
The `--lock` flag controls security enforcement:
- `verify` (default): Checks digests against `pack.lock`. Fails if tampering is detected.
- `require`: Fails if `pack.lock` is missing.
- `update`: Re-generates lock hashes based on newly vendored dependencies.
- `ignore`: Bypasses lock verification.

### 7.3 Air-Gapped Bundles (`norca pack bundle`)
For isolated, classified, or air-gapped data centers without external internet access:
```bash
# 1. On connected workstation: Create signed tarball bundle of all dependencies
norca pack bundle create --output norca-production-bundle.tar \
  --sign-key /keys/cosign.key \
  postgres consul openobserve

# 2. In air-gapped datacenter: Inspect and import bundle into the local cache
norca pack bundle inspect norca-production-bundle.tar
norca pack bundle import --verify-key /keys/cosign.pub norca-production-bundle.tar
```

---

## 8. CLI Command Cheat-Sheet

All packaging workflows are unified under `norca pack` (`norca/pack/internal/cli/main.go:127`):

### Packaging & Supply-Chain Operations
| Command | Purpose |
| :--- | :--- |
| `norca pack generate pack <name>` | Scaffold a brand new pack directory with templates. |
| `norca pack fmt <path>` | Canonical formatting of HCL templates and variable definitions. |
| `norca pack render <path>` | Dry-run template rendering to inspect generated Nomad HCL. |
| `norca pack push <path> <oci-url>` | Build OCI artifact, calculate digest, and push to registry. |
| `norca pack sign` / `verify` | Sign or verify an OCI pack artifact using Cosign keys (`key generate`). |
| `norca pack bundle create / import / inspect` | Create, inspect, and import signed multi-pack offline tarball bundles. |
| `norca pack registry add / list / update / delete` | Manage local pack registries. |

### Deployment & Release Operations
| Command | Purpose |
| :--- | :--- |
| `norca pack run <pack-name>` | Deploy pack as a managed, versioned release on the cluster. |
| `norca pack upgrade <pack-name>` | Upgrade an existing release to a new pack version or variable set. |
| `norca pack plan <pack-name>` | Preview execution plan, diffs, and resource additions. |
| `norca pack diff <release-name>` | Compare rendered templates or revisions against live cluster state. |
| `norca pack status [<name>]` | List releases or inspect phase, conditions, deployed resources, and drift status. |
| `norca pack history <name>` | Display chronological audit log of all revisions and actions. |
| `norca pack rollback <name> [rev]` | Roll back live cluster state to a specific prior revision. |
| `norca pack test <name>` | Execute verification hook jobs (`pack.hook = "test"`) for a release. |
| `norca pack uninstall <name>` | Gracefully remove all managed resources in reverse topological order. |

---

## 9. Cluster Registries, Catalog & Install Form

Besides the client-side `norca pack` flow, the servers hold **cluster pack registries** so that packs can be browsed and installed from the UI (Packages → Catalog / Registries) without a workstation.

### 9.1 Registries (`packs.norca.io/Registry`)
A registry is a cluster-scoped custom object of a region (`norca/norca/pack_registry.go`):

| Field | Meaning |
| :--- | :--- |
| `spec.type` | `oci`, `git`, `dir` (a directory on the servers; `builtin` is the packs that ship with Norca), `bundle` (an offline bundle on the servers) or `uploads` (packs uploaded with `PUT /v1/pack-upload`, kept in Raft as `packs.norca.io/PackUpload` chunks). |
| `spec.source` | `oci://…`, a Git URL, a server path, or `builtin`. |
| `spec.defaultRef` | Tag or branch to fetch (`latest`). |
| `spec.syncInterval` | Periodic sync on the leader, e.g. `15m`, `24h`; empty or `0s`: on request only. |
| `spec.trustedKeys` | Signing keys; with keys, only packs signed by them can be installed. |
| `spec.authSecret` | `{namespace, path}` of a Variable with the credentials of a private git or OCI source. |
| `status` | `phase` (`Ready`/`Error`), `message`, `lastSyncedAt` (last attempt), `lastGoodSyncAt`, `syncGeneration`, `packCount` and the indexed `packs`. |

Registry writes, pack details and server-side renders are RPCs (`Packs.*`, `norca/norca/pack_registry_endpoint.go`) forwarded to the **leader**, which fetches the registries into `<data_dir>/packs/<registry>/`. A sync fetches into a staging directory and replaces the cache only when it succeeded: a failed sync keeps the packs of the last good one (`status.lastGoodSyncAt`) and the cache keeps serving them. Every successful sync increments `status.syncGeneration`, recorded in the cache (`.generation`); a server whose cache was fetched for another generation, such as a new leader, fetches the registry again before it serves a pack.

`spec.syncInterval` (a duration of at least `1m`; empty or `0s` syncs on request only) makes the leader sync the registry periodically; it checks every 30s for registries whose interval passed since `status.lastSyncedAt`. NATS can make syncs come sooner without the cluster depending on it: `norca-natsbridge -sync-pushes` syncs the registries of a source when its CI publishes a push notice (`norca-natsbridge notify-push`), and `-sync-regions` syncs a registry when the registry of the same name and source in another region found other packs (`norca/natsbridge/README.md`). Private git and OCI sources take their credentials from a Variable named by `spec.authSecret` (`namespace`, `path`): the items `username` and `password` (or `token`) for `https://` git and OCI sources, `ssh_private_key` and `known_hosts` for git ssh sources. Saving a registry with an auth secret needs read access to that Variable, on every save, so that someone who can only write registries can't point a source at a host of their choosing and receive the credentials of others. The leader reads the Variable for every sync and sends the credentials to the host of the source only: OCI requests to other hosts (a token service elsewhere, redirects) carry no Authorization header, git gets them as a header for the source URL without following redirects, and ssh checks the host key against `known_hosts`. The credentials never reach a URL, the clone, the cache or the status, and the servers never use credentials of their own environment or docker configuration.

| HTTP | Purpose |
| :--- | :--- |
| `GET /v1/pack-registries`, `POST /v1/pack-registry`, `POST /v1/pack-registry/:name/sync`, `DELETE /v1/pack-registry/:name` | Registries (write: `packs.norca.io/Registry` write). |
| `GET /v1/packs[?registry=]` | Catalog: one entry per pack version with compatibility and signature. |
| `GET /v1/pack/:registry/:pack[?version=]` | `Pack`, `Versions`, `Readme` and the `UIFormSchema`. |
| `POST /v1/release/:name/plan-pack`, `…/deploy-pack` | The servers render the pack with `{Registry, Pack, Version, Values, Secrets}` and plan or apply it as a release revision. |

### 9.2 The Install Form
The install form is declared in HCL, next to the variables (`pack/internal/pkg/variable/decoder/ui.go`). The servers synthesize it as a JSON Schema with `x-ui-*` directives (`UIFormSchema`, `pack/sdk/pack/variables/schema.go`). Variables without a default are required. The UI sends only the values that differ from the defaults (plus required ones and those the release already had).

`metadata.hcl` declares the tabs, in their order:

```hcl
ui {
  tab "general" {
    label = "General"
  }
  tab "security" {
    label = "Security & Secrets"
    icon  = "lock"
  }
}
```

A variable may have `ui`, `secret` and `validation` blocks:

```hcl
variable "mode" {
  type    = string
  default = "standalone"
  ui {
    tab           = "general"
    group         = "Topology"
    order         = 1
    options       = ["standalone", "replica"]
    option_labels = { replica = "Primary + Replicas" }
  }
}

variable "replicas" {
  type    = number
  default = 2
  ui {
    tab     = "resources"
    widget  = "slider"
    unit    = "nodes"
    presets = [1, 3, 5]
    show_if = { field = "mode", equals = "replica" }
  }
  validation {
    integer = true
    min     = 1
    max     = 9
  }
}

variable "redis_password" {
  type    = string
  default = ""
  ui {
    tab     = "security"
    label   = "Redis Password"
    show_if = "auth_enabled"
  }
  secret {
    path = "norca/jobs/redis"   # a Variable the pack declares (*.var.hcl.tpl)
    item = "password"           # default: the variable name
  }
  validation {
    min_length = 12
  }
}
```

| Block | Attributes |
| :--- | :--- |
| `ui` | `tab`, `group`, `order`, `label`; `widget` (`text`, `number`, `slider`, `switch`, `segmented`, `select`, `secret`, `code`, `tags`, `key-value`, `object-list`, `namespace-select`, `region-select`, `node-pool-select`, `datacenter-multiselect`, `storage-volume-select`); `unit`, `step` (slider; needs a number type and validation min < max), `presets`; `options` (allowed values; a choice widget unless `widget` says otherwise) and `option_labels`; `show_if`: a variable name (shown when it is set) or `{ field = "<var>", equals \| notEquals \| in = … }`. Hidden variables are not sent. |
| `secret` | `path`, `item`. The value goes to that item of the Variable, applied by the servers but never stored in the release or rendered into the templates (they see the default). This holds for the UI, `norca pack run --var` and the server-side render; an upgrade without a value keeps the stored item. `--var-secret` overrides it. |
| `validation` | `min`, `max`, `integer`, `pattern`, `min_length`, `max_length`, `min_items`, `max_items`, `unique`, `item_pattern`, `item_min_length`. |

Conditions on the items of lists and maps or across variables are `rule` blocks of `variables.hcl` (`pack/internal/pkg/variable/rules`):

```hcl
rule "tls_mode" {
  condition     = !var.tls_required || contains(["mtls", "secret"], var.tls_mode)
  error_message = "tls_mode must be mtls or secret when tls_required is set."
}
```

The condition sees `var.<name>` and the job specification functions plus `regex`, `regexall`, `alltrue` and `anytrue`.

Without a `ui` block, the tab and group are inferred from the variable name: secret-like names (`password`, `token`, `secret`, …) → `secrets`; `cpu`, `memory`, `count`, `replica`, `scale` → `resources`; `port`, `network`, `tls`, `service`, `host` → `network`; `volume`, `storage`, `disk`, `backup` → `storage`; dependency variables → `dependencies`; everything else → `general`. Tabs used but not declared come last.

**Validation.** `validation` blocks, `ui` options and rules are checked before rendering by the CLI and by the servers that render a pack of a cluster registry. The synthesized schema of the `validation` blocks and options becomes the variables schema of the revision, which the servers check again; rules are checked where the pack is rendered. An explicit JSON Schema (`variables_schema = file("values.schema.json")` in `metadata.hcl`) is still accepted, laid over the HCL directives, and replaces them for validation; the packs of the Norca registry and the built-in packs no longer use one.

Secret-like variables without a `secret` block are recorded as `(redacted)` in the release values but are still rendered into the templates, as with `norca pack run --var`; prefer a `secret` block.

---

## 10. Architectural Comparison: Nomad-Pack vs. Norca Pack

| Capability | Upstream HashiCorp `nomad-pack` | Norca Integrated `norca pack` |
| :--- | :--- | :--- |
| **Binary Distribution** | Separate unbundled binary | **Built directly into `norca` core binary** |
| **Cluster State** | None (client-side template engine) | **First-class Raft object (`packs.norca.io/Release`)** |
| **Resource Scope** | Nomad Jobs only | **Jobs, Namespaces, Quotas, Policies, Secrets, CRDs** |
| **Rollback Support** | None (manual re-run required) | **Automatic atomic rollback on health/hook failure** |
| **Execution Order** | Arbitrary | **Topological forward apply; reverse teardown** |
| **Drift Detection** | None | **Continuous SHA-256 content drift monitoring** |
| **Multi-Region Deployments** | Unsupported | **Coordinated global/regional resource division** |
| **Distribution Format** | Git repositories only | **OCI Container Registries & Air-Gapped Bundles** |
| **Supply Chain Security** | None | **Cosign signing, digest verification, `pack.lock`** |
| **Secret Management** | CLI text interpolation | **Native integration with encrypted Variable store** |
