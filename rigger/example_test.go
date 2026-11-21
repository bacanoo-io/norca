// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// A host builds the engine from the adapters it has. The rigger program passes
// its adapters over the HTTP API of Norca; a test passes fakes.
func ExampleNew() {
	limits := rigger.DefaultLimits()
	limits.Duration = 10 * time.Second

	eng, err := rigger.New(
		rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithLimits(limits),
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
	_, err := rigger.New(rigger.WithCluster(nil))

	fmt.Println(errors.Is(err, rigger.ErrInvalidOption))
	fmt.Println(err)
	// Output:
	// true
	// rigger: invalid option: nil cluster
}
