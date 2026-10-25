# Kubernetes features in the Norca fork – detailed specification

**Status:** In progress · 2026-10-24 · Starting point: Nomad v1.6.5 (MPL 2.0) · Related document: [nomad-fork-development-plan.md](nomad-fork-development-plan.md)

Status per item:

- **1.** Namespace quotas: Implemented
- **2.** Dynamic admission control: Implemented
- **3.** Event stream extension: Implemented
- **4.** Cross-job affinity and anti-affinity: Implemented
- **5.** Disruption budget: In progress
- **6.** Health check roles: Implemented
- **7.** Network rules: Implemented (levels A and B; level C dropped)
- **8.** Operator framework: Plan

This document is the detailed specification of the seven items listed in the "Features adopted from Kubernetes" chapter of the development plan, plus the operator framework (item 8) built on top of them. The numbering matches the plan. Every chapter follows the same structure: what Kubernetes does, what already exists in Nomad 1.6.5, what we propose (semantics, jobspec, API, data model), where the code has to change, and the acceptance criteria.

Code references are to the Nomad v1.6.5 source code and were verified at the time of writing. Files and types marked "new" are proposed names.

## Contents

- [Common principles](#common-principles)
- [1. Namespace quotas](#1-namespace-quotas)
- [2. Dynamic admission control](#2-dynamic-admission-control)
- [3. Event stream extension](#3-event-stream-extension)
- [4. Cross-job affinity and anti-affinity](#4-cross-job-affinity-and-anti-affinity)
- [5. Disruption budget](#5-disruption-budget)
- [6. Health check roles](#6-health-check-roles)
- [7. Network rules](#7-network-rules)
- [8. Operator framework](#8-operator-framework)
- [Summary table](#summary-table)

## Common principles

**State in Raft.** Every new object (quota, admission policy, network rule, disruption budget) goes through the Raft log into the memdb state store (`nomad/state/schema.go`). No separate database is needed. The rationale is in the SQLite section of the development plan.

**New Raft message types.** The new write operations get new `MessageType` values at the end of the existing enumeration in `nomad/structs/structs.go`. Old servers can ignore an unknown type for messages marked with the `IgnoreUnknownTypeFlag` (128) bit. The new types must be sent with this flag wherever ignoring them is safe.

**Version gate.** Every new feature is enabled only when all servers in the region run at least the new version. The existing `ServersMeetMinimumVersion` helper (`nomad/util.go`) can be used for this.

**Backward compatibility.** The new jobspec fields are optional, and when they are absent the behavior is exactly today's. In msgpack encoding the new fields can be read with an empty value.

**ACL.** Every new object type gets its own ACL rule following the existing patterns (`acl/policy.go`). Where Nomad already defines such a rule (for example `quota`), we use it.

**Event notification.** Every new object type gets its own event stream topic (`nomad/structs/event.go`), so that external controllers and the history service can follow the changes.

**License.** Kubernetes code (Apache 2.0) may be taken over only in a separate file, with the original license header. HashiCorp's Enterprise code is not public, and nothing can be taken from the BSL-licensed Nomad code after v1.6.6. Every implementation described here is an independent design.

---

## 1. Namespace quotas

### Summary

The CPU, memory and number of allocations that can be reserved can be limited per namespace and per region, and task-level default and maximum resource values can be specified. In Nomad 1.6.5 the client side of quota handling, the API and the scheduler hook points almost entirely exist; only the server-side implementation is missing. Of the seven items, this has the best effort-to-value ratio.

### How does Kubernetes work?

A **ResourceQuota** is an object belonging to a namespace that sets a hard limit on the resource requests of all pods in the namespace (CPU, memory requests and limits) and on the number of objects. The admission layer rejects a pod that would exceed the limit. A **LimitRange** gives per-container default, minimum and maximum values, and fills in missing values during admission.

### Starting point in Nomad 1.6.5

A significant part of the elements needed for quotas already exists:

| Element | State | Location |
| --- | --- | --- |
| Go API client | Complete: `QuotaSpec`, `QuotaLimit`, `QuotaUsage`, `List`, `Info`, `Usage`, `Register`, `Delete` | `api/quota.go` |
| CLI | Complete: `nomad quota apply / delete / init / inspect / list / status` | `command/quota_*.go` |
| HTTP endpoints | Registered, but return 501 (`entOnly`) | `command/agent/http_oss.go` |
| Namespace reference | The `Namespace` struct has a `Quota` field | `nomad/structs/structs.go` |
| ACL | There is a `quota` rule block (`QuotaPolicy`) | `acl/policy.go` |
| Scheduler | Empty `NewQuotaIterator` | `scheduler/stack_oss.go` |
| Plan apply | Empty `evaluatePlanQuota` | `nomad/plan_apply_oss.go` |
| FSM | Empty `allocQuota` | `nomad/fsm_oss.go` |
| Blocked evaluations | Working quota mechanism: `QuotaLimitReached` field, `UnblockQuota` | `nomad/blocked_evals.go`, `scheduler/context.go` |

According to the existing API, a quota consists of per-region limits (`QuotaLimit.Region`, `RegionLimit *Resources`, `VariablesLimit`), which fits the "one region per cloud" setup proposed in the multi-cloud chapter.

### Proposed semantics

- One quota can be assigned to several namespaces (via the namespace's `quota` field). Usage is the sum over all assigned namespaces.
- **What counts:** the reserved resources of every non-terminal allocation: `cpu` (MHz), `cores` (converted to MHz based on the node's CPU data), `memory` (MB) and optionally `memory_max` (with memory oversubscription). The stored size of Variables follows the existing `VariablesLimit`.
- **New limit in the fork:** `allocations`, that is, the maximum number of non-terminal allocations.
- **When exceeded:** the job submission succeeds, but the scheduler places allocations only up to the limit. The remaining evaluations become blocked, marked with `QuotaLimitReached`, and are restarted automatically when quota is freed (an allocation stops, the quota is raised) using the existing `UnblockQuota` mechanism.
- **Strict mode (optional):** with `enforce = "submit"`, `job run` / `job plan` rejects a job already at submission if its total demand (`count × resources`) would by itself exceed the limit.
- **Task-level defaults and limits (LimitRange equivalent):** the new `task_defaults` and `task_max` blocks of the quota specification. Missing resource values are filled in by the system during admission, and a value above the maximum is rejected.

### Configuration

```hcl
name        = "team-a"
description = "Production quota of team A"

limit {
  region = "eu-aws"
  region_limit {
    cpu       = 40000   # MHz
    memory    = 81920   # MB
  }
  variables_limit = 50   # MB
  allocations     = 200  # new field in the fork
}

# New blocks in the fork (LimitRange equivalent)
task_defaults {
  cpu    = 200
  memory = 256
}
task_max {
  cpu    = 8000
  memory = 16384
}

enforce = "schedule"   # or "submit"
```

Assignment: `nomad namespace apply -quota team-a prod` (using the existing CLI flag).

### API and CLI

The existing endpoints and commands start working in unchanged form: `GET /v1/quotas`, `GET/POST/DELETE /v1/quota/:name`, `GET /v1/quota-usages`, `GET /v1/quota/usage/:name`. Since `api/quota.go` and `command/quota_*.go` already exist, the Terraform Nomad provider and existing scripts can use them too. The new fields (`allocations`, `task_defaults`, `task_max`, `enforce`) are optional, so old clients can also manage quotas.

### Data model

- New server-side types under `nomad/structs/`: `QuotaSpec`, `QuotaLimit`, `QuotaUsage` (the fields match those of the `api` package, extended with the fork's new fields).
- New memdb tables: `quota_specs` (index by name) and `quota_usage` (index by name). Usage is updated in the state store in the same transaction as the allocation writes, so it is always consistent for the plan applier.
- New Raft messages: `QuotaSpecUpsertRequestType`, `QuotaSpecDeleteRequestType`.
- New event topic: `Quota` (quota creation, modification and deletion, plus "quota exhausted" and "quota freed" events).

### Implementation

1. **State store:** new tables and upsert/delete functions; when allocations are written (`UpsertPlanResults`, `UpdateAllocsFromClient`), the usage of the affected quota is updated (`nomad/state/state_store.go`, new `nomad/state/state_store_quota.go`).
2. **FSM:** implement `allocQuota` (looks up the quota from the allocation's namespace), handle the new message types (`nomad/fsm.go`).
3. **Plan apply:** implement `evaluatePlanQuota`. The final, authoritative check happens here, because only here is the effect of all parallel plans visible (`nomad/plan_apply.go`).
4. **Scheduler:** implement `NewQuotaIterator`, which filters out placements that are already over the limit and signals the reason for blocking by calling `EvalEligibility.SetQuotaLimitReached` (`scheduler/stack.go`, new `scheduler/quota.go`).
5. **RPC and HTTP:** new `Quota` RPC endpoint (`nomad/quota_endpoint.go`), replacing the `entOnly` registrations in `http_oss.go` with real handlers.
6. **Admission:** filling in `task_defaults` as a mutator, `task_max` and `enforce = "submit"` as validators in the existing chain (`nomad/job_endpoint_hooks.go`).
7. **Blocked evaluations:** call `UnblockQuota` when a quota changes and when an allocation stops.

### Testing and acceptance

- Two parallel jobs that together would exceed the limit: the plan applier allows exactly up to the limit, and the remainder becomes a blocked evaluation marked `QuotaLimitReached`.
- After the quota is raised, the blocked evaluations run automatically.
- The existing `command/quota_*_test.go` tests (which are currently excluded by the `ent` build tag) are green in the OSS build as well.
- The behavior of namespaces without a quota is unchanged, and scheduling performance does not degrade measurably.

### Open questions

- Should `memory_max` count toward the limit, or only `memory`?
- Are GPU and device limits (`device` resources) needed in the first version?

---

## 2. Dynamic admission control

### Summary

The validation and mutation chain that runs at job submission becomes configurable: with external webhooks and with rules stored in Raft and written as CEL expressions. This replaces the Enterprise-only Sentinel, and makes it possible to enforce company rules without modifying code.

### How does Kubernetes work?

The API server passes every write request through an admission chain. **Mutating admission webhooks** can modify the object (with a JSON Patch response), **validating webhooks** can reject it. The rules of a **ValidatingAdmissionPolicy** are given as CEL expressions, as an object stored in the cluster, without an external service. For webhooks, error handling (`failurePolicy: Fail` or `Ignore`) and the timeout can be configured.

### Starting point in Nomad 1.6.5

- Both the `Job.Register` and `Job.Plan` RPCs go through the `admissionControllers` function (`nomad/job_endpoint.go`, lines 112 and 1743). `Job.Register` first forwards the request to the leader (line 92), so admission always runs on the leader.
- The two parts of the chain are `admissionMutators` and `admissionValidators` (`nomad/job_endpoint_hooks.go`). The built-in hooks: `jobCanonicalizer`, `jobConnectHook`, `jobExposeCheckHook`, `jobImpliedConstraints`, `jobNodePoolMutatingHook`, `jobVaultHook`, `jobValidate`, `memoryOversubscriptionValidate`, `jobNodePoolValidatingHook`.
- The place of Sentinel: `enforceSubmitJob` (`nomad/job_endpoint_oss.go`), which is empty in the OSS version.

### Proposed semantics

**Order of the chain:**

1. built-in mutators (in today's order);
2. **mutating webhooks** (in configured order);
3. filling in quota `task_defaults` (from item 1);
4. canonical form and built-in validators;
5. **CEL rules** (admission policies stored in Raft);
6. **validating webhooks**.

Mutators run before validators, so validators see the final job. `job plan` runs the same chain, so the user sees the rejection or modification already at planning time.

**Error handling:** per webhook, `failure_policy = "fail"` (default) or `"ignore"`, and `timeout` (default 5 s). Since the chain runs on the leader, a slow webhook slows down job submission, not the other operations of the cluster.

**Exclusions:** webhooks can be restricted with a namespace filter, and system namespaces can be excluded. This is important because if the webhook service itself runs as a Nomad job, it must not block its own redeployment (a well-known trap in Kubernetes as well).

**Warning mode:** for CEL rules, setting `action = "warn"` makes the rule only emit a warning (on the existing `warnings` channel) rather than reject. This is useful for the gradual rollout of new rules.

### Configuration

Webhooks in the server configuration:

```hcl
server {
  admission {
    webhook "image-policy" {
      url            = "https://admission.internal:8443/validate"
      mode           = "validate"       # or "mutate"
      timeout        = "5s"
      failure_policy = "fail"           # or "ignore"
      namespaces     = ["prod", "staging"]
      ca_file        = "/etc/nomad/admission-ca.pem"
    }
  }
}
```

A CEL rule as a cluster object (`nomad admission-policy apply rule.hcl`):

```hcl
name        = "no-privileged-container"
description = "Privileged Docker containers are forbidden in the production namespace"
action      = "deny"   # or "warn"

match {
  namespaces = ["prod"]
}

rule {
  expression = "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))"
  message    = "Privileged containers are not allowed in the prod namespace."
}
```

### Webhook protocol

The request and response follow the structure of the Kubernetes AdmissionReview, so that existing tools and experience are transferable:

- **Request:** `uid`, `operation` (`register` or `plan`), `namespace`, `user` (the token's name and roles), and the complete job in the Nomad API JSON format.
- **Response:** `uid`, `allowed` (true/false), `message`, `warnings`, and in mutating mode optionally `patch` (RFC 6902 JSON Patch).

### API and CLI

- `nomad admission-policy apply / list / inspect / delete`, HTTP: `/v1/admission-policies`, `/v1/admission-policy/:name`.
- Webhooks can be specified only in the server configuration, not through the API. This is intentional: this way a compromised token cannot register its own webhook that could see every job.

### Data model

- New type: `AdmissionPolicy` (`Name`, `Description`, `Action`, `Match`, `Rules`), new memdb table: `admission_policies`.
- New Raft messages: `AdmissionPolicyUpsertRequestType`, `AdmissionPolicyDeleteRequestType`.
- New ACL rule: `admission { policy = "read" | "write" }` (operator level).
- New event topic: `AdmissionPolicy`, and on rejection a `JobAdmissionDenied` event (who, what, which rule), which goes into the history and audit service.

### Implementation

1. **CEL environment:** using the `cel-go` library (Apache 2.0); the job is available as a typed CEL variable (`job`) built from the API JSON form. Rules are compiled at submission, and if there is a compile error the rule is not even stored (new `nomad/admission/cel.go`).
2. **Webhook client:** with a timeout, without retries, over TLS, with a connection pool (new `nomad/admission/webhook.go`).
3. **Extending the chain:** extend the `admissionMutators` and `admissionValidators` lists in the order above (`nomad/job_endpoint_hooks.go`).
4. **Sentinel slot:** `enforceSubmitJob` runs the CEL rules (a new implementation instead of `nomad/job_endpoint_oss.go`).
5. **Metrics:** per-webhook call count, latency and error count in the existing `go-metrics` framework.

### Testing and acceptance

- A `meta` field added by a mutating webhook is also visible in the `job plan` diff.
- With `failure_policy = "fail"`, an unreachable webhook rejects the submission; with `"ignore"` it lets it through with a warning.
- A deliberately invalid CEL expression cannot be saved.
- Without admission policies, the submission latency does not change measurably.

### Open questions

- Should admission also run on Variables writes? This would be useful for schema validation in the CRD-like use of item 3. **Decided 2026-10-24: yes, together with phase 2 of the operator framework.**
- Is a mutating CEL needed (the equivalent of Kubernetes MutatingAdmissionPolicy), or is the webhook enough? **Decided 2026-10-24: the webhook is enough; no mutating CEL for now.**

---

## 3. Event stream extension

### Summary

The event stream is extended with Variables changes and the fork's new object types, and with a reliable "resume or resync" signal. This makes it possible to write external controllers following the Kubernetes list + watch pattern, and to use Variables as a lightweight equivalent of CRDs.

### How does Kubernetes work?

Every resource can be listed and watched. The client receives the current state with a `resourceVersion` value, and from then on the changes. If the requested version is already too old, the server answers `410 Gone`, and the client resynchronizes with a full re-list. Controllers (operators) are built on this pattern, and CRDs make it possible to define custom object types.

### Starting point in Nomad 1.6.5

- The fixed list of topics: `Deployment`, `Evaluation`, `Allocation`, `Job`, `Node`, `NodePool`, `ACLPolicy`, `ACLToken`, `ACLRole`, `ACLAuthMethod`, `ACLBindingRule`, `Service` (`nomad/structs/event.go`). There is no Variables topic.
- Events are held in an in-memory buffer whose default size is 100 (`event_buffer_size`, `nomad/config.go`).
- The `index` parameter of the HTTP API already exists (`command/agent/event_endpoint.go`). If the requested index is no longer in the buffer, the broker continues from the closest available index (`StartAtClosest`, `nomad/stream/event_broker.go`), and reports this only in a debug-level log. The client therefore cannot tell that it lost events.
- Internally there is a `StartExactlyAtIndex` switch, which returns an error in that case (`nomad/stream/subscription.go`), but the HTTP API does not expose it.

### Proposed semantics

**Exact resume:** the new `exact=true` parameter of the HTTP API sets the internal `StartExactlyAtIndex` switch. If the requested index has already dropped out of the buffer, the server responds with a `410 Gone` status code, and the client resynchronizes following the list + watch pattern. Without the parameter, today's behavior remains.

**Variables topic:** only metadata about the changes goes into the event (namespace, path, `CreateIndex`, `ModifyIndex`, the type of modification), never the encrypted content. Subscribing requires the Variables `list` capability for the given path pattern, so the ACL rules also apply to events.

**New topics for the fork's other features:** `Quota`, `AdmissionPolicy`, `NetworkPolicy`, `Scaling`, and event types such as `JobAdmissionDenied` or `QuotaExhausted`.

**Larger buffer:** the new default is 1,000 events. The memory requirement per event depends on the payload size, which must be verified with a load test.

### CRD pattern over Variables

Today Variables is an untyped, ACL-protected, encrypted key–value store. With a path convention and the events above, custom object types can be managed:

```
crd/<group>/<kind>/<name>
crd/db-example-com/PostgresCluster/billing
```

Variables paths allow only `a-z A-Z 0-9 - _ ~ /`; dots are excluded because they act as separators in template references. A group name in domain form is therefore written with dashes instead of dots (`db.example.com` → `db-example-com`).

- The **schema** can be given in a companion Variables entry (`crd-schema/<group>/<kind>`); validation can be done by the CEL rules of item 2 if admission is extended to Variables writes (this is an open question of item 2).
- The **controller** subscribes to the `Variables` topic with the pattern `crd/db-example-com/PostgresCluster/*`, lists at startup, and reacts on change (for example starts or modifies a Nomad job).
- A small, MPL-licensed **Go controller SDK** (the separate Go module `nomad/controller` in this repository) provides the list + watch loop, handling of `exact=true`, retries and leader election. Variables locks appeared only in Nomad 1.7 and do not exist in 1.6.5, so leader election is built on the check-and-set write of Variables (with the `cas` parameter, based on `ModifyIndex`): whoever's write succeeds wins, and renews the entry periodically.

### API and CLI

- `GET /v1/event/stream?topic=Variables:crd/db-example-com/*&index=1234&exact=true`
- `nomad operator api` and the existing `nomad event` command do not change, they only get new topics.

### Implementation

1. Generate events with metadata on the Variables write path (`nomad/state/state_store_variables.go`), following the existing event-building pattern (`nomad/state/events.go`).
2. New topics and event types (`nomad/structs/event.go`), ACL filtering at subscription time (`nomad/event_endpoint.go`, `nomad/stream/event_broker.go`).
3. Processing the `exact` parameter and the `410` response (`command/agent/event_endpoint.go`).
4. Raising the default of `event_buffer_size` (`nomad/config.go`, `command/agent/config.go`).
5. The controller SDK: the separate Go module `nomad/controller` in this repository (module `github.com/hashicorp/nomad/controller`, depending only on the API module `nomad/api`).

### Testing and acceptance

- The event of a Variables modification does not contain the encrypted content, and arrives only with the appropriate ACL.
- With `exact=true`, an index that dropped out of the buffer yields `410`; without the parameter, today's behavior remains.
- A sample controller continues without losing events after a server restart and a leader change, by resynchronizing.

---

## 4. Cross-job affinity and anti-affinity

### Summary

The placement of a task group can be aligned with the allocations of other jobs: close to them (affinity, for example a cache next to the API) or away from them (anti-affinity, for example two database replicas must not end up in a common failure domain). This can be specified both as a mandatory rule and as a weighted preference.

### How does Kubernetes work?

**podAffinity** and **podAntiAffinity** use a label selector to designate the pods to be related to, and a `topologyKey` to designate the domain within which proximity counts (node, zone, region). There are two variants: mandatory (`requiredDuringSchedulingIgnoredDuringExecution`) and weighted preference (`preferredDuringScheduling...`). The rule applies only at scheduling time, does not move already running pods, and is not symmetric. This is one of the most expensive rules of the Kubernetes scheduler.

### Starting point in Nomad 1.6.5

The scheduler's iterator chain (`scheduler/stack.go`) currently knows these placement rules:

| Iterator | What it considers | File |
| --- | --- | --- |
| `NodeAffinityIterator` | Node attributes (weighted) | `scheduler/rank.go` |
| `SpreadIterator` | Distribution by node attribute | `scheduler/spread.go` |
| `DistinctHostsIterator` | The same job must not land on a node twice | `scheduler/feasible.go` |
| `DistinctPropertyIterator` | The same job at most N times per attribute value | `scheduler/feasible.go` |
| `JobAntiAffinityIterator` | Allocations of the same job should be spread out | `scheduler/rank.go` |

Each of them examines either node attributes or the allocations of the **same** job. There is no rule relating to the allocations of other jobs.

### Proposed semantics

- **Selection:** a selector picks the target allocations based on the `meta` fields of the job and the task group (the jobspec's existing `meta` block plays the role of labels). By default it searches only in its own namespace; another namespace can be specified explicitly.
- **Topology domain (`scope`):** a node-attribute interpolation, for example `${node.unique.id}` (node), `${node.datacenter}`, `${attr.platform.aws.placement.availability-zone}` (zone) or a custom `${meta.rack}`.
- **Mandatory rule** (`required = true`): a feasibility filter. For affinity, only nodes whose domain contains a matching allocation remain; for anti-affinity, only nodes whose domain contains none.
- **Preference** (`weight`, between −100 and 100): scoring, consistent with the normalization logic of the existing `NodeAffinityIterator`.
- **Within the same evaluation**, allocations that are already planned but not yet placed also count (the contents of the plan), so it also works for a job's own replicas.
- **Limitations, matching Kubernetes:** the rule applies only at placement time, does not move running allocations, and is not symmetric (if A avoids B, B can still be placed next to A). This must be stated clearly in the documentation.

### Jobspec

```hcl
job "cache" {
  meta { app = "cache" }

  group "redis" {
    # Place next to the API (on the same node), if possible
    colocate {
      selector { app = "api" }
      scope    = "${node.unique.id}"
      weight   = 50
    }

    # Do not end up in the same zone as another cache replica
    separate {
      selector { app = "cache" }
      scope    = "${attr.platform.aws.placement.availability-zone}"
      required = true
    }
  }
}
```

### Implementation

1. **Jobspec and structs:** new `colocate` and `separate` blocks (`jobspec2/`, `nomad/structs/structs.go`: new `AllocAffinity` type under `TaskGroup`), validation (`scope` can be interpolated, `weight` range, selector not empty).
2. **Data collection once per evaluation:** in the stack's `SetJob` step, collect the jobs matching the selector from the namespace, then group their non-terminal allocations by domain (domain value → count). This is done once, from the snapshot, not per node.
3. **New iterators:** `AllocAffinityFeasibilityIterator` for the mandatory rules (`scheduler/feasible.go`), `AllocAffinityRankIterator` for the preferences (`scheduler/rank.go`), inserted into the chains of `GenericStack` and `SystemStack` (`scheduler/stack.go`).
4. **Explanation:** in the `AllocMetric` data of blocked or failed placements, the number of filtered nodes and the name of the rule, so that `nomad job status` and `job plan` show why the placement failed.

### Performance

The cost is proportional to the number of matching allocations, not of nodes, because data collection happens once per evaluation. Proposed limits: at most 8 rules per task group, and a warning if a selector matches a very large number of jobs. The acceptance test measures the scheduling latency in a simulated cluster of 10,000 nodes.

### Testing and acceptance

- With mandatory anti-affinity, two replicas never land in a common domain; if there are not enough domains, the evaluation becomes blocked with a clear reason.
- A preference adds a shift to the score proportional to the weight, but does not override mandatory rules and resource filters.
- The scheduling time of jobs without rules does not change.

---

## 5. Disruption budget

### Summary

For a task group it can be specified, for voluntary disruptions (node drain, node pool scale-down, explicit eviction), how many instances must remain healthy at least, or how many may be unavailable at most at the same time. The existing `migrate` block can already do part of this; the fork generalizes and extends it.

### How does Kubernetes work?

A **PodDisruptionBudget** (PDB) selects pods with a label selector and gives a `minAvailable` or `maxUnavailable` value (as a count or a percentage). Voluntary disruptions (node drain, eviction through the Eviction API) are permitted only if the budget allows; if not, the Eviction API answers `429 Too Many Requests`, and the drain waits. Reducing the replica count (scaling down a Deployment) is not an eviction, so the PDB does not apply to it.

### Starting point in Nomad 1.6.5

The drainer already limits at the job level: `handleTaskGroup` (`nomad/drainer/watch_jobs.go`) counts all healthy allocations of the job and allows migrating only as many as keeps at least `count − migrate.max_parallel` healthy (`thresholdCount := tg.Count - tg.Migrate.MaxParallel`). This applies jointly to all nodes being drained at the same time.

What is missing compared with the Kubernetes PDB:

- The limit applies **only to drain**. `nomad alloc stop`, a future node pool scale-down or other voluntary disruptions do not take it into account.
- It can be specified only in the form **"at most N migrations at a time"**, relative to `count`. An absolute minimum (`min_healthy`) or a percentage cannot.
- There is **no eviction API** that would signal to an external tool that no allocation may be stopped right now.

### Proposed semantics

- New `disruption` block on the task group: `min_healthy` (count or percentage) **or** `max_unavailable` (count or percentage). The two are mutually exclusive.
- If the `disruption` block is absent, today's `migrate.max_parallel` behavior remains. If present, the drainer applies the stricter one.
- **Voluntary disruptions** to which the budget applies: node drain, node pool scale-down (phase 4 of the autoscaler, which happens through drain), and the new eviction API.
- **It does not apply** to reducing the job `count` (neither manual nor autoscaler scale-down), to deployment updates (those are governed by the `update` block), and to involuntary events (node failure, crash). This matches the behavior of Kubernetes.
- An allocation is **healthy** if it is running and healthy according to the deployment or migration health check, identically to the existing drainer logic.

### Jobspec

```hcl
group "api" {
  count = 6

  disruption {
    min_healthy = 4        # or: max_unavailable = "25%"
  }
}
```

### Eviction API

- `POST /v1/allocation/:id/evict`: if the budget allows, the allocation stops and is rescheduled (as in a migration); if not, a `429` response with a `Retry-After` header.
- `nomad alloc stop` with the new `-respect-disruption` flag uses the same path. By default today's behavior remains, so that existing scripts do not break.
- `nomad job status` shows the current state of the budget: how many allocations are healthy, and how many can be disrupted now.

### Implementation

1. **Jobspec and structs:** new `DisruptionBudget` under `TaskGroup` (`nomad/structs/structs.go`, `jobspec2/`), validation.
2. **Common computation:** one function that returns, for a task group, the number of allocations that can be disrupted now, based on the snapshot. The drainer and the eviction API use the same one (new `nomad/disruption.go`), so there are not two divergent logics.
3. **Drainer:** `handleTaskGroup` also takes the `disruption` block into account when computing `thresholdCount`.
4. **Eviction:** new `Alloc.Evict` RPC (`nomad/alloc_endpoint.go`), which decides on the leader, by checking the state store and setting the desired transition in a single Raft write, so two simultaneous evictions cannot exceed the limit together.
5. **Autoscaler:** node pool scale-down (phase 4) automatically respects the budget through drain; the autoscaler handles the drain wait with a timeout, and writes an event if it cannot scale down because of the budget.

### Testing and acceptance

- With three nodes drained simultaneously, a 6-instance group with `min_healthy = 4` never has fewer than 4 healthy.
- Of two parallel eviction requests, only as many succeed as the budget allows, the rest get `429`.
- Without a `disruption` block, the drainer's behavior is bit-for-bit identical to today's.

---

## 6. Health check roles

### Summary

Service checks get an explicit role: **startup** (has startup completed), **readiness** (can it receive traffic) and **liveness** (does it need to be restarted). Today a check fills several roles at once, and the role has to be inferred from the combination of its settings. The proposal makes this explicit, fully preserving today's behavior.

**Priority:** the health state of the native service catalog (the `Ready` field described below) is also a prerequisite for Consul independence, because without it native discovery, Traefik and Prometheus's native Nomad integration also return unhealthy instances. This item is therefore a high priority, and its catalog part is detailed in phase 1 of [consul-independence-plan.md](consul-independence-plan.md) (state fields, handling of Raft load, API and template options).

### How does Kubernetes work?

- The **startup probe** suspends the other two for the duration of startup. If it does not succeed within the given time, the container is restarted.
- When the **readiness probe** fails, the pod is removed from the Service's endpoints, but is not restarted.
- When the **liveness probe** fails, the kubelet restarts the container.

The three roles are independent of one another, and all three can use the same mechanisms (HTTP, TCP, gRPC, command).

### Starting point in Nomad 1.6.5

A service check (`nomad/structs/services.go`) controls its behavior with the following settings:

| Setting | What it affects | Kubernetes role it is close to |
| --- | --- | --- |
| The result of the check in the catalog (Consul or native provider) | Whether the service instance is healthy in discovery | readiness |
| `check_restart` (`limit`, `grace`, `ignore_warnings`) | After how many failed results the task restarts | liveness, partly startup via `grace` |
| `on_update` (`require_healthy`, `ignore_warnings`, `ignore`) | Whether it counts toward deployment health | readiness for deployments |
| `success_before_passing`, `failures_before_critical` | How many consecutive results are needed for a state change | the thresholds of all three |

Deployment health is tied to checks by the `update.health_check = "checks"` setting (`client/allochealth/tracker.go`), the checks of the native provider are run by `client/allocrunner/checks_hook.go`, and the `check_restart` logic is handled by `client/serviceregistration/watcher.go`.

The practical problem: for a slow-starting application, the `grace` value has to be set to the worst-case startup time, which also delays the detection of failures during runtime. And a check cannot be both "readiness, but never restart" and "liveness, but do not take out of traffic".

### Proposed semantics

A new, optional `role` field on the check: `startup`, `readiness` or `liveness`. If absent, the check behaves exactly as today.

- **startup:** runs from the start of the task. Until it succeeds, the other checks of the task count neither toward deployment health nor toward restarts, and the service instance is not healthy in the catalog. If it does not succeed within `startup_timeout` (default 5 minutes), the task is restarted (counted according to the restart policy). At most one per task.
- **readiness:** determines whether the instance is healthy in the service catalog, and whether it counts toward deployment health. Its failure never restarts the task. For the native provider the registration gets a new `Ready` field, so the `nomadService` template function and the API can filter out instances that are not ready. The autoscaler leaves allocations that are not ready out of the metric average.
- **liveness:** its failure restarts the task. If there is no `check_restart` block, the default is: `limit = 3`, `grace = 0`, because the grace period is provided by the startup check. The result of the liveness check does not affect health in the catalog.

### Jobspec

```hcl
service {
  name     = "api"
  port     = "http"
  provider = "nomad"

  check {
    role            = "startup"
    type            = "http"
    path            = "/healthz/started"
    interval        = "5s"
    timeout         = "2s"
    startup_timeout = "10m"
  }

  check {
    role     = "readiness"
    type     = "http"
    path     = "/healthz/ready"
    interval = "10s"
    timeout  = "2s"
  }

  check {
    role     = "liveness"
    type     = "http"
    path     = "/healthz/live"
    interval = "15s"
    timeout  = "3s"
    check_restart {
      limit = 3
    }
  }
}
```

### Implementation

1. **Structs and validation:** `Role` and `StartupTimeout` fields on `ServiceCheck` (`nomad/structs/services.go`, `jobspec2/`). Validation: at most one startup check per task; no `check_restart` on a readiness check; a warning for the conflict of `role` and `on_update = "ignore"`.
2. **Client, native provider:** ordering in `checks_hook.go` (startup first), computing the `Ready` state, and updating the service registration (`client/serviceregistration/nsd/`, `nomad/structs/service_registration.go`).
3. **Client, Consul provider:** readiness and startup checks are registered as Consul checks; liveness checks run only on the client, so that they do not affect Consul health.
4. **Restart:** `watcher.go` restarts only for liveness (and role-less) checks and for the startup timeout.
5. **Deployment health:** `tracker.go` takes startup and readiness checks into account, but not liveness checks.

### Testing and acceptance

- An application that starts in 3 minutes is not restarted with `startup_timeout = "5m"`, and afterwards the liveness check detects a failure within 15 seconds.
- A readiness failure removes the instance from the native catalog, but does not trigger a restart.
- With role-less checks, the behavior is unchanged.

---

## 7. Network rules

### Summary

Based on namespaces and job labels, it can be specified what traffic an allocation may receive and send. The most important design question is not the form of the rules, but that Nomad's default bridge network uses address translation (NAT) for traffic between nodes, so the sending allocation cannot be identified by IP address there. For this reason the feature can be introduced gradually, in three levels, and Consul intentions (if Consul stays) still give a better solution for the node-to-node case.

### How does Kubernetes work?

A **NetworkPolicy** defines ingress and egress rules for pods selected with a label selector; being selected automatically means default deny for that direction, the rules add up, there is no explicit deny, and enforcement is done by the CNI plugin. Kubernetes builds this on a flat pod network without address translation, where the IP address of every pod is unique in the cluster and directly reachable.

### Starting point in Nomad 1.6.5

- The `bridge` network mode creates a per-node `nomad` bridge (`client/allocrunner/networking_bridge_linux.go`), on which allocations get a private address within the node. From other nodes, allocations are reachable through the host ports, by port forwarding, so the source address is the address of the sending node, not of the allocation.
- A custom CNI configuration can also be used (`client/allocrunner/networking_cni.go`), so a CNI plugin providing a flat, routed network can be plugged in.
- Today access control is possible only with Consul intentions, on an identity (mTLS) basis, through the proxies.

### Proposed semantics

**Object:** `NetworkPolicy`, belongs to a namespace, stored in Raft. A selector over the `meta` fields of the job and task group, `ingress` and `egress` rules with a source or destination (selector, namespace, CIDR) and ports.

**Principles taken from the Kubernetes model:** default deny in the given direction for the selected allocation; permissions add up; connection-state-based filtering, response traffic does not need to be allowed separately.

**Filling the gaps of Kubernetes:**

- **Administrator layer:** a cluster-level `AdminNetworkPolicy` with priority and explicit deny (`deny`), which namespace-level rules cannot override.
- **Checking enforceability:** if a rule applies to an allocation but its node cannot enforce it (for example `host` network mode), the placement can be rejected (`enforcement = "required"`) or gives a warning (`"best_effort"`). There can be no silent rule without effect.
- **Logging:** a counter of dropped connections per rule in the client metrics, and an optional sampled log.

### Rollout in three levels

| Level | Capability | Condition | Effort |
| --- | --- | --- | --- |
| A. Egress rules | Restricting the outbound traffic of the allocation by CIDR and port (for example only the database and the internal API are reachable) | `bridge` or CNI mode; the rule is enforced in the allocation's own network namespace, with nftables | Medium |
| B. Ingress within a node and on host ports | Filtering traffic arriving at the allocation; between nodes it can be identified only at the level of the sending node | `bridge` mode | Medium |
| C. Allocation-level ingress between nodes | Label-based filtering equivalent to Kubernetes between any two allocations | A flat allocation network without address translation: a routed allocation subnet per node (for example over the WireGuard mesh of the multi-cloud chapter) or a CNI providing a flat network | High |

For **level C**, the servers have to compute, for each rule, the address set of the matching allocations, and the clients have to follow its changes (similarly to Calico's ipset-based approach). The event stream of item 3 and a blocking-query-based endpoint can be used to distribute the address sets.

### Configuration

```hcl
name      = "db-only-from-api"
namespace = "prod"

selector { app = "postgres" }

ingress {
  from {
    selector { app = "api" }
  }
  ports = ["5432/tcp"]
}

egress {
  to {
    cidr = ["10.20.0.0/16"]
  }
  ports = ["443/tcp"]
}

enforcement = "required"   # or "best_effort"
```

### Implementation

1. **Server:** `NetworkPolicy` and `AdminNetworkPolicy` types, memdb tables, Raft messages, RPC and HTTP endpoints, ACL (`network_policy { policy = "read" | "write" }` at namespace level, the administrator layer with operator privileges), `NetworkPolicy` event topic.
2. **Scheduler:** with `enforcement = "required"`, a feasibility filter for nodes that cannot enforce the rule (the client's fingerprint signals nftables support and the network mode).
3. **Client:** a new network hook in the allocation runner (next to `client/allocrunner/network_hook.go`), which writes and updates nftables rules in the allocation's network namespace; it receives the rule set from the server with a blocking query.
4. **Level C:** address-set computation on the server, distribution to the clients, and a documented reference setup with routed allocation subnets.

### Implemented (levels A and B)

Level C is dropped: node-to-node identity comes from native mTLS (see the decision point below).

- **Objects:** `NetworkPolicy` (namespace; `selector`, `ingress`/`egress` rules with `from`/`to` peers and `ports`, `default_deny`, `enforcement`, default `required`) and `AdminNetworkPolicy` (cluster; `priority` 0–1000, lower first, then name; `namespaces` globs; rule `action` = `allow` | `deny` | `pass`). Raft message types 101–104 with `IgnoreUnknownTypeFlag`, snapshot types 101 and 103, tables `network_policies` and `admin_network_policies` (`nomad/structs/network_policy.go`, `nomad/state/state_store_network_policy.go`, `nomad/fsm_network_policy.go`).
- **Semantics:** the selector matches the job meta overridden by the task group meta (empty selects all). A namespace policy isolates a direction if it has rules there or lists it in `default_deny`; then only the union of the allowed traffic of all selecting policies passes. A rule matches if one of its peers and one of its ports match (none means any). Peers are CIDR blocks or allocations on the same node selected by `namespace` (empty: the policy's namespace, `*`: all) and `selector`. Ports are `5432/tcp`, `8000-8100/udp`, `53` (TCP) or, for ingress, port labels (`db/tcp`, resolved to the `to` port). Admin rules are evaluated first in priority order: `allow` accepts and `deny` drops regardless of namespace policies, `pass` hands over to them; admin policies do not isolate by themselves. Always allowed: loopback, `established,related` (stateful), IPv6 neighbor discovery and, for ingress, the gateway of the allocation network (the node itself, so client health checks work).
- **API:** RPC `NetworkPolicy.*` (writes behind `ServersMeetMinimumNorcaVersion`), HTTP `/v1/network-policies`, `/v1/network-policy/:name[/status]`, `/v1/admin-network-policies`, `/v1/admin-network-policy/:name[/status]`, api client `NetworkPolicies()`, CLI `norca network-policy apply|list|status|delete [-admin]`. ACL: namespace capabilities `read-network-policy` (included in the namespace `read` and `write` policies) and `write-network-policy` (explicit, or `network_policy { policy = "write" }` in a namespace block); admin policies need operator read/write. Event topic `NetworkPolicy` (`NetworkPolicyUpserted/Deleted`, `AdminNetworkPolicyUpserted/Deleted`).
- **Scheduler:** `NetworkPolicyChecker` in the task group feasibility checks: when a `required` policy (namespace or admin) selects the group, only nodes with `norca.network_policy.nftables = true` are feasible, and none in `host` mode. `best_effort` does not filter; job registration warns about groups whose network mode cannot enforce the selecting policies, and the client emits a "Network Policy Not Enforced" task event when enforcement fails.
- **Client:** fingerprint `norca.network_policy.nftables`; alloc runner hook `network_policy` (after the network hook) fetches the effective rules (`NetworkPolicy.GetAllocRules`, blocking query, node secret), compiles them with the pure function `netpolicy.Compile` and programs table `inet norca` in the allocation network namespace with `github.com/google/nftables` (Apache-2.0) in one atomic transaction. Bridge and CNI modes only; without selecting policies no table is written. Drop counters per admin deny rule and per default deny: metric `client.allocs.network_policy.dropped` (with `publish_allocation_metrics`).
- **Not done:** CIDR `except` blocks, sampled drop logs, deletion protection of namespaces with network policies, and a node-level (instead of per-allocation) watch.

### Testing and acceptance

- Level A: with an egress rule, a connection to a non-allowed destination is rejected, the allowed one works, and the counter increases.
- With `enforcement = "required"`, a node in `host` network mode gets no allocation.
- Without rules, the network behavior is unchanged.

### Decision point

Phase 5 of [consul-independence-plan.md](consul-independence-plan.md) (native mTLS, `ServiceIntention`) solves the main problem of level C, identifying the sender in the presence of address translation, on a certificate basis. Once it is done, instead of the IP-based variant of level C, node-to-node control is built on native mTLS, and network rules can narrow to levels A and B (egress and local ingress).

If Consul stays part of the stack, allocation-level control between nodes is already solved by Consul intentions on an identity basis, and level C can be dropped. In that case levels A and B (egress and local ingress) are still useful as a complement, because Consul controls only the traffic passing through the proxy.

---

## 8. Operator framework

### Summary

Custom object types and the controllers that maintain them, following the Kubernetes operator pattern. Items 1–7 (especially admission, the event stream and quotas) provide the foundations; this chapter describes what else is needed: a native, generic object type, a controller SDK, lifecycle management in the core, and the ecosystem (packaging, versioning, developer tools).

The value of a Nomad operator is not the basic maintenance of workloads, because the job and the deployment already do that (restart, rescheduling, rolling update, canary, service registration). Its value lies in "day two" operations, mainly for stateful systems: building and failing over a database cluster, ordered version upgrades, scheduling backups, schema migration, managing external cloud resources.

### How does Kubernetes work?

- **CustomResourceDefinition (CRD):** a custom type with an OpenAPI schema, default values, CEL rules (for transition rules the old value is also available, `oldSelf`), `status` and `scale` subresources, display columns, multiple served versions and conversion.
- **Controller:** a list + watch based informer, a work queue, a `Reconcile` function, leader election, running as a pod, permissions with a ServiceAccount and RBAC.
- **Lifecycle:** `finalizers`, `ownerReferences` and garbage collection (with foreground, background and orphan deletion modes), optimistic concurrency control (`resourceVersion`), server-side apply with field ownership.
- **Ecosystem:** Operator Lifecycle Manager (installation, upgrade), kubebuilder and operator-sdk (code generation), envtest (test environment), `Event` objects, status conventions (`conditions`), API Priority and Fairness.

### Starting point in Nomad 1.6.5

| Element | State | Location |
| --- | --- | --- |
| Custom object type | None. Variables is an untyped, encrypted KV with a 64 KiB limit per entry (`maxVariableSize = 65536`) | `nomad/structs/variables.go` |
| Change watching | Event stream with fixed topics; an index that dropped out of the buffer silently moves on | `nomad/stream/` (see item 3) |
| API access of the controller | The task can call the Nomad API with its own workload identity on the `secrets/api.sock` task API socket | `client/allocrunner/taskrunner/api_hook.go`, `identity_hook.go` |
| Permissions of the controller | An ACL rule can be bound to a job, group or task (`JobACL`), so a static token is not needed | `nomad/structs/structs.go` |
| Leader election | No Variables locks (only from 1.7); check-and-set write exists | `nomad/variables_endpoint.go` |

### Two paths to custom objects

**A) Variables-based prototype.** Objects live at the path `crd/<group>/<kind>/<name>` (item 3). It is quick to build, but because of its limitations it is suitable only for a prototype: a 64 KiB size limit, no spec/status separation, no finalizers or owner references, a schema only if admission is extended to Variables writes.

**B) Native `CustomObject` type (the goal).** A new, generic object type in Raft, with a type registry. The rest of this chapter specifies this.

### Type definition (`ObjectKind`)

```hcl
kind "PostgresCluster" {
  group      = "db.example.com"
  namespaced = true
  schema     = file("postgrescluster.schema.json")   # JSON Schema, with default values

  rule {
    expression = "self.spec.replicas >= 1 && self.spec.replicas <= 7"
    message    = "The number of replicas must be between 1 and 7."
  }
  rule {
    # transition rule: the old value is also available
    expression = "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)"
    message    = "Storage cannot be reduced."
  }

  columns {
    column "Version"  { path = "spec.version" }
    column "Replicas" { path = "status.readyReplicas" }
    column "State"    { path = "status.conditions[?type=='Ready'].status" }
  }

  scale {
    spec_path   = "spec.replicas"
    status_path = "status.replicas"
  }

  replicated = false   # true: replicated from the authoritative region
}
```

- **Schema and default values:** JSON Schema; the server fills in the `default` values on write, so the stored object always shows the complete, effective configuration.
- **CEL rules:** the CEL environment of item 2 is extended with the `oldSelf` variable, which contains the previous object on modification and is absent on creation.
- **Display columns:** the CLI listing and the UI table show these.
- **`scale` subresource:** through this the autoscaler can scale custom objects as well as task groups (see below).
- **Versioning:** in the first version there is a single stored version per type. On a schema change, the type definition can get a migration CEL expression, which the server runs on every stored object in a background task. Multiple served versions with conversion come up only if practice requires it.

### Object

```hcl
kind      = "db.example.com/PostgresCluster"
name      = "billing"
namespace = "prod"
labels    = { team = "finance" }

spec {
  version  = "16"
  replicas = 3
  storage  = "100GiB"
  backup { schedule = "0 2 * * *" }
}
```

The structure of the stored object:

| Part | Content | Who writes it |
| --- | --- | --- |
| `metadata` | name, namespace, labels, `generation` (increases on every modification of the spec), `finalizers`, `owner` references, `CreateIndex`, `ModifyIndex`, deletion timestamp | the server, and the user for the labels |
| `spec` | the desired state | user, GitOps tool |
| `status` | `observedGeneration`, `conditions` (`Ready`, `Progressing`, `Degraded`), type-dependent fields | only the controller, on a separate endpoint |
| event log | the last N (default 20) object-level events (`Reconciled`, `FailedCreate`, `BackupSucceeded`) | controller, on a separate endpoint |

### Lifecycle and concurrency

- **Optimistic concurrency:** every modification must specify the expected `ModifyIndex` value; on a mismatch the server responds `409 Conflict`, and the controller re-reads and retries. Status writes work the same way.
- **Field ownership:** separating spec and status solves the most common conflict (user versus controller). If several writers manage the same spec (for example a GitOps tool and another controller), a `metadata.managed_by` field and an admission rule determine who may write. The first version does not adopt the field-level server-side apply mechanism of Kubernetes.
- **Finalizers:** on deletion the object is only marked for deletion (deletion timestamp), and disappears when the controllers have removed all finalizers. During this time the controller cleans up (for example stops the backup job, deletes the external resource).
- **Owner reference and garbage collection:** jobs, Variables entries and other objects created by the controller get an `owner` reference (for jobs, in the job's `meta` field, under a reserved key). A new garbage collector running on the leader deletes resources left without an owner.
- **Deletion modes:** `background` (default: the object disappears immediately, and the objects it owns are deleted afterwards), `foreground` (the objects it owns first), `orphan` (the objects it owns remain, for example when replacing an operator, so that the running database does not disappear).
- **Dry run:** a `dry_run` parameter on every write: the server runs the default filling and the validation, and returns the change, but does not write.

### API and CLI

| Operation | HTTP | CLI |
| --- | --- | --- |
| Managing types | `/v1/object-kinds`, `/v1/object-kind/:group/:kind` | `nomad object-kind apply / list / inspect / delete` |
| Listing objects (label and field filtering, pagination) | `GET /v1/objects/:group/:kind?namespace=&filter=&per_page=&next_token=` | `nomad object list <kind> [-label ...]` |
| Reading, writing, deleting an object | `GET / PUT / DELETE /v1/object/:group/:kind/:name` (`cas`, `dry_run`, `propagation`) | `nomad object get / apply / delete` |
| Status and events | `PUT /v1/object/:group/:kind/:name/status`, `/events` | (written by the controller SDK) |
| Scaling | `GET / PUT /v1/object/:group/:kind/:name/scale` | `nomad object scale <kind>/<name> <count>` |
| Watching changes | `GET /v1/event/stream?topic=CustomObject:db.example.com/PostgresCluster&exact=true` | `nomad event ...` |

Listing works with server-side indexes (type, namespace, labels) and pagination, so that it does not overload the server and the client even with a large number of objects. Filtering uses Nomad's existing `filter` expression language.

### Permissions

A new ACL block, with capabilities specifiable per type, at namespace level:

```hcl
namespace "prod" {
  object "db.example.com/PostgresCluster" {
    capabilities = ["read", "write"]          # user
  }
}

namespace "*" {
  object "db.example.com/*" {
    capabilities = ["read", "write", "write-status", "write-events", "manage-finalizers"]   # controller
  }
}

operator { policy = "write" }   # managing type definitions
```

The controller receives these capabilities through the ACL rule bound to its job (`JobACL`), and the call is made with the workload identity, on the task API socket.

### Running the controller and the SDK

- **Running:** as a Nomad job, typically with `count = 2`. Only one instance works at a time: in the native version a built-in `Lease` object type, in the prototype the check-and-set write of Variables provides leader election.
- **SDK (`nomad/controller` in this repository, a separate MPL-licensed Go module):** informer (list + watch with a local cache, resynchronization on `410`), a work queue with rate limiting and backoff, `Reconcile(ctx, key) → (retry time, error)`, watching the jobs and allocations it owns and mapping them back to the owner, status, event and finalizer helper functions, leader election, metrics (reconcile time, queue length, errors) and OpenTelemetry tracing.
- **Adoption after restore:** if an object is restored from a backup, the SDK first looks for already existing resources belonging to it (based on the owner reference and labels) and adopts them instead of creating new ones. This is also consistent with the selective and full restore of the backup plan.

### Relationship to the other items

| Item | Relationship |
| --- | --- |
| 1. Quotas | Jobs started by the controller consume from the namespace's quota, so even a faulty controller does not flood the cluster |
| 2. Admission | The CEL rules of the types run in the same environment; a rule prevents a job managed by the controller from being modified by another identity |
| 3. Event stream | `exact=true` and `410` provide the correctness of controllers; new `CustomObject` topic |
| 4. Affinity | The controller can put a `separate` rule on the managed replicas |
| 5. Disruption budget | Stateful systems are protected during drains as well |
| 6. Health check roles | The controller computes its status from the readiness state |
| Autoscaler | Through the `scale` subresource the autoscaler scales the object, and the controller derives the jobs from it. Rule: if a task group has its own `scaling` block, the controller must not reset its `count` |
| Backup | Native objects are part of the Raft snapshot, so they are included in backups and in selective restore |
| Multi-region | Controllers run per region; `replicated = true` types are replicated from the authoritative region, like the global ACL objects; for coordination between regions, the NATS bridge |

### Ecosystem

- **Operator package:** there is no separate format: operators can be distributed as extended Nomad Pack packages with the fork's package manager, which installs, updates and rolls back the type definitions, the controller's job, the ACL rules and the CEL rules as one versioned release (with a fixed application order: type definitions first, then the controller). Details: [package-management-plan.md](package-management-plan.md).
- **Type-bound webhooks:** the general registration of webhooks is intentionally possible only in the server configuration (item 2). For operators, an administrator-approved webhook registration restricted to a single type can be allowed, with the certificate provided by the package.
- **Developer tools:** a code generator for the types and the controller skeleton, and a test environment that starts an embedded Nomad dev server, so that the controllers' tests run against a real API.
- **GitOps:** objects can be applied from git; the drift and the health state can be determined uniformly from `generation`, `observedGeneration` and the `Ready` condition.
- **Server-side load management:** a per-type write limit, and per-client (per-identity) request queues, so that a misbehaving controller does not throttle user requests.

### Implementation

1. **Object store:** new memdb tables (`object_kinds`, `custom_objects` with indexes by type, namespace, name and labels, `object_events`), Raft messages, state store functions (new `nomad/state/state_store_objects.go`).
2. **Server:** type registry, schema validation and default filling (with a JSON Schema library), CEL with `oldSelf`, RPC and HTTP endpoints, ACL, event topic (new `nomad/object_endpoint.go`, `command/agent/object_endpoint.go`, `acl/policy.go`).
3. **Lifecycle:** finalizer handling, garbage collector and deletion modes on the leader (new `nomad/objectgc/`), admission protection of managed jobs.
4. **Autoscaler:** `CustomObject` target through the `scale` subresource.
5. **SDK and tools:** the SDK is the separate Go module `nomad/controller` in this repository (it depends only on the API module `nomad/api`, never on the main module); further tools are added next to it.
6. **UI:** a generic object view based on the columns of the type definition, showing spec, status, conditions and the event log; on the jobs' pages, a link to the owner object.

### Schedule

| Phase | Content | Prerequisite |
| --- | --- | --- |
| 1. Prototype | Variables-based objects, first version of the SDK, a sample operator | Item 3, the CEL part of item 2 |
| 2. Native objects | Type registry, spec/status, `generation`, default values, CEL with `oldSelf`, concurrency, dry run, ACL, event topic, CLI | Phase 1 |
| 3. Lifecycle | Finalizers, owner reference, garbage collection with deletion modes, event log, protection of managed jobs, `scale` and the autoscaler target | Phase 2 |
| 4. Ecosystem | UI, operator package (per the package management plan), type-bound webhooks, developer tools, migration CEL, load management | Phase 3 |

The first production operator needs the elements up to the end of phase 3. Phase 4 becomes urgent when other teams start writing operators too.

### First sample operator: LoadBalancer

The first operator of our own is the equivalent of the Kubernetes `LoadBalancer` service type: automatic creation and maintenance of cloud (and self-hosted) load balancers based on the healthy instances of Nomad services. In a multi-cloud environment this has the greatest practical benefit, because a different tool per cloud would otherwise have to be maintained by hand, and it is also a good model, because it uses all the elements of the framework (object, status, finalizer, owner reference, per-region controller). In Kubernetes as well, this is done by a separate component (the cloud controller manager), not by the API server.

**Marking in the jobspec** (the controller creates the `LoadBalancer` object from this):

```hcl
service {
  name = "api-public"
  port = "https"
  expose {
    load_balancer {
      class    = "aws-nlb"    # the provider plugin installed in the region
      port     = 443
      internal = false
    }
  }
}
```

**Operation:**

- One controller per region (as a Nomad job, `count = 2`, with leader election), and one provider plugin per cloud: AWS Network Load Balancer, Google Cloud Load Balancing, Hetzner Load Balancer.
- The controller watches the healthy instances of the marked service (or the nodes of the Traefik running at the edge), and updates the load balancer's targets and health check accordingly.
- The status of the `LoadBalancer` object contains the address and state of the load balancer (`Ready`, `Progressing`, `Degraded`); the address can also be entered into the native DNS.
- The object gets a finalizer: on deletion the controller first removes the cloud load balancer.
- **For self-hosted or bare-metal deployments** a MetalLB-like provider: announcing a virtual IP over L2 (ARP) or BGP, from designated nodes.
- The cloud credentials are in the Variables entries of the controller's job or in the nodes' cloud identity, never in the object.

**Schedule:** after phase 3 (lifecycle) of the framework; until then, load balancers are to be managed in the existing way (Terraform).

### Testing and acceptance

- Of two simultaneous spec modifications, the second gets `409` and succeeds after re-reading.
- A rule using `oldSelf` prevents reducing the storage, but allows increasing it.
- With `foreground` deletion the object disappears only after the jobs it owns are deleted; with `orphan` deletion the jobs remain.
- The sample operator continues without lost events and without duplicated resources after a server restart, a leader change and a restore from backup.
- Even a controller writing incorrectly, in an infinite loop, does not measurably slow down user API requests.

### Risks

| Risk | Mitigation |
| --- | --- |
| Raft load from many status writes or a rescheduling wave | SDK-side rate limiting and coalescing of status writes; server-side per-type and per-identity limits |
| A faulty controller deletes managed jobs | Finalizers, `orphan` deletion mode, admission protection, selective restore from backup |
| The controller and the autoscaler work against each other | Using the `scale` subresource; a documented `count` rule built into the SDK |
| A schema change breaks stored objects | Checking the backward-compatible schema rule on type update; migration CEL with dry run |

### Open questions

- [ ] Which JSON Schema implementation do we use (license, performance)?
- [x] Is field-level ownership (server-side apply) needed already in the first version, or is the `managed_by` rule enough? Decided 2026-10-24: the `managed_by` rule is enough.
- [x] The first sample operator is the LoadBalancer (see above).

---

## Summary table

| # | Feature | Starting point | New object or field | New event topic | Main area affected |
| --- | --- | --- | --- | --- | --- |
| 1 | Namespace quotas | API, CLI, ACL, blocking mechanism done; server side is an empty stub | `QuotaSpec` (+`allocations`, `task_defaults`, `task_max`, `enforce`) | `Quota` | state store, plan apply, scheduler |
| 2 | Dynamic admission | Built-in mutator/validator chain on the leader | `AdmissionPolicy`; `admission` block in the server configuration | `AdmissionPolicy` | `job_endpoint_hooks.go`, new `nomad/admission/` |
| 3 | Event stream extension | Fixed topics, `index` parameter, internal `StartExactlyAtIndex` | `exact` parameter; Variables metadata events | `Variables` (+ the other new topics) | state store event building, event broker |
| 4 | Cross-job affinity | Only node-level and within-job iterators | `colocate`, `separate` blocks | – | `scheduler/` iterator chain |
| 5 | Disruption budget | Job-level `migrate.max_parallel` in the drainer | `disruption` block; `Alloc.Evict` RPC | – | drainer, new `nomad/disruption.go` |
| 6 | Health check roles | One check in several roles | `role`, `startup_timeout` fields; `Ready` on the native registration | – | client checks, allochealth |
| 7 | Network rules | Bridge mode with NAT; Consul intentions | `NetworkPolicy`, `AdminNetworkPolicy` | `NetworkPolicy` | client network hook, scheduler |
| 8 | Operator framework | Variables (64 KiB), task API socket, workload identity, `JobACL` | `ObjectKind`, `CustomObject`, `Lease`; object-level event log | `CustomObject` | new object store, `nomad/objectgc/`, SDK |
