// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

// AdmissionPolicy endpoint is used to manage the CEL admission policies of a
// region.
type AdmissionPolicy struct {
	srv *Server
	ctx *RPCContext
}

func NewAdmissionPolicyEndpoint(srv *Server, ctx *RPCContext) *AdmissionPolicy {
	return &AdmissionPolicy{srv: srv, ctx: ctx}
}

// List is used to list admission policies. It supports prefix listing,
// pagination, and filtering.
func (a *AdmissionPolicy) List(args *structs.AdmissionPolicyListRequest, reply *structs.AdmissionPolicyListResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("AdmissionPolicy.List", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("admission_policy", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admission_policy", "list"}, time.Now())

	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowAdmissionRead() {
		return structs.ErrPermissionDenied
	}

	sort := state.SortOption(args.Reverse)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if prefix := args.QueryOptions.Prefix; prefix != "" {
				iter, err = store.AdmissionPoliciesByNamePrefix(ws, prefix, sort)
			} else {
				iter, err = store.AdmissionPolicies(ws, sort)
			}
			if err != nil {
				return err
			}

			tokenizer := paginator.NewStructsTokenizer(iter, paginator.StructsTokenizerOptions{WithID: true})
			policies := []*structs.AdmissionPolicy{}
			pager, err := paginator.NewPaginator(iter, tokenizer, nil, args.QueryOptions,
				func(raw interface{}) error {
					policies = append(policies, raw.(*structs.AdmissionPolicy))
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}

			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}

			reply.QueryMeta.NextToken = nextToken
			reply.AdmissionPolicies = policies

			index, err := store.Index(state.TableAdmissionPolicies)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)

			a.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return a.srv.blockingRPC(&opts)
}

// GetAdmissionPolicy returns the requested admission policy or nil if it
// doesn't exist.
func (a *AdmissionPolicy) GetAdmissionPolicy(args *structs.AdmissionPolicySpecificRequest, reply *structs.SingleAdmissionPolicyResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("AdmissionPolicy.GetAdmissionPolicy", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("admission_policy", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admission_policy", "get_admission_policy"}, time.Now())

	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowAdmissionRead() {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			policy, err := store.AdmissionPolicyByName(ws, args.Name)
			if err != nil {
				return err
			}

			reply.AdmissionPolicy = policy
			if policy != nil {
				reply.Index = policy.ModifyIndex
			} else {
				index, err := store.Index(state.TableAdmissionPolicies)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			a.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return a.srv.blockingRPC(&opts)
}

// UpsertAdmissionPolicies creates or updates admission policies. Every rule
// is compiled first; a policy with a rule that doesn't compile is rejected.
func (a *AdmissionPolicy) UpsertAdmissionPolicies(args *structs.AdmissionPolicyUpsertRequest, reply *structs.GenericResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("AdmissionPolicy.UpsertAdmissionPolicies", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("admission_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admission_policy", "upsert_admission_policies"}, time.Now())

	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowAdmissionWrite() {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumVersion(a.srv.serf.Members(), a.srv.Region(), minNorcaFeaturesVersion, false) {
		return fmt.Errorf("all servers must be running version %v or later to upsert admission policies", minNorcaFeaturesVersion)
	}

	if len(args.AdmissionPolicies) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one admission policy")
	}
	seen := make(map[string]struct{}, len(args.AdmissionPolicies))
	for _, policy := range args.AdmissionPolicies {
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "admission policy is empty")
		}
		policy.Canonicalize()
		if err := policy.Validate(); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid admission policy %q: %v", policy.Name, err)
		}
		if _, ok := seen[policy.Name]; ok {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "duplicate admission policy %q", policy.Name)
		}
		seen[policy.Name] = struct{}{}

		if err := a.srv.admission.policies.Compile(policy); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid admission policy %q: %v", policy.Name, err)
		}
	}

	_, index, err := a.srv.raftApply(structs.AdmissionPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// DeleteAdmissionPolicies deletes admission policies.
func (a *AdmissionPolicy) DeleteAdmissionPolicies(args *structs.AdmissionPolicyDeleteRequest, reply *structs.GenericResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("AdmissionPolicy.DeleteAdmissionPolicies", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("admission_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admission_policy", "delete_admission_policies"}, time.Now())

	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowAdmissionWrite() {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumVersion(a.srv.serf.Members(), a.srv.Region(), minNorcaFeaturesVersion, false) {
		return fmt.Errorf("all servers must be running version %v or later to delete admission policies", minNorcaFeaturesVersion)
	}

	if len(args.Names) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one admission policy to delete")
	}

	// Check existence first so a missing policy is a 404 and not an FSM error.
	snap, err := a.srv.State().Snapshot()
	if err != nil {
		return err
	}
	for _, name := range args.Names {
		policy, err := snap.AdmissionPolicyByName(nil, name)
		if err != nil {
			return err
		}
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "admission policy %q not found", name)
		}
	}

	_, index, err := a.srv.raftApply(structs.AdmissionPolicyDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}
