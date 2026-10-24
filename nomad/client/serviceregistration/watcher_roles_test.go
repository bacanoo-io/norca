// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package serviceregistration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// recordingRestarter records restarts without re-watching any check, so a
// test can follow several checks of the same task.
type recordingRestarter struct {
	lock     sync.Mutex
	restarts []restartRecord
}

func (r *recordingRestarter) Restart(_ context.Context, event *structs.TaskEvent, failure bool) error {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.restarts = append(r.restarts, restartRecord{
		timestamp: time.Now(),
		source:    event.Type,
		reason:    event.RestartReason,
		failure:   failure,
	})
	return nil
}

func (r *recordingRestarter) get() []restartRecord {
	r.lock.Lock()
	defer r.lock.Unlock()
	return append([]restartRecord(nil), r.restarts...)
}

func startupCheck(timeout time.Duration) *structs.ServiceCheck {
	return &structs.ServiceCheck{
		Name:           "startup",
		Role:           structs.CheckRoleStartup,
		Interval:       100 * time.Millisecond,
		Timeout:        100 * time.Millisecond,
		StartupTimeout: timeout,
	}
}

func livenessCheck(limit int) *structs.ServiceCheck {
	return &structs.ServiceCheck{
		Name:         "liveness",
		Role:         structs.CheckRoleLiveness,
		Interval:     100 * time.Millisecond,
		Timeout:      100 * time.Millisecond,
		CheckRestart: &structs.CheckRestart{Limit: limit},
	}
}

func runWatcher(cw *UniversalCheckWatcher, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cw.Run(ctx)
}

func TestCheckWatcher_Startup_Timeout(t *testing.T) {
	ci.Parallel(t)

	getter, cw := testWatcherSetup(t)
	getter.add("startup", "critical", before())

	check := startupCheck(200 * time.Millisecond)
	restarter := &recordingRestarter{}
	cw.Watch("alloc", "task", "startup", check, restarter)

	runWatcher(cw, 350*time.Millisecond)

	restarts := restarter.get()
	must.Len(t, 1, restarts)
	must.StrContains(t, restarts[0].reason, `startup check "startup" did not pass within 200ms`)
	must.True(t, restarts[0].failure)
}

func TestCheckWatcher_Startup_NomadPending(t *testing.T) {
	ci.Parallel(t)

	getter, cw := testWatcherSetup(t)
	getter.add("startup", string(structs.CheckPending), before())

	check := startupCheck(150 * time.Millisecond)
	restarter := &recordingRestarter{}
	cw.Watch("alloc", "task", "startup", check, restarter)

	runWatcher(cw, 300*time.Millisecond)
	must.Len(t, 1, restarter.get())
}

func TestCheckWatcher_Startup_PassesThenLatched(t *testing.T) {
	ci.Parallel(t)

	now := time.Now()
	getter, cw := testWatcherSetup(t)
	getter.add("startup", "critical", before())
	getter.add("startup", string(structs.CheckSuccess), now.Add(100*time.Millisecond))
	// failing again after it passed does not matter anymore
	getter.add("startup", "critical", now.Add(200*time.Millisecond))

	check := startupCheck(300 * time.Millisecond)
	restarter := &recordingRestarter{}
	cw.Watch("alloc", "task", "startup", check, restarter)

	runWatcher(cw, 600*time.Millisecond)
	must.SliceEmpty(t, restarter.get())
}

func TestCheckWatcher_Startup_GatesOtherChecks(t *testing.T) {
	ci.Parallel(t)

	now := time.Now()
	passedAt := now.Add(250 * time.Millisecond)

	getter, cw := testWatcherSetup(t)
	getter.add("startup", "critical", before())
	getter.add("startup", "passing", passedAt)
	getter.add("liveness", "critical", before())
	getter.add("legacy", "critical", before())

	restarter := &recordingRestarter{}

	// a role-less check of the same task is gated too
	legacy := testCheck()
	legacy.Name = "legacy"
	legacy.CheckRestart.Limit = 1
	legacy.CheckRestart.Grace = 0

	ForEachCheck([]*structs.Service{{Checks: []*structs.ServiceCheck{
		livenessCheck(1), legacy, startupCheck(5 * time.Second),
	}}}, func(_ *structs.Service, check *structs.ServiceCheck) {
		cw.Watch("alloc", "task", check.Name, check, restarter)
	})

	runWatcher(cw, 500*time.Millisecond)

	restarts := restarter.get()
	must.Len(t, 1, restarts)
	must.True(t, restarts[0].timestamp.After(passedAt))
}

func TestCheckWatcher_Startup_OtherTaskNotGated(t *testing.T) {
	ci.Parallel(t)

	getter, cw := testWatcherSetup(t)
	getter.add("startup", "critical", before())
	getter.add("liveness", "critical", before())

	gated := &recordingRestarter{}
	other := &recordingRestarter{}
	cw.Watch("alloc", "task1", "startup", startupCheck(5*time.Second), gated)
	cw.Watch("alloc", "task2", "liveness", livenessCheck(1), other)

	runWatcher(cw, 300*time.Millisecond)
	must.SliceEmpty(t, gated.get())
	must.Len(t, 1, other.get())
}

func TestCheckWatcher_Liveness(t *testing.T) {
	ci.Parallel(t)

	t.Run("default check_restart", func(t *testing.T) {
		getter, cw := testWatcherSetup(t)
		getter.add("liveness", string(structs.CheckFailure), before())

		check := livenessCheck(0)
		check.CheckRestart = nil
		restarter := &recordingRestarter{}
		cw.Watch("alloc", "task", "liveness", check, restarter)

		// limit 3 with a 100ms interval restarts after 200ms
		runWatcher(cw, 400*time.Millisecond)
		must.Len(t, 1, restarter.get())
	})

	t.Run("pending is not a failure", func(t *testing.T) {
		getter, cw := testWatcherSetup(t)
		getter.add("liveness", string(structs.CheckPending), before())

		restarter := &recordingRestarter{}
		cw.Watch("alloc", "task", "liveness", livenessCheck(1), restarter)

		runWatcher(cw, 300*time.Millisecond)
		must.SliceEmpty(t, restarter.get())
	})

	t.Run("pending is a failure without a role", func(t *testing.T) {
		getter, cw := testWatcherSetup(t)
		getter.add("legacy", string(structs.CheckPending), before())

		check := testCheck()
		check.CheckRestart.Limit = 1
		check.CheckRestart.Grace = 0
		restarter := &recordingRestarter{}
		cw.Watch("alloc", "task", "legacy", check, restarter)

		runWatcher(cw, 300*time.Millisecond)
		must.Len(t, 1, restarter.get())
	})
}

func TestCheckWatcher_Readiness_NotWatched(t *testing.T) {
	ci.Parallel(t)

	_, cw := testWatcherSetup(t)

	check := testCheck()
	check.Role = structs.CheckRoleReadiness
	cw.Watch("alloc", "task", "readiness", check, &recordingRestarter{})

	must.Zero(t, len(cw.checkUpdateCh))
}

type staticGetter map[string]string

func (g staticGetter) Get() (map[string]string, error) {
	result := make(map[string]string, len(g))
	for k, v := range g {
		result[k] = v
	}
	return result, nil
}

func TestCheckWatcher_LocalGetter(t *testing.T) {
	ci.Parallel(t)

	getter, cw := testWatcherSetup(t)
	getter.add("consul-check", "passing", before())
	cw.SetLocalGetter(staticGetter{
		"local-liveness": string(structs.CheckFailure),
		// the provider status wins on (unexpected) conflicts
		"consul-check": string(structs.CheckFailure),
	})

	consulCheck := testCheck()
	consulCheck.CheckRestart.Limit = 1
	consulCheck.CheckRestart.Grace = 0
	consulRestarter := &recordingRestarter{}
	cw.Watch("alloc", "task1", "consul-check", consulCheck, consulRestarter)

	localRestarter := &recordingRestarter{}
	cw.Watch("alloc", "task2", "local-liveness", livenessCheck(1), localRestarter)

	runWatcher(cw, 300*time.Millisecond)
	must.SliceEmpty(t, consulRestarter.get())
	must.Len(t, 1, localRestarter.get())
}

func TestForEachCheck_StartupFirst(t *testing.T) {
	ci.Parallel(t)

	services := []*structs.Service{
		{Checks: []*structs.ServiceCheck{{Name: "a"}, {Name: "b", Role: structs.CheckRoleLiveness}}},
		{Checks: []*structs.ServiceCheck{{Name: "c", Role: structs.CheckRoleReadiness}, {Name: "d", Role: structs.CheckRoleStartup}}},
	}

	var order []string
	ForEachCheck(services, func(_ *structs.Service, check *structs.ServiceCheck) {
		order = append(order, check.Name)
	})
	must.Eq(t, []string{"d", "a", "b", "c"}, order)
}
