// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestCheckThresholds(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, newCheckThresholds(&structs.ServiceCheck{}))

	th := newCheckThresholds(&structs.ServiceCheck{SuccessBeforePassing: 2, FailuresBeforeCritical: 3})

	run := func(status structs.CheckStatus) structs.CheckStatus {
		result := &structs.CheckQueryResult{Status: status}
		th.apply(result)
		return result.Status
	}

	// two successes before passing
	must.Eq(t, structs.CheckPending, run(structs.CheckSuccess))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckSuccess))

	// three failures before critical
	must.Eq(t, structs.CheckSuccess, run(structs.CheckFailure))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckFailure))
	must.Eq(t, structs.CheckFailure, run(structs.CheckFailure))

	// a single success does not make it pass again
	must.Eq(t, structs.CheckFailure, run(structs.CheckSuccess))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckSuccess))

	// a failure in between resets the count
	must.Eq(t, structs.CheckSuccess, run(structs.CheckFailure))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckSuccess))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckFailure))
	must.Eq(t, structs.CheckSuccess, run(structs.CheckFailure))
	must.Eq(t, structs.CheckFailure, run(structs.CheckFailure))

	// a nil threshold leaves results alone
	var none *checkThresholds
	result := &structs.CheckQueryResult{Status: structs.CheckFailure}
	none.apply(result)
	must.Eq(t, structs.CheckFailure, result.Status)
}
