// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package operatorinit

import (
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/helper/resourcespec"

	"github.com/bacanoo-io/norca/rigger"

	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
)

// The words the tests of the package repeat.
const (
	testGroup   = "db.example.com"
	testKind    = "PostgresCluster"
	testWidget  = "Widget"
	cmdOperator = "operator"
	cmdInit     = "init"
)

func TestOptions_OperatorName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "camel case", opts: Options{Kind: testKind}, want: "postgres-cluster-operator"},
		{name: "acronym", opts: Options{Kind: "HTTPRoute"}, want: "http-route-operator"},
		{name: "one word", opts: Options{Kind: testWidget}, want: "widget-operator"},
		{name: "given", opts: Options{Kind: testWidget, Name: "gadget"}, want: "gadget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.opts.OperatorName(); got != tt.want {
				t.Errorf("OperatorName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	sdk := filepath.Join(t.TempDir(), "norca")

	files, err := Render(Options{Group: testGroup, Kind: testKind, SDKPath: sdk})
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = string(f.Content)
	}

	for _, p := range []string{
		"go.mod", "main.go", "types.go", "reconcile.go", "webhook.go", "reconcile_test.go", "envtest_test.go",
		"README.md", ".gitignore", "postgrescluster.kind.hcl", "postgrescluster.schema.json", "examples/postgrescluster.hcl",
		"module/postgres-cluster-operator/metadata.hcl", "module/postgres-cluster-operator/variables.hcl",
		"module/postgres-cluster-operator/locals.hcl",
		"module/postgres-cluster-operator/postgrescluster.kind.hcl",
		"module/postgres-cluster-operator/files/postgrescluster.schema.json",
		"module/postgres-cluster-operator/controller.acl.hcl", "module/postgres-cluster-operator/controller.norca.hcl",
		"module/postgres-cluster-operator/examples/dev.vars.hcl", "module/postgres-cluster-operator/README.md",
	} {
		if _, ok := byPath[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}

	checkFiles(t, byPath)

	abs, err := filepath.Abs(sdk)
	if err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string]string{
		"go.mod":   "github.com/bacanoo-io/norca/norca/controller => " + filepath.ToSlash(abs) + "/controller",
		"types.go": `"db.example.com/postgres-cluster-cleanup"`,
	} {
		if !strings.Contains(byPath[p], want) {
			t.Errorf("%s does not contain %q", p, want)
		}
	}

	if !strings.Contains(byPath["go.mod"], "module example.com/postgres-cluster-operator") {
		t.Errorf("go.mod has not the module path:\n%s", byPath["go.mod"])
	}

	// The kind (which reads its schema next to it) and the sample parse with
	// Norca's decoder.
	dir := t.TempDir()
	if _, err := Generate(Options{Dir: dir, Group: testGroup, Kind: testKind}); err != nil {
		t.Fatal(err)
	}

	kindPath := filepath.Join(dir, "postgrescluster.kind.hcl")

	kind, err := resourcespec.ParseObjectKind(kindPath, []byte(byPath["postgrescluster.kind.hcl"]), false)
	if err != nil {
		t.Fatal(err)
	}

	if !kind.Namespaced {
		t.Error("the kind is not namespaced")
	}

	samplePath := filepath.Join(dir, "examples", "postgrescluster.hcl")
	if _, err := resourcespec.ParseCustomObject(samplePath, []byte(byPath["examples/postgrescluster.hcl"]), false); err != nil {
		t.Fatal(err)
	}
}

// checkFiles checks every file of a project: no template residue, no
// license header of the generator, no pack, gofmt-clean Go.
func checkFiles(t *testing.T, byPath map[string]string) {
	t.Helper()

	for p, content := range byPath {
		// No template residue, no license header of the generator, no pack.
		for _, unwanted := range []string{"<%", "SPDX", "pack/"} {
			if strings.Contains(content, unwanted) {
				t.Errorf("%s contains %q", p, unwanted)
			}
		}

		// The Go files are gofmt-clean Go.
		if strings.HasSuffix(p, ".go") {
			formatted, err := format.Source([]byte(content))
			if err != nil {
				t.Errorf("%s: %v\n%s", p, err, content)
			} else if string(formatted) != content {
				t.Errorf("%s is not gofmt-clean", p)
			}
		}
	}
}

func TestRender_ClusterScopedAndErrors(t *testing.T) {
	t.Parallel()

	files, err := Render(Options{Group: "infra.example.com", Kind: "Dns", ClusterScoped: true, Name: "dns", Module: "git.example.com/dns"})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{
		"go.mod":                        {"module git.example.com/dns", "// replace ("},
		"dns.kind.hcl":                  {"namespaced  = false"},
		"module/dns/controller.acl.hcl": {`namespace "*"`},
	}

	for _, f := range files {
		for _, s := range want[f.Path] {
			if !strings.Contains(string(f.Content), s) {
				t.Errorf("%s does not contain %q", f.Path, s)
			}
		}

		if f.Path == "examples/dns.hcl" && strings.Contains(string(f.Content), "namespace") {
			t.Errorf("the sample of a cluster-scoped kind has a namespace:\n%s", f.Content)
		}
	}

	for _, tt := range []struct {
		opts Options
		err  string
	}{
		{opts: Options{Group: "nodot", Kind: "X"}, err: "invalid group"},
		{opts: Options{Group: "a.b", Kind: "lower"}, err: "invalid kind"},
		{opts: Options{Group: "a.b", Kind: "X", Name: "Bad_Name"}, err: "invalid operator name"},
	} {
		if _, err := Render(tt.opts); err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("Render(%+v) = %v, want %q", tt.opts, err, tt.err)
		}
	}
}

// The operator module loads, validates and evaluates with Rigger and Norca's
// decoders: the kind, the policy bound to the job and the job, in the
// namespace of the release; a cluster-scoped kind gets its rule in "*".
func TestRender_Module(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    Options
		values  string
		kinds   string
		inSpecs []string
		absent  []string
	}{
		{
			name:  "namespaced",
			opts:  Options{Group: testGroup, Kind: testKind},
			kinds: "acl-policy controller, object-kind postgrescluster, job controller",
			inSpecs: []string{
				`"Kind":"PostgresCluster"`, `object \"db.example.com/PostgresCluster\"`,
				`"ID":"postgres-cluster-operator"`, `"Driver":"raw_exec"`, `"-namespace","apps"`,
			},
			absent: []string{`namespace \"*\"`},
		},
		{
			name:    "cluster-scoped",
			opts:    Options{Group: "infra.example.com", Kind: "Dns", ClusterScoped: true, Name: "dns"},
			kinds:   "acl-policy controller, object-kind dns, job controller",
			inSpecs: []string{`namespace \"*\"`, `"Namespaced":false`},
		},
		{
			name:    "dev example",
			opts:    Options{Group: testGroup, Kind: testWidget},
			values:  "examples/dev.vars.hcl",
			kinds:   "object-kind widget, job controller",
			inSpecs: []string{`"Driver":"mock_driver"`, `"run_for":"87600h"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			specs, kinds := evaluateModule(t, tt.opts, tt.values)

			if kinds != tt.kinds {
				t.Errorf("resources = %s, want %s", kinds, tt.kinds)
			}

			for _, want := range tt.inSpecs {
				if !strings.Contains(specs, want) {
					t.Errorf("the specs do not contain %s:\n%s", want, specs)
				}
			}

			for _, unwanted := range tt.absent {
				if strings.Contains(specs, unwanted) {
					t.Errorf("the specs contain %s", unwanted)
				}
			}
		})
	}
}

// evaluateModule writes the project of opts, validates its module and
// evaluates it in the namespace apps with the values of the file (none if
// empty); it returns the specs and the resources, "kind name, ...".
func evaluateModule(t *testing.T, opts Options, valuesFile string) (string, string) {
	t.Helper()

	opts.Dir = t.TempDir()
	if _, err := Generate(opts); err != nil {
		t.Fatal(err)
	}

	moduleDir := filepath.Join(opts.Dir, "module", opts.OperatorName())

	engine, err := rigger.New(rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}))
	if err != nil {
		t.Fatal(err)
	}

	m, diags := engine.LoadModule(t.Context(), os.DirFS(moduleDir))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if diags := engine.ValidateModule(t.Context(), m); diags.HasErrors() {
		t.Fatal(diags)
	}

	var values rigger.Values

	if valuesFile != "" {
		src, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(valuesFile)))
		if err != nil {
			t.Fatal(err)
		}

		if values, diags = rigger.ParseValues(valuesFile, src); diags.HasErrors() {
			t.Fatal(diags)
		}
	}

	res, diags := engine.Evaluate(t.Context(), m, rigger.Install{Release: opts.OperatorName(), Namespace: "apps", Region: "global", Values: values})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var (
		specs strings.Builder
		kinds []string
	)

	for _, r := range res.Resources {
		kinds = append(kinds, string(r.Kind)+" "+r.Name)
		specs.Write(r.Spec)
	}

	return specs.String(), strings.Join(kinds, ", ")
}

func TestGenerate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	written, err := Generate(Options{Dir: dir, Group: testGroup, Kind: testWidget})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(written, " "), "main.go") {
		t.Errorf("written = %v, want main.go", written)
	}

	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err != nil {
		t.Error(err)
	}

	// Existing files are kept unless forced.
	if _, err := Generate(Options{Dir: dir, Group: testGroup, Kind: testWidget}); err == nil || !strings.Contains(err.Error(), "files exist") {
		t.Errorf("second Generate = %v, want files exist", err)
	}

	if _, err := Generate(Options{Dir: dir, Group: testGroup, Kind: testWidget, Force: true}); err != nil {
		t.Error(err)
	}
}
