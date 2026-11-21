# Stack "platform"

One region's shop platform: five releases from three kinds of modules.

| Release | Module | From |
| --- | --- | --- |
| `postgres` | `example/postgres ~> 0.3` | the registry, signed, locked |
| `nats` | `example/nats ~> 1.0` | the registry, signed, locked |
| `audit` | `builtin/audit` | built into rig |
| `shop-api` | `./modules/shop-api` | the stack's own |
| `shop-worker` | `./modules/shop-worker` | the stack's own |

```
platform/
  stack.hcl            the releases and how they are wired
  variables.hcl        the inputs of the stack
  secrets.hcl          secrets created once
  outputs.hcl
  AUDIT.md             what the audit module needs until rig is embedded
  values/dev.vars.hcl  values of an environment
  values/prod.vars.hcl
  modules/             the stack's own modules
    shop-api/
    shop-worker/
  keys/                public keys the stack trusts
  rig.lock.hcl         written by `rig stack sync`
  .rig/                local data of rig (not in version control)
```

## How the releases depend on each other

```
postgres ──┬─> shop-api ──┬─> shop-worker
nats ──────┤              │
           └──────────────┘
```

Nothing states the order: it follows from the outputs one module takes from
another (`module.postgres.databases["shop"]`, `module.nats.service`,
`module.shop-api.order_subjects`). `rig stack apply` installs `postgres` and
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
rig stack sync                      # versions, signatures, lock file
rig stack plan  -f values/prod.vars.hcl   # what would change, per release and resource
rig stack apply -f values/prod.vars.hcl
rig stack status
```

The secrets (`secret` blocks) are created once with generated values; a
second apply keeps them.

## Audit

The stack writes no audit module of its own: it imports the base that rig
carries, `builtin/audit` (an idea, not designed in detail), and declares
what is audited in a resource file, `modules/shop-api/shop.audit.hcl`:
namespaces, kinds of resources, actions, how long, who may read. Run by the
standalone rig the changes come from outside: with `bus_servers` set, the
`nats` module receives the changes of the Norca region into its account
`AUDIT`, and what has to be set up around it (the leaf listener, the policy,
the token, the secret, how a reader reads) is in [AUDIT.md](AUDIT.md). Once
rig is embedded in the Norca servers, the module sits on the internal bus
and those steps are not needed.
