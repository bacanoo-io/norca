# Rigger

Rigger is the package manager of Norca. It installs modules (packages written
in typed HCL) and stacks (the modules of a region with their values) into a
Norca cluster, with versions, registries, a lock file, signatures and trust.

**Status:** phases 1 to 4 and 6 of the design, and phase 5 but for its lab
case: modules and stacks are written, tested, versioned, signed, published
to the global registry and installed into a cluster. What works and what is
checked: [STATUS.md](STATUS.md). The design is in
[design/rigger-design.md](design/rigger-design.md); [examples/](examples/) shows
the language.

## Use

```shell
rigger module validate examples/registry/modules/redis
rigger module render examples/registry/modules/redis \
    -f examples/registry/modules/redis/examples/auth.vars.hcl \
    -var memory=512 -release cache -namespace apps -region eu
```

`render` prints the evaluated resources and outputs as JSON; a job is
decoded by Norca's own job decoder. Secret items are named, never shown.

A stack names its modules by source; `norca/nats` is the module `nats` of
the namespace `norca` of the global registry
([norca-global-registry](https://github.com/bacanoo-io/norca-global-registry)):

```shell
rigger stack sync my-stack          # resolve, fetch, verify, write rigger.lock.hcl
rigger stack plan my-stack -f values/dev.vars.hcl
rigger stack apply my-stack -f values/dev.vars.hcl
rigger stack plan acme/data-platform -version "~> 1.2" -f my.vars.hcl
```

What a registry offers is listed and shown without a cluster: the program
fetches the registry into its cache, and a module or stack as an install
would fetch and verify it (`-json` for scripts):

```shell
rigger search                                   # the global registry
rigger search nats -kind module -limit 10
rigger search -registry builtin                 # the modules built into the program
rigger search -registry github.com/acme/rigger-modules
rigger show norca/redis                         # versions, signers, variables, outputs
rigger show norca/nats -version "~> 1.2" -readme
rigger show github.com/acme/rigger-modules//modules/web
```

One release of a module, without a stack, comes from a directory or by its
source: the newest version that meets `-version`, verified as a stack's
modules are (a version of the global registry only listed and signed, its
advisories shown as warnings), a built-in module of the version the
program ships:

```shell
rigger install ./modules/web -name shop -f values.vars.hcl
rigger install norca/nats -version "~> 1.2" -name nats -namespace apps
rigger install builtin/redis -name cache -var memory=512
rigger install norca/nats -name nats -server              # the servers fetch from the global registry
rigger install web -registry acme -name shop -server      # a registry of the cluster, by the module's name
rigger install builtin/redis -name cache -server          # built into the servers
```

With `-server` the servers fetch only from what they have: a registry of
the cluster they synced (`rigger registry sync <name>`; the global one is
`global`) and their built-in modules. A git or `oci://` address is
refused there; register its repository as a registry, or install without
`-server`. A directory is sent as files.

Git repositories are read with go-git, not with the git program, so the
credential helpers and `~/.ssh/config` of git do not apply. An `ssh://`
or `user@host:path` repository authenticates with the keys of the ssh
agent and the known hosts of the user (`SSH_KNOWN_HOSTS`, else `~/.ssh/known_hosts`); an https
repository with the credentials of its host, else anonymously. A
repository on the local file system (`file://` or an absolute path) is
read where it is.

**A credential goes to its host only, over https** (http on localhost),
for git and OCI sources alike: a stack, a lock file or an entry of an
index names hosts others chose. Two places hold them:

```shell
echo "$TOKEN" | rigger login git.example.com -username robot   # kept by host
rigger logout git.example.com
export RIGGER_GIT_HOST=git.example.com RIGGER_GIT_USERNAME=robot RIGGER_GIT_PASSWORD="$TOKEN"
export RIGGER_OCI_HOST=registry.example.com RIGGER_OCI_USERNAME=robot RIGGER_OCI_PASSWORD="$TOKEN"
```

`rigger login` keeps them in `rigger/credentials.json` of the
configuration directory of the user (`RIGGER_CREDENTIALS_FILE` names
another), readable by the user only, and they come first. A password in
the environment without its `_HOST` is an error.

`rigger stack apply -adopt`, `rigger install -adopt` and
`rigger release rollback -adopt` take over a release the pack manager
applied or another stack owns (a stack is its namespace and name); without
the flag it is refused.

Publishing signs a module or stack and tags it in its repository; the
global registry lists the tag once it passes its checks:

```shell
rigger key generate -out acme
rigger module publish modules/redis -key acme.key
git push origin redis/v1.0.0
```

## Layout

Two Go modules in one repository (design 12.1):

| Path | Module | Holds |
| --- | --- | --- |
| `/` | `github.com/bacanoo-io/norca/rigger` | the library: package `rigger` (the engine), `host` (the ports a host implements), `source` (the port of a fetcher), `module` (the metadata of a module as plain data), `resource` (evaluated resources and hooks), `runref` (runtime references a host resolves), `oplog` (the log as a tree of operations), `sign` (keys, signatures), `version` (semver and constraints), `index` (the format of the global registry), `fetch` (the fetcher of modules: git, OCI, directories, the global registry, built-in modules), `oci` (OCI registries), `bundle` (bundles for a cluster without a network), `builtin` (the built-in modules); `internal/lang` loads modules, `internal/eval` evaluates them |
| `cli/` | `github.com/bacanoo-io/norca/rigger/cli` | the `rigger` program, the first host of the library; `command` (the commands, shared with `norca rigger`), `norcaapi` (the ports over Norca, with norca's decoders `norca/jobspec2` and `norca/helper/resourcespec`) |

The library imports nothing of Norca. A host builds the engine from its
adapters:

```go
eng, err := rigger.New(
	rigger.WithCluster(cluster),
	rigger.WithJobDecoder(decoder),
	rigger.WithReleases(releases),
	rigger.WithLogger(logger),
)
```

## Development

Go 1.27 (the version of Norca). From the root of the repository:

```shell
make vet test     # both modules
make race lint    # the race detector, golangci-lint v2
make build        # bin/rigger
```

`go.work` joins the two modules for development. The CLI module builds
against norca's api and decoder modules in this repository (`../norca`).

## License

[MPL-2.0](LICENSE). Every file carries an SPDX header or falls under
[REUSE.toml](REUSE.toml).
