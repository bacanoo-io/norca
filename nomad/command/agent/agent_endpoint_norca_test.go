// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/version"
	"github.com/shoenig/test/must"
)

// The agent self endpoint reports the Norca product version and the Nomad
// API version, on servers and on client-only agents (which have no member).
func TestHTTP_AgentSelf_NorcaVersion(t *testing.T) {
	ci.Parallel(t)

	check := func(t *testing.T, s *TestAgent) {
		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		obj, err := s.Server.AgentSelfRequest(respW, req)
		must.NoError(t, err)

		self := obj.(agentSelf)
		must.Eq(t, version.NorcaVersion, self.NorcaVersion)
		must.Eq(t, version.GetVersion().VersionNumber(), self.NomadVersion)

		// The JSON keys decode into the api package's AgentSelf.
		raw, err := json.Marshal(self)
		must.NoError(t, err)
		var out api.AgentSelf
		must.NoError(t, json.Unmarshal(raw, &out))
		must.Eq(t, version.NorcaVersion, out.NorcaVersion)
		must.Eq(t, self.NomadVersion, out.NomadVersion)
	}

	t.Run("server", func(t *testing.T) {
		httpTest(t, nil, func(s *TestAgent) {
			check(t, s)
		})
	})

	t.Run("client only", func(t *testing.T) {
		serverAgent := NewTestAgent(t, "server", nil)
		defer serverAgent.Shutdown()

		s := makeHTTPServer(t, func(c *Config) {
			c.Server.Enabled = false
			c.Client.Servers = []string{fmt.Sprintf("localhost:%d", serverAgent.Config.Ports.RPC)}
		})
		defer s.Shutdown()

		check(t, s)

		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		obj, err := s.Server.AgentSelfRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		must.Eq(t, "", obj.(agentSelf).Member.Name)
	})
}
