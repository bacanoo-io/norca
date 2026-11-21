<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Global state across regions: a movable authority, then a global Raft

**Status:** plan, 2026-11-16. Direction decided by the maintainer on 2026-11-16: **A** (an authoritative
region that can be moved, with every region holding the global data) now, **B** (a Raft group of its own for
the global data, across regions) when a deployment has three regions or more. Nothing built; the questions
in [Decisions](#decisions) wait for the maintainer.

Why now: the `Tenant` kind of the core operators (phase 5) keeps namespaces, quotas, ACL policies and roles,
which are global, from an object that lives in one region only. The discussion of 2026-11-16 showed that a
single authoritative region is a single point of loss for the global data and the only place it can be
changed.

## 1. What exists

Each region has its own Raft (section 3 of [raft-topology-and-cross-region.md](raft-topology-and-cross-region.md));
between regions there is no consensus, only RPC forwarding over Serf (`forwardRegion`, `ErrNoRegionPath`).
Global data has one writer, the **authoritative region** (`server { authoritative_region }`, default the
server's own region):

| Data | Written | Other regions | Where |
| --- | --- | --- | --- |
| ACL policies, global tokens, roles, auth methods, binding rules | forwarded to the authoritative region (`acl_endpoint.go`, `args.Region = AuthoritativeRegion`) | copied by the leader's replication loops (`replicateACLPolicies`, `replicateACLTokens`, `replicateACLRoles`, `replicateACLAuthMethods`, `replicateACLBindingRules`) | `leader.go:477-496` |
| Namespaces, node pools, quota specs | forwarded (`namespace_endpoint.go`, `quota_endpoint.go`) | copied (`replicateNamespaces`, `replicateNodePools`, `replicateQuotaSpecs`) **only when ACLs are on**: the loops start inside `if s.config.ACLEnabled` | `leader.go:478`, `leader_quota.go` |
| Secrets of the secret store | in the authoritative region | read-only replicas; a write elsewhere is refused with 409 (`secret "<path>" is replicated from the authoritative region ...`) | `secrets_replication.go` |
| The default trust domain of native mTLS | derived from the authoritative region's name when none is set | | `mtls_ca.go:114` |
| Custom objects (`Tenant`, `CertificateIssuer`, ...) | in the region they were written in | **not copied** | |
| The keyring | per region (key material fetched from the region's own peers) | | `encrypter.go`, `KeyringReplicator` |

A replication loop fetches the whole list from the authoritative region (stale reads allowed), diffs it
against the local store and applies the difference through the local Raft. A region that loses the
authoritative region keeps its copies and its workloads; it can't change global data until the authoritative
region is back. If the authoritative region is lost for good, the copies survive but the region has to be
changed in the configuration of every server, and the custom objects of that region (tenants) are gone.

## 2. Goals and limits

- **No loss of global data with one region.** Every region holds all of it, objects of global kinds
  included.
- **Global writes resume without editing every server's configuration** (A: one command; B: by
  themselves).
- **Never two writers** of the global data at once, also during a partition.
- Workload state (jobs, allocations, deployments, volumes, services) stays in its region. Multi-region jobs
  are unchanged.
- Local work keeps its latency: only global writes may cross regions.
- **Regional writes and regional identities do not cross regions.** A node's secret, a client certificate
  (`client.<region>.norca`) and a workload identity (signed by the region's keyring) are identities of one
  region; the RPCs of nodes stay in their region (review finding JOG-18: a `client.A.norca` certificate
  registered a node in region B through a server of A, because B saw only the forwarding server's
  `server.A.norca` certificate). A cross-region RPC authorizes by its token (management, the replication
  token, a global ACL token), never by the certificate of the server that forwarded it: the TLS handshake
  accepts `server.<region>.norca` of every region, so a forwarded request of any caller arrives on such a
  connection.

## 3. Part A: an authority that can be moved

### A1. Global kinds are replicated

- A kind can be **global**: `ObjectKind.Global = true`. Built in: `Tenant` (and later `DNSRecord` if it is
  cluster scoped, decision 1). Operator-defined kinds may set it (decision 1).
- Objects of a global kind are written in the authority only, forwarded there from any region like an ACL
  policy. In the other regions they are **read-only replicas**, spec, status and finalizers included, so a
  promoted region knows what each tenant owns (`status.resources`). A write elsewhere is refused with 409,
  as for secrets.
- A new replication loop `replicateGlobalObjects` copies them, with the same shape as the other loops
  (list by kind with stale reads, diff, apply locally). Object events of a replica are not published again
  (the bus of each region would otherwise carry the same change twice): replicas are written with a flag the
  event publisher skips.
- The built-in operator of a global kind runs only in the authority. In the other regions its loop starts
  but does nothing for global kinds; after a promotion it takes over in the new authority.
- The replication of namespaces, node pools and quota specs runs **with or without ACLs** (decision 7): today
  a cluster without ACLs has no copies at all.

### A2. The authority record

Today the authority is a configuration value. It becomes a replicated record, and the configuration value
only its starting point:

```text
Authority {
  Region  string   // the region that writes global data
  Epoch   uint64   // raised by every promotion
  Since   time
  TrustDomain string  // pinned at bootstrap (A6)
}
```

- Every region keeps it in its Raft (a table of one row). The bootstrap writes `authoritative_region` with
  epoch 1. After that the configuration value is only checked against the record: a server whose
  `authoritative_region` names another region logs that the record decides and says which region it is.
- Every place that reads `config.AuthoritativeRegion` reads the record instead (`s.authority()`): the
  forwarding of global writes, the replication loops, the secret replicas, the token checks of the
  replication RPCs.
- Every replication response carries the source's record; a region applies a record with a higher epoch
  and ignores a lower one. This is the fencing of A3.
- The new cross-region RPCs of A (`Authority.Announce`, the replication RPCs that carry the record, the
  writes of A5 through `forwardRegion`) are authorized by a token: the replication token or a management
  token, checked in the receiving region. That the request came over a `server.<region>.norca` connection
  is not an authorization (section 2, JOG-18).

### A3. Promotion

```text
norca operator authority promote -region <new> [-force]
norca operator authority status
```

1. Sent to the region that is to become the authority; management token; refused while the current
   authority answers and is healthy, unless `-force` (decision 3: no automatic promotion in A).
2. The new region checks its replicas: the index each replication loop last applied and its age (A5). It
   prints what it would lose and asks for `-force` when a loop is behind or has never completed.
3. It writes `Authority{Region: new, Epoch: old+1}` through its own Raft, stops its replication loops,
   starts the operators of the global kinds, and accepts global writes.
4. It tells every region it can reach (`Authority.Announce` RPC); the others learn it from the next
   replication response anyway.

**The old authority coming back** sees a higher epoch in a response from any region (its own replication
loops start as soon as it is not the authority, and Serf brings it the peers). It then:

- stops accepting global writes (409, `the authority moved to region "<new>" at epoch <n>; write there`)
  and stops its operators of global kinds;
- before its loops overwrite anything, writes a **report of the global writes it made after the last
  index the new authority had replicated** (decision 2): which objects, policies, namespaces, tokens; and
  takes a backup of its state (`backup` with the label `authority-demotion`), so nothing is lost silently;
- then replicates from the new authority like any region.

### A4. What a promotion can lose

Replication is asynchronous: a global write acknowledged by the old authority and not yet copied to the new
one is lost by the promotion (and listed in the report of A3 if the old authority comes back). To keep that
window visible:

- metrics `norca.authority.replication_lag_seconds` and `_index` per kind of global data (in the metrics
  store, so `operator authority status` and the dashboard can show them);
- a warning event when a loop has not completed for longer than a threshold (default 5 minutes).

### A5. The tenants' network isolation

The `tenant-isolation` NetworkPolicy is a regional object. The Tenant operator (in the authority) writes it
into **every** federated region through `forwardRegion`, keeps it there like its other resources, and lists
it per region in its status (`resources[]` gets `region`). A region it can't reach is retried and reported
(`Ready` false, reason `RegionUnreachable`), not skipped silently.

### A6. The trust domain of native mTLS

The default trust domain is derived from the authority's name (`mtls_ca.go:114`). A promotion would change it
and every workload certificate with it. The trust domain is **pinned** in the authority record at bootstrap
(the value in use: the configured one, or the one derived then) and never derived again (decision 6).

### A7. Tests and lab

- Unit and server tests: the record (bootstrap, epochs, refusal of a lower epoch); replication of a global
  kind with its status; read-only replicas (409); promotion with and without `-force`; the old authority's
  demotion, report and backup; the operator of a global kind running only in the authority; replication
  without ACLs.
- Lab, case `multiregion` (r1, r2, r3): tenants written in r1 and visible in r2 and r3; r1's servers
  stopped; `promote -region r2 -force`; a tenant changed in r2; r1 started again: demoted, its report, the
  tenant of r2 everywhere, the isolation policy in all three regions.

### A8. Phases of A

| Phase | Content |
| --- | --- |
| A0 | Replication of namespaces, node pools and quota specs without ACLs (decision 7); the lag metrics (A4). Small, useful on its own. |
| A1 | Global kinds: `ObjectKind.Global`, `replicateGlobalObjects`, read-only replicas, operators of global kinds only in the authority; `Tenant` global; the Tenant's network policy in every region (A5). |
| A2 | The authority record and `s.authority()`; the trust domain pinned (A6). |
| A3 | `operator authority promote/status`, the announce RPC, the demotion with report and backup; docs and the lab step. |

## 4. Part B: a Raft group for the global data

### B1. Shape

A second, small Raft group, **global**, next to the Raft of each region:

- **Voters:** chosen servers of the regions, an odd number of at least three, spread so that no region holds
  a majority (three regions with one voter each, or five regions). A deployment with two regions adds a
  **witness region**: one server, possibly small, whose only duty is to vote in the global group (no
  clients, no jobs) (decision 5).
- **Non-voters:** at least one server of every other region joins as a non-voter (the observer role of
  [raft-observers-and-hot-standby.md](raft-observers-and-hot-standby.md)), so every region has the global log
  locally.
- **Transport:** the RPC port 4647 with a byte of its own (`RpcGlobalRaft`), mTLS with the server
  certificates; a global peer is accepted with the name `server.<region>.norca` of any region in the list of
  global members, not only its own region's.
- **Storage:** a data directory of its own (`<data_dir>/server/global-raft`), snapshots of its own.
- **Membership:** `server { global { role = "voter" | "observer" } }`; autopilot of its own for the group, with
  the rules of the observers plan (no automatic promotion of an observer to voter across regions).

### B2. How the regions read it (decision 4)

Recommended, **B-light:** the global group's FSM holds the global tables (the data of section 1 and the
global kinds). A region keeps its copies in its own state store as today, but the source of the replication
loops is the region's **own member of the global group**, read locally, not the authority over the WAN:

- the regions' schedulers, ACL checks and endpoints are unchanged (they read their region's store);
- the "authority" disappears: a global write in any region goes to the leader of the global group (forwarded
  across regions if needed) and is committed by a majority of regions;
- the loops of A become local and cheap; their lag is the global group's commit plus one local apply.

The alternative (the regions read the global tables straight from the global FSM, no copies in the regional
store) removes the copies but touches every reader of those tables and the regional snapshots; kept as a
later step.

### B3. Failures

- **One region lost** (of three or more): the global group keeps a majority; global writes go on; nothing
  to promote.
- **A region cut off** (a minority): its local work goes on; its global writes fail with
  `no quorum of the global group` and its copies stay as they were; when it is back the log catches it up.
- **Majority lost:** global writes stop everywhere; reads go on from the copies. Recovery as for a Raft that
  lost its quorum (`peers.json` of the global group), documented.
- The global group's leader should sit where most global writes come from: a leader preference by region
  (decision 8).

### B4. From A to B

1. The global group is bootstrapped from the authority's data (a snapshot of the global tables of the
   authority's store, written as the group's first snapshot).
2. The authority record (A2) gets `Mode = global`: global writes go to the group from then on, and the
   replication loops switch their source to the local member.
3. A cluster can stay on A (two regions without a witness, or by choice).

### B5. Costs

A second Raft in the chosen servers (disk, fsync latency across regions for global writes: 50-250 ms; they
are rare), a new port byte and peer name check, the sizing of the global group, its upgrades, and the tests
of a group that spans regions (partitions, a lost witness). The regional Rafts are not touched.

### B6. Phases of B

| Phase | Content |
| --- | --- |
| B1 | The global group: transport, storage, membership, autopilot; the FSM with the global tables; `operator global raft list-peers/remove-peer`. |
| B2 | Global writes through the group (the endpoints of section 1 and the global kinds); the replication loops read the local member. |
| B3 | The switch from A (B4), the witness region in the deployer (`regions[].role = "witness"`), docs, lab: case `multiregion` with r3 as witness, r1 cut off, writes in r2. |

## 5. Out of scope

Workload state across regions; multi-region jobs; the system bus and change data capture across regions
(each region keeps its own streams); a region-local mode where namespaces and ACLs are not global at all
(possible today with separate clusters and the same configuration applied by Rig stacks or the deployer:
documented as an option, not built here).

## Decisions

Open, for the maintainer:

1. **Which kinds are global?** `Tenant` yes. Operator-defined kinds may set `Global`? Recommendation: yes,
   opt-in, with a warning in the docs that their operator must then run in the authority only.
2. **The writes of a demoted authority:** discarded with a report and a backup (recommended), or kept and
   merged? Merging global security data is not safe.
3. **Automatic promotion in A?** Recommendation: no; manual with `-force`. Automatic needs a third party to
   decide, which is what B gives.
4. **B-light (copies fed by the local member) or the full split?** Recommendation: B-light first.
5. **Witness region:** one server, voter of the global group only, no clients? Recommendation: yes; the
   deployer gets a `witness` region role, and the server of a witness region refuses node registrations
   (`Node.Register` answers that the region takes no nodes), so no node, of its own region or forwarded,
   can attract work there (JOG-18).
6. **Pin the mTLS trust domain at bootstrap?** Recommendation: yes, in the authority record; a configured
   trust domain still wins.
7. **Replicate namespaces, node pools and quota specs without ACLs?** Recommendation: yes (A0).
8. **Leader preference of the global group by region?** Recommendation: yes, a soft preference (transfer the
   leadership when the preferred region's voter is healthy).
