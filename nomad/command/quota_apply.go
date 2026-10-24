// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/resourcespec"
	"github.com/posener/complete"
)

type QuotaApplyCommand struct {
	Meta
}

func (c *QuotaApplyCommand) Help() string {
	helpText := `
Usage: norca quota apply [options] <input>

  Apply is used to create or update a quota specification. The specification file
  will be read from stdin by specifying "-", otherwise a path to the file is
  expected.

  If ACLs are enabled, this command requires a token with the 'quota:write'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -json
    Parse the input as a JSON quota specification.
`

	return strings.TrimSpace(helpText)
}

func (c *QuotaApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
		})
}

func (c *QuotaApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFiles("*")
}

func (c *QuotaApplyCommand) Synopsis() string {
	return "Create or update a quota specification"
}

func (c *QuotaApplyCommand) Name() string { return "quota apply" }

// parseQuotaSpec is used to parse the quota specification from HCL
func parseQuotaSpec(input []byte) (*api.QuotaSpec, error) {
	return resourcespec.ParseQuota(input)
}

func (c *QuotaApplyCommand) Run(args []string) int {
	var jsonInput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")

	if err := flags.Parse(args); err != nil {
		return 1
	}

	// Check that we get exactly one argument
	args = flags.Args()
	if l := len(args); l != 1 {
		c.Ui.Error("This command takes one argument: <input>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	// Read the file contents
	file := args[0]
	var rawQuota []byte
	var err error
	if file == "-" {
		rawQuota, err = io.ReadAll(os.Stdin)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read stdin: %v", err))
			return 1
		}
	} else {
		rawQuota, err = os.ReadFile(file)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read file: %v", err))
			return 1
		}
	}

	var spec *api.QuotaSpec
	if jsonInput {
		var jsonSpec api.QuotaSpec
		dec := json.NewDecoder(bytes.NewBuffer(rawQuota))
		if err := dec.Decode(&jsonSpec); err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to parse quota: %v", err))
			return 1
		}
		spec = &jsonSpec
	} else {
		hclSpec, err := parseQuotaSpec(rawQuota)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error parsing quota specification: %s", err))
			return 1
		}

		spec = hclSpec
	}

	// Get the HTTP client
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	_, err = client.Quotas().Register(spec, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying quota specification: %s", err))
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Successfully applied quota specification %q!", spec.Name))
	return 0
}
