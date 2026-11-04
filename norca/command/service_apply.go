// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/helper/externalspec"
	"github.com/posener/complete"
)

// ServiceApplyCommand registers the external services of a file.
type ServiceApplyCommand struct {
	Meta
}

func (c *ServiceApplyCommand) Help() string {
	helpText := `
Usage: norca service apply [options] [-f <file>]

  Apply registers or updates the external services declared in a file, in the
  HCL form below or, with -json, in the JSON form of the API. A file can hold
  several external_service blocks. The command is idempotent: a service that
  did not change is left alone. It does not remove anything; use
  "norca service deregister" for that.

      external_service "orders-db" {
        namespace = "prod"
        address   = "10.20.0.5"
        port      = 5432

        check {
          type     = "tcp"
          tcp      = "10.20.0.5:5432"
          interval = "10s"
        }

        checked_by {
          node_pool = "edge"
        }
      }

      external_service "billing-db" {
        type   = "alias"
        target = "billing.example.com"
      }

  A service is an address (type = "address", the default) or a DNS alias
  (type = "alias", with a target). Its http and tcp checks run on a client
  node that the servers choose among the nodes of checked_by (a node pool
  and datacenters); when that node goes away, another one takes them over.
  Without a check the service is static. Declared services stay until they
  are deregistered, unless deregister_critical_after is set.

  The file is read from -f, from the argument, or from stdin with "-". The
  whole file is checked before anything is registered.

  When ACLs are enabled, this command requires a token with the
  'register-external-service' capability for the namespaces of the file;
  services without any check require operator write in addition.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -f <file>
    The file to apply; "-" reads stdin.

  -json
    Read the JSON form of the API instead of HCL.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceApplyCommand) Name() string { return "service apply" }

func (c *ServiceApplyCommand) Synopsis() string {
	return "Register the external services declared in a file"
}

func (c *ServiceApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-f":    complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json")),
			"-json": complete.PredictNothing,
		})
}

func (c *ServiceApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json"))
}

func (c *ServiceApplyCommand) Run(args []string) int {
	var file string
	var jsonInput bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&file, "f", "", "")
	flags.BoolVar(&jsonInput, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	switch {
	case file != "" && len(args) == 0:
	case file == "" && len(args) == 1:
		file = args[0]
	default:
		c.Ui.Error("This command takes the file with -f or as its one argument")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	var content []byte
	var err error
	path := file
	if file == "-" {
		if content, err = io.ReadAll(os.Stdin); err != nil {
			c.Ui.Error(fmt.Sprintf("Failed to read stdin: %v", err))
			return 1
		}
		path = "stdin.hcl"
	} else if content, err = os.ReadFile(file); err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to read file %q: %v", file, err))
		return 1
	}

	items, err := externalspec.Parse(path, content, jsonInput)
	if err != nil {
		var pe *externalspec.Error
		if errors.As(err, &pe) {
			c.Ui.Error("Failed to parse input content:")
			for _, p := range pe.Problems {
				if p.Line > 0 {
					c.Ui.Error(fmt.Sprintf("  line %d: %s", p.Line, p.Message))
				} else {
					c.Ui.Error("  " + p.Message)
				}
			}
		} else {
			c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
		}
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	// The default namespace is the one of the client configuration.
	defaultNS := c.Meta.clientConfig().Namespace
	if defaultNS == "" {
		defaultNS = api.DefaultNamespace
	}
	for _, it := range items {
		if it.Namespace == "" {
			it.Namespace = defaultNS
		}
	}

	// The servers check the whole file before anything is written.
	parsed, err := client.Services().ParseExternal(&api.ExternalServiceParseRequest{
		Text: string(externalspec.Format(items)),
	}, &api.WriteOptions{Namespace: defaultNS})
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error checking the services: %s", err))
		return 1
	}
	if len(parsed.Problems) > 0 {
		c.Ui.Error("The file is not valid:")
		for _, p := range parsed.Problems {
			c.Ui.Error("  " + p.Message)
		}
		return 1
	}

	existing := make(map[string]uint64) // namespace/id -> modify index
	namespaces := make(map[string]bool)
	for _, it := range items {
		namespaces[it.Namespace] = true
	}
	for ns := range namespaces {
		regs, _, err := client.Services().ListExternal(&api.QueryOptions{Namespace: ns})
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error listing the external services of namespace %q: %s", ns, err))
			return 1
		}
		for _, r := range regs {
			existing[r.Namespace+"/"+r.ID] = r.ModifyIndex
		}
	}

	failed := false
	for _, it := range items {
		id := it.Service.ID
		if id == "" {
			id = it.Service.Name
		}
		before, existed := existing[it.Namespace+"/"+id]
		_, wm, err := client.Services().RegisterExternal(it.Service, &api.WriteOptions{Namespace: it.Namespace})
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error applying external service %q (namespace %q): %s",
				it.Service.Name, it.Namespace, err))
			failed = true
			continue
		}
		state := "created"
		switch {
		case existed && wm != nil && wm.LastIndex == before:
			state = "unchanged"
		case existed:
			state = "updated"
		}
		c.Ui.Output(fmt.Sprintf("External service %q (namespace %q): %s", it.Service.Name, it.Namespace, state))
	}
	if failed {
		return 1
	}
	return 0
}
