// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type SecretInfoCommand struct {
	Meta
}

func (c *SecretInfoCommand) Name() string { return "secret info" }

func (c *SecretInfoCommand) Synopsis() string {
	return "Show the metadata of a secret"
}

func (c *SecretInfoCommand) Help() string {
	helpText := `
Usage: norca secret info [options] <path>

  Show the metadata and the versions of a secret: never its values, only a
  keyed hash of them.

  If ACLs are enabled, this command requires the 'list-secrets' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Info Options:

  -json
    Output the metadata in JSON format.

  -t
    Format and display the metadata using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretInfoCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *SecretInfoCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretInfoCommand) Run(args []string) int {
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
		c.Ui.Error("This command takes one argument: <path>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	secret, _, err := client.Secrets().Info(args[0], nil)
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			c.Ui.Error(fmt.Sprintf("Secret %q not found", args[0]))
			return 1
		}
		c.Ui.Error(fmt.Sprintf("Error reading secret metadata: %s", err))
		return 1
	}

	if json || tmpl != "" {
		formatted, err := Format(json, tmpl, secret)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(formatted)
		return 0
	}

	c.Ui.Output(formatSecretMetadata(secret))
	if len(secret.Versions) > 0 {
		c.Ui.Output(c.Colorize().Color("\n[bold]Versions[reset]"))
		c.Ui.Output(formatSecretVersions(secret))
	}
	return 0
}
