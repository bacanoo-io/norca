// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/version"
)

func TestRiggerCommand_Run(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
	}{
		{
			name:   "version",
			args:   []string{"version"},
			stdout: "rigger (built into Norca v" + version.NorcaVersion + ")\n",
		},
		{
			// A module of the examples of Rigger renders without a cluster.
			name:   "module render",
			args:   []string{"module", "render", "../../rigger/examples/module/web"},
			stdout: `"kind": "job",`,
		},
		{name: "unknown command", args: []string{"nope"}, code: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			cmd := &RiggerCommand{Stdout: &stdout, Stderr: &stderr}

			if code := cmd.Run(tt.args); code != tt.code {
				t.Fatalf("Run() = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.stdout)
			}
		})
	}
}

func TestRiggerCommand_Help(t *testing.T) {
	ci.Parallel(t)

	help := (&RiggerCommand{}).Help()
	for _, want := range []string{
		"Norca Rigger (built into Norca v" + version.NorcaVersion + ")\nThe package manager of Norca",
		"Usage:\n  norca rigger [flags]\n  norca rigger [command]",
		"  stack       Work with a stack: sync, validate, plan, apply,",
		"  module      Work with a module: render,",
		`Use "norca rigger help <command>" for more information about a command.`,
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not contain %q:\n%s", want, help)
		}
	}
}
