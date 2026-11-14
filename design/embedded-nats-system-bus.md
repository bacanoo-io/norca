<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Embedded NATS System Bus

## 1. Summary and Decisions

Norca servers embed a NATS server (`github.com/nats-io/nats-server/v2`, Apache-2.0)
with JetStream. The servers of a region form one NATS cluster; federated regions
are joined by NATS gateways. This **system bus** carries Norca's own traffic:

| Use | Today | With the bus |
| :--- | :--- | :--- |
| Event stream to NATS | `norca-natsbridge` job (SSE → external NATS, Variables checkpoint, leader election) | The leader publishes the events itself into a JetStream stream of the bus |
| Pack registry syncs from NATS (push notices, other regions) | `norca-natsbridge -sync-pushes/-sync-regions` | The leader consumes them itself and syncs the registries directly |
| Consul KV of the compat API | Backend `variables` (default) or `nats` (external NATS, explicit URL) | Without an external Consul: a KV bucket of the bus |
| Task group usage (autoscaler, 30-day metrics store) | Piggybacked on `Node.UpdateAlloc` | Clients publish to the bus; every server stores it (see the metrics store design); RPC stays as the fallback |

Decisions:

1. **The bus is internal.** It is a system component of the servers with its own
   accounts, not a NATS for applications. Applications keep using the `nats`
   pack; that NATS may connect to the bus as a leaf node to receive the events.
2. **`norca-natsbridge` is removed.** The agent takes over every function of it.
   Norca has no released version yet, so there is no deprecation period (decided
   2026-10-30).
3. **Consul KV:** when no external Consul is configured, the compat API stores
   the KV in a bucket of the bus.
4. **Authentication through Norca state, not NATS JWTs.** The embedded server uses
   a custom authenticator that runs in the Norca process: nodes log in with
   their node secret, people and services with an ACL token. No operator,
   account or user JWTs to issue, rotate or revoke.

## 2. Topology

```
                         region eu (3 servers)                              region us
   +---------------------------------------------------------------+       +-----------+
   |  norca server A          norca server B          norca server C |       |  ...      |
   |  +-------------+         +-------------+         +-------------+|  gw   |           |
   |  | Raft  (4647)|         | Raft        |         | Raft        ||<----->|           |
   |  | NATS  (4649)|<-route->| NATS        |<-route->| NATS        ||  4651 |           |
   |  | JetStream   |  4650   | JetStream   |         | JetStream   ||       +-----------+
   |  +------^------+         +------^------+         +------^------+|
   +---------|-----------------------|-----------------------|-------+
             | client connections (TLS, node secret)          |
       +-----+-----+           +-----+-----+           +-----+-----+
       | client 1  |           | client 2  |           | client N  |
       +-----------+           +-----------+           +-----------+
```

- **Cluster:** every server of a region runs the embedded NATS server with
  JetStream. Routes are built from the Serf LAN members of the region (servers
  advertise the ports in the Serf tags `bus` and `bus_cluster`; routes go to
  the member's Serf address, like Raft); a server joining or leaving updates
  the routes. Route compression is `s2_auto` (the servers sit in different
  zones).
- **Standalone and clustered** (implementation finding): NATS runs a
  clustered JetStream only once a route to another server is up, and a
  standalone JetStream restarted clustered does not carry its streams over.
  So the bus starts clustered when the region has or expects more than one
  server (`bootstrap_expect > 1`, or more than one server in the Raft
  configuration); a clustered server started before its peers configures a
  route to itself, which NATS drops, and JetStream waits for a peer like Raft
  does. A single-server region runs a standalone JetStream. When a second
  server appears, the standalone bus restarts clustered and moves its store
  to `<data_dir>/server/bus.standalone`; phase 3 copies the Consul KV bucket
  from there, the other streams are transient and rebuilt.
- **Regions:** federated regions are joined by gateways built from the Serf WAN
  members, so the regions form a NATS supercluster. Subjects carry the region,
  so a region reads another region's events through the gateway.
- **JetStream** stores its data in `<data_dir>/server/bus/` (a temporary
  directory in dev mode without a data dir). The JetStream meta
  leader is independent of the Raft leader of Norca; streams use
  `replicas = min(3, servers)`.
- **In-process connections:** the server's own publishers and consumers connect
  through `nats.InProcessServer`, not over TCP.
- **Clients** connect to the bus addresses of the servers they already know (the
  heartbeat response lists them) and reconnect to another server on failure.
  A client uses the bus only when every server of the region runs a version
  with the bus and the bus is enabled (`ServersMeetMinimumNorcaVersion`, as for
  other features); otherwise it keeps the RPC paths.

### 2.1 Ports

| Port | Default | Use |
| :--- | :--- | :--- |
| `ports.bus` | `4649` | Client connections (nodes, external consumers, leaf nodes) |
| `ports.bus_cluster` | `4650` | Routes between the servers of a region |
| `ports.bus_gateway` | `4651` | Gateways between regions |

They differ from the NATS defaults (4222/6222/7222), so a `nats` pack on a
server node does not collide with the bus.

## 3. Security

- **TLS:** the bus listeners use the agent's TLS configuration (`tls { rpc = true }`,
  the same certificates and CA as RPC). The client listener does not require
  a client certificate (connections authenticate with node secrets or ACL
  tokens). Routes require the certificate of a server of the region
  (`server.<region>.norca`, verified against the CA) in both directions.
  Without RPC TLS the bus is plaintext as well, like RPC and Raft
  (decision: no separate `allow_plaintext` switch).
- **Authentication** (custom authenticator, `server.Options.CustomClientAuthentication`):

  | Who | Credential in CONNECT | Account | Permissions |
  | :--- | :--- | :--- | :--- |
  | Norca server (in process) | per-start random token, never leaves the process | `NORCA` | all subjects of `NORCA` |
  | Client node | node ID and node secret | `NORCA` | publish `norca.telemetry.<region>.<node_id>`; the JetStream API and `$KV` subjects of the Consul KV bucket; nothing else |
  | ACL token | token secret | `NORCA` | from the token's policies, new block `bus` (below) |
  | Leaf node | ACL token with `bus` `leafnode = true` | `NORCA` | the token's `bus.subscribe` / `bus.publish` lists (no separate account) |

  The token or node is resolved from the state store of the server the
  connection lands on, the same way as for RPC. Revoking a token or a node
  closes its connections: the server watches the ACL token and node tables and
  disconnects the affected clients.
- **ACL policy block `bus`:**

  ```hcl
  bus {
    subscribe = ["nomad.*.>"]          # event subjects
    publish   = ["nomad.packs.pushed"] # push notices of a CI pipeline
    leafnode  = false
  }
  ```

  `management` tokens may do everything. Subjects of the system account (`$SYS`)
  are never granted.
- **Telemetry spoofing:** a node may publish only on its own subject, so a
  report can no longer name another node; the server still checks that the
  node ID in the payload matches the subject.
- **Consul KV ACL:** as today with the NATS backend, the client checks the
  Variables ACL of the request's token before it touches the bucket; the node
  credential itself may read and write the whole bucket.

## 4. Uses

### 4.1 Event stream (replaces the bridge's outbound mode)

- The leader subscribes to the event broker in process (no SSE) and publishes
  every event to `<prefix>.<region>.<topic>` with the same payload and headers
  as `norca-natsbridge` (`Nats-Msg-Id`, `Nomad-Region`, `Nomad-Index`, ...), so
  existing consumers keep working. `<prefix>` defaults to `nomad`, like the
  bridge.
- Stream `NORCA_EVENTS_<region>` (as created by the bridge with
  `-jetstream-create`), deduplication window 2 minutes, bounded twice
  (discard old, whichever limit is reached first):
  - `max_age` from `bus.events.max_age` (default `72h`): a consumer that was
    down for a long weekend resumes where it stopped;
  - `max_bytes` from `bus.events.max_bytes` (default `1GiB`): the stream lives
    on the servers' disks, and busy regions produce mostly `Allocation` and
    `Evaluation` events; the limit must fit into `bus.max_storage`.
  Topic filters (`bus.events.topics`) reduce the volume. Long-term audit
  (`SecretAccess`, ACL events) is the job of an external consumer that
  archives the stream, not of the bus.
- **Resume position:** no Variables checkpoint. On becoming leader the server
  reads the last message of the stream and continues after its `Nomad-Index`;
  duplicates around the switch are removed by `Nats-Msg-Id`. If the broker no
  longer holds that index, it publishes the same gap marker as the bridge on
  `410 Gone`.
- Topics and filters: `bus.events.topics` (default all, like the bridge).

### 4.2 Pack registry syncs (replaces the bridge's inbound mode)

- Push notices: stream `NORCA_PACK_PUSHES` (`<prefix>.packs.pushed`,
  `<prefix>.packs.pushed.>`, 24h). The leader consumes it with the durable
  consumer `norca-sync-<region>` and syncs the matching registries directly
  (no API call), with the same cooldown as the bridge.
- Other regions: the leader reads the `CustomObject` events of the other
  regions' streams through the gateways and syncs a local registry when the
  same registry changed there.
- CI pipelines publish push notices with an ACL token that has
  `bus { publish = ["nomad.packs.pushed", "nomad.packs.pushed.>"] }`.
  `norca-natsbridge notify-push` becomes `norca pack registry notify-push`.

### 4.3 Task group usage (telemetry)

- Clients publish their usage report every `client_metrics_interval` to
  `norca.telemetry.<region>.<node_id>`.
- Stream `NORCA_TELEMETRY_<region>`, `max_age = 1h`, `max_msg_size = 64KiB`,
  discard old. Every server reads it with its own durable consumer
  `norca-usage-<server_id>`: the leader's autoscaler and the metrics store of
  every server get every report, also those sent during a leader change or a
  server restart.
- No RPC fallback (decided 2026-10-31): the piggyback on `Node.UpdateAlloc`
  predates the embedded bus and is removed. Without a bus on the servers the
  heartbeat asks for no usage (interval 0); a report larger than 64KiB is
  split into several of the same window.

### 4.4 Consul KV of the compat API

- Backend selection of `consul_compat.kv.backend`:
  - `auto` (new default): the bus when it is enabled, else `variables`;
  - `bus`: the KV bucket `consul-kv` of the bus (`replicas = min(3, servers)`,
    history 1); the client uses its bus connection;
  - `variables`, `nats` (an external NATS with its own URL and credentials),
    `disabled`: unchanged.
- Key encoding, CAS, blocking queries (KV watch) and ACL checks are those of the
  existing `nats` backend.
- With an external Consul configured on the agent the compat API is not used
  for the KV, so nothing changes there.
- **Backup:** the bucket is not in the Raft snapshot. The backup subsystem
  exports the bucket as an item `consul-kv` of the backup archive and restores
  it; selective restore can pick it.
- **Migration:** `norca operator consul-kv migrate -from variables -to bus`
  copies the keys (CAS on the target, the source left in place).

## 5. Configuration

```hcl
server {
  bus {
    enabled = true                  # default true on servers

    # Limits that keep the bus from starving the control plane.
    max_memory      = "256MiB"      # JetStream memory store
    max_storage     = "4GiB"        # JetStream file store (<data_dir>/server/bus)
    max_payload     = "1MiB"
    max_connections = 10000

    events {
      enabled        = true
      subject_prefix = "nomad"
      max_age        = "72h"
      max_bytes      = "1GiB"         # within max_storage; oldest events dropped first
      topics         = ["*"]
    }
  }
}

ports {
  bus         = 4649
  bus_cluster = 4650
  bus_gateway = 4651
}

client {
  bus {
    enabled = true                  # use the servers' bus when it is available
  }
}
```

## 6. Isolation from the Control Plane

The bus runs in the server process, so it competes with Raft and the scheduler
for CPU, memory and disk. Bounds:

- JetStream memory and storage limits (above); streams have explicit limits and
  discard old messages.
- Per-connection limits: max payload, max pending, slow consumers are
  disconnected (NATS default behaviour).
- Clients publish at most one usage report per interval; the server drops
  telemetry from a node that publishes faster.
- JetStream data lives apart from `raft/` and is never written to Raft.
- The bus can be disabled; every use has a fallback (RPC usage, Variables KV,
  no event publishing).

### 6.1 Stream and Connection Settings

From the NATS tuning guidance, adapted to a bus that shares the server
process (bounded rather than maximal):

| Item | Setting |
| :--- | :--- |
| Routes | `compression = s2_auto`, `pool_size` default (3) |
| Server | `max_pending` and `write_deadline` defaults (64MiB, 10s): a slow consumer is disconnected instead of buffering in the server |
| `NORCA_EVENTS_<region>` | file storage, S2 compression (JSON), `duplicates = 2m`, discard old, `max_age`/`max_bytes` from the config, `max_msg_size = max_payload`, replicas `min(3, servers)` |
| `NORCA_TELEMETRY_<region>` | memory storage (no disk I/O for transient data), replicas `min(3, servers)`, `max_age = 1h`, `max_msg_size = 64KiB`, discard old |
| `NORCA_PACK_PUSHES` | file storage, `max_age = 24h`, small `max_bytes` |
| Consul KV bucket | file storage, history 1, replicas `min(3, servers)`; `sync_interval` stays at the NATS default (2m) until phase 3 measures it |
| Server consumers | pull consumers, bounded `max_ack_pending`, batched fetches, explicit `ack_wait`; the telemetry consumers ack per batch |
| In-process connections | unlimited reconnects (they follow a bus restart), `NoEcho` where the server publishes and consumes the same subjects |

The high limits of dedicated NATS clusters (tens of GB of memory store,
100k connections, 8MB payloads) are not defaults here; they can be raised
with the `bus` block.

## 7. Operations

- `norca operator bus status`: cluster routes, gateways, JetStream meta leader,
  streams and consumers with their lag, connections per kind.
- UI: a "Bus" tab on the Servers page with the same data.
- Metrics (go-metrics): connections, messages and bytes in and out, stream
  sizes, consumer lag, dropped telemetry.
- Logs of the embedded server go through the agent logger (`bus` subsystem).

## 8. Removal of `norca-natsbridge` (done 2026-10-30)

No release had shipped, so the bridge was removed at once instead of being
deprecated:

- norca: the `norca/natsbridge` module, its build in the release workflow,
  `windows.bat`, the make targets, the test and compliance workflows, the
  third-party notice binary list and REUSE annotation; the gap marker topic
  is `Bus` (was `Bridge`).
- norca-deployer: `release.natsbridge` and `natsbridge_local_path`, the
  install step; the examples use an `events-reader` policy with a `bus` rule;
  `network.ports.bus` / `bus_cluster` are always written and opened in the
  firewall of the servers.
- norca-registry: the `nats` pack 0.6.0 without the bridge job and
  `bridge_*` values; the release workflow sends the push notice to
  `POST /v1/pack-registries/notify-push`.
- norca-lab and norca-test check the events on the bus
  (`/v1/operator/bus` with the stream list); `noconsul` keeps its NATS KV
  until phase 3.

## 9. Implementation Phases

1. **Bus core:** embedded server, routes from Serf, TLS, authenticator, the
   `bus` ACL block, limits, `operator bus status`, Windows build.
2. **Events and registry syncs** in the agent; bridge removed.
3. **Consul KV on the bus** (`auto`/`bus` backends, backup item, migration
   command).
4. **Telemetry over the bus** together with the metrics store
   (`internal-metrics-storage-tstorage.md`).
5. **Gateways** between federated regions.
6. (Done in phase 2: the bridge is removed.)

### 9.1 Phase 1 Status (2026-10-30)

Implemented in the norca repository:

- `norca/norca/bus`: the embedded server (standalone or clustered, self route
  placeholder, restart clustered on the first route), custom authentication
  with revalidation (`DisconnectClientByID`), status, `s2_auto` routes,
  in-process connections that follow a restart.
- `norca/norca/bus_server.go`: start after Serf, routes from the Serf tags
  `bus` and `bus_cluster` (member events plus a 30s check), TLS from the
  agent configuration (routes verify `server.<region>.norca` against the CA),
  node secret and ACL token authorization, revalidation on ACL and node
  table changes (5s) and every minute.
- ACL `bus { subscribe, publish, leafnode }`; RPC `Operator.BusStatus`
  (operator read, `ServerID` forwarding); `GET /v1/operator/bus`;
  `api.Operator().BusStatus`; `norca operator bus status [-server] [-json]`.
- Agent: `server { bus { enabled, max_memory, max_storage, max_payload,
  max_connections } }`, `ports { bus, bus_cluster }`; enabled by default;
  test agents and test servers run without it unless a test enables it.

Deferred: the leaf node listener (with the events, phase 2), the client
`bus` block (phase 3/4, when nodes connect), the gateway port (phase 5), and
the `events` sub-block (phase 2).

### 9.2 Phase 2 Status (2026-10-30)

Implemented in the norca repository:

- `norca/norca/bus/events.go`: subjects, `Nats-Msg-Id`, headers, the event
  JSON with `Region` and the gap marker exactly as the bridge;
  `LastEventIndex`, `EnsureStream`, `PushNotice`, `NormalizeSource`.
- `norca/norca/bus_events.go`: on the leader (`busEventsLeader` from
  `establishLeadership`), waiting for JetStream, two loops with backoff:
  - events: `NORCA_EVENTS_<region>` (file, S2, duplicates 2m, `max_age`,
    `max_bytes`, `max_msg_size = max_payload`, replicas `min(3, servers)`
    re-checked every minute); resume after the newest `Nomad-Index` with an
    exact broker subscription; `ErrIndexNotAvailable` gives a gap marker and
    a restart from the newest events; batches published asynchronously and
    acknowledged; events encoded with `JsonHandleWithExtensions` like the
    event stream API;
  - push notices: `NORCA_PACK_PUSHES` (24h, 64MiB), durable consumer
    `norca-sync-<region>`, registries matched by name or normalized source,
    1 minute cooldown, synced with `syncPackRegistryObject`.
- `Packs.NotifyPush` RPC and `POST /v1/pack-registries/notify-push`
  (registry write): published on the bus, or synced directly without it.
- Agent `bus { events { enabled, subject_prefix, max_age, max_bytes, topics } }`;
  without `max_bytes` the stream takes at most a quarter of `max_storage`.
- `norca-natsbridge` removed (section 8).

Moved to phase 5: registry syncs on other regions' events (needs the
gateways), and the leaf node listener. Not done yet: the lab and norca-test
checks on the bus instead of the bridge, the `nats` pack's bridge values,
the deployer docs.

### 9.3 Phase 3 Status (2026-10-31)

Done: the `auto`/`bus` backends through the `ConsulKV` RPC of the servers
(nodes have no access to the bucket), the bucket keys
`<namespace>.<path>.<key>` (each namespace and Variables path apart), the
backup item and full restore, `norca operator consul-kv migrate`, and the
carry-over of the standalone store into the clustered bucket
(`consul-kv-carried-over` marker). Lab: `noconsul`.

### 9.4 Phase 4a Status (2026-10-31)

Done: clients publish the task group usage on
`norca.telemetry.<region>.<node_id>` (JSON, split at 64KiB); the leader keeps
`NORCA_TELEMETRY_<region>` (memory, 1h, a quarter of `max_memory`, replicas
`min(3, servers)`); every server reads it with `norca-usage-<server_id>`
(AckNone, deliver all, inactive after 2h) and hands fresh reports to the
autoscaler (only the leader's collector keeps them). The `Node.UpdateAlloc`
piggyback is removed. Next (4b): the metrics store fed by the same consumer.

## 10. Decided Questions (2026-10-30)

1. The bus is **enabled by default** on servers and clients.
2. Event stream retention: `max_age = 72h`, `max_bytes = 1GiB`.
3. Leaf nodes are limited by the **permission list of their ACL token**
   (`bus.subscribe` / `bus.publish`); no dedicated import-only account.
4. Stream replicas: `min(3, servers)` for every stream and the KV bucket; a
   region growing from 1 to 3 servers gets its streams scaled up by the
   leader (stream update), shrinking scales them down before a server is
   removed (`server force-leave` warns when that would lose data).

## 11. Review of 2026-11-14 and its fixes

A review of the bus as built (sections 2 to 4) found the items below. One was
confirmed with a probe, the rest read from the code. The way of each fix was
decided by the maintainer on 2026-11-14.

### 11.1 Findings

| # | Finding | Where | How found |
| --- | --- | --- | --- |
| S1 | **Any connection can read the replies of every other one.** Every grant had `_INBOX.>` in its subscribe list and all connections share one NATS account. A node, which may only publish its telemetry, received the values of the Consul KV the servers read, and every JetStream reply | `bus/auth.go` `permissions()` | probe: 6 messages, 2 with the value |
| S2 | A usage report may name any namespace, job and group: the server checks only the node ID. A node can move the autoscaler of other jobs and fill `max_series` with made-up groups | `bus_telemetry.go` `handleTelemetry` | code |
| S3 | The rate limit of section 6 ("the server drops telemetry from a node that publishes faster") was not built: one node can push the reports of the others out of the stream | `ensureTelemetryStream` | code |
| S4 | The internal token is compared in variable time | `bus/auth.go` | code |
| L1 | Events of one Raft index can be lost: after a failed batch the publisher resumes after the index of the newest message, so the events of that index not yet stored are skipped | `bus_events.go` `publishBusEvents` | code |
| L2 | With two servers the streams have two replicas and need both servers | `busReplicas` | code |
| L3 | A push notice that comes within the cooldown of its registry is acknowledged and dropped | `pushSyncer.handle` | code |
| L4 | The backoff of the bus loops never goes back: after five failures a loop waits 30 seconds for the rest of its life | `busRetryLoop` | code |
| L5 | A region named `telemetry` or `packs` (or a subject prefix under `norca.telemetry`) collides with the subjects of the other streams | subjects | code |
| L6 | Unchecked type assertion on the status of the KV bucket | `ensureConsulKVBucket` | code |
| C1 | The bus connection of a client keeps the server list of its first connect for ever | `client/bus.go` | code |
| D1 | The Consul KV bucket has no size limit and the storage budget is checked for two of the four streams only | `bus/config.go` | code |
| D2 | The Consul KV of the bus is not encrypted at rest; Variables are | `consul_kv_endpoint.go` | code |
| N1 | Gateways (phase 5) and leaf nodes are not built; the `leafnode` field of the `bus` policy block does nothing | `bus/server.go`, `bus/auth.go` | code |

### 11.2 Decisions

| # | Decision |
| --- | --- |
| S1 | A node gets **no subscribe right at all**. An ACL token may subscribe to its own replies only, `_INBOX.<accessor id>.>`: an external consumer sets that inbox prefix in its NATS client (nats.go `CustomInboxPrefix`, `nats --inbox-prefix`). The in-process connections of the servers keep every subject. No separate NATS accounts |
| S2 | The groups of a report that have no allocation on the reporting node (running or ended, as the state has them) are **dropped and counted**; the connection stays |
| S3 | The telemetry stream keeps a bounded number of messages per subject (per node), so a node that publishes too fast pushes out its own reports only |
| S4 | Constant-time comparison |
| L1 | The publisher resumes at the index of the newest message, that index included, and skips the events of it that are already in the stream (by `Nats-Msg-Id`) |
| L2 | **Stays at two**, documented: with two servers Raft needs both as well, so the bus is not less available than the control plane, and the data is on both disks |
| L3 | The notice is delivered again when the cooldown is over |
| L4 | The backoff starts again after a run that lasted |
| L5 | The two region names are **refused at start** when the bus is on; the subject prefix is checked against the telemetry and push subjects. The subjects stay as they are |
| L6 | Checked |
| C1 | When the client is not connected and a heartbeat brings another server list, the connection is opened again with the new list |
| D1 | `bus { consul_kv_max_bytes = "256MiB" }`; a write beyond it fails, nothing is dropped. The configuration checks that the events, the push notices, the telemetry and the KV fit into `max_storage`. The deployer plan gets the item |
| D2 | **Encrypted with the keyring**, as Variables are. Keys that exist are encrypted at their next write; reads understand both forms |
| N1 | Not in this round |

### 11.3 Order

Two steps. Step 1: S1 to S4, L1 to L6, C1 and D1 on one branch, with tests
and the lab cases that use the bus. Step 2: D2 on its own branch: the
envelope of a value, the endpoint, export, import and migration, the key in
use by the bucket (a root key that encrypts a KV value must not be removed)
and the rekey of a full rotation.

### 11.4 D2 as built

- **Envelope.** A stored value is a 19-byte marker, the 36-byte ID of the
  root key, the nonce and the AES-256-GCM ciphertext. The additional data is
  the key ID and the NATS key of the value, so a sealed value moved under
  another key in the store files does not open. A value without the marker
  is a value of before and is returned as it is; one with the marker that
  does not open is an error. A clear value that began with the marker would
  be refused, not misread.
- **Where.** The ConsulKV endpoint seals on Put and opens on Get; the
  clients and `helper/consulkv` see stored bytes only.
- **Backups.** The export keeps the stored (sealed) values, next to the
  keyring, like the Variables. Contents and diff open them with the keyring
  of the backup, then the one of the server, so the checksum is the one of
  the value. Single restore, full restore import and the migration from
  Variables seal with the active key; an import writes nothing when a value
  does not open. The carry-over from the standalone store copies stored
  bytes under the same NATS keys.
- **Key in use.** The state does not know of the bucket. The root key GC
  reads the key IDs of the bucket once per run, before the first key it
  would remove, and removes none when the bucket cannot be read.
  `Keyring.Delete` by hand does not look, as it does not for Variables.
- **Rekey.** The core job of a full rotation seals the values of the
  rekeyed keys again, with a check on the revision (a value written
  meanwhile is left). The revision of a rekeyed value changes.
- **Not done.** Values of before are not sealed by a sweep: only by their
  next write (decided). The events, telemetry and push notice streams are
  not encrypted at rest.
