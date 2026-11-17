// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consul

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/serviceregistration"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func roleChecksWorkload() *serviceregistration.WorkloadServices {
	w := testWorkload()
	w.AllocInfo.Group = "group"
	check := func(name, role string) *structs.ServiceCheck {
		return &structs.ServiceCheck{
			Name:     name,
			Type:     "tcp",
			Interval: 100 * time.Millisecond,
			Timeout:  100 * time.Millisecond,
			Role:     role,
		}
	}
	alive := check("alive", structs.CheckRoleLiveness)
	alive.CheckRestart = &structs.CheckRestart{Limit: 1}
	w.Services[0].Checks = []*structs.ServiceCheck{
		check("started", structs.CheckRoleStartup),
		check("ready", structs.CheckRoleReadiness),
		alive,
	}
	return w
}

type staticStatuses map[string]string

func (s staticStatuses) Get() (map[string]string, error) {
	result := make(map[string]string, len(s))
	for k, v := range s {
		result[k] = v
	}
	return result, nil
}

func TestConsul_CheckRoles_LivenessNotRegistered(t *testing.T) {
	ci.Parallel(t)

	ctx := setupFake(t)
	ctx.Workload = roleChecksWorkload()

	must.NoError(t, ctx.ServiceClient.RegisterWorkload(ctx.Workload))
	must.NoError(t, ctx.syncOnce(syncNewOps))

	service := ctx.Workload.Services[0]
	serviceID := serviceregistration.MakeAllocServiceID(ctx.Workload.AllocInfo.AllocID, ctx.Workload.Name(), service)

	checks := ctx.FakeConsul.checks["default"]
	must.MapLen(t, 2, checks)
	must.MapContainsKey(t, checks, MakeCheckID(serviceID, service.Checks[0]))
	must.MapContainsKey(t, checks, MakeCheckID(serviceID, service.Checks[1]))
	must.MapNotContainsKey(t, checks, MakeCheckID(serviceID, service.Checks[2]))

	// the liveness check is followed under the ID of its client side result
	must.Eq(t,
		string(structs.NomadCheckID(ctx.Workload.AllocInfo.AllocID, "group", service.Checks[2])),
		watchCheckID(serviceID, ctx.Workload, service.Checks[2]))
	must.Eq(t, MakeCheckID(serviceID, service.Checks[1]), watchCheckID(serviceID, ctx.Workload, service.Checks[1]))

	// removing the workload deregisters the consul checks
	ctx.ServiceClient.RemoveWorkload(ctx.Workload)
	must.NoError(t, ctx.syncOnce(syncNewOps))
	must.MapEmpty(t, ctx.FakeConsul.checks["default"])
}

func TestConsul_CheckRoles_LivenessRestart(t *testing.T) {
	ci.Parallel(t)

	run := func(t *testing.T, startupStatus string) *restartRecorder {
		ctx := setupFake(t)
		ctx.Workload = roleChecksWorkload()
		service := ctx.Workload.Services[0]

		// the startup check status comes from consul, the liveness check
		// fails on the client
		ctx.FakeConsul.SetStatus(startupStatus)
		ctx.ServiceClient.SetLocalCheckStatusGetter(staticStatuses{
			string(structs.NomadCheckID(ctx.Workload.AllocInfo.AllocID, "group", service.Checks[2])): string(structs.CheckFailure),
		})

		runCtx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go ctx.ServiceClient.checkWatcher.Run(runCtx)

		must.NoError(t, ctx.ServiceClient.RegisterWorkload(ctx.Workload))
		must.NoError(t, ctx.syncOnce(syncNewOps))
		return ctx.Workload.Restarter.(*restartRecorder)
	}

	t.Run("started", func(t *testing.T) {
		restarter := run(t, "passing")
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool { return atomic.LoadInt64(&restarter.restarts) == 1 }),
			wait.Timeout(5*time.Second),
			wait.Gap(50*time.Millisecond),
		))
	})

	t.Run("starting", func(t *testing.T) {
		// the failing startup check gates the liveness check
		restarter := run(t, "critical")
		time.Sleep(2 * time.Second)
		must.Zero(t, atomic.LoadInt64(&restarter.restarts))
	})
}

// switchStatuses returns the statuses once failing is set, none before.
type switchStatuses struct {
	failing  atomic.Bool
	statuses staticStatuses
}

func (s *switchStatuses) Get() (map[string]string, error) {
	if !s.failing.Load() {
		return map[string]string{}, nil
	}
	return s.statuses.Get()
}

// A liveness check stays watched when the workload is updated in place with a
// change of the check that its watch ID (the ID of its result on the client)
// does not cover, but its Consul check ID does.
func TestConsul_CheckRoles_LivenessRestart_AfterUpdate(t *testing.T) {
	ci.Parallel(t)

	changes := map[string]func(*structs.ServiceCheck){
		"unchanged":              func(*structs.ServiceCheck) {},
		"success_before_passing": func(c *structs.ServiceCheck) { c.SuccessBeforePassing = 2 },
		"header":                 func(c *structs.ServiceCheck) { c.Header = map[string][]string{"X-Probe": {"1"}} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			ctx := setupFake(t)
			ctx.Workload = roleChecksWorkload()
			service := ctx.Workload.Services[0]

			ctx.FakeConsul.SetStatus("passing")
			local := &switchStatuses{statuses: staticStatuses{
				string(structs.NomadCheckID(ctx.Workload.AllocInfo.AllocID, "group", service.Checks[2])): string(structs.CheckFailure),
			}}
			ctx.ServiceClient.SetLocalCheckStatusGetter(local)

			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go ctx.ServiceClient.checkWatcher.Run(runCtx)

			must.NoError(t, ctx.ServiceClient.RegisterWorkload(ctx.Workload))
			must.NoError(t, ctx.syncOnce(syncNewOps))

			updated := ctx.Workload.Copy()
			change(updated.Services[0].Checks[2])
			must.Eq(t,
				watchCheckID("", ctx.Workload, service.Checks[2]),
				watchCheckID("", updated, updated.Services[0].Checks[2]))
			must.NoError(t, ctx.ServiceClient.UpdateWorkload(ctx.Workload, updated))
			must.NoError(t, ctx.syncOnce(syncNewOps))

			// the liveness check fails after the update
			local.failing.Store(true)
			restarter := ctx.Workload.Restarter.(*restartRecorder)
			must.Wait(t, wait.InitialSuccess(
				wait.BoolFunc(func() bool { return atomic.LoadInt64(&restarter.restarts) == 1 }),
				wait.Timeout(5*time.Second),
				wait.Gap(50*time.Millisecond),
			))
		})
	}
}
