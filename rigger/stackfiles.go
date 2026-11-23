// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/targz"
)

// A local stack sent as files (design/rig-integration.md 4.2 d of norca): a
// client packs the files a loaded stack reads, and a server loads the stack
// from them to plan and apply it itself (4.6). The archive is a gzip
// compressed tar of the files at their paths in the root the stack was
// loaded from, and a manifest that names the directory of the stack. A
// local module is sent the same way (Module.Archive, LoadModuleFiles).

// stackManifest is the name of the manifest in an archive of a stack.
const stackManifest = ".rigger-stack.json"

// The bounds of an archive of a stack: its size compressed and uncompressed,
// and its number of files.
const (
	maxStackArchiveBytes = 16 << 20
	maxStackFilesBytes   = 64 << 20
	maxStackFiles        = 4096
)

type stackArchiveManifest struct {
	// Dir is the directory of the stack in the archive.
	Dir string `json:"dir"`
}

// Archive packs the files of a local stack: every regular file of its
// directory, and of the directories of its local modules and their child
// modules. Files and directories whose name starts with a dot are left out.
// They are no part of a stack or a module (the digest leaves them out, and
// nothing of the language reads them), and a server never needs them:
// .rigger/ holds the local state of the stack, its generated secrets among
// them, and a working directory keeps secrets in files as .env and .netrc.
// A stack fetched from a source has no local files to pack: the server
// fetches it by its source.
func (s *Stack) Archive() ([]byte, error) {
	if s.origin != nil {
		return nil, errors.New("a published stack is fetched by its source, not sent as files")
	}

	dirs := []string{s.s.Dir}

	// The modules of a source (git, the global registry, built in) are not
	// files of the stack: the server fetches them as the lock file pins them.
	for _, r := range s.s.Releases {
		if r.Module != nil && r.Address == nil {
			dirs = append(dirs, moduleDirs(r.Module)...)
		}
	}

	return packFiles(s.s.Root, dirs, s.s.Dir)
}

// Archive packs the files of a module loaded from files, as Stack.Archive
// packs a stack: every regular file of its directory and of the directories
// of its child modules, with a manifest that names its directory. A server
// loads it with Engine.LoadModuleFiles (design/rig-integration.md 4.6 of
// norca: a local module sent to a server).
func (m *Module) Archive() ([]byte, error) {
	if m.origin != nil {
		return nil, errors.New("a fetched module is installed by its source, not sent as files")
	}

	root, dirs := m.Files()

	return packFiles(root, dirs, m.mod.Dir)
}

// packFiles packs the regular files under dirs of root, and a manifest that
// names dir, into a gzip compressed tar.
func packFiles(root fs.FS, dirs []string, dir string) ([]byte, error) {
	files, err := stackFiles(root, dirs)
	if err != nil {
		return nil, err
	}

	manifest, err := json.Marshal(stackArchiveManifest{Dir: dir})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := targz.Close(tw, gz, writeArchiveFiles(tw, manifest, files)); err != nil {
		return nil, err
	}

	if buf.Len() > maxStackArchiveBytes {
		return nil, fmt.Errorf("the files are %d bytes compressed, more than %d", buf.Len(), maxStackArchiveBytes)
	}

	return buf.Bytes(), nil
}

// writeArchiveFiles writes the manifest and the files, by name, into tw.
func writeArchiveFiles(tw *tar.Writer, manifest []byte, files map[string][]byte) error {
	if err := writeArchiveFile(tw, stackManifest, manifest); err != nil {
		return err
	}

	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := writeArchiveFile(tw, name, files[name]); err != nil {
			return err
		}
	}

	return nil
}

// stackFiles reads the regular files under dirs of root, by their paths,
// without the files and directories whose name starts with a dot. The
// manifest of an archive has such a name, so no file can have its name.
func stackFiles(root fs.FS, dirs []string) (map[string][]byte, error) {
	files := map[string][]byte{}

	var total int64

	for _, dir := range dirs {
		err := fs.WalkDir(root, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if p != dir && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return fs.SkipDir
				}

				return nil
			}

			if d.IsDir() || !d.Type().IsRegular() || files[p] != nil {
				return nil
			}

			data, err := fs.ReadFile(root, p)
			if err != nil {
				return err
			}

			total += int64(len(data))
			if total > maxStackFilesBytes || len(files) >= maxStackFiles {
				return fmt.Errorf("the stack has more than %d files or %d bytes", maxStackFiles, maxStackFilesBytes)
			}

			files[p] = data

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return files, nil
}

func writeArchiveFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}

	_, err := tw.Write(data)

	return err
}

// LoadStackFiles loads a stack from the files a client packed with
// Stack.Archive. The files stay in memory; an entry that is not a regular
// file, whose name is not a valid path, or that is there twice (a file or
// the manifest) is refused, and the archive is bounded in size and number
// of files. The stack is then loaded as from a directory (LoadStack), so it
// reads nothing but these files.
func (e *Engine) LoadStackFiles(ctx context.Context, archive []byte) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.load_files", &retDiags)

	files, dir, err := readStackArchive(archive)
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid files of a stack", Detail: err.Error() + "."}}
	}

	return e.LoadStack(ctx, files, dir)
}

// LoadModuleFiles loads a module from the files a client packed with
// Module.Archive, bounded and checked as LoadStackFiles does; it reads
// nothing but these files.
func (e *Engine) LoadModuleFiles(ctx context.Context, archive []byte) (_ *Module, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.load_files", &retDiags)

	files, dir, err := readStackArchive(archive)
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid files of a module", Detail: err.Error() + "."}}
	}

	return e.LoadModuleAt(ctx, files, dir)
}

func readStackArchive(archive []byte) (fstest.MapFS, string, error) {
	if len(archive) > maxStackArchiveBytes {
		return nil, "", fmt.Errorf("the archive is %d bytes, more than %d", len(archive), maxStackArchiveBytes)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, "", err
	}
	defer gz.Close() //nolint:errcheck // read only

	tr := tar.NewReader(io.LimitReader(gz, maxStackFilesBytes+1))
	files := fstest.MapFS{}

	var manifest *stackArchiveManifest

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, "", err
		}

		m, err := readStackEntry(tr, h, files)
		if err != nil {
			return nil, "", err
		}

		if m == nil {
			continue
		}

		// As a file is: an archive names its directory once.
		if manifest != nil {
			return nil, "", errors.New("the archive has more than one manifest")
		}

		manifest = m
	}

	if manifest == nil || !fs.ValidPath(manifest.Dir) {
		return nil, "", errors.New("the archive names no directory of a stack")
	}

	return files, path.Clean(manifest.Dir), nil
}

// readStackEntry reads one entry of an archive of a stack into files, or
// returns it as the manifest.
func readStackEntry(tr *tar.Reader, h *tar.Header, files fstest.MapFS) (*stackArchiveManifest, error) {
	if err := targz.CheckEntry(h); err != nil {
		return nil, err
	}

	if h.Typeflag != tar.TypeReg || !fs.ValidPath(h.Name) || files[h.Name] != nil {
		return nil, fmt.Errorf("the entry %q is not a file of a stack", h.Name)
	}

	if len(files) >= maxStackFiles {
		return nil, fmt.Errorf("more than %d files", maxStackFiles)
	}

	data, err := io.ReadAll(tr)
	if err != nil {
		return nil, err
	}

	if h.Name == stackManifest {
		manifest := &stackArchiveManifest{}
		if err := json.Unmarshal(data, manifest); err != nil {
			return nil, fmt.Errorf("the manifest: %w", err)
		}

		return manifest, nil
	}

	files[h.Name] = &fstest.MapFile{Data: data, Mode: 0o644}

	return nil, nil
}
