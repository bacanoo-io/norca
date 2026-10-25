// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

// formatOwnerRef formats an owner reference as "Kind namespace/name".
func formatOwnerRef(ref *api.ObjectOwnerReference) string {
	if ref.Namespace == "" {
		return ref.Kind + " " + ref.Name
	}
	return ref.Kind + " " + ref.Namespace + "/" + ref.Name
}

// formatObjectEvents formats the event log of an object, oldest first.
func formatObjectEvents(events []*api.ObjectEvent) string {
	rows := []string{"Last Seen|Type|Reason|Count|Reporter|Message"}
	for _, e := range events {
		last := "-"
		if e.LastTime > 0 {
			last = formatTimeDifference(time.Unix(0, e.LastTime), time.Now(), time.Second) + " ago"
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%d|%s|%s", last, e.Type, e.Reason, e.Count,
			strings.ReplaceAll(e.Reporter, "|", "/"), strings.ReplaceAll(e.Message, "|", "/")))
	}
	return formatList(rows)
}

// ObjectEventsCommand shows the event log of an object.
type ObjectEventsCommand struct {
	Meta
}

func (c *ObjectEventsCommand) Name() string { return "object events" }

func (c *ObjectEventsCommand) Synopsis() string {
	return "Show the event log of a custom object"
}

func (c *ObjectEventsCommand) Help() string {
	helpText := `
Usage: norca object events [options] <group>/<Kind> <name>

  Events shows the event log of a custom object: the last events recorded
  by its controllers and by the server (for example scaling), oldest first.
  Repeated events are counted.

  If ACLs are enabled, this command requires a token with the 'read'
  capability of an object rule matching the kind in the namespace.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Events Options:

  -json
    Output the events in JSON format.

  -t
    Format and display the events using a Go template.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectEventsCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-json": complete.PredictNothing,
			"-t":    complete.PredictAnything,
		})
}

func (c *ObjectEventsCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

func (c *ObjectEventsCommand) Run(args []string) int {
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
	events, _, err := client.Objects().Events(group, kindName, args[1], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading object events: %s", err))
		return 1
	}

	if jsonOut || tmpl != "" {
		out, err := Format(jsonOut, tmpl, events)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	if len(events) == 0 {
		c.Ui.Output("No events found")
		return 0
	}
	c.Ui.Output(formatObjectEvents(events))
	return 0
}

// ObjectScaleCommand shows or sets the scale of an object.
type ObjectScaleCommand struct {
	Meta
}

func (c *ObjectScaleCommand) Name() string { return "object scale" }

func (c *ObjectScaleCommand) Synopsis() string {
	return "Show or set the replica count of a custom object"
}

func (c *ObjectScaleCommand) Help() string {
	helpText := `
Usage: norca object scale [options] <group>/<Kind>/<name> [<count>]
       norca object scale [options] <group>/<Kind> <name> [<count>]

  Scale shows or sets the replica count of a custom object, through the
  scale subresource of its kind (the "scale" block of the kind definition).
  The new count is written to the spec path of the kind, validated like any
  spec change, and recorded in the object's event log. The managed_by
  identity of the object is not enforced, so the count of an object
  managed by another tool can be changed.

  If ACLs are enabled, this command requires a token with the 'write'
  capability of an object rule matching the kind in the namespace ('read'
  to show the scale).

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Scale Options:

  -message <text>
    Message recorded in the object's event log.

  -cas <index>
    Only write if the stored object has this modify index.

  -json
    Output the scale in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *ObjectScaleCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-message": complete.PredictAnything,
			"-cas":     complete.PredictAnything,
			"-json":    complete.PredictNothing,
		})
}

func (c *ObjectScaleCommand) AutocompleteArgs() complete.Predictor {
	return objectKindPredictor(c.Meta.Client)
}

// parseObjectScaleArgs accepts "<group>/<Kind>/<name> [count]" and
// "<group>/<Kind> <name> [count]".
func parseObjectScaleArgs(args []string) (group, kind, name string, count *int64, err error) {
	if len(args) == 0 || len(args) > 3 {
		return "", "", "", nil, fmt.Errorf("This command takes the object and optionally a count")
	}
	var rest []string
	if parts := strings.Split(args[0], "/"); len(parts) == 3 {
		group, kind, name = parts[0], parts[1], parts[2]
		rest = args[1:]
	} else {
		if len(args) < 2 {
			return "", "", "", nil, fmt.Errorf("missing object name")
		}
		if group, kind, err = splitKindRef(args[0]); err != nil {
			return "", "", "", nil, err
		}
		name, rest = args[1], args[2:]
	}
	if group == "" || kind == "" || name == "" {
		return "", "", "", nil, fmt.Errorf("invalid object %q", strings.Join(args, " "))
	}
	switch len(rest) {
	case 0:
	case 1:
		n, perr := strconv.ParseInt(rest[0], 10, 64)
		if perr != nil || n < 0 {
			return "", "", "", nil, fmt.Errorf("invalid count %q, must be a non-negative integer", rest[0])
		}
		count = &n
	default:
		return "", "", "", nil, fmt.Errorf("too many arguments")
	}
	return group, kind, name, count, nil
}

func (c *ObjectScaleCommand) Run(args []string) int {
	var jsonOut bool
	var message, casFlag string

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.BoolVar(&jsonOut, "json", false, "")
	flags.StringVar(&message, "message", "", "")
	flags.StringVar(&casFlag, "cas", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	group, kindName, name, count, err := parseObjectScaleArgs(flags.Args())
	if err != nil {
		c.Ui.Error(err.Error())
		c.Ui.Error(commandErrorText(c))
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

	var scale *api.ObjectScale
	if count == nil {
		scale, _, err = client.Objects().Scale(group, kindName, name, nil)
	} else {
		scale, _, err = client.Objects().SetScale(group, kindName, name, *count, message, &api.ObjectWriteOptions{CAS: cas}, nil)
	}
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error scaling object: %s", objectWriteErrorHint(err)))
		return 1
	}

	if jsonOut {
		out, err := Format(true, "", scale)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting output: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}
	observed := "-"
	if scale.StatusReplicas != nil {
		observed = strconv.FormatInt(*scale.StatusReplicas, 10)
	}
	c.Ui.Output(formatKV([]string{
		fmt.Sprintf("Object|%s/%s/%s", scale.Group, scale.Kind, scale.Name),
		fmt.Sprintf("Replicas|%d", scale.Replicas),
		fmt.Sprintf("Observed Replicas|%s", observed),
		fmt.Sprintf("Spec Path|%s", scale.SpecPath),
		fmt.Sprintf("Generation|%d", scale.Generation),
	}))
	return 0
}
