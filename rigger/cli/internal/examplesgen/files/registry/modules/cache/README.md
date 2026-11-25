# cache

One Valkey server as a cache for the applications of a namespace. The
server requires the password of a secret of the secret store, which it
reads when it starts; with `persistence` its data is on a host volume.

```shell
rigger module validate .
rigger module test .
rigger module render . -f examples/persistent.vars.hcl
```

| Variable | Default | |
| --- | --- | --- |
| `image` | `valkey/valkey:8.0-alpine` | |
| `port` | `6379` | |
| `memory`, `eviction` | `256`, `allkeys-lru` | MiB; keys are evicted at 80 % |
| `password_secret` | `cache/password` | the secret with the item `password` |
| `persistence`, `host_volume` | `false`, `cache` | the append-only file on a host volume |

Outputs: `service`, `port`, `connection` (the value of the variable `cache`
of `webapp`), `next_steps`.

```shell
norca secret put cache/password password=-   # before the first install, unless a stack creates it
```

License: the module is yours; Valkey (BSD-3-Clause) is referenced, not
shipped.
