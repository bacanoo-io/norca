// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package serviceregistration

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-set"
	"github.com/hashicorp/nomad/nomad/structs"
)

// roleState is the part of a restarter specific to check roles.
type roleState struct {
	// role of the check, empty if none
	role string

	// ignorePending is set for liveness checks: a check that has not run yet
	// does not count as a failure.
	ignorePending bool

	// startupTimeout and startupDeadline apply to startup checks: if the check
	// has not passed by the deadline, the task is restarted.
	startupTimeout  time.Duration
	startupDeadline time.Time

	// passed is set once a startup check has passed. From then on, the
	// startup check no longer gates the other checks of its task.
	passed bool
}

// isStartup returns true if r watches a startup check.
func (r *restarter) isStartup() bool {
	return r.role == structs.CheckRoleStartup
}

// configure sets up r according to the role and the check_restart block of
// check, which is watched from now on.
func (r *restarter) configure(check *structs.ServiceCheck, now time.Time) {
	r.role = check.Role

	if r.isStartup() {
		timeout := check.StartupTimeout
		if timeout <= 0 {
			timeout = structs.DefaultStartupTimeout
		}
		r.startupTimeout = timeout
		r.startupDeadline = now.Add(timeout)

		// do not warn about the check missing until the deadline
		r.graceUntil = r.startupDeadline
		return
	}

	checkRestart := check.CheckRestart
	if checkRestart == nil && check.HasRole(structs.CheckRoleLiveness) {
		// liveness checks restart by default; the startup check provides
		// the grace period
		checkRestart = &structs.CheckRestart{Limit: structs.DefaultLivenessRestartLimit}
	}

	r.ignorePending = check.HasRole(structs.CheckRoleLiveness)
	r.grace = checkRestart.Grace
	r.graceUntil = now.Add(checkRestart.Grace)
	r.timeLimit = check.Interval * time.Duration(checkRestart.Limit-1)
	r.ignoreWarnings = checkRestart.IgnoreWarnings
}

// isPassing returns true if status is the passing status of a Consul or a
// Nomad check.
func isPassing(status string) bool {
	return status == "passing" || status == string(structs.CheckSuccess)
}

// applyStartup evaluates the startup checks in watched. A startup check that
// passes stops gating the other checks of its task; one that has not passed
// by its deadline restarts its task. available is false if the check statuses
// could not be retrieved, in which case no restart is triggered.
//
// Returns the tasks whose startup check has not passed yet.
func (w *UniversalCheckWatcher) applyStartup(
	ctx context.Context,
	now time.Time,
	available bool,
	statuses map[string]string,
	watched map[string]*restarter,
	restarts *set.Set[key],
) *set.Set[key] {

	starting := set.New[key](0)

	for checkID, r := range watched {
		if !r.isStartup() || r.passed {
			continue
		}

		if restarts.Contains(r.taskKey) {
			// task is already being restarted
			delete(watched, checkID)
			continue
		}

		if available && isPassing(statuses[checkID]) {
			r.logger.Debug("startup check passed")
			r.passed = true
			continue
		}

		if !available || now.Before(r.startupDeadline) {
			starting.Insert(r.taskKey)
			continue
		}

		// has not passed by the deadline, restart!
		r.logger.Debug("restarting due to startup check not passing", "startup_timeout", r.startupTimeout)
		reason := fmt.Sprintf("healthcheck: startup check %q did not pass within %v", r.checkName, r.startupTimeout)
		event := structs.NewTaskEvent(structs.TaskRestartSignal).SetRestartReason(reason)
		go asyncRestart(ctx, r.logger, r.task, event)

		// check will be re-registered & re-watched on startup
		delete(watched, checkID)
		restarts.Insert(r.taskKey)
	}

	return starting
}

// localGetter holds the optional getter of the statuses of checks executed by
// the Nomad client, which may be set while the watcher is running.
type localGetter struct {
	lock   sync.RWMutex
	getter CheckStatusGetter
}

// SetLocalGetter sets the getter of the statuses of checks the Nomad client
// executes on behalf of the provider of the watcher, such as liveness checks
// of Consul services. Their statuses are merged with those of the provider.
func (w *UniversalCheckWatcher) SetLocalGetter(getter CheckStatusGetter) {
	w.local.lock.Lock()
	defer w.local.lock.Unlock()
	w.local.getter = getter
}

// mergeLocalStatuses adds the statuses of the client executed checks, if any,
// to statuses. The check IDs of the client and of the provider are disjoint.
func (w *UniversalCheckWatcher) mergeLocalStatuses(statuses map[string]string) map[string]string {
	w.local.lock.RLock()
	getter := w.local.getter
	w.local.lock.RUnlock()

	if getter == nil {
		return statuses
	}

	local, err := getter.Get()
	if err != nil || len(local) == 0 {
		return statuses
	}

	if statuses == nil {
		statuses = make(map[string]string, len(local))
	}
	for checkID, status := range local {
		if _, exists := statuses[checkID]; !exists {
			statuses[checkID] = status
		}
	}
	return statuses
}

// ForEachCheck calls fn for every check of services, startup checks first, so
// a CheckWatcher learns about the startup check of a workload before the
// checks it gates.
func ForEachCheck(services []*structs.Service, fn func(service *structs.Service, check *structs.ServiceCheck)) {
	for _, startup := range []bool{true, false} {
		for _, service := range services {
			for _, check := range service.Checks {
				if check.HasRole(structs.CheckRoleStartup) == startup {
					fn(service, check)
				}
			}
		}
	}
}
