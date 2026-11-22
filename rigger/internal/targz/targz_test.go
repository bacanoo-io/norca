// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package targz_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/targz"
)

// errWriter fails every write.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errBroken }

var errBroken = errors.New("broken")

// Closed without an error, the archive is whole and reads back.
func TestClose(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{Name: "a", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}

	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	if err := targz.Close(tw, gz, nil); err != nil {
		t.Fatal(err)
	}

	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}

	h, err := tar.NewReader(zr).Next()
	if err != nil || h.Name != "a" {
		t.Fatalf("read %v, %v", h, err)
	}
}

// After an error of the writing, both writers are closed (another write
// fails) and that error is returned; a failing close of the tar is
// returned too, with the gzip closed.
func TestClose_Error(t *testing.T) {
	t.Parallel()

	gz := gzip.NewWriter(io.Discard)
	tw := tar.NewWriter(gz)

	errWrite := errors.New("write failed")
	if err := targz.Close(tw, gz, errWrite); !errors.Is(err, errWrite) {
		t.Errorf("Close() = %v, want the error of the writing", err)
	}

	if _, err := gz.Write([]byte("x")); err == nil {
		t.Error("the gzip writer is still open")
	}

	if err := tw.WriteHeader(&tar.Header{Name: "b"}); err == nil {
		t.Error("the tar writer is still open")
	}

	// The tar flushes its trailer into a gzip that can't write it.
	gz = gzip.NewWriter(errWriter{})
	tw = tar.NewWriter(gz)

	if err := targz.Close(tw, gz, nil); !errors.Is(err, errBroken) {
		t.Errorf("Close() over a broken writer = %v, want %v", err, errBroken)
	}

	if _, err := gz.Write([]byte("x")); err == nil {
		t.Error("the gzip writer is still open after a failed close of the tar")
	}
}
