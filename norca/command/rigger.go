// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	riggercmd "github.com/bacanoo-io/norca/rigger/cli/command"

	"github.com/bacanoo-io/norca/norca/version"
)

// RiggerCommand runs the commands of Rigger, the package manager of Norca, as
// "norca rigger": the same command package as the rigger program, with the
// same hosts (rigger/cli/norcaapi), so "norca rigger stack apply" and
// "rigger stack apply" are one command (design/rig-integration.md 4.7).
// Rigger evaluates locally and sends resources to the cluster of NORCA_ADDR
// and NORCA_TOKEN; evaluation on the server comes with step 4.6.
type RiggerCommand struct {
	// Stdout and Stderr default to the standard streams; tests set them.
	Stdout, Stderr io.Writer
}

// program is Rigger as norca runs it: its version is norca's, the commit
// Rigger was built from.
func (c *RiggerCommand) program() riggercmd.Program {
	return riggercmd.Program{
		Name:    "norca rigger",
		Version: "(built into " + version.GetVersion().FullVersionNumber(true) + ")",
	}
}

// Help is the usage of Rigger. The command line library prints it for -h
// anywhere after "norca rigger", so it lists every command with its flags.
func (c *RiggerCommand) Help() string {
	var out bytes.Buffer

	riggercmd.RunProgram(context.Background(), c.program(), []string{"help"}, &out, io.Discard)

	return out.String()
}

func (c *RiggerCommand) Synopsis() string {
	return "Install modules and stacks with Rigger, the package manager"
}

func (c *RiggerCommand) Run(args []string) int {
	stdout, stderr := c.Stdout, c.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}

	if stderr == nil {
		stderr = os.Stderr
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return riggercmd.RunProgram(ctx, c.program(), args, stdout, stderr)
}
