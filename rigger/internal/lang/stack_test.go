// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/version"
)

// loadStack loads the stack of the stack.hcl given, with one module of its
// own in modules/m.
func loadStack(t *testing.T, stack string) (*lang.Stack, string) {
	t.Helper()

	s, diags := lang.LoadStack(t.Context(), fstest.MapFS{
		"s/stack.hcl":              {Data: []byte(stack)},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name = \"m\"\n}\n")},
	}, "s", limits)

	var all strings.Builder
	for _, d := range diags {
		all.WriteString(d.Error() + "\n")
	}

	return s, all.String()
}

// examplesDir is examples/ of the module, seen from this package.
const examplesDir = "../../examples"

// examplesWith returns the directories of examples/ that hold a file of the
// name given, found by walking: an example that is added is tested without
// a change of the tests.
func examplesWith(t *testing.T, name string) []string {
	t.Helper()

	var dirs []string

	err := fs.WalkDir(os.DirFS(examplesDir), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			dirs = append(dirs, path.Dir(p))
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return dirs
}

// meets reports whether a version meets a constraint; it fails the test
// when one of the two can't be read.
func meets(t *testing.T, constraint, v string) bool {
	t.Helper()

	c, err := version.ParseConstraint(constraint)
	if err != nil {
		t.Errorf("the constraint %q: %v", constraint, err)

		return false
	}

	parsed, err := version.Parse(v)
	if err != nil {
		t.Errorf("the version %q: %v", v, err)

		return false
	}

	return c.Check(parsed)
}

// The stacks of examples/ are stacks of today (design 6.4, 7.5): each loads
// without a diagnostic, and its lock file is the lock of this stack: one
// entry for each source its releases and parts name, of a version that
// meets the constraint written, and no entry of a source it does not name.
func TestLoadStack_Examples(t *testing.T) {
	t.Parallel()

	stacks := examplesWith(t, "stack.hcl")
	if len(stacks) == 0 {
		t.Fatal("examples/ has no stack")
	}

	for _, dir := range stacks {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			s, diags := lang.LoadStack(t.Context(), os.DirFS(examplesDir), dir, limits)
			for _, d := range diags {
				t.Errorf("LoadStack(): %s", d)
			}

			if diags.HasErrors() {
				return
			}

			modules, parts := map[string]bool{}, map[string]bool{}

			for _, r := range s.Releases {
				if r.Address == nil {
					continue // a module of the stack's own is not locked (design 6.4)
				}

				modules[r.Source] = true

				checkLocked(t, r.Source, r.Version, s.Lock.Module(r.Source))
			}

			for _, p := range s.Parts {
				parts[p.Source] = true

				checkLocked(t, p.Source, p.Version, s.Lock.Stack(p.Source))
			}

			if s.Lock == nil {
				return
			}

			for _, m := range s.Lock.Modules {
				if !modules[m.Source] {
					t.Errorf("the lock file has an entry of the module %s, which the stack does not name", m.Source)
				}
			}

			for _, p := range s.Lock.Stacks {
				if !parts[p.Source] {
					t.Errorf("the lock file has an entry of the stack %s, which the stack does not name", p.Source)
				}
			}
		})
	}
}

// checkLocked fails the test unless entry is the entry of a source of a
// stack: there, and of a version that meets the constraint of the stack.
func checkLocked(t *testing.T, src, constraint string, entry *lang.LockedModule) {
	t.Helper()

	switch {
	case entry == nil:
		t.Errorf("the lock file has no entry of %s", src)
	case constraint != "" && !meets(t, constraint, entry.Version):
		t.Errorf("the lock file has %s at %s, which does not meet %q", src, entry.Version, constraint)
	}
}

// A registry has one block. With two of a name the second replaced the
// first in silence, and the modules of the stack came from the repository
// written last.
func TestLoadStack_Registries(t *testing.T) {
	t.Parallel()

	const module = "\nmodule \"m\" {\n  source = \"./modules/m\"\n}\n"

	tests := []struct {
		name     string
		rigger   string
		expected string
		sources  map[string]string
	}{
		{
			name:    "two registries",
			rigger:  "rigger {\n  name = \"s\"\n\n  registry \"a\" {\n    source = \"github.com/a/one\"\n  }\n\n  registry \"b\" {\n    source = \"github.com/b/two\"\n  }\n}\n",
			sources: map[string]string{"a": "github.com/a/one", "b": "github.com/b/two"},
		},
		{
			name:     "a registry twice",
			rigger:   "rigger {\n  name = \"s\"\n\n  registry \"r\" {\n    source = \"github.com/a/one\"\n  }\n\n  registry \"r\" {\n    source = \"github.com/evil/two\"\n  }\n}\n",
			expected: `Duplicate registry; The registry "r" is declared already at stack.hcl:4,3-15`,
			sources:  map[string]string{"r": "github.com/a/one"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, diags := loadStack(t, tt.rigger+module)

			switch {
			case tt.expected == "" && diags != "":
				t.Fatalf("LoadStack() = %s, want no diagnostics", diags)
			case !strings.Contains(diags, tt.expected):
				t.Errorf("diagnostics = %q, want them to contain %q", diags, tt.expected)
			}

			got, regDiags := s.Registries()
			if regDiags.HasErrors() {
				t.Fatal(regDiags)
			}

			if len(got) != len(tt.sources) {
				t.Errorf("registries = %v, want %v", got, tt.sources)
			}

			for name, source := range tt.sources {
				if got[name] != source {
					t.Errorf("registry %s = %q, want %q", name, got[name], source)
				}
			}
		})
	}
}

// max_history is the number of revisions the server keeps, or 0 for what
// the server keeps by itself: a negative number is neither.
func TestLoadStack_MaxHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		expected string
		history  int
	}{
		{name: "a number", value: "10", history: 10},
		{name: "zero", value: "0"},
		{name: "a negative number", value: "-5", expected: "Invalid max_history; max_history is -5"},
		{name: "not a whole number", value: "1.5", expected: "Unsuitable value type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, diags := loadStack(t, "rigger {\n  name = \"s\"\n}\n\nmodule \"m\" {\n  source      = \"./modules/m\"\n  max_history = "+tt.value+"\n}\n")

			if tt.expected != "" {
				if !strings.Contains(diags, tt.expected) {
					t.Errorf("diagnostics = %q, want them to contain %q", diags, tt.expected)
				}

				return
			}

			if diags != "" {
				t.Fatalf("LoadStack() = %s, want no diagnostics", diags)
			}

			if len(s.Releases) != 1 || s.Releases[0].MaxHistory != tt.history {
				t.Errorf("releases = %+v, want one with max_history %d", s.Releases, tt.history)
			}
		})
	}
}
