// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package operatorinit

import (
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/objectlint"
	"github.com/hashicorp/nomad/helper/resourcespec"
	"github.com/shoenig/test/must"
)

func TestKebab(t *testing.T) {
	must.Eq(t, "postgres-cluster", kebab("PostgresCluster"))
	must.Eq(t, "load-balancer", kebab("LoadBalancer"))
	must.Eq(t, "http-route", kebab("HTTPRoute"))
	must.Eq(t, "widget", kebab("Widget"))
}

func TestRender(t *testing.T) {
	files, err := Render(Options{Group: "db.example.com", Kind: "PostgresCluster", SDKPath: "/src/norca/nomad"})
	must.NoError(t, err)
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = string(f.Content)
	}
	for _, p := range []string{
		"go.mod", "main.go", "types.go", "reconcile.go", "webhook.go", "reconcile_test.go", "envtest_test.go",
		"README.md", ".gitignore", "postgrescluster.kind.hcl", "postgrescluster.schema.json", "examples/postgrescluster.hcl",
		"pack/postgres-cluster-operator/metadata.hcl", "pack/postgres-cluster-operator/variables.hcl",
		"pack/postgres-cluster-operator/values.schema.json", "pack/postgres-cluster-operator/README.md",
		"pack/postgres-cluster-operator/templates/_helpers.tpl",
		"pack/postgres-cluster-operator/templates/postgrescluster.kind.hcl.tpl",
		"pack/postgres-cluster-operator/templates/controller.acl.hcl.tpl",
		"pack/postgres-cluster-operator/templates/controller.nomad.tpl",
	} {
		_, ok := byPath[p]
		must.True(t, ok, must.Sprintf("missing %s", p))
	}

	// No template residue, no license header of the generator.
	for p, content := range byPath {
		must.StrNotContains(t, content, "<%", must.Sprintf("%s", p))
		must.StrNotContains(t, content, "SPDX", must.Sprintf("%s", p))
	}

	// The Go files are gofmt-clean Go.
	for p, content := range byPath {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		formatted, err := format.Source([]byte(content))
		must.NoError(t, err, must.Sprintf("%s:\n%s", p, content))
		must.Eq(t, string(formatted), content, must.Sprintf("%s is not gofmt-clean", p))
	}
	must.StrContains(t, byPath["go.mod"], "module example.com/postgres-cluster-operator")
	must.StrContains(t, byPath["go.mod"], "github.com/hashicorp/nomad/controller => /src/norca/nomad/controller")
	must.StrContains(t, byPath["types.go"], `controller.Kind{Group: "db.example.com", Kind: "PostgresCluster"}`)
	must.StrContains(t, byPath["types.go"], `"db.example.com/postgres-cluster-cleanup"`)

	// The kind and the sample pass the lint.
	dir := t.TempDir()
	for p, content := range byPath {
		path := filepath.Join(dir, filepath.FromSlash(p))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	kindPath := filepath.Join(dir, "postgrescluster.kind.hcl")
	kind, err := resourcespec.ParseObjectKind(kindPath, []byte(byPath["postgrescluster.kind.hcl"]), false)
	must.NoError(t, err)
	must.True(t, kind.Namespaced)
	samplePath := filepath.Join(dir, "examples", "postgrescluster.hcl")
	sample, err := resourcespec.ParseCustomObject(samplePath, []byte(byPath["examples/postgrescluster.hcl"]), false)
	must.NoError(t, err)
	report, err := objectlint.Lint(kind, nil)
	must.NoError(t, err)
	must.False(t, report.HasErrors(), must.Sprintf("%v", report.Findings))
	report, err = objectlint.Lint(kind, []*api.CustomObject{sample})
	must.NoError(t, err)
	must.False(t, report.HasErrors(), must.Sprintf("%v", report.Findings))
	must.Eq(t, "hello", report.Objects[0].Spec["message"])
}

func TestRender_ClusterScopedAndErrors(t *testing.T) {
	files, err := Render(Options{Group: "infra.example.com", Kind: "Dns", ClusterScoped: true, Name: "dns", Module: "git.example.com/dns"})
	must.NoError(t, err)
	for _, f := range files {
		switch f.Path {
		case "go.mod":
			must.StrContains(t, string(f.Content), "module git.example.com/dns")
			must.StrContains(t, string(f.Content), "// replace (")
		case "dns.kind.hcl":
			must.StrContains(t, string(f.Content), "namespaced  = false")
		case "examples/dns.hcl":
			must.StrNotContains(t, string(f.Content), "namespace")
		case "pack/dns/templates/controller.acl.hcl.tpl":
			must.StrContains(t, string(f.Content), `namespace "*"`)
		}
	}

	_, err = Render(Options{Group: "nodot", Kind: "X"})
	must.ErrorContains(t, err, "invalid group")
	_, err = Render(Options{Group: "a.b", Kind: "lower"})
	must.ErrorContains(t, err, "invalid kind")
	_, err = Render(Options{Group: "a.b", Kind: "X", Name: "Bad_Name"})
	must.ErrorContains(t, err, "invalid operator name")
}

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	written, err := Generate(Options{Dir: dir, Group: "db.example.com", Kind: "Widget"})
	must.NoError(t, err)
	must.SliceContains(t, written, "main.go")
	_, err = os.Stat(filepath.Join(dir, ".gitignore"))
	must.NoError(t, err)

	// Existing files are kept unless forced.
	_, err = Generate(Options{Dir: dir, Group: "db.example.com", Kind: "Widget"})
	must.ErrorContains(t, err, "files exist")
	_, err = Generate(Options{Dir: dir, Group: "db.example.com", Kind: "Widget", Force: true})
	must.NoError(t, err)
}
