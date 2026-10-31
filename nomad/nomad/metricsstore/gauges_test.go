// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestStore_Gauges checks the gauge series: the last value of an interval,
// unknown metrics, the series limit and the series file.
func TestStore_Gauges(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	s := openTest(t, cfg)

	base := time.Now().Add(-time.Hour).Truncate(30 * time.Second)
	dc1 := map[string]string{"dc": "dc1", "pool": "default"}
	s.RecordGauges(base.Add(time.Second), []Gauge{
		{Metric: MetricClusterCPUUsed, Labels: dc1, Value: 10},
		{Metric: MetricQuotaCPUUsed, Labels: map[string]string{"quota": "team"}, Value: 500},
		{Metric: "norca.cluster.unknown", Value: 1},
	})
	s.RecordGauges(base.Add(20*time.Second), []Gauge{{Metric: MetricClusterCPUUsed, Labels: dc1, Value: 30}})
	s.RecordGauges(base.Add(31*time.Second), []Gauge{{Metric: MetricClusterCPUUsed, Labels: dc1, Value: 50}})
	must.NoError(t, s.flush(base.Add(time.Minute)))

	points, _, err := s.QueryGauge(MetricClusterCPUUsed, dc1, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 2, points)
	must.Eq(t, 30.0, points[0].Avg)
	must.Eq(t, 50.0, points[1].Avg)
	_, _, err = s.QueryGauge("norca.cluster.unknown", nil, base, base.Add(time.Minute), 0)
	must.ErrorContains(t, err, "unknown metric")
	must.Len(t, 2, s.Gauges())
	must.Eq(t, 4, s.Status().Series)
	must.NoError(t, s.Close())

	// The series survive a restart; the limit counts them.
	cfg.MaxSeries = 4
	s = openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()
	must.Eq(t, []GaugeSeries{
		{Metric: MetricClusterCPUUsed, Labels: dc1},
		{Metric: MetricQuotaCPUUsed, Labels: map[string]string{"quota": "team"}},
	}, s.Gauges())
	s.RecordGauges(time.Now(), []Gauge{{Metric: MetricClusterNodesReady, Labels: dc1, Value: 3}})
	must.Len(t, 2, s.Gauges())
	must.Eq(t, 1, s.Status().DroppedGroups)
	s.Ingest(usage("api", 1, 1, 1), time.Now())
	must.SliceEmpty(t, s.Groups())
}

// TestStore_Live checks the current usage of the nodes: the parts of a
// split report add up, a later report replaces them, a stale node is left
// out.
func TestStore_Live(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	now := time.Now()
	part := func(node, ns string, allocs int, cpu, mem float64) *structs.TaskGroupUsageReport {
		return &structs.TaskGroupUsageReport{NodeID: node, Window: 10 * time.Second, Groups: []*structs.TaskGroupUsage{{
			Namespace: ns, JobID: "j", TaskGroup: "g", Allocs: allocs, Samples: 1,
			CPUMHz: structs.TaskGroupUsageValue{Avg: cpu}, MemoryMB: structs.TaskGroupUsageValue{Avg: mem},
		}}}
	}
	s.Ingest(part("n1", "default", 2, 100, 50), now.Add(-15*time.Second))
	s.Ingest(part("n1", "default", 1, 999, 999), now.Add(-5*time.Second)) // replaces
	s.Ingest(part("n1", "team", 1, 10, 20), now.Add(-4*time.Second))      // a part of it
	s.Ingest(part("n2", "team", 3, 100, 100), now.Add(-time.Minute))      // stale

	live := s.Live(now, time.Minute)
	must.Eq(t, map[string]Usage{"n1": {CPUMHz: 1009, MemoryMB: 1019}}, live.Nodes)
	must.Eq(t, Usage{CPUMHz: 999, MemoryMB: 999}, live.Namespaces["default"])
	must.Eq(t, Usage{CPUMHz: 10, MemoryMB: 20}, live.Namespaces["team"])
}

// TestStore_Summarize checks the mean, the 95th percentile and the maximum
// over a range.
func TestStore_Summarize(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-90 * time.Minute).Truncate(30 * time.Second)
	for i := 0; i < 100; i++ { // maxima 1..100
		s.Ingest(usage("api", 1, 10, float64(i+1)), base.Add(time.Duration(i)*30*time.Second))
	}
	must.NoError(t, s.flush(base.Add(time.Hour)))

	sum, err := s.Summarize(web, MetricCPU, base, base.Add(time.Hour))
	must.NoError(t, err)
	must.Eq(t, 100, sum.Samples)
	must.Eq(t, 10.0, sum.Avg)
	must.Eq(t, 95.0, sum.P95)
	must.Eq(t, 100.0, sum.Max)

	sum, err = s.Summarize(GroupKey{Namespace: "default", Job: "none", Group: "web"}, MetricCPU, base, base.Add(time.Hour))
	must.NoError(t, err)
	must.Eq(t, 0, sum.Samples)
	_, err = s.Summarize(web, MetricOOMKills, base, base.Add(time.Hour))
	must.ErrorContains(t, err, "unknown metric")
}
