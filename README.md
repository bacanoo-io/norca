# Norca

An independent, open source workload scheduler and orchestrator built on the **HashiCorp Nomad v1.6.5** source code (MPL 2.0), with its own extensions: a built-in autoscaler, namespace quotas, admission policies, backup and restore, service discovery without Consul, package management and a new web UI.

**This project is not a product of HashiCorp or IBM and is not affiliated with them.** "Nomad", "Consul" and "HashiCorp" are trademarks of their respective owners; they appear here only to describe the origin of the code.

## Origin

| Component | Based on | Commit | License |
| --- | --- | --- | --- |
| `nomad/` | HashiCorp Nomad v1.6.5 | `a7cfff372cd52b02232a02bd3ffb5e5d35f88a0f` | MPL 2.0 |
| `nomad/pack/` | HashiCorp Nomad Pack v0.4.2 | `cd4d2a41c22adba1350b8d5da63252f2dde33262` | MPL 2.0 |
| built-in autoscaler (`nomad/nomad/autoscaler/`) | HashiCorp Nomad Autoscaler v0.6.0 | `a24707db8f2abd3ee74ce0cb5cc0d2b699d2cace` | MPL 2.0 |

v1.6.5 is the last Nomad release published under MPL 2.0. `nomad/` and Nomad Pack were imported with their full git history, so the starting commits can be verified; Nomad Pack was later moved into `nomad/pack/` (part of the Nomad Go module, run as `norca pack`); its original commits remain in the history and `git blame` still reaches them. This project contains no code from HashiCorp releases published under BSL 1.1. Details: [docs/licensing.md](docs/licensing.md).

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
| Dependency updates (Go 1.27, gRPC 1.84, security fixes) | Available | [development plan](docs/design/nomad-fork-development-plan.md) |
| Built-in autoscaler (leader subsystem; built-in strategies; `nomad` metrics pushed by clients and Prometheus; external APM/strategy plugins; typed, validated `scaling` policies; quota and deployment aware; `Scaling` events; off by default) | Available | [development plan](docs/design/nomad-fork-development-plan.md) |
| Built-in autoscaler: node pool scaling with cloud providers (phase 4) | Postponed | [development plan](docs/design/nomad-fork-development-plan.md) |
| Namespace quotas | Available | [specification, item 1](docs/design/kubernetes-features-spec.md) |
| Dynamic admission control (CEL policies, webhooks) | Available | [specification, item 2](docs/design/kubernetes-features-spec.md) |
| Event stream: Variables topic, resumable subscriptions | Available | [specification, item 3](docs/design/kubernetes-features-spec.md) |
| Cross-job affinity (`colocate`, `separate`) | Available | [specification, item 4](docs/design/kubernetes-features-spec.md) |
| Disruption budget | Available | [specification, item 5](docs/design/kubernetes-features-spec.md) |
| Health check roles (startup, readiness, liveness) | Available | [specification, item 6](docs/design/kubernetes-features-spec.md) |
| Network rules | Planned | [specification, item 7](docs/design/kubernetes-features-spec.md) |
| Operator framework: native custom objects (`ObjectKind`, `CustomObject`, schema, CEL with `oldSelf`), controller SDK `nomad/controller`, LoadBalancer example; lifecycle: finalizers, deletion modes (background, foreground, orphan), garbage collection of owned objects and jobs, per-object event log, managed-job protection, `scale` subresource for the autoscaler | Available (phases 1–3) | [specification, item 8](docs/design/kubernetes-features-spec.md) |
| Event stream → NATS bridge (`nomad/natsbridge`, one job per region) | Available | [development plan](docs/design/nomad-fork-development-plan.md) |
| Multiregion deployments (one region per cloud; staged rollout, `on_failure`, global tokens, `norca deployment run`) | Available | [development plan](docs/design/nomad-fork-development-plan.md) |
| NATS pack (`packs/nats`: supercluster with JetStream and gateways, optional bridge) | Available | [packs/nats](packs/nats/README.md) |
| Backup and restore: scheduled, age-encrypted backups to S3/MinIO, browsing and selective restore (`norca operator backup`) | Available (phases 1–2) | [plan](docs/design/backup-restore-plan.md) |
| Backup and restore: full region restore with preview and two-person approval, keyring restore, `restore-bootstrap` for an empty cluster | Available (phase 4) | [plan](docs/design/backup-restore-plan.md) |
| Backup and restore: web UI | Planned | [plan](docs/design/backup-restore-plan.md) |
| Service discovery without Consul: health-based catalog, built-in DNS, Consul-compatible read layer for Traefik, Prometheus and consul-template (DNS and the compat layer are off by default) | Available (phases 1–3) | [plan](docs/design/consul-independence-plan.md) |
| Service discovery without Consul: external services (address or DNS alias, TTL/HTTP/TCP checks, `norca service register`), Consul agent write endpoints in the compat layer | Available (phase 4) | [plan](docs/design/consul-independence-plan.md) |
| Service discovery without Consul: native mTLS, managed Consul install | Planned | [plan](docs/design/consul-independence-plan.md) |
| Package management: Nomad Pack built in (`norca pack`) | Available | [plan](docs/design/package-management-plan.md) |
| Package management, client-side supply chain: OCI registries, cosign-compatible signing and trust policies, offline bundles (air-gapped), `pack.lock` | Available | [plan](docs/design/package-management-plan.md), [usage](nomad/pack/docs/supply-chain.md) |
| Package management: server-side releases, rollback, signature enforcement in admission | Planned | [plan](docs/design/package-management-plan.md) |
| New web UI | Planned | [plan](docs/design/ui-plan.md) |

Only features marked "Available" can be used; the others are being built or are in the design phase.

## License

The code and documentation of this project are licensed under the **MPL 2.0** ([LICENSES/MPL-2.0.txt](LICENSES/MPL-2.0.txt)). Files taken from third parties keep their original license; these are marked by their file headers and by [REUSE.toml](REUSE.toml). The licenses and notices of the third-party components of the binaries and the web UI are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), generated by `make notices` in `nomad/` and shipped with every release archive and container image.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Every commit requires a DCO sign-off (`git commit -s`).
