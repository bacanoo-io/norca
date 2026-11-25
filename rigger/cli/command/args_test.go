// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The flags of the tests of the command line, written as a user may.
const (
	optCheck        = "-check"
	optHelp         = "--help"
	optHelpOneDash  = "-help"
	optMaxWidth     = "--max-width"
	optVersionLong  = "--version"
	optServerLong   = "--server"
	levelOfTheTests = "error"
)

// newTestRoot is the command tree of the rigger program, as a run builds it.
func newTestRoot() *cobra.Command {
	a := &app{prog: riggerProgram, stdout: io.Discard, stderr: io.Discard, logger: slog.New(slog.DiscardHandler)}

	return a.newRoot()
}

// deployerArgs is the command line the deployer runs (norca-deployer,
// riggerApplyArgs): one dash, the values of the flags as they come.
var deployerArgs = []string{
	cmdStack, cmdApply, "/srv/deploy/stack", "-f", "values/base.vars.hcl", "-f", "values/prod.vars.hcl",
	flagVar, "domain=example.com", flagVar, "offset=-2", "-prune", "-server=false",
}

// The one-dash form of a long flag is read as the two-dash one, with the
// flags of the command the arguments name; nothing else is rewritten.
func TestNormalizeArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		expected []string
	}{
		{
			name: "the deployer",
			args: deployerArgs,
			expected: []string{
				cmdStack, cmdApply, "/srv/deploy/stack", "-f", "values/base.vars.hcl", "-f", "values/prod.vars.hcl",
				"--var", "domain=example.com", "--var", "offset=-2", "--prune", "--server=false",
			},
		},
		{name: "a bool flag with a value", args: []string{cmdInstall, "-server=false", "-adopt=true"}, expected: []string{cmdInstall, "--server=false", "--adopt=true"}},
		{name: "a value after =", args: []string{cmdInstall, "-name=web"}, expected: []string{cmdInstall, "--name=web"}},
		{name: "two dashes already", args: []string{cmdInstall, "--name", riggerRelease, optServerLong}, expected: []string{cmdInstall, "--name", riggerRelease, optServerLong}},
		{name: "a flag of the program before the command", args: []string{flagLogLevel, levelOfTheTests, cmdStack, cmdPlan}, expected: []string{"--log-level", levelOfTheTests, cmdStack, cmdPlan}},
		{name: "a flag of the program with =", args: []string{"-log-format=json", cmdVersion}, expected: []string{"--log-format=json", cmdVersion}},
		{name: "a flag of the program after the command", args: []string{cmdSearch, searchText, flagLogLevel, levelOfTheTests}, expected: []string{cmdSearch, searchText, "--log-level", levelOfTheTests}},
		{name: "flags before the argument", args: []string{cmdShow, "-max-width", "0", builtinRedis}, expected: []string{cmdShow, optMaxWidth, "0", builtinRedis}},
		{name: "flags after the argument", args: []string{cmdShow, builtinRedis, "-max-width", "0", "-json"}, expected: []string{cmdShow, builtinRedis, optMaxWidth, "0", "--json"}},
		{name: "a value that starts with a dash", args: []string{cmdInstall, "x", flagName, "-check"}, expected: []string{cmdInstall, "x", "--name", "-check"}},
		{name: "a negative value", args: []string{cmdInstall, "x", flagVersion, "-1"}, expected: []string{cmdInstall, "x", optVersionLong, "-1"}},
		{name: "a var whose value has a dash", args: []string{cmdModule, cmdRender, flagVar, "x=-y"}, expected: []string{cmdModule, cmdRender, "--var", "x=-y"}},
		{name: "the value of a shorthand", args: []string{cmdInstall, "x", "-f", flagVar}, expected: []string{cmdInstall, "x", "-f", flagVar}},
		{name: "the value of a two-dash flag", args: []string{cmdInstall, "x", optVersionLong, flagName}, expected: []string{cmdInstall, "x", optVersionLong, flagName}},
		{name: "after --", args: []string{cmdFmt, "--", optCheck, flagName}, expected: []string{cmdFmt, "--", optCheck, flagName}},
		{name: "a lone dash", args: []string{cmdFmt, "-", optCheck}, expected: []string{cmdFmt, "-", "--check"}},
		{name: "a negative number", args: []string{cmdFmt, "-1"}, expected: []string{cmdFmt, "-1"}},
		{name: "an unknown flag", args: []string{cmdFmt, "-zzz"}, expected: []string{cmdFmt, "-zzz"}},
		{name: "a flag of another command", args: []string{cmdFmt, flagName, "x"}, expected: []string{cmdFmt, flagName, "x"}},
		{name: "help", args: []string{cmdStack, cmdApply, optHelpOneDash}, expected: []string{cmdStack, cmdApply, optHelp}},
		{name: "the shorthand of help", args: []string{cmdStack, cmdApply, "-h"}, expected: []string{cmdStack, cmdApply, "-h"}},
		{name: "the version of the program", args: []string{flagVersion}, expected: []string{optVersionLong}},
		{name: "the version of install", args: []string{cmdInstall, builtinRedis, flagVersion, ">= 9"}, expected: []string{cmdInstall, builtinRedis, optVersionLong, ">= 9"}},
		{
			// A word after an argument is an argument, not a command: the
			// flags are those of stack plan.
			name:     "an argument that names a command",
			args:     []string{cmdStack, cmdPlan, cmdApply, "-prune"},
			expected: []string{cmdStack, cmdPlan, cmdApply, "--prune"},
		},
		{name: "a flag the command hides", args: []string{cmdStack, cmdSync, flagServer}, expected: []string{cmdStack, cmdSync, optServerLong}},
		{name: "the output of bundle create", args: []string{cmdBundle, cmdCreate, "-o", "x.riggerbundle"}, expected: []string{cmdBundle, cmdCreate, "-o", "x.riggerbundle"}},
		{name: "nothing", args: []string{}, expected: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeArgs(newTestRoot(), tt.args)
			if !slices.Equal(got, tt.expected) {
				t.Errorf("normalizeArgs(%q) = %q, want %q", tt.args, got, tt.expected)
			}
		})
	}
}

// The command line of the deployer parses as it did with the flag package:
// the files in order, the values, -prune, and -server=false over
// RIGGER_SERVER.
func TestNormalizeArgs_DeployerParses(t *testing.T) {
	t.Setenv(serverEnv, "1")

	root := newTestRoot()
	args := normalizeArgs(root, deployerArgs)

	cmd, rest, err := root.Find(args)
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}

	flags := cmd.Flags()

	if got := flags.Args(); !slices.Equal(got, []string{"/srv/deploy/stack"}) {
		t.Errorf("arguments %q, want the directory", got)
	}

	files, _ := flags.GetStringArray(valuesFlag)
	vars, _ := flags.GetStringArray(varFlag)
	prune, _ := flags.GetBool(pruneFlag)
	server, _ := flags.GetBool(serverFlag)

	if !slices.Equal(files, []string{"values/base.vars.hcl", "values/prod.vars.hcl"}) {
		t.Errorf("files %q", files)
	}

	if !slices.Equal(vars, []string{"domain=example.com", "offset=-2"}) {
		t.Errorf("vars %q", vars)
	}

	if !prune || server {
		t.Errorf("prune %v, server %v; want true, false", prune, server)
	}
}

// An unknown flag of one dash is reported by pflag, with where the flags
// of the command are listed.
func TestRun_UnknownFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		expected string
	}{
		{name: "one dash", args: []string{cmdInstall, "x", "-zzz"}, expected: "rigger: unknown shorthand flag: 'z' in -zzz (see rigger help install)"},
		{name: "two dashes", args: []string{cmdStack, cmdPlan, "--zzz"}, expected: "rigger: unknown flag: --zzz (see rigger help stack plan)"},
		{name: "a missing value", args: []string{cmdInstall, "x", flagName}, expected: "rigger: flag needs an argument: --name (see rigger help install)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder

			if code := Run(t.Context(), tt.args, &stdout, &stderr); code != exitError {
				t.Errorf("Run() = %d, want %d", code, exitError)
			}

			if got := strings.TrimSpace(stderr.String()); got != tt.expected {
				t.Errorf("stderr = %q, want %q", got, tt.expected)
			}
		})
	}
}
