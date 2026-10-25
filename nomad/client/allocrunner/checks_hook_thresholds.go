// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"fmt"

	"github.com/hashicorp/nomad/nomad/structs"
)

// checkThresholds applies the success_before_passing and
// failures_before_critical settings of a check to its results, in the manner
// of Consul: a check that is not passing becomes passing only after the given
// number of consecutive successes, and a passing check becomes failing only
// after the given number of consecutive failures. The status kept in the
// check store, and therefore seen by deployments, restarts and the catalog, is
// the one after the thresholds.
type checkThresholds struct {
	successBeforePassing   int
	failuresBeforeCritical int

	successes int
	failures  int

	// status is the last status after the thresholds
	status structs.CheckStatus
}

func newCheckThresholds(check *structs.ServiceCheck) *checkThresholds {
	if check.SuccessBeforePassing <= 0 && check.FailuresBeforeCritical <= 0 {
		return nil
	}
	return &checkThresholds{
		successBeforePassing:   check.SuccessBeforePassing,
		failuresBeforeCritical: check.FailuresBeforeCritical,
		status:                 structs.CheckPending,
	}
}

// apply updates the counters with result and adjusts its status according to
// the thresholds. A nil receiver leaves result as it is.
func (t *checkThresholds) apply(result *structs.CheckQueryResult) {
	if t == nil {
		return
	}

	switch result.Status {
	case structs.CheckSuccess:
		t.successes++
		t.failures = 0
		if t.status != structs.CheckSuccess && t.successes < t.successBeforePassing {
			result.Output = fmt.Sprintf("nomad: %d of %d successes required before passing; %s",
				t.successes, t.successBeforePassing, result.Output)
			result.Status = t.notPassing()
		}

	case structs.CheckFailure:
		t.failures++
		t.successes = 0
		if t.status == structs.CheckSuccess && t.failures < t.failuresBeforeCritical {
			result.Output = fmt.Sprintf("nomad: %d of %d failures required before critical; %s",
				t.failures, t.failuresBeforeCritical, result.Output)
			result.Status = structs.CheckSuccess
		}

	default:
		return
	}

	t.status = result.Status
}

// notPassing is the status reported while successes accumulate.
func (t *checkThresholds) notPassing() structs.CheckStatus {
	if t.status == structs.CheckFailure {
		return structs.CheckFailure
	}
	return structs.CheckPending
}
