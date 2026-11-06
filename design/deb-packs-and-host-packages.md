<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Pack Types: HCL Packs and Debian Host Packages

**Status:** Parked (draft; to be continued) · 2026-10-29 · Norca checkout at `248279425`. All Go paths are under
`norca/` unless stated otherwise.

## 1. Goal

The pack manager installs one kind of pack today: HCL templates that render Nomad jobs and companion
resources (namespaces, quotas, ACLs, variables, object kinds, admission policies, objects). In practice
these run Docker containers. `exec` and `raw_exec` tasks have no way to get their host dependencies
(runtimes, libraries, tools) onto the nodes: operators install them by hand or with external
configuration management.

The pack manager gets a **pack type**:

| Type | What it installs | Scope | Unit of state |
| --- | --- | --- | --- |
| `hcl` (default, existing) | jobs and cluster resources rendered from templates | job / namespace | release (server state) |
| `deb` (new) | Debian packages on client nodes | node | release (desired) + per-node installed state |

Requirements:

- **D1.** A `deb` pack is bound to a platform (distribution, release, architecture) and to package
  versions. The pack carries no installer scripts of its own: it installs packages with dpkg.
- **D2.** Installation is node-level, not a job. Nodes are chosen by node pool, datacenter and constraints.
  A node that joins later gets the packages automatically.
- **D3.** For each node we know which managed packages are installed, in which version, since when, and
  the outcome of the last operation (install, upgrade, remove, failure).
- **D4.** Mass upgrades run in waves, with a health gate, pause, resume, abort and revert.
- **D5.** Jobs that depend on a package are not placed on a node that lacks the package. When a package
  is upgraded, the dependent allocations on that node are replaced in a controlled way.
- **D6.** UI, backup and the other stored data are covered (§8–§10).

## 2. Findings in the current code

1. **Packs have no type.** `metadata.hcl` (`pack/sdk/pack/metadata.go:93`), `ReleasePack`
   (`structs/release.go:396`), `IndexedPack` (`pack/catalog/catalog.go:45`) and `PackCatalogEntry`
   (`norca/pack_registry.go:121`) have no type field. The only type decision is the template file suffix:
   each suffix maps to a resource type (`pack/internal/pkg/release/resources.go:25-38`).
2. **Releases are custom objects.** `Release`, `ReleaseRevision`, `Registry` and `PackUpload` are built-in
   kinds of group `packs.norca.io` (`structs/release.go:31-45`, `norca/release_kinds.go:35-213`). They
   share the object table, the Raft types 82–87, snapshots, ACL object rules, the `CustomObject` event
   topic and backups. New kinds in the same group get all of this for free.
3. **Resource types are one switch.** `ReleaseResource` has one pointer field per type
   (`structs/release.go:632`). Apply, delete, read, hash and compare are switches in
   `norca/norca/release_resources.go` (`applyResource` :209, `deleteResource` :287, `readLive` :49,
   `releaseResourceHash` :139). The order is fixed: namespace 1 … job 8 (`structs/release.go:143-152`).
4. **No path from server to node except allocations and client RPCs.** The client RPC set is ClientStats,
   CSI, FileSystem, Allocations, Agent and NodeMeta (`client/rpc.go:279-311`). `NodeMeta.Apply`
   (`client/meta_endpoint.go:24`, forwarder `norca/client_meta_endpoint.go:28`) is the closest template:
   persist in client state, change the node, re-register.
5. **No Debian handling anywhere.** No Go code calls dpkg or apt-get.
6. **Fingerprints give `os.name` (`debian`/`ubuntu`), `os.version`, `kernel.arch` and `cpu.arch`**
   (`client/fingerprint/host.go:36-49`, `arch.go:25`). They do not give the Debian codename or the dpkg
   architecture (`armhf` vs `arm64`, `i386`).
7. **Changed node attributes or meta trigger node evaluations** (`shouldCreateNodeEval`,
   `norca/node_endpoint.go:272-300`). Plain attribute keys also go into the computed node class
   (`structs/node_class.go:34`); keys under `unique.` do not.
8. **An exec task does not see an upgraded package after an in-place restart.** The exec chroot is built by
   hard-linking (or copying) host files into the task directory, and existing entries are skipped
   (`client/allocdir/task_dir.go:222`, `fs_unix.go:94`). dpkg replaces files by rename, so the chroot
   keeps the old inodes. `Allocations.Restart` reuses the same alloc dir. Only a **new allocation** picks up
   the new files. `raw_exec` has no chroot, so a restart is enough there, but one rule for both is simpler.
9. **The default exec chroot is `/bin /etc /lib /lib32 /lib64 /sbin /usr` plus two `/run` paths**
   (`client/config/config.go:57-71`). A package that installs under `/opt` is invisible to exec tasks unless
   the client's `chroot_env` is extended.
10. **Binaries do not fit Raft.** Uploads are capped at 8 MiB (`pack/catalog/upload.go:26`) and stored as
    160 KiB `PackUpload` chunks. A custom object is at most 256 KiB (`structs/objects.go:43`). Everything in
    Raft goes to every server, every snapshot and every backup.
11. **Useful building blocks exist:**
    - the eviction API, which honours disruption budgets (`norca/disruption.go:30`, `Alloc.Evict`);
    - the drainer (`norca/drainer/`) and node eligibility;
    - the OCI client and cosign signing of the pack supply chain (`pack/internal/pkg/oci`, `signing`,
      `supplychain`);
    - the leader loops of releases (`leader.go:416`);
    - the controller SDK (`controller/`), but it reaches nodes only through the HTTP API.

## 3. Options considered

**A. A `deb` pack renders a sysbatch job that runs dpkg** (raw_exec as root, one run per node,
`max_parallel` for waves).

- No new client code.
- Weak on D3–D5. There is no per-node ledger beyond allocation history, and nothing stops a dependent job
  from landing on a node before the installer ran.
- Reverting means writing a second job. Nodes that join later need the sysbatch job to be re-run.
- `raw_exec` must be enabled cluster-wide, and that widens the attack surface for every job.

**B. A native host-package manager in the client, driven by server-side desired state** (recommended).

- Desired state lives in the server. The client pulls it like allocations and reports per-node state.
- The scheduler sees installed packages as node attributes, so dependent jobs wait for them.

Option A is a possible throw-away prototype only. The rest of this document describes B.

## 4. Pack format

### 4.1 Type in `metadata.hcl`

```hcl
pack {
  name    = "ffmpeg-runtime"
  version = "1.0.0"
  type    = "deb"          # "hcl" (default) | "deb"
}
```

- The `type` field is added to `MetadataPack`, `ReleasePack`, `IndexedPack`, `PackCatalogEntry`, the kind
  schemas and `api/releases.go`.
- A missing `type` means `hcl`, so every existing pack and release stays valid.
- A `deb` pack has no `templates/` directory. Mixing types in one pack is rejected. A dependency between an
  HCL pack and a deb pack goes through `requires` (§7.1), not through mixing.

### 4.2 Package declaration (`packages.hcl`)

```hcl
platform "debian-12-amd64" {
  distribution = "debian"   # matches attr.os.name
  release      = "12"       # matches the major of attr.os.version, or the codename
  codename     = "bookworm" # matches attr.os.codename (new fingerprint)
  arch         = "amd64"    # matches attr.dpkg.arch (new fingerprint)

  package "ffmpeg-norca" {
    version = "7.1.1-1~bpo12+1"
    source  = "oci://registry.example.com/debs/ffmpeg-norca:7.1.1-1-bookworm-amd64"
    sha256  = "…"
  }

  # The dependency closure that the base system does not have.
  package "libavcodec61" {
    version = "7.1.1-1~bpo12+1"
    source  = "https://debs.example.com/pool/libavcodec61_7.1.1-1~bpo12+1_amd64.deb"
    sha256  = "…"
  }
}

platform "debian-12-arm64" { … }

install {
  hold           = true      # apt-mark hold, so unattended-upgrades does not touch them
  adopt_existing = false     # refuse if the package is already installed but not by Norca
  remove_on_uninstall = true
  purge          = false
  debconf        = { "ffmpeg-norca/enable_x" = "boolean false" }
}
```

- **`variables.hcl` works as for HCL packs.** Variables can choose the version (one `platform`/`package`
  set per version), the debconf answers, the targets and the rollout strategy. `ui`, `validation` and
  `rule` blocks keep working, so the install form works without changes.
- **The server resolves every `platform` block when it renders the pack.** Each platform becomes a
  `host-package` resource (§5.1). One release covers all its platforms. A node gets the platform block that
  matches its fingerprints. A node that matches no platform but is in the target set is reported as
  `unsupported`; it is not an error of the release.
- **Debian versions are not semver.** Ordering uses the Debian algorithm: epoch, upstream version,
  revision, `~` sorts before everything. This needs an own implementation (about 100 lines) or a vetted
  MIT/BSD library. It is also needed for a new constraint operator (§7.2).

### 4.3 Where the `.deb` files live

They are **not** stored in Raft (§2/10). Sources:

| Source | Use | Notes |
| --- | --- | --- |
| `oci://` | default | Reuse the ORAS client; the same registry can hold packs and packages; cosign signatures. |
| `https://` | external pool or a mirror | `sha256` mandatory. |
| `bundle` | air-gapped sites | The bundle carries the `.deb` files. The leader unpacks them into `<data_dir>/packages` and serves them (§4.4). |

- **The client downloads the files itself.** It uses the go-getter sandbox (Landlock), but into its own
  cache directory `<data_dir>/host-packages/cache/` instead of an alloc dir.
- **The `sha256` is checked before dpkg sees the file.** A missing or wrong digest blocks the install.
- **The previous version's files stay in the cache** until the next successful rollout, so a revert works
  without the source.

### 4.4 Air-gapped bundles (later phase)

In an air-gapped site the clients can only reach the servers.

- **Proposal:** a read-only server endpoint `/v1/host-package-blob/<sha256>` that serves files from the
  servers' local blob cache. The cache is filled from bundles on every server, not only on the leader.
- **The leader does not replicate files through Raft.** Each server fetches missing blobs from the leader
  over the RPC port, in chunks, with digest checks.
- **The blob cache is local data, not state:** it is not in snapshots or backups (§9).

### 4.5 Custom packages and the Norca package convention

The packages are mostly **custom Debian packages** built for Norca, not stock distribution packages. We
control how they are built, so many problems are avoided by convention instead of handled at install
time. A package that follows the convention is a *Norca package*:

| Rule | Why | Checked |
| --- | --- | --- |
| Installs under `/opt/norca/<package>/` only (binaries, libraries, default config) | No clash with distribution files; one path to add to `chroot_env` (P2); clean removal | Client, after install: `dpkg -L` against the prefix |
| No systemd units, no init scripts, no service start or restart in maintainer scripts | Norca runs the process; no second daemon on the same port or data (§7.4) | Build helper lint; client checks `dpkg -c` for `lib/systemd`, `etc/init.d` |
| No data in the package; data paths are host volumes | Data survives swap and removal | Lint: no files under `/var/lib` |
| Self-contained: bundled or statically linked libraries; `Depends` only on base-system packages (`libc6`, `libssl3`, …) | The closure stays small; the simulation (§6.2) rarely fails; same result on every node | Simulation on the node; lint lists non-base `Depends` |
| Maintainer scripts are minimal and idempotent, without prompts | Unattended install; safe retry after an interrupted run (P7) | Lint |
| Control fields `X-Norca-Package: 1` and optionally `X-Norca-Restart: stop-start` | The client recognises Norca packages; the package can suggest its own `dependents` strategy | Client reads the control file |
| Version format `<upstream>-<n>norca<m>` | Ordering stays correct against stock versions (§4.2) | Lint |

Consequences:

- **The default is closed resolution only.** `resolve = "apt"` remains, but only for stock packages.
- **Debconf is not needed for Norca packages.** Configuration comes from job templates (`template` blocks
  into `/local`), not from the package. The `debconf` option stays only for stock packages.
- **Stock packages are second-class but supported.** They are installed with `prevent_service_start` and
  get no prefix check. The UI shows them as "not a Norca package".
- **A build helper keeps the convention cheap:**
  - `norca pack deb build` builds the `.deb` from a directory and a small manifest;
  - `norca pack deb lint` checks the rules above;
  - `norca pack deb push` uploads it to OCI with platform annotations and a cosign signature.
- **The helper does not need dpkg.** It can use the Go library `goreleaser/nfpm` (MIT), so packages build
  on Windows and macOS too. This matches the Windows build of Norca.

## 5. Server-side model

### 5.1 New resource type and kinds

**Resource type `host-package`.** It is added to `ReleaseResource`, to `Validate`/`Ref`/`Copy` and to the
switches in `release_resources.go`.

- **Order:** after `object` (7) and before `job` (8). This way an HCL release that installs both through
  `requires` sees the packages before its jobs. Deletion runs in reverse as usual.
- **Apply does not touch nodes.** It writes or updates the desired state, i.e. a `HostPackageSet` object,
  and starts a rollout. The release apply returns quickly. Rollouts can take hours on large fleets, so the
  per-release lock (`release_kinds.go:249`) must not be held for that long.

**New built-in kinds** in `packs.norca.io`. They are cluster-scoped within a region and cannot be written
through the generic object API, like the other built-in kinds (`isBuiltinObjectGroup`).

| Kind | Name | Written by | Content |
| --- | --- | --- | --- |
| `HostPackageSet` | `<release>` | release apply | spec: platforms → packages (name, version, source, sha256), install options, target selector, rollout strategy; status: counts per state, `Ready`/`Progressing`/`Degraded` conditions |
| `HostPackageRollout` | `<release>.<revision>` | rollout controller (leader) | waves, current wave, per-wave node lists, pause/abort flags, health results, start/end times |
| `NodeHostPackages` | `<node-id>` | server, from client reports | per package: desired version, installed version, previous version, owning release, state (`pending`, `downloading`, `installing`, `installed`, `failed`, `held-by-other`, `unsupported`, `removing`), last error, timestamps |

**Size and write rate.**

- **One `NodeHostPackages` object per node** keeps the object count at O(nodes), not O(nodes × packages).
- **A status write happens on state transitions only,** not on heartbeats. That is about 4–6 writes per
  node per rollout.
- **The 20-entry object event log** (`DefaultObjectEventLogLimit`) is enough for a per-node history of
  recent operations. Longer history is out of scope.
- **When a node is garbage-collected, its object goes too:** an owner-reference style cleanup on node GC.

### 5.2 Target selection

The target selection lives in the `HostPackageSet` spec and defaults to the pack's install form.

```hcl
targets {
  node_pool   = "media"
  datacenters = ["dc1", "dc2"]
  constraint { attribute = "${node.class}" value = "gpu" }
}
```

- **The node pool is the primary boundary.** It is the existing way to split fleets, and it is already
  an ACL and quota boundary.
- **Only Linux nodes with the new `dpkg` fingerprint qualify.** Windows and non-Debian Linux nodes are
  never targets.
- **Two releases may not own the same package name on overlapping node sets.** This is checked at apply
  and reported as a conflict, like resource ownership in `release_apply.go:833`. Without this check, two
  releases could fight over a package version.

### 5.3 Rollout controller

The controller is a leader loop next to `releaseLeaderLoop`, started from `leader.go`.

```
rollout {
  max_parallel      = 1        # nodes, or "10%"
  canary            = 1        # nodes in a first wave that waits for approval (optional)
  min_healthy_time  = "2m"     # after install and dependent allocs healthy
  healthy_deadline  = "15m"
  dependents        = "stop-start" # "stop-start" | "replace" | "drain" | "none"   (§7.3, §7.5)
  approve           = "each-wave"  # "each-wave" | "canary" | "none"
  order_by          = ""           # e.g. "meta.redis_role", with order = ["replica", "primary"]
  on_failure        = "pause"      # "pause" | "revert"
  max_failed_nodes  = 1
}
```

1. **Plan.** The controller computes the target nodes from the selector and splits them into waves:
   - canary first;
   - then `max_parallel` per wave;
   - spread across datacenters, so that one wave does not hit one datacenter only.
2. **Assign.** For the nodes of the current wave, it sets the desired version in their `NodeHostPackages`
   objects. Nodes outside the wave keep their old desired version.
3. **Clients act** (§6) and report back.
4. **Gate.** A node passes when:
   - the package reached `installed` with the right version;
   - the node fingerprint reports it;
   - the dependent allocations are replaced and healthy for `min_healthy_time`, judged by deployment
     health or checks.
5. **Next wave**, or **pause/revert** when `max_failed_nodes` is exceeded. Revert sets the previous
   version as desired on the nodes already done. Downgrades use
   `apt-get install --allow-downgrades pkg=ver` from the cached previous `.deb`.
6. **Supersede.** A new revision supersedes a running rollout, like deployments. The old rollout stops
   assigning, and the new one plans from the current per-node state.

**Nodes that join later or come back** get the desired version of the latest finished wave plan directly,
without waiting for a rollout. A node that was down during a rollout catches up when it returns, because
the client pulls its desired state.

## 6. Client side

### 6.1 Pull model

The client works like `watchAllocations`, not like a pushed RPC. That way it survives disconnects, server
restarts and leader changes.

- **`Node.GetHostPackages`:** a blocking query for the node's desired package set, from its
  `NodeHostPackages` object.
- **`Node.UpdateHostPackages`:** reports state transitions. The server writes them into the object and
  the event log.
- **Placement:** the client loop lives in a new `client/hostpkg/` package. It is Linux only
  (`_linux.go`); on other platforms it is a no-op, which keeps the Windows build working.

### 6.2 Install sequence on the node

1. **Preflight checks:**
   - `dpkg --print-architecture` matches;
   - the dpkg lock is free (wait with a timeout, because unattended-upgrades may hold it);
   - there is enough disk space;
   - the package is not owned by another release;
   - when `adopt_existing = false`, the package is not installed outside Norca.
2. **Download and verify.** The client downloads into the cache and checks `sha256`. It checks the cosign
   signature too when the pack requires it.
3. **Simulate.** `apt-get install --simulate --no-download ./a.deb ./b.deb` reports unmet dependencies
   **before anything changes on the node**. Unmet dependencies mean `failed` with the list.
4. **Debconf.** Preseed the answers with `debconf-set-selections`.
5. **Install.** `DEBIAN_FRONTEND=noninteractive apt-get install --no-download -o Dpkg::Options::=--force-confold ./*.deb`,
   one transaction for the whole closure.
6. **Hold.** `apt-mark hold` on the managed packages when `hold = true`.
7. **Record and report.** Write the ledger (§6.3), set the fingerprint attributes, and report `installed`.

**Resolver choice.** The default is closed resolution: the pack carries the closure, and the simulation
fails on gaps. An opt-in `resolve = "apt"` allows apt to fetch missing dependencies from the node's
configured repositories. That is not reproducible across nodes, so the UI shows it as a warning.

### 6.3 Client state

A new bolt bucket `host_packages` (`client/state/db_bolt.go`) holds the ledger. It needs interface methods
in `client/state/interface.go`, plus mem and noop backends.

- **The ledger is the source of truth for "managed".** It decides which installed packages Norca owns:
  - Norca never removes a package it did not install;
  - an OS-installed package with the same name is a conflict, unless `adopt_existing` is set.
- **Per package it stores:** name, installed version, previous version, owning release and revision,
  cached file digests, and the time of the last operation.
- **After a client restart in the middle of a dpkg run,** the client runs `dpkg --audit`. If needed it
  runs `dpkg --configure -a`, then reconciles the ledger with `dpkg-query` before it reports anything.

### 6.4 Fingerprints

A new Linux fingerprinter `dpkg`, registered in `fingerprint_linux.go`. It sets these attributes:

| Attribute | Example | Notes |
| --- | --- | --- |
| `dpkg.version` | `1.21.22` | Its presence means the node is eligible. |
| `dpkg.arch` | `amd64`, `arm64`, `armhf` | `cpu.arch` cannot tell `armhf` from `armel`. |
| `os.codename` | `bookworm` | From `/etc/os-release`. |
| `pkg.deb.<name>` | `7.1.1-1~bpo12+1` | **Only managed packages**, plus an optional allowlist in client config. |

Keeping the `pkg.deb.*` attributes to managed packages matters for three reasons:

- **Node size and Raft writes.** A node has 1000+ packages; reporting all of them would bloat every
  `Node.Register`.
- **The computed class.** `pkg.deb.*` changes the computed class. That is acceptable for a few managed
  keys, but not for every OS package update.
- **Evaluation churn.** Every attribute change triggers a node evaluation (`shouldCreateNodeEval`).

## 7. Jobs that depend on packages

### 7.1 Declaring the dependency

**In a pack (`metadata.hcl`).** An HCL pack lists the host packages it needs:

```hcl
requires {
  host_package "ffmpeg-norca" { version = ">= 7.1" }
}
```

The release apply checks that a deb release provides the package on the pack's target pool. It
**warns**, and does not fail, because the package can arrive later.

**In a job (jobspec).** Phase 1 uses job meta, handled by an admission mutator:
`meta { "norca.host_packages" = "ffmpeg-norca>=7.1" }`.

- **The mutator adds implicit constraints.** The constraint is
  `${attr.pkg.deb.ffmpeg-norca}` with operator `debversion` and value `>= 7.1`. Jobs are then not placed
  on nodes without the package.
- **The mutator records the dependency**, which the rollout controller uses to find dependents (§7.3).

A first-class `host_package` block in task or group is cleaner. It is more work: struct, API, HCL parser,
diff and the legacy UI. It can come later and reuse the same mutator logic.

### 7.2 Constraint operator `debversion`

A new operator in `scheduler/feasible.go` (`checkConstraint` :833) with Debian ordering (§4.2). The
`semver` and `version` operators order Debian versions wrongly. Examples are epochs (`1:2.0` > `3.0`) and
`~` (`7.1~rc1` < `7.1`).

### 7.3 What happens to dependent allocations on upgrade

**Finding dependents.** The dependents of a package on a node are the running allocations on that node
whose job carries the dependency (§7.1). This includes `exec`, `raw_exec`, and `java` / `qemu` when they
use the host's runtime.

**Docker tasks** are only affected if they bind-mount host paths from the package. They are not detected
automatically; they can declare the dependency to opt in.

| `dependents` | Order on the node | Use |
| --- | --- | --- |
| `replace` | 1. Install the new version. 2. Replace each dependent allocation through the **eviction API** (`Alloc.Evict`), so disruption budgets apply. 3. Wait for the replacements to be healthy. | Services with several instances, system jobs. |
| `stop-start` (hard swap, default) | 1. Stop the dependents on the node (eviction API; `ignore_budgets = true` allows a planned outage). 2. Install. 3. The dependents are placed again on the same node (system jobs, or jobs pinned to the node) and must become healthy. | A daemon whose binary comes from the package and must not run while it is overwritten (the Redis example in §7.4). |
| `drain` | 1. Mark the node ineligible and evict the dependents (respecting budgets). 2. Install. 3. Mark the node eligible again; the scheduler places allocations back. | Packages that must not change under a running process (shared libraries with an ABI change). |
| `none` | Install only. | Tools that are run fresh by each task. |

Notes:

- **A restart is not enough for `exec`** (§2/8): the chroot keeps the old inodes. Replacing the
  allocation is the only correct action, and it is also correct for `raw_exec`.
- **The running process keeps the old files during `replace`.** dpkg renames files, so the old inodes stay
  open until the allocation is replaced. This is fine for most packages. Packages whose maintainer scripts
  restart a systemd service, or migrate data, need `drain`.
- **Mass upgrades are paced by both the waves and the disruption budgets.** A wave cannot complete while
  budgets block evictions. The rollout shows that as `blocked by disruption budget`, not as a failure.

### 7.4 Example: native Redis with a hard swap

**Scenario.** Redis runs natively on the nodes, not in Docker. Its binaries come from a `.deb` that
installs under a fixed local path, here `/opt/norca/redis` (the real path is a property of the package
build). A Norca job runs `redis-server` from that path. An upgrade installs the new `.deb`, which
overwrites the binaries. The Redis tasks on the node are then restarted **as a hard swap**: stop, replace,
start. No old and new process run side by side.

**Deb pack `redis-native`** (`packages.hcl`):

```hcl
platform "debian-12-amd64" {
  distribution = "debian"
  codename     = "bookworm"
  arch         = "amd64"

  package "norca-redis" {
    version = var.redis_version            # e.g. "8.0.2-1"
    source  = "oci://registry.example.com/debs/norca-redis:${var.redis_version}-bookworm-amd64"
    sha256  = var.redis_sha256
  }
}

install {
  hold                = true
  adopt_existing      = false
  remove_on_uninstall = true
}

targets {
  node_pool = "redis"
}

rollout {
  max_parallel     = 1
  min_healthy_time = "1m"
  dependents       = "stop-start"   # hard swap
  ignore_budgets   = false          # true: accept the outage of the whole wave
  on_failure       = "pause"
}
```

**Package requirements.** The package `norca-redis` is built for Norca:

- **No systemd unit, and no automatic service start in `postinst`.** Norca runs the process. The stock
  Debian `redis-server` package starts its own service on port 6379, which would clash with the job. If a
  stock package must be used, the installer runs it with a `policy-rc.d` that forbids starts, and masks
  the unit. This is a per-package option: `install { prevent_service_start = true }`.
- **Binaries and config only.** The data directory is not in the package. It lives in a host volume, so it
  survives the swap.

**HCL pack (or plain job) that runs Redis on those nodes:**

```hcl
job "redis" {
  type      = "system"            # one instance per node of the pool; or a service job pinned per node
  node_pool = "redis"

  meta {
    "norca.host_packages" = "norca-redis>=8.0"
  }

  group "redis" {
    volume "data" {
      type   = "host"
      source = "redis-data"
    }

    task "redis" {
      driver = "exec"             # or raw_exec
      config {
        command = "/opt/norca/redis/bin/redis-server"
        args    = ["/local/redis.conf"]
      }
      volume_mount {
        volume      = "data"
        destination = "/data"
      }
    }
  }
}
```

**`/opt` must be visible in the chroot** for `exec`. The client adds it to `chroot_env`, for example
`"/opt/norca/redis" = "/opt/norca/redis"`. Otherwise the binary is not found (P2). With `raw_exec` no
chroot change is needed.

**Upgrade flow, per node and per wave** (`max_parallel = 1`):

1. The operator upgrades the release `redis-native` with `redis_version = "8.0.3-1"`. The plan preview
   lists every node of the `redis` pool with `8.0.2-1 → 8.0.3-1`, and the `redis` allocation on each node.
2. The controller assigns the new version to the first node.
3. The client checks, downloads and verifies the package, then simulates the install. Nothing has changed
   on the node yet. If the check fails, the node is `failed` and Redis keeps running.
4. **Stop:** the controller evicts the `redis` allocation on the node and waits until it is stopped.
5. **Swap:** the client installs `norca-redis 8.0.3-1` (dpkg overwrites `/opt/norca/redis/bin/*`) and
   reports `installed`. The fingerprint `pkg.deb.norca-redis` becomes `8.0.3-1`.
6. **Start:** the system scheduler places a new `redis` allocation on the node. It is a new alloc dir, so
   the exec chroot gets the new binaries (P1). It reads its data from the host volume.
7. **Gate:** the allocation is healthy for `min_healthy_time`. Then the next node follows.
8. **On failure** (the new Redis does not start), the rollout pauses. A revert reinstalls `8.0.2-1` from
   the node cache, with the same stop → swap → start sequence.

**Risks specific to this example:**

- **Downtime per node.** Each node's Redis is down from step 4 to step 7. Redis replication or Sentinel
  failover must cover it; with `max_parallel = 1` only one node is down at a time.
- **Data format after an upgrade.** A newer Redis may write an RDB/AOF that the older version cannot read.
  A revert after the new version has written data may then fail (P6). For major versions, take a
  snapshot of the host volume before the swap. A `pre_swap` hook is a possible addition: a batch job run
  on the node before step 5.
- **Clients of Redis are not dependents.** Other jobs that connect to Redis over the network are not
  restarted. Only jobs that run binaries from the package declare the dependency.

### 7.5 Workload model: static, long-running services

The main use case is close to Kubernetes static, always-running services: databases, caches and other
long-lived systems that are updated rarely. They are bound to nodes and keep local data. This shapes the
defaults:

| Kubernetes | Norca with deb packs |
| --- | --- |
| DaemonSet / static pod | `system` job on a node pool, binaries from a deb release |
| StatefulSet (stable identity, local data) | service job pinned per node (`${node.unique.name}` constraint) + host volume |
| `OnDelete` / partitioned rolling update | rollout with `max_parallel = 1`, ordering and manual approval |
| PodDisruptionBudget | `disruption` block, honoured by the eviction API |
| Image tag of the container | package version of the deb release |

**Defaults for deb packs.** These favour safety over speed, because updates are rare and each one
matters:

- `dependents = "stop-start"`: a hard swap, since the old and the new daemon must not run together on the
  same data.
- `max_parallel = 1` and `on_failure = "pause"`. There is no automatic revert, because data formats may
  have moved on (P6).
- **Approval per wave** (`approve = "each-wave"`): every wave waits for an operator. The alternatives are
  `canary` (approve after the first wave only) and `none`.
- **Ordering.** `order_by` sorts the nodes of a rollout by a node or alloc meta value, for example
  `order_by = "meta.redis_role"` with `order = ["replica", "primary"]`. Replicas go first, the primary
  last. Promotion or failover before the primary's swap belongs to the application. It runs as a
  `pre_swap` hook, a batch job on that node.
- **`pre_swap` / `post_swap` hooks.** These are batch jobs constrained to the node being swapped, using
  the existing hook machinery (`release_hooks.go`). Typical uses: snapshot the host volume before the
  swap, failover, a schema check after the swap.
- **Maintenance window.** An optional `window { cron = "…", duration = "2h" }`: waves start only inside it.
- **New nodes do not wait for a window.** A node that joins the pool gets the current version right away.
  It has no running data yet.

**What is out of scope for this model:** autoscaling of these services, and in-place restarts. A
rarely updated database is better served by an explicit, observable, operator-paced swap than by
automation.

## 8. UI

The views fit into the existing pages; there are no Ember Data models for packs, everything goes through
`ui/app/utils/releases.js`.

- **Catalog** (`components/packages/catalog.*`): a type badge (`HCL` / `DEB`), a filter by type, and the
  platforms supported by a deb pack in the detail.
- **Install form** (`components/packages/schema-form.*`): it works unchanged for the pack's own variables.
  Deb packs get two built-in tabs, like the existing `json`/`cli` tabs:
  - **Targets:** node pool, datacenters, constraints, and a live count of matching nodes per platform plus
    `unsupported`;
  - **Rollout:** max parallel, canary, health times, `dependents`, `on_failure`.
- **Plan preview** (`plan-pack`): for deb packs it lists the nodes that would change, per version
  transition (`not installed → 7.1.1`, `7.0.3 → 7.1.1`), the dependent allocations per node, and
  conflicts.
- **Release page** (`components/packages/release.*`): a **Rollout** card next to "Regions".
  - Wave progress and the current wave's nodes with their state.
  - Pause, resume, abort and revert actions, plus canary approval.
  - A per-node table filterable by state.
  - Subscription to `CustomObject:packs.norca.io/HostPackageRollout/<name>`.
- **Node detail page:** a new **Packages** tab (`router.js:52-54`, `components/clients/tabs.js`,
  `routes/clients/client/packages.js`). It shows each managed package with its version, previous version,
  owning release (linked), state, last error and the object's event log. It also lists the dependent
  allocations on that node.
- **Nodes list:** an optional column or filter: "package X: version / missing / failed".

## 9. Backup

**All three kinds are in every backup automatically.** A backup is a full FSM snapshot
(`backup/manifest.go`). The kinds are custom objects, so they need no new snapshot type or persist code.
The problems are about meaning:

1. **Restore can downgrade nodes.** Restoring an older backup restores older desired versions. The
   rollout controller would then downgrade the fleet. Proposal:
   - after a full restore (`backup_full_restore.go`), every `HostPackageSet` starts **paused**, with
     condition `RestoredPaused`;
   - an operator resumes it after checking the plan preview;
   - the restore preview lists the host package sets whose desired version differs from the current
     state.
2. **Per-node state in a backup is stale by nature.** After a restore, the `NodeHostPackages` status is
   treated as a cache. Clients overwrite it with their actual ledger on their first report. Desired
   versions in it are recomputed from the restored sets.
3. **Binaries are not in the backup.** `.deb` files live in OCI or HTTPS sources, in node caches and in the
   server blob cache (§4.4). The source registry must be backed up separately, and the backup
   documentation must say so. A restore plan check marks sets whose sources are unreachable.
4. **Browse, compare and selective restore** (`backup_browse.go`, `backup_restore.go`) do not cover
   releases or objects today. Deb releases inherit this gap. `Summarize()` (`backup/manager.go:1105`) can
   get a `host_package_sets` count; it is cheap and useful in the manifest.

## 10. Other stored data and side effects

| Data | Where | Notes |
| --- | --- | --- |
| Desired sets, rollouts, per-node status | Raft custom objects (§5.1) | O(nodes) objects; writes only on transitions. |
| Node attributes `pkg.deb.*`, `dpkg.*`, `os.codename` | node in Raft (re-register) | Managed packages only; changes trigger node evals and computed-class changes. |
| Client ledger | client bolt `host_packages` | Not backed up; rebuilt from `dpkg-query` if lost (managed-ness is then lost too, see open question 6). |
| `.deb` cache | `<data_dir>/host-packages/cache` | GC keeps the current and the previous version per package. |
| Server blob cache (air-gap) | `<data_dir>/packages` on every server | Local, not in snapshots. |
| Events | `CustomObject` topic, object event logs | The NATS bridge forwards them like other objects. |
| Audit | object event logs (20 per object) | There is no general audit log; package changes to nodes are security-relevant (open question 7). |

**Multi-region.** Deb releases follow the existing multiregion apply. Each region has its own sets,
rollouts and node objects. Waves are per region.

## 11. Security

- **A deb pack runs maintainer scripts as root on every target node.** That is as strong as `node:write`,
  or stronger. Proposal:
  - installing or upgrading a deb release needs the release `write` capability, `node:write`, and a new
    object capability on `packs.norca.io/HostPackageSet` in namespace `*`;
  - read-only users see per-node package state with `node:read`.
- **Clients opt in:** `client { host_packages { enabled = true } }`. The default is off.
- **Signatures are mandatory for deb packs.** The pack's cosign signature is verified with the registry
  trusted keys, and every `.deb` is checked with `sha256`. Optionally, a client-side
  `allowed_signers` list makes the node verify too, so a compromised server cannot push arbitrary
  packages.
- **The client runs dpkg itself, not a task.** So `raw_exec` does not have to be enabled.
- **Secrets do not belong in debconf answers.** Debconf answers are stored in the release revision.
  Secret values go to Variables (existing `ReleaseVariableSecret` path) and must not be preseeded into
  debconf in plain text (open question 5).

## 12. Problems and risks

| # | Problem | Impact | Handling |
| --- | --- | --- | --- |
| P1 | exec chroot keeps old inodes after restart | Dependents keep running old code silently | Always replace allocations (§7.3). |
| P2 | Packages outside the default exec chroot (`/opt`) | exec tasks do not see them | Norca packages use `/opt/norca/<package>` (§4.5); the client adds `/opt/norca` to `chroot_env` when host packages are enabled. |
| P3 | Dependency closure: dpkg does not resolve; apt resolves non-reproducibly | Partial installs, node-to-node differences | Self-contained Norca packages (§4.5); simulate before install; `resolve = "apt"` only for stock packages. |
| P4 | Debian version ordering ≠ semver | Wrong constraints and wrong "newer" decisions | `debversion` operator and ordering everywhere. |
| P5 | dpkg lock held by unattended-upgrades / admins | Install hangs or fails | Lock timeout, retry; `apt-mark hold` on managed packages. |
| P6 | Maintainer scripts are not reversible (data migration, service restarts) | Revert does not restore the old state | Document; `on_failure = "pause"` by default; `drain` for such packages. |
| P7 | Interrupted dpkg run (client crash, reboot) | Half-configured packages | `dpkg --audit` and `--configure -a` on startup before reporting. |
| P8 | Backup restore downgrades the fleet | Unintended mass downgrade | Restored sets start paused (§9). |
| P9 | Binaries outside the backup | Restore without installable packages | Document; unreachable sources marked in the restore check. |
| P10 | Two releases owning the same package | Fight over versions | Ownership check on overlapping targets (§5.2). |
| P11 | Package already installed by the OS or an admin | Norca takes over, or removes, foreign packages | `adopt_existing = false` by default; never remove unmanaged packages. |
| P12 | Long rollouts vs the per-release lock | Release blocked for hours | Apply returns after writing desired state; the rollout runs in its own loop. |
| P13 | Raft load on large fleets | Write bursts during waves | Transition-only writes, one object per node, waves bound the burst. |
| P14 | Attribute churn | Node evals and computed-class changes | Managed packages only; a later option is `unique.pkg.deb.*` if class churn hurts. |
| P15 | Disruption budgets stall waves | Rollout looks stuck | Explicit `blocked by disruption budget` state in UI and CLI. |
| P16 | Platform drift: an OS upgrade moves a node from bookworm to trixie | The node now matches another platform, or none | The fingerprint changes; the controller plans the node again as a normal transition. |
| P17 | Windows and non-Debian clients | Feature missing | Not targets; client code is Linux only, the Windows build stays unaffected. |

## 13. Phases

1. **Pack type groundwork.**
   - `type` in metadata, release, catalog and API.
   - Catalog badge and filter.
   - `packages.hcl` parsing and validation.
   - Debian version ordering.
   - `norca pack deb build|lint|push` for Norca packages (§4.5).
   - Render of `host-package` resources (desired state only, no client).
2. **Client manager.**
   - `dpkg` fingerprints.
   - `client/hostpkg` with pull/report RPCs, ledger, download and verify, simulate, install, hold.
   - `NodeHostPackages` objects.
   - Node "Packages" tab.
   - One node at a time, no waves.
3. **Rollouts.**
   - Rollout controller with waves, health gate, pause, resume, abort and revert.
   - Release page Rollout card.
   - Plan preview per node.
   - CLI `norca pack rollout status|pause|resume|abort|promote`.
4. **Dependents.**
   - `norca.host_packages` meta mutator.
   - `debversion` operator.
   - `replace` / `drain` handling through the eviction API.
   - `requires { host_package }` in packs.
5. **Backup and hardening.**
   - Restore pause and preview.
   - Manifest summary.
   - Client `allowed_signers`.
6. **Later.**
   - Air-gapped blob serving (§4.4).
   - A first-class `host_package` jobspec block.
   - RPM as another type, which the type split allows.

## 14. Open questions

1. Daemons that Norca runs from package binaries (Redis, §7.4) are in scope. Are packages that bring their
   own systemd units (agents, exporters) in scope too? That changes the health gate: unit health, not
   only allocations.
2. Which distributions and releases must be supported at first (Debian 12/13, Ubuntu 22.04/24.04)? And
   which architectures?
3. Mostly custom packages built by us (§4.5). Is `nfpm` acceptable as the build library? Should
   non-conforming packages be rejected, or only flagged?
4. Should `drain` drain the whole node (simpler, safer) or only the dependents (less disruptive)?
5. Debconf only for stock packages (§4.5): is supporting stock packages worth it at all in the first
   phases?
6. If a client's ledger is lost (data dir wiped), should packages installed earlier be re-adopted
   automatically when they match a desired set?
7. Do we need a durable audit trail of package changes per node beyond the 20-entry event log, for
   example by forwarding through the NATS bridge?
8. Are host packages in scope for multi-tenant clusters? Namespace owners must not be able to change nodes
   shared with other tenants. If yes, it binds deb releases to node pools owned by the namespace.
