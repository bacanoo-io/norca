// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"fmt"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A claim refused because the volume is held on another node is not retried:
// the allocation was placed on the wrong node, and waiting does not help
// (review finding SCH-7). A full volume is still waited for.
func TestCSIHook_ClaimRetry(t *testing.T) {
	ci.Parallel(t)

	rpc := func(err error) error {
		return fmt.Errorf("rpc error: CSI.Claim: %v", err)
	}
	must.True(t, isRetryableClaimRPCError(rpc(structs.ErrCSIVolumeMaxClaims)))
	must.False(t, isRetryableClaimRPCError(rpc(structs.ErrCSIVolumeClaimedOnOtherNode)))
}
