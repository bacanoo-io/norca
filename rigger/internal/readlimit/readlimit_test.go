// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package readlimit_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/readlimit"
)

func TestReadAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "empty", size: 0},
		{name: "under the bound", size: 9},
		{name: "at the bound", size: 10},
		{name: "one byte more", size: 11, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := readlimit.ReadAll(bytes.NewReader(make([]byte, tt.size)), 10)

			switch {
			case tt.wantErr && !errors.Is(err, readlimit.ErrTooLarge):
				t.Errorf("ReadAll() = %d bytes, %v; want ErrTooLarge", len(data), err)
			case !tt.wantErr && (err != nil || len(data) != tt.size):
				t.Errorf("ReadAll() = %d bytes, %v; want %d", len(data), err, tt.size)
			}
		})
	}
}

// The largest bound there is has no number after it: it reads all, and
// does not turn into a bound of nothing.
func TestReadAll_LargestBound(t *testing.T) {
	t.Parallel()

	data, err := readlimit.ReadAll(bytes.NewReader(make([]byte, 11)), math.MaxInt64)
	if err != nil || len(data) != 11 {
		t.Errorf("ReadAll() with the largest bound = %d bytes, %v; want 11", len(data), err)
	}

	if got := readlimit.Limit(math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("Limit() of the largest bound = %d", got)
	}

	if got := readlimit.Limit(10); got != 11 {
		t.Errorf("Limit(10) = %d, want 11", got)
	}
}
