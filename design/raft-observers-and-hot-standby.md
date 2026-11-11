<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Raft observers and hot standby servers

**Status:** Draft · 2026-11-11 · all questions decided · phase 1 built (norca PR #14, docs PR #23) · phase 2 built (branches feat/raft-standby, deployer feat/server-role, lab case-raft-standby, docs raft-standby; lab run pending) · clean room development (see [Clean room](#1-clean-room))
· inputs: two Hungarian drafts of the user ("Observer Tier alapok és Hot-Standby failover",
"Observer Tier rendszerbővítések"), not derived from Enterprise code (user, 2026-11-11), and the
review of 2026-11-11. Related: [raft-topology-and-cross-region.md](raft-topology-and-cross-region.md),
[backup-architecture.md](backup-architecture.md), [opentelemetry-plan.md](opentelemetry-plan.md),
[metric-storage-update.md](metric-storage-update.md).

## Summary

| Topic | Decision proposed here |
| --- | --- |
| Roles | `server { role = "voter" \| "standby" \| "observer" }`; replaces `non_voting_server` |
| Who decides suffrage | **autopilot only**, through a Norca `Promoter`; no code path calls `AddVoter`/`DemoteVoter` around it |
| Hot standby | a standby is promoted only when a voter is failed long enough and the voters are below the target; the most caught-up healthy standby wins, its datacenter preferred |
| Manual failover | `norca operator raft promote-standby -id <standby> [-replace <failed voter>]`: checks, removes the failed voter, and lets autopilot promote |
| Reads | observers and standbys serve `?stale` reads; `?consistent` reads always go to the leader |
| Backups | scheduled and manual backups may run on an observer (`backup { prefer_observer = true }`), the leader is the fallback |
| Heartbeats | relay and coalescing on observers is a later phase, only after measurements |
| Job admission | validation, admission policies and a dry-run capacity check before forwarding, on the receiving server (observer or not); API rate limits per token |
| History | **no database**: the event stream of the system bus with a longer retention on observers (90 days / 15 GiB), and the built-in metrics store; nothing new to install |

## Table of contents

1. [Clean room](#1-clean-room)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [Starting point](#3-starting-point)
4. [Roles and configuration](#4-roles-and-configuration)
5. [The Norca promoter](#5-the-norca-promoter)
6. [Manual failover](#6-manual-failover)
7. [Reads, scheduling and RPC](#7-reads-scheduling-and-rpc)
8. [System extensions on observers](#8-system-extensions-on-observers)
9. [Observability](#9-observability)
10. [Failure scenarios](#10-failure-scenarios)
11. [Testing](#11-testing)
12. [Documentation](#12-documentation)
13. [Phases](#13-phases)
14. [Decisions](#14-decisions)

## 1. Clean room

Nomad Enterprise has non-voting servers, redundancy zones and an enterprise autopilot; HashiCorp never
published their code. The MPL tree of Nomad v1.6.5 only has the public side: the `non_voting_server`
agent option, the `nonvoter` Serf tag, the bootstrap that counts voters only, and the `!ent` stubs of
`norca/autopilot_oss.go` (`autopilotPromoter`, `autopilotServerExt`, `autopilotConfigExt`), which
return the default promoter and nothing.

Rules for this work (as for multiregion, see `CLEAN_ROOM.md`):

- **Allowed sources:** the Norca tree (MPL v1.6.5 base and Norca's own code), `hashicorp/raft` (MPL,
  the `bacanoo-io/raft` fork), `hashicorp/raft-autopilot` v0.1.6 (MPL: the `Promoter` interface and
  `StablePromoter`), this document and the user's drafts.
- **Not used:** Enterprise code or binaries, the enterprise autopilot module, HashiCorp sources or
  documentation of BSL releases (after 2023-08-10), descriptions of the Enterprise behaviour. The
  behaviour is designed from Norca's requirements; public option names (`non_voting_server`) may be
  mentioned.
- **Files:** new files carry the Norca header only. Where a `!ent` stub is replaced and its signature
  and doc comment are kept (`autopilotPromoter` and the two ext functions), the HashiCorp copyright
  line stays, as in `deploymentwatcher/multiregion.go`.
- **Commits** say "clean room" in the message, so the `bugfix` set of the audit picks them up.
- **Audit:** after the build the auditor runs `clean-room-audit/audit.ps1`. The Go files are in no
  set (HashiCorp's implementation is not public); `CLEAN_ROOM.md` and the clean room pages of the
  docs get a section like "Multiregion deployments".

## 2. Goals and non-goals

Goals:

1. Servers that replicate the Raft log and the state store but never vote (**observers**): more servers
   for stale reads, RPC load, backups and history without a larger quorum.
2. Servers that do not vote until a voter fails (**standbys**): the voter set is restored without a
   person, in seconds to minutes, with a caught-up server.
3. One owner of the voter set: autopilot. Manual commands only ask it.
4. Offload work from the voters where it is safe: scheduled backups, reads, job checks.
5. Built-in history for longer than the state store keeps it, without an external system and without a
   database.

Non-goals:

- Changes to the Raft protocol or the fork (see `technology-updates/raft` of the docs: "v4" not taken).
- Linearizable reads served by observers (would need a read index protocol; `?consistent` goes to the
  leader).
- Observers in another region (Raft stays intra-region, see raft-topology-and-cross-region.md).
- A relational or analytical database (DuckDB, SQLite or other).
- Web UI changes (UI work is postponed); the API and CLI carry everything.

## 3. Starting point

What Norca has today (main b6dd36769):

| Piece | Where | Today |
| --- | --- | --- |
| `non_voting_server` | `command/agent/config.go`, `norca/config.go` `NonVoter` | Serf tag `nonvoter=1`; bootstrap counts voters only (`serf.go`); bootstrap gives `raft.Nonvoter` |
| Join | `leader.go` `addRaftPeer` | every new server joins with `AddNonvoter` (protocol 3 only) |
| Promotion | `autopilot_oss.go` → `autopilot.DefaultPromoter()` (`StablePromoter`) | **every stable nonvoter is promoted**, so `non_voting_server` has no lasting effect (the docs say so) |
| Failed servers | autopilot `CleanupDeadServers`, `MinQuorum`, `RemoveFailedServer` | removes failed voters while the voters stay at or above `MinQuorum` |
| Health | autopilot `ServerHealth` (`Healthy`, `StableSince`, `LastContact`, `LastIndex`, `LastTerm`) | the autopilot health API (`/v1/operator/autopilot/health`; there is no CLI command), metrics `norca.cluster.autopilot_*` |
| Reads | `forward()` | `?stale` on any server; `?consistent` on the confirmed leader |
| Backups | `norca/backup`, `leader.go` `s.backup.SetLeader` | scheduled backups run on the leader only |
| Heartbeats | `heartbeat.go` | the leader keeps a TTL timer per node; `max_heartbeats_per_second` (50) stretches the TTL in large clusters, so heartbeats are not "every second" |
| Admission | `norca/admission` | runs in `Job.Register` after the forward, on the leader |
| History | metrics store (tstorage, 30 days by default, `norca.group.*` per task group: cpu, memory, restarts, OOM kills), event stream `NORCA_EVENTS_<region>` on the system bus (72 h by default) | short event history, numeric history per group |

## 4. Roles and configuration

```hcl
server {
  enabled = true
  role    = "voter"     # "voter" (default) | "standby" | "observer"
}

autopilot {
  # Number of voters autopilot keeps. 0 (default): every server with role "voter" is a voter
  # (today's behaviour). 3 or 5: standbys fill the gaps up to this number.
  target_voters = 3

  # How long a voter must be failed (autopilot health) before a standby replaces it.
  standby_failover_after = "1m"

  # Minimum time between two changes of the voter set made for standbys.
  standby_cooldown = "5m"
}
```

- `role` replaces `non_voting_server` (no released versions, so the old key is removed, not aliased).
- The role travels as the Serf tag `raft_role` (the tag `role` is taken: it marks server members as
  `nomad`); the `nonvoter` tag goes away; a missing or unknown value is a voter. Autopilot reads it
  from the member metadata (`autopilot.Server.Meta`).
- `bootstrap_expect` counts servers with role `voter` only; standbys and observers never bootstrap.
- Validation: `target_voters` must be 0, 1, 3, 5 or 7; `role = "standby"` with `target_voters = 0`
  logs a warning (a standby is then never promoted).
- The role can change with a restart only (not on reload): the Serf tag is set at startup.

## 5. The Norca promoter

`autopilotPromoter()` returns a Norca type implementing `autopilot.Promoter` instead of
`DefaultPromoter()`:

| Method | Behaviour |
| --- | --- |
| `GetServerExt` | the role from `Meta["role"]` |
| `GetStateExt` | the time of the last standby promotion or demotion (cooldown) |
| `GetNodeTypes` | `voter`, `standby` or `observer` per server, so the autopilot health API shows it |
| `CalculatePromotionsAndDemotions` | see below |
| `FilterFailedServerRemovals` | failed observers and standbys are always removable; failed voters as today (`MinQuorum`) |

`CalculatePromotionsAndDemotions`, per autopilot round, at most one change of a standby per round:

1. **Role voter:** a stable nonvoter with role `voter` is promoted (today's behaviour).
2. **Role observer:** never promoted; a voter whose role is now `observer` is demoted (a restarted
   server with a changed role).
3. **Standby promotion** when all hold:
   - `target_voters > 0` and the healthy voters are fewer than `target_voters`;
   - a voter has been unhealthy for at least `standby_failover_after`, or is gone from the
     configuration (removed by cleanup or by hand);
   - the cooldown has passed;
   - a standby is healthy and stable (`ServerStabilizationTime`) and its `LastIndex` is within
     `MaxTrailingLogs` of the leader.

   The chosen standby: same datacenter as the failed voter first, then the highest `LastIndex`, then
   the lowest ID (deterministic).
4. **Standby demotion** when the voters exceed `target_voters` because a `voter`-role server came back
   and is stable: the promoted standby with the lowest `LastIndex` is demoted, after the cooldown. The
   voter set never drops below `target_voters` by a demotion.
5. The voter count stays odd where possible: with 3 healthy voters and a returning fourth, the standby
   is demoted before the count settles at 4.

Removal of the failed voter stays with autopilot's cleanup (`CleanupDeadServers`, `MinQuorum`) or with
the manual command. Promotion first and removal later is safe: a promoted standby raises the voters by
one; with one voter of three failed the quorum becomes 3 of 4 until the cleanup, which the remaining
healthy voters plus the standby provide.

## 6. Manual failover

```text
norca operator raft promote-standby -id <standby> [-replace <failed voter>] [-force]
```

- RPC `Operator.RaftPromoteStandby` (operator write ACL), forwarded to the leader.
- Checks: the target exists, has role `standby`, is healthy and within `MaxTrailingLogs`; `-replace`
  names a voter that autopilot reports unhealthy (or Serf failed/left). `-force` skips the health
  check of the replaced voter only; the checks of the standby always apply.
- Action: `RemoveServer(<failed voter>)` by ID (address taken from the Raft configuration, never
  derived from the ID); on error the command stops. The promotion itself is left to the promoter,
  which now sees fewer healthy voters than `target_voters`; the command waits for it (timeout 30 s)
  and reports the new configuration. The cooldown does not apply to a manual promotion.
- Without `target_voters` the command refuses (nothing would promote the standby).

## 7. Reads, scheduling and RPC

- **Reads:** observers and standbys answer `?stale` reads from their state store, like followers;
  `?consistent` reads are forwarded to the leader. Clients may list observers in `server_join` /
  `servers` for their RPC.
- **Scheduling workers:** observers are servers and run scheduler workers by default
  (`num_schedulers`), which adds scheduling capacity (workers dequeue from the leader's broker). An
  observer meant only for reads and backups sets `num_schedulers = 0`.
- **Leader load:** the leader replicates to every observer and standby; its CPU and network grow
  linearly with their number. The documentation states a tested count (lab: up to 4 non-voters).
- **Leadership:** non-voters never become leader; leadership transfer only targets voters (raft).

## 8. System extensions on observers

### 8.1 Backups from an observer

Today the leader takes scheduled backups (`backup-architecture.md`). Raft snapshots for log compaction
stay where they are (every server compacts its own log); what moves is the **backup**:

- `backup { prefer_observer = true }`: the leader picks a healthy observer (lowest lag) for each
  scheduled **and manual** backup (decided 2026-11-11) and sends it the job over RPC; the observer takes the backup from its local state store,
  records the Raft index it covers in the manifest, encrypts and uploads it to the configured
  destinations, and reports back. With no healthy observer, or after a failure, the leader takes the
  backup itself.
- The manifest records the server that took the backup (`LeaderID` becomes `TakenBy`) and its index;
  restore is unchanged.
- The seal and the backup recipients are the same on every server, so an observer can encrypt.

### 8.2 Heartbeat relay (later phase)

Clients heartbeat to any server; the RPC is forwarded to the leader, which resets the node's TTL timer.
A relay on observers would answer heartbeats locally and send the leader one batch per interval
(`Node.BatchHeartbeat`: the nodes seen alive, their status unchanged); a status change, a missed
heartbeat and the first heartbeat of a node go to the leader at once.

Conditions before building it:

- a measurement shows heartbeat handling as a significant leader cost (lab bench with 1000+ simulated
  clients); the TTL stretching by `max_heartbeats_per_second` already bounds the rate;
- the failure detection time stays `TTL + heartbeat_grace`: the batch interval is below the grace;
- an observer failure must not mark its nodes down: the clients fall back to other servers within the
  TTL, and the leader treats a missing batch as "unknown", not as down, for one batch interval.

### 8.3 Job checks before the leader

`Job.Register` forwards to the leader first and validates there. The receiving server (an observer or
any follower) can reject bad requests before they reach the leader:

1. **Validation** of the job (`Job.Validate` logic) and the **admission policies** of
   `norca/admission` that do not need the leader (CEL and webhooks); the leader repeats them, the
   result of the edge check is an early rejection only.
2. **Dry-run capacity check** (`?dry_run_capacity` on register and plan): a scheduler run on the
   receiving server's snapshot, as `job plan` does, reporting unplaceable groups; non-binding, the
   leader schedules as usual.
3. **Rate limits per token**, optional and off by default (decided 2026-11-11): a token bucket per
   ACL token (anonymous requests share one bucket) on the HTTP API of every server,
   `limits { token_requests_per_second, token_burst }`, `0` = off; `429` with `Retry-After`. Write and
   read limits separately. No limit per client address.

### 8.4 History without a database

There is no database in Norca, and none is added. The history the drafts asked for comes from the
existing stores:

| What | Source | On observers |
| --- | --- | --- |
| Allocation lifecycle, task exit codes, restarts, placement, deployment and evaluation events | the event stream of the system bus (`NORCA_EVENTS_<region>`, JetStream) | a second stream `NORCA_HISTORY_<region>` mirrors the event subjects of jobs, allocations, deployments and nodes with a long retention (`bus { history { max_age = "90d", max_bytes = "15GiB" } }`), stored only on observers (JetStream placement tags) |
| CPU, memory, restarts, OOM kills per task group, cluster and node gauges | the metrics store (`norca.group.*`, `norca.cluster.*`, host metrics), history API `/v1/metrics/history` | observers may run a longer `metrics_store { retention }` (the retention update of metric-storage-update.md, 1-30 days today, longer on observers) |
| Consumption per job over time | the usage reports of the autoscaler | unchanged |

- Queries: `/v1/event/history?topic=Allocation&job=...&from=...&to=...` reads the history stream by
  subject filters and time; the metrics come from the existing history API. No SQL, no new store.
- The metric names follow the OpenTelemetry plan (`norca.*`); external export stays optional (OTLP),
  and no Norca feature reads an external system (goal 7 of that plan).
- Garbage collection of the state store is unchanged; the history is outside Raft and can be lost
  with an observer (another observer has the replicated stream).

### 8.5 Operator events

The event stream sees state changes, not who caused them: a server gone from the Raft configuration
shows, the `operator raft remove-peer` that removed it does not. A new **`Operator`** topic records every
operator **write** call:

- the operation (`Operator.RaftRemovePeerByID`, `Operator.AutopilotSetConfiguration`,
  `Operator.SchedulerSetConfiguration`, `Operator.RaftPromoteStandby`, `Keyring.Rotate`,
  `Backup.Restore`, `Backup.Run`, seal and unseal, `Node.UpdateDrain`, `Node.UpdateEligibility`, the
  snapshot restore, `System.GarbageCollect`, ...); the list is kept in one table next to the endpoints;
- who: the token accessor ID and name, or the workload identity (job, task); the server that received
  the call; the result (success, or the error text);
- a parameter summary built from an **allow list** per operation (IDs, names, flags, durations); no
  secret ever enters the event: unseal shares, keys, tokens, recovery material and secret values are
  never copied.

Reads are not recorded (volume, little value). The events go to the event stream like the other topics
(ACL: operator read for the `Operator` topic) and into the history stream by default (section 14).
This is Norca's own design, not modelled on any audit log product (clean room, section 1).

## 9. Observability

- Serf tag `raft_role`; `operator raft list-peers` gets a `Role` column (voter / standby / observer next to
  the Raft suffrage); the autopilot health API shows the role (`Role`).
- Metrics store (leader only, built in phase 2): `norca.cluster.autopilot_voters`,
  `norca.cluster.autopilot_standbys_ready`, `norca.cluster.autopilot_standby_promotions` and
  `_demotions` (the `autopilot_` prefix keeps them under operator read); telemetry counters
  `nomad.autopilot.standby_promotions` and `_demotions` until the OpenTelemetry rename. Later phases:
  `norca.backup.taken_by_observer` (phase 3); a per-server role gauge was not needed (the role is in
  the Raft configuration and the health API).
- Events: a `Server` topic event (`StandbyPromoted`, `StandbyDemoted`) on every standby promotion or
  demotion, published by the leader that sees the change in the autopilot state (not written to Raft,
  like the `Scaling` events).
- Logs: one INFO line per change with the failed voter, the chosen standby, its lag and the reason.

## 10. Failure scenarios

| Scenario | Expected behaviour |
| --- | --- |
| One voter of 3 dies, a standby is ready | after `standby_failover_after` the standby is promoted (4 voters, 1 unhealthy), cleanup removes the dead voter (3 voters) |
| The dead voter comes back | it joins as nonvoter; with role `voter` it is promoted when stable; then 4 voters → the standby is demoted after the cooldown |
| Network flap of a voter shorter than `standby_failover_after` | nothing changes |
| Two voters of 3 die | no quorum: autopilot cannot change anything (Raft needs a quorum for configuration changes); manual `peers.json` recovery as today |
| A standby is lagging | not chosen; with no caught-up standby nothing is promoted, autopilot logs why |
| The leader dies | a normal election among the voters; standby promotion follows if a voter is still missing |
| All observers die | stale reads move to the other servers; backups fall back to the leader; the history stream is unavailable until an observer returns |
| A server's role changes from voter to observer | after its restart it is demoted; if that leaves fewer voters than `target_voters`, a standby fills in |

## 11. Testing

- **Unit:** the promoter on autopilot `State` fixtures: every rule of section 5, the datacenter and
  lag choice, cooldown, odd counts, no change without quorum.
- **Server tests:** 3 voters + 1 standby + 1 observer in-process; kill a voter, see the promotion and
  the cleanup; bring it back, see the demotion; observer never promoted; `promote-standby` checks.
- **Lab case** (norca-lab): the same topology on VMs with a voter powered off and back; stale reads
  from the observer; a scheduled backup taken by the observer and restored; a 5-minute bench with
  observers to measure the leader's CPU and replication times (`norca.server.raft_append_entries_seconds`).
- **Clean room:** audit run and record (section 1).

## 12. Documentation

- configuration: `server.role` (replacing `non_voting_server`), the new `autopilot` keys,
  `backup.prefer_observer`, `bus.history`, `limits` rate limits.
- operations: a page "Observers and hot standby" (topologies, sizing, failover, recovery).
- commands: `operator raft promote-standby`, the `Role` column of `list-peers`.
- concepts/consensus and technology-updates/raft: a row for observers and standbys.
- clean-room pages: the record of section 1.

## 13. Phases

| Phase | Content |
| --- | --- |
| 1 | `role` and the Serf tag, the Norca promoter (voter / observer rules), `bootstrap_expect`, list-peers and health output, docs — **built 2026-11-11** (norca `8087e57bc`, PR #14; docs PR #23) |
| 2 | Standby: `target_voters`, failover and demotion rules, cooldown, `promote-standby`, events and metrics, lab case — **built 2026-11-11** (norca `feat/raft-standby`; deployer `server_settings.role`; lab case `standby`; lab run pending) |
| 3 | Backups from observers (`prefer_observer`) |
| 4 | Job checks on the receiving server and API rate limits |
| 5 | History stream on observers and its API; the `Operator` and `Server` topics |
| 6 | Heartbeat relay, only if the measurement of 8.2 asks for it |

Each phase is its own branch and PR; the lab case runs after phases 2, 3 and 5.

## 14. Decisions

Decided by the user on 2026-11-11:

1. **`target_voters` defaults to 0**: every `voter`-role server votes, as today; standbys are promoted
   only where a target is set.
2. **A promoted standby steps back** when the old voter returns and is stable (rule 4 of section 5).
3. **Backups from observers: scheduled and manual** ones.
4. **History retention: 90 days / 15 GiB** by default.
5. **Rate limits per token, optional** (off by default); no limit per client address.
6. **History topics.** The event topics of Norca today: `Job`, `Allocation`, `Deployment`,
   `Evaluation`, `Node`, `NodePool`, `Service`, `Scaling`, `Backup`, `Quota`, `NetworkPolicy`,
   `ServiceIntention`, `AdmissionPolicy`, `CustomObject`, `Variables`, `Secret`, `SecretAccess`,
   `ACLPolicy`, `ACLToken`, `ACLRole`, `ACLAuthMethod`, `ACLBindingRule` (plus `Bus` for gap notices).
   This plan adds `Server` (standby promotions and demotions) and `Operator` (section 8.5). Kept by
   default: `Job`, `Allocation`, `Deployment`, `Node`, `Scaling`, `Backup`, `Server`, `Operator`; the
   rest on request (`bus { history { topics = [...] } }`), `Evaluation` and `Service` with a warning
   about their volume; ACL, `Variables`, `Secret` and `SecretAccess` only when listed explicitly.
7. **Operator events** (section 8.5) as proposed.

Postponed (the user, 2026-11-11: later, if there is time):

- **Operator reads served by observers.** Read operator endpoints (raft configuration, autopilot
  health, scheduler configuration, metrics) answered by an observer with `?stale`; the operator writes
  always go to the leader. Not part of the phases of section 13.
