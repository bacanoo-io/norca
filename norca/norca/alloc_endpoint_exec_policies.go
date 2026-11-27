// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-metrics"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// CheckExecBoundPolicies answers for a client whether the caller may exec
// into the allocation as far as the ACL policies bound to its job go
// (review finding JOG-14): nil when it may, a 403 error that names the
// policies the caller doesn't hold when it may not. The client calls it with
// the caller's token after its own alloc-exec and alloc-node-exec checks,
// before it starts the exec session (client.Allocations.execImpl); the caller
// needs alloc-exec in the allocation's namespace here too, so that only the
// one who could exec learns the names.
func (a *Alloc) CheckExecBoundPolicies(args *structs.AllocSpecificRequest, reply *structs.GenericResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.CheckExecBoundPolicies", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"server", "alloc", "check_exec_bound_policies"}, time.Now())

	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	snap, err := a.srv.State().Snapshot()
	if err != nil {
		return err
	}
	alloc, err := getAlloc(snap, args.AllocID)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.AllowNsOp(alloc.Namespace, acl.NamespaceCapabilityAllocExec) {
		return structs.ErrPermissionDenied
	}
	denied, err := a.srv.execBoundPoliciesDenied(snap, aclObj, args.GetIdentity(), alloc)
	if err != nil {
		return err
	}
	if denied != nil {
		return structs.NewErrRPCCoded(http.StatusForbidden, denied.Error())
	}
	return nil
}

// execBoundPoliciesDenied applies the rule of checkJobBoundPolicies to an
// exec into the allocation: a command run in a task sees the task's
// environment and secrets directory, and so its workload identity token and
// the ACL policies bound to the job (resolvePoliciesForClaims). When ACL
// policies are bound to the allocation's job (its parent's for a child job),
// the identity must hold each of them, by name or through a role, or be a
// management token. denied is the "Permission denied" error that names the
// policies it doesn't hold, nil when it may exec; err is a failure to read
// the state. aclObj is nil when ACLs are disabled.
func (s *Server) execBoundPoliciesDenied(snap *state.StateSnapshot, aclObj *acl.ACL,
	identity *structs.AuthenticatedIdentity, alloc *structs.Allocation) (denied, err error) {

	if aclObj == nil || aclObj.IsManagement() {
		return nil, nil
	}
	if alloc.Job == nil {
		// resolvePoliciesForClaims fails the same way: without the job the
		// bound policies can't be known, so refuse.
		return nil, fmt.Errorf("allocation %q has no job to check its bound ACL policies", alloc.ID)
	}
	boundID, missing, err := s.boundPoliciesNotHeld(snap, aclObj, identity, alloc.Job)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	to := "its job " + strconv.Quote(boundID)
	if boundID != alloc.Job.ID {
		to = fmt.Sprintf("the parent job %q of its job %q", boundID, alloc.Job.ID)
	}
	return fmt.Errorf("%w: allocation %q runs with the ACL policies bound to %s, and a command run in it can use them, so exec into it needs a token that holds them; the token doesn't hold %s",
		structs.ErrPermissionDenied, alloc.ID, to, strings.Join(missing, ", ")), nil
}
