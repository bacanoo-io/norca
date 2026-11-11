// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/hashicorp/go-multierror"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Job checks on the server that receives a request (clean room, design
// raft-observers-and-hot-standby.md, phase 4): a follower or an observer
// rejects a job that can't be valid before forwarding it, and runs a dry run
// of the placements on its own copy of the state. The leader repeats every
// check; these only answer earlier and take work off the leader.

// receivesForLeader reports whether this server is the first to see a
// request of its own region and forwards it to the leader.
func (s *Server) receivesForLeader(args structs.RPCInfo) bool {
	return !args.IsForwarded() && args.RequestRegion() == s.config.Region && !s.IsLeader()
}

// edgeValidateRegister runs the static checks of a job registration on a
// copy of the job: the mutators without the admission webhooks, then the job
// validation (Job.Validate, payload, type and priority). It returns the error
// the leader would return for an invalid job, and nil when the job passes or
// can't be checked here; the checks that read the state of the region
// (quotas, node pools, namespaces, secrets), the admission policies and the
// webhooks stay with the leader, which publishes their denials.
func (j *Job) edgeValidateRegister(args *structs.JobRegisterRequest) error {
	job := args.Job
	if job == nil || args.RequestNamespace() != job.Namespace || job.Multiregion != nil {
		return nil
	}
	// A mutating webhook may complete the job: only the leader can tell.
	if d := j.srv.admission; d != nil && len(d.webhooksFor(job.Namespace, true)) > 0 {
		return nil
	}

	copied := *args
	copied.Job = job.Copy()
	areq := &admissionRequest{operation: structs.AdmissionOperationRegister, args: &copied}
	out := copied.Job
	for _, mutator := range j.mutators {
		if _, ok := mutator.(admissionWebhookMutator); ok {
			continue
		}
		var err error
		if rm, ok := mutator.(jobRequestMutator); ok {
			out, _, err = rm.MutateWithRequest(areq, out)
		} else {
			out, _, err = mutator.Mutate(out)
		}
		if err != nil {
			// The state of this server may be behind: the leader decides.
			j.logger.Trace("edge check skipped", "job", job.ID, "mutator", mutator.Name(), "error", err)
			return nil
		}
	}

	if _, err := (&jobValidate{srv: j.srv}).Validate(out.Copy()); err != nil {
		j.logger.Debug("job rejected before the leader", "job", job.ID, "namespace", job.Namespace)
		return multierror.Append(nil, err)
	}
	return nil
}

// capacityCheck runs the plan of a job registration on this server's state
// and returns the task groups that would not be placed now.
func (j *Job) capacityCheck(args *structs.JobRegisterRequest) *structs.JobCapacityCheck {
	check := &structs.JobCapacityCheck{Server: j.srv.config.NodeName}
	if args.Job == nil {
		return nil
	}
	planArgs := &structs.JobPlanRequest{
		Job:            args.Job.Copy(),
		PolicyOverride: args.PolicyOverride,
		WriteRequest:   args.WriteRequest,
	}
	var reply structs.JobPlanResponse
	if err := j.plan(planArgs, &reply); err != nil {
		check.Error = err.Error()
		return check
	}
	check.FailedTGAllocs = reply.FailedTGAllocs
	check.Index, _ = j.srv.State().LatestIndex()
	return check
}
