// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

func TestPlugins_WhenNotClientSkip(t *testing.T) {
	s, _, _ := testServer(t, false, nil)
	must.Nil(t, s.Agent.pluginSingletonLoader)
}

func TestPlugins_WhenClientRun(t *testing.T) {
	s, _, _ := testServer(t, true, nil)
	must.NotNil(t, s.Agent.pluginSingletonLoader)
}

func testServer(t *testing.T, runClient bool, cb func(*Config)) (*TestAgent, *api.Client, string) {
	// Make a new test server
	a := NewTestAgent(t, t.Name(), func(config *Config) {
		config.Client.Enabled = runClient

		if cb != nil {
			cb(config)
		}
	})
	t.Cleanup(a.Shutdown)

	c := a.Client()
	return a, c, a.HTTPAddr()
}
