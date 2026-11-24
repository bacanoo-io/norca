// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// A variable that only a rule reads is used: its value decides whether the
// module evaluates. Such a variable is a gate the installer sets, for
// example the acceptance of a license.
func TestUnusedVariables(t *testing.T) {
	t.Parallel()

	m, diags := lang.Load(t.Context(), fstest.MapFS{
		"metadata.hcl": {Data: []byte(metadata)},
		"variables.hcl": {Data: []byte(`variable "in_local" {}
variable "in_dynamic" {}
variable "in_output" {}
variable "in_rule" {}
variable "nowhere" {}

rule "r" {
  condition     = var.in_rule != ""
  error_message = "x"
}
`)},
		"locals.hcl":  {Data: []byte("locals {\n  a = var.in_local\n}\n")},
		"outputs.hcl": {Data: []byte("output \"o\" {\n  value = upper(var.in_output)\n}\n")},
		"x.object.hcl": {Data: []byte(`object "x" {
  dynamic "y" {
    for_each = var.in_dynamic
    content {}
  }
}
`)},
	}, limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var names []string

	for _, d := range lang.UnusedVariables(m) {
		if d.Severity != hcl.DiagWarning {
			t.Errorf("severity = %v, want a warning", d.Severity)
		}

		names = append(names, strings.Fields(d.Detail)[0])
	}

	if got := strings.Join(names, " "); got != "var.nowhere" {
		t.Errorf("unused = %q, want var.nowhere", got)
	}
}

// The examples use every variable.
func TestUnusedVariables_Examples(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"module/web":                     "",
		"registry/modules/redis":         "",
		"registry/modules/nats":          "",
		"registry/modules/postgres":      "",
		"registry/modules/lib/placement": "",
	}

	for dir, expected := range tests {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			m, diags := lang.LoadAt(t.Context(), os.DirFS("../../examples"), dir, limits)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			var names []string
			for _, d := range lang.UnusedVariables(m) {
				names = append(names, strings.Fields(d.Detail)[0])
			}

			if got := strings.Join(names, " "); got != expected {
				t.Errorf("unused = %q, want %q", got, expected)
			}
		})
	}
}
