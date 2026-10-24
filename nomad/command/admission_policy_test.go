// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestAdmissionPolicyCommands_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &AdmissionPolicyCommand{}
	var _ cli.Command = &AdmissionPolicyApplyCommand{}
	var _ cli.Command = &AdmissionPolicyListCommand{}
	var _ cli.Command = &AdmissionPolicyInspectCommand{}
	var _ cli.Command = &AdmissionPolicyDeleteCommand{}
}

func TestAdmissionPolicyCommands_Run(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()

	spec := `
name        = "no-privileged-container"
description = "Privileged Docker containers are forbidden in prod"
action      = "warn"

match {
  namespaces = ["prod"]
}

rule {
  expression = "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))"
  message    = "Privileged containers are not allowed in the prod namespace."
}
`
	path := filepath.Join(t.TempDir(), "policy.hcl")
	must.NoError(t, os.WriteFile(path, []byte(spec), 0o600))

	// apply
	ui := cli.NewMockUi()
	cmd := &AdmissionPolicyApplyCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, cmd.Run([]string{"-address", url, path}), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "no-privileged-container")

	got, err := srv.Agent.Server().State().AdmissionPolicyByName(nil, "no-privileged-container")
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "warn", got.Action)
	must.Eq(t, []string{"prod"}, got.Match.Namespaces)
	must.Len(t, 1, got.Rules)

	// apply with an invalid expression fails
	badPath := filepath.Join(t.TempDir(), "bad.hcl")
	must.NoError(t, os.WriteFile(badPath, []byte("name = \"bad\"\nrule {\n  expression = \"job.ID ==\"\n}\n"), 0o600))
	ui = cli.NewMockUi()
	cmd = &AdmissionPolicyApplyCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, cmd.Run([]string{"-address", url, badPath}))
	must.StrContains(t, ui.ErrorWriter.String(), "rule 0")

	// list
	ui = cli.NewMockUi()
	listCmd := &AdmissionPolicyListCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, listCmd.Run([]string{"-address", url}))
	must.StrContains(t, ui.OutputWriter.String(), "no-privileged-container")
	must.StrContains(t, ui.OutputWriter.String(), "prod")

	// inspect
	ui = cli.NewMockUi()
	inspectCmd := &AdmissionPolicyInspectCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, inspectCmd.Run([]string{"-address", url, "no-privileged-container"}))
	must.StrContains(t, ui.OutputWriter.String(), "Privileged containers are not allowed")

	ui = cli.NewMockUi()
	inspectCmd = &AdmissionPolicyInspectCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, inspectCmd.Run([]string{"-address", url, "-json", "no-privileged-container"}))
	must.StrContains(t, ui.OutputWriter.String(), `"Action": "warn"`)

	// delete
	ui = cli.NewMockUi()
	deleteCmd := &AdmissionPolicyDeleteCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, deleteCmd.Run([]string{"-address", url, "no-privileged-container"}))
	got, err = srv.Agent.Server().State().AdmissionPolicyByName(nil, "no-privileged-container")
	must.NoError(t, err)
	must.Nil(t, got)

	ui = cli.NewMockUi()
	deleteCmd = &AdmissionPolicyDeleteCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, deleteCmd.Run([]string{"-address", url, "no-privileged-container"}))
}
