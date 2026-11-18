// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signedStack is a stack of the module greeter of the registry repo,
// trusting the public key in keys/trusted.pub.
func signedStack(repo string) string {
	return `rig {
  name = "signed"

  registry "local" {
    source = "file://` + repo + `"
  }

  trust {
    registry          = "local"
    require_signature = true
    public_keys       = [file("keys/trusted.pub")]
  }
}

module "hello" {
  source = "local/greeter"
}
`
}

func TestRun_KeyAndTrust(t *testing.T) {
	repo := registryRepo(t)
	keys := t.TempDir()

	t.Setenv("RIG_CACHE_DIR", t.TempDir())
	t.Setenv(passwordEnv, "secret")

	rigRun(t, exitOK, []string{wroteLock, "key sha256:"}, "key", "generate", "-out", filepath.Join(keys, "signer"))
	rigRun(t, exitOK, []string{wroteLock}, "key", "generate", "-out", filepath.Join(keys, "other"))

	// The greeter 1.1.1 is signed in the repository and tagged.
	module := filepath.Join(repo, "modules", "greeter")
	if err := os.WriteFile(filepath.Join(module, "metadata.hcl"), []byte("module {\n  name    = \"greeter\"\n  version = \"1.1.1\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rigRun(t, exitOK, []string{"signed greeter 1.1.1"}, "key", "sign", module, "-key", filepath.Join(keys, "signer.key"))
	rigRun(t, exitOK, []string{"is signed by sha256:"}, "key", "verify", module, "-key", filepath.Join(keys, "signer.pub"))
	rigRunErr(t, "not signed", "key", "verify", module, "-key", filepath.Join(keys, "other.pub"))

	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter 1.1.1, signed")
	gitIn(t, repo, "tag", "-a", "-m", "1.1.1", "greeter/v1.1.1")

	stack := func(trusted string) string {
		pub, err := os.ReadFile(filepath.Join(keys, trusted))
		if err != nil {
			t.Fatal(err)
		}

		return writeTree(t, map[string]string{stackFile: signedStack(repo), "keys/trusted.pub": string(pub)})
	}

	ok := stack("signer.pub")
	rigRun(t, exitOK, []string{wroteLock}, "stack", "sync", ok)

	lock, err := os.ReadFile(filepath.Join(ok, lockFile))
	if err != nil || !strings.Contains(string(lock), "signers") || !strings.Contains(string(lock), "greeter/v1.1.1") {
		t.Fatalf("lock = %s, %v; want the signer of greeter/v1.1.1", lock, err)
	}

	rigRun(t, exitOK, []string{"is valid"}, "stack", "validate", ok)

	rigRunErr(t, "not signed by a key the stack trusts", "stack", "sync", stack("other.pub"))

	if err := os.Unsetenv(passwordEnv); err != nil {
		t.Fatal(err)
	}

	rigRunErr(t, passwordEnv, "key", "generate", "-out", filepath.Join(keys, "third"))
}
