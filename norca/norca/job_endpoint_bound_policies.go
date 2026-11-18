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
	if aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	job := args.Job
	boundID := boundPolicyJobID(job)
	bound, err := jobBoundPolicyNames(snap, job.Namespace, boundID)
	if err != nil || len(bound) == 0 {
		return err
	}
	if !jobRunsChanged(existing, args) {
		return nil
	}
	held, err := j.srv.heldPolicyNames(snap, args.GetIdentity())
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range bound {
		if !held[name] {
			missing = append(missing, strconv.Quote(name))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	to := "it"
	if boundID != job.ID {
		to = "its parent job " + strconv.Quote(boundID)
	}
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"%s: job %q runs with the ACL policies bound to %s, so a token that changes it must hold them; the token doesn't hold %s",
		structs.ErrPermissionDenied, job.ID, to, strings.Join(missing, ", "))
}
