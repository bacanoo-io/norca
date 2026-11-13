// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package resources

import "time"

// A Containment will cleanup resources created by an executor.
type Containment interface {
	// Apply enables containment on pid.
	Apply(pid int) error

	// Cleanup will purge executor resources like cgroups.
	Cleanup() error

	// GetPIDs will return the processes overseen by the Containment
	GetPIDs() PIDs

	// CPUTime returns the CPU time all the processes overseen by the
	// Containment have used since it was applied, those that have exited
	// included. It is false when the Containment does not account for it.
	CPUTime() (user, system time.Duration, ok bool)
}
