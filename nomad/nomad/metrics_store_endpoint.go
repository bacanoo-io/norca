// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// defaultMetricsHistory is the range of a history request without From.
	defaultMetricsHistory = time.Hour

	// defaultMetricsSummaryWindow is the window of a summary without one.
	defaultMetricsSummaryWindow = 30 * 24 * time.Hour
)

// MetricsStore is the RPC endpoint of the history of the task group usage
// and of the gauges the servers sample.
type MetricsStore struct {
	srv *Server
	ctx *RPCContext
}

func NewMetricsStoreEndpoint(srv *Server, ctx *RPCContext) *MetricsStore {
	return &MetricsStore{srv: srv, ctx: ctx}
}

// metricsStoreRequest is a request of the MetricsStore endpoint.
type metricsStoreRequest interface {
	structs.RPCInfo
	structs.RequestWithIdentity
}

// local authenticates a request answered by one server of the region: the
// one that receives it, or the one of serverID. It returns the ACL (nil:
// ACLs disabled) and done when the request was answered elsewhere.
func (m *MetricsStore) local(method, serverID string, args metricsStoreRequest, opts *structs.QueryOptions, reply any) (*acl.ACL, bool, error) {
	// The store is local to a server: never forwarded to the leader.
	opts.AllowStale = true

	authErr := m.srv.Authenticate(m.ctx, args)
	if done, err := m.srv.forward(method, args, args, reply); done {
		return nil, true, err
	}
	m.srv.MeasureRPCRate("metrics_store", structs.RateMetricRead, args)
	if authErr != nil {
		return nil, true, structs.ErrPermissionDenied
	}
	aclObj, err := m.srv.ResolveACL(args)
	if err != nil {
		return nil, true, err
	}
	if serverID != "" {
		target, err := m.srv.getServer(m.srv.config.Region, serverID)
		if err != nil {
			return nil, true, err
		}
		if target.Name != m.srv.LocalMember().Name {
			return nil, true, m.srv.forwardServer(target, method, args, reply)
		}
	}
	return aclObj, false, nil
}

// allowGauge reports whether aclObj (nil: ACLs disabled) may read a gauge
// metric: node read for the cluster, quota read for the quotas, operator
// read for the health of the servers.
func allowGauge(aclObj *acl.ACL, metric string) bool {
	if aclObj == nil {
		return true
	}
	switch {
	case strings.HasPrefix(metric, "norca.cluster.autopilot_"):
		return aclObj.AllowOperatorRead()
	case strings.HasPrefix(metric, "norca.cluster."):
		return aclObj.AllowNodeRead()
	case strings.HasPrefix(metric, "norca.quota."):
		return aclObj.AllowQuotaRead()
	}
	return false
}

// History returns the history of a metric: of a task group (read-job in the
// namespace of the request) or of a gauge (see allowGauge). Every server
// keeps every report, so the request is answered locally, by ServerID when
// set.
func (m *MetricsStore) History(args *structs.MetricsHistoryRequest, reply *structs.MetricsHistoryResponse) error {
	aclObj, done, err := m.local(structs.MetricsStoreHistoryRPCMethod, args.ServerID, args, &args.QueryOptions, reply)
	if done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "metrics_store", "history"}, time.Now())

	if args.Metric == "" {
		args.Metric = metricsstore.MetricCPU
	}
	_, gauge := metricsstore.GaugeLabels(args.Metric)
	if gauge {
		if !allowGauge(aclObj, args.Metric) {
			return structs.ErrPermissionDenied
		}
	} else {
		if aclObj != nil && !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityReadJob) {
			return structs.ErrPermissionDenied
		}
		if args.JobID == "" || args.Group == "" {
			return structs.NewErrRPCCoded(http.StatusBadRequest, "job and group are required")
		}
	}

	reply.Metric = args.Metric
	reply.Points = []*structs.MetricsPoint{}
	store := m.srv.metricsStore
	if store == nil {
		return nil
	}
	reply.Enabled = true

	to := args.To
	if to.IsZero() {
		to = time.Now()
	}
	from := args.From
	if from.IsZero() {
		from = to.Add(-defaultMetricsHistory)
	}
	var points []metricsstore.Point
	var step time.Duration
	if gauge {
		points, step, err = store.QueryGauge(args.Metric, args.Labels, from, to, args.MaxPoints)
	} else {
		key := metricsstore.GroupKey{Namespace: args.RequestNamespace(), Job: args.JobID, Group: args.Group}
		points, step, err = store.Query(key, args.Metric, from, to, args.MaxPoints)
	}
	if err != nil {
		return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
	}
	reply.Step = step
	for _, p := range points {
		reply.Points = append(reply.Points, &structs.MetricsPoint{Time: p.Time, Avg: p.Avg, Max: p.Max})
	}
	return nil
}

// Series lists the task groups with series in the namespaces the token may
// read jobs of, and the gauge series it may read.
func (m *MetricsStore) Series(args *structs.MetricsSeriesRequest, reply *structs.MetricsSeriesResponse) error {
	aclObj, done, err := m.local(structs.MetricsStoreSeriesRPCMethod, args.ServerID, args, &args.QueryOptions, reply)
	if done {
		return err
	}
	reply.Groups = []*structs.MetricsGroup{}
	reply.Gauges = []*structs.MetricsGaugeSeries{}
	store := m.srv.metricsStore
	if store == nil {
		return nil
	}
	reply.Enabled = true
	for _, g := range store.Groups() {
		if aclObj == nil || aclObj.AllowNsOp(g.Namespace, acl.NamespaceCapabilityReadJob) {
			reply.Groups = append(reply.Groups, &structs.MetricsGroup{Namespace: g.Namespace, JobID: g.Job, Group: g.Group})
		}
	}
	for _, g := range store.Gauges() {
		if allowGauge(aclObj, g.Metric) {
			reply.Gauges = append(reply.Gauges, &structs.MetricsGaugeSeries{Metric: g.Metric, Labels: g.Labels})
		}
	}
	return nil
}

// summaryMetrics are the metrics of a summary.
var summaryMetrics = []string{
	metricsstore.MetricCPU, metricsstore.MetricCPUMHz, metricsstore.MetricCPUThrottled,
	metricsstore.MetricMemory, metricsstore.MetricMemoryMB, metricsstore.MetricMemoryCacheMB,
}

// Summary returns the usage of a task group over a window against the
// resources an allocation of the current job version has, for right-sizing.
// It needs read-job in the namespace of the request.
func (m *MetricsStore) Summary(args *structs.MetricsSummaryRequest, reply *structs.MetricsSummaryResponse) error {
	aclObj, done, err := m.local(structs.MetricsStoreSummaryRPCMethod, args.ServerID, args, &args.QueryOptions, reply)
	if done {
		return err
	}
	if aclObj != nil && !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}
	if args.JobID == "" || args.Group == "" {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "job and group are required")
	}
	window := args.Window
	if window <= 0 {
		window = defaultMetricsSummaryWindow
	}
	reply.Window = window
	reply.Metrics = map[string]*structs.MetricsSummary{}

	job, err := m.srv.State().JobByID(nil, args.RequestNamespace(), args.JobID)
	if err != nil {
		return err
	}
	var tg *structs.TaskGroup
	if job != nil {
		tg = job.LookupTaskGroup(args.Group)
	}
	if tg != nil {
		for _, task := range tg.Tasks {
			if task.Resources != nil {
				reply.AllocatedCPUMHz += int64(task.Resources.CPU)
				reply.AllocatedMemoryMB += int64(task.Resources.MemoryMB)
			}
		}
	}

	store := m.srv.metricsStore
	if store == nil {
		return nil
	}
	reply.Enabled = true
	now := time.Now()
	key := metricsstore.GroupKey{Namespace: args.RequestNamespace(), Job: args.JobID, Group: args.Group}
	for _, metric := range summaryMetrics {
		sum, err := store.Summarize(key, metric, now.Add(-window), now)
		if err != nil {
			return err
		}
		reply.Metrics[metric] = &structs.MetricsSummary{Avg: sum.Avg, P95: sum.P95, Max: sum.Max, Samples: sum.Samples}
	}
	return nil
}

// MetricsStoreStatus returns the state of the metrics store of a server. It
// needs operator read.
func (op *Operator) MetricsStoreStatus(args *structs.MetricsStoreStatusRequest, reply *structs.MetricsStoreStatusResponse) error {
	// The status is local to a server: never forwarded to the leader.
	args.AllowStale = true

	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward(structs.MetricsStoreStatusRPCMethod, args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if aclObj, err := op.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorRead() {
		return structs.ErrPermissionDenied
	}

	if args.ServerID != "" {
		target, err := op.srv.getServer(op.srv.config.Region, args.ServerID)
		if err != nil {
			return err
		}
		if target.Name != op.srv.LocalMember().Name {
			return op.srv.forwardServer(target, structs.MetricsStoreStatusRPCMethod, args, reply)
		}
	}

	status := &structs.MetricsStoreStatus{Server: op.srv.LocalMember().Name}
	reply.Status = status
	store := op.srv.metricsStore
	if store == nil {
		return nil
	}
	st := store.Status()
	status.Enabled = true
	status.Partitions = st.Partitions
	status.DiskPartitions = st.DiskPartitions
	status.DiskBytes = st.DiskBytes
	status.MaxDiskSize = st.MaxDiskSize
	status.Series = st.Series
	status.MaxSeries = st.MaxSeries
	status.Oldest = st.Oldest
	status.Newest = st.Newest
	status.DroppedReports = st.DroppedReports
	status.DroppedGroups = st.DroppedGroups
	status.Retention = st.Retention
	status.SampleInterval = st.SampleInterval
	return nil
}
