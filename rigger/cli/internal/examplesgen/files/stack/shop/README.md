# Stack "shop"

A stack of one's own: two modules of the registry of this project and one
module of the stack's own, wired through their outputs, with values per
environment, trust in the key of the registry and a lock file.

| Release | Module | From |
| --- | --- | --- |
| `cache` | `<% .Owner %>/cache ~> 0.1` | the registry, signed, locked |
| `web` | `<% .Owner %>/webapp ~> 0.1` | the registry, signed, locked |
| `heartbeat` | `./modules/heartbeat` | the stack's own |

```
shop/
  stack.hcl             the rigger block (name, namespace, registry, trust) and the releases
  variables.hcl         the inputs of the stack
  locals.hcl            locals and a check
  outputs.hcl           what an apply shows
  secrets.hcl           secrets created once, with generated values
  values/dev.vars.hcl   the values of an environment
  values/prod.vars.hcl
  modules/heartbeat/    the stack's own module
  keys/registry.pub     the public key of the registry (copied in, keys/README.md)
  rigger.lock.hcl       written by rigger stack sync; committed
  .gitattributes        LF line ends
  .gitignore            .rigger/: local data of rigger
```

## How the releases depend on each other

```
cache ──> web ──> heartbeat
  └────────────────┘
```

Nothing states the order but `depends_on` of `heartbeat`: it follows from
the outputs one release takes from another (`module.cache.connection`,
`module.web.service`). An apply installs `cache`, waits until it is ready,
then `web`, then `heartbeat`.

## Running it

The registry must have published versions first (`../../README.md`, steps
1 to 4), and `keys/registry.pub` must be here.

```shell
rigger stack sync                               # versions, signatures, rigger.lock.hcl
rigger stack validate -f values/dev.vars.hcl    # evaluate, without a cluster
rigger stack validate -f values/prod.vars.hcl

# with a cluster: NORCA_ADDR, NORCA_TOKEN
rigger stack plan  -f values/dev.vars.hcl       # exit code 2: there are changes
rigger stack apply -f values/dev.vars.hcl
rigger stack status
rigger stack destroy                            # uninstall every release of the stack
```

**`rigger stack validate` needs the lock file**: the modules of the
registry are loaded at the versions and digests it pins. Run `rigger stack
sync` again after a change of a version constraint (`--upgrade` takes the
newest version that meets each constraint).

A value can also come from the environment or the command line, the last
one winning: `RIGGER_VAR_web_count=2`, `--var web_count=2`.
