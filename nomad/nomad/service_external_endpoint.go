// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// errExternalServicesDisabled is returned while not every server of the
// region supports external services.
var errExternalServicesDisabled = errors.New("all servers of the region must run Norca to use external services")

// externalServicesEnabled returns an error unless every server of the region
// supports external services.
func (s *ServiceRegistration) externalServicesEnabled() error {
	if !ServersMeetMinimumNorcaVersion(s.srv.Members(), s.srv.Region(), false) {
		return errExternalServicesDisabled
	}
	if s.srv.externalServices == nil {
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable, "server is starting")
	}
	return nil
}

// resolveExternalACL resolves the ACL of a request writing external services.
// Node identities cannot write them with ACLs enabled (ResolveACL denies them
// too; the explicit check keeps the intent visible).
func (s *ServiceRegistration) resolveExternalACL(args structs.RequestWithIdentity) (*acl.ACL, error) {
	if identity := args.GetIdentity(); identity != nil && identity.ClientID != "" && s.srv.config.ACLEnabled {
		return nil, structs.ErrPermissionDenied
	}
	return s.srv.ResolveACL(args)
}

// UpsertExternal registers or updates an external service. The caller needs
// the register-external-service capability on the namespace; registrations
// without TTL checks that no client node owns (static registrations) need
// operator write as well.
func (s *ServiceRegistration) UpsertExternal(
	args *structs.ExternalServiceUpsertRequest,
	reply *structs.ExternalServiceUpsertResponse) error {

	authErr := s.srv.Authenticate(s.ctx, args)
	if done, err := s.srv.forward(structs.ServiceRegistrationUpsertExternalRPCMethod, args, args, reply); done {
		return err
	}
	s.srv.MeasureRPCRate("service_registration", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_registration", "upsert_external"}, time.Now())

	if err := s.externalServicesEnabled(); err != nil {
		return err
	}

	aclObj, err := s.resolveExternalACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityRegisterExternalService) {
		return structs.ErrPermissionDenied
	}

	reg := args.Service
	if reg == nil || reg.External == nil {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "missing external service")
	}
	reg.Namespace = args.RequestNamespace()
	reg.NodeID = ""

	// Bind the registration to the client node it was registered through.
	if args.NodeSecretID != "" {
		node, err := s.srv.fsm.State().NodeBySecretID(nil, args.NodeSecretID)
		if err != nil {
			return err
		}
		if node == nil {
			return structs.ErrPermissionDenied
		}
		reg.NodeID = node.ID
		if reg.Datacenter == "" {
			reg.Datacenter = node.Datacenter
		}
	} else if args.NodeID != "" {
		// Bound by the caller to a node of its choice (e.g. from the UI):
		// that node runs the http and tcp checks.
		if aclObj != nil && !aclObj.AllowNodeWrite() {
			return structs.NewErrRPCCoded(http.StatusForbidden,
				"Permission denied: binding an external service to a node requires node write")
		}
		node, err := s.srv.fsm.State().NodeByID(nil, args.NodeID)
		if err != nil {
			return err
		}
		if node == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "node %q not found", args.NodeID)
		}
		reg.NodeID = node.ID
		if reg.Datacenter == "" {
			reg.Datacenter = node.Datacenter
		}
	}

	for _, check := range reg.External.Checks {
		if check != nil {
			check.Canonicalize()
		}
	}
	if err := reg.ValidateExternal(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid external service: %v", err)
	}
	if reg.External.HasNodeChecks() && reg.NodeID == "" {
		return structs.NewErrRPCCoded(http.StatusBadRequest,
			"http and tcp checks are run by a client node: register the service through a client agent")
	}

	// A registration without TTL checks that no node owns never expires.
	if reg.NodeID == "" && !reg.External.HasTTLChecks() && aclObj != nil && !aclObj.AllowOperatorWrite() {
		return structs.NewErrRPCCoded(http.StatusForbidden,
			"Permission denied: external services without TTL checks require operator write")
	}

	index, err := s.srv.externalServices.upsert(reg)
	if err != nil {
		return err
	}
	reply.ID = reg.ID
	reply.Index = index
	return nil
}

// DeleteExternal deregisters an external service.
func (s *ServiceRegistration) DeleteExternal(
	args *structs.ExternalServiceDeleteRequest,
	reply *structs.ExternalServiceDeleteResponse) error {

	authErr := s.srv.Authenticate(s.ctx, args)
	if done, err := s.srv.forward(structs.ServiceRegistrationDeleteExternalRPCMethod, args, args, reply); done {
		return err
	}
	s.srv.MeasureRPCRate("service_registration", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_registration", "delete_external"}, time.Now())

	if err := s.externalServicesEnabled(); err != nil {
		return err
	}

	aclObj, err := s.resolveExternalACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityRegisterExternalService) {
		return structs.ErrPermissionDenied
	}

	index, err := s.srv.externalServices.delete(args.RequestNamespace(), args.ID)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// UpdateExternalChecks updates the status of checks of an external service:
// TTL renewals by the registrant (register-external-service capability), or
// the results of http and tcp checks reported by the client node owning the
// registration (node secret).
func (s *ServiceRegistration) UpdateExternalChecks(
	args *structs.ExternalServiceCheckUpdateRequest,
	reply *structs.ExternalServiceCheckUpdateResponse) error {

	authErr := s.srv.Authenticate(s.ctx, args)
	if done, err := s.srv.forward(structs.ServiceRegistrationUpdateExternalChecksRPCMethod, args, args, reply); done {
		return err
	}
	s.srv.MeasureRPCRate("service_registration", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_registration", "update_external_checks"}, time.Now())

	if err := s.externalServicesEnabled(); err != nil {
		return err
	}

	var nodeID string
	if args.NodeReport {
		node, err := s.srv.fsm.State().NodeBySecretID(nil, args.AuthToken)
		if err != nil {
			return err
		}
		if node == nil {
			return structs.ErrPermissionDenied
		}
		nodeID = node.ID
	} else {
		aclObj, err := s.resolveExternalACL(args)
		if err != nil {
			return err
		}
		if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityRegisterExternalService) {
			return structs.ErrPermissionDenied
		}
	}

	index, err := s.srv.externalServices.updateChecks(args.RequestNamespace(), args.ID, args.Updates, nodeID)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// ListExternal lists external services: those of a namespace (or all
// namespaces with "*") the caller may read, or those owned by a client node.
// A client node may list its own registrations in every namespace.
func (s *ServiceRegistration) ListExternal(
	args *structs.ExternalServiceListRequest,
	reply *structs.ExternalServiceListResponse) error {

	authErr := s.srv.Authenticate(s.ctx, args)
	if done, err := s.srv.forward(structs.ServiceRegistrationListExternalRPCMethod, args, args, reply); done {
		return err
	}
	s.srv.MeasureRPCRate("service_registration", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "service_registration", "list_external"}, time.Now())

	if err := s.externalServicesEnabled(); err != nil {
		return err
	}

	namespace := args.RequestNamespace()
	var allowNS func(string) bool
	if identity := args.GetIdentity(); identity != nil && identity.ClientID != "" {
		if args.NodeID != identity.ClientID {
			return structs.ErrPermissionDenied
		}
		allowNS = func(string) bool { return true }
	} else {
		aclObj, err := s.srv.ResolveACL(args)
		if err != nil {
			return err
		}
		allowNS = func(ns string) bool { return aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadJob) }
		if namespace != structs.AllNamespacesSentinel && !allowNS(namespace) {
			return structs.ErrPermissionDenied
		}
	}

	return s.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, stateStore *state.StateStore) error {
			var regs []*structs.ServiceRegistration
			if args.NodeID != "" {
				// watch the registrations of the node only
				byNode, err := stateStore.GetServiceRegistrationsByNodeID(ws, args.NodeID)
				if err != nil {
					return err
				}
				regs = byNode
			} else {
				var iter memdb.ResultIterator
				var err error
				if namespace == structs.AllNamespacesSentinel {
					iter, err = stateStore.GetServiceRegistrations(ws)
				} else {
					iter, err = stateStore.GetServiceRegistrationsByNamespace(ws, namespace)
				}
				if err != nil {
					return err
				}
				for raw := iter.Next(); raw != nil; raw = iter.Next() {
					regs = append(regs, raw.(*structs.ServiceRegistration))
				}
			}

			out := make([]*structs.ServiceRegistration, 0)
			for _, reg := range regs {
				if !reg.IsExternal() || !allowNS(reg.Namespace) {
					continue
				}
				if namespace != structs.AllNamespacesSentinel && reg.Namespace != namespace {
					continue
				}
				out = append(out, reg)
			}
			sort.Slice(out, func(i, j int) bool {
				if out[i].Namespace != out[j].Namespace {
					return out[i].Namespace < out[j].Namespace
				}
				return out[i].ID < out[j].ID
			})
			reply.Services = out
			return s.srv.setReplyQueryMeta(stateStore, state.TableServiceRegistrations, &reply.QueryMeta)
		},
	})
}

// errExternalNotFound is returned for unknown external services.
func errExternalNotFound(namespace, id string) error {
	return structs.NewErrRPCCodedf(http.StatusNotFound, "external service %q not found in namespace %q", id, namespace)
}

// errNotExternal is returned for registrations that are not external.
func errNotExternal(id string) error {
	return structs.NewErrRPCCoded(http.StatusBadRequest,
		fmt.Sprintf("service registration %q is not an external service", id))
}
