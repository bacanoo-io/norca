<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca deployment examples

Step-by-step setups of Norca with its new features, from a development
agent on a laptop to a federation of regions. Each scenario has its agent
configuration, the commands in order and what to look at in the web UI.
They share a small test application ([packs/test-app](packs/test-app/README.md))
and policy files ([policies](policies)).

| Scenario | Machines | Pick it to |
| --- | --- | --- |
| [01-local-dev](01-local-dev/README.md) | 1, `norca agent -dev` | try the features in minutes: new UI, catalog, DNS, Consul-compatible API, secrets, autoscaler; nothing persists |
| [02-single-node](02-single-node/README.md) | 1 (server + client) | run a small, secured installation: TLS, ACLs, gossip encryption, the `shares` seal, backups to MinIO, quotas, admission |
| [03-three-node](03-three-node/README.md) | 3 (server + client each) | run a highly available region: unseal per server, native mTLS and intentions, network rules, placement rules over zones, drains, operators, NATS |
| [04-multi-region](04-multi-region/README.md) | 3 per region | federate regions (one per cloud): ACL replication, multiregion deployments, cross-region mTLS gateway, NATS supercluster, multi-region pack installs |

Each scenario builds on the previous one; read 02 before 03 and 03 before 04.

## Prerequisites

| For | Needed |
| --- | --- |
| The Docker driver (every scenario) | Linux with Docker (01 also works on macOS with Docker) |
| Bridge networking (`network { mode = "bridge" }`, the default of the test app outside 01) | A root client and the [CNI reference plugins](https://github.com/containernetworking/plugins/releases) in `/opt/cni/bin` |
| Network rules (03) | Bridge networking and a kernel with nftables (`nf_tables`); the client sets the node attribute `norca.network_policy.nftables` |
| Native mTLS sidecars and gateways (03, 04) | Docker; the clients pull the Envoy image (`envoyproxy/envoy:v1.40.1` by default, configurable by node meta, see [docs/native-mtls.md](../docs/native-mtls.md#envoy-image)) |
| Backups (02–04) | An S3-compatible store (MinIO, AWS S3, ...); [age](https://github.com/FiloSottile/age) for the backup key pair |
| The LoadBalancer operator (03) | Go, to build `norca-loadbalancer`, and the `raw_exec` driver |
| Multi region (04) | A network between the regions (e.g. a WireGuard VPN) |

Secrets, credentials and keys in these files are placeholders (`CHANGE_ME`).
Never commit real ones.

## Getting the norca binary

Norca is one binary, `norca`, for the agent and the CLI. Build it with the
web UI embedded (the `ui` build tag; the UI assets are committed in
`nomad/command/agent/bindata_assetfs.go`):

```shell
cd nomad
make dev                          # bin/norca, also copied to $GOBIN
# or: go build -tags ui -o norca .
sudo install bin/norca /usr/local/bin/norca
norca version                     # Norca v0.1.0 (Nomad API 1.6.5 compatible)
```

Without `-tags ui` the binary works the same but serves no web UI.

## Feature map

Only features marked "Available" in the [feature table](../README.md#feature-status)
are used, plus the embedded secret store, which is still in progress
overall: its phases 0–4 (the `shares` seal, versioned secrets with
`norca secret`, the `secret` block and Vault-style reads served by the
built-in store) are merged and used here.

| Feature | 01 | 02 | 03 | 04 |
| --- | --- | --- | --- | --- |
| New web UI | ✓ | ✓ | ✓ | ✓ |
| Native service catalog with health, topology-aware `discovery { prefer }` | ✓ | ✓ | ✓ (zones) | ✓ |
| Built-in DNS | ✓ | ✓ | ✓ | ✓ |
| Consul-compatible API | ✓ | | | |
| External services, DNS alias (`norca service register`) | ✓ | | | |
| Health check roles (startup, readiness, liveness) | ✓ | ✓ | ✓ | ✓ |
| Built-in autoscaler, typed scaling policy | ✓ | ✓ | ✓ | |
| Secret store: `secret` block, `secret_access`, Vault-style template reads, audit | ✓ | ✓ | ✓ | ✓ (per region) |
| Keyring `shares` seal, `rewrap`, `unseal` | | ✓ | ✓ (per server) | ✓ (per region) |
| TLS, ACLs, gossip encryption | | ✓ | ✓ | ✓ |
| Namespace quotas (`task_defaults`, `task_max`, `allocations`) | | ✓ | ✓ | ✓ (per region limits) |
| Admission policies (CEL, incl. secret references) | | ✓ | ✓ | ✓ |
| Disruption budget, eviction (`alloc stop -respect-disruption`), drains | | ✓ | ✓ | |
| Backups: schedule, age encryption, browse, selective restore | | ✓ | ✓ | ✓ (per region) |
| Cross-job affinity (`separate`, `colocate`) | | | ✓ | ✓ |
| Native mTLS: sidecar, upstreams, `ServiceIntention` | | | ✓ | ✓ |
| Native mTLS: cross-region gateway | | | | ✓ |
| Network rules: `NetworkPolicy`, `AdminNetworkPolicy` | | | ✓ | |
| Operator framework: object kind, objects, LoadBalancer operator pack | | | ✓ | |
| `packs/nats` | | | ✓ | ✓ (supercluster) |
| Federation, ACL replication, multiregion deployment, `norca deployment run` | | | | ✓ |
| Pack releases (`pack run/status/history/uninstall`), `variables_schema` | ✓ | ✓ | ✓ | ✓ (`--regions`) |

Not shown, with pointers:

- Admission **webhooks** (`server { admission { webhook ... } }`) and the
  webhooks of object kinds: they need a webhook service with a
  certificate; see [kubernetes-features-spec.md, item 2](../docs/design/kubernetes-features-spec.md#2-dynamic-admission-control)
  and [packs/loadbalancer-operator](../packs/loadbalancer-operator/README.md#webhooks).
- Native mTLS **app mode** (`mtls { mode = "app" }`): the application must
  load the workload certificate itself (Go library `api/mtls`).
- Full region restore with two-person approval and `restore-bootstrap`
  (mentioned in 02), keyring restore: [backup plan](../docs/design/backup-restore-plan.md).
- `packs/consul` (mentioned in 03): Consul is BSL 1.1 and is installed from
  HashiCorp's release after you accept its license.
- The event stream → NATS bridge (`bridge_enabled` of packs/nats, mentioned
  in 04) and the Variables topic of the event stream.
- Pack supply chain (OCI registries, signatures, trust policies, bundles,
  `pack.lock`) and signature enforcement in admission:
  [supply-chain.md](../nomad/pack/docs/supply-chain.md), [releases.md](../nomad/pack/docs/releases.md).
- `norca secret import -from-vault` / `-from-variable`, task `vault` blocks
  served by the client's Vault-compatible endpoint (`client { secrets { vault_compat } }`).
- Node pool scaling with cloud providers (postponed) and the secret store's
  external providers and KMS seals (not implemented yet).

## Checking the files offline

```shell
norca config validate <dir with the agent files>         # syntax and keys
norca pack render ./example/packs/test-app -f <vars.hcl>  # the variables schema and the templates
norca job validate <job file>                             # without an agent: structural checks only
```

`norca config validate` checks the syntax and the known keys; the values of
the `keyring`, `backup` and `autoscaler` blocks are checked when the agent
starts. Without a reachable agent `norca job validate` skips the server-side
checks (admission, mTLS injection, typed scaling policies), so a job with
only a gateway service (no task, like `04-multi-region/mtls-gateway.nomad.hcl`)
fails there although the servers accept it.
