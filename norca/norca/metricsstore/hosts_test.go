// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/internal/tstorage"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// TestHostMetric checks which metrics the hosts store keeps.
func TestHostMetric(t *testing.T) {
	ci.Parallel(t)
	for _, m := range []string{
		MetricNodeCPUUsed, MetricNodeDiskUsedMB + ".avg", MetricNodeInodesUsedPercent + ".max",
		MetricServerRaftDBBytes, MetricServerRaftAppendEntries, MetricServerRPCErrors,
		MetricBusJetStreamBytes, MetricBusRoutes,
	} {
		must.True(t, HostMetric(m), must.Sprint(m))
	}
	for _, m := range []string{
		MetricCPU, MetricAllocsRunning + ".avg", MetricNodesDown, MetricNodesIneligible,
		MetricNodeGroupReplicas, MetricClusterCPUUsed, MetricStoreDiskBytes, MetricSchedBrokerReady,
		MetricAutopilotHealthy, MetricLBReady, "norca.nodes", "norca.node",
	} {
		must.False(t, HostMetric(m), must.Sprint(m))
	}
	// Every host metric is a gauge.
	n := 0
	for _, m := range GaugeMetrics {
		if HostMetric(m) {
			n++
		}
	}
	must.Eq(t, 7+9+3, n)
}

// readyLabels are the labels of the cluster gauge of hostGauges.
var readyLabels = map[string]string{"dc": "dc1", "pool": "default", "class": ""}

// hostGauges records a host and a cluster gauge at the time at.
func hostGauges(s *Store, at time.Time, v float64) {
	s.RecordGauges(at, []Gauge{
		{Metric: MetricServerRaftCommitLag, Value: v},
		{Metric: MetricClusterNodesReady, Labels: readyLabels, Value: v},
	})
}

// TestStore_HostStore checks that the host metrics are written to and read
// from the hosts store only, in its own directory, and the status of both
// stores.
func TestStore_HostStore(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	cfg := testConfig(filepath.Join(dir, "tsdb"))
	cfg.HostRetention = 12 * time.Hour
	s := openTest(t, cfg)

	base := time.Now().Add(-time.Hour).Truncate(30 * time.Second)
	hostGauges(s, base.Add(time.Second), 3)
	n1 := map[string]string{"node": "n1", "dc": "dc1", "pool": "default", "class": ""}
	s.RecordNodeHost(n1, &structs.NodeHostUsage{Samples: 1, CPUPercent: structs.TaskGroupUsageValue{Avg: 20, Max: 30}}, base.Add(2*time.Second))
	s.Ingest(usage("api", 1, 10, 20), base.Add(3*time.Second))
	must.NoError(t, s.flush(base.Add(30*time.Second)))

	// Each series is in one database.
	end := base.Add(time.Minute).Unix()
	in := func(d *database, metric string, labels []tstorage.Label) bool {
		points, err := d.db.Select(metric, labels, base.Unix(), end)
		if errors.Is(err, tstorage.ErrNoDataPoints) {
			return false
		}
		must.NoError(t, err)
		return len(points) > 0
	}
	commitLag := gaugeTSLabels(MetricServerRaftCommitLag, nil)
	nodeCPU := gaugeTSLabels(MetricNodeCPUUsed, n1)
	ready := gaugeTSLabels(MetricClusterNodesReady, readyLabels)
	must.True(t, in(s.hosts, MetricServerRaftCommitLag+".avg", commitLag))
	must.False(t, in(s.main, MetricServerRaftCommitLag+".avg", commitLag))
	must.True(t, in(s.hosts, MetricNodeCPUUsed+".max", nodeCPU))
	must.False(t, in(s.main, MetricNodeCPUUsed+".max", nodeCPU))
	must.True(t, in(s.main, MetricClusterNodesReady+".avg", ready))
	must.False(t, in(s.hosts, MetricClusterNodesReady+".avg", ready))
	must.True(t, in(s.main, MetricCPU+".avg", web.labels()))
	must.False(t, in(s.hosts, MetricCPU+".avg", web.labels()))

	// The queries read from the right database.
	points, _, err := s.QueryGauge(MetricServerRaftCommitLag, nil, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, 3.0, points[0].Avg)
	points, _, err = s.QueryGauge(MetricNodeCPUUsed, n1, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, 30.0, points[0].Max)
	points, _, err = s.QueryGauge(MetricClusterNodesReady, readyLabels, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)

	must.Eq(t, 12*time.Hour, s.RetentionOf(MetricNodeCPUUsed))
	must.Eq(t, 12*time.Hour, s.RetentionOf(MetricBusRoutes))
	must.Eq(t, 24*time.Hour, s.RetentionOf(MetricClusterNodesReady))
	must.Eq(t, 24*time.Hour, s.RetentionOf(MetricCPU))

	// max_series counts both; the status splits them.
	st := s.Status()
	must.Eq(t, 2+2+2+seriesPerGroup, st.Series)
	must.Eq(t, 4, st.Hosts.Series)
	must.Eq(t, 24*time.Hour, st.Retention)
	must.Eq(t, 12*time.Hour, st.Hosts.Retention)
	must.Eq(t, DefaultHostMaxDiskSize, st.Hosts.MaxDiskSize)
	must.Eq(t, DefaultMaxDiskSize, st.MaxDiskSize)
	must.Eq(t, base.UTC(), st.Hosts.Oldest)
	must.Eq(t, base.UTC(), st.Oldest)
	must.NoError(t, s.Close())

	// The hosts store is kept beside the main store, and both survive a
	// restart; the series file is in the main store.
	_, err = os.Stat(filepath.Join(dir, "tsdb-hosts"))
	must.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "tsdb", seriesFile))
	must.NoError(t, err)
	s = openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()
	points, _, err = s.QueryGauge(MetricServerRaftCommitLag, nil, base, base.Add(time.Minute), 0)
	must.NoError(t, err)
	must.Len(t, 1, points)
	must.Eq(t, 4, s.Status().Hosts.Series)
}

// TestStore_HostForget checks that a host series is forgotten after
// host_retention and any other after retention.
func TestStore_HostForget(t *testing.T) {
	ci.Parallel(t)
	cfg := testConfig(t.TempDir()) // retention 24h
	cfg.PartitionDuration = time.Hour
	cfg.HostRetention = 2 * time.Hour
	s := openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()

	now := time.Now()
	hostGauges(s, now.Add(-30*time.Minute), 1)
	must.NoError(t, s.flush(now))
	must.Len(t, 2, s.Gauges())

	// Within both retentions: kept.
	must.Eq(t, 0, s.forget(now.Add(time.Hour)))
	// Past the host retention only: the host series goes.
	must.Eq(t, 2, s.forget(now.Add(3*time.Hour)))
	must.Eq(t, []GaugeSeries{{Metric: MetricClusterNodesReady, Labels: readyLabels}}, s.Gauges())
	must.Eq(t, 0, s.Status().Hosts.Series)
	// Past the retention: the other one too.
	must.Eq(t, 2, s.forget(now.Add(25*time.Hour)))
	must.SliceEmpty(t, s.Gauges())

	// A reload of host_retention applies to the next sweep.
	hostGauges(s, now, 1)
	must.NoError(t, s.flush(now.Add(time.Minute)))
	next := cfg.Copy()
	next.HostRetention = 24 * time.Hour
	must.NoError(t, s.Reload(next))
	must.Eq(t, 0, s.forget(now.Add(3*time.Hour)))
	must.Len(t, 2, s.Gauges())
}

// TestStore_HostDiskCap checks that each store keeps to its own disk cap:
// the oldest partitions of the store above its cap go, the other store is
// untouched.
func TestStore_HostDiskCap(t *testing.T) {
	ci.Parallel(t)
	cfg := testConfig(t.TempDir())
	cfg.PartitionDuration = 5 * time.Minute
	s := openTest(t, cfg)

	base := time.Now().Add(-90 * time.Minute).Truncate(30 * time.Second)
	for i := 0; i < 120; i++ {
		at := base.Add(time.Duration(i) * 30 * time.Second)
		hostGauges(s, at, float64(i))
		must.NoError(t, s.flush(at.Add(30*time.Second)))
	}
	must.NoError(t, s.Close())
	s = openTest(t, cfg)
	defer func() { must.NoError(t, s.Close()) }()

	st := s.Status()
	must.Greater(t, 1, st.DiskPartitions)
	must.Greater(t, 1, st.Hosts.DiskPartitions)
	must.Eq(t, base.UTC(), st.Oldest)
	must.Eq(t, base.UTC(), st.Hosts.Oldest)

	// Above the cap of the hosts store.
	s.hosts.maxDiskSize.Store(st.Hosts.DiskBytes / 2)
	s.maintain()
	after := s.Status()
	must.LessEq(t, st.Hosts.DiskBytes/2, after.Hosts.DiskBytes)
	must.True(t, after.Hosts.Oldest.After(st.Hosts.Oldest))
	must.Eq(t, st.Oldest, after.Oldest)

	// Above the cap of the main store.
	s.main.maxDiskSize.Store(st.DiskBytes / 2)
	s.maintain()
	last := s.Status()
	must.True(t, last.Oldest.After(st.Oldest))
	must.Eq(t, after.Hosts.Oldest, last.Hosts.Oldest)
}
