// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// greeterRepo is a git repository of a registry layout with the module
// greeter at version, committed, not tagged.
func greeterRepo(t *testing.T, version string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	repo := writeTree(t, map[string]string{
		"modules/greeter/metadata.hcl": "module {\n  name        = \"greeter\"\n  version     = \"" + version + "\"\n" +
			"  description = \"Says hello\"\n  category    = \"application\"\n  keywords    = [\"greeting\"]\n  license     = \"MPL-2.0\"\n}\n",
		"modules/greeter/variables.hcl": "variable \"text\" {\n  default = \"hello\"\n}\n",
		"modules/greeter/g.var.hcl":     "variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = var.text\n  }\n}\n",
	})

	gitIn(t, repo, "init", "--quiet", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeter "+version)

	// With slashes: the tests write it into JSON, where \ starts an escape.
	return filepath.ToSlash(repo)
}

// setVersion commits a new version of the greeter.
func setVersion(t *testing.T, repo, version string) {
	t.Helper()

	meta := filepath.Join(repo, "modules", "greeter", metadataFile)
	if err := os.WriteFile(meta, []byte("module {\n  name    = \"greeter\"\n  version = \""+version+"\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gitIn(t, repo, "commit", "--quiet", "-am", "greeter "+version)
}

// riggerRunAll runs rigger and returns its exit code, standard output and error.
func riggerRunAll(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), append([]string{flagLogLevel, "warn"}, args...), &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

func writeFile(t *testing.T, p, data string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// publishedRegistry publishes the greeter 1.0.0 signed and 1.1.0 unsigned,
// lists them in a global registry of the namespace acme, which it
// commits, and returns the repository, the registry and the entry.
func publishedRegistry(t *testing.T) (repo, reg, entry, keys string) {
	t.Helper()

	repo = greeterRepo(t, "1.0.0")
	keys = publishEnv(t)

	module := filepath.Join(repo, "modules", "greeter")

	// Publishing signs, commits the signature and tags; a tag is never
	// made twice.
	riggerRun(t, exitOK, []string{"signed greeter 1.0.0", taggedGreeter}, "module", "publish", module, "-key", filepath.Join(keys, "acme.key"))
	riggerRunErr(t, "is tagged already", "module", "publish", module, "-key", filepath.Join(keys, "acme.key"))

	// 1.1.0 is tagged without a signature.
	setVersion(t, repo, "1.1.0")
	riggerRun(t, exitOK, []string{"as greeter/v1.1.0"}, "module", "publish", module)

	reg = acmeRegistry(t, repo, keys)

	out := riggerRun(t, exitOK, []string{listedGreeter, "acme/greeter 1.1.0 (greeter/v1.1.0): not signed by a key of the namespace"},
		"index", "update", "-dir", reg, "-today", "2026-11-18")

	entry = filepath.Join(reg, "modules", "a", "acme", "greeter.json")

	data, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	for _, want := range []string{
		`"version": "1.0.0"`, `"tag": "greeter/v1.0.0"`, `"path": "modules/greeter"`, `"key_id": "sha256:`, `"listed": "2026-11-18"`,
		`"description": "Says hello"`, `"category": "application"`, `"greeting"`, `"license": "MPL-2.0"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("entry does not hold %s:\n%s", want, data)
		}
	}

	riggerRun(t, exitOK, []string{isValid}, "index", "check", "-dir", reg)

	gitIn(t, reg, "init", "--quiet", "-b", "main")
	gitIn(t, reg, "add", "-A")
	gitIn(t, reg, "commit", "--quiet", "-m", "acme/greeter 1.0.0")

	return repo, reg, entry, keys
}

func TestRun_GlobalRegistry(t *testing.T) {
	repo, reg, entry, keys := publishedRegistry(t)

	// A stack takes acme/greeter from the global registry.
	t.Setenv("RIGGER_GLOBAL_REGISTRY", reg)

	stack := writeTree(t, map[string]string{stackFile: "rigger {\n  name = \"greet\"\n}\n\nmodule \"hello\" {\n  source  = \"acme/greeter\"\n  version = \"~> 1.0\"\n}\n"})
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", stack)

	lock, _ := os.ReadFile(filepath.Join(stack, lockFile))
	for _, want := range []string{`module "acme/greeter"`, `version    = "1.0.0"`, `repository = "` + repo + `"`, `signers`} {
		if !strings.Contains(string(lock), want) {
			t.Fatalf("lock does not hold %s:\n%s", want, lock)
		}
	}

	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", stack)

	publishedStack(t, repo, reg, keys)
	advisories(t, reg, stack)
	tampered(t, reg, entry, stack)
}

// publishedStack publishes a stack over acme/greeter in the repository,
// lists it, and installs it from the registry.
func publishedStack(t *testing.T, repo, reg, keys string) {
	t.Helper()

	dir := filepath.Join(repo, "stacks", "greeting")
	writeFile(t, filepath.Join(dir, metadataFile), "stack {\n  name    = \"greeting\"\n  version = \"1.0.0\"\n}\n")
	writeFile(t, filepath.Join(dir, stackFile), "rigger {\n  name = \"greeting\"\n}\n\nmodule \"hello\" {\n  source  = \"acme/greeter\"\n  version = \"~> 1.0\"\n}\n")

	// Publishing needs the lock file.
	riggerRunErr(t, "acme/greeter has no entry in rigger.lock.hcl", "stack", "publish", dir, "-key", filepath.Join(keys, "acme.key"))
	riggerRun(t, exitOK, []string{wroteLock}, "stack", "sync", dir)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "--quiet", "-m", "greeting 1.0.0")
	riggerRun(t, exitOK, []string{"tagged stack greeting 1.0.0 as greeting/v1.0.0"}, "stack", "publish", dir, "-key", filepath.Join(keys, "acme.key"))

	riggerRun(t, exitOK, []string{"listed stack acme/greeting 1.0.0"}, "index", "update", "-dir", reg, "-today", "2026-11-18")

	data, err := os.ReadFile(filepath.Join(reg, "stacks", "a", "acme", "greeting.json"))
	if err != nil || !strings.Contains(string(data), `"source": "acme/greeter"`) || !strings.Contains(string(data), `"path": "stacks/greeting"`) {
		t.Fatalf("stack entry = %s, %v", data, err)
	}

	riggerRun(t, exitOK, []string{isValid}, "index", "check", "-dir", reg, "-base", "HEAD")
	riggerRun(t, exitOK, []string{"stack greeting", isValid}, "stack", "validate", "acme/greeting", "-version", "~> 1.0")
	riggerRunErr(t, "no version meets", "stack", "validate", "acme/greeting", "-version", "~> 2.0")

	gitIn(t, reg, "add", "-A")
	gitIn(t, reg, "commit", "--quiet", "-m", "acme/greeting 1.0.0")
}

// advisories checks that a warning is shown and a block refuses.
func advisories(t *testing.T, reg, stack string) {
	t.Helper()

	// A warning is shown by sync; a block refuses the version.
	writeFile(t, filepath.Join(reg, "advisories.json"), `{"advisories": [{"kind": "module", "namespace": "acme", "name": "greeter", "version": "1.0.0", "level": "warning", "reason": "greets too loudly"}]}`+"\n")

	if code, _, stderr := riggerRunAll(t, "stack", "sync", stack); code != exitOK || !strings.Contains(stderr, "greets too loudly") {
		t.Fatalf("sync with a warning = %d:\n%s", code, stderr)
	}

	writeFile(t, filepath.Join(reg, "advisories.json"), `{"advisories": [{"kind": "module", "namespace": "acme", "name": "greeter", "version": "1.0.0", "level": "blocked", "reason": "exposed a secret", "decision": "https://example.org/issues/1"}]}`+"\n")
	riggerRunErr(t, "blocked by the global registry: exposed a secret", "stack", "validate", stack)

	// An advisory is never taken back.
	if code, out, _ := riggerRunAll(t, "index", "check", "-dir", reg, "-base", "HEAD"); code != exitOK {
		t.Fatalf("check after adding an advisory = %d:\n%s", code, out)
	}

	if err := os.Remove(filepath.Join(reg, "advisories.json")); err != nil {
		t.Fatal(err)
	}
}

// tampered checks that content other than the entry lists is refused, and
// that a changed entry fails the check.
func tampered(t *testing.T, reg, entry, stack string) {
	t.Helper()

	// Content other than the entry lists is refused, and a listed version
	// is never changed.
	src, _ := os.ReadFile(entry)
	at := strings.Index(string(src), `"digest": "sha256:`) + len(`"digest": "sha256:`)
	other := byte('0')

	if src[at] == other {
		other = '1'
	}

	writeFile(t, entry, string(src[:at])+string(other)+string(src[at+1:]))
	riggerRunErr(t, "is not what the index lists", "stack", "sync", "-upgrade", stack)

	code, out, _ := riggerRunAll(t, "index", "check", "-dir", reg, "-base", "HEAD")
	if code == exitOK || !strings.Contains(out, "is never changed or removed (changed)") {
		t.Fatalf("check of a changed version = %d:\n%s", code, out)
	}
}
