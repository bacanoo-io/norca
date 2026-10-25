// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	multierror "github.com/hashicorp/go-multierror"
)

const (
	// MultiregionOnFailureFailAll fails the deployment in every region when
	// the deployment fails in one region.
	MultiregionOnFailureFailAll = "fail_all"

	// MultiregionOnFailureFailLocal fails only the deployment of the region
	// that failed; the other regions continue and wait in the blocked state.
	MultiregionOnFailureFailLocal = "fail_local"

	// MultiregionRegistrationPeer marks the per-region registration of a
	// multiregion job that the submitting region sends to each region of the
	// job. The registration is part of a coordinated rollout: the job version
	// is set by the submitting region and the registration creates a
	// multiregion deployment that waits for its turn.
	MultiregionRegistrationPeer = "peer"

	// MultiregionRegistrationLocal registers a multiregion job only in the
	// receiving region, without sending it to the other regions and without
	// coordinating its deployment with them. Job revert uses it, because the
	// stored job of a region is already interpolated for that region.
	MultiregionRegistrationLocal = "local"
)

// Validate checks the multiregion block of a job of the given type and
// datacenters.
func (m *Multiregion) Validate(jobType string, jobDatacenters []string) error {
	if m == nil {
		return nil
	}

	var mErr multierror.Error
	if len(m.Regions) == 0 {
		mErr.Errors = append(mErr.Errors, errors.New("Multiregion block must have at least one region"))
	}

	if m.Strategy != nil {
		if m.Strategy.MaxParallel < 0 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf(
				"Multiregion max_parallel must be non-negative, got %d", m.Strategy.MaxParallel))
		}
		switch m.Strategy.OnFailure {
		case "", MultiregionOnFailureFailAll, MultiregionOnFailureFailLocal:
		default:
			mErr.Errors = append(mErr.Errors, fmt.Errorf(
				"Multiregion on_failure must be %q, %q or empty, got %q",
				MultiregionOnFailureFailAll, MultiregionOnFailureFailLocal, m.Strategy.OnFailure))
		}
	}

	seen := make(map[string]struct{}, len(m.Regions))
	for i, region := range m.Regions {
		if region == nil {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("Multiregion region %d is empty", i+1))
			continue
		}
		if region.Name == "" {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("Multiregion region %d must have a name", i+1))
		} else if _, ok := seen[region.Name]; ok {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("Multiregion region %q is listed more than once", region.Name))
		}
		seen[region.Name] = struct{}{}

		if region.Count < 0 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf(
				"Multiregion region %q count must be non-negative, got %d", region.Name, region.Count))
		}
		if len(region.Datacenters) == 0 && len(jobDatacenters) == 0 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf(
				"Multiregion region %q must have datacenters when the job has none", region.Name))
		}
		for _, dc := range region.Datacenters {
			if dc == "" {
				mErr.Errors = append(mErr.Errors, fmt.Errorf(
					"Multiregion region %q datacenter must be non-empty string", region.Name))
			}
		}
	}

	return mErr.ErrorOrNil()
}

// Region returns the region of the multiregion block with the given name, or
// nil if the block does not list it.
func (m *Multiregion) Region(name string) *MultiregionRegion {
	if m == nil {
		return nil
	}
	for _, region := range m.Regions {
		if region != nil && region.Name == name {
			return region
		}
	}
	return nil
}

// RegionNames returns the names of the regions of the multiregion block in
// their rollout order.
func (m *Multiregion) RegionNames() []string {
	if m == nil {
		return nil
	}
	names := make([]string, 0, len(m.Regions))
	for _, region := range m.Regions {
		if region != nil {
			names = append(names, region.Name)
		}
	}
	return names
}

// MaxParallel returns the number of regions that deploy at the same time:
// max_parallel, or all regions when it is not set.
func (m *Multiregion) MaxParallel() int {
	n := len(m.Regions)
	if m.Strategy == nil || m.Strategy.MaxParallel <= 0 || m.Strategy.MaxParallel > n {
		return n
	}
	return m.Strategy.MaxParallel
}

// OnFailure returns the on_failure strategy of the multiregion block.
func (m *Multiregion) OnFailure() string {
	if m == nil || m.Strategy == nil {
		return ""
	}
	return m.Strategy.OnFailure
}

// InterpolateMultiregion returns a copy of a multiregion job with the fields
// of the given region filled in: the job region, the datacenters and the node
// pool of the region when it sets them, the region meta merged over the job
// meta, and the region count for every task group with a count of zero.
//
// Interpolating a job that is already interpolated for the same region
// returns an equal job, so a stored job can be interpolated again.
func (j *Job) InterpolateMultiregion(region string) (*Job, error) {
	r := j.Multiregion.Region(region)
	if r == nil {
		return nil, fmt.Errorf("region %q is not one of the regions of multiregion job %q", region, j.ID)
	}

	job := j.Copy()
	job.Region = region
	if len(r.Datacenters) > 0 {
		job.Datacenters = slices.Clone(r.Datacenters)
	}
	if r.NodePool != "" {
		job.NodePool = r.NodePool
	}
	if len(r.Meta) > 0 {
		if job.Meta == nil {
			job.Meta = make(map[string]string, len(r.Meta))
		}
		maps.Copy(job.Meta, r.Meta)
	}
	for _, tg := range job.TaskGroups {
		if tg.Count == 0 {
			tg.Count = r.Count
		}
	}
	return job, nil
}

// UsesMultiregionDeployments returns whether the registration of a multiregion
// job creates a coordinated multiregion deployment: the job uses deployments
// and at least one of its task groups has an update strategy, so that the
// deployment can complete.
func (j *Job) UsesMultiregionDeployments() bool {
	if !j.IsMultiregion() || !j.UsesDeployments() || j.IsPeriodic() || j.IsParameterized() {
		return false
	}
	for _, tg := range j.TaskGroups {
		if !tg.Update.IsEmpty() {
			return true
		}
	}
	return false
}

// NewMultiregionDeployment creates the deployment of a multiregion job that
// Job.Register stores with the job. It starts initializing and waits, once the
// scheduler has computed it, in the pending state until its region's turn in
// the multiregion rollout.
func NewMultiregionDeployment(job *Job, evalPriority int) *Deployment {
	d := NewDeployment(job, evalPriority)
	d.IsMultiregion = true
	d.Status = DeploymentStatusInitializing
	d.StatusDescription = DeploymentStatusDescriptionPendingForPeer
	return d
}
