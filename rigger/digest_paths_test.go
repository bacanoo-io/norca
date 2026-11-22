// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// shortName is the other name Windows may give the hidden file .payload.
const shortName = "PAYLOA~1"

// aliasFS opens a hidden file by a second name that its directory does not
// list, as a file system with short names does.
type aliasFS struct{ files fstest.MapFS }

func (a aliasFS) Open(name string) (fs.File, error) {
	return a.files.Open(strings.ReplaceAll(name, shortName, ".payload"))
}

func pathsEngine(t *testing.T) *rigger.Engine {
	t.Helper()

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

func pathsMetadata(name string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("module {\n  name    = \"" + name + "\"\n  version = \"0.1.0\"\n}\n")}
}

// What the digest leaves out is not content of a module: no function reads
// it, under no name.
func TestEvaluate_HiddenFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		expr  string
		alias bool
	}{
		{name: "file", expr: `file("files/.payload")`},
		{name: "directory", expr: `file("files/.d/data")`},
		{name: "top", expr: `file(".payload")`},
		{name: "signatures", expr: `file(".rigger-signatures/one.json")`},
		{name: "template", expr: `templatefile("files/.payload", {})`},
		{name: "second name", expr: `file("files/` + shortName + `")`, alias: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := fstest.MapFS{
				metadataFile:                  pathsMetadata("m"),
				"locals.hcl":                  {Data: []byte("locals {\n  x = " + tt.expr + "\n}\n")},
				".payload":                    {Data: []byte("unchecked")},
				".rigger-signatures/one.json": {Data: []byte("{}")},
				"files/.payload":              {Data: []byte("unchecked")},
				"files/.d/data":               {Data: []byte("unchecked")},
			}

			var fsys fs.FS = files
			if tt.alias {
				fsys = aliasFS{files}
			}

			e := pathsEngine(t)

			m, diags := e.LoadModule(t.Context(), fsys)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			_, diags = e.Evaluate(t.Context(), m, rigger.Install{Release: "m", Namespace: testNamespace, Region: "eu"})
			if !strings.Contains(diags.Error(), "not in the digest") {
				t.Errorf("diagnostics = %q, want the path refused", diags.Error())
			}
		})
	}
}

// A hidden directory is no module, for a child and for a release.
func TestLoad_HiddenSources(t *testing.T) {
	t.Parallel()

	e := pathsEngine(t)

	tests := []struct {
		name  string
		src   string
		alias bool
	}{
		{name: "hidden", src: "./modules/.payload"},
		{name: "second name", src: "./modules/" + shortName, alias: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			block := "module \"a\" {\n  source = \"" + tt.src + "\"\n}\n"
			files := stackFiles("rigger {\n  name = \"p\"\n}\n\n" + block)
			files["s/modules/.payload/"+metadataFile] = pathsMetadata("a")
			files["m/"+metadataFile] = pathsMetadata("m")
			files["m/modules.hcl"] = &fstest.MapFile{Data: []byte(strings.Replace(block, "./modules/", "../s/modules/", 1))}

			var fsys fs.FS = files
			if tt.alias {
				fsys = aliasFS{files}
			}

			if _, diags := e.LoadStack(t.Context(), fsys, "s"); !strings.Contains(diags.Error(), "not in the digest") {
				t.Errorf("a stack: %q, want the source refused", diags.Error())
			}

			if _, diags := e.LoadModuleAt(t.Context(), fsys, "m"); !strings.Contains(diags.Error(), "not in the digest") {
				t.Errorf("a module: %q, want the source refused", diags.Error())
			}
		})
	}
}

// The icon is content of the module as its files are.
func TestValidateModule_HiddenIcon(t *testing.T) {
	t.Parallel()

	e := pathsEngine(t)

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{
		metadataFile: {Data: []byte("module {\n  name    = \"m\"\n  version = \"0.1.0\"\n  icon    = \".icon.png\"\n}\n")},
		".icon.png":  {Data: []byte("x")},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if diags := e.ValidateModule(t.Context(), m); !strings.Contains(diags.Error(), "not in the digest") {
		t.Errorf("diagnostics = %q, want the icon refused", diags.Error())
	}
}

// A line of the digest is a hash and a name: a name with a line break in it
// could stand for two files.
func TestLoad_ControlCharacters(t *testing.T) {
	t.Parallel()

	const want = "control character"

	e := pathsEngine(t)

	for _, name := range []string{"files/a\nb", "files/a\rb", "files/a\x7fb", "files/d\tir/a"} {
		files := fstest.MapFS{metadataFile: pathsMetadata("m"), name: {Data: []byte("A")}}

		if _, diags := e.LoadModule(t.Context(), files); !strings.Contains(diags.Error(), want) {
			t.Errorf("module with %q: %q", name, diags.Error())
		}
	}

	const stack = "rigger {\n  name = \"p\"\n}\n"

	files := stackFiles(stack)
	files["s/a\nb"] = &fstest.MapFile{Data: []byte("A")}

	if _, diags := e.LoadStack(t.Context(), files, "s"); !strings.Contains(diags.Error(), want) {
		t.Errorf("stack: %q", diags.Error())
	}

	// A directory of modules that no release uses is in the digest too.
	files = stackFiles(stack)
	files["s/modules/x/a\nb"] = &fstest.MapFile{Data: []byte("A")}

	s, diags := e.LoadStack(t.Context(), files, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if _, err := s.Digest(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("digest of a stack = %v", err)
	}
}
