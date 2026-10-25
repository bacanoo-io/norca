// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type NetworkPolicyDeleteCommand struct {
	Meta
}

func (c *NetworkPolicyDeleteCommand) Name() string {
	return "network-policy delete"
}

func (c *NetworkPolicyDeleteCommand) Synopsis() string {
	return "Delete a network policy"
}

func (c *NetworkPolicyDeleteCommand) Help() string {
	helpText := `
Usage: norca network-policy delete [options] <name>

  Delete a network policy of the namespace given with -namespace, or with
  -admin an admin network policy. The rules are removed from the running
  allocations it selected.

  If ACLs are enabled, this command requires the 'write-network-policy'
  capability in the namespace, or operator write with -admin.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Delete Options:

  -admin
    Delete an admin network policy.
`
	return strings.TrimSpace(helpText)
}

func (c *NetworkPolicyDeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-admin": complete.PredictNothing,
		})
}

func (c *NetworkPolicyDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *NetworkPolicyDeleteCommand) Run(args []string) int {
	var admin bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&admin, "admin", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	name := args[0]

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	if admin {
		if _, err := client.NetworkPolicies().AdminDelete(name, nil); err != nil {
			c.Ui.Error(fmt.Sprintf("Error deleting admin network policy: %s", err))
			return 1
		}
		c.Ui.Output(fmt.Sprintf("Successfully deleted admin network policy %q!", name))
		return 0
	}

	if _, err := client.NetworkPolicies().Delete(name, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting network policy: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Successfully deleted network policy %q!", name))
	return 0
}
