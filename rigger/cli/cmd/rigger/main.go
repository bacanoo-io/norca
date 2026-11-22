// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command rigger is the package manager of Norca: it installs modules and
// stacks into a Norca cluster.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bacanoo-io/norca/rigger/cli/command"
	"github.com/bacanoo-io/norca/rigger/cli/internal/operatorinit"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The generators are commands of this program only: "norca rigger" runs
	// the same commands without them.
	code := command.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, operatorinit.Command())

	stop()
	os.Exit(code)
}
