// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"testing"

	"github.com/shoenig/test/must"
)

// The object store of the bundles of Rigger takes 1GiB of the default
// storage, half of what the other streams leave of a small one, and what
// bus.rigger_bundles_max_bytes says when it is set; the plan of a small
// storage still fits.
func TestConfig_RiggerBundlesBytes(t *testing.T) {
	t.Parallel()

	def := DefaultConfig()
	must.Eq(t, int64(DefaultRiggerBundlesMaxBytes), def.RiggerBundlesBytes())
	must.LessEq(t, def.MaxStorage, def.PlannedStorage())

	small := DefaultConfig()
	small.MaxStorage = 128 << 20
	small.Events.MaxBytes = 16 << 20
	small.ConsulKVMaxBytes = 16 << 20
	must.Eq(t, (small.MaxStorage-small.streamsStorage())/2, small.RiggerBundlesBytes())
	must.Positive(t, small.RiggerBundlesBytes())
	must.LessEq(t, small.MaxStorage, small.PlannedStorage())

	// The other streams take all of it: no object store of the bundles.
	full := DefaultConfig()
	full.MaxStorage = 128 << 20
	full.Events.MaxBytes = 32 << 20
	full.ConsulKVMaxBytes = 16 << 20
	must.Eq(t, 0, full.RiggerBundlesBytes())
	must.LessEq(t, full.MaxStorage, full.PlannedStorage())

	set := DefaultConfig()
	set.RiggerBundlesMaxBytes = 300 << 20
	must.Eq(t, int64(300<<20), set.RiggerBundlesBytes())
}
