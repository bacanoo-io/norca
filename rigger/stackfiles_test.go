// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/internal/targz/targztest"
)

// archiveNames lists the entries of an archive of a stack.
func archiveNames(t *testing.T, archive []byte) []string {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(gz)

	var names []string

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}

		if err != nil {
			t.Fatal(err)
		}

		names = append(names, h.Name)
	}
}

// A local stack packed with Archive loads again from its files, with the
// same digest; its local state and hidden directories are not sent.
func TestStack_ArchiveAndLoadStackFiles(t *testing.T) {
	t.Parallel()

	// The stack of the stack tests, with its module app moved outside the
	// directory of the stack (shared/app, read from the same root).
	root := stackFiles(strings.Replace(twoReleases, `"./modules/app"`, `"../shared/app"`, 1))
	for name, f := range root {
		if rest, ok := strings.CutPrefix(name, "s/modules/app/"); ok {
			root["shared/app/"+rest] = f
			delete(root, name)
		}
	}

	root["s/.rigger/generated.hcl"] = &fstest.MapFile{Data: []byte(`secret = "never sent"`)}
	root["s/.git/config"] = &fstest.MapFile{Data: []byte("[core]")}
	root["unrelated/notes.txt"] = &fstest.MapFile{Data: []byte("not of the stack")}

	e := engine(t)

	s, diags := e.LoadStack(t.Context(), root, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	archive, err := s.Archive()
	if err != nil {
		t.Fatal(err)
	}

	checkSent(t, archiveNames(t, archive))

	loaded, diags := e.LoadStackFiles(t.Context(), archive)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	want, err := s.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := loaded.Digest()
	if err != nil {
		t.Fatal(err)
	}

	if got != want || loaded.Name() != s.Name() {
		t.Errorf("loaded %s digest %s, want %s digest %s", loaded.Name(), got, s.Name(), want)
	}
}

// A stack whose modules are partly of a source (here built in) packs only
// its own files and its local modules: the server fetches the others.
func TestStack_ArchiveWithSourcedModule(t *testing.T) {
	t.Parallel()

	root := fstest.MapFS{
		stackPath: {Data: []byte(`rigger {
  name      = "shop"
  region    = "eu"
  namespace = "apps"
}

module "cache" {
  source = "builtin/redis"
}

module "app" {
  source = "./modules/app"
}
`)},
		"s/modules/app/metadata.hcl": {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
	}

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(fetch.New(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}

	_, lock, diags := e.SyncStack(t.Context(), root, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	root["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	s, diags := e.LoadStack(t.Context(), root, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	archive, err := s.Archive()
	if err != nil {
		t.Fatal(err)
	}

	names := archiveNames(t, archive)
	if !containsAll(names, stackPath, "s/rigger.lock.hcl", "s/modules/app/metadata.hcl") || len(names) != 4 {
		t.Errorf("entries %v", names)
	}

	loaded, diags := e.LoadStackFiles(t.Context(), archive)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(loaded.SourcedModules()) != len(s.SourcedModules()) {
		t.Errorf("loaded %d modules of sources, want %d", len(loaded.SourcedModules()), len(s.SourcedModules()))
	}
}

// manifestName is the name of the manifest of an archive.
const manifestName = ".rigger-stack.json"

// Hidden files are no part of a stack or a module (their digest leaves them
// out), and a working directory keeps secrets in them (.env, .netrc): an
// archive holds none, in a hidden directory or not. A file named as the
// manifest of the archive is hidden too, so it can't stand in for it.
func TestArchive_HiddenFiles(t *testing.T) {
	t.Parallel()

	// The stack of the stack tests in the root itself, where a file can have
	// the name of the manifest; its module db is the module of the test.
	root := fstest.MapFS{}
	for name, f := range stackFiles(twoReleases) {
		root[strings.TrimPrefix(name, "s/")] = f
	}

	// Every hidden file says to be a manifest of another directory.
	for _, name := range []string{
		".env", ".netrc", manifestName, ".rigger/generated.hcl", "values/.env",
		"modules/db/.env", "modules/db/" + manifestName, "modules/db/files/.token", "modules/db/.git/config",
	} {
		root[name] = &fstest.MapFile{Data: []byte(`{"dir":"elsewhere"}`)}
	}

	db, err := fs.Sub(root, "modules/db")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		repack   func(*testing.T, fs.FS) ([]byte, string, hcl.Diagnostics)
		root     fs.FS
		expected string
	}{
		{name: "a stack", repack: repackStack, root: root, expected: "shop"},
		{name: "a module", repack: repackModule, root: db, expected: "db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			archive, loaded, diags := tt.repack(t, tt.root)

			names := archiveNames(t, archive)
			if len(names) < 2 || names[0] != manifestName {
				t.Fatalf("entries %v, want the manifest first and the files", names)
			}

			for _, n := range names[1:] {
				if strings.HasPrefix(n, ".") || strings.Contains(n, "/.") {
					t.Errorf("%s was sent", n)
				}
			}

			if diags.HasErrors() || loaded != tt.expected {
				t.Errorf("the archive loads as %q, want %q: %v", loaded, tt.expected, diags)
			}
		})
	}
}

// repackStack loads the stack in root, packs it and loads the archive: it
// returns the archive and the name of the stack the archive loads as.
func repackStack(t *testing.T, root fs.FS) ([]byte, string, hcl.Diagnostics) {
	t.Helper()

	e := engine(t)

	s, diags := e.LoadStack(t.Context(), root, ".")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	archive, err := s.Archive()
	if err != nil {
		t.Fatal(err)
	}

	loaded, diags := e.LoadStackFiles(t.Context(), archive)
	if diags.HasErrors() {
		return archive, "", diags
	}

	return archive, loaded.Name(), nil
}

// repackModule is repackStack for the module in root.
func repackModule(t *testing.T, root fs.FS) ([]byte, string, hcl.Diagnostics) {
	t.Helper()

	e := engine(t)

	m, diags := e.LoadModule(t.Context(), root)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	archive, err := m.Archive()
	if err != nil {
		t.Fatal(err)
	}

	loaded, diags := e.LoadModuleFiles(t.Context(), archive)
	if diags.HasErrors() {
		return archive, "", diags
	}

	return archive, loaded.Metadata().Name, nil
}

// An archive that is not one of a stack is refused: without a manifest, with
// an entry outside the root, with a file or the manifest twice.
func TestLoadStackFiles_Invalid(t *testing.T) {
	t.Parallel()

	e := engine(t)

	manifest := [2]string{manifestName, `{"dir":"s"}`}
	stack := [2]string{stackPath, "rigger {\n  name = \"s\"\n}\n"}

	for name, archive := range map[string][]byte{
		"not gzip":      []byte("plain"),
		"no manifest":   tarGz(t, stack),
		"outside root":  tarGz(t, manifest, [2]string{"../x", "y"}),
		"a file twice":  tarGz(t, manifest, stack, stack),
		"two manifests": tarGz(t, manifest, stack, manifest),
	} {
		if _, diags := e.LoadStackFiles(t.Context(), archive); !diags.HasErrors() {
			t.Errorf("%s: accepted", name)
		}
	}

	// The archive the cases above differ from.
	if _, diags := e.LoadStackFiles(t.Context(), tarGz(t, manifest, stack)); diags.HasErrors() {
		t.Errorf("a manifest and a stack: %v", diags)
	}
}

// tarGz is an archive of the entries, each a name and its content, in order.
func tarGz(t *testing.T, entries ...[2]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		name, data := e[0], e[1]

		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func containsAll(list []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(list, w) {
			return false
		}
	}

	return true
}

// checkSent fails the test unless the archive holds the files of the stack
// and of its local modules, and nothing of its local state, of hidden
// directories or outside the stack.
func checkSent(t *testing.T, names []string) {
	t.Helper()

	for _, n := range names {
		if strings.Contains(n, "/.rigger/") || strings.Contains(n, "/.git/") || strings.HasPrefix(n, "unrelated/") {
			t.Errorf("%s was sent", n)
		}
	}

	if !containsAll(names, manifestName, stackPath, "s/modules/db/metadata.hcl", "shared/app/metadata.hcl") {
		t.Errorf("entries %v", names)
	}
}

// A local module packed with Archive loads again from its files, with the
// same digest and its child module beside it; nothing else of the root is
// sent.
func TestModule_ArchiveAndLoadModuleFiles(t *testing.T) {
	t.Parallel()

	root := fstest.MapFS{
		"mods/web/metadata.hcl":  {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
		"mods/web/modules.hcl":   {Data: []byte("module \"lib\" {\n  source = \"../lib\"\n}\n")},
		"mods/web/.hidden/x.txt": {Data: []byte("hidden")},
		"mods/lib/metadata.hcl":  {Data: []byte("module {\n  name    = \"lib\"\n  version = \"0.1.0\"\n}\n")},
		"mods/other/notes.txt":   {Data: []byte("not of the module")},
	}

	e := engine(t)

	m, diags := e.LoadModuleAt(t.Context(), root, "mods/web")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	archive, err := m.Archive()
	if err != nil {
		t.Fatal(err)
	}

	names := archiveNames(t, archive)
	if !containsAll(names, manifestName, "mods/web/metadata.hcl", "mods/web/modules.hcl", "mods/lib/metadata.hcl") ||
		slices.Contains(names, "mods/other/notes.txt") || slices.Contains(names, "mods/web/.hidden/x.txt") {
		t.Errorf("entries %v", names)
	}

	loaded, diags := e.LoadModuleFiles(t.Context(), archive)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	want, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}

	if got, err := loaded.Digest(); err != nil || got != want || loaded.Metadata().Name != "web" {
		t.Errorf("loaded %s digest %s (%v), want web digest %s", loaded.Metadata().Name, got, err, want)
	}

	if _, diags := e.LoadModuleFiles(t.Context(), []byte("not an archive")); !diags.HasErrors() {
		t.Error("an archive that is not one was loaded")
	}
}

// A sparse file is a few bytes in an archive and any size when it is read:
// the bound of the archive, which counts its bytes, does not see it.
func TestLoadStackFiles_Sparse(t *testing.T) {
	t.Parallel()

	archive, err := targztest.SparseTarGz("s/files/big", 8<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, diags := engine(t).LoadStackFiles(t.Context(), archive); !strings.Contains(diags.Error(), "sparse") {
		t.Errorf("diagnostics = %q, want the sparse file refused", diags.Error())
	}

	if _, diags := engine(t).LoadModuleFiles(t.Context(), archive); !strings.Contains(diags.Error(), "sparse") {
		t.Errorf("diagnostics = %q, want the sparse file refused", diags.Error())
	}
}
