// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/posener/complete"
)

type NetworkPolicyApplyCommand struct {
	Meta
}

func (c *NetworkPolicyApplyCommand) Name() string {
	return "network-policy apply"
}

func (c *NetworkPolicyApplyCommand) Synopsis() string {
	return "Create or update a network policy"
}

func (c *NetworkPolicyApplyCommand) Help() string {
	helpText := `
Usage: norca network-policy apply [options] <input>

  Apply is used to create or update a network policy. The specification file
  is read from stdin by specifying "-", otherwise a path to the file is
  expected.

  Example network policy:

    name      = "db-only-from-api"
    namespace = "prod"

    selector { app = "postgres" }

    ingress {
      from {
        selector { app = "api" }
      }
      ports = ["5432/tcp"]
    }

    egress {
      to {
        cidr = ["10.20.0.0/16"]
      }
      ports = ["443/tcp"]
    }

    enforcement = "required"   # or "best_effort"

  The selector matches the meta of the job and the task group (the group
  meta overrides the job meta). Being selected means default deny in each
  direction the policy has rules for (or lists in default_deny = [...]);
  the rules of every policy selecting an allocation add up, and replies to
  allowed connections are always allowed. Peers are CIDR blocks or, with
  "selector" and "namespace", allocations running on the same node.
  Ingress ports are the ports inside the allocation ("to" of a port
  mapping) and may be port labels, e.g. "db/tcp".

  With -admin the file is an admin network policy, evaluated before the
  namespace policies in priority order (lower first):

    name       = "no-metadata-service"
    priority   = 10
    namespaces = ["*"]

    egress {
      name   = "metadata"
      action = "deny"    # "allow", "deny" or "pass"
      to {
        cidr = ["169.254.169.254/32"]
      }
    }

  "allow" and "deny" are final; "pass" hands the decision over to the
  namespace policies.

  If ACLs are enabled, this command requires the 'write-network-policy'
  capability in the namespace (network_policy { policy = "write" }), or
  operator write with -admin.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -admin
    Apply an admin network policy.

  -json
    Parse the input as a JSON policy in the API form.
`
	return strings.TrimSpace(helpText)
}

func (c *NetworkPolicyApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-admin": complete.PredictNothing,
			"-json":  complete.PredictNothing,
		})
}

func (c *NetworkPolicyApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(
		complete.PredictFiles("*.hcl"),
		complete.PredictFiles("*.json"),
	)
}

func (c *NetworkPolicyApplyCommand) Run(args []string) int {
	var jsonInput, admin bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	flags.BoolVar(&admin, "admin", false, "")
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
		path = "stdin.hcl"
	default:
		content, err = os.ReadFile(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read file %q: %v", path, err))
			return 1
		}
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	if admin {
		policy, err := parseAdminNetworkPolicy(path, content, jsonInput)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
			return 1
		}
		if _, err := client.NetworkPolicies().AdminRegister(policy, nil); err != nil {
			c.Ui.Error(fmt.Sprintf("Error applying admin network policy: %s", err))
			return 1
		}
		c.Ui.Output(fmt.Sprintf("Successfully applied admin network policy %q!", policy.Name))
		return 0
	}

	policy, err := parseNetworkPolicy(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
		return 1
	}
	if _, err := client.NetworkPolicies().Register(policy, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying network policy: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Successfully applied network policy %q!", policy.Name))
	return 0
}
