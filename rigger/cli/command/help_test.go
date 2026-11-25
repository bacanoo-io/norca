// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runProgram runs prog with args and returns its exit code, standard
// output and standard error.
func runProgram(t *testing.T, prog Program, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := RunProgram(t.Context(), prog, args, &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

// The rigger program and norca rigger, as norca names itself.
var (
	riggerProgram = Program{Name: "rigger", Version: "v9.9.9", Completion: true}
	norcaProgram  = Program{Name: "norca rigger", Version: "(built into Norca v1.2.3)", Build: Build{Commit: "abc123", Date: commitDate}}
)

// The values of the tests of the help.
const (
	commitDate  = "2026-11-25T10:00:00Z"
	riggerUsage = "Usage:\n  rigger [flags]"
	searchText  = "redis"
)

// The help of the program starts with a header, Norca Rigger and the
// version, and what Rigger is; it lists the commands with what they do, and
// names the help of a command. The program is named as it is run.
func TestRun_RootHelp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prog     Program
		args     []string
		expected []string
	}{
		{
			name: "help", prog: riggerProgram, args: []string{cmdHelp},
			expected: []string{
				"Norca Rigger v9.9.9\nThe package manager of Norca: ",
				"Usage:\n  rigger [flags]\n  rigger [command]",
				"  stack       Work with a stack: sync, validate, plan, apply, status, destroy, publish",
				"  completion  ",
				"rigger help environment",
				`Use "rigger help <command>" for more information about a command.`,
			},
		},
		{name: optHelp, prog: riggerProgram, args: []string{optHelp}, expected: []string{"Norca Rigger v9.9.9\n", riggerUsage}},
		{name: "-h", prog: riggerProgram, args: []string{"-h"}, expected: []string{"Norca Rigger v9.9.9\n", riggerUsage}},
		{name: optHelpOneDash, prog: riggerProgram, args: []string{optHelpOneDash}, expected: []string{"Norca Rigger v9.9.9\n", riggerUsage}},
		{
			name: "built into norca", prog: norcaProgram, args: []string{cmdHelp},
			expected: []string{
				"Norca Rigger (built into Norca v1.2.3)\nThe package manager of Norca: ",
				"Usage:\n  norca rigger [flags]\n  norca rigger [command]",
				"norca rigger help environment",
				`Use "norca rigger help <command>" for more information about a command.`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runProgram(t, tt.prog, tt.args...)
			if code != exitOK || stderr != "" {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}

			if !strings.HasPrefix(stdout, "Norca Rigger ") {
				t.Errorf("the help does not start with the header:\n%s", stdout)
			}

			for _, want := range tt.expected {
				if !strings.Contains(stdout, want) {
					t.Errorf("the help does not contain %q:\n%s", want, stdout)
				}
			}
		})
	}
}

// Every command has a help of its own: what it does in a line and in full,
// and examples of the commands that run.
func TestCommands_HaveHelp(t *testing.T) {
	t.Parallel()

	var walk func(c *cobra.Command)

	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}

		// cobra's help and completion are not Rigger's.
		path := c.CommandPath()
		if path == "rigger help" || strings.HasPrefix(path, "rigger completion") || strings.HasPrefix(c.Name(), "__") {
			return
		}

		if c.Use == "" || c.Short == "" || c.Long == "" {
			t.Errorf("%s: Use %q, Short %q, Long %q", path, c.Use, c.Short, c.Long)
		}

		if c.HasParent() && !c.HasSubCommands() && c.Runnable() && c.Example == "" {
			t.Errorf("%s has no example", path)
		}
	}

	walk(newTestRoot())
}

// rigger help <command>, <command> --help, -h and -help show the help of
// the command: its usage, what it does, its examples and the flags it
// shows, without the header of the program.
func TestRun_CommandHelp(t *testing.T) {
	t.Parallel()

	want := []string{
		"Plan a stack and apply the plan, release after release.",
		"Usage:\n  rigger stack apply [dir|source] [flags]",
		"Examples:\n  rigger stack apply --values values/prod.vars.hcl",
		"  -f, --values file ",
		"      --var name=value ",
		"      --prune[=true|false] ",
		"Global Flags:\n      --log-format text",
	}

	var first string

	for _, args := range [][]string{
		{cmdHelp, cmdStack, cmdApply},
		{cmdStack, cmdApply, optHelp},
		{cmdStack, cmdApply, "-h"},
		{cmdStack, cmdApply, optHelpOneDash},
		{cmdStack, cmdApply, "x", "-f", "y", optHelp},
	} {
		stdout := commandHelp(t, args, want)

		if first == "" {
			first = stdout
		} else if stdout != first {
			t.Errorf("%q writes another help:\n%s", args, stdout)
		}
	}
}

// commandHelp runs args, which show the help of a command, checks that it
// holds each of want and not the header of the program, and returns it.
func commandHelp(t *testing.T, args, want []string) string {
	t.Helper()

	code, stdout, stderr := runProgram(t, riggerProgram, args...)
	if code != exitOK || stderr != "" {
		t.Fatalf("%q: exit code %d, stderr %q", args, code, stderr)
	}

	for _, w := range want {
		if !strings.Contains(stdout, w) {
			t.Errorf("%q: the help does not contain %q:\n%s", args, w, stdout)
		}
	}

	if strings.HasPrefix(stdout, "Norca Rigger") {
		t.Errorf("%q: the help of a command repeats the header", args)
	}

	return stdout
}

// A command of a group parses the flags it took before for the other
// commands of the group, and hides them; the topic environment names the
// variables the commands read.
func TestRun_HiddenFlagsAndEnvironment(t *testing.T) {
	t.Parallel()

	_, sync, _ := runProgram(t, riggerProgram, cmdHelp, cmdStack, cmdSync)
	if !strings.Contains(sync, "--upgrade") || strings.Contains(sync, "--prune") || strings.Contains(sync, "--server") {
		t.Errorf("the help of stack sync shows the flags of another command, or not its own:\n%s", sync)
	}

	_, env, _ := runProgram(t, riggerProgram, cmdHelp, "environment")
	for _, w := range []string{"RIGGER_SERVER", "RIGGER_GLOBAL_REGISTRY", "RIGGER_VAR_<name>", "RIGGER_CACHE_DIR", "RIGGER_KEY_PASSWORD", "NORCA_ADDR"} {
		if !strings.Contains(env, w) {
			t.Errorf("help environment does not name %s:\n%s", w, env)
		}
	}
}

// A command of a group without a command, or with one the group does not
// hold, fails and names the commands, as before the commands had help.
func TestRun_GroupNeedsACommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args     []string
		expected string
	}{
		{args: []string{cmdStack}, expected: "rigger: stack needs a command: sync, validate, plan, apply, status, destroy or publish\n"},
		{args: []string{cmdStack, "aply"}, expected: "rigger: unknown stack command \"aply\"\n"},
		{args: []string{cmdRelease}, expected: "rigger: release needs a command: list, status, history, rollback or uninstall\n"},
		{args: []string{cmdKey}, expected: "rigger: key needs a command: generate, sign or verify\n"},
		{args: []string{cmdBundle}, expected: "rigger: bundle needs a command: create, inspect or import\n"},
		{args: []string{cmdOperation}, expected: "rigger: operation needs a command: follow, cancel or status\n"},
		{args: []string{cmdIndex}, expected: "rigger: index needs a command: update or check\n"},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runProgram(t, riggerProgram, tt.args...)
			if code != exitError || stdout != "" || stderr != tt.expected {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, nothing, %q", code, stdout, stderr, exitError, tt.expected)
			}
		})
	}
}

// Flags may come before or after the arguments of a command.
func TestRun_FlagsAnywhere(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	for _, args := range [][]string{
		{cmdSearch, searchText, "-registry", builtinRegistry},
		{cmdSearch, "--registry", builtinRegistry, searchText},
		{cmdSearch, "-registry=builtin", searchText, optMaxWidth, "0"},
	} {
		code, stdout, stderr := runProgram(t, riggerProgram, args...)
		if code != exitOK || !strings.Contains(stdout, builtinRedis) {
			t.Errorf("%q: exit code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}

	for _, args := range [][]string{
		{cmdShow, "-max-width", "0", builtinRedis},
		{cmdShow, builtinRedis, optMaxWidth, "0"},
	} {
		code, stdout, stderr := runProgram(t, riggerProgram, args...)
		if code != exitOK || !strings.Contains(stdout, "module builtin/redis") {
			t.Errorf("%q: exit code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}

	dir := writeTree(t, map[string]string{"a.hcl": "a=1\n"})
	unformatted := filepath.Join(dir, "a.hcl")

	for _, args := range [][]string{
		{cmdFmt, optCheck, dir},
		{cmdFmt, dir, optCheck},
		{cmdFmt, dir, "--check"},
	} {
		code, stdout, _ := runProgram(t, riggerProgram, args...)
		if code != exitError || !strings.Contains(stdout, unformatted) {
			t.Errorf("%q: exit code %d, stdout %q; want %d and the file", args, code, stdout, exitError)
		}
	}

	if src, err := os.ReadFile(unformatted); err != nil || string(src) != "a=1\n" {
		t.Errorf("fmt --check wrote the file: %q, %v", src, err)
	}
}

// builtinRegistry is the registry of the modules built into the program,
// as rigger search names it.
const builtinRegistry = searchBuiltin

// rigger completion writes the script of each shell; the program completes
// its commands and the values of the flags that take one of a fixed set.
// norca rigger has no completion: norca completes its own command line.
func TestRun_Completion(t *testing.T) {
	t.Parallel()

	scripts := []struct{ shell, expected string }{
		{"bash", "# bash completion V2 for rigger"},
		{"zsh", "#compdef rigger"},
		{"fish", "# fish completion for rigger"},
		{"powershell", "# powershell completion for rigger"},
	}

	for _, s := range scripts {
		code, stdout, stderr := runProgram(t, riggerProgram, "completion", s.shell)
		if code != exitOK || !strings.Contains(stdout, s.expected) {
			t.Errorf("completion %s: exit code %d, stderr %q, stdout starts %q", s.shell, code, stderr, firstLine(stdout))
		}
	}

	tests := []struct {
		name     string
		args     []string
		expected []string
	}{
		{name: "commands", args: []string{""}, expected: []string{"stack\t", "install\t", "search\t", "completion\t"}},
		{name: "stack commands", args: []string{cmdStack, ""}, expected: []string{"sync\t", "apply\t", "publish\t"}},
		{name: "kind", args: []string{cmdSearch, "--kind", ""}, expected: []string{"module\n", "stack\n", ":4\n"}},
		{name: "kind of one dash", args: []string{cmdShow, "x", "-kind", ""}, expected: []string{"module\n", "stack\n"}},
		{name: "registry", args: []string{cmdSearch, "--registry", ""}, expected: []string{"global\n", "builtin\n"}},
		{name: "category", args: []string{cmdRegistry, cmdRegistrySearch, "global", "--category", ""}, expected: []string{"database\n"}},
		{name: "log level", args: []string{"--log-level", ""}, expected: []string{"debug\n", "error\n"}},
		{name: "log format", args: []string{cmdInstall, "--log-format", ""}, expected: []string{"text\n", "json\n"}},
		{name: "select", args: []string{cmdRegistry, cmdRegistrySelect, "global", ""}, expected: []string{"module\n", "stack\n"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runProgram(t, riggerProgram, append([]string{cobra.ShellCompRequestCmd}, tt.args...)...)
			if code != exitOK {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}

			for _, want := range tt.expected {
				if !strings.Contains(stdout, want) {
					t.Errorf("the completion does not contain %q:\n%s", want, stdout)
				}
			}
		})
	}

	if code, _, stderr := runProgram(t, norcaProgram, "completion", "bash"); code != exitError || !strings.Contains(stderr, `unknown command "completion"`) {
		t.Errorf("norca rigger completion: exit code %d, stderr %q", code, stderr)
	}
}

// firstLine is the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")

	return line
}
