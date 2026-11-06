<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Advanced Scheduling, Built-in Workload Autoscaling, and Disruption Budgets Architecture

## 1. Executive Summary & Core Architectural Principles

Norca elevates workload placement, horizontal elasticity, and voluntary disruption safety into first-class, zero-external-dependency subsystems natively embedded in the server control plane and scheduler workers:

1. **Cross-Job Affinity & Anti-Affinity (`colocate` & `separate`)**: Workloads declare hard (`required = true`) or weighted soft (`weight` in `[-100, 100]`) placement rules relative to running or planned allocations of any job (including cross-namespace targets) selected by `meta` label selectors and arbitrary node topology scopes (`${node.unique.id}`, `${node.datacenter}`, `${meta.rack}`, etc.).
2. **Simplified Two-Tier Topology (`region > datacenter`)**: Norca formalizes a clean failure-domain hierarchy where **1 Datacenter = 1 Availability Zone (AZ)** within a Region. Combining `${node.datacenter}` `spread`, `separate` anti-affinity, and topology-aware service discovery (`prefer = "datacenter"` with `min_local` and `overflow_fraction` safeguards) keeps intra-AZ traffic local while preventing small-AZ overload.
3. **Zero-External-Binary Built-in Horizontal Autoscaler (`norca/norca/autoscaler/`)**: The Raft leader natively runs an in-process horizontal workload autoscaler (`engine = "builtin"`). Clients aggregate allocation CPU and memory utilization over configurable windows and piggyback `TaskGroupUsageReport` payloads onto standard `Node.UpdateAlloc` RPCs—eliminating external `nomad-autoscaler` daemons and avoiding per-evaluation `ClientAllocations.Stats` RPC storms.
4. **Transactional Disruption Budgets (`disruption` stanza & `Alloc.Evict`)**: Inspired by Kubernetes `PodDisruptionBudget`, service task groups declare `min_healthy` or `max_unavailable` (as absolute counts or ceiling-rounded percentages). Both the leader's `NodeDrainer` and the atomic `Alloc.Evict` Raft FSM transaction enforce the budget while automatically reserving disruption headroom (`DeploymentReserved`) for active rolling deployments.

### 1.1 Architectural Comparison: Upstream Nomad vs. Norca

| Capability | Upstream Nomad v1.6.5 | Norca Architecture |
| :--- | :--- | :--- |
| **Cross-Job Pod Affinity / Anti-Affinity** | Not supported (only node-attribute `affinity` and intra-group `distinct_hosts`). | Native `colocate` and `separate` blocks (`norca/scheduler/alloc_affinity.go`) with `meta` selectors, cross-namespace ACL checks, and O(1) incremental plan folding. |
| **Horizontal Workload Autoscaling** | Requires deploying and operating an external `nomad-autoscaler` binary polling HTTP APIs. | Built into the Raft leader (`norca/norca/autoscaler/`), using client-pushed ring-buffer usage metrics (`nomad.cpu`, `nomad.memory`), Prometheus APM, quota capping, and oscillation suspension. |
| **Voluntary Disruption Protection** | Only static `migrate.max_parallel` during node drains; `alloc stop` ignores group health. | Declarative `disruption` budgets (`min_healthy` / `max_unavailable`), deployment-aware drain gating, and atomic `POST /v1/allocation/:id/evict` (`429 Too Many Requests` + `Retry-After`). |
| **AZ / Datacenter Traffic Locality** | Requires external service mesh or manual template filtering. | Native `discovery { prefer = "datacenter" }` with `min_local` and `overflow_fraction` tier spillover (`norca/norca/structs/discovery.go`). |

---

## 2. End-to-End Control Plane Architecture

### 2.1 Scheduler Iterator Pipeline (`GenericStack` & `SystemStack`)

When a scheduler worker processes an `Evaluation`, it constructs a `GenericStack` (`norca/scheduler/stack.go:46`) or `SystemStack` (`norca/scheduler/stack.go:210`). Norca shares a stateful `allocAffinityTracker` (`norca/scheduler/alloc_affinity.go:42`) across both the feasibility filtering stage and the ranking stage:

```
+---------------------------------------------------------------------------------------------------+
|                                  NORCA SCHEDULER WORKER PIPELINE                                  |
|                                     (norca/scheduler/stack.go)                                    |
+---------------------------------------------------------------------------------------------------+
                                                  |
     [ Base Nodes ] -> Fisher-Yates Shuffle (NewRandomIterator, feasible.go:128)
                                                  |
                                                  v
+---------------------------------------------------------------------------------------------------+
| PHASE 1: FEASIBILITY FILTERING (Hard Constraints)                                                 |
|                                                                                                   |
|  1. FeasibilityWrapper (computed node class cache)                                                |
|     ├── Job Constraints (jobConstraint)                                                           |
|     ├── TaskGroup Drivers, Constraints, Devices, Network, NetworkPolicy                           |
|     └── HostVolumeChecker & CSIVolumeChecker                                                      |
|  2. DistinctHostsIterator & DistinctPropertyIterator                                              |
|  3. AllocAffinityFeasibilityIterator (alloc_affinity.go:542)  <---+                               |
|     └── Enforces required = true `colocate` / `separate` rules    | Shared per-eval/per-group     |
|         (Bypasses in-place probes; allows 1st self-match alloc)   | allocAffinityTracker          |
|  4. QuotaIterator (namespace resource ceiling check)              | (alloc_affinity.go:42)        |
+-------------------------------------------------------------------|-------------------------------+
                                                  |                 |
                                                  v                 |
+-------------------------------------------------------------------|-------------------------------+
| PHASE 2: RANKING & SCORING (Soft Preferences & Resource Fit)      |                               |
|                                                                   |                               |
|  1. FeasibleRankIterator (upgrades *structs.Node -> *RankedNode)  |                               |
|  2. BinPackIterator / Spread Fit (rank.go:154)                    |                               |
|     ├── Assigns Networks, Devices, Reservable CPU Cores           |                               |
|     ├── Invokes Preemptor (preemption.go:99) if resources scarce  |                               |
|     └── Emits bin-packing or spread fit score in [0, 1]           |                               |
|  3. JobAntiAffinityIterator (penalizes stacking same job on node) |                               |
|  4. NodeReschedulingPenaltyIterator (penalizes previous failures) |                               |
|  5. NodeAffinityIterator (`affinity` blocks, rank.go:753)         |                               |
|  6. AllocAffinityRankIterator (alloc_affinity.go:593)         <---+                               |
|     └── Scores preferred `colocate` / `separate` rules in [-1, 1]                                 |
|  7. SpreadIterator (`spread` blocks across ${node.datacenter})                                    |
|  8. PreemptionScoringIterator (logistic S-curve priority penalty)                                 |
|  9. ScoreNormalizationIterator (arithmetic mean of all scorers)                                   |
| 10. LimitIterator (Power-of-Two / ceil(log2(N)) / >=100 for spread/affinity)                      |
| 11. MaxScoreIterator -> Selects highest-scoring *RankedNode                                       |
+---------------------------------------------------------------------------------------------------+
                                                  |
                                                  v
                    [ Plan Queue -> EvaluatePool (plan_apply_pool.go:21) -> Raft Commit ]
```

### 2.2 Built-in Leader Autoscaler & Client Usage Pipeline

```
+---------------------------------------------------------------------------------------------------+
|                                   NORCA CLIENT NODES                                              |
|  Allocations -> TaskRunner cgroups/stats -> Aggregated over TaskGroupUsageInterval (default 10s)  |
|  Piggybacked on Node.UpdateAlloc RPC: structs.TaskGroupUsageReport (Avg & Max CPU/Memory, Samples)|
+-------------------------------------------------|-------------------------------------------------+
                                                  | RPC Forwarded to Leader (Never written to Raft)
                                                  v
+---------------------------------------------------------------------------------------------------+
|                                  NORCA RAFT LEADER SERVER                                         |
|                                                                                                   |
|  +---------------------------------------------------------------------------------------------+  |
|  | 1. In-Memory Usage Collector (norca/norca/autoscaler/metrics.go:111)                        |  |
|  |    - 30-slot Ring Buffer per task group (`usageBuckets = 30`, max 100,000 groups)           |  |
|  |    - Sample-weighted averages & max tracking over `metrics_window` (default 5m)             |  |
|  |    - Warmup guard: returns `ErrInsufficientData` until 1 full window post-leadership        |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|                                                 v                                                 |
|  +---------------------------------------------------------------------------------------------+  |
|  | 2. Policy Watcher & Evaluation Runner (norca/norca/autoscaler/evaluator.go:79)              |  |
|  |    - Watches MemDB `scaling_policy` table via `memdb.WatchSet` (policies.go:38)             |  |
|  |    - Filters `engine = "builtin"` (or server `default_engine = "builtin"`)                  |  |
|  |    - Bounded Worker Pool (`eval_workers = 10`) with initial random splay                    |  |
|  |    - Checks Cron `schedule`, `deploymentActive` (`never` vs `up_only`), & Cooldowns         |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|                                                 v                                                 |
|  +---------------------------------------------------------------------------------------------+  |
|  | 3. Check & Strategy Execution (norca/norca/autoscaler/check.go:54)                          |  |
|  |    - Metrics Sources: Built-in `usageAPM` (`nomad.cpu`, `nomad.memory`), `prometheus`, ext  |  |
|  |    - Strategies: `target-value`, `threshold`, `fixed-value`, `pass-through`, external gRPC  |  |
|  |    - Clamps to `[policy.Min, policy.Max]`; resolves check groups via `PreemptScalingAction` |  |
|  |    - Caps scale-up at Namespace Quota (`capAtQuota`, coordination.go:23)                    |  |
|  +----------------------------------------------+----------------------------------------------+  |
|                                                 |                                                 |
|                                                 v                                                 |
|  +---------------------------------------------------------------------------------------------+  |
|  | 4. Action Executor (norca/norca/autoscaler/executor.go:38)                                  |  |
|  |    - Oscillation Guard: suspends policy for 15m if direction flips >= 3 times in 10m        |  |
|  |    - Cluster Token Bucket Rate Limiter (`rate_limit = 10/s`, `burst = 50`)                  |  |
|  |    - Internal RPC with ephemeral `autoscaler` system token:                                 |  |
|  |      -> `Job.Scale` (TaskGroup) OR `CustomObject.Scale` (Operator Custom Object)            |  |
|  |    - Publishes `ScalingDecision` / `ScalingQuotaCapped` event on `TopicScaling`             |  |
|  +---------------------------------------------------------------------------------------------+  |
+---------------------------------------------------------------------------------------------------+
```

### 2.3 Voluntary Disruption Budget Enforcement Gate

```
               VOLUNTARY DISRUPTION SOURCES
                            |
        +-------------------+-------------------+
        |                                       |
        v                                       v
[ Node Drain (NodeDrainer) ]          [ Eviction API / CLI ]
(drainer/watch_jobs.go:345)           `norca alloc stop -respect-disruption`
`POST /v1/node/:id/drain`             `POST /v1/allocation/:id/evict`
        |                                       |
        | Reads StateSnapshot                   | Submits Raft Log Entry:
        | & WatchSet                            | `structs.AllocEvictRequestType`
        v                                       v
+-------------------------------------------------------------------------------+
|                 SHARED DISRUPTION BUDGET COMPUTATION                          |
|             `structs.NewDisruptionStatus` (structs/disruption.go:214)         |
|                                                                               |
|  1. `RequiredHealthy` = `min_healthy` OR `Count - ceil(max_unavailable)`      |
|  2. `Healthy` = count of running, deployment-healthy, non-migrating allocs    |
|  3. `DeploymentReserved` = active deployment `min(remaining, max_parallel -   |
|                            inFlight)` headroom reserved for rolling updates   |
|  4. `DisruptionsAllowed` = `max(0, Healthy - RequiredHealthy - Reserved)`     |
+---------------------------------------+---------------------------------------+
                                        |
        +-------------------------------+-------------------------------+
        |                                                               |
        v                                                               v
[ Drainer Gate (watch_jobs.go:420) ]                  [ FSM Write Txn (state_store_disruption.go:23) ]
- Drains unhealthy allocs first                       - Atomic check inside `WriteTxnMsgT`
- Caps healthy drains at:                             - If allowed: sets `DesiredTransition.Migrate=true`
  `min(DisruptionsAllowed,                              and inserts follow-up `Evaluation`
       Healthy - (Count - Migrate.MaxParallel))`      - If disallowed: aborts Txn, returns
- Waits on MemDB watches (`allocs`, `jobs`,             `ErrDisruptionBudgetExceeded` -> HTTP `429`
  `deployment`) until replacements become healthy       with `Retry-After: 5` header
- Force-stops only when `DrainStrategy.Deadline` hits
```

---

## 3. Cross-Job Affinity, Anti-Affinity, and Datacenter Topology (`norca/scheduler/`)

### 3.1 Declarative `colocate` and `separate` Model

Norca introduces two task-group placement blocks—`colocate` (cross-job affinity) and `separate` (cross-job anti-affinity)—backed by `structs.AllocAffinity` (`norca/norca/structs/alloc_affinity.go:46`):

| Field | Type | Default | Description & Validation Rules (`structs/alloc_affinity.go:122`) |
| :--- | :--- | :--- | :--- |
| **`selector`** | `map[string]string` | *(Required)* | Non-empty map of key/value pairs matched against target jobs' `meta` merged with their task groups' `meta` (where task group `meta` overrides job `meta`, `Matches` at `structs/alloc_affinity.go:191`). Accepts both HCL block syntax (`selector { app = "api" }`) and map attribute syntax (`selector = { "app.kubernetes.io/name" = "api" }`, `jobspec2/hcl_alloc_affinity.go:19`). |
| **`namespace`** | `string` | `""` (Job's NS) | Target namespace searched for matching allocations. Cross-namespace rules require the submitting ACL token to hold `read-job` capability in every referenced foreign namespace (`allowAllocAffinityNamespaces`, `norca/norca/job_endpoint_hook_alloc_affinity.go:98`). |
| **`scope`** | `string` | `"${node.unique.id}"` | Topology domain interpolation key. Allowed scopes (`ValidAllocAffinityScope`, `structs/alloc_affinity.go:164`): `${node.unique.id}`, `${node.datacenter}`, `${node.unique.name}`, `${node.class}`, `${node.pool}`, `${attr.<name>}`, or `${meta.<name>}`. |
| **`required`** | `bool` | `false` | When `true`, acts as a strict feasibility filter (`AllocAffinityFeasibilityIterator`); `weight` must be `0`. `system` and `sysbatch` jobs may only define `required = true` rules (`structs/alloc_affinity.go:151`). |
| **`weight`** | `int8` | `50` (canonicalized) | Soft preference weight in `[-100, 100]` (non-zero when `required = false`). Negative weights invert the rule (`colocate` with negative weight acts like `separate`). |

Each task group may define at most **`MaxAllocAffinitiesPerGroup = 8`** combined `colocate` and `separate` rules (`structs/alloc_affinity.go:34`). At job submission (`jobAllocAffinityHook.Validate`, `norca/norca/job_endpoint_hook_alloc_affinity.go:30`), Norca verifies that all cluster servers meet `minNorcaFeaturesVersion`, warns if a referenced `namespace` does not exist, and emits a warning if a single rule selects more than **`allocAffinityManyJobs = 1000`** jobs.

### 3.2 O(1) Per-Node Evaluation & Incremental Plan Folding

A naive cross-job affinity implementation would scan running allocations for every candidate node, incurring $O(N_{\text{nodes}} \times N_{\text{allocs}})$ complexity. Norca's `allocAffinityTracker` (`norca/scheduler/alloc_affinity.go:42`) eliminates this bottleneck by decoupling state collection from node evaluation:

1. **Per-Namespace Single-Pass Collection (`collect`, `alloc_affinity.go:249`)**:
   - The first time a task group is evaluated (`SetTaskGroup`, `alloc_affinity.go:147`), `buildGroup` groups all rules by their target namespace so multiple rules targeting the same namespace share a single MemDB scan (`State().JobsByNamespace` and `State().AllocsByJob`).
   - For each non-terminal allocation of a matching task group, the tracker resolves its node's topology domain (`getProperty(node, r.rule.Scope)`) and increments `r.existing[domain]` and `r.total`.
2. **Incremental Plan Folding via Fingerprinting (`refreshPlan`, `alloc_affinity.go:340`)**:
   - Before each allocation placement within the same evaluation, `refreshPlan` folds in uncommitted placements (`plan.NodeAllocation`), stops (`plan.NodeUpdate`), and preemptions (`plan.NodePreemptions`) from the current `structs.Plan`.
   - Because scheduler workers only append to or pop from per-node plan slices, Norca fingerprints each plan node via `planNodeFingerprint{nAlloc, nUpdate, nPreempt, lastAlloc, lastUpdate, lastPreempt}` (`alloc_affinity.go:124`) and a generation counter `planGen`. Only nodes whose plan slices mutated since the prior placement recompute their domain deltas (`computePlanNode`, `alloc_affinity.go:404`).
   - Consequently, checking or scoring a candidate node costs a single hash-map lookup per rule:
     $$\text{count}(r, d) = \max\bigl(0,\; r.\text{existing}[d] + r.\text{proposed}[d]\bigr)$$

### 3.3 Hard Feasibility Filtering vs. Soft Preference Scoring

#### Required Rules (`AllocAffinityFeasibilityIterator`, `alloc_affinity.go:542`)
For each candidate node, `satisfiesRequired` (`alloc_affinity.go:488`) checks every rule where `r.rule.Required == true`:
- **In-Place Upgrade Bypass (`isInplaceProbe`, `alloc_affinity.go:474`)**: When the scheduler probes whether an existing allocation can be updated in-place on its current node (`DesiredDescription == "alloc is being updated in-place"`), `AllocAffinityFeasibilityIterator` immediately passes the node so `colocate`/`separate` rules never force an in-place upgrade to become a destructive migration.
- **Missing Property**: If the node lacks the attribute specified by `Scope`, the node is filtered out with `<rule>: missing property`.
- **Self-Matching Bootstrap Exception (`alloc_affinity.go:503`)**: If a task group's `colocate` rule matches the task group itself (`r.selfMatch == true`) and there are currently zero matching allocations anywhere in the cluster (`r.total <= 0`), the first allocation is permitted to land on any feasible node—matching Kubernetes `PodAffinity` bootstrap semantics. Subsequent allocations must colocate in the chosen topology domain.
- Otherwise, a required `colocate` rule requires $\text{count}(r, d) > 0$, and a required `separate` rule requires $\text{count}(r, d) == 0$.

#### Preferred Rules (`AllocAffinityRankIterator`, `alloc_affinity.go:593`)
Let $R_{\text{pref}}$ be the set of non-required rules with $w_r = r.\text{rule.Weight} \neq 0$, and let $W_{\text{sum}} = \sum_{r \in R_{\text{pref}}} |w_r|$ (`sumWeight`, `alloc_affinity.go:68`). For a candidate node $n$, let $d_r(n)$ be the value of `r.rule.Scope` on node $n$, and let the indicator $\mathbb{I}(r, n) \in \{0, 1\}$ be $1$ if $d_r(n)$ is defined and $\text{count}(r, d_r(n)) > 0$, and $0$ otherwise.

The `alloc-affinity` score $S_{\text{aff}}(n) \in [-1, 1]$ (`alloc_affinity.go:516`) is:

$$S_{\text{aff}}(n) = \frac{1}{W_{\text{sum}}} \sum_{r \in R_{\text{pref}}} \mathbb{I}(r, n) \cdot \sigma(r) \cdot w_r \quad \text{where } \sigma(r) = \begin{cases} +1 & \text{if } r \text{ is } \texttt{colocate} \\ -1 & \text{if } r \text{ is } \texttt{separate} \end{cases}$$

Note that `AllocAffinityRankIterator` appends $S_{\text{aff}}(n)$ (even when $0$) to `option.Scores` for every evaluated node (`alloc_affinity.go:619`) so that `ScoreNormalizationIterator` (`norca/scheduler/rank.go:791`) shifts the final normalized score of matching vs. non-matching nodes in exact proportion to the rule weights:

$$S_{\text{final}}(n) = \frac{1}{K} \sum_{k=1}^{K} S_k(n)$$

Furthermore, when a task group uses `nodeAffinity`, `spread`, or preferred `allocAffinity`, `GenericStack.Select` automatically raises `LimitIterator`'s node scan limit from $\lceil\log_2(N)\rceil$ to $\max(\text{tg.Count}, 100)$ (`norca/scheduler/stack.go:185-194`) to ensure broad topology sampling.

### 3.4 Simplified `region > datacenter` Topology Model & Locality

Norca treats **1 Region** as an independent Raft consensus cluster and **1 Datacenter (`${node.datacenter}`)** within that region as **1 Availability Zone (AZ) / Failure Domain**:

1. **Datacenter Spread (`norca/scheduler/spread.go`)**:
   - When a job or task group declares `spread { attribute = "${node.datacenter}" }` without explicit target percentages, `evenSpreadScoreBoost` (`spread.go:214`) computes the current allocation counts across datacenters ($c_{\min}, c_{\max}$, and $c(d)$ for the candidate node's datacenter $d$) and scores nodes to equalize placement across AZs.
   - When explicit `target` percentages are configured, the weighted score boost (`spread.go:196`) is:
     $$S_{\text{spread}}(n) = \sum_{s \in \text{Spreads}} \left(\frac{\text{desired}_{s, d} - (\text{used}_{s, d} + 1)}{\text{desired}_{s, d}}\right) \cdot \frac{w_s}{\sum_{j} w_j}$$
2. **Topology-Aware Service Discovery (`norca/norca/structs/discovery.go`)**:
   - Services registered with the native catalog can configure a `discovery` block (`ServiceDiscovery`, `structs/discovery.go:55`) with `prefer = "datacenter"`.
   - When a consumer in `near = Topology{Region, Datacenter}` resolves the service (via DNS, template rendering, HTTP API `?near=self`, or Consul-compatible catalog endpoints), `SelectByTopology` (`structs/discovery.go:162`) partitions healthy instances into two tiers: Tier 0 ($I_{\text{dc}}$, same datacenter) and Tier 1 ($I_{\text{rest}}$, remaining datacenters in the region).
   - Tier 0 is returned exclusively if and only if it satisfies both the absolute floor $\max(1, \text{MinLocal})$ and the proportional floor $\text{OverflowFraction} \times |I_{\text{all}}|$ (default `DefaultDiscoveryOverflowFraction = 0.2`, `structs/discovery.go:41`):
     $$\text{Result} = \begin{cases} I_{\text{dc}} & \text{if } |I_{\text{dc}}| \ge \max(1, \text{min\_local}) \;\land\; |I_{\text{dc}}| \ge \text{overflow\_fraction} \cdot |I_{\text{all}}| \\ I_{\text{dc}} \mathbin{\Vert} I_{\text{rest}} & \text{otherwise (spillover to region)} \end{cases}$$
   - If the local region has zero eligible instances, resolution fails over sequentially through `discovery.failover = ["eu-west", ...]` (`structs/discovery.go:74`).

---

## 4. Built-in Horizontal Workload Autoscaler (`norca/norca/autoscaler/`)

### 4.1 Leader Lifecycle & Ephemeral System Identity

The built-in autoscaler (`autoscaler.Autoscaler`, `norca/norca/autoscaler/autoscaler.go:62`) is instantiated by the server (`setupAutoscaler`, `norca/norca/autoscaler_server.go:49`) and activated when the server wins Raft leadership (`SetLeader(true)`):

- **Cluster Version Gate**: Before starting the evaluation loop, `Autoscaler.run` waits until `ServersMeetMinimumNorcaVersion` reports that all servers in the region support Norca's built-in autoscaler (`autoscaler.go:188`).
- **Least-Privilege Ephemeral Identity**: Upon establishing leadership, the leader generates a random in-memory secret (`setAutoscalerSecret`, `autoscaler_server.go:68`) bound to the synthetic `autoscalerACLToken` (`AccessorID: "autoscaler"`). This token is **never persisted to Raft** and resolves to a hardcoded minimal ACL policy (`autoscaler_server.go:17`):
  ```hcl
  namespace "*" {
    capabilities = ["scale-job"]
    object "*" { capabilities = ["write", "write-events"] }
  }
  ```
- **Live Reload**: Updating the `autoscaler` stanza in `norca.hcl` and sending `SIGHUP` invokes `Autoscaler.SetConfig` (`autoscaler.go:111`), which diffs the config (`configEqual`) and cleanly restarts the runner and plugin host if modified.

### 4.2 Client-Pushed Ring-Buffer Metrics (`usageCollector` & `usageAPM`)

Unlike the external `nomad-autoscaler`—which opens HTTP connections to every client node to poll `/v1/client/allocation/:id/stats` on every check evaluation—Norca reverses the metric flow:

1. **Heartbeat Negotiation (`norca/norca/autoscaler_usage.go:16`)**:
   - When the built-in autoscaler is enabled and all servers meet the version gate, the server's heartbeat response (`NodeUpdateResponse.TaskGroupUsageInterval`) instructs clients to aggregate task group resource usage over `client_metrics_interval` (default `10s`, minimum `1s`).
2. **Piggybacked Client Reports (`structs.TaskGroupUsageReport`, `structs/autoscaler_usage.go:13`)**:
   - Clients attach `TaskGroupUsageReport` to `Node.UpdateAlloc` requests. Each `TaskGroupUsage` entry contains the per-allocation average (`Avg`) and maximum (`Max`) over the window for `CPUMHz`, `CPUPercent`, `MemoryMB`, and `MemoryPercent`, along with the number of `Allocs` and measurement `Samples`.
   - On the leader, `autoscalerRecordUsage` (`autoscaler_usage.go:34`) authenticates that the caller's `ClientID` or `node.SecretID` matches `report.NodeID` before passing the report to `Autoscaler.RecordUsage`. Reports are stored strictly in leader RAM and **never touch Raft**.
3. **Fixed-Memory 30-Slot Ring Buffer (`usageCollector`, `norca/norca/autoscaler/metrics.go:111`)**:
   - Each task group (`targetKey{namespace, job, group}`, capped at `maxUsageGroups = 100,000`) maintains a ring of **`usageBuckets = 30`** time buckets spanning `metrics_window` (default `5m`, so each bucket has width $w_b = 5\text{m} / 30 = 10\text{s}$).
   - When a report with $S_r = \text{g.Samples}$ arrives at timestamp $t$, it maps to slot $\lfloor t / w_b \rfloor \bmod 30$ (`metrics.go:168`):
     $$W_b \leftarrow W_b + S_r, \qquad \text{Sum}_{b, m} \leftarrow \text{Sum}_{b, m} + \text{Avg}_{r, m} \cdot S_r, \qquad \text{Max}_{b, m} \leftarrow \max(\text{Max}_{b, m}, \text{Max}_{r, m})$$
   - **Post-Failover Warmup Guard (`ErrInsufficientData`, `metrics.go:71, 234`)**: After a leader election or `metrics_window` change, `usageCollector.query` returns `ErrInsufficientData` until a full `metrics_window` ($t - t_{\text{started}} \ge W_{\text{metrics}}$) has elapsed, preventing premature scale-downs on partial data.
   - **Query Aggregation (`usageAPM.Query`, `autoscaler/usage_apm.go:62`)**: Across all buckets $b$ overlapping $[t_{\text{from}}, t_{\text{to}}]$, the collector computes:
     $$\text{agg.avg} = \frac{\sum_{b} \text{Sum}_{b, m}}{\sum_{b} W_b}, \qquad \text{agg.max} = \max_{b} \text{Max}_{b, m}$$
   - Supported built-in metrics (`metric = "..."`) and legacy `nomad-apm` queries (`query = "taskgroup_<op>_<metric>"`):

| Built-in `metric` Shorthand | Legacy `nomad-apm` Query Equivalent | Underlying Metric & Operation |
| :--- | :--- | :--- |
| **`nomad.cpu`** | `avg_cpu-allocated` | Sample-weighted mean CPU % of allocated CPU across running allocs |
| **`nomad.memory`** | `avg_memory-allocated` | Sample-weighted mean Memory % of allocated memory across running allocs |
| **`nomad.cpu_mhz`** | — | Sample-weighted mean CPU usage in MHz per allocation |
| **`nomad.memory_mb`** | `avg_memory` *(scaled $\times 1024^2$ to bytes)* | Sample-weighted mean Memory usage in MiB per allocation |
| — | `max_cpu-allocated` / `max_memory-allocated` | Maximum single-allocation CPU % or Memory % observed in window |
| — | `sum_cpu-allocated` / `sum_memory-allocated` | $\text{agg.avg} \times N_{\text{runningAllocs}}$ (`usage_apm.go:78`) |

In addition to `nomad-apm`, the in-process `pluginHost` (`autoscaler/plugins.go:45`) includes a native **`prometheus`** APM plugin (`norca/norca/autoscaler/plugins/apm/prometheus/`) and an `external.Loader` (`norca/norca/autoscaler/plugins/external/`) that launches any Nomad Autoscaler SDK-compatible external APM or Strategy binary via `go-plugin` gRPC from `plugin_dir`.

### 4.3 Scaling Algorithms & Mathematical Formulations

#### 1. `target-value` Strategy (`norca/norca/autoscaler/plugins/strategy/targetvalue/plugin.go`)
Given current running count $C_{\text{curr}}$ (`int64`), latest metric value $M$, configured `target` $T$, deadband `threshold` $\epsilon$ (default `0.01`), optional `max_scale_up` $U_{\max}$, and optional `max_scale_down` $D_{\max}$:

1. **Proportional Factor Calculation (`plugin.go:141`)**:
   $$f = \begin{cases} M & \text{if } T = 0 \\ \dfrac{M}{T} & \text{if } T \neq 0 \end{cases}$$
2. **Deadband & Direction Determination (`calculateDirection`, `plugin.go:201`)**:
   $$\text{Direction} = \begin{cases} \texttt{Up} & \text{if } (C_{\text{curr}} = 0 \land f > 0) \;\lor\; (C_{\text{curr}} > 0 \land f > 1 + \epsilon) \\ \texttt{Down} & \text{if } C_{\text{curr}} > 0 \land f < 1 - \epsilon \\ \texttt{None} & \text{if } C_{\text{curr}} > 0 \land f \in [1 - \epsilon,\; 1 + \epsilon] \;\text{(or } C_{\text{curr}} = 0 \land f \le 0\text{)} \end{cases}$$
3. **Raw Desired Count & Step Clamping (`plugin.go:159-172`)**:
   $$C_{\text{raw}} = \begin{cases} \lceil f \rceil & \text{if } C_{\text{curr}} = 0 \\ \lceil C_{\text{curr}} \cdot f \rceil & \text{if } C_{\text{curr}} > 0 \end{cases}$$
   $$C_{\text{step}} = \min\Bigl(C_{\text{curr}} + U_{\max},\; \max\bigl(C_{\text{curr}} - D_{\max},\; C_{\text{raw}}\bigr)\Bigr)$$

#### 2. `threshold` Strategy (`norca/norca/autoscaler/plugins/strategy/threshold/plugin.go`)
Evaluates whether at least `within_bounds_trigger` (default $5$; must be $1$ when `query_window = "instant"`) data points in the query window fall in $[\text{lower\_bound}, \text{upper\_bound})$ (note: lower-inclusive, upper-exclusive, `plugin.go:264`). When triggered, it computes:
$$C_{\text{step}} = \begin{cases} \max(0,\; C_{\text{curr}} + \Delta) & \text{if } \texttt{delta} = \Delta \\ \max\bigl(0,\; \lceil C_{\text{curr}} \cdot (1 + p / 100) \rceil\bigr) & \text{if } \texttt{percentage} = p \\ v & \text{if } \texttt{value} = v \end{cases}$$

#### 3. `fixed-value` & `pass-through` Strategies
- **`fixed-value`** (`plugins/strategy/fixedvalue/plugin.go`): Queries no APM metrics; sets $C_{\text{step}} = \texttt{value}$ (commonly paired with a cron `schedule` block for time-of-day capacity pre-warming).
- **`pass-through`** (`plugins/strategy/passthrough/plugin.go`): Sets $C_{\text{step}} = \lceil M \rceil$ directly from the APM query result.

#### 4. Policy Clamping, Multi-Check Preemption, and Quota Ceiling
1. **Min/Max Policy Bounds (`check.go:308`)**: Each check clamps its proposed count to the policy's `[Min, Max]`:
   $$C_{\text{check}} = \min\bigl(P_{\max},\; \max(P_{\min},\; C_{\text{step}})\bigr)$$
2. **Correlated Check Groups & Winner Selection (`pickWinnerActionFromGroups`, `evaluator.go:388`, `sdk/eval.go`)**:
   - Checks sharing a `group` name ignore `ScaleDirectionNone` votes unless *all* checks in that group vote `None` (preventing inactive shards of a grouped metric from blocking a scale-down).
   - Across checks and groups, `sdk.PreemptScalingAction` picks the safest action: **`ScaleDirectionUp` always preempts `ScaleDirectionDown`**, and among actions in the same direction, the action yielding the **largest final replica count** wins:
     $$C_{\text{winner}} = \max_{c \in \text{ActiveChecks}} C_{\text{check}}(c)$$
3. **Namespace Quota Ceiling Cap (`capAtQuota`, `norca/norca/autoscaler/coordination.go:23`)**:
   - Before executing a scale-up, the runner queries `store.QuotaRemainingForNamespace` and calculates how many additional allocations of the task group's resource profile fit (`remaining.MaxAllocs(structs.TaskGroupQuotaResources(t.group, 0))`):
     $$C_{\text{ceiling}} = C_{\text{base}} + N_{\text{fit}}, \qquad C_{\text{final}} = \min(C_{\text{winner}}, C_{\text{ceiling}})$$
   - Whenever $C_{\text{winner}} > C_{\text{ceiling}}$, Norca increments `nomad.autoscaler.scale.quota_capped` and publishes a `ScalingQuotaCapped` event on the `Scaling` event stream topic (`structs/scaling_decision.go:18`).

### 4.4 Cooldowns, Deployment Gate, and Oscillation Protection

1. **Asymmetric Scale-Up / Scale-Down Cooldowns (`target.go:146`, `processor.go:86`)**:
   - Policies support separate `cooldown` (default `5m`, applied after scale-down) and `cooldown_on_scale_up` (defaults to `cooldown`, applied after scale-up).
   - Because every count mutation is persisted in the MemDB `scaling_event` table (or the custom object's `ObjectEventLog`), `targetState.cooldownUntil` reconstructs the exact cooldown expiration across Raft leader failovers (`target.go:146`).
2. **Active Deployment Gate (`evaluator.go:277`, `job_endpoint_hook_scaling.go:96`)**:
   - By default (`scale_during_deployment = "never"`), scaling is paused while the job has an active deployment.
   - Setting `scale_during_deployment = "up_only"` permits scale-up actions during an active rolling deployment as long as no canary allocations are waiting for manual promotion (`!t.unpromotedCanary`). Scale-down remains blocked until the deployment completes.
3. **Automatic Oscillation Detection & Suspension (`executor.go:121, 221`, `target.go:186`)**:
   - Before executing an action, `executor.execute` inspects the history of built-in scaling actions within `OscillationWindow` (default `10m`) plus the proposed direction:
     $$N_{\text{flips}} = \sum_{i=1}^{k} \mathbb{I}(\text{dir}_i \neq \text{dir}_{i-1})$$
   - If $N_{\text{flips}} \ge \text{OscillationThreshold}$ (default `3`, e.g., `Up -> Down -> Up -> Down`), the executor aborts the scale action, suspends the target for `OscillationSuspend` (default `15m`), and writes a durable warning `ScalingEvent` carrying `meta["nomad_autoscaler.suspended_until"] = "<unix_nano>"` to Raft so the suspension survives leader elections.

---

## 5. Disruption Budgets & Safe Node Draining (`norca/norca/disruption.go`, `drainer/`)

### 5.1 Budget Semantics (`min_healthy` & `max_unavailable`)

Service jobs (`type = "service"`) can attach a `disruption` block (`structs.DisruptionBudget`, `norca/norca/structs/disruption.go:36`) to any task group. Exactly one of `min_healthy` or `max_unavailable` must be specified, either as a non-negative integer (`"4"`) or a percentage in `0%..100%` (`"25%"`).

Following Kubernetes `PodDisruptionBudget` conventions, percentages are rounded up (`percentOfCeil`, `structs/disruption.go:136`):

$$\text{percentOfCeil}(C, p) = \left\lfloor \frac{C \cdot p + 99}{100} \right\rfloor = \left\lceil \frac{C \cdot p}{100} \right\rceil$$

Given the task group's desired count $C = \text{tg.Count}$, the number of allocations that must remain healthy (`RequiredHealthy`, `structs/disruption.go:86`) is:

$$\text{RequiredHealthy}(C) = \begin{cases} v & \text{if } \texttt{min\_healthy} = v \text{ (absolute)} \\ \left\lceil \dfrac{C \cdot p}{100} \right\rceil & \text{if } \texttt{min\_healthy} = p\% \\ \max(0,\; C - v) & \text{if } \texttt{max\_unavailable} = v \text{ (absolute)} \\ \max\left(0,\; C - \left\lceil \dfrac{C \cdot p}{100} \right\rceil\right) & \text{if } \texttt{max\_unavailable} = p\% \end{cases}$$

At job registration (`jobDisruptionHook.Validate`, `norca/norca/disruption.go:149`), if $\text{RequiredHealthy}(C) \ge C$ (for example, `min_healthy = "100%"` or `max_unavailable = 0`), Norca returns a submission warning alerting the operator that the budget blocks every voluntary disruption and node drains will stall until their deadline.

### 5.2 Health Criteria & Active Deployment Reservation

An allocation counts toward `Healthy` for disruption budgets (`Allocation.HealthyForDisruption`, `structs/disruption.go:166`) if and only if all four conditions hold:
1. `!a.TerminalStatus()` (`DesiredStatus` is not `stop`/`evict` and `ClientStatus` is not `complete`/`failed`/`lost`),
2. `a.ClientStatus == "running"`,
3. `a.DeploymentStatus.IsHealthy()` (passed task group `migrate` / `update` service and task health checks), and
4. `!a.DesiredTransition.ShouldMigrate()` (not already marked for a pending drain or eviction).

#### Coexistence with Rolling Deployments (`deploymentDisruptionReserve`, `structs/disruption.go:238`)
Rolling deployments are governed by the group's `update` stanza (`update.max_parallel`), not by the disruption budget. However, if a node drain or API eviction consumed all available disruption budget while a deployment was in progress, the next deployment step could push the group below `RequiredHealthy`.

To prevent this race, `NewDisruptionStatus` (`structs/disruption.go:214`) inspects the job's latest deployment $d$. If $d$ is active (`running`, `pending`, `initializing`, or `unblocking`) and still has allocations left to place ($\text{remaining} = d.\text{DesiredTotal} - d.\text{PlacedAllocs} > 0$), it reserves headroom for the deployment's next step:

$$\text{inFlight} = \max\bigl(0,\; d.\text{PlacedAllocs} - d.\text{HealthyAllocs} - d.\text{UnhealthyAllocs}\bigr)$$
$$\text{DeploymentReserved} = \min\Bigl(\text{remaining},\; \max\bigl(0,\; \text{tg.Update.MaxParallel} - \text{inFlight}\bigr)\Bigr)$$
$$\text{DisruptionsAllowed} = \max\bigl(0,\; \text{Healthy} - \text{RequiredHealthy} - \text{DeploymentReserved}\bigr)$$

### 5.3 How Node Drains, Evictions, and Preemption Interact with Budgets

#### 1. Safe Node Draining (`norca/norca/drainer/watch_jobs.go:345`)
When a node is placed into drain mode (`norca node drain -enable`), the leader's `drainingJobWatcher` evaluates each task group on the draining node (`handleTaskGroup`, `watch_jobs.go:345`):
- First, it computes the standard migration limit: $N_{\text{migrate}} = \text{healthy}_{\text{mig}} - (\text{tg.Count} - \text{tg.Migrate.MaxParallel})$.
- Second, if `tg.Disruption != nil`, it calls `disruptableAllocs` (`watch_jobs.go:438`), which invokes `DisruptionStatus.Disruptable(drainable)` (`structs/disruption.go:270`):
  - If $\text{Healthy} < \text{RequiredHealthy}$, `Disruptable` returns `nil` (zero allocations may be drained).
  - If $\text{Healthy} \ge \text{RequiredHealthy}$, **unhealthy candidate allocations on the draining node are returned first** (because stopping an already-unhealthy allocation does not reduce `Healthy`), followed by at most `DisruptionsAllowed` healthy allocations.
- Third, the drainer applies the stricter of the two limits (`watch_jobs.go:427`):
  $$N_{\text{toDrain}} = \min\bigl(|\text{disruptable}|,\; N_{\text{migrate}}\bigr)$$
- `disruptionBudgetIndex` (`watch_jobs.go:516`) registers MemDB watches on the `jobs`, `allocs`, and `deployment` tables so that as soon as a replacement allocation becomes healthy on another node (or an active deployment finishes), the drainer immediately unblocks and drains the next batch.
- **Deadline Escape Hatch (`handleDeadlinedNodes`, `drainer/drainer.go:246`)**: Unless `-no-deadline` is set, when a draining node's `DrainStrategy.Deadline` expires (coalesced within `NodeDeadlineCoalesceWindow = 5s`), the drainer force-migrates all remaining allocations on the node regardless of the disruption budget.

#### 2. Atomic Allocation Eviction API (`Alloc.Evict`, `norca/norca/disruption.go:30`)
Operators and external automation (such as cluster-autoscaler node scale-down hooks or spot termination handlers) can safely evict individual allocations via `POST /v1/allocation/:id/evict` or `norca alloc stop -respect-disruption <alloc-id>`:
- The leader submits a `structs.AllocEvictRequestType` Raft log entry containing the target `AllocID` and a pre-generated `Evaluation` (`disruption.go:87`).
- Inside the Raft FSM write transaction (`StateStore.EvictAlloc`, `norca/norca/state/state_store_disruption.go:23`), the state store computes `allocDisruptionStatusTxn` against the live MemDB state.
- **Concurrency Safety**: Because the check and the `DesiredTransition{Migrate: true}` mutation execute inside the same single-threaded Raft FSM write transaction (`WriteTxnMsgT`), **concurrent eviction requests can never race to breach the budget**.
- If the budget forbids the eviction, `EvictAlloc` aborts the transaction and returns `structs.ErrDisruptionBudgetExceeded`, which the HTTP handler (`allocEvict`, `command/agent/disruption_endpoint.go:43`) translates into **`HTTP 429 Too Many Requests`** with header **`Retry-After: 5`** (`DisruptionRetryAfterSeconds = 5`).

#### 3. Scheduler Preemption (`norca/scheduler/preemption.go`)
Involuntary preemption by higher-priority jobs (where $\text{Priority}_{\text{preemptor}} - \text{Priority}_{\text{victim}} \ge 10$, `preemption.go:313`) is an involuntary capacity reclamation mechanism and is **not** blocked by `DisruptionBudget` (`structs/disruption.go:34`). Instead, `Preemptor` protects victim task groups by applying `maxParallelPenalty = 50.0` (`preemption.go:16`) in `scoreForTaskGroup` whenever the number of allocations preempted from a victim task group in the current plan would exceed that group's `migrate.max_parallel`, and scores candidate nodes via the logistic curve (`rank.go:868`):

$$S_{\text{preempt}} = \frac{1}{1 + \exp\bigl(0.0048 \cdot (\text{netPriority} - 2048)\bigr)}, \qquad \text{netPriority} = P_{\max} + \frac{\sum P_i}{P_{\max}}$$

---

## 6. Jobspec & Server HCL Configuration Reference

### 6.1 Server Configuration (`norca.hcl`)

```hcl
region     = "eu-central"
datacenter = "eu-central-1a" # 1 Datacenter = 1 Availability Zone

server {
  enabled          = true
  bootstrap_expect = 3

  # Default fraction of instances a datacenter must hold before
  # topology-aware service discovery spills over to the rest of the region.
  default_overflow_fraction = 0.2

  # Built-in Horizontal Workload Autoscaler (norca/command/agent/config_autoscaler.go)
  autoscaler {
    enabled                     = true
    default_engine              = "builtin" # "builtin" or "external"
    min_eval_interval           = "10s"
    min_cooldown                = "10s"
    default_evaluation_interval = "10s"
    default_cooldown            = "5m"

    eval_workers            = 10
    rate_limit              = 10.0 # Max Job.Scale actions/sec cluster-wide
    rate_limit_burst        = 50
    metrics_window          = "5m" # Leader ring-buffer retention window
    client_metrics_interval = "10s" # Client usage aggregation & push interval

    # Optional directory for external go-plugin APM / Strategy binaries
    plugin_dir = "/opt/norca/plugins/autoscaler"

    # Built-in Prometheus APM instance
    apm "prometheus" {
      driver = "prometheus"
      config = {
        address = "https://prometheus.internal:9090"
      }
    }
  }
}
```

### 6.2 Workload Jobspec Configuration (`job.norca.hcl`)

```hcl
job "checkout" {
  region      = "eu-central"
  datacenters = ["eu-central-1a", "eu-central-1b", "eu-central-1c"]
  type        = "service"

  meta {
    app  = "checkout"
    tier = "backend"
  }

  group "api" {
    count = 6

    # 1. Even AZ / Datacenter Distribution
    spread {
      attribute = "${node.datacenter}"
      weight    = 100
    }

    # 2. Cross-Job Affinity & Anti-Affinity (norca/norca/structs/alloc_affinity.go)
    # Hard anti-affinity: never place two checkout-api replicas on the same host
    separate {
      selector = { app = "checkout", role = "api" }
      scope    = "${node.unique.id}"
      required = true
    }

    # Soft cross-job affinity: prefer hosts in the same AZ as the local redis cache job
    colocate {
      selector  = { app = "redis-cache" }
      namespace = "infra"
      scope     = "${node.datacenter}"
      weight    = 80
    }

    # 3. Voluntary Disruption Budget (norca/norca/structs/disruption.go)
    disruption {
      min_healthy = "75%" # Or: max_unavailable = "25%" (or absolute integer, e.g. 4)
    }

    # 4. Built-in Horizontal Autoscaling (norca/norca/structs/scaling_check.go)
    scaling {
      enabled = true
      min     = 3
      max     = 24

      policy {
        evaluation_interval     = "15s"
        cooldown                = "3m"
        cooldown_on_scale_up    = "30s"
        on_check_error          = "ignore"
        scale_during_deployment = "up_only" # Allow scale-up during active deployments

        # Typed check using built-in client-pushed usage metrics (zero Client RPCs)
        check "cpu-utilization" {
          metric         = "nomad.cpu"
          query_window   = "2m"
          strategy       = "target-value"
          target         = 70
          threshold      = 0.05
          max_scale_up   = 6
          max_scale_down = 2
        }

        # Correlated check using built-in Prometheus APM and threshold strategy
        check "p99-latency-spike" {
          source                = "prometheus"
          query                 = "histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{app=\"checkout\"}[1m])) by (le))"
          query_window          = "1m"
          strategy              = "threshold"
          lower_bound           = 0.25
          delta                 = 2
          within_bounds_trigger = 3
        }

        # Business-hours capacity floor using schedule + fixed-value
        check "business-hours-floor" {
          strategy = "fixed-value"
          value    = 8
          schedule {
            start    = "0 8 * * 1-5"
            duration = "10h"
          }
        }
      }
    }

    meta {
      role = "api"
    }

    service {
      name     = "checkout-api"
      provider = "nomad"
      port     = "http"

      # 5. AZ-Local Service Discovery with Spillover Protection
      discovery {
        prefer            = "datacenter"
        min_local         = 2
        overflow_fraction = 0.2
        failover          = ["eu-west"]
      }
    }

    task "server" {
      driver = "docker"
      config {
        image = "registry.internal/checkout:v2.4.0"
      }
      resources {
        cpu    = 1000
        memory = 1024
      }
    }
  }
}
```

---

## 7. CLI Command & HTTP API Cheat-Sheet

### 7.1 CLI Commands

| Command | Action & Purpose | Key Flags |
| :--- | :--- | :--- |
| **`norca job status <job>`** | Displays job status including the **Disruption Budgets** table (`Healthy`, `Required`, `Reserved`, `Disruptions Allowed`, `command/job_status_disruption.go:15`) | `-verbose`, `-namespace` |
| **`norca alloc stop -respect-disruption <alloc>`** | Safely evicts and reschedules an allocation via `Alloc.Evict` only if the task group's disruption budget allows it (`command/alloc_stop.go:51`) | `-respect-disruption`, `-no-shutdown-delay`, `-detach` |
| **`norca node drain -enable <node>`** | Drains a node while respecting both `migrate.max_parallel` and `disruption` budgets until `-deadline` expires (`command/node_drain.go:29`) | `-deadline 1h`, `-no-deadline`, `-ignore-system`, `-force` |
| **`norca scaling policy list`** | Lists scaling policies across jobs (`command/scaling_policy_list.go:25`) | `-job <id>`, `-type horizontal`, `-json` |
| **`norca scaling policy info <id>`** | Inspects a specific scaling policy configuration | `-json`, `-verbose` |
| **`norca job scale <job> <group> [count]`** | Views scale status or manually scales a task group | `-message` |
| **`norca job scaling-events <job>`** | Displays recent scaling events, including built-in autoscaler decisions and oscillation suspension warnings (`command/job_scaling_events.go:27`) | `-verbose` |
| **`norca operator event-stream -topic Scaling`** | Streams real-time `ScalingDecision` and `ScalingQuotaCapped` events from the leader (`structs/scaling_decision.go:7`) | `-topic Scaling`, `-namespace` |

### 7.2 HTTP API Endpoints

| Method & Endpoint | RPC Method | Description |
| :--- | :--- | :--- |
| **`GET /v1/job/:id/disruption`** | `Job.Disruption` | Returns `map[string]*DisruptionStatus` for all task groups with a `disruption` block. Supports blocking queries (`?index=`). |
| **`POST /v1/allocation/:id/evict`** | `Alloc.Evict` | Atomically checks the task group's disruption budget and marks the allocation for migration. Returns `200 OK` (`{"EvalID": "...", "Index": ...}`) or **`429 Too Many Requests`** with header **`Retry-After: 5`**. |
| **`GET /v1/scaling/policies`** | `Scaling.ListPolicies` | Lists scaling policies; supports `?engine=builtin` or `?engine=external` filter (`scaling_endpoint.go:42`). |

---

## 8. Operational Takeaways for SREs and Capacity Planners

1. **Cross-Namespace `colocate` / `separate` ACL Requirements**: When a job's `colocate` or `separate` block specifies `namespace = "other"`, the ACL token submitting `norca job run` or `norca job plan` must possess `read-job` in `"other"` (`norca/norca/job_endpoint_hook_alloc_affinity.go:98`), preventing unauthorized tenants from probing workload topology in foreign namespaces.
2. **Avoid `100%` Disruption Budgets in Production**: Setting `min_healthy = "100%"` or `max_unavailable = 0` sets `RequiredHealthy == DesiredCount`. Because `DisruptionsAllowed = max(0, Healthy - RequiredHealthy - DeploymentReserved)`, `DisruptionsAllowed` will permanently be `0`, causing `norca alloc stop -respect-disruption` to return `429 Too Many Requests` and causing node drains to stall until their force-stop deadline. Always leave at least 1 replica of slack (or use `max_unavailable = 1` / `"10%"`).
3. **Post-Failover Autoscaler Warmup (`metrics_window`)**: Client usage reports (`TaskGroupUsageReport`) are held exclusively in leader memory to avoid Raft log amplification. Following a Raft leader election, checks using `nomad.cpu`, `nomad.memory`, `nomad.cpu_mhz`, or `nomad.memory_mb` wait for one full `metrics_window` (default `5m`) before acting (`ErrInsufficientData`). Keep `metrics_window` between `2m` and `5m` for responsive post-failover recovery.
4. **Oscillation Suspension Recovery**: If a workload's scaling policy flips direction `3` times within `10m`, the built-in autoscaler suspends the policy for `15m` and writes an error `ScalingEvent` visible in `norca job scaling-events -verbose <job>`. Widening the `target-value` `threshold` deadband (e.g., from `0.01` to `0.05`) or increasing `cooldown` prevents rapid scale-up/scale-down thrashing.
