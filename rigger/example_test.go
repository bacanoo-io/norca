// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig_test

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	rig "github.com/bacanoo-io/norca/rigger"
)

// A host builds the engine from the adapters it has. The rig program passes
// its adapters over the HTTP API of Norca; a test passes fakes.
func ExampleNew() {
	limits := rig.DefaultLimits()
	limits.Duration = 10 * time.Second

	eng, err := rig.New(
		rig.WithLogger(slog.New(slog.DiscardHandler)),
		rig.WithLimits(limits),
	)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(eng != nil)
	// Output: true
}

// An invalid option fails New with an error that matches ErrInvalidOption.
func ExampleNew_invalidOption() {
	_, err := rig.New(rig.WithCluster(nil))

	fmt.Println(errors.Is(err, rig.ErrInvalidOption))
	fmt.Println(err)
	// Output:
	// true
	// rig: invalid option: nil cluster
}
