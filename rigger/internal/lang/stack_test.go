// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
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
