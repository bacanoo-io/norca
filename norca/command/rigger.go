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
	"time"

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

// program is Rigger as norca runs it: its version and build are norca's,
// the commit Rigger was built from. norca completes its own command line,
// so Rigger's completion command is left out.
func (c *RiggerCommand) program() riggercmd.Program {
	v := version.GetVersion()

	build := riggercmd.Build{Commit: v.Revision}
	if !v.BuildDate.IsZero() {
		build.Date = v.BuildDate.Format(time.RFC3339)
	}

	return riggercmd.Program{
		Name:    "norca rigger",
		Version: "(built into Norca v" + version.NorcaVersion + ")",
		Build:   build,
	}
}

// Help is the help of Rigger: its commands, and "norca rigger help
// <command>" for the help of one. The command line library prints it for
// -h and --help anywhere after "norca rigger", which Rigger never sees.
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
