// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/api/mtls"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

// ServiceIntentionCommand is the parent of the service intention commands.
type ServiceIntentionCommand struct {
	Meta
}

func (c *ServiceIntentionCommand) Help() string {
	helpText := `
Usage: norca service intention <subcommand> [options] [args]

  Service intentions allow or deny connections between services that use
  native mTLS. An intention belongs to the namespace of its destination
  service (the -namespace option) and names a source service, optionally in
  another namespace as <namespace>/<service>. "*" matches every service or
  namespace. The most specific matching intention decides; at equal
  specificity deny wins over allow.

  Allow the web service to connect to the api service:

      $ norca service intention create -allow web api

  Deny every service of the namespace dev the db service:

      $ norca service intention create -deny 'dev/*' db

  List the intentions of a namespace:

      $ norca service intention list -namespace prod

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionCommand) Synopsis() string {
	return "Interact with service intentions"
}

func (c *ServiceIntentionCommand) Name() string { return "service intention" }

func (c *ServiceIntentionCommand) Run(_ []string) int { return cli.RunResultHelp }

// parseIntentionSource splits "<namespace>/<service>" or "<service>".
func parseIntentionSource(arg string) (namespace, service string) {
	if ns, svc, ok := strings.Cut(arg, "/"); ok {
		return ns, svc
	}
	return "", arg
}

// ServiceIntentionCreateCommand creates or replaces an intention.
type ServiceIntentionCreateCommand struct {
	Meta
}

func (c *ServiceIntentionCreateCommand) Help() string {
	helpText := `
Usage: norca service intention create [options] <source> <destination>

  Create creates or replaces the intention from the source service to the
  destination service of the namespace. The source is <service> (in the same
  namespace) or <namespace>/<service>; either part may be "*".

  When ACLs are enabled, this command requires a token with the
  'write-intention' capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Create Options:

  -allow
    Allow the connections (default).

  -deny
    Deny the connections.

  -description <text>
    Description of the intention.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionCreateCommand) Name() string { return "service intention create" }

func (c *ServiceIntentionCreateCommand) Synopsis() string {
	return "Create or replace a service intention"
}

func (c *ServiceIntentionCreateCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-allow":       complete.PredictNothing,
			"-deny":        complete.PredictNothing,
			"-description": complete.PredictAnything,
		})
}

func (c *ServiceIntentionCreateCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServiceIntentionCreateCommand) Run(args []string) int {
	var allow, deny bool
	var description string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&allow, "allow", false, "")
	flags.BoolVar(&deny, "deny", false, "")
	flags.StringVar(&description, "description", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <source> <destination>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if allow && deny {
		c.Ui.Error("Only one of -allow and -deny may be given")
		return 1
	}
	action := api.ServiceIntentionActionAllow
	if deny {
		action = api.ServiceIntentionActionDeny
	}
	srcNamespace, source := parseIntentionSource(args[0])

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	intention := &api.ServiceIntention{
		Source:          source,
		SourceNamespace: srcNamespace,
		Destination:     args[1],
		Action:          action,
		Description:     description,
	}
	if _, err := client.ServiceIntentions().Upsert(intention, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error writing service intention: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Service intention %s %s -> %s written", action, args[0], args[1]))
	return 0
}

// ServiceIntentionDeleteCommand deletes an intention.
type ServiceIntentionDeleteCommand struct {
	Meta
}

func (c *ServiceIntentionDeleteCommand) Help() string {
	helpText := `
Usage: norca service intention delete [options] <source> <destination>

  Delete deletes the intention from the source service to the destination
  service of the namespace.

  When ACLs are enabled, this command requires a token with the
  'write-intention' capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionDeleteCommand) Name() string { return "service intention delete" }

func (c *ServiceIntentionDeleteCommand) Synopsis() string { return "Delete a service intention" }

func (c *ServiceIntentionDeleteCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *ServiceIntentionDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServiceIntentionDeleteCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <source> <destination>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	srcNamespace, source := parseIntentionSource(args[0])
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.ServiceIntentions().Delete(srcNamespace, source, args[1], nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting service intention: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Service intention %s -> %s deleted", args[0], args[1]))
	return 0
}

// ServiceIntentionListCommand lists intentions.
type ServiceIntentionListCommand struct {
	Meta
}

func (c *ServiceIntentionListCommand) Help() string {
	helpText := `
Usage: norca service intention list [options]

  List lists the intentions of the namespace, or of every namespace the token
  may read with -namespace '*'.

  When ACLs are enabled, this command requires a token with the 'read-job'
  capability for the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -json
    Output the intentions in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionListCommand) Name() string { return "service intention list" }

func (c *ServiceIntentionListCommand) Synopsis() string { return "List service intentions" }

func (c *ServiceIntentionListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing})
}

func (c *ServiceIntentionListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServiceIntentionListCommand) Run(args []string) int {
	var json bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	intentions, _, err := client.ServiceIntentions().List(nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing service intentions: %s", err))
		return 1
	}
	if json {
		out, err := Format(true, "", intentions)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if len(intentions) == 0 {
		c.Ui.Output("No service intentions found")
		return 0
	}
	sort.Slice(intentions, func(i, j int) bool {
		a, b := intentions[i], intentions[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.SourceNamespace+"/"+a.Source < b.SourceNamespace+"/"+b.Source
	})
	rows := []string{"Namespace|Destination|Source|Action|Description"}
	for _, i := range intentions {
		rows = append(rows, fmt.Sprintf("%s|%s|%s/%s|%s|%s",
			i.Namespace, i.Destination, i.SourceNamespace, i.Source, i.Action, limit(i.Description, 40)))
	}
	c.Ui.Output(formatList(rows))
	return 0
}

// ServiceIntentionInfoCommand shows an intention.
type ServiceIntentionInfoCommand struct {
	Meta
}

func (c *ServiceIntentionInfoCommand) Help() string {
	helpText := `
Usage: norca service intention info [options] <source> <destination>

  Info shows the intention from the source service to the destination
  service of the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Info Options:

  -json
    Output the intention in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionInfoCommand) Name() string { return "service intention info" }

func (c *ServiceIntentionInfoCommand) Synopsis() string { return "Show a service intention" }

func (c *ServiceIntentionInfoCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing})
}

func (c *ServiceIntentionInfoCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServiceIntentionInfoCommand) Run(args []string) int {
	var json bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&json, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <source> <destination>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	srcNamespace, source := parseIntentionSource(args[0])
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	intention, _, err := client.ServiceIntentions().Info(srcNamespace, source, args[1], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading service intention: %s", err))
		return 1
	}
	if json {
		out, err := Format(true, "", intention)
		if err != nil {
			c.Ui.Error(err.Error())
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	c.Ui.Output(formatKV([]string{
		fmt.Sprintf("Namespace|%s", intention.Namespace),
		fmt.Sprintf("Destination|%s", intention.Destination),
		fmt.Sprintf("Source|%s/%s", intention.SourceNamespace, intention.Source),
		fmt.Sprintf("Action|%s", intention.Action),
		fmt.Sprintf("Description|%s", intention.Description),
		fmt.Sprintf("Create Index|%d", intention.CreateIndex),
		fmt.Sprintf("Modify Index|%d", intention.ModifyIndex),
	}))
	return 0
}

// ServiceIntentionCheckCommand evaluates the intentions for a connection.
type ServiceIntentionCheckCommand struct {
	Meta
}

func (c *ServiceIntentionCheckCommand) Help() string {
	helpText := `
Usage: norca service intention check [options] <source> <destination>

  Check evaluates the intentions of the namespace for a connection from the
  source service (<service> or <namespace>/<service>) to the destination
  service, as the api/mtls library does, and prints the decision. It exits
  with 0 when the connection is allowed and 2 when it is denied.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Check Options:

  -default-allow
    Allow connections no intention matches, like an application that sets
    Authorizer.DefaultAllow. By default they are denied.
`
	return strings.TrimSpace(helpText)
}

func (c *ServiceIntentionCheckCommand) Name() string { return "service intention check" }

func (c *ServiceIntentionCheckCommand) Synopsis() string {
	return "Check whether intentions allow a connection"
}

func (c *ServiceIntentionCheckCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-default-allow": complete.PredictNothing})
}

func (c *ServiceIntentionCheckCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServiceIntentionCheckCommand) Run(args []string) int {
	var defaultAllow bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&defaultAllow, "default-allow", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <source> <destination>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	namespace := c.Meta.namespace
	if namespace == "" {
		namespace = os.Getenv("NOMAD_NAMESPACE")
	}
	if namespace == "" {
		namespace = api.DefaultNamespace
	}
	srcNamespace, source := parseIntentionSource(args[0])
	if srcNamespace == "" {
		srcNamespace = namespace
	}
	intentions, _, err := client.ServiceIntentions().List(&api.QueryOptions{Namespace: namespace})
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing service intentions: %s", err))
		return 1
	}
	d := mtls.Evaluate(intentions, mtls.Source{Namespace: srcNamespace, Services: []string{source}}, namespace, args[1], defaultAllow)
	decision := "denied"
	code := 2
	if d.Allowed {
		decision, code = "allowed", 0
	}
	if d.Intention != nil {
		c.Ui.Output(fmt.Sprintf("Connection %s/%s -> %s/%s is %s by intention %s/%s -> %s (%s)",
			srcNamespace, source, namespace, args[1], decision,
			d.Intention.SourceNamespace, d.Intention.Source, d.Intention.Destination, d.Intention.Action))
	} else {
		c.Ui.Output(fmt.Sprintf("Connection %s/%s -> %s/%s is %s by the default policy (no intention matches)",
			srcNamespace, source, namespace, args[1], decision))
	}
	return code
}
