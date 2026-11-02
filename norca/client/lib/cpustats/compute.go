// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package cpustats describes the CPU capacity of a node in the units the
// scheduler uses (MHz), for drivers that turn a task's cpu value into a
// cgroup limit.
package cpustats

// Compute is the CPU capacity of the node.
type Compute struct {
	// TotalCompute is the capacity of all cores together in MHz, as the
	// CPU fingerprint reports it (the node's cpu resources).
	TotalCompute int

	// NumCores is the number of cores the capacity is spread over.
	NumCores int
}

// Usable reports whether both values are set, so that a per-core speed can
// be derived from them.
func (c Compute) Usable() bool {
	return c.TotalCompute > 0 && c.NumCores > 0
}

// PerCoreMHz is the speed of one core in MHz, or 0 when Compute is not
// usable.
func (c Compute) PerCoreMHz() int {
	if !c.Usable() {
		return 0
	}
	return c.TotalCompute / c.NumCores
}
