// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The outcomes of an evaluation.
const (
	// OutcomeScaled: the count was changed (or would have been, in a dry
	// run).
	OutcomeScaled = "scaled"

	// OutcomeNone: the checks ask for the count the target has.
	OutcomeNone = "none"

	// OutcomeHeld: the evaluation was not made, or its result not acted on;
	// HoldReason says why.
	OutcomeHeld = "held"

	// OutcomeError: the evaluation failed.
	OutcomeError = "error"
)

// The reasons an evaluation is held for.
const (
	HoldSchedule    = "schedule"    // outside the window of the policy
	HoldNotReady    = "not ready"   // the job is stopped or the object is being deleted
	HoldDeployment  = "deployment"  // a deployment of the job runs
	HoldCanary      = "canary"      // canaries wait for promotion
	HoldMultiregion = "multiregion" // the region waits for its turn in a multiregion rollout
	HoldCooldown    = "cooldown"    // cooldown after a change of the count
	HoldSuspended   = "suspended"   // suspended for oscillating
	HoldSettling    = "settling"    // scale down held: the target does not run what was asked for
	HoldNoData      = "no data"     // no check has a value to decide on
	HoldQuota       = "quota"       // the namespace quota has no room
	HoldRateLimit   = "rate limit"  // dropped by the cluster-wide limit of scaling actions
	HoldNodeGroup   = "scale_down"  // a node group is lowered by its scale_down block only
	HoldPolicy      = "policy"      // the policy was disabled or changed while it was evaluated
)

// checkNoData is the direction of a check in an Evaluation whose source gave
// no value and that was left out of the decision (on_error = "ignore").
const checkNoData = "no data"

// DefaultHoldWarn is how long an evaluation is held before a ScalingHeld
// event tells about it.
const DefaultHoldWarn = 10 * time.Minute

// CheckEvaluation is what one check of a policy gave in an evaluation.
type CheckEvaluation struct {
	Name string

	// Value is the latest value the check read, as measured (before it is
	// restated for the desired count); HasValue is false for a check without
	// a measured value (fixed-value) or without data.
	Value    float64
	HasValue bool

	// Direction is the vote of the check ("up", "down", "none"), or "no
	// data" for a check that was left out because its source gave no value;
	// Count is the count it asks for when it votes for a change.
	Direction string
	Count     int64
}

// Evaluation is the last evaluation of a policy. It is kept in the memory of
// the leader that runs the autoscaler: after a leader change a policy has
// none until it is evaluated again.
type Evaluation struct {
	// Time is when the policy was evaluated.
	Time time.Time

	// Desired is the count that was asked for and Running what ran; both
	// are zero when the evaluation was held before the target was read.
	Desired int64
	Running int64

	Checks []CheckEvaluation

	// Count is the result of the checks and Direction where it lies from
	// Desired; zero and "none" when the checks ask for no change or were not
	// run.
	Count     int64
	Direction string

	// Outcome is one of the Outcome constants; HoldReason one of the Hold
	// constants, HoldSince when the policy was first held for that reason
	// without a break, and Message the detail of a hold or an error.
	Outcome    string
	HoldReason string
	HoldSince  time.Time
	Message    string
}

// held marks the evaluation as held for a reason.
func (e *Evaluation) held(reason, message string) {
	e.Outcome, e.HoldReason, e.Message = OutcomeHeld, reason, message
}

// policyHold is the hold of a policy the status book tracks between
// evaluations.
type policyHold struct {
	reason string
	since  time.Time
	// isWarned is set once the ScalingHeld event of the hold was published.
	isWarned bool
}

// statusBook keeps the last evaluation of every policy and tells about holds
// that last: a ScalingHeld event when a policy has been held for the same
// reason for holdWarn, and one more when that hold ends.
type statusBook struct {
	holdWarn time.Duration
	publish  EventPublisher

	l     sync.Mutex
	last  map[string]*Evaluation
	holds map[string]*policyHold
}

func newStatusBook(holdWarn time.Duration, publish EventPublisher) *statusBook {
	if holdWarn <= 0 {
		holdWarn = DefaultHoldWarn
	}
	return &statusBook{
		holdWarn: holdWarn,
		publish:  publish,
		last:     map[string]*Evaluation{},
		holds:    map[string]*policyHold{},
	}
}

// record stores the evaluation of a policy and publishes the events of its
// hold.
func (b *statusBook) record(policyID string, key targetKey, eval *Evaluation) {
	if eval.Direction == "" {
		eval.Direction = "none"
	}

	b.l.Lock()
	hold := b.holds[policyID]
	var ended *policyHold
	if hold != nil && (eval.Outcome != OutcomeHeld || eval.HoldReason != hold.reason) {
		ended, hold = hold, nil
		delete(b.holds, policyID)
	}
	isWarning := false
	if eval.Outcome == OutcomeHeld {
		if hold == nil {
			hold = &policyHold{reason: eval.HoldReason, since: eval.Time}
			b.holds[policyID] = hold
		}
		eval.HoldSince = hold.since
		if !hold.isWarned && eval.Time.Sub(hold.since) >= b.holdWarn {
			hold.isWarned, isWarning = true, true
		}
	}
	b.last[policyID] = eval
	b.l.Unlock()

	if b.publish == nil {
		return
	}
	event := func(reason string) *structs.ScalingDecisionEvent {
		return &structs.ScalingDecisionEvent{
			Namespace:     key.namespace,
			JobID:         key.job,
			Group:         key.group,
			PolicyID:      policyID,
			PreviousCount: eval.Desired,
			Count:         eval.Desired,
			DesiredCount:  eval.Count,
			Direction:     eval.Direction,
			Reason:        reason,
			Time:          eval.Time.UnixNano(),
		}
	}
	if ended != nil && ended.isWarned {
		b.publish(structs.TypeScalingHeld, event(fmt.Sprintf("%s: the hold since %s ended",
			ended.reason, ended.since.UTC().Format(time.RFC3339))))
	}
	if isWarning {
		reason := fmt.Sprintf("%s: held since %s", eval.HoldReason, eval.HoldSince.UTC().Format(time.RFC3339))
		if eval.Message != "" {
			reason += ": " + eval.Message
		}
		b.publish(structs.TypeScalingHeld, event(reason))
	}
}

// get returns a copy of the last evaluation of a policy, nil if it has none.
func (b *statusBook) get(policyID string) *Evaluation {
	b.l.Lock()
	defer b.l.Unlock()
	eval := b.last[policyID]
	if eval == nil {
		return nil
	}
	out := *eval
	out.Checks = append([]CheckEvaluation(nil), eval.Checks...)
	return &out
}

// forget drops a policy that is gone.
func (b *statusBook) forget(policyID string) {
	b.l.Lock()
	defer b.l.Unlock()
	delete(b.last, policyID)
	delete(b.holds, policyID)
}
