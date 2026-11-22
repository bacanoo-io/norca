// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
)

// sensitiveText is what no message may show.
const sensitiveText = "do-not-show-me"

// An error never quotes a sensitive value (design 6.3): diagnostics go to
// the response of a server and into its log.
func TestEvaluate_SensitiveNotInErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr string
	}{
		{name: "parseint", expr: `parseint(var.v, 10)`},
		{name: "timeadd", expr: `timeadd(var.v, "1h")`},
		{name: "file", expr: `file(var.v)`},
		{name: "templatefile", expr: `templatefile("files/t.tpl", { v = var.v })`},
		{name: "try", expr: `try(parseint(var.v, 10))`},
		{name: "lookup", expr: `lookup({ a = "b" }, var.v)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := module(map[string]string{
				variablesHCL:  "variable \"v\" {\n  sensitive = true\n  default   = \"" + sensitiveText + "\"\n}\n",
				"files/t.tpl": "${parseint(v, 10)}",
				"y.var.hcl":   "variable \"y\" {\n  path = \"p\"\n\n  items {\n    a = " + tt.expr + "\n  }\n}\n",
			})

			_, diags := evaluate(t, fsys, "")
			if !diags.HasErrors() {
				t.Fatal("no diagnostics; the expression is meant to fail")
			}

			if strings.Contains(diags.Error(), sensitiveText) {
				t.Errorf("diagnostics = %q, they show the sensitive value", diags.Error())
			}
		})
	}
}

// A value that is sensitive where it comes from is hidden in the message of
// a validation, whatever the variable it is given to says.
func TestVariables_SensitiveNotInErrors(t *testing.T) {
	t.Parallel()

	secret := cty.StringVal(sensitiveText).Mark(eval.Sensitive)

	tests := []struct {
		name     string
		variable string
		value    cty.Value
	}{
		{name: "pattern", variable: "type = string\n  validation {\n    pattern = \"^[0-9]+$\"\n  }", value: secret},
		{name: "one_of", variable: "type = string\n  validation {\n    one_of = [\"x\"]\n  }", value: secret},
		{
			name:     "item_of",
			variable: "type = list(string)\n  validation {\n    item_of = [\"x\"]\n  }",
			value:    cty.ListVal([]cty.Value{secret}),
		},
		{
			name:     "item of a sensitive list",
			variable: "type = list(string)\n  validation {\n    item_of = [\"x\"]\n  }",
			value:    cty.ListVal([]cty.Value{cty.StringVal(sensitiveText)}).Mark(eval.Sensitive),
		},
		{
			name:     "key of a sensitive map",
			variable: "type = map(string)\n  validation {\n    key_pattern = \"^[0-9]+$\"\n  }",
			value:    cty.MapVal(map[string]cty.Value{sensitiveText: cty.StringVal("x")}).Mark(eval.Sensitive),
		},
		{
			name:     "path of an item of a sensitive map",
			variable: "type = map(string)\n  validation {\n    item {\n      pattern = \"^[0-9]+$\"\n    }\n  }",
			value:    cty.MapVal(map[string]cty.Value{sensitiveText: cty.StringVal("x")}).Mark(eval.Sensitive),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := load(t, module(map[string]string{
				variablesHCL: "variable \"v\" {\n  " + tt.variable + "\n}\n",
			}))

			_, diags := eval.Variables(m, map[string]eval.Input{"v": {Value: tt.value}})
			if !diags.HasErrors() {
				t.Fatal("no diagnostics; the value is meant to fail")
			}

			if strings.Contains(diags.Error(), sensitiveText) {
				t.Errorf("diagnostics = %q, they show the sensitive value", diags.Error())
			}
		})
	}
}
