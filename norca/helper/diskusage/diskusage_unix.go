// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package diskusage

import (
	"os"
	"syscall"
)

// fileBytes returns the blocks allocated to a file in bytes (st_blocks is
// in units of 512 bytes on every Unix).
func fileBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512 //nolint:unconvert // uint64 on some systems
	}
	return info.Size()
}
