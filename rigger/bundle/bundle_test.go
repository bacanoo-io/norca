// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bundle_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/internal/targz/targztest"
)

const (
	commit       = "0123456789abcdef0123456789abcdef01234567"
	manifestFile = "rigger-bundle.json"
)

// A bundle written of a module reads back with its manifest and files.
func TestWriteRead(t *testing.T) {
	t.Parallel()

	root := fstest.MapFS{
		"modules/web/metadata.hcl": {Data: []byte("module {}\n")},
		"modules/web/README.md":    {Data: []byte("# web\n")},
	}
	mod := rigger.SourcedModule{
		Source: "acme/web", Version: "1.0.0", Commit: commit, Path: "modules/web",
		Digest: "sha256:x", Dirs: []string{"modules/web"}, Root: root,
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, "shop", []rigger.SourcedModule{mod}); err != nil {
		t.Fatal(err)
	}

	m, files, err := bundle.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if m.Stack != "shop" || len(m.Modules) != 1 || string(files[commit]["modules/web/README.md"]) != "# web\n" {
		t.Errorf("read %+v and %v", m, files)
	}

	if _, _, err := bundle.ReadBounded(bytes.NewReader(buf.Bytes()), 1<<20); err != nil {
		t.Errorf("a bundle within its bound: %v", err)
	}
}

// A small archive that unpacks to more than the bound is refused, before
// all of it is held.
func TestReadBounded_TooLarge(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	manifest, err := json.Marshal(bundle.Manifest{Format: 1, Stack: "bomb"})
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		manifestFile:             manifest,
		"trees/" + commit + "/a": make([]byte, 2<<20),
		"trees/" + commit + "/b": make([]byte, 2<<20),
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	if buf.Len() > 64<<10 {
		t.Fatalf("the archive is %d bytes, not small", buf.Len())
	}

	if _, _, err := bundle.ReadBounded(bytes.NewReader(buf.Bytes()), 1<<20); !errors.Is(err, bundle.ErrTooLarge) {
		t.Errorf("ReadBounded() = %v, want ErrTooLarge", err)
	}

	if _, _, err := bundle.Read(bytes.NewReader([]byte("not a bundle"))); !errors.Is(err, bundle.ErrBundle) {
		t.Errorf("Read() of something else = %v, want ErrBundle", err)
	}
}

// A file of a bundle larger than a file may be is refused, not cut short.
func TestRead_FileTooLarge(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	big := make([]byte, 64<<20+1)

	if err := tw.WriteHeader(&tar.Header{Name: "trees/" + commit + "/big", Mode: 0o644, Size: int64(len(big)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}

	if _, err := tw.Write(big); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err := bundle.Read(bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, bundle.ErrBundle) || !strings.Contains(err.Error(), "more than its bound") {
		t.Errorf("Read() = %v, want the file refused", err)
	}
}

// WriteTree places a whole tree, keeps a tree already there, and leaves
// nothing behind when a file can't be written.
func TestWriteTree(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, commit)

	if err := bundle.WriteTree(dir, map[string][]byte{"modules/web/metadata.hcl": []byte("module {}\n")}); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "modules", "web", "metadata.hcl")); err != nil || string(got) != "module {}\n" {
		t.Fatalf("read %q, %v", got, err)
	}

	// Another writer of the same commit: the tree there is kept.
	if err := bundle.WriteTree(dir, map[string][]byte{"other.hcl": []byte("x")}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "other.hcl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the tree there was changed: %v", err)
	}

	// A path out of the tree fails, and no part of it stays.
	bad := filepath.Join(parent, "bad")
	if err := bundle.WriteTree(bad, map[string][]byte{"a.hcl": []byte("a"), "../escape.hcl": []byte("b")}); err == nil {
		t.Fatal("a path out of the tree was written")
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name() != commit {
		t.Errorf("left in the parent: %v", entries)
	}
}

// A commit names the directory its tree is unpacked into: one that is not
// a commit of git, in an entry or in the manifest, is refused.
func TestRead_Commit(t *testing.T) {
	t.Parallel()

	manifest := func(commit string) []byte {
		data, err := json.Marshal(bundle.Manifest{Format: 1, Stack: "s", Modules: []rigger.SourcedModule{{Source: "a/b", Version: "1.0.0", Commit: commit}}})
		if err != nil {
			t.Fatal(err)
		}

		return data
	}

	tests := []struct {
		name    string
		entries map[string][]byte
	}{
		{name: "entry out of the trees", entries: map[string][]byte{
			manifestFile: manifest(commit), "trees/../escape/metadata.hcl": []byte("x"),
		}},
		{name: "entry of no commit", entries: map[string][]byte{
			manifestFile: manifest(commit), "trees/main/metadata.hcl": []byte("x"),
		}},
		{name: "manifest commit out of the cache", entries: map[string][]byte{
			manifestFile: manifest("../../escape"),
		}},
		{name: "entry of a hash and a digit more", entries: map[string][]byte{
			manifestFile: manifest(commit), "trees/" + commit + "0/metadata.hcl": []byte("x"),
		}},
		{name: "manifest commit of a hash and some digits more", entries: map[string][]byte{
			manifestFile: manifest(commit + "abcdef"),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)

			for name, data := range tt.entries {
				if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}

				if _, err := tw.Write(data); err != nil {
					t.Fatal(err)
				}
			}

			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}

			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}

			if _, _, err := bundle.Read(bytes.NewReader(buf.Bytes())); !errors.Is(err, bundle.ErrBundle) {
				t.Errorf("Read() = %v, want ErrBundle", err)
			}
		})
	}
}

// A sparse file would unpack past the bound of ReadBounded, which counts
// the bytes of the archive.
func TestReadBounded_Sparse(t *testing.T) {
	t.Parallel()

	archive, err := targztest.SparseTarGz("trees/0123456789abcdef0123456789abcdef01234567/big", 8<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := bundle.ReadBounded(bytes.NewReader(archive), 1<<20); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Errorf("ReadBounded() = %v, want the sparse file refused", err)
	}
}
