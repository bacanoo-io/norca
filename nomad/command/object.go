// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type ObjectCommand struct {
	Meta
}

func (c *ObjectCommand) Name() string {
	return "object"
}

func (c *ObjectCommand) Synopsis() string {
	return "Interact with custom objects"
}

func (c *ObjectCommand) Help() string {
	helpText := `
Usage: norca object <subcommand> [options] [args]

  This command groups subcommands for managing custom objects: objects of
  a kind registered with "norca object-kind apply". Users write the spec of
  an object; controllers write its status.

  Create or update an object:

      $ norca object apply billing.hcl

  List the objects of a kind:

      $ norca object list db.example.com/PostgresCluster

  Show an object:

      $ norca object get db.example.com/PostgresCluster billing

  Delete an object:

      $ norca object delete db.example.com/PostgresCluster billing

  Show the event log of an object:

      $ norca object events db.example.com/PostgresCluster billing

  Scale an object (kinds with a scale subresource):

      $ norca object scale db.example.com/PostgresCluster/billing 5

  Please see the individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectCommand) Run(_ []string) int {
	return cli.RunResultHelp
}

// parseCASFlag parses the -cas flag value; an empty value means no
// check-and-set.
func parseCASFlag(s string) (*uint64, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid -cas value %q: %v", s, err)
	}
	return &v, nil
}

// objectWriteErrorHint adds a hint to a check-and-set conflict.
func objectWriteErrorHint(err error) string {
	if errors.Is(err, api.ErrObjectConflict) {
		return fmt.Sprintf("%s\nThe object was modified since it was read; read it again and retry.", err)
	}
	return err.Error()
}

// ObjectApplyCommand creates or updates an object.
type ObjectApplyCommand struct {
	Meta
}

func (c *ObjectApplyCommand) Name() string { return "object apply" }

func (c *ObjectApplyCommand) Synopsis() string {
	return "Create or update a custom object"
}

func (c *ObjectApplyCommand) Help() string {
	helpText := `
Usage: norca object apply [options] <input>

  Apply is used to create or update the spec and the metadata of a custom
  object. The specification file is read from stdin by specifying "-",
  otherwise a path to the file is expected. The server fills in the default
  values of the kind's schema, validates the object against the schema and
  the kind's rules, and increases the generation if the spec changed. The
  status written by controllers is kept. Labels and managed_by are
  replaced by the values in the file.

  Example specification:

    kind       = "db.example.com/PostgresCluster"
    name       = "billing"
    namespace  = "prod"
    labels     = { team = "finance" }
    managed_by = "gitops"

    spec {
      version  = "16"
      replicas = 3
      storage  = "100GiB"
      backup { schedule = "0 2 * * *" }
    }

  If ACLs are enabled, this command requires a token with the 'write'
  capability of an object rule matching the kind in the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Apply Options:

  -cas <index>
    Only write if the stored object has this modify index (0: only create
    the object). On a mismatch the write fails with a conflict.

  -dry-run
    Fill in the defaults and validate the object, and show the result
    without writing it.

  -json
    Parse the input as a JSON object in the API form (also used for .json
    files).
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-cas":     complete.PredictAnything,
			"-dry-run": complete.PredictNothing,
			"-json":    complete.PredictNothing,
		})
}

func (c *ObjectApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(complete.PredictFiles("*.hcl"), complete.PredictFiles("*.json"))
}

func (c *ObjectApplyCommand) Run(args []string) int {
	var jsonInput, dryRun bool
	var casFlag string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonInput, "json", false, "")
	flags.BoolVar(&dryRun, "dry-run", false, "")
	flags.StringVar(&casFlag, "cas", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <input>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	cas, err := parseCASFlag(casFlag)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	content, path, err := readObjectInput(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	obj, err := parseCustomObject(path, content, jsonInput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse input content: %v", err))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	out, _, err := client.Objects().Apply(obj, &api.ObjectWriteOptions{CAS: cas, DryRun: dryRun}, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying object: %s", objectWriteErrorHint(err)))
		return 1
	}

	ref := out.KindRef() + "/" + out.Name
	if dryRun {
		c.Ui.Output(fmt.Sprintf("Object %q is valid (dry run, nothing was written):\n", ref))
		c.Ui.Output(formatObject(out, nil))
		return 0
	}
	c.Ui.Output(fmt.Sprintf("Successfully applied object %q (generation %d, modify index %d)!",
		ref, out.Generation, out.ModifyIndex))
	return 0
}

// ObjectListCommand lists the objects of a kind.
type ObjectListCommand struct {
	Meta
}

func (c *ObjectListCommand) Name() string { return "object list" }

func (c *ObjectListCommand) Synopsis() string {
	return "List the custom objects of a kind"
}

func (c *ObjectListCommand) Help() string {
	helpText := `
Usage: norca object list [options] <group>/<Kind>

  List is used to list the custom objects of a kind, with the display
  columns of the kind. Use -namespace=* to list the objects of all
  namespaces.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of an object rule matching the kind.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

List Options:

  -label <key>=<value>
    Only list objects with the label. Can be given several times; objects
    must have all the labels.

  -filter
    Specifies an expression used to filter query results.

  -per-page
    How many results to show per page.

  -page-token
    Where to start pagination.

  -json
    Output the objects in JSON format.

  -t
    Format and display the objects using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectListCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-label":      complete.PredictAnything,
			"-filter":     complete.PredictAnything,
			"-per-page":   complete.PredictAnything,
			"-page-token": complete.PredictAnything,
			"-json":       complete.PredictNothing,
			"-t":          complete.PredictAnything,
		})
}

func (c *ObjectListCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectListCommand) Run(args []string) int {
	var jsonOut bool
	var perPage int
	var tmpl, filter, pageToken string
	var labels flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
	flags.StringVar(&tmpl, "t", "", "")
	flags.StringVar(&filter, "filter", "", "")
	flags.IntVar(&perPage, "per-page", 0, "")
	flags.StringVar(&pageToken, "page-token", "", "")
	flags.Var(&labels, "label", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 1 {
		c.Ui.Error("This command takes one argument: <group>/<Kind>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, kindName, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	var opts *api.ObjectListOptions
	for _, l := range labels {
		key, value, ok := strings.Cut(l, "=")
		if !ok || key == "" {
			c.Ui.Error(fmt.Sprintf("Invalid label %q, must be <key>=<value>", l))
			return 1
		}
		if opts == nil {
			opts = &api.ObjectListOptions{Labels: map[string]string{}}
		}
		opts.Labels[key] = value
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	q := &api.QueryOptions{Filter: filter, PerPage: int32(perPage), NextToken: pageToken}
	objects, qm, err := client.Objects().List(group, kindName, opts, q)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error listing objects: %s", err))
		return 1
	}

	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, objects)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	if len(objects) == 0 {
		c.Ui.Output("No objects found")
		return 0
	}

	// The columns come from the kind; a token without access to the kind
	// definition gets the listing without them.
	var columns []*api.ObjectKindColumn
	if kind, _, err := client.ObjectKinds().Info(group, kindName, nil); err == nil {
		columns = kind.Columns
	}
	c.Ui.Output(formatObjectList(objects, columns))

	if qm.NextToken != "" {
		c.Ui.Output(fmt.Sprintf(`
Results have been paginated. To get the next page run:

%s -page-token %s`, argsWithoutPageToken(os.Args), qm.NextToken))
	}
	return 0
}

// formatObjectList formats objects as a table with the kind's columns.
func formatObjectList(objects []*api.CustomObject, columns []*api.ObjectKindColumn) string {
	header := []string{"Name", "Namespace"}
	for _, col := range columns {
		header = append(header, col.Name)
	}
	header = append(header, "Generation", "Observed", "Age")
	rows := []string{strings.Join(header, "|")}

	for _, o := range objects {
		row := []string{o.Name, o.Namespace}
		if o.Namespace == "" {
			row[1] = "<cluster>"
		}
		for _, col := range columns {
			v, err := o.Field(col.Path)
			cell := api.FormatObjectValue(v)
			if err != nil {
				cell = "<error>"
			}
			row = append(row, strings.ReplaceAll(cell, "|", "/"))
		}
		age := "-"
		if o.CreateTime > 0 {
			age = formatTimeDifference(time.Unix(0, o.CreateTime), time.Now(), time.Second)
		}
		row = append(row, strconv.FormatUint(o.Generation, 10), strconv.FormatUint(o.ObservedGeneration(), 10), age)
		rows = append(rows, strings.Join(row, "|"))
	}
	return formatList(rows)
}

// ObjectGetCommand shows an object.
type ObjectGetCommand struct {
	Meta
}

func (c *ObjectGetCommand) Name() string { return "object get" }

func (c *ObjectGetCommand) Synopsis() string {
	return "Show a custom object"
}

func (c *ObjectGetCommand) Help() string {
	helpText := `
Usage: norca object get [options] <group>/<Kind> <name>

  Get is used to show a custom object: its metadata, spec, status and
  conditions.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of an object rule matching the kind in the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Get Options:

  -json
    Output the object in JSON format.

  -t
    Format and display the object using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectGetCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *ObjectGetCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectGetCommand) Run(args []string) int {
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
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <group>/<Kind> <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, kindName, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	obj, _, err := client.Objects().Get(group, kindName, args[1], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading object: %s", err))
		return 1
	}

	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, obj)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	var columns []*api.ObjectKindColumn
	if kind, _, err := client.ObjectKinds().Info(group, kindName, nil); err == nil {
		columns = kind.Columns
	}
	out := formatObject(obj, columns)
	// The event log is shown when the token may read it.
	if events, _, err := client.Objects().Events(group, kindName, args[1], nil); err == nil && len(events) > 0 {
		out += "\n\n[bold]Recent Events[reset]\n" + formatObjectEvents(events)
	}
	c.Ui.Output(c.Colorize().Color(out))
	return 0
}

// formatObject formats an object for display.
func formatObject(o *api.CustomObject, columns []*api.ObjectKindColumn) string {
	namespace := o.Namespace
	if namespace == "" {
		namespace = "<cluster>"
	}
	basic := []string{
		fmt.Sprintf("Kind|%s", o.KindRef()),
		fmt.Sprintf("Name|%s", o.Name),
		fmt.Sprintf("Namespace|%s", namespace),
		fmt.Sprintf("ID|%s", o.ID),
		fmt.Sprintf("Generation|%d", o.Generation),
		fmt.Sprintf("Observed Generation|%d", o.ObservedGeneration()),
	}
	if o.ManagedBy != "" {
		basic = append(basic, fmt.Sprintf("Managed By|%s", o.ManagedBy))
	}
	if len(o.Owners) > 0 {
		owners := make([]string, 0, len(o.Owners))
		for _, ref := range o.Owners {
			if ref != nil {
				owners = append(owners, formatOwnerRef(ref))
			}
		}
		basic = append(basic, fmt.Sprintf("Owners|%s", strings.Join(owners, ",")))
	}
	if len(o.Finalizers) > 0 {
		basic = append(basic, fmt.Sprintf("Finalizers|%s", strings.Join(o.Finalizers, ",")))
	}
	if o.Deleting() {
		basic = append(basic, fmt.Sprintf("Deleting|since %s (%s)",
			formatTime(time.Unix(0, o.DeletionTime)), o.DeletionPropagation))
	}
	if len(o.Labels) > 0 {
		keys := make([]string, 0, len(o.Labels))
		for k := range o.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = k + "=" + o.Labels[k]
		}
		basic = append(basic, fmt.Sprintf("Labels|%s", strings.Join(pairs, ",")))
	}
	for _, col := range columns {
		v, err := o.Field(col.Path)
		if err == nil {
			basic = append(basic, fmt.Sprintf("%s|%s", col.Name, api.FormatObjectValue(v)))
		}
	}
	if o.CreateTime > 0 {
		basic = append(basic, fmt.Sprintf("Created|%s", formatTime(time.Unix(0, o.CreateTime))))
	}
	if o.ModifyTime > 0 {
		basic = append(basic, fmt.Sprintf("Modified|%s", formatTime(time.Unix(0, o.ModifyTime))))
	}
	basic = append(basic,
		fmt.Sprintf("Create Index|%d", o.CreateIndex),
		fmt.Sprintf("Modify Index|%d", o.ModifyIndex))
	out := formatKV(basic)

	if conditions := o.Conditions(); len(conditions) > 0 {
		rows := []string{"Type|Status|Reason|Message|Last Transition"}
		for _, cond := range conditions {
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s",
				cond.Type, cond.Status, cond.Reason, strings.ReplaceAll(cond.Message, "|", "/"), cond.LastTransitionTime))
		}
		out += "\n\n[bold]Conditions[reset]\n" + formatList(rows)
	}

	out += "\n\n[bold]Spec[reset]\n" + indentedJSON(o.Spec)
	if o.Status != nil {
		out += "\n\n[bold]Status[reset]\n" + indentedJSON(o.Status)
	}
	return out
}

func indentedJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "    ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(buf.String())
}

// ObjectDeleteCommand deletes an object.
type ObjectDeleteCommand struct {
	Meta
}

func (c *ObjectDeleteCommand) Name() string { return "object delete" }

func (c *ObjectDeleteCommand) Synopsis() string {
	return "Delete a custom object"
}

func (c *ObjectDeleteCommand) Help() string {
	helpText := `
Usage: norca object delete [options] <group>/<Kind> <name>

  Delete is used to delete a custom object. An object with finalizers is
  only marked for deletion; it disappears when its controllers have
  removed the finalizers (after cleaning up). The objects and jobs the
  object owns are deleted according to the propagation mode.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of an object rule matching the kind in the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Delete Options:

  -cas <index>
    Only delete if the stored object has this modify index.

  -dry-run
    Check that the object can be deleted without deleting it.

  -propagation <mode>
    What happens to the objects and jobs the object owns: "background"
    (default) deletes the object at once and them afterwards, "foreground"
    deletes them first and the object afterwards, "orphan" leaves them in
    place.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectDeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-cas":         complete.PredictAnything,
			"-dry-run":     complete.PredictNothing,
			"-propagation": complete.PredictSet(api.ObjectPropagationBackground, api.ObjectPropagationForeground, api.ObjectPropagationOrphan),
		})
}

func (c *ObjectDeleteCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectDeleteCommand) Run(args []string) int {
	var dryRun bool
	var casFlag, propagation string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&dryRun, "dry-run", false, "")
	flags.StringVar(&casFlag, "cas", "", "")
	flags.StringVar(&propagation, "propagation", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	args = flags.Args()
	if len(args) != 2 {
		c.Ui.Error("This command takes two arguments: <group>/<Kind> <name>")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	group, kindName, err := splitKindRef(args[0])
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	cas, err := parseCASFlag(casFlag)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	opts := &api.ObjectWriteOptions{CAS: cas, DryRun: dryRun, Propagation: propagation}
	obj, _, err := client.Objects().Delete(group, kindName, args[1], opts, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting object: %s", objectWriteErrorHint(err)))
		return 1
	}
	ref := args[0] + "/" + args[1]
	if dryRun {
		c.Ui.Output(fmt.Sprintf("Object %q can be deleted (dry run, nothing was deleted)", ref))
		return 0
	}
	if obj != nil && obj.Deleting() {
		msg := fmt.Sprintf("Object %q is marked for deletion (%s)", ref, obj.DeletionPropagation)
		if len(obj.Finalizers) > 0 {
			msg += fmt.Sprintf("; waiting for the finalizers %s", strings.Join(obj.Finalizers, ", "))
		}
		if obj.DeletionPropagation == api.ObjectPropagationForeground {
			msg += "; the objects and jobs it owns are deleted first"
		}
		c.Ui.Output(msg)
		return 0
	}
	c.Ui.Output(fmt.Sprintf("Successfully deleted object %q!", ref))
	return 0
}
