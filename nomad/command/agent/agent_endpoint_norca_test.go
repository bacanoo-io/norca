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

		s.Config.Client.ConsulCompat = &config.ConsulCompatConfig{AnonymousToken: secret, KV: &config.ConsulCompatKVConfig{
			NATS: &config.ConsulCompatKVNATSConfig{User: "kv", Password: secret, Token: secret},
		}}
		s.Config.Client.DNS = &config.DNSConfig{Token: secret}
		s.Config.Server.Backup = &BackupConfig{Destinations: []*BackupDestinationConfig{{
			Name: "s3", AccessKey: "AKIAEXAMPLE", SecretKey: secret, SessionToken: secret,
		}}}
		s.Config.Server.Admission = &config.AdmissionConfig{Webhooks: []*config.AdmissionWebhookConfig{
			{Name: "basic", URL: "https://hook:" + secret + "@hooks.example.com/admit"},
			{Name: "plain", URL: "https://hooks.example.com/admit"},
			{Name: "query", URL: "https://hooks.example.com/admit?token=" + secret + "&api_key=" + secret + "&" + secret + "&empty="},
			{Name: "user", URL: "https://" + secret + "@hooks.example.com/admit#" + secret},
		}}
		s.Config.Server.Autoscaler = &AutoscalerConfig{
			APMs: []*AutoscalerPluginConfig{{
				Name:   "prom",
				Driver: "prometheus",
				Args:   []string{"-token=" + secret, secret},
				Config: map[string]interface{}{"address": "https://prom.example.com", "api_key": secret},
			}},
			Strategies: []*AutoscalerPluginConfig{{
				Name:   "custom",
				Config: map[string]interface{}{"password": secret, "auth": []map[string]interface{}{{"token": secret}}},
			}},
		}

		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		obj, err := s.Server.AgentSelfRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		self := obj.(agentSelf)

		must.Eq(t, "<redacted>", self.Config.Client.ConsulCompat.AnonymousToken)
		must.Eq(t, "<redacted>", self.Config.Client.DNS.Token)
		must.Eq(t, "<redacted>", self.Config.Client.ConsulCompat.KV.NATS.Password)
		must.Eq(t, "<redacted>", self.Config.Client.ConsulCompat.KV.NATS.Token)
		must.Eq(t, "kv", self.Config.Client.ConsulCompat.KV.NATS.User)
		must.Eq(t, "<redacted>", self.Config.Server.Backup.Destinations[0].SecretKey)
		must.Eq(t, "<redacted>", self.Config.Server.Backup.Destinations[0].SessionToken)
		must.Eq(t, "AKIAEXAMPLE", self.Config.Server.Backup.Destinations[0].AccessKey)
		must.Eq(t, "https://hook:xxxxx@hooks.example.com/admit", self.Config.Server.Admission.Webhooks[0].URL)
		must.Eq(t, "https://hooks.example.com/admit", self.Config.Server.Admission.Webhooks[1].URL)
		must.Eq(t, "https://hooks.example.com/admit?token=<redacted>&api_key=<redacted>&<redacted>&empty=",
			self.Config.Server.Admission.Webhooks[2].URL)
		must.Eq(t, "https://xxxxx@hooks.example.com/admit#<redacted>", self.Config.Server.Admission.Webhooks[3].URL)

		apm := self.Config.Server.Autoscaler.APMs[0]
		must.Eq(t, "prom", apm.Name)
		must.Eq(t, "prometheus", apm.Driver)
		must.Eq(t, []string{"<redacted>", "<redacted>"}, apm.Args)
		must.Eq(t, map[string]interface{}{"address": "<redacted>", "api_key": "<redacted>"}, apm.Config)
		must.Eq(t, map[string]interface{}{"password": "<redacted>", "auth": "<redacted>"},
			self.Config.Server.Autoscaler.Strategies[0].Config)

		raw, err := json.Marshal(self)
		must.NoError(t, err)
		must.StrNotContains(t, string(raw), secret)

		must.Eq(t, secret, s.Config.Client.ConsulCompat.AnonymousToken)
		must.Eq(t, secret, s.Config.Client.DNS.Token)
		must.Eq(t, secret, s.Config.Client.ConsulCompat.KV.NATS.Password)
		must.Eq(t, secret, s.Config.Server.Backup.Destinations[0].SecretKey)
		for _, w := range []int{0, 2, 3} {
			must.StrContains(t, s.Config.Server.Admission.Webhooks[w].URL, secret)
		}
		must.Eq(t, []string{"-token=" + secret, secret}, s.Config.Server.Autoscaler.APMs[0].Args)
		must.Eq(t, secret, s.Config.Server.Autoscaler.APMs[0].Config["api_key"])
		must.Eq(t, "https://prom.example.com", s.Config.Server.Autoscaler.APMs[0].Config["address"])
		must.Eq(t, secret, s.Config.Server.Autoscaler.Strategies[0].Config["password"])
	})
}

// redactURL keeps the scheme, host and path of webhook URLs and hides
// everything that can carry a credential.
func TestRedactURL(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]string{
		"https://hooks.example.com/admit":                   "https://hooks.example.com/admit",
		"http://10.0.0.1:8080/v1/admit":                     "http://10.0.0.1:8080/v1/admit",
		"https://u:p@hooks.example.com/admit":               "https://u:xxxxx@hooks.example.com/admit",
		"https://tok@hooks.example.com/admit":               "https://xxxxx@hooks.example.com/admit",
		"https://hooks.example.com/admit?a=1&b=%2F&c=":      "https://hooks.example.com/admit?a=<redacted>&b=<redacted>&c=",
		"https://hooks.example.com/admit?abc":               "https://hooks.example.com/admit?<redacted>",
		"https://hooks.example.com/admit#frag":              "https://hooks.example.com/admit#<redacted>",
		"https://u:p@hooks.example.com:8443/a?token=t#frag": "https://u:xxxxx@hooks.example.com:8443/a?token=<redacted>#<redacted>",
		"://bad\x7f": "<redacted>",
	}
	for in, want := range cases {
		must.Eq(t, want, redactURL(in), must.Sprint(in))
	}
}
