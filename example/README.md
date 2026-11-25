<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca deployment examples

Step-by-step setups of Norca and its features, from a development
agent on a laptop to a federation of regions. Each scenario has its agent
configuration, the commands in order and what to look at in the web UI.
They share a small test application ([modules/test-app](modules/test-app/README.md))
and policy files ([policies](policies)). The test application is a module of
Rigger, the package manager of Norca; the operations modules (RustFS, the
LoadBalancer operator, the mTLS gateway) come from the
[global registry](#the-module-registry) of Rigger.

| Scenario | Machines | Pick it to |
| --- | --- | --- |
| [01-local-dev](01-local-dev/README.md) | 1, `norca agent -dev` | try the features in minutes: web UI, catalog, DNS, Consul-compatible API and KV, secrets, autoscaler; nothing persists |
| [02-single-node](02-single-node/README.md) | 1 (server + client) | run a small, secured installation: TLS, ACLs, gossip encryption, the `shares` seal, backups to RustFS, quotas, admission |
| [03-three-node](03-three-node/README.md) | 3 (server + client each) | run a highly available region: unseal per server, native mTLS and intentions, network rules, placement rules over datacenters, drains, operators |
| [04-multi-region](04-multi-region/README.md) | 3 per region | federate regions (one per cloud): ACL and secret replication, multiregion deployments, cross-region mTLS gateway, a release of a module per region |

Each scenario builds on the previous one; read 02 before 03 and 03 before 04.

## Prerequisites

| For | Needed |
| --- | --- |
| The Docker driver (every scenario) | Linux with Docker (01 also works on macOS with Docker) |
| Bridge networking (`network { mode = "bridge" }`, the default of the test app outside 01) | A root client and the [CNI reference plugins](https://github.com/containernetworking/plugins/releases) in `/opt/cni/bin` |
| Network rules (03) | Bridge networking and a kernel with nftables (`nf_tables`); the client sets the node attribute `norca.network_policy.nftables` |
| Native mTLS sidecars and gateways (03, 04) | Docker; the clients pull the Envoy image (`envoyproxy/envoy:v1.40.1` by default, configurable by node meta, see [native mTLS: sidecar and gateway](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/native-mtls/sidecar-and-gateway.md#envoy-image)) |
| Clusters of several nodes (03, 04) | The system bus of the servers (on by default): clients reach the servers on 4649, the servers each other on 4650, next to 4646–4648. The autoscaler and the usage history get their data over it |
| Backups (02–04) | An S3-compatible store (RustFS with the `rustfs` module, AWS S3, ...); [age](https://github.com/FiloSottile/age) for the backup key pair |
| The modules of the registry (02–04) | Access to GitHub from the servers, which fetch the global registry and the modules they install; or a checkout of the [Norca registry](https://github.com/bacanoo-io/norca-registry) |
| The LoadBalancer operator (03) | Go and a checkout of [norca-platform](https://github.com/bacanoo-io/norca-platform) next to this repository, to build `norca-loadbalancer` (it runs with the `exec` driver) |
| Multi region (04) | A network between the regions (e.g. a WireGuard VPN) |

Secrets, credentials and keys in these files are placeholders (`CHANGE_ME`).
Never commit real ones.

## Getting the norca binary

Norca is one binary, `norca`, for the agent and the CLI. Build it with the
web UI embedded (the `ui` build tag; the UI assets are committed in
`norca/command/agent/bindata_assetfs.go`):

```shell
cd norca
make dev                          # bin/norca, also copied to $GOBIN
# or: go build -tags ui -o norca .
sudo install bin/norca /usr/local/bin/norca
norca version                     # Norca v0.3.0 (Nomad API 1.6.5 compatible)
```

Without `-tags ui` the binary works the same but serves no web UI.

On Windows, for the CLI and the web UI against these clusters (the clients
of the scenarios run Linux with Docker), `windows.bat` in the root of the
checkout builds `norca.exe` with the embedded UI
into `dist\norca_<version>_windows_amd64.zip` (`windows.bat skipui` keeps the
committed UI and needs no Node or Yarn).

## The module registry

Rigger installs modules: a module is a directory of HCL files (its
variables, its jobs and other resources, its tests). The test application is
one in this repository ([modules/test-app](modules/test-app/README.md)), and
`norca rigger install` installs a module of a directory as a release:

```shell
norca rigger install modules/test-app -name test-app -f 01-local-dev/test-app.hcl
norca rigger release list
norca rigger release status test-app
```

The operations modules are in the [Norca registry](https://github.com/bacanoo-io/norca-registry),
a separate repository, and the [global registry](https://github.com/bacanoo-io/norca-global-registry)
of Rigger lists their versions as `norca/<name>`: `rustfs` (the backup store
of 02), `loadbalancer-operator` (03), `mtls-gateway` (04), `openobserve` and
`otel-collector` (02–04; `postgres` for OpenObserve in cluster mode), and
`consul`. `norca rigger install` takes a directory. One module of a registry
is installed by its name by the servers, from a registry they have synced
(a [stack](#stacks) is the other way). Nothing syncs on its own: sync the
global registry once in every region, then send the install to the servers:

```shell
norca rigger registry sync global
norca rigger registry search global rustfs -kind module

norca operator api -X PUT /v1/rigger/install <<'EOF'
{"Release": "rustfs", "Registry": "global", "Module": "norca/rustfs", "Version": "~> 0.2",
 "ValuesJSON": {"buckets": ["norca-backup"], "console_enabled": true}}
EOF
norca rigger release status rustfs
```

The servers take the newest version of the last sync that meets `Version`
(the newest of all without it), fetch it from its repository and refuse it
unless it has the tree digest the registry lists and a signature by a key of
its namespace. `ValuesJSON` holds the values of the module's variables. The
release is in the namespace and the region of the request (`-namespace` and
`-region` of `norca operator api`). The request waits until the release is
ready and answers with its action (`install`, `upgrade` or `unchanged`) and
its revision; the same request with other values upgrades the release.
`norca operator api` prints the answer and ends with 0 also when the answer
is an error. See the
[install API](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/api/rigger-install.md)
and the [registries](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/api/rigger-registries.md).

Without access to GitHub from the servers, install from a checkout of the
Norca registry by path
(`norca rigger install <checkout>/modules/rustfs -name rustfs -f 02-single-node/rustfs.hcl`),
or point the registry `global` at a mirror.

### Stacks

A stack is a file that names several modules with their values; Rigger
installs them as releases, in the region of the stack, and keeps their
versions in a lock file. A module of the global registry is named by its
source:

```hcl
# stack.hcl
rigger {
  name   = "operations"
  region = "global" # the region of 02 and 03
}

module "rustfs" {
  source  = "norca/rustfs"
  version = "~> 0.2"

  values = {
    buckets         = ["norca-backup"]
    console_enabled = true
  }
}

module "openobserve" {
  source    = "norca/openobserve"
  version   = "~> 0.2"
  namespace = "observability"

  values = {
    tls_mode = "none"
  }
}
```

```shell
norca rigger stack sync .     # resolve the versions, fetch and verify the modules, write rigger.lock.hcl
norca rigger stack plan .     # what an apply would change
norca rigger stack apply .    # install or upgrade, release after release
```

Here the program fetches the modules on the machine it runs on, which needs
access to GitHub. The scenarios install each module on its own, at the step
that needs it. See [rigger/README.md](../rigger/README.md) and
[rigger/examples](../rigger/examples/README.md).

### The catalog in the web UI

**Packages → Registries** lists the registries of the cluster
(`rigger.norca.io/Registry` objects): `global`, which the servers create,
and the git repositories of modules that you add (**Add registry**; a
private one takes the Variable with its credentials), each with its last
sync and a **Sync** button. **Packages → Catalog** lists the modules and
stacks of a registry at its last sync; `global` shows only what was
selected for it (`norca rigger registry select`) until *The whole index* is
switched on. **Install** opens a form built from the variables of the
module (its `ui` and `validation` blocks); **Plan** shows what an install
would change, and **Install** sends the request above. The release has an
**Upgrade** with the same form. See the
[Rigger catalog](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/packages/rigger-catalog.md);
[01-local-dev](01-local-dev/README.md#7-the-catalog-of-the-web-ui) tries it.

## Feature map

Only available features are used; see the [feature table](../README.md#feature-status).

| Feature | 01 | 02 | 03 | 04 |
| --- | --- | --- | --- | --- |
| Web UI | ✓ | ✓ | ✓ | ✓ |
| Native service catalog with health, topology-aware `discovery { prefer }` | ✓ | ✓ | ✓ (datacenters) | ✓ |
| Built-in DNS | ✓ | ✓ | ✓ | ✓ |
| Consul-compatible API and KV | ✓ | | | |
| External services, DNS alias (`norca service register`) | ✓ | | | |
| Health check roles (startup, readiness, liveness) | ✓ | ✓ | ✓ | ✓ |
| Built-in autoscaler, typed scaling policy | ✓ | ✓ | ✓ | |
| Secret store: `secret` block, `secret_access`, Vault-style template reads, audit | ✓ | ✓ | ✓ | ✓ (per region) |
| Replication of secrets between regions (`norca secret put -replicate`) | | | | ✓ |
| Keyring `shares` seal, `rewrap`, `unseal` | | ✓ | ✓ (per server) | ✓ (per region) |
| TLS, ACLs, gossip encryption | | ✓ | ✓ | ✓ |
| Namespace quotas (`task_defaults`, `task_max`, `allocations`) | | ✓ | ✓ | ✓ (per region limits) |
| Admission policies (CEL, incl. secret references) | | ✓ | ✓ | ✓ |
| Disruption budget, eviction (`alloc stop -respect-disruption`), drains | | ✓ | ✓ | |
| Backups: schedule, age encryption, browse, selective restore | | ✓ | ✓ | ✓ (per region) |
| Cross-job affinity (`separate`, `colocate`) | | | ✓ | ✓ |
| Native mTLS: sidecar, upstreams, `ServiceIntention` | | | ✓ | ✓ |
| Native mTLS: cross-region gateway (`mtls-gateway` module) | | | | ✓ |
| Network rules: `NetworkPolicy`, `AdminNetworkPolicy` | | | ✓ | |
| Operator framework: object kind, objects, LoadBalancer operator module | | | ✓ | |
| Modules of the [global registry](#the-module-registry): `rustfs`, `loadbalancer-operator`, `mtls-gateway`, `openobserve`, `otel-collector` | | ✓ | ✓ | ✓ (gateway) |
| System bus (`norca operator bus status`), usage history in the metrics store | ✓ | ✓ | ✓ | ✓ (per region) |
| Federation, ACL replication, multiregion deployment, `norca deployment run` | | | | ✓ |
| Releases of Rigger (`rigger install`, `rigger release status/history/uninstall`), variable `validation` and `rule` blocks | ✓ | ✓ | ✓ | ✓ (per region) |
| Registries of the cluster, catalog and install form in the web UI | ✓ | ✓ | ✓ | ✓ (per region) |
| Observability: `openobserve`, `otel-collector` modules (metrics of the nodes and the platform, OTLP), agent `telemetry` | | ✓ (no TLS) | ✓ (Norca's certificates) | ✓ (per region) |

Not shown, with pointers:

- Admission **webhooks** (`server { admission { webhook ... } }`) and the
  webhooks of object kinds: they need a webhook service with a
  certificate; see [kubernetes-features-spec.md, item 2](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/kubernetes-features-spec.md#2-dynamic-admission-control)
  and the [loadbalancer-operator module](https://github.com/bacanoo-io/norca-registry/blob/main/modules/loadbalancer-operator/README.md#webhooks).
- Native mTLS **app mode** (`mtls { mode = "app" }`): the application must
  load the workload certificate itself (Go library `api/mtls`).
- Full region restore with two-person approval and `restore-bootstrap`
  (mentioned in 02), keyring restore: [backup plan](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/design/backup-restore-plan.md).
- The `consul` module (mentioned in 03): Consul is BSL 1.1 and is installed from
  HashiCorp's release after you accept its license.
- Consumers of the [system bus](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/system-bus.md)
  (the event stream in `NORCA_EVENTS_<region>`) and the Variables topic of the
  event stream.
- Writing, testing, signing and publishing modules and stacks, registries
  of your own, trust and lock files, bundles for a cluster without a
  network: [rigger/README.md](../rigger/README.md),
  [rigger/examples](../rigger/examples/README.md) and the
  [stacks API](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/api/rigger-stacks.md).
- `norca secret import -from-vault` / `-from-variable`, task `vault` blocks
  served by the client's Vault-compatible endpoint (`client { secrets { vault_compat } }`).
- Expiry and rotation of secrets (`-expires-at`, `-rotate-every`, rotation jobs) and
  [dynamic secrets](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/features/secrets/dynamic.md)
  (NATS credentials from the built-in engine).
- Node pool scaling with cloud providers (postponed) and the secret store's
  external providers and KMS seals (not implemented yet).

## Checking the files offline

```shell
norca config validate <dir with the agent files>                    # syntax and keys
norca rigger module validate ./example/modules/test-app              # the files and the tests of the module
norca rigger module render ./example/modules/test-app -f <values.hcl>   # the validation and rule blocks; the resources as JSON
norca rigger module render <checkout>/modules/rustfs -f <values.hcl>    # the same for a module of a checkout of the Norca registry
norca job validate <job file>                                        # without an agent: structural checks only
```

`norca config validate` checks the syntax, the known keys and the values,
including those of the Norca blocks (`keyring`, `backup`, `autoscaler`,
`mtls`, `admission`, `secrets`, `bus`, `dns`, `consul_compat`) as the agent checks
them when it starts; the placeholders (`CHANGE_ME`) must be replaced first.
Without a reachable agent `norca job validate` skips the server-side
checks (admission, mTLS injection, typed scaling policies), so a job with
only a gateway service (no task, like `04-multi-region/mtls-gateway.norca.hcl`)
fails there although the servers accept it.
