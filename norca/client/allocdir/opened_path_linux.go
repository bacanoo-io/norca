// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package allocdir

import (
	"os"
	"strconv"
)

// openedPath returns the path of the file f refers to, as the kernel names
// its descriptor in /proc/self/fd: absolute, with every symbolic link
// resolved, and the file that was opened even when a link on the way was
// swapped since. ok is true when the path is known.
func openedPath(f *os.File) (path string, ok bool, err error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return "", false, err
	}
	var readErr error
	if err := rc.Control(func(fd uintptr) {
		path, readErr = os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10))
	}); err != nil {
		return "", false, err
	}
	if readErr != nil {
		return "", false, readErr
	}
	return path, true, nil
}
