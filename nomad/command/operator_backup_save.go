// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/posener/complete"
)

type OperatorBackupSaveCommand struct {
	Meta
}

func (c *OperatorBackupSaveCommand) Name() string { return "operator backup save" }

func (c *OperatorBackupSaveCommand) Synopsis() string {
	return "Take a manual backup"
}

func (c *OperatorBackupSaveCommand) Help() string {
	helpText := `
Usage: norca operator backup save [options]

  Take a manual backup on the leader of the region, into every configured
  destination. By default the command waits until the backup is uploaded and
  verified.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Save Options:

  -label <key>=<value>
    A label of the backup, for example note="before upgrade". Can be given
    several times.

  -pin
    Pin the backup, so retention never deletes it.

  -detach
    Return as soon as the snapshot is taken, with the backup ID.

  -json
    Output the backup in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupSaveCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-label":  complete.PredictAnything,
			"-pin":    complete.PredictNothing,
			"-detach": complete.PredictNothing,
			"-json":   complete.PredictNothing,
		})
}

func (c *OperatorBackupSaveCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupSaveCommand) Run(args []string) int {
	var pin, detach, json bool
	var labels flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.Var(&labels, "label", "")
	flags.BoolVar(&pin, "pin", false, "")
	flags.BoolVar(&detach, "detach", false, "")
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	parsed, err := parseBackupLabels(labels)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	resp, _, err := client.Backups().Save(&api.BackupSaveRequest{Labels: parsed, Pin: pin, Detach: detach}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error taking backup: %s", err))
		return 1
	}

	if json {
		out, err := Format(true, "", resp)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if resp.Backup == nil {
		c.Ui.Output(fmt.Sprintf("Backup %q started", resp.ID))
		return 0
	}
	c.Ui.Output(fmt.Sprintf("Backup %q %s", resp.ID, resp.Backup.Status))
	c.Ui.Output(c.Colorize().Color(formatBackup(resp.Backup)))
	return 0
}
