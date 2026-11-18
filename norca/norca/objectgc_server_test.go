// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/shoenig/test/must"
)

// TestObjectGCConfig checks that the tombstone retention of the object
// garbage collector comes from the server configuration (review finding
// MUK-7), and the rest of its configuration from its defaults.
func TestObjectGCConfig(t *testing.T) {
	ci.Parallel(t)

	defaults := objectgc.DefaultConfig()
	c := DefaultConfig()
	must.Eq(t, 5*time.Minute, c.ObjectTombstoneGCThreshold)
	must.Eq(t, defaults, objectGCConfig(c))

	c.ObjectTombstoneGCThreshold = 2 * time.Hour
	got := objectGCConfig(c)
	must.Eq(t, 2*time.Hour, got.TombstoneRetention)
	must.Eq(t, defaults.Interval, got.Interval)
	must.Eq(t, defaults.IdleInterval, got.IdleInterval)
}
