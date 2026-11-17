// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

// TestSecretGet_EnvFormatKeys checks that -format env prints nothing when an
// item key is not a shell variable name. A line like db-password='value' is
// a command to a shell, not an assignment: the shell reports it, with the
// value, on its standard error.
func TestSecretGet_EnvFormatKeys(t *testing.T) {
	ci.Parallel(t)

	const canary = "get-canary-91c3"
	items := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&api.SecretValue{Version: 1, Items: items})
	}))
	t.Cleanup(srv.Close)
	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		code := (&SecretGetCommand{Meta: Meta{Ui: ui}}).Run(append([]string{"-address=" + srv.URL}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	items = map[string]string{"DB_USER": "app", "_x1": "it's"}
	code, out, errOut := run("-format", "env", "p")
	must.Zero(t, code, must.Sprint(errOut))
	must.Eq(t, "DB_USER='app'\n_x1='it'\\''s'\n", out)

	for _, key := range []string{"db-password", "api.key", "1st"} {
		items = map[string]string{"DB_USER": "app", key: canary}
		code, out, errOut = run("-format", "env", "p")
		must.One(t, code, must.Sprint(key))
		must.Eq(t, "", out, must.Sprint(key))
		must.StrContains(t, errOut, `"`+key+`"`)
		must.StrContains(t, errOut, "-format json")
		must.StrNotContains(t, errOut, canary)

		// The other ways of reading still serve such keys.
		code, out, errOut = run("-item", key, "p")
		must.Zero(t, code, must.Sprint(errOut))
		must.Eq(t, canary+"\n", out)
		code, out, errOut = run("-format", "json", "p")
		must.Zero(t, code, must.Sprint(errOut))
		must.StrContains(t, out, canary)
	}
}
