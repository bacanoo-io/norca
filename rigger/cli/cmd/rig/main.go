// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command rig is the package manager of Norca: it installs modules and
// stacks into a Norca cluster.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bacanoo-io/norca-rig/cli/internal/command"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := command.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}
