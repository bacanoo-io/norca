// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// A git registry synced by the server goes through the pack cache, which
// logs while cloning; the server-side cache had no logger and the clone
// panicked with a nil pointer dereference.
func TestSyncSource_GitRegistry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	pack := filepath.Join(repo, "packs", "hello")
	must.NoError(t, os.MkdirAll(filepath.Join(pack, "templates"), 0o755))
	for name, content := range map[string]string{
		"metadata.hcl":              "app {\n  url = \"https://example.com\"\n}\n\npack {\n  name    = \"hello\"\n  version = \"0.1.0\"\n}\n",
		"variables.hcl":             "variable \"greeting\" {\n  type    = string\n  default = \"hi\"\n}\n",
		"templates/hello.nomad.tpl": "job \"hello\" {\n  group \"g\" {\n    task \"t\" {\n      driver = \"raw_exec\"\n      config {\n        command = \"echo\"\n      }\n    }\n  }\n}\n",
	} {
		must.NoError(t, os.WriteFile(filepath.Join(pack, name), []byte(content), 0o644))
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		must.NoError(t, err, must.Sprint(string(out)))
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "packs")
	git("tag", "v0.1.0")

	cacheDir := t.TempDir()
	must.NoError(t, SyncSource(cacheDir, "local", "git", repo, "v0.1.0", nil, nil, nil))
	_, err := os.Stat(filepath.Join(cacheDir, "local"))
	must.NoError(t, err)
}
