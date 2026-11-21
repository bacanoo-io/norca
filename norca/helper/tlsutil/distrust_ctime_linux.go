// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package tlsutil

import (
	"os"
	"syscall"
	"time"
)

// changeTime returns the change time of a file (ctime): a copy that keeps the
// modification time still changes it.
func changeTime(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
