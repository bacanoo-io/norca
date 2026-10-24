// Copyright IBM Corp. 2020, 2025
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 sdk/target.go. Modified: cluster
// scaling (node pool) keys and node selector strategies dropped; cluster
// scaling is not part of the built-in autoscaler yet.

package sdk

import (
	"fmt"
)

// TargetScalingNoOpError is a special error type that can be used by target
// plugins to indicate that a scaling request didn't result in any action, but
// didn't fail either.
// This can be used to avoid post-scaling actions such as placing the policy in
// cooldown.
type TargetScalingNoOpError struct {
	Err error
}

// NewTargetScalingNoOpError returns a new target scaling no-op error with the
// provided formatted message.
func NewTargetScalingNoOpError(msg string, args ...interface{}) *TargetScalingNoOpError {
	return &TargetScalingNoOpError{Err: fmt.Errorf(msg, args...)}
}

// Error implements the error interface.
func (n *TargetScalingNoOpError) Error() string {
	return n.Err.Error()
}

// TargetStatus is the response object when performing the Status call of the
// target plugin interface. The response details key information about the
// current state of the target.
type TargetStatus struct {

	// Ready indicates whether the target is currently in a state where scaling
	// is permitted.
	Ready bool

	// Count is the current value of the target and thus performs the current
	// state basis when performing strategy calculations to identify the
	// desired state.
	Count int64

	// Meta is a mapping that provides additional information about the target
	// that can be used during the policy evaluation to ensure the correct
	// calculations and logic are applied to the target.
	Meta map[string]string
}

const (
	// TargetStatusMetaKeyLastEvent is an optional meta key that can be added
	// to the status return. The value represents the last scaling event of the
	// target as seen by the remote providers view point. This helps enforce
	// cooldown where out-of-band scaling activities have been triggered.
	TargetStatusMetaKeyLastEvent = "nomad_autoscaler.last_event"

	// TargetConfigKeyNamespace is the config key used within horizontal app
	// scaling to identify the Nomad namespace targeted for autoscaling.
	TargetConfigKeyNamespace = "Namespace"

	// TargetConfigKeyJob is the config key used within horizontal app scaling
	// to identify the Nomad job targeted for autoscaling.
	TargetConfigKeyJob = "Job"

	// TargetConfigKeyTaskGroup is the config key used within horizontal app
	// scaling to identify the Nomad job group targeted for autoscaling.
	TargetConfigKeyTaskGroup = "Group"
)
