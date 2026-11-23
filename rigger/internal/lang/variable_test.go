// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// loadVariables loads a module of the variables.hcl given.
func loadVariables(t *testing.T, variables string) (*lang.Module, string) {
	t.Helper()

	m, diags := lang.Load(t.Context(), fstest.MapFS{
		metadataHCL:  {Data: []byte(metadata)},
		variablesHCL: {Data: []byte(variables)},
	}, limits)

	var all strings.Builder
	for _, d := range diags {
		all.WriteString(d.Error() + "\n")
	}

	return m, all.String()
}

// A variable has one validation, one ui and one secret block. A second one
// is an error: read in silence it replaced the first, and a module lost the
// lower bound its author wrote.
func TestLoad_DuplicateVariableBlocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		variables string
		expected  string
	}{
		{
			name:      "one block of each",
			variables: "variable \"a\" {\n  type = number\n  validation {\n    min = 5\n    max = 10\n  }\n  ui {\n    label = \"A\"\n  }\n  secret {\n    path = \"app\"\n    item = \"a\"\n  }\n}\n",
		},
		{
			name:      "two validation blocks",
			variables: "variable \"a\" {\n  type = number\n  validation {\n    min = 5\n  }\n  validation {\n    max = 10\n  }\n}\n",
			expected:  `Duplicate validation block; The variable "a" has a validation block already at variables.hcl:3,3-13`,
		},
		{
			name:      "two ui blocks",
			variables: "variable \"a\" {\n  ui {\n    label = \"A\"\n  }\n  ui {\n    order = 2\n  }\n}\n",
			expected:  `Duplicate ui block; The variable "a" has a ui block already at variables.hcl:2,3-5`,
		},
		{
			name:      "two secret blocks",
			variables: "variable \"a\" {\n  secret {\n    path = \"app\"\n    item = \"a\"\n  }\n  secret {\n    path = \"other\"\n    item = \"b\"\n  }\n}\n",
			expected:  `Duplicate secret block; The variable "a" has a secret block already at variables.hcl:2,3-9`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := loadVariables(t, tt.variables)

			if tt.expected == "" {
				if diags != "" {
					t.Fatalf("Load() = %s, want no diagnostics", diags)
				}

				return
			}

			if !strings.Contains(diags, tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags, tt.expected)
			}
		})
	}
}

// The first block stays the block of the variable when a second is refused.
func TestLoad_DuplicateVariableBlocks_FirstKept(t *testing.T) {
	t.Parallel()

	m, diags := loadVariables(t, "variable \"a\" {\n  type = number\n  validation {\n    min = 5\n  }\n  validation {\n    max = 10\n  }\n  secret {\n    path = \"app\"\n    item = \"a\"\n  }\n  secret {\n    path = \"other\"\n    item = \"b\"\n  }\n}\n")
	if !strings.Contains(diags, "Duplicate validation block") || !strings.Contains(diags, "Duplicate secret block") {
		t.Fatalf("diagnostics = %q, want both duplicates named", diags)
	}

	v, ok := m.Variable("a")
	if !ok {
		t.Fatal("the variable a is not loaded")
	}

	if v.Validation == nil || v.Validation.Min == nil || v.Validation.Max != nil {
		t.Errorf("validation = %+v, want the first block: min and no max", v.Validation)
	}

	if v.Secret == nil || v.Secret.Path != "app" {
		t.Errorf("secret = %+v, want the first block: path app", v.Secret)
	}
}
