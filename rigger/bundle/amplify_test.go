// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bundle_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

const treeDir = "mod"

// craftBundle is an archive written by hand: a manifest that may name one
// tree as often as it likes, and that tree once.
func craftBundle(t *testing.T, mods []rigger.SourcedModule, file []byte) []byte {
	t.Helper()

	manifest, err := json.Marshal(bundle.Manifest{Format: 1, Stack: "repeat", Modules: mods})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, data := range map[string][]byte{manifestFile: manifest, "trees/" + commit + "/" + treeDir + "/data": file} {
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

	return buf.Bytes()
}

// The bound of a bundle is of what it has written to a disk: entries of
// the manifest that name one tree again and again don't multiply it.
func TestReadBounded_RepeatedModules(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20

	tests := []struct {
		name     string
		modules  int
		size     int
		expected error
	}{
		{name: "one", modules: 1, size: mib},
		{name: "one tree for ten", modules: 10, size: mib, expected: bundle.ErrTooLarge},
		{name: "too many", modules: 1001, size: 1, expected: bundle.ErrBundle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mods := make([]rigger.SourcedModule, 0, tt.modules)
			for i := range tt.modules {
				mods = append(mods, rigger.SourcedModule{
					Repository: "example.com/r" + strconv.Itoa(i), Path: treeDir, Commit: commit, Dirs: []string{treeDir},
				})
			}

			data := craftBundle(t, mods, bytes.Repeat([]byte("x"), tt.size))

			if _, _, err := bundle.ReadBounded(bytes.NewReader(data), 4*mib); !errors.Is(err, tt.expected) {
				t.Errorf("ReadBounded() = %v, want %v", err, tt.expected)
			}
		})
	}
}
