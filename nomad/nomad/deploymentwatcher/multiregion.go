// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// SPDX-FileCopyrightText: 2026 Norca contributors

package deploymentwatcher

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
)

// A multiregion deployment is the deployment of one region of a multiregion
// job. Job.Register stores one in every region of the job, for the same job
// version, and the scheduler moves it from initializing to pending. The
// deployment watchers of the regions then coordinate the rollout over the
// cross-region RPC, deriving every step from the deployments stored in the
// Raft state of the regions, so that a new leader continues where the old one
// stopped and repeating a step has no effect:
//
//   - A pending deployment starts (pending -> running) when it is one of the
//     first max_parallel regions and every region has a deployment for the
//     job version, or when the region max_parallel places before it is done.
//   - A running deployment that completes its rollout is blocked by the
//     scheduler. Its watcher then runs the pending deployment of the region
//     max_parallel places after it (nextRegion), so at most max_parallel
//     regions are running at a time.
//   - When the deployments of all regions are blocked (or successful), each
//     blocked region is unblocked and becomes successful.
//   - When a deployment fails, its watcher fails the deployments of the other
//     regions according to on_failure: all of them for "fail_all", the ones
//     after it for the default, none for "fail_local".
//
// The pending and blocked deployments poll their peer regions with a backoff,
// so a step that a peer could not take, for example because of a network
// partition, is taken once the regions can reach each other again. An operator
// can take any step by hand with Deployment.Run, Unblock or Fail.
//
// The cross-region calls carry the ACL token that submitted the job, which is
// therefore a global token.

// DeploymentRPC holds the deployment RPC methods used to coordinate a
// multiregion deployment with the peer regions.
type DeploymentRPC interface {
	Run(args *structs.DeploymentRunRequest, reply *structs.DeploymentUpdateResponse) error
	Unblock(args *structs.DeploymentUnblockRequest, reply *structs.DeploymentUpdateResponse) error
	Fail(args *structs.DeploymentFailRequest, reply *structs.DeploymentUpdateResponse) error
}

// JobRPC holds the job RPC methods used to coordinate a multiregion deployment
// with the peer regions.
type JobRPC interface {
	LatestDeployment(args *structs.JobSpecificRequest, reply *structs.SingleDeploymentResponse) error
}

const (
	// defaultMultiregionPollMin and defaultMultiregionPollMax bound the
	// backoff of a pending or blocked multiregion deployment polling its peer
	// regions.
	defaultMultiregionPollMin = 1 * time.Second
	defaultMultiregionPollMax = 30 * time.Second

	// multiregionPushAttempts is the number of attempts of a best-effort
	// cross-region call made when a deployment fails.
	multiregionPushAttempts = 3
)

// multiregionPoll is the polling backoff of multiregion deployments.
type multiregionPoll struct {
	min, max time.Duration
}

// SetMultiregionPollInterval sets the bounds of the backoff with which pending
// and blocked multiregion deployments poll their peer regions. It applies to
// the deployments watched from then on.
func (w *Watcher) SetMultiregionPollInterval(min, max time.Duration) {
	w.l.Lock()
	defer w.l.Unlock()
	w.multiregionPoll = multiregionPoll{min: min, max: max}
}

// multiregionRollout is the view of a multiregion deployment's rollout from
// its own region.
type multiregionRollout struct {
	// job is the job at the version of the deployment
	job *structs.Job

	// regions are the regions of the job in rollout order and self is the
	// index of this region
	regions []string
	self    int

	maxParallel int
	onFailure   string

	// token is the secret ID of the ACL token that submitted the job, used for
	// the calls to the peer regions
	token string
}

// regionDeployment is the deployment of the rollout's job version in a region.
type regionDeployment struct {
	region string

	// d is the deployment, nil when the region has none for the job version
	// (yet)
	d *structs.Deployment

	// newer is set when the region has a deployment for a newer job version,
	// so this rollout is superseded
	newer bool

	// err is set when the region could not be queried
	err error
}

// status returns the status of the region's deployment, or the empty string
// when it is unknown or there is none.
func (r regionDeployment) status() string {
	if r.err != nil || r.d == nil {
		return ""
	}
	return r.d.Status
}

// multiregionEnabled returns whether the watched deployment is a multiregion
// deployment that is coordinated with its peer regions.
func (w *deploymentWatcher) multiregionEnabled() bool {
	return w.getDeployment().IsMultiregion && w.deploymentRPC != nil && w.jobRPC != nil
}

// nextRegion is called by the watch loop with the status of the deployment when
// it changes, and with the failed status before the watch loop fails it.
func (w *deploymentWatcher) nextRegion(status string) {
	if !w.multiregionEnabled() {
		return
	}

	if status == structs.DeploymentStatusFailed {
		// The watcher stops once the deployment failed, so the peer regions
		// are notified in the background of the deployments watcher.
		go w.multiregionFailed(w.getDeployment())
		return
	}

	select {
	case w.multiregionCh <- struct{}{}:
	default:
	}
}

// watchMultiregion is the long running loop that takes the steps of a
// multiregion rollout. It runs a step whenever the deployment changes and, while
// the deployment waits as pending or blocked, polls the peer regions with a
// backoff.
func (w *deploymentWatcher) watchMultiregion() {
	timer, stop := helper.NewStoppedTimer()
	defer stop()

	wait := w.multiregionPoll.min
	for {
		if w.multiregionStep() {
			timer.Reset(wait)
			wait = min(2*wait, w.multiregionPoll.max)
		}

		select {
		case <-w.ctx.Done():
			return
		case <-w.multiregionCh:
			wait = w.multiregionPoll.min
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// multiregionStep takes the next step of the rollout from this region, if any.
// It returns whether the deployment waits for its peer regions and should poll
// them again.
func (w *deploymentWatcher) multiregionStep() bool {
	d := w.getDeployment()
	switch d.Status {
	case structs.DeploymentStatusPending, structs.DeploymentStatusBlocked, structs.DeploymentStatusUnblocking:
	default:
		// An initializing deployment waits for the scheduler, a running or
		// paused one for its own rollout, which the watch loop follows.
		return false
	}

	ro, err := w.multiregionRollout(d)
	if err != nil {
		w.logger.Error("failed to coordinate multiregion deployment", "error", err)
		return true
	}

	regions := w.regionDeployments(ro, d)
	for _, r := range regions {
		if r.newer {
			// The job has a newer version, which cancels this deployment
			// once it is registered in this region.
			w.logger.Debug("multiregion deployment superseded by a newer job version", "region", r.region)
			return true
		}
	}

	if peer := w.failedPeer(ro, regions); peer != "" {
		w.logger.Info("failing multiregion deployment after a failure in a peer region",
			"region", peer, "on_failure", ro.onFailure)
		if err := w.failDeployment(structs.DeploymentStatusDescriptionFailedByPeer, nil); err != nil {
			w.logger.Error("failed to fail multiregion deployment", "error", err)
			return true
		}
		return false
	}

	if d.Status == structs.DeploymentStatusPending {
		if !w.multiregionMayStart(ro, regions) {
			return true
		}
		w.logger.Info("starting multiregion deployment")
		var resp structs.DeploymentUpdateResponse
		if err := w.RunDeployment(&structs.DeploymentRunRequest{DeploymentID: d.ID}, &resp); err != nil {
			w.logger.Error("failed to start multiregion deployment", "error", err)
			return true
		}
		return false
	}

	// The deployment is blocked: hand over to the next region.
	if next := ro.self + ro.maxParallel; next < len(regions) &&
		regions[next].status() == structs.DeploymentStatusPending {
		if err := w.peerRun(ro, regions[next]); err != nil {
			w.logger.Warn("failed to start multiregion deployment in next region",
				"region", regions[next].region, "error", err)
		}
	}

	// Once all regions completed their rollout, unblock them.
	for _, r := range regions {
		switch r.status() {
		case structs.DeploymentStatusBlocked, structs.DeploymentStatusUnblocking,
			structs.DeploymentStatusSuccessful, structs.DeploymentStatusCancelled:
		default:
			return true
		}
	}
	for i, r := range regions {
		if i == ro.self || r.status() == structs.DeploymentStatusSuccessful ||
			r.status() == structs.DeploymentStatusCancelled {
			continue
		}
		if err := w.peerUnblock(ro, r); err != nil {
			// The peer unblocks itself when it polls this region.
			w.logger.Warn("failed to unblock multiregion deployment in peer region",
				"region", r.region, "error", err)
		}
	}
	w.logger.Info("multiregion deployment complete in all regions")
	var resp structs.DeploymentUpdateResponse
	if err := w.UnblockDeployment(&structs.DeploymentUnblockRequest{DeploymentID: d.ID}, &resp); err != nil {
		w.logger.Error("failed to unblock multiregion deployment", "error", err)
		return true
	}
	return false
}

// multiregionMayStart returns whether a pending deployment may start: it is one
// of the first max_parallel regions and all regions have accepted the job
// version, or the region max_parallel places before it is done.
func (w *deploymentWatcher) multiregionMayStart(ro *multiregionRollout, regions []regionDeployment) bool {
	if ro.self < ro.maxParallel {
		for _, r := range regions {
			if r.err != nil || r.d == nil {
				return false
			}
		}
		return true
	}

	switch regions[ro.self-ro.maxParallel].status() {
	case structs.DeploymentStatusBlocked, structs.DeploymentStatusUnblocking,
		structs.DeploymentStatusSuccessful, structs.DeploymentStatusCancelled:
		return true
	case structs.DeploymentStatusFailed:
		return ro.onFailure == structs.MultiregionOnFailureFailLocal
	default:
		return false
	}
}

// failedPeer returns the name of a peer region whose failed deployment fails
// this one under the on_failure strategy, or the empty string.
func (w *deploymentWatcher) failedPeer(ro *multiregionRollout, regions []regionDeployment) string {
	for i, r := range regions {
		if i == ro.self || r.status() != structs.DeploymentStatusFailed {
			continue
		}
		switch ro.onFailure {
		case structs.MultiregionOnFailureFailAll:
			return r.region
		case structs.MultiregionOnFailureFailLocal:
		default:
			if i < ro.self {
				return r.region
			}
		}
	}
	return ""
}

// multiregionFailed notifies the peer regions of the failure of this region's
// deployment: it fails their deployments according to on_failure, or with
// "fail_local" starts the next region. The calls are best-effort, since the
// peers also learn of the failure when they poll this region.
func (w *deploymentWatcher) multiregionFailed(d *structs.Deployment) {
	ro, err := w.multiregionRollout(d)
	if err != nil {
		w.logger.Error("failed to coordinate multiregion deployment", "error", err)
		return
	}

	var targets []int
	switch ro.onFailure {
	case structs.MultiregionOnFailureFailAll:
		for i := range ro.regions {
			if i != ro.self {
				targets = append(targets, i)
			}
		}
	case structs.MultiregionOnFailureFailLocal:
		if next := ro.self + ro.maxParallel; next < len(ro.regions) {
			w.multiregionPush(ro, d, ro.regions[next], structs.DeploymentStatusPending)
		}
		return
	default:
		for i := ro.self + 1; i < len(ro.regions); i++ {
			targets = append(targets, i)
		}
	}

	var wg sync.WaitGroup
	for _, i := range targets {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			w.multiregionPush(ro, d, region, structs.DeploymentStatusFailed)
		}(ro.regions[i])
	}
	wg.Wait()
}

// multiregionPush moves the deployment of a peer region for the same job
// version: to running if it is pending, or to failed if it is active. It
// retries a few times with a backoff.
func (w *deploymentWatcher) multiregionPush(ro *multiregionRollout, d *structs.Deployment, region, to string) {
	wait := w.multiregionPoll.min
	for attempt := 1; ; attempt++ {
		r := w.regionDeployment(ro, d, region)
		err := r.err
		if err == nil {
			if r.d == nil || r.newer {
				return
			}
			switch {
			case to == structs.DeploymentStatusPending && r.d.Status == structs.DeploymentStatusPending:
				err = w.peerRun(ro, r)
			case to == structs.DeploymentStatusFailed && r.d.Active():
				err = w.peerFail(ro, r)
			default:
				return
			}
		}
		if err == nil {
			return
		}
		if attempt == multiregionPushAttempts {
			w.logger.Warn("failed to update multiregion deployment in peer region",
				"region", region, "status", to, "error", err)
			return
		}

		select {
		case <-w.parentCtx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, w.multiregionPoll.max)
	}
}

// multiregionRollout returns the multiregion view of the deployment.
func (w *deploymentWatcher) multiregionRollout(d *structs.Deployment) (*multiregionRollout, error) {
	snap, err := w.state.Snapshot()
	if err != nil {
		return nil, err
	}
	job, err := snap.JobByIDAndVersion(nil, d.Namespace, d.JobID, d.JobVersion)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %q version %d not found", d.JobID, d.JobVersion)
	}
	if !job.IsMultiregion() {
		return nil, fmt.Errorf("job %q version %d is not a multiregion job", d.JobID, d.JobVersion)
	}

	ro := &multiregionRollout{
		job:         job,
		regions:     job.Multiregion.RegionNames(),
		maxParallel: job.Multiregion.MaxParallel(),
		onFailure:   job.Multiregion.OnFailure(),
	}
	ro.self = slices.Index(ro.regions, job.Region)
	if ro.self < 0 {
		return nil, fmt.Errorf("job %q region %q is not one of its multiregion regions", job.ID, job.Region)
	}

	if job.NomadTokenID != "" {
		token, err := snap.ACLTokenByAccessorID(nil, job.NomadTokenID)
		if err != nil {
			return nil, err
		}
		if token == nil {
			w.logger.Warn("ACL token that submitted the multiregion job not found", "accessor_id", job.NomadTokenID)
		} else {
			ro.token = token.SecretID
		}
	}
	return ro, nil
}

// regionDeployments returns the deployments of the rollout's job version in all
// regions, querying the peer regions concurrently.
func (w *deploymentWatcher) regionDeployments(ro *multiregionRollout, d *structs.Deployment) []regionDeployment {
	out := make([]regionDeployment, len(ro.regions))
	var wg sync.WaitGroup
	for i, region := range ro.regions {
		if i == ro.self {
			out[i] = regionDeployment{region: region, d: d}
			continue
		}
		wg.Add(1)
		go func(i int, region string) {
			defer wg.Done()
			out[i] = w.regionDeployment(ro, d, region)
		}(i, region)
	}
	wg.Wait()
	return out
}

// regionDeployment returns the deployment of the job version of d in a peer
// region.
func (w *deploymentWatcher) regionDeployment(ro *multiregionRollout, d *structs.Deployment, region string) regionDeployment {
	args := &structs.JobSpecificRequest{
		JobID: ro.job.ID,
		QueryOptions: structs.QueryOptions{
			Region:    region,
			Namespace: ro.job.Namespace,
			AuthToken: ro.token,
		},
	}
	var reply structs.SingleDeploymentResponse
	if err := w.jobRPC.LatestDeployment(args, &reply); err != nil {
		return regionDeployment{region: region, err: err}
	}

	peer := reply.Deployment
	switch {
	case peer == nil:
		return regionDeployment{region: region}
	case peer.JobVersion > d.JobVersion:
		return regionDeployment{region: region, newer: true}
	case peer.JobVersion < d.JobVersion || !peer.IsMultiregion:
		return regionDeployment{region: region}
	default:
		return regionDeployment{region: region, d: peer}
	}
}

// peerRun runs the deployment of a peer region.
func (w *deploymentWatcher) peerRun(ro *multiregionRollout, r regionDeployment) error {
	args := &structs.DeploymentRunRequest{
		DeploymentID: r.d.ID,
		WriteRequest: ro.writeRequest(r.region),
	}
	return w.deploymentRPC.Run(args, &structs.DeploymentUpdateResponse{})
}

// peerUnblock unblocks the deployment of a peer region.
func (w *deploymentWatcher) peerUnblock(ro *multiregionRollout, r regionDeployment) error {
	args := &structs.DeploymentUnblockRequest{
		DeploymentID: r.d.ID,
		WriteRequest: ro.writeRequest(r.region),
	}
	return w.deploymentRPC.Unblock(args, &structs.DeploymentUpdateResponse{})
}

// peerFail fails the deployment of a peer region.
func (w *deploymentWatcher) peerFail(ro *multiregionRollout, r regionDeployment) error {
	args := &structs.DeploymentFailRequest{
		DeploymentID: r.d.ID,
		WriteRequest: ro.writeRequest(r.region),
	}
	return w.deploymentRPC.Fail(args, &structs.DeploymentUpdateResponse{})
}

// writeRequest returns the write request of a call to a peer region.
func (ro *multiregionRollout) writeRequest(region string) structs.WriteRequest {
	return structs.WriteRequest{
		Region:    region,
		Namespace: ro.job.Namespace,
		AuthToken: ro.token,
	}
}

// RunDeployment is used to run a pending multiregion deployment.  In
// single-region deployments, the pending state is unused.
func (w *deploymentWatcher) RunDeployment(req *structs.DeploymentRunRequest, resp *structs.DeploymentUpdateResponse) error {
	d, err := w.currentDeployment()
	if err != nil {
		return err
	}
	switch d.Status {
	case structs.DeploymentStatusPending:
	case structs.DeploymentStatusInitializing:
		return fmt.Errorf("deployment %q is initializing; it can be run once it is pending", d.ID)
	default:
		return fmt.Errorf("deployment %q is %s; only a pending deployment can be run", d.ID, d.Status)
	}

	update := w.getDeploymentStatusUpdate(structs.DeploymentStatusRunning, structs.DeploymentStatusDescriptionRunning)
	eval := w.getEval()
	i, err := w.upsertDeploymentStatusUpdate(update, eval, nil)
	if err != nil {
		return err
	}

	resp.EvalID = eval.ID
	resp.EvalCreateIndex = i
	resp.DeploymentModifyIndex = i
	resp.Index = i
	return nil
}

// UnblockDeployment is used to unblock a multiregion deployment.  In
// single-region deployments, the blocked state is unused.
func (w *deploymentWatcher) UnblockDeployment(req *structs.DeploymentUnblockRequest, resp *structs.DeploymentUpdateResponse) error {
	d, err := w.currentDeployment()
	if err != nil {
		return err
	}
	if d.Status != structs.DeploymentStatusBlocked && d.Status != structs.DeploymentStatusUnblocking {
		return fmt.Errorf("deployment %q is %s; only a blocked deployment can be unblocked", d.ID, d.Status)
	}

	update := w.getDeploymentStatusUpdate(structs.DeploymentStatusSuccessful, structs.DeploymentStatusDescriptionSuccessful)
	i, err := w.upsertDeploymentStatusUpdate(update, nil, nil)
	if err != nil {
		return err
	}

	resp.DeploymentModifyIndex = i
	resp.Index = i
	return nil
}

// CancelDeployment is used to cancel a multiregion deployment.  In
// single-region deployments, the deploymentwatcher has sole responsibility to
// cancel deployments so this RPC is never used.
func (w *deploymentWatcher) CancelDeployment(req *structs.DeploymentCancelRequest, resp *structs.DeploymentUpdateResponse) error {
	update := w.getDeploymentStatusUpdate(structs.DeploymentStatusCancelled, structs.DeploymentStatusDescriptionCancelled)
	i, err := w.upsertDeploymentStatusUpdate(update, nil, nil)
	if err != nil {
		return err
	}

	resp.DeploymentModifyIndex = i
	resp.Index = i
	return nil
}

// currentDeployment returns the watched deployment from the state store.
func (w *deploymentWatcher) currentDeployment() (*structs.Deployment, error) {
	snap, err := w.state.Snapshot()
	if err != nil {
		return nil, err
	}
	d, err := snap.DeploymentByID(nil, w.deploymentID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("deployment %q not found", w.deploymentID)
	}
	return d, nil
}
