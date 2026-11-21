// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package builtin holds the modules built into the rigger program (design 7.1:
// builtin/<name>). Each is a module of modules/<name>, of the version its
// metadata says.
package builtin

import (
	"embed"
	"io/fs"
)

//go:embed modules
var files embed.FS

// FS returns the built-in modules: modules/<name>/...
func FS() fs.FS {
	return files
}
