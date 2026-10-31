// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type OperatorConsulKVCommand struct {
	Meta
}

func (c *OperatorConsulKVCommand) Name() string { return "operator consul-kv" }

func (c *OperatorConsulKVCommand) Run(args []string) int {
	return cli.RunResultHelp
}

func (c *OperatorConsulKVCommand) Synopsis() string {
	return "Provides tools for the Consul KV of the Consul-compatible API"
}

func (c *OperatorConsulKVCommand) Help() string {
	helpText := `
Usage: norca operator consul-kv <subcommand> [options]

  This command groups subcommands for the Consul KV of the Consul-compatible
  API of the clients (consul_compat).

  Copy the keys kept in Variables into the bucket of the system bus:

      $ norca operator consul-kv migrate

  Please see the individual subcommand help for detailed usage information.
  `
	return strings.TrimSpace(helpText)
}

type OperatorConsulKVMigrateCommand struct {
	Meta
}

func (c *OperatorConsulKVMigrateCommand) Name() string { return "operator consul-kv migrate" }

func (c *OperatorConsulKVMigrateCommand) Synopsis() string {
	return "Copy the Consul KV from Variables to the system bus"
}

func (c *OperatorConsulKVMigrateCommand) Help() string {
	helpText := `
Usage: norca operator consul-kv migrate [options]

  Copies the keys of the Consul KV kept in Variables (the variables backend
  of consul_compat) into the bucket of the system bus, which the bus and auto
  backends use. The keys are the Variables below -path in the namespace of
  the request. Keys already in the bucket are not overwritten and are
  reported as unchanged or as conflicts; the Variables are left in place.
  Keys with flags are skipped: the bus does not keep flags.

  If ACLs are enabled, this command requires a token with the
  'operator:write' capability; only the keys the token may read as Variables
  are copied.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Migrate Options:

  -path=<path>
    The Variables path of the kv block of consul_compat. Defaults to
    "consul/kv".

  -dry-run
    Report what would be copied without writing.

  -json
    Output the result in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorConsulKVMigrateCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-path":    complete.PredictAnything,
			"-dry-run": complete.PredictNothing,
			"-json":    complete.PredictNothing,
		})
}

func (c *OperatorConsulKVMigrateCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorConsulKVMigrateCommand) Run(args []string) int {
	opts := &api.ConsulKVMigrateOptions{}
	var jsonOutput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&opts.Path, "path", "", "")
	flags.BoolVar(&opts.DryRun, "dry-run", false, "")
	flags.BoolVar(&jsonOutput, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	result, _, err := client.Operator().ConsulKVMigrate(opts, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error migrating the Consul KV: %s", err))
		return 1
	}

	if jsonOutput {
		out, err := Format(true, "", result)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	copied := "Copied"
	if opts.DryRun {
		copied = "Would copy"
	}
	c.Ui.Output(formatKV([]string{
		fmt.Sprintf("%s|%d", copied, len(result.Copied)),
		fmt.Sprintf("Unchanged|%d", len(result.Unchanged)),
		fmt.Sprintf("Conflicts|%d", len(result.Conflicts)),
		fmt.Sprintf("Skipped|%d", len(result.Skipped)),
	}))
	if len(result.Conflicts) > 0 {
		c.Ui.Output(c.Colorize().Color("\n[bold]Conflicts[reset] (in the bucket with another value, left as is)"))
		for _, k := range result.Conflicts {
			c.Ui.Output("  " + k)
		}
	}
	if len(result.Skipped) > 0 {
		c.Ui.Output(c.Colorize().Color("\n[bold]Skipped[reset]"))
		rows := []string{"Key|Reason"}
		for _, s := range result.Skipped {
			rows = append(rows, s.Key+"|"+s.Reason)
		}
		c.Ui.Output(formatList(rows))
	}
	return 0
}
