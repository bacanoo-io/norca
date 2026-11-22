// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package targz closes the writers of a gzip compressed tar.
package targz

import (
	"archive/tar"
	"compress/gzip"
)

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
