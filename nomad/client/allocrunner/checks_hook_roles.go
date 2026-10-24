// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"sync"

	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/nomad/structs"
)

// groupWorkload is the workload key of group level services.
const groupWorkload = "\x00group"

// checkedService is a service whose checks are executed by the client, along
// with the workload it belongs to (a task name, or groupWorkload).
type checkedService struct {
	workload string
	service  *structs.Service
}

// clientCheckedServices returns the services of group whose checks the client
// executes: every Nomad service, and the Consul services restricted to their
// liveness checks, which are not registered in Consul.
func clientCheckedServices(group *structs.TaskGroup) []checkedService {
	var result []checkedService

	add := func(workload string, services []*structs.Service) {
		for _, service := range services {
			switch service.Provider {
			case structs.ServiceProviderNomad:
				result = append(result, checkedService{workload, service})
			default:
				var liveness []*structs.ServiceCheck
				for _, check := range service.Checks {
					if check.IsClientExecuted(service.Provider) {
						liveness = append(liveness, check)
					}
				}
				if len(liveness) > 0 {
					s := service.Copy()
					s.Checks = liveness
					result = append(result, checkedService{workload, s})
				}
			}
		}
	}

	add(groupWorkload, group.Services)
	for _, task := range group.Tasks {
		add(task.Name, task.Services)
	}
	return result
}

// interpolateCheckedServices interpolates the services with env.
func interpolateCheckedServices(env *taskenv.TaskEnv, services []checkedService) []checkedService {
	result := make([]checkedService, len(services))
	for i, cs := range services {
		result[i] = checkedService{cs.workload, taskenv.InterpolateService(env, cs.service)}
	}
	return result
}

// startupGate tracks whether the startup check of a workload has passed. Until
// it has, the readiness and liveness checks of the workload are not executed,
// so their results stay pending.
type startupGate struct {
	// id of the startup check
	id structs.CheckID

	lock   sync.Mutex
	passed bool
}

// pass opens the gate.
func (g *startupGate) pass() {
	g.lock.Lock()
	defer g.lock.Unlock()
	g.passed = true
}

// isOpen returns true if the startup check has passed. The results of a
// restarted workload are reset to pending (see the service registration
// wrapper), which closes the gate again until the startup check passes.
func (g *startupGate) isOpen(store checkstore.Shim, allocID string) bool {
	g.lock.Lock()
	defer g.lock.Unlock()

	if g.passed {
		if current, exists := store.List(allocID)[g.id]; exists && current.Status == structs.CheckPending {
			g.passed = false
		}
	}
	return g.passed
}

// startupGates returns a gate for each workload of services that has a startup
// check, reusing the gates in existing.
func startupGates(alloc *structs.Allocation, services []checkedService, existing map[string]*startupGate) map[string]*startupGate {
	for _, cs := range services {
		for _, check := range cs.service.Checks {
			if !check.HasRole(structs.CheckRoleStartup) {
				continue
			}
			id := structs.NomadCheckID(alloc.ID, alloc.TaskGroup, check)
			if gate, exists := existing[cs.workload]; exists {
				// the startup check may have changed in an update; existing
				// observers keep using the same gate
				gate.lock.Lock()
				gate.id = id
				gate.lock.Unlock()
			} else {
				existing[cs.workload] = &startupGate{id: id}
			}
		}
	}
	return existing
}

// waitForStartup returns true if the check of o must not be executed yet,
// because the startup check of its workload has not passed.
func (o *observer) waitForStartup() bool {
	if o.gate == nil {
		return false
	}

	switch o.check.Role {
	case structs.CheckRoleReadiness, structs.CheckRoleLiveness:
		return !o.gate.isOpen(o.checkStore, o.allocID)
	default:
		// the startup check itself and checks without a role always run
		return false
	}
}

// observed records the result of a startup check in the gate of its workload.
func (o *observer) observed(result *structs.CheckQueryResult) {
	if o.gate != nil && o.check.HasRole(structs.CheckRoleStartup) && result.Status == structs.CheckSuccess {
		o.gate.pass()
	}
}
