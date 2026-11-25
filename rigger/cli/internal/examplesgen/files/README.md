# Rigger examples: a registry, modules and stacks of your own

Written by `rigger generate examples`. Everything here is local: a
registry repository of your own, its modules and a published stack, a
stack of your own that installs from that registry, and the files that
register the registry in a cluster. Every file is commented: what a block
or field means, its default and its allowed values. Commented-out lines
show optional fields.

```
.
  registry/                  the registry repository (a git repository once you run git init)
    modules/webapp/          a module with every kind of file a module may have
    modules/cache/           a smaller module that webapp may use
    modules/lib/sizing/      a library module webapp calls (no resources)
    stacks/web-platform/     a published stack: webapp + cache of this registry
    scripts/check.sh         the checks of the registry, without a cluster
  stack/shop/                a stack of your own: the two modules from the registry,
                             one module of its own, values per environment, trust
  keys/                      the signing key pair, outside every repository (made in step 2)
  index/                     an index of the shape of the global registry over the registry
  cluster/registry.hcl       the Registry object that registers the registry in a cluster
```

The owner of the modules is **<% .Owner %>**: the stacks name the registry
`registry "<% .Owner %>"` and its modules `<% .Owner %>/webapp`,
`<% .Owner %>/cache` (`rigger generate examples --owner <name>` writes
another). The stacks and `cluster/registry.hcl` name the registry by the
path of its repository on this machine:

```
<% .RegistryPath %>
```

When you move the directory or push the registry to a git host, change
that path in `registry/stacks/web-platform/stack.hcl`,
`stack/shop/stack.hcl`, `index/namespaces/<% .Owner %>.json` and
`cluster/registry.hcl`, and run `rigger stack sync` again.

Nothing below needs a cluster until step 9. You need `rigger` and `git` on
the `PATH`; the commands run from this directory unless they say `cd`.

## 1. Make the registry a git repository

Versions are git tags, and a module reads its child modules from the same
repository, so the registry is a git repository before anything else:

```shell
cd registry
git init -b main
git config user.name  "Your Name"            # if git has none yet: publishing commits
git config user.email "you@example.com"
git add -A
git commit -s -m "The registry of <% .Owner %>"
cd ..
```

## 2. Make the signing key

```shell
export RIGGER_KEY_PASSWORD='a long password'   # the private key is encrypted with it
rigger key generate --out keys/registry         # keys/registry.key, keys/registry.pub
```

The private key stays in `keys/`, outside every repository
(`keys/README.md`). `registry/.gitignore` refuses `*.key` too.

## 3. Check the modules

```shell
cd registry
rigger fmt --check modules stacks                           # formatted as rigger fmt writes
rigger module validate modules/webapp                       # files, tests parsed, examples rendered,
rigger module validate modules/cache                        #   jobs decoded as Norca decodes them
rigger module test modules/webapp                           # tests/*.riggertest.hcl
rigger module test modules/cache
rigger module render modules/webapp --release shop -f modules/webapp/examples/public.vars.hcl
rigger module render modules/webapp --var count=3 --var 'features={ beta = true }'
```

`render` prints the evaluated resources and outputs as JSON: the jobs as
Norca decodes them, the ACL policy, the Norca Variable (its secret items
named, never shown). A file of `--values` (`-f`) is read relative to the
current directory.

## 4. Publish the modules

```shell
rigger module publish modules/webapp --key ../keys/registry.key
rigger module publish modules/cache  --key ../keys/registry.key
git tag                                                     # cache/v0.1.0, webapp/v0.1.0
```

Each publish validates the module, signs the tree digest of its commit
into `modules/<name>/.rigger-signatures/<key ID>.json`, commits the
signature and tags the commit `<name>/v<version>`. The tree must be
committed first. **Nothing is pushed**; on a git host:
`git push origin main --tags`.

## 5. Publish the stack of the registry

A published stack is synced against the registry, and its lock file is
published with it:

```shell
rigger stack sync stacks/web-platform                       # writes stacks/web-platform/rigger.lock.hcl
git add stacks/web-platform/rigger.lock.hcl
git commit -s -m "web-platform: lock"
rigger stack validate stacks/web-platform -f stacks/web-platform/examples/public.vars.hcl
rigger stack publish stacks/web-platform --key ../keys/registry.key   # signs, tags web-platform/v1.0.0
scripts/check.sh                                            # everything at once, as a CI would
cd ..
```

## 6. A stack of your own

`stack/shop` installs `<% .Owner %>/cache` and `<% .Owner %>/webapp` from the
registry and trusts only modules signed by its key:

```shell
cp keys/registry.pub stack/shop/keys/registry.pub
cd stack/shop
rigger stack sync                                           # resolves the versions, verifies the
                                                            #   signatures, writes rigger.lock.hcl
rigger stack validate -f values/dev.vars.hcl                # evaluates the stack without a cluster
rigger stack validate -f values/prod.vars.hcl
rigger stack validate                                       # with each file of values/ in turn
cd ../..
```

`rigger stack validate` needs the lock file: it loads the registry modules
at the versions and digests the lock pins. Without `keys/registry.pub`
the sync fails on the `file()` of the trust block. If the stack is a git
repository of its own, commit `rigger.lock.hcl` with it.

## 7. Find modules without a cluster

```shell
export RIGGER_CACHE_DIR="$PWD/.rigger-cache"                # optional: where fetched sources are kept
rigger search --registry ./registry                         # every module and stack of the registry
rigger search webapp --registry ./registry --json
rigger show '<% .RegistryPath %>//modules/webapp'
rigger show '<% .RegistryPath %>//modules/webapp' --readme
rigger show '<% .RegistryPath %>//stacks/web-platform'
```

`search` reads the newest commit of the default branch of the registry,
`show` the newest tagged version (`--version "~> 0.1"` narrows it). Of a
registry repository `show` checks the commit and the tree digest; the
signatures are checked against keys, by a stack's `trust` block, by an
index (step 8) or by a cluster (`trustedKeys`).

## 8. An index of the shape of the global registry (optional)

The global registry lists modules as `<namespace>/<name>`. `index/` is
one over this registry (`index/README.md`):

```shell
mkdir -p index/keys/<% .Owner %>
cp keys/registry.pub index/keys/<% .Owner %>/registry.pub
rigger index update --dir index                             # writes index/modules/.../<% .Owner %>/*.json
rigger index check --dir index
RIGGER_GLOBAL_REGISTRY="$PWD/index" rigger search webapp
RIGGER_GLOBAL_REGISTRY="$PWD/index" rigger show <% .Owner %>/webapp
```

## 9. With a cluster

The commands talk to the cluster of `NORCA_ADDR` with the token of
`NORCA_TOKEN`:

```shell
export NORCA_ADDR=https://norca.example.com:4646
export NORCA_TOKEN=<a token that may write releases in the namespaces>

# One module, without a stack: from its directory, or by its address.
rigger install registry/modules/webapp --name web --namespace default -f registry/modules/webapp/examples/public.vars.hcl
rigger install '<% .RegistryPath %>//modules/webapp' --version "~> 0.1" --name web
rigger release status web
rigger release list

# The stack of your own.
cd stack/shop
rigger stack plan  -f values/dev.vars.hcl                   # exit code 2: there are changes
rigger stack apply -f values/dev.vars.hcl
rigger stack status
cd ../..

# The published stack, by its address, with values of your own.
rigger stack apply '<% .RegistryPath %>//stacks/web-platform' --version "~> 1.0" -f my.vars.hcl
```

The secrets the modules read are created by the `secrets.hcl` of a stack
at its first apply. A module installed alone needs them first:
`norca secret put webapp/session key=-`.

**The registry in the cluster.** `cluster/registry.hcl` is the object of
the kind `rigger.norca.io/Registry` that makes the registry a registry of
the cluster: the servers fetch its index at a sync, the web UI shows its
modules, and the servers install from it. The servers read the path only
when they run on this machine; otherwise push the registry and put its
git address into the file first.

```shell
norca object apply -dry-run cluster/registry.hcl
norca object apply cluster/registry.hcl
rigger registry sync <% .Owner %>
rigger registry status <% .Owner %>
rigger registry search <% .Owner %> webapp
rigger install webapp --registry <% .Owner %> --name web --server    # the servers fetch and evaluate
```

## The environment

| Variable | What it does |
| --- | --- |
| `RIGGER_KEY_PASSWORD` | the password of the private key: `key generate`, `key sign`, `module publish`, `stack publish`; empty for a key without one |
| `NORCA_ADDR`, `NORCA_TOKEN` | the cluster and the token (plan, apply, install, release, registry) |
| `NORCA_REGION`, `NORCA_NAMESPACE` | the region and the namespace to talk to; `NORCA_CACERT` and the other TLS settings of the norca command |
| `RIGGER_SERVER=1` | plan, apply, status, destroy and install go to the servers of the cluster, as `--server` |
| `RIGGER_VAR_<name>` | the value of the variable `<name>`, over the files of `-f`, under `--var` |
| `RIGGER_GLOBAL_REGISTRY` | the global registry: a git address or a directory (`index/` of step 8) |
| `RIGGER_CACHE_DIR` | the cache of fetched modules and registries |
| `RIGGER_CREDENTIALS_FILE` | the file `rigger login` writes the credentials of git and OCI hosts to |
| `RIGGER_GIT_HOST`, `RIGGER_GIT_USERNAME`, `RIGGER_GIT_PASSWORD` | credentials for one git host, when `rigger login` has none for it |
| `RIGGER_OCI_HOST`, `RIGGER_OCI_USERNAME`, `RIGGER_OCI_PASSWORD` | the same for an OCI registry |

`rigger help environment` lists them; `rigger help <command>` shows every
flag of a command. A flag is written `--name` (`-name` is read the same).

## A new version

1. Change the module, raise `version` in its `metadata.hcl` (a patch for a
   change of what it renders, a minor for a new variable, a major for a
   renamed or removed variable or a changed default; the minor while at
   `0.x`).
2. `rigger module validate` and `rigger module test`, commit.
3. `rigger module publish modules/<name> --key ../keys/registry.key`.
4. In a stack: `rigger stack sync --upgrade` takes the newest version that
   meets each constraint and writes the lock again; a constraint such as
   `"~> 0.1"` decides how far it goes.
