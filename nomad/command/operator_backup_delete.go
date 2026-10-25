// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type OperatorBackupDeleteCommand struct {
	Meta
}

func (c *OperatorBackupDeleteCommand) Name() string { return "operator backup delete" }

func (c *OperatorBackupDeleteCommand) Synopsis() string {
	return "Delete a backup"
}

func (c *OperatorBackupDeleteCommand) Help() string {
	helpText := `
Usage: norca operator backup delete [options] <id>

  Delete a backup from the destinations that allow pruning, and its record.
  Pinned backups and the newest backups kept by min_keep can't be deleted.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupDeleteCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *OperatorBackupDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupDeleteCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	id := flags.Args()[0]

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.Backups().Delete(id, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting backup: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Backup %q deleted", id))
	return 0
}
