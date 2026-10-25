// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"

	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Quota endpoint is used for managing quota specifications and reading quota
// usage.
type Quota struct {
	srv *Server
	ctx *RPCContext
}

func NewQuotaEndpoint(srv *Server, ctx *RPCContext) *Quota {
	return &Quota{srv: srv, ctx: ctx}
}

// UpsertQuotaSpecs is used to create or update a set of quota specifications.
// Quota specifications are global objects written in the authoritative region
// and replicated to the other regions.
func (q *Quota) UpsertQuotaSpecs(args *structs.QuotaSpecUpsertRequest, reply *structs.GenericResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	args.Region = q.srv.config.AuthoritativeRegion
	if done, err := q.srv.forward("Quota.UpsertQuotaSpecs", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "upsert_quota_specs"}, time.Now())

	aclObj, err := q.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowQuotaWrite() {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(q.srv.serf.Members(), q.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to upsert quotas", minNorcaFeaturesVersion)
	}

	if len(args.Quotas) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one quota specification")
	}
	for _, spec := range args.Quotas {
		if err := spec.Validate(); err != nil {
			name := ""
			if spec != nil {
				name = spec.Name
			}
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid quota specification %q: %v", name, err)
		}
		spec.Canonicalize()
		spec.SetHash()
	}

	_, index, err := q.srv.raftApply(structs.QuotaSpecUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// DeleteQuotaSpecs is used to delete a set of quota specifications. A quota
// that is still attached to a namespace can't be deleted.
func (q *Quota) DeleteQuotaSpecs(args *structs.QuotaSpecDeleteRequest, reply *structs.GenericResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	args.Region = q.srv.config.AuthoritativeRegion
	if done, err := q.srv.forward("Quota.DeleteQuotaSpecs", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "delete_quota_specs"}, time.Now())

	aclObj, err := q.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowQuotaWrite() {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(q.srv.serf.Members(), q.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to delete quotas", minNorcaFeaturesVersion)
	}

	if len(args.Names) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one quota specification to delete")
	}

	// Check up front that the quotas exist and are unused so the caller gets a
	// meaningful error instead of a failed Raft apply.
	snap, err := q.srv.State().Snapshot()
	if err != nil {
		return err
	}
	for _, name := range args.Names {
		spec, err := snap.QuotaSpecByName(nil, name)
		if err != nil {
			return err
		}
		if spec == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "quota %q not found", name)
		}
		iter, err := snap.NamespacesByQuota(nil, name)
		if err != nil {
			return err
		}
		if raw := iter.Next(); raw != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"quota %q is in use by namespace %q", name, raw.(*structs.Namespace).Name)
		}
	}

	_, index, err := q.srv.raftApply(structs.QuotaSpecDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// ListQuotaSpecs is used to list the quota specifications.
func (q *Quota) ListQuotaSpecs(args *structs.QuotaSpecListRequest, reply *structs.QuotaSpecListResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	if done, err := q.srv.forward("Quota.ListQuotaSpecs", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "list_quota_specs"}, time.Now())

	if err := q.checkRead(args); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if prefix := args.QueryOptions.Prefix; prefix != "" {
				iter, err = store.QuotaSpecsByNamePrefix(ws, prefix, state.SortDefault)
			} else {
				iter, err = store.QuotaSpecs(ws, state.SortDefault)
			}
			if err != nil {
				return err
			}

			reply.Quotas = nil
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				reply.Quotas = append(reply.Quotas, raw.(*structs.QuotaSpec))
			}

			index, err := store.Index(state.TableQuotaSpecs)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			return nil
		}}
	return q.srv.blockingRPC(&opts)
}

// GetQuotaSpec is used to get a single quota specification.
func (q *Quota) GetQuotaSpec(args *structs.QuotaSpecSpecificRequest, reply *structs.SingleQuotaSpecResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	if done, err := q.srv.forward("Quota.GetQuotaSpec", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "get_quota_spec"}, time.Now())

	if err := q.checkRead(args); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			spec, err := store.QuotaSpecByName(ws, args.Name)
			if err != nil {
				return err
			}

			reply.Quota = spec
			if spec != nil {
				reply.Index = spec.ModifyIndex
				return nil
			}

			index, err := store.Index(state.TableQuotaSpecs)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			return nil
		}}
	return q.srv.blockingRPC(&opts)
}

// ListQuotaUsages is used to list the quota usages in this region.
func (q *Quota) ListQuotaUsages(args *structs.QuotaSpecListRequest, reply *structs.QuotaUsageListResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	if done, err := q.srv.forward("Quota.ListQuotaUsages", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "list_quota_usages"}, time.Now())

	if err := q.checkRead(args); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if prefix := args.QueryOptions.Prefix; prefix != "" {
				iter, err = store.QuotaUsagesByNamePrefix(ws, prefix, state.SortDefault)
			} else {
				iter, err = store.QuotaUsages(ws, state.SortDefault)
			}
			if err != nil {
				return err
			}

			reply.Usages = nil
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				usage, err := store.QuotaUsageWithVariables(ws, raw.(*structs.QuotaUsage))
				if err != nil {
					return err
				}
				reply.Usages = append(reply.Usages, usage)
			}

			index, err := store.Index(state.TableQuotaUsage)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			return nil
		}}
	return q.srv.blockingRPC(&opts)
}

// GetQuotaUsage is used to get the usage of a single quota in this region.
func (q *Quota) GetQuotaUsage(args *structs.QuotaSpecSpecificRequest, reply *structs.SingleQuotaUsageResponse) error {
	authErr := q.srv.Authenticate(q.ctx, args)
	if done, err := q.srv.forward("Quota.GetQuotaUsage", args, args, reply); done {
		return err
	}
	q.srv.MeasureRPCRate("quota", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "quota", "get_quota_usage"}, time.Now())

	if err := q.checkRead(args); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			usage, err := store.QuotaUsageByName(ws, args.Name)
			if err != nil {
				return err
			}

			reply.Usage, err = store.QuotaUsageWithVariables(ws, usage)
			if err != nil {
				return err
			}
			if usage != nil {
				reply.Index = usage.ModifyIndex
				return nil
			}

			index, err := store.Index(state.TableQuotaUsage)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			return nil
		}}
	return q.srv.blockingRPC(&opts)
}

// checkRead verifies the request has the quota read capability.
func (q *Quota) checkRead(args structs.RequestWithIdentity) error {
	aclObj, err := q.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowQuotaRead() {
		return structs.ErrPermissionDenied
	}
	return nil
}
