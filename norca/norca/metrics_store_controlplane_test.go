// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// gaugeValues maps the gauges by metric and label values ("metric/v1/v2",
// the labels in the order of the metric).
func gaugeValues(gauges []metricsstore.Gauge) map[string]float64 {
	out := map[string]float64{}
	for _, g := range gauges {
		key := g.Metric
		names, _ := metricsstore.GaugeLabels(g.Metric)
		for _, name := range names {
			key += "/" + g.Labels[name]
		}
		out[key] = g.Value
	}
	return out
}

// TestMetricsStore_ControlPlaneGauges checks the gauges of the scheduler and
// the control plane from the values a server read: the leader's own values
// on the leader only, the counters as their increase per sample.
func TestMetricsStore_ControlPlaneGauges(t *testing.T) {
	ci.Parallel(t)

	values := controlPlaneValues{
		leader: true,
		broker: &BrokerStats{TotalReady: 3, TotalUnacked: 2, TotalPending: 1, TotalWaiting: 4,
			DelayedEvals: map[string]*structs.Evaluation{"a": {}, "b": {}}},
		planQueueDepth: 5,
		blocked:        &BlockedStats{TotalBlocked: 6, TotalEscaped: 1, TotalQuotaLimit: 2},
		heartbeats:     7,
		leasesActive:   8,
		secretsExpire:  2,
		secretsExpired: 1,
		rotationDue:    3,
		autoscaler:     autoscaler.Stats{Running: true, Queue: 1, Policies: 9, Suspended: 2},

		certificatesIssued: 10,
		admissionDenied:    4,
		webhooks:           map[string][2]uint64{"policy": {20, 3}},
		store:              &metricsstore.Status{DiskBytes: 4096, Series: 120, DroppedReports: 5},
	}
	var counters counterSampler
	got := gaugeValues(controlPlaneGauges(values, &counters))
	must.Eq(t, map[string]float64{
		metricsstore.MetricSchedBrokerReady:                  3,
		metricsstore.MetricSchedBrokerUnacked:                2,
		metricsstore.MetricSchedBrokerPending:                1,
		metricsstore.MetricSchedBrokerWaiting:                4,
		metricsstore.MetricSchedBrokerDelayed:                2,
		metricsstore.MetricSchedPlanQueueDepth:               5,
		metricsstore.MetricSchedBlockedTotal:                 6,
		metricsstore.MetricSchedBlockedEscaped:               1,
		metricsstore.MetricSchedBlockedQuotaLimit:            2,
		metricsstore.MetricSchedHeartbeatsActive:             7,
		metricsstore.MetricSecretsLeasesActive:               8,
		metricsstore.MetricSecretsExpiring:                   2,
		metricsstore.MetricSecretsExpired:                    1,
		metricsstore.MetricSecretsRotationDue:                3,
		metricsstore.MetricAutoscalerQueue:                   1,
		metricsstore.MetricAutoscalerPolicies:                9,
		metricsstore.MetricAutoscalerSuspended:               2,
		metricsstore.MetricSecurityCertificatesIssued:        10,
		metricsstore.MetricSecurityAdmissionDenied:           4,
		metricsstore.MetricSecurityWebhookCalls + "/policy":  20,
		metricsstore.MetricSecurityWebhookErrors + "/policy": 3,
		metricsstore.MetricStoreDiskBytes:                    4096,
		metricsstore.MetricStoreSeries:                       120,
		metricsstore.MetricStoreDroppedReports:               5,
	}, got)

	// The next sample has the increase of the counters since the last one.
	values.certificatesIssued = 13
	values.webhooks = map[string][2]uint64{"policy": {21, 3}}
	values.store = &metricsstore.Status{DiskBytes: 8192, Series: 122, DroppedReports: 5}
	got = gaugeValues(controlPlaneGauges(values, &counters))
	must.Eq(t, 3.0, got[metricsstore.MetricSecurityCertificatesIssued])
	must.Eq(t, 0.0, got[metricsstore.MetricSecurityAdmissionDenied])
	must.Eq(t, 1.0, got[metricsstore.MetricSecurityWebhookCalls+"/policy"])
	must.Eq(t, 0.0, got[metricsstore.MetricSecurityWebhookErrors+"/policy"])
	must.Eq(t, 0.0, got[metricsstore.MetricStoreDroppedReports])
	must.Eq(t, 8192.0, got[metricsstore.MetricStoreDiskBytes])

	// A follower has its counters and its store, nothing of the leader; a
	// stopped autoscaler has no series.
	values.leader = false
	got = gaugeValues(controlPlaneGauges(values, &counters))
	for metric := range got {
		must.False(t, strings.HasPrefix(metric, "norca.sched.") || strings.HasPrefix(metric, "norca.secrets.") ||
			strings.HasPrefix(metric, "norca.autoscaler."), must.Sprint(metric))
	}
	must.MapContainsKey(t, got, metricsstore.MetricSecurityCertificatesIssued)
	must.MapContainsKey(t, got, metricsstore.MetricStoreSeries)
	values.leader = true
	values.autoscaler = autoscaler.Stats{}
	got = gaugeValues(controlPlaneGauges(values, &counters))
	must.MapNotContainsKey(t, got, metricsstore.MetricAutoscalerPolicies)
	must.MapContainsKey(t, got, metricsstore.MetricSchedBrokerReady)

	// A counter that went back (created again) counts from zero.
	must.Eq(t, 2.0, counters.increase("c", 2))
	must.Eq(t, 1.0, counters.increase("c", 1))
}

// TestMetricsStore_JobAndBackupGauges checks the jobs of every namespace by
// status and the gauges of the newest usable backup.
func TestMetricsStore_JobAndBackupGauges(t *testing.T) {
	ci.Parallel(t)
	st := state.TestStateStore(t)

	must.NoError(t, st.UpsertNamespaces(900, []*structs.Namespace{{Name: "team"}, {Name: "empty"}}))
	// The state sets the status: a periodic job runs until it is stopped.
	for i, stop := range []bool{false, false, true} {
		job := mock.PeriodicJob()
		job.Namespace = "team"
		job.Stop = stop
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, uint64(1000+i), nil, job))
	}

	snap, err := st.Snapshot()
	must.NoError(t, err)
	gauges, err := jobStatusGauges(snap)
	must.NoError(t, err)
	got := gaugeValues(gauges)
	must.Eq(t, 2.0, got[metricsstore.MetricJobsRunning+"/team"])
	must.Eq(t, 1.0, got[metricsstore.MetricJobsDead+"/team"])
	must.Eq(t, 0.0, got[metricsstore.MetricJobsPending+"/team"])
	must.MapContainsKey(t, got, metricsstore.MetricJobsRunning+"/empty")
	must.Eq(t, 0.0, got[metricsstore.MetricJobsRunning+"/empty"])

	// No usable backup: no series.
	now := time.Now()
	gauges, err = backupGauges(snap, now)
	must.NoError(t, err)
	must.SliceEmpty(t, gauges)

	verified := func(name string, size int64, took time.Duration) *structs.BackupDestinationStatus {
		return &structs.BackupDestinationStatus{Name: name, Status: structs.BackupDestinationStatusVerified, Duration: took,
			Objects: map[string]*structs.BackupObject{structs.BackupObjectState: {Size: size}}}
	}
	must.NoError(t, st.UpsertBackups(structs.MsgTypeTestSetup, 1200, []*structs.BackupRecord{
		{ID: "old", Status: structs.BackupStatusComplete, CompletedAt: now.Add(-2 * time.Hour),
			Destinations: []*structs.BackupDestinationStatus{verified("s3", 100, time.Second)}},
		{ID: "new", Status: structs.BackupStatusPartial, CompletedAt: now.Add(-time.Hour),
			Destinations: []*structs.BackupDestinationStatus{
				verified("s3", 2048, 3*time.Second),
				{Name: "local", Status: structs.BackupDestinationStatusFailed},
			}},
		{ID: "failed", Status: structs.BackupStatusFailed, CompletedAt: now},
	}))
	snap, err = st.Snapshot()
	must.NoError(t, err)
	gauges, err = backupGauges(snap, now)
	must.NoError(t, err)
	must.Eq(t, map[string]float64{
		metricsstore.MetricBackupLastSuccessAge:   3600,
		metricsstore.MetricBackupSize + "/s3":     2048,
		metricsstore.MetricBackupDuration + "/s3": 3,
	}, gaugeValues(gauges))
}

// TestMetricsStore_ObjectGauges checks the object counts by kind, the node
// groups and the load balancers.
func TestMetricsStore_ObjectGauges(t *testing.T) {
	ci.Parallel(t)
	st := state.TestStateStore(t)

	for i, kind := range []*structs.ObjectKind{
		{Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Schema: `{"type":"object"}`},
		{Group: "norca.io", Kind: "LoadBalancer", Schema: `{"type":"object"}`, Namespaced: true},
		{Group: "db.example.com", Kind: "Cluster", Schema: `{"type":"object"}`, Namespaced: true},
	} {
		must.NoError(t, st.UpsertObjectKind(structs.MsgTypeTestSetup, uint64(1000+i), kind))
	}
	conditions := func(pairs ...string) []any {
		var out []any
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, map[string]any{"type": pairs[i], "status": pairs[i+1]})
		}
		return out
	}
	for i, obj := range []*structs.CustomObject{
		{Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: "workers",
			Spec: map[string]any{"replicas": float64(4)},
			Status: map[string]any{"replicas": float64(3), "joining": int64(1), "failed": 0,
				"rollout":    map[string]any{"replacing": []any{map[string]any{"node": "n1"}}, "outdated": []any{"n1", "n2"}},
				"conditions": conditions("RollBlocked", "True", "ProviderAvailable", "False")}},
		{Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: "new"},
		{Group: "norca.io", Kind: "LoadBalancer", Namespace: "default", Name: "web",
			Status: map[string]any{"targets": []any{"10.0.0.1:80", "10.0.0.2:80"}, "conditions": conditions("Ready", "True")}},
		{Group: "db.example.com", Kind: "Cluster", Namespace: "default", Name: "a",
			Status: map[string]any{"conditions": conditions("Degraded", "True")}},
		{Group: "db.example.com", Kind: "Cluster", Namespace: "default", Name: "b"},
	} {
		obj.ID = obj.Name
		must.NoError(t, st.UpsertCustomObject(structs.MsgTypeTestSetup, uint64(1100+i), obj, 0))
	}

	snap, err := st.Snapshot()
	must.NoError(t, err)
	gauges, err := objectGauges(snap)
	must.NoError(t, err)
	must.Eq(t, map[string]float64{
		metricsstore.MetricObjectsCount + "/norca.io/NodeGroup":              2,
		metricsstore.MetricObjectsDegraded + "/norca.io/NodeGroup":           0,
		metricsstore.MetricObjectsCount + "/norca.io/LoadBalancer":           1,
		metricsstore.MetricObjectsDegraded + "/norca.io/LoadBalancer":        0,
		metricsstore.MetricObjectsCount + "/db.example.com/Cluster":          2,
		metricsstore.MetricObjectsDegraded + "/db.example.com/Cluster":       1,
		metricsstore.MetricNodeGroupReplicas + "/workers":                    4,
		metricsstore.MetricNodeGroupReady + "/workers":                       3,
		metricsstore.MetricNodeGroupJoining + "/workers":                     1,
		metricsstore.MetricNodeGroupFailed + "/workers":                      0,
		metricsstore.MetricNodeGroupRolloutReplacing + "/workers":            1,
		metricsstore.MetricNodeGroupRolloutOutdated + "/workers":             2,
		metricsstore.MetricNodeGroupCondition + "/workers/RollBlocked":       1,
		metricsstore.MetricNodeGroupCondition + "/workers/ProviderAvailable": 0,
		metricsstore.MetricNodeGroupReady + "/new":                           0,
		metricsstore.MetricNodeGroupJoining + "/new":                         0,
		metricsstore.MetricNodeGroupFailed + "/new":                          0,
		metricsstore.MetricNodeGroupRolloutReplacing + "/new":                0,
		metricsstore.MetricNodeGroupRolloutOutdated + "/new":                 0,
		metricsstore.MetricLBTargets + "/default/web":                        2,
		metricsstore.MetricLBReady + "/default/web":                          1,
	}, gaugeValues(gauges))
}

// TestMetricsStore_ControlPlaneACL checks the ACL of the series of the
// scheduler, the control plane and the objects.
func TestMetricsStore_ControlPlaneACL(t *testing.T) {
	ci.Parallel(t)

	aclFor := func(rules string) *acl.ACL {
		p, err := acl.Parse(rules)
		must.NoError(t, err)
		a, err := acl.NewACL(false, []*acl.Policy{p})
		must.NoError(t, err)
		return a
	}
	operator := aclFor(`operator { policy = "read" }`)
	jobs := aclFor(`namespace "team" { policy = "read" }`)
	backups := aclFor(`backup { policy = "read" }`)
	objects := aclFor(`
namespace "*" { object "norca.io/NodeGroup" { policy = "read" } }
namespace "web" { object "norca.io/LoadBalancer" { policy = "read" } }`)

	for _, tc := range []struct {
		metric string
		labels map[string]string
		allow  []*acl.ACL
	}{
		{metricsstore.MetricSchedBrokerReady, nil, []*acl.ACL{operator}},
		{metricsstore.MetricSecretsExpired, nil, []*acl.ACL{operator}},
		{metricsstore.MetricSecurityAdmissionDenied, nil, []*acl.ACL{operator}},
		{metricsstore.MetricAutoscalerQueue, nil, []*acl.ACL{operator}},
		{metricsstore.MetricStoreSeries, nil, []*acl.ACL{operator}},
		{metricsstore.MetricServerRaftDBBytes, nil, []*acl.ACL{operator}},
		{metricsstore.MetricServerRaftCommitLag, nil, []*acl.ACL{operator}},
		{metricsstore.MetricBusJetStreamBytes, nil, []*acl.ACL{operator}},
		{metricsstore.MetricBusRoutes, nil, []*acl.ACL{operator}},
		{metricsstore.MetricObjectsCount, map[string]string{"kind": "norca.io/NodeGroup"}, []*acl.ACL{operator}},
		{metricsstore.MetricJobsRunning, map[string]string{"ns": "team"}, []*acl.ACL{jobs}},
		{metricsstore.MetricJobsRunning, map[string]string{"ns": "other"}, nil},
		{metricsstore.MetricBackupSize, map[string]string{"destination": "s3"}, []*acl.ACL{backups}},
		{metricsstore.MetricNodeGroupCondition, map[string]string{"group": "g", "type": "RollBlocked"}, []*acl.ACL{objects}},
		{metricsstore.MetricLBTargets, map[string]string{"ns": "web", "name": "lb"}, []*acl.ACL{objects}},
		{metricsstore.MetricLBTargets, map[string]string{"ns": "other", "name": "lb"}, nil},
	} {
		for _, a := range []*acl.ACL{operator, jobs, backups, objects} {
			want := false
			for _, b := range tc.allow {
				want = want || a == b
			}
			must.Eq(t, want, allowGauge(a, tc.metric, tc.labels), must.Sprintf("%s %v", tc.metric, tc.labels))
		}
		must.True(t, allowGauge(nil, tc.metric, tc.labels))
		must.True(t, allowGauge(acl.ManagementACL, tc.metric, tc.labels))
	}
}

// TestMetricsStore_SampleControlPlane checks that the leader samples the
// scheduler, its counters and its store with the other gauges.
func TestMetricsStore_SampleControlPlane(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	s.metricsCounters.certificatesIssued.Add(1)
	gauges, err := s.sampleGauges(time.Now())
	must.NoError(t, err)
	got := gaugeValues(gauges)
	must.MapContainsKey(t, got, metricsstore.MetricSchedBrokerReady)
	must.MapContainsKey(t, got, metricsstore.MetricSchedPlanQueueDepth)
	must.MapContainsKey(t, got, metricsstore.MetricSchedHeartbeatsActive)
	must.MapContainsKey(t, got, metricsstore.MetricStoreSeries)
	must.MapContainsKey(t, got, metricsstore.MetricJobsRunning+"/default")
	must.MapContainsKey(t, got, metricsstore.MetricSecurityCertificatesIssued)
}

// TestServer_Reload_MetricsStore checks that a reload changes the retention
// and the disk cap of the metrics store without a restart, and keeps the
// partition duration.
func TestServer_Reload_MetricsStore(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	must.Eq(t, 2*time.Hour, s.metricsStore.Status().Retention)

	cfg := s.GetConfig()
	cfg.MetricsStore = cfg.MetricsStore.Copy()
	cfg.MetricsStore.Retention = 7 * 24 * time.Hour
	cfg.MetricsStore.MaxDiskSize = 64 << 20
	cfg.MetricsStore.PartitionDuration = 2 * time.Hour // needs a restart
	must.NoError(t, s.Reload(cfg))
	st := s.metricsStore.Status()
	must.Eq(t, 7*24*time.Hour, st.Retention)
	must.Eq(t, int64(64<<20), st.MaxDiskSize)

	// An invalid retention is refused and changes nothing.
	cfg.MetricsStore = cfg.MetricsStore.Copy()
	cfg.MetricsStore.Retention = 31 * 24 * time.Hour
	must.ErrorContains(t, s.Reload(cfg), "retention")
	must.Eq(t, 7*24*time.Hour, s.metricsStore.Status().Retention)

	// Turning the store off needs a restart.
	cfg.MetricsStore = cfg.MetricsStore.Copy()
	cfg.MetricsStore.Retention = time.Hour
	cfg.MetricsStore.PartitionDuration = time.Hour
	cfg.MetricsStore.Enabled = false
	must.NoError(t, s.Reload(cfg))
	must.Eq(t, 7*24*time.Hour, s.metricsStore.Status().Retention)
}
