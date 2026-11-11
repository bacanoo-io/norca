// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/internal/tstorage"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The metrics the servers sample from their state and from the live usage
// of the nodes, apart from the task groups. Each is stored as <name>.avg and
// <name>.max with the same value, the last sampled in the interval; the
// node metrics (norca.node.*) come from the reports of the clients and have
// the mean and the largest measurement of the interval.
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

	// The nodes of a datacenter and node pool (labels dc, pool) that are
	// down, disconnected, ready and being drained, and ready but not
	// eligible for scheduling (draining ones included): beside nodes_ready,
	// ready + ineligible are the ready nodes.
	MetricNodesDown         = "norca.nodes.down"
	MetricNodesDisconnected = "norca.nodes.disconnected"
	MetricNodesDraining     = "norca.nodes.draining"
	MetricNodesIneligible   = "norca.nodes.ineligible"

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

	// The voters of the Raft configuration, the standby servers ready for a
	// promotion (healthy non-voters with the role standby), and the standby
	// promotions and demotions seen by the leader since it became the
	// leader; only the leader has them.
	MetricAutopilotVoters            = "norca.cluster.autopilot_voters"
	MetricAutopilotStandbysReady     = "norca.cluster.autopilot_standbys_ready"
	MetricAutopilotStandbyPromotions = "norca.cluster.autopilot_standby_promotions"
	MetricAutopilotStandbyDemotions  = "norca.cluster.autopilot_standby_demotions"

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

	// The metrics store of the sampling server: its disk use in bytes (of
	// the main and the hosts store together), its series, and the reports it
	// dropped in the interval.
	MetricStoreDiskBytes      = "norca.store.disk_bytes"
	MetricStoreSeries         = "norca.store.series"
	MetricStoreDroppedReports = "norca.store.dropped_reports"
)

// The internals of the sampling server (family 8). Every server samples its
// own values into its own store, so the series carry no server label: the
// history of a server is read from that server. Only the AppendEntries time
// has a label, the follower it was sent to.
const (
	// MetricServerRaftDBBytes is the space the Raft log store takes on disk
	// in bytes: the files of the Badger database, or the file of BoltDB
	// (raft_store).
	MetricServerRaftDBBytes = "norca.server.raft_db_bytes"
	// MetricServerRaftLastContact is the time since the server last heard
	// from the leader in seconds; 0 on the leader, no point on a server
	// that never heard from one.
	MetricServerRaftLastContact = "norca.server.raft_last_contact_seconds"
	// MetricServerRaftCommitLag is the commit index less the applied index:
	// the committed entries the server has not applied yet.
	MetricServerRaftCommitLag = "norca.server.raft_commit_lag"
	// MetricServerRaftFSMPending is the number of batches of entries waiting
	// for the state machine.
	MetricServerRaftFSMPending = "norca.server.raft_fsm_pending"
	// MetricServerRaftTerm is the current Raft term; a rise is an election.
	MetricServerRaftTerm = "norca.server.raft_term"
	// MetricServerRaftCommit is the mean time in seconds from the moment the
	// leader appends an entry until a quorum stores it (committed), over the
	// sample interval; only the leader has it, and no point in an interval
	// without a commit.
	MetricServerRaftCommit = "norca.server.raft_commit_seconds"
	// MetricServerRaftAppendEntries is the mean time in seconds of the
	// AppendEntries requests of the leader to a follower (label peer, the
	// Raft server ID of the follower) over the sample interval; only the
	// leader has it.
	MetricServerRaftAppendEntries = "norca.server.raft_append_entries_seconds"
	// The RPC requests the server handled in the sample interval, from the
	// network and in process, and those answered with an error or that
	// failed on the connection.
	MetricServerRPCRequests = "norca.server.rpc_requests"
	MetricServerRPCErrors   = "norca.server.rpc_errors"

	// The system bus of the server: the bytes and messages of the JetStream
	// streams it stores a replica of, and the other servers of the region
	// it has a route to.
	MetricBusJetStreamBytes    = "norca.bus.jetstream_bytes"
	MetricBusJetStreamMessages = "norca.bus.jetstream_messages"
	MetricBusRoutes            = "norca.bus.routes"
)

// The host statistics of a client node (family 7; labels node, dc, pool,
// class), from the usage reports of the clients: the mean of the
// measurements of the sample interval and the largest one. The node label is
// the node ID; dc, pool and class are those of the node in the state of the
// server, not what the report says.
const (
	// MetricNodeCPUUsed is the CPU time in user and system mode in percent
	// of the CPU of the node, MetricNodeCPUMHz the same in MHz (only when
	// the client knows the CPU of the node).
	MetricNodeCPUUsed = "norca.node.cpu_used"
	MetricNodeCPUMHz  = "norca.node.cpu_mhz"
	// The memory in use and of the node in MiB.
	MetricNodeMemoryUsedMB  = "norca.node.memory_used_mb"
	MetricNodeMemoryTotalMB = "norca.node.memory_total_mb"
	// The space in use and the size in MiB, and the inodes in use in
	// percent, of the file system of the allocation directory.
	MetricNodeDiskUsedMB        = "norca.node.disk_used_mb"
	MetricNodeDiskTotalMB       = "norca.node.disk_total_mb"
	MetricNodeInodesUsedPercent = "norca.node.inodes_used_percent"
)

// HostMetric reports whether a metric (with or without the .avg or .max of
// its series) is a host metric, kept in the hosts store with its own
// retention (host_retention) and disk cap (host_max_disk_size): the host
// statistics of the client nodes (norca.node.*), and the internals of the
// server (norca.server.*) and of its system bus (norca.bus.*). Every other
// metric is kept in the main store.
func HostMetric(name string) bool {
	return strings.HasPrefix(name, "norca.node.") ||
		strings.HasPrefix(name, "norca.server.") ||
		strings.HasPrefix(name, "norca.bus.")
}

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
	// MetricNodeGroupCertExpiresMinHours is the time in hours until the
	// earliest certificate expiry among the nodes of a node group (label
	// group) that are not down, from the node meta norca.cert_expires; 0 once
	// a certificate has expired. No point for a group without such nodes.
	MetricNodeGroupCertExpiresMinHours = "norca.nodegroup.cert_expires_min_hours"

	// A load balancer (labels ns, name): the healthy targets, and 1 while
	// its Ready condition is True.
	MetricLBTargets = "norca.lb.targets"
	MetricLBReady   = "norca.lb.ready"

	// The custom objects of a kind (label kind, "group/Kind"), and those
	// whose Degraded condition is True.
	MetricObjectsCount    = "norca.objects.count"
	MetricObjectsDegraded = "norca.objects.degraded"

	// A CSI volume on a node (labels ns, volume, plugin, node), as its node
	// plugin reports it to NodeGetVolumeStats: bytes used, available and in
	// total, inodes used and free, and 1 while the plugin reports the volume
	// abnormal (plugins with the VOLUME_CONDITION capability).
	MetricVolumeUsedBytes      = "norca.volume.used_bytes"
	MetricVolumeAvailableBytes = "norca.volume.available_bytes"
	MetricVolumeCapacityBytes  = "norca.volume.capacity_bytes"
	MetricVolumeInodesUsed     = "norca.volume.inodes_used"
	MetricVolumeInodesFree     = "norca.volume.inodes_free"
	MetricVolumeAbnormal       = "norca.volume.abnormal"

	// The capacity of the storage of a CSI plugin (labels plugin, topology:
	// "" for the whole storage, else the sorted key=value segments), as a
	// controller with GET_CAPACITY reports it to the leader: bytes available,
	// and the largest and the smallest volume size when the plugin reports
	// them.
	MetricCSICapacityAvailable = "norca.csi.capacity_available_bytes"
	MetricCSIMaximumVolumeSize = "norca.csi.maximum_volume_size_bytes"
	MetricCSIMinimumVolumeSize = "norca.csi.minimum_volume_size_bytes"
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
	MetricAutopilotVoters:               {},
	MetricAutopilotStandbysReady:        {},
	MetricAutopilotStandbyPromotions:    {},
	MetricAutopilotStandbyDemotions:     {},
	MetricQuotaCPUAllocated:             {"quota"},
	MetricQuotaCPUUsed:                  {"quota"},
	MetricQuotaMemoryAllocated:          {"quota"},
	MetricQuotaMemoryUsed:               {"quota"},
	MetricNodesDown:                     {"dc", "pool"},
	MetricNodesDisconnected:             {"dc", "pool"},
	MetricNodesDraining:                 {"dc", "pool"},
	MetricNodesIneligible:               {"dc", "pool"},

	MetricNodeCPUUsed:           {"node", "dc", "pool", "class"},
	MetricNodeCPUMHz:            {"node", "dc", "pool", "class"},
	MetricNodeMemoryUsedMB:      {"node", "dc", "pool", "class"},
	MetricNodeMemoryTotalMB:     {"node", "dc", "pool", "class"},
	MetricNodeDiskUsedMB:        {"node", "dc", "pool", "class"},
	MetricNodeDiskTotalMB:       {"node", "dc", "pool", "class"},
	MetricNodeInodesUsedPercent: {"node", "dc", "pool", "class"},

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

	MetricServerRaftDBBytes:       {},
	MetricServerRaftLastContact:   {},
	MetricServerRaftCommitLag:     {},
	MetricServerRaftFSMPending:    {},
	MetricServerRaftTerm:          {},
	MetricServerRaftCommit:        {},
	MetricServerRaftAppendEntries: {"peer"},
	MetricServerRPCRequests:       {},
	MetricServerRPCErrors:         {},
	MetricBusJetStreamBytes:       {},
	MetricBusJetStreamMessages:    {},
	MetricBusRoutes:               {},

	MetricNodeGroupReplicas:            {"group"},
	MetricNodeGroupReady:               {"group"},
	MetricNodeGroupJoining:             {"group"},
	MetricNodeGroupFailed:              {"group"},
	MetricNodeGroupRolloutReplacing:    {"group"},
	MetricNodeGroupRolloutOutdated:     {"group"},
	MetricNodeGroupCondition:           {"group", "type"},
	MetricNodeGroupCertExpiresMinHours: {"group"},
	MetricLBTargets:                    {"ns", "name"},
	MetricLBReady:                      {"ns", "name"},
	MetricObjectsCount:                 {"kind"},
	MetricObjectsDegraded:              {"kind"},

	MetricVolumeUsedBytes:      {"ns", "volume", "plugin", "node"},
	MetricVolumeAvailableBytes: {"ns", "volume", "plugin", "node"},
	MetricVolumeCapacityBytes:  {"ns", "volume", "plugin", "node"},
	MetricVolumeInodesUsed:     {"ns", "volume", "plugin", "node"},
	MetricVolumeInodesFree:     {"ns", "volume", "plugin", "node"},
	MetricVolumeAbnormal:       {"ns", "volume", "plugin", "node"},

	MetricCSICapacityAvailable: {"plugin", "topology"},
	MetricCSIMaximumVolumeSize: {"plugin", "topology"},
	MetricCSIMinimumVolumeSize: {"plugin", "topology"},
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

// gaugeValue is the value of a gauge series in one sample interval: the
// weighted sum and the weight of the average, and the maximum.
type gaugeValue struct {
	sum, weight, max float64
}

// gaugeWindowLocked returns the pending values of the sample interval of ts
// and its start, nil when the interval is written already; s.mu is held.
func (s *Store) gaugeWindowLocked(ts int64) (map[string]gaugeValue, int64) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	start := ts - ts%interval
	if start+interval <= s.written {
		return nil, 0
	}
	s.newest = max(s.newest, ts)
	pending := s.gaugePending[start]
	if pending == nil {
		pending = map[string]gaugeValue{}
		s.gaugePending[start] = pending
	}
	return pending, start
}

// gaugeSeriesLocked returns the key of the series of a gauge metric with the
// labels, adds the series if it is new, and notes a point in the window that
// starts at start; false for an unknown metric or a new series beyond
// max_series. s.mu is held.
func (s *Store) gaugeSeriesLocked(metric string, labels map[string]string, start int64) (string, bool) {
	names, ok := gaugeLabels[metric]
	if !ok {
		return "", false
	}
	key := gaugeKey(metric, labels)
	if _, ok := s.gauges[key]; !ok {
		if s.cfg.MaxSeries > 0 && s.seriesCountLocked()+2 > s.cfg.MaxSeries {
			s.droppedGroups.Add(1)
			return "", false
		}
		own := make(map[string]string, len(names))
		for _, name := range names {
			own[name] = labels[name]
		}
		s.gauges[key] = GaugeSeries{Metric: metric, Labels: own}
		s.seriesDirty = true
	}
	s.gaugeLast[key] = max(s.gaugeLast[key], start)
	return key, true
}

// RecordGauges stores values sampled by the server at the time at: the last
// value of an interval counts. Unknown metrics are ignored; new series
// beyond max_series are left out.
func (s *Store) RecordGauges(at time.Time, gauges []Gauge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, start := s.gaugeWindowLocked(at.Unix())
	if pending == nil {
		return
	}
	for _, g := range gauges {
		if key, ok := s.gaugeSeriesLocked(g.Metric, g.Labels, start); ok {
			pending[key] = gaugeValue{sum: g.Value, weight: 1, max: g.Value}
		}
	}
}

// RecordNodeHost adds the usage of a node from a usage report published at
// the time at to the window of its sample interval: the measurements of the
// reports of an interval are averaged, and the largest one is the maximum.
// labels are those of the node metrics (node, dc, pool, class). A value the
// client does not know (a total of zero) is left out.
func (s *Store) RecordNodeHost(labels map[string]string, host *structs.NodeHostUsage, at time.Time) {
	if host == nil || host.Samples <= 0 || labels["node"] == "" {
		return
	}
	type value struct {
		metric string
		v      structs.TaskGroupUsageValue
	}
	total := func(v float64) structs.TaskGroupUsageValue { return structs.TaskGroupUsageValue{Avg: v, Max: v} }
	values := []value{{MetricNodeCPUUsed, host.CPUPercent}}
	if host.CPUTotalMHz > 0 {
		values = append(values, value{MetricNodeCPUMHz, host.CPUMHz})
	}
	if host.MemoryTotalMB > 0 {
		values = append(values,
			value{MetricNodeMemoryUsedMB, host.MemoryUsedMB},
			value{MetricNodeMemoryTotalMB, total(host.MemoryTotalMB)})
	}
	if host.DiskTotalMB > 0 {
		values = append(values,
			value{MetricNodeDiskUsedMB, host.DiskUsedMB},
			value{MetricNodeDiskTotalMB, total(host.DiskTotalMB)},
			value{MetricNodeInodesUsedPercent, host.InodesUsedPercent})
	}
	ts := at.Unix()
	weight := float64(host.Samples)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Like the task groups: a report more than a partition older than the
	// newest is not stored.
	if ts < s.newest-int64(s.cfg.PartitionDuration/time.Second) {
		return
	}
	pending, start := s.gaugeWindowLocked(ts)
	if pending == nil {
		return
	}
	for _, v := range values {
		key, ok := s.gaugeSeriesLocked(v.metric, labels, start)
		if !ok {
			continue
		}
		cur := pending[key]
		if cur.weight == 0 || v.v.Max > cur.max {
			cur.max = v.v.Max
		}
		cur.sum += v.v.Avg * weight
		cur.weight += weight
		pending[key] = cur
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
			if !ok || v.weight <= 0 {
				continue
			}
			labels := gaugeTSLabels(series.Metric, series.Labels)
			rows = append(rows,
				tstorage.Row{Metric: series.Metric + ".avg", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v.sum / v.weight}},
				tstorage.Row{Metric: series.Metric + ".max", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v.max}})
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
