# data-platform

A stack of the registry built on **the registry's own modules**: `postgres`
and `nats`, with the secrets they need generated once.

| File | Holds |
| --- | --- |
| `metadata.hcl` | the `stack` block: name, version, requirements, form tabs |
| `stack.hcl` | the registry and the two modules, by address and version |
| `variables.hcl` | the inputs of the stack |
| `secrets.hcl` | secrets created once: the superuser, one per database, one per application |
| `outputs.hcl` | `databases` and `bus`: what an application needs to connect |
| `rig.lock.hcl` | the module versions and digests this version of the stack was published with |

```shell
rig stack show  example/data-platform
rig stack plan  example/data-platform --version "~> 1.2" -f examples/shop.vars.hcl
rig stack apply example/data-platform --version "~> 1.2" -f examples/shop.vars.hcl
```

A stack of your own can build on it:

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

Published as the tag `data-platform/v1.2.0`, signed like a module.
