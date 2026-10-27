<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# 02 – Single node

One Linux machine running the server and the client in one agent, set up the
way a small production node would be: persistent data, TLS, ACLs, gossip
encryption, the `shares` seal, scheduled age-encrypted backups to RustFS,
a namespace with a quota and an admission policy, and the test application
with secrets and health check roles.

A single server has no failover: use it for labs, edge sites and small
internal tools. [03-three-node](../03-three-node/README.md) is the highly
available version.

## Prerequisites

- Linux with Docker, the [CNI reference plugins](https://github.com/containernetworking/plugins/releases)
  in `/opt/cni/bin` (bridge networking), `curl` 7.75 or newer and
  [age](https://github.com/FiloSottile/age) (`age-keygen`).
- The `norca` binary in `/usr/local/bin` (see [../README.md](../README.md#getting-the-norca-binary)).
- The [Norca pack registry](https://github.com/bacanoo-io/norca-registry) for the RustFS store (step 7):
  git and access to GitHub, or a checkout of the registry (see
  [../README.md](../README.md#the-pack-registry)).
- This directory and [../policies](../policies) on the machine.
- Below, `10.0.0.10` is the address of the machine: replace it everywhere.

## Files

| File | |
| --- | --- |
| [norca.hcl](norca.hcl) | Agent configuration (server and client) |
| [norca.service](norca.service) | systemd unit |
| [backup-credentials.example](backup-credentials.example) | S3 credentials of the backup destination |
| [rustfs.hcl](rustfs.hcl) | Variables of the [rustfs pack](https://github.com/bacanoo-io/norca-registry/blob/main/packs/rustfs/README.md): RustFS as the backup store, the backup bucket |
| [test-app.hcl](test-app.hcl) | Variables of the [test-app pack](../packs/test-app/README.md) |

## Steps

### 1. Directories, TLS, keys

```shell
sudo mkdir -p /etc/norca.d/tls /opt/norca/data /var/log/norca
sudo install -d -o 10001 -g 10001 -m 750 /srv/rustfs   # the RustFS data, owned by its container user

# CA and server certificate (valid for server.global.nomad, localhost and the
# machine address). Keep nomad-agent-ca-key.pem offline.
cd /etc/norca.d/tls
sudo norca tls ca create
sudo norca tls cert create -server -additional-ipaddress 10.0.0.10

# Gossip encryption key, for server.encrypt in norca.hcl.
norca operator gossip keyring generate

# age key pair of the backups: the public key (age1...) goes into
# server.backup.encryption.recipients, the identity file stays offline.
age-keygen -o ~/backup-identity.txt
```

### 2. Configuration and start

```shell
sudo cp norca.hcl /etc/norca.d/norca.hcl          # then replace every CHANGE_ME
sudo install -m 600 backup-credentials.example /etc/norca.d/backup-credentials   # CHANGE_ME: the RustFS root credentials of step 7
norca config validate /etc/norca.d/
sudo cp norca.service /etc/systemd/system/norca.service
sudo systemctl daemon-reload && sudo systemctl enable --now norca
```

`config validate` checks the syntax, the keys and the values of the files,
including the `keyring`, `backup`, `autoscaler`, `secrets` and `dns` blocks
as the agent checks them when it starts, so a placeholder left in place (the
age recipient `age1CHANGE_ME`, the gossip key) fails here. Files, addresses
and the backup destination are only used when the agent runs
(`journalctl -u norca`).

CLI environment for the following steps:

```shell
export NORCA_ADDR=https://10.0.0.10:4646
export NORCA_CACERT=/etc/norca.d/tls/nomad-agent-ca.pem
```

### 3. ACL bootstrap

```shell
norca acl bootstrap            # prints the management token once
export NORCA_TOKEN=<Secret ID of the bootstrap token>
```

### 4. Initialize the keyring seal

The server starts with the `shares` seal and an uninitialized keyring. The
first rewrap to `shares` generates the unseal key and prints its shares
**once**; they are stored nowhere.

```shell
norca operator keyring rewrap -to shares     # 5 shares, threshold 3 (from norca.hcl)
norca operator seal-status
```

Hand the shares to five different people. After every restart of the agent
the server is sealed until three shares are entered: it serves everything
that needs no decryption, but rejects Variables, workload identity signing,
mTLS issuance and the secret store.

```shell
norca operator unseal          # prompts for one share without echo; run it three times
norca operator seal-status
```

### 5. Namespace, quota, admission policy, developer access

```shell
norca quota apply ../policies/quota-apps.hcl
norca namespace apply ../policies/namespace-apps.hcl
norca admission-policy apply ../policies/admission-apps.hcl
norca acl policy apply -description "Developers of apps" app-developer ../policies/acl-app-developer.hcl
norca acl token create -name dev -policy app-developer       # token for the developers
norca quota status apps
```

### 6. Secrets of the application

Secrets are versioned and audited, and a task can read only what its job
grants (`secret_access`). Values given on the command line end up in the
shell history: read them from standard input or a file.

```shell
norca secret put -namespace apps -description "Database login of test-app" -owner demo \
  test-app/db username=app password=-        # type the password, then Ctrl-D
norca secret info -namespace apps test-app/db  # metadata and versions, never values
```

### 7. Backup store

RustFS (Apache-2.0), an S3-compatible store, from the `rustfs` pack of the
Norca pack registry. Its root credentials live in the secret store: the
access key and the secret key are the values of
`/etc/norca.d/backup-credentials`.

```shell
norca secret put rustfs/root access_key=norca-backup secret_key=-   # type the secret key, then Ctrl-D

norca pack registry add norca https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
norca pack run rustfs --registry=norca --name=rustfs -f rustfs.hcl
norca service info -verbose rustfs
norca alloc logs -task create-buckets <alloc ID>     # "bucket norca-backup created"
```

`buckets` in `rustfs.hcl` makes the pack create the backup bucket once the
store is ready (Norca never creates it). Without it, create the bucket with
an S3 CreateBucket signed by curl:

```shell
curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "norca-backup:<secret key>" \
  -X PUT http://10.0.0.10:9000/norca-backup
```

The `backup` block of `norca.hcl` points to this store (`endpoint`,
`bucket`, `region` = the pack's `s3_region`, `force_path_style`); the first
scheduled backup runs at the next full hour. The console is at
`http://10.0.0.10:9001/rustfs/console/` with the same credentials.

### 8. Deploy the test application

```shell
norca pack run ../packs/test-app --name=test-app --namespace=apps -f test-app.hcl
norca pack status test-app --name=test-app --namespace=apps
norca pack history test-app --namespace=apps
```

The release is applied by the servers: the admission policy, the quota
(`task_max`, `allocations`) and the typed scaling policy are checked at
submission. `apps` is a production namespace (`meta.environment`), so a
failed upgrade restores the previous revision.

### 9. Verify

```shell
norca job status -namespace apps test-app                  # allocations and the disruption budget
norca service info -namespace apps -verbose test-app      # instances, health, the three check roles
norca scaling policy list -namespace apps                 # typed policy, engine builtin
dig @127.0.0.1 -p 8600 test-app.service.apps.nomad SRV     # built-in DNS
curl http://<address:port from service info>/              # "Secret test-app/db: version 1"

# Rotate the secret: the secret block delivers it again (SIGHUP), the
# Vault-style template re-renders the page with version 2.
norca secret put -namespace apps test-app/db username=app password=-
sudo tail -n 5 /var/log/norca/secrets-audit.log            # reads by the workload identities

# Disruption budget (max_unavailable = 1): the first eviction stops one
# instance, a second one is refused until its replacement is healthy.
norca alloc stop -respect-disruption <alloc ID 1>
norca alloc stop -respect-disruption <alloc ID 2>

norca quota status apps
```

### 10. Backups

```shell
norca operator backup save -label note=first-backup        # manual backup, waits for upload and verification
norca operator backup list
norca operator backup inspect <backup ID>
norca operator backup contents -age-identity ~/backup-identity.txt -type jobs <backup ID>
norca operator backup contents -age-identity ~/backup-identity.txt -type secrets -namespace apps <backup ID>

# Selective restore: preview, then restore the job and the secret as new versions.
norca operator backup restore -age-identity ~/backup-identity.txt -namespace apps \
  -job test-app -secret test-app/db -dry-run <backup ID>
norca operator backup restore -age-identity ~/backup-identity.txt -namespace apps \
  -job test-app -secret test-app/db <backup ID>
```

A full restore of the region (`restore -full -confirm global`) needs a second
administrator with another management token (`norca operator backup approve`);
`norca operator backup restore-bootstrap` builds a new cluster from a backup.

### 11. Observability

OpenObserve (single mode: SQLite and the data on the host volume
`openobserve` of `norca.hcl`) and an OpenTelemetry Collector on the node,
both from the registry ([openobserve](https://github.com/bacanoo-io/norca-registry/blob/main/packs/openobserve/README.md),
[otel-collector](https://github.com/bacanoo-io/norca-registry/blob/main/packs/otel-collector/README.md),
both in the registry tag `v0.1.0`). The
agent serves its Prometheus metrics already (`telemetry` in `norca.hcl`).
Native mTLS is not enabled here, so both run without TLS:

```shell
sudo install -d -m 700 /srv/openobserve
norca namespace apply -description "Observability" observability
norca secret put -namespace observability openobserve/root password=-   # 8+ chars: upper, lower, digit, special
norca pack run openobserve --registry=norca --namespace=observability \
  --var namespace=observability --var tls_mode=none
norca alloc logs -namespace observability -task bootstrap <alloc ID>     # "ingest credentials ... stored"

norca secret put -namespace observability norca/agent-ca ca=@/etc/norca.d/tls/nomad-agent-ca.pem
norca pack run otel-collector --registry=norca --namespace=observability \
  --var namespace=observability --var openobserve_tls=none \
  --var nomad_address=https://127.0.0.1:4646 --var nomad_ca_secret_path=norca/agent-ca \
  --var 'discovery_namespaces=["default","apps"]'
```

Open `http://10.0.0.10:5080/` and log in as `root@openobserve.internal`: the
host and agent metrics arrive in the organization `default` within a minute.
RustFS pushes its metrics to the collector with
`norca pack upgrade rustfs --registry=norca --name=rustfs -f rustfs.hcl --var metrics_enabled=true`; the
applications of the node send OTLP to `http://10.0.0.10:4318`.

### 12. Teardown

```shell
norca pack uninstall otel-collector --namespace=observability
norca pack uninstall openobserve --namespace=observability
norca pack uninstall test-app --namespace=apps
norca pack uninstall rustfs
sudo systemctl disable --now norca
sudo rm -rf /opt/norca/data /srv/rustfs /srv/openobserve /etc/norca.d /var/log/norca
```

## What to look at in the web UI

Open `https://10.0.0.10:4646/ui/` and sign in with a token (the bootstrap
token, or the `dev` token for the developer's view).

- **Dashboard**: cluster health, allocations, live updates.
- **Jobs → test-app**: allocations, services with the check roles, the
  scaling page of the typed policy, versions.
- **Services**: `test-app` and `rustfs` with the health of every instance.
- **Quotas**: usage of `apps` against its limits.
- **Admission**: the `apps-baseline` policy and its rules.
- **Scaling**: the policy, its checks and the scaling events.
- **Backups**: the list, contents (with the age identity), comparison and
  restore.
- **Packages**: the `test-app` and `rustfs` releases with their revisions.

There is no page for the built-in secret store yet: use `norca secret ...`;
the task events of an allocation show which provider delivered each secret
(`secret db: provider norca (fallback)`), never values.
