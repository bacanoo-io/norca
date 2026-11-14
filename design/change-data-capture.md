<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Change Data Capture (CDC) and the Links of the System Bus (draft)

Status: draft for decision, 2026-11-14. This draft began with finding N1 of
`embedded-nats-system-bus.md` section 11: the `leafnode` field of the `bus`
policy rule does nothing, and neither gateways (phase 5) nor leaf nodes are
built. On the maintainer's word (2026-11-14) the feature is named **Change
Data Capture (CDC)**: the one-way change feed of a region to systems outside
Norca. A leaf node is one connection mode of CDC, not the feature; gateways
are a link between regions and not part of CDC.

## 0. Change Data Capture (CDC)

**What it is.** The changes of a region's state, as events, delivered one
way to systems outside Norca: they read, they never write back through it.
Changes are made through the HTTP API.

**What it carries** (streams of the region's bus, section 1.4):

| Stream | Content | Who reads it |
| --- | --- | --- |
| `NORCA_EVENTS_<region>` | the events of the namespaces (jobs, evaluations, allocations, deployments, services, Variables and secret metadata, CSI volumes, scaling, custom objects, ...); Job events without secrets; header `Norca-Job` | tokens and workloads by their `bus` rule |
| `NORCA_ADMIN_EVENTS_<region>` | ACL objects, node pools, nodes, quotas, admission policies, admin network policies, the secret store audit, server roles, backups | live: each topic with the right the event stream API asks; the stream itself: management tokens only |
| `NORCA_KV_CHANGES_<region>` | the Consul KV notices: key and type of each write or delete, never the value | by the `bus` rule |

**Connection modes:**

| Mode | State | Credentials | Catch-up |
| --- | --- | --- | --- |
| Client with an ACL token | built | token, inbox `_INBOX.<accessor>` | its own durable consumer, if its rule allows the JetStream API |
| Task with its workload identity | branch `fix/bus-workload-identity` | identity, inbox `_INBOX.<allocation>.<task>` | as a client |
| Leaf node | planned (option B) | token with the leaf right; one way, server allow list | the server-owned consumer `leaf-<accessor>` per stream |

**Guarantees.** Events of a region in Raft index order, at least once
(deduplication by `Nats-Msg-Id` for two minutes), a gap marker when the
servers no longer hold the events a resume needs, retention `max_age`
(72h) and `max_bytes`. The KV notices in the order the leader wrote the
keys.

**What CDC does not give.** The full record: Job events are redacted, admin
topics need their rights, KV values never travel; a consumer that needs the
full record reads it from the API with its own rights. Other regions'
changes: each region has its own CDC (and `Event.Stream` over RPC for
another region, section 1.3).

**Docs:** a feature page "Change data capture" (what you get, how to
connect, the guarantees); the system bus page stays the technical layer
(streams, subjects, headers).

## 1. Current State

### 1.1 What exists

- **One NATS cluster per region.** Each server runs an embedded
  nats-server (v2.15.0) with JetStream; the routes follow the Serf members
  of the region (`norca/bus_server.go:324-347`). The Serf pool holds the
  servers of every federated region, and the route loop leaves the other
  regions out (`parts.Region != s.config.Region`).
- **Listeners:** client port `4649` and route port `4650` only
  (`norca/bus/config.go:13-20`). The server options set no `Gateway` and
  no `LeafNode` block (`norca/bus/server.go:107-141`). The deployer plan
  knows `bus` and `bus_cluster` only (`norca-deployer/pkg/plan/types.go:302-310`).
- **No NATS accounts.** Every connection lands in the global account of the
  embedded server; the custom authenticator registers a user with
  permissions per connection (`norca/bus/auth.go:87-120`). Since the bus
  fixes of 2026-11-14 a token connection gets its own replies only, under
  `_INBOX.<accessor id>` (`norca/norca/bus_server.go:263-273`,
  `bus/auth.go:127-141`); a node subscribes to nothing.
- **Leaf connections are refused** even if a leaf listener existed: the
  authenticator returns false for every kind but `CLIENT`
  (`norca/bus/auth.go:88`). In nats-server a leaf connection without
  configured leaf users falls through to the client authenticator
  (`nats-server/v2@v2.15.0/server/auth.go:1668-1673`), so the same
  authenticator would decide.
- **Streams and names:**
  - `NORCA_EVENTS_<region>` on `<prefix>.<region>.>` (`bus_events.go:204-208`);
  - `NORCA_TELEMETRY_<region>` on `norca.telemetry.<region>.*`;
  - `NORCA_PACK_PUSHES` on `<prefix>.packs.pushed[.>]`, without a region
    (`bus_events.go:43`, `:377-386`), durable consumer `norca-sync-<region>`;
  - the KV bucket `consul-kv` (`bus/events.go:53`), values sealed with the
    keyring (section 11.4 of the bus design).
  - `bus.CheckSubjects` refuses region names whose subjects collide with the
    telemetry and push subjects (`bus/events.go:246-265`).

### 1.2 The `leafnode` field

- Parsed into `BusPolicy.Leafnode` (`acl/bus.go:29-31`), merged as an OR of
  the policies (`acl/bus.go:120`), returned in `BusGrant.Leafnode`
  (`acl/bus.go:104-111`; true for management and with ACLs off).
- Nothing reads `BusGrant.Leafnode`. `BusGrant.Empty` ignores it
  (`acl/bus.go:94-96`), so a policy with only `leafnode = true` gives a
  connection that is refused.
- Documented as reserved: `norca-doc/content/docs/other-specifications/acl-policy.md:530-531`
  ("the bus does not accept leaf nodes yet"). The system bus page says
  registry syncs on other regions' events come "with the gateways"
  (`features/system-bus.md:173-174`).
- Removing the field from the struct would not refuse it: the policy is
  decoded with HCL v1 (`acl/policy.go:549`), which ignores unknown keys. A
  refusal has to be a check in `BusPolicy.validate` (`acl/bus.go:34-43`).

### 1.3 What crosses regions today without the bus

- **Events of another region:** `Event.Stream` is forwarded to the region
  named in the request (`norca/event_endpoint.go:48-50`), so a leader or an
  outside consumer can already read another region's events over RPC.
- **Keyring:** the keyring is replicated from the authoritative region
  (`norca/encrypter.go:579-`), so a value sealed in one region opens in
  another; this matters only if KV values cross regions.
- **Registry syncs on other regions' pushes:** not built (moved to phase 5
  in section 9.2 of the bus design). Today a pipeline calls `notify-push`
  in each region, or each region syncs on its own schedule.

### 1.4 Since the first draft (branches of 2026-11-14, not merged)

- `fix/bus-workload-identity`: a task connects with its workload identity
  and gets the bus rules of the policies bound to its job; its inbox is
  `_INBOX.<allocation ID>.<task>`; the connection closes when the
  allocation ends.
- `fix/bus-admin-events`:
  - the events the event stream API shows only with a right beyond
    read-job (ACL objects, node pools, nodes, quotas, admission policies,
    admin network policies, the secret store audit, server roles, backups)
    go to `NORCA_ADMIN_EVENTS_<region>` on
    `<prefix>.admin.<region>.<namespace>.<topic>`; a connection gets an admin
    topic only with its right, the admin stream's JetStream API only with a
    management token; no token creates or changes streams;
  - Job events on the bus leave out what may hold a secret (env and meta
    values, embedded templates, driver config, artifact credentials, check
    headers and bodies, dispatch payload), header `Norca-Redacted: true`;
  - Consul KV notices without the value on
    `<prefix>.<region>.<namespace>.ConsulKV.KeySet|KeyDeleted`, stored in
    `NORCA_KV_CHANGES_<region>` (the maintainer's decision); the event
    stream takes `<prefix>.<region>.*.*`, so the two do not share subjects;
  - the header `Norca-Job` on the events of a job.

## 2. What Each Would Give

### 2.1 Gateways (server to server, between regions)

A NATS supercluster: the region clusters joined by gateway connections,
interest-only forwarding between them.

Uses:

1. A consumer connected in one region reads the event stream of another
   (`norca.<other region>.>`), without a second connection.
2. The leader of a region reads the `CustomObject` events of the other
   regions and syncs a local registry when the same registry changed there
   (the deferred phase 5 item).
3. A push notice published in one region reaches the leaders of all
   regions.

What it costs (from how JetStream works in a supercluster):

- **One JetStream meta group for all regions.** The servers of every region
  form one JetStream meta group, with one meta leader somewhere in the
  supercluster. Stream and consumer creates and updates need its quorum.
  A region cut off from the others by a WAN failure keeps its existing
  streams, but can no longer create or update streams or durable consumers
  (the leader does both at every leadership change). Today a region is an
  independent failure domain, as Raft is; with gateways the bus is not.
  JetStream domains do not separate the regions of one supercluster; they
  separate leaf-connected servers.
- **Account-wide names.** Stream names and stream subjects are unique in an
  account across the supercluster. `NORCA_PACK_PUSHES` (no region in its
  name or subjects) and the bucket `consul-kv` would be created by every
  region with the same name and overlapping subjects: they must get the
  region in their names (`NORCA_PACK_PUSHES_<region>`, `consul-kv-<region>`)
  and the push subjects a region token, or one shared stream must be chosen
  on purpose.
- **Policies change meaning.** A rule `subscribe = ["norca.*.>"]` reads the
  events of every region once the regions are joined. Policies are
  replicated from the authoritative region, so the same rule applies
  everywhere.
- **Standalone regions.** A single-server region runs a standalone
  JetStream (section 2 of the bus design). Joining a supercluster makes it
  clustered; the standalone-to-clustered restart and the KV carry-over would
  run on gateway changes too.
- **TLS:** gateway connections verify the peer's certificate. The routes
  accept `server.<region>.norca` of their own region only
  (`bus_server.go:188-218`); gateways would accept the server name of any
  federated region (`server.*.norca` against the CA).

### 2.2 Leaf nodes (a CDC connection mode: an outside NATS server attached to a region's bus)

A NATS server (the `nats` pack, an edge site, a developer's NATS) connects
to a region's bus as a leaf and sees the subjects its token allows, as a
local subscriber would.

Uses:

1. Applications on the `nats` pack consume the events of the region from
   their own NATS, with their own accounts and users, and only the leaf
   holds a Norca token (decision 1 of the bus design).
2. A site with a weak link keeps a local NATS that buffers and forwards
   (store and forward with a JetStream of its own domain).
3. Region to region without a supercluster: each region a JetStream domain
   of its own, joined by leaf connections; a stream in one region sources or
   mirrors another region's event stream over the domain API
   (`$JS.<domain>.API`). Regions stay independent failure domains.

Today an application reaches the same events as a plain client with a
token and its inbox prefix (the lab case `packs` checks this since
norca-lab branch `case-clients-bus-reader`), so use 1 is a convenience,
not a missing capability.

What it costs:

- **Authentication** goes through the same authenticator: allow
  `LEAF` connections when the token's grant has `Leafnode`
  (`bus/auth.go:88`), with `Empty` counting `Leafnode` as a right.
- **Permissions are from the leaf's side.** For a leaf connection the
  publish list is what the leaf may send into the bus, the subscribe list
  what it may receive; nats-server applies them to the subscriptions the
  leaf propagates (`leafnode.go:2470-2490`). A leaf aggregates many
  subscribers behind one connection, so the replies of every client behind
  it share the leaf's grant.
- **The inbox rule.** A leaf propagates the interest of its own clients,
  whose inboxes are `_INBOX.<random>`. With the rule of 2026-11-14 the leaf
  connection may subscribe only below `_INBOX.<accessor id>`: every client
  behind the leaf that makes JetStream requests to the bus would have to
  set that prefix, or the leaf would need `_INBOX.>` and see the replies of
  every other bus connection, which is finding S1 again. The clean way is a
  **NATS account per leaf** on the bus side: the leaf binds to an account
  of its own, which imports the event subjects (and, if wanted, the
  JetStream API of the event stream) from the account of the servers, so
  inboxes no longer share one subject space. That reverses decision 3 of
  the bus design ("no dedicated import-only account").
- **Accounts on the bus** mean the server connections, nodes and tokens
  move from the global account to a named one, the streams are created in
  it, and leaf accounts are made from the tokens at connect time
  (nats-server can register accounts at run time; the account of a leaf is
  named after the token's accessor). This is the largest part of the work.

## 3. Options

### A. Refuse the field now; build neither

- `BusPolicy.validate` refuses `leafnode = true` ("the system bus does not
  accept leaf nodes"); `leafnode = false` stays accepted so that policies
  written from the old examples keep loading. The field and
  `BusGrant.Leafnode` go out of the code later, or stay as the place of a
  future option.
- Docs: `acl-policy.md` says the field is refused when true;
  `system-bus.md` drops the sentence on gateways and names the RPC path for
  other regions' events (`/v1/event/stream?region=<other>`).
- The deferred registry syncs on other regions' changes, if wanted, are
  built over RPC: the leader of each region holds an `Event.Stream`
  subscription (topic `CustomObject`, kind `PackRegistry`) to each other
  region, forwarded by the existing region forwarding, and syncs the
  matching local registry with the existing cooldown. No bus change.
- Config surface: none. Firewall: none. Tests: policy parse test
  (`acl/bus_test.go`), the RPC sync with two test regions. Lab: the
  `multiregion` case pushes to r1 and checks a sync in r2 (only if the RPC
  sync is built).
- Effort: refusal and docs half a day; the RPC registry sync 2-3 days with
  tests and the lab step.

### B. CDC through leaf nodes (leaf nodes only)

- **Agent:** `server { bus { leafnodes { enabled = false } } }` and
  `ports { bus_leafnode = 4652 }`; the listener uses the client TLS of the
  bus (client certificate optional, the token authenticates).
- **ACL:** `leafnode = true` allows a leaf connection; `subscribe` is read
  from the leaf's side as above (what the leaf may receive), narrowed by the
  rights of the token like any connection (admin topics, section 1.4).
- **One way: a leaf only reads (the maintainer's intent, 2026-11-14).** A
  leaf must neither overload the bus nor overwrite anything on it, so only
  the least flow control works, enforced by the server whatever the policy
  says (the policy can only narrow it):
  - **Publish is an allow list the server sets, not the policy.** Live
    events need no publish at all: the leaf only propagates its interest.
    For catch-up after a disconnect the leaf gets exactly two subjects of
    its own consumer in each stream it may read (`NORCA_EVENTS_<region>`,
    `NORCA_KV_CHANGES_<region>`):
    `$JS.API.CONSUMER.MSG.NEXT.<stream>.leaf-<accessor>` and
    `$JS.ACK.<stream>.leaf-<accessor>.>`. Everything else is
    closed: event, admin, telemetry, push and `$KV` subjects, every other
    `$JS.API` subject (no stream or consumer create, update or delete), no
    `_INBOX` beyond its own.
  - **The consumer belongs to the server:** a durable pull consumer
    `leaf-<accessor>` created by the leader when the leaf is allowed, with
    the filter subjects the token may receive, `InactiveThreshold` for a
    leaf that is gone, and request limits (`MaxRequestBatch`,
    `MaxRequestMaxBytes`, `MaxRequestExpires`, `MaxWaiting`), so one pull
    can't ask for everything. Deleted when the token or the leaf right goes.
  - **Limits of the leaf account:** connections (one leaf), subscriptions
    (the interest of the clients behind the leaf counts), payload (control
    messages are small). A slow leaf is cut off as a slow consumer and
    catches up from its consumer; it never holds the bus back.
  - **Data flows one way:** the leaf account imports the allowed subjects
    from the servers' account and exports nothing, so the clients behind
    the leaf reach nothing on the bus directly.
  - Revocation as for clients: a changed token or policy closes the leaf.
- **Accounts:** the servers' account `NORCA`; every leaf bound to its own
  account `LEAF_<accessor>`, which imports the subjects of its rule from
  `NORCA` (stream exports), plus the JetStream API of the event stream as a
  service import if the rule names it. Revocation closes the leaf as for
  clients (Revalidate).
- **Isolation:** the leaf sees only the imported subjects; a KV value never
  crosses (the `$KV` subjects are not exportable); telemetry is not
  exported.
- **Deployer:** `network.ports.bus_leafnode`, opened in the firewall of the
  servers to `api_cidrs` (or a new `leafnode_cidrs`); the `nats` pack gets
  values `leafnode_url`, `leafnode_token_secret` to connect to the bus.
- **Failure modes:** a leaf reconnects by itself (NATS); a server restart
  of the bus (standalone to clustered) drops the leaves, they reconnect; a
  slow leaf is cut off like a slow consumer; a token revoked closes it.
- **Tests:** unit tests of the authenticator for `LEAF` (grant, no grant,
  revoked), an integration test with a second nats-server as leaf (events
  received, inbox of another connection not received, KV not reachable).
  Lab: the `packs` case configures the `nats` pack as a leaf and a client of
  that NATS reads the events.
- **Phases:** B1 accounts on the bus (move everything into `NORCA`, exports;
  no behaviour change); B2 leaf listener and auth; B3 pack and deployer; B4
  docs and lab. Effort: about 6-9 days, B1 the riskiest (every bus test and
  lab case that uses the bus).

### C. Gateways only

- **Agent:** `ports { bus_gateway = 4651 }` (already in section 2.1 of the
  bus design), `server { bus { gateways { enabled = false } } }`; gateways
  built from the Serf members of the other regions, as the routes are from
  those of the own region.
- **Names:** region in every stream name and subject (`NORCA_PACK_PUSHES_<region>`,
  `<prefix>.packs.pushed.<region>`, `consul-kv-<region>`), with the
  carry-over of the existing push stream and KV bucket; the push notice of a
  pipeline names the region or is published to all.
- **ACL:** no new field; the policy docs warn that region wildcards now
  cross regions. `leafnode` refused as in A.
- **Isolation:** each region's KV bucket stays in its own cluster (stream
  placement by cluster); KV values are sealed with the replicated keyring,
  so they would open in another region, but nothing reads them there.
  Telemetry stays per region by subject.
- **Deployer:** `network.ports.bus_gateway`, opened between the servers of
  all regions (the WAN Serf/RPC peers).
- **Failure modes:** the meta group quorum spans regions (section 2.1): a
  region cut off cannot create streams or consumers, so a leader elected
  during the partition cannot set up its event publisher, push consumer or
  telemetry stream. This would need the leader loops to keep working on
  existing streams without a create, and an answer for a region that starts
  for the first time while cut off.
- **Tests:** two-region test servers with gateways; the partition case
  (gateway down) with a leader change in the small side. Lab: the
  `multiregion` case (r1, r2, r3) checks cross-region events, a push in r1
  synced in r3, and a gateway cut with a leader change.
- **Phases:** C1 names with the region and carry-over; C2 gateways from
  Serf and TLS; C3 cross-region registry syncs on the bus; C4 partition
  behaviour, docs, lab. Effort: about 8-12 days, C4 open-ended.

### D. Both

B and C together, B first (accounts are needed before the gateways carry
leaf interest across regions). Effort: the sum, about 14-20 days, and the
partition question of C stays.

## 4. Comparison

| | A | B | C | D |
| --- | --- | --- | --- | --- |
| `leafnode` field means something | refused | yes | refused | yes |
| Regions stay independent failure domains | yes | yes | no | no |
| Other regions' events | RPC (exists) | RPC, or leaf + domains later | bus | bus |
| Accounts on the bus | no | yes | no | yes |
| New ports | none | 4652 | 4651 | both |
| Rough effort | 0.5 d (+2-3 d RPC sync) | 6-9 d | 8-12 d | 14-20 d |

## 5. Decisions for the Maintainer

1. **Which option now?** Recommendation: **A**. The field is refused when
   true, the docs say so, and no NATS linking is built. Nothing in Norca
   needs leaf nodes or gateways today; applications already read the events
   as clients with a token.
2. **Refuse or remove the field?** Recommendation: refuse `leafnode = true`
   in `validate`, accept `false`, keep the struct field (an unknown key
   would be ignored silently by the HCL decoder, which is worse than a
   refusal).
3. **Cross-region registry syncs:** build them over RPC (`Event.Stream`
   forwarded to the other regions) instead of gateways? Recommendation:
   yes, as a separate small item when wanted; not part of this change.
4. **Gateways at all?** Recommendation: **no**, not as designed in phase 5:
   a supercluster ties the JetStream of all regions to one meta group,
   against the region independence the rest of Norca keeps. If regions
   must share bus traffic later, use leaf connections with one JetStream
   domain per region (option B, use 3) instead. Section 2.1, 9 and 10 of the
   bus design are updated to say so.
5. **Leaf nodes later?** Recommendation: only on a concrete need (the
   `nats` pack as a leaf, or an edge site), and then with an account per
   leaf (reversing decision 3 of the bus design), never with `_INBOX.>` on
   the leaf.
6. **Port 4651 in the docs and the deployer?** Recommendation: drop
   `bus_gateway` from the design's port table until gateways are decided;
   it is in no code and no plan.
7. **One-way leaves (section B):** decided by the maintainer: a leaf reads
   only; publish is the server's allow list (none for live events, two
   subjects of a server-owned consumer for catch-up).
8. **Job filter header (section 6, G1)?** Decided and built (2026-11-14,
   `fix/bus-admin-events`): the header `Norca-Job` on the events of the
   topics Job, Evaluation, Allocation, Deployment and Service; no job token
   in the subject.
9. **Service check status as events (section 6, G3)?** Closed: they are
   events already. The clients report the state of every check of a native
   service instance to the servers (health-based catalog); `Ready`,
   `Health`, `Checks` and `HealthUpdatedAt` are written to Raft, at most
   every 5 seconds per instance (`norca/service_registration_health.go`), and
   every recorded change is a `Service` / `ServiceRegistration` event. No new
   topic. On the bus the outputs of the checks are left out (built
   2026-11-14, `fix/bus-redact-check-output`).
10. **Feature name:** decided by the maintainer: **Change Data Capture
    (CDC)**; leaf node is a connection mode of it (section 0).
11. **Consul KV notices stored?** Decided and built: yes, in
    `NORCA_KV_CHANGES_<region>` (an eighth of `bus.events.max_bytes`).
12. **The `leafnode` policy field.** Decided by the maintainer
    (2026-11-14): **a `cdc` rule of its own** replaces it: what may be
    received and by which connection modes (client, workload, leaf). Its
    shape is designed with option B; until then option A (`leafnode = true`
    refused).
13. **Consumers of the CDC streams, step 1:** decided and built (2026-11-14,
    `fix/bus-admin-events`): the limits policy set explicitly, at most
    `bus.events.max_consumers` (64) consumers per stream, consumer limits
    (inactive threshold at most `max_age`, 1000 pending acknowledgements).
14. **Consumers bound to their reader, step 2 (section 7):** decided to plan;
    built with option B.
15. **A reader with several filters:** decided by the maintainer: the server
    creates one consumer with all the filters after checking them against
    the reader's rights (section 7.1); one filter may still be created
    through the JetStream API under the reader's name.
16. **Encryption of the JetStream store at rest:** decided by the maintainer
    (2026-11-14): **optional**, off by default. It protects the stream files
    on the servers' disks (events with their full Raft history of the
    retention, admin events, the audit of the secret store, the KV keys);
    the KV values stay sealed with the keyring in any case, and a reader with
    rights still gets clear text. Open when built: where the key comes from
    (derived from the keyring, or wrapped by the keyring's provider such as a
    KMS - never a plain key in the agent configuration next to the store),
    and its rotation (nats-server re-encrypts on a restart with a new key).

## 6. CDC: Following the Lifecycle of a Job's Allocations

A controller outside Norca registers a job and must learn in real time,
whatever the outcome, when its allocations are placed, wait for capacity,
start (which may take long), run and are ready, fail, stop, are lost or
rescheduled, and act on each (report an error, report that the service can
be used, give up after its own timeout). The bus carries this today as the
events of the namespace; this section names the signals, the gaps, and how
a one-way leaf reads them.

### 6.1 The signals

| Step | Topic / type | What to read |
| --- | --- | --- |
| Registered | `Job` / `JobRegistered` | job ID, version, `Status` (redacted payload, section 1.4) |
| Scheduled or not | `Evaluation` / `EvaluationUpdated` | `Status` (`complete`, `blocked`, `failed`), `FailedTGAllocs` per group with the reasons (`NodesExhausted`, `DimensionExhausted`, `ConstraintFiltered`, `ClassExhausted`), `BlockedEval`, `QueuedAllocations`: waiting for capacity or not placeable |
| Placed | `Allocation` / `PlanResult` | new allocation: `NodeID`, `NodeName`, `DesiredStatus` `run`, `ClientStatus` `pending`, `AllocatedResources` with the ports and their `HostIP` (the address, without node read) |
| Starting | `Allocation` / `AllocationUpdated` | `TaskStates[task].Events`: `Received`, `Task Setup`, `Downloading Artifacts`, `Driver` (e.g. an image pull, the long part), `Started`, `Driver Failure`, `Restarting` |
| Running | `Allocation` / `AllocationUpdated` | `ClientStatus` `running`, task `State` `running` |
| Ready | one of: `Service` / `ServiceRegistration` (address and port of a native service); `Deployment` / `DeploymentAllocHealth` and the allocation's `DeploymentStatus.Healthy` (a job with an update block and health checks); or `Started` alone | the controller decides what ready means |
| Stopped | `Allocation` / `AllocationUpdateDesiredStatus`, `AllocationUpdated` | `DesiredStatus` `stop` or `evict`, then `ClientStatus` `complete` or `failed`; `Service` / `ServiceDeregistration` |
| Lost or rescheduled | `Allocation` / `AllocationUpdated`, `PlanResult` | `ClientStatus` `lost`; the replacement's `PreviousAllocation`, the old one's `NextAllocation`, `RescheduleTracker` |
| Job gone | `Job` / `JobDeregistered` | |

All of these are events of the namespace in `NORCA_EVENTS_<region>`, read
with read-job rights; the controller needs no admin topic (the address is in
the allocation's ports). Latency: the leader publishes an index's events
after the Raft apply, one JetStream round trip; a live subscriber has them
within milliseconds, a pull consumer after its next pull.

### 6.2 Gaps

- **G1 Filtering by job.** The subject names the namespace and topic, not
  the job: a controller of many jobs gets every allocation of the
  namespace and filters on the payload (`JobID`). Built: the header
  `Norca-Job` with the job of the event (its filter key), so a client
  filters without decoding the JSON. A job token in the subject would change
  the stream subjects (`<prefix>.<region>.*.*`) and collide with the Consul
  KV notices; not proposed.
- **G2 Catch-up.** A live subscription loses what happens while it is
  disconnected. A durable consumer replays from where it stopped; for a
  one-way leaf that is the server-owned consumer of section B. On every
  (re)start the controller reconciles once with the API (the job's
  allocations, evaluations), then follows the events.
- **G3 Service check results (corrected 2026-11-14).** The first draft
  said they are not events; they are, for native services: the
  health-based catalog writes `Ready`, `Health` and the status of each
  check of an instance to Raft (at most every 5 seconds per instance), and
  each recorded change is a `Service` / `ServiceRegistration` event. Ready
  for a controller is `Ready: true`. Liveness checks are not in `Health`
  (they restart the task: `Restarting` in the allocation's task events);
  services of the Consul provider have their health in Consul. The outputs
  of the checks are left out on the bus (decision 9).
- **G4 Timeouts are the controller's.** A pending allocation may wait for
  capacity (a blocked evaluation) or for a long download (`Driver` events);
  the events say which state it is in, not how long it will take.
- **G5 Volume.** A namespace with many allocations produces many
  `AllocationUpdated` events (each task state change); a controller narrows
  by subject to the topics it needs and by `Norca-Job` (G1) to its jobs.

### 6.3 Through a one-way leaf

The leaf's token may subscribe to the namespace's `Job`, `Evaluation`,
`Allocation`, `Deployment` and `Service` subjects; its server-owned consumer
filters on the same subjects. The controller behind the leaf follows the
live events, pulls from the consumer after a reconnect, and acknowledges
what it processed; it never publishes data to the bus. Registering the job
and any other change go through the HTTP API, not the bus.

## 7. Consumers Bound to Their Reader (CDC step 2)

**Why.** The NATS permissions name a consumer as one whole subject token.
A rule that lets a reader manage "its" consumers with a wildcard in the
consumer's place (`$JS.API.CONSUMER.DELETE.NORCA_EVENTS_eu.*`) lets it read,
change and delete the consumers of every other reader of the stream, and
create as many as the stream allows. Step 1 bounds the number per stream;
it does not keep readers apart. A rule that names the consumer keeps them
apart, but only if every policy writer does so.

**What.** The server, not the policy, decides which consumers a reader may
use: **one durable consumer per reader and stream**, named after the
reader, with a filter inside what the reader may receive.

- **Name.** `<accessor ID>` for a token; `<allocation ID>-<task>` for a
  workload identity (the characters a consumer name can't hold replaced);
  `leaf-<accessor ID>` for a leaf (section B). Names of other readers are
  never reachable.
- **Grant rewrite.** When a token's `bus` rule grants any consumer subject
  of a CDC stream, the authorizer replaces all of them by the reader's own:
  - `$JS.API.CONSUMER.CREATE.<stream>.<name>.<filter>` for each subject the
    reader may subscribe to inside the stream (its `subscribe` patterns
    within `<prefix>.<region>.*.*`, `<prefix>.<region>.*.ConsulKV.*`, and
    the admin subjects only with a management token), so a consumer's filter
    is never wider than the reader's live rights; no create without a
    filter, none with several filters;
  - `INFO`, `DELETE` and `MSG.NEXT` of `<stream>.<name>`, and
    `$JS.ACK.<stream>.<name>.>`.
  Other `$JS.API.CONSUMER` subjects of the CDC streams in the rule are left
  out, with a warning as for the inbox subjects.
- **Several filters: the server creates the consumer (decided by the
  maintainer, 2026-11-14; section 7.1).** The JetStream API carries the
  filters of a multi-filter consumer in the request body, not in the
  subject, so the NATS permissions can't check them. A reader that needs
  several topics asks Norca for its consumer; Norca checks every filter
  against the reader's rights and creates one consumer with all of them.
  A reader with one filter may also create its consumer itself through the
  JetStream API (the grant rewrite above). Both give the same name: one
  consumer per reader and stream.
- **Count.** The consumers of a stream are bounded by the readers allowed
  to it; `max_consumers` of step 1 stays as the outer bound.
- **Revocation.** When the token or the right goes, the leader deletes the
  reader's consumers (as for the leaf consumer), so no orphaned consumer
  keeps its place until the inactive threshold.
- **Management tokens** keep every right (operators, debugging).

### 7.1 Consumers created by the server

**Why this way.** Of the options for a reader with several topics (a wider
filter and filtering on headers; one numbered consumer per topic; a stream
per reader; the server creating one consumer with several filters), only the
last gives exact filters and the stream's order across topics in one
consumer: the lifecycle of a job (section 6) needs the evaluation before
the allocation that it placed. A numbered consumer per topic loses that
order; a wider filter carries events the reader may not want, and only when
its rule allows the wider pattern. It is also the model of the leaf's
consumer (section B): the server owns every CDC consumer.

**API.** HTTP, forwarded to the leader of the region:

| Method | Path | Does |
| --- | --- | --- |
| `PUT` | `/v1/cdc/consumer/:stream` | create or update the reader's consumer of the stream |
| `GET` | `/v1/cdc/consumer/:stream` | the consumer's settings and state (pending, last delivered index) |
| `DELETE` | `/v1/cdc/consumer/:stream` | delete it |

`:stream` is `events`, `admin` or `kv` (`NORCA_EVENTS_`, `NORCA_ADMIN_EVENTS_`,
`NORCA_KV_CHANGES_<region>`). The body of a `PUT`:

```json
{
  "Filters": ["norca.eu.games.Evaluation", "norca.eu.games.Allocation"],
  "Deliver": "all",
  "StartTime": "",
  "AckWait": "30s",
  "MaxAckPending": 500
}
```

- `Deliver`: `all`, `new`, `last-per-subject` or `by-start-time` (with
  `StartTime`). Not a start index: a reader that keeps its `Nomad-Index`
  resumes the same consumer and skips what it has.
- `AckWait` and `MaxAckPending` within the stream's consumer limits (step 1).

The request is authenticated like any API request (an ACL token or a
workload identity); the reader is the one of the request, never named in the
body. CLI: `norca cdc consumer apply|read|delete -stream events -filter ...`;
Go: `api.CDC().UpsertConsumer(...)`.

**Checks, all before the consumer is touched:**

- the stream is a CDC stream of the region; `admin` only with a management
  token;
- every filter lies inside what the reader may receive live: a subset of
  its `bus` subscribe patterns, minus the admin topics it lacks the right
  for (the deny lists of the authorizer), inside the stream's subjects;
- the filters do not overlap each other (JetStream refuses overlapping
  filter subjects) and are at most a fixed number (16);
- the stream has room (`max_consumers`), unless the reader's consumer
  exists already.

Answers: `403` a filter outside the reader's rights or the admin stream
without management, `400` an unknown stream, a bad filter or overlapping
filters, `409` no room left on the stream.

**The consumer:** a durable pull consumer named after the reader (as
above), `FilterSubjects` the filters, explicit acknowledgement, the
inactive threshold and pending limit of the stream, request limits
(`MaxRequestBatch`, `MaxRequestMaxBytes`, `MaxRequestExpires`), and metadata
`norca.reader=<kind>:<name>` and `norca.token=<accessor>` for the cleanup. A
`PUT` on an existing consumer changes its filters and keeps its position,
so a reader adds a topic without reading the stream again.

**The reader's grant:** `INFO`, `MSG.NEXT` of `<stream>.<name>` and
`$JS.ACK.<stream>.<name>.>`; the single-filter `CREATE` of the grant rewrite
only if its rule names consumer subjects; no `DELETE` through the JetStream
API (the API's `DELETE` does it, with the same checks).

**Cleanup.** The leader's revalidation (on changes of tokens, policies and
allocations) deletes the consumers whose reader lost the right: a deleted or
expired token, a policy without the stream's subjects, an allocation that
ended. The inactive threshold removes the rest.

**Leaf.** The leaf's consumer (section B) is made by the same function, with
the leaf's filters, when the leaf connects; the leaf gets only `MSG.NEXT` and
the acknowledgements.

**Tests.** Authorizer: a rule with `CONSUMER.*.<stream>.*` yields only the
own names; another reader's consumer can't be read or deleted. Endpoint: a
filter outside the rights, an admin filter without management, overlapping
filters and a full stream are refused with their codes; a `PUT` with two
filters delivers both topics in the stream's order; a second `PUT` with a
third filter keeps the position; revoking the token deletes the consumer.
Integration: two readers on one stream, each with its consumer, neither
reaching the other's. Lab: the `packs` case bus reader asks for its
consumer through the API (Evaluation and Allocation of its namespace) and
fails on another reader's.

**Docs.** The CDC page: one consumer per reader and stream, made by the
server (`/v1/cdc/consumer`), or for one filter through the JetStream API
with the reader's own name; the API page of the endpoint; the CLI page.
