// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// order evaluates a stack of the releases app and db, with the module block
// of app and the locals given, and returns its releases in their order,
// each with the releases it depends on: "db app(db)". The names sort
// against the references: app reads db.
func order(t *testing.T, app, locals string) string {
	t.Helper()

	fsys := fstest.MapFS{
		"s/stack.hcl": {Data: []byte("rigger {\n  name = \"shop\"\n}\n\nmodule \"app\" {\n  source = \"./modules/m\"\n" + app + "}\n\n" +
			"module \"db\" {\n  source = \"./modules/m\"\n}\n\n" + locals)},
		"s/modules/m/metadata.hcl":  {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/m/variables.hcl": {Data: []byte("variable \"peer\" {\n  default = \"\"\n}\n")},
		"s/modules/m/outputs.hcl":   {Data: []byte("output \"service\" {\n  value = \"${rigger.release}-svc\"\n}\n\noutput \"peer\" {\n  value = var.peer\n}\n")},
	}

	s, diags := lang.LoadStack(t.Context(), fsys, "s", limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	vars, diags := eval.Variables(s.Vars, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	res, diags := eval.EvaluateStack(t.Context(), s, vars, func(string) (string, bool) { return "", false }, eval.Config{
		FileBytes: 1 << 20, Resources: 10, Jobs: eval.GenericJobs,
	})
	if diags.HasErrors() {
		return diags.Error()
	}

	out := make([]string, len(res.Releases))
	for i, r := range res.Releases {
		out[i] = r.Name
		if len(r.DependsOn) > 0 {
			out[i] += "(" + strings.Join(r.DependsOn, ",") + ")"
		}

		for _, o := range r.Result.Outputs {
			if o.Name == "peer" && string(o.Value) != `""` {
				out[i] += "=" + string(o.Value)
			}
		}
	}

	return strings.Join(out, " ")
}

// A release is installed after the releases it reads, however it names
// them: as an attribute, as an index with the name, or as the whole root
// module, which reads every other release.
func TestEvaluateStack_ReleaseReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		app      string
		locals   string
		expected string
	}{
		{name: "no reference", expected: "app db"},
		{name: "by its name", app: "  values = { peer = module.db.service }\n", expected: `db app(db)="db-svc"`},
		{name: "an index with the name", app: "  values = { peer = module[\"db\"].service }\n", expected: `db app(db)="db-svc"`},
		{name: "the whole root", app: "  values = { peer = join(\",\", keys(module)) }\n", expected: `db app(db)="db"`},
		{name: "depends_on", app: "  depends_on = [module.db]\n", expected: "db app(db)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := order(t, tt.app, tt.locals); got != tt.expected {
				t.Errorf("releases = %s, want %s", got, tt.expected)
			}
		})
	}
}
