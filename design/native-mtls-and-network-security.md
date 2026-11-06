<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Native mTLS PKI, SPIFFE Workload Identity, and Network Policy Architecture

## 1. Executive Summary & Architectural Principles

Norca introduces a native, zero-external-dependency workload identity and microsegmentation subsystem comprising a built-in SPIFFE X.509 Certificate Authority (`norca/norca/mtls_ca.go`), declarative L4/L7 Service Intentions (`norca/norca/service_intention_endpoint.go`), and kernel-enforced L3/L4 Network Policies (`norca/norca/network_policy_endpoint.go` and `norca/client/allocrunner/netpolicy/`).

Whereas standard Nomad deployments require an external control plane (such as HashiCorp Consul) and an external CNI policy engine (such as Calico or Cilium) to achieve mutual TLS and network isolation, Norca embeds cryptographic workload identity, intention-based authorization, and `nftables` network namespace microsegmentation directly into the orchestrator's Raft state machine and client allocation lifecycle.

### Key Architectural Tenets:
1. **Zero External Control-Plane Dependencies**: Every Norca region operates an autonomous, Raft-backed X.509 Certificate Authority (`mtlsCA`) and intention store. No Consul agents, external PKI vaults, or gRPC xDS control planes are required.
2. **Private Keys Never Traverse the Network**: Client nodes generate ephemeral ECDSA P-256 key pairs locally per task (`norca/client/allocrunner/taskrunner/mtls_hook.go:526`) and submit only a PKCS#10 Certificate Signing Request (CSR) alongside the task's signed JWT Workload Identity token (`MTLSSignRequest`). Server nodes derive the SPIFFE ID and DNS Subject Alternative Names (SANs) strictly from the authoritative Raft allocation state—never trusting attributes inside the CSR.
3. **Keyring-Wrapped CA Persistence**: Each regional CA private key is PKCS#8-encoded and encrypted using the active root key of Norca's cryptographic keyring (`s.encrypter.Encrypt`) before being committed to Raft (`mtls_roots` table). CA keys are automatically re-wrapped when the keyring rotates and are seamlessly preserved across encrypted cluster backups.
4. **Multi-Region Federation & SNI Passthrough Gateways**: Regional leaders periodically federate public CA certificates across WAN-joined regions (`MTLS.Roots`). Cross-region traffic flows through Envoy SNI passthrough gateways (`mode = "gateway"`), ensuring end-to-end mutual TLS verification without terminating or decrypting traffic at intermediate region boundaries.
5. **Two-Tier Kernel Microsegmentation via `nftables`**: L3/L4 network isolation is programmed directly into each allocation's Linux network namespace (`table inet norca`) via netlink (`github.com/google/nftables`). Cluster-wide `AdminNetworkPolicy` rules (supporting `allow`, `deny`, and `pass` verdicts in priority order) execute ahead of namespace-scoped `NetworkPolicy` rules, backed by scheduler feasibility checking (`norca.network_policy.nftables = "true"`) and per-rule packet drop telemetry.

---

## 2. System Architecture & Data Path Diagrams

### 2.1 Regional PKI, Keyring Wrapping, and Cross-Region Trust Federation

```
+---------------------------------------------------------------------------------------------------+
|                                   NORCA REGION A (us-east)                                        |
|                                                                                                   |
|  +-----------------------------+       Encrypts / Decrypts       +-----------------------------+  |
|  |   Active Keyring Root Key   | ------------------------------> |     mtlsCA Leader Loop      |  |
|  |   (AES-GCM / Root Key ID)   |                                 |   (norca/norca/mtls_ca.go)  |  |
|  +-----------------------------+                                 +--------------+--------------+  |
|                                                                                 |                 |
|                                           Raft Apply (MTLSRootsUpsertRequestType = 97)            |
|                                                                                 v                 |
|  +---------------------------------------------------------------------------------------------+  |
|  |                              MemDB Table: mtls_roots                                        |  |
|  |  +-------------------------------------------+  +----------------------------------------+  |  |
|  |  | Local Active & Retired Roots (us-east)    |  | Federated Foreign Roots (eu-west, ...) |  |  |
|  |  | - RootCert (PEM, URI: spiffe://global...) |  | - RootCert (PEM, Public Certificate)   |  |  |
|  |  | - SigningKey (Ciphertext, PKCS#8 P-256)   |  | - SigningKey: nil (Never leaves owner) |  |  |
|  |  | - SigningKeyID (Keyring Root Key UUID)    |  | - Active / NotBefore / NotAfter        |  |  |
|  |  +-------------------------------------------+  +----------------------------------------+  |  |
|  +---------------------------------------------------------------------------------------------+  |
+-------------------------------------------------|-------------------------------------------------+
                                                  |
                        Periodic WAN RPC          |  Blocking Query (MTLS.Roots) &
                        MTLS.Roots (Public Only)  |  CSR Signing (MTLS.SignCertificate)
                                                  |
                    +-----------------------------+-----------------------------+
                    |                                                           |
                    v                                                           v
+---------------------------------------+                   +---------------------------------------+
|       NORCA REGION B (eu-west)        |                   |           NORCA CLIENT NODE           |
|  Replicates us-east public RootCerts  |                   |     (taskrunner/mtls_hook.go)         |
|  into its local mtls_roots table so   |                   |  1. Generates ECDSA P-256 key locally |
|  eu-west workloads verify us-east     |                   |  2. Sends JWT IdentityToken + CSR     |
|  SPIFFE certificates natively.        |                   |  3. Writes key.pem, cert.pem,         |
+---------------------------------------+                   |     bundle.pem into ${NORCA_SECRETS}  |
                                                            +---------------------------------------+
```

### 2.2 Client-Side Certificate Lifecycle & Dual-Mode Task Injection (`app` vs `sidecar`)

```
+---------------------------------------------------------------------------------------------------+
|                                   NORCA CLIENT ALLOCATION                                         |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  | TaskRunner: mtlsHook (norca/client/allocrunner/taskrunner/mtls_hook.go)                     |  |
|  | - Watches MTLS.Roots via blocking query (5m wait)                                           |  |
|  | - Renews leaf cert at: T_renew = NotBefore + (2/3 * Lifetime) - Jitter(0..5%)               |  |
|  | - Immediately re-issues leaf cert (with 0..5s splay) if ActiveRootID rotates                |  |
|  | - Atomically writes into ${NORCA_SECRETS_DIR}/mtls/:                                        |  |
|  |     ├── key.pem    (ECDSA P-256 PKCS#8 private key, written FIRST)                          |  |
|  |     ├── cert.pem   (X.509 leaf cert: URI SAN = SPIFFE ID, DNS SANs = *.service.*.norca)     |  |
|  |     └── bundle.pem (Combined PEM of all active/retired local & federated CA roots)          |  |
|  +--------------------+---------------------------------------------------+--------------------+  |
|                       |                                                   |                       |
|        mode = "app"   |                                  mode = "sidecar" |                       |
|                       v                                                   v                       |
|  +-----------------------------------------+         +-----------------------------------------+  |
|  | Application Task                        |         | Injected Prestart Sidecar Task          |  |
|  | - Reads $NORCA_MTLS_CERT_FILE,          |         | Kind: "mtls-proxy:<service>"            |  |
|  |   $NORCA_MTLS_KEY_FILE,                 |         | Hook: mtlsEnvoyHook (mtls_envoy_hook.go)|  |
|  |   $NORCA_MTLS_CA_FILE,                  |         | - Watches ServiceIntention.List         |  |
|  |   $NORCA_MTLS_SPIFFE_ID                 |         | - Watches Service Discovery Catalog     |  |
|  | - Uses Go SDK (norca/api/mtls) or       |         | - Atomically writes file-based xDS into |  |
|  |   triggers change_mode ("noop",         |         |   ${NORCA_SECRETS_DIR}/envoy/:          |  |
|  |   "signal", or "restart") on rotation   |         |   bootstrap.json, lds.json, cds.json,   |  |
|  +-----------------------------------------+         |   sds-cert.json, sds-trust.json,        |  |
|                                                      |   eds-*.json, sds-trust-*.json          |  |
|                                                      +--------------------+--------------------+  |
|                                                                           |                       |
|                                                                           v                       |
|                                                      +-----------------------------------------+  |
|                                                      | Envoy Proxy Container                   |  |
|                                                      | - Inbound Listener (0.0.0.0:proxy_port) |  |
|                                                      |   Terminates mTLS + Enforces RBAC       |  |
|                                                      |   Intentions -> 127.0.0.1:app_port      |  |
|                                                      | - Upstream Listeners (127.0.0.1:local)  |  |
|                                                      |   Originates mTLS + Verifies Regional   |  |
|                                                      |   CA & Service DNS SAN                  |  |
|                                                      +-----------------------------------------+  |
+---------------------------------------------------------------------------------------------------+
```

### 2.3 Allocation Network Namespace L3/L4 Filtering Data Path (`nftables`)

```
+---------------------------------------------------------------------------------------------------+
|                        LINUX ALLOCATION NETWORK NAMESPACE (bridge / cni/*)                        |
|                                   nftables: table inet norca                                      |
|                                                                                                   |
|   INGRESS PACKET (hook input, priority filter)       EGRESS PACKET (hook output, priority filter) |
|                       |                                                       |                   |
|                       v                                                       v                   |
|   +---------------------------------------+               +---------------------------------------+
|   | Base Chain: ingress (policy accept)   |               | Base Chain: egress (policy accept)    |
|   | 1. iifname "lo" -> accept             |               | 1. oifname "lo" -> accept             |
|   | 2. ct state established,related       |               | 2. ct state established,related       |
|   |    -> accept                          |               |    -> accept                          |
|   | 3. ct state invalid -> drop           |               | 3. ct state invalid -> drop           |
|   | 4. ICMPv6 ND (types 133-136) -> accept|               | 4. ICMPv6 ND (types 133-136) -> accept|
|   | 5. ip/ip6 saddr == <NodeGateway>      |               | 5. [Jump egress_admin (if present)]   |
|   |    -> accept (Health Checks)          |               | 6. [Jump egress_ns (if isolated)]     |
|   | 6. [Jump ingress_admin (if present)]  |               +-------------------+-------------------+
|   | 7. [Jump ingress_ns (if isolated)]    |                                   |
|   +-------------------+-------------------+                                   |
|                       |                                                       |
|                       +---------------------------+---------------------------+
|                                                   |
|                                                   v
|   +-------------------------------------------------------------------------------------------+   |
|   | Tier 1: Admin Chain (ingress_admin / egress_admin)                                        |   |
|   | Evaluated in ascending (Priority, Name) order across matching AdminNetworkPolicy objects: |   |
|   | - Action == "allow" -> Verdict: accept (Final; bypasses Namespace Policy layer)           |   |
|   | - Action == "deny"  -> Counter: <dir>_admin_<N> + Verdict: drop (Final)                   |   |
|   | - Action == "pass"  -> Verdict: return (Proceeds to Tier 2 Namespace Chain)               |   |
|   +-----------------------------------------------+-------------------------------------------+   |
|                                                   | (No Admin match, or "pass" returned)
|                                                   v
|   +-------------------------------------------------------------------------------------------+   |
|   | Tier 2: Namespace Chain (ingress_ns / egress_ns) — Present ONLY if direction is Isolated  |   |
|   | Evaluated across all matching NetworkPolicy objects in the allocation's namespace:        |   |
|   | - Union of all matching Ingress/Egress allow rules (CIDRs + Same-Node Peer Allocs + Ports)|   |
|   |   -> Verdict: accept                                                                      |   |
|   | - End of Chain Fallback (Default Deny):                                                   |   |
|   |   -> Counter: <dir>_default + Verdict: drop                                               |   |
|   +-------------------------------------------------------------------------------------------+   |
+---------------------------------------------------------------------------------------------------+
```

---

## 3. Built-in PKI & SPIFFE Workload Identity

### 3.1 Regional Certificate Authority (`mtls_ca.go`)
Every Norca region runs an independent Certificate Authority managed by the active Raft leader (`Server.runMTLSCA` in `norca/norca/mtls_ca.go:371`). Unlike multi-tier external PKIs that require manual offline root ceremonies, Norca's regional CA uses a self-contained, keyring-encrypted root architecture (`structs.MTLSRoot` in `norca/norca/structs/mtls.go:654`):

- **Cryptographic Parameters (`generateMTLSRoot`, `mtls_ca.go:158`)**:
  - **Algorithm**: ECDSA with the NIST P-256 curve (`elliptic.P256()`).
  - **Serial Number**: 127-bit cryptographically random integer (`rand.Int(rand.Reader, 1<<127)`).
  - **Subject**: `CN=Norca mTLS CA <region> <id[:8]>`, `O=Norca`.
  - **URI SAN**: `spiffe://<trust-domain>` (identifies the trust domain for downstream SPIFFE validators).
  - **Validity**: `NotBefore = now - 1m` (clock-skew tolerance) to `NotAfter = now + ca_cert_ttl` (default 5 years / `43800h`).
  - **Constraints**: `BasicConstraintsValid = true`, `IsCA = true`, `MaxPathLenZero = true` (prevents leaf workload certificates from acting as intermediate CAs), and `KeyUsage = CertSign | CRLSign | DigitalSignature`.
- **Keyring Encryption at Rest**:
  - Before writing the `MTLSRoot` to Raft, the CA private key is marshaled to PKCS#8 DER and encrypted via `s.encrypter.Encrypt(keyDER)` using the active root key in Norca's keyring (`mtls_ca.go:205`).
  - The resulting `SigningKey` ciphertext and `SigningKeyID` are stored alongside `RootCert` in the MemDB `mtls_roots` table (`norca/norca/state/state_store_mtls.go:16`).
  - **Keyring Garbage Collection Protection**: `isRootKeyInUseByMTLS` (`state_store_mtls.go:175`) indexes `mtls_roots` by `signing_key_id`, ensuring that keyring rotation or pruning never deletes a root key that still encrypts an active CA key.
  - **Automatic Re-wrapping (`mtlsCA.rewrap`, `mtls_ca.go:523`)**: On its 1-minute maintenance tick (`mtlsCAInterval = 1m`), if the leader detects that the active keyring root key has rotated (`keyMeta.KeyID != active.SigningKeyID`), it decrypts the CA private key in memory, re-encrypts it under the new active root key, and commits the updated `MTLSRoot` to Raft.

### 3.2 Zero-Downtime CA Rotation & Lifecycle Equations
The leader's `reconcile()` loop (`mtls_ca.go:424`) and the manual operator rotation RPC (`MTLS.RotateCA`, `mtls_endpoint.go:238`) manage CA transitions atomically using Raft message type `MTLSRootsUpsertRequestType = 97` (`norca/norca/fsm_mtls.go:25`).

1. **Automatic Rotation Trigger**: A regional CA is automatically rotated when two-thirds of its lifetime has elapsed (`mtlsRootNeedsRotation`, `mtls_ca.go:502`), or immediately if `trust_domain` is changed in `norca.hcl`:
   $$\text{RotateAt} = \text{NotBefore}_{\text{CA}} + \frac{2}{3}\left(\text{NotAfter}_{\text{CA}} - \text{NotBefore}_{\text{CA}}\right)$$
2. **Retirement Semantics (`retireMTLSRoot`, `mtls_ca.go:509`)**: When a new active CA is generated, the previous CA is **not** immediately deleted. In the same atomic Raft transaction:
   - The old root's `Active` flag is set to `false`.
   - Its encrypted private key (`SigningKey` and `SigningKeyID`) is wiped (`nil`), ensuring a retired CA can never sign new certificates.
   - `RotatedOutAt` is recorded as the current UTC timestamp.
   - Its public `RootCert` remains in the trust bundle so existing workload certificates signed by the retiring CA continue to validate while clients re-issue their leaf certificates against the new active CA.
3. **Grace Period & Pruning**: A retired local root is removed from `mtls_roots` only after every leaf certificate it could possibly have signed has expired, plus a 1-hour grace window (`mtlsRetiredRootGrace = 1h`, `mtls_ca.go:473`):
   $$\text{DeleteAt} = \min\left(\text{NotAfter}_{\text{CA}},\; \text{RotatedOutAt} + \text{LeafCertTTL} + 1\text{h}\right)$$

### 3.3 Cross-Region Trust Bundle Federation
When `federation = true` (the default), the regional leader executes `mtlsCA.federate()` (`mtls_ca.go:586`) every minute:
- It queries `MTLS.Roots` across every peer region known to the WAN federation (`m.srv.Regions()`).
- Because `MTLS.Roots` strips private key material via `root.Public()` (`mtls_endpoint.go:61`) and requires only basic authentication (permitting even anonymous tokens when ACLs are enabled so inter-region leaders need no special cross-region ACL token), each leader fetches the public roots owned by each remote region (`root.Region == other`).
- Differences are computed by `mtlsFederationChanges` (`mtls_ca.go:637`) and committed to the local `mtls_roots` table. If a remote region becomes unreachable due to a WAN partition, the local region **retains the last known roots** of that region until they naturally reach `NotAfter`, preventing partition-induced mTLS outages.

### 3.4 SPIFFE ID & X.509 Leaf Certificate Issuance (`MTLS.SignCertificate`)
When a task belongs to a task group containing at least one `service` block with an `mtls` stanza, the client requests a signed leaf certificate via `MTLS.SignCertificate` (`norca/norca/mtls_endpoint.go:96`).

#### Multi-Layered Verification in `MTLS.SignCertificate`:
1. **Cluster Version Gate**: Verifies all regional servers meet `minNorcaFeaturesVersion` (`mtls_endpoint.go:107`).
2. **Cryptographic Claim Verification**: Verifies the EdDSA/RSA signature of `args.IdentityToken` (`m.srv.VerifyClaim`), confirming `claims.AllocationID == args.AllocID` and `claims.TaskName == args.TaskName`.
3. **State Cross-Check**: Looks up the allocation in MemDB (`store.AllocByID`), verifying it exists, is non-terminal (`!alloc.TerminalStatus()`), matches the token's `Namespace` and `JobID`, and contains a task group with `MTLSServices()`.
4. **ACL Caller Binding**: When `acl.enabled = true`, verifies the RPC caller identity is either the specific client node assigned to run the allocation (`identity.ClientID == alloc.NodeID`) or the allocation's workload identity itself (`mtls_endpoint.go:139`).
5. **CSR Validation (`parseWorkloadCSR`, `mtls_ca.go:295`)**: Verifies the PEM block is a valid `CERTIFICATE REQUEST` with a valid self-signature (`csr.CheckSignature()`) and an approved public key algorithm:
   - **ECDSA**: P-256 or P-384 curves only.
   - **Ed25519**: Supported natively.
   - **RSA**: Minimum 2048-bit modulus (`pub.N.BitLen() >= 2048`).

#### Issued X.509 Leaf Certificate Profile (`signWorkloadCert`, `mtls_ca.go:325`):
- **URI SAN (Workload SPIFFE ID)**: Constructed exclusively from the server's allocation record (`structs.MTLSWorkloadSPIFFEID` in `norca/norca/structs/mtls.go:630`):
  $$\text{SPIFFE ID} = \text{spiffe://}\langle\text{trust-domain}\rangle\text{/ns/}\langle\text{namespace}\rangle\text{/job/}\langle\text{job-id}\rangle\text{/group/}\langle\text{group}\rangle$$
  *(Note: For `dispatch` and `periodic` child jobs, `mtlsWorkloadJobID` (`mtls_endpoint.go:212`) normalizes `<job-id>` to `job.ParentID` so dispatched instances share the stable SPIFFE identity of their parent job.)*
- **DNS SANs (`MTLSServiceDNSNames`, `norca/norca/structs/mtls.go:639`)**: Populated for every non-interpolated mTLS service in the task group, matching Norca's built-in DNS server naming convention:
  - `<service>.service.<namespace>.norca`
  - `<service>.service.<namespace>.<region>.norca`
  - `<service>.service.norca` *(included when `<namespace>` is `"default"`)*
- **Key Usage & Validity**:
  - `KeyUsage`: `DigitalSignature | KeyEncipherment | KeyAgreement`
  - `ExtKeyUsage`: `ServerAuth`, `ClientAuth`
  - `IsCA`: `false`
  - `NotBefore`: `now - 1m`
  - `NotAfter`: $\min(\text{now} + \text{LeafCertTTL},\; \text{NotAfter}_{\text{CA}})$

---

## 4. Client Allocation mTLS Lifecycle & Proxy Modes

### 4.1 TaskRunner `mtlsHook` (`norca/client/allocrunner/taskrunner/mtls_hook.go`)
For every task in a task group where `tg.UsesMTLS()` is true, the client registers `mtlsHook`. During `Prestart` (`mtls_hook.go:205`), before the task container or process starts:

1. **Directory & File Layout**: Creates `${NORCA_SECRETS_DIR}/mtls/` (mode `0755`) and populates three files owned by the task's user (`users.WriteFileFor` with atomic `.tmp` write-and-rename):
   - `key.pem`: PKCS#8 PEM private key (always written **before** `cert.pem` so concurrent readers observing a new certificate are guaranteed to find its matching private key on disk, `mtls_hook.go:553`).
   - `cert.pem`: Signed X.509 leaf certificate.
   - `bundle.pem`: Concatenated PEM of all active and retired CA certificates across the region and federation.
2. **Injected Environment Variables (`mtls_hook.go:268`)**:
   | Environment Variable | Value Inside Task | Purpose |
   | :--- | :--- | :--- |
   | **`NORCA_MTLS_CERT_FILE`** | `${NORCA_SECRETS_DIR}/mtls/cert.pem` | Path to the workload X.509 leaf certificate |
   | **`NORCA_MTLS_KEY_FILE`** | `${NORCA_SECRETS_DIR}/mtls/key.pem` | Path to the workload PKCS#8 private key |
   | **`NORCA_MTLS_CA_FILE`** | `${NORCA_SECRETS_DIR}/mtls/bundle.pem` | Path to the federated X.509 CA trust bundle |
   | **`NORCA_MTLS_SPIFFE_ID`** | `spiffe://<td >/ns/<ns>/job/<job>/group/<grp>` | The workload's canonical SPIFFE ID |

3. **Deterministic Renewal & Jitter (`mtlsRenewTime`, `mtls_hook.go:312`)**:
   To prevent thundering herds when thousands of allocations start simultaneously, each task renews its certificate at two-thirds of its validity minus a deterministic hash jitter of up to $5\%$ of the certificate lifetime (seeded by `spiffeID + allocID + taskName`):
   $$T_{\text{renew}} = \text{NotBefore} + \frac{2}{3}L - L \times 0.05 \times \frac{\text{Hash31}(\text{seed}) \bmod 1000}{1000}, \quad L = \text{NotAfter} - \text{NotBefore}$$
4. **Coalesced Change Notification (`applyChanges`, `mtls_hook.go:678`)**:
   For `mode = "app"` services, operators can configure `change_mode` (`"noop"`, `"signal"`, or `"restart"`) and `change_on` (`"any"`, `"certificate"`, or `"bundle"`). When a CA rotates, `watchRoots` updates `bundle.pem` and sets an atomic `reissuing` flag (`mtls_hook.go:420`) while triggering leaf re-issuance. The `applyChanges` loop coalesces events over `1s + rand(0..5s)` (`mtlsChangeCoalesce + mtlsChangeSplay`) and waits up to 2 minutes (`mtlsChangeMaxWait`) for `reissuing` to finish so the application is signaled or restarted **only once** with both the new trust bundle and the newly signed leaf certificate on disk.

### 4.2 The Three Service mTLS Modes (`norca/norca/structs/mtls.go:24`)

| Mode | Injected Task | Networking Requirement | TLS Termination & Authorization |
| :--- | :--- | :--- | :--- |
| **`mode = "app"`** *(default)* | None | Any (`host`, `bridge`, `cni/*`) | Application consumes `NORCA_MTLS_*` files directly (or uses the `github.com/bacanoo-io/norca/norca/api/mtls` Go SDK, which auto-reloads files every 5s and enforces intentions in-process). |
| **`mode = "sidecar"`** | Prestart sidecar `mtls-proxy-<service>` (`TaskKind = "mtls-proxy:<service>"`) | `mode = "bridge"` (exactly 1 network block) | Server `jobMTLSHook.Mutate` (`job_endpoint_hook_mtls.go:78`) injects an Envoy sidecar task and a dynamic port `mtls-proxy-<service>`. Envoy terminates inbound mTLS, enforces `ServiceIntention` RBAC policies, proxies cleartext to `127.0.0.1:<app_port>`, and originates mTLS for declared `upstream` blocks on `127.0.0.1:<local_port>`. |
| **`mode = "gateway"`** | Main task `mtls-gateway-<service>` (`TaskKind = "mtls-gateway:<service>"`) | `mode = "bridge"` or `mode = "host"` | Server injects an Envoy SNI passthrough gateway listening on the service's port. Uses `envoy.filters.listener.tls_inspector` to route incoming cross-region TLS connections by SNI (`<service>.service.<ns>.<region>.norca`) to regional `app` or `sidecar` endpoints **without terminating TLS**. |

### 4.3 File-Based xDS Envoy Architecture (`norca/client/mtlsproxy/`)
Rather than running a complex gRPC xDS server inside the Nomad client agent, `mtlsEnvoyHook` (`norca/client/allocrunner/taskrunner/mtls_envoy_hook.go`) drives Envoy (`defaultEnvoyTag = "v1.40.1"`, customizable via `meta.norca.mtls.envoy_*` node metadata for air-gapped mirrors) through atomic file-based dynamic resources (`path_config_source` in `${NORCA_SECRETS_DIR}/envoy/`):

- **`bootstrap.json`**: Static Envoy node identity pointing `lds_config` to `lds.json` and `cds_config` to `cds.json`.
- **`sds-cert.json` & `sds-trust.json`**: Downstream TLS secrets containing the workload certificate/key and the `envoy.tls.cert_validator.spiffe` multi-trust-domain bundle (`envoy.go:419`).
- **`lds.json` & `cds.json`**: Inbound listener (with `envoy.filters.network.rbac`) and outbound `127.0.0.1:<local_port>` upstream listeners.
- **`eds-upstream_<name>.json` & `sds-trust-upstream_<name>.json`**: Per-upstream endpoint assignments and upstream TLS validation contexts.
  - **Regional CA Pinning & Fail-Closed Security (`envoy.go:271`, `451`)**: Each upstream cluster validates that the destination server's certificate is signed specifically by a CA belonging to the **destination region** (`RegionCAs`) and carries the exact regional DNS SAN `<service>.service.<ns>.<region>.norca`. If the destination region's CAs are not yet known, the upstream **fails closed** (empty endpoints and an unmatchable SAN `no-trusted-ca.invalid`).

---

## 5. Service Intentions (L4/L7 Service-to-Service Authorization)

`ServiceIntention` objects (`norca/norca/structs/mtls.go:811`) define which source services may establish mTLS connections to destination services. Intentions are stored in MemDB table `service_intentions` (`norca/norca/state/state_store_mtls.go:19`) and replicated via Raft message types `ServiceIntentionUpsertRequestType = 98` and `ServiceIntentionDeleteRequestType = 99`.

### 5.1 Intention Precedence & Specificity Scoring
Both the Go SDK (`api/mtls.Evaluate` in `norca/api/mtls/intentions.go:40`) and the Envoy RBAC compiler (`intentionPrecedence` in `norca/client/mtlsproxy/rbac.go:80`) evaluate intentions using a deterministic 3-bit specificity score $\text{Score}(I) \in \{0, 1, \dots, 7\}$:

$$\text{Score}(I) = 4 \cdot \mathbb{I}(\text{Destination} \neq \text{"*"}) + 2 \cdot \mathbb{I}(\text{SourceNamespace} \neq \text{"*"}) + 1 \cdot \mathbb{I}(\text{Source} \neq \text{"*"})$$

| Destination | Source Namespace | Source Service | Specificity Score | Precedence Rank |
| :--- | :--- | :--- | :---: | :---: |
| `<exact>` | `<exact>` | `<exact>` | **7** (`111₂`) | 1 (Highest) |
| `<exact>` | `<exact>` | `*` | **6** (`110₂`) | 2 |
| `<exact>` | `*` | `<exact>` | **5** (`101₂`) | 3 |
| `<exact>` | `*` | `*` | **4** (`100₂`) | 4 |
| `*` | `<exact>` | `<exact>` | **3** (`011₂`) | 5 |
| `*` | `<exact>` | `*` | **2** (`010₂`) | 6 |
| `*` | `*` | `<exact>` | **1** (`001₂`) | 7 |
| `*` | `*` | `*` | **0** (`000₂`) | 8 (Lowest) |

- **Tie-Breaking Rule**: The highest-scoring matching intention decides. If a workload certificate asserts multiple services (or if two matching intentions achieve the same score), **`deny` strictly wins over `allow`** (`intentions.go:87`).
- **Default Policy**: Default-deny (`DefaultAllow = false`). If no intention matches, the connection is rejected.

### 5.2 Boolean Compilation into Envoy RBAC (`norca/client/mtlsproxy/rbac.go`)
Because Envoy's `envoy.filters.network.rbac` filter evaluates an unordered map of policies and has no native concept of specificity precedence, `RBACPolicies` (`rbac.go:170`) compiles each applicable `allow` intention $I$ into a self-contained boolean expression that explicitly excludes any non-disjoint `deny` intention $D_k$ with higher or equal specificity:

$$\text{Policy}(I) = \text{Match}(I) \;\land\; \neg \left( \bigvee_{\substack{D_k \in \text{Denies} \\ \text{Score}(D_k) \ge \text{Score}(I) \\ \neg\text{Disjoint}(I, D_k)}} \text{Match}(D_k) \right)$$

Where $\text{Match}(I)$ matches the peer certificate's URI SAN against `^spiffe://[^/]+/ns/<SourceNamespace>/job/[^/]+/group/[^/]+$` and its DNS SAN against `<Source>.service.<SourceNamespace>.norca`.

---

## 6. Declarative Network Policies & Kernel Microsegmentation

While mTLS and Service Intentions authenticate and authorize service-to-service connections, Norca's Network Policy subsystem (`norca/norca/network_policy_endpoint.go` and `norca/client/allocrunner/netpolicy/`) provides kernel-level L3/L4 packet filtering for allocations running in `bridge` or `cni/*` network modes.

### 6.1 Two-Tier Policy Model: `AdminNetworkPolicy` vs `NetworkPolicy`
Norca stores network rules in two MemDB tables (`network_policies` and `admin_network_policies` in `norca/norca/state/state_store_network_policy.go:15`), replicated via Raft message types `101`–`104`:

| Characteristic | `AdminNetworkPolicy` (Cluster Tier) | `NetworkPolicy` (Namespace Tier) |
| :--- | :--- | :--- |
| **Scope** | Cluster-wide; selects namespaces via exact names or glob patterns (`namespaces = ["prod-*"]`). | Single namespace (`namespace = "prod"`). |
| **ACL Requirement** | `operator = "write"` (or `"read"` for queries). | `network_policy { policy = "write" }` in the target namespace (`acl/network_policy.go:29`). |
| **Evaluation Order** | Evaluated **first**, sorted by ascending `(Priority [0..1000], Name)`. | Evaluated **after** Admin policies (unless an Admin rule terminated evaluation with `allow` or `deny`). |
| **Rule Actions** | Explicit `action = "allow"`, `"deny"`, or `"pass"`. `allow` and `deny` are **final** and cannot be overridden by namespace policies; `pass` delegates to the namespace tier. | Implicit `allow` only. All policies selecting an allocation are additive (union of allowed traffic). |
| **Directional Isolation** | Does not implicitly isolate a direction; unmatched traffic falls through to the namespace tier. | Selecting an allocation with at least one rule in a direction (or listing the direction in `default_deny = ["ingress", "egress"]`) **isolates** that direction—dropping all traffic not explicitly allowed. |

### 6.2 Selectors, Peers, and Port Specifications (`norca/norca/structs/network_policy.go`)
- **Workload Selection (`NetworkPolicyMeta`, `network_policy.go:440`)**: Policies select allocations by matching key-value pairs against the merged metadata map $\text{Meta} = \text{Job.Meta} \cup \text{TaskGroup.Meta}$ (where `TaskGroup.Meta` overrides `Job.Meta`). An empty `selector {}` selects every allocation in the namespace(s).
- **Peer Resolution (`networkPolicyAllocRules`, `network_policy_endpoint.go:675`)**:
  - **CIDR Peers**: IPv4 or IPv6 prefixes (`cidr = ["10.20.0.0/16", "fd00::/64"]`), up to 256 CIDRs per peer block.
  - **Allocation Peers**: Defined by `namespace` (empty for same namespace, `"*"` for all namespaces) and `selector`. When a client queries `NetworkPolicy.GetAllocRules`, the server resolves matching non-terminal peer allocations running on the same node (`store.AllocsByNode`) and returns their `NetworkStatus.Address` IPs in `rules.Peers`, waking the client's blocking query whenever allocations, namespace policies, or admin policies change.
- **Port Specifications (`ParseNetworkPolicyPort`, `network_policy.go:189`)**:
  - Supports single ports (`"5432/tcp"`, `"53/udp"`), port ranges (`"8000-8100/tcp"`), and protocols `tcp`, `udp`, and `sctp`.
  - **Ingress Port Labels**: Ingress rules can reference named allocation port labels (e.g. `"db/tcp"`), which the client compiler resolves to the internal network-namespace port (`mapping.To` or `mapping.Value`, `compile.go:332`).

### 6.3 Scheduler Feasibility & Client `nftables` Compilation
1. **Client Fingerprinting (`NetworkPolicyFingerprint`, `norca/client/fingerprint/network_policy.go:29`)**:
   On startup, the client verifies kernel `nf_tables` support and `CAP_NET_ADMIN` by opening a netlink connection and listing `TableFamilyINet` tables (`netpolicy.Supported()`). If successful, it sets node attribute `norca.network_policy.nftables = "true"`.
2. **Scheduler Enforcement (`NetworkPolicyChecker`, `norca/scheduler/network_policy.go:28`)**:
   - When a task group is selected by any policy with `enforcement = "required"` (the default), `NetworkPolicyChecker.Feasible` rejects nodes lacking `norca.network_policy.nftables = "true"` and rejects placements altogether if the task group uses an unenforceable network mode (such as `host`).
   - At job submission time, `jobNetworkPolicyHook` (`norca/norca/job_endpoint_hook_network_policy.go:25`) emits explicit warnings if a task group's network mode cannot enforce selecting policies.
3. **Atomic Netlink Programming (`netpolicy.Apply`, `norca/client/allocrunner/netpolicy/nft_linux.go:61`)**:
   During allocation `Prerun` and continuously via blocking query (`watch` in `network_policy_hook.go:352`), the client compiles the `Ruleset` and opens a netlink socket directly inside the allocation's network namespace file descriptor (`nftables.WithNetNSFd`). In a single atomic netlink transaction (`conn.Flush()`), it deletes any previous `table inet norca` and programs:
   - Base chains (`ingress` on `ChainHookInput`, `egress` on `ChainHookOutput`) that unconditionally permit loopback (`lo`), `ESTABLISHED | RELATED` conntrack states, ICMPv6 Neighbor Discovery (types 133–136), and—on ingress—the node's own default route gateway IPs (`netpolicy.Gateways`) so client-initiated health checks always succeed.
   - Named packet drop counters (`<dir>_admin_<N>` for admin deny rules and `<dir>_default` for namespace default-deny drops), which `collectCounters` (`network_policy_hook.go:401`) reads every 10 seconds and publishes as `client.allocs.network_policy.dropped` telemetry metrics labeled with `alloc_id`, `namespace`, `job`, `task_group`, `direction`, `kind`, `policy`, and `rule`.

---

## 7. Server, Policy, and Job HCL Configuration Reference

### 7.1 Server Configuration (`norca.hcl`)
Configured inside the `server` or top-level `mtls` block (`norca/norca/structs/config/mtls.go:30`):

```hcl
server {
  enabled = true
}

mtls {
  enabled       = true               # Default: true
  trust_domain  = "prod.norca"       # Default: "<authoritative_region>.norca"
  leaf_cert_ttl = "24h"              # Default: 24h (minimum: 10m)
  ca_cert_ttl   = "43800h"           # Default: 5 years (must be >= 10 * leaf_cert_ttl)
  federation    = true               # Default: true (replicates public CAs across WAN regions)
}
```

### 7.2 Job Specification (`service.mtls` Stanzas)
```hcl
job "payments" {
  namespace = "prod"

  group "api" {
    meta {
      app = "payments-api"
    }

    network {
      mode = "bridge"
      port "http" { to = 8080 }
    }

    # Mode 1: Envoy Sidecar terminating inbound mTLS and originating upstream mTLS
    service {
      name     = "payments-api"
      port     = "http"
      provider = "nomad"

      mtls {
        mode = "sidecar"

        upstream "ledger-db" {
          service    = "postgres"
          namespace  = "prod"
          local_port = 5432
        }

        upstream "eu-audit" {
          service    = "audit-log"
          namespace  = "security"
          region     = "eu-west"
          gateway    = "mtls-gateway"
          local_port = 9000
        }
      }
    }

    # Mode 2: Native App Mode (application reads NORCA_MTLS_* files directly)
    task "worker" {
      driver = "docker"
      config {
        image = "registry.internal/payments:v2.4.0"
      }

      service {
        name     = "payments-worker"
        provider = "nomad"

        mtls {
          mode          = "app"
          change_mode   = "signal"
          change_signal = "SIGHUP"
          change_on     = "any"       # "any" (default), "certificate", or "bundle"
        }
      }
    }
  }
}
```

### 7.3 Namespace & Admin Network Policy HCL Specifications
```hcl
# Namespace Network Policy: norca network-policy apply db-policy.hcl
name        = "postgres-isolation"
namespace   = "prod"
description = "Allow ingress to Postgres only from payments-api; deny all other ingress and egress"

selector {
  app = "postgres"
}

default_deny = ["ingress", "egress"]
enforcement  = "required" # "required" (default) or "best_effort"

ingress {
  name  = "from-payments-api"
  ports = ["db/tcp", "5432/tcp"]
  from {
    selector {
      app = "payments-api"
    }
  }
}

egress {
  name  = "dns-and-s3-archive"
  ports = ["443/tcp"]
  to {
    cidr = ["10.100.0.0/16"]
  }
}
```

```hcl
# Cluster Admin Network Policy: norca network-policy apply -admin block-imds.hcl
name        = "block-cloud-metadata"
description = "Block all workloads across all namespaces from reaching cloud IMDS"
priority    = 10
namespaces  = ["*"]
enforcement = "required"

egress {
  name   = "deny-imds"
  action = "deny" # "allow", "deny", or "pass"
  to {
    cidr = ["169.254.169.254/32", "fd00:ec2::254/128"]
  }
}
```

---

## 8. CLI Command & HTTP API Cheat-Sheet

### 8.1 CLI Commands
| Command | Action | Key Flags & Arguments |
| :--- | :--- | :--- |
| `norca service intention create <src> <dst>` | Create or update an mTLS service intention | `-allow` (default), `-deny`, `-description`, `-namespace` |
| `norca service intention list` | List intentions in a namespace (or all with `-namespace '*'`) | `-json`, `-namespace` |
| `norca service intention info <src> <dst>` | Display details and Raft indices of a specific intention | `-json`, `-namespace` |
| `norca service intention check <src> <dst>` | Evaluate intention precedence for a connection (exits `0` if allowed, `2` if denied) | `-default-allow`, `-namespace` |
| `norca service intention delete <src> <dst>` | Delete a service intention | `-namespace` |
| `norca network-policy apply <file\|->` | Create or update a namespace or admin network policy | `-admin`, `-json` |
| `norca network-policy list` | List namespace network policies or admin policies | `-admin`, `-prefix`, `-filter`, `-json` |
| `norca network-policy status <name>` | Inspect policy rules and verify per-allocation node enforceability | `-admin`, `-verbose`, `-json` |
| `norca network-policy delete <name>` | Remove a namespace or admin network policy | `-admin`, `-namespace` |

### 8.2 HTTP API Endpoints & RPC Methods
| HTTP Endpoint | Method(s) | Internal RPC Method | ACL Capability Required |
| :--- | :--- | :--- | :--- |
| `/v1/mtls/roots` | `GET` | `MTLS.Roots` | Any authenticated token (including anonymous) |
| `/v1/mtls/ca/rotate` | `PUT`, `POST` | `MTLS.RotateCA` | `management` token |
| *(Internal Client RPC)* | *(RPC)* | `MTLS.SignCertificate` | Task JWT `IdentityToken` + Node/Task Identity |
| `/v1/service-intentions` | `GET`, `PUT`, `POST` | `ServiceIntention.List`, `Upsert` | `read-job` (or workload JWT) for `GET`; `write-intention` for writes |
| `/v1/service-intention` | `GET`, `PUT`, `POST`, `DELETE` | `ServiceIntention.Get`, `Upsert`, `Delete` | `read-job` for `GET`; `write-intention` for `PUT`/`POST`/`DELETE` |
| `/v1/network-policies` | `GET`, `PUT`, `POST` | `NetworkPolicy.List`, `UpsertNetworkPolicies` | `read-network-policy` / `write-network-policy` |
| `/v1/network-policy/:name` | `GET`, `PUT`, `POST`, `DELETE` | `NetworkPolicy.GetNetworkPolicy`, `Upsert...`, `Delete...` | `read-network-policy` / `write-network-policy` |
| `/v1/network-policy/:name/status` | `GET` | `NetworkPolicy.Status` | `read-network-policy` + `read-job` |
| `/v1/admin-network-policies` | `GET`, `PUT`, `POST` | `NetworkPolicy.ListAdminNetworkPolicies`, `Upsert...` | `operator: read` / `operator: write` |
| `/v1/admin-network-policy/:name` | `GET`, `PUT`, `POST`, `DELETE` | `NetworkPolicy.GetAdminNetworkPolicy`, `Upsert...`, `Delete...` | `operator: read` / `operator: write` |

---

## 9. Summary for SREs and Security Officers

1. **Zero Secret Leakage on CSR Signing**: Workload private keys (`key.pem`) are generated in-memory on the Nomad client node and written only to the allocation's tmpfs/secrets directory. The RPC layer (`MTLS.SignCertificate`) receives only the public key inside the CSR and binds the certificate's URI and DNS SANs strictly from Raft state verified against the task's signed JWT identity.
2. **Explicit `write-intention` Separation of Duties**: Writing `ServiceIntention` objects requires the dedicated `write-intention` namespace capability (`norca/norca/service_intention_endpoint.go:28`). Intentionally, no built-in ACL policy shorthand (`read`, `write`, `scale`) grants `write-intention`, preventing developers who can submit jobs (`submit-job`) from unilaterally authorizing their workloads to connect to protected services.
3. **Defense-in-Depth (L3/L4 + L7)**: Combine `AdminNetworkPolicy` / `NetworkPolicy` with `mtls`:
   - `nftables` network policies drop unauthorized packets at the Linux kernel netfilter hook (`input`/`output`) before they ever reach user-space sockets.
   - Mutual TLS with SPIFFE verification and Service Intentions cryptographically authenticates workload identities across nodes and federated regions where IP addresses alone are untrusted.
4. **Air-Gapped Envoy Customization**: Sidecar and gateway Envoy containers default to `docker.io/envoyproxy/envoy:v1.40.1` (`mtls_envoy_hook.go:33`), but every component of the image reference can be overridden per client node via `meta.norca.mtls.envoy_registry`, `meta.norca.mtls.envoy_repository`, `meta.norca.mtls.envoy_tag`, `meta.norca.mtls.envoy_digest`, or `meta.norca.mtls.envoy_image`.
