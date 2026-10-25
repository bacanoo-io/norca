// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type NetworkPolicyStatusCommand struct {
	Meta
}

func (c *NetworkPolicyStatusCommand) Name() string {
	return "network-policy status"
}

func (c *NetworkPolicyStatusCommand) Synopsis() string {
	return "Show a network policy and the allocations it selects"
}

func (c *NetworkPolicyStatusCommand) Help() string {
	helpText := `
Usage: norca network-policy status [options] <name>

  Status shows a network policy of the namespace given with -namespace (or,
  with -admin, an admin network policy), its rules, and the non-terminal
  allocations it selects with whether their node can enforce the rules.

  If ACLs are enabled, this command requires the 'read-network-policy'
  capability in the namespace, or operator read with -admin. Allocations
  are listed from the namespaces the token can read jobs in.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Status Options:

  -admin
    Show an admin network policy.

  -json
    Output the policy and its allocations in JSON format.

  -t
    Format and display the output using a Go template.

  -verbose
    Display full identifiers.
`
	return strings.TrimSpace(helpText)
}

func (c *NetworkPolicyStatusCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-admin":   complete.PredictNothing,
			"-json":    complete.PredictNothing,
			"-t":       complete.PredictAnything,
			"-verbose": complete.PredictNothing,
		})
}

func (c *NetworkPolicyStatusCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *NetworkPolicyStatusCommand) Run(args []string) int {
	var json, admin, verbose bool
	var tmpl string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&admin, "admin", false, "")
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	flags.BoolVar(&verbose, "verbose", false, "")
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

	length := shortId
	if verbose {
		length = fullId
	}

	var policy any
	var header []string
	var rules []string
	var allocs []*api.NetworkPolicyAllocStatus
	if admin {
		p, _, err := client.NetworkPolicies().AdminInfo(name, nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying admin network policy: %s", err))
			return 1
		}
		allocs, _, err = client.NetworkPolicies().AdminStatus(name, nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying admin network policy status: %s", err))
			return 1
		}
		policy = p
		namespaces := "<all>"
		if len(p.Namespaces) > 0 {
			namespaces = strings.Join(p.Namespaces, ",")
		}
		header = []string{
			fmt.Sprintf("Name|%s", p.Name),
			fmt.Sprintf("Description|%s", p.Description),
			fmt.Sprintf("Priority|%d", p.Priority),
			fmt.Sprintf("Namespaces|%s", namespaces),
			fmt.Sprintf("Selector|%s", formatNetworkPolicySelector(p.Selector)),
			fmt.Sprintf("Enforcement|%s", p.Enforcement),
		}
		rules = append(formatNetworkPolicyRules("ingress", p.Ingress), formatNetworkPolicyRules("egress", p.Egress)...)
	} else {
		p, _, err := client.NetworkPolicies().Info(name, nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying network policy: %s", err))
			return 1
		}
		allocs, _, err = client.NetworkPolicies().Status(name, nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error querying network policy status: %s", err))
			return 1
		}
		policy = p
		defaultDeny := "<none>"
		if len(p.DefaultDeny) > 0 {
			defaultDeny = strings.Join(p.DefaultDeny, ",")
		}
		header = []string{
			fmt.Sprintf("Name|%s", p.Name),
			fmt.Sprintf("Namespace|%s", p.Namespace),
			fmt.Sprintf("Description|%s", p.Description),
			fmt.Sprintf("Selector|%s", formatNetworkPolicySelector(p.Selector)),
			fmt.Sprintf("Default Deny|%s", defaultDeny),
			fmt.Sprintf("Enforcement|%s", p.Enforcement),
		}
		rules = append(formatNetworkPolicyRules("ingress", p.Ingress), formatNetworkPolicyRules("egress", p.Egress)...)
	}

	if json || tmpl != "" {
		out, err := Format(json, tmpl, map[string]any{
			"Policy":      policy,
			"Allocations": allocs,
		})
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	c.Ui.Output(formatKV(header))
	c.Ui.Output(c.Colorize().Color("\n[bold]Rules[reset]"))
	if len(rules) == 0 {
		c.Ui.Output("No rules")
	} else {
		c.Ui.Output(formatList(append([]string{"Direction|Name|Action|Peers|Ports"}, rules...)))
	}
	c.Ui.Output(c.Colorize().Color("\n[bold]Allocations[reset]"))
	c.Ui.Output(formatNetworkPolicyAllocs(allocs, length))
	return 0
}
