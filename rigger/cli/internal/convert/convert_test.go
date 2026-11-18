// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package convert

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPack(t *testing.T) {
	t.Parallel()

	out := filepath.Join(t.TempDir(), "demo")

	res, err := Pack("testdata/pack", "testdata/examples", out)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"README.md", "demo.norca.hcl", "examples/debug.vars.hcl", "locals.hcl", "metadata.hcl", "outputs.hcl", "variables.hcl"}
	if !slices.Equal(res.Files, want) {
		t.Fatalf("files = %v, want %v", res.Files, want)
	}

	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	contains(t, "metadata.hcl", read("metadata.hcl"),
		[]string{"# Licence notice.\n", "module {", `name        = "demo"`, `url         = "https://example.com"`, "requires {", `tab "general"`},
		[]string{"pack {", "app {"})
	contains(t, "variables.hcl", read("variables.hcl"),
		[]string{
			"item {\n      min_length = 1\n    }",
			`one_of = ["debug", "info"]`,
			"attribute \"protocol\" {\n        one_of = [\"tcp\", \"udp\"]",
			`widget = "object-list"`,
		},
		[]string{`variable "namespace"`, `variable "region"`, "item_min_length", "options", "\n\n\n"})

	if ex := read("examples/debug.vars.hcl"); strings.Contains(ex, "namespace") || !strings.Contains(ex, `log_level = "debug"`) {
		t.Errorf("examples/debug.vars.hcl:\n%s", ex)
	}

	if job := read("demo.norca.hcl"); !strings.Contains(job, "# job [[ var \"job_name\" . | quote ]] {\n# }\n") || !strings.HasPrefix(job, "# Licence notice.\n") {
		t.Errorf("demo.norca.hcl:\n%s", job)
	}

	contains(t, "the notes", strings.Join(res.Notes, "\n"),
		[]string{"var.region is the release's now", "examples/debug.hcl: namespace", "demo.norca.hcl: the template of templates/demo.nomad.tpl"}, nil)
}

// contains checks that text has every one of want and none of avoid.
func contains(t *testing.T, name, text string, want, avoid []string) {
	t.Helper()

	for _, s := range want {
		if !strings.Contains(text, s) {
			t.Errorf("%s has no %q:\n%s", name, s, text)
		}
	}

	for _, s := range avoid {
		if strings.Contains(text, s) {
			t.Errorf("%s keeps %q:\n%s", name, s, text)
		}
	}
}

func TestPackRefusesAFullDirectory(t *testing.T) {
	t.Parallel()

	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Pack("testdata/pack", "", out); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("err = %v", err)
	}

	if _, err := Pack("testdata/pack", "testdata/none", filepath.Join(out, "new")); err == nil {
		t.Fatal("a missing examples directory was taken")
	}
}
