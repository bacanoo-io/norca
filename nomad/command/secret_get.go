// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type SecretGetCommand struct {
	Meta
}

func (c *SecretGetCommand) Name() string { return "secret get" }

func (c *SecretGetCommand) Synopsis() string {
	return "Read the value of a secret"
}

func (c *SecretGetCommand) Help() string {
	helpText := `
Usage: norca secret get [options] <path>

  Read the value of a version of a secret. The read is recorded in the audit
  trail of the secret store. Values are only printed when asked for
  explicitly with -item or -format; to see the metadata, use 'secret info'.

  If ACLs are enabled, this command requires the 'read-secret' capability
  for the path.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Get Options:

  -version <n>
    The version to read. Defaults to the current version.

  -item <key>
    Print only the value of the item.

  -format ( json | env )
    Print all items, as a JSON object or as KEY='value' lines.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretGetCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-version": complete.PredictAnything,
			"-item":    complete.PredictAnything,
			"-format":  complete.PredictSet("json", "env"),
		})
}

func (c *SecretGetCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretGetCommand) Run(args []string) int {
	var version uint64
	var item, format string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.Uint64Var(&version, "version", 0, "")
	flags.StringVar(&item, "item", "", "")
	flags.StringVar(&format, "format", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <path>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	switch {
	case item == "" && format == "":
		c.Ui.Error("Refusing to print secret values: name an item with -item or choose -format json|env")
		c.Ui.Error(commandErrorText(c))
		return 1
	case item != "" && format != "":
		c.Ui.Error("Only one of -item and -format can be given")
		return 1
	case format != "" && format != "json" && format != "env":
		c.Ui.Error(fmt.Sprintf("Invalid format %q, must be json or env", format))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	value, _, err := client.Secrets().Read(args[0], version, nil)
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			c.Ui.Error(fmt.Sprintf("Secret %q not found", args[0]))
			return 1
		}
		c.Ui.Error(fmt.Sprintf("Error reading secret: %s", err))
		return 1
	}

	if item != "" {
		v, ok := value.Items[item]
		if !ok {
			c.Ui.Error(fmt.Sprintf("Secret %q has no item %q", args[0], item))
			return 1
		}
		c.Ui.Output(v)
		return 0
	}

	switch format {
	case "json":
		data, err := json.MarshalIndent(value.Items, "", "  ")
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(string(data))
	case "env":
		keys := make([]string, 0, len(value.Items))
		for k := range value.Items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines := make([]string, len(keys))
		for i, k := range keys {
			lines[i] = k + "=" + shellQuote(value.Items[k])
		}
		c.Ui.Output(strings.Join(lines, "\n"))
	}
	return 0
}

// shellQuote quotes a value for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
