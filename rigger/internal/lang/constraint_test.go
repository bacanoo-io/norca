// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"strings"
	"testing"
)

// A validation has one item block and one attribute block of a name, at
// every depth. A second one is an error and does not replace the first.
func TestLoad_DuplicateConstraintBlocks(t *testing.T) {
	t.Parallel()

	const (
		list   = "variable \"a\" {\n  type = list(string)\n  validation {\n"
		object = "variable \"a\" {\n  type = object({ x = number, y = list(string) })\n  validation {\n"
		end    = "  }\n}\n"
	)

	tests := []struct {
		name      string
		variables string
		expected  string
	}{
		{
			name:      "one item block",
			variables: list + "    item {\n      min_length = 1\n      max_length = 3\n    }\n" + end,
		},
		{
			name:      "attribute blocks of two names",
			variables: object + "    attribute \"x\" {\n      min = 1\n    }\n    attribute \"y\" {\n      min_items = 1\n    }\n" + end,
		},
		{
			name:      "two item blocks",
			variables: list + "    item {\n      min_length = 1\n    }\n    item {\n      max_length = 3\n    }\n" + end,
			expected:  "Duplicate item block; The block has an item block already at variables.hcl:4,5-9",
		},
		{
			name:      "two attribute blocks of one name",
			variables: object + "    attribute \"x\" {\n      min = 1\n    }\n    attribute \"x\" {\n      max = 3\n    }\n" + end,
			expected:  `Duplicate attribute block; The attribute "x" has a block already at variables.hcl:4,5-18`,
		},
		{
			name:      "two item blocks in an attribute block",
			variables: object + "    attribute \"y\" {\n      item {\n        min_length = 1\n      }\n      item {\n        max_length = 3\n      }\n    }\n" + end,
			expected:  "Duplicate item block; The block has an item block already at variables.hcl:5,7-11",
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

// The first item block stays when a second is refused.
func TestLoad_DuplicateConstraintBlocks_FirstKept(t *testing.T) {
	t.Parallel()

	m, diags := loadVariables(t, "variable \"a\" {\n  type = list(string)\n  validation {\n    item {\n      min_length = 1\n    }\n    item {\n      max_length = 3\n    }\n  }\n}\n")
	if !strings.Contains(diags, "Duplicate item block") {
		t.Fatalf("diagnostics = %q, want the duplicate named", diags)
	}

	v, ok := m.Variable("a")
	if !ok || v.Validation == nil || v.Validation.Item == nil {
		t.Fatalf("the variable a = %+v, want it loaded with its item block", v)
	}

	if item := v.Validation.Item; item.MinLength == nil || item.MaxLength != nil {
		t.Errorf("item = %+v, want the first block: min_length and no max_length", item)
	}
}
