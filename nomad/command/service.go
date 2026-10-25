// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"

	"github.com/mitchellh/cli"
)

type ServiceCommand struct {
	Meta
}

func (c *ServiceCommand) Help() string {
	helpText := `
Usage: norca service <subcommand> [options]

  This command groups subcommands for interacting with the services API.

  List services:

      $ norca service list

  Detail an individual service:

      $ norca service info <service_name>

  Delete an individual service registration:

      $ norca service delete <service_name> <service_id>

  Register an external service, renew its TTL check and deregister it:

      $ norca service register -address 10.0.0.5 -port 5432 -ttl 30s <service_name>
      $ norca service ttl <service_id>
      $ norca service deregister <service_id>

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceCommand) Name() string { return "service" }

func (c *ServiceCommand) Synopsis() string { return "Interact with registered services" }

func (c *ServiceCommand) Run(_ []string) int { return cli.RunResultHelp }
