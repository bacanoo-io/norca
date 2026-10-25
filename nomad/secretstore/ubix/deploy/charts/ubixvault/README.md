# uBixVault Helm chart

Deploys uBixVault to Kubernetes: one replica by default, or several in
active/standby HA over MySQL storage (`ha.enabled`).

> The 1.x API is stable ([VERSIONING](../../../docs/VERSIONING.md)); an external
> security review has **not** happened yet and is tracked separately. Weigh that
> before trusting it with production secrets.

## Prerequisites

- Kubernetes ≥ 1.24
- A container image built from the repo-root `Dockerfile`, pushed somewhere your
  cluster can pull. No image is published yet — build and push your own:

  ```sh
  docker build -t <your-registry>/ubixvault:0.2.0-beta.1 \
    --build-arg VERSION=0.2.0-beta.1 .
  docker push <your-registry>/ubixvault:0.2.0-beta.1
  ```
- A `StorageClass` for the encrypted data volume (or an existing PVC).
- For TLS: either a `kubernetes.io/tls` Secret, or cert-manager.

## Install

```sh
# Minimal (TLS via an existing Secret, auto-unseal via an existing Secret):
helm install vault ./deploy/charts/ubixvault \
  --namespace ubixvault-prod \
  --set image.repository=<your-registry>/ubixvault \
  --set tls.existingSecret=ubixvault-tls \
  --set autoUnseal.existingSecret=ubixvault-kek
```

Create the auto-unseal KEK Secret first (32 random bytes, 64 hex chars):

```sh
kubectl -n ubixvault-prod create secret generic ubixvault-kek \
  --from-literal=auto-unseal-key="$(head -c32 /dev/urandom | xxd -p | tr -d '\n')"
```

Then **initialize once** — see the post-install notes (`helm status`), or:

```sh
kubectl -n ubixvault-prod exec -it vault-ubixvault-0 -- \
  ubixvault operator init -shares 5 -threshold 3    # save the shares + root token
```

## Deploying with MySQL storage

Run the vault against a MySQL/MariaDB database so the node is replaceable. The
example below uses the production environment (`ubixvault-prod`,
`vault.prod.ubixsys.com`); for dev, substitute `ubixvault-dev`,
`vault.dev.ubixsys.com`, and the `tls-wildcard-ubixsys-dev` secret. Ordered runbook:

```sh
NS=ubixvault-prod          # or ubixvault-dev

# 1. Namespace: managed in the kube-stuff/kubernetes repo (namespaces/$NS.yml),
#    labeled cert-manager-tls: tls-prod (or tls-dev) so Replikate syncs the
#    matching *.ubixsys.com wildcard TLS secret into it.

# 2. Prepare the database — the vault creates its own tables, you create the DB + user.
#    On your MariaDB:
#      CREATE DATABASE ubixvault CHARACTER SET binary;
#      CREATE USER 'ubixvault'@'%' IDENTIFIED BY 'STRONG-PASSWORD';
#      GRANT SELECT, INSERT, UPDATE, DELETE, CREATE ON ubixvault.* TO 'ubixvault'@'%';

# 3. Secrets: the DSN and the auto-unseal KEK (never put these in values/git).
kubectl -n "$NS" create secret generic ubixvault-db \
  --from-literal=dsn='ubixvault:STRONG-PASSWORD@tcp(MARIADB-HOST:3306)/ubixvault?tls=true'
kubectl -n "$NS" create secret generic ubixvault-kek \
  --from-literal=auto-unseal-key="$(head -c32 /dev/urandom | xxd -p | tr -d '\n')"

# 4. Install (DSN comes from the Secret; no data PVC is provisioned).
helm upgrade --install vault ./deploy/charts/ubixvault -n "$NS" \
  --set image.tag=0.2.0-beta.10 \
  --set storage.type=mysql --set storage.mysql.dsnSecret=ubixvault-db \
  --set autoUnseal.existingSecret=ubixvault-kek \
  --set tls.existingSecret=tls-wildcard-ubixsys-prod \
  --set ingress.enabled=true \
  --set ingress.host=vault.prod.ubixsys.com \
  --set ingress.tlsSecret=tls-wildcard-ubixsys-prod

# 5. Initialize once — SAVE the recovery keys and root token (shown once).
kubectl -n "$NS" exec -it vault-ubixvault-0 -- ubixvault operator init

# 6. Verify: it auto-unseals from the KEK; readiness turns green once unsealed.
kubectl -n "$NS" exec -it vault-ubixvault-0 -- \
  ubixvault operator seal-status -tls-skip-verify -address https://127.0.0.1:8200
```

Because storage is the database, there is **no data PVC** and the pod can be
rescheduled freely — it reconnects to the same store and auto-unseals. Enable
scheduled backups afterward (see [Backups](#backups)) once you have created a
least-privilege `sys/snapshot` token Secret.

## Design notes

- **StatefulSet.** With file storage: 1 replica and a `volumeClaimTemplate` for
  the encrypted data directory. `replicaCount > 1` is refused unless
  `ha.enabled` (which requires MySQL storage).
- **HA (`ha.enabled`).** Every replica unseals; the one holding a lock in the
  database is active and the others forward to it over the cluster port (8201,
  mutual TLS from a CA the vault manages itself — no certificates to supply).
  The chart then passes `-ha` and the pod IP (`POD_IP`), exposes the cluster
  port, probes readiness with `/v1/sys/health?standbyok=true` (standbys are
  ready: they serve by forwarding), adds a PodDisruptionBudget
  (`maxUnavailable: 1`) and soft pod anti-affinity. A drain or rolling restart
  hands the active role over in about half a second. Enabling it on an existing
  install and upgrading it: follow the order in
  [DEPLOYMENT.md § Upgrades](../../../docs/DEPLOYMENT.md#8-upgrades).
- **Auto-unseal first.** In Kubernetes, Shamir unseal after every restart is
  painful, so auto-unseal (a KEK in a Secret) is the default. Shamir still works
  — set `autoUnseal.enabled=false` and unseal manually via `kubectl exec`.
- **Probes are split on purpose.** A sealed vault returns `503` on
  `/v1/sys/health`. Liveness and startup therefore use `/v1/sys/livez`, which
  answers `200` whenever the process serves, sealed or not, so a sealed-but-alive
  pod is not crash-looped; **readiness** uses `/v1/sys/health`, so only an
  unsealed vault receives traffic (with HA, `?standbyok=true`: standbys too).
- **Kubernetes auth method.** When `kubernetesAuth.enabled` (default), the
  release's ServiceAccount is bound to `system:auth-delegator` so the vault can
  validate pod ServiceAccount tokens via TokenReview.
- **TLS.** With `tls.enabled` (default) the server serves HTTPS from a mounted
  Secret. Disabling TLS requires `devNoTLS=true` and is INSECURE.
- **Audit is off by default** and fail-closed: enable it with a dedicated volume
  (`audit.enabled=true`), never sharing the data disk.
- **Ingress is optional and encrypted end to end.** When `ingress.enabled`, the
  Ingress is annotated `backend-protocol: HTTPS` so ingress-nginx re-encrypts to
  the vault (which serves HTTPS) — the ingress never sees plaintext secrets. A
  secrets manager is sensitive: prefer internal access, or lock it down with
  `ingress.whitelistSourceRange`.
- **Hardened pod:** non-root (uid 65532), read-only root filesystem, all
  capabilities dropped, `RuntimeDefault` seccomp.

## Key values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | 1 unless `ha.enabled`; then 3 is recommended. |
| `ha.enabled` | `false` | Active/standby HA; requires `storage.type=mysql`. |
| `ha.clusterPort` | `8201` | Replica-to-replica port (mutual TLS). |
| `ha.lockTTL` / `ha.retryInterval` | `15s` / `500ms` | Unplanned failover bound / standby lock retry. |
| `ha.podDisruptionBudget.enabled` / `.maxUnavailable` | `true` / `1` | PDB when HA is on. |
| `ha.antiAffinity` | `soft` | `soft`, `hard` or `none`; ignored if `affinity` is set. |
| `terminationGracePeriodSeconds` | `30` | Time to step down and drain on shutdown. |
| `image.repository` | `ghcr.io/cwolsen7905/ubixvault` | Build/push your own; no official image yet. |
| `image.tag` | `""` | Defaults to the chart `appVersion`. |
| `persistence.enabled` / `.size` | `true` / `1Gi` | Encrypted data volume. |
| `autoUnseal.enabled` | `true` | Wrap the master key with a KEK for self-unseal. |
| `autoUnseal.existingSecret` | `""` | Secret holding the KEK (key `auto-unseal-key`). |
| `tls.enabled` | `true` | Serve HTTPS. |
| `tls.existingSecret` | `""` | `kubernetes.io/tls` Secret to mount. |
| `tls.certManager.enabled` | `false` | Issue the cert via cert-manager instead. |
| `devNoTLS` | `false` | Allow plaintext on a non-loopback address (INSECURE). |
| `audit.enabled` | `false` | Fail-closed audit log; use a dedicated volume. |
| `kubernetesAuth.enabled` | `true` | Bind SA to `system:auth-delegator`. |
| `service.type` / `.port` | `ClusterIP` / `8200` | In-cluster Service. |
| `ingress.enabled` | `false` | Expose over an Ingress (requires `tls.enabled`). |
| `ingress.host` | `""` | Hostname, e.g. `vault.prod.ubixsys.com`. |
| `ingress.tlsSecret` | `""` | TLS Secret in this namespace (e.g. a Replikate-synced wildcard). |
| `ingress.whitelistSourceRange` | `""` | Optional CIDR allow-list to restrict access. |

See [`values.yaml`](values.yaml) for the full list.

## Backups

Snapshots are encrypted ciphertext and safe to store at rest; a restore still
requires the unseal shares or the KEK.

### Manual snapshot

```sh
kubectl -n ubixvault-prod exec -it vault-ubixvault-0 -- \
  ubixvault operator snapshot save /var/lib/ubixvault/backup.snapshot
```

### Scheduled off-node backups (`backup.enabled`)

A CronJob runs `operator snapshot save` against the running vault on a schedule
and writes the snapshot to a **separate PVC** — put it on a network-backed
`storageClass` so the backup survives loss of the vault's node. This keeps the
latest snapshot (timestamped history + object-storage upload are a planned
follow-up).

It needs a token authorized for `sys/snapshot`. Create a least-privilege policy
and token, then a Secret:

```sh
# 1. A policy that allows only taking a snapshot.
cat > backup.json <<'EOF'
{"path": {"sys/snapshot": {"capabilities": ["create", "update"]}}}
EOF
curl -sf -H "X-Vault-Token: $ROOT" -X PUT \
  "$VAULT/v1/sys/policies/acl/backup" --data @backup.json

# 2. A token bound to that policy.
TOKEN=$(curl -sf -H "X-Vault-Token: $ROOT" -X POST \
  "$VAULT/v1/auth/token/create" -d '{"policies":["backup"]}' \
  | jq -r .auth.client_token)

# 3. A Secret the CronJob reads.
kubectl -n ubixvault-prod create secret generic uv-backup-token \
  --from-literal=token="$TOKEN"
```

Then enable it:

```sh
helm upgrade vault ./deploy/charts/ubixvault \
  --reuse-values \
  --set backup.enabled=true \
  --set backup.tokenSecret=uv-backup-token \
  --set backup.schedule="0 */6 * * *" \
  --set backup.persistence.storageClass=<network-backed-class>
```

### Restoring (test this before you need it)

Restore is offline — the server must not be running against the target
directory. Copy the snapshot out of the backup PVC, then:

```sh
ubixvault operator snapshot restore -data ./restored /path/to/ubixvault.snapshot
# start the server on ./restored and unseal as usual
```

See the top-level [`docs/DEPLOYMENT.md`](../../../docs/DEPLOYMENT.md).
