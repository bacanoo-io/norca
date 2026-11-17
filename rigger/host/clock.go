// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import "time"

// Clock tells the time. Tests give the engine a clock they control.
type Clock interface {
	Now() time.Time
}

// SystemClock is the clock of the operating system.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time {
	return time.Now()
}
