// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

type OperatorMetricsStoreCommand struct {
	Meta
}

func (c *OperatorMetricsStoreCommand) Name() string { return "operator metrics-store" }

func (c *OperatorMetricsStoreCommand) Run(args []string) int {
	return cli.RunResultHelp
}

func (c *OperatorMetricsStoreCommand) Synopsis() string {
	return "Provides tools for the metrics store of the servers"
}

func (c *OperatorMetricsStoreCommand) Help() string {
	helpText := `
Usage: norca operator metrics-store <subcommand> [options]

  This command groups subcommands for the metrics store: the history of the
  task group usage the clients publish on the system bus, which every server
  keeps in its data directory.

  Show the state of the metrics store of the server answering the request:

      $ norca operator metrics-store status

  Please see the individual subcommand help for detailed usage information.
  `
	return strings.TrimSpace(helpText)
}

type OperatorMetricsStoreStatusCommand struct {
	Meta
}

func (c *OperatorMetricsStoreStatusCommand) Name() string { return "operator metrics-store status" }

func (c *OperatorMetricsStoreStatusCommand) Synopsis() string {
	return "Display the state of the metrics store of a server"
}

func (c *OperatorMetricsStoreStatusCommand) Help() string {
	helpText := `
Usage: norca operator metrics-store status [options]

  Displays the state of the metrics store of one server: its partitions, the
  disk use, the series and the time range of the stored points. Without
  -server, the server answering the request reports.

  If ACLs are enabled, this command requires a token with the 'operator:read'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Metrics Store Status Options:

  -server=<name or ID>
    The server of the region to report.

  -json
    Output the state in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorMetricsStoreStatusCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-server": complete.PredictAnything,
			"-json":   complete.PredictNothing,
		})
}

func (c *OperatorMetricsStoreStatusCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorMetricsStoreStatusCommand) Run(args []string) int {
	var serverID string
	var jsonOutput bool
	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&serverID, "server", "", "")
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
	status, _, err := client.Operator().MetricsStoreStatus(serverID, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying the metrics store: %s", err))
		return 1
	}

	if jsonOutput {
		f, err := DataFormat("json", "")
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error getting formatter: %s", err))
			return 1
		}
		out, err := f.TransformData(status)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error formatting the data: %s", err))
			return 1
		}
		c.Ui.Output(out)
		return 0
	}

	c.Ui.Output(formatMetricsStoreStatus(status))
	return 0
}

func formatMetricsStoreStatus(st *api.MetricsStoreStatus) string {
	if !st.Enabled {
		return fmt.Sprintf("The metrics store is disabled on server %s", st.Server)
	}
	timeOrNone := func(t interface{ IsZero() bool }, s string) string {
		if t.IsZero() {
			return "<none>"
		}
		return s
	}
	maxSeries := "unlimited"
	if st.MaxSeries > 0 {
		maxSeries = fmt.Sprint(st.MaxSeries)
	}
	maxDisk := "unlimited"
	if st.MaxDiskSize > 0 {
		maxDisk = humanize.IBytes(uint64(st.MaxDiskSize))
	}
	return formatKV([]string{
		fmt.Sprintf("Server|%s", st.Server),
		fmt.Sprintf("Partitions|%d (%d on disk)", st.Partitions, st.DiskPartitions),
		fmt.Sprintf("Disk|%s of %s", humanize.IBytes(uint64(st.DiskBytes)), maxDisk),
		fmt.Sprintf("Series|%d of %s", st.Series, maxSeries),
		fmt.Sprintf("Oldest Point|%s", timeOrNone(st.Oldest, formatTime(st.Oldest))),
		fmt.Sprintf("Newest Point|%s", timeOrNone(st.Newest, formatTime(st.Newest))),
		fmt.Sprintf("Retention|%s", st.Retention),
		fmt.Sprintf("Sample Interval|%s", st.SampleInterval),
		fmt.Sprintf("Dropped Reports|%d", st.DroppedReports),
		fmt.Sprintf("Dropped Groups|%d", st.DroppedGroups),
	})
}
