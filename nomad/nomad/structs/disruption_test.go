// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/shoenig/test/must"
)

func TestDisruptionBudget_Validate(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name   string
		budget DisruptionBudget
		err    string
	}{
		{"min count", DisruptionBudget{MinHealthy: "4"}, ""},
		{"min percent", DisruptionBudget{MinHealthy: "50%"}, ""},
		{"max zero", DisruptionBudget{MaxUnavailable: "0"}, ""},
		{"max percent", DisruptionBudget{MaxUnavailable: "100%"}, ""},
		{"empty", DisruptionBudget{}, "must set min_healthy or max_unavailable"},
		{"both", DisruptionBudget{MinHealthy: "1", MaxUnavailable: "1"}, "cannot set both"},
		{"negative", DisruptionBudget{MinHealthy: "-1"}, "must not be negative"},
		{"over 100%", DisruptionBudget{MaxUnavailable: "101%"}, "must not exceed 100%"},
		{"garbage", DisruptionBudget{MinHealthy: "half"}, "not a count or a percentage"},
		{"fraction", DisruptionBudget{MinHealthy: "1.5"}, "not a count or a percentage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.budget.Validate()
			if tc.err == "" {
				must.NoError(t, err)
			} else {
				must.ErrorContains(t, err, tc.err)
			}
		})
	}
}

func TestDisruptionBudget_RequiredHealthy(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		budget DisruptionBudget
		count  int
		exp    int
	}{
		{DisruptionBudget{MinHealthy: "4"}, 6, 4},
		{DisruptionBudget{MinHealthy: "8"}, 6, 8},
		{DisruptionBudget{MinHealthy: "50%"}, 5, 3}, // rounded up
		{DisruptionBudget{MinHealthy: "100%"}, 6, 6},
		{DisruptionBudget{MaxUnavailable: "2"}, 6, 4},
		{DisruptionBudget{MaxUnavailable: "10"}, 6, 0},
		{DisruptionBudget{MaxUnavailable: "25%"}, 6, 4}, // 1.5 rounded up to 2
		{DisruptionBudget{MaxUnavailable: "25%"}, 2, 1}, // 0.5 rounded up to 1
		{DisruptionBudget{MaxUnavailable: "0%"}, 6, 6},
	}
	for _, tc := range cases {
		must.Eq(t, tc.exp, tc.budget.RequiredHealthy(tc.count), must.Sprintf("%s count=%d", tc.budget.String(), tc.count))
	}
}

func TestTaskGroup_Validate_Disruption(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{Disruption: &DisruptionBudget{MinHealthy: "1"}}

	must.NoError(t, tg.validateDisruption(&Job{Type: JobTypeService}))
	must.ErrorContains(t, tg.validateDisruption(&Job{Type: JobTypeBatch}), "does not allow disruption block")
	must.ErrorContains(t, tg.validateDisruption(&Job{Type: JobTypeSystem}), "does not allow disruption block")

	tg.Disruption.MaxUnavailable = "1"
	must.ErrorContains(t, tg.validateDisruption(&Job{Type: JobTypeService}), "cannot set both")
}

func TestTaskGroup_Disruption_CopyDiff(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{Name: "web", Disruption: &DisruptionBudget{MinHealthy: "4"}}
	c := tg.Copy()
	c.Disruption.MinHealthy = "3"
	must.Eq(t, "4", tg.Disruption.MinHealthy)

	diff, err := tg.Diff(c, false)
	must.NoError(t, err)
	must.Eq(t, DiffTypeEdited, diff.Type)
	must.Len(t, 1, diff.Objects)
	must.Eq(t, "Disruption", diff.Objects[0].Name)
	must.Eq(t, "MinHealthy", diff.Objects[0].Fields[0].Name)
	must.Eq(t, "4", diff.Objects[0].Fields[0].Old)
	must.Eq(t, "3", diff.Objects[0].Fields[0].New)

	c.Disruption = nil
	diff, err = tg.Diff(c, false)
	must.NoError(t, err)
	must.Eq(t, DiffTypeDeleted, diff.Objects[0].Type)
}

func disruptionTestAlloc(tg string, healthy bool) *Allocation {
	return &Allocation{
		ID:            uuid.Generate(),
		TaskGroup:     tg,
		DesiredStatus: AllocDesiredStatusRun,
		ClientStatus:  AllocClientStatusRunning,
		DeploymentStatus: &AllocDeploymentStatus{
			Healthy: pointer.Of(healthy),
		},
	}
}

func TestAllocation_HealthyForDisruption(t *testing.T) {
	ci.Parallel(t)

	a := disruptionTestAlloc("web", true)
	must.True(t, a.HealthyForDisruption())

	b := a.Copy()
	b.DeploymentStatus.Healthy = nil
	must.False(t, b.HealthyForDisruption())

	b = a.Copy()
	b.ClientStatus = AllocClientStatusPending
	must.False(t, b.HealthyForDisruption())

	b = a.Copy()
	b.DesiredStatus = AllocDesiredStatusStop
	must.False(t, b.HealthyForDisruption())

	b = a.Copy()
	b.DesiredTransition.Migrate = pointer.Of(true)
	must.False(t, b.HealthyForDisruption(), must.Sprint("an alloc marked for migration is a pending disruption"))
}

func TestNewDisruptionStatus(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{Name: "web", Count: 6, Disruption: &DisruptionBudget{MinHealthy: "4"}}
	must.Nil(t, NewDisruptionStatus(&TaskGroup{Name: "web", Count: 6}, nil, nil))

	var allocs []*Allocation
	for i := 0; i < 5; i++ {
		allocs = append(allocs, disruptionTestAlloc("web", true))
	}
	allocs = append(allocs, disruptionTestAlloc("web", false))
	// allocs of other groups are ignored
	allocs = append(allocs, disruptionTestAlloc("other", true))

	s := NewDisruptionStatus(tg, allocs, nil)
	must.Eq(t, &DisruptionStatus{
		TaskGroup:          "web",
		MinHealthy:         "4",
		DesiredCount:       6,
		RequiredHealthy:    4,
		Healthy:            5,
		DisruptionsAllowed: 1,
	}, s)

	// The unhealthy alloc comes first, then one healthy alloc
	got := s.Disruptable(allocs[:6])
	must.Len(t, 2, got)
	must.Eq(t, allocs[5].ID, got[0].ID)
	must.Eq(t, allocs[0].ID, got[1].ID)

	// With healthy below the requirement nothing may be disrupted, not even
	// unhealthy allocations
	allocs[0].DeploymentStatus.Healthy = pointer.Of(false)
	allocs[1].DeploymentStatus.Healthy = pointer.Of(false)
	s = NewDisruptionStatus(tg, allocs, nil)
	must.Eq(t, 3, s.Healthy)
	must.Eq(t, 0, s.DisruptionsAllowed)
	must.SliceEmpty(t, s.Disruptable(allocs[:6]))
	must.True(t, errors.Is(s.Error(), ErrDisruptionBudgetExceeded))
}

func TestNewDisruptionStatus_DeploymentReserve(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{
		Name:       "web",
		Count:      6,
		Disruption: &DisruptionBudget{MinHealthy: "3"},
		Update:     &UpdateStrategy{MaxParallel: 2},
	}
	var allocs []*Allocation
	for i := 0; i < 6; i++ {
		allocs = append(allocs, disruptionTestAlloc("web", true))
	}

	d := &Deployment{
		Status: DeploymentStatusRunning,
		TaskGroups: map[string]*DeploymentState{
			"web": {DesiredTotal: 6, PlacedAllocs: 2, HealthyAllocs: 2},
		},
	}

	// Between two deployment steps: the next step's max_parallel is reserved
	s := NewDisruptionStatus(tg, allocs, d)
	must.Eq(t, 2, s.DeploymentReserved)
	must.Eq(t, 1, s.DisruptionsAllowed)

	// During a step the in-flight allocations already count as unhealthy
	d.TaskGroups["web"].PlacedAllocs = 4
	allocs[0].DeploymentStatus.Healthy = nil
	allocs[1].DeploymentStatus.Healthy = nil
	s = NewDisruptionStatus(tg, allocs, d)
	must.Eq(t, 0, s.DeploymentReserved)
	must.Eq(t, 1, s.DisruptionsAllowed)

	// Nothing reserved once everything is placed
	d.TaskGroups["web"].PlacedAllocs = 6
	d.TaskGroups["web"].HealthyAllocs = 4
	allocs[0].DeploymentStatus.Healthy = pointer.Of(true)
	allocs[1].DeploymentStatus.Healthy = pointer.Of(true)
	s = NewDisruptionStatus(tg, allocs, d)
	must.Eq(t, 0, s.DeploymentReserved)
	must.Eq(t, 3, s.DisruptionsAllowed)

	// Nothing reserved for paused or terminal deployments
	d.TaskGroups["web"].PlacedAllocs = 2
	d.TaskGroups["web"].HealthyAllocs = 2
	for _, status := range []string{DeploymentStatusPaused, DeploymentStatusSuccessful, DeploymentStatusFailed} {
		d.Status = status
		must.Eq(t, 0, NewDisruptionStatus(tg, allocs, d).DeploymentReserved)
	}
}

// TestDisruption_MsgpackRoundTrip asserts the generated codec carries the new
// fields.
func TestDisruption_MsgpackRoundTrip(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{Name: "web", Disruption: &DisruptionBudget{MaxUnavailable: "25%"}}
	buf, err := Encode(JobRegisterRequestType, tg)
	must.NoError(t, err)
	var out TaskGroup
	must.NoError(t, Decode(buf[1:], &out))
	must.Eq(t, tg.Disruption, out.Disruption)

	req := &AllocEvictRequest{AllocID: "a", NoShutdownDelay: true, Eval: &Evaluation{ID: "e"}}
	buf, err = Encode(AllocEvictRequestType, req)
	must.NoError(t, err)
	must.Eq(t, byte(AllocEvictRequestType), buf[0])
	var outReq AllocEvictRequest
	must.NoError(t, Decode(buf[1:], &outReq))
	must.Eq(t, "a", outReq.AllocID)
	must.True(t, outReq.NoShutdownDelay)
	must.Eq(t, "e", outReq.Eval.ID)
}
