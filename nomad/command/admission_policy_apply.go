// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type AdmissionPolicyApplyCommand struct {
	Meta
}

func (c *AdmissionPolicyApplyCommand) Name() string {
	return "admission-policy apply"
}

func (c *AdmissionPolicyApplyCommand) Synopsis() string {
	return "Create or update an admission policy"
}

func (c *AdmissionPolicyApplyCommand) Help() string {
	helpText := `
Usage: norca admission-policy apply [options] <input>

  Apply is used to create or update an admission policy. The specification
  file is read from stdin by specifying "-", otherwise a path to the file is
  expected. The CEL expressions of the rules are compiled by the server and
  the policy is rejected if one of them does not compile.

  Example specification:

    name        = "no-privileged-container"
    description = "Privileged Docker containers are forbidden in prod"
    action      = "deny" # or "warn"

    match {
      namespaces = ["prod"]
    }

    rule {
      expression = "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))"
      message    = "Privileged containers are not allowed in the prod namespace."
    }

  Policies apply to jobs only unless "match" selects other resources. A
  policy for Variables writes sees the written Variable as "variable" and
  the Variable it replaces or deletes as "oldVariable" (null if none), each
  with "namespace", "path" and "items"; "request.operation" is "set" or
  "delete". With schema_from = "crd-schema", the items of a
  crd/<group>/<kind>/<name> Variable must also match the JSON Schema in the
  "schema" item of the crd-schema/<group>/<kind> Variable if it exists:

    name        = "crd-objects"
    action      = "deny"
    schema_from = "crd-schema"

    match {
      resources      = ["variable"]
      variable_paths = ["crd/*"]
    }

    rule {
      expression = "request.operation == 'delete' || has(variable.items.spec)"
      message    = "CRD objects need a spec item."
    }

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'admission' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -json
    Parse the input as a JSON admission policy.
`
	return strings.TrimSpace(helpText)
}

func (c *AdmissionPolicyApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
		})
}

func (c *AdmissionPolicyApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(
		complete.PredictFiles("*.hcl"),
		complete.PredictFiles("*.json"),
	)
}

func (c *AdmissionPolicyApplyCommand) Run(args []string) int {
	var jsonInput bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <input>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	path := args[0]
	var content []byte
	var err error
	switch path {
	case "-":
		content, err = io.ReadAll(os.Stdin)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read stdin: %v", err))
			return 1
		}
		// Set .hcl extension so the decoder doesn't fail.
		path = "stdin.hcl"
	default:
		content, err = os.ReadFile(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read file %q: %v", path, err))
			return 1
		}
	}

	policy, err := parseAdmissionPolicy(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	if _, err := client.AdmissionPolicies().Register(policy, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying admission policy: %s", err))
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Successfully applied admission policy %q!", policy.Name))
	return 0
}

// parseAdmissionPolicy parses an admission policy specification in HCL or,
// if jsonInput is set, in the API JSON form.
func parseAdmissionPolicy(path string, content []byte, jsonInput bool) (*api.AdmissionPolicy, error) {
	var policy api.AdmissionPolicy
	if jsonInput {
		if err := json.Unmarshal(content, &policy); err != nil {
			return nil, err
		}
		return &policy, nil
	}
	if !strings.HasSuffix(path, ".hcl") && !strings.HasSuffix(path, ".json") {
		path += ".hcl"
	}
	if err := hclsimple.Decode(path, content, nil, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}
