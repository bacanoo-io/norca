// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package diskusage

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shoenig/test/must"
)

func TestPath(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	must.NoError(t, os.WriteFile(data, bytes.Repeat([]byte("x"), 64<<10), 0o600))
	must.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "more"), bytes.Repeat([]byte("y"), 32<<10), 0o600))

	file, err := Path(data)
	must.NoError(t, err)
	must.GreaterEq(t, int64(64<<10), file)

	// A directory counts every file under it.
	all, err := Path(dir)
	must.NoError(t, err)
	must.GreaterEq(t, file+32<<10, all)

	// A file extended ahead of its data (sparse) counts the data only where
	// the file system tells.
	sparse := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(sparse)
	must.NoError(t, err)
	must.NoError(t, f.Truncate(256<<20))
	must.NoError(t, f.Close())
	size, err := Path(sparse)
	must.NoError(t, err)
	if runtime.GOOS == "linux" {
		must.Less(t, int64(1<<20), size)
	}

	_, err = Path(filepath.Join(dir, "missing"))
	must.Error(t, err)
}
