// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestServiceIntentionCommands_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &ServiceIntentionCommand{}
	var _ cli.Command = &ServiceIntentionCreateCommand{}
	var _ cli.Command = &ServiceIntentionDeleteCommand{}
	var _ cli.Command = &ServiceIntentionListCommand{}
	var _ cli.Command = &ServiceIntentionInfoCommand{}
	var _ cli.Command = &ServiceIntentionCheckCommand{}
}

func TestServiceIntentionCommands_Run(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()

	run := func(cmd interface{ Run([]string) int }, ui *cli.MockUi, args ...string) int {
		return cmd.Run(append([]string{"-address", url}, args...))
	}

	ui := cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionCreateCommand{Meta: Meta{Ui: ui}}, ui, "-allow", "web", "api"), must.Sprint(ui.ErrorWriter.String()))
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionCreateCommand{Meta: Meta{Ui: ui}}, ui, "-deny", "-description", "nobody else", "*/*", "api"), must.Sprint(ui.ErrorWriter.String()))

	got, err := srv.Agent.Server().State().ServiceIntentionByKey(nil, "default", "api", "*", "*")
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "deny", got.Action)

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionListCommand{Meta: Meta{Ui: ui}}, ui))
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "default/web")
	must.StrContains(t, out, "nobody else")

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionInfoCommand{Meta: Meta{Ui: ui}}, ui, "web", "api"))
	must.StrContains(t, ui.OutputWriter.String(), "allow")

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionCheckCommand{Meta: Meta{Ui: ui}}, ui, "web", "api"))
	must.StrContains(t, ui.OutputWriter.String(), "is allowed by intention default/web")
	ui = cli.NewMockUi()
	must.Eq(t, 2, run(&ServiceIntentionCheckCommand{Meta: Meta{Ui: ui}}, ui, "other/db", "api"))
	must.StrContains(t, ui.OutputWriter.String(), "is denied by intention */*")

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ServiceIntentionDeleteCommand{Meta: Meta{Ui: ui}}, ui, "web", "api"))
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ServiceIntentionDeleteCommand{Meta: Meta{Ui: ui}}, ui, "web", "api"))
	must.StrContains(t, ui.ErrorWriter.String(), "not found")
}
