// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type OperatorBackupInspectCommand struct {
	Meta
}

func (c *OperatorBackupInspectCommand) Name() string { return "operator backup inspect" }

func (c *OperatorBackupInspectCommand) Synopsis() string {
	return "Display the details of a backup"
}

func (c *OperatorBackupInspectCommand) Help() string {
	helpText := `
Usage: norca operator backup inspect [options] <id>

  Display the details of a backup: its status per destination, the object
  counts recorded at backup time, labels and pinning. The content is not
  decrypted; use "operator backup contents" to browse it.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Inspect Options:

  -json
    Output the backup in JSON format.

  -t
    Format and display the backup using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupInspectCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *OperatorBackupInspectCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupInspectCommand) Run(args []string) int {
	var json bool
	var tmpl string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	backup, _, err := client.Backups().Info(flags.Args()[0], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading backup: %s", err))
		return 1
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, backup)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	c.Ui.Output(c.Colorize().Color(formatBackup(backup)))
	return 0
}
