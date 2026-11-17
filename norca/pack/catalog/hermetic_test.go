// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// writeHermeticPack writes a pack whose job carries the result of a template
// expression in its meta.
func writeHermeticPack(t *testing.T, dir, expr string) {
	t.Helper()
	files := map[string]string{
		"metadata.hcl":   "app {\n  url = \"https://example.com\"\n}\n\npack {\n  name    = \"herm\"\n  version = \"0.1.0\"\n}\n",
		"variables.hcl":  "variable \"unused\" {\n  type    = string\n  default = \"\"\n}\n",
		"files/note.txt": "from-the-pack",
		"templates/herm.nomad.tpl": `job "herm" {
  meta {
    got = [[ ` + expr + ` | quote ]]
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "echo"
      }
    }
  }
}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

func renderHermeticPack(t *testing.T, expr string) (string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "herm")
	writeHermeticPack(t, dir, expr)
	// A file of the server next to the pack.
	must.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(dir), "sibling.txt"), []byte("server-file"), 0o644))
	out, err := RenderPack(RenderInput{
		PackDir:      dir,
		RegistryName: "test",
		PackName:     "herm",
		ReleaseName:  "herm",
	})
	if err != nil {
		return "", err
	}
	for _, r := range out.Resources {
		if r.Job != nil {
			return r.Job.Meta["got"], nil
		}
	}
	t.Fatal("the render has no job")
	return "", nil
}

// The server renders a pack its caller chose (review finding SRV-6): the
// templates must not read the environment of the server process nor the
// files of the server outside the pack.
func TestRenderPack_Hermetic(t *testing.T) {
	// No ci.Parallel: the test sets an environment variable.
	t.Setenv("NORCA_TEST_SERVER_SECRET", "s3cr3t")

	outside := filepath.Join(t.TempDir(), "outside.txt")
	must.NoError(t, os.WriteFile(outside, []byte("server-file"), 0o644))

	denied := map[string]string{
		"env":                     `env "NORCA_TEST_SERVER_SECRET"`,
		"expandenv":               `expandenv "$NORCA_TEST_SERVER_SECRET"`,
		"getHostByName":           `getHostByName "localhost"`,
		"fileContents absolute":   `fileContents ` + "`" + outside + "`",
		"fileRelative dotdot":     `fileRelative "../sibling.txt" .`,
		"fileContents via parent": `fileContents (printf "%s/../sibling.txt" packPath)`,
	}
	for name, expr := range denied {
		t.Run(name, func(t *testing.T) {
			got, err := renderHermeticPack(t, expr)
			must.Error(t, err, must.Sprintf("the render gave %q", got))
		})
	}

	allowed := map[string]string{
		"fileContents of the pack": `fileContents (printf "%s/files/note.txt" packPath)`,
		"fileRelative of the pack": `fileRelative "files/note.txt" .`,
	}
	for name, expr := range allowed {
		t.Run(name, func(t *testing.T) {
			got, err := renderHermeticPack(t, expr)
			must.NoError(t, err)
			must.Eq(t, "from-the-pack", got)
		})
	}

	t.Run("symlink out of the pack", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "herm")
		writeHermeticPack(t, dir, `fileRelative "files/link.txt" .`)
		if err := os.Symlink(outside, filepath.Join(dir, "files", "link.txt")); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		_, err := RenderPack(RenderInput{PackDir: dir, RegistryName: "test", PackName: "herm", ReleaseName: "herm"})
		must.Error(t, err)
	})
}
