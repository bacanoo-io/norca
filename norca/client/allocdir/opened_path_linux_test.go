// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package allocdir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// checkOpened judges the file that was opened, not the path asked for: a
// secret, a private file or a file outside the allocation directory is
// refused whatever path led to it; an ordinary file passes, and openedPath
// names the file a link led to.
func TestAllocDir_CheckOpened(t *testing.T) {
	ci.Parallel(t)
	d, td := newSecretsTestDir(t)

	open := func(path string) *os.File {
		f, err := os.Open(path)
		must.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	err := d.checkOpened("web/local/x", open(filepath.Join(td.SecretsDir, "norca_token")))
	must.ErrorContains(t, err, "Reading secret file prohibited: web/local/x")
	err = d.checkOpened("web/local/x", open(td.SecretsDir))
	must.ErrorContains(t, err, "Reading secret file prohibited")
	err = d.checkOpened("web/local/x", open(filepath.Join(td.PrivateDir, "key")))
	must.ErrorContains(t, err, "Reading private file prohibited")

	outside := filepath.Join(t.TempDir(), "outside")
	must.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	must.ErrorContains(t, d.checkOpened("web/local/x", open(outside)), "Path escapes the alloc directory")

	plain := filepath.Join(td.LocalDir, "plain")
	must.NoError(t, d.checkOpened("web/local/plain", open(plain)))

	linked := filepath.Join(td.LocalDir, "plain-link")
	must.NoError(t, os.Symlink("plain", linked))
	got, ok, err := openedPath(open(linked))
	must.NoError(t, err)
	must.True(t, ok)
	want, err := filepath.EvalSymlinks(plain)
	must.NoError(t, err)
	must.Eq(t, want, got)
}
