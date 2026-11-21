// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"errors"
	"fmt"
	"time"
)

// Limits bound the work of the engine for one module or stack, so that a
// module written by someone else can't exhaust the host that evaluates it
// (design 12.4). The host sets them.
type Limits struct {
	// ModuleBytes bounds the size of all files of a module.
	ModuleBytes int64
	// FileBytes bounds the size of one file of a module.
	FileBytes int64
	// Resources bounds the number of resources of an evaluated module.
	Resources int
	// Depth bounds the nesting of child modules.
	Depth int
	// Duration bounds the time of one evaluation.
	Duration time.Duration
}

// DefaultLimits returns the limits the engine uses unless the host sets
// others. They are starting values, to be tuned on the modules converted
// from the packs (design 14).
func DefaultLimits() Limits {
	return Limits{
		ModuleBytes: 16 << 20,
		FileBytes:   1 << 20,
		Resources:   1000,
		Depth:       8,
		Duration:    30 * time.Second,
	}
}

func (l Limits) validate() error {
	var errs []error

	if l.ModuleBytes <= 0 {
		errs = append(errs, fmt.Errorf("module bytes %d is not positive", l.ModuleBytes))
	}

	if l.FileBytes <= 0 {
		errs = append(errs, fmt.Errorf("file bytes %d is not positive", l.FileBytes))
	}

	if l.Resources <= 0 {
		errs = append(errs, fmt.Errorf("resources %d is not positive", l.Resources))
	}

	if l.Depth <= 0 {
		errs = append(errs, fmt.Errorf("depth %d is not positive", l.Depth))
	}

	if l.Duration <= 0 {
		errs = append(errs, fmt.Errorf("duration %s is not positive", l.Duration))
	}

	return errors.Join(errs...)
}
