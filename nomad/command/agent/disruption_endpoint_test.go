// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestHTTP_AllocEvict(t *testing.T) {
	ci.Parallel(t)
	// Without schedulers the evictions are not processed, which keeps the
	// allocations (on nonexistent nodes) running.
	noSchedulers := func(c *Config) { c.Server.NumSchedulers = pointer.Of(0) }
	httpTest(t, noSchedulers, func(s *TestAgent) {
		store := s.Agent.server.State()

		job := mock.Job()
		job.TaskGroups[0].Count = 2
		job.TaskGroups[0].Disruption = &structs.DisruptionBudget{MaxUnavailable: "1"}
		must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))
		allocs := make([]*structs.Allocation, 2)
		for i := range allocs {
			a := mock.Alloc()
			a.Job = job
			a.JobID = job.ID
			a.TaskGroup = job.TaskGroups[0].Name
			a.ClientStatus = structs.AllocClientStatusRunning
			a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
			allocs[i] = a
		}
		must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, allocs))

		client := s.Client()

		// Budget status before the eviction
		statuses, _, err := client.Jobs().Disruption(job.ID, nil)
		must.NoError(t, err)
		must.Eq(t, 1, statuses["web"].DisruptionsAllowed)
		must.Eq(t, "1", statuses["web"].MaxUnavailable)

		resp, err := client.Allocations().Evict(allocs[0].ID, false, nil)
		must.NoError(t, err)
		must.NotEq(t, "", resp.EvalID)
		must.Positive(t, resp.LastIndex)

		// The second eviction exceeds the budget
		_, err = client.Allocations().Evict(allocs[1].ID, false, nil)
		must.Error(t, err)
		must.True(t, api.IsDisruptionBudgetExceeded(err), must.Sprintf("got: %v", err))

		// The raw HTTP response carries 429 and Retry-After
		httpResp, err := http.Post(s.HTTPAddr()+"/v1/allocation/"+allocs[1].ID+"/evict", "", nil)
		must.NoError(t, err)
		defer httpResp.Body.Close()
		must.Eq(t, http.StatusTooManyRequests, httpResp.StatusCode)
		must.Eq(t, "5", httpResp.Header.Get("Retry-After"))

		// Unknown allocation and job
		_, err = client.Allocations().Evict("00000000-0000-0000-0000-000000000000", false, nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.Jobs().Disruption("missing", nil)
		must.ErrorContains(t, err, "404")

		// Wrong method
		httpResp2, err := http.Get(s.HTTPAddr() + "/v1/allocation/" + allocs[1].ID + "/evict")
		must.NoError(t, err)
		defer httpResp2.Body.Close()
		must.Eq(t, http.StatusMethodNotAllowed, httpResp2.StatusCode)
	})
}

func TestJobs_ApiDisruptionToStructs(t *testing.T) {
	ci.Parallel(t)

	// JSON accepts both numbers and strings
	var tg api.TaskGroup
	must.NoError(t, json.Unmarshal([]byte(`{"Name": "web", "Disruption": {"MinHealthy": 4}}`), &tg))
	must.Eq(t, &api.DisruptionBudget{MinHealthy: pointer.Of("4")}, tg.Disruption)
	must.NoError(t, json.Unmarshal([]byte(`{"Name": "web", "Disruption": {"MaxUnavailable": "25%"}}`), &tg))
	must.Eq(t, &api.DisruptionBudget{MaxUnavailable: pointer.Of("25%")}, tg.Disruption)
	must.Error(t, json.Unmarshal([]byte(`{"Disruption": {"MinHealthy": true}}`), &tg))

	apiJob := &api.Job{
		ID:   pointer.Of("web"),
		Name: pointer.Of("web"),
		Type: pointer.Of("service"),
		TaskGroups: []*api.TaskGroup{{
			Name:       pointer.Of("web"),
			Count:      pointer.Of(6),
			Disruption: &api.DisruptionBudget{MinHealthy: pointer.Of("4")},
			Tasks:      []*api.Task{{Name: "web", Driver: "docker"}},
		}},
	}
	apiJob.Canonicalize()
	job := ApiJobToStructJob(apiJob)
	must.Eq(t, &structs.DisruptionBudget{MinHealthy: "4"}, job.TaskGroups[0].Disruption)
}
