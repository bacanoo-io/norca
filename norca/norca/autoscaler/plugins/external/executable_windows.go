// Copyright IBM Corp. 2020, 2025
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/manager/util_windows.go.

//go:build windows

package external

import (
	"os"
	"path/filepath"
	"strings"
)

// On windows, an executable can be any file with any extension. To avoid
// introspecting the file, here we skip executability checks on windows systems
// and simply check for the convention of an `.exe` extension.
func executable(path string, _ os.FileInfo) bool {
	return strings.EqualFold(filepath.Ext(path), ".exe")
}
