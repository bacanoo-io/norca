// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestObjectKindEcosystemCommands_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &ObjectKindLintCommand{}
	var _ cli.Command = &ObjectKindWebhookCommand{}
	var _ cli.Command = &ObjectKindWebhookApproveCommand{}
	var _ cli.Command = &OperatorInitCommand{}
}

func TestObjectKindLintCommand(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "pg.schema.json"), []byte(testCLIObjectSchema), 0o600))
	kindPath := filepath.Join(dir, "pg.hcl")
	must.NoError(t, os.WriteFile(kindPath, []byte(testCLIObjectKind), 0o600))
	good := filepath.Join(dir, "good.hcl")
	must.NoError(t, os.WriteFile(good, []byte(`
kind = "db.example.com/PostgresCluster"
name = "billing"
spec {
  version = "16"
}`), 0o600))
	bad := filepath.Join(dir, "bad.hcl")
	must.NoError(t, os.WriteFile(bad, []byte(`
kind = "db.example.com/PostgresCluster"
name = "broken"
spec {
  replicas = 2
}`), 0o600))

	ui := cli.NewMockUi()
	cmd := &ObjectKindLintCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, cmd.Run([]string{"-show-defaults", kindPath, good}), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), `"storage":"10GiB"`)
	must.StrContains(t, ui.OutputWriter.String(), "0 errors")
	must.StrContains(t, ui.ErrorWriter.String(), "no description")

	ui = cli.NewMockUi()
	cmd = &ObjectKindLintCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, cmd.Run([]string{kindPath, good, bad}))
	must.StrContains(t, ui.ErrorWriter.String(), "object broken")
	must.StrContains(t, ui.ErrorWriter.String(), "version")

	ui = cli.NewMockUi()
	cmd = &ObjectKindLintCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, cmd.Run(nil))
}

func TestObjectKindEcosystemCommands_Run(t *testing.T) {
	ci.Parallel(t)

	srv, client, url := testServer(t, false, nil)
	defer srv.Shutdown()

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "pg.schema.json"), []byte(testCLIObjectSchema), 0o600))
	kindPath := filepath.Join(dir, "pg.hcl")
	must.NoError(t, os.WriteFile(kindPath, []byte(`
kind "PostgresCluster" {
  group  = "db.example.com"
  schema = file("pg.schema.json")

  webhook "validate" {
    url            = "https://pg-webhook.example.invalid/validate"
    failure_policy = "ignore"
  }

  limits {
    write_rate = 10
  }
}`), 0o600))

	run := func(cmd cli.Command, args ...string) int {
		return cmd.Run(append([]string{"-address", url}, args...))
	}

	// Without ACLs every writer is management: the webhook is approved.
	ui := cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, "-dry-run", kindPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), `webhook "validate"`)
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, kindPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrNotContains(t, ui.ErrorWriter.String(), "pending approval")

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindInspectCommand{Meta: Meta{Ui: ui}}, "db.example.com/PostgresCluster"))
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "Webhooks")
	must.StrContains(t, out, " by ")
	must.StrNotContains(t, out, "pending")
	must.StrContains(t, out, "writes 10/s (burst 10)")
	must.StrContains(t, out, "Schema Version")

	// Revoke and approve.
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindWebhookApproveCommand{Meta: Meta{Ui: ui}, revoke: true}, "db.example.com/PostgresCluster", "validate"),
		must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "Revoked")
	kind, _, err := client.ObjectKinds().Info("db.example.com", "PostgresCluster", nil)
	must.NoError(t, err)
	must.False(t, kind.Webhooks[0].Approved())
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindWebhookApproveCommand{Meta: Meta{Ui: ui}}, "db.example.com/PostgresCluster", "validate"))
	kind, _, err = client.ObjectKinds().Info("db.example.com", "PostgresCluster", nil)
	must.NoError(t, err)
	must.True(t, kind.Webhooks[0].Approved())
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectKindWebhookApproveCommand{Meta: Meta{Ui: ui}}, "db.example.com/PostgresCluster", "missing"))
	must.StrContains(t, ui.ErrorWriter.String(), "no webhook")

	// An incompatible schema: the dry run reports the stored objects.
	_, _, err = client.Objects().Apply(&api.CustomObject{Group: "db.example.com", Kind: "PostgresCluster",
		Namespace: "default", Name: "billing", Spec: map[string]any{"version": "16"}}, nil, nil)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(filepath.Join(dir, "pg.schema.json"), []byte(`{
  "type": "object",
  "properties": {"spec": {"type": "object", "required": ["engine"]}}
}`), 0o600))
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, "-dry-run", kindPath))
	must.StrContains(t, ui.OutputWriter.String(), "Invalid objects")
	must.StrContains(t, ui.OutputWriter.String(), "billing")
	must.StrContains(t, ui.ErrorWriter.String(), "1 stored objects invalid")
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, kindPath))
	must.StrContains(t, ui.ErrorWriter.String(), "would make 1 stored objects invalid")
}

func TestOperatorInitCommand(t *testing.T) {
	ci.Parallel(t)

	dir := filepath.Join(t.TempDir(), "pg")
	ui := cli.NewMockUi()
	cmd := &OperatorInitCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 0, cmd.Run([]string{"-sdk", "/src/norca/nomad", "db.example.com/PostgresCluster", dir}), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "reconcile.go")
	for _, f := range []string{"main.go", "postgrescluster.kind.hcl", "pack/postgres-cluster-operator/metadata.hcl"} {
		_, err := os.Stat(filepath.Join(dir, f))
		must.NoError(t, err)
	}

	// Existing files are not overwritten.
	ui = cli.NewMockUi()
	cmd = &OperatorInitCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, cmd.Run([]string{"db.example.com/PostgresCluster", dir}))
	must.StrContains(t, ui.ErrorWriter.String(), "files exist")

	ui = cli.NewMockUi()
	cmd = &OperatorInitCommand{Meta: Meta{Ui: ui}}
	must.Eq(t, 1, cmd.Run([]string{"PostgresCluster"}))
	must.Eq(t, 1, cmd.Run([]string{"nodot/X"}))
}
