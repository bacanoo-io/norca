// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/mitchellh/cli"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

const importCanary = "import-canary-5b7e"

// fakeVault serves the parts of the Vault KV API that the import reads, in
// the shapes of the Vault HTTP API documentation: a KV v2 mount "kv" and a
// KV v1 mount "old".
func fakeVault(t *testing.T) *vaultapi.Client {
	kv2 := map[string]map[string]any{
		"app/db":      {"password": importCanary, "port": json.Number("5432")},
		"app/sub/api": {"key": importCanary},
		"app/deleted": nil,
	}
	kv1 := map[string]map[string]any{"cfg": {"a": importCanary}}
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	list := func(store map[string]map[string]any, dir string) []string {
		seen := map[string]bool{}
		var keys []string
		for p := range store {
			rest, ok := strings.CutPrefix(p, dir)
			if !ok {
				continue
			}
			if i := strings.Index(rest, "/"); i >= 0 {
				rest = rest[:i+1]
			}
			if !seen[rest] {
				seen[rest] = true
				keys = append(keys, rest)
			}
		}
		return keys
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "root-token" {
			write(w, http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v1/")
		isList := r.URL.Query().Get("list") == "true"
		switch {
		case strings.HasPrefix(p, "sys/internal/ui/mounts/kv"):
			write(w, 200, map[string]any{"data": map[string]any{"path": "kv/", "type": "kv", "options": map[string]any{"version": "2"}}})
		case strings.HasPrefix(p, "sys/internal/ui/mounts/old"):
			write(w, 200, map[string]any{"data": map[string]any{"path": "old/", "type": "kv", "options": nil}})
		case strings.HasPrefix(p, "kv/metadata/") && isList:
			keys := list(kv2, strings.TrimPrefix(p, "kv/metadata/")+"/")
			if len(keys) == 0 {
				write(w, 404, map[string]any{"errors": []string{}})
				return
			}
			write(w, 200, map[string]any{"data": map[string]any{"keys": keys}})
		case strings.HasPrefix(p, "kv/metadata/"):
			if _, ok := kv2[strings.TrimPrefix(p, "kv/metadata/")]; !ok {
				write(w, 404, map[string]any{"errors": []string{}})
				return
			}
			write(w, 200, map[string]any{"data": map[string]any{"current_version": 4}})
		case strings.HasPrefix(p, "kv/data/"):
			data, ok := kv2[strings.TrimPrefix(p, "kv/data/")]
			if !ok {
				write(w, 404, map[string]any{"errors": []string{}})
				return
			}
			var inner any = data
			if data == nil {
				inner = nil
			}
			write(w, 200, map[string]any{"data": map[string]any{"data": inner,
				"metadata": map[string]any{"version": 4, "deletion_time": ""}}})
		case (p == "old" || strings.HasPrefix(p, "old/")) && isList:
			dir := strings.TrimPrefix(strings.TrimPrefix(p, "old"), "/")
			if dir != "" {
				dir += "/"
			}
			write(w, 200, map[string]any{"data": map[string]any{"keys": list(kv1, dir)}})
		case strings.HasPrefix(p, "old/"):
			data, ok := kv1[strings.TrimPrefix(p, "old/")]
			if !ok {
				write(w, 404, map[string]any{"errors": []string{}})
				return
			}
			write(w, 200, map[string]any{"data": data})
		default:
			write(w, 404, map[string]any{"errors": []string{"no handler for route"}})
		}
	}))
	t.Cleanup(srv.Close)
	conf := vaultapi.DefaultConfig()
	conf.Address = srv.URL
	conf.MaxRetries = 0
	vc, err := vaultapi.NewClient(conf)
	must.NoError(t, err)
	vc.SetToken("root-token")
	return vc
}

func TestSecretImportCommand(t *testing.T) {
	ci.Parallel(t)
	srv, client, url := testServer(t, false, nil)
	testutil.WaitForLeader(t, srv.Agent.RPC)
	waitForSecretStoreReady(t, client)
	vc := fakeVault(t)

	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		cmd := &SecretImportCommand{Meta: Meta{Ui: ui}, vaultClient: vc}
		code := cmd.Run(append([]string{"-address=" + url}, args...))
		out, errOut := ui.OutputWriter.String(), ui.ErrorWriter.String()
		must.StrNotContains(t, out, importCanary)
		must.StrNotContains(t, errOut, importCanary)
		return code, out, errOut
	}
	read := func(path string) map[string]string {
		t.Helper()
		v, _, err := client.Secrets().Read(path, 0, nil)
		must.NoError(t, err, must.Sprint(path))
		return v.Items
	}
	notFound := func(path string) {
		t.Helper()
		_, _, err := client.Secrets().Info(path, nil)
		must.True(t, errors.Is(err, api.ErrSecretNotFound), must.Sprint(path, err))
	}

	// Dry run: nothing is written.
	code, out, errOut := run("-dry-run", "-from-vault", "kv/app/")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "kv/data/app/db (version 4) -> app/db: would import")
	must.StrContains(t, out, "kv/data/app/sub/api (version 4) -> app/sub/api: would import")
	must.StrContains(t, out, "Would import 3, skipped 0, failed 0 of 3 secrets")
	notFound("app/db")

	// Import with a prefix map; the deleted version is skipped.
	code, out, errOut = run("-from-vault", "kv/data/app/", "-prefix-map", "app/sub/=shared/")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "kv/data/app/db (version 4) -> app/db: imported as version 1")
	must.StrContains(t, out, "-> shared/api: imported as version 1")
	must.StrContains(t, out, "app/deleted: skipped, the latest version is deleted in Vault")
	must.StrContains(t, out, "Imported 2, skipped 1, failed 0 of 3 secrets")
	must.Eq(t, map[string]string{"password": importCanary, "port": "5432"}, read("app/db"))
	must.Eq(t, importCanary, read("shared/api")["key"])
	notFound("app/sub/api")

	// Existing secrets are skipped, unless -overwrite.
	code, out, _ = run("-from-vault", "kv/app/db")
	must.Zero(t, code)
	must.StrContains(t, out, "app/db: skipped, the secret exists (version 1)")
	code, out, _ = run("-from-vault", "kv/app/db", "-overwrite")
	must.Zero(t, code)
	must.StrContains(t, out, "app/db: imported as version 2")

	// KV v1.
	code, out, errOut = run("-from-vault", "old/")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "old/cfg -> cfg: imported as version 1")
	must.Eq(t, importCanary, read("cfg")["a"])

	// Variables, moved.
	_, _, err := client.Variables().Create(&api.Variable{Path: "app/var", Items: api.VariableItems{"k": importCanary}}, nil)
	must.NoError(t, err)
	code, out, errOut = run("-from-variable", "app/var", "-move", "-prefix-map", "app/=vars/")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "var app/var -> vars/var: imported as version 1, source deleted")
	must.Eq(t, importCanary, read("vars/var")["k"])
	_, _, err = client.Variables().Read("app/var", nil)
	must.ErrorIs(t, err, api.ErrVariablePathNotFound)

	// Usage errors.
	code, _, errOut = run("-from-vault", "kv/", "-from-variable", "x")
	must.One(t, code)
	must.StrContains(t, errOut, "Exactly one of")
	code, _, errOut = run("-from-vault", "kv/", "-move")
	must.One(t, code)
	must.StrContains(t, errOut, "-move")
}

func TestSecretImport_PrefixMap(t *testing.T) {
	ci.Parallel(t)
	m, err := parsePrefixMaps([]string{"app/=prod/app/", "app/sub/=shared/"})
	must.NoError(t, err)
	must.Eq(t, "prod/app/db", m("app/db"))
	must.Eq(t, "shared/x", m("app/sub/x"))
	must.Eq(t, "other/x", m("other/x"))
	_, err = parsePrefixMaps([]string{"nope"})
	must.Error(t, err)
}
