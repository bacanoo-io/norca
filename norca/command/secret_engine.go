// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

// The commands of the dynamic secrets engines of the built-in secret store
// and of their leases: "norca secret engine ..." and "norca secret lease
// ...".

type SecretEngineCommand struct {
	Meta
}

func (c *SecretEngineCommand) Help() string {
	helpText := `
Usage: norca secret engine <subcommand> [options] [args]

  This command groups subcommands for the dynamic secrets engines of the
  built-in secret store. An engine creates short-lived credentials for the
  tasks that ask for them with a secret block naming the engine and a role:

      secret "db" {
        engine = "postgres"
        role   = "worker"
      }

  The credential is delivered to the task like any secret, with file and env
  blocks. The engine names the service of an operator, which creates the
  credentials of its target system.

  Create or update an engine from a specification:

      $ norca secret engine apply postgres.hcl

  List the engines and show one:

      $ norca secret engine list
      $ norca secret engine info postgres

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretEngineCommand) Synopsis() string {
	return "Interact with dynamic secrets engines"
}

func (c *SecretEngineCommand) Name() string { return "secret engine" }

func (c *SecretEngineCommand) Run(args []string) int {
	return cli.RunResultHelp
}

// SecretEngineListCommand lists the engines.
type SecretEngineListCommand struct {
	Meta
}

func (c *SecretEngineListCommand) Name() string { return "secret engine list" }

func (c *SecretEngineListCommand) Synopsis() string { return "List dynamic secrets engines" }

func (c *SecretEngineListCommand) Help() string {
	helpText := `
Usage: norca secret engine list [options] [<prefix>]

  List the dynamic secrets engines of the namespace given with -namespace
  ("*" for every namespace) whose name starts with the prefix.

  If ACLs are enabled, this command requires the 'read-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -json
    Output the engines in JSON format.

  -t
    Format and display the engines using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretEngineListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing, "-t": complete.PredictAnything})
}

func (c *SecretEngineListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretEngineListCommand) Run(args []string) int {
	var jsonOut bool
	var tmpl string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
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
	engines, _, err := client.SecretEngines().List(prefix, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing secret engines: %s", err))
		return 1
	}
	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, engines)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if len(engines) == 0 {
		c.Ui.Output("No secret engines found")
		return 0
	}
	rows := []string{"Namespace|Name|Type|Roles|Modify Time"}
	for _, e := range engines {
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s", e.Namespace, e.Name, e.Type,
			strings.Join(e.Roles, ","), formatUnixNanoTime(e.ModifyTime)))
	}
	c.Ui.Output(formatList(rows))
	return 0
}

// SecretEngineInfoCommand shows an engine.
type SecretEngineInfoCommand struct {
	Meta
}

func (c *SecretEngineInfoCommand) Name() string { return "secret engine info" }

func (c *SecretEngineInfoCommand) Synopsis() string { return "Show a dynamic secrets engine" }

func (c *SecretEngineInfoCommand) Help() string {
	helpText := `
Usage: norca secret engine info [options] <name>

  Show a dynamic secrets engine: its connection, roles and the state of its
  admin keys. The engine never contains a credential.

  If ACLs are enabled, this command requires the 'read-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Info Options:

  -json
    Output the engine in JSON format.

  -t
    Format and display the engine using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretEngineInfoCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing, "-t": complete.PredictAnything})
}

func (c *SecretEngineInfoCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretEngineInfoCommand) Run(args []string) int {
	var jsonOut bool
	var tmpl string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	engine, _, err := client.SecretEngines().Info(args[0], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading secret engine: %s", err))
		return 1
	}
	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, engine)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	c.Ui.Output(formatSecretEngine(engine))
	return 0
}

// formatSecretEngine renders an engine.
func formatSecretEngine(e *api.SecretEngine) string {
	kv := []string{
		fmt.Sprintf("Namespace|%s", e.Namespace),
		fmt.Sprintf("Name|%s", e.Name),
		fmt.Sprintf("Type|%s", e.Type),
		fmt.Sprintf("Description|%s", e.Description),
	}
	if e.Operator != nil {
		kv = append(kv, fmt.Sprintf("Service|%s", e.Operator.Service))
	}
	kv = append(kv,
		fmt.Sprintf("Modify Time|%s", formatUnixNanoTime(e.ModifyTime)),
		fmt.Sprintf("Modify Index|%d", e.ModifyIndex))
	out := formatKV(kv)

	rows := []string{"Role|TTL|Max TTL|Params"}
	for _, r := range e.Roles {
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s", r.Name, r.TTL, r.MaxTTL, strings.Join(r.Params, ",")))
	}
	return out + "\n\n" + formatList(rows)
}

// secretEngineFile is the HCL specification of an engine.
type secretEngineFile struct {
	Name        string                    `hcl:"name"`
	Type        string                    `hcl:"type"`
	Description string                    `hcl:"description,optional"`
	Operator    *secretEngineOperatorFile `hcl:"operator,block"`
	Roles       []*secretEngineRoleFile   `hcl:"role,block"`
}

type secretEngineOperatorFile struct {
	Service   string `hcl:"service"`
	Namespace string `hcl:"namespace,optional"`
}

type secretEngineRoleFile struct {
	Name   string            `hcl:"name,label"`
	TTL    string            `hcl:"ttl,optional"`
	MaxTTL string            `hcl:"max_ttl,optional"`
	Config map[string]string `hcl:"config,optional"`
	Params []string          `hcl:"params,optional"`
}

// parseSecretEngine parses an engine specification in HCL or, if jsonInput
// is set, in the API JSON form. Files named in the HCL form are read
// relative to the specification's directory.
func parseSecretEngine(path string, content []byte, jsonInput bool) (*api.SecretEngine, error) {
	if jsonInput {
		var engine api.SecretEngine
		if err := json.Unmarshal(content, &engine); err != nil {
			return nil, err
		}
		return &engine, nil
	}
	name := path
	if !strings.HasSuffix(name, ".hcl") && !strings.HasSuffix(name, ".json") {
		name += ".hcl"
	}
	var file secretEngineFile
	if err := hclsimple.Decode(name, content, nil, &file); err != nil {
		return nil, err
	}
	engine := &api.SecretEngine{
		Name:        file.Name,
		Type:        file.Type,
		Description: file.Description,
	}
	if o := file.Operator; o != nil {
		engine.Operator = &api.SecretEngineOperator{Service: o.Service, Namespace: o.Namespace}
	}
	for _, r := range file.Roles {
		role := &api.SecretEngineRole{Name: r.Name, Config: r.Config, Params: r.Params}
		var err error
		if r.TTL != "" {
			if role.TTL, err = time.ParseDuration(r.TTL); err != nil {
				return nil, fmt.Errorf("role %q: invalid ttl: %v", r.Name, err)
			}
		}
		if r.MaxTTL != "" {
			if role.MaxTTL, err = time.ParseDuration(r.MaxTTL); err != nil {
				return nil, fmt.Errorf("role %q: invalid max_ttl: %v", r.Name, err)
			}
		}
		engine.Roles = append(engine.Roles, role)
	}
	return engine, nil
}

// SecretEngineApplyCommand creates or updates an engine.
type SecretEngineApplyCommand struct {
	Meta
}

func (c *SecretEngineApplyCommand) Name() string { return "secret engine apply" }

func (c *SecretEngineApplyCommand) Synopsis() string {
	return "Create or update a dynamic secrets engine"
}

func (c *SecretEngineApplyCommand) Help() string {
	helpText := `
Usage: norca secret engine apply [options] <input>

  Create or update a dynamic secrets engine from a specification file, or
  from standard input with "-". The engine names the service of an operator,
  which creates the credentials of its target system; the operator may start
  after the engine.

      name = "postgres"
      type = "operator"

      operator {
        service = "postgres-operator"
      }

      role "app" {
        ttl     = "1h"
        max_ttl = "24h"
        config  = { database = "app" }
        params  = ["schema"]
      }

  config is passed to the operator and must hold nothing secret; params lists
  the keys of the params a job may send.

  If ACLs are enabled, this command requires the 'write-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -check-index
    Apply the engine only if its modify index is the given one; 0 creates
    an engine that must not exist yet.

  -json
    Parse the input as a JSON engine in the API form.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretEngineApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-json": complete.PredictNothing, "-check-index": complete.PredictAnything})
}

func (c *SecretEngineApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json"))
}

func (c *SecretEngineApplyCommand) Run(args []string) int {
	var jsonInput bool
	var checkIndex int64
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	flags.Int64Var(&checkIndex, "check-index", -1, "")
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
	if path == "-" {
		content, err = io.ReadAll(os.Stdin)
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to read %q: %v", path, err))
		return 1
	}
	engine, err := parseSecretEngine(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse the engine: %v", err))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if idx := optionalCheckIndex(checkIndex); idx != nil {
		_, _, err = client.SecretEngines().CheckedWrite(engine, *idx, nil)
	} else {
		_, _, err = client.SecretEngines().Write(engine, nil)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying secret engine: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Successfully applied secret engine %q!", engine.Name))
	return 0
}

// SecretEngineDeleteCommand deletes an engine.
type SecretEngineDeleteCommand struct {
	Meta
}

func (c *SecretEngineDeleteCommand) Name() string { return "secret engine delete" }

func (c *SecretEngineDeleteCommand) Synopsis() string { return "Delete a dynamic secrets engine" }

func (c *SecretEngineDeleteCommand) Help() string {
	helpText := `
Usage: norca secret engine delete [options] <name>

  Delete a dynamic secrets engine. An engine with live leases can't be
  deleted: revoke them first with "norca secret lease revoke -engine". The
  engine's admin credential (a built-in secret) is kept.

  If ACLs are enabled, this command requires the 'write-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *SecretEngineDeleteCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *SecretEngineDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretEngineDeleteCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.SecretEngines().Delete(args[0], nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting secret engine: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Successfully deleted secret engine %q!", args[0]))
	return 0
}

type SecretLeaseCommand struct {
	Meta
}

func (c *SecretLeaseCommand) Help() string {
	helpText := `
Usage: norca secret lease <subcommand> [options] [args]

  This command groups subcommands for the leases of dynamic secrets. A lease
  belongs to a credential an engine created for a task; it is renewed by the
  task's client and revoked when the task stops, when it is not renewed, or
  at its max_ttl. Leases never contain the credential.

      $ norca secret lease list -engine postgres
      $ norca secret lease revoke <lease-id>

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretLeaseCommand) Synopsis() string { return "Interact with the leases of dynamic secrets" }

func (c *SecretLeaseCommand) Name() string { return "secret lease" }

func (c *SecretLeaseCommand) Run(args []string) int { return cli.RunResultHelp }

// SecretLeaseListCommand lists leases.
type SecretLeaseListCommand struct {
	Meta
}

func (c *SecretLeaseListCommand) Name() string { return "secret lease list" }

func (c *SecretLeaseListCommand) Synopsis() string { return "List the leases of dynamic secrets" }

func (c *SecretLeaseListCommand) Help() string {
	helpText := `
Usage: norca secret lease list [options]

  List the leases of dynamic secrets of the namespace given with -namespace
  ("*" for every namespace). Revoked leases are kept until their credential
  has expired.

  If ACLs are enabled, this command requires the 'read-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -engine
    List the leases of this engine only.

  -json
    Output the leases in JSON format.

  -t
    Format and display the leases using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretLeaseListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient), complete.Flags{
		"-engine": complete.PredictAnything, "-json": complete.PredictNothing, "-t": complete.PredictAnything})
}

func (c *SecretLeaseListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *SecretLeaseListCommand) Run(args []string) int {
	var jsonOut bool
	var tmpl, engine string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	flags.StringVar(&engine, "engine", "", "")
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
	leases, _, err := client.SecretLeases().List(engine, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing leases: %s", err))
		return 1
	}
	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, leases)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if len(leases) == 0 {
		c.Ui.Output("No leases found")
		return 0
	}
	rows := []string{"ID|Engine|Role|Job|Task|Allocation|Expires|State"}
	for _, l := range leases {
		state := "live"
		if l.Revoked {
			state = "revoked (" + l.RevokeReason + ")"
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", limit(l.ID, 8), l.Engine, l.Role,
			l.JobID, l.Task, limit(l.AllocID, 8), formatUnixNanoTime(l.ExpireTime), state))
	}
	c.Ui.Output(formatList(rows))
	return 0
}

// SecretLeaseRevokeCommand revokes leases.
type SecretLeaseRevokeCommand struct {
	Meta
}

func (c *SecretLeaseRevokeCommand) Name() string { return "secret lease revoke" }

func (c *SecretLeaseRevokeCommand) Synopsis() string { return "Revoke leases of dynamic secrets" }

func (c *SecretLeaseRevokeCommand) Help() string {
	helpText := `
Usage: norca secret lease revoke [options] <lease-id>
       norca secret lease revoke [options] -engine <name>

  Revoke a lease, or every live lease of an engine. The credential stops
  working;
  a task that still runs gets a new credential from its client, delivered
  with the change mode of its secret block.

  If ACLs are enabled, this command requires the 'write-secret-engine'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Revoke Options:

  -engine
    Revoke every live lease of this engine.
`
	return strings.TrimSpace(helpText)
}

func (c *SecretLeaseRevokeCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{"-engine": complete.PredictAnything})
}

func (c *SecretLeaseRevokeCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictAnything
}

func (c *SecretLeaseRevokeCommand) Run(args []string) int {
	var engine string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&engine, "engine", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if (engine == "") == (len(args) != 1) || len(args) > 1 {
		c.Ui.Error("This command takes either a lease ID or -engine")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	var revoked []string
	if engine != "" {
		revoked, _, err = client.SecretEngines().RevokeLeases(engine, nil)
	} else {
		revoked, _, err = client.SecretLeases().Revoke(args[0], nil)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error revoking leases: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Revoked %d lease(s)", len(revoked)))
	for _, id := range revoked {
		c.Ui.Output("  " + id)
	}
	return 0
}
