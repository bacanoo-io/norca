// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package targz closes the writers of a gzip compressed tar, and holds the
// rule of the readers of one: no sparse file.
package targz

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"strings"
)

// ErrSparse is the error of a sparse file in an archive.
var ErrSparse = errors.New("a sparse file is not taken")

// paxSparse is how the records of a sparse file begin.
const paxSparse = "GNU.sparse."

// CheckEntry refuses a sparse file. It is a few bytes in an archive and as
// large as it claims when it is read, so a bound on the bytes of the
// archive, which is how the readers of Rigger bound one, does not hold it.
// Nothing that writes the archives of Rigger writes one.
func CheckEntry(h *tar.Header) error {
	sparse := h.Typeflag == tar.TypeGNUSparse

	for key := range h.PAXRecords {
		sparse = sparse || strings.HasPrefix(key, paxSparse)
	}

	if sparse {
		return fmt.Errorf("the entry %q: %w", h.Name, ErrSparse)
	}

	return nil
}

// Close closes tw, then gz, on every path: after err, the error of the
// writing (both are closed and err returned), and when closing tw fails.
// It returns the first error.
func Close(tw *tar.Writer, gz *gzip.Writer, err error) error {
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()

		return err
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()

		return err
	}

	return gz.Close()
}
