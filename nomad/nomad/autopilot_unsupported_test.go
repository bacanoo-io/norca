// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestOperator_AutopilotSetConfiguration_Unsupported(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	cases := []struct {
		name   string
		modify func(*structs.AutopilotConfig)
		err    string
	}{
		{
			name:   "supported options",
			modify: func(c *structs.AutopilotConfig) { c.MaxTrailingLogs = 99 },
		},
		{
			name:   "redundancy zones",
			modify: func(c *structs.AutopilotConfig) { c.EnableRedundancyZones = true },
			err:    "autopilot options are not supported: enable_redundancy_zones",
		},
		{
			name: "upgrade options",
			modify: func(c *structs.AutopilotConfig) {
				c.DisableUpgradeMigration = true
				c.EnableCustomUpgrades = true
			},
			err: "autopilot options are not supported: disable_upgrade_migration, enable_custom_upgrades",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf := *s1.config.AutopilotConfig
			tc.modify(&conf)
			req := structs.AutopilotSetConfigRequest{
				Datacenter:   s1.config.Datacenter,
				Config:       conf,
				WriteRequest: structs.WriteRequest{Region: s1.config.Region},
			}
			var reply bool
			err := msgpackrpc.CallWithCodec(codec, "Operator.AutopilotSetConfiguration", &req, &reply)
			if tc.err == "" {
				must.NoError(t, err)
				return
			}
			must.EqError(t, err, tc.err)

			_, stored, err := s1.fsm.State().AutopilotConfig()
			must.NoError(t, err)
			must.False(t, stored.EnableRedundancyZones)
			must.False(t, stored.DisableUpgradeMigration)
			must.False(t, stored.EnableCustomUpgrades)
		})
	}
}
