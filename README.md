# Norca

An independent, open source workload scheduler and orchestrator built on the **HashiCorp Nomad v1.6.5** source code (MPL 2.0), with its own extensions: a built-in autoscaler, namespace quotas, admission policies, backup and restore, service discovery without Consul, package management and a new web UI.

**This project is not a product of HashiCorp or IBM and is not affiliated with them.** "Nomad", "Consul" and "HashiCorp" are trademarks of their respective owners; they appear here only to describe the origin of the code.

## Documentation

The documentation lives in its own repository, [bacanoo-io/norca-doc](https://github.com/bacanoo-io/norca-doc) (a Hugo site): introduction and compatibility, getting started, the Norca features with examples, configuration and diagrams, the job specification, agent configuration, CLI and HTTP API reference, operations, security, and the design documents with their status. It replaces the former `docs/` directory of this repository (design documents, `licensing.md`, `native-mtls.md`), which moved there on 2026-10-27. The reference pages adapt the Nomad v1.6.5 documentation text, which stays here unchanged as the upstream reference in [nomad/website/content/](nomad/website/README.md).

## Origin

| Component | Based on | Commit | License |
| --- | --- | --- | --- |
| `nomad/` | HashiCorp Nomad v1.6.5 | `a7cfff372cd52b02232a02bd3ffb5e5d35f88a0f` | MPL 2.0 |
| `nomad/pack/` | HashiCorp Nomad Pack v0.4.2 | `cd4d2a41c22adba1350b8d5da63252f2dde33262` | MPL 2.0 |
| built-in autoscaler (`nomad/nomad/autoscaler/`) | HashiCorp Nomad Autoscaler v0.6.0 | `a24707db8f2abd3ee74ce0cb5cc0d2b699d2cace` | MPL 2.0 |

v1.6.5 is the last Nomad release published under MPL 2.0. `nomad/` and Nomad Pack were imported with their full git history, so the starting commits can be verified; Nomad Pack was later moved into `nomad/pack/` (part of the Nomad Go module, run as `norca pack`); its original commits remain in the history and `git blame` still reaches them. This project contains no code from HashiCorp releases published under BSL 1.1. Details: [licence and attribution](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/introduction/license.md).

## Usage

Norca is a single binary, `norca`, which runs both the agent and the CLI. Build it from the `nomad/` directory (the Go module keeps its original path, `github.com/hashicorp/nomad`):

```shell
cd nomad
make dev                  # writes bin/norca and copies it to $GOBIN
# or: go build -o norca .
```

```shell
norca agent -dev          # start a development agent
norca job init            # write an example job file
norca job run example.nomad.hcl
norca version             # Norca v0.1.0 (Nomad API 1.6.5 compatible)
```

The version line shows the Norca version and the Nomad version whose HTTP API and protocols Norca is compatible with; existing Nomad API clients, agents of the cluster and tools keep working against it.

Step-by-step deployment examples with the new features (development agent, single node, three nodes, multi region): [example/](example/README.md).

### Packs

`norca pack` (Nomad Pack, built in) installs packs from git and OCI registries. The operations packs (NATS, the cross-region mTLS gateway, RustFS as the backup store, Consul, the LoadBalancer operator) live in the [Norca pack registry](https://github.com/bacanoo-io/norca-registry):

```shell
norca pack registry add norca https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
norca pack run nats --registry=norca
```

See [nomad/pack/README.md](nomad/pack/README.md) and the [pack docs](nomad/pack/docs/).

### Compatibility alias `nomad`

A copy of the binary, or a symlink to it, named `nomad` (`nomad.exe` on Windows) behaves exactly like `norca`: the same commands, the same output, and no extra text, so existing scripts keep working. The alias is not shipped or installed by default; create it yourself if you need it, e.g. `ln -s "$(command -v norca)" /usr/local/bin/nomad`. Help and usage texts always say `norca`.

### Environment variables

The CLI and the API client read their settings from `NORCA_*` environment variables. Each one is also accepted with its Nomad name (`NOMAD_*`), which is used when the `NORCA_*` variable is unset or empty:

| Variable | Fallback | Meaning |
| --- | --- | --- |
| `NORCA_ADDR` | `NOMAD_ADDR` | Address of the HTTP API, default `http://127.0.0.1:4646` |
| `NORCA_TOKEN` | `NOMAD_TOKEN` | ACL token secret ID |
| `NORCA_REGION` | `NOMAD_REGION` | Region to forward requests to |
| `NORCA_NAMESPACE` | `NOMAD_NAMESPACE` | Target namespace |
| `NORCA_HTTP_AUTH` | `NOMAD_HTTP_AUTH` | HTTP basic auth, `user:password` |
| `NORCA_CACERT`, `NORCA_CAPATH` | `NOMAD_CACERT`, `NOMAD_CAPATH` | CA certificate file or directory |
| `NORCA_CLIENT_CERT`, `NORCA_CLIENT_KEY` | `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY` | Client certificate and key for mTLS |
| `NORCA_TLS_SERVER_NAME` | `NOMAD_TLS_SERVER_NAME` | SNI server name |
| `NORCA_SKIP_VERIFY` | `NOMAD_SKIP_VERIFY` | Skip TLS verification |
| `NORCA_CLI_NO_COLOR`, `NORCA_CLI_FORCE_COLOR` | `NOMAD_CLI_NO_COLOR`, `NOMAD_CLI_FORCE_COLOR` | Disable or force colored output |
| `NORCA_PACK_ALLOW_UNSET_VARS`, `NORCA_PACK_PLAN_EXIT_CODE_*` | `NOMAD_PACK_*` | `norca pack` settings |
| `NORCA_PACK_CONFIG`, `NORCA_PACK_CACHE_DIR`, `NORCA_PACK_REGISTRY_USERNAME`, `NORCA_PACK_REGISTRY_PASSWORD`, `NORCA_PACK_KEY_PASSWORD`, `NORCA_PACK_LOCK` | – | `norca pack` OCI registries, signing and lock files ([details](nomad/pack/docs/supply-chain.md)) |

Command-line flags take precedence over both. Names that are part of the Nomad compatibility surface are unchanged: the variables injected into workloads (`NOMAD_ALLOC_ID`, `NOMAD_TASK_DIR`, ...), the jobspec variable prefix `NOMAD_VAR_`, the pack variable prefix `NOMAD_PACK_VAR_`, the `X-Nomad-Token` HTTP header and the agent's configuration, metric and Raft/Serf names.

## Feature status

| Feature | Status | Design |
| --- | --- | --- |
| Nomad v1.6.5 core features | Available | – |
| Dependency updates (Go 1.27, gRPC 1.84, security fixes) | Available | [development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md) |
| Built-in autoscaler (leader subsystem; built-in strategies; `nomad` metrics pushed by clients and Prometheus; external APM/strategy plugins; typed, validated `scaling` policies; quota and deployment aware; `Scaling` events; off by default) | Available | [development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md) |
| Built-in autoscaler: node pool scaling with cloud providers (phase 4; targets: Hetzner Cloud, Scaleway, AWS, GCP) | Postponed | [development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md) |
| Namespace quotas | Available | [specification, item 1](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Dynamic admission control (CEL policies, webhooks) | Available | [specification, item 2](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Event stream: Variables topic, resumable subscriptions | Available | [specification, item 3](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Cross-job affinity (`colocate`, `separate`) | Available | [specification, item 4](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Disruption budget | Available | [specification, item 5](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Health check roles (startup, readiness, liveness) | Available | [specification, item 6](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Network rules, levels A and B (`NetworkPolicy`, cluster-level `AdminNetworkPolicy` with explicit deny; egress by CIDR and port, ingress on the node and host ports; nftables in the allocation's network namespace; nodes that cannot enforce are filtered out) | Available | [specification, item 7](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Operator framework: native custom objects (`ObjectKind`, `CustomObject`, schema, CEL with `oldSelf`), controller SDK `nomad/controller`, LoadBalancer example; lifecycle: finalizers, deletion modes (background, foreground, orphan), garbage collection of owned objects and jobs, per-object event log, managed-job protection, `scale` subresource for the autoscaler; ecosystem: operator packs (`loadbalancer-operator` of the [pack registry](https://github.com/bacanoo-io/norca-registry)), type-bound webhooks, schema migrations with CEL, per-kind limits, `norca operator init`, `norca object-kind lint`, SDK `webhook` and `envtest` packages | Available (phases 1–4, including the web UI: object and type definition pages with lifecycle, webhook approval and limits) | [specification, item 8](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md) |
| Event stream → NATS bridge (`nomad/natsbridge`, one job per region) | Available | [development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md) |
| Multiregion deployments (one region per cloud; staged rollout, `on_failure`, global tokens, `norca deployment run`) | Available | [development plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/nomad-fork-development-plan/index.md) |
| NATS pack (`nats` of the pack registry: supercluster with JetStream and gateways, optional bridge) | Available | [nats](https://github.com/bacanoo-io/norca-registry/blob/main/packs/nats/README.md) |
| Backup and restore: scheduled, age-encrypted backups to S3-compatible stores (AWS S3, RustFS with the `rustfs` pack, ...), browsing and selective restore (`norca operator backup`) | Available (phases 1–2) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/backup-restore-plan.md) |
| Backup and restore: full region restore with preview and two-person approval, keyring restore, `restore-bootstrap` for an empty cluster | Available (phase 4) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/backup-restore-plan.md) |
| Backup and restore: web UI (backup list, browse, compare and restore with an age identity, schedules) | Available (backup phase 3, in the new web UI) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/backup-restore-plan.md) |
| Service discovery without Consul: health-based catalog, built-in DNS, Consul-compatible read layer for Traefik, Prometheus and consul-template (DNS and the compat layer are off by default) | Available (phases 1–3) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/consul-independence-plan.md) |
| Service discovery without Consul: external services (address or DNS alias, TTL/HTTP/TCP checks, `norca service register`), Consul agent write endpoints in the compat layer | Available (phase 4) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/consul-independence-plan.md) |
| Full Consul as an option: the `consul` pack of the pack registry installs and manages the official Consul release (not distributed by this project; BSL 1.1, accepted by the user at install) with gossip, TLS and ACL bootstrap through Norca Variables | Available (phase 6) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/consul-independence-plan.md), [consul](https://github.com/bacanoo-io/norca-registry/blob/main/packs/consul/README.md) |
| Native mTLS, steps 1–2: per-region CA (key encrypted in Raft, backed up, federated trust bundle), short-lived SPIFFE workload certificates in `secrets/mtls` (`mtls { mode = "app" }`), `ServiceIntention` objects (`norca service intention`), Go library `api/mtls` | Available (phase 5, steps 1–2) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/consul-independence-plan.md) |
| Native mTLS, step 3: Envoy sidecar (`mtls { mode = "sidecar" }`, file-based xDS, intentions as Envoy RBAC), upstreams on local ports, cross-region SNI passthrough gateway (`mode = "gateway"`, `upstream { region }`) | Available (phase 5, step 3; tested against Envoy 1.39 and 1.40) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/consul-independence-plan.md), [usage](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/native-mtls/sidecar-and-gateway.md) |
| Package management: Nomad Pack built in (`norca pack`) | Available | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/package-management-plan.md) |
| Norca pack registry ([norca-registry](https://github.com/bacanoo-io/norca-registry), a separate repository): operations packs `nats`, `mtls-gateway` (cross-region mTLS gateway), `rustfs` (S3 store of the backups), `consul`, `loadbalancer-operator` | Available | [registry](https://github.com/bacanoo-io/norca-registry#readme) |
| Package management, client-side supply chain: OCI registries, cosign-compatible signing and trust policies, offline bundles (air-gapped), `pack.lock` | Available | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/package-management-plan.md), [usage](nomad/pack/docs/supply-chain.md) |
| Package management: server-side releases (`packs.norca.io/Release` objects with revision history, atomic install/upgrade, hooks and tests, rollback, drift detection, uninstall), signature enforcement in admission (`trusted_keys`) | Available | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/package-management-plan.md), [usage](nomad/pack/docs/releases.md) |
| Package management: JSON Schema for pack variables (`variables_schema`, checked by the CLI and the servers), multi-region installs (`--regions`, global resources from the authoritative region, per-region status) | Available | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/package-management-plan.md), [usage](nomad/pack/docs/releases.md) |
| Embedded secret storage (sealed keyring with cloud KMS, transit or unseal shares; versioned, audited secrets; `secret` block delivering files and env); the existing Vault integration stays, further external providers (OpenBao, AWS, GCP and Scaleway secret managers, plugins) can be added, and lookups fall back to the built-in store when no external store is defined | In progress (phases 0–4: uBixVault core on Norca's Raft; `shares` seal with `norca operator unseal`, seal status and online rewrap; versioned `Secret` objects with `norca secret`, ACL capabilities, `SecretAccess` audit topic and audit file; `secret` block delivering files and env with `norca` and `vault` providers and fallback, `nomadSecret` template function, `secret_access` grants; Vault compatibility without a Vault: Vault-style `secret` template reads and task `vault` blocks served by the built-in store through a client Vault-compatible KV endpoint, `norca secret import -from-vault` / `-from-variable`; web UI Secrets page with audited value reveal and seal status) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/secrets-plan.md) |
| New web UI: Ember 6.12 LTS with a Tabler base and ECharts, live updates over the event stream, dashboard as the landing page; every page on the Norca component layer (jobs, allocations, clients, servers and regions, topology, storage, evaluations, Variables, access, backups, quotas, scaling, services, intentions, network rules, multiregion deployments, packages, objects, type definitions, admission policies, with unified lists and HCL/JSON editors); WCAG 2.1 AA in light and dark mode; the previous UI stays at `/ui/legacy` for one release | Available (phases 1–4) | [plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/ui-plan.md) |

Only features marked "Available" can be used; the others are being built or are in the design phase.

## License

The code and documentation of this project are licensed under the **MPL 2.0** ([LICENSES/MPL-2.0.txt](LICENSES/MPL-2.0.txt)). Files taken from third parties keep their original license; these are marked by their file headers and by [REUSE.toml](REUSE.toml). The licenses and notices of the third-party components of the binaries and the web UI are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), generated by `make notices` in `nomad/` and shipped with every release archive and container image.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Every commit requires a DCO sign-off (`git commit -s`).
