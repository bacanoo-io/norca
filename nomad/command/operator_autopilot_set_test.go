// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/stretchr/testify/require"
)

func TestOperator_Autopilot_SetConfig_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &OperatorRaftListCommand{}
}

func TestOperatorAutopilotSetConfigCommand(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)
	s, _, addr := testServer(t, false, nil)
	defer s.Shutdown()

	ui := cli.NewMockUi()
	c := &OperatorAutopilotSetCommand{Meta: Meta{Ui: ui}}
	args := []string{
		"-address=" + addr,
		"-cleanup-dead-servers=false",
		"-max-trailing-logs=99",
		"-min-quorum=3",
		"-last-contact-threshold=123ms",
		"-server-stabilization-time=123ms",
	}

	code := c.Run(args)
	require.EqualValues(0, code)
	output := strings.TrimSpace(ui.OutputWriter.String())
	require.Contains(output, "Configuration updated")

	client, err := c.Client()
	require.NoError(err)

	conf, _, err := client.Operator().AutopilotGetConfiguration(nil)
	require.NoError(err)

	require.False(conf.CleanupDeadServers)
	require.EqualValues(99, conf.MaxTrailingLogs)
	require.EqualValues(3, conf.MinQuorum)
	require.EqualValues(123*time.Millisecond, conf.LastContactThreshold)
	require.EqualValues(123*time.Millisecond, conf.ServerStabilizationTime)
	require.False(conf.EnableRedundancyZones)
	require.False(conf.DisableUpgradeMigration)
	require.False(conf.EnableCustomUpgrades)

	// The redundancy zone, upgrade migration and custom upgrade options are
	// not supported and are rejected.
	ui = cli.NewMockUi()
	c = &OperatorAutopilotSetCommand{Meta: Meta{Ui: ui}}
	code = c.Run([]string{"-address=" + addr, "-enable-redundancy-zones=true"})
	require.EqualValues(1, code)
	require.Contains(ui.ErrorWriter.String(), "autopilot options are not supported: enable_redundancy_zones")
}
