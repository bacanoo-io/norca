<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# test-app

A small web application for the Norca examples, as a module of
[Rigger](../../../rigger/README.md). It runs the official **nginx** image
(`nginx:1.28.0-alpine`), which is small, widely known, has a shell for
`norca alloc exec`, reloads on `SIGHUP`, and whose software is under
permissive licenses (nginx: BSD-2-Clause; the Alpine base packages under
their own licenses). Norca only references the image; the clients pull it.
Pin a digest in production.

The job is built to show Norca features through its variables:

| Feature | In the job | Variables |
| --- | --- | --- |
| Built-in secret store | `secret "db"` block: the secret `<job_name>/db` as `secrets/db.env` and as `DB_USER`/`DB_PASSWORD`; `secret_access { path = "${job}/*" }` grants the read to the workload identities | `secrets_enabled` |
| Vault compatibility without a Vault | the page template reads `{{ with secret "kv/data/<job_name>/db" }}`, served by the built-in store, and shows only `.Data.metadata.version` | `secrets_enabled` |
| Health check roles | `startup` (`/healthz`, `startup_timeout`), `readiness` (`/ready`), `liveness` (`/healthz`) | `startup_timeout` |
| Native service catalog | group service (the default provider), optional `discovery { prefer, min_local, failover }` | `discovery_prefer`, `discovery_min_local`, `discovery_failover` |
| Disruption budget | `disruption { max_unavailable }` | `disruption_max_unavailable` |
| Cross-job anti-affinity | `separate { selector { app = <job_name> } scope required }` | `separate_scope`, `separate_required` |
| Typed scaling policy | `scaling { engine = "builtin" policy { check "cpu" { metric = "norca.cpu" strategy = "target-value" target } } }` | `scaling_enabled`, `scaling_min`, `scaling_max`, `scaling_cpu_target` |
| Native mTLS | `mtls { mode = "sidecar" }`: Envoy sidecar, the app port is not mapped to the host, the checks address the allocation | `mtls_sidecar` |
| Multiregion deployment | `multiregion { strategy region ... }`, group `count = 0` | `multiregion`, `multiregion_max_parallel`, `multiregion_on_failure` |
| Constraints and rules | `validation` blocks of the variables and `rule` blocks across variables (`count` within the scaling bounds, `mtls_sidecar` only with bridge networking, multiregion without scaling, no `:latest` image), checked before anything is applied; the `ui` blocks and the tabs in `metadata.hcl` lay out the install form of the web UI for a module of a registry | – |

The job meta `app = <job_name>` and `team` are what the network policies,
`colocate`/`separate` selectors and admission policies of the examples match.

## Usage

```shell
# The task waits for its secret (unless secrets_enabled = false).
norca secret put [-namespace <ns>] test-app/db username=app password=-

norca rigger module validate ./example/modules/test-app        # offline: files, tests
norca rigger module test     ./example/modules/test-app        # offline: tests/*.riggertest.hcl
norca rigger module render   ./example/modules/test-app [-f values.hcl]   # offline: the resources as JSON
norca rigger install ./example/modules/test-app -name test-app [-namespace <ns>] [-f values.hcl]
norca rigger release status test-app [-namespace <ns>]
norca rigger release uninstall test-app [-namespace <ns>]
```

`install` installs the release, or upgrades it when it exists, and waits
until it is ready. The name of the job comes from `job_name`, not from the
release: a second release of the module needs another `job_name`
([03-three-node/test-api.hcl](../../03-three-node/test-api.hcl)).

The scenarios have value files for it:
[01-local-dev/test-app.hcl](../../01-local-dev/test-app.hcl),
[02-single-node/test-app.hcl](../../02-single-node/test-app.hcl),
[03-three-node/test-app.hcl](../../03-three-node/test-app.hcl) and
[test-api.hcl](../../03-three-node/test-api.hcl),
[04-multi-region/test-app-multiregion.hcl](../../04-multi-region/test-app-multiregion.hcl).

## Variables

See [variables.hcl](variables.hcl); the most important:

| Variable | Default | |
| --- | --- | --- |
| `job_name` | `test-app` | job and service name; the secret is `<job_name>/db` |
| `count` | `2` | between `scaling_min` and `scaling_max` with scaling |
| `network_mode` | `bridge` | `host` for dev agents without the CNI plugins |
| `secrets_enabled` | `true` | |
| `disruption_max_unavailable` | `"1"` | empty removes the block |
| `separate_scope` | `""` | e.g. `"$${node.datacenter}"` in value files |
| `scaling_enabled` | `true` | needs `server { autoscaler { enabled = true } }`; `false` with `multiregion` |
| `mtls_sidecar` | `false` | needs `network_mode = "bridge"` |
| `multiregion` | `[]` | `[{ name = "eu-1", count = 2 }, ...]` |

The namespace and the region of the job are those of the release
(`-namespace`, `-region` of `install`). `${...}` in value files must be
written as `$${...}`, so that it reaches the job unchanged.

Outputs: `service` (the name in the catalog) and `notes`.

## Requirements on the clients

Docker; for `network_mode = "bridge"` and `mtls_sidecar` a root client with
the CNI plugins; for `mtls_sidecar` also the Envoy image
([native mTLS: sidecar and gateway](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/native-mtls/sidecar-and-gateway.md#envoy-image)).
