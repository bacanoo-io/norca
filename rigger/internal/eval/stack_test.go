// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/sign"
)

// order evaluates a stack of the releases app and db, with the module block
// of app and the file of the locals given, and returns its releases in
// their order, each with the releases it depends on and the value its
// module was given: `db app(db)="db-svc"`. The names sort against the
// references: app reads db.
func order(t *testing.T, app, locals string) string {
	t.Helper()

	fsys := fstest.MapFS{
		"s/stack.hcl": {Data: []byte("rigger {\n  name = \"shop\"\n}\n\nmodule \"app\" {\n  source = \"./modules/m\"\n" + app + "}\n\n" +
			"module \"db\" {\n  source = \"./modules/m\"\n}\n")},
		"s/locals.hcl":              {Data: []byte(locals)},
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

// A release depends on the releases it reads through the locals of the
// stack too: a local is not a release, and what it reads is read by the
// release that reads the local.
func TestEvaluateStack_DependsOnThroughLocals(t *testing.T) {
	t.Parallel()

	const peer = "  values = { peer = local.a }\n"

	tests := []struct {
		name     string
		locals   string
		expected string
	}{
		{name: "a local that reads a release", locals: "locals {\n  a = module.db.service\n}\n", expected: `db app(db)="db-svc"`},
		{name: "a local that reads such a local", locals: "locals {\n  a = upper(local.b)\n  b = module.db.service\n}\n", expected: `db app(db)="DB-SVC"`},
		{name: "a local that reads every release", locals: "locals {\n  a = join(\",\", keys(module))\n}\n", expected: "Values refer to each other"},
		{name: "a local that reads no release", locals: "locals {\n  a = \"v\"\n}\n", expected: `app="v" db`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := order(t, peer, tt.locals); !strings.Contains(got, tt.expected) {
				t.Errorf("releases = %s, want %s", got, tt.expected)
			}
		})
	}
}

// exampleStacks returns the directories of the stacks of examples/, found
// by walking: a stack that is added is tested without a change of the tests.
func exampleStacks(t *testing.T) []string {
	t.Helper()

	var dirs []string

	err := fs.WalkDir(os.DirFS("../../examples"), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "stack.hcl" {
			dirs = append(dirs, path.Dir(p))
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dirs) == 0 {
		t.Fatal("examples/ has no stack")
	}

	return dirs
}

// The trust of the stacks of examples/ is trust a stack of today has
// (design 8.2): the blocks evaluate, a rule of a registry names a registry
// of the stack, every key is a public key that verifies signatures, and the
// signers of the lock file are keys of the trust. A text in the place of a
// key (as the examples had) is found only when a module is checked against
// it, which needs the module.
func TestStackTrust_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range exampleStacks(t) {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			s, diags := lang.LoadStack(t.Context(), os.DirFS("../../examples"), dir, limits)
			if diags.HasErrors() {
				t.Fatalf("LoadStack(): %s", diags)
			}

			rules, diags := eval.StackTrust(s, limits.FileBytes)
			for _, d := range diags {
				t.Errorf("StackTrust(): %s", d)
			}

			checkLockedSigners(t, s, trustKeys(t, s, rules))
		})
	}
}

// trustKeys checks the trust rules of a stack, and returns the IDs of the
// keys of each by its registry (the default rule by "").
func trustKeys(t *testing.T, s *lang.Stack, rules []eval.TrustRule) map[string][]string {
	t.Helper()

	registries, diags := s.Registries()
	if diags.HasErrors() {
		t.Fatalf("Registries(): %s", diags)
	}

	keys := map[string][]string{}

	for _, rule := range rules {
		keys[rule.Registry] = []string{}

		if _, ok := registries[rule.Registry]; rule.Registry != "" && !ok {
			t.Errorf("a trust block is for the registry %q, which the stack does not have", rule.Registry)
		}

		if rule.Require && len(rule.Keys) == 0 {
			t.Errorf("the trust of the registry %q requires a signature and has no key", rule.Registry)
		}

		for i, text := range rule.Keys {
			key, err := sign.ParsePublicKey([]byte(text))
			if err != nil {
				t.Errorf("public key %d of the trust of the registry %q: %v", i+1, rule.Registry, err)

				continue
			}

			keys[rule.Registry] = append(keys[rule.Registry], key.ID)
		}
	}

	return keys
}

// checkLockedSigners fails the test when the lock file of a stack records,
// for a module of a registry of the stack, a signer that is no key of the
// trust of that registry (its own rule, else the default one). A sync
// writes no other; the keys of a namespace of the global registry, which
// its index gives, are not in a stack and are not checked.
func checkLockedSigners(t *testing.T, s *lang.Stack, keys map[string][]string) {
	t.Helper()

	for _, r := range s.Releases {
		entry := s.Lock.Module(r.Source)
		if r.Address == nil || r.Address.Registry == "" || entry == nil {
			continue
		}

		trusted, ok := keys[r.Address.Registry]
		if !ok {
			trusted = keys[""]
		}

		for _, id := range entry.Signers {
			if !slices.Contains(trusted, id) {
				t.Errorf("the lock file records the signer %s of %s, which is no key of the trust of the stack", id, r.Source)
			}
		}
	}
}
