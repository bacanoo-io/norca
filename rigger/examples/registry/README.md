# Example registry

The layout of a registry repository: a git repository that holds modules
and stacks, each with its own version.

```
registry/
  modules/
    lib/placement/     a child module other modules use (no resources, only outputs)
    nats/              a NATS cluster: users in accounts, TLS, the changes of the region
    postgres/          a PostgreSQL server: databases, an init hook, periodic backups
    redis/             the built-in pack redis of norca as a module, its faults kept
      .rigger-signatures/  in every published module: its signatures, one file per key
  stacks/
    data-platform/     postgres + nats of this registry, with their secrets
    observability/     modules of another registry (norca), wired and sized,
                       plus a module of the stack's own
  .gitattributes       LF line ends: the digest of a module is over file contents
```

## Stacks

A stack of a registry is a tested composition others install as one thing.

- It names its modules **by address and version**, never by a path into the
  repository, also when they are in the same registry (`data-platform`).
- It may build on **another registry** (`observability`): it names that
  registry and carries the public key its modules are signed with.
- It may have **modules of its own** in `modules/`; they are part of the
  stack's digest and are not published separately.
- Its **lock file is published with it**, so a version of the stack always
  means the same module versions.
- Its `rigger` block names it as `metadata.hcl` does: the name is the owner of
  the releases it installs.

## Versions and tags

- Every module and every stack has its own version in `metadata.hcl`.
- A version is published as the tag `<name>/v<version>`: `nats/v1.0.0`,
  `postgres/v0.3.0`, `data-platform/v1.2.0`. A tag is never moved. Names are
  unique across the modules and stacks of the registry.
- A change to the resources a module produces raises the patch version, a
  new variable the minor version, a renamed or removed variable or a changed
  default the major version (the minor one while at `0.x`).

## Publishing

```shell
rigger key generate --out registry         # registry.key (encrypted with RIGGER_KEY_PASSWORD), registry.pub

rigger module validate modules/nats                  # schemas, examples, tests, icon
rigger module publish  modules/nats --key registry.key # the signature of its tree digest, committed; tag nats/v1.0.0

rigger stack sync stacks/data-platform               # resolve the modules, write the lock
cd stacks/data-platform
rigger stack validate --values examples/shop.vars.hcl # evaluate with the example values
rigger stack publish  --values examples/shop.vars.hcl --key ../../registry.key # validate, sign, tag data-platform/v1.2.0
```

`rigger stack validate` and `publish` evaluate a stack with the files of its
`values/`, or with those given with `--values`. A published stack keeps its value
files in `examples/`, and one with a variable without a default
(`data-platform`) is validated and published only with `--values`. Nothing is pushed: the tags
and the commits of the signatures are pushed by hand.

A signature is a file `.rigger-signatures/<key ID>.json` in the module or the
stack. The directory is hidden, so the tree digest it signs leaves it out.
The modules here carry the signatures of the key of the examples
(`../README.md`, "The lock files and the signatures").

## Using it

```hcl
rigger {
  name = "shop"

  registry "example" {
    source = "github.com/example/rigger-registry"
  }
}

module "nats" {
  source  = "example/nats"
  version = "~> 1.0"
}
```

## Rules of this registry

- Third-party software is referenced (an image, a release archive), never
  shipped; `metadata.hcl` names its license.
- Secrets come from the secret store of the cluster, never from a variable.
- Every module has at least one file in `examples/` and one test; every
  stack at least one file in `examples/`, planned in CI.
- CI: `rigger fmt -check`, `rigger module validate`, `rigger module test`,
  `rigger stack validate`, license and sign-off checks.
