// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package allocdir

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/shoenig/test/must"
)

// ReadAt refuses a path in a task's secrets directory, but checks the path
// as given: a symbolic link elsewhere in the allocation directory that points
// into the secrets directory is read through (review finding JOG-8, question
// 3, checked 2026-11-18). Only something running in the task (the task
// itself, or an `alloc exec` session) can make such a link, so a read-fs token
// alone gets the workload identity token only when the job links or copies it
// out of the secrets directory. CURRENT BEHAVIOUR.
func TestAllocDir_ReadAt_SymlinkIntoSecretDir(t *testing.T) {
	ci.Parallel(t)

	d := NewAllocDir(testlog.HCLogger(t), t.TempDir(), "test")
	must.NoError(t, d.Build())
	defer func() { _ = d.Destroy() }()
	td := d.NewTaskDir(t1.Name)
	must.NoError(t, td.Build(false, nil))

	must.NoError(t, os.WriteFile(filepath.Join(td.SecretsDir, "norca_token"), []byte("jwt"), 0o600))
	must.NoError(t, os.Symlink(filepath.Join("..", TaskSecrets, "norca_token"),
		filepath.Join(td.LocalDir, "token-link")))

	_, err := d.ReadAt(filepath.Join(t1.Name, TaskSecrets, "norca_token"), 0)
	must.ErrorContains(t, err, "Reading secret file prohibited")

	// CURRENT BEHAVIOUR: the link is read.
	r, err := d.ReadAt(filepath.Join(t1.Name, TaskLocal, "token-link"), 0)
	must.NoError(t, err)
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	must.NoError(t, err)
	must.Eq(t, "jwt", string(got))
}
