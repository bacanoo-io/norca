// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package testutil

import (
	"os"
	"syscall"
	"testing"
)

// FileOwner returns the numeric user and group IDs that own the file.
func FileOwner(t *testing.T, fi os.FileInfo) (uid, gid int) {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("expected %s to have a *syscall.Stat_t but found %T", fi.Name(), fi.Sys())
	}
	return int(st.Uid), int(st.Gid)
}
