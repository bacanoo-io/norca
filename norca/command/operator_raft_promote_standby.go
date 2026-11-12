// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/posener/complete"
)

// OperatorRaftPromoteStandbyCommand is the manual failover to a standby
// server.
type OperatorRaftPromoteStandbyCommand struct {
	Meta
}

func (c *OperatorRaftPromoteStandbyCommand) Help() string {
	helpText := `
Usage: norca operator raft promote-standby -id <standby> [options]

  Promote a standby server to a voter by hand (manual failover).

  The leader checks that the server has the role "standby", is healthy and
  caught up, and that autopilot target_voters is set. With -replace it then
  removes the failed voter from the Raft configuration; autopilot promotes the
  standby without waiting for the standby cooldown or for
  standby_failover_after. Without -replace the healthy voters must already be
  fewer than target_voters. The command waits up to 30 seconds for the
  promotion and prints the new Raft configuration.

  Nothing is removed unless the promotion can follow: the command refuses
  when the healthy voters without the replaced one still meet target_voters,
  and while a server with the role "voter" waits for its vote.

  Autopilot also promotes standbys on its own when a voter is unhealthy for
  standby_failover_after; this command is for a failover before that time,
  or after a voter is known to be lost.

  If ACLs are enabled, this command requires a token with the
  'operator:write' capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Promote Standby Options:

  -id=<standby>
    The Raft ID of the standby server to promote. Required.

  -replace=<voter>
    The Raft ID of the failed voter the standby replaces. The voter is
    removed from the Raft configuration before the promotion. It must be
    unhealthy for autopilot, unless -force is set. The leader is never
    replaced.

  -force
    Replace the voter even when autopilot reports it healthy. The checks of
    the standby always apply.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorRaftPromoteStandbyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-id":      complete.PredictAnything,
			"-replace": complete.PredictAnything,
			"-force":   complete.PredictNothing,
		})
}

func (c *OperatorRaftPromoteStandbyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorRaftPromoteStandbyCommand) Synopsis() string {
	return "Promote a standby server to a voter (manual failover)"
}

func (c *OperatorRaftPromoteStandbyCommand) Name() string {
	return "operator raft promote-standby"
}

func (c *OperatorRaftPromoteStandbyCommand) Run(args []string) int {
	var id, replace string
	var force bool

	flags := c.Meta.FlagSet("raft", FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&id, "id", "", "")
	flags.StringVar(&replace, "replace", "", "")
	flags.BoolVar(&force, "force", false, "")
	if err := flags.Parse(args); err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse args: %v", err))
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if id == "" {
		c.Ui.Error("The -id of the standby server is required")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if force && replace == "" {
		c.Ui.Error("-force needs -replace")
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	conf, _, err := client.Operator().RaftPromoteStandby(id,
		&api.RaftPromoteStandbyOptions{Replace: replace, Force: force}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error promoting standby: %s", err))
		return 1
	}

	if replace != "" {
		c.Ui.Output(fmt.Sprintf("Removed voter %q, promoted standby %q", replace, id))
	} else {
		c.Ui.Output(fmt.Sprintf("Promoted standby %q", id))
	}
	c.Ui.Output("")
	c.Ui.Output(formatRaftPeers(conf))
	return 0
}
