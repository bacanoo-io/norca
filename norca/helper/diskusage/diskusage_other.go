// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !unix

package diskusage

import "os"

// fileBytes returns the length of a file.
func fileBytes(info os.FileInfo) int64 {
	return info.Size()
}
