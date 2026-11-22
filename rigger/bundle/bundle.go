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
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/internal/readlimit"
	"github.com/bacanoo-io/norca/rigger/internal/targz"
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

	return targz.Close(tw, gz, writeBundle(tw, stack, modules))
}

// writeBundle writes the entries of a bundle into tw.
func writeBundle(tw *tar.Writer, stack string, modules []rigger.SourcedModule) error {
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

	return nil
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

// DefaultMaxBytes bounds what Read unpacks of a bundle.
const DefaultMaxBytes = 4 << 30

// ErrTooLarge is the error of a bundle that unpacks to more than its bound.
var ErrTooLarge = errors.New("the bundle unpacks to more than its bound")

// Read reads a bundle: its manifest, and the files of each commit by path,
// unpacking at most DefaultMaxBytes.
func Read(r io.Reader) (*Manifest, map[string]map[string][]byte, error) {
	return ReadBounded(r, DefaultMaxBytes)
}

// ReadBounded reads a bundle as Read does, unpacking at most maxBytes: a
// host that reads bundles others send (a server) bounds what a small
// archive may unpack to.
func ReadBounded(r io.Reader, maxBytes int64) (*Manifest, map[string]map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	defer gz.Close() //nolint:errcheck // read only

	lr := &io.LimitedReader{R: gz, N: maxBytes + 1}
	tr := tar.NewReader(lr)

	var manifest *Manifest

	files := map[string]map[string][]byte{}

	for {
		h, err := tr.Next()

		if lr.N <= 0 {
			return nil, nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, maxBytes)
		}

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrBundle, err)
		}

		m, err := readEntry(tr, h, files)

		if lr.N <= 0 {
			return nil, nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, maxBytes)
		}

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

	for _, m := range manifest.Modules {
		if !commitPattern.MatchString(m.Commit) {
			return nil, nil, fmt.Errorf("%w: module %s %s: commit %q", ErrBundle, m.Source, m.Version, m.Commit)
		}
	}

	return manifest, files, nil
}

// commitPattern is a commit of git: SHA-1 or SHA-256 in hex. A commit names
// the directory its tree is unpacked into, so nothing else may pass.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// maxFileBytes bounds one file of a bundle.
const maxFileBytes = 64 << 20

// readEntry reads an entry of a bundle: the manifest, which it returns, or
// a file of a commit, which it adds to files.
func readEntry(tr *tar.Reader, h *tar.Header, files map[string]map[string][]byte) (*Manifest, error) {
	if err := targz.CheckEntry(h); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	data, err := readlimit.ReadAll(tr, maxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: entry %q: %w", ErrBundle, h.Name, err)
	}

	if h.Name == manifestName {
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBundle, err)
		}

		return &m, nil
	}

	commit, p, ok := strings.Cut(strings.TrimPrefix(h.Name, "trees/"), "/")
	if !ok || !strings.HasPrefix(h.Name, "trees/") || !commitPattern.MatchString(commit) || !fs.ValidPath(p) {
		return nil, fmt.Errorf("%w: entry %q", ErrBundle, h.Name)
	}

	if files[commit] == nil {
		files[commit] = map[string][]byte{}
	}

	files[commit][p] = data

	return nil, nil
}

// WriteTree writes the files of a commit as the directory dir, whole or not
// at all: into a directory beside it, through an os.Root, then renamed to
// dir. A reader never sees a part of a tree, and a failed write leaves
// nothing behind. A tree already at dir (another writer was first) is
// kept.
func WriteTree(dir string, files map[string][]byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := writeFiles(tmp, files); err != nil {
		return err
	}

	if err := os.Rename(tmp, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			return os.RemoveAll(tmp)
		}

		return err
	}

	return nil
}

// writeFiles writes files into dir through an os.Root, which it closes
// before it returns: a directory still open can't be renamed on Windows.
func writeFiles(dir string, files map[string][]byte) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, root.Close())
	}()

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
