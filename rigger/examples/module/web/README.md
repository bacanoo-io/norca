# web

A stateless web service: one job, a native service with readiness and
liveness checks, optional connections to a PostgreSQL and a NATS.

```shell
rigger module validate .
rigger module test .
rigger module render . --name shop -f examples/with-database.vars.hcl
rigger install . --name shop -f examples/with-database.vars.hcl
```

| Variable | Default | |
| --- | --- | --- |
| `image`, `image_digest` | `ghcr.io/example/web:1.4.2`, `""` | the image; with a digest the digest is run |
| `count` | `2` | instances |
| `datacenters` | `["*"]` | |
| `resources` | `{cpu = 200, memory = 128}` | per instance |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `database`, `database_secret` | `null`, `""` | a PostgreSQL service and the secret with its credentials |
| `bus` | `null` | a NATS service |

Outputs: `service` (the native service name), `next_steps`.

This module has no `icon.png`, so it is shown with the default icon.

License: the module is MPL-2.0; the image it runs is referenced, not shipped.
