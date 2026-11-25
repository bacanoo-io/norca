// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package operatorinit

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/command"
)

// Command is "rigger operator init", a command of the rigger program only.
func Command() command.Command {
	return newCommand
}

// newCommand is "operator" and its one command, "init".
func newCommand() *cobra.Command {
	var opts Options

	initCmd := &cobra.Command{
		Use:   "init <group>/<Kind> [dir]",
		Short: "Write the project of an operator for the Norca controller SDK",
		Long: `Write the project of an operator for the Norca controller SDK into dir
(default: the operator name): the object kind <group>/<Kind>, the
controller, a webhook, tests and the module that installs them. No cluster
is needed.`,
		Example: `  rigger operator init db.example.com/Widget
  rigger operator init infra.example.com/Gadget gadget --cluster-scoped --sdk ../norca/norca`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts, args)
		},
	}

	flags := initCmd.Flags()
	flags.StringVar(&opts.Name, "name", "", `name of the operator: binary, controller, job and module (default: "<kind in kebab case>-operator")`)
	flags.StringVar(&opts.Module, "module", "", `Go module `+"`path`"+` (default: "example.com/<name>")`)
	flags.BoolVar(&opts.ClusterScoped, "cluster-scoped", false, "make the kind cluster-scoped instead of namespaced")
	flags.StringVar(&opts.SDKPath, "sdk", "", "the norca `directory` of a Norca checkout: go.mod replaces the SDK and API modules with its controller and api directories")
	flags.BoolVar(&opts.Force, "force", false, "overwrite existing files")

	operator := &cobra.Command{
		Use:   "operator",
		Short: "Generate the project of an operator: init",
		Long:  `Generators of operators for the Norca controller SDK.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New(`unknown command; the operator command is "operator init"`)
		},
	}

	operator.AddCommand(initCmd)

	return operator
}

// run runs "operator init" with its arguments and options.
func run(stdout, stderr io.Writer, opts Options, positional []string) error {
	if len(positional) < 1 || len(positional) > 2 {
		return errors.New("operator init takes one or two arguments: <group>/<Kind> [dir]")
	}

	group, kind, ok := strings.Cut(positional[0], "/")
	if !ok || group == "" || kind == "" || strings.Contains(kind, "/") {
		return fmt.Errorf("invalid object kind %q: want <group>/<Kind>", positional[0])
	}

	opts.Group, opts.Kind = group, kind

	opts.Dir = opts.OperatorName()
	if len(positional) == 2 {
		opts.Dir = positional[1]
	}

	written, err := Generate(opts)
	if err != nil {
		return fmt.Errorf("create the project: %w", err)
	}

	return report(stdout, stderr, positional[0], opts, written)
}

// report lists the files written and the next steps.
func report(stdout, stderr io.Writer, ref string, opts Options, written []string) error {
	var out strings.Builder

	fmt.Fprintf(&out, "created the operator project of %s in %s:\n", ref, opts.Dir)

	for _, f := range written {
		fmt.Fprintf(&out, "  %s\n", f)
	}

	out.WriteString("\nnext: see README.md (go mod tidy, go test ./..., rigger module validate)\n")

	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return err
	}

	if opts.SDKPath == "" {
		_, err := io.WriteString(stderr, "rigger: go.mod needs replace directives for the SDK and API modules of your Norca checkout (or use --sdk)\n")

		return err
	}

	return nil
}
