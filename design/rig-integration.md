<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Rigger in Norca: Integration and Implementation Plan

**Status:** plan (2026-11-18); the layout of 3.2 and the name Rigger
decided 2026-11-21, step 4.0 started that day. Phase 7 of the design of
Rigger (`norca-rig/design/rig-design.md`, `rigger/design/rigger-design.md`
after 4.0; section 15): the server embeds the Rigger library, `norca rigger`
commands join the binary, the standalone `rigger` program stays, and the
pack manager goes once the lab is rewritten. Go paths are under `norca/`
unless they start with `rigger/`. Rigger was called Rig until 4.0; the
repository `norca-rig` and its history keep that name.

## 1. In short

Rigger (`github.com/bacanoo-io/norca-rig`) is the package manager that replaces
`norca pack`: modules in typed HCL instead of templates, stacks of modules,
versions and a lock file, tree digests and signatures, a global registry. It
is a library with a CLI as its first host. It imports nothing of Norca; a
host gives it Norca through small interfaces (`host/`), and gets plain data
back (resources, plans, diagnostics).

```go
eng, _ := rigger.New(
	rigger.WithCluster(cluster),           // version, features, region, namespaces
	rigger.WithJobDecoder(jobDecoder),     // jobspec2.DecodeJob
	rigger.WithResourceDecoder(decoder),   // object kinds and objects
	rigger.WithReleases(releases),         // the release engine
	rigger.WithSecrets(secrets),           // create-once secrets of a stack
	rigger.WithFetcher(fetcher),           // Rigger's fetcher, cache in the data dir
	rigger.WithLogger(logger),             // slog, written into hclog
)
s, diags := eng.FetchStack(ctx, "acme/data-platform", "~> 1.2")
plan, diags := eng.PlanStack(ctx, s, values)
outcomes, diags := eng.ApplyStack(ctx, plan, rigger.ApplyOptions{})
```

What Norca already has and Rigger uses as it is: the release engine
(`packs.norca.io/Release` and its revisions, plan, apply in the order of the
kinds, hooks, drift, rollback, the release lock), admission with
`trusted_keys` (a module signed by Rigger is admitted today), the secret store,
custom objects, the system bus.

## 2. Decisions (2026-11-18)

1. **`DecodeJob` lives in Norca.** `jobspec2.DecodeJob(block, ctx)` is
   exported from `jobspec2`, a module of its own (`norca/jobspec2`, 4.1); Rigger's
   host over the API imports it. The copy of jobspec2 in
   `cli/internal/jobspec2` of Rigger is deleted.
2. **Both ways stay:** standalone (the `rigger` program evaluates and sends
   resources to the release API) and embedded (the server fetches, verifies
   and evaluates). Releases made either way are the same releases.
3. **The server evaluates stacks too,** not only single modules.
4. **Rigger is available both as `norca rigger ...` in the norca binary and as the
   standalone `rigger` program.** Both are the same command package of Rigger
   (`rigger/command`) with the same hosts over the HTTP API, so a command, its
   flags and its output are the same either way. The `rigger` program stays
   installable on its own (`go install` of its module at a tag, or the
   release binaries), for machines and pipelines that have no norca binary.
5. **Norca uses Rigger's fetcher** (sources, cache, the global registry).
6. **The pack manager and Rigger run side by side** until the tests of
   norca-lab are rewritten for Rigger; then the pack manager goes.
7. This plan is kept here, in `norca/design`.

Added 2026-11-21:

8. **The layout of 3.2:** Rigger moves into the norca repository. The rule
   of Rig that norca, norca-deployer and norca-registry are not changed
   until it is ready is lifted.
9. **Rig is renamed Rigger, every name at once, as part of 4.0:** the
   directory and the modules (`rigger/`, `github.com/bacanoo-io/norca/rigger`,
   `.../rigger/cli`), the Go package (`rigger.New`), the commands
   (`norca rigger ...`, the `rigger` program), the object group
   (`rigger.norca.io`), the routes (`/v1/rigger/...`), the facts of the
   module language (`rigger.*` instead of `rig.*`), the file and directory
   names (`rigger.lock.hcl`, `*.riggertest.hcl`, `.rigger/`,
   `.rigger-signatures/`), the environment variables (`RIGGER_*`), and the
   local clones and their content (`norca-rigger-deployer` with its
   `rigger:` plan section, `norca-rigger-registry`). Nothing is released, so
   no old name is kept.

## 3. Repository and modules: who imports whom

### 3.1 The Go rules this layout follows

- **Go applies the `replace` directives of the main module only**; the ones
  of a dependency are ignored, and `go install <path>@<version>` refuses a
  module that has any. The norca program module
  (`github.com/bacanoo-io/norca/norca`) needs six (the raft fork,
  `hashicorp/nomad/api` to `./compat/upstreamapi`, `./api`, go-winio,
  go-metrics, hcl v1). So **no other module imports the norca program
  module**; what others need of Norca is in small modules without `replace`.
  (Today `norca-rig/cli` imports it for three small packages, `helper/pointer`,
  `jobspec2/addrs`, `jobspec2/hclutil`, and copies its `replace` lines with
  relative paths to a sibling checkout; that goes.)
- `replace` stays only in program modules and only for paths inside the
  repository. A workspace (`go.work`) is used only inside `rigger/` (3.2);
  CI builds every module with `GOWORK=off` as well, so nothing works only
  through the workspace.
- **One hcl/v2 for all:** Norca uses a pseudo-version of the Nomad branch
  (`v2.20.2-0.20240517235513-55d9c02d147d`; `DecodeJob` needs its
  `UndefinedVariable` and the block decoders of `gohcl.Decoder`). Minimal
  version selection takes the highest version any module asks for, so a
  module asking for upstream v2.21 or later would silently replace it.
  Every module asks for exactly this version; a CI check compares
  `go list -m github.com/hashicorp/hcl/v2` across the modules.
- The library of Rigger imports nothing of Norca (hexagonal: ports in `host/`
  and `source/`, adapters given by the host, manual constructor injection);
  it stays usable outside Norca and testable with fakes.

### 3.2 Layout: Rigger in the norca repository (decided 2026-11-21)

The work on Rigger and Norca crosses all the time (the release API, the
decoders, the hosts, the server endpoints, the UI); with two repositories
every such change is two commits, two pushes and an order of tags. Rigger moves
into the norca repository as modules of its own, joined by `replace` lines
with paths inside the repository:

```
norca repository
  norca/                       github.com/bacanoo-io/norca/norca  (program; its replace lines stay)
    api/                       .../norca/norca/api        (exists)
    controller/                .../norca/norca/controller (exists)
    jobspec2/                  .../norca/norca/jobspec2   module since 4.1: jobspec2 + DecodeJob
    helper/resourcespec/       .../helper/resourcespec    module since 4.1
    helper/externalspec/       .../helper/externalspec    module since 4.1
  rigger/                      github.com/bacanoo-io/norca/rigger      the library
    go.work                    Rigger's two modules, for work inside rigger/
    command/                   the commands; hosts given by the caller (command.Hosts)
    fetch/ oci/ bundle/ ...    moved out of cli/internal (4.2)
  rigger/cli/                  github.com/bacanoo-io/norca/rigger/cli  the `rigger` program
    norcaapi/                  the hosts over the HTTP API (imports api and spec)
    cmd/rigger/                main: rigger/command with norcaapi
```

**No `go.work` at the root of the repository** (a change from the first
draft of this plan). A workspace there would put every `go` command under
`norca/` into workspace mode, with Rigger's modules in `go list -m all`:
`make notices`, the go-licenses and forbidden-module checks, the codec
generation and the clean room audit read that list, and the lab and test
scripts mount the repository root. The modules are joined by `replace`
lines with paths inside the repository instead (`rigger/cli` replaces
`rigger`, and until 4.1 the norca program module and `norca/api`); Rigger's
own `rigger/go.work` stays for work inside `rigger/` and touches nothing
outside it.

Who imports whom (no module imports the norca program module):

```
norca/api   decoder modules     rigger (library)
     ^  ^       ^  ^               ^      ^
     |  +-------|--|---------------|---+  |
     |          |  |               |   |  |
     +---- rigger/cli (norcaapi, cmd/rigger) ---+
                   ^
                   |
     norca (program): server hosts (rigger_host.go), rigger endpoint,
                      `norca rigger` = rigger/command with rigger/cli/norcaapi
```

- `command.Run(ctx, args, stdout, stderr, hosts)` takes a `command.Hosts`
  (constructors of the cluster, releases and secrets for a region, the job
  and resource decoders). `norca rigger` and the `rigger` program pass
  `norcaapi`; a test passes fakes. So `norca rigger` and `rigger` are the same
  commands (decision 4).
- Versions: the modules inside the repository are tagged with their path as
  prefix (`norca/api/v0.x.y`, `norca/jobspec2/v0.x.y`, `rigger/v0.x.y`,
  `rigger/cli/v0.x.y`), so `go install github.com/bacanoo-io/norca/rigger/cli/cmd/rigger@rigger/cli/v0.x.y`
  works. Today `norca/api` is required as a pseudo-version of an upstream
  hash (`v0.0.0-20230103221135-ce00d683f9be`); the first tag replaces it.
  The same tag ends the relative `replace` of `norca/api` and
  `norca/controller` in norca-platform and in `operators/postgres` of
  norca-registry.
- CI: build and test every module with `GOWORK=off`; check that no module
  but the program depends on `github.com/bacanoo-io/norca/norca`
  (`go list -deps`); the hcl/v2 check of 3.1; a smoke test of
  `go install .../rigger/cli/cmd/rigger@<tag>` once tags exist.

### 3.3 Other layouts considered

| Layout | Crossing work | Go | Why not |
| --- | --- | --- | --- |
| Two repositories, done right (the decoder modules and `norca/api` tagged, a `go.work.local` per repository, not committed) | stays: two commits, an order of tags | clean | the crossing work is the cost to avoid; kept as the way back: a module can leave the repository later (`git filter-repo --subdirectory-filter rigger`) if Rigger ever ships on its own |
| git submodule (`rigger/` points at a commit of norca-rig) | not less: commit in norca-rig, then a pointer commit in norca | the module proxy does not fetch submodule content; works only in checkouts | `--recursive` clones, stale pointers, the audit and `git ls-files` don't see the files |
| git subtree (a copy of Rigger in norca, pushed back with `git subtree push`) | one repository to work in, two sources of truth | which repository is the module fetched from | the two-way sync is manual and conflicts |
| Rigger inside the norca program module (one module) | none | the `rigger` program could not be installed with `go install` (the program's `replace` lines), and the library would pull the server's dependencies | breaks decision 4 and the library rule |

## 4. Steps

Each step is a commit series of its own, with tests, and leaves both package
managers working.

### 4.0 Rigger moves into the norca repository

On a branch of a norca worktree, as a series of commits:

1. **The history comes along.** The work in norca-rig stops for the move
   (checked 2026-11-21: no other session, clean tree, 36 commits, all signed
   off, one author). In a fresh clone of norca-rig,
   `git filter-repo --to-subdirectory-filter rigger`; in norca
   `git merge --allow-unrelated-histories` of it. The merge brings the files
   as they were (old module paths, the name Rig), so the module builds only
   after step 2; the DCO check of CI skips merge commits and finds every
   imported commit signed off.
2. **Builds inside the repository.** Module paths
   `github.com/bacanoo-io/norca-rig` → `github.com/bacanoo-io/norca/rigger`,
   `.../norca-rig/cli` → `.../norca/rigger/cli`, imports rewritten; the
   `replace` lines of `rigger/cli/go.mod` point inside the repository
   (`../` for the library, `../../norca` and `../../norca/api` until 4.1,
   with the other replacements of the program module repeated until 4.1
   ends the dependency); `make check-copy` compares with `norca/` of the
   same repository.
3. **The rename** (decision 9), as one commit: Go package and identifiers,
   the program `cmd/rigger`, command names and help, the language facts
   `rigger.*`, file names (`rigger.lock.hcl`, `*.riggertest.hcl`,
   `.rigger/`, `.rigger-signatures/`, `rigger_decode.go`), environment
   variables `RIGGER_*`, the design (`design/rigger-design.md`), README,
   STATUS, examples and builtin modules. Tree digests change with the file
   names: digests in lock files and test fixtures are written again, nothing
   that is published is signed yet.
4. **The rules of the repository cover `rigger/`:** REUSE (Rigger's
   `REUSE.toml` and `LICENSES/` folded into the root ones), file headers,
   `compliance-audit.sh` (forbidden modules, headers), the CI jobs per module
   (build, vet, unit tests, go-licenses matrix with `rigger` and
   `rigger/cli`), Rigger's `check-copy` and `check-licenses` as make targets.
   `CLAUDE.md` of Rigger stays in `rigger/`, so its rule that no code is
   taken from source that is not open stays where the code is.
5. **After the merge into main and the push:** the local `main` of norca-rig
   (23 commits ahead of origin) is pushed, then the GitHub repository
   `bacanoo-io/norca-rig` is archived (the maintainer); the local clones are
   renamed `norca-rigger-deployer` and `norca-rigger-registry` (4.9).

Done when: `go build`, `go vet` and `go test ./...` of every module pass
(`rigger` and `rigger/cli` with and without `rigger/go.work`), the norca
module's tests and checks are unchanged (`compliance-audit.sh`, notices,
codec), and the `rigger` program and its integration tests against a dev
agent work as the `rig` program did.

### 4.1 The decoders in Norca

**As decided 2026-11-21 (the maintainer): nested modules in place**, not a
new `norca/spec` module. The decoders' directories get a `go.mod` of their
own, as `norca/api` has; no file moves, so the import paths, the history
and the path mapping of both audits (`clean-room-audit/run.sh` maps a Norca
file to its v1.6.5 file by path; `compliance-audit.sh` check 4 likewise)
stay as they are. Moving inherited files to `norca/spec` would have made
the similarity audit read jobspec2 as new Norca code.

- `norca/jobspec2` (`github.com/bacanoo-io/norca/norca/jobspec2`, with
  `addrs` and `hclutil`) imports only `norca/api` and hcl. It gets
  `DecodeJob(block *hcl.Block, ctx *hcl.EvalContext) (*api.Job, hcl.Diagnostics)`
  and its test, taken from Rigger's copy. It decodes one `job` block with
  an evaluation context from outside, the way `ParseWithConfig` decodes a
  file: `dynblock`, `meta`, top-level `task` and `vault`, the block decoders
  of `hcl_conversions.go`, `decodeMapInterfaceType`, `normalizeJob`. The
  program module's `helper/pointer` is replaced by a `pointerOf` of its own;
  its tests use `t.Parallel`. The tests that also parse with `jobspec` (v1)
  move into `norca/jobspec` (external test package): v1 stays where it is,
  Rigger does not need it.
- `norca/helper/resourcespec` and `norca/helper/externalspec` are modules
  too (the parsers of namespaces, quotas, variables, external services,
  object kinds and objects; no `objectspec` split is needed, the whole
  package needs nothing of the program module but `helper.CheckHCLKeys`,
  which is copied with its HashiCorp and Norca lines).
- The program module requires the three with `replace` lines to their
  directories; its importers are unchanged.

Rigger: `rigger/cli/norcaapi` imports these modules; `cli/internal/jobspec2`,
`cli/internal/resourcespec` and `make check-copy` go, and `rigger/cli` no
longer requires the norca program module (only `norca/api` and the three
decoder modules, by `replace` lines inside the repository).

Done when: Norca's tests of jobspec2 and resourcespec pass in the new
module; Rigger's module tests and `compare-packs.sh` of the registry give the
same results.

### 4.2 Rigger prepared for embedding (under `rigger/`, no change of the server)

- `command`, `fetch`, `oci`, `bundle`, `indexer`, `builtin`, `convert` move
  from `rigger/cli/internal` into the library, public; `command` gets its
  hosts from the caller (`command.Hosts`).
- **The fetcher for a server:** git through `go-git` (Apache-2.0, already a
  dependency of Norca) instead of the `git` program, so that a server needs
  no `git` binary; credentials from the host per repository (a callback:
  username and password or token, SSH key and known hosts, as
  `pack/catalog.SourceAuth`); the cache directory and the global registry
  given by the host. The `rigger` program keeps the same fetcher.
- **The install form:** `Module.FormSchema()` in the library, the
  `UIFormSchema` the UI renders today (JSON Schema with `x-ui-tabs`,
  `x-ui-widget`, `x-ui-show-if` ...; `pack/sdk/pack/variables/schema.go`),
  built from the types, validation and `ui` hints of the variables, so that
  `ui/app/components/packages/schema-form` serves modules unchanged.
- A stack sent as files (`rigger.LoadStackFiles` from an archive) for the
  server to evaluate a local stack of a client (4.6).

Done when: the `rigger` program works as today on the moved packages; the
fetcher passes its tests with go-git, with and without credentials.

### 4.3 The hosts in Norca

- `rigger/cli/norcaapi` (stays in the module of the `rigger` program, 3.2): the
  hosts over the HTTP API, for `norca rigger` and the `rigger` program; it imports
  `norca/api` and the decoder modules, never the program module. Its integration
  tests run against a dev agent as they do today.
- In the server (`norca/rigger_host.go`), the hosts in process:

  | Port | Implemented by |
  | --- | --- |
  | `Cluster` | the state store and the server's version and features (`evaluatePackCompatibility` checks the same) |
  | `JobDecoder` | `jobspec2.DecodeJob` |
  | `ResourceDecoder` | `resourcespec` (object kinds and objects) |
  | `Releases` (`ReleaseReader`, `ReleasePlanner`, `ReleaseWriter`) | the `releaseManager` directly: plan, apply, history, rollback, uninstall, drift, under the release lock |
  | `Secrets` | the secret store, create-once |
  | `Store` | `<data_dir>/rigger/` of the server |
  | `Fetcher` | Rigger's fetcher, cache in `<data_dir>/rigger/cache`, credentials from the Variable a registry names |
  | logger | an `slog` handler writing into the server's hclog |

Done when: an engine built from the server's hosts plans and applies a
module in a server test as the `rigger` program does through the API.

### 4.4 The release API

Gaps found in phase 0 (`rigger-design.md` 12.2), closed without breaking the
stored objects (the group `packs.norca.io` stays; new fields are optional):

- `ReleaseSpec` and `ReleaseRevisionSpec` get `Owner` (kind `stack`, name,
  version, source, part) in place of the marker Rigger writes into
  `Description` today; `List` filters by it (prune, `stack status`).
- `ReleasePack` gets `Manager` (`pack` or `rigger`), `Commit`, and for Rigger
  `Source` as the module address and `Digest` as the tree digest (it is
  the OCI digest for packs).
- The digest Rigger compares for "unchanged" (module digest, values, settings)
  gets a field of its own instead of `ValuesHash`.
- `ServerRendered` is set for a release the server evaluated from a module
  it fetched and verified itself; then the signature covers what was
  applied (`rigger-design.md` 8.2).
- **The owner is checked, not only written** (review finding JOG-23, dropped
  for the pack 2026-11-20): today the job meta `pack.release` /
  `pack.release_namespace` can be set by anyone who registers or dispatches
  a job, and the release operations take such a job as their own. `Owner`
  must be set only by the release engine, and the release operations must
  find their jobs by it.
- **Written against the release engine as it is since 2026-11-21**, not as
  phase 0 saw it: the claim lock between releases and tenants (ATM-4), the
  fencing of release writes on a deposed leader and `ReleaseOperation.ID`
  (ATM-12, 16, 17, 18), the check of job-bound policies on release jobs
  (JOG-2, JOG-14) and `auto_revert_bound_policies` (JOG-17). The `Releases`
  host of 4.3 calls the engine through these paths.

Done when: releases of the pack manager read and roll back as before; Rigger
writes the new fields in both ways.

### 4.5 Registries on the server

A new object group `rigger.norca.io` (next to `packs.norca.io` while both run):

- `rigger.norca.io/Registry`: `type` (`git` registry repository, `oci`,
  `global`), `source`, trusted public keys, `authSecret` (a Variable, as for
  pack registries), sync interval. The global registry is there by default
  (`github.com/bacanoo-io/norca-global-registry`, or a mirror for a closed
  network). Kept by the leader: the sync loop of the pack registries
  (`packRegistrySyncLoop`) gets a Rigger twin that refreshes the mirrors and the
  index; a push notice on the bus (`norca.rigger.pushed`) syncs at once.
- ACL as for pack registries: object capabilities on the kind refs
  (`rigger.norca.io/Registry` read and write in the cluster namespace).
- Bundles imported into the cache of the leader for a cluster without a
  network (`rigger bundle import` through the API), in place of the `uploads`
  registries of packs.

### 4.6 Evaluation on the server

An RPC endpoint `Rigger` (`norca/rigger_endpoint.go`) and its HTTP routes
(`/v1/rigger/...`), forwarded to the leader as the pack registry calls are:

| Call | What it does |
| --- | --- |
| `Catalog` | modules and stacks of the registries: versions, description, category, signature, requirements and whether the cluster meets them, icon digest |
| `Detail` | one version: README, variables, `FormSchema`, outputs, the modules a stack pins |
| `Icon` | the icon of a version by its digest, from the verified cache |
| `Install` | one module as one release: fetch, verify, evaluate, plan or apply; `ServerRendered` |
| `StackPlan`, `StackApply`, `StackStatus`, `StackDestroy` | a published stack by source and version, or a local stack sent as files; values; `prune` |

- **A stack apply is long** (it waits until each release is ready), so it
  runs on the leader as an operation: an object `rigger.norca.io/Operation`
  with the plan, its state and the log of Rigger as a tree (`oplog`); the
  client follows it with blocking queries or the event stream and can
  cancel it. A plan answers at once.
- The leader evaluates with the install facts of its own state. The option
  of evaluating on an observer (`rigger-design.md` 12.7) stays open.
- **Same results both ways:** evaluation is a pure function, so a stack
  applied by the `rigger` program and then by the server is "unchanged" when
  both run the same library version; the generated values are in the same
  place of the secret store (`rigger/stacks/<stack>/generated`).

### 4.7 `norca rigger`

`command/rigger.go` registers the commands of `rigger/command` under `norca rigger`
with `rigger/cli/norcaapi`, beside `norca pack`. A flag (or the configuration)
chooses the way: evaluate locally and send resources (standalone), or send
source, version and values and let the server evaluate (embedded).

The `rigger` program (`rigger/cli/cmd/rigger`) is the same command package with the
same hosts: `norca rigger stack apply ...` and `rigger stack apply ...` are one
command. The `rigger` program is built and released as a binary of its own
next to norca, and installable with `go install` at its tag (3.2); its
version is the version of the library it carries, and `rigger version` and
`norca rigger version` say it.

`norca operator init` gets a module template next to its pack template
(`command/operatorinit/templates/`).

### 4.8 Web UI

Beside the pages of packs (`ui/app/routes/packages`, `components/packages/*`):

- a catalog of modules and stacks from `Catalog`, with icons from `Icon`;
- the install form from `Detail.FormSchema` through the existing
  `schema-form`, posting to `Install`;
- stacks: the releases of an owner together, plan and apply as an
  operation with its log, status and drift;
- the release page shows the module, its source and digest, the owner stack
  and part.

### 4.9 Deployer and registries

Both stay repositories of their own: neither imports Rigger or Norca in Go;
they depend on the command line of Rigger and on the module language.

The local clones are renamed (`norca-rig-deployer` → `norca-rigger-deployer`,
`norca-rig-registry` → `norca-rigger-registry`) and their branches `rig` →
`rigger`; the rename of their content (decision 9) goes with their rebase.

- `norca-deployer`: the branch of the local clone `norca-rigger-deployer`
  merges (1 commit, `c9c668a`, cut from deployer main `93ae92b`: the
  `rig:` section beside the packs, which becomes `rigger:`). Deployer main
  moved 7 commits since (on 2026-11-21: the namespaces step and its
  check-and-set, the registry pin v0.5.0, the region in machine names, the
  replication token of the authoritative region): rebase first. Its
  `internal/engine/rigger.go` runs the `rigger` program
  (`rigger stack apply <dir> -f ... -var ... -prune`); it gets the choice of
  `norca rigger` on the first server (no separate binary on the host),
  embedded by default. The clone's `origin` points at a path of a cloud
  session and is set to the local norca-deployer first.
- `norca-registry`: the branch of the local clone `norca-rigger-registry`
  merges (17 commits: `modules/` beside `packs/`, the scripts
  `compare-packs.sh`, `check-modules*.sh`); it was cut from `cea420a` and is
  rebased onto the current main first. Registry main moved 4 commits since,
  and the modules follow each of them as their packs did: `deletion_policy`
  and the column `Deletion` of the kind of norca-dns (`53e57f0`); the
  operator packs' jobs in the namespace `norca-system` by default
  (`27ffaff`); their jobs off nodes with the raw_exec driver (`870208a`);
  the proxy jobs of loadbalancer-operator on exec (`c780727`). Also the
  modules of norca-dns and norca-acme: `-leader-election` (norca-acme also
  `-lease-namespace`) and the ACL path `controller-lease/<controller>`
  (CON-6e); for several norca-acme instances a `/readyz` check with
  `on_update = "ignore"`, `-http01-settle` as a variable, and Traefik
  clients with `default_healthy_only` (CON-6f, CON-6g). Rigger's own order
  of resource kinds follows Norca's (quota before namespace). Its versions
  are signed with the key of the namespace `norca` and listed in the
  global registry. `operators/postgres` loses its relative `replace` once
  `norca/api` and `norca/controller` are tagged.

### 4.10 The lab, then the pack manager goes

The tests of norca-lab are rewritten for Rigger (stacks instead of packs,
`norca rigger` instead of `norca pack`). Until then both run; afterwards:

- removed: `pack/` (the CLI, catalog, SDK), `norca/pack_*.go` (registries,
  the built-in catalog, uploads, the codec, the endpoint),
  `command/agent/pack_endpoint.go` and the `/v1/pack*` routes, the pack
  commands of `command/commands.go`, the pack pages and registry tab of the
  UI, `packs/` of norca-registry, the pack sections of the deployer;
- kept: the release engine and its stored releases (group `packs.norca.io`;
  renaming it would be a migration of stored data for no gain); a revision
  of a pack still reads, rolls back and uninstalls;
- the design documents 07, 07b get a note that Rigger replaced them; 07c (deb
  host packages, parked) continues, if at all, as a resource kind of Rigger.

## 5. Both package managers at once

Until 4.10:

- **One release has one manager.** Rigger refuses to upgrade a release whose
  newest revision a pack made, unless asked to take it over (`-adopt`); the
  pack manager refuses a release Rigger made. The `Manager` field of 4.4 tells.
- Separate objects (`packs.norca.io/Registry` and `rigger.norca.io/Registry`),
  routes (`/v1/pack*` and `/v1/rigger/*`), commands (`norca pack`, `norca rigger`)
  and pages; one release engine and one admission under both.
- A pack and its module (the 16 converted packs) render the same resources
  (`compare-packs.sh`), so a release can move from the pack to the module by
  adoption, with no change of its resources.

## 6. Order and size

| Step | Where | Size | Depends on |
| --- | --- | --- | --- |
| 4.0 Rigger into the norca repository | norca, norca-rig | small | |
| 4.1 decoders as modules (`norca/jobspec2`, `helper/resourcespec`, `helper/externalspec`) | norca | small | 4.0 |
| 4.2 Rigger for embedding | rigger | medium | 4.1 |
| 4.3 hosts | norca | medium | 4.2 |
| 4.4 release API | norca, rigger | small | |
| 4.5 registries | norca | medium | 4.3 |
| 4.6 evaluation on the server | norca | large | 4.3, 4.4, 4.5 |
| 4.7 `norca rigger` | norca | small | 4.3 |
| 4.8 UI | norca | large | 4.6 |
| 4.9 deployer, registry | repositories | small | 4.7 |
| 4.10 lab, removal | norca-lab, norca | large | all |

4.0 comes first; then 4.1 and 4.4 can start at once; 4.7 gives `norca rigger`
in the standalone way before the server evaluates anything.

## 7. Working in parallel

Several agents (or people) work on the steps at once. The rules, most of
them learnt from the review fix rounds of 2026-11-17 and 18:

1. **One worktree and one branch per agent**, never the main checkout.
   Branch names carry the agent or the step (`rigger/4.2-fetcher`,
   `norca/4.4-release-owner`). Each worktree has its own checkout of the
   committed `go.work`, so the workspace does not clash.
2. **No `git stash`.** `refs/stash` is shared by all worktrees of a
   repository (two workers once got each other's stash); save a diff to a
   file and `git apply` it instead.
3. **Shared by all worktrees:** the hooks (the DCO `commit-msg` hook), the
   refs (`refs/audit/*`, branch names), the Go module and build caches.
   Nobody installs hooks, rewrites refs or cleans the caches.
4. **Split by module.** One agent works under `rigger/`, another under
   `norca/`; they meet only where a change crosses a module border
   (the decoder modules, the release API fields of 4.4, `command.Hosts`). A change
   that crosses goes on one branch, or the two branches are merged in a
   set order (the module that is imported first).
5. **Files only the lead writes:** the generated ones (`structs.generated.go`
   through `generate.sh`, `THIRD_PARTY_NOTICES.md` through `make notices`)
   and the shared records (`CLEAN_ROOM.md` and its copy in norca-lab, the
   `go.work`, the version tags). Agents name in their report what needs
   them (a changed struct field, a new dependency, a fix in a file from
   v1.6.5); the lead runs them once, at the merge.
6. **Small, local edits in shared files** (`go.mod`/`go.sum` of a module,
   `object_endpoint.go`, `operators.go`, `acl/acl.go`, `api/*.go`): only
   what the step needs, no drive-by renames or reformatting.
7. **One budget for tests:** at most 24 vCPU for test containers and lab
   VMs together; every agent runs its containers with a CPU cap
   (`DT_CPUS=2`) and one container at a time. **The lab runs one case at a
   time** (the VMs are shared), and only the lead starts it, with a norca
   binary built from the branch under test.
8. **One hand merges and pushes** (the lead): fast-forward or picked
   commits, then `compliance-audit.sh`, the codec and notices checks, the
   tests of every module with `GOWORK=off`, and the lab where it applies.
9. **A session already working in a repository is told before a move or a
   rebase** (4.0 in particular: the work in norca-rig stops while its
   history moves, and continues afterwards in a worktree of the norca
   repository).

## 8. Open, to settle while building

- The lab case of Rigger's phase 5 (a stack on the lab) is not written; it
  is written for `rigger` after 4.0.
- The operation object of a stack apply (4.6): its fields, how long it is
  kept, how a client reattaches.
- Whether a stack apply holds one lock for the stack besides the per-release
  locks.
- How the version of the library is pinned between servers of a region, for
  the "unchanged" digests during a rolling upgrade (a mismatch shows an
  upgrade that changes nothing; it is not wrong).
- Where the default of the way (standalone or embedded) of `norca rigger` is
  configured.
