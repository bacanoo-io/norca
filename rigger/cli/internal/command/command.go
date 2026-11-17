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

	logFormat := flags.String("log-format", "text", "format of the log on standard error: text or json")
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

	name := flags.Arg(0)
	logger.DebugContext(ctx, "command started", "command", name)

	switch name {
	case cmdVersion:
		if _, err := fmt.Fprintf(stdout, "rig %s\n", version()); err != nil {
			return exitError
		}

		return exitOK
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

// newLogger returns the logger of the program: text or JSON on w, from the
// given level up.
func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: want debug, info, warn or error", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
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
