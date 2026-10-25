// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/serviceregistration/checks"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// checksHookName is the name of this hook as appears in logs
	checksHookName = "checks_hook"
)

// observers maintains a map from check_id -> observer for a particular check. Each
// observer in the map must share the same context.
type observers map[structs.CheckID]*observer

// An observer is used to execute a particular check on its interval and update the
// check store with those results.
type observer struct {
	ctx        context.Context
	cancel     context.CancelFunc
	checker    checks.Checker
	checkStore checkstore.Shim

	qc      *checks.QueryContext
	check   *structs.ServiceCheck
	allocID string

	// gate is the startup gate of the workload of the check, nil if the
	// workload has no startup check.
	gate *startupGate

	// thresholds applies success_before_passing and failures_before_critical,
	// nil if the check sets neither.
	thresholds *checkThresholds
}

// start checking our check on its interval
func (o *observer) start() {
	// compromise between immediate (too early) and waiting full interval (slow)
	firstWait := o.check.Interval / 2

	timer, cancel := helper.NewSafeTimer(firstWait)
	defer cancel()

	for {
		select {

		// exit the observer
		case <-o.ctx.Done():
			return

		// time to execute the check
		case <-timer.C:
			// startup check of the workload has not passed yet
			if o.waitForStartup() {
				timer.Reset(o.check.Interval)
				continue
			}

			query := checks.GetCheckQuery(o.check)
			result := o.checker.Do(o.ctx, o.qc, query)
			result.Role = o.check.Role
			o.thresholds.apply(result)

			// and put the results into the store (already logged)
			_ = o.checkStore.Set(o.allocID, result)
			o.observed(result)

			// setup timer for next interval
			timer.Reset(o.check.Interval)
		}
	}
}

// stop checking our check - this will also interrupt an in-progress execution
func (o *observer) stop() {
	o.cancel()
}

// checksHook manages checks of Nomad service registrations, at both the group and
// task level, by storing / removing them from the Client state store.
//
// Does not manage Consul service checks; see groupServiceHook instead.
type checksHook struct {
	logger  hclog.Logger
	network structs.NetworkStatus
	shim    checkstore.Shim
	checker checks.Checker
	allocID string
	taskEnv *taskenv.TaskEnv

	// fields that get re-initialized on allocation update
	lock      sync.RWMutex
	ctx       context.Context
	stop      func()
	observers observers
	alloc     *structs.Allocation

	// gates holds the startup gate of each workload with a startup check
	gates map[string]*startupGate
}

func newChecksHook(
	logger hclog.Logger,
	alloc *structs.Allocation,
	shim checkstore.Shim,
	network structs.NetworkStatus,
	taskEnv *taskenv.TaskEnv,
) *checksHook {
	h := &checksHook{
		logger:  logger.Named(checksHookName),
		allocID: alloc.ID,
		alloc:   alloc,
		shim:    shim,
		network: network,
		checker: checks.New(logger),
		taskEnv: taskEnv,
	}
	h.initialize(alloc)
	return h
}

// initialize the dynamic fields of checksHook, which is to say setup all the
// observers and query context things associated with the alloc.
//
// Should be called during initial setup only.
func (h *checksHook) initialize(alloc *structs.Allocation) {
	h.lock.Lock()
	defer h.lock.Unlock()

	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return
	}

	// fresh context and stop function for this allocation
	h.ctx, h.stop = context.WithCancel(context.Background())

	// fresh set of observers
	h.observers = make(observers)
	h.gates = make(map[string]*startupGate)

	// set the initial alloc
	h.alloc = alloc
}

// observe will create the observer for each service in services.
// services must contain only checks executed by the client (see
// clientCheckedServices).
//
// Caller must hold h.lock.
func (h *checksHook) observe(alloc *structs.Allocation, services []checkedService) {
	var ports structs.AllocatedPorts
	var networks structs.Networks
	if alloc.AllocatedResources != nil {
		ports = alloc.AllocatedResources.Shared.Ports
		networks = alloc.AllocatedResources.Shared.Networks
	}

	gates := startupGates(alloc, services, h.gates)

	for _, cs := range services {
		service := cs.service
		for _, check := range service.Checks {

			// remember the initialization time
			now := time.Now().UTC().Unix()

			// create the deterministic check id for this check
			id := structs.NomadCheckID(alloc.ID, alloc.TaskGroup, check)

			// an observer for this check already exists
			if _, exists := h.observers[id]; exists {
				continue
			}

			ctx, cancel := context.WithCancel(h.ctx)

			// create the observer for this check
			h.observers[id] = &observer{
				ctx:        ctx,
				cancel:     cancel,
				check:      check.Copy(),
				checkStore: h.shim,
				checker:    h.checker,
				allocID:    h.allocID,
				qc: &checks.QueryContext{
					ID:               id,
					CustomAddress:    service.Address,
					ServicePortLabel: service.PortLabel,
					Ports:            ports,
					Networks:         networks,
					NetworkStatus:    h.network,
					Group:            alloc.Name,
					Task:             service.TaskName,
					Service:          service.Name,
					Check:            check.Name,
				},
				gate:       gates[cs.workload],
				thresholds: newCheckThresholds(check),
			}

			// insert a pending result into state store for each check
			result := checks.Stub(id, structs.GetCheckMode(check), now, alloc.Name, service.TaskName, service.Name, check.Name)
			result.Role = check.Role
			if err := h.shim.Set(h.allocID, result); err != nil {
				h.logger.Error("failed to set initial check status", "id", h.allocID, "error", err)
				continue
			}

			// start the observer
			go h.observers[id].start()
		}
	}
}

func (h *checksHook) Name() string {
	return checksHookName
}

func (h *checksHook) Prerun() error {
	h.lock.Lock()
	defer h.lock.Unlock()

	group := h.alloc.Job.LookupTaskGroup(h.alloc.TaskGroup)
	if group == nil {
		return nil
	}

	interpolatedServices := interpolateCheckedServices(h.taskEnv, clientCheckedServices(group))

	// create and start observers of nomad service checks in alloc, and of
	// the liveness checks of consul services
	h.observe(h.alloc, interpolatedServices)

	return nil
}

func (h *checksHook) Update(request *interfaces.RunnerUpdateRequest) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	group := request.Alloc.Job.LookupTaskGroup(request.Alloc.TaskGroup)
	if group == nil {
		return nil
	}

	// get all group and task level services using nomad provider, and the
	// liveness checks of those using consul
	services := clientCheckedServices(group)

	// create a set of the updated set of checks
	next := make([]structs.CheckID, 0, len(h.observers))
	for _, cs := range services {
		for _, check := range cs.service.Checks {
			next = append(next, structs.NomadCheckID(
				request.Alloc.ID,
				request.Alloc.TaskGroup,
				check,
			))
		}
	}

	// stop the observers of the checks we are removing
	remove := h.shim.Difference(request.Alloc.ID, next)
	for _, id := range remove {
		h.observers[id].stop()
		delete(h.observers, id)
	}

	// remove checks that are no longer part of the allocation
	if err := h.shim.Remove(request.Alloc.ID, remove); err != nil {
		return err
	}

	// remember this new alloc
	h.alloc = request.Alloc

	// ensure we are observing new checks (idempotent)
	h.observe(request.Alloc, services)

	return nil
}

func (h *checksHook) PreKill() {
	h.lock.Lock()
	defer h.lock.Unlock()

	// terminate our hook context, which threads down to all observers
	h.stop()

	// purge all checks for this allocation from the client state store
	if err := h.shim.Purge(h.allocID); err != nil {
		h.logger.Error("failed to purge check results", "alloc_id", h.allocID, "error", err)
	}
}
