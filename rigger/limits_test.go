// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultLimits(t *testing.T) {
	t.Parallel()

	if err := DefaultLimits().validate(); err != nil {
		t.Errorf("DefaultLimits() is invalid: %v", err)
	}
}

func TestLimits_validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		change   func(*Limits)
		expected string
	}{
		{name: "module bytes", change: func(l *Limits) { l.ModuleBytes = 0 }, expected: "module bytes 0"},
		{name: "file bytes", change: func(l *Limits) { l.FileBytes = -1 }, expected: "file bytes -1"},
		{name: "resources", change: func(l *Limits) { l.Resources = 0 }, expected: "resources 0"},
		{name: "depth", change: func(l *Limits) { l.Depth = 0 }, expected: "depth 0"},
		{name: "duration", change: func(l *Limits) { l.Duration = -time.Second }, expected: "duration -1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := DefaultLimits()
			tt.change(&l)

			err := l.validate()
			if err == nil {
				t.Fatal("validate() = nil, want an error")
			}

			if !strings.Contains(err.Error(), tt.expected) {
				t.Errorf("validate() = %q, want it to name %q", err, tt.expected)
			}
		})
	}
}
