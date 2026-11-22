// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package readlimit_test

import (
	"bytes"
	"errors"
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
