// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// Digest returns the tree digest of m (design 8.1): sha256 over one line
// per file, "<sha256 of the content>  <path>\n", sorted by path. The files
// are those of the module and of its local child modules, named from the
// directory of the module as in diagnostics (../lib/placement/outputs.hcl),
// so that the digest changes when a child changes. Hidden files and
// directories are left out. The result is "sha256:<hex>".
func Digest(m *Module) (string, error) {
	lines := map[string]string{}
	if err := digestFiles(m, "", lines); err != nil {
		return "", err
	}

	names := make([]string, 0, len(lines))
	for name := range lines {
		names = append(names, name)
	}

	slices.Sort(names)

	h := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(h, lines[name]+"  "+name+"\n") // a hash takes every write
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// digestFiles adds the digests of the files of m and of its children, named
// with prefix.
func digestFiles(m *Module, prefix string, lines map[string]string) error {
	if m.FS == nil {
		return fmt.Errorf("the module %s has no files", m.Metadata.Name)
	}

	err := fs.WalkDir(m.FS, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p != "." && strings.HasPrefix(d.Name(), "."):
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		case d.IsDir():
			return nil
		}

		f, err := m.FS.Open(p)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read only

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}

		lines[path.Join(prefix, p)] = hex.EncodeToString(h.Sum(nil))

		return nil
	})
	if err != nil {
		return err
	}

	for _, c := range m.Children {
		if err := digestFiles(c.Module, path.Join(prefix, c.Source), lines); err != nil {
			return err
		}
	}

	return nil
}
