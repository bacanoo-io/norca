# nats

A NATS cluster for the applications of one Norca region.

- **Cluster:** `count` servers (1, or an odd number), spread over the
  datacenters, never two on a node, one at a time down for a drain, lame duck
  on stop. The servers find each other in the native catalog.
- **JetStream** on the ephemeral disk of the allocation (`jetstream`).
- **Users in accounts** (`users`): every account has its own subjects and
  streams. Passwords come from the secret store
  (`<users_secret_prefix>/<name>`, item `password`), never from a variable.
- **TLS** on the client and route ports from the secret `tls_secret`
  (items `ca`, `cert`, `key`).
- **The changes of the Norca region** (`norca_bus`): the servers attach to
  the system bus of the Norca servers as leaf nodes and receive the subjects
  of the token's `cdc` rule, one way; the output `cdc_domain` names the
  JetStream domain of the consumers.

```shell
rigger install . --name nats --values examples/cluster-with-users.vars.hcl
```

| Output | |
| --- | --- |
| `service`, `client_port` | where clients connect |
| `accounts` | the accounts of the users |
| `cdc_domain` | the JetStream domain of the Norca changes, or null |

| File | |
| --- | --- |
| `metadata.hcl` | name, version, license, requirements, form tabs |
| `variables.hcl` | inputs and rules |
| `locals.hcl` | the server configuration as data, and a check |
| `modules.hcl` | the call of the shared `lib/placement` module |
| `nats.norca.hcl` | the job |
| `outputs.hcl` | outputs |

License: the module is MPL-2.0. NATS server is Apache-2.0; the official
`nats` image is referenced, not shipped.
