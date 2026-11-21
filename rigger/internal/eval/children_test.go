// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

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
