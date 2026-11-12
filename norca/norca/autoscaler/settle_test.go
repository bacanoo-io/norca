// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestSettleTracker_Observe(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	key := targetKey{namespace: "default", job: "web", group: "web"}
	other := targetKey{namespace: "default", job: "api", group: "api"}
	tracker := newSettleTracker()

	// A settled target has no wait.
	must.Eq(t, 0, tracker.observe(key, true, base))

	// The wait starts when the target is first seen unsettled.
	must.Eq(t, 0, tracker.observe(key, false, base.Add(10*time.Second)))
	must.Eq(t, 50*time.Second, tracker.observe(key, false, base.Add(time.Minute)))

	// Targets are told apart.
	must.Eq(t, 0, tracker.observe(other, false, base.Add(time.Minute)))

	// Settling ends the wait; the next one starts from zero.
	must.Eq(t, 0, tracker.observe(key, true, base.Add(2*time.Minute)))
	must.Eq(t, 0, tracker.observe(key, false, base.Add(3*time.Minute)))
	must.Eq(t, time.Minute, tracker.observe(key, false, base.Add(4*time.Minute)))
}

func TestSettleTracker_Forget(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	key := targetKey{namespace: "default", job: "web", group: "web"}
	tracker := newSettleTracker()

	must.Eq(t, 0, tracker.observe(key, false, base))
	tracker.forget(key)

	// The wait of a forgotten target starts again.
	must.Eq(t, 0, tracker.observe(key, false, base.Add(time.Minute)))
}
