<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Dynamic Application Autoscaling on Native Nomad Metrics (White-Room Design)

## 1. Executive Summary & Architectural Motivation

Traditional horizontal workload autoscaling in Nomad (`nomad-autoscaler`) relies on either an external time-series database (Prometheus, Datadog) or an expensive pull-based polling loop where an external process queries `/v1/client/allocation/:id/stats` across every client node on every evaluation tick. Furthermore, infrastructure-level CPU and memory metrics are often **lagging indicators** for modern services: an I/O-bound API server waiting on upstream locks or database connections may saturate its worker pool or request queue while CPU utilization remains below 20%.

Norca already replaces the external infrastructure polling loop with a **client-pushed, heartbeat-negotiated, zero-Raft-I/O metric pipeline** (`norca/client/taskgroup_usage.go` and `norca/norca/autoscaler/metrics.go`) for `nomad.cpu` and `nomad.memory`. This document specifies the clean-room (white-room) architecture and extension design for **Dynamic Application Autoscaling using Native Nomad Metrics**—enabling workloads to scale on L4/L7 traffic, active connections, in-flight requests, latency percentiles, and shared queue backlogs without deploying Prometheus or any external metrics stack.

### Core Architectural Tenets:
1. **Zero-Fan-Out Piggybacked Push**: Clients sample local allocations every `StatsCollectionInterval` (`1s`) and pre-aggregate metrics per task group over `client_metrics_interval` (`10s`). Completed windows ride on existing `Node.UpdateAlloc` RPCs to the Raft leader—requiring zero server-to-client polling connections.
2. **Zero Raft Write Amplification**: Usage reports are authenticated against the sending node's identity (`autoscalerRecordUsage`) and stored strictly in leader RAM inside a fixed-size 30-slot sliding ring buffer (`usageBuckets = 30`). Raft is only written when an actual count change (`Job.Scale` / `CustomObject.Scale`) or oscillation suspension occurs.
3. **Three-Tier Native Application Metric Collection**:
   - **Tier 1 (Zero-Config L4/L7 Kernel & Sidecar Metrics)**: Network namespace byte/connection counters (`nomad.net.*`) and built-in mTLS Envoy proxy telemetry (`nomad.proxy.*`).
   - **Tier 2 (Local HTTP/OpenMetrics & Check Header Extraction)**: Client-local scraping of a task group's declared `metrics` port/path or `X-Norca-Load` health check response headers, filtered strictly to an explicit metric allowlist (`include`).
   - **Tier 3 (Symlink-Safe Allocation File Sink)**: Worker and batch-queue tasks write JSON key-value gauges to `$NORCA_ALLOC_DIR/tmp/metrics.json`, read atomically by the client via `os.OpenRoot`.
4. **Intensive vs. Extensive (Backlog) Scaling Mathematics**: Native support for both per-allocation intensive metrics (`inflight_requests`, `rps_per_alloc`) and shared global backlog metrics (`queue_depth` with `total_target` / `divisor = "count"` normalization and $0 \leftrightarrow N$ scale-from-zero).

```
+---------------------------------------------------------------------------------------------------+
|                                        NORCA CLIENT NODE                                          |
|                                                                                                   |
|  +---------------------------+  +-----------------------------+  +-----------------------------+  |
|  | Tier 1: Zero-Config Infra |  | Tier 2: Local HTTP / Probe  |  | Tier 3: Alloc File Sink     |  |
|  | - cgroup CPU & Memory     |  | - Local /metrics (OpenMet.) |  | - $NORCA_ALLOC_DIR/tmp/     |  |
|  | - netns RX/TX & TCP Conns |  |   filtered by `include`     |  |   metrics.json (<= 4 KiB)   |  |
|  | - mTLS Envoy Admin Socket |  | - Service Check Header      |  | - Read via os.OpenRoot      |  |
|  |   (RPS, Inflight, p99)    |  |   (X-Norca-Load)            |  |   (symlink-safe, mtime opt) |  |
|  +-------------+-------------+  +--------------+--------------+  +--------------+--------------+  |
|                |                               |                                |                 |
|                +-------------------------------+--------------------------------+                 |
|                                                | 1s Sample Loop (collectTaskGroupUsage)           |
|                                                v                                                  |
|                        +-----------------------------------------------+                          |
|                        |    TaskGroupUsageAggregator (allocrunner)     |                          |
|                        |  Key: {Namespace, JobID, TaskGroup}           |                          |
|                        |  Accumulates: Allocs, Samples, Avg, Max       |                          |
|                        |  - Core: [CPUPct, MemPct, CPUMHz, MemMB]      |                          |
|                        |  - App:  Custom map[string]TaskGroupUsageValue|                          |
|                        +-----------------------+-----------------------+                          |
|                                                | Flush() every 10s (TaskGroupUsageInterval)       |
|                                                v                                                  |
|                        +-----------------------------------------------+                          |
|                        |   Node.UpdateAlloc RPC (TaskGroupUsageReport) |                          |
|                        +-----------------------+-----------------------+                          |
+------------------------------------------------|--------------------------------------------------+
                                                 | Multiplexed TCP/mTLS (:4647)
                                                 v
+---------------------------------------------------------------------------------------------------+
|                                    NORCA SERVER (RAFT LEADER)                                     |
|                                                                                                   |
|  1. Node Identity Verification (`autoscalerRecordUsage`, `norca/norca/autoscaler_usage.go:34`):   |
|     Rejects report unless `identity.ClientID == report.NodeID` (prevents cross-node spoofing).    |
|                                                |                                                  |
|                                                v                                                  |
|  2. Leader In-Memory Ring Buffer (`usageCollector`, `norca/norca/autoscaler/metrics.go:111`):     |
|     30 Time Buckets (`usageBuckets = 30`, e.g. 30 x 10s = 5m sliding window, 0 Raft writes):      |
|     +---------+---------+---------+---------+-----+---------+                                     |
|     | Slot 00 | Slot 01 | Slot 02 | Slot 03 | ... | Slot 29 |   slot = (nowNano / width) % 30     |
|     +---------+---------+---------+---------+-----+---------+                                     |
|                                                |                                                  |
|                                                v                                                  |
|  3. `usageAPM.Query` (`norca/norca/autoscaler/usage_apm.go:62`)                                   |
|     Computes sample-weighted Avg, Max, or RunningAllocs-scaled Sum across `[From, To]`.           |
|                                                |                                                  |
|                                                v                                                  |
|  4. Evaluator & Safeguards (`evaluator.go`, `coordination.go`, `executor.go`):                    |
|     Strategy (`target-value` / `threshold`) -> Multi-Check Winner (`PreemptScalingAction`)        |
|     -> Namespace Quota Ceiling (`capAtQuota`) -> Oscillation Guard -> `Job.Scale` / `Object.Scale`|
+---------------------------------------------------------------------------------------------------+
```

---

## 2. Foundation: The Existing Native Metric Pipeline in Code

Before examining the application-level extensions, this section documents the exact mechanics already implemented in Norca's codebase.

### 2.1 Heartbeat-Driven Activation & Zero Idle Overhead

Clients do not collect or transmit autoscaling usage unless instructed by the servers:
1. During client heartbeats (`Node.UpdateStatus` / `Node.UpdateAlloc`), the server computes `s.autoscalerUsageInterval()` (`norca/norca/autoscaler_usage.go:16`):
   - Returns `0` if the built-in autoscaler is disabled or if any server in the region has not yet upgraded to `minNorcaFeaturesVersion` (`0.1.0`).
   - Otherwise, returns `client_metrics_interval` (default `10s`, minimum `1s`).
2. The client's `taskGroupUsageReporter.setInterval` (`norca/client/taskgroup_usage.go:37`) starts or resets the local aggregation window when `NodeUpdateResponse.TaskGroupUsageInterval > 0`.

### 2.2 Client-Side Sampling & Task Group Aggregation

In `Client.collectTaskGroupUsage` (`norca/client/taskgroup_usage.go:98`), the client ticks every `sampleInterval = min(StatsCollectionInterval, TaskGroupUsageInterval)` (typically `1s`):
1. Iterates over all local `AllocRunner` instances where `AllocState().ClientStatus == structs.AllocClientStatusRunning`.
2. Queries `ar.StatsReporter().LatestAllocStats("")` and passes the allocation and `AllocResourceUsage` to `TaskGroupUsageAggregator.Observe` (`norca/client/allocrunner/taskgroup_usage.go:65`).
3. `Observe` extracts raw `cpuMHz` (`CpuStats.TotalTicks`) and `memoryMB` (`MemoryStats.Usage`, falling back to `MemoryStats.RSS`), compares them against `alloc.ComparableResources()`, and updates the per-group accumulator `taskGroupUsageAcc`:
   - Records unique allocation IDs in `acc.allocs[alloc.ID]`.
   - Increments `acc.samples`.
   - Accumulates `sum` and `max` for `cpuMHz`, `memoryMB`, `cpuPercent`, and `memoryPercent`.
4. When `now.Sub(windowStart) >= interval`, `windowDone` flushes the sorted slice of `*structs.TaskGroupUsage` into `r.pending`. The next `allocSync` attaches `r.take()` to `structs.AllocUpdateRequest.TaskGroupUsage`. If an RPC fails, stale reports are dropped rather than queued, ensuring the leader only receives fresh window data.

### 2.3 Leader Authentication & Fixed-Memory Ring Buffer

When `Node.UpdateAlloc` reaches the Raft leader:
1. **Spoofing Prevention (`norca/norca/autoscaler_usage.go:34`)**: `autoscalerRecordUsage` verifies that the authenticated mTLS/RPC identity (`args.GetIdentity().ClientID`) or `node.SecretID` matches `report.NodeID`. A compromised client cannot inject synthetic metrics to trigger scale-downs unless it actually hosts allocations for that task group.
2. **Ring-Buffer Bucket Mapping (`norca/norca/autoscaler/metrics.go:153`)**:
   - The leader's `usageCollector` divides `metrics_window` $W$ (default `5m`) into $B = 30$ buckets (`usageBuckets = 30`), giving each bucket a fixed width $w_b = W / 30$ (default `10s`).
   - For an incoming report at timestamp $t$ (`nowNano`), the bucket start and ring index are:
     $$\text{start} = t - (t \bmod w_b), \qquad \text{slot} = \left\lfloor \frac{t}{w_b} \right\rfloor \bmod 30$$
   - If `b.start != start` (meaning the ring slot wrapped around from a previous window cycle), the bucket is zeroed in-place (`*b = usageBucket{start: start}`).
   - For each reported group $g$ with sample count $S_g = \text{float64}(g.\text{Samples})$:
     $$b.\text{weight} \leftarrow b.\text{weight} + S_g, \qquad b.\text{sum}[m] \leftarrow b.\text{sum}[m] + g.\text{Avg}_m \cdot S_g, \qquad b.\text{max}[m] \leftarrow \max\bigl(b.\text{max}[m],\; g.\text{Max}_m\bigr)$$
3. **Bounded Memory Footprint**:
   - Each `groupUsage` struct holds a fixed array `[30]usageBucket` independent of how many nodes or allocations belong to the task group.
   - Total tracked groups are capped at `maxUsageGroups = 100_000` (`metrics.go:66`), with inactive groups swept every `metrics_window` (`sweepLocked`, `metrics.go:218`).
4. **Post-Election Warmup Guard (`ErrInsufficientData`, `metrics.go:71, 234`)**:
   - Because the ring buffer lives exclusively in leader RAM, a newly elected Raft leader starts with an empty buffer (`c.started = now`).
   - Until a full `metrics_window` has elapsed ($t - t_{\text{started}} \ge W$), `usageCollector.query` returns `ErrInsufficientData`. The evaluator suppresses scaling decisions on built-in metrics during warmup, preventing false scale-downs immediately after a leader failover.

---

## 3. White-Room Extension: Native Application & Network Metrics

To support dynamic application-level autoscaling without external dependencies, Norca extends the client-to-leader pipeline across three native collection tiers.

### 3.1 Tier 1: Zero-Config Network & Built-in mTLS Proxy Metrics

Every service allocation in Norca runs inside an isolated network namespace (or host port mapping) and optionally alongside Norca's built-in Envoy mTLS proxy (`norca/client/mtlsproxy/`). The client agent can observe L4 and L7 traffic saturation directly from the kernel and local sidecar without requiring application code changes:

| Built-in Metric Name | Unit | Source on Client Node | Primary Scaling Use Case |
| :--- | :--- | :--- | :--- |
| **`nomad.net.rx_bytes_per_sec`** | Bytes/s | Allocation netns / driver network interface counter derivative $\Delta \text{RX} / \Delta t$ | Ingress bandwidth-bound stream processors, media ingestion, or file upload gateways. |
| **`nomad.net.tx_bytes_per_sec`** | Bytes/s | Allocation netns / driver network interface counter derivative $\Delta \text{TX} / \Delta t$ | Egress cache servers, CDN origin nodes, or artifact mirrors. |
| **`nomad.net.conns`** | Count | Active `ESTABLISHED` TCP sockets in the allocation's netns (or matched to allocated ports) | Connection-heavy WebSocket servers, MQTT brokers, or TCP proxies. |
| **`nomad.proxy.rps`** | Req/s | Built-in mTLS Envoy sidecar (`mtls_envoy_hook.go`) local admin stats (`downstream_rq_total` rate) | Zero-config HTTP/gRPC request-rate scaling for any service with `mtls { mode = "sidecar" }`. |
| **`nomad.proxy.inflight`** | Count | Built-in mTLS Envoy sidecar gauge `downstream_rq_active` | Concurrency-bound synchronous APIs where latency spikes when active requests exceed worker threads. |
| **`nomad.proxy.latency_p99_ms`** | ms | Built-in mTLS Envoy sidecar histogram `downstream_rq_time` (P99) | SLO-driven scaling when tail latency degrades under downstream contention. |

#### Derivative Rate Calculation on the Client
For monotonic counters (network bytes, total requests $C(t)$), the client's `AllocRunner` maintains the prior sample $(t_{i-1}, C_{i-1})$ in memory and computes the instantaneous rate before feeding `TaskGroupUsageAggregator.Observe`:

$$R_i = \begin{cases} \dfrac{C_i - C_{i-1}}{(t_i - t_{i-1}) \text{ in seconds}} & \text{if } C_i \ge C_{i-1} \;\land\; t_i > t_{i-1} \\ 0 & \text{if counter reset (task restart)} \end{cases}$$

### 3.2 Tier 2: Declarative Local Application Metrics & Health Check Piggybacking

When an application exposes domain-specific internal metrics (e.g., `http_requests_in_flight`, `db_pool_waiters`, `checkout_queue_depth`), the client collects them locally via two lightweight mechanisms:

```
+-----------------------------------------------------------------------------------+
|                            ALLOCATION CONTAINER / TASK                            |
|                                                                                   |
|  Option A: Health Check Piggyback        Option B: Declarative Local Scrape       |
|  GET /healthz HTTP/1.1                   GET /metrics HTTP/1.1 (or JSON)          |
|  HTTP/1.1 200 OK                         # TYPE queue_depth gauge                 |
|  X-Norca-Load: inflight=42,lag=180       queue_depth 180                          |
|                                          http_requests_in_flight{method="POST"} 42|
+-------------------+-------------------------------------------+-------------------+
                    |                                           |
                    +---------------------+---------------------+
                                          | (Loopback / Bridge NetNS, <= 64 KiB body)
                                          v
+-----------------------------------------------------------------------------------+
|                      CLIENT ALLOWLIST FILTER & AGGREGATOR                         |
|  - Only retains metrics explicitly listed in group/task `metrics { include = [] }`|
|  - Sums label series sharing a metric name (unless label filter specified)        |
|  - Enforces max 16 custom metrics per task group (`maxCustomMetricsPerGroup = 16`)|
+-----------------------------------------------------------------------------------+
```

1. **Health Check Response Header (`X-Norca-Load`)**:
   - Applications already responding to `service { check { type = "http" } }` can include an `X-Norca-Load` response header containing comma-separated `<key>=<float64>` pairs (e.g., `X-Norca-Load: inflight=42, queue_depth=180`).
   - Zero extra network sockets or timers are opened; the client health check runner (`client/serviceregistration/checks/`) parses the header on every successful `2xx` probe and records the values into the task group's usage accumulator.
2. **Local Loopback Scrape (`metrics` Stanza)**:
   - A task group or task can declare a lightweight `metrics` block specifying `port`, `path` (default `"/metrics"`), `format` (`"prometheus"` or `"json"`), and an explicit `include` allowlist of metric names.
   - Because the client only extracts the numeric values of the allowlisted names (`len(include) <= 16`) using a zero-allocation line scanner capped at `io.LimitReader(resp.Body, 64*1024)`, the client never parses or buffers thousands of unused histogram buckets.

### 3.3 Tier 3: Symlink-Safe Allocation File Sink (`$NORCA_ALLOC_DIR/tmp/metrics.json`)

Batch workers, queue consumers, and CLI sub-processes often lack an HTTP server altogether. For these workloads, the task can atomically write a small JSON object or key-value file to `$NORCA_ALLOC_DIR/tmp/metrics.json`:

```json
{
  "queue_depth": 1240,
  "active_jobs": 8,
  "batch_lag_seconds": 14.2
}
```

#### Security & Symlink-Traversal Prevention
Following the same security model as Norca's secret delivery hook (`norca/client/allocrunner/taskrunner/secret_hook.go`), the client reads the metrics file using Go's `os.OpenRoot` scoped to the allocation directory:
1. **No Symlink Following**: `root, err := os.OpenRoot(allocDir)` followed by `root.Open("tmp/metrics.json")` guarantees that a malicious or compromised task cannot symlink `tmp/metrics.json` to a host file outside the allocation sandbox.
2. **Modification Time (`mtime`) Short-Circuit**: The client caches the file's `ModTime()` per allocation. If the file has not been updated within `2 * TaskGroupUsageInterval`, it is treated as stale and skipped.
3. **Strict Size Limit**: Reads are bounded by `io.LimitReader(f, 4096)` (`4 KiB`).

---

## 4. Wire Protocol & Ring-Buffer Data Structures

### 4.1 Extending `structs.TaskGroupUsage` (`norca/norca/structs/autoscaler_usage.go`)

To carry dynamic application and network metrics over `Node.UpdateAlloc` while preserving backward compatibility and fixed memory bounds, `TaskGroupUsage` is extended with a bounded `Custom` map:

```go
const (
	// MaxCustomUsageMetricsPerGroup caps the number of distinct application or
	// network metrics a single task group can report in one window.
	MaxCustomUsageMetricsPerGroup = 16

	// MaxCustomUsageMetricNameLen bounds metric identifier length.
	MaxCustomUsageMetricNameLen = 128
)

type TaskGroupUsage struct {
	Namespace     string
	JobID         string
	TaskGroup     string
	Allocs        int
	Samples       int
	CPUMHz        TaskGroupUsageValue
	CPUPercent    TaskGroupUsageValue
	MemoryMB      TaskGroupUsageValue
	MemoryPercent TaskGroupUsageValue

	// Custom holds pre-aggregated application, network, and proxy metrics
	// keyed by metric name (e.g. "nomad.net.conns", "nomad.app.queue_depth").
	Custom        map[string]TaskGroupUsageValue `codec:"custom,omitempty"`
}
```

### 4.2 Ring-Buffer Storage for Custom Metrics (`norca/norca/autoscaler/metrics.go`)

Inside `usageCollector`, each `groupUsage` maintains the fixed `[30]usageBucket` array for the 4 core metrics alongside a bounded per-metric ring `custom map[string]*[usageBuckets]customUsageBucket` (capped at `MaxCustomUsageMetricsPerGroup = 16` per `groupUsage`):

```go
type customUsageBucket struct {
	start  int64
	weight float64
	sum    float64
	max    float64
}
```

Why track `weight` per custom metric rather than reusing `usageBucket.weight`?
Because an application metric (such as an HTTP check header or `metrics.json`) may be sampled at a different cadence (e.g., every `5s` check interval) than cgroup CPU stats (`1s`), or may only be emitted once the application finishes warming up. Storing `weight` per custom metric ensures that sample-weighted averaging across multiple client nodes remains mathematically exact even when sampling frequencies differ.

---

## 5. Mathematical Formulations for Application Scaling

Application-level scaling requires distinguishing between **intensive metrics** (where adding replicas divides the metric value per replica) and **extensive/backlog metrics** (where every replica observes the same shared global queue depth).

### 5.1 Case 1: Intensive Per-Allocation Metrics (`avg` / `max`)

Examples: `nomad.proxy.rps`, `nomad.proxy.inflight`, `nomad.net.conns`, `nomad.app.active_workers`.

When $K$ client nodes report usage for a task group over the query window $[t_{\text{from}}, t_{\text{to}}]$, the leader computes the sample-weighted per-allocation average:

$$M_{\text{avg}} = \frac{\sum_{b \in [t_{\text{from}}, t_{\text{to}}]} \text{sum}_b}{\sum_{b \in [t_{\text{from}}, t_{\text{to}}]} \text{weight}_b}$$

With `target-value` strategy (`target` $= T$, deadband `threshold` $= \epsilon$, current running allocations $= C_{\text{curr}}$), the proportional factor is $f = \frac{M_{\text{avg}}}{T}$. Outside the deadband $[1 - \epsilon, 1 + \epsilon]$, the raw desired count is:

$$C_{\text{raw}} = \left\lceil C_{\text{curr}} \cdot \frac{M_{\text{avg}}}{T} \right\rceil$$

*Verification*: Suppose $C_{\text{curr}} = 4$ allocations each handle an average of $M_{\text{avg}} = 75$ in-flight requests (total cluster load $= 300$ in-flight requests), and the target per allocation is $T = 50$. Then:
$$C_{\text{raw}} = \left\lceil 4 \cdot \frac{75}{50} \right\rceil = 6$$
Once 6 allocations are running and load distributes evenly, $M_{\text{avg}}$ drops to $300 / 6 = 50 = T$, and $f = 1.0 \in [1 - \epsilon, 1 + \epsilon]$, stabilizing the replica count in a single step.

### 5.2 Case 2: Extensive Shared Backlog Metrics (`divisor = "count"` / Total Queue Depth)

Examples: `nomad.app.queue_depth`, `nomad.app.kafka_partition_lag`, `nomad.app.pending_jobs`.

Consider a worker group where every allocation queries or reports the length of a **shared queue** $Q_{\text{total}} = 1000$, and each worker is designed to handle $T = 100$ queued items.
- **Why Naive Proportional Scaling Fails on Shared Queues**:
  If $C_{\text{curr}} = 2$ and we apply the intensive formula $\lceil C_{\text{curr}} \cdot (Q_{\text{total}} / T) \rceil$, the first evaluation scales to $\lceil 2 \cdot (1000 / 100) \rceil = 20$. On the next evaluation (while the queue still has 900 items), $C_{\text{curr}}$ is now $20$, so the naive formula computes $\lceil 20 \cdot (900 / 100) \rceil = 180$—causing runaway exponential overscaling!
- **The White-Room Backlog Normalization (`divisor = "count"`)**:
  When a `target-value` strategy configures `divisor = "count"` (or uses the `backlog` metric mode), the per-allocation effective load is defined as $\frac{M_{\text{avg}}}{\max(1, C_{\text{curr}})}$, which makes the factor:

  $$f_{\text{backlog}} = \frac{M_{\text{avg}}}{\max(1, C_{\text{curr}}) \cdot T}$$

  Substituting $f_{\text{backlog}}$ into the `target-value` equation $\lceil C_{\text{curr}} \cdot f_{\text{backlog}} \rceil$ cancels $C_{\text{curr}}$ cleanly for $C_{\text{curr}} > 0$ and naturally supports **Scale-from-Zero ($C_{\text{curr}} = 0$)**:

  $$C_{\text{raw}} = \left\lceil \frac{M_{\text{avg}}}{T} \right\rceil$$

  With the deadband $\epsilon$:
  $$\text{Direction} = \begin{cases} \texttt{Up} & \text{if } (C_{\text{curr}} = 0 \land M_{\text{avg}} > 0) \;\lor\; (C_{\text{curr}} > 0 \land f_{\text{backlog}} > 1 + \epsilon) \\ \texttt{Down} & \text{if } C_{\text{curr}} > 0 \land f_{\text{backlog}} < 1 - \epsilon \\ \texttt{None} & \text{otherwise} \end{cases}$$

*Verification*: With $Q_{\text{total}} = 1000$ and $T = 100$, the first evaluation scales from $C_{\text{curr}} = 2$ to $\lceil 1000 / 100 \rceil = 10$. On the next evaluation, if $Q_{\text{total}} = 950$ and $C_{\text{curr}} = 10$, then $f_{\text{backlog}} = \frac{950}{10 \cdot 100} = 0.95$, which scales smoothly to $\lceil 9.5 \rceil = 10$ (zero overscaling). When the queue drains to $Q_{\text{total}} = 0$ and `min = 0`, $C_{\text{raw}} = 0$, scaling the worker pool down to zero.

### 5.3 Multi-Signal Composite Scaling & Safety Boundaries

When a task group combines infrastructure metrics (`nomad.cpu`, `nomad.memory`) with application metrics (`nomad.proxy.inflight`, `nomad.app.queue_depth`) in a single `scaling` policy:

```
                  +--------------------------------------------------+
                  |        Parallel Check Evaluation (evaluator)     |
                  +------------------------+-------------------------+
                                           |
        +----------------------------------+----------------------------------+
        |                                  |                                  |
        v                                  v                                  v
+---------------+                  +---------------+                  +---------------+
|  check "cpu"  |                  |check "inflight|                  | check "queue" |
| nomad.cpu=45% |                  | inflight=90   |                  | lag=1200      |
| target=70%    |                  | target=50     |                  | target=100    |
| -> Vote: Down |                  | -> Vote: Up(8)|                  | -> Vote:Up(12)|
|    Count: 3   |                  |    Count: 8   |                  |    Count: 12  |
+-------+-------+                  +-------+-------+                  +-------+-------+
        |                                  |                                  |
        +----------------------------------+----------------------------------+
                                           |
                                           v
                  +--------------------------------------------------+
                  | 1. Winner Selection (`sdk.PreemptScalingAction`) |
                  |    - `Up` preempts `Down`                        |
                  |    - Largest desired count wins: C_winner = 12   |
                  +------------------------+-------------------------+
                                           |
                                           v
                  +--------------------------------------------------+
                  | 2. Namespace Quota Cap (`capAtQuota`)            |
                  |    - Remaining quota fits +6 allocs (C_base=4)   |
                  |    - C_final = min(12, 4 + 6) = 10               |
                  |    - Emits `ScalingQuotaCapped` event            |
                  +------------------------+-------------------------+
                                           |
                                           v
                  +--------------------------------------------------+
                  | 3. Oscillation & Cooldown Gate (`executor.go`)   |
                  |    - Verifies < 3 direction flips in 10m         |
                  |    - Dispatches internal `Job.Scale` RPC         |
                  +--------------------------------------------------+
```

1. **Upward Preemption (`sdk.PreemptScalingAction`)**: Even if CPU is idle (`45%`, voting to scale down to 3), high in-flight requests or queue depth (`ScaleDirectionUp`) unconditionally preempts `ScaleDirectionDown`. Among checks voting `Up`, the highest replica count (`12`) wins.
2. **Safe Scale-Down Consensus**: A scale-down only proceeds when **every** active check (CPU, Memory, In-Flight Requests, and Queue Depth) simultaneously agrees that utilization is below its target threshold.
3. **Quota Ceiling Enforcement (`capAtQuota`, `norca/norca/autoscaler/coordination.go:23`)**: The winning count is automatically clamped to the namespace's remaining CPU/Memory quota capacity before invoking `Job.Scale`.

---

## 6. Cross-Group Metric Referencing (Scale-from-Zero Producer/Consumer Pattern)

A classic challenge with scale-from-zero (`min = 0`) is that when a consumer group has $0$ running allocations, no allocation of that group is running to report `metrics.json` or `/metrics`.

Norca's `usageAPM.parseQuery` (`norca/norca/autoscaler/usage_apm.go:101`) already supports cross-group target addressing via the canonical suffix `/<group>/<job>@<namespace>`:

$$\texttt{<metric\_or\_query>/<producer\_group>/<job>@<namespace>}$$

This enables a running API gateway or queue-monitor group (`group "api"`) to report `nomad.app.checkout_queue_depth`, while the consumer worker group (`group "worker"`, configured with `min = 0`) queries the metric from the `api` group:

```hcl
check "wake-on-queue" {
  source = "nomad-apm"
  query  = "nomad.app.checkout_queue_depth/api/checkout-service@default"

  strategy "target-value" {
    target  = 50
    divisor = "count"
  }
}
```

When `api` reports `checkout_queue_depth = 150`, the leader's `usageCollector` stores it under `{default, checkout-service, api}`. The `worker` group's check queries that key, computes $\lceil 150 / 50 \rceil = 3$, and scales `worker` from `0` to `3` allocations!

---

## 7. HCL Configuration Reference

### 7.1 Server Configuration (`norca.hcl`)

```hcl
server {
  enabled = true

  autoscaler {
    enabled                 = true
    default_engine          = "builtin"
    default_evaluation_interval = "10s"

    # Client-pushed native metric pipeline tuning
    client_metrics_interval = "10s"  # How often clients flush TaskGroupUsageReport
    metrics_window          = "5m"   # 30 buckets x 10s in leader RAM

    # Anti-flapping oscillation circuit breaker
    oscillation_window      = "10m"
    oscillation_threshold   = 3      # Suspend after 3 direction flips in 10m
    oscillation_suspend     = "15m"
  }
}
```

### 7.2 Full Application Autoscaling Job Specification

```hcl
job "order-processor" {
  datacenters = ["dc-a", "dc-b", "dc-c"]
  type        = "service"

  group "api" {
    count = 3

    network {
      mode = "bridge"
      port "http" { to = 8080 }
    }

    # Native mTLS sidecar automatically provides nomad.proxy.* metrics
    service {
      name = "order-api"
      port = "http"

      mtls {
        mode = "sidecar"
      }

      check {
        type     = "http"
        path     = "/healthz"
        interval = "5s"
        timeout  = "2s"
        # Optional header emitted by app:
        # X-Norca-Load: db_pool_waiters=4, checkout_queue=320
      }
    }

    # Optional: Local loopback OpenMetrics/JSON scrape filtered to an allowlist
    metrics {
      port    = "http"
      path    = "/metrics"
      include = ["http_requests_in_flight", "checkout_queue"]
    }

    scaling {
      enabled = true
      min     = 2
      max     = 30

      policy {
        evaluation_interval     = "10s"
        cooldown                = "3m"
        cooldown_on_scale_up    = "20s"
        scale_during_deployment = "up_only"

        # 1. Infrastructure safety floor (built-in cgroup CPU %)
        check "cpu-safety" {
          metric = "nomad.cpu"
          strategy "target-value" {
            target = 75
          }
        }

        # 2. L7 concurrency scaling via built-in Envoy mTLS sidecar
        check "envoy-inflight" {
          metric = "nomad.proxy.inflight"
          strategy "target-value" {
            target       = 40
            threshold    = 0.05
            max_scale_up = 10
          }
        }

        # 3. Application custom metric scraped locally by client
        check "app-inflight" {
          metric = "nomad.app.http_requests_in_flight"
          strategy "target-value" {
            target = 35
          }
        }
      }
    }

    task "server" {
      driver = "docker"
      config {
        image = "org/order-api:v1.2.0"
      }
      resources {
        cpu    = 1000
        memory = 512
      }
    }
  }

  # Scale-to-zero background worker driven by queue depth
  group "worker" {
    count = 0

    scaling {
      enabled = true
      min     = 0
      max     = 50

      policy {
        cooldown             = "5m"
        cooldown_on_scale_up = "10s"

        check "queue-backlog" {
          # Reads queue depth reported by the always-on "api" group
          query = "nomad.app.checkout_queue/api/order-processor@default"
          strategy "target-value" {
            target  = 100
            divisor = "count"
          }
        }
      }
    }

    task "consumer" {
      driver = "docker"
      config {
        image = "org/order-worker:v1.2.0"
      }
      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
```

---

## 8. Summary of Architectural Advantages

| Dimension | External `nomad-autoscaler` + Prometheus | Norca White-Room Native Application Scaling |
| :--- | :--- | :--- |
| **External Dependencies** | Requires external autoscaler binary + Prometheus server + scrape discovery | **Zero external dependencies** (100% embedded in `norca` server & client) |
| **Network Topology** | Server/Autoscaler polls $N$ clients or scrapes $M$ allocations ($O(M)$ connections) | **Piggybacked on existing `Node.UpdateAlloc` RPCs** (zero extra connections) |
| **Raft Disk I/O** | N/A (stored in external TSDB disk) | **Zero Raft I/O for metrics** (30-slot fixed ring buffer in leader RAM) |
| **Spoofing & Security** | Unauthenticated HTTP `/metrics` endpoints exposed on cluster network | **Loopback-only collection + mTLS Node ID verification (`autoscalerRecordUsage`)** |
| **Lagging vs. Leading Signals** | Only CPU/Memory without Prometheus | **CPU, Memory, Net RX/TX/Conns, Envoy RPS/Inflight/P99, App `/metrics`, `metrics.json`** |
| **Shared Queue Scaling** | Prone to exponential overscaling with standard `target-value` | **First-class `divisor = "count"` backlog normalization & cross-group scale-from-zero** |
| **Quota & Deployment Awareness** | Blind to namespace quotas; fails at scheduler time | **Pre-clamps scale-up to remaining namespace quota (`capAtQuota`) & respects `up_only` deployments** |

