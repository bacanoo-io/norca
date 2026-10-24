// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"time"

	"github.com/mitchellh/mapstructure"
)

const (
	// TopicScaling is the event stream topic of the decisions of the
	// built-in autoscaler.
	TopicScaling Topic = "Scaling"

	// ScaleDuringDeploymentUpOnly allows the built-in autoscaler to scale a
	// group up while a deployment of its job is active.
	ScaleDuringDeploymentUpOnly = "up_only"
)

// ScalingCheck is one check of a typed scaling policy of the built-in
// autoscaler.
type ScalingCheck struct {
	Name              string
	Source            string            `json:",omitempty"`
	Metric            string            `json:",omitempty"`
	Query             string            `json:",omitempty"`
	QueryWindow       time.Duration     `json:",omitempty"`
	QueryWindowOffset time.Duration     `json:",omitempty"`
	QueryInstant      bool              `json:",omitempty"`
	Group             string            `json:",omitempty"`
	Strategy          string            `json:",omitempty"`
	StrategyConfig    map[string]string `json:",omitempty"`
	OnError           string            `json:",omitempty"`
}

// ScalingDecisionEvent is the payload of the events of the Scaling topic.
type ScalingDecisionEvent struct {
	Namespace     string
	JobID         string
	Group         string
	PolicyID      string
	PreviousCount int64
	Count         int64
	DesiredCount  int64
	Direction     string
	Check         string
	Reason        string
	DryRun        bool
	Quota         string
	Time          int64
}

// ScalingDecision returns the payload of an event of the Scaling topic.
func (e *Event) ScalingDecision() (*ScalingDecisionEvent, error) {
	var out ScalingDecisionEvent
	if err := mapstructure.Decode(e.Payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
