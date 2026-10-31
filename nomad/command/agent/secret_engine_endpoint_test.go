// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/secretengine/natstest"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestHTTP_SecretEngines drives the engine and lease endpoints through the
// HTTP API with the api client, against an embedded NATS server.
func TestHTTP_SecretEngines(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	httpTest(t, cb, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.Agent.RPC)
		client := s.Client()
		testutil.WaitForResult(func() (bool, error) {
			_, _, err := client.Secrets().Write("nats/admin", &api.SecretWrite{Items: ns.Credential()}, nil)
			return err == nil, err
		}, func(err error) { t.Fatalf("secret store not ready: %v", err) })

		engines := client.SecretEngines()
		engine := &api.SecretEngine{
			Name:           "nats",
			Type:           api.SecretEngineTypeNATS,
			CredentialPath: "nats/admin",
			NATS:           &api.SecretEngineNATS{Servers: []string{ns.URL}, AccountJWT: ns.AccountJWT},
			Roles: []*api.SecretEngineRole{{Name: "worker", TTL: time.Minute,
				NATS: &api.SecretRoleNATS{Publish: []string{"orders.>"}}}},
		}
		_, _, err := engines.CheckedWrite(engine, 5, nil)
		must.ErrorContains(t, err, "409")
		written, wm, err := engines.CheckedWrite(engine, 0, nil)
		must.NoError(t, err)
		must.Positive(t, wm.LastIndex)
		must.Eq(t, "default", written.Namespace)
		must.Eq(t, 24*time.Hour, written.Roles[0].MaxTTL)

		list, _, err := engines.List("", nil)
		must.NoError(t, err)
		must.Len(t, 1, list)
		must.Eq(t, []string{"worker"}, list[0].Roles)
		info, _, err := engines.Info("nats", nil)
		must.NoError(t, err)
		must.Eq(t, ns.URL, info.NATS.Servers[0])
		_, _, err = engines.Info("missing", nil)
		must.ErrorContains(t, err, "404")

		rotated, key, _, err := engines.RotateRoot("nats", nil)
		must.NoError(t, err)
		must.StrHasPrefix(t, "A", key)
		must.Len(t, 1, rotated.State.RetiredKeys)
		secret, _, err := client.Secrets().Info("nats/admin", nil)
		must.NoError(t, err)
		must.Eq(t, 2, secret.CurrentVersion)

		leases, _, err := client.SecretLeases().List("nats", nil)
		must.NoError(t, err)
		must.Len(t, 0, leases)
		revoked, _, err := engines.RevokeLeases("nats", nil)
		must.NoError(t, err)
		must.Len(t, 0, revoked)
		_, _, err = client.SecretLeases().Info("unknown", nil)
		must.ErrorContains(t, err, "404")

		_, err = engines.Delete("nats", nil)
		must.NoError(t, err)
		list, _, err = engines.List("", nil)
		must.NoError(t, err)
		must.Len(t, 0, list)
	})
}
