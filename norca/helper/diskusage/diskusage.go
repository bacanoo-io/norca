// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package diskusage measures the space files take on disk.
package diskusage

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Path returns the bytes a file, or every file under a directory, takes on
// disk. Where the file system tells (Unix), it counts the blocks allocated to
// a file, so a sparse file counts what it holds rather than its length:
// Badger and BoltDB extend their files ahead of the data. Elsewhere it counts
// the length of the files.
func Path(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return fileBytes(info), nil
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// removed while walking (a compaction): it takes no space now
			return nil
		}
		total += fileBytes(info)
		return nil
	})
	return total, err
}
