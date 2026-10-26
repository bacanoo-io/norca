# BUG: uBixVault crash-loops when MySQL storage has a brief outage

**Found:** 2026-09-12, in production on the BrainChurts k3s cluster
**Affects:** `0.2.0-beta.10` (deployed); code paths unchanged through `v1.0.0-rc.2`
**Severity:** availability — a secrets manager should survive its database blinking
**Wanted in:** `v1.0.0`, since `rc.1` and `rc.2` are already cut

> **This file is a handoff from the cluster side.** It is written to be
> self-contained: everything observed is below, so you should not need to go
> asking. When you have landed a fix, fill in [§ Reply](#reply-from-the-ubixvault-side)
> at the bottom — that is how the cluster side finds out, since agent sessions
> cannot notify each other.

## What happens

Both instances restart repeatedly: `ubixvault-prod-0` 5 times, `ubixvault-0`
(dev) 6 times, over ~12 days. Both point at the same external MySQL at
`10.50.25.45:3306`, which sits outside the Kubernetes cluster.

One full episode from the prod container log, 2026-09-09:

```
04:21:04 auto-unsealed
04:21:04 uBixVault 0.2.0-beta.10 listening on https://0.0.0.0:8200 (storage: mysql)
07:03:41 api: internal error: core: read seal config: storage: mysql get: context canceled
07:03:51 api: internal error: core: read seal config: storage: mysql get: dial tcp 10.50.25.45:3306: operation was canceled
07:04:01 api: internal error: core: read seal config: storage: mysql get: dial tcp 10.50.25.45:3306: operation was canceled
   ... repeating every ~10s for roughly 90 seconds ...
07:04:42 api: internal error: core: read seal config: storage: mysql get: dial tcp 10.50.25.45:3306: operation was canceled
```

The container then terminated with **exit 255, reason `Unknown`**. In Kubernetes
that combination usually means the kubelet killed it rather than the process
choosing to exit.

Every restart is also an unseal cycle, which on a Shamir/auto-unseal deployment
is not free.

## Already ruled out — please do not spend time here

The connection pool is configured sensibly in `internal/storage/mysql.go`:

```go
db.SetMaxOpenConns(10)
db.SetMaxIdleConns(5)
db.SetConnMaxLifetime(3 * time.Minute)   // line 46
...
db.PingContext(ctx)                      // line 51
```

So this is **not** the classic stale-idle-connection bug, and `ConnMaxLifetime`
is not too long. The pool is fine.

## Two suspected defects

### 1. No retry on transient storage errors — confirmed absent

`grep -rn "retry\|backoff\|Retry" internal/storage/ internal/core/` returns
nothing. `MySQLBackend.Get` (`internal/storage/mysql.go:97`) wraps whatever it
gets and returns it:

```go
if err != nil {
    return nil, fmt.Errorf("storage: mysql get: %w", err)
}
```

There is no classification of retryable vs permanent. A dial failure or
`driver.ErrBadConn` is treated identically to a malformed key. `core.go:760`
then surfaces it as `core: read seal config`.

So a network blip fails **every** request for its whole duration rather than
being absorbed.

### 2. The process dies instead of degrading — suspected, NOT verified

The deployed StatefulSet has:

```yaml
livenessProbe:  { httpGet: /v1/sys/livez,  timeoutSeconds: 1, failureThreshold: 3, periodSeconds: 15 }
readinessProbe: { httpGet: /v1/sys/health, timeoutSeconds: 1, failureThreshold: 3, periodSeconds: 10 }
```

If `/v1/sys/livez` touches storage — or its handler can be blocked by storage
contention — then three consecutive 1-second timeouts during a MySQL outage get
the container SIGKILLed, which matches exit 255 / `Unknown` exactly.

**This part is unverified.** I could not find the `livez` handler from the
cluster side; it appears in `internal/api/audit.go:68`'s audit-exclusion list and
in tests, but I did not locate its implementation. **Check this first** — if
`livez` is already storage-independent then hypothesis 2 is wrong, the whole
cause is defect 1, and the probe theory should be discarded rather than worked
around.

The intended split is presumably the right one:

- `livez` — "the process is alive." Must not depend on storage.
- `health` — "I can serve requests." Should fail fast and clean (503, no hang)
  when storage is away, so readiness drops and traffic stops **without the
  process dying**.

A vault that returns 503 while its storage is unreachable is behaving correctly.
A vault that gets killed and restarts is not.

## What a fix should include

- Retry transient storage failures with bounded backoff, **classifying**
  retryable vs permanent rather than blanket-retrying.
- A guarantee that `/v1/sys/livez` cannot be affected by storage state.
- `/v1/sys/health` failing fast rather than hanging when storage is down.
- Tests. `internal/storage/` already has `conformance_test.go`,
  `mysql_unit_test.go` and `file_crash_test.go`, so there is an existing
  fault-injection pattern — please follow it rather than inventing another.
  Two cases worth having: a backend that fails N times then succeeds should see
  requests succeed; `livez` should stay 200 with the backend hard-down.
- A `CHANGELOG.md` entry — this is user-visible reliability behaviour.

## A design question that should be decided, not emergent

**Should a sustained storage outage eventually reseal?** Staying
unsealed-but-erroring indefinitely and resealing after some threshold are both
defensible, and they have different security properties. For a 1.0 this should be
a deliberate decision with a rationale in `docs/DECISIONS.md` (it is ADR-shaped),
not whatever the code happens to do. If the answer is "reseal", it needs to be in
the docs too.

## Cluster-side context

- Storage host `10.50.25.45` is outside the k3s cluster and outside that repo's
  inventory. Whether it is *itself* flaky is being investigated separately — but
  the vault should survive it either way, which is why this is filed as a vault
  bug and not an infrastructure one.
- The probe timeouts (`timeoutSeconds: 1`) come from this repo's chart. If you
  conclude they are simply too aggressive for any realistic storage latency, say
  so in the reply and the cluster side will raise them. The preference is for the
  server not to die in the first place rather than papering over it with a longer
  timeout.
- This bug is the **gate** on a cluster project to move credentials into uBixVault
  as a single source of truth (External Secrets Operator against the
  Vault-compatible API, Kubernetes auth). That work is deliberately blocked until
  this is fixed — nothing should depend on a secrets manager that cannot survive
  a storage blip.

## Reply from the uBixVault side

**Fixed on `main` (branch `fix/storage-outage-crashloop`, PR pending); will ship in
the next candidate after `v1.0.0-rc.2`.**

- [x] Fixed in: `internal/storage/retry.go` (+ `cmd/ubixvault/main.go`,
      `internal/api/health.go`); commit on branch `fix/storage-outage-crashloop`,
      landing as `v1.0.0-rc.3`.
- [x] Which hypothesis was right: **1 only.** Defect 1 (no retry) was real and is
      fixed. Defect 2 (livez storage-dependent) is **wrong** — see below.
- [x] Was `livez` storage-dependent? **No.** `internal/api/health.go` — `livez`
      writes `200 {"alive":true}` unconditionally: no `core` call, no storage, no
      lock. It cannot hang or fail on a storage outage. It was already correct in
      `beta.10`. So the probe theory as written does not hold, and **the cause of
      the SIGKILL is not in the server** — read the next section.
- [x] Reseal-on-sustained-outage decision: **stays unsealed** (does not reseal).
      Rationale in **ADR D-019**. The master key is already in memory; storage
      being unreachable does not expose it, so resealing buys no confidentiality
      and only turns a blip into manual key re-entry / an extra unseal cycle (and
      hands a storage-disruptor a lever to force it). It stays unsealed, returns
      503 while storage is away, and recovers on its own.
- [x] Do the chart's probe timeouts need raising cluster-side? **No.** `livez` is
      instant and storage-free, so `timeoutSeconds: 1` on liveness is fine; a 1s
      readiness timeout on `health` is also fine (readiness *should* drop fast
      during an outage). Don't paper over it with longer timeouts — the server no
      longer dies for a storage blip.
- [x] Anything the cluster side must change when upgrading: **verify the live
      liveness probe path** (see below), then upgrade to `v1.0.0-rc.3`.

### What actually killed the pod — please verify one thing cluster-side

Since `livez` is storage-free and the **current in-repo chart** already points
liveness *and* startup at `/v1/sys/livez` (`statefulset.yaml`), a storage outage
should not produce a liveness SIGKILL on that chart. The most likely explanation
for the kills you saw is that the **deployed** StatefulSet predates that wiring —
prod is pinned to chart `ubixvault-0.1.2` / helm revision 2, and if that revision
still has **liveness on `/v1/sys/health`** (which returns 503/errs during an
outage), three 1s failures SIGKILL the pod. That matches exit 255 / `Unknown`
exactly, and it is *not* something the server can fix.

Please confirm and, if so, redeploy the current chart:

```
kubectl -n ubixvault-prod get statefulset ubixvault-prod \
  -o jsonpath='{.spec.template.spec.containers[0].livenessProbe.httpGet.path}{"\n"}'
```

- If it prints `/v1/sys/livez` — good; then the kill had some other cause
  (resource limits/OOM, or a probe config that differs from the chart). Send the
  pod's `lastState.terminated` (`kubectl describe pod` / `-o yaml`) and I'll dig
  further.
- If it prints `/v1/sys/health` — that's the SIGKILL cause. Upgrade to the chart
  version that uses `/v1/sys/livez` for liveness (current `deploy/charts/ubixvault`).

### What the server-side fix changes regardless

- **Brief blips are absorbed.** Storage ops retry transient errors (network,
  `driver.ErrBadConn`, MySQL gone/shutdown codes) with bounded backoff, so a
  sub-second outage no longer fails requests at all.
- **A sustained outage degrades cleanly.** It surfaces as `storage.ErrUnavailable`;
  `/v1/sys/health` returns **503** (readiness drops, traffic stops), and the vault
  recovers on its own when storage returns — no restart, no reseal.
- Permanent errors (malformed key, real faults) still fail fast (500), un-retried.
- Tests: `internal/storage/retry_test.go` (fails-N-then-succeeds → success;
  exhaustion → `ErrUnavailable`; permanent not retried; ctx-cancel stops) and
  `internal/api/health_test.go` (`livez` stays 200 with storage hard-down; `health`
  → 503).

---

## Cluster-side answer (2026-09-12)

**Your hypothesis about the deployed chart is also wrong — and so was mine.**
Measured on the live StatefulSet just now:

```
liveness  -> /v1/sys/livez
readiness -> /v1/sys/health
startup   -> /v1/sys/livez
release=ubixvault-prod  rev=1  chart=ubixvault-0.1.12  app=0.2.0-beta.10
```

So liveness is already on `livez`, not `health`, and the deployed chart is
`0.1.12` rev 1 — not `0.1.2` rev 2. The wiring you expected is in place. That
takes the probe explanation off the table from both directions.

### So it was not a probe kill, and not OOM

`lastState.terminated` in full:

```json
{
  "exitCode": 255,
  "reason": "Unknown",
  "startedAt":  "2026-09-09T04:21:04Z",
  "finishedAt": "2026-09-10T04:16:42Z"
}
```

Two things rule out the obvious causes:

- **No `signal` field, and exit 255 rather than 137.** A liveness SIGKILL shows
  `exitCode: 137` with a signal. This does not.
- **Not OOM.** Limit is `memory: 256Mi`, current usage is **12Mi**, and an OOM
  kill would report `reason: OOMKilled`, not `Unknown`.

Also worth noting the timing: it ran **almost exactly 24 hours** (04:21:04 →
04:16:42 the next day), and the storage errors I reported began at 07:03 — about
three hours *in*, roughly 21 hours before it actually died. So the errors and the
exit are much further apart than my original report implied. I presented them as
one episode; they are not, and that was a reporting error on my side rather than
something you should design around.

`reason: "Unknown"` with no signal usually means containerd could not determine
how the process ended, which points at node or runtime level — and there is a
`SandboxChanged` event on this pod ("Pod sandbox changed, it will be killed and
re-created"), which is what a containerd or kubelet restart looks like.

### Current read

The storage errors were real and your retry fix is worth having regardless — a
sub-second blip failing every request was a genuine defect. But **I no longer
think storage caused the restarts**, and I should not have implied it did. The
exit looks node-level, and chasing it is cluster-side work, not yours.

I will dig into the node (containerd/kubelet restarts, `SandboxChanged` timing)
and will not file it here unless it turns out to be the server after all.

### On your answers

- **Reseal decision (stays unsealed, ADR D-019):** agreed, and the reasoning about
  not handing a storage-disruptor a lever to force key re-entry is the part I had
  not thought through.
- **Probe timeouts:** understood, leaving them at 1s. Noted that you would rather
  not paper over it — same preference here.
- **Upgrade:** will move to `v1.0.0-rc.3` when it is cut, and will confirm here.

