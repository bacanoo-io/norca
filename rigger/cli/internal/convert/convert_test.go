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

	want := []string{"README.md", "demo.acl.hcl", "demo.norca.hcl", "examples/debug.vars.hcl", "locals.hcl", "metadata.hcl", "outputs.hcl", "variables.hcl"}
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

// The text of a heredoc is the pack's: the module has it as it is, its
// empty lines too.
func TestPack_KeepsHeredoc(t *testing.T) {
	t.Parallel()

	const text = "first\n\n\nsecond\n"

	pack := t.TempDir()
	for name, data := range map[string]string{
		"metadata.hcl":  "pack {\n  name    = \"p\"\n  version = \"1.0.0\"\n}\n",
		"variables.hcl": "variable \"config\" {\n  type    = string\n  default = <<EOT\n" + text + "EOT\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(pack, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(t.TempDir(), "p")
	if _, err := Pack(pack, "", out); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(out, "variables.hcl"))
	if err != nil || !strings.Contains(string(data), "<<EOT\n"+text+"EOT\n") {
		t.Errorf("variables.hcl does not have the heredoc as the pack has it (%v):\n%s", err, data)
	}
}

func TestTidy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		// expected is empty for a text that stays as it is.
		expected string
	}{
		{name: "one empty line", in: "a = 1\n\nb = 2\n"},
		{name: "more empty lines", in: "a = 1\n\n\n\nb = 2\n\n\nc = 3\n", expected: "a = 1\n\nb = 2\n\nc = 3\n"},
		{name: "empty lines after a comment", in: "# c\n\n\na = 1\n", expected: "# c\n\na = 1\n"},
		{name: "empty lines in a block", in: "a {\n  b = 1\n\n\n  c = 2\n}\n", expected: "a {\n  b = 1\n\n  c = 2\n}\n"},
		{name: "a heredoc, and empty lines after it", in: "a = <<EOT\nx\n\n\ny\nEOT\n\n\n\nb = 2\n", expected: "a = <<EOT\nx\n\n\ny\nEOT\n\nb = 2\n"},
		{name: "an indented heredoc", in: "a {\n  b = <<-EOT\n    x\n\n\n    y\n  EOT\n}\n"},
		{name: "a heredoc with a template", in: "a = <<EOT\n${x}\n\n\n%{ if y }\n\n\nz\n%{ endif }\nEOT\n"},
		{name: "a heredoc that ends with empty lines", in: "a = <<EOT\nx\n\n\nEOT\n"},
		{name: "a block comment", in: "/* one\n\n\n   two */\na = 1\n"},
		{name: "not HCL", in: "a = \"\n\n\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expected := tt.expected
			if expected == "" {
				expected = tt.in
			}

			if got := string(tidy([]byte(tt.in))); got != expected {
				t.Errorf("tidy(%q) =\n%q, want\n%q", tt.in, got, expected)
			}
		})
	}
}
