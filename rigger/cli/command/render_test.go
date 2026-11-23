// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

const (
	redisDir = "../../examples/registry/modules/redis"
	flagVar  = "-var"
	// jsonTrue is true as the text of a -var and as JSON.
	jsonTrue = "true"
)

func TestRun_ModuleRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout []string
		expectedStderr string
	}{
		{
			name:           "defaults",
			args:           []string{cmdModule, cmdRender, redisDir},
			expectedCode:   exitOK,
			expectedStdout: []string{`"release": "redis"`, `"kind": "job"`, `"ID": "redis"`, `"Region": "global"`},
		},
		{
			name: "values from a file and flags",
			args: []string{
				cmdModule, cmdRender, redisDir, "-f", redisDir + "/examples/auth.vars.hcl",
				flagVar, "count=2", "-release", "cache", "-namespace", "apps", "-region", "eu",
			},
			expectedCode: exitOK,
			expectedStdout: []string{
				`"kind": "variable"`, `"secret_items": [`, `"password"`, `"Count": 2`, `"Region": "eu"`,
			},
		},
		{
			name:           "flags before the directory",
			args:           []string{cmdModule, cmdRender, flagVar, "job_name=kv", redisDir},
			expectedCode:   exitOK,
			expectedStdout: []string{`"ID": "kv"`},
		},
		{
			name:           "an invalid value",
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "mode=cluster"},
			expectedCode:   exitError,
			expectedStderr: `var.mode is "cluster", not one of`,
		},
		{
			name:           "not name=value",
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "count"},
			expectedCode:   exitError,
			expectedStderr: `"count" is not name=value`,
		},
		{
			name:           "two directories",
			args:           []string{cmdModule, cmdRender, redisDir, redisDir},
			expectedCode:   exitError,
			expectedStderr: "module render takes one directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), tt.args, &stdout, &stderr)
			if code != tt.expectedCode {
				t.Errorf("Run() = %d, want %d (stderr %q)", code, tt.expectedCode, stderr.String())
			}

			for _, want := range tt.expectedStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %s, want it to contain %s", stdout.String(), want)
				}
			}

			if strings.Contains(stdout.String(), "example-only") {
				t.Error("stdout holds the value of a secret item")
			}

			if !strings.Contains(stderr.String(), tt.expectedStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.expectedStderr)
			}

			if tt.expectedCode == exitOK && !json.Valid(stdout.Bytes()) {
				t.Errorf("stdout is not JSON: %s", stdout.String())
			}
		})
	}
}

// typedVariables declares a variable of each kind of type: text, count, on,
// tags, and free, which has no type.
const typedVariables = `variable "text" {
  type    = string
  default = ""
}

variable "count" {
  type    = number
  default = 0
}

variable "on" {
  type    = bool
  default = false
}

variable "tags" {
  type    = list(string)
  default = []
}

variable "free" {
  default = ""
}
`

// A -var is the value of a variable as it is written: the text itself for a
// string, converted for a number and a bool, HCL for a list, and for a
// variable without a type a number only when it is written as one is
// printed.
func TestRun_ModuleRender_VarByType(t *testing.T) {
	t.Parallel()

	var outputs strings.Builder
	for _, name := range []string{"text", "count", "on", "tags", "free"} {
		outputs.WriteString("output \"" + name + "\" {\n  value = var." + name + "\n}\n\n")
	}

	dir := writeTree(t, map[string]string{
		metadataFile:  "module {\n  name    = \"typed\"\n  version = \"1.0.0\"\n}\n",
		variablesFile: typedVariables,
		"outputs.hcl": outputs.String(),
	})

	tests := []struct {
		// v is the -var; expected the JSON of the output of its variable.
		v        string
		expected string
		stderr   string
	}{
		{v: "text=1.20", expected: `"1.20"`},
		{v: "text=007", expected: `"007"`},
		{v: "text=1e3", expected: `"1e3"`},
		{v: "text=3", expected: `"3"`},
		{v: "text=true", expected: `"true"`},
		{v: "text=null", expected: `"null"`},
		{v: "text=", expected: `""`},
		{v: "text=a=b", expected: `"a=b"`},
		{v: `text="quoted"`, expected: `"\"quoted\""`},
		{v: `text=["a"]`, expected: `"[\"a\"]"`},
		{v: `text={"a": 1}`, expected: `"{\"a\": 1}"`},
		{v: "count=3", expected: `3`},
		{v: "count=1.20", expected: `1.2`},
		{v: "count=1e3", expected: `1000`},
		{v: "count=007", expected: `7`},
		{v: "count=many", stderr: "var.count must be a number"},
		{v: "on=true", expected: jsonTrue},
		{v: "on=maybe", stderr: "var.on must be a bool"},
		{v: `tags=["1.20", "b"]`, expected: `["1.20","b"]`},
		{v: "tags=a", stderr: "var.tags must be a list of string"},
		{v: "free=3", expected: `3`},
		{v: "free=-1.5", expected: `-1.5`},
		{v: "free=1.20", expected: `"1.20"`},
		{v: "free=007", expected: `"007"`},
		{v: "free=1e3", expected: `"1e3"`},
		{v: "free=1+1", expected: `"1+1"`},
		{v: "free=redis", expected: `"redis"`},
		{v: "free=true", expected: jsonTrue},
		{v: `free=["a", 2]`, expected: `["a",2]`},
	}

	for _, tt := range tests {
		t.Run(tt.v, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), []string{cmdModule, cmdRender, dir, flagVar, tt.v}, &stdout, &stderr)

			if tt.stderr != "" {
				if code != exitError || !strings.Contains(stderr.String(), tt.stderr) {
					t.Errorf("Run() = %d, stderr %q, want an error with %q", code, stderr.String(), tt.stderr)
				}

				return
			}

			if code != exitOK {
				t.Fatalf("Run() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
			}

			name, _, _ := strings.Cut(tt.v, "=")
			if got := renderedOutput(t, stdout.Bytes(), name); got != tt.expected {
				t.Errorf("-var %s: var.%s = %s, want %s", tt.v, name, got, tt.expected)
			}
		})
	}
}

// renderedOutput returns the JSON of the value of an output of a module
// render, without spaces.
func renderedOutput(t *testing.T, out []byte, name string) string {
	t.Helper()

	var r struct {
		Outputs []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"outputs"`
	}

	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}

	for _, o := range r.Outputs {
		if o.Name != name {
			continue
		}

		var compact bytes.Buffer
		if err := json.Compact(&compact, o.Value); err != nil {
			t.Fatal(err)
		}

		return compact.String()
	}

	t.Fatalf("no output %s:\n%s", name, out)

	return ""
}

// Without the type of the variable (a published stack that the servers
// fetch) a -var is guessed: what is sent is the text, unless HCL reads and
// writes it the same way.
func TestReadValues_UnknownTypes(t *testing.T) {
	t.Parallel()

	values, diags := readValues(&renderFlags{vars: listFlag{"tag=1.20", "replicas=3", "name=redis", "on=true", `tags=["a"]`}}, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	expected := "name     = \"redis\"\non       = true\nreplicas = 3\ntag      = \"1.20\"\ntags     = [\"a\"]\n"
	if got := valuesHCL(values); got != expected {
		t.Errorf("valuesHCL() =\n%s\nwant\n%s", got, expected)
	}
}

func TestVarValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		jsonType string
		expected cty.Value
	}{
		{name: "a string as written", text: "1.20", jsonType: "string", expected: cty.StringVal("1.20")},
		{name: "a number converts later", text: "1e3", jsonType: "number", expected: cty.StringVal("1e3")},
		{name: "a bool converts later", text: jsonTrue, jsonType: "boolean", expected: cty.StringVal(jsonTrue)},
		{name: "a list is HCL", text: `["a", "b"]`, jsonType: "array", expected: cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})},
		{name: "an object is HCL", text: `{ a = 1 }`, jsonType: "object", expected: cty.ObjectVal(map[string]cty.Value{"a": cty.NumberIntVal(1)})},
		{name: "no type: a number", text: "3", expected: cty.NumberIntVal(3)},
		{name: "no type: a number written otherwise", text: "3.0", expected: cty.StringVal("3.0")},
		{name: "no type: a bool", text: jsonTrue, expected: cty.True},
		{name: "no type: null", text: "null", expected: cty.NullVal(cty.DynamicPseudoType)},
		{name: "no type: a word", text: "redis", expected: cty.StringVal("redis")},
		{name: "no type: not an expression", text: "eu-1", expected: cty.StringVal("eu-1")},
		{name: "no type: empty", text: "", expected: cty.StringVal("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := varValue(tt.text, tt.jsonType); !got.RawEquals(tt.expected) {
				t.Errorf("varValue(%q, %q) = %#v, want %#v", tt.text, tt.jsonType, got, tt.expected)
			}
		})
	}
}

// After a step of a command its warnings are written and the command goes
// on; its errors are written and end it.
func TestCheckDiags(t *testing.T) {
	t.Parallel()

	warning := &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "Advisory", Detail: "greets too loudly."}
	failure := &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Release not compared", Detail: "no leader."}

	tests := []struct {
		name     string
		diags    hcl.Diagnostics
		expected error
		// written is what standard error has; empty for nothing at all.
		written []string
	}{
		{name: "nothing"},
		{name: "a warning", diags: hcl.Diagnostics{warning}, written: []string{"Warning: Advisory", "greets too loudly."}},
		{name: "an error", diags: hcl.Diagnostics{failure}, expected: errReported, written: []string{"Error: Release not compared"}},
		{
			name: "a warning and an error", diags: hcl.Diagnostics{warning, failure}, expected: errReported,
			written: []string{"Warning: Advisory", "Error: Release not compared"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer

			if err := checkDiags(&stderr, ".", tt.diags); !errors.Is(err, tt.expected) || (tt.expected == nil && err != nil) {
				t.Errorf("checkDiags() = %v, want %v", err, tt.expected)
			}

			if len(tt.written) == 0 && stderr.Len() > 0 {
				t.Errorf("standard error has %q, want nothing", stderr.String())
			}

			for _, want := range tt.written {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("standard error has no %q:\n%s", want, stderr.String())
				}
			}
		})
	}
}
