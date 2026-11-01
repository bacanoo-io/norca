// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/api"
	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestHTTP_SecretEngines drives the engine and lease endpoints through the
// HTTP API with the api client.
func TestHTTP_SecretEngines(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, cb, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.Agent.RPC)
		client := s.Client()
		engines := client.SecretEngines()
		engine := &api.SecretEngine{
			Name:     "nats",
			Type:     api.SecretEngineTypeOperator,
			Operator: &api.SecretEngineOperator{Service: "pg"},
			Roles:    []*api.SecretEngineRole{{Name: "worker", TTL: time.Minute}},
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
		must.Eq(t, "pg", info.Operator.Service)
		_, _, err = engines.Info("missing", nil)
		must.ErrorContains(t, err, "404")

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
