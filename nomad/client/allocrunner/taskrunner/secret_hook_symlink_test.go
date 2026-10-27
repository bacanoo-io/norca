// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// symlinkOrSkip creates a symlink, or skips the test where that needs
// privileges (Windows without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks need privileges here: %v", err)
		}
		t.Fatal(err)
	}
}

// dirEntries returns the names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteSecretFile(t *testing.T) {
	ci.Parallel(t)
	task := &structs.Task{Name: "web", Driver: "mock_driver"}

	t.Run("nested", func(t *testing.T) {
		taskDir := t.TempDir()
		must.NoError(t, os.Mkdir(filepath.Join(taskDir, "secrets"), 0o755))
		must.NoError(t, writeSecretFile(taskDir, "secrets/certs/tls.key", []byte("one"), task))
		must.NoError(t, writeSecretFile(taskDir, "secrets/certs/tls.key", []byte("two"), task))
		b, err := os.ReadFile(filepath.Join(taskDir, "secrets", "certs", "tls.key"))
		must.NoError(t, err)
		must.Eq(t, "two", string(b))
		must.Eq(t, []string{"tls.key"}, dirEntries(t, filepath.Join(taskDir, "secrets", "certs")),
			must.Sprint("no temporary file is left"))
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(filepath.Join(taskDir, "secrets", "certs", "tls.key"))
			must.NoError(t, err)
			must.Eq(t, os.FileMode(0o400), fi.Mode().Perm())
		}
	})

	t.Run("invalid destinations", func(t *testing.T) {
		taskDir := t.TempDir()
		for _, dest := range []string{"", "secrets", "secrets/../x", "secrets//x", "secrets/./x", `secrets/a\b`, "secrets/c:x"} {
			must.Error(t, writeSecretFile(taskDir, dest, []byte("x"), task), must.Sprint(dest))
		}
	})
}

// TestWriteSecretFile_PlantedSymlinks checks that a task can't redirect the
// write of a secret file out of its directory with symlinks it plants in its
// secrets directory (SEC-2).
func TestWriteSecretFile_PlantedSymlinks(t *testing.T) {
	ci.Parallel(t)
	task := &structs.Task{Name: "web", Driver: "mock_driver"}

	t.Run("symlinked parent directory", func(t *testing.T) {
		taskDir, outside := t.TempDir(), t.TempDir()
		must.NoError(t, os.Mkdir(filepath.Join(taskDir, "secrets"), 0o755))
		symlinkOrSkip(t, outside, filepath.Join(taskDir, "secrets", "certs"))

		err := writeSecretFile(taskDir, "secrets/certs/tls.key", []byte("secret"), task)
		must.ErrorContains(t, err, "symlinks are not followed")
		must.Eq(t, []string{}, dirEntries(t, outside))
	})

	t.Run("symlink to a directory inside the task", func(t *testing.T) {
		taskDir := t.TempDir()
		must.NoError(t, os.MkdirAll(filepath.Join(taskDir, "secrets", "real"), 0o755))
		symlinkOrSkip(t, "real", filepath.Join(taskDir, "secrets", "certs"))

		err := writeSecretFile(taskDir, "secrets/certs/tls.key", []byte("secret"), task)
		must.ErrorContains(t, err, "symlinks are not followed")
		must.Eq(t, []string{}, dirEntries(t, filepath.Join(taskDir, "secrets", "real")))
	})

	t.Run("symlinked secrets directory", func(t *testing.T) {
		taskDir, outside := t.TempDir(), t.TempDir()
		symlinkOrSkip(t, outside, filepath.Join(taskDir, "secrets"))

		err := writeSecretFile(taskDir, "secrets/tls.key", []byte("secret"), task)
		must.Error(t, err)
		must.Eq(t, []string{}, dirEntries(t, outside))
	})

	t.Run("symlink at the destination", func(t *testing.T) {
		taskDir, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(outside, "victim")
		must.NoError(t, os.WriteFile(target, []byte("original"), 0o644))
		must.NoError(t, os.Mkdir(filepath.Join(taskDir, "secrets"), 0o755))
		symlinkOrSkip(t, target, filepath.Join(taskDir, "secrets", "tls.key"))

		// The link is replaced by the file, the target is untouched.
		must.NoError(t, writeSecretFile(taskDir, "secrets/tls.key", []byte("secret"), task))
		b, err := os.ReadFile(target)
		must.NoError(t, err)
		must.Eq(t, "original", string(b))
		fi, err := os.Lstat(filepath.Join(taskDir, "secrets", "tls.key"))
		must.NoError(t, err)
		must.True(t, fi.Mode().IsRegular())
	})
}
