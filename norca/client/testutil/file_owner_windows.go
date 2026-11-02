// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package testutil

import (
	"os"
	"testing"
)

// FileOwner skips the test: Windows files are not owned by numeric user and
// group IDs.
func FileOwner(t *testing.T, fi os.FileInfo) (uid, gid int) {
	t.Helper()
	t.Skipf("file %s has no uid/gid owner on Windows", fi.Name())
	return -1, -1
}
