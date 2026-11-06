<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca Technical Architecture & Design Reference

Norca (`v0.1.0`) is an independent, clean-room, MPL-2.0 licensed distributed orchestrator forked from HashiCorp Nomad `v1.6.5` (the final MPL-2.0 release), retaining wire and API compatibility (`1.6.5`) while introducing a comprehensive cloud-native control plane in a single zero-external-dependency binary (`norca`).

This directory contains the authoritative technical architecture and implementation design documents for each functional domain introduced in Norca. Every document maps architectural concepts directly to the implementation in `norca/`, including Raft FSM message types, `MemDB` schemas, cryptographic envelopes, mathematical models, HCL specifications, and CLI/HTTP interfaces.

---

## 1. System Architecture Overview

```
+---------------------------------------------------------------------------------------------------+
|                                      NORCA UNIFIED BINARY (norca)                                 |
|                                                                                                   |
|  +-----------------------------+  +-----------------------------+  +---------------------------+  |
|  |   CLI & Integrated Pack     |  |    Dual Embedded Web UI     |  |  Controller/Operator SDK  |  |
|  | (norca pack, OCI, Cosign,   |  |  (/ui/ Modern Ember+Tabler  |  | (norca/controller Informer|  |
|  |  offline bundles, releases) |  |   /ui/legacy/ Transition)   |  |  WorkQueue, Leader Lease) |  |
|  +--------------+--------------+  +--------------+--------------+  +-------------+-------------+  |
+-----------------|--------------------------------|-------------------------------|----------------+
                  |                                |                               |
                  +--------------------------------+-------------------------------+
                                                   | HTTP/TLS (:4646) & Multiplexed RPC/mTLS (:4647)
                                                   v
+---------------------------------------------------------------------------------------------------+
|                                   NORCA SERVER CONTROL PLANE                                      |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  | 1. Synchronous Admission & Governance Pipeline (norca/norca/admission/)                     |  |
|  |    Built-in Mutators -> Mutating Webhooks (RFC 6902) -> JSON Schema (Draft 2020-12) ->      |  |
|  |    Google CEL Policy Engine -> Validating Webhooks (fail-closed) -> Cosign Verification     |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|  +--------------------------+  +----------------v---------------+  +---------------------------+  |
|  | 2. Security & Identity   |  | 3. Scheduling & Capacity       |  | 4. Extensibility & State  |  |
|  | - Server Seal (Shamir /  |  | - Cross-Job Affinity/Anti-Aff. |  | - ObjectKind (CRDs) &     |  |
|  |   AEAD) & Keyring Wrap   |  | - Topology: Region > Datacenter|  |   CEL Schema Migrations   |  |
|  | - Embedded uBixVault KV2 |  | - Multi-Region & Var Quotas +   |  | - CustomObjects, Status,  |  |
|  | - Native mTLS CA (P-256) |  |   LimitRanges (task_defaults)  |  |   Finalizers & OwnerRefs  |  |
|  |   & SPIFFE IDs           |  | - Leader Workload Autoscaler   |  | - Cascading 3-Mode GC     |  |
|  | - Service Intentions     |  | - Disruption Budgets & Drainer |  | - Server-Side Releases    |  |
|  | - Admin & NS NetPolicies |  | - Native Service Catalog &     |  |   (8-Stage Atomic Apply & |  |
|  |                          |  |   5s Health Flapping Damper    |  |   SHA-256 Drift Detector) |  |
|  +-------------+------------+  +----------------+---------------+  +-------------+-------------+  |
|                |                                |                                |                |
|  +-------------v--------------------------------v--------------------------------v-------------+  |
|  | 5. Intra-Region Raft v3 Consensus, MemDB FSM & Automated Backup Subsystem                   |  |
|  | - Strict intra-region Raft (`validateRaftTLS`); multi-DC (1 DC = 1 AZ) fault tolerance      |  |
|  | - Serf WAN gossip mesh (:4648) & Yamux-pooled cross-region RPC forwarding (:4647)           |  |
|  | - Scheduled multi-destination backups (S3 + Directory), `age` encryption, 2-Person Restore  |  |
|  +----------------------------------------------+----------------------------------------------+  |
+-------------------------------------------------|-------------------------------------------------+
                                                  |
          +---------------------------------------+---------------------------------------+
          | Client RPC & Task Hooks                                                       | SSE (/v1/event/stream)
          v                                                                               v
+---------------------------------------------------+           +-----------------------------------+
|                NORCA CLIENT NODE                  |           |         norca-natsbridge          |
| - Atomic symlink-safe secret delivery (OpenRoot)  |           | - Exact-index SSE consumer        |
| - Local Vault-compat (:8250) & Consul-compat HTTP |           | - CAS VariableCheckpointer        |
| - Ephemeral P-256 mTLS keys & file-based Envoy xDS|           | - NATS Core / JetStream publisher |
| - Kernel nftables L3/L4 policy enforcement        |           |   with Nats-Msg-Id deduplication  |
+---------------------------------------------------+           +-----------------------------------+
```

---

## 2. Design Documents by Functional Domain

| # | Document | Domain | Core Subsystems & Source Packages Covered |
| :--- | :--- | :--- | :--- |
| **01** | [**Raft Consensus, Serf Gossip & Cross-Region Architecture**](raft-topology-and-cross-region.md) | Consensus & Topology | Port `4647` byte multiplexer (`RpcNomad`, `RpcRaft`, `RpcTLS`, `RpcStreaming`, `RpcMultiplexV2`), cryptographic regional Raft isolation (`validateRaftTLS`), `region > datacenter` (1 DC = 1 AZ) quorum sizing, Serf WAN mesh (`:4648`), cross-region RPC forwarding, and multiregion deployment watchers. |
| **02** | [**Built-in Secret Store & Cryptographic Seal Architecture**](secret-store-and-seal-architecture.md) | Secrets & Cryptography | Server seal (`seal.Shares` Shamir over $\text{GF}(2^8)$ & `seal.AEAD`), sibling root key wrapping (`encrypter_seal.go`), embedded `uBixVault` KV v2 over Raft (`norca/secretstore`), fail-closed two-phase audit logging, conservative glob delegation (`AllowSecretGrant`), local Vault-compat shim (`:8250`), and symlink-safe `os.OpenRoot` task delivery. |
| **03** | [**Native mTLS PKI, SPIFFE Identity & Network Security**](native-mtls-and-network-security.md) | Zero-Trust Networking | Built-in ECDSA P-256 Root CA (`mtls_ca.go`), keyring-encrypted CA signing keys, cross-region trust bundle federation, client-side ephemeral CSR flow, SPIFFE ID issuance, file-based Envoy xDS (`sidecar`/`gateway`), 3-bit specificity Service Intentions, and two-tier kernel `nftables` microsegmentation (`AdminNetworkPolicy` & `NetworkPolicy`). |
| **04** | [**Admission Control (CEL & Webhooks) & Resource Quotas**](admission-control-and-quotas.md) | Governance & Policy | 5-phase admission pipeline (`norca/norca/admission/`), built-in mutators, external RFC 6902 Mutating & Validating HTTP Webhooks (fail-closed default), sandboxed Google CEL (`cel-go`) with cost/timeout limits, `crd-schema` JSON Schema validation, Cosign signature verification, multi-region Resource Quotas, LimitRanges (`task_defaults`/`task_max`), and Variables byte quotas. |
| **05** | [**Advanced Scheduling, Built-in Autoscaler & Disruption Budgets**](scheduling-autoscaling-and-disruption.md) | Scheduling & Scaling | Cross-job affinity & anti-affinity (`colocate`/`separate` in `norca/scheduler/alloc_affinity.go`), incremental `planNodeFingerprint` evaluation, `${node.datacenter}` spread, leader-embedded Horizontal Autoscaler (`norca/norca/autoscaler/`, 30-bucket ring buffer, `target-value`/`threshold`/`fixed-value` strategies, quota capping), and transactional Disruption Budgets (`Alloc.Evict` & `NodeDrainer`). |
| **05b** | [**Dynamic Application Autoscaling on Native Metrics**](dynamic-application-autoscaling.md) | App-Level Scaling | White-room design for zero-Prometheus application autoscaling over `Node.UpdateAlloc` piggybacking: Tier 1 L4/L7 kernel & Envoy mTLS metrics (`nomad.net.*`, `nomad.proxy.*`), Tier 2 local HTTP `/metrics` & `X-Norca-Load` check headers, Tier 3 symlink-safe `$NORCA_ALLOC_DIR/tmp/metrics.json`, `divisor = "count"` shared-queue backlog normalization, and cross-group scale-from-zero. |
| **05c** | [**Embedded 30-Day Metrics Storage (`tstorage` Integration)**](internal-metrics-storage-tstorage.md) | Internal TSDB | Zero-Raft-impact 30-day historical metrics engine based on `nakabonne/tstorage` (Apache-2.0): Gorilla (delta-of-delta + XOR) compressed 6h disk partitions, cross-platform Unix/Windows `mmap` abstraction, non-blocking 30s batch ingestion, `max_series` / `max_disk_size` guards, autoscaler ring-buffer warmup seeding, and Web UI historical range queries. |
| **05d** | [**Embedded NATS System Bus**](embedded-nats-system-bus.md) | Messaging & Telemetry | nats-server with JetStream embedded in the servers (`ports.bus` 4649, routes from Serf, gateways between regions), Norca-state authentication (node secret, ACL `bus` block), in-agent event publishing and pack registry syncs replacing `norca-natsbridge`, the Consul KV bucket of the compat API, the telemetry stream of the metrics store, limits and phasing out the bridge. |
| **06** | [**Custom Objects (CRDs) & Go Operator Framework**](custom-objects-and-operator-framework.md) | Extensibility & Operators | `ObjectKind` definitions, Draft 2020-12 JSON Schema validation (`norca/norca/objectschema/`), CEL validation & transition rules (`self`, `oldSelf`), in-place CEL schema migrations (`merge`/`omit`), `spec`/`status` subresource separation, finalizers, 3-mode cascading Garbage Collection (`norca/norca/objectgc/`), and the `norca/controller` Go Operator SDK. |
| **07** | [**Integrated Package Manager & Server-Side Releases**](package-manager-and-releases.md) | Packaging & Lifecycle | Unified `norca pack` CLI (`norca/pack/`), first-class `packs.norca.io/Release` Raft state, 8-stage topological resource planner (Namespaces -> Quotas -> ACLs -> Variables -> ObjectKinds -> Admission -> CustomObjects -> Jobs), lifecycle hooks, atomic rollbacks, SHA-256 drift detection, OCI registries, `pack.lock`, and air-gapped bundles. |
| **07b** | [**Server-Side Pack Registry, App Catalog & Schema-Driven UI**](pack-registry-and-ui-catalog.md) | UI Package Management | Gap analysis and design for cluster-wide `packs.norca.io/Registry` management, `/v1/packs` application catalog, server-side template rendering & plan preview (`/deploy-pack`), and the multi-tab `UIFormSchema` (`x-ui-tabs`, `x-ui-widget`, `x-ui-show-if`, live cluster pickers, and secret field isolation). |
| **07c** | [**Pack Types: HCL Packs and Debian Host Packages**](deb-packs-and-host-packages.md) | Packaging & Nodes (parked draft) | `type = "hcl"|"deb"` in `metadata.hcl`; `deb` packs declare per-platform `.deb` closures (OCI/HTTPS, sha256, cosign) rendered to a `host-package` resource; built-in kinds `HostPackageSet`, `HostPackageRollout`, `NodeHostPackages`; client pull model (`client/hostpkg`, bolt ledger, simulate-then-install, `apt-mark hold`), `dpkg`/`pkg.deb.*` fingerprints, `debversion` constraint operator; wave rollouts with health gate and revert; dependent allocations replaced via the eviction API (exec chroot keeps old inodes); UI, backup (restored sets start paused) and risk analysis. |
| **08** | [**Native Service Discovery, Consul Compat & NATS Event Bridge**](service-discovery-and-event-bridge.md) | Discovery & Eventing | Native service catalog (`TableServiceRegistrations`), `startup`/`readiness`/`liveness` probes, 5s leader `serviceHealthDamper`, External Services (`address`/`alias`, active probing, TTLs), `SelectByTopology` (`prefer = none\|datacenter\|region`, `min_local`, `overflow_fraction`) & cross-region `failover`, client Consul HTTP/DNS compat (`norca/client/consulcompat/`), and `norca-natsbridge` (`norca/natsbridge/`). |
| **09** | [**Automated, Encrypted Backup & Disaster Recovery**](backup-architecture.md) | Disaster Recovery | Leader-scheduled backups (`norca/norca/backup/`), asymmetric client-side `filippo.io/age` encryption, concurrent S3 multipart and shared-directory destinations, GFS retention & pinning, selective item restore, 2-Person Rule live region rollback (`RequestedBy != ApprovedBy`), and bare-metal `restore-bootstrap`. |
| **10** | [**Web UI Architecture & Dual-UI Embedding Pipeline**](web-ui-architecture.md) | User Interface | Clean-room Ember 6.12 Octane + Tabler SPA (`norca/ui/`), dual `/ui/` and `/ui/legacy/` HTTP mounting (`norca/command/agent/http.go`), strict zero-CDN Content-Security-Policy, adaptive blocking-query polling & SSE event streaming, full operational coverage of all Norca subsystems, and the Linux/Windows (`windows.bat`) `bindata_assetfs.go` build pipeline. |

---

## 3. Raft State Machine (`MemDB`) & MessageType Extensions

Norca preserves upstream Nomad v1.6.5 Raft wire compatibility while utilizing dedicated Raft `MessageType` IDs and `MemDB` tables gated by `ServersMeetMinimumNorcaVersion` (`0.1.0` in `norca/norca/norca_features.go`).

### 3.1 Key Norca Raft MessageTypes (`norca/norca/structs/structs.go`)

| MessageType Constant | ID | Subsystem | Purpose |
| :--- | :--- | :--- | :--- |
| `NamespaceUpsertRequestType` / `Delete` | `64` / `65` | Multi-Tenancy | Create, update, or delete cluster namespaces (`namespaces`). |
| `QuotaSpecUpsertRequestType` / `Delete` | `66` / `67` | Resource Quotas | Persist or remove multi-region `QuotaSpec` definitions (`quotas`). |
| `AdmissionPolicyUpsertRequestType` / `Delete` | `70` / `71` | Admission Control | Persist or remove CEL and Webhook `AdmissionPolicy` specifications (`admission_policies`). |
| `AllocEvictRequestType` | `74` | Disruption Budgets | Atomic FSM check-and-evict enforcing task group `DisruptionBudget`. |
| `BackupUpsertRequestType` / `Delete` | `78` / `79` | Backups | Record backup manifests, destination statuses, and pin flags (`backups`). |
| `BackupRestoreUpsertRequestType` | `80` | Backups | Track pending two-person full region restore requests and approvals (`backup_restores`). |
| `ObjectKindUpsertRequestType` / `Delete` | `82` / `83` | Custom Objects | Register or delete `ObjectKind` (CRD) schemas, CEL rules, and webhook SHA-256 digests (`object_kinds`). |
| `CustomObjectUpsertRequestType` / `Delete` | `84` / `85` | Custom Objects | Create/update `CustomObject` spec, status, finalizers, or delete (`custom_objects`). |
| `CustomObjectEventsRequestType` | `86` | Custom Objects | Emit batch lifecycle and controller events for custom objects. |
| `ObjectGCRequestType` | `87` | Custom Objects | Execute cascading garbage collection batch deletes and owner-reference patches. |
| `MTLSRootsUpsertRequestType` | `97` | Native mTLS | Commit rotated/active ECDSA P-256 CA roots (`mtls_roots`). |
| `ServiceIntentionUpsertRequestType` / `Delete` | `98` / `99` | Native mTLS | Persist L4/L7 SPIFFE `ServiceIntention` authorization rules (`service_intentions`). |
| `NetworkPolicyUpsertRequestType` / `Delete` | `101` / `102` | Network Security | Persist namespace-scoped `NetworkPolicy` specifications (`network_policies`). |
| `AdminNetworkPolicyUpsertRequestType` / `Delete` | `103` / `104` | Network Security | Persist cluster-wide `AdminNetworkPolicy` guardrails (`admin_network_policies`). |
| `SecretStoreBlobsUpsertRequestType` / `Delete` | `105` / `106` | Secret Store | Persist or delete raw `uBixVault` storage entries (`secret_store_blobs`). |
| `SealConfigUpsertRequestType` / `RewrapDone` | `107` / `108` | Server Seal | Persist `SealConfig` (`shares` or `aead`) and complete root key rewrapping (`seal_config`). |
| `SecretApplyRequestType` / `Purge` | `109` / `110` | Secret Store | Atomic batch commit of secret metadata (`secrets`) and `uBixVault` blobs (`secret_store_blobs`). |

