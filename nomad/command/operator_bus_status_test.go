// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestOperatorBusStatusCommand(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &OperatorBusStatusCommand{}

	s, client, addr := testServer(t, false, func(c *agent.Config) {
		c.Server.Bus = &agent.BusConfig{Enabled: pointer.Of(true), MaxStorage: "128MiB", MaxMemory: "32MiB"}
	})
	defer s.Shutdown()

	// The API reports the bus of the agent's server.
	st, _, err := client.Operator().BusStatus("", nil)
	must.NoError(t, err)
	must.True(t, st.Enabled)
	must.False(t, st.Clustered)
	must.NotNil(t, st.JetStream)
	must.Eq(t, int64(128<<20), st.JetStream.MaxStorage)

	ui := cli.NewMockUi()
	cmd := &OperatorBusStatusCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr}), must.Sprint(ui.ErrorWriter.String()))
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "standalone (single server)")
	must.StrContains(t, out, "Meta Leader")
	must.StrContains(t, out, st.Server)

	ui = cli.NewMockUi()
	cmd = &OperatorBusStatusCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr, "-json"}))
	var decoded api.BusStatus
	must.NoError(t, json.Unmarshal(ui.OutputWriter.Bytes(), &decoded))
	must.Eq(t, st.Server, decoded.Server)

	// Without the bus, the status says so.
	s2, _, addr2 := testServer(t, false, nil)
	defer s2.Shutdown()
	ui = cli.NewMockUi()
	cmd = &OperatorBusStatusCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr2}))
	must.StrContains(t, ui.OutputWriter.String(), "disabled")
}
