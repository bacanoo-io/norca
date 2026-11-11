// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca"
	"github.com/shoenig/test/must"
)

// TestAgent_ServerConfig_Role checks the server role option: the default is
// the voter role, standby and observer pass through, an unknown role and a
// non-voting server that would bootstrap a cluster alone are refused.
func TestAgent_ServerConfig_Role(t *testing.T) {
	ci.Parallel(t)

	newConf := func(role string, expect int) *Config {
		conf := DevConfig(nil)
		conf.Server.Role = role
		conf.Server.BootstrapExpect = expect
		must.NoError(t, conf.normalizeAddrs())
		return conf
	}

	for _, tc := range []struct {
		role   string
		expect int
		want   norca.ServerRole
		err    string
	}{
		{role: "", expect: 1, want: norca.ServerRoleVoter},
		{role: "voter", expect: 3, want: norca.ServerRoleVoter},
		{role: "standby", expect: 3, want: norca.ServerRoleStandby},
		{role: "observer", expect: 0, want: norca.ServerRoleObserver},
		{role: "witness", expect: 3, err: `invalid server role "witness"`},
		{role: "observer", expect: 1, err: "cannot bootstrap a cluster alone"},
	} {
		t.Run(tc.role+"/"+string(rune('0'+tc.expect)), func(t *testing.T) {
			out, err := convertServerConfig(newConf(tc.role, tc.expect))
			if tc.err != "" {
				must.ErrorContains(t, err, tc.err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tc.want, out.Role)
		})
	}
}
