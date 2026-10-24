// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type AdmissionPolicyDeleteCommand struct {
	Meta
}

func (c *AdmissionPolicyDeleteCommand) Name() string {
	return "admission-policy delete"
}

func (c *AdmissionPolicyDeleteCommand) Synopsis() string {
	return "Delete an admission policy"
}

func (c *AdmissionPolicyDeleteCommand) Help() string {
	helpText := `
Usage: norca admission-policy delete [options] <name>

  Delete is used to delete an admission policy.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'admission' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *AdmissionPolicyDeleteCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *AdmissionPolicyDeleteCommand) AutocompleteArgs() complete.Predictor {
	return admissionPolicyPredictor(c.Meta.Client)
}

func (c *AdmissionPolicyDeleteCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	if _, err := client.AdmissionPolicies().Delete(args[0], nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting admission policy: %s", err))
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Successfully deleted admission policy %q!", args[0]))
	return 0
}
