// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
)

type AdmissionPolicyCommand struct {
	Meta
}

func (c *AdmissionPolicyCommand) Name() string {
	return "admission-policy"
}

func (c *AdmissionPolicyCommand) Synopsis() string {
	return "Interact with admission policies"
}

func (c *AdmissionPolicyCommand) Help() string {
	helpText := `
Usage: norca admission-policy <subcommand> [options] [args]

  This command groups subcommands for interacting with admission policies.
  Admission policies are CEL rules that every job submitted with "job run" or
  checked with "job plan" in a matching namespace must satisfy. Policies that
  select the "variable" resource also apply to Variables writes.

  Create or update an admission policy:

    $ norca admission-policy apply <path>

  List all admission policies:

    $ norca admission-policy list

  Inspect an admission policy:

    $ norca admission-policy inspect <name>

  Delete an admission policy:

    $ norca admission-policy delete <name>

  Please refer to individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *AdmissionPolicyCommand) Run(args []string) int {
	return cli.RunResultHelp
}

func formatAdmissionPolicyList(policies []*api.AdmissionPolicy) string {
	out := make([]string, len(policies)+1)
	out[0] = "Name|Action|Resources|Namespaces|Rules|Description"
	for i, p := range policies {
		out[i+1] = fmt.Sprintf("%s|%s|%s|%s|%d|%s",
			p.Name, p.Action, admissionPolicyResources(p), admissionPolicyNamespaces(p),
			len(p.Rules), p.Description)
	}
	return formatList(out)
}

// admissionPolicyNamespaces returns the namespace selection of the policy.
func admissionPolicyNamespaces(p *api.AdmissionPolicy) string {
	if p.Match != nil && len(p.Match.Namespaces) > 0 {
		return strings.Join(p.Match.Namespaces, ",")
	}
	return "*"
}

// admissionPolicyResources returns the resource selection of the policy.
// Policies without one apply to jobs only.
func admissionPolicyResources(p *api.AdmissionPolicy) string {
	if p.Match != nil && len(p.Match.Resources) > 0 {
		return strings.Join(p.Match.Resources, ",")
	}
	return api.AdmissionResourceJob
}

func formatAdmissionPolicy(p *api.AdmissionPolicy) string {
	basic := []string{
		fmt.Sprintf("Name|%s", p.Name),
		fmt.Sprintf("Description|%s", p.Description),
		fmt.Sprintf("Action|%s", p.Action),
		fmt.Sprintf("Resources|%s", admissionPolicyResources(p)),
		fmt.Sprintf("Namespaces|%s", admissionPolicyNamespaces(p)),
	}
	if p.Match != nil && len(p.Match.VariablePaths) > 0 {
		basic = append(basic, fmt.Sprintf("Variable Paths|%s", strings.Join(p.Match.VariablePaths, ",")))
	}
	if p.SchemaFrom != "" {
		basic = append(basic, fmt.Sprintf("Schema From|%s", p.SchemaFrom))
	}
	basic = append(basic,
		fmt.Sprintf("Create Index|%d", p.CreateIndex),
		fmt.Sprintf("Modify Index|%d", p.ModifyIndex),
	)
	out := formatKV(basic)

	for i, r := range p.Rules {
		out += fmt.Sprintf("\n\n[bold]Rule %d[reset]\n", i)
		out += formatKV([]string{
			fmt.Sprintf("Expression|%s", r.Expression),
			fmt.Sprintf("Message|%s", r.Message),
		})
	}
	return out
}
