// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"os"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	rigger "github.com/bacanoo-io/norca/rigger"
)

func TestEngine_Evaluate(t *testing.T) {
	t.Parallel()

	eng := engine(t)

	m, diags := eng.LoadModule(t.Context(), os.DirFS("examples/registry/modules/redis"))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	tests := []struct {
		name     string
		install  rigger.Install
		expected string
	}{
		{
			name:     "missing facts",
			install:  rigger.Install{Release: "cache"},
			expected: "Missing namespace; An install names its namespace.",
		},
		{
			name:     "a job without a decoder",
			install:  rigger.Install{Release: "cache", Namespace: testNamespace, Region: "eu"},
			expected: "No job decoder",
		},
		{
			name: "invalid values",
			install: rigger.Install{Release: "cache", Namespace: testNamespace, Region: "eu", Values: rigger.Values{
				"count": {Value: cty.NumberIntVal(0)},
			}},
			expected: "var.count is 0, less than 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := eng.Evaluate(t.Context(), m, tt.install)
			if res != nil {
				t.Errorf("Evaluate() = %+v, want no result", res)
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
