// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// usageReport returns a report of one task group with the same average and
// maximum for every value.
func usageReport(job, group string, samples int, cpuPercent, memoryMB float64) *structs.TaskGroupUsageReport {
	return &structs.TaskGroupUsageReport{
		NodeID: "node",
		Window: 10 * time.Second,
		Groups: []*structs.TaskGroupUsage{{
			Namespace:     structs.DefaultNamespace,
			JobID:         job,
			TaskGroup:     group,
			Allocs:        1,
			Samples:       samples,
			CPUPercent:    structs.TaskGroupUsageValue{Avg: cpuPercent, Max: cpuPercent},
			CPUMHz:        structs.TaskGroupUsageValue{Avg: cpuPercent * 10, Max: cpuPercent * 10},
			MemoryMB:      structs.TaskGroupUsageValue{Avg: memoryMB, Max: memoryMB},
			MemoryPercent: structs.TaskGroupUsageValue{Avg: memoryMB / 2, Max: memoryMB / 2},
		}},
	}
}

func webKey(job string) targetKey {
	return targetKey{namespace: structs.DefaultNamespace, job: job, group: "web"}
}

func TestUsageCollector_InsufficientDataUntilFullWindow(t *testing.T) {
	ci.Parallel(t)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newUsageCollector()
	key := webKey("api")

	// Inactive (not the leader): reports are ignored.
	c.record(clock.Now(), usageReport("api", "web", 10, 50, 100))
	_, err := c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Minute), clock.Now())
	must.ErrorIs(t, err, ErrInsufficientData)

	c.setActive(true, 5*time.Minute, clock.Now())
	must.Eq(t, 0, c.numGroups())

	// Data arrives, but the window since the start is not complete.
	for i := 0; i < 29; i++ {
		clock.Advance(10 * time.Second)
		c.record(clock.Now(), usageReport("api", "web", 10, 50, 100))
		_, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Minute), clock.Now())
		must.ErrorIs(t, err, ErrInsufficientData)
	}

	clock.Advance(10 * time.Second)
	c.record(clock.Now(), usageReport("api", "web", 10, 50, 100))
	agg, err := c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Minute), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 50.0, agg.avg)

	// Unknown task groups have no data.
	_, err = c.query(clock.Now(), webKey("other"), usageCPUPercent, clock.Now().Add(-time.Minute), clock.Now())
	must.ErrorIs(t, err, errNoUsage)
}

func TestUsageCollector_WeightedAverageAndMax(t *testing.T) {
	ci.Parallel(t)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newUsageCollector()
	c.setActive(true, time.Minute, clock.Now())
	clock.Advance(time.Minute)
	key := webKey("api")

	// Two nodes report in the same bucket, with different sample counts.
	c.record(clock.Now(), usageReport("api", "web", 10, 20, 100))
	c.record(clock.Now(), usageReport("api", "web", 30, 60, 300))
	clock.Advance(10 * time.Second)
	c.record(clock.Now(), usageReport("api", "web", 20, 40, 200))

	from, to := clock.Now().Add(-time.Minute), clock.Now()
	agg, err := c.query(clock.Now(), key, usageCPUPercent, from, to)
	must.NoError(t, err)
	// (10*20 + 30*60 + 20*40) / 60
	must.Eq(t, 2800.0/60, agg.avg)
	must.Eq(t, 60.0, agg.max)

	agg, err = c.query(clock.Now(), key, usageMemoryMB, from, to)
	must.NoError(t, err)
	must.Eq(t, (10*100.0+30*300+20*200)/60, agg.avg)
	must.Eq(t, 300.0, agg.max)

	// A short range only sees the latest bucket.
	agg, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Second), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 40.0, agg.avg)

	// So does an instant query.
	agg, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now(), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 40.0, agg.avg)
}

func TestUsageCollector_Expiry(t *testing.T) {
	ci.Parallel(t)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	c := newUsageCollector()
	c.setActive(true, time.Minute, clock.Now())
	clock.Advance(time.Minute)
	key := webKey("api")

	c.record(clock.Now(), usageReport("api", "web", 1, 90, 100))
	c.record(clock.Now(), usageReport("old", "web", 1, 90, 100))
	clock.Advance(30 * time.Second)
	c.record(clock.Now(), usageReport("api", "web", 1, 10, 100))

	// The query range is limited to the window.
	agg, err := c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Hour), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 50.0, agg.avg)

	// Older data leaves the window.
	clock.Advance(45 * time.Second)
	agg, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Hour), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 10.0, agg.avg)

	// The ring buffer reuses the expired buckets.
	for i := 0; i < 12; i++ {
		clock.Advance(5 * time.Second)
		c.record(clock.Now(), usageReport("api", "web", 1, 30, 100))
	}
	agg, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Hour), clock.Now())
	must.NoError(t, err)
	must.Eq(t, 30.0, agg.avg)

	// Task groups without reports within the window are dropped.
	must.Eq(t, 1, c.numGroups())
	_, err = c.query(clock.Now(), webKey("old"), usageCPUPercent, clock.Now().Add(-time.Hour), clock.Now())
	must.ErrorIs(t, err, errNoUsage)

	// Without new reports the group has no data in range.
	clock.Advance(2 * time.Minute)
	_, err = c.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Hour), clock.Now())
	must.ErrorIs(t, err, errNoUsage)
}

func TestUsageCollector_LeaderReset(t *testing.T) {
	ci.Parallel(t)

	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	// The run itself waits for the servers forever; only the collector is
	// exercised.
	a := New(hclog.NewNullLogger(), &Config{Enabled: true, MetricsWindow: time.Minute}, &Deps{
		Now:          clock.Now,
		ServersReady: func() bool { return false },
	})
	key := webKey("api")
	query := func() error {
		_, err := a.usage.query(clock.Now(), key, usageCPUPercent, clock.Now().Add(-time.Minute), clock.Now())
		return err
	}

	// Followers ignore reports.
	a.RecordUsage(usageReport("api", "web", 1, 50, 100))
	must.Eq(t, 0, a.usage.numGroups())

	a.SetLeader(true)
	t.Cleanup(a.Stop)

	clock.Advance(time.Minute)
	a.RecordUsage(usageReport("api", "web", 1, 50, 100))
	must.NoError(t, query())

	// A configuration change that keeps the window keeps the data.
	a.SetConfig(&Config{Enabled: true, MetricsWindow: time.Minute, EvalWorkers: 1})
	must.NoError(t, query())

	// Losing leadership drops the data; a new leadership starts empty and
	// reports insufficient data for a full window.
	a.SetLeader(false)
	must.Eq(t, 0, a.usage.numGroups())
	a.SetLeader(true)
	a.RecordUsage(usageReport("api", "web", 1, 50, 100))
	must.ErrorIs(t, query(), ErrInsufficientData)
	clock.Advance(time.Minute)
	a.RecordUsage(usageReport("api", "web", 1, 50, 100))
	must.NoError(t, query())

	// A new window restarts collection.
	a.SetConfig(&Config{Enabled: true, MetricsWindow: 2 * time.Minute, EvalWorkers: 1})
	must.ErrorIs(t, query(), ErrInsufficientData)

	// Disabling the autoscaler stops collecting.
	a.SetConfig(&Config{Enabled: false, MetricsWindow: 2 * time.Minute})
	a.RecordUsage(usageReport("api", "web", 1, 50, 100))
	must.Eq(t, 0, a.usage.numGroups())
	must.Eq(t, time.Duration(0), a.UsageInterval())
}

func TestUsageCollector_IgnoresInvalidGroups(t *testing.T) {
	ci.Parallel(t)

	now := time.Unix(1_700_000_000, 0)
	c := newUsageCollector()
	c.setActive(true, time.Minute, now)

	c.record(now, nil)
	c.record(now, &structs.TaskGroupUsageReport{Groups: []*structs.TaskGroupUsage{
		nil,
		{JobID: "api", TaskGroup: "web", Samples: 0},
		{JobID: "", TaskGroup: "web", Samples: 1},
		{JobID: "api", TaskGroup: "", Samples: 1},
	}})
	must.Eq(t, 0, c.numGroups())

	// An empty namespace is the default namespace.
	c.record(now, &structs.TaskGroupUsageReport{Groups: []*structs.TaskGroupUsage{
		{JobID: "api", TaskGroup: "web", Samples: 1},
	}})
	_, ok := c.groups[webKey("api")]
	must.True(t, ok)
}

func TestUsageAPM_Query(t *testing.T) {
	ci.Parallel(t)

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	c := newUsageCollector()
	c.setActive(true, time.Minute, h.clock.Now())
	h.clock.Advance(time.Minute)

	group := h.job.TaskGroups[0].Name
	key := targetKey{namespace: h.job.Namespace, job: h.job.ID, group: group}
	c.record(h.clock.Now(), usageReport(h.job.ID, group, 10, 40, 64))
	c.record(h.clock.Now(), &structs.TaskGroupUsageReport{Groups: []*structs.TaskGroupUsage{{
		Namespace: h.job.Namespace, JobID: h.job.ID, TaskGroup: group, Samples: 10,
		CPUPercent: structs.TaskGroupUsageValue{Avg: 60, Max: 90},
		CPUMHz:     structs.TaskGroupUsageValue{Avg: 600, Max: 900},
		MemoryMB:   structs.TaskGroupUsageValue{Avg: 64, Max: 64},
	}}})

	apm := &usageAPM{collector: c, state: h.deps.State, target: key, now: h.clock.Now}
	r := sdk.TimeRange{From: h.clock.Now().Add(-time.Minute), To: h.clock.Now()}
	suffix := fmt.Sprintf("/%s/%s@%s", group, h.job.ID, h.job.Namespace)

	cases := []struct {
		query string
		value float64
		err   string
	}{
		{query: MetricCPU, value: 50},
		{query: MetricCPUMHz, value: 500},
		{query: MetricMemoryMB, value: 64},
		{query: MetricMemory, value: 16},
		{query: "taskgroup_" + MetricCPUMHz + suffix, value: 500},
		{query: "taskgroup_avg_cpu-allocated" + suffix, value: 50},
		{query: "taskgroup_max_cpu-allocated" + suffix, value: 90},
		{query: "taskgroup_avg_memory" + suffix, value: 64 * 1024 * 1024},
		// Two running allocations in the harness.
		{query: "taskgroup_sum_cpu-allocated" + suffix, value: 100},
		{query: "max_cpu-allocated", value: 90},
		{query: "taskgroup_min_cpu-allocated" + suffix, err: "invalid operation"},
		{query: "taskgroup_avg_cpu" + suffix, err: "invalid metric"},
		{query: "nomad.disk", err: "unsupported metric"},
		{query: "nomad.cpu/web", err: "expected <query>"},
		{query: "nomad.cpu/web/job", err: "expected <query>"},
		{query: "taskgroup_avg_cpu-allocated/web/other@default", err: errNoUsage.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			ms, err := apm.Query(tc.query, r)
			if tc.err != "" {
				must.ErrorContains(t, err, tc.err)
				return
			}
			must.NoError(t, err)
			must.Len(t, 1, ms)
			must.Eq(t, tc.value, ms[0].Value)
			must.Eq(t, r.To, ms[0].Timestamp)
		})
	}
}

// usagePolicy returns a policy document with one target-value check on a
// built-in metric.
func usagePolicy(metric string, target int) map[string]interface{} {
	return map[string]interface{}{
		"evaluation_interval": "10s",
		"cooldown":            "1m",
		"check": []interface{}{
			map[string]interface{}{
				"cpu": []interface{}{
					map[string]interface{}{
						"metric": metric,
						"strategy": []interface{}{
							map[string]interface{}{
								"target-value": []interface{}{
									map[string]interface{}{"target": target},
								},
							},
						},
					},
				},
			},
		},
	}
}

// withUsage makes the harness runner serve the nomad-apm source from a
// fresh usage collector, as the autoscaler does on a new leader.
func (h *harness) withUsage(window time.Duration) *usageCollector {
	c := newUsageCollector()
	c.setActive(true, window, h.clock.Now())

	logger := hclog.NewNullLogger()
	host, err := newPluginHost(logger, h.cfg, h.deps, nil)
	must.NoError(h.t, err)
	h.runner = newRunner(logger, h.cfg, h.deps, h.clock.Now, host)
	h.runner.usage = c
	h.sync()
	return c
}

func TestAutoscaler_BuiltinMetricNoScalingBeforeFullWindow(t *testing.T) {
	ci.Parallel(t)

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, usagePolicy(MetricCPU, 50), nil)
	c := h.withUsage(time.Minute)
	group := h.job.TaskGroups[0].Name

	p := h.runner.handles[h.policyID()].compiled.policy
	must.Eq(t, "nomad-apm", p.Checks[0].Source)
	must.Eq(t, MetricCPU, p.Checks[0].Query)

	// Low usage would scale down, but the window after the leader change is
	// not complete: no scaling.
	h.clock.Advance(30 * time.Second)
	c.record(h.clock.Now(), usageReport(h.job.ID, group, 10, 10, 100))
	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())

	// After a full window the policy scales on the reported usage.
	h.clock.Advance(30 * time.Second)
	c.record(h.clock.Now(), usageReport(h.job.ID, group, 10, 10, 100))
	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(1), *reqs[0].Count)
}

func TestAutoscaler_BuiltinMetricScaleUp(t *testing.T) {
	ci.Parallel(t)

	// nomad.memory_mb has an underscore and is canonicalized like a short
	// nomad-apm query; the usage APM still serves it.
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, usagePolicy(MetricMemoryMB, 100), nil)
	c := h.withUsage(time.Minute)
	group := h.job.TaskGroups[0].Name

	p := h.runner.handles[h.policyID()].compiled.policy
	must.StrHasPrefix(t, "taskgroup_"+MetricMemoryMB+"/", p.Checks[0].Query)

	h.clock.Advance(time.Minute)
	c.record(h.clock.Now(), usageReport(h.job.ID, group, 10, 10, 200))
	must.NoError(t, h.evaluate())
	reqs := h.scaler.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(4), *reqs[0].Count)
}

// validateDocPolicy validates a policy given as an untyped document, as
// the servers do at submission.
func validateDocPolicy(p *structs.ScalingPolicy) error {
	return ValidatePolicy(p, DefaultConfig())
}

func TestAutoscaler_BuiltinMetricValidation(t *testing.T) {
	ci.Parallel(t)

	check := func(doc map[string]interface{}) map[string]interface{} {
		return doc["check"].([]interface{})[0].(map[string]interface{})["cpu"].([]interface{})[0].(map[string]interface{})
	}
	policy := func(mod func(map[string]interface{})) *structs.ScalingPolicy {
		doc := usagePolicy(MetricCPU, 50)
		mod(check(doc))
		return &structs.ScalingPolicy{
			ID:     "id",
			Target: map[string]string{structs.ScalingTargetJob: "j", structs.ScalingTargetGroup: "g"},
			Policy: doc,
		}
	}

	must.NoError(t, validateDocPolicy(policy(func(map[string]interface{}) {})))
	must.NoError(t, validateDocPolicy(policy(func(c map[string]interface{}) { c["source"] = "nomad-apm" })))
	must.ErrorContains(t, validateDocPolicy(policy(func(c map[string]interface{}) { c["metric"] = "nomad.disk" })),
		"must be one of")
	must.ErrorContains(t, validateDocPolicy(policy(func(c map[string]interface{}) { c["query"] = "avg_cpu" })),
		"only one of metric and query")
	must.ErrorContains(t, validateDocPolicy(policy(func(c map[string]interface{}) { c["source"] = "prometheus" })),
		"requires the nomad-apm source")
	must.ErrorContains(t, validateDocPolicy(policy(func(c map[string]interface{}) { c["metric"] = 1 })),
		"must be a string")
}

// BenchmarkUsageCollector_Memory measures the heap used by the collector
// for 1,000 task groups with a full window of reports. The memory of a task
// group does not depend on the number of nodes that report it.
func BenchmarkUsageCollector_Memory(b *testing.B) {
	const groups = 1000
	window := DefaultMetricsWindow

	var perThousand float64
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		now := time.Unix(1_700_000_000, 0)
		c := newUsageCollector()
		c.setActive(true, window, now)
		reports := make([]*structs.TaskGroupUsageReport, groups)
		for g := 0; g < groups; g++ {
			reports[g] = usageReport(fmt.Sprintf("job-%04d", g), "web", 10, 50, 100)
		}
		// One report per group and client interval for a full window, from
		// ten nodes.
		for t := time.Duration(0); t < window; t += DefaultClientMetricsInterval {
			for n := 0; n < 10; n++ {
				for _, r := range reports {
					c.record(now.Add(t), r)
				}
			}
		}
		reports = nil

		runtime.GC()
		runtime.ReadMemStats(&after)
		perThousand = float64(after.HeapAlloc-before.HeapAlloc) * 1000 / groups
		runtime.KeepAlive(c)
	}
	b.ReportMetric(perThousand, "bytes/1000groups")
}

// TestUsageCollector_Seed checks that seeds from the metrics store make the
// built-in metrics available at once on a new leader, and which seeds are
// left out.
func TestUsageCollector_Seed(t *testing.T) {
	ci.Parallel(t)

	start := time.Unix(1_700_000_000, 0)
	c := newUsageCollector()
	c.setActive(true, 5*time.Minute, start)
	key := webKey("api")
	_, err := c.query(start, key, usageCPUPercent, start.Add(-time.Minute), start)
	must.ErrorIs(t, err, ErrInsufficientData)

	seed := func(ago time.Duration, cpu float64) UsageSeed {
		return UsageSeed{At: start.Add(-ago), Usage: usageReport("api", "web", 1, cpu, 100).Groups[0]}
	}
	// A window of seeds every 30s, one too old, one not older than the
	// start (live reports cover it).
	var seeds []UsageSeed
	for ago := 30 * time.Second; ago <= 5*time.Minute; ago += 30 * time.Second {
		seeds = append(seeds, seed(ago, 40))
	}
	seeds = append(seeds, seed(10*time.Minute, 99), seed(0, 99))
	must.Eq(t, 10, c.seed(start, seeds))

	agg, err := c.query(start, key, usageCPUPercent, start.Add(-5*time.Minute), start)
	must.NoError(t, err)
	must.Eq(t, 40.0, agg.avg)
	must.Eq(t, 40.0, agg.max)

	// A live report of the next bucket is kept apart from the seeds.
	c.record(start.Add(time.Second), usageReport("api", "web", 1, 70, 100))
	agg, err = c.query(start.Add(time.Second), key, usageCPUPercent, start, start.Add(time.Second))
	must.NoError(t, err)
	must.Eq(t, 70.0, agg.avg)

	// An inactive collector takes no seeds.
	idle := newUsageCollector()
	must.Eq(t, 0, idle.seed(start, seeds))
}
