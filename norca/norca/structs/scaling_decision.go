// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

const (
	// TopicScaling is the event stream topic of the decisions of the
	// built-in autoscaler. Its events are not written to Raft; they are
	// published by the leader that runs the autoscaler.
	TopicScaling Topic = "Scaling"

	// TypeScalingDecision is a scaling action of the built-in autoscaler
	// (including dry-run actions).
	TypeScalingDecision = "ScalingDecision"

	// TypeScalingQuotaCapped is published when a scale up was limited by
	// the quota of the job's namespace.
	TypeScalingQuotaCapped = "ScalingQuotaCapped"

	// TypeScalingNodeAllocationTooLarge is published by the blocked work
	// trigger when an allocation does not fit an empty node of a node group.
	// JobID and Group name the task group, Reason the size and the group.
	TypeScalingNodeAllocationTooLarge = "ScalingNodeAllocationTooLarge"

	// TypeScalingNodeUnverifiable is published by the blocked work trigger
	// when work is blocked by a constraint that cannot be checked against a
	// node that does not exist yet. No node is added for it; the fix is in
	// the job or in the node template. Reason names the constraint.
	TypeScalingNodeUnverifiable = "ScalingNodeUnverifiable"

	// TypeScalingNodeBlocked is published by the blocked work trigger when
	// work that cannot be placed is not helped by more nodes: the groups
	// that could take it are at their maximum or the limit of the pool, the
	// size of their node is unknown, or no group fits. Reason says which.
	TypeScalingNodeBlocked = "ScalingNodeBlocked"
)

// ScalingDecisionEvent is the payload of the events of the Scaling topic.
type ScalingDecisionEvent struct {
	Namespace string
	JobID     string
	Group     string
	PolicyID  string

	// PreviousCount is the count of the group the decision was based on and
	// Count the count decided. For a TypeScalingQuotaCapped event, Count is
	// the quota ceiling and DesiredCount the count proposed by the checks.
	PreviousCount int64
	Count         int64
	DesiredCount  int64

	// Direction is "up" or "down".
	Direction string

	// Check is the name of the check that decided the action, and Reason
	// the explanation of its strategy.
	Check  string
	Reason string

	// DryRun is set when the policy runs in dry-run mode and the count was
	// not changed.
	DryRun bool

	// Quota is the name of the quota that capped a scale up.
	Quota string

	// Time is the time of the decision, in Unix nanoseconds.
	Time int64
}
