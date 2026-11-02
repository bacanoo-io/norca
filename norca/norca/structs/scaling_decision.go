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
