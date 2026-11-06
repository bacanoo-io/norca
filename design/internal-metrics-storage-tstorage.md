<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Embedded 30-Day Metrics Storage Architecture (`tstorage` & NATS Integration)

## 1. Executive Summary & Architectural Scope

While Norca's built-in horizontal autoscaler maintains a 5-minute in-memory ring buffer (`norca/norca/autoscaler/metrics.go`) for real-time scaling evaluations, operators and platform teams frequently require up to **30 days of historical resource telemetry** for Web UI trend charts (`/ui/jobs`, `/ui/scaling`, `/ui/quotas`), multi-hour autoscaling windows, post-incident forensics, and task group right-sizing—without standing up an external Prometheus, Thanos, or OpenObserve stack.

At the same time, **this internal store is explicitly not a replacement for enterprise APM or full-cardinality observability platforms**. Its sole purpose is bounded, self-contained cluster telemetry that **imposes zero load on Norca's critical control-plane subsystems** (Raft log, BoltDB, `MemDB`, and client-server RPC thread pools).

To achieve this, Norca adopts a **strict separation between the Control Plane and the Telemetry Plane**:
1. **Control Plane (TCP 4647)** remains exclusively dedicated to scheduling, cluster leadership, allocation state transitions, node heartbeats, and Raft consensus.
2. **Telemetry Plane (NATS Message Bus, TCP 4222)** offloads all high-frequency client metric telemetry, preventing control-plane worker pool contention, TCP head-of-line blocking, and node throttling.
3. **Embedded Time-Series Engine (`github.com/nakabonne/tstorage`)** ingests coalesced batches from the NATS stream into time-partitioned immutable disk segments using Facebook Gorilla (delta-of-delta timestamp + XOR floating-point) compression, `mmap`-backed zero-heap reads, and automatic 30-day retention pruning.

### Core Architectural Tenets:

1. **Strict Isolation from Raft & `MemDB`**: Time-series telemetry is never written to Raft (`raft.db`), never stored in `MemDB`, and never included in `state.snap.age` cluster backups. It resides in an isolated directory (`<data_dir>/tsdb/`) managed asynchronously.
2. **Telemetry Decoupling via the Embedded NATS Bus (No RPC Contention)**: Worker nodes publish high-frequency (10s) resource usage to the system bus embedded in the servers (`norca.telemetry.<region>.<node_id>`, bus port 4649), bypassing the control-plane RPC port (4647). Node heartbeats and scheduling updates are insulated from metric traffic.
3. **Non-Blocking Drop-on-Full Ingestion & Fallback Mode**: If NATS is temporarily unavailable or in minimal single-node dev environments, clients can fall back to non-blocking RPC enqueue (`select { case ch <- report: default: }`). In all cases, metrics never block client execution or control-plane RPC goroutines.
4. **Bounded Cardinality (Task-Group & Node Scope Only)**: To prevent time-series cardinality explosion from short-lived batch jobs or churned `alloc_id` UUIDs, the store indexes metrics strictly at **Task Group** (`namespace`, `job`, `group`), **NodePool/Datacenter**, and **Namespace Quota** granularity, enforced by a hard `max_series` ceiling (default `50,000`).
5. **Cross-Platform `mmap` (Linux, macOS, Windows)**: Upstream `nakabonne/tstorage` maps files on Unix and Windows; Norca's vendored package (`norca/internal/tstorage`) unmaps a partition before removing it, which Windows requires (see 1.1), preserving the Windows build and CI (`windows.bat`, `test.yml`).
6. **Dual-Bound Storage Lifecycle (30-Day Time Retention + Hard Disk Cap)**: Partitions are pruned automatically both by age (`retention = "720h"`, max 30 days) and by total disk footprint (`max_disk_size = "2GiB"`), guaranteeing that historical metrics can never exhaust server disk space.
7. **Full Toggleability & External APM Offload (`enabled = false`)**: The embedded `metrics_store` and client telemetry publisher are completely opt-in and toggleable. If an organization already operates an external monitoring infrastructure (e.g., Prometheus, VictoriaMetrics, OpenObserve, Datadog), both the server-side TSDB ingestion and client telemetry publishing can be disabled (`enabled = false`). When disabled, no TSDB directory (`<data_dir>/tsdb/`) is created, zero disk I/O occurs, zero background flusher goroutines or NATS telemetry consumers are spawned, and zero telemetry messages traverse NATS or RPC. Concurrently, Norca provides a standard `/v1/metrics?format=prometheus` pull endpoint so external collectors can scrape live metrics directly without requiring embedded `tstorage` at all.

```
+---------------------------------------------------------------------------------------------------------+
|                                    NORCA CLUSTER ARCHITECTURE                                           |
|                                                                                                         |
|  [WORKER NODES (CLIENTS)]                                                                               |
|                                                                                                         |
|  Node Agent (cgroups, OS stats, TaskGroupUsageReport)                                                   |
|       │                                                 │                                               |
|       │ 1. Lifecycle & Heartbeats (TCP 4647)            │ 2. Telemetry Stream (NATS TCP 4222)           |
|       ▼                                                 ▼                                               |
|  ===========================================      ====================================================  |
|  [CRITICAL CONTROL PLANE (TCP 4647)]              [TELEMETRY DATA PLANE (NATS BUS TCP 4222)]            |
|                                                                                                         |
|  Master Server RPC Port                               NATS Cluster / Supercluster                       |
|       │                                               Subject: norca.telemetry.<region>.<node_id>       |
|       ├──> Raft Consensus (raft.db)                                     │                               |
|       │    (Leader election, Job submissions)                           │                               |
|       │                                                                 ▼                               |
|       └──> MemDB State Store (Allocs, Nodes)          +----------------------------------------------+  |
|            [NO METRIC WRITES - ZERO CONTENTION]       | 3. NATS Ingestion Consumer (Queue Group)     |  |
|                                                       |    (norca/norca/metricsstore/nats_ingest.go) |  |
|                                                       +----------------------+-----------------------+  |
|                                                                              │                          |
|                                                       ┌──────────────────────┴──────────────────────┐   |
|                                                       │ Live 10s Stream                             │   |
|                                                       ▼                                             ▼   |
|                                         +----------------------------+  +----------------------------+  |
|                                         | Autoscaler 5m RAM Buffer   |  | 4. 30s Downsampling Batch  |  |
|                                         | (Immediate reactive scale) |  |    Accumulator             |  |
|                                         +----------------------------+  +--------------+-------------+  |
|                                                                                        │                |
|                                                                                        ▼                |
|  =====================================================================================================  |
|  [ISOLATED ASYNCHRONOUS TSDB SUBSYSTEM (`norca/norca/metricsstore/` -> `<data_dir>/tsdb/`)]            |
|                                                                                                         |
|  +---------------------------------------------------------------------------------------------------+  |
|  | 5. Embedded `tstorage` Engine                                                                     |  |
|  |                                                                                                   |  |
|  |    +----------------------------------+          Flush every `partition_duration` (6h)             |  |
|  |    | Active Memory Partition (Hot)    | -----------------------------------------+                |  |
|  |    | + Sequential Write-Ahead Log     |                                          │                |  |
|  |    +----------------------------------+                                          ▼                |  |
|  |                                                  +-------------------------------------------+    |  |
|  |    +----------------------------------+          | Immutable Disk Partitions (Cold, `mmap`)  |    |  |
|  |    | Retention & Quota Enforcer       | -------> | p-1730000000-1730021600/                  |    |  |
|  |    | - Max Age:  30 days (`720h`)     |          |   ├── data (Gorilla Delta-of-Delta + XOR) |    |  |
|  |    | - Max Disk: `2GiB` (oldest first)|          |   └── meta.json (Min/Max TS, Offsets)     |    |  |
|  |    +----------------------------------+          +-------------------------------------------+    |  |
|  +--------------------------------------------------+------------------------------------------------+  |
|                                                     │ Zero-copy `mmap` `Select()` + LTTB Downsampling    |
|                                                     ▼                                                   |
|  +---------------------------------------------------------------------------------------------------+  |
|  | 6. Internal Query Consumers                                                                       |  |
|  |    - Web UI Historical Charts (`GET /v1/metrics/history`, ECharts 1h / 24h / 7d / 30d)            |  |
|  |    - Autoscaler Warmup Seeding (pre-warms 5m RAM ring buffer after leader election/restart)       |  |
|  |    - Long-Window Autoscaler Checks (`query_window = "1h" .. "24h"`)                               |  |
|  |    - Task Group Right-Sizing Inspector (`p95` / `max` 30-day CPU & Memory vs. Requested)          |  |
|  +---------------------------------------------------------------------------------------------------+  |
+---------------------------------------------------------------------------------------------------------+
```

### 1.1 Decisions (2026-10-30)

These decisions take precedence over anything below that contradicts them.
Confirmed: the metrics store and the bus are enabled by default; the first
metric set is that of point 4.

1. **Transport: the embedded NATS system bus** (`embedded-nats-system-bus.md`), not an
   external NATS. The servers embed nats-server; clients publish their usage
   report to `norca.telemetry.<region>.<node_id>` on the bus, authenticated with
   their node secret and allowed to publish on their own subject only. The
   piggyback on `Node.UpdateAlloc` stays as the fallback while the bus is
   disabled or unreachable.
2. **Every server stores everything.** The reports go to the JetStream stream
   `NORCA_TELEMETRY_<region>` (`max_age = 1h`); every server reads it with its
   own durable consumer `norca-usage-<server_id>` into its local tstorage. No
   queue group: after a leader change the new leader has the full history, and
   queries are answered by the server that receives them (no forwarding to the
   leader). A server that was down catches up from the stream (up to 1h).
3. **Stored by every server, enabled by default** (`metrics_store.enabled = true`);
   reporting by clients no longer depends on the autoscaler being enabled.
4. **First metric set:** Family 1 (task group) with the client additions
   throttling, page cache and OOM kills; `allocs.running` / `allocs.queued`
   computed by the servers from the state; Families 3 and 4 sampled by the
   servers from the state. Family 2 (custom metrics) waits for a source
   (`dynamic-application-autoscaling.md`).
5. **Corrections to the text below:**
   - Upstream tstorage v0.3.6 already maps files on Windows
     (`internal/syscall/mmap_windows.go`); what Norca must change is the
     partition removal: `diskPartition.clean()` calls `os.RemoveAll` without
     unmapping, which fails on Windows and leaks the mapping elsewhere.
   - tstorage has no disk cap, no series limit and no series listing:
     `max_disk_size`, `max_series` and the series index (for the UI pickers and
     `Metrics.Query` without exact labels) are part of the Norca layer.
   - tstorage keeps two writable memory partitions (`writablePartitionsNum = 2`),
     so the heap holds up to two `partition_duration` periods (about twice the
     estimate of 3.4).
   - tstorage's `internal/cgroup` (worker limit) is dropped in the vendored copy.
   - Configuration: the agent already has a top-level `telemetry {}` block; the
     client block is `client { usage { ... } }` and the transport is chosen
     automatically (bus when available, else RPC). See 7.1.
   - `/ui/dashboard` does not exist; the cluster overview charts go to the
     Servers/Clients overview (to be decided with the UI work).

### 1.2 Implementation Status (2026-10-31, phase 4b)

Done:
- `norca/internal/tstorage`: tstorage v0.3.6 copied (Apache-2.0, listed as a
  source copy in THIRD_PARTY_NOTICES, REUSE annotation). Changes: a disk
  partition is unmapped before its files are removed and when the storage
  closes; selects hold a read lock against the unmap (a select racing a
  removal read unmapped memory); flushes are serialized (the background flush
  could race Close); no cgroup package; `Stats` and
  `RemoveOldestDiskPartition` for the disk cap.
- `norca/norca/metricsstore`: the window per sample interval keyed by the
  publish time of the report (catch-up after a restart lands at the right
  time), rows written in time order (tstorage finds out-of-order points of a
  memory partition only after the flush), late reports of a written window
  dropped, series index in `series.json`, `max_series`, `max_disk_size`,
  downsampled `Query`.
- Every server opens the store with the bus (`<DataDir>/tsdb`) and feeds it
  from `norca-usage-<server_id>`; the heartbeat asks for usage when the
  autoscaler or the store needs it (10s for the store alone).
- `server { metrics_store { ... } }`, `MetricsStore.History`
  (`GET /v1/metrics/history`, read-job), `Operator.MetricsStoreStatus`
  (`GET /v1/operator/metrics-store`, operator read; not `/status`, like
  `/v1/operator/bus`), `api.MetricsStore().History`,
  `norca operator metrics-store status`.
- First metric set: `norca.group.cpu`, `cpu_mhz`, `memory`, `memory_mb`
  (`.avg`/`.max`), from the report as it is.

Phase 4c (2026-10-31): `cpu_throttled` (increase of the throttled time
between two measurements; no nr_periods in the client stats),
`memory_cache_mb`, `oom_kills` (task events with oom_killed, by event time;
a counter: sum per window and step), `allocs_running`/`allocs_queued` sampled
by every server from the job summaries; the warmup seeding of 5.2
(`UsageSince` incl. pending windows; seeds older than the activation only;
`started` moved back to the oldest seed).

Phase 4d-4f (2026-10-31): Families 3 and 4 as labelled gauge series
(`norca.cluster.*` by dc and pool, autopilot on the leader only,
`norca.quota.*` allocated and used), `/v1/metrics/series`,
`/v1/metrics/summary` (mean, p95, max against the job's allocation),
long-window autoscaler queries (query_window beyond metrics_window, and the
warmup, read from the store; the leader's window answers when the store has
no points), `client { usage { enabled } }`.

UI (2026-10-31): task group page (CPU with throttling, memory with page
cache and OOM marks, right-sizing table), scaling policy page (usage
against running allocations), dashboard capacity card (allocated against
used by dc and pool); ranges 1h/24h/7d/30d. UI tests run from a test build
made in a Linux container (ember-auto-import breaks on Windows paths) with
the Windows Chrome: `ember test --path dist-test-linux`.

---

## 2. Why `nakabonne/tstorage`? Engine Internals & Clean-Room Adaptation

### 2.1 License & Dependency Compliance
`github.com/nakabonne/tstorage` (`v0.3.6`) is licensed under the **Apache License 2.0**, written in ~1,500 lines of pure Go with zero external runtime dependencies beyond the Go standard library. In accordance with Norca's clean-room and licensing rules:
- The engine is vendored as an internal package (`norca/internal/tstorage`) preserving the original Apache-2.0 copyright headers and documented in `THIRD_PARTY_NOTICES.md`.
- No CGO, B+Tree (`bbolt`), or LSM-tree (`badger`/`rocksdb`) write amplification is introduced.

### 2.2 Storage Format & Gorilla Compression Mechanics

`tstorage` organizes time-series data into chronological partitions (`partition` interface) split between an active **Memory Partition** (backed by an append-only Write-Ahead Log) and immutable **Disk Partitions**:

```
<data_dir>/tsdb/
├── wal/
│   └── 00000001.wal                 # Append-only crash recovery log for active memory partition
├── p-1761523200-1761544800/         # Closed 6-hour partition #1
│   ├── data                         # Gorilla-compressed binary series blocks (read via mmap)
│   └── meta.json                    # JSON index: minTimestamp, maxTimestamp, metric offsets & point counts
├── p-1761544800-1761566400/         # Closed 6-hour partition #2
│   ├── data
│   └── meta.json
└── ...                              # Up to 120 partitions (120 x 6h = 30 days)
```

When a memory partition spans `partition_duration` (default `6h`), `tstorage` flushes each metric series sequentially into the partition's `data` file using **Facebook Gorilla compression** (`internal/tstorage/encoding.go`):

1. **Timestamps (Delta-of-Delta Bit Packing)**:
   - Given consecutive timestamps $t_{i-2}, t_{i-1}, t_i$ sampled at a regular cadence (`sample_interval` $= 30\text{s}$), the first delta is $\Delta_i = t_i - t_{i-1}$ and the delta-of-delta is:
     $$D_i = (t_i - t_{i-1}) - (t_{i-1} - t_{i-2})$$
   - When intervals are uniform, $D_i = 0$, which is encoded as a **single `0` bit**. Small jitter fits in 9, 12, or 16 bits instead of 64 bits.
2. **Floating-Point Values (XOR Bit Packing)**:
   - Each IEEE-754 `float64` value $v_i$ is XORed with the prior value $v_{i-1}$: $X_i = v_i \oplus v_{i-1}$.
   - If $X_i = 0$ (unchanged metric, common for idle task groups or steady replica counts), a **single `0` bit** is written.
   - Otherwise, only the meaningful bits between leading and trailing zeros of $X_i$ are written, reducing typical 8-byte floats to **1.3–2.1 bytes per point**.

### 2.3 Cross-Platform `mmap` Adaptation (`mmap_unix.go` vs. `mmap_windows.go`)

Upstream `nakabonne/tstorage` calls `syscall.Mmap` and `syscall.Munmap` directly inside `internal/syscall/mmap.go`. While this compiles on Linux and Darwin, **Go's standard `syscall` package on Windows does not expose `syscall.Mmap`**, which would break Norca's mandatory Windows build (`windows.bat` and `test.yml`).

Norca resolves this cleanly in `norca/internal/tstorage/internal/syscall/` using build-tagged files:

| File | Build Constraint | Implementation |
| :--- | :--- | :--- |
| **`mmap_unix.go`** | `//go:build unix` | Delegates to `unix.Mmap(fd, 0, length, unix.PROT_READ, unix.MAP_SHARED)` and `unix.Munmap(b)`. |
| **`mmap_windows.go`** | `//go:build windows` | Uses `windows.CreateFileMapping(handle, nil, windows.PAGE_READONLY, 0, 0, nil)` followed by `windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, uintptr(length))`, and unmaps via `windows.UnmapViewOfFile(addr)` + `windows.CloseHandle(h)`. |

> [!IMPORTANT]
> **Windows File Deletion Semantics (`Unmap` Before `RemoveAll`)**: On Windows, a file with an active `MapViewOfFile` handle cannot be deleted (`ERROR_ACCESS_DENIED`). When `tstorage` prunes an expired disk partition directory (`p-<min>-<max>`), the adapted `diskPartition.clean()` explicitly unmaps the `data` slice and closes the file descriptor *before* calling `os.RemoveAll(dirPath)`.

---

## 3. Data Model, Metric Catalog & Field Semantics

### 3.1 NATS Telemetry Subject Hierarchy & Payload

Client nodes publish telemetry asynchronously over the embedded bus without contacting the Norca RPC port. One message per node and interval, on the node's own subject (the only one it may publish on):

```
norca.telemetry.<region>.<node_id>
```

The message carries the usage of every task group running on the node (the `TaskGroupUsageReport` of today, extended); the namespace, job and group are fields of each entry, not subject tokens.

#### Message Payload (`TaskGroupUsageReport`):
The payload is binary-packed (or compact JSON) matching `structs.TaskGroupUsageReport`:
```json
{
  "Region": "aws-eu",
  "NodeID": "c3d2e1a0-...",
  "Namespace": "default",
  "JobID": "web-api",
  "TaskGroup": "server",
  "Timestamp": 1761523200,
  "AllocsRunning": 3,
  "AllocsQueued": 0,
  "CPUMHzAvg": 450.5,
  "CPUMHzMax": 820.0,
  "CPUThrottledRatio": 2.4,
  "MemoryWorkingSetMB": 248.0,
  "MemoryCacheMB": 64.0,
  "MemoryMBMax": 312.0,
  "OOMKills": 0,
  "Custom": {
    "requests_per_sec": 142.5
  }
}
```

---

### 3.2 Comprehensive Metric Catalog (Prometheus & Grafana Aligned)

In `tstorage`, a unique time series is identified by combining `Metric` and sorted `[]tstorage.Label` (`marshalMetricName`). To capture critical operational and scaling insights while preventing cardinality explosion, Norca stores four canonical metric families:

#### Family 1: Task Group Resource Telemetry (Workload Performance & Autoscaling)

| Metric Name (`tstorage.Row.Metric`) | Labels (`[]tstorage.Label`) | Unit | Field Semantics & Formulas | Primary Consumer |
| :--- | :--- | :--- | :--- | :--- |
| **`norca.group.cpu.avg`** | `ns`, `job`, `group` | `%` (0–100%) | Average CPU utilization relative to allocated CPU quota. | Web UI Charts, Autoscaler |
| **`norca.group.cpu.max`** | `ns`, `job`, `group` | `%` (0–100%) | Peak single-sample CPU utilization during the 30s window. | Right-Sizing, Burst Detection |
| **`norca.group.cpu_mhz.avg`** | `ns`, `job`, `group` | `MHz` | Aggregate raw CPU clock frequency consumed across running allocs. | Capacity Planning |
| **`norca.group.cpu_mhz.max`** | `ns`, `job`, `group` | `MHz` | Peak instantaneous CPU clock frequency consumed in window. | Headroom Calculation |
| **`norca.group.cpu.throttled_ratio`** | `ns`, `job`, `group` | `%` (0–100%) | CFS quota throttling ratio: $\frac{\text{throttled\_periods}}{\text{total\_periods}} \times 100$. Indicates CPU starvation even when utilization is capped at 100%. | **Autoscaler Scale-Out Trigger** |
| **`norca.group.memory.working_set`** | `ns`, `job`, `group` | `Bytes` / `MiB` | **True active memory**: $\text{memory\_usage} - \text{memory\_cache}$. Excludes reclaimable OS page cache. | **Vertical Autoscaler, Memory Sizing** |
| **`norca.group.memory.cache`** | `ns`, `job`, `group` | `Bytes` / `MiB` | Inactive/reclaimable page cache and buffer memory. | Web UI Memory Breakdown |
| **`norca.group.memory.max`** | `ns`, `job`, `group` | `Bytes` / `MiB` | Peak memory observed during the 30s window. | Peak Forensics |
| **`norca.group.memory.utilization`**| `ns`, `job`, `group` | `%` (0–100%) | $\frac{\text{memory.working\_set}}{\text{allocated\_memory}} \times 100$. | Autoscaler Scale-Out, Quota Check |
| **`norca.group.oom_kills`** | `ns`, `job`, `group` | `Count` | Cumulative Linux cgroup OOM killer terminations within window. | Web UI Event Markers, Forensic Alert |
| **`norca.group.allocs.running`** | `ns`, `job`, `group` | `Count` | Active running replica count. | Replica Timeline Overlay |
| **`norca.group.allocs.queued`** | `ns`, `job`, `group` | `Count` | Blocked or queued allocations waiting for cluster capacity. | Cluster Scale-Up Trigger |

#### Family 2: Custom Application & Ingress Metrics (APM / Scale Triggers)

| Metric Name (`tstorage.Row.Metric`) | Labels (`[]tstorage.Label`) | Unit | Description |
| :--- | :--- | :--- | :--- |
| **`norca.group.custom.<name>.avg`** | `ns`, `job`, `group` | Float | Generic user-defined app metrics (e.g. `qps`, `active_conns`, `nats_lag`, `p99_latency`) up to 16 per group. |
| **`norca.group.custom.<name>.max`** | `ns`, `job`, `group` | Float | Peak user metric in window. |

#### Family 3: Cluster & Node Pool Capacity (Headroom & Overcommit)

Matches the top-level KPI metrics from the production Grafana dashboard:

| Metric Name (`tstorage.Row.Metric`) | Labels (`[]tstorage.Label`) | Unit | Description |
| :--- | :--- | :--- | :--- |
| **`norca.cluster.cpu.allocated_ratio`** | `region`, `dc`, `pool` | `%` (0–100%) | Cluster/pool CPU allocated capacity vs. total hardware capacity. |
| **`norca.cluster.cpu.actual_used_ratio`** | `region`, `dc`, `pool` | `%` (0–100%) | Actual hardware CPU utilized vs. total hardware capacity (**Overcommit indicator**). |
| **`norca.cluster.memory.allocated_ratio`**| `region`, `dc`, `pool` | `%` (0–100%) | Cluster/pool Memory allocated capacity vs. total hardware capacity. |
| **`norca.cluster.memory.actual_used_ratio`**| `region`, `dc`, `pool` | `%` (0–100%) | Actual working set memory utilized vs. total hardware capacity. |
| **`norca.cluster.disk.allocated_ratio`** | `region`, `dc` | `%` (0–100%) | Ephemeral / root disk space allocation percentage. |
| **`norca.cluster.nodes.ready_count`** | `region`, `pool` | `Count` | Count of active, schedulable (eligible) client nodes. |
| **`norca.cluster.autopilot.healthy`** | `region` | `0` or `1` | Raft cluster Autopilot health status (1 = Healthy, 0 = Degraded). |
| **`norca.cluster.autopilot.failure_tolerance`** | `region` | `Count` | Server loss failure tolerance before quorum degradation. |

#### Family 4: Regional Namespace Quotas

| Metric Name (`tstorage.Row.Metric`) | Labels (`[]tstorage.Label`) | Unit | Description |
| :--- | :--- | :--- | :--- |
| **`norca.quota.cpu_mhz.used`** | `quota`, `region` | `MHz` | Aggregate CPU consumed across namespaces bound to quota. |
| **`norca.quota.memory_mb.used`** | `quota`, `region` | `MiB` | Aggregate memory working set consumed across quota. |

---

### 3.3 Cardinality Defense: Avoiding the Prometheus High-Cardinality Trap

In traditional Prometheus installations (as shown in standard dashboards), metric series carry `alloc_id="<uuid>"` labels. In dynamic clusters with batch jobs, canary deployments, and autoscaling, allocations churn continuously:
- If a cluster runs 500 long-lived task groups, each redeployed twice daily over 30 days, indexing per `alloc_id` creates:
  $$500 \text{ groups} \times 2 \text{ deploys/day} \times 30 \text{ days} \times 5 \text{ allocs} \times 10 \text{ metrics} = 1,500,000 \text{ series!}$$
- This cardinality explosion causes memory exhaustion, GC pressure, and sluggish query responses.

**Norca's Architectural Solution**:
1. **Long-Term Series Boundary (30 Days)**: Series in `tstorage` are indexed strictly at `(namespace, job, group)` and `(region, dc, pool)`. For a 500-group cluster, the series count is constant:
   $$N_{\text{series}} = (500 \times 12) + (10 \text{ pools} \times 6) \approx 6,060 \text{ series} \ll 50,000 \text{ hard cap}$$
2. **Short-Term Allocation Detail (5 Minutes)**: Individual allocation-level breakdowns are retained in client local memory and streamed live over NATS or the 5-minute RAM buffer, but are **rolled up** into task-group summaries before persistent disk ingestion.

---

### 3.4 30-Day Disk & Memory Footprint Calculation

Let us calculate the exact storage cost for a production cluster running **$G = 500$ active task groups**, each recording **$M = 12$ metrics** ($N_{\text{series}} = 6,000$ series), sampled at $\Delta t = 30\text{s}$ over **$T_{\text{ret}} = 30\text{ days}$** ($2,592,000\text{ seconds}$):

1. **Samples per Series over 30 Days**:
   $$N_{\text{points/series}} = \frac{30 \times 86,400\text{ s}}{30\text{ s}} = 86,400 \text{ points}$$
2. **Total Data Points Across Cluster**:
   $$N_{\text{total}} = 6,000 \times 86,400 = 518,400,000 \text{ points}$$
3. **Disk Usage with Gorilla Compression ($\approx 1.85\text{ bytes/point}$ including `meta.json` offsets)**:
   $$\text{Disk}_{30\text{d}} = 518.4 \times 10^6 \times 1.85\text{ B} \approx 959\text{ MB}$$
   Even with 12 metrics per group across 500 groups, the entire 30-day compressed database fits in **less than 1 GB of disk space**, well below the default `max_disk_size = "2GiB"` limit.
4. **Go Heap Memory Footprint**:
   - Only the single active 6-hour memory partition (`720` samples per series) resides on the Go heap:
     $$\text{Heap}_{\text{active}} \approx 6,000 \text{ series} \times 720 \text{ points} \times 16\text{ B} \approx 69\text{ MB}$$
   - All 119 historical 6-hour partitions are read-only `mmap` regions managed by the OS page cache, adding **zero GC scan pressure** to the Norca process.

---

## 4. Decoupled Ingestion Pipeline: NATS Telemetry Bus & Batch Downsampling

### 4.1 Why Control-Plane RPC (`Node.UpdateAlloc`) Must Be Avoided for High-Scale Metrics

In standard clusters with 1,000+ nodes, having every worker node send 10-second metric reports over `Node.UpdateAlloc` RPC introduces severe control-plane overhead:
- **Connection & Thread Contention**: Master servers spend significant CPU and goroutine pool capacity unmarshaling RPC payloads, reading authentication tokens, and verifying client node identities for pure telemetry.
- **Raft & Heartbeat Starvation**: Critical cluster state transitions (node heartbeats, task failures, placement plans) share the same TCP connection multiplexer as metric reports. Under network saturation or high churn, telemetry can delay node heartbeats, triggering false node failure detections.
- **Strict Separation of Concerns**: Moving telemetry to NATS isolates the **Control Plane** (scheduling/Raft on TCP 4647) from the **Telemetry Plane** (metrics on NATS TCP 4222).

### 4.2 NATS Ingestion Architecture & Queue Groups

```
+------------------+         +------------------+         +------------------+
| Client Node 1    |         | Client Node 2    |         | Client Node N    |
+--------+---------+         +--------+---------+         +--------+---------+
         │ Publish (10s)              │ Publish (10s)              │ Publish (10s)
         ▼                            ▼                            ▼
+────────────────────────────────────────────────────────────────────────────+
|                      NATS STREAM: norca.telemetry.>                        |
+─────────────────────────────────────+──────────────────────────────────────+
                                      │
              ┌───────────────────────┴───────────────────────┐
              ▼ Queue Group: "norca-tsdb-ingester"            ▼
+─────────────────────────────+             +─────────────────────────────+
| MetricsStore Ingester Pod 1 |             | MetricsStore Ingester Pod 2 |
| - Window Acc (30s batch)    |             | (Load-balanced standby)     |
| - storage.InsertRows()      |             +─────────────────────────────+
+--------------+--------------+
               │
               ▼
+─────────────────────────────+
| Embedded tstorage Engine    |
| (<data_dir>/tsdb/)          |
+-----------------------------+
```

1. **Client Publisher**: Worker nodes initialize a lightweight NATS client (`client.telemetry.transport = "nats"`). Every 10s, the local cgroup collector formats the `TaskGroupUsageReport` and fires an asynchronous, non-blocking `nc.Publish("norca.telemetry." + key, payload)`.
2. **One durable consumer per server**: The reports are kept in the JetStream stream `NORCA_TELEMETRY_<region>` (`max_age = 1h`). Every server reads it with its own durable consumer `norca-usage-<server_id>`, so every server stores every report (no queue group: a queue group would leave no server with the full history).
3. **Dual Ingestion Mode (Fallback for Standalone/Dev)**:
   - **Primary**: NATS Telemetry stream.
   - **Fallback**: If `client.telemetry.transport = "rpc"`, nodes fall back to the legacy `Node.UpdateAlloc` channel enqueue (`autoscalerRecordUsage`). This ensures single-binary zero-dependency development clusters run out-of-the-box without requiring a separate NATS broker.

### 4.3 30-Second Window Coalescing Before `InsertRows`

Clients report every `10s` (`client_metrics_interval`) to keep the autoscaler's 5-minute ring buffer highly responsive. However, storing 30 days of history only requires `sample_interval = 30s` (or `60s`) resolution.

The `MetricsStore` background worker maintains a lightweight 30-second coalescing map `pending map[targetKey]*windowAcc`:
1. As `10s` reports arrive from NATS during the `30s` tick, `windowAcc` combines them using the sample-weighted formula:
   $$W \leftarrow W + S_g, \qquad \text{Sum}_m \leftarrow \text{Sum}_m + \text{Avg}_{g,m} \cdot S_g, \qquad \text{Max}_m \leftarrow \max(\text{Max}_m, \text{Max}_{g,m})$$
2. Every `sample_interval` (`30s`), the worker transforms all accumulated `windowAcc` entries into a single `[]tstorage.Row` batch with timestamp $t_{\text{tick}} = \text{now.Unix()}$ and executes one `m.storage.InsertRows(rows)` call.

---

## 5. High Availability, Leader Transitions & Autoscaler Warmup Seeding

### 5.1 Why Historical Metrics Are Not Replicated via Raft

Replicating metric values every 30 seconds through Raft would generate tens of thousands of Raft log commits per day, bloating `raft.db`, triggering frequent Raft snapshots, and slowing down job scheduling commits. Therefore, `<data_dir>/tsdb/` is **server-local or dedicated shared storage**, completely off the Raft ledger.

### 5.2 Fast Autoscaler Ring-Buffer Warmup on Leader Restart / Failover

One of the key operational synergies between `tstorage` and the built-in autoscaler (`norca/norca/autoscaler/metrics.go`) is eliminating the 5-minute `ErrInsufficientData` cold-start window:

```
[Server Restarts or Re-Establishes Leadership]
                      │
                      ▼
     Does `<data_dir>/tsdb/` contain points
       within `[now - metrics_window, now]`?
                      │
          ┌───────────┴───────────┐
          │ (Yes)                 │ (No - New Leader without local history)
          ▼                       ▼
  [Seed `usageCollector`]   [Standard 5m Warmup via NATS Live Stream]
  - Backfills the 30 RAM    - Returns `ErrInsufficientData`
    buckets from `tstorage`   until `metrics_window` (5m)
  - Marks `c.started` back    fills from live NATS client stream
    to `oldestPointTime`
```

When a leader restarts (e.g., during a rolling `norca` binary upgrade or brief network blip), `s.metricsStore.SeedAutoscaler(s.autoscaler, now)` reads the last 5 minutes of `norca.group.*` points from `tstorage` (`mmap` or recovered WAL) and populates the 30-bucket `usageCollector` in `< 2 ms`, allowing the autoscaler to resume scaling immediately without waiting 5 minutes.

### 5.3 Cross-Server Query Routing (`forwardLeader`)

Every server holds the full history (1.1), so `Metrics.Query` is answered by the server that receives it, without forwarding to the leader; only requests for another region are forwarded to that region. A server that was down for more than the stream's `max_age` has a gap in its history for that time; queries may name a server (`?server=`) to read from another one.

---

## 6. Query Engine, Web UI Visualizations & Autoscaling Synergy

### 6.1 Downsampled Range Queries (`Metrics.Query` & `GET /v1/metrics/history`)

Querying 30 days of raw 30-second data for a task group yields $86,400$ points per metric—far too many points to render in a browser chart (which typically has $800\text{–}1,600$ horizontal pixels).

When a caller requests `[from, to]` with a target `step` (or `max_points`, default `360`), the server reads the raw `[]*tstorage.DataPoint` slice directly from the `mmap` partitions via `storage.Select(metric, labels, from, to)` and downsamples in-place before serialization:

$$\text{step} = \max\left(\Delta t_{\text{raw}},\; \left\lceil \frac{t_{\text{to}} - t_{\text{from}}}{N_{\max}} \right\rceil\right)$$

For each output bucket $k \in [0, N_{\max})$ covering $[t_{\text{from}} + k \cdot \text{step},\; t_{\text{from}} + (k+1) \cdot \text{step})$:
- For `.avg` metrics, the bucket value is the arithmetic mean of points in bucket $k$.
- For `.max` metrics, the bucket value is the maximum of points in bucket $k$.

---

### 6.2 Web UI Dashboards & Chart UX (Ember.js & ECharts)

Using the canonical metric schema, Norca's built-in Web UI renders clear, intuitive visualizations across three key areas:

#### 1. Task Group View (`/ui/jobs/:id`)
- **CPU Utilization & Throttling Card (1h / 24h / 7d / 30d)**:
  - Solid blue line: Average CPU usage (`norca.group.cpu_mhz.avg`).
  - Shaded light-blue area: Peak burst headroom (`norca.group.cpu_mhz.max`).
  - Dashed red threshold: Configured CPU allocation limit.
  - Orange warning badge on timeline: Periods where `cpu.throttled_ratio > 5%`.
- **Memory Profile Card (Working Set vs. Cache)**:
  - Stacked area chart displaying:
    - **Blue area**: Active Working Set (`norca.group.memory.working_set`).
    - **Gray area**: Page Cache & buffers (`norca.group.memory.cache`).
  - Red vertical event pins: Exact timestamps where `oom_kills > 0`.
- **Right-Sizing Advisor Card**:
  - Automatically compares 30-day $P_{95}$ consumption against configured allocation:
    $$\text{Efficiency}_{\text{CPU}} = \frac{P_{95}(\text{cpu\_mhz})}{R_{\text{allocated\_cpu\_mhz}}}, \qquad \text{Headroom}_{\text{Mem}} = \frac{R_{\text{allocated\_memory\_mb}} - \max_{30\text{d}}(\text{memory\_mb})}{R_{\text{allocated\_memory\_mb}}}$$
  - Surfaces actionable advice (e.g., *"Allocated: 2,000 MHz, 30d P95: 450 MHz $\to$ Recommended: 600 MHz (Potential 70% cost reduction)"*).

#### 2. Horizontal Autoscaler View (`/ui/scaling`)
- **Dual-Axis Correlation Chart**:
  - Left Y-Axis: Target Metric utilization (`cpu.avg`, `memory.utilization`, or custom `qps`) with target threshold line (e.g., Target: 80%).
  - Right Y-Axis (Stepped green line): Active Replica Count (`norca.group.allocs.running`: $1 \to 3 \to 5$).
  - Proves at a glance whether scale-out events successfully relieved workload pressure.

#### 3. Cluster & Node Pool Overview (`/ui/dashboard`)
- **Capacity & Overcommit Gauges**:
  - 3 Circular / Bar gauges modeled after the production Grafana dashboard:
    - **CPU Allocation vs. Real Consumption**: e.g., 82% Allocated, but only 34% Actually Consumed.
    - **Memory Allocation vs. Real Consumption**: e.g., 75% Allocated, 41% Working Set.
    - **Ephemeral Disk Utilization**.
- **Autopilot & Resiliency Status**:
  - Cluster Health: `Healthy` / `Degraded`.
  - Fault Tolerance: Count of server failures survivable before quorum loss.

#### 4. Behavior When Internal Metrics Storage Is Disabled (`metrics_store.enabled = false`)
When an operator opts to disable internal metrics storage in favor of an external APM stack (e.g., Prometheus / VictoriaMetrics / OpenObserve):
- **Graceful UI Fallback**: The Web UI charts (`/ui/jobs/:id`, `/ui/scaling`) do not fail or show blank errors. Instead, an informative status callout is rendered:
  > *"Internal historical telemetry is disabled (`metrics_store.enabled = false`). Real-time and historical trends are collected by your external monitoring stack (Prometheus / Grafana). Real-time allocation heartbeats remain available."*
- **Autoscaler Resilience**: Short-window autoscaling continues unhindered via the in-memory ring buffer or can be directed to the external Prometheus APM driver (`usage_apm.go` with `source = "prometheus"`).
- **HTTP Endpoint Transparency**: `GET /v1/operator/metrics-store/status` returns `{"enabled": false}`, and `GET /v1/metrics/history` returns an empty point array with HTTP 200 and `"enabled": false`, preventing client-side exceptions.

---

### 6.3 Autoscaling & APM Synergy

1. **Short-Window Reactive Scaling ($\le 5\text{m}$)**:
   - Handled directly by `Autoscaler` RAM ring buffer (`norca/norca/autoscaler/metrics.go`).
2. **Long-Window Smooth Scaling ($> 5\text{m}$ to $24\text{h}$)**:
   - In `norca/norca/autoscaler/usage_apm.go`, queries spanning multi-hour windows automatically query `MetricsStore.QueryAggregate()`. Prevents erratic scaling for bursty batch workflows.
3. **Throttling-Driven Scaling**:
   - Autoscaler policies can target `cpu.throttled_ratio` as a trigger condition, scaling out replicas even when average CPU utilization appears normal due to kernel CFS clamping.

---

### 6.4 ACL Authorization Model

Historical metrics respect Norca's existing namespace and node ACL boundaries:
- **Task Group Metrics (`ns`, `job`, `group`)**: Requires `read-job` capability in the target `namespace`.
- **Namespace Quota Metrics (`quota`)**: Requires `quota:read` or `read-job` in a namespace bound to that quota.
- **Datacenter / Node Pool Metrics (`dc`, `pool`)**: Requires `node:read`.

---

## 7. Configuration & API Reference

### 7.1 Server & Client Configuration (`norca.hcl`)

#### Default: Embedded 30-Day Metrics Enabled (NATS Telemetry Plane)
```hcl
server {
  enabled = true

  # Built-in 30-day historical metrics storage (backed by adapted tstorage + NATS)
  # Toggle enabled = false if using an external monitoring stack (Prometheus / VictoriaMetrics / Datadog)
  metrics_store {
    enabled            = true         # Set false to completely disable TSDB, WAL, and usage ingestion
    data_dir           = "tsdb"       # Resolves to <data_dir>/tsdb (isolated from raft/)
    retention          = "720h"       # Maximum retention: 720h (30 days)
    partition_duration = "6h"         # Flush immutable Gorilla-compressed mmap partitions every 6h
    sample_interval    = "30s"        # Coalescing interval for disk writes
    max_disk_size      = "2GiB"       # Hard disk ceiling; oldest p-* partition pruned if exceeded
    max_series         = 50000        # Cardinality cap across active memory partition
    wal_buffered_size  = 4096         # WAL flush buffer size in bytes (0 = sync every batch)
  }
}

client {
  enabled = true

  # Usage reports (autoscaler and metrics store). Sent over the embedded bus
  # when the servers offer it, else piggybacked on Node.UpdateAlloc.
  usage {
    enabled = true                    # Set false to send no usage at all
  }
}
```

#### External APM Only Profile (`enabled = false`, Zero Overhead)
For clusters where operators prefer external Prometheus, VictoriaMetrics, or Datadog agents:

```hcl
server {
  enabled = true

  # Internal TSDB disabled: zero disk allocations, zero background flusher threads, zero NATS consumer load
  metrics_store {
    enabled = false
  }

}

# Standard Prometheus exposition endpoint (/v1/metrics?format=prometheus);
# the agent-level telemetry block, unchanged
telemetry {
  prometheus_metrics         = true
  publish_allocation_metrics = true
  publish_node_metrics       = true
}

client {
  enabled = true

  # No usage reports: zero bus/RPC telemetry traffic (the autoscaler's
  # built-in metrics are then unavailable too)
  usage {
    enabled = false
  }
}
```

### 7.2 HTTP API Endpoints

| Method & Path | ACL Required | Query Parameters | Description |
| :--- | :--- | :--- | :--- |
| `GET /v1/metrics/history` | `read-job` (NS) or `node:read` | `namespace`, `job`, `group`, `metric`, `from`, `to`, `step` / `max_points` | Returns downsampled time-series points `[{"timestamp": 1761523200, "avg": 42.1, "max": 68.4}, ...]` for Web UI ECharts and CLI inspection. |
| `GET /v1/metrics/summary` | `read-job` (NS) | `namespace`, `job`, `group`, `window` (default `720h`) | Returns 30-day `avg`, `p95`, `max` usage vs. allocated resources for right-sizing analysis. |
| `GET /v1/operator/metrics-store/status` | `operator:read` | — | Reports active partition count, oldest/newest timestamp, disk bytes used vs. `max_disk_size`, active series count vs. `max_series`, and dropped report count. |

---

## 8. Architectural Comparison & Boundary Summary

| Aspect | Autoscaler RAM Ring Buffer (`metrics.go`) | Embedded 30-Day Store (`tstorage` + NATS) | External APM (Prometheus / OpenObserve) |
| :--- | :--- | :--- | :--- |
| **Transport Plane** | Ingested via NATS or live stream | **Dedicated Telemetry Plane (NATS TCP 4222)** | HTTP / gRPC / Pushgateway |
| **Control-Plane Contention**| **Zero** (decoupled from RPC) | **Zero (Never touches TCP 4647 RPC in NATS mode)** | Zero |
| **Retention Window** | `5m` (30 buckets $\times$ `10s`) | **Up to `30d` (`720h`)** (`120` $\times$ `6h` partitions) | Months to Years (object storage backed) |
| **Storage Medium** | Pure Leader RAM (`0` disk I/O) | **`<data_dir>/tsdb/` (WAL + Gorilla `mmap`)** | External cluster / TSDB nodes |
| **Raft / `MemDB` Impact** | **Zero** | **Zero** | **Zero** |
| **Backup (`state.snap.age`)** | Excluded | **Excluded** (keeps DR snapshots small & fast) | Managed externally |
| **Granularity** | Task Group (`10s` resolution) | **Task Group, Quota, DC (`30s` resolution)** | Arbitrary per-allocation, per-task, per-endpoint labels |
| **Primary Purpose** | Sub-minute reactive scaling loop | **UI historical charts, long-window scaling, warmup seeding, 30d right-sizing** | Enterprise alerting, distributed tracing, high-cardinality PromQL |
| **Toggleability / External APM Offload** | Built-in fallback | **Fully toggleable (`enabled = false`) with zero disk / CPU overhead** | Standalone or parallel operation via `/v1/metrics?format=prometheus` |
