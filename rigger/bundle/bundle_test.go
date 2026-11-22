// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bundle_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

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
		"rigger-bundle.json":     manifest,
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
