// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"regexp"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestFormatDisruptionStatuses(t *testing.T) {
	ci.Parallel(t)

	out := formatDisruptionStatuses(map[string]*api.DisruptionStatus{
		"web":   {MinHealthy: "4", Healthy: 6, RequiredHealthy: 4, DisruptionsAllowed: 2},
		"cache": {MaxUnavailable: "25%", Healthy: 3, RequiredHealthy: 3, DeploymentReserved: 1},
	})
	must.StrContains(t, out, "Disruptions Allowed")
	must.RegexMatch(t, regexp.MustCompile(`cache\s+max_unavailable = 25%\s+3\s+3\s+1\s+0`), out)
	must.RegexMatch(t, regexp.MustCompile(`web\s+min_healthy = 4\s+6\s+4\s+0\s+2`), out)
}

func TestAllocStopCommand_RespectDisruption_Fails(t *testing.T) {
	ci.Parallel(t)

	ui := cli.NewMockUi()
	cmd := &AllocStopCommand{Meta: Meta{Ui: ui}}

	// Fails on connection failure
	code := cmd.Run([]string{"-address=nope", "-respect-disruption", "foobar"})
	must.One(t, code)
	must.StrContains(t, ui.ErrorWriter.String(), "Error querying allocation")
	must.StrContains(t, cmd.Help(), "-respect-disruption")
}
