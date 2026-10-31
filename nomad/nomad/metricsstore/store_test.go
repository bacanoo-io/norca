// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testConfig(dir string) *Config {
	cfg := DefaultConfig()
	cfg.Dir = dir
	cfg.PartitionDuration = 2 * time.Hour
	cfg.Retention = 24 * time.Hour
	return cfg
}

func openTest(t *testing.T, cfg *Config) *Store {
	t.Helper()
	s, err := Open(cfg, hclog.NewNullLogger())
	must.NoError(t, err)
	return s
}

func usage(job string, samples int, cpuAvg, cpuMax float64) *structs.TaskGroupUsageReport {
	return &structs.TaskGroupUsageReport{NodeID: "node", Window: 10 * time.Second, Groups: []*structs.TaskGroupUsage{{
		Namespace: "default", JobID: job, TaskGroup: "web", Allocs: 1, Samples: samples,
		CPUPercent: structs.TaskGroupUsageValue{Avg: cpuAvg, Max: cpuMax},
		CPUMHz:     structs.TaskGroupUsageValue{Avg: cpuAvg * 10, Max: cpuMax * 10},
		MemoryMB:   structs.TaskGroupUsageValue{Avg: 100, Max: 120},
	}}}
}

var web = GroupKey{Namespace: "default", Job: "api", Group: "web"}

// TestStore_Coalesce checks the weighted averages and the maxima of the
// reports of one window, and that a late report of a written window is
// dropped.
func TestStore_Coalesce(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-time.Hour).Truncate(30 * time.Second)
	s.Ingest(usage("api", 1, 10, 20), base.Add(time.Second))
	s.Ingest(usage("api", 3, 50, 60), base.Add(11*time.Second))
	must.NoError(t, s.flush(base.Add(30*time.Second)))

	points, _, err := s.Query(web, MetricCPU, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, base.UTC(), points[0].Time)
	must.Eq(t, 40.0, points[0].Avg) // (10*1 + 50*3) / 4
	must.Eq(t, 60.0, points[0].Max)
	points, _, err = s.Query(web, MetricMemoryMB, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Eq(t, 100.0, points[0].Avg)

	s.Ingest(usage("api", 1, 99, 99), base.Add(20*time.Second))
	must.Eq(t, 1, s.Status().DroppedReports)
	must.NoError(t, s.flush(base.Add(time.Minute)))
	points, _, err = s.Query(web, MetricCPU, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, 40.0, points[0].Avg)

	_, _, err = s.Query(web, "norca.group.unknown", base, base.Add(time.Minute), 0)
	must.ErrorContains(t, err, "unknown metric")
}

// TestStore_CounterAndState checks the OOM kills (a sum per window and per
// step), the new usage values and the allocations sampled by the server.
func TestStore_CounterAndState(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	oom := func(kills int, throttled float64) *structs.TaskGroupUsageReport {
		r := usage("api", 2, 10, 10)
		r.Groups[0].OOMKills = kills
		r.Groups[0].CPUThrottledPercent = structs.TaskGroupUsageValue{Avg: throttled, Max: throttled * 2}
		r.Groups[0].MemoryCacheMB = structs.TaskGroupUsageValue{Avg: 30, Max: 40}
		return r
	}
	s.Ingest(oom(1, 10), base.Add(time.Second))
	s.Ingest(oom(2, 20), base.Add(2*time.Second))
	s.Ingest(oom(3, 0), base.Add(31*time.Second))
	s.RecordAllocs(base.Add(5*time.Second), map[GroupKey]AllocCounts{
		web: {Running: 3, Queued: 1},
		{Namespace: "default", Job: "idle", Group: "web"}: {},
	})
	s.RecordAllocs(base.Add(35*time.Second), map[GroupKey]AllocCounts{web: {Running: 4}})
	must.NoError(t, s.flush(base.Add(time.Minute)))

	// Per window: 3 then 3 kills; one step of a minute sums them.
	points, _, err := s.Query(web, MetricOOMKills, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 2, points)
	must.Eq(t, 3.0, points[0].Avg)
	must.Eq(t, 2.0, points[0].Max)
	points, _, err = s.Query(web, MetricOOMKills, base, base.Add(time.Minute), 1)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, 6.0, points[0].Avg)

	points, _, err = s.Query(web, MetricCPUThrottled, base, base.Add(30*time.Second), 0)
	must.NoError(t, err)
	must.Eq(t, 15.0, points[0].Avg)
	must.Eq(t, 40.0, points[0].Max)
	points, _, err = s.Query(web, MetricMemoryCacheMB, base, base.Add(30*time.Second), 0)
	must.NoError(t, err)
	must.Eq(t, 30.0, points[0].Avg)

	points, _, err = s.Query(web, MetricAllocsRunning, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 2, points)
	must.Eq(t, 3.0, points[0].Avg)
	must.Eq(t, 4.0, points[1].Avg)
	points, _, err = s.Query(web, MetricAllocsQueued, base, base.Add(30*time.Second), 0)
	must.NoError(t, err)
	must.Eq(t, 1.0, points[0].Avg)

	// A group with nothing running or queued gets no series.
	must.Eq(t, []GroupKey{web}, s.Groups())
}

// TestStore_UsageSince checks the usage read back for the autoscaler: the
// stored windows and those not written yet.
func TestStore_UsageSince(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-3 * time.Minute).Truncate(30 * time.Second)
	s.Ingest(usage("api", 2, 10, 20), base.Add(time.Second))
	s.Ingest(usage("api", 2, 30, 40), base.Add(31*time.Second))
	must.NoError(t, s.flush(base.Add(30*time.Second))) // the second stays pending
	s.RecordAllocs(base.Add(time.Second), map[GroupKey]AllocCounts{{Namespace: "default", Job: "batch", Group: "w"}: {Running: 1}})

	windows, err := s.UsageSince(base)
	must.NoError(t, err)
	must.Len(t, 2, windows)
	must.Eq(t, base.Unix(), windows[0].Start.Unix())
	must.Eq(t, "api", windows[0].Usage.JobID)
	must.Eq(t, 10.0, windows[0].Usage.CPUPercent.Avg)
	must.Eq(t, 20.0, windows[0].Usage.CPUPercent.Max)
	must.Eq(t, 100.0, windows[0].Usage.MemoryMB.Avg)
	must.Eq(t, 30.0, windows[1].Usage.CPUPercent.Avg)
	must.Eq(t, 1, windows[1].Usage.Samples)

	windows, err = s.UsageSince(base.Add(time.Minute))
	must.NoError(t, err)
	must.SliceEmpty(t, windows)
}

// TestStore_Downsample checks the steps of a query: the mean of the
// averages and the maximum of the maxima of each step.
func TestStore_Downsample(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-90 * time.Minute).Truncate(time.Hour)
	if time.Since(base) > 100*time.Minute {
		base = base.Add(time.Hour)
	}
	for i := 0; i < 120; i++ { // one hour of 30s windows
		v := float64(i % 10)
		s.Ingest(usage("api", 1, v, v+100), base.Add(time.Duration(i)*30*time.Second))
	}
	must.NoError(t, s.flush(base.Add(time.Hour)))

	points, step, err := s.Query(web, MetricCPU, base, base.Add(time.Hour), 6)
	must.NoError(t, err)
	must.Eq(t, 10*time.Minute, step)
	must.Len(t, 6, points)
	for i, p := range points {
		must.Eq(t, base.Add(time.Duration(i)*10*time.Minute).UTC(), p.Time)
		must.Eq(t, 4.5, p.Avg)
		must.Eq(t, 109.0, p.Max)
	}

	// The raw resolution when the range is short.
	points, step, err = s.Query(web, MetricCPU, base, base.Add(5*time.Minute), 0)
	must.NoError(t, err)
	must.Eq(t, 30*time.Second, step)
	must.Len(t, 10, points)

	// A group without points.
	points, _, err = s.Query(GroupKey{Namespace: "default", Job: "none", Group: "web"}, MetricCPU, base, base.Add(time.Hour), 0)
	must.NoError(t, err)
	must.SliceEmpty(t, points)
}

// TestStore_MaxSeries checks that the task groups beyond max_series are
// left out.
func TestStore_MaxSeries(t *testing.T) {
	ci.Parallel(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSeries = seriesPerGroup
	s := openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()

	now := time.Now()
	s.Ingest(usage("api", 1, 1, 1), now)
	s.Ingest(usage("other", 1, 1, 1), now)
	must.Eq(t, []GroupKey{web}, s.Groups())
	st := s.Status()
	must.Eq(t, 1, st.DroppedGroups)
	must.Eq(t, seriesPerGroup, st.Series)
}

// TestStore_Reopen checks that the points and the task groups survive a
// restart, and the disk cap.
func TestStore_Reopen(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.PartitionDuration = 5 * time.Minute
	s := openTest(t, cfg)

	base := time.Now().Add(-90 * time.Minute).Truncate(30 * time.Second)
	for i := 0; i < 120; i++ {
		at := base.Add(time.Duration(i) * 30 * time.Second)
		s.Ingest(usage("api", 1, 5, 5), at)
		must.NoError(t, s.flush(at.Add(30*time.Second)))
	}
	must.NoError(t, s.Close())

	s = openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()
	must.Eq(t, []GroupKey{web}, s.Groups())
	points, _, err := s.Query(web, MetricCPU, base, base.Add(time.Hour), 120)
	must.NoError(t, err)
	must.Len(t, 120, points)
	st := s.Status()
	must.Greater(t, 1, st.DiskPartitions)
	must.Positive(t, st.DiskBytes)
	must.Eq(t, base.UTC(), st.Oldest)

	// Above the disk cap the oldest partitions go first.
	s.cfg.MaxDiskSize = st.DiskBytes / 2
	s.maintain()
	after := s.Status()
	must.LessEq(t, s.cfg.MaxDiskSize, after.DiskBytes)
	must.True(t, after.Oldest.After(st.Oldest))
	points, _, err = s.Query(web, MetricCPU, base, base.Add(time.Hour), 120)
	must.NoError(t, err)
	must.Less(t, 120, len(points))
	must.Positive(t, len(points))
}

// TestConfig_Validate checks the limits of the configuration.
func TestConfig_Validate(t *testing.T) {
	ci.Parallel(t)
	must.NoError(t, DefaultConfig().Validate())
	cfg := DefaultConfig()
	cfg.Retention = 31 * 24 * time.Hour
	must.ErrorContains(t, cfg.Validate(), "retention")
	cfg = DefaultConfig()
	cfg.SampleInterval = time.Hour
	cfg.PartitionDuration = time.Minute
	must.ErrorContains(t, cfg.Validate(), "partition_duration")
	cfg.Enabled = false
	must.NoError(t, cfg.Validate())
}
