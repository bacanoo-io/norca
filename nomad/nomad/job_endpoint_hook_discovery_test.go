// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func Test_setDiscoveryDefaults(t *testing.T) {
	ci.Parallel(t)

	job := &structs.Job{
		TaskGroups: []*structs.TaskGroup{
			{
				Services: []*structs.Service{
					{Name: "unset", Discovery: &structs.ServiceDiscovery{Prefer: "datacenter"}},
					{Name: "explicit", Discovery: &structs.ServiceDiscovery{OverflowFraction: pointer.Of(0.5)}},
					{Name: "off", Discovery: &structs.ServiceDiscovery{OverflowFraction: pointer.Of(0.0)}},
					{Name: "none"},
					nil,
				},
				Tasks: []*structs.Task{
					{Services: []*structs.Service{{Name: "task", Discovery: &structs.ServiceDiscovery{}}}},
					nil,
				},
			},
			nil,
		},
	}

	setDiscoveryDefaults(job, 0.35)

	services := job.TaskGroups[0].Services
	must.Eq(t, pointer.Of(0.35), services[0].Discovery.OverflowFraction)
	must.Eq(t, pointer.Of(0.5), services[1].Discovery.OverflowFraction)
	must.Eq(t, pointer.Of(0.0), services[2].Discovery.OverflowFraction)
	must.Nil(t, services[3].Discovery)
	must.Eq(t, pointer.Of(0.35), job.TaskGroups[0].Tasks[0].Services[0].Discovery.OverflowFraction)

	// resolving again keeps the resolved values
	setDiscoveryDefaults(job, 0.9)
	must.Eq(t, pointer.Of(0.35), services[0].Discovery.OverflowFraction)

	setDiscoveryDefaults(nil, 0.35)
}

// discoveryJob returns a job with nomad services whose discovery blocks leave
// the overflow fraction unset, set it explicitly and set it to 0.
func discoveryJob() *structs.Job {
	job := mock.Job()
	job.TaskGroups[0].Tasks[0].Services = []*structs.Service{
		{
			Name:      "unset",
			PortLabel: "http",
			Provider:  structs.ServiceProviderNomad,
			Discovery: &structs.ServiceDiscovery{Prefer: structs.DiscoveryPreferDatacenter},
		},
		{
			Name:      "explicit",
			PortLabel: "http",
			Provider:  structs.ServiceProviderNomad,
			Discovery: &structs.ServiceDiscovery{Prefer: structs.DiscoveryPreferDatacenter, OverflowFraction: pointer.Of(0.5)},
		},
		{
			Name:      "off",
			PortLabel: "http",
			Provider:  structs.ServiceProviderNomad,
			Discovery: &structs.ServiceDiscovery{Prefer: structs.DiscoveryPreferDatacenter, OverflowFraction: pointer.Of(0.0)},
		},
	}
	return job
}

// overflowFractions returns the overflow fractions of the services of the
// first task of job by service name.
func overflowFractions(t *testing.T, job *structs.Job) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, s := range job.TaskGroups[0].Tasks[0].Services {
		must.NotNil(t, s.Discovery.OverflowFraction, must.Sprint(s.Name))
		out[s.Name] = *s.Discovery.OverflowFraction
	}
	return out
}

// overflowFractionDiffs returns the OverflowFraction field diffs of a job diff.
func overflowFractionDiffs(diff *structs.JobDiff) []*structs.FieldDiff {
	var out []*structs.FieldDiff
	var walk func(objects []*structs.ObjectDiff)
	walk = func(objects []*structs.ObjectDiff) {
		for _, o := range objects {
			for _, f := range o.Fields {
				if f.Name == "OverflowFraction" && f.Type != structs.DiffTypeNone {
					out = append(out, f)
				}
			}
			walk(o.Objects)
		}
	}
	for _, tg := range diff.TaskGroups {
		walk(tg.Objects)
		for _, task := range tg.Tasks {
			walk(task.Objects)
		}
	}
	return out
}

// The servers fill in their default overflow fraction at registration, keep
// explicit values, and apply a reloaded default to later registrations, which
// the plan of an unchanged job shows as a change.
func TestJobEndpoint_Register_DiscoveryDefaultOverflowFraction(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.DiscoveryDefaultOverflowFraction = 0.35
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	register := func(job *structs.Job) {
		t.Helper()
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	}
	plan := func(job *structs.Job) *structs.JobDiff {
		t.Helper()
		req := &structs.JobPlanRequest{
			Job:          job,
			Diff:         true,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobPlanResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Plan", req, &resp))
		return resp.Diff
	}
	stored := func(job *structs.Job) *structs.Job {
		t.Helper()
		out, err := s1.fsm.State().JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.NotNil(t, out)
		return out
	}

	job := discoveryJob()
	register(job.Copy())
	must.Eq(t, map[string]float64{"unset": 0.35, "explicit": 0.5, "off": 0}, overflowFractions(t, stored(job)))
	must.Eq(t, uint64(0), stored(job).Version)

	// the unchanged job does not change
	must.Len(t, 0, overflowFractionDiffs(plan(job.Copy())))
	register(job.Copy())
	must.Eq(t, uint64(0), stored(job).Version)

	// a reloaded default shows in the plan and applies to the next
	// registration; explicit values stay
	cfg := s1.GetConfig()
	cfg.DiscoveryDefaultOverflowFraction = 0.6
	must.NoError(t, s1.Reload(cfg))
	must.Eq(t, 0.6, s1.discoveryDefaultOverflowFraction())

	diffs := overflowFractionDiffs(plan(job.Copy()))
	must.Len(t, 1, diffs)
	must.Eq(t, structs.DiffTypeEdited, diffs[0].Type)
	must.Eq(t, "0.35", diffs[0].Old)
	must.Eq(t, "0.6", diffs[0].New)

	register(job.Copy())
	must.Eq(t, map[string]float64{"unset": 0.6, "explicit": 0.5, "off": 0}, overflowFractions(t, stored(job)))
	must.Eq(t, uint64(1), stored(job).Version)

	// a job that sets the value the stored job resolved to does not change
	pinned := job.Copy()
	pinned.TaskGroups[0].Tasks[0].Services[0].Discovery.OverflowFraction = pointer.Of(0.6)
	must.Len(t, 0, overflowFractionDiffs(plan(pinned)))
}
