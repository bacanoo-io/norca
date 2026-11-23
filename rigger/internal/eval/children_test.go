// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// evaluateAt evaluates the module in dir of fsys with its children.
func evaluateAt(t *testing.T, fsys fstest.MapFS, dir string) (*eval.Result, hcl.Diagnostics) {
	t.Helper()

	m, diags := lang.LoadAt(t.Context(), fsys, dir, limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	vars, diags := eval.Variables(m, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return eval.Evaluate(t.Context(), m, vars, facts, eval.Config{FileBytes: 1 << 20, Resources: 10, Jobs: eval.GenericJobs})
}

// family is a module app calling the module lib/x.
func family(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"app/metadata.hcl":   {Data: []byte(metadata)},
		"lib/x/metadata.hcl": {Data: []byte("module {\n  name    = \"x\"\n  version = \"2.0.0\"\n}\n")},
		"lib/x/variables.hcl": {Data: []byte(`variable "prefix" {
  type = string
}

variable "token" {
  type      = string
  default   = "none"
  sensitive = true
}
`)},
		"lib/x/outputs.hcl": {Data: []byte(`output "name" {
  value = "${var.prefix}-${rigger.module.name}-${rigger.module.version}"
}

output "token" {
  value     = var.token
  sensitive = true
}
`)},
		"lib/x/y.acl.hcl": {Data: []byte("policy \"y\" {\n  rules = var.prefix\n}\n")},
	}

	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}

	return fsys
}

// The files of the module app.
const (
	appModules = "app/modules.hcl"
	appLocals  = "app/locals.hcl"
)

const callX = "module \"x\" {\n  source = \"../lib/x\"\n  prefix = local.prefix\n}\n"

func TestEvaluate_Children(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		names    string
		contains string
		expected string
	}{
		{
			name: "locals and a child read each other",
			files: map[string]string{
				appModules:          callX,
				appLocals:           "locals {\n  prefix = rigger.release\n  name   = upper(module.x.name)\n}\n",
				"app/app.norca.hcl": "job \"app\" {\n  id = local.name\n}\n",
			},
			names:    "acl-policy x.y, job app",
			contains: `"rules":"cache"`,
		},
		{
			name: "the outputs of the child",
			files: map[string]string{
				appModules:          callX,
				appLocals:           "locals {\n  prefix = \"p\"\n}\n",
				"app/app.norca.hcl": "job \"app\" {\n  id = module.x.name\n}\n",
			},
			names: "acl-policy x.y, job app",
		},
		{
			name: "a cycle",
			files: map[string]string{
				appModules: callX,
				appLocals:  "locals {\n  prefix = module.x.name\n}\n",
			},
			expected: "local.prefix → module.x → local.prefix",
		},
		{
			name: "an undeclared argument",
			files: map[string]string{
				appModules: "module \"x\" {\n  source = \"../lib/x\"\n  prefix = \"p\"\n  prefx  = \"p\"\n}\n",
			},
			expected: `The module "x" has no variable "prefx"`,
		},
		{
			name: "a sensitive value into a plain variable",
			files: map[string]string{
				"app/variables.hcl": tokenVariable,
				appModules:          "module \"x\" {\n  source = \"../lib/x\"\n  prefix = var.token\n}\n",
			},
			expected: "A sensitive value can't be used in a variable of a child module that is not sensitive",
		},
		{
			name: "a sensitive output of a child stays sensitive",
			files: map[string]string{
				"app/variables.hcl": tokenVariable,
				appModules:          "module \"x\" {\n  source = \"../lib/x\"\n  prefix = \"p\"\n  token  = var.token\n}\n",
				"app/outputs.hcl":   "output \"t\" {\n  value = module.x.token\n}\n",
			},
			expected: "write sensitive = true on it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := evaluateAt(t, family(tt.files), "app")

			if tt.expected != "" {
				if !strings.Contains(diags.Error(), tt.expected) {
					t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if got := names(res); got != tt.names {
				t.Errorf("resources = %q, want %q", got, tt.names)
			}

			if tt.contains != "" && !strings.Contains(string(res.Resources[0].Spec), tt.contains) {
				t.Errorf("spec = %s, want it to contain %s", res.Resources[0].Spec, tt.contains)
			}
		})
	}
}

// A child module is not evaluated with a value that failed: the value is
// not known, and the child would report what it makes of that, or fail on
// it. The error of the value is the one reported.
func TestEvaluate_ChildAfterFailure(t *testing.T) {
	t.Parallel()

	// The module lib/l makes a block per item of a list.
	lists := map[string]string{
		"lib/l/metadata.hcl":  "module {\n  name    = \"l\"\n  version = \"1.0.0\"\n}\n",
		"lib/l/variables.hcl": "variable \"items\" {\n  type = list(string)\n}\n",
		"lib/l/y.norca.hcl":   "job \"y\" {\n  dynamic \"group\" {\n    for_each = var.items\n    labels   = [group.value]\n\n    content {\n      n = group.value\n    }\n  }\n}\n",
		"lib/l/y.var.hcl":     "variable \"y\" {\n  path  = \"p\"\n  items = { for i in var.items : i => i }\n}\n",
	}

	const (
		failedLocal = "locals {\n  a = regex(\"(\", \"x\")\n}\n"
		failedChild = "module \"x\" {\n  source = \"../lib/x\"\n  prefix = regex(\"(\", \"x\")\n}\n"
		regexFailed = "invalid regexp pattern"
	)

	tests := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "a failed local",
			files: map[string]string{
				appLocals:  failedLocal,
				appModules: "module \"l\" {\n  source = \"../lib/l\"\n  items  = local.a\n}\n",
			},
		},
		{
			name: "a local that reads a failed local",
			files: map[string]string{
				appLocals:  failedLocal + "locals {\n  b = [upper(local.a)]\n}\n",
				appModules: "module \"l\" {\n  source = \"../lib/l\"\n  items  = local.b\n}\n",
			},
		},
		{
			name: "a failed child",
			files: map[string]string{
				appModules: failedChild + "module \"l\" {\n  source = \"../lib/l\"\n  items  = [module.x.name]\n}\n",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := family(lists)
			for name, data := range tt.files {
				fsys[name] = &fstest.MapFile{Data: []byte(data)}
			}

			_, diags := evaluateAt(t, fsys, "app")

			if len(diags) != 1 || !strings.Contains(diags.Error(), regexFailed) {
				t.Errorf("diagnostics = %q, want the one that contains %q", diags, regexFailed)
			}
		})
	}
}

// With no value that failed before it, a value not known at install time is
// an error of the module block that gives it to the child.
func TestEvaluate_ChildUnknownValue(t *testing.T) {
	t.Parallel()

	fsys := family(map[string]string{
		"app/variables.hcl": "variable \"p\" {\n  type = string\n}\n",
		appModules:          "module \"x\" {\n  source = \"../lib/x\"\n  prefix = var.p\n}\n",
	})

	m, diags := lang.LoadAt(t.Context(), fsys, "app", limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	vars := cty.ObjectVal(map[string]cty.Value{"p": cty.UnknownVal(cty.String)})

	_, diags = eval.Evaluate(t.Context(), m, vars, facts, eval.Config{FileBytes: 1 << 20, Resources: 10, Jobs: eval.GenericJobs})

	const want = "modules.hcl:3,12-17: Unknown value; The value of prefix is not known at install time."
	if len(diags) != 1 || !strings.Contains(diags.Error(), want) {
		t.Errorf("diagnostics = %q, want the one that contains %q", diags, want)
	}
}

func TestEvaluate_ChildOutputs(t *testing.T) {
	t.Parallel()

	res, diags := evaluateAt(t, family(map[string]string{
		appModules:        "module \"x\" {\n  source = \"../lib/x\"\n  prefix = \"p\"\n}\n",
		"app/outputs.hcl": "output \"name\" {\n  value = module.x.name\n}\n",
	}), "app")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if got := string(res.Outputs[0].Value); got != `"p-x-2.0.0"` {
		t.Errorf("output = %s, want the name with the facts of the child", got)
	}
}

func TestEvaluate_Library(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"lib/metadata.hcl": {Data: []byte("module {\n  name    = \"lib\"\n  version = \"1.0.0\"\n  library = true\n}\n")}}

	_, diags := evaluateAt(t, fsys, "lib")
	if want := `The module "lib" is a library`; !strings.Contains(diags.Error(), want) {
		t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), want)
	}
}
