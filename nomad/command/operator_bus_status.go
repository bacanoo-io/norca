// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/api"
	"github.com/posener/complete"
)

type OperatorBusStatusCommand struct {
	Meta
}

func (c *OperatorBusStatusCommand) Name() string { return "operator bus status" }

func (c *OperatorBusStatusCommand) Synopsis() string {
	return "Display the status of the system bus of a server"
}

func (c *OperatorBusStatusCommand) Help() string {
	helpText := `
Usage: norca operator bus status [options]

  Displays the status of the system bus of one server: its NATS cluster and
  routes, JetStream (meta leader, usage and limits) and the client
  connections. Without -server, the server answering the request reports.

  If ACLs are enabled, this command requires a token with the 'operator:read'
  capability.

General Options:

  ` + generalOptionsUsage(usageOptsDefault|usageOptsNoNamespace) + `

Bus Status Options:

  -server=<name or ID>
    The server of the region to report.

  -json
    Output the status in JSON format.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBusStatusCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-server": complete.PredictAnything,
			"-json":   complete.PredictNothing,
		})
}

func (c *OperatorBusStatusCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorBusStatusCommand) Run(args []string) int {
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
	status, _, err := client.Operator().BusStatus(serverID, nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying the bus status: %s", err))
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

	c.Ui.Output(formatBusStatus(status))
	return 0
}

func formatBusStatus(st *api.BusStatus) string {
	if !st.Enabled {
		return "The system bus is disabled on this server"
	}

	mode := "standalone (single server)"
	if st.Clustered {
		mode = "clustered"
	}
	var b strings.Builder
	b.WriteString(formatKV([]string{
		fmt.Sprintf("Server|%s", st.Server),
		fmt.Sprintf("Cluster|%s", st.Cluster),
		fmt.Sprintf("Mode|%s", mode),
		fmt.Sprintf("Client URL|%s", st.ClientURL),
		fmt.Sprintf("Cluster Port|%d", st.ClusterPort),
		fmt.Sprintf("TLS|%t", st.TLS),
		fmt.Sprintf("Started|%s", formatTime(st.Started)),
	}))

	if js := st.JetStream; js != nil {
		leader := js.MetaLeader
		if leader == "" {
			leader = "<none>"
		}
		b.WriteString("\n\n[bold]JetStream[reset]\n")
		b.WriteString(formatKV([]string{
			fmt.Sprintf("Meta Leader|%s", leader),
			fmt.Sprintf("Cluster Size|%d", js.ClusterSize),
			fmt.Sprintf("Streams|%d", js.Streams),
			fmt.Sprintf("Consumers|%d", js.Consumers),
			fmt.Sprintf("Messages|%d", js.Messages),
			fmt.Sprintf("Memory|%s of %s", humanize.IBytes(js.Memory), humanize.IBytes(uint64(js.MaxMemory))),
			fmt.Sprintf("Storage|%s of %s", humanize.IBytes(js.Storage), humanize.IBytes(uint64(js.MaxStorage))),
		}))
	}

	if js := st.JetStream; js != nil && len(js.StreamDetails) > 0 {
		b.WriteString("\n\n[bold]Streams[reset]\n")
		rows := []string{"Name|Replicas|Leader|Messages|Size|Max Age|Max Bytes"}
		for _, s := range js.StreamDetails {
			maxAge, maxBytes := "unlimited", "unlimited"
			if s.MaxAge > 0 {
				maxAge = s.MaxAge.String()
			}
			if s.MaxBytes > 0 {
				maxBytes = humanize.IBytes(uint64(s.MaxBytes))
			}
			rows = append(rows, fmt.Sprintf("%s|%d|%s|%d|%s|%s|%s",
				s.Name, s.Replicas, s.Leader, s.Messages, humanize.IBytes(s.Bytes), maxAge, maxBytes))
		}
		b.WriteString(formatList(rows))
	}

	b.WriteString("\n\n[bold]Routes[reset]\n")
	if len(st.Routes) == 0 {
		b.WriteString("No routes")
	} else {
		rows := []string{"Server|Address|Direction|RTT"}
		for _, r := range st.Routes {
			dir := "in"
			if r.Outgoing {
				dir = "out"
			}
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s", r.Server, r.Address, dir, r.RTT))
		}
		b.WriteString(formatList(rows))
	}

	if conns := st.Connections; conns != nil {
		b.WriteString("\n\n[bold]Connections[reset]\n")
		b.WriteString(formatKV([]string{
			fmt.Sprintf("Total|%d", conns.Total),
			fmt.Sprintf("Nodes|%d", conns.Nodes),
			fmt.Sprintf("Tokens|%d", conns.Tokens),
			fmt.Sprintf("Internal|%d", conns.Internal),
			fmt.Sprintf("Messages In/Out|%d / %d", conns.InMsgs, conns.OutMsgs),
			fmt.Sprintf("Bytes In/Out|%s / %s", humanize.IBytes(uint64(conns.InBytes)), humanize.IBytes(uint64(conns.OutBytes))),
		}))
	}
	return b.String()
}
