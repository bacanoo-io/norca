// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type ObjectKindCommand struct {
	Meta
}

func (c *ObjectKindCommand) Name() string {
	return "object-kind"
}

func (c *ObjectKindCommand) Synopsis() string {
	return "Interact with custom object kinds"
}

func (c *ObjectKindCommand) Help() string {
	helpText := `
Usage: norca object-kind <subcommand> [options] [args]

  This command groups subcommands for managing the kinds (types) of custom
  objects. A kind has a JSON Schema with default values, CEL validation
  rules and display columns. Objects of a kind are managed with the
  "norca object" commands.

  Create or update an object kind:

      $ norca object-kind apply postgrescluster.hcl

  List object kinds:

      $ norca object-kind list

  Inspect an object kind:

      $ norca object-kind inspect db.example.com/PostgresCluster

  Delete an object kind (it must not have objects):

      $ norca object-kind delete db.example.com/PostgresCluster

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindCommand) Run(_ []string) int {
	return cli.RunResultHelp
}

// splitKindRef splits a "group/Kind" argument.
func splitKindRef(ref string) (string, string, error) {
	group, kind, ok := strings.Cut(ref, "/")
	if !ok || group == "" || kind == "" || strings.Contains(kind, "/") {
		return "", "", fmt.Errorf("invalid object kind %q, must be <group>/<Kind>", ref)
	}
	return group, kind, nil
}

func objectKindPredictor(factory ApiClientFactory) complete.Predictor {
	return complete.PredictFunc(func(a complete.Args) []string {
		client, err := factory()
		if err != nil {
			return nil
		}
		kinds, _, err := client.ObjectKinds().List(&api.QueryOptions{Prefix: a.Last})
		if err != nil {
			return nil
		}
		refs := make([]string, 0, len(kinds))
		for _, k := range kinds {
			refs = append(refs, k.KindRef())
		}
		return refs
	})
}

// ObjectKindApplyCommand creates or updates an object kind.
type ObjectKindApplyCommand struct {
	Meta
}

func (c *ObjectKindApplyCommand) Name() string { return "object-kind apply" }

func (c *ObjectKindApplyCommand) Synopsis() string {
	return "Create or update an object kind"
}

func (c *ObjectKindApplyCommand) Help() string {
	helpText := `
Usage: norca object-kind apply [options] <input>

  Apply is used to create or update an object kind. The specification file
  is read from stdin by specifying "-", otherwise a path to the file is
  expected. The server compiles the JSON Schema and the CEL rules and
  rejects the kind if one of them is invalid. Objects already stored are
  not validated again when the schema changes.

  Example specification:

    kind "PostgresCluster" {
      group      = "db.example.com"
      namespaced = true # the default
      schema     = file("postgrescluster.schema.json") # JSON Schema of {spec, status}

      rule {
        expression = "self.spec.replicas >= 1 && self.spec.replicas <= 7"
        message    = "The number of replicas must be between 1 and 7."
      }
      rule {
        # transition rule: oldSelf is the stored object, null on create
        expression = "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)"
        message    = "Storage cannot be reduced."
      }

      columns {
        column "Version"  { path = "spec.version" }
        column "Replicas" { path = "status.readyReplicas" }
        column "Ready"    { path = "status.conditions[?type=='Ready'].status" }
      }
    }

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'operator' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -dry-run
    Validate and compile the object kind without writing it.

  -json
    Parse the input as a JSON object kind (also used for .json files).
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-dry-run": complete.PredictNothing,
			"-json":    complete.PredictNothing,
		})
}

func (c *ObjectKindApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json"))
}

func (c *ObjectKindApplyCommand) Run(args []string) int {
	var jsonInput, dryRun bool

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	flags.BoolVar(&dryRun, "dry-run", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <input>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}

	content, path, err := readObjectInput(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	kind, err := parseObjectKind(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	if _, _, err := client.ObjectKinds().Register(kind, &api.ObjectWriteOptions{DryRun: dryRun}, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying object kind: %s", err))
		return 1
	}

	if dryRun {
		c.Ui.Output(fmt.Sprintf("Object kind %q is valid (dry run, nothing was written)", kind.KindRef()))
		return 0
	}
	c.Ui.Output(fmt.Sprintf("Successfully applied object kind %q!", kind.KindRef()))
	return 0
}

// ObjectKindListCommand lists object kinds.
type ObjectKindListCommand struct {
	Meta
}

func (c *ObjectKindListCommand) Name() string { return "object-kind list" }

func (c *ObjectKindListCommand) Synopsis() string {
	return "List object kinds"
}

func (c *ObjectKindListCommand) Help() string {
	helpText := `
Usage: norca object-kind list [options]

  List is used to list the object kinds the token may read.

  If ACLs are enabled, kinds are listed for tokens with the 'read'
  capability of the 'operator' policy, or with any object capability on
  the kind.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -prefix
    Only list kinds whose "group/Kind" reference starts with the prefix.

  -json
    Output the object kinds in JSON format.

  -t
    Format and display the object kinds using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json":   complete.PredictNothing,
			"-prefix": complete.PredictAnything,
			"-t":      complete.PredictAnything,
		})
}

func (c *ObjectKindListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ObjectKindListCommand) Run(args []string) int {
	var jsonOut bool
	var tmpl, prefix string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
	flags.StringVar(&prefix, "prefix", "", "")
	flags.StringVar(&tmpl, "t", "", "")
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

	kinds, _, err := client.ObjectKinds().List(&api.QueryOptions{Prefix: prefix})
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying object kinds: %s", err))
		return 1
	}

	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, kinds)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	if len(kinds) == 0 {
		c.Ui.Output("No object kinds found")
		return 0
	}
	rows := []string{"Kind|Namespaced|Rules|Description"}
	for _, k := range kinds {
		rows = append(rows, fmt.Sprintf("%s|%t|%d|%s", k.KindRef(), k.Namespaced, len(k.Rules), k.Description))
	}
	c.Ui.Output(formatList(rows))
	return 0
}

// ObjectKindInspectCommand shows an object kind.
type ObjectKindInspectCommand struct {
	Meta
}

func (c *ObjectKindInspectCommand) Name() string { return "object-kind inspect" }

func (c *ObjectKindInspectCommand) Synopsis() string {
	return "Inspect an object kind"
}

func (c *ObjectKindInspectCommand) Help() string {
	helpText := `
Usage: norca object-kind inspect [options] <group>/<Kind>

  Inspect is used to show the details of an object kind, including its
  schema.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Inspect Options:

  -json
    Output the object kind in JSON format.

  -t
    Format and display the object kind using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindInspectCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *ObjectKindInspectCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectKindInspectCommand) Run(args []string) int {
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
		c.Ui.Error("This command takes one argument: <group>/<Kind>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, name, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	kind, _, err := client.ObjectKinds().Info(group, name, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading object kind: %s", err))
		return 1
	}

	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, kind)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	basic := []string{
		fmt.Sprintf("Kind|%s", kind.KindRef()),
		fmt.Sprintf("Description|%s", kind.Description),
		fmt.Sprintf("Namespaced|%t", kind.Namespaced),
		fmt.Sprintf("Create Index|%d", kind.CreateIndex),
		fmt.Sprintf("Modify Index|%d", kind.ModifyIndex),
	}
	if kind.Scale != nil {
		basic = append(basic, fmt.Sprintf("Scale|%s -> %s", kind.Scale.SpecPath, kind.Scale.StatusPath))
	}
	out := formatKV(basic)

	if len(kind.Rules) > 0 {
		rows := []string{"#|Expression|Message"}
		for i, r := range kind.Rules {
			rows = append(rows, fmt.Sprintf("%d|%s|%s", i, r.Expression, r.Message))
		}
		out += "\n\n[bold]Rules[reset]\n" + formatList(rows)
	}
	if len(kind.Columns) > 0 {
		rows := []string{"Name|Path"}
		for _, col := range kind.Columns {
			rows = append(rows, fmt.Sprintf("%s|%s", col.Name, col.Path))
		}
		out += "\n\n[bold]Columns[reset]\n" + formatList(rows)
	}
	out += "\n\n[bold]Schema[reset]\n" + prettyJSONString(kind.Schema)
	c.Ui.Output(c.Colorize().Color(out))
	return 0
}

// ObjectKindDeleteCommand deletes an object kind.
type ObjectKindDeleteCommand struct {
	Meta
}

func (c *ObjectKindDeleteCommand) Name() string { return "object-kind delete" }

func (c *ObjectKindDeleteCommand) Synopsis() string {
	return "Delete an object kind"
}

func (c *ObjectKindDeleteCommand) Help() string {
	helpText := `
Usage: norca object-kind delete [options] <group>/<Kind>

  Delete is used to delete an object kind. A kind that still has objects
  can't be deleted; delete its objects first.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of the 'operator' policy.

General Options:

  ` + generalOptionsUsage(usageOptsDefault)
	return strings.TrimSpace(helpText)
}

func (c *ObjectKindDeleteCommand) AutocompleteFlags() complete.Flags {
	return c.Meta.AutocompleteFlags(FlagSetClient)
}

func (c *ObjectKindDeleteCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectKindDeleteCommand) Run(args []string) int {
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <group>/<Kind>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, name, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	if _, err := client.ObjectKinds().Delete(group, name, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting object kind: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Successfully deleted object kind %q!", args[0]))
	return 0
}

// prettyJSONString indents a JSON document, or returns it unchanged if it
// isn't valid JSON.
func prettyJSONString(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "    "); err != nil {
		return s
	}
	return buf.String()
}
