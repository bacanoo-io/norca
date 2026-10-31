// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/hashicorp/nomad/nomad/structs"
)

// defaultMetricsHistory is the range of a history request without From.
const defaultMetricsHistory = time.Hour

// MetricsStore is the RPC endpoint of the history of the task group usage.
type MetricsStore struct {
	srv *Server
	ctx *RPCContext
}

func NewMetricsStoreEndpoint(srv *Server, ctx *RPCContext) *MetricsStore {
	return &MetricsStore{srv: srv, ctx: ctx}
}

// History returns the history of a metric of a task group. It needs
// read-job in the namespace of the request. Every server keeps every
// report, so the request is answered locally, by ServerID when set.
func (m *MetricsStore) History(args *structs.MetricsHistoryRequest, reply *structs.MetricsHistoryResponse) error {
	// The history is local to a server: never forwarded to the leader.
	args.AllowStale = true

	authErr := m.srv.Authenticate(m.ctx, args)
	if done, err := m.srv.forward(structs.MetricsStoreHistoryRPCMethod, args, args, reply); done {
		return err
	}
	m.srv.MeasureRPCRate("metrics_store", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "metrics_store", "history"}, time.Now())

	if aclObj, err := m.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}

	if args.ServerID != "" {
		target, err := m.srv.getServer(m.srv.config.Region, args.ServerID)
		if err != nil {
			return err
		}
		if target.Name != m.srv.LocalMember().Name {
			return m.srv.forwardServer(target, structs.MetricsStoreHistoryRPCMethod, args, reply)
		}
	}

	if args.JobID == "" || args.Group == "" {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "job and group are required")
	}
	if args.Metric == "" {
		args.Metric = metricsstore.MetricCPU
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
	key := metricsstore.GroupKey{Namespace: args.RequestNamespace(), Job: args.JobID, Group: args.Group}
	points, step, err := store.Query(key, args.Metric, from, to, args.MaxPoints)
	if err != nil {
		return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
	}
	reply.Step = step
	for _, p := range points {
		reply.Points = append(reply.Points, &structs.MetricsPoint{Time: p.Time, Avg: p.Avg, Max: p.Max})
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
