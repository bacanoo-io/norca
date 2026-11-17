// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca-rig/internal/lang"
)

var limits = lang.Limits{ModuleBytes: 16 << 20, FileBytes: 1 << 20}

// The modules of examples/ load without an error.
func TestLoad_Examples(t *testing.T) {
	t.Parallel()

	dirs := []string{
		"module/web",
		"registry/modules/redis",
		"registry/modules/nats",
		"registry/modules/postgres",
		"registry/modules/lib/placement",
		"registry/stacks/observability/modules/dashboards",
		"stack/platform/modules/shop-api",
		"stack/platform/modules/shop-worker",
	}

	for _, dir := range dirs {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			m, diags := lang.Load(t.Context(), os.DirFS("../../examples/"+dir), limits)
			for _, d := range diags {
				t.Errorf("%s", d)
			}

			t.Logf("%s %s: %d variables, %d rules, %d resources, %d examples, %d tests",
				m.Metadata.Name, m.Metadata.Version, len(m.Variables), len(m.Rules), len(m.Resources), len(m.Examples), len(m.Tests))
		})
	}
}

const metadata = `module {
  name    = "app"
  version = "1.0.0"
}
`

const (
	metadataHCL  = "metadata.hcl"
	variablesHCL = "variables.hcl"
)

type loadErrorCase struct {
	name     string
	files    fstest.MapFS
	limits   lang.Limits
	expected string
}

// loadErrorCases are modules that Load refuses, and why.
func loadErrorCases() []loadErrorCase {
	return []loadErrorCase{
		{
			name:     "no metadata",
			files:    fstest.MapFS{variablesHCL: {Data: []byte("")}},
			expected: "Missing metadata.hcl",
		},
		{
			name:     "no module block",
			files:    fstest.MapFS{metadataHCL: {Data: []byte("ui {}\n")}},
			expected: "Missing module block",
		},
		{
			name:     "invalid version",
			files:    fstest.MapFS{metadataHCL: {Data: []byte("module {\n  name = \"app\"\n  version = \"1\"\n}\n")}},
			expected: "Invalid version",
		},
		{
			name:     "unknown file",
			files:    fstest.MapFS{metadataHCL: {Data: []byte(metadata)}, "job.nomad.tpl": {Data: []byte("")}},
			expected: "job.nomad.tpl is not a file a module may have",
		},
		{
			name:     "unknown directory",
			files:    fstest.MapFS{metadataHCL: {Data: []byte(metadata)}, "templates/x.hcl": {Data: []byte("")}},
			expected: "templates is not a directory a module may have",
		},
		{
			name: "label and file name differ",
			files: fstest.MapFS{
				metadataHCL:     {Data: []byte(metadata)},
				"web.norca.hcl": {Data: []byte("job \"api\" {}\n")},
			},
			expected: `labelled "api"; the file says "web"`,
		},
		{
			name: "two resources in a file",
			files: fstest.MapFS{
				metadataHCL: {Data: []byte(metadata)},
				"x.acl.hcl": {Data: []byte("policy \"x\" {}\npolicy \"x\" {}\n")},
			},
			expected: "One resource per file",
		},
		{
			name: "a block in the wrong file",
			files: fstest.MapFS{
				metadataHCL:  {Data: []byte(metadata)},
				"locals.hcl": {Data: []byte("job \"x\" {}\n")},
			},
			expected: `Blocks of type "job" are not expected here`,
		},
		{
			name: "validation does not fit the type",
			files: fstest.MapFS{
				metadataHCL:  {Data: []byte(metadata)},
				variablesHCL: {Data: []byte("variable \"n\" {\n  type = number\n  validation {\n    pattern = \"x\"\n  }\n}\n")},
			},
			expected: "pattern, min_length and max_length applies to a string, not to number",
		},
		{
			name: "unknown attribute of the ui",
			files: fstest.MapFS{
				metadataHCL:  {Data: []byte(metadata)},
				variablesHCL: {Data: []byte("variable \"n\" {\n  ui {\n    options = []\n  }\n}\n")},
			},
			expected: `An argument named "options" is not expected here`,
		},
		{
			name: "duplicate variable",
			files: fstest.MapFS{
				metadataHCL:  {Data: []byte(metadata)},
				variablesHCL: {Data: []byte("variable \"n\" {}\nvariable \"n\" {}\n")},
			},
			expected: "Duplicate variable",
		},
		{
			name: "default of the wrong type",
			files: fstest.MapFS{
				metadataHCL:  {Data: []byte(metadata)},
				variablesHCL: {Data: []byte("variable \"n\" {\n  type = number\n  default = \"x\"\n}\n")},
			},
			expected: "The default is not a number",
		},
		{
			name: "a file too large",
			files: fstest.MapFS{
				metadataHCL: {Data: []byte(metadata)},
				"files/big": {Data: make([]byte, 100)},
			},
			limits:   lang.Limits{ModuleBytes: 1000, FileBytes: 99},
			expected: "files/big has 100 bytes, more than the 99 a file may have",
		},
		{
			name: "a module too large",
			files: fstest.MapFS{
				metadataHCL: {Data: []byte(metadata)},
				"files/big": {Data: make([]byte, 100)},
			},
			limits:   lang.Limits{ModuleBytes: 100, FileBytes: 1000},
			expected: "more than the 100 bytes a module may have",
		},
	}
}

func TestLoad_Errors(t *testing.T) {
	t.Parallel()

	for _, tt := range loadErrorCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := limits
			if tt.limits != (lang.Limits{}) {
				l = tt.limits
			}

			_, diags := lang.Load(t.Context(), tt.files, l)
			if !diags.HasErrors() {
				t.Fatal("Load() gave no error")
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

func TestLoad_Variables(t *testing.T) {
	t.Parallel()

	m, diags := lang.Load(t.Context(), os.DirFS("../../examples/registry/modules/redis"), limits)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	v, ok := m.Variable("redis_password")
	if !ok || !v.Sensitive || v.Default.AsString() != "" {
		t.Errorf("redis_password = %+v, want a sensitive string with an empty default", v)
	}

	v, ok = m.Variable("mode")
	if !ok || len(v.Validation.OneOf) != 3 {
		t.Errorf("mode = %+v, want three options in one_of", v)
	}

	if _, ok := m.Variable("namespace"); ok {
		t.Error("namespace is a variable; it is the release's")
	}

	if len(m.Tabs) != 5 || m.Tabs[0].Label != "General" {
		t.Errorf("tabs = %+v", m.Tabs)
	}
}
