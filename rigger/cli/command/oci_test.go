// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/oci/ocitest"
)

func TestRun_OCI(t *testing.T) {
	registry := ocitest.New()
	defer registry.Close()

	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	t.Setenv(passwordEnv, "")

	keys := t.TempDir()
	riggerRun(t, exitOK, []string{wroteLock}, "key", "generate", "-out", filepath.Join(keys, "k"))

	module := writeTree(t, map[string]string{
		metadataFile:    "module {\n  name    = \"greeter\"\n  version = \"1.0.0\"\n}\n",
		"variables.hcl": "variable \"text\" {\n  default = \"hello\"\n}\n",
		"g.var.hcl":     "variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = var.text\n  }\n}\n",
	})
	repo := "oci://" + registry.Host() + "/rigger/greeter"

	riggerRun(t, exitOK, []string{"signed greeter 1.0.0"}, "key", "sign", module, "-key", filepath.Join(keys, "k.key"))
	riggerRun(t, exitOK, []string{"published greeter 1.0.0 to " + repo + ":1.0.0 (sha256:"}, "module", "publish", module, "-to", repo)

	pub, err := os.ReadFile(filepath.Join(keys, "k.pub"))
	if err != nil {
		t.Fatal(err)
	}

	dir := writeTree(t, map[string]string{
		stackFile: `rigger {
  name = "oci"

  trust {
    require_signature = true
    public_keys       = [file("keys/k.pub")]
  }
}

module "hello" {
  source  = "` + repo + `"
  version = "~> 1.0"
}
`,
		"keys/k.pub": string(pub),
	})

	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	lock, _ := os.ReadFile(filepath.Join(dir, lockFile))
	for _, want := range []string{`ref        = "1.0.0"`, `commit     = "sha256:`, "signers"} {
		if !strings.Contains(string(lock), want) {
			t.Fatalf("lock does not hold %q:\n%s", want, lock)
		}
	}

	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", dir)

	// Without the registry the cached layer of the locked manifest serves.
	registry.Close()
	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", dir)
}

func TestRun_OCITagMoved(t *testing.T) {
	registry := ocitest.New()
	defer registry.Close()

	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	module := writeTree(t, map[string]string{metadataFile: "module {\n  name    = \"greeter\"\n  version = \"1.0.0\"\n}\n"})
	repo := "oci://" + registry.Host() + "/rigger/greeter"

	riggerRun(t, exitOK, []string{"published"}, "module", "publish", module, "-to", repo)

	dir := writeTree(t, map[string]string{stackFile: "rigger {\n  name = \"oci\"\n}\n\nmodule \"hello\" {\n  source = \"" + repo + "\"\n}\n"})
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	if err := os.WriteFile(filepath.Join(module, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	riggerRun(t, exitOK, []string{"published"}, "module", "publish", module, "-to", repo)
	riggerRunErr(t, "the tag points to another commit", "stack", "validate", dir)
}
