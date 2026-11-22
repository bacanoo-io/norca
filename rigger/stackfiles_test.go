// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
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

// An archive that is not one of a stack is refused.
func TestLoadStackFiles_Invalid(t *testing.T) {
	t.Parallel()

	e := engine(t)

	for name, archive := range map[string][]byte{
		"not gzip":     []byte("plain"),
		"no manifest":  tarGz(t, map[string]string{"s/stack.hcl": "rigger {\n  name = \"s\"\n}\n"}),
		"outside root": tarGz(t, map[string]string{".rigger-stack.json": `{"dir":"s"}`, "../x": "y"}),
	} {
		if _, diags := e.LoadStackFiles(t.Context(), archive); !diags.HasErrors() {
			t.Errorf("%s: accepted", name)
		}
	}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, data := range files {
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

	if !containsAll(names, ".rigger-stack.json", "s/stack.hcl", "s/modules/db/metadata.hcl", "shared/app/metadata.hcl") {
		t.Errorf("entries %v", names)
	}
}
