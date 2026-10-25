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

type NetworkPolicyListCommand struct {
	Meta
}

func (c *NetworkPolicyListCommand) Name() string {
	return "network-policy list"
}

func (c *NetworkPolicyListCommand) Synopsis() string {
	return "List network policies"
}

func (c *NetworkPolicyListCommand) Help() string {
	helpText := `
Usage: norca network-policy list [options]

  List the network policies of the namespace given with -namespace ("*" for
  every namespace), or with -admin the admin network policies in evaluation
  order.

  If ACLs are enabled, this command requires the 'read-network-policy'
  capability in the namespace, or operator read with -admin.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -admin
    List the admin network policies.

  -filter
    Specifies an expression used to filter query results.

  -json
    Output the policies in JSON format.

  -page-token
    Where to start pagination.

  -per-page
    How many results to show per page.

  -prefix
    Filter the policies by a name prefix.

  -t
    Format and display the policies using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *NetworkPolicyListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-admin":      complete.PredictNothing,
			"-filter":     complete.PredictAnything,
			"-json":       complete.PredictNothing,
			"-page-token": complete.PredictAnything,
			"-per-page":   complete.PredictAnything,
			"-prefix":     complete.PredictAnything,
			"-t":          complete.PredictAnything,
		})
}

func (c *NetworkPolicyListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *NetworkPolicyListCommand) Run(args []string) int {
	var json, admin bool
	var perPage int
	var tmpl, pageToken, filter, prefix string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&admin, "admin", false, "")
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

	var out any
	var rows []string
	var qm *api.QueryMeta
	if admin {
		var policies []*api.AdminNetworkPolicy
		policies, qm, err = client.NetworkPolicies().AdminList(opts)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying admin network policies: %s", err))
			return 1
		}
		out = policies
		if len(policies) > 0 {
			rows = append(rows, "Name|Priority|Namespaces|Selector|Ingress|Egress|Enforcement")
		}
		for _, p := range policies {
			namespaces := "<all>"
			if len(p.Namespaces) > 0 {
				namespaces = strings.Join(p.Namespaces, ",")
			}
			rows = append(rows, fmt.Sprintf("%s|%d|%s|%s|%d|%d|%s", p.Name, p.Priority, namespaces,
				formatNetworkPolicySelector(p.Selector), len(p.Ingress), len(p.Egress), p.Enforcement))
		}
	} else {
		var policies []*api.NetworkPolicy
		policies, qm, err = client.NetworkPolicies().List(opts)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying network policies: %s", err))
			return 1
		}
		out = policies
		if len(policies) > 0 {
			rows = append(rows, "Name|Namespace|Selector|Ingress|Egress|Enforcement")
		}
		for _, p := range policies {
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%d|%d|%s", p.Name, p.Namespace,
				formatNetworkPolicySelector(p.Selector), len(p.Ingress), len(p.Egress), p.Enforcement))
		}
	}

	if json || tmpl != "" {
		formatted, err := Format(json, tmpl, out)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(formatted)
		return 0
	}

	if len(rows) == 0 {
		c.Ui.Output("No network policies found")
		return 0
	}
	c.Ui.Output(formatList(rows))

	if qm != nil && qm.NextToken != "" {
		c.Ui.Output(fmt.Sprintf(`
Results have been paginated. To get the next page run:

%s -page-token %s`, argsWithoutPageToken(os.Args), qm.NextToken))
	}
	return 0
}
