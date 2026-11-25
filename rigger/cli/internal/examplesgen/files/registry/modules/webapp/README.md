# webapp

A stateless HTTP service on Norca: a job with health checks, a route
through Traefik, its settings in a Norca Variable, a session key from the
secret store and an optional cache. The default image,
`traefik/whoami`, answers every request with what it received; put the
image of your own service in `image`.

The module shows every kind of file a module may have:

| File | Holds |
| --- | --- |
| `metadata.hcl` | the `module` block (every field) and the tabs of the install form |
| `variables.hcl` | the variables with types, defaults, validation, form hints, a `secret` block, a sensitive variable, and the rules |
| `locals.hcl` | locals and a `check` block |
| `modules.hcl` | the call of the library module `../lib/sizing` |
| `outputs.hcl` | outputs, `next_steps` among them |
| `webapp.norca.hcl` | the service job |
| `smoke.norca.hcl` | a hook job that checks the service after an install or an upgrade |
| `webapp.acl.hcl` | the ACL policy of the job |
| `settings.var.hcl` | the Norca Variable with the settings |
| `files/app.toml.tftpl` | a template file, read with `templatefile()` |
| `files/smoke.sh` | a plain file, read with `file()` |
| `examples/*.vars.hcl` | values to render and validate the module with |
| `tests/webapp.riggertest.hcl` | the tests |

```shell
rigger module validate .
rigger module test .
rigger module render . --release shop -f examples/public.vars.hcl
rigger install . --name shop -f examples/public.vars.hcl   # needs a cluster
```

| Variable | Default | |
| --- | --- | --- |
| `image`, `image_digest`, `args` | `traefik/whoami:v1.10.3`, `""`, `[]` | the image; with a digest it is pinned |
| `env`, `log_level`, `features` | `{}`, `info`, `{}` | environment, log level, feature switches |
| `count`, `size` | `2`, `small` | instances and their size (`small`, `medium`, `large`) |
| `memory_oversubscription` | `false` | up to twice the memory; needs oversubscription on in the cluster |
| `datacenters`, `node_pool` | `["*"]`, `""` | placement |
| `port`, `health_path` | `8080`, `/health` | the listener and its health check |
| `public`, `hostname`, `service_tags` | `false`, `""`, `[]` | a route through Traefik |
| `session_secret` | `webapp/session` | the secret with the item `key` |
| `api_token` | `""` (sensitive) | goes into the settings Variable as a secret item |
| `cache_enabled`, `cache` | `false`, `{}` | a release of the module `cache` |
| `smoke_test` | `false` | the hook job |

Outputs: `service`, `port`, `url`, `settings_variable`, `next_steps`.

Before the first install, the session key must be in the secret store
(a stack creates it with a `secret` block of its `secrets.hcl`):

```shell
norca secret put webapp/session key=-
```

License: the module is yours; the image it runs is referenced, not shipped
(`traefik/whoami` is Apache-2.0).
