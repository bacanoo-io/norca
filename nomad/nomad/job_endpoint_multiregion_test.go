// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// multiregionCluster is a federation of in-process test servers, one or more
// per region, with a node registered in each region.
type multiregionCluster struct {
	t       *testing.T
	regions []string
	servers map[string][]*Server
}

// newMultiregionCluster starts the given number of servers per region and joins
// them. Every region gets a ready node in the datacenter "<region>-1".
func newMultiregionCluster(t *testing.T, regions []string, sizes map[string]int) *multiregionCluster {
	c := &multiregionCluster{t: t, regions: regions, servers: map[string][]*Server{}}

	var all []*Server
	for _, region := range regions {
		size := max(sizes[region], 1)
		for i := 0; i < size; i++ {
			s, cleanup := TestServer(t, func(cfg *Config) {
				cfg.Region = region
				cfg.BootstrapExpect = size
				cfg.NumSchedulers = 2
			})
			t.Cleanup(cleanup)
			s.deploymentWatcher.SetMultiregionPollInterval(100*time.Millisecond, time.Second)
			c.servers[region] = append(c.servers[region], s)
			all = append(all, s)
		}
	}
	TestJoin(t, all...)

	for _, region := range regions {
		waitForStableLeadership(t, c.servers[region])
	}
	for _, s := range all {
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			return len(s.Regions()) == len(regions)
		}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
	}

	for _, region := range regions {
		node := mock.Node()
		node.Datacenter = region + "-1"
		req := &structs.NodeRegisterRequest{
			Node:         node,
			WriteRequest: structs.WriteRequest{Region: region},
		}
		var resp structs.NodeUpdateResponse
		must.NoError(t, c.leader(region).RPC("Node.Register", req, &resp))
	}
	return c
}

// leader returns the leader of a region, or a live server of the region while
// it has none.
func (c *multiregionCluster) leader(region string) *Server {
	var live *Server
	for _, s := range c.servers[region] {
		if s.IsShutdown() {
			continue
		}
		if s.IsLeader() {
			return s
		}
		live = s
	}
	return live
}

// deployment returns the latest deployment of the job in a region.
func (c *multiregionCluster) deployment(region string, job *structs.Job) *structs.Deployment {
	d, err := c.leader(region).State().LatestDeploymentByJobID(nil, job.Namespace, job.ID)
	must.NoError(c.t, err)
	return d
}

// statuses returns the deployment statuses of the job in all regions, or nil
// while a region has no leader ready for consistent reads, since the state of
// a follower can lag. The regions are read in reverse rollout order, so that a
// region seen running was started after the regions before it were seen in
// their current state.
func (c *multiregionCluster) statuses(job *structs.Job) []string {
	out := make([]string, len(c.regions))
	for i := len(c.regions) - 1; i >= 0; i-- {
		s := c.leader(c.regions[i])
		if s == nil || !s.IsLeader() || !s.isReadyForConsistentReads() {
			return nil
		}
		d, err := s.State().LatestDeploymentByJobID(nil, job.Namespace, job.ID)
		must.NoError(c.t, err)
		if d != nil {
			out[i] = d.Status
		}
	}
	return out
}

// waitStatuses waits until the deployments of the job have the given statuses.
func (c *multiregionCluster) waitStatuses(job *structs.Job, want ...string) {
	var got []string
	must.Wait(c.t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		got = c.statuses(job)
		if !slices.Equal(got, want) {
			return fmt.Errorf("deployment statuses %v, want %v", got, want)
		}
		return nil
	}), wait.Timeout(60*time.Second), wait.Gap(50*time.Millisecond)))
}

// simulateClients marks the allocations of a region running and healthy (or
// unhealthy), as the clients of the region would, until the test ends.
func (c *multiregionCluster) simulateClients(region string, healthy bool) {
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s := c.leader(region)
			if s == nil {
				continue
			}
			iter, err := s.State().Allocs(nil, state.SortDefault)
			if err != nil {
				continue
			}
			var updates []*structs.Allocation
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				alloc := raw.(*structs.Allocation)
				switch {
				case alloc.DesiredStatus == structs.AllocDesiredStatusRun && !alloc.DeploymentStatus.HasHealth():
					alloc = alloc.Copy()
					alloc.ClientStatus = structs.AllocClientStatusRunning
					alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
						Healthy:   pointer.Of(healthy),
						Timestamp: time.Now(),
					}
					updates = append(updates, alloc)
				case alloc.DesiredStatus != structs.AllocDesiredStatusRun && !alloc.ClientTerminalStatus():
					alloc = alloc.Copy()
					alloc.ClientStatus = structs.AllocClientStatusComplete
					updates = append(updates, alloc)
				}
			}
			if len(updates) == 0 {
				continue
			}
			req := &structs.AllocUpdateRequest{
				Alloc:        updates,
				WriteRequest: structs.WriteRequest{Region: region},
			}
			var resp structs.GenericResponse
			_ = s.RPC("Node.UpdateAlloc", req, &resp)
		}
	}()
}

// partition cuts the cross-region RPC between a region and the other regions,
// as a network partition would, and returns the function that heals it.
func (c *multiregionCluster) partition(region string) (heal func()) {
	type cut struct {
		s      *Server
		region string
		peers  []*serverParts
	}
	var cuts []cut
	for r, servers := range c.servers {
		for _, s := range servers {
			s.peerLock.Lock()
			for _, other := range c.regions {
				if (r == region) == (other == region) {
					continue
				}
				cuts = append(cuts, cut{s: s, region: other, peers: s.peers[other]})
				delete(s.peers, other)
			}
			s.peerLock.Unlock()
		}
	}
	return func() {
		for _, cut := range cuts {
			cut.s.peerLock.Lock()
			cut.s.peers[cut.region] = cut.peers
			cut.s.peerLock.Unlock()
		}
	}
}

// multiregionTestJob returns a service job deploying to the given regions, with
// a count of two per region set by the multiregion block.
func multiregionTestJob(regions []string, maxParallel int, onFailure string) *structs.Job {
	job := mock.Job()
	job.Region = "global"
	job.TaskGroups[0].Count = 0
	update := *structs.DefaultUpdateStrategy
	update.MaxParallel = 2
	update.MinHealthyTime = 0
	update.ProgressDeadline = 0
	job.Update = update
	job.TaskGroups[0].Update = &update
	job.Multiregion = &structs.Multiregion{
		Strategy: &structs.MultiregionStrategy{MaxParallel: maxParallel, OnFailure: onFailure},
	}
	for _, region := range regions {
		job.Multiregion.Regions = append(job.Multiregion.Regions, &structs.MultiregionRegion{
			Name:        region,
			Count:       2,
			Datacenters: []string{region + "-1"},
			Meta:        map[string]string{"region": region},
		})
	}
	return job
}

// registerMultiregionJob submits a multiregion job to a region.
func registerMultiregionJob(t *testing.T, s *Server, region string, job *structs.Job) *structs.JobRegisterResponse {
	req := &structs.JobRegisterRequest{
		Job:          job.Copy(),
		WriteRequest: structs.WriteRequest{Region: region, Namespace: job.Namespace},
	}
	var resp structs.JobRegisterResponse
	must.NoError(t, s.RPC("Job.Register", req, &resp))
	return &resp
}

// rolloutMonitor samples the deployment statuses of a job and records the order
// in which the regions start, and any sample where a running region does not
// have all regions before it done and all regions after it waiting.
type rolloutMonitor struct {
	l          sync.Mutex
	started    []string
	violations []string
}

func (c *multiregionCluster) monitorRollout(job *structs.Job) *rolloutMonitor {
	m := &rolloutMonitor{}
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			statuses := c.statuses(job)
			if statuses == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			m.l.Lock()
			for i, status := range statuses {
				if status != structs.DeploymentStatusRunning {
					continue
				}
				if !slices.Contains(m.started, c.regions[i]) {
					m.started = append(m.started, c.regions[i])
				}
				for j, other := range statuses {
					done := other == structs.DeploymentStatusBlocked || other == structs.DeploymentStatusSuccessful
					waiting := other == "" || other == structs.DeploymentStatusInitializing ||
						other == structs.DeploymentStatusPending
					if (j < i && !done) || (j > i && !waiting) {
						m.violations = append(m.violations, fmt.Sprintf("%v", statuses))
					}
				}
			}
			m.l.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return m
}

func (m *rolloutMonitor) result() ([]string, []string) {
	m.l.Lock()
	defer m.l.Unlock()
	return slices.Clone(m.started), slices.Clone(m.violations)
}

// TestJobEndpoint_Multiregion_Sequential asserts that a multiregion job with
// max_parallel = 1 is registered with the same version and the fields of each
// region in three regions, deploys to the regions one after another in order,
// and completes in all of them; that submitting it again is a no-op; and that
// a global stop stops it in every region.
func TestJobEndpoint_Multiregion_Sequential(t *testing.T) {
	ci.Parallel(t)

	regions := []string{"aws", "gcp", "hetzner"}
	c := newMultiregionCluster(t, regions, nil)
	for _, region := range regions {
		c.simulateClients(region, true)
	}

	job := multiregionTestJob(regions, 1, "")
	monitor := c.monitorRollout(job)
	resp := registerMultiregionJob(t, c.leader("gcp"), "gcp", job)
	must.NotEq(t, "", resp.EvalID)

	c.waitStatuses(job, "successful", "successful", "successful")

	started, violations := monitor.result()
	must.Eq(t, regions, started)
	must.SliceEmpty(t, violations, must.Sprintf("%v", violations))

	for _, region := range regions {
		stored, err := c.leader(region).State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.Eq(t, region, stored.Region)
		must.Eq(t, 0, stored.Version)
		must.True(t, stored.Stable)
		must.Eq(t, []string{region + "-1"}, stored.Datacenters)
		must.Eq(t, 2, stored.TaskGroups[0].Count)
		must.Eq(t, region, stored.Meta["region"])

		deployments, err := c.leader(region).State().DeploymentsByJobID(nil, job.Namespace, job.ID, true)
		must.NoError(t, err)
		must.Len(t, 1, deployments)
		must.True(t, deployments[0].IsMultiregion)
	}

	// Submitting the same job again changes nothing.
	again := registerMultiregionJob(t, c.leader("aws"), "aws", job)
	must.Eq(t, "", again.Warnings)
	for _, region := range regions {
		stored, err := c.leader(region).State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.Eq(t, 0, stored.Version)
	}

	// A change in the fields of one region is a new version in all regions.
	job.Multiregion.Regions[2].Count = 3
	registerMultiregionJob(t, c.leader("aws"), "aws", job)
	for _, region := range regions {
		stored, err := c.leader(region).State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.Eq(t, 1, stored.Version)
	}
	c.waitStatuses(job, "successful", "successful", "successful")

	// A global stop stops the job in every region.
	stopReq := &structs.JobDeregisterRequest{
		JobID:        job.ID,
		Global:       true,
		WriteRequest: structs.WriteRequest{Region: "hetzner", Namespace: job.Namespace},
	}
	var stopResp structs.JobDeregisterResponse
	must.NoError(t, c.leader("hetzner").RPC("Job.Deregister", stopReq, &stopResp))
	for _, region := range regions {
		stored, err := c.leader(region).State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.True(t, stored.Stop, must.Sprintf("job not stopped in region %s", region))
	}
}

// TestJobEndpoint_Multiregion_FailAll asserts that with on_failure = "fail_all"
// a failed deployment in one region fails the deployments of all regions: the
// blocked region before it and the pending region after it.
func TestJobEndpoint_Multiregion_FailAll(t *testing.T) {
	ci.Parallel(t)

	regions := []string{"aws", "gcp", "hetzner"}
	c := newMultiregionCluster(t, regions, nil)
	c.simulateClients("aws", true)
	c.simulateClients("gcp", false)
	c.simulateClients("hetzner", true)

	job := multiregionTestJob(regions, 1, structs.MultiregionOnFailureFailAll)
	registerMultiregionJob(t, c.leader("aws"), "aws", job)

	c.waitStatuses(job, "failed", "failed", "failed")

	gcp := c.deployment("gcp", job)
	must.Eq(t, structs.DeploymentStatusDescriptionFailedAllocations, gcp.StatusDescription)
	allocs, err := c.leader("hetzner").State().AllocsByJob(nil, job.Namespace, job.ID, true)
	must.NoError(t, err)
	must.SliceEmpty(t, allocs, must.Sprint("hetzner must not have started"))
}

// TestJobEndpoint_Multiregion_FailLocal asserts that with on_failure =
// "fail_local" a failed region does not fail the others: the next region still
// deploys, and the regions wait blocked for the operator.
func TestJobEndpoint_Multiregion_FailLocal(t *testing.T) {
	ci.Parallel(t)

	regions := []string{"aws", "gcp", "hetzner"}
	c := newMultiregionCluster(t, regions, nil)
	c.simulateClients("aws", true)
	c.simulateClients("gcp", false)
	c.simulateClients("hetzner", true)

	job := multiregionTestJob(regions, 1, structs.MultiregionOnFailureFailLocal)
	registerMultiregionJob(t, c.leader("aws"), "aws", job)

	c.waitStatuses(job, "blocked", "failed", "blocked")

	// The operator unblocks the blocked regions.
	for _, region := range []string{"aws", "hetzner"} {
		req := &structs.DeploymentUnblockRequest{
			DeploymentID: c.deployment(region, job).ID,
			WriteRequest: structs.WriteRequest{Region: region},
		}
		var resp structs.DeploymentUpdateResponse
		must.NoError(t, c.leader("aws").RPC("Deployment.Unblock", req, &resp))
	}
	c.waitStatuses(job, "successful", "failed", "successful")
}

// TestJobEndpoint_Multiregion_LeaderLoss asserts that losing the leader of the
// submitting region in the middle of a rollout neither repeats nor loses a
// step: the new leader continues from the Raft state.
func TestJobEndpoint_Multiregion_LeaderLoss(t *testing.T) {
	ci.Parallel(t)

	regions := []string{"aws", "gcp", "hetzner"}
	c := newMultiregionCluster(t, regions, map[string]int{"aws": 3})
	for _, region := range regions {
		c.simulateClients(region, true)
	}

	job := multiregionTestJob(regions, 1, "")
	monitor := c.monitorRollout(job)
	registerMultiregionJob(t, c.leader("aws"), "aws", job)

	// Lose the leader of the submitting region once its own deployment is
	// done and the rollout moved on.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		d := c.deployment("aws", job)
		return d != nil && d.Status == structs.DeploymentStatusBlocked
	}), wait.Timeout(60*time.Second), wait.Gap(20*time.Millisecond)))
	old := c.leader("aws")
	must.NoError(t, old.Shutdown())
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		s := c.leader("aws")
		return s != nil && s != old && s.IsLeader()
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)))

	c.waitStatuses(job, "successful", "successful", "successful")

	started, violations := monitor.result()
	must.Eq(t, regions, started)
	must.SliceEmpty(t, violations, must.Sprintf("%v", violations))
	for _, region := range regions {
		deployments, err := c.leader(region).State().DeploymentsByJobID(nil, job.Namespace, job.ID, true)
		must.NoError(t, err)
		must.Len(t, 1, deployments, must.Sprintf("region %s", region))
	}
}

// TestJobEndpoint_Multiregion_Partition asserts that while a region is cut off
// from the others the rollout waits, with the regions that completed staying
// blocked, and that it completes once the partition heals.
func TestJobEndpoint_Multiregion_Partition(t *testing.T) {
	ci.Parallel(t)

	regions := []string{"aws", "gcp", "hetzner"}
	c := newMultiregionCluster(t, regions, nil)
	for _, region := range regions {
		c.simulateClients(region, true)
	}

	job := multiregionTestJob(regions, 1, "")
	registerMultiregionJob(t, c.leader("aws"), "aws", job)

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		d := c.deployment("aws", job)
		return d != nil && d.Status == structs.DeploymentStatusRunning
	}), wait.Timeout(60*time.Second), wait.Gap(20*time.Millisecond)))
	heal := c.partition("hetzner")

	c.waitStatuses(job, "blocked", "blocked", "pending")
	time.Sleep(3 * time.Second)
	must.Eq(t, []string{"blocked", "blocked", "pending"}, c.statuses(job))

	heal()
	c.waitStatuses(job, "successful", "successful", "successful")
}

// TestJobEndpoint_Multiregion_Validation asserts that multiregion jobs are
// validated and that a submission to unknown regions is rejected before
// anything is registered.
func TestJobEndpoint_Multiregion_Validation(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	register := func(job *structs.Job) error {
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		return s1.RPC("Job.Register", req, &resp)
	}

	job := mock.Job()
	job.Multiregion = &structs.Multiregion{}
	must.ErrorContains(t, register(job), "Multiregion block must have at least one region")

	job = multiregionTestJob([]string{"global", "global"}, 1, "fail_some")
	err := register(job)
	must.ErrorContains(t, err, `Multiregion region "global" is listed more than once`)
	must.ErrorContains(t, err, "Multiregion on_failure must be")

	job = multiregionTestJob([]string{"global", "west"}, 1, "")
	must.ErrorContains(t, register(job), `multiregion job region "west" is not a known region`)
	stored, err := s1.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Nil(t, stored)

	// A job that lists only this region is a multiregion job of one region.
	job = multiregionTestJob([]string{"global"}, 1, "")
	must.NoError(t, register(job))
	d, err := s1.State().LatestDeploymentByJobID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.NotNil(t, d)
	must.True(t, d.IsMultiregion)
	must.Eq(t, structs.DeploymentStatusInitializing, d.Status)

	// A plan is interpolated for the region.
	planReq := &structs.JobPlanRequest{
		Job:          multiregionTestJob([]string{"global"}, 1, ""),
		Diff:         true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	planReq.Job.ID = job.ID
	var planResp structs.JobPlanResponse
	must.NoError(t, s1.RPC("Job.Plan", planReq, &planResp))
	must.Eq(t, structs.DiffTypeNone, planResp.Diff.Type)
}

// TestJobEndpoint_Multiregion_ACL asserts that a multiregion job needs a global
// ACL token, which the other regions accept.
func TestJobEndpoint_Multiregion_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.Region = "aws"
		c.AuthoritativeRegion = "aws"
	})
	defer cleanupS1()
	s2, _, cleanupS2 := TestACLServer(t, func(c *Config) {
		c.Region = "gcp"
		c.AuthoritativeRegion = "aws"
		c.ReplicationBackoff = 20 * time.Millisecond
		c.ReplicationToken = root.SecretID
	})
	defer cleanupS2()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		token, _ := s2.State().ACLTokenBySecretID(nil, root.SecretID)
		return token != nil && len(s1.Regions()) == 2
	}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))

	local := mock.ACLManagementToken()
	local.Global = false
	must.NoError(t, s1.State().UpsertACLTokens(structs.MsgTypeTestSetup, 1000, []*structs.ACLToken{local}))

	job := multiregionTestJob([]string{"aws", "gcp"}, 1, "")
	register := func(token string) error {
		req := &structs.JobRegisterRequest{
			Job: job.Copy(),
			WriteRequest: structs.WriteRequest{
				Region: "aws", Namespace: job.Namespace, AuthToken: token,
			},
		}
		var resp structs.JobRegisterResponse
		return s1.RPC("Job.Register", req, &resp)
	}

	must.ErrorContains(t, register(local.SecretID), "global ACL token")
	must.NoError(t, register(root.SecretID))

	stored, err := s2.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.NotNil(t, stored)
	must.Eq(t, "gcp", stored.Region)
	must.Eq(t, root.AccessorID, stored.NomadTokenID)
}
