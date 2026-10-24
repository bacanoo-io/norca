// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nsd

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func roleWorkload() *serviceregistration.WorkloadServices {
	check := func(name, role string) *structs.ServiceCheck {
		return &structs.ServiceCheck{
			Name:     name,
			Type:     "http",
			Path:     "/" + name,
			Interval: 5 * time.Second,
			Timeout:  time.Second,
			Role:     role,
		}
	}

	w := mockWorkload()
	w.Services = []*structs.Service{
		{
			Name:      "web",
			PortLabel: "http",
			Checks: []*structs.ServiceCheck{
				check("started", structs.CheckRoleStartup),
				check("alive", structs.CheckRoleLiveness),
			},
		},
		{
			Name:      "api",
			PortLabel: "http",
			Checks:    []*structs.ServiceCheck{check("ready", structs.CheckRoleReadiness)},
		},
		{
			Name:      "plain",
			PortLabel: "db",
		},
	}
	return w
}

// readinessHarness drives a readinessTracker against a real check store.
type readinessHarness struct {
	t        *testing.T
	shim     checkstore.Shim
	tracker  *readinessTracker
	workload *serviceregistration.WorkloadServices
	upserts  [][]*structs.ServiceRegistration
	fail     bool
}

func newReadinessHarness(t *testing.T, w *serviceregistration.WorkloadServices) *readinessHarness {
	logger := testlog.HCLogger(t)
	h := &readinessHarness{
		t:        t,
		shim:     checkstore.NewStore(logger, state.NewMemDB(logger)),
		workload: w,
	}
	h.tracker = newReadinessTracker(logger, h.shim, func(regs []*structs.ServiceRegistration) error {
		if h.fail {
			return errors.New("rpc failed")
		}
		h.upserts = append(h.upserts, regs)
		return nil
	})
	return h
}

// set records the status of the check of the given name.
func (h *readinessHarness) set(name string, status structs.CheckStatus) {
	for _, service := range h.workload.Services {
		for _, check := range service.Checks {
			if check.Name == name {
				id := structs.NomadCheckID(h.workload.AllocInfo.AllocID, h.workload.AllocInfo.Group, check)
				must.NoError(h.t, h.shim.Set(h.workload.AllocInfo.AllocID, &structs.CheckQueryResult{
					ID:        id,
					Status:    status,
					Timestamp: time.Now().Unix(),
				}))
				return
			}
		}
	}
	h.t.Fatalf("no check %q", name)
}

func (h *readinessHarness) registrations() []*structs.ServiceRegistration {
	regs := make([]*structs.ServiceRegistration, len(h.workload.Services))
	for i, service := range h.workload.Services {
		regs[i] = &structs.ServiceRegistration{
			ID:          serviceregistration.MakeAllocServiceID(h.workload.AllocInfo.AllocID, h.workload.Name(), service),
			ServiceName: service.Name,
		}
	}
	return regs
}

// update runs one update pass and returns the names and Ready state of the
// registrations it sent.
func (h *readinessHarness) update() map[string]bool {
	before := len(h.upserts)
	h.tracker.update()
	if len(h.upserts) == before {
		return nil
	}
	result := make(map[string]bool)
	for _, reg := range h.upserts[len(h.upserts)-1] {
		result[reg.ServiceName] = reg.Ready
	}
	return result
}

func readyByName(regs []*structs.ServiceRegistration) map[string]bool {
	result := make(map[string]bool, len(regs))
	for _, reg := range regs {
		result[reg.ServiceName] = reg.Ready
	}
	return result
}

func TestReadinessTracker_Lifecycle(t *testing.T) {
	ci.Parallel(t)

	h := newReadinessHarness(t, roleWorkload())
	h.set("started", structs.CheckPending)
	h.set("alive", structs.CheckFailure)
	h.set("ready", structs.CheckPending)

	// the startup check gates every service of the workload
	regs := h.registrations()
	h.tracker.track(h.workload, regs)(nil)
	must.Eq(t, map[string]bool{"web": false, "api": false, "plain": false}, readyByName(regs))
	must.Nil(t, h.update())

	// startup passes: the services without readiness checks become ready,
	// whatever the liveness check says
	h.set("started", structs.CheckSuccess)
	must.Eq(t, map[string]bool{"web": true, "plain": true}, h.update())
	must.Nil(t, h.update())

	// readiness passes
	h.set("ready", structs.CheckSuccess)
	must.Eq(t, map[string]bool{"api": true}, h.update())

	// a startup failure after startup completed changes nothing
	h.set("started", structs.CheckFailure)
	must.Nil(t, h.update())

	// a readiness failure only affects its own service
	h.set("ready", structs.CheckFailure)
	must.Eq(t, map[string]bool{"api": false}, h.update())

	// once removed, the registrations are no longer updated
	for _, reg := range regs {
		h.tracker.untrack(reg.ID)
	}
	h.set("ready", structs.CheckSuccess)
	must.Nil(t, h.update())
}

func TestReadinessTracker_NoRoleChecks(t *testing.T) {
	ci.Parallel(t)

	h := newReadinessHarness(t, mockWorkload())
	regs := h.registrations()
	h.tracker.track(h.workload, regs)(nil)

	for _, reg := range regs {
		must.True(t, reg.Ready)
	}
	must.MapEmpty(t, h.tracker.entries)

	// a nil tracker (no check store) marks everything ready
	var tracker *readinessTracker
	regs = h.registrations()
	tracker.track(h.workload, regs)(nil)
	for _, reg := range regs {
		must.True(t, reg.Ready)
	}
	tracker.untrack(regs[0].ID)
}

func TestReadinessTracker_RestoredResults(t *testing.T) {
	ci.Parallel(t)

	// results restored after a client restart make the services ready at
	// registration time
	h := newReadinessHarness(t, roleWorkload())
	h.set("started", structs.CheckSuccess)
	h.set("ready", structs.CheckSuccess)

	regs := h.registrations()
	h.tracker.track(h.workload, regs)(nil)
	must.Eq(t, map[string]bool{"web": true, "api": true, "plain": true}, readyByName(regs))
}

func TestReadinessTracker_KeepsStartupLatchOnUpdate(t *testing.T) {
	ci.Parallel(t)

	h := newReadinessHarness(t, roleWorkload())
	h.set("started", structs.CheckSuccess)
	h.tracker.track(h.workload, h.registrations())(nil)

	// the running workload is updated while its startup check fails
	h.set("started", structs.CheckFailure)
	regs := h.registrations()
	h.tracker.track(h.workload, regs)(nil)
	must.True(t, readyByName(regs)["web"])
}

func TestReadinessTracker_RegisterFailure(t *testing.T) {
	ci.Parallel(t)

	h := newReadinessHarness(t, roleWorkload())
	h.tracker.track(h.workload, h.registrations())(errors.New("rpc failed"))
	must.MapEmpty(t, h.tracker.entries)
}

func TestReadinessTracker_UpdateRetries(t *testing.T) {
	ci.Parallel(t)

	h := newReadinessHarness(t, roleWorkload())
	h.tracker.track(h.workload, h.registrations())(nil)

	h.set("started", structs.CheckSuccess)
	h.fail = true
	must.Nil(t, h.update())

	h.fail = false
	must.Eq(t, map[string]bool{"web": true, "plain": true}, h.update())
}

func TestServiceRegistrationHandler_Readiness(t *testing.T) {
	ci.Parallel(t)

	logger := testlog.HCLogger(t)
	shim := checkstore.NewStore(logger, state.NewMemDB(logger))

	var sent []*structs.ServiceRegistration
	rpc := func(method string, args, _ interface{}) error {
		if method == structs.ServiceRegistrationUpsertRPCMethod {
			sent = append(sent, args.(*structs.ServiceRegistrationUpsertRequest).Services...)
		}
		return nil
	}

	h := NewServiceRegistrationHandler(logger, &ServiceRegistrationHandlerCfg{
		Enabled:      true,
		CheckWatcher: new(mockCheckWatcher),
		CheckStore:   shim,
		RPCFn:        rpc,
	}).(*ServiceRegistrationHandler)
	defer h.Shutdown()

	w := roleWorkload()
	must.NoError(t, h.RegisterWorkload(w))
	must.Len(t, 3, sent)
	for _, reg := range sent {
		must.False(t, reg.Ready)
	}
	must.MapLen(t, 3, h.readiness.entries)

	h.RemoveWorkload(w)
	must.MapEmpty(t, h.readiness.entries)

	// services without role checks are registered as ready
	sent = nil
	must.NoError(t, h.RegisterWorkload(mockWorkload()))
	names := make([]string, 0, len(sent))
	for _, reg := range sent {
		must.True(t, reg.Ready)
		names = append(names, reg.ServiceName)
	}
	sort.Strings(names)
	must.Eq(t, []string{"redis-db", "redis-http"}, names)
}
