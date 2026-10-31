// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestSecretEngine_Parse(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "app.jwt"), []byte("eyJ.account.jwt\n"), 0o600))
	spec := filepath.Join(dir, "engine.hcl")
	content := []byte(`
name            = "nats"
type            = "nats"
description     = "orders"
credential_path = "nats/admin"

nats {
  servers          = ["nats://127.0.0.1:4222"]
  account_jwt_file = "app.jwt"
}

role "worker" {
  ttl             = "30m"
  max_ttl         = "12h"
  publish         = ["orders.>"]
  subscribe       = ["orders.>", "_INBOX.>"]
  publish_deny    = ["orders.secret"]
  allow_responses = true
}

role "reader" {
  subscribe = ["orders.>"]
}
`)
	engine, err := parseSecretEngine(spec, content, false)
	must.NoError(t, err)
	must.Eq(t, &api.SecretEngine{
		Name:           "nats",
		Type:           "nats",
		Description:    "orders",
		CredentialPath: "nats/admin",
		NATS:           &api.SecretEngineNATS{Servers: []string{"nats://127.0.0.1:4222"}, AccountJWT: "eyJ.account.jwt"},
		Roles: []*api.SecretEngineRole{
			{Name: "worker", TTL: 30 * time.Minute, MaxTTL: 12 * time.Hour, NATS: &api.SecretRoleNATS{
				Publish: []string{"orders.>"}, Subscribe: []string{"orders.>", "_INBOX.>"},
				PublishDeny: []string{"orders.secret"}, AllowResponses: true}},
			{Name: "reader", NATS: &api.SecretRoleNATS{Subscribe: []string{"orders.>"}}},
		},
	}, engine)

	_, err = parseSecretEngine(spec, []byte(`name = "x"`), false)
	must.Error(t, err)
	bad := []byte(`
name = "nats"
type = "nats"
credential_path = "a"
role "r" { ttl = "soon" }
`)
	_, err = parseSecretEngine(spec, bad, false)
	must.ErrorContains(t, err, "invalid ttl")

	engine, err = parseSecretEngine("-", []byte(`{"Name":"nats","Type":"nats","CredentialPath":"a"}`), true)
	must.NoError(t, err)
	must.Eq(t, "a", engine.CredentialPath)
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
}
