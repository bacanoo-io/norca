// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package packcmd

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
)

// isolateCache points the Nomad Pack cache, which every command except
// "version" creates in the user cache directory, into a temporary directory.
func isolateCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir) // Linux and other Unix systems
	t.Setenv("LocalAppData", dir)   // Windows
	t.Setenv("HOME", dir)           // macOS
}

// run runs args through a CLI that has only the Nomad Pack commands, like the
// Nomad CLI does.
func run(t *testing.T, args ...string) int {
	t.Helper()
	c := &cli.CLI{
		Name:     "nomad",
		Args:     args,
		Commands: Commands(),
	}
	code, err := c.Run()
	must.NoError(t, err)
	return code
}

func TestPackCmd_Commands(t *testing.T) {
	commands := Commands()

	for _, name := range []string{
		"pack", "pack run", "pack plan", "pack render", "pack destroy", "pack stop",
		"pack status", "pack list", "pack info", "pack fmt", "pack version",
		"pack registry", "pack registry add", "pack registry delete",
		"pack registry list", "pack registry update",
		"pack generate", "pack generate pack", "pack generate registry",
		"pack generate var-file", "pack deps", "pack deps vendor",
	} {
		factory, ok := commands[name]
		must.True(t, ok, must.Sprintf("missing command %q", name))
		cmd, err := factory()
		must.NoError(t, err)
		must.NotEq(t, "", cmd.Synopsis(), must.Sprintf("synopsis of %q", name))
		must.NotEq(t, "", cmd.Help(), must.Sprintf("help of %q", name))
	}

	// The documentation generator is not exposed.
	_, ok := commands["pack gen-cli-docs"]
	must.False(t, ok)

	for name := range commands {
		must.True(t, name == Prefix || strings.HasPrefix(name, Prefix+" "), must.Sprintf("command %q", name))
	}
}

func TestPackCmd_Help(t *testing.T) {
	cmd, err := Commands()["pack render"]()
	must.NoError(t, err)
	must.StrContains(t, cmd.Help(), "nomad pack render <pack-name> [options]")
	must.StrNotContains(t, cmd.Help(), "nomad-pack")

	cmd, err = Commands()["pack"]()
	must.NoError(t, err)
	must.StrContains(t, cmd.Help(), "Usage: nomad pack <subcommand>")
	must.Eq(t, cli.RunResultHelp, cmd.Run(nil))
}

func TestPackCmd_Version(t *testing.T) {
	must.Zero(t, run(t, "pack", "version"))
	must.Zero(t, run(t, "pack", "--version"))
}

func TestPackCmd_Render(t *testing.T) {
	isolateCache(t)
	outDir := t.TempDir()

	code := run(t, "pack", "render",
		testfixture.AbsPath(t, "v2/simple_raw_exec_v2"),
		"--to-dir", outDir, "--auto-approve")
	must.Zero(t, code)

	var rendered []string
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rendered = append(rendered, filepath.Base(path))
		}
		return err
	})
	must.NoError(t, err)
	must.SliceContains(t, rendered, "simple_raw_exec.nomad")
}

func TestPackCmd_UnknownFlag(t *testing.T) {
	isolateCache(t)
	must.One(t, run(t, "pack", "render", "--no-such-flag", "."))
}
