// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type OperatorBackupListCommand struct {
	Meta
}

func (c *OperatorBackupListCommand) Name() string { return "operator backup list" }

func (c *OperatorBackupListCommand) Synopsis() string {
	return "List the backups of a region"
}

func (c *OperatorBackupListCommand) Help() string {
	helpText := `
Usage: norca operator backup list [options]

  List the backups of the region, newest first, and the state of the backup
  schedule.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -json
    Output the backups in JSON format.

  -t
    Format and display the backups using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *OperatorBackupListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupListCommand) Run(args []string) int {
	var json bool
	var tmpl string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
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

	list, _, err := client.Backups().List(nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing backups: %s", err))
		return 1
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, list)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	if s := formatBackupSchedule(list.Schedule); s != "" {
		c.Ui.Output(c.Colorize().Color(s))
		c.Ui.Output("")
	}
	if len(list.Backups) == 0 {
		c.Ui.Output("No backups found")
		return 0
	}
	c.Ui.Output(formatBackupList(list.Backups))
	return 0
}
