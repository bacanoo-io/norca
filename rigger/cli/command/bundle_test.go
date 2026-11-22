// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

func TestRun_Bundle(t *testing.T) {
	repo := registryRepo(t)
	online := t.TempDir()
	t.Setenv("RIGGER_CACHE_DIR", online)

	dir := writeTree(t, map[string]string{
		stackFile: `rigger {
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

	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	first, second := filepath.Join(t.TempDir(), "a.riggerbundle"), filepath.Join(t.TempDir(), "b.riggerbundle")
	riggerRun(t, exitOK, []string{"1 module(s) of the stack offline"}, "bundle", "create", dir, "-o", first)
	riggerRun(t, exitOK, []string{wroteLock}, "bundle", "create", dir, "-o", second)

	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)

	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatalf("two bundles of the same modules differ (%d, %d bytes)", len(a), len(b))
	}

	riggerRun(t, exitOK, []string{"local/greeter 1.1.0 (greeter/v1.1.0, sha256:"}, "bundle", "inspect", first)

	// Without a network: the repository is gone and the cache is empty.
	if err := os.Rename(repo, repo+".gone"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	riggerRunErr(t, "Module not loaded", "stack", "validate", dir)

	riggerRun(t, exitOK, []string{"imported local/greeter 1.1.0"}, "bundle", "import", first)
	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", dir)
}

// wrongVersion is the version of the modules of a wrong bundle.
const wrongVersion = "1.0.0"

// A bundle with a module whose content is not its digest is refused and
// leaves nothing in the cache, not even the modules that were right.
func TestRun_BundleImport_WrongDigest(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("RIGGER_CACHE_DIR", cache)

	module := func(name string) fstest.MapFS {
		return fstest.MapFS{"modules/" + name + "/metadata.hcl": {Data: []byte("module {\n  name    = \"" + name + "\"\n  version = \"" + wrongVersion + "\"\n}\n")}}
	}

	eng, err := rigger.New()
	if err != nil {
		t.Fatal(err)
	}

	good, bad := module("good"), module("bad")

	digest, err := eng.ModuleDigest(t.Context(), good, "modules/good")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, "s", []rigger.SourcedModule{
		{Source: "a/good", Version: wrongVersion, Path: "modules/good", Commit: strings.Repeat("a", 40), Digest: digest, Dirs: []string{"modules/good"}, Root: good},
		{Source: "a/bad", Version: wrongVersion, Path: "modules/bad", Commit: strings.Repeat("b", 40), Digest: digest, Dirs: []string{"modules/bad"}, Root: bad},
	}); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(t.TempDir(), "wrong.riggerbundle")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	riggerRunErr(t, "a/bad "+wrongVersion+": the bundle holds content of the digest", "bundle", "import", p)

	_ = filepath.WalkDir(cache, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("left in the cache: %s", path)
		}

		return nil
	})
}
