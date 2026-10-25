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

func TestAdmissionPolicyCommands_Variables(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()

	spec := `
name        = "crd-objects"
action      = "warn"
schema_from = "crd-schema"

match {
  resources      = ["variable"]
  variable_paths = ["crd/*"]
}

rule {
  expression = "request.operation == 'delete' || has(variable.items.owner)"
  message    = "CRD objects should have an owner."
}
`
	path := filepath.Join(t.TempDir(), "policy.hcl")
	must.NoError(t, os.WriteFile(path, []byte(spec), 0o600))

	ui := cli.NewMockUi()
	cmd := &AdmissionPolicyApplyCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, cmd.Run([]string{"-address", url, path}), must.Sprint(ui.ErrorWriter.String()))

	got, err := srv.Agent.Server().State().AdmissionPolicyByName(nil, "crd-objects")
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, []string{"variable"}, got.Match.Resources)
	must.Eq(t, []string{"crd/*"}, got.Match.VariablePaths)
	must.Eq(t, "crd-schema", got.SchemaFrom)

	// inspect and list show the new fields
	ui = cli.NewMockUi()
	inspectCmd := &AdmissionPolicyInspectCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, inspectCmd.Run([]string{"-address", url, "crd-objects"}))
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "Resources")
	must.StrContains(t, out, "Variable Paths")
	must.StrContains(t, out, "crd/*")
	must.StrContains(t, out, "Schema From")

	ui = cli.NewMockUi()
	listCmd := &AdmissionPolicyListCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, listCmd.Run([]string{"-address", url}))
	must.StrContains(t, ui.OutputWriter.String(), "Resources")
	must.StrContains(t, ui.OutputWriter.String(), "variable")

	// var put prints the warnings of the policy
	ui = cli.NewMockUi()
	putCmd := &VarPutCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, putCmd.Run([]string{"-address=" + url, "crd/g/k/n", "spec=x"}), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.ErrorWriter.String(), "Admission Warnings")
	must.StrContains(t, ui.ErrorWriter.String(), "CRD objects should have an owner.")

	// a deny policy makes var put fail with the policy message
	denySpec := `
name = "crd-deny"
match {
  resources      = ["variable"]
  variable_paths = ["crd/*"]
}
rule {
  expression = "request.operation == 'delete' || variable.items.size != 'huge'"
  message    = "Huge objects are not allowed."
}
`
	denyPath := filepath.Join(t.TempDir(), "deny.hcl")
	must.NoError(t, os.WriteFile(denyPath, []byte(denySpec), 0o600))
	ui = cli.NewMockUi()
	cmd = &AdmissionPolicyApplyCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, cmd.Run([]string{"-address", url, denyPath}), must.Sprint(ui.ErrorWriter.String()))

	ui = cli.NewMockUi()
	putCmd = &VarPutCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, putCmd.Run([]string{"-address=" + url, "-force", "crd/g/k/n", "owner=a", "size=huge"}))
	must.StrContains(t, ui.ErrorWriter.String(), "Huge objects are not allowed.")

	// var purge prints the warnings too
	ui = cli.NewMockUi()
	purgeCmd := &VarPurgeCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, purgeCmd.Run([]string{"-address=" + url, "crd/g/k/n"}), must.Sprint(ui.ErrorWriter.String()))
}
