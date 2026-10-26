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
	"github.com/hashicorp/nomad/nomad/structs/config"
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

// The agent self endpoint redacts the secrets of the Norca configuration
// blocks: the tokens of the Consul-compatible API and of the DNS server, the
// credentials of the backup destinations and the password of a webhook URL.
// The running configuration keeps them.
func TestHTTP_AgentSelf_RedactsNorcaSecrets(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		const secret = "badc0deb-adc0-deba-dc0d-ebadc0debadc"

		s.Config.Client.ConsulCompat = &config.ConsulCompatConfig{AnonymousToken: secret}
		s.Config.Client.DNS = &config.DNSConfig{Token: secret}
		s.Config.Server.Backup = &BackupConfig{Destinations: []*BackupDestinationConfig{{
			Name: "s3", AccessKey: "AKIAEXAMPLE", SecretKey: secret, SessionToken: secret,
		}}}
		s.Config.Server.Admission = &config.AdmissionConfig{Webhooks: []*config.AdmissionWebhookConfig{
			{Name: "basic", URL: "https://hook:" + secret + "@hooks.example.com/admit"},
			{Name: "plain", URL: "https://hooks.example.com/admit"},
		}}

		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		obj, err := s.Server.AgentSelfRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		self := obj.(agentSelf)

		must.Eq(t, "<redacted>", self.Config.Client.ConsulCompat.AnonymousToken)
		must.Eq(t, "<redacted>", self.Config.Client.DNS.Token)
		must.Eq(t, "<redacted>", self.Config.Server.Backup.Destinations[0].SecretKey)
		must.Eq(t, "<redacted>", self.Config.Server.Backup.Destinations[0].SessionToken)
		must.Eq(t, "AKIAEXAMPLE", self.Config.Server.Backup.Destinations[0].AccessKey)
		must.Eq(t, "https://hook:xxxxx@hooks.example.com/admit", self.Config.Server.Admission.Webhooks[0].URL)
		must.Eq(t, "https://hooks.example.com/admit", self.Config.Server.Admission.Webhooks[1].URL)

		raw, err := json.Marshal(self)
		must.NoError(t, err)
		must.StrNotContains(t, string(raw), secret)

		must.Eq(t, secret, s.Config.Client.ConsulCompat.AnonymousToken)
		must.Eq(t, secret, s.Config.Client.DNS.Token)
		must.Eq(t, secret, s.Config.Server.Backup.Destinations[0].SecretKey)
		must.StrContains(t, s.Config.Server.Admission.Webhooks[0].URL, secret)
	})
}
