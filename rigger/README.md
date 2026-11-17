# Rig

Rig is the package manager of Norca. It installs modules (packages written
in typed HCL) and stacks (the modules of a region with their values) into a
Norca cluster, with versions, registries, a lock file, signatures and trust.

**Status:** phase 0 of the design. The library has its ports and its
constructor; nothing loads or evaluates a module yet. The design is in
[design/rig-design.md](design/rig-design.md); [examples/](examples/) shows
the language, which nothing parses yet.

## Layout

Two Go modules in one repository (design 12.1):

| Path | Module | Holds |
| --- | --- | --- |
| `/` | `github.com/bacanoo-io/norca-rig` | the library: package `rig` (the engine), `host` (the ports a host implements), `source` (the port of a fetcher), `resource` (evaluated resources), `oplog` (the log as a tree of operations) |
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
