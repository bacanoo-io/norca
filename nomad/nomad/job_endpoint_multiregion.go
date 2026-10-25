// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"slices"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/structs"
)

// A job with a multiregion block is registered in every region it lists. The
// region that receives the submission (the submitting region) interpolates the
// region-specific fields of the job for each region and registers the result
// there with Job.Register, marked as a peer registration, so that the receiving
// region does not send it on. Every region stores the same job version and,
// for jobs that use deployments, a multiregion deployment that waits in the
// pending state. The deployment watchers of the regions then coordinate the
// rollout over the cross-region RPC: see nomad/deploymentwatcher/multiregion.go.

// multiregionRegistration carries the state of the registration of a
// multiregion job through Job.Register.
type multiregionRegistration struct {
	// mode is the MultiregionRegistration of the request.
	mode string

	// template is the job as submitted, before interpolation, which the
	// submitting region interpolates for each of the other regions. It is nil
	// for peer and local registrations.
	template *structs.Job

	// changed is set by the submitting region when the job changed in at least
	// one region, so a new version is registered in all of them.
	changed bool

	// dropped are the regions that a previous version of the job listed but
	// the submitted one does not.
	dropped []string

	// warnings are the registration warnings of the other regions.
	warnings []error
}

// submitter returns whether this region is the submitting region of the
// multiregion job.
func (mr *multiregionRegistration) submitter() bool {
	return mr != nil && mr.template != nil
}

// multiregionRequestRegion routes the submission of a multiregion job that is
// sent to a region the job does not list to the first region it lists, so that
// the submitting region is one of the regions of the job.
func multiregionRequestRegion(args *structs.JobRegisterRequest) {
	if args.Job == nil || !args.Job.IsMultiregion() || args.MultiregionRegistration != "" {
		return
	}
	if args.Job.Multiregion.Region(args.Region) == nil {
		args.Region = args.Job.Multiregion.Regions[0].Name
	}
}

// multiregionPrepare interpolates a multiregion job for this region before the
// admission controllers run, so that they see the job this region stores. For a
// submission, it keeps the submitted job as the template for the other
// regions. It returns nil for a job without regions.
func (j *Job) multiregionPrepare(args *structs.JobRegisterRequest) (*multiregionRegistration, error) {
	if !args.Job.IsMultiregion() {
		return nil, nil
	}

	mr := &multiregionRegistration{mode: args.MultiregionRegistration}
	switch mr.mode {
	case "":
		mr.template = args.Job
	case structs.MultiregionRegistrationPeer, structs.MultiregionRegistrationLocal:
	default:
		return nil, fmt.Errorf("invalid multiregion registration %q", mr.mode)
	}

	job, err := args.Job.InterpolateMultiregion(j.srv.Region())
	if err != nil {
		return nil, err
	}
	args.Job = job
	return mr, nil
}

// multiregionRegister sends a submitted multiregion job to the other regions
// of the job. It runs after the job passed admission in this region and before
// it is registered here. When the job changed in at least one region, it
// registers the job with the same new version in every other region, and sets
// that version on the job of this region. A peer registration only checks that
// the version set by the submitting region can be stored.
func (j *Job) multiregionRegister(mr *multiregionRegistration, args *structs.JobRegisterRequest,
	reply *structs.JobRegisterResponse, existingJob *structs.Job, token *structs.ACLToken) error {

	if mr == nil {
		return nil
	}

	if mr.mode == structs.MultiregionRegistrationPeer {
		if existingJob != nil && existingJob.Version >= args.Job.Version && existingJob.SpecChanged(args.Job) {
			return fmt.Errorf("multiregion job %q has version %d in region %q, which is not older than the submitted version %d; submit the job again",
				args.Job.ID, existingJob.Version, j.srv.Region(), args.Job.Version)
		}
		return nil
	}
	if !mr.submitter() {
		return nil
	}

	regions := args.Job.Multiregion.RegionNames()
	if err := j.multiregionCheckRegions(regions); err != nil {
		return err
	}

	// The registration in the other regions and the deployment watchers
	// coordinating the rollout carry the token of the submitter, so it must be
	// valid in every region.
	if token != nil && token != structs.AnonymousACLToken && !token.Global {
		return errors.New("a multiregion job must be submitted with a global ACL token, which is valid in all regions")
	}

	// Look up the job in the other regions to find whether it changed and the
	// version to register.
	peers := make(map[string]*structs.Job, len(regions))
	for _, region := range regions {
		if region == j.srv.Region() {
			continue
		}
		job, err := j.multiregionGetJob(region, args.Job.ID, args)
		if err != nil {
			return fmt.Errorf("failed to look up job %q in region %q: %w", args.Job.ID, region, err)
		}
		peers[region] = job
	}

	var version uint64
	versioned := existingJob != nil
	if existingJob != nil {
		version = existingJob.Version
	}
	mr.changed = existingJob == nil || existingJob.SpecChanged(args.Job)
	for _, job := range peers {
		if job == nil {
			mr.changed = true
			continue
		}
		// A region that is at a different version was changed on its own,
		// for example by a job revert or scale in that region.
		if existingJob == nil || job.Stop || job.Version != existingJob.Version {
			mr.changed = true
		}
		if !versioned || job.Version > version {
			version = job.Version
		}
		versioned = true
	}
	if !mr.changed {
		return nil
	}
	if versioned {
		version++
	}
	args.Job.Version = version

	// Regions listed by the previous versions of the job but no longer by
	// the submitted one.
	previous := []*structs.Job{existingJob}
	for _, job := range peers {
		previous = append(previous, job)
	}
	for _, job := range previous {
		if job == nil {
			continue
		}
		for _, region := range job.Multiregion.RegionNames() {
			if !slices.Contains(regions, region) && !slices.Contains(mr.dropped, region) {
				mr.dropped = append(mr.dropped, region)
			}
		}
	}

	// Register the job in the other regions in their rollout order.
	for _, region := range regions {
		if region == j.srv.Region() {
			continue
		}
		job, err := mr.template.InterpolateMultiregion(region)
		if err != nil {
			return err
		}
		job.Version = version
		req := &structs.JobRegisterRequest{
			Submission:              args.Submission,
			Job:                     job,
			PreserveCounts:          args.PreserveCounts,
			PolicyOverride:          args.PolicyOverride,
			EvalPriority:            args.EvalPriority,
			MultiregionRegistration: structs.MultiregionRegistrationPeer,
			WriteRequest: structs.WriteRequest{
				Region:    region,
				Namespace: args.RequestNamespace(),
				AuthToken: args.AuthToken,
			},
		}
		var resp structs.JobRegisterResponse
		if err := j.srv.forwardRegion(region, "Job.Register", req, &resp); err != nil {
			return fmt.Errorf("failed to register job %q in region %q: %w", job.ID, region, err)
		}
		if resp.Warnings != "" {
			mr.warnings = append(mr.warnings, fmt.Errorf("region %q: %s", region, resp.Warnings))
		}
	}
	return nil
}

// multiregionCheckRegions checks that every region of a multiregion job is
// known and that all its servers support multiregion deployments.
func (j *Job) multiregionCheckRegions(regions []string) error {
	known := j.srv.Regions()
	members := j.srv.Members()
	for _, region := range regions {
		if !slices.Contains(known, region) {
			return fmt.Errorf("multiregion job region %q is not a known region", region)
		}
		if !ServersMeetMinimumNorcaVersion(members, region, false) {
			return fmt.Errorf("all servers in region %q must run Norca %s or later for multiregion jobs",
				region, minNorcaFeaturesVersion)
		}
	}
	return nil
}

// multiregionGetJob looks up a job in another region with the token of the
// request.
func (j *Job) multiregionGetJob(region, jobID string, args *structs.JobRegisterRequest) (*structs.Job, error) {
	req := &structs.JobSpecificRequest{
		JobID: jobID,
		QueryOptions: structs.QueryOptions{
			Region:    region,
			Namespace: args.RequestNamespace(),
			AuthToken: args.AuthToken,
		},
	}
	var resp structs.SingleJobResponse
	if err := j.srv.forwardRegion(region, "Job.GetJob", req, &resp); err != nil {
		return nil, err
	}
	return resp.Job, nil
}

// multiregionSpecChanged returns whether the job must be registered in this
// region. For a submission it is whether the job changed in any region, since
// the regions of a multiregion job are registered with the same version. A peer
// registration is stored when its version is newer, even if the job of this
// region did not change.
func (j *Job) multiregionSpecChanged(mr *multiregionRegistration, existingJob *structs.Job, args *structs.JobRegisterRequest) bool {
	switch {
	case mr.submitter():
		return mr.changed
	case mr != nil && mr.mode == structs.MultiregionRegistrationPeer:
		return existingJob == nil || existingJob.Version < args.Job.Version || existingJob.SpecChanged(args.Job)
	default:
		return existingJob.SpecChanged(args.Job)
	}
}

// multiregionCreateDeployment creates the multiregion deployment to register
// along with the job of a multiregion rollout. It waits in the pending state
// until the deployment watcher starts it in its turn.
func (j *Job) multiregionCreateDeployment(mr *multiregionRegistration, job *structs.Job, eval *structs.Evaluation) *structs.Deployment {
	if mr == nil || mr.mode == structs.MultiregionRegistrationLocal || eval == nil ||
		!job.UsesMultiregionDeployments() {
		return nil
	}
	return structs.NewMultiregionDeployment(job, eval.Priority)
}

// multiregionDrop stops the job in the regions that a previous version of the
// multiregion job listed but the registered one does not. Failures are
// returned as warnings, since the job is already registered.
func (j *Job) multiregionDrop(mr *multiregionRegistration, args *structs.JobRegisterRequest, reply *structs.JobRegisterResponse) {
	if !mr.submitter() || !mr.changed || len(mr.dropped) == 0 {
		return
	}

	var warnings []error
	for _, region := range mr.dropped {
		job, err := j.multiregionGetJob(region, args.Job.ID, args)
		if err == nil && (job == nil || !job.IsMultiregion() || job.Stop) {
			continue
		}
		if err == nil {
			req := &structs.JobDeregisterRequest{
				JobID: args.Job.ID,
				WriteRequest: structs.WriteRequest{
					Region:    region,
					Namespace: args.RequestNamespace(),
					AuthToken: args.AuthToken,
				},
			}
			var resp structs.JobDeregisterResponse
			err = j.srv.forwardRegion(region, "Job.Deregister", req, &resp)
		}
		if err != nil {
			j.logger.Warn("failed to stop multiregion job in dropped region", "job", args.Job.ID, "region", region, "error", err)
			warnings = append(warnings, fmt.Errorf("failed to stop job in region %q, which the job no longer lists: %v", region, err))
		}
	}

	if len(warnings) > 0 {
		if reply.Warnings != "" {
			warnings = append([]error{errors.New(reply.Warnings)}, warnings...)
		}
		reply.Warnings = multierror.Append(nil, warnings...).Error()
	}
}

// multiregionStop sends Job.Deregister to the other regions of a multiregion
// job when the global flag is passed to Job.Deregister.
func (j *Job) multiregionStop(job *structs.Job, args *structs.JobDeregisterRequest, reply *structs.JobDeregisterResponse) error {
	if !args.Global || !job.IsMultiregion() {
		return nil
	}

	var mErr *multierror.Error
	for _, region := range job.Multiregion.RegionNames() {
		if region == j.srv.Region() {
			continue
		}
		req := &structs.JobDeregisterRequest{
			JobID:           args.JobID,
			Purge:           args.Purge,
			EvalPriority:    args.EvalPriority,
			NoShutdownDelay: args.NoShutdownDelay,
			WriteRequest: structs.WriteRequest{
				Region:    region,
				Namespace: args.RequestNamespace(),
				AuthToken: args.AuthToken,
			},
		}
		var resp structs.JobDeregisterResponse
		if err := j.srv.forwardRegion(region, "Job.Deregister", req, &resp); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("failed to stop job in region %q: %w", region, err))
		}
	}
	return mErr.ErrorOrNil()
}

// interpolateMultiregionFields interpolates a multiregion job for this region,
// so that it is planned as this region would register it.
func (j *Job) interpolateMultiregionFields(args *structs.JobPlanRequest) error {
	if !args.Job.IsMultiregion() {
		return nil
	}
	job, err := args.Job.InterpolateMultiregion(j.srv.Region())
	if err != nil {
		return err
	}
	args.Job = job
	return nil
}
