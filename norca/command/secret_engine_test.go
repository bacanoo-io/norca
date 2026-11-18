// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestSecretEngine_Parse(t *testing.T) {
	ci.Parallel(t)
	spec := filepath.Join(t.TempDir(), "engine.hcl")
	content := []byte(`
name        = "postgres"
type        = "operator"
description = "orders"

operator {
  service   = "postgres-operator"
  namespace = "platform"
  job       = "pg-operator"
}

role "app" {
  ttl     = "30m"
  max_ttl = "12h"
  config  = { database = "app" }
  params  = ["schema"]
}

role "reader" {}
`)
	engine, err := parseSecretEngine(spec, content, false)
	must.NoError(t, err)
	must.Eq(t, &api.SecretEngine{
		Name:        "postgres",
		Type:        "operator",
		Description: "orders",
		Operator:    &api.SecretEngineOperator{Service: "postgres-operator", Namespace: "platform", JobID: "pg-operator"},
		Roles: []*api.SecretEngineRole{
			{Name: "app", TTL: 30 * time.Minute, MaxTTL: 12 * time.Hour,
				Config: map[string]string{"database": "app"}, Params: []string{"schema"}},
			{Name: "reader"},
		},
	}, engine)

	_, err = parseSecretEngine(spec, []byte(`name = "x"`), false)
	must.Error(t, err)
	bad := []byte(`
name = "postgres"
type = "operator"
role "r" { ttl = "soon" }
`)
	_, err = parseSecretEngine(spec, bad, false)
	must.ErrorContains(t, err, "invalid ttl")

	engine, err = parseSecretEngine("-", []byte(`{"Name":"pg","Type":"operator","Operator":{"Service":"s"}}`), true)
	must.NoError(t, err)
	must.Eq(t, "s", engine.Operator.Service)
}

func TestSecretEngineCommands_Usage(t *testing.T) {
	ci.Parallel(t)
	ui := cli.NewMockUi()
	cmd := &SecretLeaseRevokeCommand{Meta: Meta{Ui: ui}}
	must.One(t, cmd.Run([]string{}))
	must.StrContains(t, ui.ErrorWriter.String(), "either a lease ID or -engine")

	ui = cli.NewMockUi()
	apply := &SecretEngineApplyCommand{Meta: Meta{Ui: ui}}
	must.One(t, apply.Run([]string{filepath.Join(t.TempDir(), "missing.hcl")}))
	must.StrContains(t, ui.ErrorWriter.String(), "Failed to read")

	del := &SecretEngineDeleteCommand{Meta: Meta{Ui: cli.NewMockUi()}}
	must.StrContains(t, del.Help(), "-force")
	must.StrContains(t, del.Help(), "may still be valid at the backend")
	must.StrContains(t, del.Help(), "management token")
}

func TestSecretEngineDeleteCommand_Force(t *testing.T) {
	ci.Parallel(t)
	srv, client, url := testServer(t, false, nil)
	defer srv.Shutdown()

	write := func() {
		_, _, err := client.SecretEngines().Write(&api.SecretEngine{
			Name:     "pg",
			Type:     api.SecretEngineTypeOperator,
			Operator: &api.SecretEngineOperator{Service: "pg", JobID: "pg-operator"},
			Roles:    []*api.SecretEngineRole{{Name: "worker", TTL: time.Minute}},
		}, nil)
		must.NoError(t, err)
	}
	engines := func() int {
		list, _, err := client.SecretEngines().List("", nil)
		must.NoError(t, err)
		return len(list)
	}

	write()
	ui := cli.NewMockUi()
	cmd := &SecretEngineDeleteCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + url, "pg"}))
	must.StrNotContains(t, ui.ErrorWriter.String(), "may still be valid")
	must.Zero(t, engines())

	write()
	ui = cli.NewMockUi()
	cmd = &SecretEngineDeleteCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + url, "-force", "pg"}))
	must.StrContains(t, ui.OutputWriter.String(), "Successfully deleted")
	must.StrContains(t, ui.ErrorWriter.String(), "may still be valid at the backend")
	must.Zero(t, engines())
}
