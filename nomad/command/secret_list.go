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

type SecretListCommand struct {
	Meta
}

func (c *SecretListCommand) Name() string { return "secret list" }

func (c *SecretListCommand) Synopsis() string {
	return "List secrets"
}

func (c *SecretListCommand) Help() string {
	helpText := `
Usage: norca secret list [options] [<prefix>]

  List the secrets of the namespace given with -namespace ("*" for every
  namespace) whose path starts with the prefix. Only paths and metadata are
  shown, never values.

  If ACLs are enabled, this command requires the 'list-secrets' capability
  for the listed paths; secrets the token may not list are left out.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -filter
    Specifies an expression used to filter query results.

  -json
    Output the secrets in JSON format.

  -page-token
    Where to start pagination.

  -per-page
    How many results to show per page.

  -t
    Format and display the secrets using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-filter":     complete.PredictAnything,
			"-json":       complete.PredictNothing,
			"-page-token": complete.PredictAnything,
			"-per-page":   complete.PredictAnything,
			"-t":          complete.PredictAnything,
		})
}

func (c *SecretListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretListCommand) Run(args []string) int {
	var json bool
	var perPage int
	var tmpl, pageToken, filter string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&filter, "filter", "", "")
	flags.BoolVar(&json, "json", false, "")
	flags.StringVar(&pageToken, "page-token", "", "")
	flags.IntVar(&perPage, "per-page", 0, "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) > 1 {
		c.Ui.Error("This command takes at most one argument: [<prefix>]")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	prefix := ""
	if len(args) == 1 {
		prefix = args[0]
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	secrets, qm, err := client.Secrets().List(prefix, &api.QueryOptions{
		Filter:    filter,
		PerPage:   int32(perPage),
		NextToken: pageToken,
	})
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing secrets: %s", err))
		return 1
	}

	if json || tmpl != "" {
		formatted, err := Format(json, tmpl, secrets)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(formatted)
		return 0
	}

	if len(secrets) == 0 {
		c.Ui.Output("No secrets found")
		return 0
	}
	rows := []string{"Namespace|Path|Version|Deleted|Owner|Expires At|Modify Time"}
	for _, s := range secrets {
		rows = append(rows, fmt.Sprintf("%s|%s|%d|%t|%s|%s|%s",
			s.Namespace, s.Path, s.CurrentVersion, s.Deleted, s.Owner,
			formatSecretTime(s.ExpiresAt), formatUnixNanoTime(s.ModifyTime)))
	}
	c.Ui.Output(formatList(rows))

	if qm != nil && qm.NextToken != "" {
		c.Ui.Output(fmt.Sprintf(`
Results have been paginated. To get the next page run:

%s -page-token %s`, argsWithoutPageToken(os.Args), qm.NextToken))
	}
	return 0
}
