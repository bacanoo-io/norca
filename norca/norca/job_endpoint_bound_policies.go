// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// boundPolicyJobID returns the ID of the job whose bound ACL policies the
// allocations of job are granted: the parent's for a dispatched or periodic
// child, the job's own otherwise (see resolvePoliciesForClaims).
func boundPolicyJobID(job *structs.Job) string {
	if job.ParentID != "" {
		return job.ParentID
	}
	return job.ID
}

// jobBoundPolicyNames returns the sorted names of the ACL policies bound to
// the job ID in the namespace, whatever group or task they name.
func jobBoundPolicyNames(snap *state.StateSnapshot, namespace, jobID string) ([]string, error) {
	iter, err := snap.ACLPolicyByJob(nil, namespace, jobID)
	if err != nil {
		return nil, err
	}
	var names []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		p := raw.(*structs.ACLPolicy)
		if p.JobACL == nil || p.JobACL.Namespace != namespace || p.JobACL.JobID != jobID {
			continue
		}
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return names, nil
}

// heldPolicyNames returns the names of the ACL policies an identity holds.
// For an ACL token they are the policies it names and those of its roles,
// as resolveACLFromToken resolves them; for a workload identity, the
// policies bound to its job, group and task, which resolvePoliciesForClaims
// grants it. Any other identity holds none. A management token is not
// asked: it may do anything.
func (s *Server) heldPolicyNames(snap *state.StateSnapshot, identity *structs.AuthenticatedIdentity) (map[string]bool, error) {
	held := map[string]bool{}
	if identity == nil {
		return held, nil
	}
	if token := identity.GetACLToken(); token != nil {
		for _, name := range token.Policies {
			held[name] = true
		}
		for _, link := range token.Roles {
			role, err := snap.GetACLRoleByID(nil, link.ID)
			if err != nil {
				return nil, err
			}
			if role == nil {
				continue
			}
			for _, p := range role.Policies {
				held[p.Name] = true
			}
		}
		return held, nil
	}
	if claims := identity.GetClaims(); claims != nil {
		policies, err := s.resolvePoliciesForClaims(claims)
		if err != nil {
			return nil, err
		}
		for _, p := range policies {
			held[p.Name] = true
		}
	}
	return held, nil
}

// jobRunsChanged reports whether registering args.Job changes the job as it
// is stored: the comparison of Job.SpecChanged, without the fields that
// Job.Register sets or clears itself and that don't change what runs (the
// accessor of the submitting token, the Vault and Consul tokens) and with
// the task group counts Job.Register keeps (keepScaledCounts).
func jobRunsChanged(existing *structs.Job, args *structs.JobRegisterRequest) bool {
	if existing == nil {
		return true
	}
	job := args.Job.Copy()
	job.NomadTokenID = existing.NomadTokenID
	job.VaultToken, job.ConsulToken = existing.VaultToken, existing.ConsulToken
	keepScaledCounts(existing, job, args.PreserveCounts, args.ResetCounts)
	return existing.SpecChanged(job)
}

// checkJobBoundPolicies authorizes a registration of a job that has ACL
// policies bound to it (review finding JOG-2). resolvePoliciesForClaims
// grants those policies to every allocation of the job, whoever registered
// it, so a token that may submit jobs could otherwise run tasks of its own
// under them, in other namespaces too. A registration (a new job, an
// update, a revert, a release apply) that changes the job in any way needs
// a token that holds each policy bound to the job: by name, through one of
// its roles, or a management token. Not only the tasks count: meta reaches
// them through interpolation, and datacenters, node pools and constraints
// choose their nodes. A job registered first under an ID that has policies
// bound to it needs them too. Registering the job exactly as it is changes
// nothing and needs none of them.
//
// existing is the stored job, nil for a new one. aclObj is nil when ACLs
// are disabled.
func (j *Job) checkJobBoundPolicies(snap *state.StateSnapshot, aclObj *acl.ACL, args *structs.JobRegisterRequest, existing *structs.Job) error {
	denied, err := j.srv.missingBoundPolicies(snap, aclObj, args.GetIdentity(), args.Job)
	if err != nil || denied == nil {
		return err
	}
	if !jobRunsChanged(existing, args) {
		return nil
	}
	return denied
}

// boundPoliciesNotHeld returns the ID of the job whose bound ACL policies the
// allocations of job are granted (boundPolicyJobID) and the quoted names of
// those policies the identity doesn't hold. missing is empty when the
// identity holds them all, when none are bound, and when aclObj is nil (ACLs
// disabled) or a management ACL.
func (s *Server) boundPoliciesNotHeld(snap *state.StateSnapshot, aclObj *acl.ACL,
	identity *structs.AuthenticatedIdentity, job *structs.Job) (boundID string, missing []string, err error) {

	if aclObj == nil || aclObj.IsManagement() {
		return "", nil, nil
	}
	boundID = boundPolicyJobID(job)
	bound, err := jobBoundPolicyNames(snap, job.Namespace, boundID)
	if err != nil || len(bound) == 0 {
		return boundID, nil, err
	}
	held, err := s.heldPolicyNames(snap, identity)
	if err != nil {
		return boundID, nil, err
	}
	for _, name := range bound {
		if !held[name] {
			missing = append(missing, strconv.Quote(name))
		}
	}
	return boundID, missing, nil
}

// missingBoundPolicies returns as denied the 403 error of
// checkJobBoundPolicies when ACL policies are bound to the job and the
// identity doesn't hold all of them, whether or not the registration would
// change the job; denied is nil when the identity may change it. err is a
// failure to read the state.
func (s *Server) missingBoundPolicies(snap *state.StateSnapshot, aclObj *acl.ACL,
	identity *structs.AuthenticatedIdentity, job *structs.Job) (denied, err error) {

	boundID, missing, err := s.boundPoliciesNotHeld(snap, aclObj, identity, job)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	to := "it"
	if boundID != job.ID {
		to = "its parent job " + strconv.Quote(boundID)
	}
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"%s: job %q runs with the ACL policies bound to %s, so a token that changes it must hold them; the token doesn't hold %s",
		structs.ErrPermissionDenied, job.ID, to, strings.Join(missing, ", ")), nil
}

// releaseJobBoundPolicies applies the rule of checkJobBoundPolicies to a job
// of a release before the release applies anything (the release permission
// check, SEC-5): the job as Job.Register would store it, after the
// admission controllers, is compared with the stored one. It returns the
// 403 error when the caller doesn't hold the policies bound to the job and
// the job is new or would change. Job.Register checks again when the job is
// applied.
func (s *Server) releaseJobBoundPolicies(snap *state.StateSnapshot, aclObj *acl.ACL,
	args structs.RequestWithIdentity, job *structs.Job) error {

	denied, err := s.missingBoundPolicies(snap, aclObj, args.GetIdentity(), job)
	if err != nil || denied == nil {
		return err
	}
	existing, err := snap.JobByID(nil, job.Namespace, job.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return denied
	}

	// Prepare the job as Job.Plan does: the multiregion fields of this
	// region, the admission controllers, the scaling policy IDs.
	req := &structs.JobPlanRequest{
		Job:          job.Copy(),
		WriteRequest: structs.WriteRequest{Region: s.Region(), Namespace: job.Namespace},
	}
	req.SetIdentity(args.GetIdentity())
	ep := NewJobEndpoints(s, nil)
	if err := ep.interpolateMultiregionFields(req); err != nil {
		return err
	}
	prepared, _, err := ep.admissionControllers(req.Job, &admissionRequest{
		operation: structs.AdmissionOperationPlan, args: req})
	if err != nil {
		return err
	}
	if err := propagateScalingPolicyIDs(existing, prepared); err != nil {
		return err
	}
	if !jobRunsChanged(existing, &structs.JobRegisterRequest{Job: prepared}) {
		return nil
	}
	return denied
}

// steerRevertBoundPolicies authorizes a request that steers the automatic
// revert of a job (review finding JOG-17, option C): marking a version
// stable or unstable (Job.Stable), failing a deployment (Deployment.Fail),
// or setting the health of its allocations (Deployment.SetAllocHealth). For
// a job with ACL policies bound to it, such a request needs a token that
// holds them, or a management token, as a registration that changes the
// job does (checkJobBoundPolicies): the revert registers a stable version
// again without a token, and with the server option
// auto_revert_bound_policies = "any_stable" it may be a version from before
// the binding. It returns nil for a job that doesn't exist; the caller
// reports that.
func (s *Server) steerRevertBoundPolicies(snap *state.StateSnapshot, aclObj *acl.ACL,
	identity *structs.AuthenticatedIdentity, namespace, jobID string) error {

	if aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	job, err := snap.JobByID(nil, namespace, jobID)
	if err != nil || job == nil {
		return err
	}
	denied, err := s.missingBoundPolicies(snap, aclObj, identity, job)
	if err != nil {
		return err
	}
	return denied
}
