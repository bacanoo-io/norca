// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shoenig/test/must"
)

// TestParseObjects_NoFiles checks the parsers of the texts that are not a
// file of the caller (the resource templates of a pack, which a server
// renders too): file() reads nothing there, by an absolute path or a
// relative one, in an attribute or in a string template. The parsers of the
// command line keep file().
func TestParseObjects_NoFiles(t *testing.T) {
	const canary = "file-canary-7d1f"
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	must.NoError(t, os.WriteFile(secret, []byte(canary), 0o600))
	// HCL string literals take forward slashes on every platform.
	abs := filepath.ToSlash(secret)
	t.Chdir(dir)

	kind := func(expr string) []byte {
		return []byte(`kind "Widget" {
  group  = "example.com"
  schema = ` + expr + `
}
`)
	}
	object := func(expr string) []byte {
		return []byte(`kind = "example.com/Widget"
name = "w1"
spec {
  v = ` + expr + `
}
`)
	}
	exprs := []string{
		`file("` + abs + `")`,
		`file("secret.txt")`,
		`"${file("` + abs + `")}"`,
		`jsonencode({ v = file("` + abs + `") })`,
	}

	for _, expr := range exprs {
		k, err := ParseObjectKindNoFiles("widget.kind.hcl", kind(expr), false)
		must.Error(t, err, must.Sprint(expr))
		must.Nil(t, k, must.Sprint(expr))
		must.StrContains(t, err.Error(), "file() can't be used here", must.Sprint(expr))
		must.StrNotContains(t, err.Error(), canary)

		o, err := ParseCustomObjectNoFiles("w1.object.hcl", object(expr), false)
		must.Error(t, err, must.Sprint(expr))
		must.Nil(t, o, must.Sprint(expr))
		must.StrContains(t, err.Error(), "file() can't be used here", must.Sprint(expr))
		must.StrNotContains(t, err.Error(), canary)
	}

	// The other functions stay.
	k, err := ParseObjectKindNoFiles("widget.kind.hcl", kind(`jsonencode({ type = "object" })`), false)
	must.NoError(t, err)
	must.Eq(t, `{"type":"object"}`, k.Schema)
	o, err := ParseCustomObjectNoFiles("w1.object.hcl", object(`jsondecode("[1]")`), false)
	must.NoError(t, err)
	must.Eq(t, map[string]any{"v": []any{float64(1)}}, o.Spec)

	// The command line reads the files of its user, next to the
	// specification.
	k, err = ParseObjectKind(filepath.Join(dir, "widget.kind.hcl"), kind(`file("secret.txt")`), false)
	must.NoError(t, err)
	must.Eq(t, canary, k.Schema)
	o, err = ParseCustomObject(filepath.Join(dir, "w1.object.hcl"), object(`file("`+abs+`")`), false)
	must.NoError(t, err)
	must.True(t, strings.Contains(o.Spec["v"].(string), canary))
}
