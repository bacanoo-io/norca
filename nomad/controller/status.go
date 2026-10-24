// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"slices"
	"time"
)

// Standard condition types of the status conventions.
const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// Condition status values.
const (
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"
)

// Condition is one aspect of the observed state of an object.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// CommonStatus holds the status fields every object should have. Embed it in
// the status type of a kind:
//
//	type MyStatus struct {
//		controller.CommonStatus
//		Address string `json:"address,omitempty"`
//	}
type CommonStatus struct {
	// ObservedSpecHash is the SpecHash of the spec the controller acted on
	// last. It stands in for observedGeneration until objects get a server
	// maintained generation (phase 2).
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`

	// Conditions are the standard conditions (Ready, Progressing, Degraded)
	// and kind specific ones.
	Conditions []Condition `json:"conditions,omitempty"`
}

// SetCondition adds or replaces the condition of the same type. The
// transition time is kept when the status of the condition does not change,
// and set to now otherwise (or when it is zero).
func (s *CommonStatus) SetCondition(c Condition, now time.Time) {
	for i := range s.Conditions {
		if s.Conditions[i].Type != c.Type {
			continue
		}
		if s.Conditions[i].Status == c.Status && c.LastTransitionTime.IsZero() {
			c.LastTransitionTime = s.Conditions[i].LastTransitionTime
		}
		if c.LastTransitionTime.IsZero() {
			c.LastTransitionTime = now.UTC().Truncate(time.Second)
		}
		s.Conditions[i] = c
		return
	}
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = now.UTC().Truncate(time.Second)
	}
	s.Conditions = append(s.Conditions, c)
}

// Condition returns a copy of the condition of the given type, or nil.
func (s CommonStatus) Condition(conditionType string) *Condition {
	i := slices.IndexFunc(s.Conditions, func(c Condition) bool { return c.Type == conditionType })
	if i < 0 {
		return nil
	}
	c := s.Conditions[i]
	return &c
}

// IsTrue reports whether the condition of the given type is True.
func (s CommonStatus) IsTrue(conditionType string) bool {
	c := s.Condition(conditionType)
	return c != nil && c.Status == ConditionTrue
}
