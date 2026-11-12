<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Application Scaling: Code Review and Build Proposal

**Date:** 2026-11-12. **Status:** accepted on 2026-11-12; its decisions are in sections 7.5, 8.4, 12 and 13 of the design document. Nothing is built.
**Companion:** [dynamic-application-autoscaling.md](dynamic-application-autoscaling.md) (the design,
its problems P1–P10 and questions Q1–Q21). This report says how I would fix the problems if the
choice were mine, and why.

**Method and limits.** Everything below comes from reading the code on `main` (1f8a3ccb7). Nothing
was run: no test, no lab case reproduces the findings yet. Each finding names the function it was
read from; the first build step is a failing test for each.

---

## 1. Findings

Verified in the code. F1–F4 are new compared with the design document.

| # | Finding | Where | Effect |
| --- | --- | --- | --- |
| F1 | The evaluator compares the computed count with the **running** allocations, not with the group's `count` (`scalingNeeded(action, currentCount)`, `currentCount = t.status.Count` = running, non-canary). | `autoscaler/evaluator.go`, `target.go` (`readTarget`) | While allocations start, wait for capacity or migrate, "a change is needed" is true although the group's count already is what the check wants. |
| F2 | `Job.Scale` has no shortcut for an unchanged count: it registers the job again (Raft), creates an evaluation and writes a scaling event with `Count == PreviousCount`. | `job_endpoint.go` (`Scale`) | With F1: one job registration, one evaluation and one event per cooldown for nothing, as long as the group is not fully running. |
| F3 | The cooldown is taken from the newest scaling event; an event that does not raise the count gets the **long** `cooldown`, not `cooldown_on_scale_up` (`*e.Count > e.PreviousCount` is false for an unchanged count). | `target.go` (`cooldownUntil`) | With F1+F2: after a scale-up that is still starting, the next evaluation writes a no-change event, and the group is locked for the long cooldown (5m by default) exactly while the load is rising. |
| F4 | The value of a check is an average over the query window (1m by default, `DefaultQueryWindow`), the count it is multiplied with is the running allocations at the end of the window. | `check.go` (`queryMetrics`), `usage_apm.go`, `target.go` | When the running count drops inside the window (drain, lost node), the product is too small: a scale-**down** decision during a migration or a datacenter loss. |
| F5 | A computed count below the group's `count` is written as it is, even when the direction is "up" (computed from fewer running allocations). | `executor.go` (`execute`), `job_endpoint.go` | Blocked work that node scaling is already buying machines for is cancelled. (`Job.Scale` refuses it during a deployment, not otherwise.) |
| F6 | `Job.Register` keeps the counts only with `PreserveCounts`; `norca job run -preserve-counts` is off by default; the HTTP API and multiregion peers pass the flag on, nothing sets it. | `job_endpoint.go` (`Register`), `command/job_run.go`, `job_endpoint_multiregion.go` | Every new job version sets the scaled groups back to the jobspec count. |
| F7 | The blocked-work candidates of node scaling are filtered by datacenter, pool, class and size only; a group's health is not read. The `spread` expander picks the smallest `spec.replicas + opened`. | `autoscaler/demand.go` (`scaleForDemand`, `expand`) | After a datacenter loss the dead datacenter's group can be chosen; the work waits for machines that never join. |
| F8 | A node group is lowered when **its own** allocated share is low; among the eligible groups of a pool the one with the most ready nodes goes first. | `autoscaler/scaledown.go` (`unneeded`, `scaleDown`) | After a datacenter returns empty, it is the one that is shrunk; the datacenters that grew keep the work. |
| F9 | The demand items carry the job's datacenters, pool, class and `distinct_*` constraints, not its `spread` or affinities. | `metrics_store_blocked.go` (`collectBlocked`), `demand.go` | New nodes can all go to one datacenter of a job that asked for an even spread. |
| F10 | While a deployment is `pending`, `paused` or `initializing`, or canaries are unpromoted, the reconciler places nothing new except replacements of lost allocations. | `scheduler/reconcile.go` (`computeDeploymentPaused`, `deploymentPlaceReady`) | Scaling a waiting region or a canarying group cannot work; the hold is right, but nobody sees it. |
| F11 | `scaleBlockedByDeployment` reads the group's typed policy for every caller of `Job.Scale`. | `job_endpoint_hook_scaling.go` | A changed default of `scale_during_deployment` would change the external engine too, unless it is bound to the engine. |
| F12 | The app scaler knows the namespace quota (`capAtQuota`, base = the group's `count`) but nothing of node scaling's limits or of work it cannot help. | `autoscaler/coordination.go`, `demand.go` | A count that can never run. |
| F13 | Skipped evaluations (deployment, cooldown, schedule, no data) are debug log lines and counters only. | `evaluator.go` | "Why does it not scale?" cannot be answered from the API or the UI. |
| F14 | The usage of a group has no datacenter; one count per group. | `client/allocrunner/taskgroup_usage.go`, `autoscaler/metrics.go` | A hot datacenter is averaged away. |

---

## 2. The Root Cause

F1–F5 are one defect seen from five sides: **the app scaler has a single number, the running
allocations, and uses it for three different things**:

1. the size of the population the measured value belongs to (the multiplier of the strategy),
2. what has already been asked for (to decide whether a change is needed),
3. the base of the direction (up or down).

These are equal only when the group is at rest. Every interplay problem with node scaling (P2, P3,
P7 on the app side) happens while they differ: allocations starting, blocked, migrating or being
replaced. Fixing the symptoms one by one (a wait here, a gate there) would add four timers; fixing
the model removes the cases.

---

## 3. What I Would Build

### 3.1 A target status with three numbers (fixes F1, F4, F5; P2, P3, P7 app side)

`readTarget` returns:

- `desired`: the group's `count` (for an object target: the scale subresource's spec value);
- `running`: as today;
- `observed`: the average number of running allocations over the check's query window, from the
  usage reports (allocation-seconds per bucket: the report already carries `Samples` and `Window`;
  for long windows the store's `norca.group.allocs_running`). For sources that are not the usage
  pipeline, `running`.

Rules:

1. **The strategy multiplies with `observed`.** Value and population then describe the same period:
   `ceil(observed × value / target)` is the average total load divided by the target, whatever
   happened to the population inside the window. Built-in strategies get the exact average;
   external strategy plugins keep their integer API and get the rounded value.
2. **A change is needed when the result differs from `desired`**, and the direction is the sign of
   that difference. No call when nothing changes.
3. **Settle rule: no scale-down while the group is not settled.** Not settled means
   `running < desired`, or allocations of the group are migrating or being replaced. Bounded by
   `settle_timeout` (default 10m) so a count that can never run does not block forever; when the
   time is up, see 3.6.
4. **Scale-up while not settled is allowed** when the result is above `desired`: the formula already
   counts on the allocations that are on their way, so a larger result means the load needs more
   than what was asked.

This replaces three proposals of the design document with one rule: the "an up never lowers"
guard and the `capacity_wait` hold of P2, and the recovery gate of P3/P7. There is one timer
(`settle_timeout`) instead of three.

### 3.2 No-change scaling is free (fixes F2, F3)

- `Job.Scale` with `count == previous` returns success without registering the job or creating an
  evaluation. The scaling event is still written when the caller sent a message or an error
  (the external engine and operators use events as a log), marked as unchanged.
- `cooldownUntil` and `lastScaleUp` skip events whose count did not change.

Both are needed: the first for every caller, the second for events already stored.

### 3.3 A new job version keeps scaled counts (fixes F6; P1)

`Job.Register` keeps the current count of every group that has an **enabled** scaling policy (any
engine), unless the request says `ResetCounts` (`norca job run -reset-counts`). `-preserve-counts`
stays and keeps its meaning for groups without a policy. Multiregion peers apply the same per
region. A group whose policy is disabled or removed takes the jobspec count as today.

### 3.4 Evaluation status and one hold event (fixes F13; P10, P8a)

- The leader keeps the last evaluation of every policy in memory: time, each check's value and
  vote, the result, and why nothing was done (`deployment`, `canary`, `multiregion`, `cooldown`,
  `settling`, `schedule`, `no data`, `quota`, `capacity`, `rate limit`, `suspended`). Served by
  `GET /v1/scaling/policy/<id>` (a `Status` part) and shown by `norca scaling policy info` and the
  UI policy page.
- One event type `ScalingHeld` with the reason, written once per hold after `hold_warn` (10m), and
  once more when the hold ends.
- `scale_during_deployment` defaults to `up_only` **for built-in engine policies**; the default is
  resolved from the engine in one place, which `scaleBlockedByDeployment` also uses, so the external
  engine and hand-made scaling keep `never` (F11).
- Waiting regions and canaries keep their hold (F10); they become visible through the status.

### 3.5 Node scaling: healthy, spread-aware, balanced (fixes F7, F8, F9; P5, P7 node side)

- **Health filter.** A group is not a candidate for blocked work while its operator reports
  `JoinFailing`, `ProviderAvailable=false` or `NoBootstrap`, or while it has had instances and no
  ready node (or fewer than half ready) for 2 minutes. Reasons go into the existing
  `ScalingNodeBlocked` event.
- **Spread first.** The demand item carries the job's datacenter spread targets (and datacenter
  affinities). Among the healthy candidates the datacenters below the job's target are taken
  first; the pool's expander decides among equals.
- **Balance in the scale-down.** Per pool: when the pool as a whole is below `utilization_below`
  without one node, the group most above its share (`weight`, or even) is lowered. Default on,
  `balance = false` per pool turns it off.

### 3.6 Capacity link (fixes F12; P6)

When `settle_timeout` ends and the group's missing allocations are blocked work that node scaling
reports as not helpable (no group, at its maximum, pool maximum, too large, unverifiable), the app
scaler stops raising the count (`ScalingHeld`, reason `capacity`) and keeps the scale-down hold off:
it may lower again. It never lowers the count by itself to "what runs": that decision stays with
the checks.

### 3.7 After these: the application metrics

The phases 1–3 of the design document, unchanged: task metrics from a loopback endpoint,
operator-reported metrics, the `backlog` strategy. Their value depends on the fixes above: a
backlog check that scales from zero meets every case of section 2 on its first step.

### 3.8 Later or not at all

| Item | Decision | Why |
| --- | --- | --- |
| Pre-warm for scheduled `fixed-value` policies (P9) | later, off by default | Useful, small, but no defect; the node group windows already cover it by hand. |
| `dc` label and per-datacenter checks (P4, F14) | later | Needs the label in the report, the ring buffer and the store (more series); the pattern "one group per datacenter" works today. |
| File sink `metrics.json` | later | The loopback endpoint covers most tasks. |
| Envoy and network values | not until asked | No admin endpoint in the sidecar today; a security-sensitive change for an unproven need. |
| Health check header | drop | Works only for checks the Norca client runs. |
| Scaling in regions waiting for a multiregion rollout | no | Needs a scheduler change against the rollout's order (F10). |
| A per-datacenter count inside one group | no | Scheduler and jobspec change; separate groups do it. |
| Reading another region's metrics | no | Breaks the region-local design; shares are the operator's (SDK weights). |

---

## 4. Build Order

Each step starts with tests that fail on `main` and ends with a lab case. Steps 1–3 touch only the
built-in engine and `Job.Scale` / `Job.Register`; 4 only node scaling.

| Step | Content | Proof |
| --- | --- | --- |
| 1 | 3.2 (no-change scaling, cooldown reads real changes) | Unit: a scale-up followed by an evaluation while allocations start writes nothing and leaves `cooldown_on_scale_up` in force. |
| 2 | 3.1 (target status, `observed`, compare with `desired`, settle rule) | A simulation test in the autoscaler package: fake clock, a load model, allocations that start late, drains and a lost node; asserts: no scale-down while unsettled, converges to `ceil(load/target)`, no call without a change. |
| 3 | 3.3 (counts on register), 3.4 (status, `ScalingHeld`, `up_only` default bound to the engine) | Unit + API tests; an external-engine policy behaves as before (regression tests for the plugin path). |
| 4 | 3.5 (health filter, spread first, balance) | `demand_*_test.go` cases; lab: `nodescaler` case with a datacenter switched off and on. |
| 5 | 3.6 (capacity link) | Unit; lab: app `max` above the pool's `max_nodes`. |
| 6 | Lab case for the whole: app policy + node groups, load step, deploy under load, node scale-down under load. | Passed run recorded in the commit messages. |
| 7+ | Application metrics phases 1–3; then the "later" items by need. | As in the design document. |

Docs (norca-doc), `ui-design-review.md` and the clean room record follow each step.

---

## 5. Answers I Would Give to the Open Questions

| Q | Answer | Why |
| --- | --- | --- |
| Q1 | Add `group.*` names next to the existing ones; remove nothing. | The plugin path must keep working; aliases cost nothing. |
| Q2 | Yes, short `app.*` / `reported.*` for the policy's own group. | The common case should be one line. |
| Q3 | Prometheus text and flat JSON both. | The JSON reader is a few lines and serves tasks without a metrics library. |
| Q4 | Drop the header. | Section 3.8. |
| Q5 | New capability `submit-metrics`. | Least privilege: a reporter must not be able to scale. |
| Q6 | Yes, in the phase of the `backlog` strategy. | A policy on a custom object should read the object's own values. |
| Q7 | Do not refuse; show it. A hand-made change on a policy's target is allowed, the reply carries a warning, and the status shows "changed by hand, policy resumes after the cooldown". | Refusing breaks emergency use and needs an exception list (external engine, operators' child jobs); the cooldown already gives the hand-made change its time. |
| Q8 | Own strategy `backlog`. | Different formula and direction rule; an option would change `target-value` for everyone. |
| Q9 | Accept the limits. | They bound memory and series; raising them later is easy, lowering is not. |
| Q10 | Out of the plan until asked. | Section 3.8. |
| Q11 | Yes (3.3). | The largest daily harm, the smallest change. |
| Q12 | Replaced by the settle rule (3.1). | One rule instead of a wait and a guard. |
| Q14 | Document the pattern now, the label later. | Section 3.8. |
| Q15 | Yes (3.5). | The job said where it wants to run; node scaling should not overrule it. |
| Q16 | Yes, as a hold with an event (3.6). | Visible, and no count is changed behind the checks. |
| Q17 | Yes, the fixes first. | Section 3.7. |
| Q18 | Filter with conditions and the ready-node signal; balance on by default with an opt-out. | Conditions alone take tens of minutes; balance costs migrations, so it must be switchable. |
| Q19 | (a) hold, visible; (b) `scaling { min max }` per region for every scaled group; (c) SDK weights, no takeover. | F10; same shape as `count`; takeover needs cross-region reads. |
| Q20 | Automatic pre-warm, off by default, built last. | Section 3.8. |
| Q21 | `up_only` for built-in engine policies only, with the visible hold. | F11. |

---

## 6. Why These Decisions

1. **Fix the model, not the symptoms.** Five findings share one cause (section 2). A status with
   `desired`, `running` and `observed` makes the wrong decisions impossible to express; separate
   gates would each need a timer, a test matrix and documentation, and would interact.
2. **Never scale down on incomplete information.** A late scale-down costs some money for minutes;
   a wrong scale-down during a recovery costs an outage. The settle rule is biased on purpose.
3. **Scale-ups stay fast.** No hold was put on scale-ups: with `observed` the formula is already
   right while allocations are on their way, and waiting for node joins would turn a capacity
   problem into a latency problem.
4. **The cheapest fixes first.** Steps 1 and 3 are small, local and remove daily harm (lost
   cooldowns, counts reset by deploys). The larger step 2 then lands on a quiet base.
5. **Additions only on the plugin path.** Every behaviour change is bound to the built-in engine
   or is invisible to a caller that used the API correctly (`Job.Scale` with an unchanged count
   still succeeds). The external engine, plugin binaries and their tests are untouched.
6. **Visibility is part of the fix.** Most reports of "the autoscaler does nothing" are correct
   holds. Without the evaluation status every later problem costs a debug-log session; with it the
   answer is one command.
7. **Node scaling follows what the job asked.** Health, spread and balance are three cases of the
   same principle: capacity is bought where the work can and wants to run, and removed where it is
   in surplus for the pool, not for one group.
8. **No scheduler changes.** Scaling in waiting regions and per-datacenter counts would need them;
   their benefit is small against the risk in the most sensitive code of the system.
9. **Fixes before features.** The application metrics multiply how often the scaler acts (scale
   from zero, backlog steps). Shipping them on the current decision path would multiply the
   defects' effect.
10. **Proof by simulation, then lab.** The interplay bugs are timing bugs; unit tests of single
    functions did not catch them (they are all on `main` with green tests). A deterministic
    simulation of load, start delays and drains tests the rules; the lab case tests the wiring.

---

## 7. Risks and Open Points

- **Not reproduced yet.** F1–F3 in particular should be confirmed by a test before step 2 is
  designed in detail; if `Job.Scale` is reached less often than read (for example because a
  deployment usually follows a scale-up), the effect is smaller, the fix is the same.
- **`observed` for external sources.** A Prometheus check has no allocation-seconds; it falls back to
  `running`, so F4 stays for those checks, softened by the settle rule.
- **External strategy plugins get a rounded population.** Up to half an allocation of error on
  small groups; acceptable against changing the plugin API.
- **Counts kept on register (3.3) change what `norca job run` does** for scaled groups. There are no
  released versions, but lab cases and packs that rely on the reset need `-reset-counts`.
- **Balance-aware scale-down adds migrations.** Bounded (one node per pool per `unneeded_for`,
  disruption budgets apply) and switchable.
- **Size.** Steps 1–6 are roughly: 1 small, 2 large, 3 medium, 4 medium, 5 small, 6 a lab case.

---

## 8. Addendum: Node Scaling by Datacenter and Region (second review, 2026-11-12)

Read from the code as before; nothing run. The design document lists these as P15–P22 (section
8.6); the proposed fixes were accepted on 2026-11-12 (decisions D19–D26).

| # | Finding | Where |
| --- | --- | --- |
| F15 | The datacenters of blocked work are those with ready nodes at evaluation time plus the job's explicit names; patterns (`*` is the default of `datacenters`) add none. An empty datacenter's group is never a candidate for such jobs. | `metrics_store_blocked.go` (`blockedDatacenters`), `structs.go` (default `Datacenters`) |
| F16 | Capacity gauges are by datacenter, pool and class; class "" is every node of the datacenter and pool; no rule keeps two groups from having the same three. A group's share, headroom and scale-down use them. | `metrics_store_sample.go` (`sampleCapacity`), `autoscaler/demand.go` (`groupUsage`, `readyNodes`), `nodescaling_kinds.go` |
| F17 | `max_nodes` of a pool is checked against the sum of `spec.replicas`. | `autoscaler/demand.go` (`apply`) |
| F18 | The headroom is per group, from its own datacenter's free capacity. | `autoscaler/demand.go` (`apply`) |
| F19 | Schedule windows are cron expressions in UTC. | `autoscaler/nodegroup_schedule.go`, `schedule.go` |
| F20 | The operator pack has `distinct_hosts` and no datacenter spread. | `norca-registry/packs/nodescaler-operator/templates/operator.nomad.tpl` |
| F21 | Rolling replacement limits are per group. | `norca-platform/nodescaler/operator/rollout.go` |
| F22 | A node that is down while its instance runs is counted neither ready nor joining and is not replaced; the autoscaler does not lower a group with `spec != ready`. | `norca-platform/nodescaler/operator/reconcile.go` (`counts`), `autoscaler/scaledown.go` (`unneeded`) |

What I would do: fix F15, F16, F17 and F22 with step 4 (they decide whether the health filter, the
balance and the capacity link of the accepted plan work correctly), change the pack for F20 at
once, offer the reserve of F18 as an opt-in after step 4, and leave F19 and F21 for later.

Why: F15 hits the default jobspec (`datacenters = ["*"]`), so scale from zero per datacenter does
not work for most jobs; F16 makes every per-group number wrong as soon as a datacenter holds more
than one group or any node the operator did not create; F17 and F22 are the two ways a pool gets
stuck after a failure; F20 is cheap insurance for the moment node scaling is needed most.

**Checked afterwards: can two provider instances register under one name?** Yes, nothing stops
them, but it is not a standby (`norca-platform/provider/registrar/registrar.go`,
`nodescaler/operator/providers.go`):

- a `NodeProvider` object has one `spec.endpoint`, written by `Apply` without a lock or a compare:
  the instance that registered last owns it;
- every instance updates `status.heartbeat`, and the operator judges the provider by the
  heartbeat alone: when the instance behind the endpoint crashes, the other keeps the heartbeat
  fresh, `ProviderAvailable` stays true and the calls fail;
- a clean exit deletes the object (`DeregisterOnExit`), and the other instance registers again at
  its next heartbeat: a hand-over on clean exits only;
- the `norca-provider` pack runs `count = 1` on purpose (its comment: registered once, and some
  platforms keep state in the process).

So F20 is fixed for the provider by letting its job be placed in a second datacenter, not by a
second instance; a standby needs a lease in the object's status (later, by need; the creates are
already idempotent by `request_id`).
