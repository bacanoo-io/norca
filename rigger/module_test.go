// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/resource"
)

func engine(t *testing.T) *rigger.Engine {
	t.Helper()

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

func TestEngine_LoadModule(t *testing.T) {
	t.Parallel()

	m, diags := engine(t).LoadModule(t.Context(), os.DirFS("examples/registry/modules/redis"))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if md := m.Metadata(); md.Name != "redis" || md.Version != "0.4.2" || md.License != "BSD-3-Clause" {
		t.Errorf("metadata = %+v", md)
	}

	vars := m.Variables()
	if len(vars) != 15 || vars[0].Name != "job_name" || vars[0].Required {
		t.Errorf("variables = %+v", vars)
	}

	for _, v := range vars {
		if v.Name == "redis_password" && !v.Sensitive {
			t.Error("redis_password is not sensitive")
		}
	}
}

// summaryModule is a module with variables of each kind of default, outputs
// and a README, as rigger show describes one.
var summaryModule = fstest.MapFS{
	"metadata.hcl": {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
	"variables.hcl": {Data: []byte(`variable "replicas" {
  type        = number
  default     = 2
  description = "How many run"
}

variable "name" {
  type = string
}

variable "password" {
  type      = string
  default   = "s3cret"
  sensitive = true
}

variable "tags" {
  type    = list(string)
  default = ["a", "b"]
}

variable "extra" {
  default = null
}
`)},
	"outputs.hcl": {Data: []byte(`output "url" {
  value       = "http://${var.name}"
  description = "Where it answers"
}

output "token" {
  value     = var.password
  sensitive = true
}
`)},
	"README.md": {Data: []byte("# web\n")},
}

// The variables of a module say their default as JSON text, but that of a
// sensitive one, which is never shown.
func TestModule_Variables(t *testing.T) {
	t.Parallel()

	m, diags := engine(t).LoadModule(t.Context(), summaryModule)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var got []string
	for _, v := range m.Variables() {
		got = append(got, fmt.Sprintf("%s %s %q required=%t sensitive=%t", v.Name, v.Type, v.Default, v.Required, v.Sensitive))
	}

	want := []string{
		`replicas number "2" required=false sensitive=false`,
		`name string "" required=true sensitive=false`,
		`password string "" required=false sensitive=true`,
		`tags list(string) "[\"a\",\"b\"]" required=false sensitive=false`,
		`extra any "null" required=false sensitive=false`,
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Variables() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The outputs of a module in the order of outputs.hcl, with what they say
// of themselves; nothing is evaluated.
func TestModule_Outputs(t *testing.T) {
	t.Parallel()

	m, diags := engine(t).LoadModule(t.Context(), summaryModule)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	got := fmt.Sprintf("%+v", m.Outputs())
	if want := "[{Name:url Description:Where it answers Sensitive:false} {Name:token Description: Sensitive:true}]"; got != want {
		t.Errorf("Outputs() = %s, want %s", got, want)
	}
}

func TestModule_Readme(t *testing.T) {
	t.Parallel()

	m, diags := engine(t).LoadModule(t.Context(), summaryModule)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if readme, err := m.Readme(); err != nil || string(readme) != "# web\n" {
		t.Errorf("Readme() = %q, %v", readme, err)
	}

	bare, diags := engine(t).LoadModule(t.Context(), fstest.MapFS{"metadata.hcl": summaryModule["metadata.hcl"]})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if _, err := bare.Readme(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Readme() of a module without one = %v, want fs.ErrNotExist", err)
	}
}

// Every module of examples/ validates: its icon and its example values.
func TestEngine_ValidateModule_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{
		"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres",
		"registry/modules/lib/placement",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			e := engine(t)

			m, diags := e.LoadModuleAt(t.Context(), os.DirFS("examples"), dir)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if diags := e.ValidateModule(t.Context(), m); diags.HasErrors() {
				t.Error(diags)
			}
		})
	}
}

// A task of Norca has its environment as NORCA_*: a module that reads a
// NOMAD_* variable gets an empty value. The redis module 0.4.0 wrote the
// port of its server from NOMAD_PORT_db, and Redis refused the configuration.
func TestModules_ReadNoNomadEnvironment(t *testing.T) {
	t.Parallel()

	nomadEnv := regexp.MustCompile(`\bNOMAD_[A-Z]`)

	for _, root := range []string{"builtin/modules", "examples"} {
		fsys := os.DirFS(root)

		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || path.Ext(p) != ".hcl" {
				return err
			}

			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}

			for i, line := range strings.Split(string(data), "\n") {
				if nomadEnv.MatchString(line) {
					t.Errorf("%s/%s:%d names a NOMAD_ variable, which no task has (NORCA_ is the prefix): %s", root, p, i+1, strings.TrimSpace(line))
				}
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A module whose defaults do not evaluate is not valid, examples or not.
func TestEngine_ValidateModule_Defaults(t *testing.T) {
	t.Parallel()

	const metadata = "module {\n  name = \"app\"\n  version = \"1.0.0\"\n}\n"

	tests := []struct {
		name, variables, expected string
	}{
		{name: "defaults that evaluate", variables: "variable \"n\" {\n  type    = string\n  default = \"1\"\n}\n"},
		{name: "defaults that do not", variables: "variable \"n\" {\n  type    = string\n  default = \"x\"\n}\n", expected: "Invalid function argument"},
		{name: "a required variable", variables: "variable \"n\" {\n  type = string\n}\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := engine(t)

			m, diags := e.LoadModule(t.Context(), fstest.MapFS{
				metadataFile:  {Data: []byte(metadata)},
				variablesFile: {Data: []byte(tt.variables)},
				"outputs.hcl": {Data: []byte("output \"n\" {\n  value = tonumber(var.n)\n}\n")},
			})
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			diags = e.ValidateModule(t.Context(), m)

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Error(diags)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

// The example stack of 2026-11-27 had a resource of the kind audit, which
// the loader takes and the release API of Norca does not: the stack
// validated and failed its apply. An engine given the kinds of its host
// refuses such a resource where it validates and evaluates, once, even
// when it is not enabled; an engine without them takes every kind.
func TestEngine_ValidateModule_Kinds(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		metadataFile:         {Data: []byte("module {\n  name = \"shop\"\n  version = \"1.0.0\"\n}\n")},
		"shop.audit.hcl":     {Data: []byte("audit \"shop\" {\n  enabled = false\n  keep    = \"90d\"\n}\n")},
		"shop.namespace.hcl": {Data: []byte("namespace \"shop\" {\n  description = \"the shop\"\n}\n")},
	}

	tests := []struct {
		name     string
		kinds    []resource.Kind
		expected string
	}{
		{name: "kinds of the host", kinds: resource.Builtin(), expected: "The resource shop is of the kind audit, which this host cannot apply"},
		{name: "every kind", kinds: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []rigger.Option{rigger.WithLogger(slog.New(slog.DiscardHandler))}
			if tt.kinds != nil {
				opts = append(opts, rigger.WithKinds(tt.kinds...))
			}

			e, err := rigger.New(opts...)
			if err != nil {
				t.Fatal(err)
			}

			m, diags := e.LoadModule(t.Context(), files)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			validated := e.ValidateModule(t.Context(), m)
			_, evaluated := e.Evaluate(t.Context(), m, rigger.Install{Release: "shop", Namespace: "default", Region: "global"})

			for what, diags := range map[string]hcl.Diagnostics{"ValidateModule": validated, "Evaluate": evaluated} {
				if tt.expected == "" {
					if diags.HasErrors() {
						t.Errorf("%s() = %v, want no error", what, diags)
					}

					continue
				}

				if len(diags) != 1 || !strings.Contains(diags[0].Detail, tt.expected) {
					t.Errorf("%s() = %v, want one error that contains %q", what, diags, tt.expected)
				}
			}
		})
	}
}

func icon(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestEngine_ValidateModule_Icon(t *testing.T) {
	t.Parallel()

	const metadata = "module {\n  name = \"app\"\n  version = \"1.0.0\"\n}\n"

	tests := []struct {
		name     string
		icon     []byte
		expected string
	}{
		{name: "64 by 64", icon: icon(t, 64, 64)},
		{name: "another size", icon: icon(t, 128, 64), expected: "is 128x64 pixels, not 64x64"},
		{name: "not a PNG", icon: []byte("<svg/>"), expected: "is not a PNG"},
		{name: "too large", icon: make([]byte, 33<<10), expected: "more than 32768"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := engine(t)

			m, diags := e.LoadModule(t.Context(), fstest.MapFS{
				metadataFile: {Data: []byte(metadata)},
				"icon.png":   {Data: tt.icon},
			})
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			diags = e.ValidateModule(t.Context(), m)

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Error(diags)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
