// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
)

// errPath is the error of a path that is none in a repository.
var errPath = errors.New("not a path in a repository")

// treePath returns the path of a module in its tree. A lock file, an index
// or a manifest names it, and it is joined to a directory of the cache: it
// stays below that, also where a backslash separates (Windows).
func treePath(p string) (string, error) {
	dir := path.Clean(p)
	if _, err := filepath.Localize(dir); err != nil {
		return "", fmt.Errorf("%q: %w", p, errPath)
	}

	return dir, nil
}
