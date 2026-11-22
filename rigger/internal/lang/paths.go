// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
)

// What the digest covers. It is of the files a walk finds, hidden ones left
// out, a line for each with its name. So nothing else is content of a
// module or a stack: no hidden path, no path under another name than its
// directory lists (a short name or another case, where a file system takes
// those), and no name that could be read as more lines than one.

// The errors of a path the digest does not cover.
var (
	errHiddenPath  = errors.New("a hidden path is not in the digest, so it is no part of a module or a stack")
	errOtherName   = errors.New("a path written otherwise than its directory lists it is not in the digest")
	errControlName = errors.New("a name with a control character can't be in a digest")
)

func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// checkName refuses a name with a control character.
func checkName(name string) error {
	if strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%q: %w", name, errControlName)
	}

	return nil
}

// nameDiag is the diagnostic of a name that can't be in a digest, nil for
// one that can.
func nameDiag(name string) *hcl.Diagnostic {
	if err := checkName(name); err != nil {
		return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid name", Detail: err.Error() + "."}
	}

	return nil
}

// CheckPath refuses a path that leads from the directory from of fsys to
// what the digest does not cover: each element is neither hidden nor
// written otherwise than its directory lists it. One that is not there is
// left to who reads it.
func CheckPath(fsys fs.FS, from, p string) error {
	cur := path.Clean(from)

	for elem := range strings.SplitSeq(path.Clean(p), "/") {
		switch elem {
		case ".":
			continue
		case "..":
			cur = path.Dir(cur)

			continue
		}

		if isHidden(elem) {
			return fmt.Errorf("%q: %w", p, errHiddenPath)
		}

		child := path.Join(cur, elem)
		if _, err := fs.Stat(fsys, child); err != nil {
			return nil // the reader of the path says what is wrong with it
		}

		entries, err := fs.ReadDir(fsys, cur)
		if err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}

		if !slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == elem }) {
			return fmt.Errorf("%q: %w", p, errOtherName)
		}

		cur = child
	}

	return nil
}
