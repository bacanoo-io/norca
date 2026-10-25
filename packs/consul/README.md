<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# consul

Installs and manages the **official, current HashiCorp Consul release** on a
Norca cluster: the Consul servers as a service job, a Consul client agent on
every other node as a system job, and optionally a periodic snapshot job. It
is item 6 ("Full Consul as an option") of the
[Consul independence plan](../../docs/design/consul-independence-plan.md):
for what still needs a real Consul (Consul Connect with L7 features, sessions
and locks, the full KV, prepared queries, config entries). Service discovery,
DNS, the Consul-compatible read layer and external services do **not** need
this pack.

## License: Consul is not part of Norca

> **Consul is licensed by HashiCorp under the Business Source License 1.1
> (BSL 1.1), not under an open source license.** Norca does not distribute,
> include, vendor or modify Consul, and ships no Consul binary or image. This
> pack only *references* the official release by name, tag and digest (the
> `hashicorp/consul` image) or by URL and checksum (the release archive);
> the nodes pull it from HashiCorp, or from your internal mirror of the
> official artifacts.
>
> **You accept the Consul license when you install it.** The pack does not
> render until you set `consul_license_accepted = true`, confirming that you
> have read the [Consul license](https://github.com/hashicorp/consul/blob/main/LICENSE)
> and that your use is permitted. BSL 1.1 allows internal production use; the
> restriction concerns products competing with HashiCorp. Confirm this
> legally for your own use case.

The pack itself (these templates) is MPL 2.0 like the rest of Norca. Norca
talks to Consul only through its documented HTTP API. "Consul" is a trademark
of HashiCorp and is used here only to describe what the pack installs.

Why not the last MPL release (v1.16.4)? It misses years of security fixes
and supports only outdated Envoy releases; see the plan. The pack defaults to
a current release (`consul_version`, see [Provisional defaults](#provisional-defaults)).

## What it deploys

**Job `consul`** (name: `job_name`), group `servers`, `server_count` servers
(3 or 5; 1 only for development):

- one server per node (`distinct_hosts`), only on nodes with the node meta
  `consul_server = "true"` (`server_node_meta`), spread over the zones of the
  node meta `zone` (`zone_attribute`; `zone_separate_required` adds the fork's
  `separate` block for at most one server per zone);
- the Raft data on the client host volume `consul-server`
  (`server_host_volume`), mounted at `/consul/data`;
- static ports on the host network (`host_network`, e.g. the WireGuard
  interface of the VPN); the agent is the local agent of its node;
- a `bootstrap` prestart task, an `acl-setup` poststart task (see
  [Bootstrap](#bootstrap-no-manual-steps)) and the `server` task;
- `disruption { max_unavailable = 1 }`, rolling updates and migrations one
  server at a time (see [Upgrades](#upgrades-rolling-one-server-at-a-time));
- services (native provider): `consul-serf` (group service, the join
  addresses, no checks) and `consul` (the API port, with startup, readiness
  and liveness checks on the RPC and API ports).

**Job `consul-clients`** (`clients_enabled`, default on): a system job with a
client agent on every node of the node pool that is not a server node
(servers already are the local agent of their nodes). Workloads reach their
local agent on `127.0.0.1:8500` (HTTP) and `127.0.0.1:8502` (gRPC, e.g. for
Envoy). The agent state is on a sticky ephemeral disk.

**Job `consul-snapshot`** (`snapshot_enabled`, default off): see
[Backup](#backup-snapshots-to-s3).

Nomad does not need Consul to start or to run these jobs: discovery between
the Consul agents uses Norca's native service discovery (`nomadService`), and
the secrets come from Norca Variables. There is no circular dependency.

## Prerequisites

1. **Server nodes.** Pick `server_count` nodes, one per zone, and set on each
   (client configuration):

   ```hcl
   client {
     meta {
       consul_server = "true"
       zone          = "eu-central-1a"   # filled in by the installer
     }
     host_volume "consul-server" {
       path = "/srv/consul"
     }
   }
   ```

   With `driver = "raw_exec"` (no volume mounts) set `server_data_path` to a
   directory on the node instead of the host volume. With more marked nodes
   than servers, a server that moves to a spare node
   starts with an empty data directory and is synced by the others (the old
   peer is cleaned up by autopilot).

2. **A driver for the bootstrap task**: `exec` (default; root client) or
   `raw_exec` (`bootstrap_driver`), with the `norca` binary at `norca_binary`
   (default `/usr/local/bin/norca`) on the server nodes. The Consul agents
   themselves use `docker` (default), `exec` or `raw_exec` (`driver`).

3. **Norca's own `consul {}` block.** Norca, like Nomad, defaults to a Consul
   agent on `127.0.0.1:8500` and would try to register itself there once the
   local agent runs, failing with "ACL not found" (default deny). Either wire
   it (see [Wiring Norca's consul block](#wiring-norcas-consul-block-optional))
   or turn it off in the agent configuration of every node:

   ```hcl
   consul {
     auto_advertise   = false
     server_auto_join = false
     client_auto_join = false
   }
   ```

4. **With Norca ACLs enabled**: the ACL policy of the bootstrap task (the
   only manual step). `norca pack render --render-output-template` and
   `norca pack run` print it with the right names:

   ```
   norca acl policy apply -namespace default -job consul -group servers -task bootstrap \
     consul-bootstrap consul-bootstrap.hcl
   ```

   It allows writing the Variables listed below; everything else the tasks
   read with their workload identity without any policy.

Then:

```
norca pack run consul --registry=norca --var consul_license_accepted=true
norca pack run ./packs/consul --var-file consul.hcl          # from a checkout
```

## Bootstrap (no manual steps)

On the first start of the servers the `bootstrap` prestart task (a shell
script running the `norca` CLI against the task API socket with the task's
workload identity) generates:

| Secret | How |
|---|---|
| gossip encryption key | `norca operator gossip keyring generate` (32 bytes, AES-256) |
| CA and CA key | `norca tls ca create` (`ca_days`, default 5 years) |
| server certificate | `norca tls cert create -server -region <datacenter> -domain <domain>`: valid for `server.<datacenter>.<domain>` (Consul's `verify_server_hostname` name), `localhost` and 127.0.0.1 (`cert_days`, default 1 year) |
| ACL tokens | random UUIDs: the `initial_management` token, the agent token and the snapshot token, with fixed accessor IDs |

and stores them in Norca Variables (encrypted at rest by the keyring of the
Norca servers):

| Variable | Items | Read by |
|---|---|---|
| `nomad/jobs/consul/servers/bootstrap` | all, including `ca_key` (the source of truth) | only the bootstrap task |
| `nomad/jobs/consul/servers` | all but `ca_key` | the server and acl-setup tasks |
| `nomad/jobs/consul-clients` | `gossip_key`, `ca_cert`, `agent_token` | the client job |
| `nomad/jobs/consul-snapshot/snapshot` | `consul_token` | the snapshot job (only with `snapshot_enabled`) |

The source Variable is created with check-and-set (`-check-index=0`): when
the servers start at the same time, one of them wins and all continue with
its secrets. The other Variables are derived from it on every server start,
so deleting one repairs it, and the secrets are never generated twice. The
bootstrap task then waits (at most 5 minutes) until every server is
registered in `consul-serf`, so that the `retry_join` lists of the first
start contain each other.

The agents get their configuration from templates (`nomadVar`); the task does
not start until the Variable exists, so the clients simply wait for the
servers' first bootstrap.

**ACL bootstrap.** The servers carry the generated token as
`acl.tokens.initial_management`, which bootstraps the ACL system on the first
leader election: there is no `consul acl bootstrap` step and no token to copy.
The `acl-setup` poststart task then waits for a leader and creates,
idempotently, the `norca-agent` policy (`node_prefix "" write`,
`service_prefix "" read`) with the agent token (used by every agent as
`acl.tokens.agent`) and, with snapshots, the `norca-snapshot` policy
(`acl = "write"`, which `consul snapshot save` needs) with the snapshot token.

The management token, for operators:

```
norca var get -item=management_token nomad/jobs/consul/servers/bootstrap
```

## Security

The pack applies the interim security minimum of the plan to every install:

- **ACLs are mandatory**: `enabled = true`, `default_policy = "deny"`,
  `down_policy = "extend-cache"`; there is no anonymous access (no default
  token). This is not configurable.
- **Gossip encryption** with incoming and outgoing verification.
- **TLS** (`tls_enabled`, default on): verified server names for RPC
  (`verify_incoming`, `verify_outgoing`, `verify_server_hostname`), client
  certificates through `auto_encrypt` (the clients only get the CA), HTTPS and
  gRPC-TLS without client certificates (ACL tokens authenticate API clients).
- **Bind addresses**: gossip and RPC on the private network (`host_network`);
  plain HTTP (8500) and gRPC (8502) on **localhost only**, HTTPS (8501) and
  gRPC-TLS (8503) on localhost and the private network; DNS (8600) on
  localhost (`dns_address`). Without TLS the plain ports also listen on the
  private network, never on a public one. Put `host_network` on the VPN
  interface in multi-cloud setups.
- `disable_update_check = true`: no call home to HashiCorp's checkpoint
  service.
- Use L4 (service-level) intentions; move service mesh traffic to Norca's
  native mTLS as it becomes available.

The secrets live only in Variables and in the tasks' `secrets/` directories
(mode 0600, readable by root): the Consul agents run as root inside their
container (the image's entrypoint script, which drops to a `consul` user, is
bypassed) or the exec chroot. The server certificate grants the authority of
a server: it is readable only by the server tasks.

The generated certificates also carry the Nomad-style names
`server.<datacenter>.nomad` and `server.global.nomad` (added by
`norca tls cert create`); they are harmless because the Consul CA is a
separate CA that Norca does not trust.

## Air-gapped installs and the internal registry

Following the package management decision (an internal, configurable OCI
registry), every artifact is referenced by a configurable location:

| What | Variables | Default |
|---|---|---|
| Consul image | `image_registry`, `image_repository`, `image_tag` (or `consul_version`), `image_digest` | `docker.io/hashicorp/consul:<consul_version>` |
| Consul release archive (exec, raw_exec) | `download_base_url`, `download_arch`, `download_checksum`, or a preinstalled `consul_binary` | `https://releases.hashicorp.com/consul/<version>/consul_<version>_linux_amd64.zip` |
| Snapshot upload image | `snapshot_upload_image` | `docker.io/amazon/aws-cli:2.27.0` (Apache-2.0) |

Mirror the **unmodified official** image into the internal registry and pin
it by digest, e.g. with `crane` or `skopeo`:

```
crane copy docker.io/hashicorp/consul:2.1.0 registry.internal:5000/hashicorp/consul:2.1.0
crane digest registry.internal:5000/hashicorp/consul:2.1.0
```

```hcl
image_registry = "registry.internal:5000"
image_digest   = "sha256:<digest printed above>"
```

For exec/raw_exec mirror the release archive and the official `SHA256SUMS`
file (verify their signature with HashiCorp's public key) to an internal HTTP
server and set `download_base_url` and `download_checksum = "sha256:<hex>"`.
Whoever mirrors the artifacts accepts the Consul license; the Norca pack
registry itself never contains them.

## Wiring Norca's consul block (optional)

The pack does not change the Norca agents. To use the installed Consul from
Norca (Consul service registrations of jobs, Consul Connect, Consul KV in
templates), create a token for the Norca agents with the management token,
on any server allocation (`norca alloc exec -task server <alloc> sh`):

```
export CONSUL_HTTP_TOKEN=<management token>
cat >norca-agents.hcl <<'EOF'
agent_prefix "" {
  policy = "read"
}
node_prefix "" {
  policy = "read"
}
service_prefix "" {
  policy = "write"
}
# Only with Consul Connect (service identity tokens), on the Norca servers:
acl = "write"
# Only if templates read Consul KV:
# key_prefix "" { policy = "read" }
EOF
consul acl policy create -name norca-agents -rules @norca-agents.hcl
consul acl token create -policy-name norca-agents -description "Norca agents"
```

and configure every Norca agent (the local Consul agent, plain HTTP on
localhost):

```hcl
consul {
  address = "127.0.0.1:8500"
  token   = "<secret ID of the token above>"

  # Norca does not depend on Consul: it keeps its own discovery.
  server_auto_join = false
  client_auto_join = false

  # Register the Norca agents in the Consul catalog (optional).
  auto_advertise = true
}
```

Keep `server_auto_join` and `client_auto_join` off, so Norca never needs
Consul to form its own cluster; the Consul jobs of this pack keep working when
Consul is down, and Norca keeps working when Consul is removed. For Connect,
also set `grpc_address = "127.0.0.1:8502"` and, with TLS, `grpc_ca_file`
pointing to the Consul CA (`ca_cert` of `nomad/jobs/consul-clients`).

## Upgrades: rolling, one server at a time

Consul's upgrade rule is servers first, one at a time, then the clients;
read the version-specific upgrade notes of the target release first.

1. Take a snapshot (the snapshot job, or `consul snapshot save`).
2. Upgrade the **servers**: change `consul_version` (and `image_digest`) and
   run only the server job, e.g.
   `norca pack render ./packs/consul --var-file consul.hcl --to-dir out` and
   `norca job run out/consul/consul.nomad`. The deployment replaces one server
   at a time (`max_parallel = 1`); a server keeps its Raft data on the host
   volume and does not leave the cluster when it stops
   (`skip_leave_on_interrupt`, no `leave_on_terminate`), so it rejoins as the
   same peer. The next server is replaced only after the previous one has
   passed its checks for `min_healthy_time` (30s, longer than autopilot's
   server stabilization time). Between steps you can check
   `consul operator raft list-peers` and `consul operator autopilot state`.
3. Upgrade the **clients**: `norca pack run` with the same variables. The
   system job replaces one client at a time.

Drains go one server at a time as well (`disruption`). Consul does not
support downgrading the Raft state: roll back by restoring a snapshot taken
before the upgrade, not by lowering the version.

**Certificate renewal** (the server certificate is valid for `cert_days`):
with the CA from the source Variable, issue a new one and update the source;
the next server start propagates it, or update `nomad/jobs/consul/servers`
too and the templates reload the agents with `SIGHUP`:

```
norca var get -item=ca_cert nomad/jobs/consul/servers/bootstrap >ca.pem
norca var get -item=ca_key  nomad/jobs/consul/servers/bootstrap >ca-key.pem
norca tls cert create -server -region=dc1 -domain=consul -ca=ca.pem -key=ca-key.pem
norca var get -out=json nomad/jobs/consul/servers/bootstrap >src.json   # keep the other items
norca var put nomad/jobs/consul/servers/bootstrap @src.json \
  server_cert=@dc1-server-consul.pem server_key=@dc1-server-consul-key.pem
```

**Gossip key rotation** uses Consul's keyring (`consul keyring -install`,
`-use`, `-remove`); afterwards store the new key as `gossip_key` in the
source Variable so new agents get it.

## Backup: snapshots to S3

With `snapshot_enabled = true` the periodic batch job `consul-snapshot`
(`snapshot_cron`, default hourly, UTC, no overlap) runs on a server node:
`consul snapshot save` against the local agent with the snapshot token, then
an upload with the AWS CLI image to the S3-compatible store used by the Norca
backups (MinIO or AWS S3), as
`s3://<snapshot_s3_bucket>/<snapshot_s3_prefix>/<datacenter>/<UTC timestamp>.snap`.

```hcl
snapshot_enabled       = true
snapshot_s3_bucket     = "company-backup"
snapshot_s3_prefix     = "consul/eu-aws"
snapshot_s3_endpoint   = "https://minio.internal:9000"   # empty for AWS S3
snapshot_s3_path_style = true
snapshot_s3_sse        = "AES256"                         # or aws:kms
```

The S3 credentials come from the Variable `nomad/jobs/consul-snapshot`
(items `aws_access_key_id`, `aws_secret_access_key`), or from the node's
environment with `snapshot_s3_credentials = false`:

```
norca var put nomad/jobs/consul-snapshot aws_access_key_id=<id> aws_secret_access_key=-
```

Snapshots contain the ACL tokens and KV data: encrypt them at rest
(`snapshot_s3_sse`, bucket encryption) and expire them with the store's
lifecycle rules. Restore with `consul snapshot restore <file>` against a
server, using the management token.

## Variables

See `variables.hcl` for all of them; the most important:

| Variable | Default | |
|---|---|---|
| `consul_license_accepted` | `false` | must be `true` (BSL 1.1, see above) |
| `consul_version` | `2.1.0` | provisional, pin your validated release |
| `server_count` | `3` | 3 or 5 (1 for development) |
| `driver` | `docker` | `docker`, `exec`, `raw_exec` |
| `image_registry`, `image_digest` | `docker.io`, `""` | internal mirror, pinning |
| `server_node_meta`, `zone_attribute` | `consul_server`, `${meta.zone}` | placement |
| `host_network` | `""` | private network (VPN) interface |
| `datacenter`, `domain` | `dc1`, `consul` | part of the certificate name |
| `tls_enabled` | `true` | |
| `bootstrap_driver`, `norca_binary` | `exec`, `/usr/local/bin/norca` | bootstrap task |
| `clients_enabled` | `true` | client system job |
| `snapshot_enabled` | `false` | snapshot job |
| `port_*` | Consul's defaults | static |

### Provisional defaults

Reversible choices made without a user decision; change them with variables:

- `consul_version = "2.1.0"` (the latest release named in the plan) and
  `image_registry = "docker.io"`; no default digest (it must come from your
  own verification of the official image).
- `datacenter = "dc1"` (Consul's default), not the Norca region name.
- `snapshot_upload_image = "docker.io/amazon/aws-cli:2.27.0"` and the hourly
  `snapshot_cron`.
- Server nodes marked by node meta `consul_server = "true"`.

## Development and testing

`norca pack render ./packs/consul --var consul_license_accepted=true`
renders the jobs without submitting them; `norca pack plan` validates them
against a cluster. The pack's tests
(`nomad/pack/internal/cli/consul_pack_test.go`) render several variable sets,
parse and validate the jobs and plan them against a test agent; they never
pull or run Consul.

On a single development node: `server_count = 1`, and remove the meta
constraints with `server_node_meta = ""` (then do not run the clients there:
`clients_enabled = false`).

## Limitations

- The pack cannot create Norca ACL policies: with Norca ACLs the bootstrap
  task's policy is applied once by hand (printed by the pack).
- No automatic certificate renewal or gossip key rotation (documented manual
  steps above); a Consul operator could automate them later
  (`kubernetes-features-spec.md` item 8).
- Consul DNS on the agents has no token: with default deny it answers only
  after you give it one (e.g. a policy with `node_prefix`, `service_prefix`
  read and `consul acl set-agent-token default <token>` on the agents, which
  also applies to anonymous local HTTP requests). Norca's native DNS is the
  recommended resolver.
- One Consul datacenter per pack install; WAN federation between Norca
  regions is not configured (the WAN port is reserved on the servers).
- The bootstrap task needs the `exec` or `raw_exec` driver with the `norca`
  binary on the server nodes; the snapshot upload needs `docker`.
- Consul 1.14 or newer (the `tls` stanza, `grpc_tls` port and
  `initial_management` token names).
