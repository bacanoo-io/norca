// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mitchellh/cli"
	"github.com/posener/complete"

	flag "github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/helper"
)

// EmbeddedCommands returns the Nomad Pack commands keyed by their name below
// the given prefix (e.g. "pack run", "pack registry add"), for registration in
// another mitchellh/cli based CLI. The map also contains the prefix itself as a
// command that groups the subcommands.
//
// Every command keeps Nomad Pack's own flag parsing and UI. Like Main, each run
// gets a fresh base command with its own interrupt context and flag sets, so the
// commands can be registered once in a long-lived command map.
func EmbeddedCommands(prefix string) map[string]cli.CommandFactory {
	_, packCommands := Commands(context.Background(), WithFlags(flag.NewSets()))

	commands := make(map[string]cli.CommandFactory, len(packCommands)+1)
	for name := range packCommands {
		commands[prefix+" "+name] = func() (cli.Command, error) {
			return &embeddedCommand{name: name}, nil
		}
	}
	commands[prefix] = func() (cli.Command, error) {
		return &packCommand{prefix: prefix}, nil
	}
	return commands
}

// embeddedCommand runs a single Nomad Pack command inside another CLI.
type embeddedCommand struct {
	name string
}

var _ cli.CommandAutocomplete = (*embeddedCommand)(nil)

// command builds the wrapped Nomad Pack command with a new base command that
// uses ctx.
func (c *embeddedCommand) command(ctx context.Context) (*baseCommand, cli.Command, error) {
	base, commands := Commands(ctx, WithFlags(flag.NewSets()))
	factory, ok := commands[c.name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown pack command %q", c.name)
	}
	cmd, err := factory()
	if err != nil {
		return nil, nil, err
	}
	return base, cmd, nil
}

func (c *embeddedCommand) Run(args []string) int {
	ctx, closer := helper.WithInterrupt(context.Background())
	defer closer()

	base, cmd, err := c.command(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer base.Close()

	return cmd.Run(args)
}

func (c *embeddedCommand) Help() string {
	_, cmd, err := c.command(context.Background())
	if err != nil {
		return err.Error()
	}
	return cmd.Help()
}

func (c *embeddedCommand) Synopsis() string {
	_, cmd, err := c.command(context.Background())
	if err != nil {
		return ""
	}
	return cmd.Synopsis()
}

func (c *embeddedCommand) AutocompleteArgs() complete.Predictor {
	_, cmd, err := c.command(context.Background())
	if err != nil {
		return complete.PredictNothing
	}
	if ac, ok := cmd.(cli.CommandAutocomplete); ok {
		return ac.AutocompleteArgs()
	}
	return complete.PredictNothing
}

func (c *embeddedCommand) AutocompleteFlags() complete.Flags {
	_, cmd, err := c.command(context.Background())
	if err != nil {
		return nil
	}
	if ac, ok := cmd.(cli.CommandAutocomplete); ok {
		return ac.AutocompleteFlags()
	}
	return nil
}

// packCommand groups the Nomad Pack subcommands. The surrounding CLI appends
// the list of subcommands to its help.
type packCommand struct {
	prefix string
}

func (c *packCommand) Run(args []string) int {
	// "<prefix> -v" and "<prefix> --version" print the Nomad Pack version, as
	// the standalone binary did.
	if len(args) == 1 {
		switch args[0] {
		case "-v", "-version", "--version":
			return (&embeddedCommand{name: "version"}).Run(nil)
		}
	}
	return cli.RunResultHelp
}

func (c *packCommand) Synopsis() string {
	return "Render, plan, run and manage Norca Packs"
}

func (c *packCommand) Help() string {
	helpText := `
Usage: norca PREFIX <subcommand> [options] [args]

  Norca Pack is a templating and packaging tool for Norca jobs. A pack is a
  directory of parameterized job templates and variables; packs are shared
  through registries. Packs are rendered on the client and the resulting jobs
  are submitted to the cluster.

  Render a pack from a local directory:

      $ norca PREFIX render ./my-pack

  Plan and run a pack with variable overrides:

      $ norca PREFIX plan ./my-pack --var-file=./overrides.hcl
      $ norca PREFIX run ./my-pack --var-file=./overrides.hcl

  Add a registry and list its packs:

      $ norca PREFIX registry add norca https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
      $ norca PREFIX list

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(strings.ReplaceAll(helpText, "PREFIX", c.prefix))
}
