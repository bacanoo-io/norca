// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRun_Bundle(t *testing.T) {
	repo := registryRepo(t)
	online := t.TempDir()
	t.Setenv("RIG_CACHE_DIR", online)

	dir := writeTree(t, map[string]string{
		stackFile: `rig {
  name = "offline"

  registry "local" {
    source = "file://` + repo + `"
  }
}

module "hello" {
  source  = "local/greeter"
  version = "~> 1.1.0"
}
`,
	})

	rigRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	first, second := filepath.Join(t.TempDir(), "a.rigbundle"), filepath.Join(t.TempDir(), "b.rigbundle")
	rigRun(t, exitOK, []string{"1 module(s) of the stack offline"}, "bundle", "create", dir, "-o", first)
	rigRun(t, exitOK, []string{wroteLock}, "bundle", "create", dir, "-o", second)

	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)

	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatalf("two bundles of the same modules differ (%d, %d bytes)", len(a), len(b))
	}

	rigRun(t, exitOK, []string{"local/greeter 1.1.0 (greeter/v1.1.0, sha256:"}, "bundle", "inspect", first)

	// Without a network: the repository is gone and the cache is empty.
	if err := os.Rename(repo, repo+".gone"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_CACHE_DIR", t.TempDir())
	rigRunErr(t, "Module not loaded", "stack", "validate", dir)

	rigRun(t, exitOK, []string{"imported local/greeter 1.1.0"}, "bundle", "import", first)
	rigRun(t, exitOK, []string{"is valid"}, "stack", "validate", dir)
}
