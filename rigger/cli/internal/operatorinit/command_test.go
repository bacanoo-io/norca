// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package operatorinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/cli/command"
)

// "rigger operator init" through the dispatch of the rigger program: the
// project is written, the errors are named; "norca rigger" (a program
// without the command) does not know it.
func TestCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   func(dir string) []string
		code   int
		stdout string
		stderr string
		file   string // written, relative to the directory
	}{
		{
			name:   cmdInit,
			args:   func(dir string) []string { return []string{cmdOperator, cmdInit, "db.example.com/Widget", dir} },
			stdout: "created the operator project of db.example.com/Widget in ",
			stderr: "go.mod needs replace directives",
			file:   "module/widget-operator/controller.norca.hcl",
		},
		{
			name: "init with flags",
			args: func(dir string) []string {
				return []string{cmdOperator, cmdInit, "-name", "gadget", "-cluster-scoped", "-sdk", dir, "infra.example.com/Gadget", dir}
			},
			stdout: "module/gadget/controller.acl.hcl",
			file:   "module/gadget/metadata.hcl",
		},
		{
			name: "flags after the arguments",
			args: func(dir string) []string {
				return []string{cmdOperator, cmdInit, "infra.example.com/Gadget", dir, "-name", "sprocket", "-sdk", dir}
			},
			stdout: "module/sprocket/controller.acl.hcl",
			file:   "module/sprocket/metadata.hcl",
		},
		{
			name: "flags between the arguments",
			args: func(dir string) []string {
				return []string{cmdOperator, cmdInit, "-sdk", dir, "infra.example.com/Gadget", "-name", "cog", dir, "-force"}
			},
			stdout: "module/cog/controller.acl.hcl",
			file:   "module/cog/metadata.hcl",
		},
		{
			name:   "three arguments",
			args:   func(dir string) []string { return []string{cmdOperator, cmdInit, "a.b/X", dir, "-force", dir} },
			code:   1,
			stderr: "operator init takes one or two arguments",
		},
		{
			name:   "invalid kind",
			args:   func(string) []string { return []string{cmdOperator, cmdInit, testWidget} },
			code:   1,
			stderr: `invalid object kind "Widget": want <group>/<Kind>`,
		},
		{
			name:   "no arguments",
			args:   func(string) []string { return []string{cmdOperator, cmdInit} },
			code:   1,
			stderr: "operator init takes one or two arguments",
		},
		{
			name:   "unknown subcommand",
			args:   func(string) []string { return []string{cmdOperator, "nope"} },
			code:   1,
			stderr: `the operator command is "operator init"`,
		},
		{
			name:   "unknown flag",
			args:   func(string) []string { return []string{cmdOperator, cmdInit, "-nope", "a.b/X"} },
			code:   1,
			stderr: "flag provided but not defined: -nope",
		},
		{name: "help", args: func(string) []string { return []string{cmdOperator, cmdInit, "-h"} }, stderr: "-cluster-scoped"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			var stdout, stderr bytes.Buffer

			code := command.Run(t.Context(), tt.args(dir), &stdout, &stderr, Command())
			if code != tt.code {
				t.Fatalf("Run() = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.stdout)
			}

			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.stderr)
			}

			if tt.file != "" {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(tt.file))); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

// A flag after the kind is a flag, not the directory of the project: the
// project goes where it goes without the flag, and no directory is named
// after the flag.
func TestCommand_FlagIsNotTheDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	var stdout, stderr bytes.Buffer

	code := command.Run(t.Context(), []string{cmdOperator, cmdInit, "db.example.com/Widget", "-force"}, &stdout, &stderr, Command())
	if code != 0 {
		t.Fatalf("Run() = %d (stderr %q)", code, stderr.String())
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name() != "widget-operator" {
		t.Errorf("the project was written into %v, want widget-operator alone", entries)
	}
}

// The usage of the rigger program lists the command; that of a program
// without it ("norca rigger") neither lists nor runs it.
func TestCommand_OnlyInTheRiggerProgram(t *testing.T) {
	t.Parallel()

	var withIt, without, stderr bytes.Buffer

	command.Run(t.Context(), []string{"help"}, &withIt, &stderr, Command())

	if !strings.Contains(withIt.String(), "operator init [-name n]") {
		t.Errorf("the usage of the rigger program does not list operator init:\n%s", withIt.String())
	}

	if !strings.Contains(withIt.String(), "outputs as JSON; secret items are named, not shown\n  operator init") {
		t.Error("operator init is not after the other commands")
	}

	prog := command.Program{Name: "norca rigger", Version: "test"}
	command.RunProgram(t.Context(), prog, []string{"help"}, &without, &stderr)

	if strings.Contains(without.String(), cmdOperator) {
		t.Errorf("the usage of norca rigger lists operator init:\n%s", without.String())
	}

	stderr.Reset()

	if code := command.RunProgram(t.Context(), prog, []string{cmdOperator, cmdInit, "a.b/X"}, &without, &stderr); code != 1 || !strings.Contains(stderr.String(), `unknown command "operator"`) {
		t.Errorf("norca rigger operator init = %d (stderr %q), want 1 and unknown command", code, stderr.String())
	}
}
