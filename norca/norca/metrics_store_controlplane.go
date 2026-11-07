// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The scheduler and the control plane (family 5) and the custom objects
// (family 9) in the metrics store; see the metric names in metricsstore.

// loadBalancerKindRef is the kind of the load balancers, which the
// LoadBalancer operator pack installs.
const loadBalancerKindRef = "norca.io/LoadBalancer"

// serverEventCounters count the security events and the RPC requests of this
// server.
type serverEventCounters struct {
	admissionDenied    atomic.Uint64
	certificatesIssued atomic.Uint64

	// rpcRequests are the RPC requests the server handled, from the network
	// and in process; rpcErrors those of them answered with an error or that
	// failed on the connection.
	rpcRequests atomic.Uint64
	rpcErrors   atomic.Uint64
}

// countRPC counts an RPC request this server handled, and its error.
func (c *serverEventCounters) countRPC(failed bool) {
	c.rpcRequests.Add(1)
	if failed {
		c.rpcErrors.Add(1)
	}
}

// secretsGaugeValues are the numbers the secrets watchers of the leader
// found at their last evaluation; zero elsewhere.
type secretsGaugeValues struct {
	leasesActive atomic.Int64
	expiring     atomic.Int64
	expired      atomic.Int64
	rotationDue  atomic.Int64
}

// counterSampler turns counters into their increase since the previous
// sample. The counters start at zero with the server, so the first sample is
// the increase since the start.
type counterSampler struct {
	mu   sync.Mutex
	last map[string]uint64
}

// increase returns how much the counter of key grew since the last call
// and remembers value. A counter that went back (created again) counts from
// zero.
func (c *counterSampler) increase(key string, value uint64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]uint64{}
	}
	prev := c.last[key]
	c.last[key] = value
	if value < prev {
		return float64(value)
	}
	return float64(value - prev)
}

// controlPlaneValues are the values of the scheduler and the control plane
// a server reads for a sample, apart from those in the state.
type controlPlaneValues struct {
	// leader is set on the leader, which alone has the values of the eval
	// broker, the plan queue, the blocked evaluations, the heartbeats, the
	// secrets watchers and the autoscaler.
	leader         bool
	broker         *BrokerStats
	planQueueDepth int
	blocked        *BlockedStats
	heartbeats     int
	leasesActive   int64
	secretsExpire  int64
	secretsExpired int64
	rotationDue    int64
	autoscaler     autoscaler.Stats

	// The counters of this server: the certificates issued, the requests
	// denied by admission control, and the calls and failures of each
	// admission webhook by name.
	certificatesIssued uint64
	admissionDenied    uint64
	webhooks           map[string][2]uint64

	// store is the status of the metrics store of this server.
	store *metricsstore.Status
}

// controlPlaneValues reads the values of the scheduler and the control
// plane of this server.
func (s *Server) controlPlaneValues() controlPlaneValues {
	v := controlPlaneValues{
		leader:             s.IsLeader(),
		certificatesIssued: s.metricsCounters.certificatesIssued.Load(),
		admissionDenied:    s.metricsCounters.admissionDenied.Load(),
	}
	if v.leader {
		if s.evalBroker != nil {
			v.broker = s.evalBroker.Stats()
		}
		if s.planner != nil && s.planQueue != nil {
			v.planQueueDepth = s.planQueue.Stats().Depth
		}
		if s.blockedEvals != nil {
			v.blocked = s.blockedEvals.Stats()
		}
		if s.nodeHeartbeater != nil {
			v.heartbeats = s.activeHeartbeats()
		}
		v.leasesActive = s.secretsGauges.leasesActive.Load()
		v.secretsExpire = s.secretsGauges.expiring.Load()
		v.secretsExpired = s.secretsGauges.expired.Load()
		v.rotationDue = s.secretsGauges.rotationDue.Load()
		if s.autoscaler != nil {
			v.autoscaler = s.autoscaler.Stats()
		}
	}
	if s.admission != nil && len(s.admission.webhooks) > 0 {
		v.webhooks = map[string][2]uint64{}
		for _, w := range s.admission.webhooks {
			calls, failures := w.Counts()
			v.webhooks[w.Name()] = [2]uint64{calls, failures}
		}
	}
	if s.metricsStore != nil {
		v.store = s.metricsStore.Status()
	}
	return v
}

// controlPlaneGauges returns the gauges of the scheduler and the control
// plane from the values a server read; counters turns the counters into
// their increase since the last sample.
func controlPlaneGauges(v controlPlaneValues, counters *counterSampler) []metricsstore.Gauge {
	var gauges []metricsstore.Gauge
	add := func(metric string, value float64) {
		gauges = append(gauges, metricsstore.Gauge{Metric: metric, Value: value})
	}
	if v.leader {
		if b := v.broker; b != nil {
			add(metricsstore.MetricSchedBrokerReady, float64(b.TotalReady))
			add(metricsstore.MetricSchedBrokerUnacked, float64(b.TotalUnacked))
			add(metricsstore.MetricSchedBrokerPending, float64(b.TotalPending))
			add(metricsstore.MetricSchedBrokerWaiting, float64(b.TotalWaiting))
			add(metricsstore.MetricSchedBrokerDelayed, float64(len(b.DelayedEvals)))
		}
		add(metricsstore.MetricSchedPlanQueueDepth, float64(v.planQueueDepth))
		if b := v.blocked; b != nil {
			add(metricsstore.MetricSchedBlockedTotal, float64(b.TotalBlocked))
			add(metricsstore.MetricSchedBlockedEscaped, float64(b.TotalEscaped))
			add(metricsstore.MetricSchedBlockedQuotaLimit, float64(b.TotalQuotaLimit))
		}
		add(metricsstore.MetricSchedHeartbeatsActive, float64(v.heartbeats))
		add(metricsstore.MetricSecretsLeasesActive, float64(v.leasesActive))
		add(metricsstore.MetricSecretsExpiring, float64(v.secretsExpire))
		add(metricsstore.MetricSecretsExpired, float64(v.secretsExpired))
		add(metricsstore.MetricSecretsRotationDue, float64(v.rotationDue))
		if a := v.autoscaler; a.Running {
			add(metricsstore.MetricAutoscalerQueue, float64(a.Queue))
			add(metricsstore.MetricAutoscalerPolicies, float64(a.Policies))
			add(metricsstore.MetricAutoscalerSuspended, float64(a.Suspended))
		}
	}

	add(metricsstore.MetricSecurityCertificatesIssued,
		counters.increase(metricsstore.MetricSecurityCertificatesIssued, v.certificatesIssued))
	add(metricsstore.MetricSecurityAdmissionDenied,
		counters.increase(metricsstore.MetricSecurityAdmissionDenied, v.admissionDenied))
	names := make([]string, 0, len(v.webhooks))
	for name := range v.webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := v.webhooks[name]
		labels := map[string]string{"webhook": name}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricSecurityWebhookCalls, Labels: labels,
				Value: counters.increase(metricsstore.MetricSecurityWebhookCalls+"\x00"+name, c[0])},
			metricsstore.Gauge{Metric: metricsstore.MetricSecurityWebhookErrors, Labels: labels,
				Value: counters.increase(metricsstore.MetricSecurityWebhookErrors+"\x00"+name, c[1])})
	}

	if st := v.store; st != nil {
		add(metricsstore.MetricStoreDiskBytes, float64(st.DiskBytes+st.Hosts.DiskBytes))
		add(metricsstore.MetricStoreSeries, float64(st.Series))
		add(metricsstore.MetricStoreDroppedReports,
			counters.increase(metricsstore.MetricStoreDroppedReports, st.DroppedReports))
	}
	return gauges
}

// jobStatusGauges counts the jobs of every namespace by status; a namespace
// without jobs has zeros.
func jobStatusGauges(snap *state.StateSnapshot) ([]metricsstore.Gauge, error) {
	counts := map[string]*[3]int{}
	nsIter, err := snap.Namespaces(nil)
	if err != nil {
		return nil, err
	}
	for raw := nsIter.Next(); raw != nil; raw = nsIter.Next() {
		counts[raw.(*structs.Namespace).Name] = &[3]int{}
	}
	iter, err := snap.Jobs(nil)
	if err != nil {
		return nil, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		job := raw.(*structs.Job)
		c := counts[job.Namespace]
		if c == nil {
			c = &[3]int{}
			counts[job.Namespace] = c
		}
		switch job.Status {
		case structs.JobStatusPending:
			c[0]++
		case structs.JobStatusRunning:
			c[1]++
		case structs.JobStatusDead:
			c[2]++
		}
	}
	gauges := make([]metricsstore.Gauge, 0, 3*len(counts))
	for ns, c := range counts {
		labels := map[string]string{"ns": ns}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricJobsPending, Labels: labels, Value: float64(c[0])},
			metricsstore.Gauge{Metric: metricsstore.MetricJobsRunning, Labels: labels, Value: float64(c[1])},
			metricsstore.Gauge{Metric: metricsstore.MetricJobsDead, Labels: labels, Value: float64(c[2])})
	}
	return gauges, nil
}

// backupGauges returns the age of the newest usable backup at now, and the
// size of its state object and its upload time on each destination that
// verified it. Without a usable backup there is none.
func backupGauges(snap *state.StateSnapshot, now time.Time) ([]metricsstore.Gauge, error) {
	iter, err := snap.Backups(nil, state.SortDefault)
	if err != nil {
		return nil, err
	}
	var newest *structs.BackupRecord
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		b := raw.(*structs.BackupRecord)
		if b.Usable() && (newest == nil || b.CompletedAt.After(newest.CompletedAt)) {
			newest = b
		}
	}
	if newest == nil {
		return nil, nil
	}
	gauges := []metricsstore.Gauge{{
		Metric: metricsstore.MetricBackupLastSuccessAge,
		Value:  max(now.Sub(newest.CompletedAt).Seconds(), 0),
	}}
	for _, d := range newest.Destinations {
		if d == nil || d.Status != structs.BackupDestinationStatusVerified {
			continue
		}
		labels := map[string]string{"destination": d.Name}
		if obj := d.Objects[structs.BackupObjectState]; obj != nil {
			gauges = append(gauges, metricsstore.Gauge{Metric: metricsstore.MetricBackupSize, Labels: labels, Value: float64(obj.Size)})
		}
		gauges = append(gauges, metricsstore.Gauge{Metric: metricsstore.MetricBackupDuration, Labels: labels, Value: d.Duration.Seconds()})
	}
	return gauges, nil
}

// objectGauges returns the number of objects of every kind and those
// degraded, the node groups and the load balancers.
func objectGauges(snap *state.StateSnapshot) ([]metricsstore.Gauge, error) {
	kinds, err := snap.ObjectKinds(nil, "", state.SortDefault)
	if err != nil {
		return nil, err
	}
	nodeGroupRef := structs.ObjectKindRef(structs.NodeScalingGroup, structs.NodeGroupKind)
	var gauges []metricsstore.Gauge
	for raw := kinds.Next(); raw != nil; raw = kinds.Next() {
		kind := raw.(*structs.ObjectKind)
		ref := structs.ObjectKindRef(kind.Group, kind.Kind)
		iter, err := snap.CustomObjects(nil, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return nil, err
		}
		var count, degraded int
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			obj := raw.(*structs.CustomObject)
			count++
			conditions := objectConditions(obj.Status)
			if conditions["Degraded"] {
				degraded++
			}
			switch ref {
			case nodeGroupRef:
				gauges = append(gauges, nodeGroupGauges(obj, conditions)...)
			case loadBalancerKindRef:
				labels := map[string]string{"ns": obj.Namespace, "name": obj.Name}
				targets, _ := obj.Status["targets"].([]any)
				gauges = append(gauges,
					metricsstore.Gauge{Metric: metricsstore.MetricLBTargets, Labels: labels, Value: float64(len(targets))},
					metricsstore.Gauge{Metric: metricsstore.MetricLBReady, Labels: labels, Value: boolValue(conditions["Ready"])})
			}
		}
		labels := map[string]string{"kind": ref}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricObjectsCount, Labels: labels, Value: float64(count)},
			metricsstore.Gauge{Metric: metricsstore.MetricObjectsDegraded, Labels: labels, Value: float64(degraded)})
	}
	return gauges, nil
}

// nodeGroupGauges returns the gauges of a node group: its replicas, the
// state of its nodes and its rollout, and its conditions.
func nodeGroupGauges(obj *structs.CustomObject, conditions map[string]bool) []metricsstore.Gauge {
	labels := map[string]string{"group": obj.Name}
	var gauges []metricsstore.Gauge
	add := func(metric string, value float64) {
		gauges = append(gauges, metricsstore.Gauge{Metric: metric, Labels: labels, Value: value})
	}
	if v, ok := objectNumber(obj.Spec["replicas"]); ok {
		add(metricsstore.MetricNodeGroupReplicas, v)
	}
	ready, _ := objectNumber(obj.Status["replicas"])
	joining, _ := objectNumber(obj.Status["joining"])
	failed, _ := objectNumber(obj.Status["failed"])
	add(metricsstore.MetricNodeGroupReady, ready)
	add(metricsstore.MetricNodeGroupJoining, joining)
	add(metricsstore.MetricNodeGroupFailed, failed)
	rollout, _ := obj.Status["rollout"].(map[string]any)
	replacing, _ := rollout["replacing"].([]any)
	outdated, _ := rollout["outdated"].([]any)
	add(metricsstore.MetricNodeGroupRolloutReplacing, float64(len(replacing)))
	add(metricsstore.MetricNodeGroupRolloutOutdated, float64(len(outdated)))

	types := make([]string, 0, len(conditions))
	for typ := range conditions {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		gauges = append(gauges, metricsstore.Gauge{Metric: metricsstore.MetricNodeGroupCondition,
			Labels: map[string]string{"group": obj.Name, "type": typ}, Value: boolValue(conditions[typ])})
	}
	return gauges
}

// nodeGroupCertGauges returns the hours until the earliest certificate expiry
// among the nodes of every node group at now: the nodes that name the group
// in their meta (norca.node_group) and carry the expiry of their certificate
// (norca.cert_expires). Down nodes are left out: they no longer use their
// certificate, and the state keeps them until they are garbage collected. A
// certificate that has expired counts as 0. Only the node groups in the
// state count, so a node cannot add a series by naming a group of its own;
// a group without such nodes has no point.
func nodeGroupCertGauges(snap *state.StateSnapshot, now time.Time) ([]metricsstore.Gauge, error) {
	groups, err := snap.CustomObjects(nil, structs.NodeScalingGroup, structs.NodeGroupKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return nil, err
	}
	earliest := map[string]time.Time{}
	for raw := groups.Next(); raw != nil; raw = groups.Next() {
		earliest[raw.(*structs.CustomObject).Name] = time.Time{}
	}
	if len(earliest) == 0 {
		return nil, nil
	}
	nodes, err := snap.Nodes(nil)
	if err != nil {
		return nil, err
	}
	for raw := nodes.Next(); raw != nil; raw = nodes.Next() {
		node := raw.(*structs.Node)
		if node.Status == structs.NodeStatusDown {
			continue
		}
		group := node.Meta[structs.NodeMetaNodeGroup]
		cur, ok := earliest[group]
		if !ok {
			continue
		}
		expires, err := time.Parse(time.RFC3339, node.Meta[structs.NodeMetaCertExpires])
		if err != nil {
			continue
		}
		if cur.IsZero() || expires.Before(cur) {
			earliest[group] = expires
		}
	}
	var gauges []metricsstore.Gauge
	for group, expires := range earliest {
		if expires.IsZero() {
			continue
		}
		gauges = append(gauges, metricsstore.Gauge{
			Metric: metricsstore.MetricNodeGroupCertExpiresMinHours,
			Labels: map[string]string{"group": group},
			Value:  max(expires.Sub(now).Hours(), 0),
		})
	}
	return gauges, nil
}

// objectConditions returns the conditions of the status of an object by
// type: true when the status of the condition is True.
func objectConditions(status map[string]any) map[string]bool {
	list, _ := status["conditions"].([]any)
	out := make(map[string]bool, len(list))
	for _, raw := range list {
		c, _ := raw.(map[string]any)
		typ, _ := c["type"].(string)
		if typ == "" {
			continue
		}
		st, _ := c["status"].(string)
		out[typ] = strings.EqualFold(st, "True")
	}
	return out
}

// objectNumber returns a number of the spec or the status of an object.
func objectNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
