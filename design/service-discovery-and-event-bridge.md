<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Native Service Discovery, Consul Compatibility, and NATS Event Bridge Architecture

## 1. Executive Summary & Core Architectural Principles

Norca eliminates the mandatory operational dependency on an external HashiCorp Consul cluster for service registration, health evaluation, DNS resolution, topology-aware routing, external service tracking, and event-driven automation. While upstream Nomad v1.6 introduced a rudimentary `provider = "nomad"` catalog that lacked health states in the catalog, readiness roles, topology tiers, DNS resolution, or external service support, Norca upgrades the native service catalog into a complete, self-contained service discovery and event streaming subsystem.

At the same time, Norca preserves drop-in compatibility for existing infrastructure tooling (Traefik, Prometheus `consul_sd_configs`, `consul-template`, and self-registering frameworks) via a client-local Consul HTTP & DNS compatibility layer (`norca/client/consulcompat/` and `norca/client/dns/`), and bridges cluster state transitions into cloud-native, multi-region event architectures via a dedicated, at-least-once NATS / JetStream event bridge (`norca/natsbridge/`).

### Key Architectural Tenets:
1. **Zero-Consul Operational Autonomy**: Service registration, check role evaluation (`startup`, `readiness`, `liveness`), flapping protection, DNS resolution (`A`, `AAAA`, `CNAME`, `SRV`), and cross-region failover operate natively over Norca's Raft state machine and RPC mesh without external agents.
2. **Health-First Catalog with Flapping Damping**: Client nodes evaluate check roles locally and push check states (`passing`, `warning`, `critical`), readiness flags (`Ready`), and truncated check outputs into the Raft catalog. A leader-side health damper (`serviceHealthDamper`) coalesces rapid health oscillations within a `5s` window (`serviceHealthWindow`) to protect Raft log throughput.
3. **Failure-Domain Topology & Cross-Region Failover**: Reflecting Norca's cloud topology (`1 Region = 1 Cloud Region`, `1 Datacenter = 1 Availability Zone`), services declare `discovery` blocks (`prefer = "datacenter"`, `min_local`, `overflow_fraction`, `failover = [...]`) that keep traffic local to the caller's AZ until capacity thresholds trigger regional overflow or cross-region failover.
4. **First-Class External Services**: Off-cluster databases, SaaS endpoints (`alias` / `CNAME`), and legacy VMs are registered directly into the native catalog (`ServiceExternal`) and monitored via leader-tracked in-memory TTL heartbeats or client-executed HTTP/TCP active probes.
5. **Drop-In Consul HTTP & DNS Emulation**: Each Norca client can expose a local Consul-compatible HTTP server (`127.0.0.1:8500`) and `.consul` DNS zone backed by the native catalog and local external service agent, enabling unmodified third-party proxies and scrapers to operate without Consul while avoiding self-registration loops (`avoidConsulCompatLoop`).
6. **Decoupled, At-Least-Once NATS Event Streaming**: `norca-natsbridge` consumes the server's exact-index NDJSON event stream (`/v1/event/stream?exact=true`) and publishes raw, precision-preserving event payloads to NATS Core or JetStream (`<prefix>.<region>.<topic>`) with `Nats-Msg-Id` deduplication, Variables-backed CAS leader election, cursor checkpointing, and `410 Gone` gap marker emission.

```
+---------------------------------------------------------------------------------------------------+
|                                      NORCA CLIENT NODE (AZ: eu-west-1a)                           |
|                                                                                                   |
|  +---------------------------+    +-----------------------------+    +-------------------------+  |
|  |   Allocation Workloads    |    |  External Service Runner    |    |   Built-in DNS Server   |  |
|  | - Startup/Readiness/Live  |    |  (client/externalsvc)       |    |   (client/dns: :8600)   |  |
|  | - Template (nomadService) |    | - Active HTTP / TCP Probes  |    | - *.service.norca       |  |
|  +-------------+-------------+    +--------------+--------------+    | - *.service.consul      |  |
|                |                                 |                   +------------+------------+  |
|                | Upsert / Check Status           | NodeReport=true                |               |
|                v                                 v                                |               |
|  +---------------------------------------------------------------+                |               |
|  |               Client RPC & Discovery Subsystem                |<---------------+               |
|  |               - Catalog Cache (client/discovery/cache.go)     |                                |
|  |               - Topology Resolver (resolver.go)               |<---------------+               |
|  +-------------------------------+-------------------------------+                |               |
|                                  |                                                |               |
|                                  |             +----------------------------------+------------+  |
|                                  |             |     Consul HTTP Compatibility (:8500)         |  |
|                                  |             |     (client/consulcompat/server.go)           |  |
|                                  |             | - /v1/catalog/*, /v1/health/*, /v1/status/*   |  |
|                                  |             | - /v1/agent/service/*, /v1/agent/check/*      |  |
|                                  |             +-----------------------------------------------+  |
+----------------------------------|----------------------------------------------------------------+
                                   | mTLS RPC
                                   v
+---------------------------------------------------------------------------------------------------+
|                                      NORCA REGION LEADER                                          |
|                                                                                                   |
|  +-------------------------------+    +--------------------------+    +------------------------+  |
|  |  ServiceRegistration Endpoint |    |   serviceHealthDamper    |    |    externalServices    |  |
|  | (service_registration_*.go)   |--->| (5s Flapping Window)     |    | (service_external.go)  |  |
|  +-------------------------------+    +-------------+------------+    | - 1s TTL Expiry Ticker |  |
|                                                     |                 | - 30s Reconcile/Reap   |  |
|                                                     v                 +-----------+------------+  |
|                                       +---------------------------+               |               |
|                                       |  Raft Log & MemDB State   |<--------------+               |
|                                       | (TableServiceRegistrations|                               |
|                                       +-------------+-------------+                               |
|                                                     | Publish(events)                             |
|                                                     v                                             |
|                                       +---------------------------+                               |
|                                       |   In-Memory EventBroker   |                               |
|                                       |   (norca/stream/)         |                               |
|                                       | - Per-Topic Watermarks    |                               |
|                                       | - Exact-Index Ring Buffer |                               |
|                                       +-------------+-------------+                               |
+-----------------------------------------------------|---------------------------------------------+
                                                      | GET /v1/event/stream?exact=true&index=N
                                                      v
+---------------------------------------------------------------------------------------------------+
|                                   NORCA-NATSBRIDGE HA WORKLOAD                                    |
|                                   (norca/natsbridge/)                                             |
|                                                                                                   |
|  +-------------------------------+    +--------------------------+    +------------------------+  |
|  | Variables Leader Election     |    |  APISource Stream Reader |    | VariableCheckpoint     |  |
|  | (norca/jobs/natsbridge/lease) |--->|  (Byte-Exact Raw JSON)   |--->| (.../checkpoint CAS)   |  |
|  +-------------------------------+    +-------------+------------+    +------------------------+  |
|                                                     |                                             |
|                                                     v PublishBatch (Nats-Msg-Id Deduplication)    |
+-----------------------------------------------------|---------------------------------------------+
                                                      |
                                                      v
                                    +-----------------------------------+
                                    |     NATS CORE / JETSTREAM         |
                                    | Subjects:                         |
                                    | - <prefix>.<region>.<Topic>       |
                                    | - <prefix>.<region>.Bridge (Gap)  |
                                    +-----------------------------------+
```

---

## 2. Native Service Catalog & Health Subsystems

### 2.1 Raft-Backed Service Registration Data Model
All native allocation services (`provider = "nomad"`) and external services are stored in the regional Raft state machine within the `service_registrations` MemDB table (`state.TableServiceRegistrations`). Each entry is represented by `structs.ServiceRegistration` (`norca/norca/structs/service_registration.go:49`):

| Field | Type | Description |
| :--- | :--- | :--- |
| **`ID`** | `string` | Unique identifier within the namespace (deterministic hash for task/group services, or explicit/generated ID for external services). |
| **`ServiceName`** | `string` | RFC-1123 compliant human-friendly service identifier (`^[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$`). |
| **`Namespace`** | `string` | Namespace scope (`Job.Namespace` or target external service namespace). |
| **`NodeID` / `Datacenter`**| `string` | Client node ID and denormalized failure domain (`Node.Datacenter`), enabling O(1) datacenter filtering without secondary node lookups. Empty `NodeID` for static/unowned external services. |
| **`JobID` / `AllocID`** | `string` | Owning workload identifiers (empty for external services). |
| **`Tags` / `Meta`** | `[]string` / `map[string]string` | Active tags and key-value metadata (automatically switches to `CanaryTags` and `CanaryMeta` while an allocation is a canary). |
| **`Address` / `Port`** | `string` / `int` | Resolved IP address (or target DNS hostname for `alias` external services) and port according to `address_mode` (`auto`, `host`, `alloc`, `driver`). |
| **`Ready`** | `bool` | Traffic readiness flag computed from `startup` and `readiness` checks (`norca/norca/structs/service_registration.go:99`). |
| **`Health`** | `string` | Aggregate health status: `"passing"`, `"warning"`, or `"critical"` (`norca/norca/structs/service_registration_health.go:15`). |
| **`Checks`** | `[]*ServiceRegistrationCheck` | Per-check status snapshots (`ID`, `Name`, `Role`, `Status`, and UTF-8 safe `Output` truncated to `ServiceCheckOutputLimit = 256` bytes). |
| **`HealthUpdatedAt`** | `int64` | Unix nanosecond timestamp recorded by the leader when the instance's health last changed. |
| **`Discovery`** | `*ServiceDiscovery` | Topology routing and regional failover configuration attached to the service. |
| **`External`** | `*ServiceExternal` | Non-nil for external services (`Kind`, `Source`, `LocalID`, `Checks`, `DeregisterCriticalAfter`). |

### 2.2 Check Roles: Startup, Readiness, and Liveness
Norca extends `ServiceCheck` with explicit Kubernetes-style probe roles (`norca/norca/structs/check_roles.go:18`):

```
+-----------------------------------------------------------------------------------+
|                        ALLOCATION TASK / GROUP LIFECYCLE                          |
|                                                                                   |
|   [ Task Started ]                                                                |
|          |                                                                        |
|          v                                                                        |
|   +-----------------------------------------+                                     |
|   | role = "startup"                        |  Fails > startup_timeout (def 5m)   |
|   | - Gates all other checks                |----------------------------------+  |
|   | - Holds ServiceRegistration.Ready=false |                                  |  |
|   +--------------------+--------------------+                                  |  |
|                        | Passes                                                |  |
|                        v                                                       |  |
|        +---------------+---------------+                                       |  |
|        |                               |                                       |  |
|        v                               v                                       v  |
|   +----------------------------+  +----------------------------+     +---------+--+
|   | role = "readiness"         |  | role = "liveness"          |     | Task       |
|   | - Controls Ready (true/fls)|  | - Executed by client node  |---->| Restarted  |
|   | - Counts in deployments    |  | - Never alters catalog     |     +------------+
|   | - Never restarts task      |  | - Default limit = 3        |
|   +----------------------------+  +----------------------------+
+-----------------------------------------------------------------------------------+
```

1. **`role = "startup"`** (`CheckRoleStartup`): Indicates whether the workload has finished initializing. At most one startup check is permitted per task or per group (`validateCheckRoles` in `norca/norca/structs/check_roles.go:116`). Until it passes, all other checks on the workload are gated from triggering restarts and `ServiceRegistration.Ready` is held `false`. If it fails to pass within `startup_timeout` (default `5m`, `DefaultStartupTimeout`), the task is restarted.
2. **`role = "readiness"`** (`CheckRoleReadiness`): Determines whether the service instance can receive traffic (`ServiceRegistration.Ready`). It counts toward deployment health (unless `on_update = "ignore"`), updates the catalog health state, and **never** restarts the task (`check_restart` is rejected on readiness checks).
3. **`role = "liveness"`** (`CheckRoleLiveness`): Detects deadlocked or wedged processes. Even on `provider = "consul"` services, liveness checks are always executed directly by the Norca client (`IsClientExecuted` in `norca/norca/structs/check_roles.go:53`) so that transient liveness probe failures trigger local task restarts (default `check_restart { limit = 3 }`) without polluting the service catalog or failing active deployments.
4. **Role-less Checks (`role = ""`)**: Retain classic Nomad behavior, contributing to catalog health, readiness, and deployment status.

#### Mixed-Version Upgrade Safety
During a rolling cluster upgrade (`norca/norca/service_registration_endpoint.go:95-120`):
- Clients running `< 0.1.0` (or upstream Nomad clients without a `norca.version` node attribute) do not populate `ServiceRegistration.Ready`; `nodeReportsServiceReadiness` (`service_registration_endpoint.go:163`) automatically forces `service.Ready = true` so their instances remain discoverable.
- Until all regional servers report Norca support (`ServersMeetMinimumNorcaVersion`), `Upsert` invokes `service.StripNorcaFields()` (`norca/norca/structs/service_registration_health.go:134`) and elides no-op re-assertions to ensure identical FSM state across mixed servers.

### 2.3 Leader-Side Flapping Protection (`serviceHealthDamper`)
Because clients periodically re-assert service health and flapping services could otherwise saturate Raft consensus with `ServiceRegistrationUpsertRequestType` entries, the regional leader passes all upserts through `serviceHealthDamper` (`norca/norca/service_registration_health.go:35`):

- **No-Op Elision**: Check outputs (`Output`) are ignored when comparing health equality (`HealthEqual` in `norca/norca/structs/service_registration_health.go:87`). If a check's output changes on every execution (e.g., including a timestamp) without changing `Ready`, `Health`, or check `Status`, `filter()` drops the registration from the Raft batch. If no registrations remain in the request, the Raft write is skipped completely (`service_registration_endpoint.go:132`).
- **Damping Window (`serviceHealthWindow = 5s`)**: When an existing registration changes *only* its health fields (`reg.EqualExceptHealth(current)`) and the elapsed time since `current.HealthUpdatedAt` is less than `5s`:
  $$\Delta t = t_{\text{now}} - t_{\text{HealthUpdatedAt}} < 5\text{s}$$
  the update is withheld from Raft and stored in `d.pending[serviceHealthKey{namespace, id}]` with $\text{due} = t_{\text{HealthUpdatedAt}} + 5\text{s}$, incrementing `nomad.service_registration.health_updates_deferred`.
- **Self-Canceling Oscillations & Flush Loop**: If a subsequent upsert within the 5-second window reports that the instance has returned to its currently committed Raft health (`reg.HealthEqual(current)`), the pending entry is deleted and zero Raft writes occur (`service_registration_health.go:93-100`). Otherwise, a background ticker running every `250ms` (`serviceHealthFlushInterval`, `service_registration_health.go:22`) flushes matured pending entries to Raft in a single batch (`d.flush()`).

### 2.4 External Services (`service_external.go` & `client/externalsvc/`)
External services (`structs.ServiceExternal`, `norca/norca/structs/service_external.go:105`) allow operators and off-cluster applications to register workloads that are not tied to a Nomad allocation (`JobID == ""` and `AllocID == ""`):

1. **Service Kinds**:
   - `address` (`ExternalServiceKindAddress`): Points to an IPv4/IPv6 address or DNS hostname and port. Answered with `A`, `AAAA`, and `SRV` records.
   - `alias` (`ExternalServiceKindAlias`): Points to a target DNS name (`Target` stored in `ServiceRegistration.Address`). Answered by the built-in DNS server with a `CNAME` record (`norca/client/dns/server.go:358`) and `SRV` target.
2. **Ownership & Check Execution Models**:
   - **Leader-Managed TTL Checks (`type = "ttl"`)**: Tracked in-memory on the Raft leader in `externalServices.deadlines` (`norca/norca/service_external.go:45`), similar to node heartbeats. Renewals via `PUT /v1/service/external/:id/ttl` (or `norca service ttl <id>`) reset the in-memory deadline `now.Add(check.TTL)` **without writing to Raft** unless the check's status changes (`service_external.go:431-449`). The leader's `1s` ticker (`externalServiceTick`) expires overdue checks to `critical` (`Output: "TTL expired"`). Upon leadership transitions, a new leader grants all non-critical TTL checks a fresh full TTL grace period (`service_external.go:98-103`).
   - **Client-Executed Active Probes (`type = "http" | "tcp"`)**: Require registering the external service through a Norca client agent (`service_external_endpoint.go:107`), which attaches its `NodeSecretID` to bind `reg.NodeID = node.ID`. The client's `externalsvc.Agent` (`norca/client/externalsvc/agent.go:47`) watches its owned registrations via a blocking RPC query (`ServiceRegistration.ListExternal`) and spawns a background `runner` (`norca/client/externalsvc/checks.go:33`) per check with initial random jitter $\in [0, \min(\text{Interval}, 2\text{s})]$. HTTP `2xx` maps to `passing`, `429 Too Many Requests` maps to `warning`, and all other codes/errors map to `critical` (`checks.go:163-170`). Status reports (`NodeReport: true`) are sent to the servers only on state transitions (`checks.go:87-96`).
   - **Static External Services**: Registrations with neither TTL checks nor client node ownership never expire; therefore, `UpsertExternal` enforces both `register-external-service` namespace capability and `operator:write` privileges (`service_external_endpoint.go:113`).
3. **Automatic Critical Reaping**: Every `30s` (`externalServiceReconcileInterval`, `service_external.go:24`), `reconcileLocked` inspects all external registrations and automatically deregisters any service whose aggregate health has remained `critical` for at least `DeregisterCriticalAfter` (defaults to `1h` via the native API, `DefaultExternalDeregisterCriticalAfter`, or `0` to disable reaping), incrementing `nomad.service_registration.external_reaped`.

---

## 3. Topology-Aware Service Discovery & Failover

### 3.1 The `Region > Datacenter` Topology Model
In Norca's multi-cloud architecture, a **Region** represents an independent Raft cluster in a cloud region (e.g., `aws-eu-west-1`, `gcp-europe-west1`), and a **Datacenter** represents a failure domain or Availability Zone within that region (e.g., `eu-west-1a`, `eu-west-1b`).

A native `service` stanza can configure a `discovery` block (`structs.ServiceDiscovery`, `norca/norca/structs/discovery.go:54`):
- `prefer`: `"none"` (default), `"datacenter"`, or `"region"`.
- `min_local`: Minimum number of eligible instances a local tier must have before the next tier is omitted (floor of `1`).
- `overflow_fraction`: Minimum fraction $\in [0, 1]$ of total regional eligible instances the local tier must hold before the next tier is omitted. If unset (`nil`) in the jobspec, the server populates it at job registration from `server.native_service_discovery.default_overflow_fraction` (default `0.2`, `DefaultDiscoveryOverflowFraction`).
- `failover`: Ordered list of peer regions (`[]string`) to query when the local region has zero eligible instances.
- `include_not_ready`: When `true`, `reg.Discoverable()` returns `true` even for unhealthy/unready instances (`norca/norca/structs/service_registration_health.go:75`), enabling peer discovery for bootstrapping stateful clusters (e.g., Raft/etcd/Cassandra peers) before readiness probes pass.

### 3.2 Mathematical Formulation of Topology Selection
Given the set of discoverable instances $I$ in the consumer's local region $r_{\text{near}}$ and the consumer's local datacenter $dc_{\text{near}}$, `structs.SelectByTopology` (`norca/norca/structs/discovery.go:162`) partitions $I$ when `prefer = "datacenter"` into two ordered tiers:

$$T_0 = \{ s \in I \mid s.\text{Datacenter} = dc_{\text{near}} \}, \qquad T_1 = \{ s \in I \mid s.\text{Datacenter} \neq dc_{\text{near}} \}$$

Let $m = \max(\text{MinLocal}, 1)$ and let $f = \text{EffectiveOverflowFraction}() \in [0, 1]$. The local datacenter tier $T_0$ is sufficient on its own if and only if it satisfies both the absolute floor and the proportional capacity threshold:

$$|T_0| \ge m \quad \land \quad |T_0| \ge f \cdot |I|$$

Consequently, the selected instance set $S(I, dc_{\text{near}})$ is:

$$S(I, dc_{\text{near}}) = \begin{cases} T_0 & \text{if } \text{Prefer} = \text{"datacenter"} \land |T_0| \ge \max(m, \lceil f \cdot |I| \rceil) \\ T_0 \cup T_1 & \text{if } \text{Prefer} = \text{"datacenter"} \land |T_0| < \max(m, \lceil f \cdot |I| \rceil) \\ I & \text{if } \text{Prefer} \in \{\text{"none"}, \text{"region"}\} \end{cases}$$

This prevents the classic "small-AZ overload" failure mode: if a region has $|I| = 20$ instances across 3 AZs, and `eu-west-1a` degrades to $|T_0| = 2$ healthy instances while $f = 0.2$ requires $0.2 \times 20 = 4$ instances, `SelectByTopology` automatically overflows to include $T_1$ (`eu-west-1b` and `eu-west-1c`), returning $T_0$ first followed by $T_1$.

```
                Service Lookup: GET /v1/service/payments?near=self&healthy=true
               (or DNS query: payments.service.norca / template: nomadService)
                                              |
                                              v
                       +---------------------------------------------+
                       | 1. Filter by Discoverable()                 |
                       |    Ready == true && Health != "critical"    |
                       |    (or Discovery.IncludeNotReady == true)   |
                       +----------------------+----------------------+
                                              |
                                              v
                       +---------------------------------------------+
                       | 2. Evaluate Local Region Instances (I)      |
                       |    Is |I| > 0 ?                             |
                       +----------+---------------------------+------+
                             Yes  |                           | No
                                  v                           v
        +-----------------------------------------+   +---------------------------------------+
        | 3a. Apply SelectByTopology (prefer=dc)  |   | 3b. Cross-Region Failover Chain       |
        |     T0: inst.Datacenter == near.DC      |   |     For each reg in Discovery.Failover|
        |     T1: inst.Datacenter != near.DC      |   |     -> Query peer region via RPC      |
        |     If |T0| >= max(MinLocal, 1)         |   |     -> Return first region with       |
        |        && |T0| >= OverflowFraction*|I|  |   |        > 0 eligible instances         |
        |     Then return T0                      |   |     (Keep local region QueryMeta.Index|
        |     Else return T0 + T1                 |   |      so blocking queries wake locally)|
        +--------------------+--------------------+   +-------------------+-------------------+
                             |                                            |
                             +---------------------+----------------------+
                                                   |
                                                   v
                       +---------------------------------------------+
                       | 4. Optional Rendezvous Hashing (?choose)    |
                       |    w_i = MD5(Port || AllocID || ID || NS || |
                       |              Address || Name || Tags || Key)|
                       |    Sort ascending by w_i, take top N        |
                       +---------------------------------------------+
```

### 3.3 Cross-Region Failover & Blocking Query Semantics
When all instances in the local region are unhealthy or absent ($|I| = 0$), cross-region failover is executed in two places depending on the access path:
1. **HTTP API & Workload Templates (`norca/command/agent/service_discovery.go:83`)**: `HTTPServer.serviceFailover` iterates over `reply.Discovery.Failover` in order. For each target region, it issues a non-blocking `ServiceRegistration.GetService` RPC (`MinQueryIndex = 0`, `Near = nil`) over the WAN RPC mesh. The first region returning non-empty services populates `reply.Services`. Crucially, **`reply.QueryMeta.Index` retains the local region's index** so that blocking queries (`consul-template` / `nomadService`) immediately wake up and fail back as soon as a local instance recovers.
2. **Built-in DNS Server & Client Cache (`norca/client/discovery/resolver.go:59`)**: The client-side `discovery.Cache` (`norca/client/discovery/cache.go:51`) maintains background blocking watches (`defaultWaitTime = 1m`, `defaultIdleTimeout = 5m`) per `(Region, Namespace, Name)` key and preserves the last known `ServiceDiscovery` settings even when local registrations drop to zero (`cache.go:175`). `Resolver.Resolve` then queries the cached failover regions in order (`resolver.go:80-92`).

### 3.4 Workload Defaults (`builtin` Transport) & Rendezvous Hashing
Requests arriving over the client's internal workload listener (`s.Addr == "builtin"`, used by `template` blocks and the Task API socket `${NORCA_SECRETS_DIR}/api.sock`) receive automatic discovery defaults (`norca/command/agent/service_discovery.go:22-74`):
- **`near`**: Defaults to `"self"` (the client agent's `(Region, Datacenter)` topology) unless `?near=none` is explicitly passed.
- **`healthy`**: Defaults to `client.native_service_discovery.default_healthy_only` (`norca/norca/structs/config/discovery.go:19`) unless `?healthy=true|false` is explicitly passed. When `nomadService` is invoked in a template with the 3-argument rendezvous hashing signature `nomadService <count> <key> <name>`, `?choose=<count>|<key>` selects a consistent subset of $N$ instances using MD5 rendezvous hashing (`ServiceRegistration.HashWith` in `norca/norca/structs/service_registration.go:228`).

---

## 4. Client-Local Consul Compatibility Layer (`norca/client/consulcompat/`)

To allow existing Consul-coupled ecosystems—such as Traefik's `consulCatalog` provider, Prometheus's `consul_sd_configs`, `consul-template`'s `service` / `connect` functions, and legacy self-registering microservices—to run unmodified on Norca without deploying Consul, each Norca client includes a Consul HTTP API emulator (`norca/client/consulcompat/`) and a `.consul` DNS zone handler (`norca/client/dns/`).

### 4.1 Supported Endpoints vs. Intentional Boundaries
The compatibility server (`consulcompat.Server`, `norca/client/consulcompat/server.go:95`) listens on `client.consul_compat.bind` (default `127.0.0.1:8500`) and translates Consul HTTP requests into native Norca RPCs:

| Category | HTTP Method & Path | Norca Backing Implementation & Behavior |
| :--- | :--- | :--- |
| **Agent & Status** | `GET /v1/agent/self` | Returns local `structs.Node` metadata, `Product: "norca"`, `Version: version.NorcaVersion` (`endpoints.go:250`). |
| | `GET /v1/status/leader` | Calls `Status.Leader` RPC (`endpoints.go:288`). |
| | `GET /v1/status/peers` | Calls `Status.Peers` RPC (`endpoints.go:298`). |
| **Catalog Read** | `GET /v1/catalog/datacenters` | Calls `Region.List` when `datacenter_from = "region"` (default), or collects datacenters from `Node.List` when `datacenter_from = "datacenter"` (`endpoints.go:312`). |
| | `GET /v1/catalog/services` | Aggregates service names and union of tags across configured `namespaces` via `ServiceRegistration.List` (`endpoints.go:341`). |
| | `GET /v1/catalog/service/:name` | Calls `ServiceRegistration.GetService`, enriches with 30s-cached node attributes (`nodeCache`), applies `?tag`, `?dc`, `?node-meta`, `?near=_agent`, and `go-bexpr` `?filter` (`endpoints.go:423`). |
| | `GET /v1/catalog/nodes` | Lists cluster nodes via `Node.List` with `nodeCache` enrichment (`endpoints.go:440`). |
| | `GET /v1/catalog/node/:node` | Returns node details and all native service instances running on that node (`endpoints.go:472`). |
| **Health Read** | `GET /v1/health/service/:name` | Returns `[]*consulapi.ServiceEntry` combining `serfHealth`, synthetic `nomad-health:<id>` readiness check, and individual checks; filters by `?passing` and `?filter` (`endpoints.go:513`). |
| | `GET /v1/health/checks/:name` | Returns `[]*consulapi.HealthCheck` for all instances of `:name` (`endpoints.go:534`). |
| | `GET /v1/health/state/:state` | Returns all node and service checks matching `any`, `passing`, `warning`, or `critical` (`endpoints.go:553`). |
| | `GET /v1/health/connect/:name` | Blocks on catalog index for `:name` and returns an empty `[]*consulapi.ServiceEntry{}` so Connect-aware watchers do not spin (`endpoints.go:591`). |
| **Agent Write / Local** | `PUT /v1/agent/service/register` | Converts `consulapi.AgentServiceRegistration` into a node-owned external service (`ID: "_consul-agent-<nodeID>-<localID>"`, `Source: "consul"`) via `ServiceRegistration.UpsertExternal` (`agent.go:105`). |
| | `PUT /v1/agent/service/deregister/:id`| Deletes the local node-owned external service via `ServiceRegistration.DeleteExternal` (`agent.go:258`). |
| | `PUT /v1/agent/check/pass/:id`<br>`PUT /v1/agent/check/warn/:id`<br>`PUT /v1/agent/check/fail/:id` | Renews/updates the TTL check `:id` with optional `?note=` output via `ServiceRegistration.UpdateExternalChecks` (`agent.go:280`). |
| | `PUT /v1/agent/check/update/:id` | Updates TTL check `:id` with JSON `{"Status": "...", "Output": "..."}` (`agent.go:288`). |
| | `GET /v1/agent/services`<br>`GET /v1/agent/service/:id`<br>`GET /v1/agent/checks` | Serves external services and checks owned by the local node from `externalsvc.Agent` (`agent.go:360-424`). |
| **Unsupported** | `/v1/kv/*`, `/v1/session/*`, etc. | Returns `501 Not Implemented` (`server.go:201`). Key-value configuration and secrets in Norca use native encrypted Variables (`nomadVar`) and the native Secret Store (`secret`). |

### 4.2 Namespace Prefixing, Blocking Headers, and ACL Passthrough
- **Multi-Namespace Mapping (`namespaceSeparator = "--"`)**: Because OSS Consul has a flat service namespace, `client.consul_compat.namespaces` accepts an ordered list of Nomad namespaces (default `["default"]`). Services in `namespaces[0]` appear under their bare name (e.g., `web`), whereas services in subsequent namespaces are exposed as `<namespace>--<name>` (e.g., `prod--web`) and transparently split back via `nomadName()` on lookups and registrations (`norca/client/consulcompat/server.go:442-457`).
- **Blocking Queries & Consul Headers**: Query parameters `?index=<uint64>`, `?wait=<duration>` (default `5m`, capped at `maxWait = 10m`), and `?stale` map directly to `structs.QueryOptions`. Responses emit `X-Consul-Index`, `X-Consul-KnownLeader`, `X-Consul-LastContact`, and `X-Consul-Effective-Consistency` (`server.go:426-439`).
- **ACL Token Resolution**: The compatibility layer extracts tokens in priority order from `X-Consul-Token`, `Authorization: Bearer <token>`, `?token=`, or falls back to `client.consul_compat.anonymous_token` (`server.go:333-344`), forwarding the token as `AuthToken` on every Nomad RPC so standard Nomad ACL policies govern all reads and writes.
- **Node Cache (`nodeCache`)**: Because Consul catalog responses embed node IP addresses and `NodeMeta` on every service instance, `consulcompat` caches `Node.GetNode` responses for `nodeTTL = 30s` using the client's `NodeSecret()` (`norca/client/consulcompat/nodes.go:15-70`). Unowned external services are synthesized on a virtual node `Name: "external"` with `Meta: {"external-node": "true", "external-probe": "false"}` (`nodes.go:74`).

### 4.3 Automatic Self-Loop Prevention (`avoidConsulCompatLoop`)
Both Nomad's legacy `consul` block and `client.consul_compat.bind` default to `127.0.0.1:8500`. If both are active on the same host, the Nomad agent would attempt to register its own internal agent services into its own `consulcompat` listener, fingerprint itself as a real Consul agent, and loop indefinitely.

During agent startup, `avoidConsulCompatLoop` (`norca/command/agent/consul_compat_loop.go:117`) inspects whether `consul.address` resolves to `client.consul_compat.bind` (`addrTargetsListener`). If they overlap, Norca automatically disables:
- `consul.auto_advertise = false`
- `consul.server_auto_join = false`
- `consul.client_auto_join = false`
- Appends `"consul"` to `client.options["fingerprint.denylist"]`

while keeping `consul.address` active for `template` blocks so `{{ service "web" }}` templates render seamlessly through `client.consul_compat`.

---

## 5. Event Broker & NATS Event Bridge (`norca/norca/stream/` & `norca/natsbridge/`)

### 5.1 Server-Side Event Broker & Per-Topic Watermarks
Whenever a Raft log entry commits on the servers, state changes are published to the in-memory `EventBroker` (`norca/norca/stream/event_broker.go:50`), which maintains a ring buffer (`eventBuffer`, default `event_buffer_size = 1000`).

To support lossless event bridging, Norca extends the event broker with **Exact-Index Subscriptions (`StartExactlyAtIndex` / `?exact=true`)** and **Per-Topic Index Watermarks (`indexWatermarks`)** (`norca/norca/stream/index_watermarks.go:22`):
1. **Watermark Tracking**: `indexWatermarks` records `base` (the Raft snapshot index from which the server's state store was restored, refined per topic via `baseByTopic`) and `dropped[topic]` (the highest Raft index of any evicted ring-buffer item containing an event of `topic`).
2. **Strict Gap Detection (`410 Gone`)**: When a subscriber requests `?index=N&exact=true`, `EventBroker.subscribe` (`event_broker.go:182-190`) computes the highest potentially missing index across the subscribed topics:
   $$M(T) = \max_{t \in T} \left( \max(\text{baseByTopic}[t], \text{dropped}[t]) \right)$$
   If $N \le M(T)$, the broker immediately rejects the subscription with `ErrIndexNotAvailable`, which the HTTP handler translates to `HTTP 410 Gone`.
3. **Fine-Grained Topic ACL Policies**: In addition to core topics (`Job`, `Evaluation`, `Allocation`, `Deployment`, `Node`, `Service`), Norca registers custom `topicPolicy` handlers in `norca/norca/stream/` for:
   - `Variables` (`topics.go:41`): Supports glob key patterns (`*`) authorized against `variables { path "..." { capabilities = ["list"] } }`.
   - `CustomObject` (`topics_objects.go:18`): Supports `group/Kind` or `group/Kind/name` glob keys authorized per event against object `read` rules.
   - `Scaling` (`topics_scaling.go:14`): Authorized with `read-job` or `list-scaling-policies`.
   - `Secret` & `SecretAccess` (`topics_secrets.go:13`): Metadata/hash events authorized via `list-secrets`; access audit events require `operator:read`.
   - `ServiceIntention` (`topics_intention.go:14`), `NetworkPolicy` (`topics_network_policy.go:15`), and `Backup` (`topics_backup.go:14`).

### 5.2 `norca-natsbridge` Architecture & Streaming Pipeline
`norca-natsbridge` (`norca/natsbridge/`) is an independent Go module and binary (`norca/natsbridge/cmd/norca-natsbridge/main.go`) that streams events from one Norca region into NATS Core or NATS JetStream. Crucially, **cluster operations never depend on NATS**: NATS is used strictly as an outbound event and audit bus.

```
+---------------------------------------------------------------------------------------------------+
|                                   NORCA-NATSBRIDGE PIPELINE                                       |
|                                                                                                   |
|  1. Acquire Lease         +----------------------------------------------------+                  |
|     (LeaderElector)  ---> | Variable: <state-prefix>/lease (20s TTL, CAS)      |                  |
|                           +-------------------------+--------------------------+                  |
|                                                     | Elected Leader                              |
|                                                     v                                             |
|  2. Load Checkpoint       +----------------------------------------------------+                  |
|     (Checkpointer)   ---> | Variable: <state-prefix>/checkpoint (index K)      |                  |
|                           +-------------------------+--------------------------+                  |
|                                                     | Resume from next = K + 1 (exact=true)       |
|                                                     v                                             |
|  3. Subscribe NDJSON      +----------------------------------------------------+                  |
|     (APISource)      ---> | GET /v1/event/stream?index=K+1&exact=true&topic=.. |                  |
|                           +-------------------------+--------------------------+                  |
|                                                     |                                             |
|                        +----------------------------+----------------------------+                |
|                        | HTTP 200 OK (NDJSON Batches)                            | HTTP 410 Gone  |
|                        v                                                         v                |
|  4a. Filter & Enrich   +------------------------------------+   4b. Emit Gap     +--------------+ |
|      - Skip own Lease/ | Prepend {"Region":"<region>",...}  |       Marker       | Topic:Bridge | |
|        Checkpoint Vars | Keep raw bytes (no float64 loss)   |                    | Type:EventGap| |
|      - Filter types    +-----------------+------------------+                    +-------+------+ |
|                                          |                                               |        |
|                                          +-----------------------+-----------------------+        |
|                                                                  |                                |
|                                                                  v                                |
|  5. PublishBatch       +-----------------------------------------------------------------------+  |
|     (Publisher)        | Subject: <prefix>.<region>.<Topic>                                    |  |
|                        | Header Nats-Msg-Id: <region>:<index>:<topic>:<type>:<namespace>:<key> |  |
|                        | - JetStream: PublishMsgAsync + wait for all PubAckFutures             |  |
|                        | - Core NATS: PublishMsg + FlushWithContext                            |  |
|                        +------------------------------------+----------------------------------+  |
|                                                             |                                     |
|                                                             v                                     |
|  6. Save Checkpoint    +-----------------------------------------------------------------------+  |
|     (CAS Monotonic)    | Every 1s (dirty) or 1m (idle skipped events) or on graceful shutdown  |  |
|                        +-----------------------------------------------------------------------+  |
+---------------------------------------------------------------------------------------------------+
```

#### Subject Hierarchy, Headers, and Byte-Exact Payloads
- **Subject Format (`norca/natsbridge/publish.go:86`)**:
  $$\text{Subject} = \langle\text{prefix}\rangle.\langle\text{sanitized\_region}\rangle.\langle\text{sanitized\_topic}\rangle$$
  where `<prefix>` defaults to `nomad` (configurable via `-subject-prefix`, e.g., `norca.events`) and forbidden subject characters (`.`, `*`, `>`, whitespace) in region or topic names are mapped to `_`.
- **Zero-Reencoding JSON Injection (`withRegion`, `publish.go:98`)**: Standard `api.EventStream` unmarshals event payloads into `map[string]interface{}`, which converts 64-bit Raft indexes and Unix nanosecond timestamps into IEEE-754 `float64` values (losing precision above $2^{53}$). `APISource` (`norca/natsbridge/stream.go:109`) keeps `Event.Raw` as `json.RawMessage` and splices `{"Region":"<region>",` directly after the opening `{` byte, preserving 64-bit integer precision byte-for-byte.
- **NATS Message Headers (`publish.go:23-30`)**: Every message carries metadata headers so NATS consumers and JetStream subject filters can inspect events without parsing JSON bodies:
  - `Nats-Msg-Id`: `<region>:<index>:<topic>:<type>:<namespace>:<key>` (deterministic JetStream deduplication ID)
  - `Nomad-Region`, `Nomad-Index`, `Nomad-Topic`, `Nomad-Type`, `Nomad-Namespace`, `Nomad-Key`

#### Delivery Guarantees, Checkpointing, and Self-Write Loop Prevention
- **At-Least-Once Delivery with JetStream Deduplication**: A batch (all events sharing one Raft `Index`) is considered published only after `JetStreamPublisher` receives `PubAck` futures for every message in the batch (`publish.go:214-235`), or `CorePublisher` completes `Conn.FlushWithContext` (`publish.go:189`). Furthermore, `connectNATS` disables the NATS client's internal in-memory reconnect buffer (`nats.ReconnectBufSize(-1)` in `norca/natsbridge/run.go:188`) so disconnected publishes fail immediately and retry cleanly from the exact Raft index upon reconnection.
- **Monotonic CAS Checkpointing (`VariableCheckpoint`, `norca/natsbridge/checkpoint.go:47`)**: By default (`-checkpoint=variable`), the bridge persists the last published Raft index in a Nomad Variable at `<state-prefix>/checkpoint` (default `norca/jobs/<NORCA_JOB_ID>/checkpoint`). Writes use `ModifyIndex` check-and-set and verify `stored < index` (`checkpoint.go:75-100`), guaranteeing that a zombie leader cannot roll the checkpoint backwards.
- **Self-Write Feedback Prevention (`SkipVariables`, `norca/natsbridge/bridge.go:120`)**: When the bridge subscribes to the `Variables` or `*` topic, its own writes to `<state-prefix>/checkpoint` and `<state-prefix>/lease` generate new `Variables` events in the stream. `SkipVariables(stateNamespace, checkpointPath, leasePath)` drops those events (`norca_natsbridge_events_skipped_total{reason="own"}`) and throttles idle checkpoint advances to `CheckpointIdleInterval = 1m` (`bridge.go:98-99`), keeping the cursor inside the server's ring buffer without creating a write amplification loop.
- **`410 Gone` Gap Marker Emission (`handleGone`, `bridge.go:240`)**: If the bridge is offline longer than the server's `event_buffer_size` window and receives `410 Gone` (`ErrIndexGone`), it publishes a structured `EventGap` message to `<prefix>.<region>.Bridge` with `Nats-Msg-Id: <region>:gap:<FromIndex>` (`publish.go:133-160`), increments `norca_natsbridge_gaps_total`, and resumes streaming from the newest events (`index = 0, exact = false`).

---

## 6. Server, Client, Jobspec, and NATS Bridge Configuration Reference

### 6.1 Server & Client Configuration (`norca.hcl`)

```hcl
region     = "aws-eu-west-1"
datacenter = "eu-west-1a"

server {
  enabled = true

  # Ring buffer size for /v1/event/stream (size for maximum NATS outage window)
  event_buffer_size = 10000

  # Default topology overflow threshold applied to services at job registration
  native_service_discovery {
    default_overflow_fraction = 0.2
  }
}

client {
  enabled = true

  # Default healthy-only filtering for workload template / Task API lookups
  native_service_discovery {
    default_healthy_only = true
  }

  # Built-in DNS server answering from the native service catalog
  dns {
    enabled       = true
    bind          = "127.0.0.1:8600"
    domain        = "norca"
    consul_compat = true               # Also answer *.service.consul
    ttl           = "5s"
    namespaces    = ["default", "prod"]
    recursors     = ["10.0.0.2:53"]
  }

  # Local Consul-compatible HTTP API (:8500) for Traefik, Prometheus, etc.
  consul_compat {
    enabled         = true
    bind            = "127.0.0.1:8500"
    datacenter_from = "region"         # "region" (default) or "datacenter"
    namespaces      = ["default", "prod"]
    anonymous_token = ""               # Optional read-only ACL token for unauthenticated callers
  }
}
```

### 6.2 Workload Jobspec with Check Roles & Topology Discovery

```hcl
job "payments-api" {
  region      = "aws-eu-west-1"
  datacenters = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
  type        = "service"

  group "api" {
    count = 6

    network {
      port "http" { to = 8080 }
    }

    service {
      name        = "payments-api"
      port        = "http"
      provider    = "nomad"
      tags        = ["v2", "http"]
      canary_tags = ["v2", "canary"]

      # AZ-aware routing with regional overflow and multi-cloud failover
      discovery {
        prefer            = "datacenter"
        min_local         = 2
        overflow_fraction = 0.25
        failover          = ["aws-eu-central-1", "gcp-europe-west1"]
        include_not_ready = false
      }

      # 1. Startup probe: gates readiness/liveness until migrations complete
      check {
        name            = "startup-probe"
        role            = "startup"
        type            = "http"
        path            = "/healthz/startup"
        interval        = "2s"
        timeout         = "1s"
        startup_timeout = "3m"
      }

      # 2. Readiness probe: controls ServiceRegistration.Ready in catalog/DNS
      check {
        name                   = "readiness-probe"
        role                   = "readiness"
        type                   = "http"
        path                   = "/healthz/ready"
        interval               = "5s"
        timeout                = "2s"
        failures_before_critical = 2
        success_before_passing   = 2
      }

      # 3. Liveness probe: restarts wedged container without altering catalog
      check {
        name     = "liveness-probe"
        role     = "liveness"
        type     = "http"
        path     = "/healthz/live"
        interval = "10s"
        timeout  = "2s"
        check_restart {
          limit = 3
        }
      }
    }

    task "server" {
      driver = "docker"
      config {
        image = "registry.example.com/payments:v2.4.0"
        ports = ["http"]
      }

      # Renders healthy, same-AZ database endpoints (with regional failover)
      template {
        destination = "local/upstreams.json"
        data        = <<-EOT
          [
          {{- range $i, $s := nomadService "postgres-primary" }}
            {{- if $i }},{{ end }}
            {"address": "{{ $s.Address }}", "port": {{ $s.Port }}, "dc": "{{ $s.Datacenter }}"}
          {{- end }}
          ]
        EOT
      }
    }
  }
}
```

### 6.3 `norca-natsbridge` Flag & Environment Reference
All `norca-natsbridge` flags (`norca/natsbridge/config.go:129-168`) can be configured via CLI flags or environment variables:

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `-addr` | `NORCA_ADDR` / `NORCA_ADDR` | `http://127.0.0.1:4646` | Norca HTTP API URL or `unix://${NORCA_SECRETS_DIR}/api.sock` |
| `-token` | `NORCA_TOKEN` / `NORCA_TOKEN` | `""` | Norca ACL token or workload identity JWT |
| `-region` | `NORCA_REGION` / `NORCA_REGION`| Agent's region | Source region for event stream and NATS subject token |
| `-namespace` | `NATSBRIDGE_NAMESPACE` | `"*"` | Target namespace (`"*"` for all namespaces) |
| `-topics` | `NATSBRIDGE_TOPICS` | `Job,Deployment,Evaluation,Allocation,Node,Service,Scaling` | Comma-separated `Topic` or `Topic:key` server-side filters |
| `-event-types` | `NATSBRIDGE_EVENT_TYPES` | `""` (all) | Comma-separated client-side `Event.Type` filter |
| `-start-at` | `NATSBRIDGE_START_AT` | `"now"` | Initial stream cursor when no checkpoint exists (`"now"` or `"oldest"`) |
| `-nats-url` | `NATS_URL` | `nats://127.0.0.1:4222` | Comma-separated NATS server URLs |
| `-nats-creds` / `-nats-nkey` / `-nats-token` | `NATS_CREDS` / `NATS_NKEY` / `NATS_TOKEN` | `""` | NATS authentication credentials, NKey seed file, or token |
| `-nats-ca` / `-nats-cert` / `-nats-key` | `NATS_CA` / `NATS_CERT` / `NATS_KEY` | `""` | NATS TLS root CA and mTLS client certificate/key |
| `-subject-prefix`| `NATSBRIDGE_SUBJECT_PREFIX` | `"nomad"` | Subject prefix (`<prefix>.<region>.<Topic>`) |
| `-jetstream` | `NATSBRIDGE_JETSTREAM` | `false` | Publish via JetStream and await `PubAck` per batch |
| `-jetstream-stream` | `NATSBRIDGE_JETSTREAM_STREAM` | `""` | Expected JetStream stream name |
| `-jetstream-create` | `NATSBRIDGE_JETSTREAM_CREATE` | `false` | Auto-create `-jetstream-stream` for `<prefix>.<region>.>` if absent |
| `-jetstream-duplicates` | `NATSBRIDGE_JETSTREAM_DUPLICATES`| `10m` | `Nats-Msg-Id` deduplication window for auto-created streams |
| `-checkpoint` | `NATSBRIDGE_CHECKPOINT` | `"variable"` | Cursor persistence backend: `"variable"`, `"file"`, or `"none"` |
| `-checkpoint-path`| `NATSBRIDGE_CHECKPOINT_PATH` | `<state-prefix>/checkpoint` | Variable path or filesystem path for cursor storage |
| `-checkpoint-interval`| `NATSBRIDGE_CHECKPOINT_INTERVAL`| `1s` | Minimum interval between checkpoint writes after publishing |
| `-leader-elect` | `NATSBRIDGE_LEADER_ELECT` | `true` | Elect a single active publisher using a Nomad Variable lease |
| `-lease-duration`| *(flag only)* | `20s` | Lease TTL before a standby replica takes over (`min 2s`) |
| `-metrics-addr` | `NATSBRIDGE_METRICS_ADDR` | `127.0.0.1:9464` | Prometheus `/metrics` and `/healthz` HTTP bind address |

---

## 7. CLI Command Cheat-Sheet (`norca service`)

All service catalog, external service, and intention operations are grouped under `norca service` (`norca/command/service.go:16`):

| Command | Action | Key Flags |
| :--- | :--- | :--- |
| `norca service list` | List all registered services and tags in namespace(s) | `-namespace *`, `-json`, `-t` |
| `norca service info <name>` | Inspect instances, readiness, health age, and checks | `-verbose`, `-healthy`, `-filter`, `-json` |
| `norca service delete <name> <id>` | Force-remove a specific service registration ID | `-namespace` |
| `norca service register <name>` | Register or update an external IP/hostname service | `-address <ip>`, `-port <p>`, `-id <id>`, `-tag`, `-meta k=v` |
| `norca service register <name>` | Register an external DNS `CNAME` alias service | `-alias <fqdn>`, `-port <p>`, `-deregister-critical-after 1h` |
| `norca service register <name>` | Attach TTL or client-probed HTTP/TCP checks | `-ttl 30s`, `-http <url>`, `-tcp <h:p>`, `-interval 10s` |
| `norca service ttl <id>` | Heartbeat/update external service TTL check status | `-status passing\|warning\|critical`, `-output "..."`, `-check <id>` |
| `norca service deregister <id>` | Deregister an external service registration | `-namespace` |
| `norca service intention create <src> <dst>` | Create/update an mTLS service intention rule | `-allow` (default), `-deny`, `-description "..."` |
| `norca service intention list` | List mTLS service intentions in namespace(s) | `-namespace *`, `-json` |
| `norca service intention info <src> <dst>` | Inspect a specific mTLS service intention | `-json` |
| `norca service intention check <src> <dst>` | Evaluate whether intentions allow `<src> -> <dst>` | `-default-allow` (exits `0` allowed, `2` denied) |

---

## 8. Summary for Network Engineers and SREs

1. **Enable `default_healthy_only = true` on Clients**: For backward compatibility with Nomad 1.6, `client.native_service_discovery.default_healthy_only` defaults to `false`. Production clusters should explicitly set `default_healthy_only = true` in `client.native_service_discovery` so `nomadService` templates and Task API lookups automatically exclude unhealthy or starting allocations.
2. **Register HTTP/TCP External Services via Client Agents**: External services with active `http` or `tcp` checks must be registered against a **Client Agent's HTTP address** (not a Server-only node) so the client attaches its `NodeSecretID` and spawns `externalsvc` check runners. If the owning client node is drained or goes down, understand that its node-bound external registrations are bound to that node's lifecycle; for node-independent external services, use `-ttl` checks or static registrations against any agent.
3. **Dimension `event_buffer_size` for NATS Maintenance Windows**: The server's in-memory `event_buffer_size` (default `1000` Raft index batches) determines how long `norca-natsbridge` or NATS can be unreachable before the server returns `410 Gone` and the bridge emits an `EventGap` marker on `<prefix>.<region>.Bridge`. Monitor `norca_natsbridge_lag_index`, `norca_natsbridge_lag_seconds`, and `norca_natsbridge_gaps_total` via Prometheus.
4. **Resetting the Bridge Checkpoint After Snapshot Restores**: Because `VariableCheckpoint` enforces strict monotonicity (`stored >= index` is ignored), restoring a cluster from an older backup snapshot whose Raft index is lower than the checkpoint requires deleting the checkpoint Variable (`norca var purge norca/jobs/natsbridge/checkpoint`) if the backup itself contained a newer checkpoint or if an external file checkpoint is used.
5. **Drop-In Consul Migration Without Rewriting Templates**: Enabling `client.consul_compat { enabled = true }` and `client.dns { enabled = true, consul_compat = true }` allows workloads using `{{ service "api" }}` in `template` blocks and `api.service.consul` DNS queries to resolve native `provider = "nomad"` services immediately, while `avoidConsulCompatLoop` prevents the Nomad client from registering itself into its own compatibility listener.
