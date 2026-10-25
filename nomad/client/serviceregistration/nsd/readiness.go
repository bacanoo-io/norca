// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nsd

import (
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// readinessInterval is how often the health of the tracked registrations
	// is recomputed. Changes found in one pass are sent to the servers in a
	// single upsert.
	readinessInterval = 1 * time.Second

	// reassertInterval is how often the health of every tracked registration
	// is sent to the servers even if it did not change, so a change the
	// servers lost (e.g. one held back by the flapping protection of a leader
	// that lost its leadership) is corrected. Servers skip the Raft write of
	// registrations that did not change.
	reassertInterval = 1 * time.Minute
)

// readinessTracker maintains the health of the Nomad service registrations of
// the client: the Ready state, the aggregate Health and the states of the
// checks of every registration.
//
// A registration is ready once the startup check of its workload (task or
// group) has passed, and while all the readiness checks of its service pass.
// Its Health is critical until the startup check has passed, and then the
// worst state of the readiness and role-less checks of its service. Liveness
// checks restart the task and do not count toward health. Registrations
// without checks are always ready and passing, which is how they behaved
// before check roles existed.
//
// The health is sent to the servers only when it changes.
type readinessTracker struct {
	log      hclog.Logger
	shim     checkstore.Shim
	upsert   func([]*structs.ServiceRegistration) error
	interval time.Duration

	// reassert is the number of passes between two re-assertions
	reassert int
	passes   int

	// lock protects entries, and is held while tracked registrations are
	// sent to the servers, so an outdated state never overwrites a newer
	// one.
	lock    sync.Mutex
	entries map[string]*readyEntry

	// failed is set when the last update failed, to avoid repeated logging
	failed bool
}

// readyEntry is the health of one registration, along with the checks it
// depends on.
type readyEntry struct {
	reg       *structs.ServiceRegistration
	allocID   string
	startup   structs.CheckID
	readiness []structs.CheckID

	// checks are the readiness and role-less checks of the service, which
	// make up its Health
	checks []healthCheck

	// started latches once the startup check has passed; it is reset when the
	// workload is removed (e.g. the task restarts).
	started bool
}

// healthCheck is a check counting toward the health of a registration.
type healthCheck struct {
	id   structs.CheckID
	name string
	role string
}

func newReadinessTracker(log hclog.Logger, shim checkstore.Shim,
	upsert func([]*structs.ServiceRegistration) error) *readinessTracker {
	return &readinessTracker{
		log:      log.Named("readiness"),
		shim:     shim,
		upsert:   upsert,
		interval: readinessInterval,
		reassert: int(reassertInterval / readinessInterval),
		entries:  make(map[string]*readyEntry),
	}
}

// newReadyEntry returns the entry of the registration of service, which
// belongs to workload, or nil if the service has no checks to follow.
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
		id := structs.NomadCheckID(allocID, group, check)
		switch check.Role {
		case structs.CheckRoleReadiness:
			e.readiness = append(e.readiness, id)
		case "":
		default:
			continue
		}
		e.checks = append(e.checks, healthCheck{id: id, name: check.Name, role: check.Role})
	}

	if e.startup == "" && len(e.checks) == 0 {
		return nil
	}
	return e
}

// checkStatus maps the result of a Nomad check to the state of a catalog
// check. Checks that have not run yet are critical, as in Consul; an HTTP
// check answered with 429 Too Many Requests is a warning, as in Consul.
func checkStatus(result *structs.CheckQueryResult) string {
	if result == nil {
		return structs.ServiceHealthCritical
	}
	switch result.Status {
	case structs.CheckSuccess:
		return structs.ServiceHealthPassing
	case structs.CheckFailure:
		if result.StatusCode == http.StatusTooManyRequests {
			return structs.ServiceHealthWarning
		}
	}
	return structs.ServiceHealthCritical
}

// worse returns the worse of two health states.
func worse(a, b string) string {
	rank := func(s string) int {
		switch s {
		case structs.ServiceHealthPassing:
			return 0
		case structs.ServiceHealthWarning:
			return 1
		default:
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// apply computes the health of e from the check results of its allocation and
// sets it on reg.
func (e *readyEntry) apply(reg *structs.ServiceRegistration, results map[structs.CheckID]*structs.CheckQueryResult) {
	passing := func(id structs.CheckID) bool {
		result, exists := results[id]
		return exists && result.Status == structs.CheckSuccess
	}

	if e.startup != "" && !e.started && passing(e.startup) {
		e.started = true
	}
	started := e.startup == "" || e.started

	ready := started
	for _, id := range e.readiness {
		if !passing(id) {
			ready = false
		}
	}

	health := structs.ServiceHealthPassing
	if !started {
		health = structs.ServiceHealthCritical
	}

	var checks []*structs.ServiceRegistrationCheck
	for _, c := range e.checks {
		result := results[c.id]
		status := checkStatus(result)
		health = worse(health, status)

		check := &structs.ServiceRegistrationCheck{Name: c.name, Role: c.role, Status: status}
		if result != nil {
			check.Output = structs.TruncateCheckOutput(result.Output)
		}
		checks = append(checks, check)
	}

	reg.Ready = ready
	reg.Health = health
	reg.Checks = checks
}

// track sets the health of registrations, which are generated from the
// services of workload in the same order, and starts following the ones with
// checks.
//
// The returned function must be called with the result of sending the
// registrations to the servers. If any registration is followed, the tracker
// stays locked until then.
func (r *readinessTracker) track(workload *serviceregistration.WorkloadServices,
	registrations []*structs.ServiceRegistration) func(error) {

	if r == nil {
		for _, reg := range registrations {
			reg.Ready = true
			reg.Health = structs.ServiceHealthPassing
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
			reg.Health = structs.ServiceHealthPassing
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
		e.apply(reg, results)
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

// run updates the health of the tracked registrations on an interval until
// stopCh is closed.
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

// update recomputes the health of every tracked registration, and sends those
// that changed to the servers in a single upsert. Every reassert passes, it
// sends every tracked registration.
func (r *readinessTracker) update() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.entries) == 0 {
		return
	}

	r.passes++
	reassert := r.reassert > 0 && r.passes%r.reassert == 0

	results := make(map[string]map[structs.CheckID]*structs.CheckQueryResult)
	var changed []*structs.ServiceRegistration

	for _, e := range r.entries {
		allocResults, exists := results[e.allocID]
		if !exists {
			allocResults = r.shim.List(e.allocID)
			results[e.allocID] = allocResults
		}

		reg := e.reg.Copy()
		e.apply(reg, allocResults)
		if reassert || !reg.HealthEqual(e.reg) {
			changed = append(changed, reg)
		}
	}

	if len(changed) == 0 {
		return
	}

	if err := r.upsert(changed); err != nil {
		// retried on the next interval
		if !r.failed {
			r.log.Warn("failed to update health of service registrations", "error", err)
		}
		r.failed = true
		return
	}
	r.failed = false

	for _, reg := range changed {
		r.log.Trace("updated health of service registration",
			"service_id", reg.ID, "ready", reg.Ready, "health", reg.Health)
		if e, exists := r.entries[reg.ID]; exists {
			e.reg = reg
		}
	}
}
