# Rig

Rig is the package manager of Norca. It installs modules (packages written
in typed HCL) and stacks (the modules of a region with their values) into a
Norca cluster, with versions, registries, a lock file, signatures and trust.

**Status:** phases 1 to 4 and 6 of the design, and phase 5 but for its lab
case: modules and stacks are written, tested, versioned, signed, published
to the global registry and installed into a cluster. What works and what is
checked: [STATUS.md](STATUS.md). The design is in
[design/rig-design.md](design/rig-design.md); [examples/](examples/) shows
the language.

## Use

```shell
rig module validate examples/registry/modules/redis
rig module render examples/registry/modules/redis \
    -f examples/registry/modules/redis/examples/auth.vars.hcl \
    -var count=2 -release cache -namespace apps -region eu
```

`render` prints the evaluated resources and outputs as JSON; a job is
decoded by Norca's own job decoder. Secret items are named, never shown.

A stack names its modules by source; `norca/nats` is the module `nats` of
the namespace `norca` of the global registry
([norca-global-registry](https://github.com/bacanoo-io/norca-global-registry)):

```shell
rig stack sync my-stack          # resolve, fetch, verify, write rig.lock.hcl
rig stack plan my-stack -f values/dev.vars.hcl
rig stack apply my-stack -f values/dev.vars.hcl
rig stack plan acme/data-platform -version "~> 1.2" -f my.vars.hcl
```

Publishing signs a module or stack and tags it in its repository; the
global registry lists the tag once it passes its checks:

```shell
rig key generate -out acme
rig module publish modules/redis -key acme.key
git push origin redis/v1.0.0
```

## Layout

Two Go modules in one repository (design 12.1):

| Path | Module | Holds |
| --- | --- | --- |
| `/` | `github.com/bacanoo-io/norca-rig` | the library: package `rig` (the engine), `host` (the ports a host implements), `source` (the port of a fetcher), `module` (the metadata of a module as plain data), `resource` (evaluated resources and hooks), `runref` (runtime references a host resolves), `oplog` (the log as a tree of operations), `sign` (keys, signatures), `version` (semver and constraints), `index` (the format of the global registry); `internal/lang` loads modules, `internal/eval` evaluates them |
| `cli/` | `github.com/bacanoo-io/norca-rig/cli` | the `rig` program, the first host of the library; `norcaapi` (the ports over Norca); until phase 7 a copy of norca's job decoder (`internal/jobspec2`, see its `SOURCE.md`) |

The library imports nothing of Norca. A host builds the engine from its
adapters:

```go
eng, err := rig.New(
	rig.WithCluster(cluster),
	rig.WithJobDecoder(decoder),
	rig.WithReleases(releases),
	rig.WithLogger(logger),
)
```

## Development

Go 1.27 (the version of Norca). From the root of the repository:

```shell
make vet test     # both modules
make race lint    # the race detector, golangci-lint v2
make build        # bin/rig
```

`go.work` joins the two modules for development. The CLI module builds
against the norca checkout next to this repository (`../norca`);
`make check-copy` checks that the copy of norca's job decoder still matches
it.

## License

[MPL-2.0](LICENSE). Every file carries an SPDX header or falls under
[REUSE.toml](REUSE.toml).
