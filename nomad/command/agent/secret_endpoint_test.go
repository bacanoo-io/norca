// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestHTTP_Secrets drives the secret endpoints through the HTTP API with the
// api client.
func TestHTTP_Secrets(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, cb, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.Agent.RPC)
		secrets := s.Client().Secrets()
		testutil.WaitForResult(func() (bool, error) {
			_, _, err := secrets.Write("ready", &api.SecretWrite{Items: map[string]string{"k": "v"}}, nil)
			return err == nil, err
		}, func(err error) { t.Fatalf("secret store not ready: %v", err) })

		// Write two versions with metadata.
		meta := &api.SecretMeta{Owner: "team", Labels: map[string]string{"tier": "db"}, MaxVersions: 5,
			RotateEvery: time.Hour, ExpiresAt: pointer.Of(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))}
		secret, wm, err := secrets.Write("app/db", &api.SecretWrite{Items: map[string]string{"pw": "one"}, Meta: meta}, nil)
		must.NoError(t, err)
		must.Positive(t, wm.LastIndex)
		must.Eq(t, 1, secret.CurrentVersion)
		must.Eq(t, "team", secret.Owner)
		must.Eq(t, 5, secret.MaxVersions)

		// Check-and-set.
		_, _, err = secrets.CheckedWrite("app/db", 0, &api.SecretWrite{Items: map[string]string{"pw": "x"}}, nil)
		must.ErrorContains(t, err, "409")
		secret, _, err = secrets.CheckedWrite("app/db", secret.ModifyIndex, &api.SecretWrite{Items: map[string]string{"pw": "two"}}, nil)
		must.NoError(t, err)
		must.Eq(t, 2, secret.CurrentVersion)
		must.Eq(t, "team", secret.Owner)

		// Metadata, list and values.
		info, _, err := secrets.Info("app/db", nil)
		must.NoError(t, err)
		must.Len(t, 2, info.Versions)
		must.Eq(t, []string{"pw"}, info.ItemKeys)
		must.Eq(t, time.Hour, info.RotateEvery)
		must.Eq(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), info.ExpiresAt.UTC())
		list, _, err := secrets.List("app/", nil)
		must.NoError(t, err)
		must.Len(t, 1, list)
		must.Eq(t, "app/db", list[0].Path)
		list, _, err = secrets.List("other/", nil)
		must.NoError(t, err)
		must.Len(t, 0, list)

		value, _, err := secrets.Read("app/db", 0, nil)
		must.NoError(t, err)
		must.Eq(t, 2, value.Version)
		must.Eq(t, "two", value.Items["pw"])
		value, _, err = secrets.Read("app/db", 1, nil)
		must.NoError(t, err)
		must.Eq(t, "one", value.Items["pw"])

		_, _, err = secrets.Read("app/missing", 0, nil)
		must.True(t, errors.Is(err, api.ErrSecretNotFound))
		_, _, err = secrets.Info("app/missing", nil)
		must.True(t, errors.Is(err, api.ErrSecretNotFound))

		// The metadata responses never carry values.
		resp, err := http.Get(s.HTTPAddr() + "/v1/secret/app/db?metadata")
		must.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		must.Eq(t, http.StatusOK, resp.StatusCode)
		must.StrNotContains(t, string(body), "two")
		resp, err = http.Get(s.HTTPAddr() + "/v1/secrets")
		must.NoError(t, err)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		must.StrNotContains(t, string(body), "two")
		must.StrContains(t, string(body), "app/db")

		// Delete, undelete, destroy.
		secret, _, err = secrets.Delete("app/db", nil, nil)
		must.NoError(t, err)
		must.True(t, secret.Deleted)
		_, _, err = secrets.Read("app/db", 0, nil)
		must.ErrorContains(t, err, "deleted")
		secret, _, err = secrets.Undelete("app/db", &api.SecretVersionsOptions{Versions: []uint64{2}}, nil)
		must.NoError(t, err)
		must.False(t, secret.Deleted)
		secret, _, err = secrets.Destroy("app/db", &api.SecretVersionsOptions{Versions: []uint64{1},
			CheckIndex: pointer.Of(secret.ModifyIndex)}, nil)
		must.NoError(t, err)
		must.True(t, secret.Versions[0].Destroyed)
		_, _, err = secrets.Destroy("app/db", &api.SecretVersionsOptions{CheckIndex: pointer.Of(uint64(1))}, nil)
		must.ErrorContains(t, err, "409")
		secret, _, err = secrets.Destroy("app/db", nil, nil)
		must.NoError(t, err)
		must.Nil(t, secret)
		_, _, err = secrets.Info("app/db", nil)
		must.True(t, errors.Is(err, api.ErrSecretNotFound))

		// Methods and paths.
		req, err := http.NewRequest(http.MethodPost, s.HTTPAddr()+"/v1/secret/app/db",
			strings.NewReader(`{"Items":{"k":"v"}}`))
		must.NoError(t, err)
		resp, err = http.DefaultClient.Do(req)
		must.NoError(t, err)
		resp.Body.Close()
		must.Eq(t, http.StatusBadRequest, resp.StatusCode)
		_, _, err = secrets.Write("app/empty", &api.SecretWrite{}, nil)
		must.ErrorContains(t, err, "requires Items or Meta")
	})
}

func TestConfig_SecretsAudit(t *testing.T) {
	ci.Parallel(t)

	var nilConf *SecretsConfig
	audit, err := nilConf.toAuditFileConfig()
	must.NoError(t, err)
	must.Nil(t, audit)

	c := &SecretsConfig{AuditFile: "/var/log/a.log", AuditRotateBytes: 100, AuditRotateDuration: "24h", AuditRotateMaxFiles: 3}
	audit, err = c.toAuditFileConfig()
	must.NoError(t, err)
	must.Eq(t, "/var/log/a.log", audit.Path)
	must.Eq(t, 100, audit.RotateBytes)
	must.Eq(t, 24*time.Hour, audit.RotateDuration)
	must.Eq(t, 3, audit.RotateMaxFiles)

	_, err = (&SecretsConfig{AuditFile: "x", AuditRotateDuration: "soon"}).toAuditFileConfig()
	must.ErrorContains(t, err, "audit_rotate_duration")

	hcl := `server {
  enabled = true
  secrets {
    audit_file             = "/var/log/norca/secrets-audit.log"
    audit_rotate_bytes     = 1048576
    audit_rotate_duration  = "12h"
    audit_rotate_max_files = 4
  }
}
`
	path := filepath.Join(t.TempDir(), "server.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	parsed, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.Eq(t, "/var/log/norca/secrets-audit.log", parsed.Server.Secrets.AuditFile)
	must.Eq(t, 4, parsed.Server.Secrets.AuditRotateMaxFiles)

	must.NoError(t, os.WriteFile(path, []byte(`server { secrets { audit_fiel = "x" } }`), 0o600))
	_, err = ParseConfigFile(path)
	must.ErrorContains(t, err, "audit_fiel")

	merged := c.Merge(&SecretsConfig{AuditRotateMaxFiles: 9})
	must.Eq(t, "/var/log/a.log", merged.AuditFile)
	must.Eq(t, 9, merged.AuditRotateMaxFiles)
}
