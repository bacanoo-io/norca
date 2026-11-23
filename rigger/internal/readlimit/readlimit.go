// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package readlimit reads what a bound allows, and says when there is more.
package readlimit

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrTooLarge is the error of a read that has more than its bound.
var ErrTooLarge = errors.New("more than its bound")

// Limit is what a reader bounded by maxBytes is let read: one byte more,
// by which a reader that has more is told from one that ends at the bound.
// The largest bound there is has no number after it, and nothing has more.
func Limit(maxBytes int64) int64 {
	if maxBytes == math.MaxInt64 {
		return maxBytes
	}

	return maxBytes + 1
}

// ReadAll reads r to its end, at most maxBytes. A reader with more is an
// error wrapping ErrTooLarge, never data cut short.
func ReadAll(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, Limit(maxBytes)))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, maxBytes)
	}

	return data, nil
}
