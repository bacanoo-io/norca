// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package atomicfile writes a file so that it is never seen half written:
// the data goes into a new file in the same directory, which is renamed
// over the old one. A reader has the old content or the new one, and a
// write that is cut (a crash, a full disk, an interrupt) leaves the old
// file as it was. Writing in place truncates the file first, and what is
// left when the write stops is neither.
package atomicfile

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes data to the file at path as os.WriteFile does, by a rename:
// a file that is there keeps its mode, a new file has perm (before umask).
// A link is written through; a file that may not be written is not
// replaced either.
func Write(path string, data []byte, perm fs.FileMode) error {
	return write(path, data, perm, false)
}

// WriteMode is Write for a file whose mode is the caller's: it has exactly
// the mode perm, whatever mode a file that is there has (also one that may
// not be written), and from before it holds data: the file of a secret.
func WriteMode(path string, data []byte, perm fs.FileMode) error {
	return write(path, data, perm, true)
}

func write(path string, data []byte, perm fs.FileMode, exact bool) (err error) {
	// The rename would put a file in the place of a link.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}

	if info, statErr := os.Stat(path); statErr == nil && !exact {
		// The rename needs no leave of the file, only of its directory:
		// without this a file that is read-only would be replaced.
		if err := writable(path); err != nil {
			return err
		}

		perm, exact = info.Mode().Perm(), true
	}

	tmp, err := createBeside(path, perm)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()

	if err := fill(tmp, data, perm, exact); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// writable reports why the file at path may not be written, nil if it may.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: the file the caller writes
	if err != nil {
		return err
	}

	return f.Close()
}

// createBeside creates a new file in the directory of path, hidden and
// named after it, with the mode perm (before umask).
func createBeside(path string, perm fs.FileMode) (*os.File, error) {
	for {
		name := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-"+rand.Text())

		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: beside the file the caller writes
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// fill writes data into the new file and closes it; with exact the file
// has the mode perm before it has the data.
func fill(f *os.File, data []byte, perm fs.FileMode, exact bool) error {
	var err error

	if exact {
		err = f.Chmod(perm)
	}

	if err == nil {
		_, err = f.Write(data)
	}

	// On disk before the rename: a crash after it must not leave the name
	// with an empty file.
	if err == nil {
		err = f.Sync()
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	return err
}
