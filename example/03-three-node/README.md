<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# 03 – Three nodes

A highly available region on three Linux machines. Each machine runs a
**server and a client** in one agent: three servers are the smallest Raft
quorum that survives the loss of one, and three clients are needed for the
placement rules (one instance per zone), drains under a disruption budget
and native mTLS between nodes. Combining both roles keeps the example at
three machines; in production run the servers on their own machines and the
workloads on separate clients (same configuration, split into a `server`
and a `client` part).

On top of [02-single-node](../02-single-node/README.md) this shows: joining
servers, unsealing every server, native mTLS with an Envoy sidecar and
service intentions, network rules, cross-job affinity and anti-affinity
over zones, the disruption budget during a drain, the operator framework
with the LoadBalancer operator, and the NATS pack.

## Prerequisites

- Three Linux machines (`10.0.0.11`–`10.0.0.13` below; replace them) that
  reach each other on ports 4646–4648 and on the dynamic port range
  (20000–32000), with Docker, the CNI plugins in `/opt/cni/bin` and a
  kernel with **nftables** (`nf_tables`; the agent programs the rules over
  netlink, the `nft` tool is optional). Nodes that can enforce network rules
  get the node attribute `norca.network_policy.nftables = true`.
- The Envoy image (`envoyproxy/envoy:v1.40.1` by default, see
  [docs/native-mtls.md](../../docs/native-mtls.md#envoy-image)) is pulled by
  the clients for the sidecars.
- An S3-compatible store outside the cluster for the backups, and
  `age-keygen`.
- `norca` in `/usr/local/bin` on every machine; for the LoadBalancer
  operator also `norca-loadbalancer` (step 9).

## Files

| File | |
| --- | --- |
| [norca.hcl](norca.hcl) | Shared agent configuration (server and client) of the three nodes |
| [node-1.hcl](node-1.hcl), [node-2.hcl](node-2.hcl), [node-3.hcl](node-3.hcl) | Name, addresses and zone of each node, installed as `/etc/norca.d/node.hcl` |
| [test-app.hcl](test-app.hcl) | test-app pack: 3 instances spread over the zones, autoscaled up to 6 |
| [test-api.hcl](test-api.hcl) | test-app pack as `test-api`, behind an mTLS sidecar |
| [probe.nomad.hcl](probe.nomad.hcl) | A client of `test-api` through an mTLS upstream, colocated with test-app |
| [lb-test-app.hcl](lb-test-app.hcl) | A `norca.io/LoadBalancer` object for test-app |
| [nats.hcl](nats.hcl) | Variables of the [nats pack](https://github.com/bacanoo-io/norca-registry/blob/main/packs/nats/README.md) |
| [../02-single-node/norca.service](../02-single-node/norca.service) | systemd unit |

## Steps

### 1. Certificates and keys (on an admin machine)

```shell
mkdir tls && cd tls
norca tls ca create
for i in 1 2 3; do
  mkdir node-$i
  (cd node-$i && norca tls cert create -server -ca ../nomad-agent-ca.pem \
     -key ../nomad-agent-ca-key.pem -additional-ipaddress 10.0.0.1$i)
done
norca operator gossip keyring generate       # server.encrypt, the same on every node
age-keygen -o ~/backup-identity.txt          # public key into backup.encryption.recipients
```

Every server certificate is valid for `server.global.nomad`, `localhost`
and the address of its node.

### 2. Install and start every node

On node `n` (1, 2, 3):

```shell
sudo mkdir -p /etc/norca.d/tls /opt/norca/data /var/log/norca
sudo cp tls/nomad-agent-ca.pem tls/node-n/global-server-nomad*.pem /etc/norca.d/tls/
sudo cp norca.hcl /etc/norca.d/norca.hcl         # replace every CHANGE_ME
sudo cp node-n.hcl /etc/norca.d/node.hcl
sudo install -m 600 ../02-single-node/backup-credentials.example /etc/norca.d/backup-credentials   # CHANGE_ME
norca config validate /etc/norca.d/
sudo cp ../02-single-node/norca.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now norca
```

The servers find each other through `server_join.retry_join` and elect a
leader once all three (`bootstrap_expect = 3`) are up.

```shell
export NORCA_ADDR=https://10.0.0.11:4646
export NORCA_CACERT=$PWD/tls/nomad-agent-ca.pem
norca server members
```

### 3. ACLs and the seal

```shell
norca acl bootstrap
export NORCA_TOKEN=<Secret ID of the bootstrap token>

# Initializes the keyring of the region on the leader and prints the five
# shares once. The other two servers stay sealed.
norca operator keyring rewrap -to shares
norca operator seal-status                       # names of the sealed servers

# Unseal every other server with three shares (prompts without echo).
norca operator unseal -server-id <name of server 2>   # three times
norca operator unseal -server-id <name of server 3>   # three times
norca operator seal-status
```

After a restart a server is sealed again and needs three shares; the region
keeps working while the other two servers are unsealed.

### 4. Namespace, quota, policies

```shell
norca quota apply ../policies/quota-apps.hcl
norca namespace apply ../policies/namespace-apps.hcl
norca admission-policy apply ../policies/admission-apps.hcl
norca acl policy apply -description "Developers of apps" app-developer ../policies/acl-app-developer.hcl

# Network rules: the cluster-wide deny of the metadata service and the
# egress policy of test-app (set the node subnet in the file first).
norca network-policy apply -admin ../policies/admin-netpol-no-metadata.hcl
norca network-policy apply ../policies/netpol-test-app.hcl
```

### 5. Deploy test-app

```shell
norca secret put -namespace apps test-app/db username=app password=-
norca pack run ../packs/test-app --name=test-app --namespace=apps -f test-app.hcl

norca job status -namespace apps test-app             # one instance per zone, disruption budget
norca service info -namespace apps -verbose test-app  # zones and health of the instances
norca network-policy status -namespace apps test-app  # selected allocations, enforced on their nodes
```

Topology-aware discovery (`discovery { prefer = "zone", min_local = 2 }`): on
a node, the built-in DNS answers with the instances of the node's own zone
first, and adds the rest of the datacenter while the zone has fewer than two
healthy instances (or fewer than 20% of all, `overflow_fraction`). The job
doesn't set `overflow_fraction`, so the servers fill in their
`native_service_discovery { default_overflow_fraction }` (0.2, `norca.hcl`) at
registration; `norca job inspect` shows the resolved value, and after changing
the setting (reloaded on SIGHUP) `norca job plan` shows the job's value
changing at its next submission. With three
instances (one per zone) every answer holds all three; scale to six to see
the zone filter:

```shell
norca job scale -namespace apps test-app 6
dig @127.0.0.1 -p 8600 test-app.service.apps.nomad SRV   # on node-1: the two zone-a instances
norca alloc stop -namespace apps <one zone-a alloc ID>   # while it is replaced: zone-a + rest of dc1
norca job scale -namespace apps test-app 3
```

### 6. Native mTLS: test-api, probe and intentions

```shell
norca pack run ../packs/test-app --name=test-api --namespace=apps -f test-api.hcl
norca job run probe.nomad.hcl

# Without an intention the connection is denied:
norca alloc logs -namespace apps -task probe <probe alloc ID>    # "test-api: no connection"
norca service intention check -namespace apps probe test-api      # exit code 2: denied

# Allow probe -> test-api; applies to new connections within about a second.
norca service intention create -namespace apps -description "probe reads test-api" probe test-api
norca service intention list -namespace apps
norca alloc logs -namespace apps -task probe <probe alloc ID>    # "<h1>test-api</h1>"

# The trust bundle of the region (public CA certificates)
norca operator api /v1/mtls/roots
```

`test-api` is registered with the address of its sidecar: whoever reaches it
through the catalog or DNS needs a workload certificate and an intention.
See [docs/native-mtls.md](../../docs/native-mtls.md) for the sidecar, the
upstreams and the limits (TCP only).

### 7. Disruption budget and drains

```shell
norca node status
norca node drain -enable -deadline 15m -yes <node ID>
norca job status -namespace apps test-app    # never more than one test-app instance down
norca node drain -disable -yes <node ID>
norca node eligibility -enable <node ID>
```

### 8. Autoscaler

```shell
norca scaling policy list -namespace apps
norca scaling policy info -namespace apps <policy ID>
norca job scaling-events -namespace apps test-app
```

The built-in autoscaler on the leader keeps the average CPU usage of the
test-app instances near 70% of their reservation, between 3 and 6
instances, within the quota of the namespace.

### 9. Operator framework: the LoadBalancer operator

```shell
# Build the operator binary (it is also the TCP proxy of the "nomad" class)
# and install it on every node. The pack is loadbalancer-operator of the
# Norca pack registry (see ../README.md#the-pack-registry).
(cd ../../nomad/controller && go build -o norca-loadbalancer ./examples/loadbalancer)
sudo install ../../nomad/controller/norca-loadbalancer /usr/local/bin/   # on every node

# One release: object kind, ACL policy bound to the controller job,
# admission policy and the controller job (two instances, leader election).
norca pack run loadbalancer-operator --registry=norca --name=lb --namespace=apps --var namespace=apps
norca pack history lb --namespace=apps

norca object-kind list
norca object apply lb-test-app.hcl
norca object list norca.io/LoadBalancer -namespace apps
norca object events norca.io/LoadBalancer test-app -namespace apps
curl http://<address from the object status>:8080/   # through the proxy job of the load balancer
```

### 10. NATS (optional)

```shell
norca pack run nats --registry=norca --name=nats -f nats.hcl
norca service info nats-monitor
```

Three NATS servers, one per zone, with JetStream and the disruption budget
of the pack. Full Consul is available the same way with
the [consul pack](https://github.com/bacanoo-io/norca-registry/blob/main/packs/consul/README.md) (Consul is BSL 1.1 and is
installed from HashiCorp's releases after you accept its license); service
discovery, DNS and mTLS above don't need it.

### 11. Backups

```shell
norca operator backup save -label note=after-setup
norca operator backup list                           # the schedule and the backups of the region
norca operator backup contents -age-identity ~/backup-identity.txt -type secrets -namespace apps <backup ID>
```

Scheduled backups run on the leader every 6 hours (`schedule`); after a
leader change the new leader continues the schedule.

### 12. Observability

OpenObserve (single mode on node-1, which has the host volume `openobserve`)
with TLS from Norca's workload certificates, and an OpenTelemetry Collector
on every node (host, agent and NATS metrics, OTLP of the
applications), from the registry's
[openobserve](https://github.com/bacanoo-io/norca-registry/blob/main/packs/openobserve/README.md)
and [otel-collector](https://github.com/bacanoo-io/norca-registry/blob/main/packs/otel-collector/README.md)
packs (newer than `v0.1.0`: `--ref=main` or a later tag):

```shell
sudo install -d -m 700 /srv/openobserve                                 # on node-1
norca namespace apply -description "Observability" observability
norca secret put -namespace observability openobserve/root password=-   # 8+ chars: upper, lower, digit, special
norca pack run openobserve --registry=norca --namespace=observability --var namespace=observability
norca alloc logs -namespace observability -task bootstrap <alloc ID>     # "ingest credentials ... stored"

norca secret put -namespace observability norca/agent-ca ca=@nomad-agent-ca.pem
norca pack run otel-collector --registry=norca --namespace=observability \
  --var namespace=observability \
  --var nomad_address=https://127.0.0.1:4646 --var nomad_ca_secret_path=norca/agent-ca \
  --var 'discovery_namespaces=["default","apps"]'
norca pack upgrade nats --registry=norca --name=nats -f nats.hcl --var metrics_enabled=true
```

The UI is `https://openobserve.service.observability.nomad:5080/` (the
certificate's name; resolve it to node-1, e.g. in `/etc/hosts`, or through the
built-in DNS) once the CA of the region is trusted by the browser:
`norca operator api /v1/mtls/roots | jq -r '.Roots[].RootCert' > norca-mtls-ca.pem`.
For OpenObserve in cluster mode (routers, ingesters, queriers, compactor,
alertmanager) add the `postgres` pack (database `openobserve`), RustFS and
NATS with JetStream: see the openobserve pack's README.

### 13. Teardown

```shell
norca pack uninstall otel-collector --namespace=observability
norca pack uninstall openobserve --namespace=observability
norca object delete norca.io/LoadBalancer test-app -namespace apps
norca pack uninstall lb --namespace=apps
norca pack uninstall nats
norca job stop -namespace apps -purge probe
norca pack uninstall test-api --namespace=apps
norca pack uninstall test-app --namespace=apps
# on every node:
sudo systemctl disable --now norca && sudo rm -rf /opt/norca/data /etc/norca.d
```

## What to look at in the web UI

`https://10.0.0.11:4646/ui/`, signed in with the bootstrap token:

- **Servers and regions**: the three servers, the leader, Raft state.
- **Topology**: the nodes by zone with their allocations; test-app spread
  over the three zones, the probe next to a test-app instance.
- **Jobs → test-app → Placement** (and probe): the `separate` and
  `colocate` rules and the live state of the disruption budget.
- **Services**: `test-app` (zones, health), `test-api` and `probe` behind
  their sidecars.
- **Network**: the intentions of `apps`, the network policy of test-app with
  its selected allocations, the admin policy.
- **Objects** and **Type definitions**: the `norca.io/LoadBalancer` kind and
  the `test-app` object with its status and events.
- **Packages**: the releases `test-app`, `test-api`, `lb` (and `nats`).
- **Scaling**, **Quotas**, **Admission**, **Backups** as in 02.
