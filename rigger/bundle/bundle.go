// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package bundle writes and reads the bundles of the rigger program (design
// 7.1, 10): one reproducible archive with the modules of a stack from git
// sources, as its lock file pins them, for an install without a network.
// An import writes them into the cache, where the fetcher finds them by the
// commit the lock records.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// manifestName is the name of the manifest in a bundle.
const manifestName = "rigger-bundle.json"

// format is the version of the layout of a bundle.
const format = 1

// Manifest says what a bundle holds.
type Manifest struct {
	Format  int                    `json:"format"`
	Stack   string                 `json:"stack"`
	Modules []rigger.SourcedModule `json:"modules"`
}

// ErrBundle is the error of an archive that is not a bundle.
var ErrBundle = errors.New("not a bundle of rigger")

// Write writes a bundle of the modules of a stack: the manifest, then the
// files of each module's directories under trees/<commit>/, in order, with
// no time, owner or mode of the machine, so the same modules make the same
// bundle.
func Write(w io.Writer, stack string, modules []rigger.SourcedModule) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	manifest, err := json.MarshalIndent(Manifest{Format: format, Stack: stack, Modules: modules}, "", "  ")
	if err != nil {
		return err
	}

	if err := writeFile(tw, manifestName, manifest); err != nil {
		return err
	}

	for _, m := range modules {
		for _, dir := range m.Dirs {
			if err := writeDir(tw, m, dir); err != nil {
				return err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}

	return gz.Close()
}

// writeDir writes the files of a directory of a module, signatures
// included, sorted.
func writeDir(tw *tar.Writer, m rigger.SourcedModule, dir string) error {
	var files []string

	err := fs.WalkDir(m.Root, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			files = append(files, p)
		}

		return nil
	})
	if err != nil {
		return err
	}

	slices.Sort(files)

	for _, p := range files {
		data, err := fs.ReadFile(m.Root, p)
		if err != nil {
			return err
		}

		if err := writeFile(tw, "trees/"+m.Commit+"/"+p, data); err != nil {
			return err
		}
	}

	return nil
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}

	_, err := tw.Write(data)

	return err
}

// Read reads a bundle: its manifest, and the files of each commit by path.
func Read(r io.Reader) (*Manifest, map[string]map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	tr := tar.NewReader(gz)

	var manifest *Manifest

	files := map[string]map[string][]byte{}

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrBundle, err)
		}

		m, err := readEntry(tr, h, files)
		if err != nil {
			return nil, nil, err
		}

		if m != nil {
			manifest = m
		}
	}

	if manifest == nil || manifest.Format != format {
		return nil, nil, fmt.Errorf("%w: no manifest of format %d", ErrBundle, format)
	}

	return manifest, files, nil
}

// readEntry reads an entry of a bundle: the manifest, which it returns, or
// a file of a commit, which it adds to files.
func readEntry(tr *tar.Reader, h *tar.Header, files map[string]map[string][]byte) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
	if err != nil {
		return nil, err
	}

	if h.Name == manifestName {
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBundle, err)
		}

		return &m, nil
	}

	commit, p, ok := strings.Cut(strings.TrimPrefix(h.Name, "trees/"), "/")
	if !ok || !strings.HasPrefix(h.Name, "trees/") || !fs.ValidPath(p) {
		return nil, fmt.Errorf("%w: entry %q", ErrBundle, h.Name)
	}

	if files[commit] == nil {
		files[commit] = map[string][]byte{}
	}

	files[commit][p] = data

	return nil, nil
}

// WriteTree writes the files of a commit into dir, through an os.Root.
func WriteTree(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the files are closed one by one

	for p, data := range files {
		if err := root.MkdirAll(path.Dir(p), 0o750); err != nil {
			return err
		}

		if err := root.WriteFile(p, data, 0o640); err != nil {
			return err
		}
	}

	return nil
}
