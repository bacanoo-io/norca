// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package operatorinit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bacanoo-io/norca/rigger/cli/command"
)

// usage is the line of the command in the usage of the rigger program.
const usage = `  operator init [-name n] [-module path] [-cluster-scoped] [-sdk dir]
                [-force] <group>/<Kind> [dir]
                          write the project of an operator for the Norca
                          controller SDK: the object kind, the controller,
                          a webhook, tests and the module that installs
                          them (dir: the operator name); no cluster needed
`

// Command is "rigger operator init", a command of the rigger program only.
func Command() command.Command {
	return command.Command{Name: "operator", Usage: usage, Run: run}
}

// run runs "operator init" with the arguments after "operator".
func run(_ context.Context, stdout, stderr io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New(`unknown command; the operator command is "operator init"`)
	}

	var opts Options

	flags := flag.NewFlagSet("rigger operator init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.Name, "name", "", `name of the operator: binary, controller, job and module (default: "<kind in kebab case>-operator")`)
	flags.StringVar(&opts.Module, "module", "", `Go module path (default: "example.com/<name>")`)
	flags.BoolVar(&opts.ClusterScoped, "cluster-scoped", false, "make the kind cluster-scoped instead of namespaced")
	flags.StringVar(&opts.SDKPath, "sdk", "", "the norca directory of a Norca checkout: go.mod replaces the SDK and API modules with its controller and api directories")
	flags.BoolVar(&opts.Force, "force", false, "overwrite existing files")

	positional, err := parseMixed(flags, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}

		return command.ErrReported
	}

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

// parseMixed parses flags that may be before, between and after the other
// arguments, and returns those. The flag package stops at the first
// argument that is no flag: a flag after it would be taken for an argument,
// "-force" for the directory of the project.
func parseMixed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string

	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}

		if flags.NArg() == 0 {
			return positional, nil
		}

		positional, args = append(positional, flags.Arg(0)), flags.Args()[1:]
	}
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
		_, err := io.WriteString(stderr, "rigger: go.mod needs replace directives for the SDK and API modules of your Norca checkout (or use -sdk)\n")

		return err
	}

	return nil
}
