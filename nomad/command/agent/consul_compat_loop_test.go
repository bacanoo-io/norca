// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func TestAddrTargetsListener(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		addr, bind string
		want       bool
	}{
		{"127.0.0.1:8500", "127.0.0.1:8500", true},
		{"http://127.0.0.1:8500", "127.0.0.1:8500", true},
		{"https://127.0.0.1:8500", "127.0.0.1:8500", true},
		{"localhost:8500", "127.0.0.1:8500", true},
		{"LOCALHOST:8500", "[::1]:8500", true},
		{"127.0.0.1:8500", "0.0.0.0:8500", true},
		{"[::1]:8500", "[::]:8500", true},
		{"127.0.0.1:8500", ":8500", true},
		{"0.0.0.0:8500", "127.0.0.1:8500", true},
		{"consul.example.com:8500", "consul.example.com:8500", true},

		{"127.0.0.1:8501", "127.0.0.1:8500", false},
		{"127.0.0.1:8500", "127.0.0.2:8500", false},
		{"[::1]:8500", "127.0.0.1:8500", false},
		{"localhost:8500", "192.0.2.10:8500", false},
		{"consul.example.com:8500", "0.0.0.0:8500", false},
		{"192.0.2.10:8500", "127.0.0.1:8500", false},
		{"unix:///run/consul.sock", "127.0.0.1:8500", false},
		{"127.0.0.1", "127.0.0.1:8500", false},
		{"127.0.0.1:8500", "bad", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr+"->"+tc.bind, func(t *testing.T) {
			must.Eq(t, tc.want, addrTargetsListener(tc.addr, tc.bind))
		})
	}
}

// compatConfig returns a dev agent configuration with the Consul-compatible
// API enabled on bind.
func compatConfig(bind string) *Config {
	c := DevConfig(nil)
	c.Client.ConsulCompat = &config.ConsulCompatConfig{Enabled: pointer.Of(true), Bind: bind}
	return c
}

// An agent whose consul block points at its own Consul-compatible API does
// not advertise itself there, does not fingerprint it as Consul, and its
// client and servers do not auto-join through it.
func TestAgent_ConsulCompatLoop(t *testing.T) {
	ci.Parallel(t)

	bind := fmt.Sprintf("127.0.0.1:%d", ci.PortAllocator.Grab(1)[0])
	a := NewTestAgent(t, t.Name(), func(c *Config) {
		c.Client.ConsulCompat = &config.ConsulCompatConfig{Enabled: pointer.Of(true), Bind: bind}
		c.Consul.Addr = bind
		c.Consul.AutoAdvertise = pointer.Of(true)
	})
	defer a.Shutdown()

	must.Eq(t, bind, a.Agent.Client().ConsulCompatAddr())
	must.False(t, *a.Config.Consul.AutoAdvertise)
	must.False(t, *a.Agent.Client().GetConfig().ConsulConfig.ClientAutoJoin)
	must.False(t, *a.Agent.Server().GetConfig().ConsulConfig.ServerAutoJoin)
	must.MapContainsKey(t, a.Agent.Client().GetConfig().ReadStringListToMap("fingerprint.denylist"), consulFingerprintName)
	must.MapNotContainsKey(t, a.Agent.Client().Node().Attributes, "consul.version")
}

func TestAvoidConsulCompatLoop(t *testing.T) {
	ci.Parallel(t)

	t.Run("default addresses", func(t *testing.T) {
		c := compatConfig("")
		c.Consul.Addr = "127.0.0.1:8500"
		must.True(t, *c.Consul.AutoAdvertise)
		must.NotEq(t, "", consulCompatLoopWarning(c))

		avoidConsulCompatLoop(c, testlog.HCLogger(t))
		must.False(t, *c.Consul.AutoAdvertise)
		must.False(t, *c.Consul.ServerAutoJoin)
		must.False(t, *c.Consul.ClientAutoJoin)
		must.Eq(t, consulFingerprintName, c.Client.Options["fingerprint.denylist"])
		// the other options are kept
		must.Eq(t, "true", c.Client.Options["driver.raw_exec.enable"])

		// idempotent
		avoidConsulCompatLoop(c, testlog.HCLogger(t))
		must.Eq(t, consulFingerprintName, c.Client.Options["fingerprint.denylist"])
	})

	t.Run("existing denylist", func(t *testing.T) {
		c := compatConfig("127.0.0.1:8500")
		c.Consul.Addr = "localhost:8500"
		c.Client.Options["fingerprint.blacklist"] = "env_aws, env_gce"
		avoidConsulCompatLoop(c, testlog.HCLogger(t))
		must.Eq(t, "env_aws, env_gce,consul", c.Client.Options["fingerprint.denylist"])

		c = compatConfig("127.0.0.1:8500")
		c.Consul.Addr = "127.0.0.1:8500"
		c.Client.Options["fingerprint.denylist"] = "consul,env_aws"
		avoidConsulCompatLoop(c, testlog.HCLogger(t))
		must.Eq(t, "consul,env_aws", c.Client.Options["fingerprint.denylist"])
	})

	t.Run("no conflict", func(t *testing.T) {
		for name, c := range map[string]*Config{
			"other bind": func() *Config {
				c := compatConfig("127.0.0.1:18500")
				c.Consul.Addr = "127.0.0.1:8500"
				return c
			}(),
			"other consul address": func() *Config {
				c := compatConfig("")
				c.Consul.Addr = "10.0.0.5:8500"
				return c
			}(),
			"compat disabled": func() *Config {
				c := DevConfig(nil)
				c.Consul.Addr = "127.0.0.1:8500"
				return c
			}(),
			"client disabled": func() *Config {
				c := compatConfig("")
				c.Consul.Addr = "127.0.0.1:8500"
				c.Client.Enabled = false
				return c
			}(),
		} {
			t.Run(name, func(t *testing.T) {
				must.Eq(t, "", consulCompatLoopWarning(c))
				avoidConsulCompatLoop(c, testlog.HCLogger(t))
				must.True(t, *c.Consul.AutoAdvertise)
				must.True(t, *c.Consul.ServerAutoJoin)
				must.True(t, *c.Consul.ClientAutoJoin)
				must.MapNotContainsKey(t, c.Client.Options, "fingerprint.denylist")
			})
		}
	})
}
