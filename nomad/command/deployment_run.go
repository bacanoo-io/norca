// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api/contexts"
	"github.com/posener/complete"
)

type DeploymentRunCommand struct {
	Meta
}

func (c *DeploymentRunCommand) Help() string {
	helpText := `
Usage: norca deployment run [options] <deployment id>

  Run is used to start a pending multiregion deployment that waits for its
  turn in the rollout, for example when a peer region that would start it is
  unreachable.

  When ACLs are enabled, this command requires a token with the 'submit-job'
  and 'read-job' capabilities for the deployment's namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Run Options:

  -detach
    Return immediately instead of entering monitor mode. After deployment
    run, the evaluation ID will be printed to the screen, which can be used
    to examine the evaluation using the eval-status command.

  -verbose
    Display full information.
`
	return strings.TrimSpace(helpText)
}

func (c *DeploymentRunCommand) Synopsis() string {
	return "Run a pending multiregion deployment"
}

func (c *DeploymentRunCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-detach":  complete.PredictNothing,
			"-verbose": complete.PredictNothing,
		})
}

func (c *DeploymentRunCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFunc(func(a complete.Args) []string {
		client, err := c.Meta.Client()
		if err != nil {
			return nil
		}

		resp, _, err := client.Search().PrefixSearch(a.Last, contexts.Deployments, nil)
		if err != nil {
			return []string{}
		}
		return resp.Matches[contexts.Deployments]
	})
}

func (c *DeploymentRunCommand) Name() string { return "deployment run" }

func (c *DeploymentRunCommand) Run(args []string) int {
	var detach, verbose bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&detach, "detach", false, "")
	flags.BoolVar(&verbose, "verbose", false, "")

	if err := flags.Parse(args); err != nil {
		return 1
	}

	// Check that we got exactly one argument
	args = flags.Args()
	if l := len(args); l != 1 {
		c.Ui.Error("This command takes one argument: <deployment id>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	dID := args[0]

	// Truncate the id unless full length is requested
	length := shortId
	if verbose {
		length = fullId
	}

	// Get the HTTP client
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	// Do a prefix lookup
	deploy, possible, err := getDeployment(client.Deployments(), dID)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error retrieving deployment: %s", err))
		return 1
	}

	if len(possible) != 0 {
		c.Ui.Error(fmt.Sprintf("Prefix matched multiple deployments\n\n%s", formatDeployments(possible, length)))
		return 1
	}

	u, _, err := client.Deployments().Run(deploy.ID, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error running deployment: %s", err))
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Deployment %q started", deploy.ID))
	if u.EvalID == "" {
		return 0
	}

	if detach {
		c.Ui.Output("Evaluation ID: " + u.EvalID)
		return 0
	}

	c.Ui.Output("")
	mon := newMonitor(c.Ui, client, length)
	return mon.monitor(u.EvalID)
}
