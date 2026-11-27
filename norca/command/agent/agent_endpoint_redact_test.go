// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

// The agent self endpoint returns no credential of the URLs of the NATS
// servers of the Consul KV: "nats://user:password@host" and
// "nats://token@host" are how the NATS client takes them.
func TestHTTP_AgentSelf_RedactsNATSURLs(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		const secret = "badc0deb-adc0-deba-dc0d-ebadc0debadc"

		urls := []string{
			"nats://kv:" + secret + "@10.0.0.1:4222",
			"nats://" + secret + "@10.0.0.2:4222",
			"kv:" + secret + "@10.0.0.3:4222",
			"nats://10.0.0.4:4222,tls://" + secret + "@10.0.0.5:4222",
			"nats://10.0.0.6:4222",
			"10.0.0.7:4222",
		}
		s.Config.Client.ConsulCompat = &config.ConsulCompatConfig{KV: &config.ConsulCompatKVConfig{
			NATS: &config.ConsulCompatKVNATSConfig{URLs: urls},
		}}

		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		obj, err := s.Server.AgentSelfRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		self := obj.(agentSelf)

		must.Eq(t, []string{
			"nats://xxxxx@10.0.0.1:4222",
			"nats://xxxxx@10.0.0.2:4222",
			"xxxxx@10.0.0.3:4222",
			"nats://10.0.0.4:4222,tls://xxxxx@10.0.0.5:4222",
			"nats://10.0.0.6:4222",
			"10.0.0.7:4222",
		}, self.Config.Client.ConsulCompat.KV.NATS.URLs)

		raw, err := json.Marshal(self)
		must.NoError(t, err)
		must.StrNotContains(t, string(raw), secret)

		// the running configuration keeps them
		must.Eq(t, "nats://kv:"+secret+"@10.0.0.1:4222", s.Config.Client.ConsulCompat.KV.NATS.URLs[0])
		must.Len(t, len(urls), s.Config.Client.ConsulCompat.KV.NATS.URLs)
	})
}

// The agent self endpoint returns no credential of the other settings that
// can hold one: the basic authentication of Consul, addresses given as URLs,
// the cloud auto-join entries, and (as far as their names tell) the free-form settings of plugins and client options.
func TestHTTP_AgentSelf_RedactsAddressesAndFreeForm(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		const secret = "badc0deb-adc0-deba-dc0d-ebadc0debadc"

		join := func() []string {
			return []string{
				"10.0.0.1:4648",
				"provider=aws tag_key=role tag_value=server access_key_id=AKIAEXAMPLE secret_access_key=" + secret,
			}
		}
		s.Config.Consul.Auth = "consul:" + secret
		s.Config.Consul.Addr = "https://consul:" + secret + "@consul.example.com:8501"
		s.Config.Vault.Addr = "https://" + secret + "@vault.example.com:8200"
		s.Config.Client.Servers = join()
		s.Config.Client.ServerJoin = &ServerJoin{RetryJoin: join(), StartJoin: join()}
		s.Config.Server.RetryJoin = join()
		s.Config.Server.StartJoin = join()
		s.Config.Server.ServerJoin = &ServerJoin{RetryJoin: join(), StartJoin: join()}
		s.Config.Server.Backup = &BackupConfig{Destinations: []*BackupDestinationConfig{{
			Name: "s3", Endpoint: "https://minio:" + secret + "@minio.example.com:9000",
		}}}
		s.Config.Client.Options = map[string]string{
			"docker.cleanup.image":  "false",
			"driver.x.registry_url": "https://u:" + secret + "@registry.example.com",
			"driver.x.api_token":    secret,
		}
		s.Config.Plugins = []*config.PluginConfig{{
			Name: "docker",
			Args: []string{"-v", "-token=" + secret, "--password", secret, "-url=https://u:" + secret + "@h.example.com/x"},
			Config: map[string]interface{}{
				"allow_privileged": true,
				"endpoint":         "tcp://127.0.0.1:2375",
				"registry":         "https://u:" + secret + "@registry.example.com",
				"api_token":        secret,
				"auth":             []map[string]interface{}{{"config": "/etc/docker.json", "password": secret}},
				"gc":               []map[string]interface{}{{"image": true, "webhook_secret": secret}},
				"list":             []interface{}{"a", "https://" + secret + "@x.example.com"},
			},
		}}

		req, err := http.NewRequest(http.MethodGet, "/v1/agent/self", nil)
		must.NoError(t, err)
		obj, err := s.Server.AgentSelfRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		self := obj.(agentSelf)
		c := self.Config

		raw, err := json.Marshal(self)
		must.NoError(t, err)
		must.StrNotContains(t, string(raw), secret)

		must.Eq(t, "<redacted>", c.Consul.Auth)
		must.Eq(t, "https://xxxxx@consul.example.com:8501", c.Consul.Addr)
		must.Eq(t, "https://xxxxx@vault.example.com:8200", c.Vault.Addr)
		wantJoin := []string{
			"10.0.0.1:4648",
			"provider=aws tag_key=<redacted> tag_value=<redacted> access_key_id=<redacted> secret_access_key=<redacted>",
		}
		must.Eq(t, wantJoin, c.Client.Servers)
		must.Eq(t, wantJoin, c.Client.ServerJoin.RetryJoin)
		must.Eq(t, wantJoin, c.Client.ServerJoin.StartJoin)
		must.Eq(t, wantJoin, c.Server.RetryJoin)
		must.Eq(t, wantJoin, c.Server.StartJoin)
		must.Eq(t, wantJoin, c.Server.ServerJoin.RetryJoin)
		must.Eq(t, wantJoin, c.Server.ServerJoin.StartJoin)
		must.Eq(t, "https://xxxxx@minio.example.com:9000", c.Server.Backup.Destinations[0].Endpoint)
		must.Eq(t, map[string]string{
			"docker.cleanup.image":  "false",
			"driver.x.registry_url": "https://xxxxx@registry.example.com",
			"driver.x.api_token":    "<redacted>",
		}, c.Client.Options)
		must.Eq(t, []string{"-v", "-token=<redacted>", "--password", "<redacted>", "-url=https://xxxxx@h.example.com/x"},
			c.Plugins[0].Args)
		must.Eq(t, map[string]interface{}{
			"allow_privileged": true,
			"endpoint":         "tcp://127.0.0.1:2375",
			"registry":         "https://xxxxx@registry.example.com",
			"api_token":        "<redacted>",
			"auth":             "<redacted>",
			"gc":               []map[string]interface{}{{"image": true, "webhook_secret": "<redacted>"}},
			"list":             []interface{}{"a", "https://xxxxx@x.example.com"},
		}, c.Plugins[0].Config)

		// the running configuration keeps everything
		must.Eq(t, "consul:"+secret, s.Config.Consul.Auth)
		must.Eq(t, join(), s.Config.Client.Servers)
		must.Eq(t, join(), s.Config.Server.ServerJoin.RetryJoin)
		must.Eq(t, secret, s.Config.Client.Options["driver.x.api_token"])
		must.Eq(t, "-token="+secret, s.Config.Plugins[0].Args[1])
		must.Eq(t, secret, s.Config.Plugins[0].Config["api_token"])
		must.Eq(t, secret, s.Config.Plugins[0].Config["auth"].([]map[string]interface{})[0]["password"])
		must.StrContains(t, s.Config.Server.Backup.Destinations[0].Endpoint, secret)
	})
}

func TestRedactAddress(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]string{
		"":                            "",
		"10.0.0.1:4222":               "10.0.0.1:4222",
		"localhost:4222":              "localhost:4222",
		"[::1]:4222":                  "[::1]:4222",
		"nats.example.com":            "nats.example.com",
		"unix:///var/run/consul.sock": "unix:///var/run/consul.sock",
		"nats://10.0.0.1:4222":        "nats://10.0.0.1:4222",
		"nats://u:p@10.0.0.1:4222":    "nats://xxxxx@10.0.0.1:4222",
		"nats://tok@10.0.0.1:4222":    "nats://xxxxx@10.0.0.1:4222",
		"u:p@10.0.0.1:4222":           "xxxxx@10.0.0.1:4222",
		"tok@nats.example.com":        "xxxxx@nats.example.com",
		"10.0.0.1:4222?token=t":       "10.0.0.1:4222?token=<redacted>",
		"https://h.example.com/a?k=v": "https://h.example.com/a?k=<redacted>",
		"u:p@bad host\x7f:1":          "<redacted>",
	}
	for in, want := range cases {
		must.Eq(t, want, redactAddress(in), must.Sprint(in))
	}

	must.Nil(t, redactAddresses(nil))
	must.Nil(t, redactJoinAddresses(nil))
	must.Nil(t, redactArgs(nil))
	must.Nil(t, redactPlugins(nil))
	must.Nil(t, redactOptions(nil))
	must.Eq(t, []string{"10.0.0.1:4648", "provider=x <redacted> a=<redacted>"},
		redactJoinAddresses([]string{"10.0.0.1:4648", "provider=x bare a=1"}))
}
