<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# 04 – Multi region

Two federated regions, for example one per cloud, each a three-node
cluster built like [03-three-node](../03-three-node/README.md): `eu-1`
(authoritative, nodes `10.0.0.11`–`13`) and `eu-2` (nodes `10.0.1.11`–`13`),
connected by a VPN between the clouds. Every region has its own Raft, seal,
secret store and backups; the outage of one region does not stop the other.

This shows: federation with ACL replication, a multiregion deployment of
the test application (one version, rolled out region by region), native
mTLS across regions through a gateway, a NATS supercluster, a pack release
installed in both regions at once, and per-region backups.

A third region is added the same way as `eu-2`.

## Prerequisites

- Everything of 03 in every region.
- The regions reach each other over the VPN on 4647 (RPC) and 4648 (gossip)
  between the servers, 8443 (mTLS gateway) and 7222 (NATS gateways)
  between the clients.
- One CA for both regions (the servers verify `server.<region>.nomad`).

## Files

| File | |
| --- | --- |
| [region-eu-1.hcl](region-eu-1.hcl) | Region, certificate and authoritative region of the eu-1 nodes (`/etc/norca.d/region.hcl`) |
| [region-eu-2.hcl](region-eu-2.hcl) | Same for eu-2, with the replication token, the join address in eu-1 and the catalog token of the clients |
| [quota-apps.hcl](quota-apps.hcl) | The quota of `apps` with a limit per region |
| [test-app-multiregion.hcl](test-app-multiregion.hcl) | test-app pack as a multiregion job |
| [mtls-gateway.nomad.hcl](mtls-gateway.nomad.hcl) | The mTLS gateway of a region |
| [probe-eu-2.nomad.hcl](probe-eu-2.nomad.hcl) | The probe in eu-2, reading test-api of eu-1 |
| [nats-eu-1.hcl](nats-eu-1.hcl), [nats-eu-2.hcl](nats-eu-2.hcl) | packs/nats per region, joined into a supercluster |

## Steps

### 1. Certificates

As in 03, with one CA and a server certificate per node that names its
region:

```shell
norca tls ca create
# In a directory per node (eu-1: 10.0.0.11-13, eu-2: 10.0.1.11-13):
norca tls cert create -server -region eu-1 -ca ../nomad-agent-ca.pem -key ../nomad-agent-ca-key.pem \
  -additional-ipaddress 10.0.0.11     # eu-1-server-nomad.pem, valid for server.eu-1.nomad
norca tls cert create -server -region eu-2 -ca ../nomad-agent-ca.pem -key ../nomad-agent-ca-key.pem \
  -additional-ipaddress 10.0.1.11     # eu-2-server-nomad.pem
```

Use the same gossip key (`server.encrypt`) in both regions: the servers of
a federation share one gossip pool.

### 2. Region eu-1

Install every eu-1 node as in 03 (step 2), with its `eu-1-server-nomad*.pem`
in `/etc/norca.d/tls/` and `region-eu-1.hcl` as `/etc/norca.d/region.hcl`
before the first start, then:

```shell
export NORCA_ADDR=https://10.0.0.11:4646 NORCA_CACERT=$PWD/nomad-agent-ca.pem
norca acl bootstrap                       # the bootstrap token is global
export NORCA_TOKEN=<Secret ID>
norca operator keyring rewrap -to shares  # shares of eu-1
norca operator unseal -server-id <name>   # the other two servers, three shares each

# Tokens for eu-2: ACL replication, and the catalog token of its clients.
norca acl token create -type management -global -name replication-eu-2
norca acl policy apply -description "Catalog read" catalog-read ../policies/acl-catalog-read.hcl
norca acl token create -name catalog-read -policy catalog-read -global
```

Put the two secret IDs into `region-eu-2.hcl` (`acl.replication_token`,
`client.dns.token`).

### 3. Region eu-2

Install every eu-2 node the same way (with its own `node.hcl` addresses and
`region-eu-2.hcl` as `region.hcl`). Its `retry_join` list contains a server
of eu-1, which federates the regions; no ACL bootstrap in eu-2, the tokens
and policies are replicated from eu-1.

```shell
norca server members                     # the servers of both regions
norca status -region eu-2                # forwarded to eu-2 with the global token

# eu-2 has its own keyring and shares. Run the seal commands against a
# server of eu-2:
export NORCA_ADDR=https://10.0.1.11:4646
norca operator keyring rewrap -to shares   # shares of eu-2
norca operator unseal -server-id <name>    # the other two eu-2 servers
norca operator seal-status
export NORCA_ADDR=https://10.0.0.11:4646
```

The native mTLS CAs of the regions trust each other (`mtls.federation`, a
trust domain shared by the federation):

```shell
norca operator api /v1/mtls/roots          # roots of eu-1 and eu-2
```

### 4. Global objects

Namespaces, quotas and ACL policies are written in the authoritative region
and replicated:

```shell
norca quota apply quota-apps.hcl
norca namespace apply ../policies/namespace-apps.hcl
norca admission-policy apply -region eu-1 ../policies/admission-apps.hcl
norca admission-policy apply -region eu-2 ../policies/admission-apps.hcl   # admission policies are per region
norca namespace list -region eu-2
```

Secrets are region-local: put the secret of test-app in both regions.

```shell
norca secret put -region eu-1 -namespace apps test-app/db username=app password=-
norca secret put -region eu-2 -namespace apps test-app/db username=app password=-
```

### 5. Multiregion deployment of test-app

```shell
norca pack run ../packs/test-app --name=test-app --namespace=apps \
  --regions=eu-1,eu-2 -f test-app-multiregion.hcl
norca pack status test-app --name=test-app --namespace=apps    # one row per region
```

The job is registered in both regions with the same version; eu-1 deploys
first (`max_parallel = 1`), eu-2 starts when eu-1 is healthy, and with
`on_failure = "fail_all"` a failure in one region fails the deployment in
both.

```shell
norca deployment list -region eu-1 -namespace apps
norca deployment list -region eu-2 -namespace apps          # pending or blocked until eu-1 is done
norca deployment status -region eu-2 -namespace apps <deployment ID>

# A deployment that waits for its turn while its peer region was unreachable
# can be started by hand:
norca deployment run -region eu-2 -namespace apps <deployment ID>
```

The same job without the pack: `norca pack render ../packs/test-app -f test-app-multiregion.hcl`
and `norca job run -namespace apps <rendered file>` from any region.

### 6. mTLS across regions

```shell
# A gateway in every region that offers services to the others.
norca job run -var region=eu-1 mtls-gateway.nomad.hcl
norca job run -var region=eu-2 mtls-gateway.nomad.hcl

# test-api in eu-1 (behind its sidecar, variables of 03), the probe in eu-2:
norca pack run ../packs/test-app --region=eu-1 --name=test-api --namespace=apps -f ../03-three-node/test-api.hcl
norca job run probe-eu-2.nomad.hcl

# The intentions of the destination region decide.
norca service intention create -region eu-1 -namespace apps probe test-api
norca alloc logs -region eu-2 -namespace apps -task probe <probe alloc ID>    # "<h1>test-api</h1>"
```

The probe's sidecar connects to the gateway service of eu-1 (static port
8443 on the eu-1 nodes); the gateway forwards the TLS stream by its SNI name
`test-api.service.apps.eu-1.nomad` to a sidecar of test-api without
terminating it.

### 7. NATS supercluster

```shell
norca pack run ../../packs/nats --name=nats --region=eu-1 -f nats-eu-1.hcl
norca pack run ../../packs/nats --name=nats --region=eu-2 -f nats-eu-2.hcl
norca service info -region eu-1 nats-gateway
```

The gateways join the two clusters into one supercluster (cluster names =
region names). Turn on TLS and authentication across clouds
(`tls_enabled`, `auth_mode`, see [packs/nats](../../packs/nats/README.md#secrets-tls-and-client-authentication));
`bridge_enabled = true` adds the event stream → NATS bridge per region.

### 8. Backups per region

The `backup` block of 03 is in every region; its prefix `norca/${region}`
keeps the backups of the regions apart in one bucket, and each region's
leader takes its own.

```shell
norca operator backup list -region eu-1
norca operator backup list -region eu-2
norca operator backup save -region eu-2 -label note=after-federation
```

### 9. Teardown

```shell
norca job stop -region eu-2 -namespace apps -purge probe
norca pack uninstall test-api --region=eu-1 --namespace=apps
norca pack uninstall test-app --namespace=apps --all-regions
norca job stop -region eu-1 -purge mtls-gateway
norca job stop -region eu-2 -purge mtls-gateway
norca pack uninstall nats --region=eu-1
norca pack uninstall nats --region=eu-2
```

Then stop the agents of eu-2 and eu-1 as in 03.

## What to look at in the web UI

Either region's UI shows the whole federation:

- **Servers and regions**: both regions, their servers and leaders; the
  region switcher.
- **Deployments**: the multiregion deployment of test-app with the state of
  each region (pending, blocked, running, successful).
- **Packages**: the `test-app` release with one entry per region.
- **Network**: the intention `probe → test-api` in eu-1.
- **Services** (per region): `mtls-gateway`, `test-api` in eu-1, `probe` in eu-2.
- **Backups** (per region).
