// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

func TestAdvice_Events(t *testing.T) {
	ci.Parallel(t)

	demand := map[[2]string][]Unplaced{}
	type event struct {
		typ string
		ev  structs.ScalingDecisionEvent
	}
	var events []event
	a := New(hclog.NewNullLogger(), DefaultConfig(), &Deps{
		BlockedDemand: func() map[[2]string][]Unplaced { return demand },
		PublishEvent: func(typ string, ev *structs.ScalingDecisionEvent) {
			events = append(events, event{typ, *ev})
		},
		Now: time.Now,
	})
	cfg := DefaultConfig()
	cfg.Advice = []*AdviceGroup{{Name: "workers-a", Pool: "default", Datacenter: "dc-a",
		Size: NodeSize{CPU: 4000, Memory: 8000, Disk: 10000}}}
	must.NoError(t, cfg.Validate())
	st := &advice{nodes: map[[2]string]int{}, tooLarge: map[string]bool{}}

	// Nothing blocked: nothing is said.
	a.adviseOnce(cfg, st)
	must.Len(t, 0, events)

	// Four allocations of 6 GB, and one that never fits.
	demand[[2]string{"dc-a", "default"}] = []Unplaced{
		{Namespace: "ns", Job: "web", Group: "g", Count: 4, CPU: 100, Memory: 6000},
		{Namespace: "ns", Job: "huge", Group: "h", Count: 1, CPU: 100, Memory: 9000},
	}
	a.adviseOnce(cfg, st)
	must.Len(t, 2, events)
	must.Eq(t, structs.TypeScalingNodeAllocationTooLarge, events[0].typ)
	must.Eq(t, "huge", events[0].ev.JobID)
	must.Eq(t, structs.TypeScalingNodeAdvice, events[1].typ)
	must.Eq(t, int64(4), events[1].ev.Count)
	must.Eq(t, "default/dc-a", events[1].ev.Group)
	must.True(t, events[1].ev.DryRun)

	// The same picture again: no new events.
	a.adviseOnce(cfg, st)
	must.Len(t, 2, events)

	// The work is placed: the advice is withdrawn once.
	delete(demand, [2]string{"dc-a", "default"})
	a.adviseOnce(cfg, st)
	must.Len(t, 3, events)
	must.Eq(t, int64(0), events[2].ev.Count)
	must.Eq(t, int64(4), events[2].ev.PreviousCount)
	must.Eq(t, "down", events[2].ev.Direction)
}

func TestConfig_ValidateAdvice(t *testing.T) {
	ci.Parallel(t)
	good := &AdviceGroup{Name: "a", Pool: "p", Datacenter: "d", Size: NodeSize{CPU: 1, Memory: 1}}
	for name, groups := range map[string][]*AdviceGroup{
		"no datacenter": {{Name: "a", Pool: "p", Size: NodeSize{CPU: 1, Memory: 1}}},
		"no size":       {{Name: "a", Pool: "p", Datacenter: "d"}},
		"twice":         {good, good},
	} {
		cfg := DefaultConfig()
		cfg.Advice = groups
		must.Error(t, cfg.Validate(), must.Sprint(name))
	}
	cfg := DefaultConfig()
	cfg.Advice = []*AdviceGroup{good}
	must.NoError(t, cfg.Validate())
}
