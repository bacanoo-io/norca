# data-platform

A stack of the registry built on **the registry's own modules**: `postgres`
and `nats`, with the password of the database superuser generated once.

| File | Holds |
| --- | --- |
| `metadata.hcl` | the `stack` block: name, version, requirements, form tabs |
| `stack.hcl` | the `rigger` block (the name of the stack, the registry) and the two modules, by address and version |
| `variables.hcl` | the inputs of the stack |
| `secrets.hcl` | the secret created once: the password of the superuser |
| `outputs.hcl` | `databases` and `bus`: what an application needs to connect |
| `rigger.lock.hcl` | the module versions, commits and digests this version of the stack was published with |
| `examples/shop.vars.hcl` | values to try it with |

A published stack is used by its address, with values of one's own:

```shell
rigger stack plan  github.com/example/rigger-registry//stacks/data-platform -version "~> 1.2" -f examples/shop.vars.hcl
rigger stack apply github.com/example/rigger-registry//stacks/data-platform -version "~> 1.2" -f examples/shop.vars.hcl
```

(`<namespace>/data-platform` is the address of a stack the global registry
lists.)

## Secrets

The stack creates `postgres/superuser` with a generated password, once; a
later apply keeps it. The modules read two more kinds of secrets, which
whoever installs the stack creates before the first apply, because a
`secret` block names one path and can't be written for each element of a
variable:

```shell
norca secret put postgres/apps/<database> user=<owner> password=-   # per element of databases
norca secret put nats/users/<application> password=-                # per element of bus_applications
```

## Building on it

A stack of your own, which names the registry `example` in its `rigger`
block, takes it as a part:

```hcl
stack "data" {
  source  = "example/data-platform"
  version = "~> 1.2"
  values  = { database_node = "worker-1", databases = [{ name = "shop" }], bus_applications = ["shop"] }
}

module "shop-api" {
  source = "./modules/shop-api"
  values = {
    database = stack.data.databases["shop"]
    bus      = stack.data.bus["shop"]
  }
}
```

Published as the tag `data-platform/v1.2.0`, signed like a module
(`rigger stack publish -f examples/shop.vars.hcl -key <key>`).
