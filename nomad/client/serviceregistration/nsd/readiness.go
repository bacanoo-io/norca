// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nsd

import (
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// readinessInterval is how often the Ready state of the tracked
	// registrations is recomputed. Changes found in one pass are sent to the
	// servers in a single upsert.
	readinessInterval = 1 * time.Second
)

// readinessTracker maintains the Ready state of the Nomad service registrations
// of the client. A registration is ready once the startup check of its workload
// (task or group) has passed, and while all the readiness checks of its service
// pass. Registrations without such checks are always ready, which is how they
// behaved before check roles existed.
//
// The Ready state is sent to the servers only when it changes.
type readinessTracker struct {
	log      hclog.Logger
	shim     checkstore.Shim
	upsert   func([]*structs.ServiceRegistration) error
	interval time.Duration

	// lock protects entries, and is held while tracked registrations are
	// sent to the servers, so an outdated Ready state never overwrites a
	// newer one.
	lock    sync.Mutex
	entries map[string]*readyEntry

	// failed is set when the last update failed, to avoid repeated logging
	failed bool
}

// readyEntry is the Ready state of one registration, along with the checks it
// depends on.
type readyEntry struct {
	reg       *structs.ServiceRegistration
	allocID   string
	startup   structs.CheckID
	readiness []structs.CheckID

	// started latches once the startup check has passed; it is reset when the
	// workload is removed (e.g. the task restarts).
	started bool
}

func newReadinessTracker(log hclog.Logger, shim checkstore.Shim,
	upsert func([]*structs.ServiceRegistration) error) *readinessTracker {
	return &readinessTracker{
		log:      log.Named("readiness"),
		shim:     shim,
		upsert:   upsert,
		interval: readinessInterval,
		entries:  make(map[string]*readyEntry),
	}
}

// newReadyEntry returns the entry of the registration of service, which
// belongs to workload, or nil if the service has no startup or readiness
// checks to follow.
func newReadyEntry(workload *serviceregistration.WorkloadServices, service *structs.Service) *readyEntry {
	allocID := workload.AllocInfo.AllocID
	group := workload.AllocInfo.Group

	e := &readyEntry{allocID: allocID}

	// the startup check gates every service of the workload
	for _, s := range workload.Services {
		for _, check := range s.Checks {
			if check.HasRole(structs.CheckRoleStartup) {
				e.startup = structs.NomadCheckID(allocID, group, check)
			}
		}
	}

	for _, check := range service.Checks {
		if check.HasRole(structs.CheckRoleReadiness) {
			e.readiness = append(e.readiness, structs.NomadCheckID(allocID, group, check))
		}
	}

	if e.startup == "" && len(e.readiness) == 0 {
		return nil
	}
	return e
}

// ready computes the Ready state of e from the check results of its allocation.
func (e *readyEntry) ready(results map[structs.CheckID]*structs.CheckQueryResult) bool {
	passing := func(id structs.CheckID) bool {
		result, exists := results[id]
		return exists && result.Status == structs.CheckSuccess
	}

	if e.startup != "" && !e.started {
		if !passing(e.startup) {
			return false
		}
		e.started = true
	}

	for _, id := range e.readiness {
		if !passing(id) {
			return false
		}
	}
	return true
}

// track sets the Ready state of registrations, which are generated from the
// services of workload in the same order, and starts following the ones with
// startup or readiness checks.
//
// The returned function must be called with the result of sending the
// registrations to the servers. If any registration is followed, the tracker
// stays locked until then.
func (r *readinessTracker) track(workload *serviceregistration.WorkloadServices,
	registrations []*structs.ServiceRegistration) func(error) {

	if r == nil {
		for _, reg := range registrations {
			reg.Ready = true
		}
		return func(error) {}
	}

	r.lock.Lock()

	var tracked []string
	var results map[structs.CheckID]*structs.CheckQueryResult

	for i, service := range workload.Services {
		reg := registrations[i]

		e := newReadyEntry(workload, service)
		if e == nil {
			reg.Ready = true
			delete(r.entries, reg.ID)
			continue
		}

		// keep the startup latch across updates of a running workload
		if previous, exists := r.entries[reg.ID]; exists {
			e.started = previous.started
		}

		if results == nil {
			results = r.shim.List(e.allocID)
		}
		reg.Ready = e.ready(results)
		e.reg = reg.Copy()

		r.entries[reg.ID] = e
		tracked = append(tracked, reg.ID)
	}

	if len(tracked) == 0 {
		r.lock.Unlock()
		return func(error) {}
	}

	return func(err error) {
		defer r.lock.Unlock()
		if err != nil {
			// the registrations did not make it to the servers; do not
			// update them behind the back of the caller
			for _, id := range tracked {
				delete(r.entries, id)
			}
		}
	}
}

// untrack stops following the registration of the given ID.
func (r *readinessTracker) untrack(id string) {
	if r == nil {
		return
	}

	r.lock.Lock()
	defer r.lock.Unlock()
	delete(r.entries, id)
}

// run updates the Ready state of the tracked registrations on an interval
// until stopCh is closed.
func (r *readinessTracker) run(stopCh <-chan struct{}) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.update()
		}
	}
}

// update recomputes the Ready state of every tracked registration, and sends
// those that changed to the servers in a single upsert.
func (r *readinessTracker) update() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.entries) == 0 {
		return
	}

	results := make(map[string]map[structs.CheckID]*structs.CheckQueryResult)
	var changed []*structs.ServiceRegistration

	for _, e := range r.entries {
		allocResults, exists := results[e.allocID]
		if !exists {
			allocResults = r.shim.List(e.allocID)
			results[e.allocID] = allocResults
		}

		if ready := e.ready(allocResults); ready != e.reg.Ready {
			reg := e.reg.Copy()
			reg.Ready = ready
			changed = append(changed, reg)
		}
	}

	if len(changed) == 0 {
		return
	}

	if err := r.upsert(changed); err != nil {
		// retried on the next interval
		if !r.failed {
			r.log.Warn("failed to update readiness of service registrations", "error", err)
		}
		r.failed = true
		return
	}
	r.failed = false

	for _, reg := range changed {
		r.log.Trace("updated readiness of service registration", "service_id", reg.ID, "ready", reg.Ready)
		if e, exists := r.entries[reg.ID]; exists {
			e.reg = reg
		}
	}
}
