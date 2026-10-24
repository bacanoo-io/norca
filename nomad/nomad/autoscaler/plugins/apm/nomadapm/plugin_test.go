// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomadapm

import (
	"errors"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestAPMPlugin_QueryTaskGroup(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	job := mock.Job()
	job.TaskGroups[0].Tasks[0].Resources.CPU = 500
	job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 256
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))

	usage := map[string]*cstructs.ResourceUsage{}
	var allocs []*structs.Allocation
	for i, ticks := range []float64{100, 300, 9999} {
		a := mock.Alloc()
		a.Job = job
		a.JobID = job.ID
		a.Namespace = job.Namespace
		a.TaskGroup = job.TaskGroups[0].Name
		a.ClientStatus = structs.AllocClientStatusRunning
		if i == 2 {
			// Not running: ignored.
			a.ClientStatus = structs.AllocClientStatusComplete
		}
		usage[a.ID] = &cstructs.ResourceUsage{
			CpuStats:    &cstructs.CpuStats{TotalTicks: ticks, Percent: ticks / 10},
			MemoryStats: &cstructs.MemoryStats{Usage: uint64(ticks) * 1024 * 1024},
		}
		allocs = append(allocs, a)
	}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, allocs))

	var calls int
	stats := func(namespace, allocID string) (*cstructs.AllocResourceUsage, error) {
		calls++
		must.Eq(t, job.Namespace, namespace)
		return &cstructs.AllocResourceUsage{ResourceUsage: usage[allocID]}, nil
	}

	p := NewNomadPlugin(hclog.NewNullLogger(), func() *state.StateStore { return store }, stats)
	now := time.Now()
	r := sdk.TimeRange{From: now.Add(-time.Minute), To: now}
	query := func(op, metric string) float64 {
		m, err := p.Query("taskgroup_"+op+"_"+metric+"/web/"+job.ID+"@default", r)
		must.NoError(t, err)
		must.Len(t, 1, m)
		return m[0].Value
	}

	must.Eq(t, 40.0, query("avg", "cpu-allocated"))
	must.Eq(t, 20.0, query("avg", "cpu"))
	must.Eq(t, 30.0, query("max", "cpu"))
	must.Eq(t, float64(400*1024*1024), query("sum", "memory"))
	must.Eq(t, 117.1875, query("max", "memory-allocated"))
	must.Eq(t, 10, calls)

	// Unknown job.
	_, err := p.Query("taskgroup_avg_cpu-allocated/web/nope@default", r)
	must.ErrorContains(t, err, "not found")

	// Instant queries are not supported.
	_, err = p.Query("taskgroup_avg_cpu/web/"+job.ID+"@default", sdk.TimeRange{From: now, To: now})
	must.ErrorContains(t, err, "instant")

	// A stats error fails the query.
	p = NewNomadPlugin(hclog.NewNullLogger(), func() *state.StateStore { return store },
		func(string, string) (*cstructs.AllocResourceUsage, error) { return nil, errors.New("node down") })
	_, err = p.Query("taskgroup_avg_cpu/web/"+job.ID+"@default", r)
	must.ErrorContains(t, err, "node down")
}
