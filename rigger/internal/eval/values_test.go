// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

var limits = lang.Limits{ModuleBytes: 16 << 20, FileBytes: 1 << 20}

func load(t *testing.T, fsys fs.FS) *lang.Module {
	t.Helper()

	m, diags := lang.Load(t.Context(), fsys, limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return m
}

func values(t *testing.T, src string) map[string]eval.Input {
	t.Helper()

	inputs, diags := eval.ParseValues("test.vars.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return inputs
}

func TestVariables_Redis(t *testing.T) {
	t.Parallel()

	m := load(t, os.DirFS("../../examples/registry/modules/redis"))

	tests := []struct {
		name     string
		values   string
		expected string
	}{
		{name: "defaults", values: ``},
		{name: "an option", values: `mode = "replica"`},
		{name: "not an option", values: `mode = "cluster"`, expected: `var.mode is "cluster", not one of "standalone", "replica", "sentinel"`},
		{name: "below the minimum", values: `count = 0`, expected: "var.count is 0, less than 1"},
		{name: "not whole", values: `count = 1.5`, expected: "var.count is 1.5, not a whole number"},
		{name: "wrong type", values: `count = "three"`, expected: "var.count must be a number"},
		{name: "a rule", values: `redis_password = "x"`, expected: "Rule not met: password_needs_auth; redis_password is written only with auth_enabled = true"},
		{name: "undeclared", values: `namspace = "x"`, expected: `The module "redis" has no variable "namspace"`},
		{name: "undeclared with a hint", values: `job_nam = "x"`, expected: `Did you mean "job_name"?`},
		{name: "too few items", values: `datacenters = []`, expected: "var.datacenters has 0 items, fewer than 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := eval.Variables(m, values(t, tt.values))

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

const metadata = "module {\n  name = \"app\"\n  version = \"1.0.0\"\n}\n"

const variables = `
variable "token" {
  type      = string
  sensitive = true

  validation {
    pattern = "^[a-z]+$"
  }
}

variable "users" {
  type = list(object({
    name    = string
    account = optional(string, "APP")
    tags    = optional(list(string), [])
  }))
  default = []

  validation {
    unique_by = "name"

    attribute "account" {
      one_of = ["APP", "SYS"]
    }

    attribute "tags" {
      item {
        pattern = "^[a-z]+$"
      }
    }
  }
}
`

func TestVariables_Constraints(t *testing.T) {
	t.Parallel()

	m := load(t, fstest.MapFS{
		"metadata.hcl": {Data: []byte(metadata)},
		variablesHCL:   {Data: []byte(variables)},
	})

	tests := []struct {
		name     string
		values   string
		expected string
	}{
		{name: "valid", values: `token = "abc"
users = [{ name = "a" }, { name = "b", account = "SYS", tags = ["x"] }]`},
		{name: "a sensitive value is not shown", values: `token = "Secret-123"`, expected: "var.token is (sensitive), which does not match"},
		{name: "unique by", values: `token = "a"
users = [{ name = "a" }, { name = "a" }]`, expected: "var.users[1].name is the same as var.users[0].name"},
		{name: "an attribute", values: `token = "a"
users = [{ name = "a", account = "X" }]`, expected: `var.users[0].account is "X", not one of "APP", "SYS"`},
		{name: "an item of an attribute", values: `token = "a"
users = [{ name = "a", tags = ["ok", "NO"] }]`, expected: `var.users[0].tags[1] is "NO", which does not match`},
		{name: "no value", values: ``, expected: `The variable "token" has no default`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vars, diags := eval.Variables(m, values(t, tt.values))

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				if !vars.GetAttr("token").HasMark(eval.Sensitive) {
					t.Error("the sensitive value is not marked")
				}

				account := vars.GetAttr("users").Index(cty.NumberIntVal(0)).GetAttr("account")
				if account.AsString() != "APP" {
					t.Errorf("the default of an optional attribute is not applied: %v", account)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}

			if strings.Contains(diags.Error(), "Secret-123") {
				t.Error("a sensitive value is shown in a message")
			}
		})
	}
}

// The value files of the examples are valid for their modules.
func TestVariables_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres"} {
		m, diags := lang.LoadAt(t.Context(), os.DirFS("../../examples"), dir, limits)
		if diags.HasErrors() {
			t.Fatal(diags)
		}

		fsys := m.FS

		for _, p := range m.Examples {
			t.Run(dir+"/"+p, func(t *testing.T) {
				t.Parallel()

				src, err := fs.ReadFile(fsys, p)
				if err != nil {
					t.Fatal(err)
				}

				inputs, diags := eval.ParseValues(p, src)
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				if _, diags := eval.Variables(m, inputs); diags.HasErrors() {
					t.Error(diags)
				}
			})
		}
	}
}

// A value with a number of more digits than the bound is refused before it
// is converted to the type of its variable or shown in a message: either
// writes the number. Values written as HCL are refused as they are read;
// those of JSON are read by another parser, which has no bound.
func TestVariables_LongNumbers(t *testing.T) {
	t.Parallel()

	m := load(t, module(map[string]string{
		variablesHCL: "variable \"s\" {\n  type    = string\n  default = \"\"\n}\n\nvariable \"n\" {\n  type    = number\n  default = 1\n\n  validation {\n    max = 5\n  }\n}\n\n" +
			"variable \"o\" {\n  type    = map(list(number))\n  default = {}\n}\n\nvariable \"secret\" {\n  type      = string\n  default   = \"\"\n  sensitive = true\n}\n",
	}))

	const over = ": the number has more than 1000 digits, the limit of the engine."

	tests := []struct {
		name     string
		json     string
		expected string
	}{
		{name: "for a string", json: `{"s": 1e4000000}`, expected: "v.json:1,7-16: Invalid value; var.s" + over},
		{name: "for a number with a validation", json: `{"n": 1e-4000000}`, expected: "v.json:1,7-17: Invalid value; var.n" + over},
		{name: "inside a value", json: `{"o": {"a": [1, 1e4000000]}}`, expected: "v.json:1,7-28: Invalid value; var.o" + over},
		{name: "for a sensitive variable", json: `{"secret": 1e4000000}`, expected: "Invalid value; var.secret" + over},
		{name: "at the bound", json: `{"s": 1e999, "n": 1e-1000, "o": {"a": [5]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inputs, diags := eval.ParseValuesJSON("v.json", []byte(tt.json))
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			_, diags = eval.Variables(m, inputs)

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Error(diags)
				}

				return
			}

			if got := diags.Error(); len(diags) != 1 || !strings.Contains(got, tt.expected) {
				t.Errorf("diagnostics = %q, want the one that contains %q", got, tt.expected)
			}
		})
	}

	t.Run("written as HCL", func(t *testing.T) {
		t.Parallel()

		_, diags := eval.ParseValues("v.vars.hcl", []byte("s = 1e4000000\n"))

		const want = "v.vars.hcl:1,5-14: Text too complex; The text can't be read: it has a number of more than 1000 digits, the limit of the engine."
		if got := diags.Error(); len(diags) != 1 || got != want {
			t.Errorf("diagnostics = %q, want %q", got, want)
		}
	})
}

func TestCoalesce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args     []cty.Value
		expected cty.Value
		err      bool
	}{
		{args: []cty.Value{cty.StringVal(""), cty.StringVal("b")}, expected: cty.StringVal("b")},
		{args: []cty.Value{cty.NullVal(cty.String), cty.StringVal("a")}, expected: cty.StringVal("a")},
		{args: []cty.Value{cty.NumberIntVal(0), cty.NumberIntVal(1)}, expected: cty.NumberIntVal(0)},
		{args: []cty.Value{cty.StringVal(""), cty.NullVal(cty.String)}, err: true},
	}

	for _, tt := range tests {
		got, err := eval.Functions()["coalesce"].Call(tt.args)
		if (err != nil) != tt.err {
			t.Fatalf("coalesce(%#v) error = %v", tt.args, err)
		}

		if !tt.err && !got.RawEquals(tt.expected) {
			t.Errorf("coalesce(%#v) = %#v, want %#v", tt.args, got, tt.expected)
		}
	}
}

// strlen counts the characters of a string, as the rules of the packs use
// it.
func TestStrlen(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]int64{"": 0, "https://a": 9, "árvíztűrő": 9} {
		got, err := eval.Functions()["strlen"].Call([]cty.Value{cty.StringVal(in)})
		if err != nil || !got.RawEquals(cty.NumberIntVal(want)) {
			t.Errorf("strlen(%q) = %#v, %v; want %d", in, got, err, want)
		}
	}
}
