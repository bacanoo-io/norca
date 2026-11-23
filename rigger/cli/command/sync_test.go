// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wroteLock is what rigger stack sync says when it writes the lock file.
const wroteLock = "wrote"

// gitIn runs git in dir for a test.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.org"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// registryRepo makes a git registry with the module greeter at 1.0.0 and
// 1.1.0, and returns its directory.
func registryRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	repo := writeTree(t, map[string]string{
		"modules/greeter/metadata.hcl":  "module {\n  name    = \"greeter\"\n  version = \"1.0.0\"\n}\n",
		"modules/greeter/variables.hcl": "variable \"text\" {\n  default = \"hello\"\n}\n",
		"modules/greeter/g.var.hcl":     "variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = var.text\n  }\n}\n",
		"modules/greeter/outputs.hcl":   "output \"path\" {\n  value = \"greeter/${rigger.release}\"\n}\n",
	})

	gitIn(t, repo, "init", "--quiet", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter 1.0.0")
	gitIn(t, repo, "tag", "greeter/v1.0.0")

	meta := filepath.Join(repo, "modules/greeter/metadata.hcl")
	if err := os.WriteFile(meta, []byte("module {\n  name    = \"greeter\"\n  version = \"1.1.0\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gitIn(t, repo, "commit", "--quiet", "-am", "greeter 1.1.0")
	gitIn(t, repo, "tag", "-a", "-m", "1.1.0", "greeter/v1.1.0")

	// With slashes: the tests write it into HCL, where \ starts an escape.
	return filepath.ToSlash(repo)
}

func TestRun_StackSync(t *testing.T) {
	repo := registryRepo(t)
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	dir := writeTree(t, map[string]string{
		stackFile: `rigger {
  name = "greet"

  registry "local" {
    source = "file://` + repo + `"
  }
}

module "hello" {
  source  = "local/greeter"
  version = "~> 1.0.0"
}

module "redis" {
  source = "builtin/redis"
}
`,
	})

	out := riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	lock, err := os.ReadFile(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`module "local/greeter"`, `version    = "1.0.0"`, `ref        = "greeter/v1.0.0"`, `module "builtin/redis"`, `version = "0.3.0"`, `digest`} {
		if !strings.Contains(string(lock), want) {
			t.Fatalf("lock file does not hold %q:\n%s\n%s", want, lock, out)
		}
	}

	riggerRun(t, exitOK, []string{"is up to date"}, "stack", "sync", dir)
	riggerRun(t, exitOK, []string{"stack greet"}, "stack", "validate", dir)

	// -upgrade takes no newer version outside ~> 1.0.0; with ~> 1.0 it does.
	stack := filepath.Join(dir, stackFile)
	src, _ := os.ReadFile(stack)

	if err := os.WriteFile(stack, []byte(strings.Replace(string(src), "~> 1.0.0", "~> 1.0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	if lock, _ = os.ReadFile(filepath.Join(dir, lockFile)); !strings.Contains(string(lock), `"greeter/v1.0.0"`) || !strings.Contains(string(lock), `"~> 1.0"`) {
		t.Fatalf("lock after a new constraint keeps the version it has:\n%s", lock)
	}

	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir, "-upgrade")

	lock, _ = os.ReadFile(filepath.Join(dir, lockFile))
	if !strings.Contains(string(lock), `"greeter/v1.1.0"`) {
		t.Fatalf("lock after -upgrade:\n%s", lock)
	}

	// A tag of another version than the metadata is refused.
	gitIn(t, repo, "tag", "greeter/v1.2.0")
	riggerRunErr(t, "the tag greeter/v1.2.0 is version 1.2.0, the metadata of the module says \"1.1.0\"", "stack", "sync", dir, "-upgrade")
	gitIn(t, repo, "tag", "-d", "greeter/v1.2.0")

	// A tag that moves is refused.
	gitIn(t, repo, "commit", "--quiet", "--allow-empty", "-m", "moved")
	gitIn(t, repo, "tag", "-f", "-a", "-m", "moved", "greeter/v1.1.0")
	riggerRunErr(t, "the tag points to another commit", "stack", "validate", dir)

	// Content that is not what the lock records is refused.
	redis := strings.Index(string(lock), `module "builtin/redis"`)
	tampered := string(lock[:redis]) + strings.Replace(string(lock[redis:]), "\"sha256:", "\"sha256:00", 1)

	if err := os.WriteFile(filepath.Join(dir, lockFile), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	riggerRunErr(t, "is not what the lock file records", "stack", "validate", dir)
}

// riggerRunErr runs rigger, which must fail with a message.
func riggerRunErr(t *testing.T, want string, args ...string) {
	t.Helper()

	var stdout, stderr strings.Builder

	if code := Run(t.Context(), append([]string{flagLogLevel, "warn"}, args...), &stdout, &stderr); code != exitError {
		t.Fatalf("rigger %s = %d, want an error", strings.Join(args, " "), code)
	}

	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("rigger %s: stderr does not contain %q:\n%s", strings.Join(args, " "), want, stderr.String())
	}
}

// A lock file that changes is written beside the old one and takes its
// place: a sync that is cut leaves the lock that was there, not half of the
// new one, which nothing could read.
func TestRun_StackSync_ReplacesTheLock(t *testing.T) {
	repo := registryRepo(t)
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	stack := func(version string) string {
		return "rigger {\n  name = \"greet\"\n\n  registry \"local\" {\n    source = \"file://" + repo + "\"\n  }\n}\n\n" +
			"module \"hello\" {\n  source  = \"local/greeter\"\n  version = \"" + version + "\"\n}\n"
	}

	dir := writeTree(t, map[string]string{stackFile: stack("1.0.0")})
	lock := filepath.Join(dir, lockFile)

	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	isReplaced := replaced(t, lock)

	writeFile(t, filepath.Join(dir, stackFile), stack("1.1.0"))
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)

	if !isReplaced() {
		t.Error("the lock file was written in place")
	}

	if data, err := os.ReadFile(lock); err != nil || !strings.Contains(string(data), `"greeter/v1.1.0"`) {
		t.Errorf("the lock file = %q, %v", data, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Errorf("the directory of the stack has %v (%v), want the stack and its lock alone", entries, err)
	}
}
