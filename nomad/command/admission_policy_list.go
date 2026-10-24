// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type AdmissionPolicyListCommand struct {
	Meta
}

func (c *AdmissionPolicyListCommand) Name() string {
	return "admission-policy list"
}

func (c *AdmissionPolicyListCommand) Synopsis() string {
	return "List admission policies"
}

func (c *AdmissionPolicyListCommand) Help() string {
	helpText := `
Usage: nomad admission-policy list [options]

  List is used to list the admission policies.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of the 'admission' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -filter
    Specifies an expression used to filter results.

  -json
    Output the admission policies in JSON format.

  -page-token
    Where to start pagination.

  -per-page
    How many results to show per page. If not specified, or set to 0, all
    results are returned.

  -prefix
    Only list admission policies whose name starts with the prefix.

  -t
    Format and display the admission policies using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *AdmissionPolicyListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-filter":     complete.PredictAnything,
			"-json":       complete.PredictNothing,
			"-page-token": complete.PredictAnything,
			"-per-page":   complete.PredictAnything,
			"-prefix":     complete.PredictAnything,
			"-t":          complete.PredictAnything,
		})
}

func (c *AdmissionPolicyListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *AdmissionPolicyListCommand) Run(args []string) int {
	var json bool
	var perPage int
	var tmpl, pageToken, filter, prefix string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&filter, "filter", "", "")
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&pageToken, "page-token", "", "")
	flags.IntVar(&perPage, "per-page", 0, "")
	flags.StringVar(&prefix, "prefix", "", "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	opts := &api.QueryOptions{
		Filter:    filter,
		PerPage:   int32(perPage),
		NextToken: pageToken,
		Prefix:    prefix,
	}
	policies, qm, err := client.AdmissionPolicies().List(opts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying admission policies: %s", err))
		return 1
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, policies)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	if len(policies) == 0 {
		c.Ui.Output("No admission policies found")
		return 0
	}
	c.Ui.Output(formatAdmissionPolicyList(policies))

	if qm.NextToken != "" {
		c.Ui.Output(fmt.Sprintf(`
Results have been paginated. To get the next page run:

%s -page-token %s`, argsWithoutPageToken(os.Args), qm.NextToken))
	}
	return 0
}
