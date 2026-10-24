// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/posener/complete"
)

type AdmissionPolicyInspectCommand struct {
	Meta
}

func (c *AdmissionPolicyInspectCommand) Name() string {
	return "admission-policy inspect"
}

func (c *AdmissionPolicyInspectCommand) Synopsis() string {
	return "Inspect an admission policy"
}

func (c *AdmissionPolicyInspectCommand) Help() string {
	helpText := `
Usage: norca admission-policy inspect [options] <name>

  Inspect is used to show the details of an admission policy.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'admission' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Inspect Options:

  -json
    Output the admission policy in JSON format.

  -t
    Format and display the admission policy using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *AdmissionPolicyInspectCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *AdmissionPolicyInspectCommand) AutocompleteArgs() complete.Predictor {
	return admissionPolicyPredictor(c.Meta.Client)
}

func (c *AdmissionPolicyInspectCommand) Run(args []string) int {
	var json bool
	var tmpl string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
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

	policy, _, err := client.AdmissionPolicies().Info(args[0], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading admission policy: %s", err))
		return 1
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, policy)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	c.Ui.Output(c.Colorize().Color(formatAdmissionPolicy(policy)))
	return 0
}

func admissionPolicyPredictor(factory ApiClientFactory) complete.Predictor {
	return complete.PredictFunc(func(a complete.Args) []string {
		client, err := factory()
		if err != nil {
			return nil
		}
		policies, _, err := client.AdmissionPolicies().PrefixList(a.Last, nil)
		if err != nil {
			return nil
		}
		names := make([]string, 0, len(policies))
		for _, p := range policies {
			names = append(names, p.Name)
		}
		return names
	})
}
