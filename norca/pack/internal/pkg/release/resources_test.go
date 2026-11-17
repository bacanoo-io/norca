// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package release

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

func TestExternalServiceTemplate(t *testing.T) {
	must.Eq(t, api.ReleaseResourceExternalService, TemplateType("templates/orders-db.external.hcl.tpl"))
	must.True(t, IsResourceTemplate("templates/orders-db.external.hcl.tpl"))

	res, err := ParseResource("templates/orders-db.external.hcl.tpl", `
external_service "orders-db" {
  address = "10.20.0.5"
  port    = 5432
}
`)
	must.NoError(t, err)
	must.Eq(t, api.ReleaseResourceExternalService, res.Type)
	must.Eq(t, "orders-db", res.ExternalService.Service.Name)

	// the services come after the admission policies and before the objects
	// and jobs
	resources := []*api.ReleaseResource{
		{Type: api.ReleaseResourceJob},
		{Type: api.ReleaseResourceObject},
		res,
		{Type: api.ReleaseResourceAdmissionPolicy},
	}
	SortResources(resources)
	var order []string
	for _, r := range resources {
		order = append(order, r.Type)
	}
	must.Eq(t, []string{"admission-policy", "external-service", "object", "job"}, order)
}

// TestParseResource_NoFiles checks that the rendered resource templates of a
// pack read no file: a server renders packs too (Packs.Deploy), and file()
// in a kind or an object would put a file of the server into the release.
func TestParseResource_NoFiles(t *testing.T) {
	const canary = "file-canary-3e9a"
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	must.NoError(t, os.WriteFile(secret, []byte(canary), 0o600))
	abs := filepath.ToSlash(secret)
	// Relative paths were read from the working directory of the process.
	t.Chdir(dir)

	for _, expr := range []string{
		`file("` + abs + `")`,
		`file("secret.txt")`,
		`"${file("` + abs + `")}"`,
	} {
		res, err := ParseResource("templates/widget.kind.hcl.tpl", `
kind "Widget" {
  group  = "example.com"
  schema = `+expr+`
}
`)
		must.Error(t, err, must.Sprint(expr, res))
		must.StrContains(t, err.Error(), "file() can't be used here", must.Sprint(expr))
		must.StrNotContains(t, err.Error(), canary)

		res, err = ParseResource("templates/w1.object.hcl.tpl", `
kind = "example.com/Widget"
name = "w1"
spec {
  v = `+expr+`
}
`)
		must.Error(t, err, must.Sprint(expr, res))
		must.StrContains(t, err.Error(), "file() can't be used here", must.Sprint(expr))
		must.StrNotContains(t, err.Error(), canary)
	}

	// Inline content and the JSON functions work as before.
	res, err := ParseResource("templates/widget.kind.hcl.tpl", `
kind "Widget" {
  group  = "example.com"
  schema = jsonencode({ type = "object" })
}
`)
	must.NoError(t, err)
	must.Eq(t, `{"type":"object"}`, res.ObjectKind.Schema)
}
