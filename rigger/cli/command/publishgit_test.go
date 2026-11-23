// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// listedGreeter is what rigger index update says of a version whose
	// signature a key of the namespace made.
	listedGreeter = "listed module acme/greeter 1.0.0"
	// taggedGreeter is what a publish of the greeter 1.0.0 says of its tag.
	taggedGreeter = "as greeter/v1.0.0"
	// notCommitted is the refusal of a publish with changes on disk.
	notCommitted = "changes that are not committed"
	// childOutputs is a file of the child module of childRepo.
	childOutputs = "lib/x/outputs.hcl"
)

// publishEnv gives a publish its cache, the password of its key and the
// author of its commits, and returns the directory of a new key pair acme.
func publishEnv(t *testing.T) string {
	t.Helper()

	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	t.Setenv(passwordEnv, "secret")

	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(v, "t")
	}

	for _, v := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(v, "t@example.org")
	}

	keys := t.TempDir()
	riggerRun(t, exitOK, []string{wroteLock}, "key", "generate", "-out", filepath.Join(keys, "acme"))

	return keys
}

// acmeRegistry is a registry of the shape of the global one: its namespace
// acme lists the repository and has the public key of keys.
func acmeRegistry(t *testing.T, repo, keys string) string {
	t.Helper()

	pub, err := os.ReadFile(filepath.Join(keys, "acme.pub"))
	if err != nil {
		t.Fatal(err)
	}

	return writeTree(t, map[string]string{
		"namespaces/acme.json": `{"name": "acme", "owner": "acme", "repositories": [{"url": "` + repo + `", "layout": "registry"}]}` + "\n",
		"keys/acme/acme.pub":   string(pub),
	})
}

// checkedOutWithCRLF turns core.autocrlf on in the repository and checks
// the files of dir out again: they have CRLF on disk and LF in the
// repository, and git status is clean.
func checkedOutWithCRLF(t *testing.T, repo, dir string) {
	t.Helper()

	gitIn(t, repo, "config", "core.autocrlf", "true")

	entries, err := os.ReadDir(filepath.Join(repo, dir))
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		if err := os.Remove(filepath.Join(repo, dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	gitIn(t, repo, "checkout", "--", dir)

	data, err := os.ReadFile(filepath.Join(repo, dir, metadataFile))
	if err != nil || !strings.Contains(string(data), "\r\n") {
		t.Fatalf("%s/%s has no CRLF after the checkout (%v):\n%q", dir, metadataFile, err, data)
	}
}

// childRepo is greeterRepo whose greeter calls the module lib/x, outside
// its own directory.
func childRepo(t *testing.T) string {
	t.Helper()

	repo := greeterRepo(t, "1.0.0")

	writeFile(t, filepath.Join(repo, "modules", "greeter", "modules.hcl"), "module \"x\" {\n  source = \"../../lib/x\"\n}\n")
	writeFile(t, filepath.Join(repo, "lib", "x", metadataFile), "module {\n  name    = \"x\"\n  version = \"1.0.0\"\n  library = true\n}\n")
	writeFile(t, filepath.Join(repo, childOutputs), "output \"name\" {\n  value = \"x\"\n}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter calls lib/x")

	return repo
}

// The signature of a published version is of the tree its tag names: the
// registry, which reads that tree, lists the version.
func TestPublishGit_SignsCommittedTree(t *testing.T) {
	tests := []struct {
		name    string
		repo    func(t *testing.T) string
		prepare func(t *testing.T, repo string)
	}{
		{
			name:    "the work tree as it was committed",
			prepare: func(*testing.T, string) {},
		},
		{
			name: "core.autocrlf: CRLF in the work tree, LF in the repository",
			prepare: func(t *testing.T, repo string) {
				t.Helper()
				checkedOutWithCRLF(t, repo, "modules/greeter")
			},
		},
		{
			name: "a file that git ignores under the module",
			prepare: func(t *testing.T, repo string) {
				t.Helper()

				writeFile(t, filepath.Join(repo, ".gitignore"), "*.bak\n")
				gitIn(t, repo, "add", "-A")
				gitIn(t, repo, "commit", "--quiet", "-m", "ignore")
				writeFile(t, filepath.Join(repo, "modules", "greeter", "files", "notes.bak"), "left by an editor\n")
			},
		},
		{
			name:    "a child module outside the directory of the module",
			repo:    childRepo,
			prepare: func(*testing.T, string) {},
		},
		{
			name: "a linked work tree of the repository",
			prepare: func(t *testing.T, repo string) {
				t.Helper()

				// The module is published from the linked work tree; the
				// registry reads the repository.
				linked := filepath.Join(t.TempDir(), "linked")
				gitIn(t, repo, "worktree", "add", "--quiet", "--detach", linked)
				t.Chdir(filepath.Join(linked, "modules", "greeter"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.repo == nil {
				tt.repo = func(t *testing.T) string {
					t.Helper()

					return greeterRepo(t, "1.0.0")
				}
			}

			repo := tt.repo(t)
			keys := publishEnv(t)

			t.Chdir(filepath.Join(repo, "modules", "greeter"))
			tt.prepare(t, repo)

			riggerRun(t, exitOK, []string{"signed greeter 1.0.0", taggedGreeter}, "module", "publish", "-key", filepath.Join(keys, "acme.key"))

			reg := acmeRegistry(t, repo, keys)
			riggerRun(t, exitOK, []string{listedGreeter}, "index", "update", "-dir", reg, "-today", "2026-11-18")
		})
	}
}

// A change of a child module that is not committed is not in the tree of
// the tag: the publish refuses it as it refuses one of the module itself,
// and makes no tag.
//
//nolint:paralleltest // publishEnv sets the environment of the publish (t.Setenv)
func TestPublishGit_ChildNotCommitted(t *testing.T) {
	repo := childRepo(t)
	keys := publishEnv(t)
	module := filepath.Join(repo, "modules", "greeter")
	key := filepath.Join(keys, "acme.key")

	writeFile(t, filepath.Join(repo, childOutputs), "output \"name\" {\n  value = \"changed\"\n}\n")

	code, stdout, stderr := riggerRunAll(t, "module", "publish", module, "-key", key)
	if code != exitError || !strings.Contains(stderr, notCommitted) || !strings.Contains(stderr, childOutputs) {
		t.Fatalf("publish with a changed child = %d, want the refusal of %s\nstdout:\n%s\nstderr:\n%s", code, childOutputs, stdout, stderr)
	}

	// Committed, the change is published, and the registry lists the version.
	gitIn(t, repo, "commit", "--quiet", "-am", "x says changed")
	riggerRun(t, exitOK, []string{taggedGreeter}, "module", "publish", module, "-key", key)

	reg := acmeRegistry(t, repo, keys)
	riggerRun(t, exitOK, []string{listedGreeter}, "index", "update", "-dir", reg, "-today", "2026-11-18")
}

// A module that is valid only with a file that git ignores is valid on
// disk and not in its commit, which is what a registry reads: it is not
// tagged, so its version is not lost.
//
//nolint:paralleltest // publishEnv sets the environment of the publish (t.Setenv)
func TestPublishGit_CommitLacksFile(t *testing.T) {
	repo := greeterRepo(t, "1.0.0")
	keys := publishEnv(t)
	module := filepath.Join(repo, "modules", "greeter")
	key := filepath.Join(keys, "acme.key")

	writeFile(t, filepath.Join(repo, ".gitignore"), "motd.txt\n")
	writeFile(t, filepath.Join(module, "files", "motd.txt"), "welcome\n")
	writeFile(t, filepath.Join(module, "g.var.hcl"),
		"variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = var.text\n    motd = file(\"files/motd.txt\")\n  }\n}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter reads files/motd.txt")

	riggerRun(t, exitOK, []string{isValid}, "module", "validate", module)

	code, stdout, stderr := riggerRunAll(t, "module", "publish", module, "-key", key)
	if code != exitError || !strings.Contains(stderr, "as the commit") || !strings.Contains(stderr, "motd.txt") || !strings.Contains(stderr, "nothing is tagged") {
		t.Fatalf("publish of a commit without a file of the module = %d, want the diagnostics of the commit\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// With the file committed the same version is published: no tag was made.
	gitIn(t, repo, "add", "--force", "modules/greeter/files/motd.txt")
	gitIn(t, repo, "commit", "--quiet", "-m", "the file of the greeter")
	riggerRun(t, exitOK, []string{taggedGreeter}, "module", "publish", module, "-key", key)

	reg := acmeRegistry(t, repo, keys)
	riggerRun(t, exitOK, []string{listedGreeter}, "index", "update", "-dir", reg, "-today", "2026-11-18")
}

// The commit of the signature must have the files that were signed: when a
// hook changes the module in it, the signature is of another tree, and no
// tag is made.
//
//nolint:paralleltest // publishEnv sets the environment of the publish (t.Setenv)
func TestPublishGit_HookChangesSignedTree(t *testing.T) {
	repo := greeterRepo(t, "1.0.0")
	keys := publishEnv(t)
	module := filepath.Join(repo, "modules", "greeter")

	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\necho '# added by a hook' >> modules/greeter/variables.hcl\ngit add modules/greeter/variables.hcl\n")

	if err := os.Chmod(hook, 0o700); err != nil { //nolint:gosec // G302: git runs a hook only when it is executable
		t.Fatal(err)
	}

	code, stdout, stderr := riggerRunAll(t, "module", "publish", module, "-key", filepath.Join(keys, "acme.key"))
	if code != exitError || !strings.Contains(stderr, "changed more of the version than its signatures") {
		t.Fatalf("publish with a hook that changes the module = %d, want a refusal\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	if strings.Contains(stdout, "tagged") {
		t.Fatalf("the version was tagged:\n%s", stdout)
	}
}

// The lock file and the files of a published stack are signed as the
// repository has them too.
func TestPublishStack_SignsCommittedTree(t *testing.T) {
	repo := greeterRepo(t, "1.0.0")
	keys := publishEnv(t)
	key := filepath.Join(keys, "acme.key")

	riggerRun(t, exitOK, []string{taggedGreeter}, "module", "publish", filepath.Join(repo, "modules", "greeter"), "-key", key)

	reg := acmeRegistry(t, repo, keys)
	riggerRun(t, exitOK, []string{listedGreeter}, "index", "update", "-dir", reg, "-today", "2026-11-18")
	t.Setenv("RIGGER_GLOBAL_REGISTRY", reg)

	dir := filepath.Join(repo, "stacks", "greeting")
	writeFile(t, filepath.Join(dir, metadataFile), "stack {\n  name    = \"greeting\"\n  version = \"1.0.0\"\n}\n")
	writeFile(t, filepath.Join(dir, stackFile), "rigger {\n  name = \"greeting\"\n}\n\nmodule \"hello\" {\n  source  = \"acme/greeter\"\n  version = \"~> 1.0\"\n}\n")
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeting 1.0.0")
	checkedOutWithCRLF(t, repo, "stacks/greeting")

	riggerRun(t, exitOK, []string{"tagged stack greeting 1.0.0 as greeting/v1.0.0"}, "stack", "publish", dir, "-key", key)
	riggerRun(t, exitOK, []string{"listed stack acme/greeting 1.0.0"}, "index", "update", "-dir", reg, "-today", "2026-11-18")
}
