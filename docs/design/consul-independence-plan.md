# Consul independence – plan

**Status:** In progress · 2026-10-25 (phases 1–4 and 6 implemented; phase 5, native mTLS: steps 1–2 implemented, step 3 in progress) · Starting point: Nomad v1.6.5 (MPL 2.0) · Related documents: [nomad-fork-development-plan.md](nomad-fork-development-plan.md), [kubernetes-features-spec.md](kubernetes-features-spec.md)

One of Nomad's biggest practical drawbacks is that service discovery, DNS and the service mesh require Consul to be installed and operated separately. The goal is that most users no longer need Consul, while external tools built on the Consul API (Traefik, Prometheus and many others) keep working unchanged or through a native Nomad integration. Anyone who needs a full service mesh can still use Consul, but Nomad installs and manages it.

## Contents

- [Starting point](#starting-point)
- [External tools built on the Consul API](#external-tools-built-on-the-consul-api)
- [Strategy](#strategy)
- [1. Health-based catalog](#1-health-based-catalog)
- [2. Built-in DNS](#2-built-in-dns)
- [3. Consul-compatible read layer](#3-consul-compatible-read-layer)
- [4. External service registration](#4-external-service-registration)
- [5. Native mTLS and service authorization](#5-native-mtls-and-service-authorization)
- [6. Full Consul as an option](#6-full-consul-as-an-option)
- [Migration from Consul](#migration-from-consul)
- [Schedule](#schedule)
- [Testing and acceptance](#testing-and-acceptance)
- [Risks](#risks)
- [License](#license)
- [Open questions](#open-questions)

## Starting point

### What does Consul provide, and what does Nomad 1.6.5 have without it?

| Area | With Consul | Nomad 1.6.5 natively | Gap |
| --- | --- | --- | --- |
| Service registration and discovery | Yes | Yes, `provider = "nomad"`, with the `nomadService` template function and the API | – |
| Health checks | Yes | Yes, HTTP and TCP, for restarts and deployment health | – |
| Filtering by health in discovery | Yes | **No**: the `ServiceRegistration` struct has no health field, so unhealthy instances are returned as well | Significant |
| DNS interface | Yes (`*.service.consul`) | **No** | Significant |
| Key-value store | Consul KV | Variables (encrypted, with ACLs) | Covered |
| Service mesh (mTLS, Envoy) | Consul Connect | **No**; according to the code, the native providers do not support Connect | The largest |
| Authorization between services | Intentions | **No** | Significant |
| Cross-region discovery | WAN federation | Possible through the API with region forwarding, but inconvenient in templates and DNS | Medium |

### Verified code locations

| Fact | Location |
| --- | --- |
| The native provider is named `nomad`; the code itself notes it comes with "numerous limitations" | `nomad/structs/services.go` (`ServiceProviderNomad`) |
| The native providers do not support Connect | `nomad/structs/services.go` |
| The fields of a native registration contain no health state | `nomad/structs/service_registration.go` |
| The registration path: client (`client/serviceregistration/nsd/`) → `ServiceRegistration.Upsert` RPC → Raft (`ServiceRegistrationUpsertRequestType`) → `service_registrations` table | `nomad/service_registration_endpoint.go`, `nomad/state/schema.go` |
| The `Service` event topic already exists | `nomad/structs/event.go` |
| The `miekg/dns` DNS library is already a dependency | `client/allocrunner/network_hook.go`, `nomad/structs/structs.go` |
| `consul-template` v0.35.0 is built in, with the `nomadService` and `nomadVar` functions | `go.mod`, `client/allocrunner/taskrunner/template/` |

Consul is therefore needed today mainly for three things: health-based discovery, DNS and the service mesh.

## External tools built on the Consul API

Many tools are built on Consul's catalog API. We examined the source code of the two most important ones (Traefik v3.1.0, Prometheus v2.53.0):

| Tool | Consul API calls used | Native Nomad support |
| --- | --- | --- |
| Traefik (`consulcatalog` provider) | `Catalog().Services` (blocking query), `Health().Service` (healthy instances only); in Connect mode `Health().Connect` | Yes, `nomad` provider (`pkg/provider/nomad`) |
| Prometheus (`consul_sd_configs`) | `Agent().Self` (for the datacenter), `Catalog().Services` (blocking, `NodeMeta`, `AllowStale`), `Health().ServiceMultipleTags` (blocking) | Yes, `nomad_sd_configs` (`discovery/nomad`) |

Other typical Consul users: Fabio, older Traefik and Prometheus versions, Spring Cloud Consul and other application frameworks (these often register themselves), consul-template outside Nomad, in-house scripts, and configurations with hard-coded `.service.consul` DNS names.

**Lessons:**

- The most important tools already have a native Nomad integration, but it is built on the Nomad catalog, which lacks health state. Step 1 is therefore a prerequisite for everything else.
- Tools built on the catalog use only a small slice of the Consul API, with blocking queries. This is realistically achievable in a compatibility layer.
- Some frameworks also write (self-registration), which requires a separate concept (step 4).

## Strategy

**Three levels:**

1. **Native integration** where the tool supports it (Traefik `nomad` provider, Prometheus `nomad_sd_configs`, Nomad templates).
2. **Consul-compatible layer** for tools that only know Consul, without configuration changes.
3. **Real Consul as an option**, installed by Nomad, for whatever falls outside the layer (service mesh, sessions, full KV).

**Rejected alternative: embedding Consul in the Nomad binary.** It is legally possible (Nomad 1.6.5 and Consul 1.16.4 are both MPL), and technically they are closer than one would expect: `raft` (v1.5.0), `serf` (v0.10.1), `memberlist` (v0.5.0) and `go-memdb` (v1.3.4) are exactly the same version, gRPC differs (1.56.3 versus 1.59.0), and Consul also brings the Envoy control-plane libraries. The drawbacks are still too large: two Raft instances in one process, a Consul bug can bring down the Nomad server as well, coupled upgrades, and the user still has to work with Consul's concepts. At most it could be considered later as a convenient "all-in-one" mode.

## 1. Health-based catalog

### Goal

The native service catalog stores a health state for every instance, and discovery (API, template, DNS, compatibility layer) returns only healthy instances by default. This is the `Ready` field of item 6 (health check roles) of [kubernetes-features-spec.md](kubernetes-features-spec.md), here with elevated priority.

### Semantics

- New fields of `ServiceRegistration`: `Health` (`passing`, `warning`, `critical`), `Checks` (per check: name, state, last output in brief), `HealthUpdatedAt`.
- If the service has readiness (or role-less) checks, `Health` is their aggregate; if it has no checks, it is `passing`.
- The state is computed by the client (it already runs the native checks today, `client/allocrunner/checks_hook.go`) and sent to the server only **on a state change**, after the `success_before_passing` and `failures_before_critical` thresholds.
- **Topology field:** in addition to `Datacenter` (which already exists today), the registration also gets a `Zone` field, which the client fills in from its own node's attributes at registration time. This is the basis of the topology-aware routing in phase 2.
- **Publishing not-ready instances:** per service, the `discovery { include_not_ready = true }` setting makes instances that are not yet healthy appear in discovery as well (the equivalent of Kubernetes `publishNotReadyAddresses`). It is needed for stateful clusters (databases, NATS), where members must find their not-yet-ready peers at startup.

### Raft load

State changes mean Raft writes, therefore:

- the client collects changes for a short time (default 1 s) and sends them in a single `ServiceRegistration.Upsert` call;
- the state of an instance can change on the server at most every 5 seconds (flapping protection), and intermediate states are merged;
- new metric: `nomad.service_registration.health_updates` per region, measured in the load test.

### API and template

- `GET /v1/service/:name?healthy=true` (new parameter), with the new fields in the response.
- Template: a new `healthy` option for `nomadService`, plus an agent-level setting that makes filtering for healthy instances the default. The old behavior is preserved in a backward-compatible way until the setting is enabled:

```hcl
client {
  native_service_discovery {
    default_healthy_only = true
  }
}
```

### Implementation

`nomad/structs/service_registration.go` (new fields), `client/serviceregistration/nsd/` (sending state changes, batching), `client/allocrunner/checks_hook.go` (state computation), `nomad/service_registration_endpoint.go` (filtering), `client/allocrunner/taskrunner/template/` (template option).

## 2. Built-in DNS

### Goal

Every Nomad client runs a DNS server that answers from the native catalog, so applications can reach each other by name without Consul.

### Names

| Name | Answer |
| --- | --- |
| `<service>.service.nomad` | The healthy instances of the service in the client's region, in the default namespace (A/AAAA) |
| `<service>.service.<namespace>.nomad` | The same in the given namespace |
| `<service>.service.<namespace>.<region>.nomad` | The same in another region, with region forwarding |
| `<tag>.<service>.service.nomad` | Only instances registered with the given tag |
| SRV records | Address and port together |
| `*.service.consul` (optional) | The same, compatible with the old names (part of step 3) |

### How it works

- **Local cache:** the client keeps the catalog for its own region up to date by subscribing to the existing `Service` event topic, so a DNS answer does not require a server request. For requests aimed at another region it caches for a short time.
- **Reachability:** the client listens on `127.0.0.1:8600` (Consul's default DNS port, which also simplifies migration). In `bridge` network mode the client automatically writes the bridge address as the nameserver into the allocations' `resolv.conf`, for the `.nomad` (and optionally `.consul`) zone. **Deferred (decided 2026-10-24):** `resolv.conf` has no port field and cannot be restricted to one zone, so this would need a root-owned listener on `<bridge IP>:53` with mandatory upstream recursors — an architecture change. For now workloads reach the catalog through templates, the Task API and environment variables, and the DNS server is used on the host.
- **Recursion:** requests outside its own zone are forwarded to the configured recursive nameservers, or rejected.
- **TTL:** default 5 s, configurable.
- **Authorization:** for DNS answers the client uses a configured read-only token, which can be restricted per namespace.

```hcl
client {
  dns {
    enabled        = true
    bind           = "127.0.0.1:8600"
    domain         = "nomad"
    consul_compat  = true        # *.service.consul names as well
    recursors      = ["10.0.0.2"]
    ttl            = "5s"
    namespaces     = ["default", "prod"]
  }
}
```

### Topology-aware routing and regional failover

**Goal:** discovery (DNS, template, API, sidecar upstream, Consul-compatible layer) prefers healthy instances in the same zone, then in the same datacenter and region, and if there is no healthy instance in the local region, it fails over to other regions in a specified order. No external Consul is needed.

**Starting point in Nomad 1.6.5:**

| Required data or capability | Status |
| --- | --- |
| The instance's datacenter | Present: the `Datacenter` field of the registration |
| The node's zone | Partly: AWS `platform.aws.placement.availability-zone`, Google Cloud `platform.gce.zone`, Azure zone attribute (`client/fingerprint/env_*.go`); DigitalOcean provides only a region, there is no fingerprinter for Hetzner, so it must be given in the node's `meta` field |
| The instance's zone in the registration | Missing; the `Zone` field of phase 1 fills the gap |
| Consistent subset selection | Present: the `choose` parameter of the service API |

**The source of the zone** is in the client configuration, because it differs per cloud:

```hcl
client {
  topology {
    zone = "${attr.platform.aws.placement.availability-zone}"   # for Hetzner, for example: "${meta.zone}"
  }
}
```

**Per-service configuration:**

```hcl
service {
  name     = "api"
  provider = "nomad"

  discovery {
    prefer            = "zone"                    # "zone", "datacenter", "region" or "none"
    min_local         = 2
    failover          = ["eu-gcp", "us-hetzner"]  # in order, if there is no healthy instance in the local region
    include_not_ready = false
  }
}
```

**Ordering in tiers:** (1) same zone, (2) same datacenter, (3) same region, (4) the regions of the `failover` list in order. By default the response returns the first tier that contains enough instances.

**Overflow protection:** the next tier is also included in the response if the current tier has fewer healthy instances than the `min_local` value, or fewer than a configurable fraction (default 20%) of all healthy instances. This way a small zone does not receive the entire traffic of a large region.

**Where it applies:**

| Consumer | Topology-aware? |
| --- | --- |
| Application using DNS (`*.service.nomad`) | Yes |
| Nomad template (`nomadService`) | Yes |
| Native mTLS sidecar upstream (phase 5) | Yes, with the same ordering |
| Own code on the Nomad API, with the `near=self` parameter | Yes |
| Consul-compatible layer (`/v1/health/service`) | Optionally: the layer can return a list ordered and filtered by the client's zone |
| Traefik `nomad` provider, Prometheus `nomad_sd` and other tools that read the catalog themselves | No: they receive all healthy instances |

For Traefik running at the edge, one Traefik instance per zone is therefore recommended (with a `constraint` block filtering on the zone), or Traefik reads through the Consul-compatible layer with a zone-filtered configuration.

**Cross-region failover:** responses pointing to other regions reach the client's cache through region forwarding, and the connection is established through the native mTLS gateway (phase 5); until then, over the WireGuard mesh between regions. In the multi-cloud setup (one cloud per region), local preference also reduces the cost of cross-region traffic.

**Comparison with Consul:** Consul solves the same thing with the "Near" setting of prepared queries and the failover rules of service resolvers; the native solution replaces this.

### Implementation

A new `client/dns/` package (a server built with the `miekg/dns` library, a catalog cache built on the event stream), and the `resolv.conf` setup in the bridge network configuration (`client/allocrunner/networking_bridge_linux.go`, `network_hook.go`). The topology ordering lives in a shared package (new `nomad/structs/discovery.go` and `client/discovery/`) that DNS, the template functions, the API (`near` parameter, `nomad/service_registration_endpoint.go`) and the Consul-compatible layer all use; the `topology` client setting is part of `client/config/` and `command/agent/config.go`.

## 3. Consul-compatible read layer

### Goal

Tools built on Consul's catalog API connect to the Nomad client instead of a real Consul agent, without modification.

### Endpoints

| Consul endpoint | Mapping to Nomad |
| --- | --- |
| `GET /v1/agent/self` | The client's data; `Config.Datacenter` = the Nomad region (or the datacenter, depending on configuration), `Member` = the client's node |
| `GET /v1/status/leader` | The address of the region's current leader |
| `GET /v1/status/peers` | The addresses of the region's servers |
| `GET /v1/catalog/datacenters` | The regions of the federation |
| `GET /v1/catalog/services` | Service names and tags, with blocking query; `node-meta` filtering based on node meta |
| `GET /v1/catalog/service/:name` | The instances of the service (node, address, port, tags, meta) |
| `GET /v1/catalog/nodes`, `/v1/catalog/node/:node` | The client nodes and the services running on them |
| `GET /v1/health/service/:name` (`passing`, `tag`, `filter`) | Instances with check state; the `passing` filter is based on the `Health` field of step 1 |
| `GET /v1/health/checks/:name`, `/v1/health/state/:state` | The results of the native checks in Consul format |
| `GET /v1/health/connect/:name` | Empty list (there is no Connect); this way Traefik works without Connect mode |

Every other endpoint returns `501 Not Implemented` with a clear error message, so that the gap does not stay hidden.

### Mapping rules

- **Blocking query:** Nomad uses the same model as Consul (`index` and `wait` parameters), so the `X-Consul-Index` header returns the Nomad catalog index, and `X-Consul-KnownLeader` and `X-Consul-LastContact` are computed from the corresponding Nomad data. The `stale` and `consistent` parameters map to Nomad's `stale` setting.
- **Tokens:** a token arriving in the `X-Consul-Token` header or the `token` parameter is interpreted by the layer as a Nomad ACL token, and Nomad's existing permission rules apply to reading services. For requests without a token, a configured default read-only token can be used.
- **Namespaces:** Consul OSS has no namespaces, so the layer shows one configured Nomad namespace, or all of them with namespace prefixes. The `ns` parameter of Consul Enterprise is mapped to the corresponding Nomad namespace.
- **Datacenter:** the `dc` parameter maps to the Nomad region, so in the multi-cloud setup (one region per cloud) the Consul-style view is preserved.
- **Identifiers:** the Consul service ID is the Nomad registration ID, and the node name is the Nomad node name.

### Configuration

```hcl
client {
  consul_compat {
    enabled            = true
    bind               = "127.0.0.1:8500"
    datacenter_from    = "region"      # or "datacenter"
    namespaces         = ["default"]
    anonymous_token    = "…"           # read-only Nomad ACL token
  }
}
```

### Conformance testing

- The layer's tests run with the official `hashicorp/consul/api` Go client (MPL), in parallel against a real Consul and the layer, with the same expected results.
- Integration tests with the Traefik `consulcatalog` and Prometheus `consul_sd_configs` integrations: the appearance, disappearance and health-state changes of a service show up in both the same way as with a real Consul.

### Implementation

A new `client/consulcompat/` package: HTTP handlers, conversion of Nomad structs to Consul format, blocking query on the client's catalog cache (shared with DNS).

## 4. External service registration

### Goal

Registering services in the catalog without an allocation: machines running outside Nomad, managed databases, and frameworks that self-register through the Consul agent API (for example Spring Cloud Consul).

### Semantics

- New registration type: `external`. It is not tied to an allocation but is **TTL-based**: the registrant must renew it periodically, otherwise it goes to `critical` state and is deleted after a configurable time.
- A static (TTL-less) external registration is also possible, for example for the address of a managed database, with operator permission only.
- Its own ACL capability: `register-external-service` at namespace level.
- **DNS alias (the equivalent of Kubernetes ExternalName):** the `alias` type of external registration points to an external DNS name and appears as a CNAME in the native DNS. This way applications reach, for example, a managed database by an internal name, and when the target is replaced (migration, cutover) only the alias needs to be changed:

```hcl
external_service "szamlazas-db" {
  namespace = "prod"
  type      = "alias"
  target    = "szamlazas.c9xk2.eu-central-1.rds.amazonaws.com"
}
# DNS: szamlazas-db.service.prod.nomad  CNAME  szamlazas.c9xk2.eu-central-1.rds.amazonaws.com
```

### Endpoints

- Native: `PUT /v1/service/external/:name`, `PUT /v1/service/external/:id/ttl`, `DELETE ...`
- Consul-compatible (in the layer of step 3): `PUT /v1/agent/service/register`, `PUT /v1/agent/service/deregister/:id`, `PUT /v1/agent/check/pass|warn|fail/:check_id` and `PUT /v1/agent/check/update/:check_id` (TTL checks).

### Implementation

External services are rows of the existing `service_registrations` table with an `External` block (kind, source, check definitions, `DeregisterCriticalAfter`) and no job or allocation, so the catalog, the event stream, DNS and the compatibility layer serve them unchanged (`nomad/structs/service_external.go`). No new Raft message type is needed. TTL checks are tracked in memory on the leader like node heartbeats (`nomad/service_external.go`): renewals reset a deadline, only status changes are written, a new leader gives every check a full TTL, and registrations critical for longer than `DeregisterCriticalAfter` (default 1h through the native API) are deregistered. Registrations made through a client agent with HTTP or TCP checks, and every registration made through the Consul agent endpoints, are owned by that client node: it runs their checks (`client/externalsvc/`) and they are removed when the node goes down. The Consul agent endpoints live in `client/consulcompat/agent.go`; the native endpoints, API client and the `service register|deregister|ttl` commands in `command/agent/service_external_endpoint.go`, `api/services_external.go` and `command/service_external.go`.

### Non-goal

Mapping Consul KV to Variables. The two models differ too much (Variables are a key-value map belonging to a path, with encryption, while Consul KV stores raw bytes with flags and sessions); a half-working mapping would do more harm than good.

## 5. Native mTLS and service authorization

### Goal

An encrypted, mutually authenticated connection between services, and control over who may connect to whom, without Consul Connect. This is the largest item, but strategically the most important, because it also solves the hardest problem of item 7 (network rules) of [kubernetes-features-spec.md](kubernetes-features-spec.md): because of address translation between nodes, the IP address does not identify the sender, but the certificate does.

### Design

- **Per-region Nomad CA:** managed by the leader, with the key material in the keystore (backed up according to the backup plan). The CAs of the regions know each other through a shared trust bundle, so it works across regions and clouds.
- **Certificate from the workload identity:** based on the task's workload identity, the client requests a short-lived (default 24-hour) X.509 certificate with a SPIFFE-style identifier: `spiffe://<trust-domain>/ns/<namespace>/job/<job>/group/<group>`. The certificate and the trust bundle are placed in the task's `secrets` directory and are renewed automatically before expiry.
- **Usage:** either the application uses it itself (most languages' TLS libraries handle it), or a lightweight sidecar proxy, enabled by a block of the jobspec. Envoy can also be used; it receives the certificates from a file or through SDS.
- **Authorization:** a new `ServiceIntention` object (source and destination service, `allow` or `deny`), which the sidecar or a library checks at connection setup based on the certificate's identifier. The rules are cached on the clients and updated through the event stream.
- **Gateways:** in the multi-cloud setup, instead of Consul mesh gateways, a native mTLS gateway (a TCP proxy job managed by Nomad) that forwards between regions based on SNI.

### Jobspec

```hcl
service {
  name     = "api"
  port     = "http"
  provider = "nomad"

  mtls {
    mode = "sidecar"      # or "app": the application uses the certificate itself
    upstream "db" {
      service    = "postgres"
      local_port = 5432
    }
  }
}
```

### Schedule within this item

1. CA and certificate issuance, "app" mode (the application uses it itself).
2. `ServiceIntention` objects and a Go library for the check.
3. Sidecar mode, upstreams, cross-region gateway.

### Implementation

Steps 1 and 2 are implemented; step 3 (the Envoy sidecar, upstreams, the gateway) is not.

- **CA and key material** (`nomad/mtls_ca.go`): the leader of every region creates an ECDSA P-256 CA whose self-signed certificate carries the trust domain as URI SAN (`spiffe://<trust-domain>`). Certificate and key live in the Raft table `mtls_roots` (message type 97, snapshot type 97); the PKCS #8 key is encrypted with the active root key of the keyring, like Variables. It is therefore replicated to every server, part of every snapshot and backup, and decryptable after a full restore, which restores the keyring of the backup first. After a root key rotation the leader re-encrypts the CA key with the new root key, and the keyring garbage collector never deletes a root key that still encrypts a CA key. The CA is rotated automatically after two thirds of its lifetime (default 5 years) or when the trust domain changes, and on request (`PUT /v1/mtls/ca/rotate`, `MTLS.RotateCA`, management token). A retired CA loses its key but stays in the trust bundle until `leaf_cert_ttl` + 1h after the rotation.
- **Trust across regions:** every leader periodically (1 min) fetches the public roots of every other region through region forwarding (`MTLS.Roots`, which needs no ACL capability because the certificates are public) and stores them next to its own; a region that is unreachable keeps its last known roots. No region is special. The default trust domain is `<authoritative region>.nomad`, so a federation shares one trust domain; the library still trusts a CA only for the trust domain in its own certificate.
- **Issuance** (`MTLS.SignCertificate`, `client/allocrunner/taskrunner/mtls_hook.go`): for tasks of a task group with a service that has an `mtls` block, the client generates the key pair itself and sends only a CSR, together with the workload identity token of the task. The server derives everything else from the allocation: `spiffe://<trust-domain>/ns/<namespace>/job/<job>/group/<group>` (dispatched and periodic children use the parent job ID) and DNS SANs `<service>.service.<namespace>.nomad`, `<service>.service.<namespace>.<region>.nomad` (and `<service>.service.nomad` in the default namespace) for the mtls services of the group. With ACLs, the caller must also be the node running the allocation or the task itself. The files are `secrets/mtls/cert.pem`, `key.pem` and `bundle.pem`, named by `NOMAD_MTLS_CERT_FILE`, `NOMAD_MTLS_KEY_FILE`, `NOMAD_MTLS_CA_FILE` (plus `NOMAD_MTLS_SPIFFE_ID`). The client renews at two thirds of the lifetime (minus up to 5% jitter), follows the trust bundle with a blocking query, rewrites the bundle on every change and re-issues the certificate when the active CA is no longer the one that signed it.
- **Jobspec:** `service { mtls { mode = "app" } }`, only with `provider = "nomad"`. `mode = "sidecar"` and `upstream` blocks are parsed and rejected at validation as not implemented yet. Job names, group names and namespaces of mtls jobs must fit into a SPIFFE path segment (`[A-Za-z0-9._-]`). Enabling or disabling mtls on a group is a destructive update.
- **Server configuration:** `server { mtls { enabled = true, trust_domain, leaf_cert_ttl = "24h", ca_cert_ttl = "43800h", federation = true } }`.
- **ServiceIntention** (`nomad/service_intention_endpoint.go`, message types 98/99, snapshot type 98, table `service_intentions`, event topic `ServiceIntention`): source namespace and service, destination service, `allow` or `deny`, owned by the namespace of the destination; `*` matches any service or namespace. The most specific matching intention decides (exact destination, then exact source namespace, then exact source service), and at equal specificity deny wins. Without a matching intention the library denies by default (`Authorizer.DefaultAllow` changes it). Intentions are regional: the destination region's intentions apply. ACL: writing needs the new namespace capability `write-intention`, which no policy shorthand contains (like `register-external-service`); reading needs `read-job`, or a workload identity of the same namespace, so an application reads the intentions it enforces with its own token. HTTP: `GET /v1/service-intentions`, `GET|PUT|DELETE /v1/service-intention?source=&source_namespace=&destination=`, `GET /v1/mtls/roots`. CLI: `norca service intention create|delete|list|info|check`.
- **Go library** `github.com/hashicorp/nomad/api/mtls` (standard library and the API client only): SPIFFE ID parsing, a trust bundle that verifies peers per trust domain, `FileSource` that reloads the files after a renewal and builds server and client `tls.Config` values (peers are verified by SPIFFE ID, not host name), `Evaluate`/`Authorizer` for intentions and `IntentionWatcher`, which keeps the intentions of a namespace cached with blocking queries.
- Every new RPC and the CA are gated on `ServersMeetMinimumNorcaVersion`; message types 97–99 are sent with `IgnoreUnknownTypeFlag`, 100 is reserved.

## 6. Full Consul as an option

Whatever is left out of the above still requires a real Consul:

- Consul Connect, if native mTLS is not yet enough (for example L7 traffic management, Traefik's Connect mode);
- sessions and locking (for example Vault's Consul storage; for Vault the built-in Raft storage is the recommended one today anyway);
- the full KV, prepared queries and config entries.

### Why not Consul v1.16.4?

Since Consul's last MPL release, v1.16.4 (December 2023), nine BSL release branches have appeared (the latest being 2.1.0, October 2026). Among the bugs affecting 1.16.4 that have since been fixed only in BSL releases, the most important ones (based on the open-source CHANGELOG entries) are:

| Fixed in | Bug | Impact on 1.16.4 |
| --- | --- | --- |
| 1.21.5 | Authentication bypass through URL parameters; non-constant-time comparison for sensitive values | Exploitable when the HTTP API is reachable |
| 1.16.6 → 2.x | Envoy support: 1.16.4 supports Envoy 1.23–1.26, which have not received fixes for a long time; today's Consul supports Envoy 1.36–1.39 | The service mesh data plane is outdated |
| 1.20.1 | CVE-2024-10005, CVE-2024-10006: bypass of L7 intentions (path and header normalization) | L7 controls can be circumvented |
| 1.22.0 | CVE-2025-11374, CVE-2025-11375: DoS on the KV and event endpoints; path traversal on KV keys when ACLs are missing | Exploitable when the HTTP API is reachable |
| 1.22.5 | HTTP timeouts against Slowloris; arbitrary file read through the Vault CA provider | DoS; file read when the Vault CA is used |
| 1.16.5, 1.16.7 | At least 2048-bit RSA keys; xDS deadlock and goroutine leak | Weak keys; mesh stability |

For this reason 1.16.4 is not recommended for a production service mesh.

### The recommended path: the official Consul, through the API

- If a full Consul is needed, the user installs **the current version of the official Consul** (the binary or container image released by HashiCorp). The fork connects to it only through the MPL-licensed `github.com/hashicorp/consul/api` client and the documented HTTP API.
- The project **does not distribute, include or modify** Consul. Consul's BSL 1.1 license permits internal production use; the restriction applies to products competing with HashiCorp, and it is worth confirming this legally for your own use case.
- If the package manager offers a Consul package, it only refers to the official HashiCorp image or download, and the user accepts its license themselves at installation; there is no Consul binary in the project's registry.
- Nomad can install and manage Consul (clients as a system job, servers as a service job with a host volume; Nomad does not require Consul to start, so there is no circular dependency), and in the longer term the configuration (gossip key, CA, ACL bootstrap) can be automated with a Consul operator (item 8 of [kubernetes-features-spec.md](kubernetes-features-spec.md)).
- The fork's integration tests run against the current version of the official Consul; this is testing of behavior, and we do not take over any code from it.

**Rejected alternative: a self-maintained MPL Consul 1.16.4.** The fixes above would have to be re-implemented with a clean-room method, and Envoy compatibility would also have to be raised to newer Envoy branches. This is a permanent, large burden and does not serve the project's main goal (Consul independence).

### Interim security minimum if Consul v1.16.4 is running

For example during migration, until traffic moves to native mTLS:

- Consul's HTTP and gRPC APIs are reachable only on localhost and the WireGuard mesh, never on a public network;
- ACLs are mandatory, enabled with a default `deny` rule; there is no anonymous token;
- only L4 (service-level) intentions instead of L7 intentions;
- move the mesh traffic to native mTLS as soon as possible (phase 5).

### Consequence for priorities

The importance of native mTLS (phase 5) grows: it is the only path to a service mesh without a BSL component. If the native solution uses Envoy as a sidecar, it works with the current, openly licensed (Apache 2.0) Envoy releases, not with the old branch tied to Consul 1.16.

### Implementation

The `consul` pack of the pack registry (`packs/consul/`, rendered and run with `norca pack`) installs the official release; the project ships no Consul code, binary or image. The pack references the official `hashicorp/consul` image by registry, repository, tag and optional digest (`image_registry` defaults to `docker.io`; air-gapped installs mirror the unmodified image into the internal OCI registry), or the official release archive by URL and checksum for `exec`/`raw_exec`. It does not render until `consul_license_accepted = true`: the user accepts the BSL 1.1 at install time, and the README states the license.

- **Servers** (`consul`, service job): 3 or 5 servers, one per node marked with node meta `consul_server = "true"`, spread over node meta `zone` (optionally one per zone with the fork's `separate` block), Raft data on a client host volume, static ports on the host network. **Clients** (`consul-clients`, system job): on every other node. Agents find each other through Norca native service discovery (`nomadService "consul-serf"` in a `retry_join` template), so Nomad does not depend on Consul.
- **Bootstrap without manual steps:** a prestart task runs the `norca` CLI through the task API socket with its workload identity (`norca` now accepts `unix://` agent addresses, `command/meta_unix_socket.go`): it generates the gossip key (`operator gossip keyring generate`), a CA and a server certificate for `server.<dc>.<domain>` (`tls ca/cert create`) and the ACL tokens once, stores them in the Variable `nomad/jobs/<job>/servers/bootstrap` with check-and-set (concurrent servers converge on one set) and derives the Variables of the server group, the client job and the snapshot job from it. The servers bootstrap ACLs through `acl.tokens.initial_management`; a poststart task creates the agent policy and token (and the snapshot token) with pre-generated IDs, idempotently. With Norca ACLs, one policy bound to the bootstrap task is the only manual step (printed by the pack, which cannot create policies).
- **Interim security minimum on every install:** ACLs mandatory with `default_policy = "deny"` and no anonymous token, gossip encryption, TLS with verified server names and `auto_encrypt` for the clients, plain HTTP/gRPC on localhost only, HTTPS/gRPC-TLS on localhost and the private network (`host_network`), DNS on localhost, no update check.
- **Operations:** rolling upgrades one server at a time (`update`, `migrate`, `disruption { max_unavailable = 1 }`, servers do not leave on stop), the documented order servers first, then clients; certificate renewal and gossip key rotation are documented manual steps. Optional periodic `consul-snapshot` batch job: `consul snapshot save` on a server node, uploaded with the AWS CLI image (Apache 2.0) to the S3-compatible store of the Norca backups.
- **Norca's own `consul {}` block** is not changed; the README documents the optional wiring (token policy, `auto_advertise`, `server_auto_join = false`, `client_auto_join = false`) and turning the defaults off when Consul is installed but not wired.
- Tests (`nomad/pack/internal/cli/consul_pack_test.go`) render several variable sets, parse and validate the jobs and plan them against a test agent; no test pulls or runs Consul.
- Provisional defaults (reversible): `consul_version = "2.1.0"`, `datacenter = "dc1"`, `image_registry = "docker.io"` without a default digest, `snapshot_upload_image = "docker.io/amazon/aws-cli:2.27.0"`, hourly snapshots.
- Not included: automatic certificate renewal and key rotation (a later Consul operator), WAN federation between regions, a DNS token for Consul DNS.

README feature table row after the merge: "Service discovery without Consul: native mTLS, managed Consul install" — managed Consul install available (phase 6: `consul` pack installing the official, user-accepted BSL release with automated gossip/TLS/ACL bootstrap, rolling upgrades and S3 snapshots); native mTLS (phase 5) still planned.

## Migration from Consul

1. **Parallel registration.** In a transition period, services can be registered with both providers (at the jobspec level: two `service` blocks with different `provider` values, or a new `providers = ["consul", "nomad"]` list).
2. **Switching the readers** one by one: Traefik to the `nomad` provider or the compatible layer, Prometheus to `nomad_sd_configs` or the layer, DNS to the built-in server using the compatibility mode of the `.consul` zone.
3. **Turning off the Consul registration** per service, once all readers have switched.
4. **Removing Consul**, or keeping it only for the mesh (step 6).

## Schedule

| Phase | Content | Prerequisite | Result |
| --- | --- | --- | --- |
| 1 | Health-based catalog, `Zone` field, option for not-ready instances | – | The native Nomad integration of Traefik and Prometheus is safe in production |
| 2 | Built-in DNS, topology-aware routing, regional failover | 1 | Name resolution without Consul, local instances preferred, cross-region failover |
| 3 | Consul-compatible read layer | 1, 2 (shared cache) | Tools that only know Consul work without modification |
| 4 | External registration (with DNS alias) and Consul agent write endpoints | 3 | Self-registering frameworks and services outside Nomad |
| 5 | Native mTLS and authorization | 1 | Replaces most of the service mesh; simplifies level C of K8s item 7 |
| 6 | Installing and managing the official Consul by Nomad, optionally (through the API, not distributed by the project) | – (in parallel) | Easier installation for those who need a full Consul |

After phases 1–3, most users no longer need Consul. Phase 6 can proceed at any time, independently of the others.

## Testing and acceptance

- A failing instance disappears from the responses of the native API, the template, DNS and the compatibility layer within at most 10 seconds after reaching the check threshold.
- The Consul integrations of Traefik and Prometheus with the compatibility layer give the same end result as with a real Consul (conformance test suite).
- The `hashicorp/consul/api` client works flawlessly on the supported endpoints, and gets a clear `501` error on the unsupported ones.
- With 1,000 service instances changing state at the same time, the number of Raft writes is significantly lower than the number of changes thanks to batching and flapping protection, and scheduling latency does not measurably degrade.
- With `prefer = "zone"` and `min_local = 2`, the DNS answer for a client in a zone contains only instances of its own zone as long as at least two are healthy there; if that drops to one, the other instances of the datacenter appear as well; if there is none in the region, those of the first region of the `failover` list.
- An external service of type `alias` appears in the native DNS as a CNAME, and a change of the target takes effect after the TTL expires.
- With `include_not_ready = true`, the members of a starting stateful cluster find each other even before readiness.
- With native mTLS, services running in two different regions in different clouds can connect to each other, and a `deny` intention rejects the connection.

## Risks

| Risk | Mitigation |
| --- | --- |
| Health state changes overload Raft | Only on state change, batched, with flapping protection; metric and load test |
| The compatibility layer's behavior differs subtly from Consul's, and this only shows up in production | Conformance test suite with the official client and the main tools; explicit `501` for unsupported features |
| Users expect full Consul compatibility | The documentation clearly states the supported endpoints and the non-goals |
| A native mTLS bug causes a service outage | Gradual rollout ("app" mode first), certificate renewal well before expiry, the CA's key material is part of the backups |
| Consul v1.16.4 runs temporarily, with unpatched authentication and DoS bugs and an outdated Envoy | The API only on localhost and the WireGuard mesh, mandatory ACL with default `deny`, no L7 intentions; move as soon as possible to native mTLS or the current version of the official Consul |
| The DNS server on the clients adds an attack surface | By default listens only on localhost and the bridge address; read-only token |

## License

- The developments build on the code of Nomad v1.6.5 (MPL 2.0); `miekg/dns` (BSD-3) and `consul-template` (MPL 2.0) are already dependencies.
- `hashicorp/consul/api`, used for the conformance tests, is MPL 2.0.
- The Consul-compatible layer implements the public, documented behavior of Consul's API; it takes over no Consul code. If parts were nevertheless taken over, only from Consul v1.16.4 (MPL), following the file-level MPL rules.
- "Consul" is a trademark: in the documentation it may be used to describe compatibility, but not as the fork's product name or feature name.

## Open questions

- [x] Which Consul-based tools are in use today? These determine which endpoints of the compatibility layer are needed in the first version. Decided 2026-10-24: Traefik, Prometheus and consul-template (service and health functions; Consul KV stays a non-goal).
- [x] Should filtering for healthy instances be the default in the new fork version (a behavior change), or remain opt-in? Decided 2026-10-24: opt-in (`default_healthy_only = false`).
- [x] For native mTLS, do we use Envoy as the sidecar proxy, or our own lighter proxy? Decided 2026-10-24: Envoy.
- [x] Is handling the Consul Enterprise namespace parameter needed in the compatibility layer? Decided 2026-10-24: no.
- [x] What should the default of topology ordering be (`prefer = "zone"` or `"none"`), and what is the default overflow fraction? Decided 2026-10-24: `prefer = "none"` by default, zone preference is opt-in; the overflow fraction keeps the default proposed in this plan.
- [x] From which `meta` key do we take the zone of Hetzner nodes, and who fills it in (installer, configuration management)? Decided 2026-10-24: node meta `zone`, filled in by the installer.
