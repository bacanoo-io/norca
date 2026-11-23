// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
)

// testModule is a module for the tests of the test runner.
var testModule = map[string]string{
	variablesHCL: `variable "n" {
  type    = number
  default = 2

  validation {
    max = 5
  }
}

variable "token" {
  type      = string
  default   = "t"
  sensitive = true
}
`,
	appNorca: `job "app" {
  group "app" {
    count = var.n

    task "app" {
      config {
        args = var.n > 3 ? ["big"] : null
      }
    }

    dynamic "volume" {
      for_each = []
      content {}
    }
  }
}
`,
	"w.norca.hcl": "job \"w\" {\n  count = var.n\n  id    = \"w${count.index}\"\n}\n",
	"x.var.hcl":   "variable \"x\" {\n  for_each = toset([\"a\", \"b\"])\n  path     = \"p/${each.key}\"\n\n  items {\n    token = var.token\n  }\n}\n",
	"outputs.hcl": "output \"names\" {\n  value = distinct([\"a\", \"b\", \"a\"])\n}\n",
}

func TestRunTest(t *testing.T) {
	t.Parallel()

	m := load(t, module(testModule))

	tests := []struct {
		name     string
		src      string
		failures string
	}{
		{name: "asserts that hold", src: `test "t" {
  release = "r"

  assert {
    condition     = resource.job.app.group.app.count == 2 && resource.job.app.group.app.task.app.config.args == null
    error_message = "count and a null attribute"
  }

  assert {
    condition     = length(resource.job.app.group.app.volume) == 0 && length(resource.policy) == 0
    error_message = "an empty dynamic block and a type without resources"
  }

  assert {
    condition     = resource.job.w[1].id == "w1" && resource.variable.x.b.path == "p/b"
    error_message = "count is a list, for_each an object"
  }

  assert {
    condition     = issensitive(resource.variable.x.a.items.token) && output.names == ["a", "b"] && rigger.release == "r"
    error_message = "marks, outputs as literals, facts"
  }
}
`},
		{name: "a failed assert", src: `test "t" {
  values = { n = 4 }

  assert {
    condition     = resource.job.app.group.app.task.app.config.args == null
    error_message = "no arguments"
  }
}
`, failures: "test.riggertest.hcl:5,21-76: no arguments"},
		{name: "an assert that does not evaluate", src: `test "t" {
  assert {
    condition     = resource.job.nothing.id == "x"
    error_message = "x"
  }
}
`, failures: "the condition could not be evaluated"},
		{name: "an expected error", src: `test "t" {
  values       = { n = 9 }
  expect_error = "var.n is 9, more than 5"
}
`},
		{name: "an expected error that does not come", src: `test "t" {
  expect_error = "anything"
}
`, failures: `expected an error containing "anything"; the module evaluated`},
		{name: "another error than expected", src: `test "t" {
  values       = { n = 9 }
  expect_error = "anything"
}
`, failures: `expected an error containing "anything"; got other errors`},
		{name: "values that are not an object", src: `test "t" {
  values = ["n"]
}
`, failures: "the module did not evaluate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, diags := eval.ParseTests("test.riggertest.hcl", []byte(tt.src))
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			out := eval.RunTest(t.Context(), m, parsed[0], eval.Config{FileBytes: 1 << 20, Resources: 20, Jobs: eval.GenericJobs})
			got := strings.Join(out.Failures, "; ")

			if tt.failures == "" && !out.Passed() {
				t.Fatalf("failures = %q (%v)", got, out.Diagnostics)
			}

			if !strings.Contains(got, tt.failures) {
				t.Errorf("failures = %q, want them to contain %q", got, tt.failures)
			}
		})
	}
}

// The values a test reads are literals whatever their shape: the items of a
// list of lists have tuples of different lengths, the values of a map of
// objects different attributes.
func TestRunTest_NestedCollections(t *testing.T) {
	t.Parallel()

	m := load(t, module(map[string]string{
		variablesHCL: `variable "x" {
  type    = list(list(string))
  default = [["a"], ["b", "c"]]
}

variable "m" {
  type    = map(set(number))
  default = { one = [1], two = [1, 2] }
}

variable "o" {
  type    = list(map(string))
  default = [{ a = "1" }, { b = "2", c = "3" }]
}
`,
		"outputs.hcl": "output \"x\" {\n  value = var.x\n}\n",
	}))

	parsed, diags := eval.ParseTests("test.riggertest.hcl", []byte(`test "t" {
  assert {
    condition     = var.x == [["a"], ["b", "c"]] && output.x == [["a"], ["b", "c"]]
    error_message = "a list of lists"
  }

  assert {
    condition     = var.m == { one = [1], two = [1, 2] }
    error_message = "a map of sets"
  }

  assert {
    condition     = var.o == [{ a = "1" }, { b = "2", c = "3" }]
    error_message = "a list of maps"
  }
}
`))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	out := eval.RunTest(t.Context(), m, parsed[0], eval.Config{FileBytes: 1 << 20, Resources: 20, Jobs: eval.GenericJobs})
	if !out.Passed() {
		t.Errorf("failures = %q (%v)", strings.Join(out.Failures, "; "), out.Diagnostics)
	}
}

func TestParseTests_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{name: "twice", src: "test \"a\" {}\ntest \"a\" {}\n", expected: `The test "a" is in this file twice`},
		{
			name:     "asserts and an expected error",
			src:      "test \"a\" {\n  expect_error = \"x\"\n\n  assert {\n    condition     = true\n    error_message = \"x\"\n  }\n}\n",
			expected: "A test expects an error or asserts over the result, not both",
		},
		{name: "an unknown attribute", src: "test \"a\" {\n  value = {}\n}\n", expected: `An argument named "value" is not expected here`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := eval.ParseTests("t.riggertest.hcl", []byte(tt.src))
			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
