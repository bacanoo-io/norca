// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"

	"github.com/mitchellh/cli"
)

type OperatorBusCommand struct {
	Meta
}

func (c *OperatorBusCommand) Name() string { return "operator bus" }

func (c *OperatorBusCommand) Run(args []string) int {
	return cli.RunResultHelp
}

func (c *OperatorBusCommand) Synopsis() string {
	return "Provides tools for the system bus of the servers"
}

func (c *OperatorBusCommand) Help() string {
	helpText := `
Usage: norca operator bus <subcommand> [options]

  This command groups subcommands for the system bus: the NATS server with
  JetStream embedded in the Norca servers. The servers of a region form one
  NATS cluster; nodes and authorized ACL tokens connect to it.

  Show the status of the bus of the server answering the request:

      $ norca operator bus status

  Please see the individual subcommand help for detailed usage information.
  `
	return strings.TrimSpace(helpText)
}
