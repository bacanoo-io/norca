# Stack "platform"

One region's shop platform: four releases from two kinds of modules, and
the place of a fifth from a third kind.

| Release | Module | From |
| --- | --- | --- |
| `postgres` | `example/postgres ~> 0.3` | the registry, signed, locked |
| `nats` | `example/nats ~> 1.0` | the registry, signed, locked |
| `shop-api` | `./modules/shop-api` | the stack's own |
| `shop-worker` | `./modules/shop-worker` | the stack's own |
| (`audit`) | `builtin/audit` | built into rigger: an idea of the design, not built, so `stack.hcl` has it as a comment ("Audit" below) |

```
platform/
  stack.hcl            the releases and how they are wired
  variables.hcl        the inputs of the stack
  secrets.hcl          secrets created once
  outputs.hcl
  AUDIT.md             what the audit module needs until rigger is embedded
  values/dev.vars.hcl  values of an environment
  values/prod.vars.hcl
  modules/             the stack's own modules
    shop-api/
    shop-worker/
  keys/                public keys the stack trusts
  rigger.lock.hcl      written by `rigger stack sync`
  .gitattributes       LF line ends: the digest of a module is over file contents
  .rigger/             local data of rigger (not in version control)
```

`rigger.lock.hcl` is the file a sync writes for this stack against
`../../registry`, and `keys/example-registry.pub` is the key the modules
there are signed with: the tests run the sync and keep both true
(`../../README.md`, "The lock files and the signatures").

## How the releases depend on each other

```
postgres ──┬─> shop-api ──┬─> shop-worker
nats ──────┤              │
           └──────────────┘
```

Nothing states the order: it follows from the outputs one module takes from
another (`module.postgres.databases["shop"]`, `module.nats.service`,
`module.shop-api.order_subjects`). `rigger stack apply` installs `postgres` and
`nats` first, waits until each is ready, then `shop-api`, then the rest.

## What the own modules show

- **`shop-api`** takes an element of the postgres module's output
  `databases` as it is; its variable has the same object type. It passes its
  bus connection and its subjects on as outputs.
- **`shop-worker`** makes one task group per subject with a `dynamic "group"`
  block.
- Neither of them has a registry, a tag or a signature: they are part of the
  repository of the stack and are versioned with it.

## Running it

```shell
rigger stack sync                      # versions, signatures, lock file
rigger stack plan  -f values/prod.vars.hcl   # what would change, per release and resource
rigger stack apply -f values/prod.vars.hcl
rigger stack status
```

The secrets (`secret` blocks) are created once with generated values; a
second apply keeps them.

## Audit

The stack writes no audit module of its own: it is to import the base that
rigger carries, `builtin/audit` (an idea, not designed in detail and not
built: a stack that names it is refused today, so the block is a comment in
`stack.hcl`), and declares what is audited in a resource file,
`modules/shop-api/shop.audit.hcl`:
namespaces, kinds of resources, actions, how long, who may read. Run by the
standalone rigger the changes come from outside: with `bus_servers` set, the
`nats` module receives the changes of the Norca region into its account
`AUDIT`, and what has to be set up around it (the leaf listener, the policy,
the token, the secret, how a reader reads) is in [AUDIT.md](AUDIT.md). Once
rigger is embedded in the Norca servers, the module sits on the internal bus
and those steps are not needed.
