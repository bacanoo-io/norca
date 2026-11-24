// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	rigger "github.com/bacanoo-io/norca/rigger"
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
				flagVar, "memory=512", "-release", "cache", "-namespace", "apps", "-region", "eu",
			},
			expectedCode: exitOK,
			expectedStdout: []string{
				`"kind": "variable"`, `"secret_items": [`, `"password"`, `"MemoryMB": 512`, `"Region": "eu"`,
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
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "network_mode=overlay"},
			expectedCode:   exitError,
			expectedStderr: `var.network_mode is "overlay", not one of`,
		},
		{
			name:           "not name=value",
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "memory"},
			expectedCode:   exitError,
			expectedStderr: `"memory" is not name=value`,
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

// The names of the test variables of the environment.
const (
	envTag     = "RIGGER_VAR_tag"
	envSize    = "RIGGER_VAR_size"
	varTag     = "tag"
	varSize    = "size"
	typeString = "string"
)

// A variable of the environment RIGGER_VAR_<name> is the value of the
// variable <name> (design 6.1), read by the type of the variable as a -var
// is. One that names no variable here is not used, and a warning says so:
// the environment is shared by every module and stack that is run in it.
func TestEnvValues(t *testing.T) {
	t.Parallel()

	types := map[string]string{varTag: typeString, varSize: "number", "free": ""}

	tests := []struct {
		name     string
		environ  []string
		known    bool
		values   rigger.Values
		expected map[string]cty.Value
		warnings []string
	}{
		{
			name:     "the text as it is for a typed variable",
			environ:  []string{envTag + "=1.50", envSize + "=007", "PATH=/bin", "RIGGER_VARIABLE=x"},
			known:    true,
			expected: map[string]cty.Value{varTag: cty.StringVal("1.50"), varSize: cty.StringVal("007")},
		},
		{
			name:     "a guess for a variable of any type",
			environ:  []string{"RIGGER_VAR_free=3", envTag + "="},
			known:    true,
			expected: map[string]cty.Value{"free": cty.NumberIntVal(3), varTag: cty.StringVal("")},
		},
		{
			name:     "over the value of a file",
			environ:  []string{envTag + "=of the environment"},
			known:    true,
			values:   rigger.Values{varTag: {Value: cty.StringVal("of the file")}, varSize: {Value: cty.NumberIntVal(1)}},
			expected: map[string]cty.Value{varTag: cty.StringVal("of the environment"), varSize: cty.NumberIntVal(1)},
		},
		{
			name:     "a name of no variable is not used",
			environ:  []string{"RIGGER_VAR_region=eu", "RIGGER_VAR_not-a-name=x", "RIGGER_VAR_=y", envTag + "=a=b"},
			known:    true,
			expected: map[string]cty.Value{varTag: cty.StringVal("a=b")},
			warnings: []string{
				`RIGGER_VAR_ is not used: there is no variable "" here.`,
				`RIGGER_VAR_not-a-name is not used: there is no variable "not-a-name" here.`,
				`RIGGER_VAR_region is not used: there is no variable "region" here.`,
			},
		},
		{
			name:     "none is used when the variables are not known",
			environ:  []string{envTag + "=x"},
			expected: map[string]cty.Value{},
			warnings: []string{envTag + " is not used: the variables of a published stack that the servers fetch are not known here; give the value with -var."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			values := rigger.Values{}
			maps.Copy(values, tt.values)

			known := types
			if !tt.known {
				known = nil
			}

			diags := envValues(values, tt.environ, tt.known, known)

			got := map[string]cty.Value{}
			for name, v := range values {
				got[name] = v.Value
			}

			if len(got) != len(tt.expected) {
				t.Errorf("values = %#v, want %#v", got, tt.expected)
			}

			for name, want := range tt.expected {
				if v, ok := got[name]; !ok || !v.RawEquals(want) {
					t.Errorf("%s = %#v, want %#v", name, v, want)
				}
			}

			var warnings []string

			for _, d := range diags {
				if d.Severity != hcl.DiagWarning {
					t.Errorf("a diagnostic that is no warning: %s", d.Error())
				}

				warnings = append(warnings, d.Detail)
			}

			if !slices.Equal(warnings, tt.warnings) {
				t.Errorf("warnings = %q, want %q", warnings, tt.warnings)
			}
		})
	}
}

// Through the program: the environment is over a value file and under a
// -var, and a variable of the environment that is not used is a warning,
// not an error.
func TestRun_ModuleRender_EnvVar(t *testing.T) {
	dir := writeTree(t, map[string]string{
		metadataFile:  "module {\n  name    = \"sized\"\n  version = \"1.0.0\"\n}\n",
		variablesFile: "variable \"tag\" {\n  type = string\n}\n\nvariable \"size\" {\n  type    = number\n  default = 0\n}\n",
		"outputs.hcl": "output \"tag\" {\n  value = var.tag\n}\n\noutput \"size\" {\n  value = var.size\n}\n",
	})

	// A value file is no file of the module: it lies beside it.
	file := filepath.Join(writeTree(t, map[string]string{"dev.vars.hcl": "tag  = \"of the file\"\nsize = 1\n"}), "dev.vars.hcl")

	t.Setenv(envTag, "1.50")
	t.Setenv(envSize, "007")
	t.Setenv("RIGGER_VAR_region", "eu")

	var stdout, stderr bytes.Buffer

	if code := Run(t.Context(), []string{cmdModule, cmdRender, dir, "-f", file, flagVar, "size=5"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("Run() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}

	if tag, size := renderedOutput(t, stdout.Bytes(), varTag), renderedOutput(t, stdout.Bytes(), varSize); tag != `"1.50"` || size != "5" {
		t.Errorf("tag = %s, size = %s; want \"1.50\" of the environment and 5 of the -var", tag, size)
	}

	if !strings.Contains(stderr.String(), "RIGGER_VAR_region is not used") {
		t.Errorf("stderr = %q, want the warning of RIGGER_VAR_region", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()

	// Without the -var the value of the environment is the one, by the type
	// of the variable: 007 is the number 7.
	if code := Run(t.Context(), []string{cmdModule, cmdRender, dir}, &stdout, &stderr); code != exitOK {
		t.Fatalf("Run() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}

	if size := renderedOutput(t, stdout.Bytes(), varSize); size != "7" {
		t.Errorf("size = %s, want 7", size)
	}

	t.Setenv(envSize, "many")
	stderr.Reset()

	if code := Run(t.Context(), []string{cmdModule, cmdRender, dir}, &stdout, &stderr); code != exitError ||
		!strings.Contains(stderr.String(), "var.size must be a number") {
		t.Errorf("Run() = %d, stderr %q; want an error of var.size", code, stderr.String())
	}
}
