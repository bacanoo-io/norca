<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Rig in Norca: Integration and Implementation Plan

**Status:** plan (2026-11-18), not built. Phase 7 of the design of Rig
(`norca-rig/design/rig-design.md`, section 15): the server embeds the Rig
library, `norca rig` commands join the binary, and the pack manager goes once
the lab is rewritten. All Go paths are under `norca/` unless stated otherwise.

## 1. In short

Rig (`github.com/bacanoo-io/norca-rig`) is the package manager that replaces
`norca pack`: modules in typed HCL instead of templates, stacks of modules,
versions and a lock file, tree digests and signatures, a global registry. It
is a library with a CLI as its first host. It imports nothing of Norca; a
host gives it Norca through small interfaces (`host/`), and gets plain data
back (resources, plans, diagnostics).

```go
eng, _ := rig.New(
	rig.WithCluster(cluster),           // version, features, region, namespaces
	rig.WithJobDecoder(jobDecoder),     // jobspec2.DecodeJob
	rig.WithResourceDecoder(decoder),   // object kinds and objects
	rig.WithReleases(releases),         // the release engine
	rig.WithSecrets(secrets),           // create-once secrets of a stack
	rig.WithFetcher(fetcher),           // Rig's fetcher, cache in the data dir
	rig.WithLogger(logger),             // slog, written into hclog
)
s, diags := eng.FetchStack(ctx, "acme/data-platform", "~> 1.2")
plan, diags := eng.PlanStack(ctx, s, values)
outcomes, diags := eng.ApplyStack(ctx, plan, rig.ApplyOptions{})
```

What Norca already has and Rig uses as it is: the release engine
(`packs.norca.io/Release` and its revisions, plan, apply in the order of the
kinds, hooks, drift, rollback, the release lock), admission with
`trusted_keys` (a module signed by Rig is admitted today), the secret store,
custom objects, the system bus.

## 2. Decisions (2026-11-18)

1. **`DecodeJob` lives in Norca.** `jobspec2.DecodeJob(block, ctx)` is
   exported from `norca/jobspec2`; Rig imports it. The copy of jobspec2 in
   `norca-rig/cli/internal/jobspec2` is deleted.
2. **Both ways stay:** standalone (the `rig` program evaluates and sends
   resources to the release API) and embedded (the server fetches, verifies
   and evaluates). Releases made either way are the same releases.
3. **The server evaluates stacks too,** not only single modules.
4. **`norca rig ...` commands in the norca binary, and the `rig` program in
   norca-rig as well.** Both are the same command package of Rig.
5. **Norca uses Rig's fetcher** (sources, cache, the global registry).
6. **The pack manager and Rig run side by side** until the tests of
   norca-lab are rewritten for Rig; then the pack manager goes.
7. This plan is kept here, in `norca/design`.

## 3. Layering: who imports whom

Decision 4 puts the commands of Rig into `norca`, and Rig's CLI imports
Norca's API client: imported as they are today, the two Go modules would
need each other. They don't, if the parts that need Norca move to Norca:

```
github.com/bacanoo-io/norca-rig            (library; imports nothing of Norca)
  rig, host, source, sign, version, index, module, resource, runref, oplog
  command   the CLI, its hosts given by the caller (was cli/internal/command)
  fetch     sources, cache, global registry   (was cli/internal/fetch)
  oci, bundle, indexer, builtin, convert       (were cli/internal/...)
        ^                                ^
        |                                |
github.com/bacanoo-io/norca/norca        |
  jobspec2.DecodeJob                     |
  helper/resourcespec/objectspec         |
  rigapi      the hosts over the HTTP API (was norca-rig/cli/norcaapi)
  norca       the hosts in the server (rig_*.go), registries, RPC
  command     `norca rig` = rig/command with rigapi
        ^
        |
github.com/bacanoo-io/norca-rig/cli      (thin: the `rig` program)
  cmd/rig     rig/command with norca's rigapi; integration tests
```

- `command.Run(ctx, args, stdout, stderr, hosts)` takes a `command.Hosts`
  (constructors of the cluster, releases and secrets for a region, the job
  and resource decoders). `norca rig` and the `rig` program pass `rigapi`;
  a test passes fakes.
- The library keeps Norca's hcl version (`v2.20.2-0.20240517235513-55d9c02d147d`,
  the Nomad branch): `DecodeJob` needs its `UndefinedVariable` and the
  block decoders of `gohcl.Decoder`, which upstream hcl has not.
- Until the first tagged versions: `replace` and a `go.work` over the
  checkouts, as `norca-rig/cli` does today. The library is tagged (`v0.x`)
  before Norca requires it.

## 4. Steps

Each step is a commit series of its own, with tests, and leaves both package
managers working.

### 4.1 The decoders in Norca

Norca:
- `jobspec2/rig_decode.go`: `DecodeJob(block *hcl.Block, ctx *hcl.EvalContext) (*api.Job, hcl.Diagnostics)`
  and its test, taken from `norca-rig/cli/internal/jobspec2` (42 lines; a
  new file, no existing code changes). It decodes one `job` block with an
  evaluation context from outside, the way `ParseWithConfig` decodes a file:
  `dynblock`, `meta`, top-level `task` and `vault`, the block decoders of
  `hcl_conversions.go`, `decodeMapInterfaceType`, `normalizeJob`.
- `helper/resourcespec/objectspec`: the parsers of object kinds and objects
  (`object.go`) in a package that imports no server package, so that Rig
  and the pack manager use the same parser. `resourcespec` calls it.

Rig: `cli/norcaapi` imports both; `cli/internal/jobspec2`,
`cli/internal/resourcespec` and `make check-copy` go.

Done when: Norca's tests of jobspec2 and resourcespec pass with the new
files; Rig's module tests and `compare-packs.sh` of norca-rig-registry give
the same results.

### 4.2 Rig prepared for embedding (in norca-rig, no change in Norca)

- `command`, `fetch`, `oci`, `bundle`, `indexer`, `builtin`, `convert` move
  from `cli/internal` into the library, public; `command` gets its hosts
  from the caller (`command.Hosts`).
- **The fetcher for a server:** git through `go-git` (Apache-2.0, already a
  dependency of Norca) instead of the `git` program, so that a server needs
  no `git` binary; credentials from the host per repository (a callback:
  username and password or token, SSH key and known hosts, as
  `pack/catalog.SourceAuth`); the cache directory and the global registry
  given by the host. The `rig` program keeps the same fetcher.
- **The install form:** `Module.FormSchema()` in the library, the
  `UIFormSchema` the UI renders today (JSON Schema with `x-ui-tabs`,
  `x-ui-widget`, `x-ui-show-if` ...; `pack/sdk/pack/variables/schema.go`),
  built from the types, validation and `ui` hints of the variables, so that
  `ui/app/components/packages/schema-form` serves modules unchanged.
- A stack sent as files (`rig.LoadStackFiles` from an archive) for the
  server to evaluate a local stack of a client (4.6).

Done when: the `rig` program works as today on the moved packages; the
fetcher passes its tests with go-git, with and without credentials.

### 4.3 The hosts in Norca

- `rigapi` (`norca/rigapi`, moved from `norca-rig/cli/norcaapi`): the hosts
  over the HTTP API, for `norca rig` and the `rig` program; its integration
  tests run against a dev agent as they do in norca-rig.
- In the server (`norca/rig_host.go`), the hosts in process:

  | Port | Implemented by |
  | --- | --- |
  | `Cluster` | the state store and the server's version and features (`evaluatePackCompatibility` checks the same) |
  | `JobDecoder` | `jobspec2.DecodeJob` |
  | `ResourceDecoder` | `objectspec` |
  | `Releases` (`ReleaseReader`, `ReleasePlanner`, `ReleaseWriter`) | the `releaseManager` directly: plan, apply, history, rollback, uninstall, drift, under the release lock |
  | `Secrets` | the secret store, create-once |
  | `Store` | `<data_dir>/rig/` of the server |
  | `Fetcher` | Rig's fetcher, cache in `<data_dir>/rig/cache`, credentials from the Variable a registry names |
  | logger | an `slog` handler writing into the server's hclog |

Done when: an engine built from the server's hosts plans and applies a
module in a server test as the `rig` program does through the API.

### 4.4 The release API

Gaps found in phase 0 (`rig-design.md` 12.2), closed without breaking the
stored objects (the group `packs.norca.io` stays; new fields are optional):

- `ReleaseSpec` and `ReleaseRevisionSpec` get `Owner` (kind `stack`, name,
  version, source, part) in place of the marker Rig writes into
  `Description` today; `List` filters by it (prune, `stack status`).
- `ReleasePack` gets `Manager` (`pack` or `rig`), `Commit`, and for Rig
  `Source` as the module address and `Digest` as the tree digest (it is
  the OCI digest for packs).
- The digest Rig compares for "unchanged" (module digest, values, settings)
  gets a field of its own instead of `ValuesHash`.
- `ServerRendered` is set for a release the server evaluated from a module
  it fetched and verified itself; then the signature covers what was
  applied (`rig-design.md` 8.2).

Done when: releases of the pack manager read and roll back as before; Rig
writes the new fields in both ways.

### 4.5 Registries on the server

A new object group `rig.norca.io` (next to `packs.norca.io` while both run):

- `rig.norca.io/Registry`: `type` (`git` registry repository, `oci`,
  `global`), `source`, trusted public keys, `authSecret` (a Variable, as for
  pack registries), sync interval. The global registry is there by default
  (`github.com/bacanoo-io/norca-global-registry`, or a mirror for a closed
  network). Kept by the leader: the sync loop of the pack registries
  (`packRegistrySyncLoop`) gets a Rig twin that refreshes the mirrors and the
  index; a push notice on the bus (`norca.rig.pushed`) syncs at once.
- ACL as for pack registries: object capabilities on the kind refs
  (`rig.norca.io/Registry` read and write in the cluster namespace).
- Bundles imported into the cache of the leader for a cluster without a
  network (`rig bundle import` through the API), in place of the `uploads`
  registries of packs.

### 4.6 Evaluation on the server

An RPC endpoint `Rig` (`norca/rig_endpoint.go`) and its HTTP routes
(`/v1/rig/...`), forwarded to the leader as the pack registry calls are:

| Call | What it does |
| --- | --- |
| `Catalog` | modules and stacks of the registries: versions, description, category, signature, requirements and whether the cluster meets them, icon digest |
| `Detail` | one version: README, variables, `FormSchema`, outputs, the modules a stack pins |
| `Icon` | the icon of a version by its digest, from the verified cache |
| `Install` | one module as one release: fetch, verify, evaluate, plan or apply; `ServerRendered` |
| `StackPlan`, `StackApply`, `StackStatus`, `StackDestroy` | a published stack by source and version, or a local stack sent as files; values; `prune` |

- **A stack apply is long** (it waits until each release is ready), so it
  runs on the leader as an operation: an object `rig.norca.io/Operation`
  with the plan, its state and the log of Rig as a tree (`oplog`); the
  client follows it with blocking queries or the event stream and can
  cancel it. A plan answers at once.
- The leader evaluates with the install facts of its own state. The option
  of evaluating on an observer (`rig-design.md` 12.7) stays open.
- **Same results both ways:** evaluation is a pure function, so a stack
  applied by the `rig` program and then by the server is "unchanged" when
  both run the same library version; the generated values are in the same
  place of the secret store (`rig/stacks/<stack>/generated`).

### 4.7 `norca rig`

`command/rig.go` registers the commands of `rig/command` under `norca rig`
with `rigapi`, beside `norca pack`. A flag (or the configuration) chooses
the way: evaluate locally and send resources (standalone), or send source,
version and values and let the server evaluate (embedded). The `rig`
program of norca-rig is the same command package.

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

- `norca-deployer`: the branch `rig` of `norca-rig-deployer` merges (the
  `rig:` section beside the packs); it runs `norca rig stack apply` on the
  first server, embedded by default.
- `norca-registry`: the branch `rig` of `norca-rig-registry` merges
  (`modules/` beside `packs/`); its versions are signed with the key of the
  namespace `norca` and listed in the global registry.

### 4.10 The lab, then the pack manager goes

The tests of norca-lab are rewritten for Rig (stacks instead of packs,
`norca rig` instead of `norca pack`). Until then both run; afterwards:

- removed: `pack/` (the CLI, catalog, SDK), `norca/pack_*.go` (registries,
  the built-in catalog, uploads, the codec, the endpoint),
  `command/agent/pack_endpoint.go` and the `/v1/pack*` routes, the pack
  commands of `command/commands.go`, the pack pages and registry tab of the
  UI, `packs/` of norca-registry, the pack sections of the deployer;
- kept: the release engine and its stored releases (group `packs.norca.io`;
  renaming it would be a migration of stored data for no gain); a revision
  of a pack still reads, rolls back and uninstalls;
- the design documents 07, 07b get a note that Rig replaced them; 07c (deb
  host packages, parked) continues, if at all, as a resource kind of Rig.

## 5. Both package managers at once

Until 4.10:

- **One release has one manager.** Rig refuses to upgrade a release whose
  newest revision a pack made, unless asked to take it over (`-adopt`); the
  pack manager refuses a release Rig made. The `Manager` field of 4.4 tells.
- Separate objects (`packs.norca.io/Registry` and `rig.norca.io/Registry`),
  routes (`/v1/pack*` and `/v1/rig/*`), commands (`norca pack`, `norca rig`)
  and pages; one release engine and one admission under both.
- A pack and its module (the 16 converted packs) render the same resources
  (`compare-packs.sh`), so a release can move from the pack to the module by
  adoption, with no change of its resources.

## 6. Order and size

| Step | Where | Size | Depends on |
| --- | --- | --- | --- |
| 4.1 decoders | norca, rig | small | |
| 4.2 Rig for embedding | rig | medium | 4.1 |
| 4.3 hosts | norca | medium | 4.2 |
| 4.4 release API | norca, rig | small | |
| 4.5 registries | norca | medium | 4.3 |
| 4.6 evaluation on the server | norca | large | 4.3, 4.4, 4.5 |
| 4.7 `norca rig` | norca | small | 4.3 |
| 4.8 UI | norca | large | 4.6 |
| 4.9 deployer, registry | repositories | small | 4.7 |
| 4.10 lab, removal | norca-lab, norca | large | all |

4.1 and 4.4 can start at once; 4.7 gives `norca rig` in the standalone way
before the server evaluates anything.

## 7. Open, to settle while building

- The operation object of a stack apply (4.6): its fields, how long it is
  kept, how a client reattaches.
- Whether a stack apply holds one lock for the stack besides the per-release
  locks.
- How the version of the library is pinned between servers of a region, for
  the "unchanged" digests during a rolling upgrade (a mismatch shows an
  upgrade that changes nothing; it is not wrong).
- Where the default of the way (standalone or embedded) of `norca rig` is
  configured.
