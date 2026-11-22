// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
)

// What a few bytes of a module multiply is bounded before it is made, not
// counted after: the memory is gone by then.
func TestEvaluate_Expansion(t *testing.T) {
	t.Parallel()

	const (
		tooMany   = "more than 10 resources"
		tooBlocks = "more than 100000 blocks"
		// Three levels of 50 are 125000 blocks.
		nested = `  dynamic "a" {
    for_each = range(50)
    content {
      dynamic "b" {
        for_each = range(50)
        content {
          dynamic "c" {
            for_each = range(50)
            content {
            }
          }
        }
      }
    }
  }
`
	)

	tests := []struct {
		name     string
		files    map[string]string
		expected string
	}{
		{
			name:     "a count that can't be allocated",
			files:    map[string]string{xACL: "policy \"x\" {\n  count = 1000000000000000000\n}\n"},
			expected: tooMany,
		},
		{
			// The body fails: the limit has to answer before any copy of
			// it is evaluated.
			name:     "count before its copies",
			files:    map[string]string{xACL: "policy \"x\" {\n  count = 11\n  rules = file(\"missing\")\n}\n"},
			expected: tooMany,
		},
		{
			name:     "for_each before its copies",
			files:    map[string]string{xACL: "policy \"x\" {\n  for_each = toset([for i in range(11) : tostring(i)])\n  rules    = file(\"missing\")\n}\n"},
			expected: tooMany,
		},
		{
			name:     "nested dynamic blocks",
			files:    map[string]string{"o.object.hcl": "object \"o\" {\n" + nested + "}\n"},
			expected: tooBlocks,
		},
		{
			name:     "nested dynamic blocks of a job",
			files:    map[string]string{appNorca: "job \"app\" {\n" + nested + "}\n"},
			expected: tooBlocks,
		},
		{
			name:     "nested dynamic blocks of rules",
			files:    map[string]string{xACL: "policy \"x\" {\n  rules {\n" + nested + "  }\n}\n"},
			expected: tooBlocks,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()

			_, diags := evaluate(t, module(tt.files), "")
			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %.200q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
