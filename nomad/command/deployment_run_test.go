// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestDeploymentRunCommand_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &DeploymentRunCommand{}
}

func TestDeploymentRunCommand_Fails(t *testing.T) {
	ci.Parallel(t)
	ui := cli.NewMockUi()
	cmd := &DeploymentRunCommand{Meta: Meta{Ui: ui}}

	must.One(t, cmd.Run([]string{"some", "bad", "args"}))
	must.StrContains(t, ui.ErrorWriter.String(), commandErrorText(cmd))
	ui.ErrorWriter.Reset()

	must.One(t, cmd.Run([]string{"-address=nope", "12"}))
	must.StrContains(t, ui.ErrorWriter.String(), "Error retrieving deployment")
}

func TestDeploymentRunCommand_Run(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()
	testutil.WaitForLeader(t, srv.Agent.RPC)

	state := srv.Agent.Server().State()
	job := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))
	d := mock.Deployment()
	d.JobID = job.ID
	d.Status = structs.DeploymentStatusPending
	must.NoError(t, state.UpsertDeployment(1001, d))

	ui := cli.NewMockUi()
	cmd := &DeploymentRunCommand{Meta: Meta{Ui: ui}}
	code := cmd.Run([]string{"-address=" + url, "-detach", d.ID[:8]})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "started")

	out, err := state.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.NotEq(t, structs.DeploymentStatusPending, out.Status)

	// A deployment that is no longer pending cannot be run again.
	ui = cli.NewMockUi()
	cmd = &DeploymentRunCommand{Meta: Meta{Ui: ui}}
	must.One(t, cmd.Run([]string{"-address=" + url, "-detach", d.ID}))
	must.StrContains(t, ui.ErrorWriter.String(), "Error running deployment")
}

func TestFormatMultiregionDeployment(t *testing.T) {
	ci.Parallel(t)

	out := formatMultiregionDeployment([]regionResult{
		{region: "hetzner", d: &api.Deployment{ID: "11111111-aaaa", Status: "blocked", StatusDescription: "waiting"}},
		{region: "aws", d: &api.Deployment{ID: "22222222-bbbb", Status: "running", StatusDescription: "running"}},
		{region: "gcp", err: errors.New("no path to region")},
	}, 8)

	lines := strings.Split(out, "\n")
	must.Len(t, 4, lines)
	must.StrContains(t, lines[0], "Region")
	must.StrContains(t, lines[1], "hetzner")
	must.StrContains(t, lines[1], "11111111")
	must.StrContains(t, lines[2], "aws")
	must.StrContains(t, lines[3], "no path to region")
}
