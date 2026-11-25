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
	"strings"

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
	cmdHelp     = "help"
	cmdVersion  = "version"
	cmdModule   = "module"
	cmdFmt      = "fmt"
	cmdStack    = "stack"
	cmdInstall  = "install"
	cmdRelease  = "release"
	cmdRegistry = "registry"
	cmdSearch   = "search"
	cmdShow     = "show"
	cmdKey      = "key"
	cmdBundle   = "bundle"
	cmdConvert  = "convert"
	cmdLogin    = "login"
	cmdLogout   = "logout"

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
  stack destroy [dir]     uninstall the stack's releases
  stack publish [dir] [-key name.key]
                          validate a stack with metadata.hcl and its lock
                          file, sign it, commit the signature and tag the
                          version in its git repository (not pushed)
  Every stack command but sync and publish takes, instead of a directory,
  a published stack: <namespace>/<name> of the global registry or a git
  address, with -version "~> 1.2".
  With -server (or RIGGER_SERVER=1), stack plan, apply, status and
  destroy, install and bundle import go to the servers of the cluster,
  which fetch and evaluate; an apply or a destroy runs there as an
  operation whose log the command follows. Ctrl-C cancels the operation
  before its next release; a second Ctrl-C leaves it running.
  operation follow|cancel|status <name> [-namespace ns]
                          an apply or a destroy the servers run
  install <dir|source> -name release [-version constraint] [-namespace ns]
         [-f values] [-var name=value] [-registry name] [-adopt]
                          install or upgrade one release of a module: in a
                          directory, or by its source (<namespace>/<name> of
                          the global registry, builtin/<name>, a git or
                          oci:// address), the newest version that meets
                          -version. With -server the servers fetch it from
                          a registry they have synced (rigger registry
                          sync): <namespace>/<name> from the global one,
                          <name> -registry <registry> from another, or
                          builtin/<name> from their own; no git or oci://
  release list|status|history|rollback|uninstall [name] [-namespace ns] [-adopt]
                          one release, without a stack
  search [text] [-registry global|builtin|<repository>] [-kind module|stack]
         [-category c] [-keyword k] [-offset n] [-limit n] [-max-width n] [-json]
                          the modules and stacks of a registry, without a
                          cluster: the global registry (default), the
                          modules built into the program, or a registry
                          repository (a git address or a directory) at the
                          newest commit of its default branch, fetched
                          into the cache
  show <source> [-version constraint] [-kind module|stack] [-readme]
       [-max-width n] [-json]
                          one module or stack, without a cluster: the
                          newest version that meets -version, fetched and
                          verified as an install does; its metadata,
                          versions, digest, signers, variables, outputs
                          and, of a stack, its modules. The source is
                          <namespace>/<name> of the global registry,
                          builtin/<name>, a git address (a module of a
                          registry repository: <repository>//modules/<name>)
                          or oci://
  registry list|status|sync [name]
                          the registries of the cluster; sync fetches the
                          index of one now (it is never synced on its own)
  registry search <name> [text] [-kind module|stack] [-category c]
         [-keyword k] [-offset n] [-limit n]
                          the modules and stacks of a registry of the
                          cluster at its last sync (without a cluster:
                          rigger search)
  registry select|unselect <name> module|stack <namespace>/<name>
                          what a registry shows in the UI (display
                          selected); the UI changes with the next sync
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
  bundle import file [-server]
                          put a bundle into the cache, for an install
                          without a network; with -server, into the
                          cluster, for its servers
  login <host> [-username name]
                          keep the credentials of a host of git or OCI
                          sources; the password (a token) is the first
                          line of standard input
  logout <host>           forget them
  A credential goes to its host only, over https: those of login, else
  RIGGER_GIT_USERNAME, RIGGER_GIT_PASSWORD for the host RIGGER_GIT_HOST,
  and RIGGER_OCI_USERNAME, RIGGER_OCI_PASSWORD for RIGGER_OCI_HOST.
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
	cmdModule: runModule, cmdFmt: runFmt, cmdStack: runStack, cmdInstall: runInstall, cmdRegistry: runRegistry,
	cmdSearch: runSearch, cmdShow: runShow, cmdRelease: runRelease, cmdKey: runKey, cmdBundle: runBundle, cmdConvert: runConvert,
	cmdIndex: runIndex, cmdOperation: runOperation, cmdLogin: runLogin, cmdLogout: runLogout,
}

// Program is the program that runs the commands: the name its usage shows,
// the version it prints and the commands only it has. The rigger program is
// "rigger" with the version of its module; norca runs the same commands as
// "norca rigger" with its own version, the commit Rigger was built from
// (design/rig-integration.md 4.7).
type Program struct {
	Name    string
	Version string
	// Commands are the commands of this program only, such as the
	// generators of the rigger program ("operator init"): norca does not
	// carry their code, because a program that does not name them does not
	// link them.
	Commands []Command
}

// Command is a command of one program only (Program.Commands).
type Command struct {
	// Name is the first word of the command line ("operator").
	Name string
	// Usage is its lines in the usage of the program, in the layout of the
	// other commands.
	Usage string
	// Run runs it with the arguments after its name. It returns ErrReported
	// when it has written its error itself (a flag error), flag.ErrHelp for
	// -h.
	Run func(ctx context.Context, stdout, stderr io.Writer, args []string) error
}

// ErrReported is the error of a command that has written its error to
// standard error itself: the program exits with 1 and says nothing more.
var ErrReported = errReported

// Run runs the rigger program with its arguments, without the program name,
// and the commands only it has, and returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, cmds ...Command) int {
	return RunProgram(ctx, Program{Name: "rigger", Version: version(), Commands: cmds}, args, stdout, stderr)
}

// RunProgram runs the commands as prog, with the arguments that follow its
// name, and returns the exit code.
func RunProgram(ctx context.Context, prog Program, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(prog.Name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printUsage(flags, prog) }

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

	for _, cmd := range prog.Commands {
		if cmd.Name == name {
			return runCommand(ctx, logger, stdout, stderr, cmd, flags.Args()[1:])
		}
	}

	switch name {
	case cmdVersion:
		return run(ctx, logger, stderr, name, func(context.Context) error {
			_, err := fmt.Fprintf(stdout, "rigger %s\n", prog.Version)

			return err
		})
	case cmdHelp:
		flags.SetOutput(stdout)
		printUsage(flags, prog)

		return exitOK
	case "":
		printUsage(flags, prog)

		return exitError
	default:
		warn(stderr, "unknown command %q", name)
		printUsage(flags, prog)

		return exitError
	}
}

// runCommand runs a command of the program only, as run runs the others; -h
// is not an error.
func runCommand(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, cmd Command, args []string) int {
	return run(ctx, logger, stderr, cmd.Name, func(ctx context.Context) error {
		err := cmd.Run(ctx, stdout, stderr, args)
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return err
	})
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

// printUsage writes the usage, with the name of the program that runs the
// commands and the commands only it has after the others.
func printUsage(flags *flag.FlagSet, prog Program) {
	var own strings.Builder
	for _, cmd := range prog.Commands {
		own.WriteString(cmd.Usage)
	}

	text := strings.Replace(usage, "Usage: rigger", "Usage: "+prog.Name, 1)
	text = strings.Replace(text, "\nFlags:\n", own.String()+"\nFlags:\n", 1)

	w := flags.Output()
	if _, err := io.WriteString(w, text); err != nil {
		return
	}

	flags.PrintDefaults()
}

// warn writes a message for the user to standard error. A failed write
// there can't be reported anywhere else, so its error is dropped.
func warn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "rigger: "+format+"\n", args...)
}
