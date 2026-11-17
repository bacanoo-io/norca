// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	flaghelper "github.com/bacanoo-io/norca/norca/helper/flags"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

// CDCCommand groups the change data capture subcommands.
type CDCCommand struct {
	Meta
}

func (c *CDCCommand) Name() string { return "cdc" }

func (c *CDCCommand) Run(args []string) int { return cli.RunResultHelp }

func (c *CDCCommand) Synopsis() string {
	return "Provides tools for change data capture"
}

func (c *CDCCommand) Help() string {
	helpText := `
Usage: norca cdc <subcommand> [options]

  Change data capture delivers the changes of a region (the events, the
  admin events and the Consul KV notices) one way to readers outside the
  servers, through the system bus. A reader's token or workload identity
  has a cdc rule; the servers keep one consumer per reader and stream.

Subcommands:

  consumer apply    Create or change the reader's consumer of a stream
  consumer read     Show the reader's consumer of a stream
  consumer delete   Delete the reader's consumer of a stream
`
	return strings.TrimSpace(helpText)
}

// CDCConsumerCommand groups the consumer subcommands.
type CDCConsumerCommand struct {
	Meta
}

func (c *CDCConsumerCommand) Name() string { return "cdc consumer" }

func (c *CDCConsumerCommand) Run(args []string) int { return cli.RunResultHelp }

func (c *CDCConsumerCommand) Synopsis() string {
	return "Manage the reader's consumers of the change streams"
}

func (c *CDCConsumerCommand) Help() string {
	return (&CDCCommand{}).Help()
}

const cdcConsumerStreamHelp = `
  -stream=<events|admin|kv>
    The change stream: the events of the namespaces (default), the admin
    events (management tokens only) or the Consul KV notices.

  -json
    Output the consumer in JSON format.`

// CDCConsumerApplyCommand creates or changes the reader's consumer.
type CDCConsumerApplyCommand struct {
	Meta
}

func (c *CDCConsumerApplyCommand) Name() string { return "cdc consumer apply" }

func (c *CDCConsumerApplyCommand) Synopsis() string {
	return "Create or change the reader's consumer of a change stream"
}

func (c *CDCConsumerApplyCommand) Help() string {
	helpText := `
Usage: norca cdc consumer apply [options] -filter=<subject> [-filter=...]

  Creates the consumer the servers keep for the reader (the token of the
  command, or the workload identity in NORCA_TOKEN) of a change stream, or
  changes its filters: a change keeps its position. Every filter lies within
  the reader's cdc rule; the filters do not overlap. The reader reads the
  consumer on the system bus by its name (the accessor ID of the token).

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Apply Options:

  -filter=<subject>
    A subject of the stream to deliver, e.g. norca.eu.games.Allocation. May
    be given up to 16 times.

  -deliver=<all|new|last-per-subject|by-start-time>
    Where a new consumer starts (default all); it can't change later.

  -start-time=<RFC3339 time>
    The start of -deliver=by-start-time.

  -ack-wait=<duration>
    How long a delivered event waits for its acknowledgement (default 30s).

  -max-ack-pending=<n>
    The events delivered and not acknowledged (default and limit 1000).
` + cdcConsumerStreamHelp
	return strings.TrimSpace(helpText)
}

func (c *CDCConsumerApplyCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-stream":          complete.PredictSet(api.CDCStreamEvents, api.CDCStreamAdmin, api.CDCStreamKV),
			"-filter":          complete.PredictAnything,
			"-deliver":         complete.PredictSet("all", "new", "last-per-subject", "by-start-time"),
			"-start-time":      complete.PredictAnything,
			"-ack-wait":        complete.PredictAnything,
			"-max-ack-pending": complete.PredictAnything,
			"-json":            complete.PredictNothing,
		})
}

func (c *CDCConsumerApplyCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CDCConsumerApplyCommand) Run(args []string) int {
	var stream, deliver, startTime string
	var ackWait time.Duration
	var maxAckPending int
	var filters []string
	var jsonOutput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&stream, "stream", api.CDCStreamEvents, "")
	flags.Var((*flaghelper.StringFlag)(&filters), "filter", "")
	flags.StringVar(&deliver, "deliver", "", "")
	flags.StringVar(&startTime, "start-time", "", "")
	flags.DurationVar(&ackWait, "ack-wait", 0, "")
	flags.IntVar(&maxAckPending, "max-ack-pending", 0, "")
	flags.BoolVar(&jsonOutput, "json", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	spec := &api.CDCConsumerSpec{Filters: filters, Deliver: deliver, AckWait: ackWait, MaxAckPending: maxAckPending}
	if startTime != "" {
		t, err := time.Parse(time.RFC3339, startTime)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Invalid -start-time: %v", err))
			return 1
		}
		spec.StartTime = t
	}
	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	consumer, _, err := client.CDC().UpsertConsumer(stream, spec, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error applying the consumer: %s", err))
		return 1
	}
	return outputCDCConsumer(c.Meta, consumer, jsonOutput)
}

// CDCConsumerReadCommand shows the reader's consumer.
type CDCConsumerReadCommand struct {
	Meta
}

func (c *CDCConsumerReadCommand) Name() string { return "cdc consumer read" }

func (c *CDCConsumerReadCommand) Synopsis() string {
	return "Show the reader's consumer of a change stream"
}

func (c *CDCConsumerReadCommand) Help() string {
	helpText := `
Usage: norca cdc consumer read [options]

  Shows the consumer the servers keep for the reader of a change stream:
  its filters and how far it has read.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Read Options:
` + cdcConsumerStreamHelp
	return strings.TrimSpace(helpText)
}

func (c *CDCConsumerReadCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-stream": complete.PredictSet(api.CDCStreamEvents, api.CDCStreamAdmin, api.CDCStreamKV),
			"-json":   complete.PredictNothing,
		})
}

func (c *CDCConsumerReadCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CDCConsumerReadCommand) Run(args []string) int {
	var stream string
	var jsonOutput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&stream, "stream", api.CDCStreamEvents, "")
	flags.BoolVar(&jsonOutput, "json", false, "")
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
	consumer, _, err := client.CDC().ReadConsumer(stream, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error reading the consumer: %s", err))
		return 1
	}
	return outputCDCConsumer(c.Meta, consumer, jsonOutput)
}

// CDCConsumerDeleteCommand deletes the reader's consumer.
type CDCConsumerDeleteCommand struct {
	Meta
}

func (c *CDCConsumerDeleteCommand) Name() string { return "cdc consumer delete" }

func (c *CDCConsumerDeleteCommand) Synopsis() string {
	return "Delete the reader's consumer of a change stream"
}

func (c *CDCConsumerDeleteCommand) Help() string {
	helpText := `
Usage: norca cdc consumer delete [options]

  Deletes the consumer the servers keep for the reader of a change stream.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Delete Options:

  -stream=<events|admin|kv>
    The change stream (default events).
`
	return strings.TrimSpace(helpText)
}

func (c *CDCConsumerDeleteCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-stream": complete.PredictSet(api.CDCStreamEvents, api.CDCStreamAdmin, api.CDCStreamKV),
		})
}

func (c *CDCConsumerDeleteCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *CDCConsumerDeleteCommand) Run(args []string) int {
	var stream string
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&stream, "stream", api.CDCStreamEvents, "")
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
	if _, err := client.CDC().DeleteConsumer(stream, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error deleting the consumer: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Deleted the consumer of the stream %q", stream))
	return 0
}

// outputCDCConsumer prints a consumer.
func outputCDCConsumer(meta Meta, consumer *api.CDCConsumer, jsonOutput bool) int {
	if jsonOutput {
		out, err := Format(true, "", consumer)
		if err != nil {
			meta.Ui.Error(err.Error())
			return 1
		}
		meta.Ui.Output(out)
		return 0
	}
	meta.Ui.Output(formatKV([]string{
		fmt.Sprintf("Stream|%s (%s)", consumer.Stream, consumer.StreamName),
		fmt.Sprintf("Name|%s", consumer.Name),
		fmt.Sprintf("Filters|%s", strings.Join(consumer.Filters, ", ")),
		fmt.Sprintf("Deliver|%s", consumer.Deliver),
		fmt.Sprintf("Ack Wait|%s", consumer.AckWait),
		fmt.Sprintf("Max Ack Pending|%d", consumer.MaxAckPending),
		fmt.Sprintf("Pending|%d", consumer.NumPending),
		fmt.Sprintf("Ack Pending|%d", consumer.NumAckPending),
		fmt.Sprintf("Delivered (stream sequence)|%d", consumer.Delivered),
	}))
	return 0
}
