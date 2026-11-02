// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package subproc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	// ExitSuccess indicates the subprocess completed successfully.
	ExitSuccess = iota

	// ExitFailure indicates the subprocess terminated unsuccessfully.
	ExitFailure

	// ExitTimeout indicates the subprocess timed out before completion.
	ExitTimeout
)

// ExitNotRunnable indicates the subprocess could not find or run the command
// it was to start. It is 127, the code a shell uses for a command it cannot
// find, so that the task's exit code reads the same as from a shell.
const ExitNotRunnable = 127

// MainFunc is the function that runs for this sub-process.
//
// The return value is a process exit code.
type MainFunc func() int

// Do f if nomad was launched as, "nomad [name]". This process will exit without
// running any other part of Nomad.
func Do(name string, f MainFunc) {
	if len(os.Args) > 1 && os.Args[1] == name {
		os.Exit(f())
	}
}

// Print the given message to standard error.
func Print(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Log the given output to the logger.
//
// r should be a buffer containing output (typically combined stdin + stdout)
// f should be an HCLogger Print method (e.g. log.Debug)
func Log(r io.Reader, f func(msg string, args ...any)) string {
	scanner := bufio.NewScanner(r)
	lines := ""
	for scanner.Scan() {
		line := scanner.Text()
		lines += line + "\n"
		f("sub-process", "OUTPUT", line)
	}
	return strings.TrimSpace(lines)
}

// Context creates a context setup with the given timeout.
func Context(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	return context.WithTimeout(ctx, timeout)
}

// SetExpiration is used to ensure the process terminates, once ctx
// is complete. A short grace period is added to allow any cleanup
// to happen first.
func SetExpiration(ctx context.Context) {
	const graceful = 5 * time.Second
	go func() {
		<-ctx.Done()
		time.Sleep(graceful)
		os.Exit(ExitTimeout)
	}()
}
