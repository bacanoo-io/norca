// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package allocdir

import "os"

// openedPath does not know the path of an opened file on this platform: the
// file system API checks the path before the open only (checkProtected).
func openedPath(*os.File) (path string, ok bool, err error) {
	return "", false, nil
}
