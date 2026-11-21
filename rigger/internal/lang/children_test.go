// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca-rig/internal/lang"
)

// tree returns a root with the module app/ calling the module lib/x with
// the module block src, and the files given.
func tree(src string, more map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"app/metadata.hcl":   {Data: []byte(metadata)},
		"app/modules.hcl":    {Data: []byte(src)},
		"lib/x/metadata.hcl": {Data: []byte("module {\n  name    = \"x\"\n  version = \"1.0.0\"\n  library = true\n}\n")},
	}

	for name, data := range more {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}

	return fsys
}

func TestLoadAt_Children(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    fstest.MapFS
		limits   lang.Limits
		expected string
	}{
		{name: "a local child", files: tree("module \"x\" {\n  source = \"../lib/x\"\n  a = 1\n}\n", nil)},
		{
			name:     "outside the root",
			files:    tree("module \"x\" {\n  source = \"../../x\"\n}\n", nil),
			expected: "../../x is outside the files the module was loaded from",
		},
		{
			name:     "not a local source",
			files:    tree("module \"x\" {\n  source = \"norca/x\"\n}\n", nil),
			expected: "norca/x is not a local source",
		},
		{
			name:     "a version of a local source",
			files:    tree("module \"x\" {\n  source  = \"../lib/x\"\n  version = \"1.0.0\"\n}\n", nil),
			expected: "A local source is used as it is",
		},
		{
			name:     "no source",
			files:    tree("module \"x\" {\n  a = 1\n}\n", nil),
			expected: "Missing source",
		},
		{
			name:     "twice",
			files:    tree("module \"x\" {\n  source = \"../lib/x\"\n}\nmodule \"x\" {\n  source = \"../lib/x\"\n}\n", nil),
			expected: "module.x is called already",
		},
		{
			name: "a cycle",
			files: tree("module \"x\" {\n  source = \"../lib/x\"\n}\n", map[string]string{
				"lib/x/modules.hcl": "module \"app\" {\n  source = \"../../app\"\n}\n",
			}),
			expected: "../../app is a module that calls this one already",
		},
		{
			name:     "too deep",
			files:    tree("module \"x\" {\n  source = \"../lib/x\"\n}\n", nil),
			limits:   lang.Limits{ModuleBytes: 1 << 20, FileBytes: 1 << 20, Depth: 1},
			expected: "nested more than 1 deep",
		},
		{
			name: "a resource in a library",
			files: tree("module \"x\" {\n  source = \"../lib/x\"\n}\n", map[string]string{
				"lib/x/x.acl.hcl": "policy \"x\" {}\n",
			}),
			expected: "A module with library = true has no resources",
		},
		{
			name: "the files of a child are named from the module",
			files: tree("module \"x\" {\n  source = \"../lib/x\"\n}\n", map[string]string{
				"lib/x/outputs.hcl": "output {}\n",
			}),
			expected: "../lib/x/outputs.hcl:1,8-9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := limits
			if tt.limits != (lang.Limits{}) {
				l = tt.limits
			}

			m, diags := lang.LoadAt(t.Context(), tt.files, "app", l)

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				if len(m.Children) != 1 || m.Children[0].Module.Metadata.Name != "x" || m.Children[0].Args["a"] == nil {
					t.Errorf("children = %+v, want x with the argument a", m.Children)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
