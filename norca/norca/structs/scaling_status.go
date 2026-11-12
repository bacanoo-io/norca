// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

// ScalingPolicyStatus is the last evaluation of a scaling policy by the
// built-in autoscaler. It is kept in the memory of the leader, not in the
// state: after a leader change a policy has none until it is evaluated
// again, and a policy of another engine never has one.
type ScalingPolicyStatus struct {
	// Time is when the policy was evaluated, in Unix nanoseconds.
	Time int64

	// Desired is the count that was asked for (the count of the group) and
	// Running what ran of it; both are zero when the evaluation was held
	// before the target was read.
	Desired int64
	Running int64

	// Checks are the checks of the policy that were run.
	Checks []*ScalingCheckStatus

	// Count is the result of the checks and Direction ("up", "down",
	// "none") where it lies from Desired; zero and "none" when the checks
	// ask for no change or were not run.
	Count     int64
	Direction string

	// Outcome is "scaled", "none", "held" or "error". HoldReason says why an
	// evaluation was held ("deployment", "canary", "multiregion",
	// "cooldown", "suspended", "settling", "no data", "quota", "rate
	// limit", "schedule", ...), HoldSince since when without a break (Unix
	// nanoseconds), and Message gives the detail of a hold or an error.
	Outcome    string
	HoldReason string
	HoldSince  int64
	Message    string
}

// ScalingCheckStatus is what one check of a policy gave in its last
// evaluation.
type ScalingCheckStatus struct {
	Name string

	// Value is the latest value the check read, as measured; HasValue is
	// false for a check without a measured value or without data.
	Value    float64
	HasValue bool

	// Direction is the vote of the check ("up", "down", "none", or "no
	// data" for a check left out because its source gave no value) and
	// Count the count it asks for when it votes for a change.
	Direction string
	Count     int64
}

// ScalingPolicyStatusResponse is the reply of Scaling.GetPolicyStatus. Status
// is nil for a policy that was not evaluated by the built-in autoscaler since
// the leader was elected.
type ScalingPolicyStatusResponse struct {
	Status *ScalingPolicyStatus
	QueryMeta
}
