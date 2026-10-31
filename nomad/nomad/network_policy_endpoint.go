// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

// NetworkPolicy endpoint manages the network policies of namespaces and the
// admin network policies of the region, and serves the effective rules of
// allocations to clients.
type NetworkPolicy struct {
	srv *Server
	ctx *RPCContext
}

func NewNetworkPolicyEndpoint(srv *Server, ctx *RPCContext) *NetworkPolicy {
	return &NetworkPolicy{srv: srv, ctx: ctx}
}

// List lists the network policies of the namespace of the request, or of
// every readable namespace with the "*" namespace. It supports name prefix
// listing, pagination and filtering.
func (n *NetworkPolicy) List(args *structs.NetworkPolicyListRequest, reply *structs.NetworkPolicyListResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.List", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "list"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if namespace != structs.AllNamespacesSentinel && !aclObj.AllowNetworkPolicyRead(namespace) {
		return structs.ErrPermissionDenied
	}

	sort := state.SortOption(args.Reverse)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if namespace == structs.AllNamespacesSentinel {
				iter, err = store.NetworkPolicies(ws, sort)
			} else {
				iter, err = store.NetworkPoliciesByNamespace(ws, namespace, sort)
			}
			if err != nil {
				return err
			}

			tokenizer := paginator.NewStructsTokenizer(iter, paginator.StructsTokenizerOptions{
				WithNamespace: true,
				WithID:        true,
			})
			filters := []paginator.Filter{
				paginator.GenericFilter{
					Allow: func(raw interface{}) (bool, error) {
						policy := raw.(*structs.NetworkPolicy)
						if !strings.HasPrefix(policy.Name, args.Prefix) {
							return false, nil
						}
						return aclObj.AllowNetworkPolicyRead(policy.Namespace), nil
					},
				},
			}
			policies := []*structs.NetworkPolicy{}
			pager, err := paginator.NewPaginator(iter, tokenizer, filters, args.QueryOptions,
				func(raw interface{}) error {
					policies = append(policies, raw.(*structs.NetworkPolicy))
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
			reply.NetworkPolicies = policies

			index, err := store.Index(state.TableNetworkPolicies)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// GetNetworkPolicy returns the requested network policy or nil.
func (n *NetworkPolicy) GetNetworkPolicy(args *structs.NetworkPolicySpecificRequest, reply *structs.SingleNetworkPolicyResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.GetNetworkPolicy", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "get_network_policy"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !aclObj.AllowNetworkPolicyRead(namespace) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			policy, err := store.NetworkPolicyByName(ws, namespace, args.Name)
			if err != nil {
				return err
			}
			reply.NetworkPolicy = policy
			if policy != nil {
				reply.Index = policy.ModifyIndex
			} else {
				index, err := store.Index(state.TableNetworkPolicies)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// UpsertNetworkPolicies creates or updates network policies. Each policy
// carries its namespace, which must exist.
func (n *NetworkPolicy) UpsertNetworkPolicies(args *structs.NetworkPolicyUpsertRequest, reply *structs.GenericResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.UpsertNetworkPolicies", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "upsert_network_policies"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	if !ServersMeetMinimumNorcaVersion(n.srv.serf.Members(), n.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to upsert network policies", minNorcaFeaturesVersion)
	}

	if len(args.NetworkPolicies) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one network policy")
	}
	snap, err := n.srv.State().Snapshot()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(args.NetworkPolicies))
	for _, policy := range args.NetworkPolicies {
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "network policy is empty")
		}
		if policy.Namespace == "" {
			policy.Namespace = args.RequestNamespace()
		}
		policy.Canonicalize()
		if !aclObj.AllowNetworkPolicyWrite(policy.Namespace) {
			return structs.ErrPermissionDenied
		}
		if err := policy.Validate(); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid network policy %q: %v", policy.Name, err)
		}
		key := policy.Namespace + "\x00" + policy.Name
		if _, ok := seen[key]; ok {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "duplicate network policy %q", policy.Name)
		}
		seen[key] = struct{}{}

		ns, err := snap.NamespaceByName(nil, policy.Namespace)
		if err != nil {
			return err
		}
		if ns == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "namespace %q does not exist", policy.Namespace)
		}
	}

	_, index, err := n.srv.raftApply(structs.NetworkPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// DeleteNetworkPolicies deletes network policies of the namespace of the
// request.
func (n *NetworkPolicy) DeleteNetworkPolicies(args *structs.NetworkPolicyDeleteRequest, reply *structs.GenericResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.DeleteNetworkPolicies", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "delete_network_policies"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !aclObj.AllowNetworkPolicyWrite(namespace) {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(n.srv.serf.Members(), n.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to delete network policies", minNorcaFeaturesVersion)
	}

	if len(args.Names) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one network policy to delete")
	}
	snap, err := n.srv.State().Snapshot()
	if err != nil {
		return err
	}
	for _, name := range args.Names {
		policy, err := snap.NetworkPolicyByName(nil, namespace, name)
		if err != nil {
			return err
		}
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "network policy %q not found in namespace %q", name, namespace)
		}
	}

	_, index, err := n.srv.raftApply(structs.NetworkPolicyDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// ListAdminNetworkPolicies lists the admin network policies in evaluation
// order (priority, then name).
func (n *NetworkPolicy) ListAdminNetworkPolicies(args *structs.AdminNetworkPolicyListRequest, reply *structs.AdminNetworkPolicyListResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.ListAdminNetworkPolicies", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("admin_network_policy", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admin_network_policy", "list"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !allowNetworkPolicyOperator(aclObj, false) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			iter, err := store.AdminNetworkPolicies(ws, state.SortDefault)
			if err != nil {
				return err
			}
			policies := []*structs.AdminNetworkPolicy{}
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				policy := raw.(*structs.AdminNetworkPolicy)
				if strings.HasPrefix(policy.Name, args.Prefix) {
					policies = append(policies, policy)
				}
			}
			structs.SortAdminNetworkPolicies(policies)
			reply.AdminNetworkPolicies = policies

			index, err := store.Index(state.TableAdminNetworkPolicies)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// GetAdminNetworkPolicy returns the requested admin network policy or nil.
func (n *NetworkPolicy) GetAdminNetworkPolicy(args *structs.AdminNetworkPolicySpecificRequest, reply *structs.SingleAdminNetworkPolicyResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.GetAdminNetworkPolicy", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("admin_network_policy", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admin_network_policy", "get_admin_network_policy"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !allowNetworkPolicyOperator(aclObj, false) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			policy, err := store.AdminNetworkPolicyByName(ws, args.Name)
			if err != nil {
				return err
			}
			reply.AdminNetworkPolicy = policy
			if policy != nil {
				reply.Index = policy.ModifyIndex
			} else {
				index, err := store.Index(state.TableAdminNetworkPolicies)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// UpsertAdminNetworkPolicies creates or updates admin network policies. It
// requires operator write.
func (n *NetworkPolicy) UpsertAdminNetworkPolicies(args *structs.AdminNetworkPolicyUpsertRequest, reply *structs.GenericResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.UpsertAdminNetworkPolicies", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("admin_network_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admin_network_policy", "upsert_admin_network_policies"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !allowNetworkPolicyOperator(aclObj, true) {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(n.srv.serf.Members(), n.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to upsert admin network policies", minNorcaFeaturesVersion)
	}

	if len(args.AdminNetworkPolicies) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one admin network policy")
	}
	seen := make(map[string]struct{}, len(args.AdminNetworkPolicies))
	for _, policy := range args.AdminNetworkPolicies {
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "admin network policy is empty")
		}
		policy.Canonicalize()
		if err := policy.Validate(); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid admin network policy %q: %v", policy.Name, err)
		}
		if _, ok := seen[policy.Name]; ok {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "duplicate admin network policy %q", policy.Name)
		}
		seen[policy.Name] = struct{}{}
	}

	_, index, err := n.srv.raftApply(structs.AdminNetworkPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// DeleteAdminNetworkPolicies deletes admin network policies. It requires
// operator write.
func (n *NetworkPolicy) DeleteAdminNetworkPolicies(args *structs.AdminNetworkPolicyDeleteRequest, reply *structs.GenericResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.DeleteAdminNetworkPolicies", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("admin_network_policy", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "admin_network_policy", "delete_admin_network_policies"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !allowNetworkPolicyOperator(aclObj, true) {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(n.srv.serf.Members(), n.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to delete admin network policies", minNorcaFeaturesVersion)
	}

	if len(args.Names) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one admin network policy to delete")
	}
	snap, err := n.srv.State().Snapshot()
	if err != nil {
		return err
	}
	for _, name := range args.Names {
		policy, err := snap.AdminNetworkPolicyByName(nil, name)
		if err != nil {
			return err
		}
		if policy == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "admin network policy %q not found", name)
		}
	}

	_, index, err := n.srv.raftApply(structs.AdminNetworkPolicyDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// Status lists the non-terminal allocations a network policy (or, with
// Admin set, an admin network policy) selects, and whether their nodes can
// enforce the rules. Allocations of namespaces the token may not read jobs
// in are left out.
func (n *NetworkPolicy) Status(args *structs.NetworkPolicyStatusRequest, reply *structs.NetworkPolicyStatusResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.Status", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "status"}, time.Now())

	aclObj, err := n.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if args.Admin {
		if !allowNetworkPolicyOperator(aclObj, false) {
			return structs.ErrPermissionDenied
		}
	} else if !aclObj.AllowNetworkPolicyRead(namespace) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var selects func(namespace string, meta map[string]string) bool
			var iter memdb.ResultIterator
			if args.Admin {
				policy, err := store.AdminNetworkPolicyByName(ws, args.Name)
				if err != nil {
					return err
				}
				if policy == nil {
					return structs.NewErrRPCCodedf(http.StatusNotFound, "admin network policy %q not found", args.Name)
				}
				selects = policy.Selects
				iter, err = store.Allocs(ws, state.SortDefault)
				if err != nil {
					return err
				}
			} else {
				policy, err := store.NetworkPolicyByName(ws, namespace, args.Name)
				if err != nil {
					return err
				}
				if policy == nil {
					return structs.NewErrRPCCodedf(http.StatusNotFound, "network policy %q not found in namespace %q", args.Name, namespace)
				}
				selects = policy.Selects
				iter, err = store.AllocsByNamespace(ws, namespace)
				if err != nil {
					return err
				}
			}

			nodes := make(map[string]*structs.Node)
			out := []*structs.NetworkPolicyAllocStatus{}
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				alloc := raw.(*structs.Allocation)
				if alloc.TerminalStatus() || alloc.Job == nil {
					continue
				}
				if !aclObj.AllowNsOp(alloc.Namespace, acl.NamespaceCapabilityReadJob) {
					continue
				}
				tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
				if tg == nil || !selects(alloc.Namespace, structs.NetworkPolicyMeta(alloc.Job, tg)) {
					continue
				}
				node, ok := nodes[alloc.NodeID]
				if !ok {
					var err error
					node, err = store.NodeByID(nil, alloc.NodeID)
					if err != nil {
						return err
					}
					nodes[alloc.NodeID] = node
				}
				mode := structs.NetworkPolicyGroupMode(tg)
				enforceable, reason := structs.NetworkPolicyNodeCanEnforce(node, mode)
				status := &structs.NetworkPolicyAllocStatus{
					ID:          alloc.ID,
					Namespace:   alloc.Namespace,
					JobID:       alloc.JobID,
					TaskGroup:   alloc.TaskGroup,
					NodeID:      alloc.NodeID,
					NetworkMode: mode,
					Enforceable: enforceable,
					Reason:      reason,
				}
				if node != nil {
					status.NodeName = node.Name
				}
				out = append(out, status)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			reply.Allocations = out

			index, err := store.Index("allocs")
			if err != nil {
				return err
			}
			for _, table := range []string{state.TableNetworkPolicies, state.TableAdminNetworkPolicies} {
				ti, err := store.Index(table)
				if err != nil {
					return err
				}
				index = max(index, ti)
			}
			reply.Index = max(1, index)
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// GetAllocRules returns the effective network rules of an allocation. It is
// called by the client running the allocation with a blocking query; when
// ACLs are enabled other callers need the read-job and read-network-policy
// capabilities in the namespace of the allocation.
func (n *NetworkPolicy) GetAllocRules(args *structs.NetworkPolicyAllocRequest, reply *structs.NetworkPolicyAllocResponse) error {
	authErr := n.srv.Authenticate(n.ctx, args)
	if done, err := n.srv.forward("NetworkPolicy.GetAllocRules", args, args, reply); done {
		return err
	}
	n.srv.MeasureRPCRate("network_policy", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "network_policy", "get_alloc_rules"}, time.Now())

	if args.AllocID == "" {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing allocation ID")
	}
	aclObj, err := n.srv.ResolveClientOrACL(args)
	if err != nil {
		return err
	}
	var clientID string
	if identity := args.GetIdentity(); identity != nil {
		clientID = identity.ClientID
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			alloc, err := store.AllocByID(ws, args.AllocID)
			if err != nil {
				return err
			}
			if alloc != nil {
				if clientID != "" && clientID != alloc.NodeID {
					return structs.ErrPermissionDenied
				}
				if !aclObj.AllowNsOp(alloc.Namespace, acl.NamespaceCapabilityReadJob) ||
					!aclObj.AllowNetworkPolicyRead(alloc.Namespace) {
					return structs.ErrPermissionDenied
				}
			} else if clientID == "" {
				// white room bugfix: a missing allocation needs the same
				// permissions on the namespace of the request, so a caller
				// without them gets the same answer for an existing and a
				// missing allocation (no existence oracle). Nodes keep
				// blocking on allocations they do not have yet.
				ns := args.RequestNamespace()
				if !aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadJob) || !aclObj.AllowNetworkPolicyRead(ns) {
					return structs.ErrPermissionDenied
				}
			}

			reply.Rules, err = networkPolicyAllocRules(ws, store, alloc)
			if err != nil {
				return err
			}

			var index uint64
			for _, table := range []string{"allocs", state.TableNetworkPolicies, state.TableAdminNetworkPolicies} {
				ti, err := store.Index(table)
				if err != nil {
					return err
				}
				index = max(index, ti)
			}
			reply.Index = max(1, index)
			n.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return n.srv.blockingRPC(&opts)
}

// allowNetworkPolicyOperator checks the operator rule that admin network
// policies require. A nil ACL object means ACLs are disabled.
func allowNetworkPolicyOperator(aclObj *acl.ACL, write bool) bool {
	if aclObj == nil {
		return true
	}
	if write {
		return aclObj.AllowOperatorWrite()
	}
	return aclObj.AllowOperatorRead()
}

// networkPolicyAllocRules computes the effective rules of an allocation. It
// returns an empty rule set for missing allocations.
func networkPolicyAllocRules(ws memdb.WatchSet, store *state.StateStore, alloc *structs.Allocation) (*structs.NetworkPolicyAllocRules, error) {
	rules := &structs.NetworkPolicyAllocRules{}
	if alloc == nil || alloc.Job == nil {
		return rules, nil
	}
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return rules, nil
	}

	policies, admin, err := store.NetworkPoliciesForGroup(ws, alloc.Namespace, structs.NetworkPolicyMeta(alloc.Job, tg))
	if err != nil {
		return nil, err
	}
	rules.Policies = policies
	rules.AdminPolicies = admin
	if rules.Empty() {
		return rules, nil
	}

	// Resolve the allocation peers to the other allocations of the node.
	type peerRef struct {
		namespace string
		peer      *structs.NetworkPolicyPeer
	}
	var peers []peerRef
	collect := func(namespace string, list ...[]*structs.NetworkPolicyRule) {
		for _, ruleList := range list {
			for _, rule := range ruleList {
				for _, peer := range rule.Peers {
					if peer != nil && len(peer.CIDR) == 0 {
						peers = append(peers, peerRef{namespace, peer})
					}
				}
			}
		}
	}
	for _, p := range policies {
		collect(p.Namespace, p.Ingress, p.Egress)
	}
	for _, p := range admin {
		collect("", p.Ingress, p.Egress)
	}
	if len(peers) == 0 {
		return rules, nil
	}

	allocs, err := store.AllocsByNode(ws, alloc.NodeID)
	if err != nil {
		return nil, err
	}
	for _, other := range allocs {
		if other.ID == alloc.ID || other.TerminalStatus() || other.Job == nil ||
			other.NetworkStatus == nil || other.NetworkStatus.Address == "" {
			continue
		}
		otherTG := other.Job.LookupTaskGroup(other.TaskGroup)
		if otherTG == nil {
			continue
		}
		meta := structs.NetworkPolicyMeta(other.Job, otherTG)
		for _, ref := range peers {
			if ref.peer.Matches(ref.namespace, other.Namespace, meta) {
				rules.Peers = append(rules.Peers, &structs.NetworkPolicyPeerAlloc{
					ID:        other.ID,
					Namespace: other.Namespace,
					Meta:      meta,
					Address:   other.NetworkStatus.Address,
				})
				break
			}
		}
	}
	sort.Slice(rules.Peers, func(i, j int) bool { return rules.Peers[i].ID < rules.Peers[j].ID })
	return rules, nil
}
