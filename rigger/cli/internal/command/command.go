// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package command holds the commands of the rig program.
//
// The program is configured by flags and logs to standard error (design
// 12.1); the engine reads neither.
package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"

	"github.com/bacanoo-io/norca-rig/cli/internal/logging"
	"github.com/bacanoo-io/norca-rig/oplog"
)

// Exit codes. A plan exits with 2 when it has changes (design 9.2), so every
// error, a usage error too, exits with 1.
const (
	exitOK    = 0
	exitError = 1
)

// The commands.
const (
	cmdHelp    = "help"
	cmdVersion = "version"
)

const usage = `Usage: rig [flags] <command>

Commands:
  help      print this help
  version   print the version of rig

Flags:
`

// Run runs the rig program with its arguments, without the program name,
// and returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rig", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printUsage(flags) }

	logFormat := flags.String("log-format", "text", "format of the log on standard error: text (a tree of operations) or json")
	logLevel := flags.String("log-level", "info", "lowest level logged: debug, info, warn or error")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}

		return exitError
	}

	logger, err := newLogger(stderr, *logFormat, *logLevel)
	if err != nil {
		warn(stderr, "%v", err)

		return exitError
	}

	switch name := flags.Arg(0); name {
	case cmdVersion:
		return run(ctx, logger, stderr, name, func(context.Context) error {
			_, err := fmt.Fprintf(stdout, "rig %s\n", version())

			return err
		})
	case cmdHelp:
		flags.SetOutput(stdout)
		printUsage(flags)

		return exitOK
	case "":
		printUsage(flags)

		return exitError
	default:
		warn(stderr, "unknown command %q", name)
		printUsage(flags)

		return exitError
	}
}

// run runs a command as the root operation of the log (package oplog) and
// reports its error once, on standard error.
func run(ctx context.Context, logger *slog.Logger, stderr io.Writer, name string, cmd func(context.Context) error) int {
	ctx, span := oplog.Start(ctx, logger, slog.LevelDebug, "command."+name)
	err := cmd(ctx)
	span.End(ctx, err)

	if err != nil {
		warn(stderr, "%v", err)

		return exitError
	}

	return exitOK
}

// newLogger returns the logger of the program on w, from the given level
// up: a tree of operations for a person (text) or flat lines with the IDs
// of the operations (json).
func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: want debug, info, warn or error", level)
	}

	switch format {
	case "text":
		return slog.New(oplog.NewHandler(logging.NewTree(w, lvl))), nil
	case "json":
		return slog.New(oplog.NewHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))), nil
	default:
		return nil, fmt.Errorf("invalid log format %q: want text or json", format)
	}
}

// version returns the version of the module the program was built from.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}

	return info.Main.Version
}

func printUsage(flags *flag.FlagSet) {
	w := flags.Output()
	if _, err := io.WriteString(w, usage); err != nil {
		return
	}

	flags.PrintDefaults()
}

// warn writes a message for the user to standard error. A failed write
// there can't be reported anywhere else, so its error is dropped.
func warn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "rig: "+format+"\n", args...)
}
