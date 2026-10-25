// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// maxServiceIntentionsPerRequest bounds the intentions of one write.
const maxServiceIntentionsPerRequest = 1000

// ServiceIntention endpoint manages the service intentions, which decide
// which services may connect to which over native mTLS.
//
// ACL: reading the intentions of a namespace requires read-job on it, or a
// workload identity of the namespace (so applications can fetch the
// intentions they enforce with their own identity); writing requires the
// write-intention capability, which no policy shorthand includes.
type ServiceIntention struct {
	srv *Server
	ctx *RPCContext
}

func NewServiceIntentionEndpoint(srv *Server, ctx *RPCContext) *ServiceIntention {
	return &ServiceIntention{srv: srv, ctx: ctx}
}

// intentionReadFilter returns a function reporting whether the caller may
// read the intentions of a namespace.
func (si *ServiceIntention) intentionReadFilter(args structs.RequestWithIdentity) (func(string) bool, error) {
	if identity := args.GetIdentity(); identity != nil && identity.Claims != nil {
		ns := identity.Claims.Namespace
		return func(namespace string) bool { return namespace == ns }, nil
	}
	aclObj, err := si.srv.ResolveACL(args)
	if err != nil {
		return nil, err
	}
	return func(namespace string) bool {
		return aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadJob)
	}, nil
}

// List returns the intentions of the request namespace, or of every
// namespace the caller may read with the "*" namespace.
func (si *ServiceIntention) List(args *structs.ServiceIntentionListRequest, reply *structs.ServiceIntentionListResponse) error {
	authErr := si.srv.Authenticate(si.ctx, args)
	if done, err := si.srv.forward("ServiceIntention.List", args, args, reply); done {
		return err
	}
	si.srv.MeasureRPCRate("service_intention", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_intention", "list"}, time.Now())

	allowed, err := si.intentionReadFilter(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if namespace != structs.AllNamespacesSentinel && !allowed(namespace) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			intentions, err := store.ServiceIntentions(ws, namespace)
			if err != nil {
				return err
			}
			reply.Intentions = make([]*structs.ServiceIntention, 0, len(intentions))
			for _, intention := range intentions {
				if allowed(intention.Namespace) {
					reply.Intentions = append(reply.Intentions, intention)
				}
			}
			index, err := store.Index(state.TableServiceIntentions)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			si.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return si.srv.blockingRPC(&opts)
}

// Get returns one intention of the request namespace or nil.
func (si *ServiceIntention) Get(args *structs.ServiceIntentionGetRequest, reply *structs.SingleServiceIntentionResponse) error {
	authErr := si.srv.Authenticate(si.ctx, args)
	if done, err := si.srv.forward("ServiceIntention.Get", args, args, reply); done {
		return err
	}
	si.srv.MeasureRPCRate("service_intention", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_intention", "get"}, time.Now())

	allowed, err := si.intentionReadFilter(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !allowed(namespace) {
		return structs.ErrPermissionDenied
	}
	sourceNamespace := args.SourceNamespace
	if sourceNamespace == "" {
		sourceNamespace = namespace
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			intention, err := store.ServiceIntentionByKey(ws, namespace, args.Destination, sourceNamespace, args.Source)
			if err != nil {
				return err
			}
			reply.Intention = intention
			if intention != nil {
				reply.Index = intention.ModifyIndex
			} else {
				index, err := store.Index(state.TableServiceIntentions)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			si.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return si.srv.blockingRPC(&opts)
}

// prepareWrite canonicalizes the intentions of a write request and checks the
// ACL, the namespaces and duplicates.
func (si *ServiceIntention) prepareWrite(args structs.RequestWithIdentity, requestNamespace string, intentions []*structs.ServiceIntention, validate bool) error {
	if !ServersMeetMinimumNorcaVersion(si.srv.serf.Members(), si.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to change service intentions", minNorcaFeaturesVersion)
	}
	if len(intentions) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one service intention")
	}
	if len(intentions) > maxServiceIntentionsPerRequest {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "at most %d service intentions can be written at once", maxServiceIntentionsPerRequest)
	}
	aclObj, err := si.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if si.srv.config.ACLEnabled && aclObj == nil {
		// Client nodes and workload identities without policies may not
		// write intentions.
		return structs.ErrPermissionDenied
	}
	snap, err := si.srv.State().Snapshot()
	if err != nil {
		return err
	}

	var seen []*structs.ServiceIntention
	for _, intention := range intentions {
		if intention == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "service intention is empty")
		}
		if intention.Namespace == "" {
			intention.Namespace = requestNamespace
		}
		intention.Canonicalize()
		if validate {
			if err := intention.Validate(); err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid service intention %s: %v", intention.Key(), err)
			}
		}
		if !aclObj.AllowNsOp(intention.Namespace, acl.NamespaceCapabilityWriteIntention) {
			return structs.ErrPermissionDenied
		}
		if slices.ContainsFunc(seen, intention.SameKey) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "duplicate service intention %s", intention.Key())
		}
		seen = append(seen, intention)
		if validate {
			ns, err := snap.NamespaceByName(nil, intention.Namespace)
			if err != nil {
				return err
			}
			if ns == nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "namespace %q does not exist", intention.Namespace)
			}
		}
	}
	return nil
}

// Upsert creates or replaces intentions. An intention without a namespace
// belongs to the request namespace.
func (si *ServiceIntention) Upsert(args *structs.ServiceIntentionUpsertRequest, reply *structs.GenericResponse) error {
	authErr := si.srv.Authenticate(si.ctx, args)
	if done, err := si.srv.forward("ServiceIntention.Upsert", args, args, reply); done {
		return err
	}
	si.srv.MeasureRPCRate("service_intention", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_intention", "upsert"}, time.Now())

	if err := si.prepareWrite(args, args.RequestNamespace(), args.Intentions, true); err != nil {
		return err
	}
	_, index, err := si.srv.raftApply(structs.ServiceIntentionUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// Delete deletes intentions by key.
func (si *ServiceIntention) Delete(args *structs.ServiceIntentionDeleteRequest, reply *structs.GenericResponse) error {
	authErr := si.srv.Authenticate(si.ctx, args)
	if done, err := si.srv.forward("ServiceIntention.Delete", args, args, reply); done {
		return err
	}
	si.srv.MeasureRPCRate("service_intention", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_intention", "delete"}, time.Now())

	if err := si.prepareWrite(args, args.RequestNamespace(), args.Intentions, false); err != nil {
		return err
	}

	// A missing intention is a 404, not an FSM error.
	snap, err := si.srv.State().Snapshot()
	if err != nil {
		return err
	}
	var missing []string
	for _, key := range args.Intentions {
		existing, err := snap.ServiceIntentionByKey(nil, key.Namespace, key.Destination, key.SourceNamespace, key.Source)
		if err != nil {
			return err
		}
		if existing == nil {
			missing = append(missing, key.Key())
		}
	}
	if len(missing) > 0 {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "service intention not found: %s", strings.Join(missing, ", "))
	}

	_, index, err := si.srv.raftApply(structs.ServiceIntentionDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}
