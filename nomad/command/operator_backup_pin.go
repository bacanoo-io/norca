// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/posener/complete"
)

type OperatorBackupPinCommand struct {
	Meta
}

func (c *OperatorBackupPinCommand) Name() string { return "operator backup pin" }

func (c *OperatorBackupPinCommand) Synopsis() string {
	return "Pin, unpin or label a backup"
}

func (c *OperatorBackupPinCommand) Help() string {
	helpText := `
Usage: norca operator backup pin [options] <id>

  Pin a backup so retention never deletes it, unpin it, or change its labels.
  The change is written to the manifest of the backup too.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'backup' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Pin Options:

  -unpin
    Unpin the backup instead.

  -label <key>=<value>
    Add or replace a label. Can be given several times. With -label only,
    the pinning is not changed.

  -remove-label <key>
    Remove a label. Can be given several times.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupPinCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-unpin":        complete.PredictNothing,
			"-label":        complete.PredictAnything,
			"-remove-label": complete.PredictAnything,
		})
}

func (c *OperatorBackupPinCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBackupPinCommand) Run(args []string) int {
	var unpin bool
	var labels, remove flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&unpin, "unpin", false, "")
	flags.Var(&labels, "label", "")
	flags.Var(&remove, "remove-label", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		c.Ui.Error("This command takes one argument: <id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	id := flags.Args()[0]
	added, err := parseBackupLabels(labels)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	req := &api.BackupUpdateRequest{RemoveLabels: remove}
	switch {
	case unpin:
		req.Pinned = pointer.Of(false)
	case len(added) == 0 && len(remove) == 0:
		req.Pinned = pointer.Of(true)
	}
	if len(added) > 0 {
		// Labels replace the existing ones; merge them here.
		current, _, err := client.Backups().Info(id, nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading backup: %s", err))
			return 1
		}
		req.Labels = make(map[string]string, len(current.Labels)+len(added))
		for k, v := range current.Labels {
			req.Labels[k] = v
		}
		for k, v := range added {
			req.Labels[k] = v
		}
	}

	backup, _, err := client.Backups().Update(id, req, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error updating backup: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Backup %q updated: pinned=%t labels=%s", backup.ID, backup.Pinned, formatBackupLabels(backup.Labels)))
	return 0
}
