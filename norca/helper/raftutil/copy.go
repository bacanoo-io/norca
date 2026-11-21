// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
)

// The operator raft commands read a raft store without changing it. Badger
// opens a database read-only only after a clean close: a server that was
// killed, ran out of memory or lost power leaves a memtable log that has to be
// truncated, which a read-only open refuses ("Log truncate required"), and
// Windows has no read-only mode at all. Exactly the stores the commands are
// for after a crash could not be read (review finding HLP-7). Such a store is
// copied into a temporary directory, the copy is opened as a server would open
// it, and the copy is removed when the store is closed.

// copiedStore is a raft store opened from a copy; Close removes the copy.
type copiedStore struct {
	*raftbadger.Store
	dir string
}

func (s *copiedStore) Close() error {
	err := s.Store.Close()
	return errors.Join(err, os.RemoveAll(s.dir))
}

// openBadgerCopy copies the Badger store p into a temporary directory and opens
// the copy writable. It tells on stderr that the copy is read, and why.
func openBadgerCopy(p, why string) (Store, error) {
	dir, err := os.MkdirTemp("", "norca-raft-copy-")
	if err != nil {
		return nil, fmt.Errorf("failed to open raft logs: %s, and a copy can't be made: %v", why, err)
	}
	if err := copyStoreDir(p, dir); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to open raft logs: %s, and the copy failed: %v", why, err)
	}
	s, err := raftbadger.Open(raftbadger.Options{Path: dir})
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to open a copy of the raft logs (%s): %v", why, err)
	}
	fmt.Fprintf(os.Stderr, "Reading a copy of the raft store %s in %s: %s; the store itself is not changed.\n", p, dir, why)
	return &copiedStore{Store: s, dir: dir}, nil
}

// copyStoreDir copies the files of a Badger directory, without its lock file.
func copyStoreDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular() || d.Name() == "LOCK":
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// badgerLockHeld reports, on Windows, whether a process holds the lock file of
// the Badger store: Badger opens it there without sharing for as long as the
// database is open, so it can't be opened by another process. A store that is
// in use is not copied.
func badgerLockHeld(p string) bool {
	f, err := os.Open(filepath.Join(p, "LOCK"))
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	f.Close()
	return false
}
