// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/internal/tstorage"
)

// The metrics the servers sample from their state and from the live usage
// of the nodes, apart from the task groups. Each is stored as <name>.avg and
// <name>.max with the same value, the last sampled in the interval.
const (
	// The capacity of the ready nodes of a datacenter and node pool
	// (labels dc, pool, class), in percent of the capacity left after the
	// reserved resources: allocated by the running allocations, and actually
	// used as the clients report it.
	//
	// The label class is the node class. The series with class "" covers all
	// the nodes of the datacenter and pool, whatever their class, and is what
	// a query without a class gets; for every node class that is set there is
	// a series of its own with the nodes of that class only.
	MetricClusterCPUAllocated    = "norca.cluster.cpu_allocated"
	MetricClusterCPUUsed         = "norca.cluster.cpu_used"
	MetricClusterMemoryAllocated = "norca.cluster.memory_allocated"
	MetricClusterMemoryUsed      = "norca.cluster.memory_used"
	MetricClusterDiskAllocated   = "norca.cluster.disk_allocated"
	// MetricClusterNodesReady is the number of ready, eligible nodes of a
	// datacenter and node pool (and, with a class, of that node class).
	MetricClusterNodesReady = "norca.cluster.nodes_ready"

	// The demand that cannot be placed for want of capacity (labels dc,
	// pool), from the blocked evaluations of the leader: the CPU (MHz) and
	// memory (MiB) of the unplaced allocations, the number of blocked
	// evaluations, and the unplaced allocations that need a node of their
	// own (distinct_hosts). The demand of a job that can use several
	// datacenters is counted in each of them.
	MetricClusterBlockedCPU             = "norca.cluster.blocked_cpu_mhz"
	MetricClusterBlockedMemory          = "norca.cluster.blocked_memory_mb"
	MetricClusterBlockedEvals           = "norca.cluster.blocked_evals"
	MetricClusterBlockedAllocsExclusive = "norca.cluster.blocked_allocs_exclusive"
	// MetricClusterBlockedUnverifiable counts the unplaced allocations whose
	// constraint cannot be checked ahead of time against a new node (a
	// distinct_property on a property other than the datacenter or the
	// pool): the work waits.
	MetricClusterBlockedUnverifiable = "norca.cluster.blocked_unverifiable"

	// MetricAutopilotHealthy (1 healthy, 0 not) and
	// MetricAutopilotFailureTolerance are the health of the servers; only
	// the leader has them.
	MetricAutopilotHealthy          = "norca.cluster.autopilot_healthy"
	MetricAutopilotFailureTolerance = "norca.cluster.autopilot_failure_tolerance"

	// The resources of the namespaces of a quota (label quota) in the
	// region: allocated to their allocations (MHz, MiB), and actually used
	// as the clients report it.
	MetricQuotaCPUAllocated    = "norca.quota.cpu_mhz_allocated"
	MetricQuotaCPUUsed         = "norca.quota.cpu_mhz_used"
	MetricQuotaMemoryAllocated = "norca.quota.memory_mb_allocated"
	MetricQuotaMemoryUsed      = "norca.quota.memory_mb_used"
)

// The scheduler and the control plane (family 5). The values of the eval
// broker, the plan queue, the blocked evaluations, the heartbeats, the
// secrets watchers and the autoscaler exist only on the leader, which alone
// samples them. The jobs and the backups are read from the state by every
// server; the security counters and the store's own values are those of the
// server that samples them.
const (
	// The evaluations of the eval broker: ready for a scheduler, handed to
	// one and not acknowledged yet, pending behind another evaluation of the
	// same job, waiting for their wait time, and delayed until a time
	// (reschedules).
	MetricSchedBrokerReady   = "norca.sched.broker_ready"
	MetricSchedBrokerUnacked = "norca.sched.broker_unacked"
	MetricSchedBrokerPending = "norca.sched.broker_pending"
	MetricSchedBrokerWaiting = "norca.sched.broker_waiting"
	MetricSchedBrokerDelayed = "norca.sched.broker_delayed"
	// MetricSchedPlanQueueDepth is the number of plans waiting to be
	// applied.
	MetricSchedPlanQueueDepth = "norca.sched.plan_queue_depth"
	// The blocked evaluations: all of them, those that escaped the node
	// classes, and those blocked by a quota.
	MetricSchedBlockedTotal      = "norca.sched.blocked_total"
	MetricSchedBlockedEscaped    = "norca.sched.blocked_escaped"
	MetricSchedBlockedQuotaLimit = "norca.sched.blocked_quota_limit"
	// MetricSchedHeartbeatsActive is the number of nodes whose heartbeat
	// the leader tracks.
	MetricSchedHeartbeatsActive = "norca.sched.heartbeats_active"

	// The jobs of a namespace (label ns) by status.
	MetricJobsPending = "norca.jobs.pending"
	MetricJobsRunning = "norca.jobs.running"
	MetricJobsDead    = "norca.jobs.dead"

	// The secret store: the active leases, the secrets about to expire and
	// expired, and those whose rotation is due.
	MetricSecretsLeasesActive = "norca.secrets.leases_active"
	MetricSecretsExpiring     = "norca.secrets.expiring"
	MetricSecretsExpired      = "norca.secrets.expired"
	MetricSecretsRotationDue  = "norca.secrets.rotation_due"

	// The backups: the age of the newest usable backup in seconds (no
	// series before the first one), and the size of its state object in
	// bytes and its upload time in seconds on each destination (label
	// destination).
	MetricBackupLastSuccessAge = "norca.backup.last_success_age_seconds"
	MetricBackupSize           = "norca.backup.size_bytes"
	MetricBackupDuration       = "norca.backup.duration_seconds"

	// Security events, as the increase in the sample interval on the
	// sampling server: workload certificates issued by the mTLS
	// certificate authority, jobs and variables denied by admission
	// control, and the calls and failed calls of each admission webhook
	// (label webhook).
	MetricSecurityCertificatesIssued = "norca.security.certificates_issued"
	MetricSecurityAdmissionDenied    = "norca.security.admission_denied"
	MetricSecurityWebhookCalls       = "norca.security.webhook_calls"
	MetricSecurityWebhookErrors      = "norca.security.webhook_errors"

	// The built-in autoscaler: the policies waiting for an evaluation, the
	// policies, and the targets suspended for oscillating.
	MetricAutoscalerQueue     = "norca.autoscaler.queue"
	MetricAutoscalerPolicies  = "norca.autoscaler.policies"
	MetricAutoscalerSuspended = "norca.autoscaler.oscillation_suspended"

	// The metrics store of the sampling server: its disk use in bytes, its
	// series, and the reports it dropped in the interval.
	MetricStoreDiskBytes      = "norca.store.disk_bytes"
	MetricStoreSeries         = "norca.store.series"
	MetricStoreDroppedReports = "norca.store.dropped_reports"
)

// The custom objects and their operators (family 9), read from the state by
// every server.
const (
	// A node group (label group, the name of the NodeGroup object): the
	// replicas wanted (spec.replicas), ready (status.replicas), joining and
	// failed, the nodes being replaced and outdated in a rollout.
	MetricNodeGroupReplicas         = "norca.nodegroup.replicas"
	MetricNodeGroupReady            = "norca.nodegroup.ready"
	MetricNodeGroupJoining          = "norca.nodegroup.joining"
	MetricNodeGroupFailed           = "norca.nodegroup.failed"
	MetricNodeGroupRolloutReplacing = "norca.nodegroup.rollout_replacing"
	MetricNodeGroupRolloutOutdated  = "norca.nodegroup.rollout_outdated"
	// MetricNodeGroupCondition is 1 while a condition of the status of a
	// node group (labels group, type) is True, 0 otherwise.
	MetricNodeGroupCondition = "norca.nodegroup.condition"

	// A load balancer (labels ns, name): the healthy targets, and 1 while
	// its Ready condition is True.
	MetricLBTargets = "norca.lb.targets"
	MetricLBReady   = "norca.lb.ready"

	// The custom objects of a kind (label kind, "group/Kind"), and those
	// whose Degraded condition is True.
	MetricObjectsCount    = "norca.objects.count"
	MetricObjectsDegraded = "norca.objects.degraded"
)

// gaugeLabels are the labels of the gauge metrics, by metric.
var gaugeLabels = map[string][]string{
	MetricClusterCPUAllocated:    {"dc", "pool", "class"},
	MetricClusterCPUUsed:         {"dc", "pool", "class"},
	MetricClusterMemoryAllocated: {"dc", "pool", "class"},
	MetricClusterMemoryUsed:      {"dc", "pool", "class"},
	MetricClusterDiskAllocated:   {"dc", "pool", "class"},
	MetricClusterNodesReady:      {"dc", "pool", "class"},
	MetricClusterBlockedCPU:      {"dc", "pool"},
	MetricClusterBlockedMemory:   {"dc", "pool"},
	MetricClusterBlockedEvals:    {"dc", "pool"},

	MetricClusterBlockedAllocsExclusive: {"dc", "pool"},
	MetricClusterBlockedUnverifiable:    {"dc", "pool"},
	MetricAutopilotHealthy:              {},
	MetricAutopilotFailureTolerance:     {},
	MetricQuotaCPUAllocated:             {"quota"},
	MetricQuotaCPUUsed:                  {"quota"},
	MetricQuotaMemoryAllocated:          {"quota"},
	MetricQuotaMemoryUsed:               {"quota"},

	MetricSchedBrokerReady:           {},
	MetricSchedBrokerUnacked:         {},
	MetricSchedBrokerPending:         {},
	MetricSchedBrokerWaiting:         {},
	MetricSchedBrokerDelayed:         {},
	MetricSchedPlanQueueDepth:        {},
	MetricSchedBlockedTotal:          {},
	MetricSchedBlockedEscaped:        {},
	MetricSchedBlockedQuotaLimit:     {},
	MetricSchedHeartbeatsActive:      {},
	MetricJobsPending:                {"ns"},
	MetricJobsRunning:                {"ns"},
	MetricJobsDead:                   {"ns"},
	MetricSecretsLeasesActive:        {},
	MetricSecretsExpiring:            {},
	MetricSecretsExpired:             {},
	MetricSecretsRotationDue:         {},
	MetricBackupLastSuccessAge:       {},
	MetricBackupSize:                 {"destination"},
	MetricBackupDuration:             {"destination"},
	MetricSecurityCertificatesIssued: {},
	MetricSecurityAdmissionDenied:    {},
	MetricSecurityWebhookCalls:       {"webhook"},
	MetricSecurityWebhookErrors:      {"webhook"},
	MetricAutoscalerQueue:            {},
	MetricAutoscalerPolicies:         {},
	MetricAutoscalerSuspended:        {},
	MetricStoreDiskBytes:             {},
	MetricStoreSeries:                {},
	MetricStoreDroppedReports:        {},

	MetricNodeGroupReplicas:         {"group"},
	MetricNodeGroupReady:            {"group"},
	MetricNodeGroupJoining:          {"group"},
	MetricNodeGroupFailed:           {"group"},
	MetricNodeGroupRolloutReplacing: {"group"},
	MetricNodeGroupRolloutOutdated:  {"group"},
	MetricNodeGroupCondition:        {"group", "type"},
	MetricLBTargets:                 {"ns", "name"},
	MetricLBReady:                   {"ns", "name"},
	MetricObjectsCount:              {"kind"},
	MetricObjectsDegraded:           {"kind"},
}

// GaugeMetrics are the gauge metrics, sorted.
var GaugeMetrics = slices.Sorted(maps.Keys(gaugeLabels))

// GaugeLabels returns the labels of a gauge metric, false if it is none.
func GaugeLabels(metric string) ([]string, bool) {
	labels, ok := gaugeLabels[metric]
	return labels, ok
}

// Gauge is one sampled value of a gauge metric.
type Gauge struct {
	Metric string
	Labels map[string]string
	Value  float64
}

// GaugeSeries identifies a series of a gauge metric.
type GaugeSeries struct {
	Metric string
	Labels map[string]string
}

// key is the identity of a gauge series: the metric and its labels in the
// order of gaugeLabels.
func gaugeKey(metric string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(metric)
	for _, name := range gaugeLabels[metric] {
		b.WriteString("\x00")
		b.WriteString(labels[name])
	}
	return b.String()
}

// gaugeTSLabels are the tstorage labels of a gauge series.
func gaugeTSLabels(metric string, labels map[string]string) []tstorage.Label {
	out := make([]tstorage.Label, 0, len(gaugeLabels[metric]))
	for _, name := range gaugeLabels[metric] {
		out = append(out, tstorage.Label{Name: name, Value: labels[name]})
	}
	return out
}

// RecordGauges stores values sampled by the server at the time at: the last
// value of an interval counts. Unknown metrics are ignored; new series
// beyond max_series are left out.
func (s *Store) RecordGauges(at time.Time, gauges []Gauge) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval

	s.mu.Lock()
	defer s.mu.Unlock()
	if start+interval <= s.written {
		return
	}
	s.newest = max(s.newest, ts)
	pending := s.gaugePending[start]
	if pending == nil {
		pending = map[string]float64{}
		s.gaugePending[start] = pending
	}
	for _, g := range gauges {
		if _, ok := gaugeLabels[g.Metric]; !ok {
			continue
		}
		key := gaugeKey(g.Metric, g.Labels)
		if _, ok := s.gauges[key]; !ok {
			if s.cfg.MaxSeries > 0 && s.seriesCountLocked()+2 > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			labels := map[string]string{}
			for _, name := range gaugeLabels[g.Metric] {
				labels[name] = g.Labels[name]
			}
			s.gauges[key] = GaugeSeries{Metric: g.Metric, Labels: labels}
			s.seriesDirty = true
		}
		pending[key] = g.Value
	}
}

// gaugeRowsLocked returns the rows of the gauge windows that ended by limit
// and drops them; s.mu is held.
func (s *Store) gaugeRowsLocked(limit, interval int64) []tstorage.Row {
	var rows []tstorage.Row
	for start, values := range s.gaugePending {
		if start+interval > limit {
			continue
		}
		for key, v := range values {
			series, ok := s.gauges[key]
			if !ok {
				continue
			}
			labels := gaugeTSLabels(series.Metric, series.Labels)
			rows = append(rows,
				tstorage.Row{Metric: series.Metric + ".avg", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v}},
				tstorage.Row{Metric: series.Metric + ".max", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v}})
		}
		delete(s.gaugePending, start)
	}
	return rows
}

// Gauges returns the gauge series, sorted by metric and labels.
func (s *Store) Gauges() []GaugeSeries {
	s.mu.Lock()
	keys := slices.Sorted(maps.Keys(s.gauges))
	out := make([]GaugeSeries, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.gauges[k])
	}
	s.mu.Unlock()
	return out
}

// QueryGauge returns the points of a gauge series like Query.
func (s *Store) QueryGauge(metric string, labels map[string]string, from, to time.Time, maxPoints int) ([]Point, time.Duration, error) {
	if _, ok := gaugeLabels[metric]; !ok {
		return nil, 0, errUnknownMetric(metric)
	}
	return s.query(metric, gaugeTSLabels(metric, labels), false, from, to, maxPoints)
}
