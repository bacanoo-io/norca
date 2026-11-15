<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Core operators plan

**Status:** 2026-11-15 · phase 1 merged (see [Phase 1 as built](#phase-1-as-built-2026-11-15)) · phase 2 built on
branches, not merged (see [Phase 2 as built](#phase-2-as-built-2026-11-15)) · phases 2b to 5 not started · clean room
(see [Clean room](#clean-room)) · open questions decided 2026-11-12 (see [Decisions](#decisions)) · checked against
the code 2026-11-15 (see [Checked against the code](#checked-against-the-code-2026-11-15))

Four operators that are built into the Norca servers and use only the core of Norca: the native mTLS
CA, the secret store, CSI volumes and snapshots, namespaces, quotas, ACL and network rules. Nothing
of them runs outside the servers.

One part is deliberately **not** in the core: the ACME client. Public certificates (Let's Encrypt and
any RFC 8555 CA) are issued by an operator of the norca-platform repository, `norca-acme`, and the
DNS-01 challenges go through `norca-provider`, which already keeps the credentials of the platforms.
The core keeps what decides security: the private keys, the names a namespace may ask for, and the
secret written for the consumers. See [ACME in norca-platform](#acme-in-norca-platform).

| Kind | Does | Runs in |
| --- | --- | --- |
| [`CertificateIssuer`](#1-certificate) | Who signs: the built-in issuing CA, or an external issuer (ACME) | core (registered by the servers) |
| [`Certificate`](#1-certificate) | A TLS certificate for DNS names, kept issued and renewed in the secret store | core |
| [`CertificateOrder`](#external-issuers-certificateorder) | One issuance handed to an external issuer: the CSR in, the chain out | core writes, the issuer's operator fulfils |
| [`VolumeSnapshotSchedule`](#2-volumesnapshotschedule) | Takes CSI snapshots of a volume on a schedule and deletes old ones | core |
| [`CSIVolumeRequest`](#3-csivolumerequest) | Keeps a CSI volume as declared: creates, expands, deletes | core |
| [`Tenant`](#4-tenant) | One object for a namespace with its quota, ACL policies and roles, network rules | core |
| `DNSZone` | A zone a provider of `norca-provider` can write TXT records in | norca-platform (registered by norca-provider) |

## Clean room

Sources: Norca's own requirements and the Norca trees (the custom object framework, the release
manager as the pattern of a built-in operator, the mTLS CA, the secret store, the CSI endpoints, the
Traefik pack; in norca-platform the provider framework, its contracts and registrar, the LoadBalancer
operator as the pattern of an external one), and public specifications: X.509 (RFC 5280), PKCS #10
(RFC 2986), ACME (RFC 8555), ACME renewal information (RFC 9773), DNS dynamic update and TSIG
(RFC 2136, RFC 8945), the CSI specification, the public API documentation of the DNS services and the
public documentation of ACME CAs (rate limits, staging endpoints). The ACME protocol client is
`golang.org/x/crypto/acme` (BSD-3-Clause). Kubernetes ecosystem projects and the ACME clients of
proxies with similar features are not read for the design or the code. The work is independent
development under the source rules: its commits end with the trailer
`Source-Rule: independent-development` (the rule of 2026-11-14; the words "clean room" are not
written into new commits), and the similarity audit covers them like every Norca file.

## Checked against the code (2026-11-15)

The plan was written on 2026-11-12. What it builds on is still there under the same names
(`establishLeadership` starting `nodeScalingKindsLoop` and `releaseLeaderLoop`, `registerKinds`,
`isManagedObjectKind`, `releaseManager.writeObject`, `recordObjectEvents`, `minNorcaFeaturesVersion`,
the secret watcher's `SecretExpiring`, `mtls_roots`, the snapshot, expand and delete RPCs of
`CSIVolume`); the path `/v1/certificates/roots` is free. What came since, and what it means here:

- **The system bus and change data capture.** Events of the topic `CustomObject` go to the bus and to
  CDC readers as they are: only job and service events are redacted. So **no spec or status field of
  these kinds may hold a secret**. The plan keeps to it (keys and CSI secrets are in the secret store,
  an order holds a CSR and a public chain, ACME account keys and EAB credentials are in the secrets of
  `norca-acme`), and two fields need the rule spelled out when they are built: `external.config` of
  a `CertificateIssuer` and `parameters` of the two volume kinds are refused or documented as public.
- **Observers.** The loops are leader loops, so observers and standbys run none of them; nothing to
  change.
- **Admission policies for objects** exist now (`admission.ObjectRuleEngine` in the object endpoint).
  The checks of this plan (names against the allow list, the writer's rights on the secret path and
  the volume) are built-in checks of the kinds in the same endpoint, before the admission policies of
  the operator, which can then add their own rules for these kinds.
- **Rig.** The `norca-acme` pack and the Traefik pack change of phase 2 would be written as packs in
  norca-registry while the package manager that replaces packs is being designed. Phase 1 does not
  touch the registry.
- **An audit trail** (an idea of 2026-11-15, HANDOVER): issuing, revoking and tenant changes are what
  such a trail must show with who asked for them; the object events of these operators should carry
  the accessor of the writer where there is one.

## Phase 1 as built (2026-11-15)

On the branches `feat/operators-certificates` (norca), `certificates-docs` (norca-doc) and
`case-certificates` (norca-lab); not merged. Where the code differs from the text below, the code is
what holds:

- **Files.** `operators.go` (the loop of the built-in operators: kinds, watch set and timer, status
  writes, events, conditions), `certificate_operator.go`, `certificate_ca.go`, `certificate_kinds.go`,
  `certificate_checks.go` (the checks in the object endpoint), `certificate_endpoint.go` (RPC
  `Certificate`), `structs/certificates.go`, `state/state_store_certificates.go`,
  `fsm_certificates.go`.
- **The issuing CA** is a Raft table of its own, `certificate_authorities` (message and snapshot type
  117), not a row of `mtls_roots`: nothing that reads the mesh's trust bundle can pick it up. Life of a
  CA the leader creates: 5 years, rotated at 2/3. **A retired CA keeps its key** (encrypted) until its
  certificates have expired, because its revocation list still has to be signed.
- **Revocation.** The revoked serials are in the row of the CA (`Revoked`), not in the status of an
  issuer: several built-in issuer objects share the one CA. Added to the plan: a signed revocation list
  (RFC 5280) at `/v1/certificates/crl` and `norca certificate crl`, since a list nobody can fetch is
  of no use. No distribution point in the certificates, no OCSP.
- **The key of an open order** waits as the item `next_key` of the certificate's own secret, next to
  the current `cert` and `key`: the status can hold no secret (object events are not redacted), and
  the secret is what the next leader finds.
- **What a namespace may ask for** is in the metadata of the namespace until `Tenant` exists:
  `norca.io/certificate-dns-suffixes`, `norca.io/certificate-ip-addresses`,
  `norca.io/certificate-issuers` (comma separated). `Tenant` (phase 5) will write these.
- **The secret is the certificate's own** by the label `norca.io/certificate=<name>`; another secret
  at the path gives `SecretConflict` and is never overwritten. An existing secret is handed over by
  setting the label (the explicit adoption of decision 9).
- **External issuers.** The controller owns the whole status of its issuer: `heartbeat` (stale after
  3 minutes) and `roots`, the roots its chains must lead to. The core writes no condition there.
  A revocation is an order of its own, `<ns>.<name>.revoke-<serial>`, dropped after 24 hours.
- **Conditions and metrics.** `Ready`, `Issuing`, `Renewing`, `Failed`, `IssuerUnavailable`,
  `Degraded`. The metrics store already counts the objects of every kind and the degraded ones
  (`norca.objects.count`, `norca.objects.degraded`), so there is no `norca.operator.*` family; new
  are `norca.certificate.ready` and `norca.certificate.expires_hours` (labels `ns`, `name`).
- **Commands:** `norca certificate roots | crl | revoke <name> | ca import | ca rotate` (the name
  with `-namespace`, not `<ns>/<name>`).
- **Objects in HCL** are written `kind = "norca.io/Certificate"`, not `group` and `kind` apart as in
  the examples below.
- **Lab:** a step at the end of the case `traefik`: the built-in certificate takes over the secret
  Traefik serves, is verified through both instances against `norca certificate roots`, revoked and
  served again; an external issuer is fulfilled by an openssl stub.

Phase 1 is merged (norca `c14b0f195`, 2026-11-15), with its web UI: a Certificates page (certificates
by expiry, issuers, open orders, the issuing CAs with the trust bundle and the revocation lists), a
panel on the page of a `Certificate` object with Revoke, and the link from a secret back to its
certificate. Found in the lab: a wildcard right under a top level name (`*.lab`) is issued as asked
for, and TLS clients refuse it; documented under the limits, not refused by the servers.

## Phase 2 as built (2026-11-15)

On the branches `feat/dns-contract` and `feat/norca-acme` (norca-platform), `norca-acme-pack`
(norca-registry), `acme-docs` (norca-doc) and `case-acme` (norca-lab); not merged. Where the code
differs from the text below, the code is what holds:

- **The DNS contract** (`provider/dns`): `PUT` and `DELETE {endpoint}/v1/txt/{name}` with the value
  in the body, served by norca-provider at `/zones/<zone>`, with `dnstest` as the conformance check.
  It is narrower than "present and clean up a TXT record": **a name is a challenge name**,
  `_acme-challenge.<name of the zone>`, so the token of a zone cannot change what the zone serves.
  There is no call that lists the zones: the registrar publishes them.
- **`DNSZone`** is registered by the norca-provider pack (`dnszone.kind.hcl`), not by the core; one
  object per zone of a `dns` section, with the heartbeat of the other registrations. The spec names
  the zone, the provider, the endpoint and the token Variable.
- **DNS types:** `rfc2136` (one update message over TCP per call, TSIG `hmac-sha256/384/512`; through
  `github.com/miekg/dns`, BSD-3-Clause, the version the core uses) and `mock`. A platform of its own
  directory, DNS only. The cloud types (phase 2b: `scaleway`) are not there.
- **The operator** (`certificate/acme`, `cmd/norca-acme`) is a loop over the issuers and the orders
  every `-interval` (5 s), not a controller of the SDK: the kinds are cluster scoped and the work is
  a state machine per order. The issuer configuration is `spec.external.config`: `directory`,
  `email`, `eab_secret`, `roots`, `http01`, `dns01 { zones, disabled }`.
- **Roots.** The servers accept a chain only if it leads to a root the issuer declared. The operator
  verifies the chain against `roots` of the configuration, or against the roots of the system it
  runs on, and **declares the root the chain ended in** in the status of the issuer before it hands
  the chain on. A public certificate authority needs no configured root.
- **Progress.** The status of the order holds the URL of the order at the certificate authority, the
  attempts with the time of the next one, and the DNS records that were presented. The proofs are
  derived from the account key when needed; nothing secret is in an object.
- **Waiting for DNS.** The operator tells the certificate authority to look only when every name
  server of the zone (found through a recursive resolver) answers the record; a record that does not
  appear within `-dns-propagation-timeout` fails the attempt. A zone whose provider has no heartbeat
  is waited for, and a name that is not a wildcard is proven over HTTP meanwhile.
- **Retries:** 1 minute doubled up to 2 hours, 8 attempts, then the order fails and the servers
  place a new one with their own delay. A refusal of the certificate authority fails the order at
  once, with its problem.
- **Revocation** needs the certificate itself, and the order holds only the serial: the operator
  keeps the leaf certificates it issued in its own secrets (`norca-acme/issued/<issuer>/<serial>`)
  until they expire.
- **Renewal information** (RFC 9773) is read with a request of the operator's own (the ACME client
  does not know the resource): a time in the suggested window, passed on as `renew_after` when it is
  within the life of the certificate.
- **The Traefik pack** (`acme_challenge = true`) replaces the redirection of the http entrypoint with
  a router of the dynamic configuration that leaves the challenge path out (`!PathPrefix`, priority
  above every other router); the service of norca-acme carries the router of the path as catalog
  tags. One instance of the operator: the HTTP-01 answers are in its memory.
- **Tests.** Unit tests with a certificate authority of the test; `certificate/acme/testdata/pebble`
  runs the operator against Pebble and a BIND name server in containers (HTTP-01, wildcard DNS-01,
  renewal information, revocation): passed on 2026-11-15. Lab: a step at the end of the case
  `traefik` with Pebble and BIND as containers on nt-r1-dc3 and the three packs of the working trees.

Left of phase 2: the certificates of the ACME step are not served by Traefik in the lab (the pack
reads its certificate secrets when it is deployed); the pack registry page of the docs lists the
packs of a tag, so norca-acme appears there with the next registry tag.

## Common design

**Where they run.** In the servers, on the leader only, like the release manager: each operator is a
leader loop started in `establishLeadership` (next to `releaseLeaderLoop` and `nodeScalingKindsLoop`)
that watches its objects with a memdb watch set (`CustomObjects` of its kind) and a timer. No process,
job or pack has to be installed, and the operators upgrade with the servers.

**Kinds.** Registered by the servers like the node scaling kinds (`registerKinds`, retried until every
server is new enough, `minNorcaFeaturesVersion`), in the group `norca.io` (which already holds
`NodeGroup` and `NodeProvider`). The kind definitions are protected (`isManagedObjectKind`); the
objects are written by users through the generic object API, as for NodeGroups. JSON Schema and CEL
rules validate the spec when it is written.

**Status.** Only the operator writes the status (`ManagedBy = "norca-operators"`), with a CAS on
ModifyIndex like `releaseManager.writeObject`; conditions as in the releases (`Ready`, `Degraded`,
kind specific ones) with `observedGeneration`. Object events (`recordObjectEvents`) record what the
operator did; the custom object topic of the event stream carries the changes. Work that spans a
leader change (a pending order, a pending snapshot) keeps its progress in the status, so the next
leader continues it instead of starting over.

**Deletion.** A finalizer `norca.io/operators` on objects whose deletion has work to do (deleting a
volume, the resources of a tenant, the snapshots of a schedule with `delete_snapshots = true`); the
operator removes it when done. Owner references only between custom objects (objectgc).

**Rights.** The operators act with the server's rights, so each kind restricts what a writer of its
objects may cause: the object ACL of the kind (object capabilities `read`/`write` per namespace), plus
checks of the referenced resources against the writer's token when the object is written (an
admission check in the object endpoint, as the release endpoint does for its resources). Details per
kind below.

**Metrics.** Leader gauges in the metrics store (`norca.operator.*`, labels `kind`, `ns`): objects,
objects not ready; per kind a few counters (certificates issued, orders failed, snapshots taken,
failures).

## 1. Certificate

### Issuers

```hcl
group = "norca.io"
kind  = "CertificateIssuer"           # cluster scoped
name  = "internal"

spec {
  builtin {}                          # the issuing CA of the region (decision 1)
}
```

```hcl
group = "norca.io"
kind  = "CertificateIssuer"
name  = "letsencrypt"

spec {
  external {
    controller = "norca-acme"         # the operator that fulfils the orders of this issuer
    config = {                        # opaque to the core; validated by the operator (see norca-acme)
      directory = "https://acme-v02.api.letsencrypt.org/directory"
      email     = "ops@example.com"
      agree_tos = true
      dns01     = { zones = ["example.com"] }
    }
  }
}
```

- **Built-in issuer.** Certificates of the cluster's issuing CA, for names inside the organisation
  (internal services, admin endpoints, the trust anchor distributed to clients). Signed by the
  leader; trust bundle at `/v1/certificates/roots`.
- **External issuer.** The core does not sign; it hands each issuance to the named controller as a
  `CertificateOrder`. The issuer is `Ready` while its controller reports a heartbeat in the issuer's
  status (as norca-provider does for its registrations); without one, certificates of the issuer get
  the condition `IssuerUnavailable` and keep their current certificate.
- **Default issuers:** none.

### Certificates

```hcl
group = "norca.io"
kind  = "Certificate"
name  = "web-tls"          # namespace of the object: where the secret is written

spec {
  issuer       = "letsencrypt"
  dns_names    = ["web.example.com", "*.web.example.com"]
  ip_addresses = []                   # built-in issuer only
  secret_path  = "certs/web"          # in the namespace of the object
  validity     = "2160h"              # built-in issuer only; ACME CAs choose
  renew_before = ""                   # empty: see decision 4
  key_type     = "ecdsa-p256"         # ecdsa-p256 | ecdsa-p384 | rsa-2048 | rsa-4096 | ed25519 (built-in)
}
```

- **Issuing.** The leader generates the key and the CSR. The built-in issuer signs right away; for an
  external issuer the leader writes a `CertificateOrder` and waits for its chain. When the chain is
  there, the leader checks it (the public key is the CSR's, the names are exactly the requested ones,
  the chain verifies to a root the issuer's controller declared) and writes a new version of
  `secret_path` with the items `cert` (the chain), `key` and `ca` (the trust bundle, built-in issuer),
  and `SecretMeta.ExpiresAt` = NotAfter, so the secret watcher warns (`SecretExpiring`) if renewing
  ever stops. **The private key never leaves the servers**: the external operator sees the CSR and the
  public chain only.
- **Renewing.** Before NotAfter (at the time an external issuer suggests, see ARI below), on a changed
  spec (names, key type, issuer) and when the built-in CA rotates; a new key and a new secret version
  each time. Consumers that read the secret through templates (`nomadSecret`, the Traefik pack's
  `tls_certificates`) pick it up without a restart. The current certificate stays in place while a
  renewal fails.
- **Status:** NotBefore, NotAfter, serial, the secret version, the issuer, the renewal time, the open
  order, `Ready`, `Issuing` / `Renewing` / `Failed` / `IssuerUnavailable` conditions.
- **Who may ask for which names.** Checked by the core for every issuer, before any CSR: with the
  built-in issuer any name would be trusted by the cluster's clients, and with ACME the edge and the
  DNS zones answer the challenges of every namespace, so one tenant could otherwise get a certificate
  for another's name. Names are allowed by an allow list of DNS suffixes per namespace (decision
  3), issuers by a list per namespace (set by a `Tenant`); IP addresses only if listed. The
  writer also needs secret write on `secret_path`.
- **Revocation.** `norca certificate revoke <ns>/<name>` revokes the current certificate (the built-in
  CA's revoked list in its status, or a revocation order for an external issuer) and issues a new one.
- **Deletion:** the secret stays, unless `delete_secret = true`.

### External issuers: CertificateOrder

```hcl
group = "norca.io"
kind  = "CertificateOrder"            # cluster scoped, written by the servers only
name  = "team-a.web-tls.7"

spec {
  issuer      = "letsencrypt"
  certificate = "team-a/web-tls"
  csr         = "-----BEGIN CERTIFICATE REQUEST-----..."
  dns_names   = ["web.example.com", "*.web.example.com"]
  revoke      = ""                    # serial, for a revocation order
}

status {                              # written by the issuer's controller
  state       = "valid"               # pending | processing | valid | failed
  chain       = "-----BEGIN CERTIFICATE-----..."
  renew_after = "2027-01-20T00:00:00Z"   # optional: the CA's suggestion (ARI)
  problem     = ""                    # the CA's error
}
```

- The contract between the core and any external issuer: the servers create the order (spec
  immutable), the controller of the issuer fulfils it by writing the status, the servers read the
  chain, write the secret and delete the order. One open order per certificate.
- The controller's token needs object read of `CertificateIssuer` and `CertificateOrder` and status
  write of `CertificateOrder` only, plus its own secrets: no secret access to the namespaces.
- Other issuers than ACME (an enterprise CA with its own API, a cloud CA service) can be controllers
  of the same contract later.

### ACME in norca-platform

A new operator `norca-acme` in norca-platform (`certificate/acme/`, `cmd/norca-acme/`), a job like
the LoadBalancer operator, with a pack in norca-registry.

- **ACME client.** For every `CertificateIssuer` with `controller = "norca-acme"` it registers the
  account (key and EAB credentials in its own secrets, `norca-acme/accounts/<issuer>`), and for every
  pending `CertificateOrder` of such an issuer it places the order with the CSR, solves the
  challenges, finalizes and writes the chain. Progress (order URL, authorizations) is in the order's
  status, so a restarted operator continues. Exponential backoff per order (minutes to hours), because
  of the CAs' rate limits; the CA's problem document goes to the order's `problem`. ACME renewal
  information (RFC 9773) is read when the CA offers it and passed on as `renew_after`.
- **HTTP-01.** The operator's job serves `/.well-known/acme-challenge/<token>` itself on a service
  with Traefik tags (a router on the http entrypoints with the highest priority). The Traefik pack's
  redirect to https must leave this path out (a pack change). Only for names whose DNS points to the
  edge; no wildcards.
- **DNS-01** through `norca-provider`: a third contract next to node and load balancer,
  `provider/dns/` (present and clean up a TXT record, list the zones), with a `dnstest` conformance
  check like `lbtest`. The registrar publishes a `DNSZone` object per zone a provider serves
  (heartbeat as for the classes). The issuer's `dns01.zones` (or `dns01.provider`) selects them; the
  solver per name is `dns01` when a zone covers it (always for wildcards), otherwise `http01`.
- **DNS types of norca-provider:** `rfc2136` (dynamic update with TSIG: BIND, Knot, PowerDNS and
  others; a platform of its own directory like `nginx`), `mock` (tests), and the DNS API of the cloud
  platforms that are there already (aws Route 53, gcp Cloud DNS, hcloud, digitalocean, vultr,
  scaleway), each a `dns.go` in its directory with the platform's existing credentials. Which cloud
  types come first: `scaleway` (decision 2).
- **TLS-ALPN-01** is not supported.
- Why outside the core: the servers need no outbound access to ACME CAs or DNS APIs, the DNS
  credentials stay in norca-provider, the ACME and provider code upgrades without the servers, and
  the core keeps a small, CA-neutral contract.

## 2. VolumeSnapshotSchedule

```hcl
group = "norca.io"
kind  = "VolumeSnapshotSchedule"
name  = "db-nightly"

spec {
  volume    = "db-data"               # CSI volume ID in the namespace of the object
  schedule  = "0 3 * * *"             # cron, in time_zone
  time_zone = "UTC"
  keep {
    last    = 7                       # at least one of last / max_age
    max_age = "720h"
  }
  name_prefix      = "db-nightly"
  parameters       = {}               # passed to CreateSnapshot
  secret_path      = ""               # optional: CSI secrets from the secret store
  delete_snapshots = false            # on deletion of the schedule
  suspend          = false
}
```

- **Taking snapshots.** At each cron time the leader calls `CSIVolume.CreateSnapshot` for the volume
  (plugin with `CREATE_DELETE_SNAPSHOT`, otherwise the condition `Unsupported`). A missed time (leader
  change, server down) is taken once, not repeated. Snapshots are crash-consistent; quiescing the
  application is out of scope (decision 7).
- **The list of snapshots** lives in the status (snapshot ID, size, time, ready), because Norca does not
  store snapshots in its state. Retention deletes the oldest beyond `keep` with
  `CSIVolume.DeleteSnapshot`; only snapshots of this schedule (listed in its status) are ever deleted.
- **Status:** last and next run, the snapshot list, `Ready`, `Failed` with the plugin's error,
  counters of taken and deleted snapshots.
- **Rights:** the writer needs `csi-write-volume` in the namespace; checked when the object is written.

## 3. CSIVolumeRequest

```hcl
group = "norca.io"
kind  = "CSIVolumeRequest"
name  = "db-data"                     # also the CSI volume ID in the namespace of the object

spec {
  plugin_id     = "seaweedfs"
  mode          = "create"            # create | register (an existing external volume)
  external_id   = ""                  # with mode = "register"
  capacity_min  = "10GiB"
  capacity_max  = "20GiB"
  capability {
    access_mode     = "single-node-writer"
    attachment_mode = "file-system"
  }
  mount_options { fs_type = "ext4" }
  topology_request { ... }
  parameters      = {}
  secret_path     = ""                # CSI secrets from the secret store, not in the object
  snapshot_id     = ""                # create from a snapshot
  clone_id        = ""
  deletion_policy = "retain"          # retain | delete
  adopt           = false
}
```

- **Reconcile.** A missing volume is created (or registered); a raised `capacity_min`/`capacity_max`
  goes through the existing expand path (controller and node expansion); other changes of fields that
  CSI can not change are refused by the kind's CEL rules (immutable after creation). The volume's
  live state (phase 4 of the CSI work) and node usage (phase 2) are summarised into the status.
- **Ownership.** CSI volumes have no owner field. A `CSIVolumeRequest` manages the volume of its name; a volume
  of that ID that exists already is taken over only with `adopt = true` (decision 9). Changes
  made to a managed volume with `volume register` / `volume create` are reported as drift (condition
  `Drifted`) and set back at the next reconcile.
- **Deletion:** with `deletion_policy = "delete"` the finalizer waits until no allocation claims the
  volume, then deletes it (`CSIVolume.Delete`); with `retain` the volume stays registered and only
  leaves the operator's care.
- **Secrets** come from `secret_path` at each call, so the object never holds them.
- **Rights:** `csi-write-volume` in the namespace; `csi-register-plugin` is not needed.

## 4. Tenant

```hcl
group = "norca.io"
kind  = "Tenant"                      # cluster scoped
name  = "team-a"

spec {
  namespace {
    name        = "team-a"
    description = "Team A"
    meta        = {}
  }
  quota {                             # a quota spec of its own, attached to the namespace
    region "global" { cpu = 8000, memory = 16384 }
  }
  roles {                             # ACL policies and a role per entry, scoped to the namespace
    developer = ["submit-job", "read-logs", "csi-mount-volume", "secrets-read"]
    viewer    = ["read-job"]
  }
  network {
    isolation = "namespace"           # none | namespace (default deny from other namespaces)
    allow_from_namespaces = ["ingress"]
  }
  certificate_dns_suffixes = ["team-a.example.com"]   # allow list for Certificate (see 1)
  certificate_issuers      = ["letsencrypt"]          # issuers the namespace may use; empty: all
}
```

- **Reconcile.** The operator creates and keeps the namespace, a quota spec, ACL policies (one per role,
  generated from capability lists in the namespace) and ACL roles, and the network policies of the
  isolation, through the existing RPCs with the server's rights. What it created is listed in the
  status (as releases list their resources, because these types have no owner field); a resource of
  the same name that it did not create is not touched (condition `Conflict`).
- **Drift:** changes to the managed resources made through their own APIs are set back at the next
  reconcile (interval as the release drift timer, 5 minutes), and reported as events.
- **Deletion:** the finalizer refuses while the namespace has jobs or volumes (condition
  `NamespaceNotEmpty`), then deletes what the tenant created, the namespace last.
- **Rights:** writing a Tenant is a management operation (it creates ACL policies): only a management
  token, or object write on `norca.io/Tenant`, which an operator must grant knowingly.
- A release (pack) that manages the same namespace and a tenant must not both own it: the one written
  second gets `Conflict`.

## Phases

| Phase | Repos | Content |
| --- | --- | --- |
| 1 | norca | Common part: the kinds registered, the leader loop skeleton, status writing, finalizer handling, metrics. `CertificateIssuer` (built-in and external), `Certificate`, `CertificateOrder` and its checks, the allow lists, renewal, revocation, the secret store output. Lab: a built-in certificate consumed by the Traefik pack, renewed; an external issuer fulfilled by a lab stub. |
| 2 | norca-platform, norca-registry | `norca-acme`; the DNS contract of norca-provider with `rfc2136` and `mock`, `DNSZone` registration; the `norca-acme` pack and the Traefik pack's redirect exception. Lab: Pebble (the test ACME CA), a BIND or Knot container for RFC 2136, an HTTP-01 and a wildcard DNS-01 certificate served by Traefik. No public CA in the lab. |
| 2b | norca-platform | The `scaleway` DNS type (Scaleway Domains and DNS API, the platform's existing credentials) with `dnstest`. The final test runs on Scaleway: a Norca cluster there, Traefik behind a Scaleway load balancer, Let's Encrypt staging with an HTTP-01 certificate and a wildcard DNS-01 certificate in a Scaleway zone, renewal, then one certificate from the production directory. The other cloud DNS types later, on demand. |
| 3 | norca | `VolumeSnapshotSchedule`: cron, retention, status list. Lab: labcsi gets CREATE_DELETE_SNAPSHOT, and the SeaweedFS driver if it supports snapshots. |
| 4 | norca | `CSIVolumeRequest`: create/register, expand, adopt, deletion policy, drift. Lab: labcap and SeaweedFS volumes. |
| 5 | norca | `Tenant`: namespace, quota, roles, network isolation, certificate suffixes and issuers, deletion. Lab: two tenants, isolation checked across them. |

Each phase is its own branch and pull request per repo, with docs (feature page per kind, object
kinds reference, the norca-acme and DNS provider pages), a lab step and the design updated. Phases 1
and 2 are released together, so the `Certificate` feature ships with ACME.

## Decisions

All decided by the maintainer on 2026-11-12. Development has not started.

1. **The built-in issuing CA:** a separate issuing CA per region in Raft (like `mtls_roots`: ECDSA,
   rotation at 2/3 of its life, the old one kept for a grace period), trust bundle at
   `/v1/certificates/roots`; an external CA's certificate and key can be imported in its place. Not
   the native mTLS CA (its leaves carry a SPIFFE URI and its trust bundle is the mesh's).
2. **Cloud DNS types:** `scaleway` first, and the final test of the certificate feature runs on
   Scaleway (phase 2b); the other platforms on demand.
3. **Who may ask for which DNS names:** an allow list of DNS suffixes per namespace (set by a `Tenant`,
   or by a management token on the namespace's meta), nothing allowed by default, for every issuer;
   IP addresses only if listed, built-in issuer only.
4. **Validity and renewal:** built-in 90 days (maximum 1 year); renewal at 2/3 of the validity unless
   `renew_before` is set or the external issuer gives `renew_after`. Nothing assumes 90 days for ACME.
5. **Kind names:** `norca.io/CertificateIssuer`, `Certificate`, `CertificateOrder`,
   `VolumeSnapshotSchedule`, `CSIVolumeRequest`, `Tenant`, `DNSZone`. Not `Volume` (it reads like the
   CSI volume API) and not `CSIVolumeClaim` (an allocation's claim of a volume: `structs.CSIVolumeClaim`,
   `CSIVolume.Claim`, `csi_volume_claim_gc_threshold`).
6. **Issuers** are cluster scoped only; namespaces get the issuers they may use from their `Tenant`
   (`certificate_issuers`).
7. **Snapshots** are crash-consistent only; no hook jobs.
8. **Tenant roles:** capability lists per role, plus an optional full `policy` HCL for special cases.
9. **Adopting existing volumes and resources:** only explicitly, `adopt = true` (`CSIVolumeRequest`) or
   `adopt = ["quota", ...]` (`Tenant`); otherwise a resource of the same name is left alone and the
   object gets `Conflict`.
10. **Order of the phases:** as in the table: certificates (1, 2, 2b), snapshot schedules (3),
    `CSIVolumeRequest` (4), `Tenant` (5).
