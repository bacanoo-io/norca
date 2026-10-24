// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"

	"github.com/mitchellh/cli"
)

type OperatorRaftCommand struct {
	Meta
}

func (c *OperatorRaftCommand) Help() string {
	helpText := `
Usage: norca operator raft <subcommand> [options]

  This command groups subcommands for interacting with Nomad's Raft subsystem.
  The command can be used to verify Raft peers or in rare cases to recover
  quorum by removing invalid peers.

  List Raft peers:

      $ norca operator raft list-peers

  Remove a Raft peer:

      $ norca operator raft remove-peer -peer-address "IP:Port"

  Display info about the raft logs in the data directory:

      $ norca operator raft info /var/nomad/data

  Display the log entries persisted in data dir in JSON format.

      $ norca operator raft logs /var/nomad/data

  Display the server state obtained by replaying raft log entries
  persisted in data dir in JSON format.

      $ norca operator raft state /var/nomad/data

  Please see the individual subcommand help for detailed usage information.


`
	return strings.TrimSpace(helpText)
}

func (c *OperatorRaftCommand) Synopsis() string {
	return "Provides access to the Raft subsystem"
}

func (c *OperatorRaftCommand) Name() string { return "operator raft" }

func (c *OperatorRaftCommand) Run(args []string) int {
	return cli.RunResultHelp
}
