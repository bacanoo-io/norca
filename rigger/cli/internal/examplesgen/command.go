// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package examplesgen

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/command"
	"github.com/bacanoo-io/norca/rigger/source"
)

// defaultDir is the directory of the project when none is given.
const defaultDir = "rigger-examples"

// Command is "rigger generate", a command of the rigger program only, with
// its generator "examples".
func Command() command.Command {
	return newCommand
}

// newCommand is "generate" and its one command, "examples".
func newCommand() *cobra.Command {
	var opts Options

	examples := &cobra.Command{
		Use:   "examples [dir]",
		Short: "Write a local example project: a registry of your own, modules and stacks",
		Long: `Write a complete example project into dir (default: ` + defaultDir + `), every file
commented: a registry repository of your own (registry/: two modules, a
library module and a published stack), a stack of your own that installs
from that registry by its path on disk (stack/shop/: values per
environment, trust, a module of its own), an index of the shape of the
global registry over the registry (index/), and the Registry object that
registers the registry in a cluster (cluster/registry.hcl).

Only files are written: no git repository, no key, no signature and no lock
file. README.md in dir has the commands that make the registry a git
repository, sign and tag its modules, and sync, validate and apply the
stacks, in their order. No cluster is needed until the apply.`,
		Example: `  rigger generate examples
  rigger generate examples ~/rigger-start --owner acme
  rigger generate examples rigger-examples --force`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Dir = defaultDir
			if len(args) == 1 {
				opts.Dir = args[0]
			}

			return run(cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}

	flags := examples.Flags()
	flags.StringVar(&opts.Owner, "owner", DefaultOwner, "the owner of the modules and stacks: the name of the registry in the stacks (<owner>/webapp) and the namespace of the index")
	flags.BoolVar(&opts.Force, "force", false, "overwrite existing files")

	generate := &cobra.Command{
		Use:   "generate",
		Short: "Generate files to start from: examples",
		Long:  `Generators of files to start from. None of them needs a cluster.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New(`unknown command; the generate command is "generate examples"`)
		},
	}

	generate.AddCommand(examples)

	return generate
}

// run writes the project of opts and reports it.
func run(stdout, stderr io.Writer, opts Options) error {
	written, err := Generate(opts)
	if err != nil {
		return fmt.Errorf("write the example project: %w", err)
	}

	var out strings.Builder

	fmt.Fprintf(&out, "created the example project in %s:\n", opts.Dir)

	for _, f := range written {
		fmt.Fprintf(&out, "  %s\n", f)
	}

	out.WriteString("\nnext: follow README.md there (git init the registry, rigger key generate, rigger module publish, rigger stack sync)\n")

	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return err
	}

	return warnAddress(stderr, opts.Dir)
}

// warnAddress warns when the path of the registry, which the stacks name
// the registry by, is not one Rigger reads as a git source: the stacks
// then need another address.
func warnAddress(stderr io.Writer, dir string) error {
	registry, err := RegistryPath(dir)
	if err != nil {
		return err
	}

	if _, err := source.Parse(registry, nil); err == nil {
		return nil
	}

	_, err = fmt.Fprintf(stderr, "rigger: the stacks name the registry by %s, which is no git address rigger reads (%v); "+
		"change the source of their registry blocks\n", registry, err)

	return err
}
