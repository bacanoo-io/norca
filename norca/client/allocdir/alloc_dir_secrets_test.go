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

// newSecretsTestDir builds an allocation directory with the task t1 and
// writes a secret (secrets/norca_token "jwt"), a private file (private/key)
// and an ordinary file (local/plain "plain").
func newSecretsTestDir(t *testing.T) (*AllocDir, *TaskDir) {
	t.Helper()
	d := NewAllocDir(testlog.HCLogger(t), t.TempDir(), "test")
	must.NoError(t, d.Build())
	t.Cleanup(func() { _ = d.Destroy() })
	td := d.NewTaskDir(t1.Name)
	must.NoError(t, td.Build(false, nil))

	must.NoError(t, os.WriteFile(filepath.Join(td.SecretsDir, "norca_token"), []byte("jwt"), 0o600))
	must.NoError(t, os.WriteFile(filepath.Join(td.PrivateDir, "key"), []byte("key"), 0o600))
	must.NoError(t, os.WriteFile(filepath.Join(td.LocalDir, "plain"), []byte("plain"), 0o644))
	return d, td
}

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	must.NoError(t, err)
	return string(b)
}

// ReadAt, List and Stat refuse a symbolic link that points into the secrets
// or private directory of a task, wherever in the allocation directory the
// link is (review finding JOG-15; inherited). The check used to look at the
// path as given only, so ReadAt read through such a link: a sidecar that
// links another task's token into the shared alloc directory handed it to
// any token with read-fs or read-logs. List and Stat did not check the
// secrets directory at all.
func TestAllocDir_SymlinkIntoSecretDir(t *testing.T) {
	ci.Parallel(t)
	d, td := newSecretsTestDir(t)

	link := func(at, target string) {
		must.NoError(t, os.Symlink(target, at))
	}
	link(filepath.Join(td.LocalDir, "token-link"), filepath.Join("..", TaskSecrets, "norca_token"))
	link(filepath.Join(td.LocalDir, "secrets-link"), filepath.Join("..", TaskSecrets))
	link(filepath.Join(td.LocalDir, "private-link"), filepath.Join("..", TaskPrivate, "key"))
	link(filepath.Join(td.LocalDir, "plain-link"), "plain")
	// as a sidecar would make it: in the shared alloc directory
	link(filepath.Join(d.SharedDir, SharedDataDir, "token-link"),
		filepath.Join("..", "..", t1.Name, TaskSecrets, "norca_token"))

	local := func(name string) string { return filepath.Join(t1.Name, TaskLocal, name) }
	secret := "Reading secret file prohibited"

	for _, path := range []string{
		filepath.Join(t1.Name, TaskSecrets, "norca_token"),
		local("token-link"),
		filepath.Join(local("secrets-link"), "norca_token"),
		filepath.Join(SharedAllocName, SharedDataDir, "token-link"),
	} {
		_, err := d.ReadAt(path, 0)
		must.ErrorContains(t, err, secret, must.Sprintf("ReadAt %s", path))
		_, err = d.Stat(path)
		must.ErrorContains(t, err, secret, must.Sprintf("Stat %s", path))
	}
	_, err := d.ReadAt(local("private-link"), 0)
	must.ErrorContains(t, err, "Reading private file prohibited")

	for _, path := range []string{filepath.Join(t1.Name, TaskSecrets), local("secrets-link")} {
		_, err := d.List(path)
		must.ErrorContains(t, err, secret, must.Sprintf("List %s", path))
	}

	// a link to an ordinary file, and the directories around, stay readable
	r, err := d.ReadAt(local("plain-link"), 0)
	must.NoError(t, err)
	must.Eq(t, "plain", readAll(t, r))
	info, err := d.Stat(local("plain-link"))
	must.NoError(t, err)
	must.Eq(t, int64(len("plain")), info.Size)

	files, err := d.List(filepath.Join(t1.Name, TaskLocal))
	must.NoError(t, err)
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	must.Eq(t, []string{"plain", "plain-link", "private-link", "secrets-link", "token-link"}, names)

	files, err = d.List(t1.Name)
	must.NoError(t, err)
	must.SliceNotEmpty(t, files)
}
