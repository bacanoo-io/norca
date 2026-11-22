// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package readlimit reads what a bound allows, and says when there is more.
package readlimit

import (
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge is the error of a read that has more than its bound.
var ErrTooLarge = errors.New("more than its bound")

// ReadAll reads r to its end, at most maxBytes. A reader with more is an
// error wrapping ErrTooLarge, never data cut short.
func ReadAll(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, maxBytes)
	}

	return data, nil
}
