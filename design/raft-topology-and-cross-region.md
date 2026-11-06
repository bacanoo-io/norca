<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Raft Consensus, Serf Gossip, and Cross-Region Communication Architecture

## 1. Executive Summary & Fundamental Principles

In Nomad and Norca, a frequent point of confusion is how consensus, state replication, and communication scale across multi-datacenter and multi-region topologies. This document details the architectural boundaries and exact mechanisms implemented in the codebase.

The foundational design principle is:
> **Raft consensus is strictly intra-region. Raft logs, heartbeats, and leader elections never cross regional boundaries.**

Instead of stretching a single Raft quorum across high-latency, unreliable wide-area networks (WANs), the architecture decouples consistency and scaling across three hierarchical tiers:

1. **Intra-Region / Intra-Datacenter (LAN)**: Maximum throughput, sub-millisecond latency. Raft peers replicate the state machine (`MemDB`) via BoltDB-backed log entries.
2. **Intra-Region / Cross-Datacenter (Metro / Low-Latency WAN)**: Servers within the *same* Region can be distributed across multiple Datacenters (Availability Zones) to form a fault-tolerant Raft quorum (e.g., across 3 AZs with $<10\text{ ms}$ round-trip time).
3. **Inter-Region (Global WAN)**: Regions are fully autonomous consensus islands. Each region maintains its own independent Raft cluster and state store. Cross-region coordination is provided by a unified **Serf Gossip mesh** (discovery and failure detection) and **Cross-Region RPC Forwarding** over connection-pooled TCP/mTLS.

```
+---------------------------------------------------------------------------------------+
|                                    SERF GOSSIP MESH                                   |
|             (Port 4648 UDP/TCP, Memberlist WAN timing, Server Discovery)              |
+-------------------------------------------+-------------------------------------------+
                                            |
                   +------------------------+------------------------+
                   |                                                 |
+------------------v------------------+           +------------------v------------------+
|          REGION: europe-west        |           |           REGION: us-east           |
|                                     |           |                                     |
|  +-------------------------------+  |           |  +-------------------------------+  |
|  |       RAFT CLUSTER (Raft v3)  |  |           |  |       RAFT CLUSTER (Raft v3)  |  |
|  |     Port 4647 (RpcRaft 0x02)  |  |           |  |     Port 4647 (RpcRaft 0x02)  |  |
|  |  mTLS: server.europe-west.norca |  |           |  |    mTLS: server.us-east.norca   |  |
|  +---------------+---------------+  |           |  +---------------+---------------+  |
|                  |                  |           |                  |                  |
|       +----------+----------+       |           |       +----------+----------+       |
|       |                     |       |           |       |                     |       |
|  +----v----+           +----v----+  |   Cross-  |  +----v----+           +----v----+  |
|  | Server  |           | Server  |  |   Region  |  | Server  |           | Server  |  |
|  |  DC: a  |           |  DC: b  |  |    RPC    |  |  DC: 1  |           |  DC: 2  |  |
|  +----+----+           +----+----+  |<--------->|  +----+----+           +----+----+  |
|       |                     |       | (Port 4647|       |                     |       |
|  +----v----+           +----v----+  |  RpcNomad)|  +----v----+           +----v----+  |
|  | Client  |           | Client  |  |           |  | Client  |           | Client  |  |
|  | Nodes   |           | Nodes   |  |           |  | Nodes   |           | Nodes   |  |
|  +---------+           +---------+  |           |  +---------+           +---------+  |
+-------------------------------------+           +-------------------------------------+
```

---

## 2. Network Protocols and Port Multiplexing

Norca uses two primary network ports on servers for control plane operations:
- **Port 4647 (TCP)**: The unified RPC listener.
- **Port 4648 (TCP & UDP)**: The Serf gossip listener.

### 2.1 The Unified Port 4647 Multiplexer

To avoid opening multiple ports for Raft, client RPCs, server RPCs, and streaming, Norca uses a single TCP listener on port 4647. The first byte transmitted after establishing the connection dictates how the stream is routed (`norca/norca/rpc.go:295`).

| Byte Identifier | Constant (`norca/helper/pool/conn.go`) | Purpose | Handled By |
| :--- | :--- | :--- | :--- |
| `0x01` | `RpcNomad` | Standard request/response RPC (API, CLI, Server-to-Server, Client-to-Server) | `r.handleNomadConn` |
| `0x02` | `RpcRaft` | Raw Raft consensus protocol stream | `r.srv.raftLayer.Handoff` |
| `0x03` | `RpcMultiplex` | Yamux stream multiplexer (v1) | `r.handleMultiplex` |
| `0x04` | `RpcTLS` | TLS handshake initiation | In-place TLS upgrade -> recurse `handleConn` |
| `0x05` | `RpcStreaming` | High-throughput streaming (logs, events, file streaming) | Streaming RPC handler |
| `0x06` | `RpcMultiplexV2` | Yamux multiplexer allowing per-channel mode switching | Yamux channel router |

```
Client / Server Connection to :4647
                  |
                  v
         Peek / Read First Byte
                  |
       +----------+----------+----------+
       |                     |          |
   Byte == 0x04          Byte == 0x02  Byte == 0x01
   (RpcTLS)              (RpcRaft)     (RpcNomad)
       |                     |          |
       v                     |          v
 Perform TLS Handshake       |     net/rpc Dispatcher
       |                     |     (API, Queries, Writes)
       v                     v
 Re-read Byte  ------> validateRaftTLS()
                             |
                   +---------+---------+
                   |                   |
               SAN matches?        SAN mismatch / remote region?
                   |                   |
                   v                   v
           Handoff to Raft        TCP Connection Closed
             StreamLayer          (Unauthorized Error)
```

### 2.2 Cryptographic Isolation of Raft: `validateRaftTLS`

A critical security safeguard guarantees that Raft never accepts connections across regions. When TLS is enabled, any connection demanding Raft handoff (`RpcRaft`, `0x02`) is inspected by `validateRaftTLS` (`norca/norca/rpc.go:908`):

```go
func (r *rpcHandler) validateRaftTLS(rpcCtx *RPCContext) error {
    tlsConf := r.srv.getTLSConfig()
    if !tlsConf.EnableRPC || !tlsConf.VerifyServerHostname || tlsConf.RPCUpgradeMode {
        return nil
    }

    // Check that `server.<region>.norca` is present in cert SAN
    expected := "server." + r.srv.Region() + ".norca"
    err := rpcCtx.ValidateCertificateForName(expected)
    if err != nil {
        return fmt.Errorf("unauthorized raft connection from %s: %v", rpcCtx.Conn.RemoteAddr(), err)
    }
    return nil
}
```

If a server in region `us-east` attempts to initiate a Raft transport connection to a server in `europe-west`, the certificate's SAN (`server.us-east.norca`) fails validation against `server.europe-west.norca`. The connection is immediately closed.

---

## 3. Intra-Region Raft Consensus & Datacenter Topology

### 3.1 Raft Engine Specifications

Within a single region, Norca runs HashiCorp Raft Protocol Version 3 (`norca/norca/server.go:1431`):
- **Log and Stable Storage**: BoltDB (`bbolt`) backend saved at `<data_dir>/raft/raft.db` wrapped by an in-memory `LogCache` (`raftLogCacheSize = 512`).
- **Snapshot Storage**: Disk-based snapshot store at `<data_dir>/raft/snapshots/`.
- **Server Identity**: Protocol v3 uses permanent GUID-based `ServerID` (matching `s.config.NodeID`), preventing cluster corruption when server IP addresses change.
- **State Machine (FSM)**: Applies committed log entries directly to the in-memory `MemDB` state store (`norca/norca/fsm.go`), dispatching events to the evaluation broker and periodic scheduler.

### 3.2 Spanning Datacenters within a Region

In Norca's topology, **1 Datacenter = 1 Availability Zone (AZ)**.
Raft servers may—and should—be distributed across datacenters in the same region:

- **Quorum Sizing**: A cluster of $N$ voting servers requires a majority quorum of $\lfloor N/2 \rfloor + 1$ voters to commit any write.
  - **3 Servers (across 3 Datacenters)**: Quorum is 2. The cluster can tolerate the complete failure of 1 datacenter without interruption.
  - **5 Servers (across 3 or 5 Datacenters)**: Quorum is 3. The cluster can tolerate the loss of 2 datacenters.
- **Latency Requirement**: Because Raft requires confirmation from a majority before an entry is committed, write latency is dictated by the round-trip time (RTT) between datacenters. Inter-datacenter RTT within the same region should ideally be $< 10\text{ ms}$. If RTT exceeds Raft election timeouts (`HeartbeatTimeout`, typically $1000\text{ ms}$), spurious leader elections occur.

### 3.3 Serf-Driven Raft Peer Reconciliation

Nomad servers automatically manage the Raft configuration using Serf events. The leader runs a continuous reconciliation loop (`norca/norca/leader.go:1536`):

```go
func (s *Server) reconcileMember(member serf.Member) error {
    valid, parts := isNomadServer(member)
    // Strictly filter out members that do NOT belong to this region!
    if !valid || parts.Region != s.config.Region {
        return nil
    }

    switch member.Status {
    case serf.StatusAlive:
        return s.addRaftPeer(member, parts)
    case serf.StatusLeft, StatusReap:
        return s.removeRaftPeer(member, parts)
    }
    return nil
}
```

Notice:
1. `parts.Region != s.config.Region`: Any Serf member from a different region is ignored by Raft reconciliation.
2. `parts.Datacenter`: The datacenter tag is **not** used to exclude members. Servers from `dc-a`, `dc-b`, and `dc-c` within `s.config.Region` are happily added to the same regional Raft voter set.

---

## 4. Serf Gossip Mesh (Inter-Region and Inter-Datacenter)

### 4.1 Single Gossip Pool Architecture

Unlike Consul, which maintains two separate Serf pools (Serf LAN within each datacenter and Serf WAN across datacenters), Nomad and Norca use a **single Serf gossip mesh** connecting all servers across all regions and datacenters.

The Serf configuration is tuned for WAN conditions (`norca/norca/config.go:595`):
```go
// Serf should use WAN timing, since we are using it
// to communicate between regions as well
c.SerfConfig.MemberlistConfig = memberlist.DefaultWANConfig()
c.SerfConfig.MemberlistConfig.BindPort = DefaultSerfPort // 4648
c.SerfConfig.ReconnectTimeout = 3 * 24 * time.Hour       // 3 days reap interval
```

### 4.2 Member Metadata Tags

Every server registers tags in Serf (`norca/norca/server.go:1659`):
- `role`: `"nomad"`
- `region`: e.g. `"europe-west"`, `"us-east"`
- `dc`: e.g. `"eu-west-1a"`, `"us-east-1a"`
- `rpc_addr`: IP for RPC communication
- `port`: Server RPC port (4647)
- `id`: Unique node ID (UUID)
- `raft_vsn`: Raft protocol version (e.g., `"3"`)
- `norca_vsn`: Product version (`"0.1.0"`)
- `bootstrap`: `"1"` if single-server bootstrap
- `expect`: Bootstrap expectation count (e.g., `"3"`)
- `nonvoter`: `"1"` if non-voting standby node

### 4.3 Routing Tables: `s.peers` vs `s.localPeers`

When Serf receives a join event (`nodeJoin` in `norca/norca/serf.go:55`), the server segments peers:
- `s.peers[region] []*serverParts`: Contains all known servers grouped by region. Used for cross-region RPC routing.
- `s.localPeers[raft.ServerAddress] *serverParts`: Strictly contains servers where `parts.Region == s.config.Region`. Used for local Raft operations and leader lookups.

---

## 5. Cross-Region Communication (What Replaces Raft)

Because Raft does not operate across regions, all inter-region interactions rely on **Cross-Region RPC Forwarding** and **Distributed Deployment Watchers**.

```
Client / User Request
   (e.g., norca job run -region=us-east my-job.norca.hcl)
         |
         v
[ Region: europe-west Server (Follower or Leader) ]
         |
         | 1. RequestRegion() == "us-east" != s.config.Region
         | 2. r.findRegionServer("us-east") -> selects random server in us-east
         v
[ Cross-Region RPC: forwardRegion() ]
   (TCP/mTLS to us-east Server Port 4647, pool.RpcNomad 0x01)
         |
         v
[ Region: us-east Remote Server ]
         |
         | 3. Remote server receives RPC.
         |    If it is a Follower and RPC is a write:
         |    r.forwardLeader() forwards to local Raft leader.
         v
[ Region: us-east Raft Leader ]
         |
         | 4. Leader applies write to us-east Raft log
         v
[ us-east Raft Quorum Replicates & Commits Entry ]
         |
         v
[ Response propagates back across regions to client ]
```

### 5.1 The `forward()` Dispatcher

Every RPC invocation passes through `r.forward()` (`norca/norca/rpc.go:575`):

```go
func (r *rpcHandler) forward(method string, info structs.RPCInfo, args interface{}, reply interface{}) (bool, error) {
    region := info.RequestRegion()
    if region == "" {
        return true, fmt.Errorf("missing region for target RPC")
    }

    // 1. Cross-Region Forwarding:
    if region != r.srv.config.Region {
        info.SetForwarded()
        err := r.forwardRegion(region, method, args, reply)
        return true, err
    }

    // 2. Local Stale Read: Answer immediately without contacting leader
    if info.IsRead() && info.AllowStaleRead() {
        return false, nil
    }

    // 3. Local Leader Forwarding: If not leader, forward to regional leader
    remoteServer, err := r.getLeaderForRPC()
    if err != nil {
        return true, err
    }
    if remoteServer == nil {
        return false, nil // We are the leader!
    }

    info.SetForwarded()
    return true, r.forwardLeader(remoteServer, method, args, reply)
}
```

### 5.2 Server Connection Pooling (`ConnPool`)

Cross-region RPC connections are pooled and multiplexed via `norca/helper/pool/pool.go`:
- **Yamux Multiplexing**: Multiple concurrent RPC calls between two regions share a single underlying TCP/mTLS connection.
- **Msgpack Encoding**: Structured arguments and replies use high-performance MessagePack serialization (`structs.MsgpackHandle`).
- **Load Balancing**: `findRegionServer` picks a random healthy server from `s.peers[region]`. If that remote server is not the leader, it forwards internally to its local leader.

---

## 6. Multi-Region Deployments & Coordination

When jobs span multiple regions (using the `multiregion { ... }` stanza), Norca orchestrates rollouts without shared Raft state.

```
Submitting Region (e.g. europe-west)              Target Region (e.g. us-east)
------------------------------------              ----------------------------
1. Receives multiregion Job.Register
   |
2. Interpolates region-specific stanzas
   |
3. Submits local Job.Register ----(Raft)----> [europe-west Raft Store]
   |
4. Submits Peer Job.Register
   via cross-region RPC --------------------> 5. Receives peer Job.Register
                                                  |
                                                  v
                                              6. Submits to local Raft
                                                 [us-east Raft Store]
                                                  |
7. Multiregion Deployment Watcher <--(RPC)--> 8. Multiregion Deployment Watcher
   (Coordinates canary promotion,                (Reports local deployment
    group rollouts, and rollbacks)                health and progression)
```

1. **Submitting Region as Coordinator**: The region that receives the initial submission parses the multiregion job, creates a canonical version, and issues parallel `Job.Register` calls to each target region (`norca/norca/job_endpoint_multiregion.go:15`).
2. **Peer Registration**: These requests are flagged as peer registrations (`args.MultiregionRegistration = "peer"`). This tells the remote region to store the job in its own Raft cluster without re-forwarding it.
3. **Deployment Watchers**: Each region creates a `Deployment` record in its local Raft store. Region deployment watchers (`norca/norca/deploymentwatcher/multiregion.go`) communicate over cross-region RPC to synchronize rollouts (e.g., wait for region A to succeed before promoting region B).

---

## 7. Topology Comparison Matrix

| Property | Intra-Datacenter (Same DC) | Inter-Datacenter (Same Region) | Inter-Region (Cross-Region) |
| :--- | :--- | :--- | :--- |
| **Consensus Protocol** | Raft Protocol v3 | Raft Protocol v3 | **None** (Serf + RPC Forwarding) |
| **State Consistency** | Strong (Linearizable Raft) | Strong (Linearizable Raft) | Autonomous per region; Eventual rollout sync |
| **Data Replication** | Synchronous Raft log commit | Synchronous Raft log commit | Asynchronous / Explicit Job Registration |
| **Network Latency** | $< 1\text{ ms}$ | Typically $< 10\text{ ms}$ | $50\text{ ms} - 250\text{ ms}+$ |
| **Transport Port** | Port 4647 (`RpcRaft` `0x02`) | Port 4647 (`RpcRaft` `0x02`) | Port 4647 (`RpcNomad` `0x01`) |
| **mTLS Requirement** | `server.<region>.norca` | `server.<region>.norca` | `server.<remote-region>.norca` |
| **Failure Domain** | Single Host / Rack | Single Availability Zone | Entire Cloud Provider / Geographic Region |
| **Failure Tolerance** | $N/2$ node loss | Minority AZ loss ($< N/2$ nodes) | Complete regional isolation; surviving regions continue unaffected |

---

## 8. Failure Scenarios and Edge Cases

### 8.1 Complete WAN Partition Between Regions
- **Behavior**: Region A and Region B cannot communicate over Serf or RPC.
- **Raft Impact**: **Zero impact on Raft.** Both regions retain their local Raft leaders and continue serving scheduling requests, evaluations, and allocations for their local nodes.
- **RPC Impact**: Requests sent to Region A with target `-region=RegionB` fail with `structs.ErrNoRegionPath` ("No path to region").
- **Recovery**: When the WAN reconnects, Serf re-gossips server health, and cross-region RPC resumes automatically.

### 8.2 Inter-Datacenter Partition within a Single Region
- **Behavior**: Datacenter 1 is cut off from Datacenters 2 and 3 in the same region.
- **Raft Impact**:
  - Datacenters 2 and 3 maintain a majority quorum (e.g., 2 out of 3 servers or 3 out of 5 servers). They elect/maintain a leader and continue operating normally.
  - Datacenter 1 holds only a minority of servers. It cannot form a quorum, and write RPCs will block until `RPCHoldTimeout` and fail with `ErrNoLeader`. Read RPCs with `AllowStale=true` continue to succeed against local memory.

### 8.3 Autopilot Dead Server Cleanup
If a server in a datacenter is terminated or permanently partitioned:
- Serf marks the node failed, then reaped.
- `norca/norca/autopilot.go` monitors health metrics (`LastContactThreshold`, `ServerStabilizationTime`).
- If `CleanupDeadServers` is enabled, Autopilot automatically executes `raft.RemoveServer` to reduce the voter set, preventing dead nodes from holding back quorum advancement.

---

## 9. Summary for Operators and Architects

1. **Never attempt to stretch a single Nomad/Norca Raft cluster across wide-area geographic regions.** Raft is designed for low-latency, high-reliability boundaries.
2. **Distribute Raft servers across 3 Datacenters (AZs) within the same Region.** This provides automatic high availability against datacenter outages without sacrificing Raft consensus speed.
3. **Use Serf and Cross-Region RPC for global operations.** Norca handles multi-cloud and multi-region routing natively via its unified Serf mesh and Yamux-multiplexed RPC forwarder.
4. **Use `multiregion` blocks for global workloads.** Each region independently schedules and maintains the workload in its regional Raft store, while deployment watchers coordinate rolling upgrades across the globe.
