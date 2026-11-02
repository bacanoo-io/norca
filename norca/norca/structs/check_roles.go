// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/hashicorp/go-multierror"
)

// Check roles give a service check an explicit purpose, in the manner of the
// Kubernetes startup, readiness and liveness probes. A check without a role
// keeps the behavior it has always had.
const (
	// CheckRoleStartup marks the check that tells whether the task has
	// finished starting. Until it passes, the other checks of the task do not
	// trigger restarts and the services of the task are not ready. If it does
	// not pass within StartupTimeout, the task is restarted.
	CheckRoleStartup = "startup"

	// CheckRoleReadiness marks a check that tells whether the service instance
	// can receive traffic. It counts toward deployment health and never
	// restarts the task.
	CheckRoleReadiness = "readiness"

	// CheckRoleLiveness marks a check whose failure restarts the task. It does
	// not affect the health of the service in the catalog nor in deployments.
	CheckRoleLiveness = "liveness"

	// DefaultStartupTimeout is the startup_timeout of a startup check that
	// does not set one.
	DefaultStartupTimeout = 5 * time.Minute

	// DefaultLivenessRestartLimit is the check_restart limit of a liveness
	// check without a check_restart block. The grace period defaults to zero,
	// since the startup check provides it.
	DefaultLivenessRestartLimit = 3
)

// HasRole returns true if the check has the given role.
func (sc *ServiceCheck) HasRole(role string) bool {
	return sc != nil && sc.Role == role
}

// IsClientExecuted returns true if the check is executed by the Nomad client
// regardless of the service provider. Checks of Nomad services always are;
// liveness checks of Consul services are too, so they do not affect the
// health of the service in Consul.
func (sc *ServiceCheck) IsClientExecuted(provider string) bool {
	return provider == ServiceProviderNomad || sc.HasRole(CheckRoleLiveness)
}

// canonicalizeRole applies the defaults implied by the role of the check.
func (sc *ServiceCheck) canonicalizeRole() {
	switch sc.Role {
	case CheckRoleStartup:
		if sc.StartupTimeout == 0 {
			sc.StartupTimeout = DefaultStartupTimeout
		}
	case CheckRoleLiveness:
		if sc.CheckRestart == nil {
			sc.CheckRestart = &CheckRestart{Limit: DefaultLivenessRestartLimit}
		}
	}
}

// validateRole validates the role of the check and the settings tied to it.
func (sc *ServiceCheck) validateRole() error {
	switch sc.Role {
	case "", CheckRoleStartup, CheckRoleReadiness, CheckRoleLiveness:
		// OK
	default:
		return fmt.Errorf("role must be %q, %q, %q or empty; got %q",
			CheckRoleStartup, CheckRoleReadiness, CheckRoleLiveness, sc.Role)
	}

	if sc.StartupTimeout < 0 {
		return errors.New("startup_timeout must be non-negative")
	}
	if sc.StartupTimeout > 0 && sc.Role != CheckRoleStartup {
		return fmt.Errorf("startup_timeout may only be set on checks with role %q", CheckRoleStartup)
	}

	if sc.CheckRestart != nil {
		switch sc.Role {
		case CheckRoleReadiness:
			return fmt.Errorf("check_restart may not be set on checks with role %q, they never restart the task", sc.Role)
		case CheckRoleStartup:
			return fmt.Errorf("check_restart may not be set on checks with role %q, use startup_timeout instead", sc.Role)
		}
	}

	return nil
}

// countStartupChecks returns the number of startup checks in services.
func countStartupChecks(services []*Service) int {
	n := 0
	for _, service := range services {
		for _, check := range service.Checks {
			if check.HasRole(CheckRoleStartup) {
				n++
			}
		}
	}
	return n
}

// validateCheckRoles ensures each task has at most one startup check, and so
// do the group level services, since the startup check gates the other checks
// of the same workload.
func (tg *TaskGroup) validateCheckRoles() error {
	var mErr multierror.Error

	if n := countStartupChecks(tg.Services); n > 1 {
		mErr.Errors = append(mErr.Errors, fmt.Errorf(
			"Group services may have at most one check with role %q, found %d", CheckRoleStartup, n))
	}

	for _, task := range tg.Tasks {
		if n := countStartupChecks(task.Services); n > 1 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf(
				"Task %s may have at most one check with role %q, found %d", task.Name, CheckRoleStartup, n))
		}
	}

	return mErr.ErrorOrNil()
}

// checkRoleWarnings warns about startup and readiness checks that will not
// count toward deployment health because of on_update = "ignore".
func (tg *TaskGroup) checkRoleWarnings() error {
	var mErr multierror.Error

	warn := func(services []*Service) {
		for _, service := range services {
			for _, check := range service.Checks {
				if check.OnUpdate != OnUpdateIgnore {
					continue
				}
				if check.HasRole(CheckRoleStartup) || check.HasRole(CheckRoleReadiness) {
					mErr.Errors = append(mErr.Errors, fmt.Errorf(
						"Service %q check %q has role %q, but on_update = %q keeps it from counting toward deployment health",
						service.Name, check.Name, check.Role, OnUpdateIgnore))
				}
			}
		}
	}

	warn(tg.Services)
	for _, task := range tg.Tasks {
		warn(task.Services)
	}

	return mErr.ErrorOrNil()
}

// hashStringIfNonEmptyNamed hashes s prefixed by name, if s is not empty.
func hashStringIfNonEmptyNamed(h hash.Hash, name, s string) {
	if s != "" {
		hashString(h, name+":"+s)
	}
}
