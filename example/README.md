<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca deployment examples

Step-by-step setups of Norca with its new features, from a development
agent on a laptop to a federation of regions. Each scenario has its agent
configuration, the commands in order and what to look at in the web UI.
They share a small test application ([packs/test-app](packs/test-app/README.md))
and policy files ([policies](policies)); the operations packs (RustFS, NATS,
the LoadBalancer operator, the mTLS gateway) come from the
[Norca pack registry](#the-pack-registry).

| Scenario | Machines | Pick it to |
| --- | --- | --- |
| [01-local-dev](01-local-dev/README.md) | 1, `norca agent -dev` | try the features in minutes: new UI, catalog, DNS, Consul-compatible API, secrets, autoscaler; nothing persists |
| [02-single-node](02-single-node/README.md) | 1 (server + client) | run a small, secured installation: TLS, ACLs, gossip encryption, the `shares` seal, backups to RustFS, quotas, admission |
| [03-three-node](03-three-node/README.md) | 3 (server + client each) | run a highly available region: unseal per server, native mTLS and intentions, network rules, placement rules over datacenters, drains, operators, NATS |
| [04-multi-region](04-multi-region/README.md) | 3 per region | federate regions (one per cloud): ACL replication, multiregion deployments, cross-region mTLS gateway, NATS supercluster, multi-region pack installs |

Each scenario builds on the previous one; read 02 before 03 and 03 before 04.

## Prerequisites

| For | Needed |
| --- | --- |
| The Docker driver (every scenario) | Linux with Docker (01 also works on macOS with Docker) |
| Bridge networking (`network { mode = "bridge" }`, the default of the test app outside 01) | A root client and the [CNI reference plugins](https://github.com/containernetworking/plugins/releases) in `/opt/cni/bin` |
| Network rules (03) | Bridge networking and a kernel with nftables (`nf_tables`); the client sets the node attribute `norca.network_policy.nftables` |
| Native mTLS sidecars and gateways (03, 04) | Docker; the clients pull the Envoy image (`envoyproxy/envoy:v1.40.1` by default, configurable by node meta, see [native mTLS: sidecar and gateway](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/native-mtls/sidecar-and-gateway.md#envoy-image)) |
| Backups (02–04) | An S3-compatible store (RustFS with the `rustfs` pack, AWS S3, ...); [age](https://github.com/FiloSottile/age) for the backup key pair |
| The packs of the registry (02–04) | git and access to GitHub, or a checkout of the [Norca pack registry](https://github.com/bacanoo-io/norca-registry) |
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

On Windows, for the CLI and the web UI against these clusters (the clients
of the scenarios run Linux with Docker), `windows.bat` in the root of the
checkout builds `norca.exe` and `norca-natsbridge.exe` with the embedded UI
into `dist
orca_<version>_windows_amd64.zip` (`windows.bat skipui` keeps the
committed UI and needs no Node or Yarn).

## The pack registry

The operations packs are in the [Norca pack registry](https://github.com/bacanoo-io/norca-registry),
a separate repository: `rustfs` (the backup store of 02), `loadbalancer-operator`
and `nats` (03), `mtls-gateway` and `nats` (04), `openobserve` and `otel-collector`
(02–04; `postgres` for OpenObserve in cluster mode), and `consul`. Add it once on
the machine you run the CLI on, pinned to a release tag:

```shell
norca pack registry add norca https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
norca pack registry list
norca pack run nats --registry=norca --name=nats -f 03-three-node/nats.hcl
```

Pack commands without `--ref` use the packs at the ref the registry was added
with (here `v0.1.0`); without `--name` the release is named after the pack.

Without network access to GitHub, use a checkout of the registry by path
(`norca pack run <checkout>/packs/nats --name=nats -f ...`), or move the packs
into an internal OCI registry or an offline bundle
([supply-chain.md](../nomad/pack/docs/supply-chain.md)). The test application
stays in this repository ([packs/test-app](packs/test-app/README.md)).

### Cluster registries and the catalog

The registries above belong to the CLI of one machine. The cluster has its
own: **Packages → Registries** in the web UI adds pack registries to the
cluster (`packs.norca.io/Registry` objects: a git repository, an OCI
repository, a directory on the servers, an offline bundle, or `builtin` for
the packs that ship with Norca), which the leader fetches when they are
saved, on **Sync** and every `syncInterval`. **Packages → Catalog** lists
their packs with version, compatibility and README, and **Install** opens a
form built from the `ui`, `validation` and `rule` blocks of the pack's
variables; the leader renders the pack and plans or applies the release.
For the operations packs:

```shell
curl -X POST "$NOMAD_ADDR/v1/pack-registry" -H "X-Nomad-Token: $NOMAD_TOKEN"   -d '{"Name": "norca", "Spec": {"type": "git", "source": "https://github.com/bacanoo-io/norca-registry", "defaultRef": "v0.1.0", "syncInterval": "1h"}}'
```

A private git or OCI source takes `"authSecret": {"namespace": "default", "path": "<variable>"}`:
a Variable with `username` and `password` (or `token`), or `ssh_private_key`
and `known_hosts`. [01-local-dev](01-local-dev/README.md#6-install-from-the-catalog)
installs the test application from the catalog. With NATS (03, 04),
`norca-natsbridge -sync-pushes` syncs the registries when a pack release is
published and `-sync-regions` keeps the registries of the regions in step
([natsbridge README](../nomad/natsbridge/README.md#inbound-pack-registry-syncs)).

## Feature map

Only features marked "Available" in the [feature table](../README.md#feature-status)
are used, plus the embedded secret store, which is still in progress
overall: its phases 0–4 (the `shares` seal, versioned secrets with
`norca secret`, the `secret` block and Vault-style reads served by the
built-in store) are merged and used here.

| Feature | 01 | 02 | 03 | 04 |
| --- | --- | --- | --- | --- |
| New web UI | ✓ | ✓ | ✓ | ✓ |
| Native service catalog with health, topology-aware `discovery { prefer }` | ✓ | ✓ | ✓ (datacenters) | ✓ |
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
| Native mTLS: cross-region gateway (`mtls-gateway` pack) | | | | ✓ |
| Network rules: `NetworkPolicy`, `AdminNetworkPolicy` | | | ✓ | |
| Operator framework: object kind, objects, LoadBalancer operator pack | | | ✓ | |
| Packs of the [registry](#the-pack-registry): `rustfs`, `loadbalancer-operator`, `nats`, `mtls-gateway`, `openobserve`, `otel-collector` | | ✓ | ✓ | ✓ (supercluster, gateway) |
| Federation, ACL replication, multiregion deployment, `norca deployment run` | | | | ✓ |
| Pack releases (`pack run/status/history/uninstall`), variable `validation` and `rule` blocks | ✓ | ✓ | ✓ | ✓ (`--regions`) |
| Cluster pack registries, catalog and install form in the web UI | ✓ | ✓ | ✓ | ✓ |
| Observability: `openobserve`, `otel-collector` packs (metrics of the nodes and the platform, OTLP), agent `telemetry` | | ✓ (no TLS) | ✓ (Norca's certificates) | ✓ (per region) |

Not shown, with pointers:

- Admission **webhooks** (`server { admission { webhook ... } }`) and the
  webhooks of object kinds: they need a webhook service with a
  certificate; see [kubernetes-features-spec.md, item 2](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md#2-dynamic-admission-control)
  and the [loadbalancer-operator pack](https://github.com/bacanoo-io/norca-registry/blob/main/packs/loadbalancer-operator/README.md#webhooks).
- Native mTLS **app mode** (`mtls { mode = "app" }`): the application must
  load the workload certificate itself (Go library `api/mtls`).
- Full region restore with two-person approval and `restore-bootstrap`
  (mentioned in 02), keyring restore: [backup plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/backup-restore-plan.md).
- The `consul` pack (mentioned in 03): Consul is BSL 1.1 and is installed from
  HashiCorp's release after you accept its license.
- The event stream → NATS bridge (`bridge_enabled` of the `nats` pack, mentioned
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
norca pack render ./example/packs/test-app -f <vars.hcl>  # the validation and rule blocks and the templates
norca pack render rustfs --registry=norca -f <vars.hcl>   # the same for a pack of the registry
norca job validate <job file>                             # without an agent: structural checks only
```

`norca config validate` checks the syntax, the known keys and the values,
including those of the Norca blocks (`keyring`, `backup`, `autoscaler`,
`mtls`, `admission`, `secrets`, `dns`, `consul_compat`) as the agent checks
them when it starts; the placeholders (`CHANGE_ME`) must be replaced first.
Without a reachable agent `norca job validate` skips the server-side
checks (admission, mTLS injection, typed scaling policies), so a job with
only a gateway service (no task, like `04-multi-region/mtls-gateway.nomad.hcl`
or the rendered `mtls-gateway` pack) fails there although the servers accept
it.
