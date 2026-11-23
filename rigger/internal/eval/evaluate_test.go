// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
)

var facts = eval.Facts{Release: "cache", Namespace: "apps", Region: "eu", ModuleName: "redis", ModuleVersion: "0.3.0"}

func evaluate(t *testing.T, fsys fs.FS, src string) (*eval.Result, hcl.Diagnostics) {
	t.Helper()

	m := load(t, fsys)

	vars, diags := eval.Variables(m, values(t, src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return eval.Evaluate(t.Context(), m, vars, facts, eval.Config{
		FileBytes: 1 << 20, Resources: 10, Jobs: eval.GenericJobs,
	})
}

func names(res *eval.Result) string {
	var out []string
	for _, r := range res.Resources {
		out = append(out, string(r.Kind)+" "+r.Name)
	}

	return strings.Join(out, ", ")
}

func TestEvaluate_Redis(t *testing.T) {
	t.Parallel()

	fsys := os.DirFS("../../examples/registry/modules/redis")

	res, diags := evaluate(t, fsys, ``)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if got := names(res); got != "job redis" {
		t.Fatalf("resources = %s, want job redis", got)
	}

	var job struct {
		ID     string `json:"id"`
		Region string `json:"region"`
		Group  struct {
			Redis struct {
				Task struct {
					Redis struct {
						Config map[string]any `json:"config"`
					} `json:"redis"`
				} `json:"task"`
			} `json:"redis"`
		} `json:"group"`
	}

	if err := json.Unmarshal(res.Resources[0].Spec, &job); err != nil {
		t.Fatal(err)
	}

	if job.ID != "redis" || job.Region != "eu" {
		t.Errorf("id, region = %q, %q, want redis, eu", job.ID, job.Region)
	}

	if _, ok := job.Group.Redis.Task.Redis.Config["args"]; ok {
		t.Error("args is in the job; a null attribute is left out")
	}

	if len(res.Outputs) != 3 || res.Outputs[0].Name != "auth_variable" || string(res.Outputs[0].Value) != "null" {
		t.Errorf("outputs = %+v", res.Outputs)
	}
}

func TestEvaluate_RedisSecretItem(t *testing.T) {
	t.Parallel()

	fsys := os.DirFS("../../examples/registry/modules/redis")

	src, err := os.ReadFile("../../examples/registry/modules/redis/examples/auth.vars.hcl")
	if err != nil {
		t.Fatal(err)
	}

	res, diags := evaluate(t, fsys, string(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if got := names(res); got != "variable auth, job redis" {
		t.Fatalf("resources = %s, want variable auth, job redis", got)
	}

	v := res.Resources[0]
	if s, ok := v.Secrets["password"]; !ok || s.Reveal() != "example-only" {
		t.Errorf("secrets = %v, want the password", v.Secrets)
	}

	if strings.Contains(string(v.Spec), "example-only") {
		t.Errorf("the spec holds the password: %s", v.Spec)
	}

	if !strings.Contains(string(v.Spec), `"items":{"mode":"standalone"}`) {
		t.Errorf("spec = %s, want the plain items", v.Spec)
	}
}

func TestEvaluate_Web(t *testing.T) {
	t.Parallel()

	fsys := os.DirFS("../../examples/module/web")

	src, err := os.ReadFile("../../examples/module/web/examples/with-database.vars.hcl")
	if err != nil {
		t.Fatal(err)
	}

	res, diags := evaluate(t, fsys, string(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	spec := string(res.Resources[0].Spec)
	for _, want := range []string{`"id":"cache"`, `"DB_SERVICE":`, `nomadSecret \"`} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec = %s, want it to contain %s", spec, want)
		}
	}
}

// module returns a module of one job file and the files given.
func module(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{"metadata.hcl": {Data: []byte(metadata)}}
	for name, src := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(src)}
	}

	return fsys
}

const tokenVariable = `variable "token" {
  type      = string
  default   = "s3cret"
  sensitive = true
}
`

// The files of the modules of the tests.
const (
	appNorca     = "app.norca.hcl"
	localsHCL    = "locals.hcl"
	outputsHCL   = "outputs.hcl"
	variablesHCL = "variables.hcl"
	xACL         = "x.acl.hcl"
	xObject      = "x.object.hcl"
	xVar         = "x.var.hcl"
)

// evaluateCase is a module of one or a few files, and what its evaluation
// gives: the names of its resources, a part of the spec of the first, or an
// error.
type evaluateCase struct {
	name     string
	files    map[string]string
	names    string
	contains string
	expected string
}

func evaluateCases() []evaluateCase {
	return []evaluateCase{
		{
			name:     "runtime references pass through",
			files:    map[string]string{appNorca: "job \"app\" {\n  meta {\n    k = \"${attr.kernel.name}-${NORCA_ALLOC_ID}\"\n  }\n}\n"},
			contains: `"k":"${attr.kernel.name}-${NORCA_ALLOC_ID}"`,
		},
		{
			name:     "a near miss of var",
			files:    map[string]string{appNorca: "job \"app\" {\n  meta {\n    k = \"${vars.x}\"\n  }\n}\n"},
			expected: "Did you mean var?",
		},
		{
			name:     "each outside for_each",
			files:    map[string]string{appNorca: "job \"app\" {\n  meta {\n    k = each.key\n  }\n}\n"},
			expected: "each.* is defined only inside a resource with for_each",
		},
		{
			name:     "no passthrough outside a job",
			files:    map[string]string{localsHCL: `locals { x = attr.kernel.name }`},
			expected: `There is no variable named "attr"`,
		},
		{
			name: "locals in the order of their references",
			files: map[string]string{
				localsHCL: "locals {\n  b = \"${local.a}-b\"\n}\nlocals {\n  a = rigger.release\n}\n",
				appNorca:  `job "app" { id = local.b }`,
			},
			contains: `"id":"cache-b"`,
		},
		{
			name:     "locals in a cycle",
			files:    map[string]string{localsHCL: "locals {\n  a = local.b\n  b = local.a\n}\n"},
			expected: "local.a → local.b → local.a",
		},
		{
			name:     "a failed check",
			files:    map[string]string{localsHCL: "check \"region\" {\n  condition     = rigger.region == \"us\"\n  error_message = \"Only in us.\"\n}\n"},
			expected: "Check failed: region; Only in us.",
		},
		{
			name:  "disabled",
			files: map[string]string{xACL: `policy "x" { enabled = false }`},
		},
		{
			name:  "for_each",
			files: map[string]string{xACL: "policy \"x\" {\n  for_each = toset([\"b\", \"a\"])\n  rules    = each.value\n}\n"},
			names: `acl-policy x["a"], acl-policy x["b"]`,
		},
		{
			name:  "count",
			files: map[string]string{"x.norca.hcl": "job \"x\" {\n  count = 2\n  id    = \"x-${count.index}\"\n}\n"},
			names: `job x[0], job x[1]`,
		},
		{
			name:     "for_each and count",
			files:    map[string]string{xACL: "policy \"x\" {\n  for_each = [\"a\"]\n  count    = 1\n}\n"},
			expected: "for_each and count together",
		},
		{
			name:     "too many",
			files:    map[string]string{xACL: `policy "x" { count = 11 }`},
			expected: "more than 10 resources",
		},
		{
			name:     "dynamic blocks",
			files:    map[string]string{xObject: "object \"x\" {\n  dynamic \"port\" {\n    for_each = [\"a\", \"b\"]\n    labels   = [port.value]\n    content {\n      to = port.key\n    }\n  }\n}\n"},
			contains: `"port":{"a":{"to":0},"b":{"to":1}}`,
		},
		{
			name:     "a sensitive value in a job",
			files:    map[string]string{variablesHCL: tokenVariable, "x.norca.hcl": "job \"x\" {\n  meta {\n    token = var.token\n  }\n}\n"},
			expected: "A sensitive value can't be used in a job",
		},
		{
			name:     "a sensitive value outside the items of a Variable",
			files:    map[string]string{variablesHCL: tokenVariable, xVar: `variable "x" { path = var.token }`},
			expected: "can't be used in a variable, except in its items",
		},
		{
			name: "sensitive labels of a dynamic block",
			files: map[string]string{
				variablesHCL: "variable \"names\" {\n  sensitive = true\n  default   = [\"a\"]\n}\n",
				xObject:      "object \"x\" {\n  dynamic \"port\" {\n    for_each = [1]\n    labels   = var.names\n    content {\n    }\n  }\n}\n",
			},
			expected: "A sensitive value can't be used in the labels of a dynamic block",
		},
		{
			name: "a sensitive object as the vars of a template",
			files: map[string]string{
				variablesHCL:  "variable \"db\" {\n  sensitive = true\n  default   = { password = \"pw\" }\n}\n",
				"files/t.tpl": "password=${password}",
				xVar:          "variable \"x\" {\n  path = \"p\"\n\n  items {\n    conf = templatefile(\"files/t.tpl\", var.db)\n  }\n}\n",
			},
			contains: `"items":{}`,
		},
		{
			name:     "a sensitive output not marked",
			files:    map[string]string{variablesHCL: tokenVariable, outputsHCL: `output "t" { value = var.token }`},
			expected: "write sensitive = true on it",
		},
	}
}

// functionCases are the modules of the tests of the functions and hooks.
func functionCases() []evaluateCase {
	return []evaluateCase{
		{
			name:     "file inside the module",
			files:    map[string]string{"files/p.hcl": "rules", xACL: `policy "x" { rules = file("files/p.hcl") }`},
			contains: `"rules":"rules"`,
		},
		{
			name:     "file outside the module",
			files:    map[string]string{xACL: `policy "x" { rules = file("../etc/passwd") }`},
			expected: `"../etc/passwd" is not a path inside the module`,
		},
		{
			name:     "templatefile",
			files:    map[string]string{"files/t.tpl": "hello ${who}", xACL: `policy "x" { rules = templatefile("files/t.tpl", { who = rigger.release }) }`},
			contains: `"rules":"hello cache"`,
		},
		{
			name: "templatefile in a template",
			files: map[string]string{
				"files/outer.tpl": `${templatefile("files/inner.tpl", {})}`,
				"files/inner.tpl": "inner",
				xACL:              `policy "x" { rules = templatefile("files/outer.tpl", {}) }`,
			},
			expected: `There is no function named "templatefile"`,
		},
		{
			name: "a template that renders itself",
			files: map[string]string{
				"files/self.tpl": `${templatefile("files/self.tpl", {})}`,
				xACL:             `policy "x" { rules = templatefile("files/self.tpl", {}) }`,
			},
			expected: `There is no function named "templatefile"`,
		},
		{
			name:     "image_ref",
			files:    map[string]string{xACL: `policy "x" { rules = image_ref("redis:7", "sha256:ab") }`},
			contains: `"rules":"redis:7@sha256:ab"`,
		},
		{
			name:     "variable_ref outside a task template",
			files:    map[string]string{xACL: `policy "x" { rules = variable_ref("nats/tls", "key", { quote = "conf" }) }`},
			expected: "variable_ref: can be used only in the data of a task template",
		},
		{
			name:     "runtime outside a job",
			files:    map[string]string{xACL: `policy "x" { rules = "${runtime("node.datacenter")}/x" }`},
			contains: `"rules":"${node.datacenter}/x"`,
		},
		{
			name:     "a hook of an unknown phase",
			files:    map[string]string{appNorca: "job \"app\" {\n  hook {\n    on = [\"post-installs\"]\n  }\n}\n"},
			expected: `"post-installs" is not a phase`,
		},
		{
			name:     "a hook of a null phase",
			files:    map[string]string{appNorca: "job \"app\" {\n  hook {\n    on = [\"test\", null]\n  }\n}\n"},
			expected: "app.norca.hcl:3,10-24: Invalid hook; on is a phase or a list of phases.",
		},
		{
			name:     "a hook with an invalid timeout",
			files:    map[string]string{appNorca: "job \"app\" {\n  hook {\n    on      = \"test\"\n    timeout = \"soon\"\n  }\n}\n"},
			expected: `timeout "soon" is not a duration`,
		},
		{
			name:     "a hook on a policy",
			files:    map[string]string{xACL: "policy \"x\" {\n  hook {\n    on = \"test\"\n  }\n}\n"},
			expected: "A hook is a job with a hook block, not a resource of kind acl-policy",
		},
		{
			name:     "a service address outside a task template",
			files:    map[string]string{xACL: `policy "x" { rules = service_address("db") }`},
			expected: "service_address: can be used only in the data of a task template",
		},
		{
			name:     "an invalid option",
			files:    map[string]string{xACL: `policy "x" { rules = secret_ref("a", "b", { quote = "xml" }) }`},
			expected: `invalid option quote: the options are quote ("conf", "sql" or false) and trim (a bool)`,
		},
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	for _, tt := range append(evaluateCases(), functionCases()...) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := evaluate(t, module(tt.files), ``)

			if tt.expected != "" {
				if !strings.Contains(diags.Error(), tt.expected) {
					t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if tt.names != "" || tt.contains == "" {
				if got := names(res); got != tt.names {
					t.Errorf("resources = %q, want %q", got, tt.names)
				}
			}

			if tt.contains != "" && !strings.Contains(string(res.Resources[0].Spec), tt.contains) {
				t.Errorf("spec = %s, want it to contain %s", res.Resources[0].Spec, tt.contains)
			}
		})
	}
}

func TestEvaluate_Hook(t *testing.T) {
	t.Parallel()

	res, diags := evaluate(t, module(map[string]string{
		appNorca: "job \"app\" {\n  hook {\n    on      = \"post-install\"\n    weight  = 5\n    timeout = \"1m\"\n    delete  = \"never\"\n  }\n}\n",
	}), ``)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	h := res.Resources[0].Hook
	if h == nil || strings.Join(h.On, ",") != "post-install" || h.Weight != 5 || h.Timeout != "1m" || h.Delete != "never" {
		t.Errorf("hook = %+v, want post-install, 5, 1m, never", h)
	}

	if strings.Contains(string(res.Resources[0].Spec), "hook") {
		t.Errorf("spec = %s; the decoder sees no hook block", res.Resources[0].Spec)
	}
}

// A value that is not known at install time is an error where its items
// are read, with the place: the items of a value not known can't be walked.
func TestEvaluate_UnknownValues(t *testing.T) {
	t.Parallel()

	vars := cty.ObjectVal(map[string]cty.Value{
		"list": cty.UnknownVal(cty.List(cty.String)),
		"map":  cty.UnknownVal(cty.Map(cty.String)),
	})

	tests := []struct {
		name     string
		file     string
		src      string
		expected string
	}{
		{
			name:     "the for_each of a dynamic block",
			file:     xObject,
			src:      "object \"x\" {\n  dynamic \"port\" {\n    for_each = var.list\n\n    content {\n      to = port.value\n    }\n  }\n}\n",
			expected: "x.object.hcl:3,16-24: Invalid dynamic block; The for_each of a dynamic block is not known at install time.",
		},
		{
			name:     "the for_each of a dynamic block of a job",
			file:     appNorca,
			src:      "job \"app\" {\n  dynamic \"group\" {\n    for_each = var.map\n\n    content {\n      n = group.value\n    }\n  }\n}\n",
			expected: "app.norca.hcl:3,16-23: Invalid dynamic block; The for_each of a dynamic block is not known at install time.",
		},
		{
			name:     "the labels of a dynamic block",
			file:     xObject,
			src:      "object \"x\" {\n  dynamic \"port\" {\n    for_each = [1]\n    labels   = var.list\n\n    content {\n    }\n  }\n}\n",
			expected: "x.object.hcl:4,16-24: Invalid dynamic block; The labels of a dynamic block are not known at install time.",
		},
		{
			name:     "the items of a Variable",
			file:     xVar,
			src:      "variable \"x\" {\n  path  = \"p\"\n  items = var.map\n}\n",
			expected: "x.var.hcl:1,1-13: Invalid items; The items of the Variable are not known at install time.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := load(t, module(map[string]string{tt.file: tt.src}))

			_, diags := eval.Evaluate(t.Context(), m, vars, facts, eval.Config{FileBytes: 1 << 20, Resources: 10, Jobs: eval.GenericJobs})
			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

// writtenNumber is a module of one file that writes a number the engine
// can't write, and the one error its evaluation gives.
type writtenNumber struct {
	name     string
	file     string
	src      string
	expected string
}

// writtenNumbers evaluates each module with a decoder of the host for its
// objects, and wants the error.
func writtenNumbers(t *testing.T, tests []writtenNumber) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := load(t, module(map[string]string{tt.file: tt.src}))

			_, diags := eval.Evaluate(t.Context(), m, cty.EmptyObjectVal, facts, eval.Config{
				FileBytes: 1 << 20, Resources: 10, Decoder: sourceDecoder{},
			})

			if got := diags.Error(); len(diags) != 1 || !strings.Contains(got, tt.expected) {
				t.Errorf("diagnostics = %q, want the one that contains %q", got, tt.expected)
			}
		})
	}
}

// An infinite number, what 1/0 gives, has no digits: where the module
// writes one it is an error with the place, not the text +Inf, which no
// reader of the resource takes for a number.
func TestEvaluate_InfiniteNumbers(t *testing.T) {
	t.Parallel()

	const infinite = "the number is infinite and can't be written."

	writtenNumbers(t, []writtenNumber{
		{
			name:     "an output",
			file:     outputsHCL,
			src:      "output \"o\" {\n  value = [1 / 0]\n}\n",
			expected: "outputs.hcl:2,11-18: Value not encoded; The value of output o could not be encoded: " + infinite,
		},
		{
			name:     "an attribute of a resource",
			file:     xACL,
			src:      "policy \"x\" {\n  level = -1 / 0\n}\n",
			expected: "x.acl.hcl:1,1-11: Value not encoded; The value of acl-policy x could not be encoded: " + infinite,
		},
		{
			name:     "an attribute of a block written for a decoder of the host",
			file:     xObject,
			src:      "object \"x\" {\n  spec {\n    a = 1 / 0\n  }\n}\n",
			expected: "x.object.hcl:3,9-14: Value not encoded; The value of a could not be encoded: " + infinite,
		},
		{
			name:     "an item of a Variable",
			file:     xVar,
			src:      "variable \"x\" {\n  path = \"p\"\n\n  items {\n    n = 1 / 0\n  }\n}\n",
			expected: "x.var.hcl:1,1-13: Invalid item; The item n of the Variable has no text: " + infinite,
		},
		{
			name:     "the value of an encoder",
			file:     xACL,
			src:      "policy \"x\" {\n  rules = jsonencode({ a = 1 / 0 })\n}\n",
			expected: `x.acl.hcl:2,11-22: Error in function call; Call to function "jsonencode" failed: a: ` + infinite,
		},
	})
}

// A number of more digits than the bound is an error where the module
// writes it, as a literal over the bound is when the module is read: the
// time to write a number grows by the square of its digits. The numbers of
// the tests are products, which no literal bound sees.
func TestEvaluate_LongNumbers(t *testing.T) {
	t.Parallel()

	const (
		long  = "1e999 * 1e999"
		short = "1e-999 * 1e-999"
		over  = "the number has more than 1000 digits, the limit of the engine."
	)

	writtenNumbers(t, []writtenNumber{
		{
			name:     "an output",
			file:     outputsHCL,
			src:      "output \"o\" {\n  value = [" + long + "]\n}\n",
			expected: "outputs.hcl:2,11-26: Value not encoded; The value of output o could not be encoded: " + over,
		},
		{
			name:     "an attribute of a resource",
			file:     xACL,
			src:      "policy \"x\" {\n  level = " + short + "\n}\n",
			expected: "x.acl.hcl:1,1-11: Value not encoded; The value of acl-policy x could not be encoded: " + over,
		},
		{
			name:     "an attribute of a block written for a decoder of the host",
			file:     xObject,
			src:      "object \"x\" {\n  spec {\n    a = " + long + "\n  }\n}\n",
			expected: "x.object.hcl:3,9-22: Value not encoded; The value of a could not be encoded: " + over,
		},
		{
			name:     "an item of a Variable",
			file:     xVar,
			src:      "variable \"x\" {\n  path = \"p\"\n\n  items {\n    n = " + long + "\n  }\n}\n",
			expected: "x.var.hcl:1,1-13: Invalid item; The item n of the Variable has no text: " + over,
		},
		{
			name:     "the value of an encoder",
			file:     xACL,
			src:      "policy \"x\" {\n  rules = yamlencode({ a = " + short + " })\n}\n",
			expected: `x.acl.hcl:2,11-22: Error in function call; Call to function "yamlencode" failed: a: ` + over,
		},
	})
}

// At the bound a number is written, with all its digits.
func TestEvaluate_NumberAtBound(t *testing.T) {
	t.Parallel()

	res, diags := evaluate(t, module(map[string]string{
		outputsHCL: "output \"large\" {\n  value = 1e999\n}\n\noutput \"small\" {\n  value = 1e-1000\n}\n",
	}), ``)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if got, want := string(res.Outputs[0].Value), "1"+strings.Repeat("0", 999); got != want {
		t.Errorf("large = %s, want 1 and 999 zeros", got)
	}

	if got, want := string(res.Outputs[1].Value), "0."+strings.Repeat("0", 999)+"1"; got != want {
		t.Errorf("small = %s, want 0.000...1 with 1000 digits after the point", got)
	}
}
