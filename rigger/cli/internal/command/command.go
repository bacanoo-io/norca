// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package command holds the commands of the rigger program.
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

	"github.com/bacanoo-io/norca/rigger/cli/internal/logging"
	"github.com/bacanoo-io/norca/rigger/oplog"
)

// Exit codes. A plan exits with 2 when it has changes (design 9.2), so every
// error, a usage error too, exits with 1.
const (
	exitOK      = 0
	exitError   = 1
	exitChanges = 2
)

// The commands.
const (
	cmdHelp    = "help"
	cmdVersion = "version"
	cmdModule  = "module"
	cmdFmt     = "fmt"
	cmdStack   = "stack"
	cmdInstall = "install"
	cmdRelease = "release"
	cmdKey     = "key"
	cmdBundle  = "bundle"
	cmdConvert = "convert"

	cmdRender   = "render"
	cmdPublish  = "publish"
	cmdTest     = "test"
	cmdValidate = "validate"
)

const usage = `Usage: rigger [flags] <command>

Commands:
  help                    print this help
  version                 print the version of rigger
  fmt [-check] [path ...] format the .hcl files under the paths (the current
                          directory); -check lists those that are not
                          formatted and fails
  stack sync [dir] [-upgrade]
                          resolve the versions of the modules of a stack,
                          fetch and verify them, write rigger.lock.hcl
  stack validate [dir] [-f values] [-var name=value]
                          evaluate a stack without a cluster (each file of
                          values/ when none is given)
  stack plan [dir] [-f values] [-var name=value] [-json]
                          what an apply would change; exit code 2 with
                          changes
  stack apply [dir] [-f values] [-var name=value] [-prune]
                          plan and apply, release after release
  stack status [dir] [-json]
                          the releases of the stack, their revision, drift
  stack destroy [dir]     uninstall the releases of the stack
  stack publish [dir] [-key name.key]
                          validate a stack with metadata.hcl and its lock
                          file, sign it, commit the signature and tag the
                          version in its git repository (not pushed)
  Every stack command but sync and publish takes, instead of a directory,
  a published stack: <namespace>/<name> of the global registry or a git
  address, with -version "~> 1.2".
  install <dir> -name release [-namespace ns] [-f values] [-var name=value]
                          install or upgrade one release of a module
  release list|status|history|rollback|uninstall [name] [-namespace ns]
                          one release, without a stack
  key generate [-out name] write name.key (encrypted with RIGGER_KEY_PASSWORD)
                          and name.pub
  key sign [dir] -key name.key
                          sign the tree digest of a module into its
                          .rigger-signatures directory
  key verify [dir] -key name.pub
                          check that a module is signed by the key
  bundle create [stack] -o file.riggerbundle
                          the modules of a stack from git sources, as its
                          lock pins them, in one archive
  bundle inspect file     what a bundle holds
  bundle import file      put a bundle into the cache, for an install
                          without a network
  The cluster is that of NORCA_ADDR and NORCA_TOKEN. A source
  <namespace>/<name> that names no registry of the stack is a module of
  the global registry, github.com/bacanoo-io/norca-global-registry, or of
  RIGGER_GLOBAL_REGISTRY (a git address or a directory).
  convert pack <dir> -out <dir> [-examples <dir>]
                          the mechanical part of turning a pack into a
                          module: metadata, variables, examples; the
                          templates kept as comments to rewrite as HCL
  module validate [dir]   check a module without a cluster: its files, its
                          icon, its tests, and its example values rendered
                          (dir: the current one)
  module test [dir]       run the tests of a module (tests/*.riggertest.hcl):
                          its values in, assertions over its resources
  module publish [dir] -to oci://host/repository
                          push the module, with its child modules and
                          signatures, tagged with its version
  module publish [dir] [-key name.key]
                          sign the module, commit the signature and tag
                          the version in its git repository (not pushed)
  index update [-dir registry] [-namespace ns] [-replace url=path]
                          list in the entries of a registry of the shape of
                          the global registry the versions the repositories
                          of its namespaces have tagged, signed and checked
  index check [-dir registry] [-base revision] [-replace url=path]
                          check the files of such a registry; with -base,
                          that a change only adds, and the added versions
                          against their sources
  module render [dir] [-f values.vars.hcl] [-var name=value]
         [-release name] [-namespace ns] [-region region]
                          evaluate a module and print its resources and
                          outputs as JSON; secret items are named, not shown

Flags:
`

// command runs a command of the rigger program with its arguments.
type command func(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int

// commands are the commands that take commands or flags of their own.
var commands = map[string]command{
	cmdModule: runModule, cmdFmt: runFmt, cmdStack: runStack, cmdInstall: runInstall,
	cmdRelease: runRelease, cmdKey: runKey, cmdBundle: runBundle, cmdConvert: runConvert,
	cmdIndex: runIndex,
}

// Run runs the rigger program with its arguments, without the program name,
// and returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rigger", flag.ContinueOnError)
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

	name := flags.Arg(0)
	if cmd, ok := commands[name]; ok {
		return cmd(ctx, logger, stdout, stderr, flags.Args()[1:])
	}

	switch name {
	case cmdVersion:
		return run(ctx, logger, stderr, name, func(context.Context) error {
			_, err := fmt.Fprintf(stdout, "rigger %s\n", version())

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

	switch {
	case errors.Is(err, errChanges):
		return exitChanges
	case err != nil:
		if !errors.Is(err, errReported) {
			warn(stderr, "%v", err)
		}

		return exitError
	default:
		return exitOK
	}
}

// errChanges is the result of a plan with changes: exit code 2, no message
// (design 9.2).
var errChanges = errors.New("changes")

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
	_, _ = fmt.Fprintf(w, "rigger: "+format+"\n", args...)
}
