// Copyright IBM Corp. 2020, 2025
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/manager/util_unix.go.

//go:build !windows

package external

import "os"

// executable checks to see if the file is executable by anyone.
func executable(_ string, f os.FileInfo) bool {
	return f.Mode().Perm()&0111 != 0
}
