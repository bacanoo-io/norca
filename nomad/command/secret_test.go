// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/testutil"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestSecretCommands_Implement(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &SecretCommand{}
	var _ cli.Command = &SecretListCommand{}
	var _ cli.Command = &SecretInfoCommand{}
	var _ cli.Command = &SecretGetCommand{}
	var _ cli.Command = &SecretPutCommand{}
	var _ cli.Command = &SecretDeleteCommand{}
	var _ cli.Command = &SecretUndeleteCommand{}
	var _ cli.Command = &SecretDestroyCommand{}
}

// TestSecretVersionsCommands_Help checks that the help of destroy does not
// promise a default version: destroy needs -versions or -all.
func TestSecretVersionsCommands_Help(t *testing.T) {
	ci.Parallel(t)
	destroy := (&SecretDestroyCommand{}).Help()
	must.StrContains(t, destroy, "Required unless -all is given")
	must.StrNotContains(t, destroy, "Defaults to the current version")
	must.StrContains(t, destroy, "-check-index")
	for _, help := range []string{(&SecretDeleteCommand{}).Help(), (&SecretUndeleteCommand{}).Help()} {
		must.StrContains(t, help, "Defaults to the current version")
		must.StrContains(t, help, "-check-index")
	}
}

func TestSecretCommands(t *testing.T) {
	ci.Parallel(t)
	srv, client, url := testServer(t, false, nil)
	testutil.WaitForLeader(t, srv.Agent.RPC)
	waitForSecretStoreReady(t, client)

	// run runs a command made by mk with a new UI.
	run := func(mk func(Meta) cli.Command, args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		code := mk(Meta{Ui: ui}).Run(append([]string{"-address=" + url}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}
	stdinPut := func(in string) func(Meta) cli.Command {
		return func(m Meta) cli.Command { return &SecretPutCommand{Meta: m, stdin: strings.NewReader(in)} }
	}
	put := func(m Meta) cli.Command { return &SecretPutCommand{Meta: m} }
	list := func(m Meta) cli.Command { return &SecretListCommand{Meta: m} }
	info := func(m Meta) cli.Command { return &SecretInfoCommand{Meta: m} }
	get := func(m Meta) cli.Command { return &SecretGetCommand{Meta: m} }
	del := func(m Meta) cli.Command { return &SecretDeleteCommand{Meta: m} }
	undel := func(m Meta) cli.Command { return &SecretUndeleteCommand{Meta: m} }
	destroy := func(m Meta) cli.Command { return &SecretDestroyCommand{Meta: m} }

	// put: k=@file and k=- from stdin, without the plain value warning.
	file := filepath.Join(t.TempDir(), "pw")
	must.NoError(t, os.WriteFile(file, []byte("from-file"), 0o600))
	code, out, errOut := run(stdinPut("from-stdin"), "-owner", "team", "-label", "tier=db", "-max-versions", "3",
		"-rotate-every", "24h", "-expires-at", "2030-01-01T00:00:00Z", "app/db", "pw=@"+file, "token=-")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, `Wrote secret "app/db" version 1`)
	must.StrNotContains(t, errOut, "Warning")

	// A plain value is accepted with a warning.
	code, _, errOut = run(put, "app/db", "pw=plain")
	must.Zero(t, code)
	must.StrContains(t, errOut, "shell history")

	// A metadata-only update keeps the other fields.
	code, out, errOut = run(put, "-description", "the db", "app/db")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "Updated the metadata")
	secret, _, err := client.Secrets().Info("app/db", nil)
	must.NoError(t, err)
	must.Eq(t, "the db", secret.Description)
	must.Eq(t, "team", secret.Owner)
	must.Eq(t, "db", secret.Labels["tier"])
	must.Eq(t, 3, secret.MaxVersions)
	must.Eq(t, 24*time.Hour, secret.RotateEvery)
	must.Eq(t, 2, secret.CurrentVersion)

	// All items from stdin as a JSON object.
	code, _, errOut = run(stdinPut(`{"a":"1","b":"2"}`), "app/json", "-")
	must.Zero(t, code, must.Sprint(errOut))

	// Check index.
	code, _, errOut = run(put, "-check-index", "0", "app/db", "pw=@"+file)
	must.One(t, code)
	must.StrContains(t, errOut, "409")

	// list and info show no values.
	code, out, _ = run(list, "app/")
	must.Zero(t, code)
	must.StrContains(t, out, "app/db")
	must.StrContains(t, out, "app/json")
	must.StrNotContains(t, out, "from-file")
	code, out, _ = run(info, "app/db")
	must.Zero(t, code)
	must.StrContains(t, out, "team")
	must.StrContains(t, out, "hmac-sha256:")
	must.StrNotContains(t, out, "from-file")
	must.StrNotContains(t, out, "plain")
	code, out, _ = run(info, "-json", "app/db")
	must.Zero(t, code)
	must.StrNotContains(t, out, "from-file")

	// get refuses to print values unless asked explicitly.
	code, out, errOut = run(get, "app/db")
	must.One(t, code)
	must.StrContains(t, errOut, "Refusing")
	must.Eq(t, "", out)
	code, out, _ = run(get, "-item", "pw", "app/db")
	must.Zero(t, code)
	must.Eq(t, "plain\n", out)
	code, out, _ = run(get, "-version", "1", "-format", "env", "app/db")
	must.Zero(t, code)
	must.Eq(t, "pw='from-file'\ntoken='from-stdin'\n", out)
	code, out, _ = run(get, "-format", "json", "app/json")
	must.Zero(t, code)
	must.StrContains(t, out, `"a": "1"`)
	code, _, errOut = run(get, "-item", "pw", "app/missing")
	must.One(t, code)
	must.StrContains(t, errOut, "not found")

	// delete, undelete, destroy.
	code, out, _ = run(del, "app/db")
	must.Zero(t, code)
	must.StrContains(t, out, "deleted the current version")
	code, out, errOut = run(undel, "-versions", "2", "app/db")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "undeleted versions 2")
	code, _, errOut = run(destroy, "app/db")
	must.One(t, code)
	must.StrContains(t, errOut, "-all")
	code, _, errOut = run(destroy, "-all", "-versions", "1", "app/db")
	must.One(t, code)
	must.StrContains(t, errOut, "Only one of")
	code, out, errOut = run(destroy, "-versions", "1", "app/db")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "destroyed versions 1")
	must.StrNotContains(t, out, "destroyd")
	code, out, errOut = run(destroy, "-all", "app/db")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "with all of its versions")
	code, _, errOut = run(info, "app/db")
	must.One(t, code)
	must.StrContains(t, errOut, "not found")
}

// waitForSecretStoreReady waits until the secret store of the test agent
// accepts writes (its core is unsealed and active on the leader).
func waitForSecretStoreReady(t *testing.T, client *api.Client) {
	t.Helper()
	testutil.WaitForResult(func() (bool, error) {
		_, _, err := client.Secrets().Write("ready", &api.SecretWrite{Items: map[string]string{"k": "v"}}, nil)
		return err == nil, err
	}, func(err error) { t.Fatalf("secret store not ready: %v", err) })
}

func TestSecretPut_ParseExpiry(t *testing.T) {
	ci.Parallel(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	got, err := parseSecretExpiry("720h", now)
	must.NoError(t, err)
	must.Eq(t, now.Add(720*time.Hour), *got)
	got, err = parseSecretExpiry("2030-01-01T00:00:00Z", now)
	must.NoError(t, err)
	must.Eq(t, 2030, got.Year())
	_, err = parseSecretExpiry("tomorrow", now)
	must.Error(t, err)
}
