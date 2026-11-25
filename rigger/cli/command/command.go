// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package command holds the commands of the rigger program.
//
// The program is configured by flags and logs to standard error (design
// 12.1); the engine reads neither. The command line is a tree of cobra
// commands; each flag is written --name, and -name is read as --name
// (normalizeArgs).
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

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

// The flags of the program, which every command takes.
const (
	logFormatFlag = "log-format"
	logLevelFlag  = "log-level"
	jsonFlag      = "json"
)

// The groups of the commands in the help of the program.
const (
	groupDevelop = "develop"
	groupFind    = "find"
	groupDeploy  = "deploy"
	groupPublish = "publish"
	groupProgram = "program"
)

// Program is the program that runs the commands: the name its help shows,
// the version and build it prints and the commands only it has. The rigger
// program is "rigger" with the version of its build; norca runs the same
// commands as "norca rigger" with its own version, the commit Rigger was
// built from (design/rig-integration.md 4.7).
type Program struct {
	// Name is the command line that runs the commands: "rigger", or "norca
	// rigger" for a program that runs them as one of its own commands.
	Name string
	// Version is the version on one line: the header of the help says
	// "Norca Rigger <Version>", rigger version "rigger <Version>".
	Version string
	// Build is what rigger version prints under the version; the Go
	// version and the platform are added.
	Build Build
	// Commands are the commands of this program only, such as the
	// generators of the rigger program ("operator init"): norca does not
	// carry their code, because a program that does not name them does not
	// link them.
	Commands []Command
	// Completion adds "completion", which writes the script that completes
	// the program in a shell. A program that runs the commands as one of its
	// own (norca) completes its own command line and leaves it out.
	Completion bool
}

// Command makes a command of one program only (Program.Commands): a tree
// of cobra commands, new on each call, because a run changes the flags it
// parses. It writes to cmd.OutOrStdout and cmd.ErrOrStderr; an error it
// returns is written by the program, unless it is ErrReported. Each
// command of the tree that runs is an operation of the log, as the other
// commands are.
type Command func() *cobra.Command

// ErrReported is the error of a command that has written its error to
// standard error itself: the program exits with 1 and says nothing more.
var ErrReported = errReported

// Run runs the rigger program with its arguments, without the program name,
// and the commands only it has, and returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, cmds ...Command) int {
	ver, build := programBuild()

	return RunProgram(ctx, Program{Name: "rigger", Version: ver, Build: build, Commands: cmds, Completion: true}, args, stdout, stderr)
}

// RunProgram runs the commands as prog, with the arguments that follow its
// name, and returns the exit code.
func RunProgram(ctx context.Context, prog Program, args []string, stdout, stderr io.Writer) int {
	a := &app{prog: prog, stdout: stdout, stderr: stderr, logger: slog.New(slog.DiscardHandler)}
	root := a.newRoot()

	if isCompletion(args) {
		// The line to complete (cobra's __complete): its words but the
		// last, which is being typed, are read as a run reads them.
		registerCompletions(root)

		if last := len(args) - 1; last > 0 {
			args = append(append([]string{args[0]}, normalizeArgs(root, args[1:last])...), args[last:]...)
		}
	} else {
		args = normalizeArgs(root, args)
	}

	root.SetArgs(args)

	return a.exitCode(root.ExecuteContext(ctx))
}

// app is one run of the program: its streams and, once the flags of the
// program are read, its logger.
type app struct {
	prog           Program
	stdout, stderr io.Writer
	logger         *slog.Logger
}

// newRoot returns the command tree of the program.
func (a *app) newRoot() *cobra.Command {
	var (
		logFormat, logLevel string
		showVersion         bool
	)

	root := &cobra.Command{
		Use:   "rigger",
		Short: "The package manager of Norca",
		Long:  a.rootLong(),
		// The name the help shows: "norca rigger" is one command.
		Annotations:       map[string]string{cobra.CommandDisplayNameAnnotation: a.prog.Name},
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: !a.prog.Completion},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			logger, err := newLogger(a.stderr, logFormat, logLevel)
			if err != nil {
				return err
			}

			a.logger = logger

			return nil
		},
		// Only --version runs the root: without a command it shows the help
		// on standard error and fails, as a mistyped script should.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				return a.span(cmd.Context(), cmdVersion, func(context.Context) error {
					return writeVersion(a.stdout, a.prog, false)
				})
			}

			cmd.SetOut(a.stderr)

			if err := cmd.Help(); err != nil {
				return err
			}

			return errReported
		},
	}

	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetFlagErrorFunc(flagError)

	root.PersistentFlags().StringVar(&logFormat, logFormatFlag, "text", "format of the log on standard error: `text` (a tree of operations) or json")
	root.PersistentFlags().StringVar(&logLevel, logLevelFlag, "info", "lowest `level` logged: debug, info, warn or error")
	root.Flags().BoolVar(&showVersion, cmdVersion, false, "print the version and the build of the program (as rigger version)")

	root.AddGroup(
		&cobra.Group{ID: groupFind, Title: "Find modules and stacks (no cluster needed):"},
		&cobra.Group{ID: groupDeploy, Title: "Install into a cluster:"},
		&cobra.Group{ID: groupDevelop, Title: "Write and check modules and stacks:"},
		&cobra.Group{ID: groupPublish, Title: "Sign, publish and log in:"},
		&cobra.Group{ID: groupProgram, Title: "The program:"},
	)
	root.SetHelpCommandGroupID(groupProgram)
	root.SetCompletionCommandGroupID(groupProgram)

	addToGroup(root, groupFind, a.newSearchCommand(), a.newShowCommand())
	addToGroup(root, groupDeploy, a.newStackCommand(), a.newInstallCommand(), a.newReleaseCommand(),
		a.newOperationCommand(), a.newRegistryCommand(), a.newBundleCommand())
	addToGroup(root, groupDevelop, a.newModuleCommand(), a.newFmtCommand(), a.newConvertCommand())
	addToGroup(root, groupPublish, a.newKeyCommand(), a.newIndexCommand(), a.newLoginCommand(), a.newLogoutCommand())
	addToGroup(root, groupProgram, a.newVersionCommand())

	for _, cmd := range a.prog.Commands {
		c := cmd()
		if c.GroupID == "" {
			c.GroupID = groupDevelop
		}

		root.AddCommand(c)
	}

	root.AddCommand(newEnvironmentTopic())

	// Each command that runs is the root operation of the log; help and
	// completion, added below, are not commands of Rigger.
	a.withSpans(root)

	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	root.SetUsageTemplate(usageTemplate(root.UsageTemplate()))

	return root
}

// addToGroup adds commands to parent, in a group of its help.
func addToGroup(parent *cobra.Command, group string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.GroupID = group
		parent.AddCommand(c)
	}
}

// withSpans wraps each command of the tree that runs and holds no
// commands, but the root, in an operation of the log named after its path
// ("command.stack.apply").
func (a *app) withSpans(c *cobra.Command) {
	for _, sub := range c.Commands() {
		a.withSpans(sub)
	}

	if c.RunE == nil || c.HasSubCommands() || !c.HasParent() {
		return
	}

	inner, name := c.RunE, spanName(c)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return a.span(cmd.Context(), name, func(ctx context.Context) error {
			cmd.SetContext(ctx)

			return inner(cmd, args)
		})
	}
}

// spanName is the path of a command without the program: "stack.apply".
func spanName(c *cobra.Command) string {
	var names []string
	for p := c; p.HasParent(); p = p.Parent() {
		names = append([]string{p.Name()}, names...)
	}

	return strings.Join(names, ".")
}

// span runs fn as the root operation of the log (package oplog).
func (a *app) span(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, span := oplog.Start(ctx, a.logger, slog.LevelDebug, "command."+name)
	err := fn(ctx)
	span.End(ctx, err)

	return err
}

// exitCode reports the error of a run once, on standard error, and returns
// the exit code.
func (a *app) exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errChanges):
		return exitChanges
	case !errors.Is(err, errReported):
		warn(a.stderr, "%v", err)
	}

	return exitError
}

// errChanges is the result of a plan with changes: exit code 2, no message
// (design 9.2).
var errChanges = errors.New("changes")

// flagError is the error of flags that do not parse: the error of pflag and
// where the flags of the command are listed.
func flagError(cmd *cobra.Command, err error) error {
	return fmt.Errorf("%w (%s)", err, helpHint(cmd))
}

// helpHint names the help of a command: "rigger help stack apply". The
// help command works where "--help" does not reach the program (norca
// takes -h and --help for itself).
func helpHint(cmd *cobra.Command) string {
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().CommandPath())

	return "see " + cmd.Root().CommandPath() + " " + cmdHelp + path
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

// warn writes a message for the user to standard error. A failed write
// there can't be reported anywhere else, so its error is dropped.
func warn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "rigger: "+format+"\n", args...)
}
