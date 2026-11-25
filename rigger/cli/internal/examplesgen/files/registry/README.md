# The registry of <% .Owner %>

A registry repository of Rigger: a git repository that holds modules and
stacks, each with its own version, released by signed git tags. Stacks
name it in a `registry` block and its modules as `<% .Owner %>/<name>`.

```
registry/
  modules/
    webapp/              a module with every kind of file a module may have
    cache/               a smaller module the web service may use
    lib/sizing/          a library module the others call (no resources)
  stacks/
    web-platform/        a published stack: webapp + cache of this registry
  scripts/check.sh       the checks a CI runs, without a cluster
  .gitattributes         LF line ends: the digest of a module is over file contents
  .gitignore             no private keys, no local data of rigger
```

**A registry repository needs no index file.** Whoever reads it (rigger
stack sync, rigger search and rigger show, the servers of a cluster at a
sync of a Registry object) reads `modules/<name>/metadata.hcl`,
`stacks/<name>/metadata.hcl` and the tags `<name>/v<version>` of the
repository. An index of the shape of the global registry is something
else: a separate repository of entries over one or more source
repositories (`../index/`).

## Versions and tags

- Every module and stack has its own version, in its `metadata.hcl`
  (Semantic Versioning, no leading `v`).
- A version is released as the tag `<name>/v<version>`: `webapp/v0.1.0`,
  `cache/v0.1.0`, `web-platform/v1.0.0`. **A tag is never moved**; a change
  is a new version. Names are unique across the modules and stacks of the
  registry.
- A change of what a module renders raises the patch version, a new
  variable the minor one, a renamed or removed variable or a changed
  default the major one (the minor one while at `0.x`).

## Publishing a version

The key pair is made once, outside the repository (`../keys/`):

```shell
export RIGGER_KEY_PASSWORD='a long password'        # the password of the private key
rigger key generate --out ../keys/registry          # ../keys/registry.key and ../keys/registry.pub
```

A module is published from a committed tree:

```shell
git add -A && git commit -s -m "webapp 0.1.0"
rigger module validate modules/webapp               # files, icon, tests parsed, examples rendered
rigger module test modules/webapp                   # tests/*.riggertest.hcl
rigger module publish modules/webapp --key ../keys/registry.key
```

`rigger module publish` validates the module, signs the tree digest of its
commit into `modules/webapp/.rigger-signatures/<key ID>.json`, commits the
signature and tags the commit `webapp/v0.1.0`. **Nothing is pushed:** push
the commits and the tags yourself when the registry lives on a git host
(`git push --follow-tags`, or `git push origin webapp/v0.1.0`).

A stack of the registry is published after its modules, with its lock
file (`stacks/web-platform/README.md`).

## Checks

```shell
scripts/check.sh                       # fmt, module validate and test, stack validate
RIGGER=/path/to/rigger scripts/check.sh
```

## Rules of a registry

- Third-party software is referenced (an image, a release archive), never
  shipped; `metadata.hcl` names its license.
- Secrets come from the secret store of the cluster, never from a variable.
- Every module has a README, at least one file in `examples/` and tests;
  every stack at least one file in `examples/` and its lock file.
- A registry that others use carries a `LICENSE` (and, for REUSE, a
  `REUSE.toml` or SPDX headers in its files).
