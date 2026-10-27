// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/command/asset"
	"github.com/hashicorp/nomad/testutil"
)

// TestNodePoolInitTemplates_Apply checks that the specifications written by
// 'node pool init' are accepted by 'node pool apply': Norca rejects a node
// pool scheduler_config, so the templates must not set one.
func TestNodePoolInitTemplates_Apply(t *testing.T) {
	ci.Parallel(t)
	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()
	testutil.WaitForLeader(t, srv.Agent.RPC)

	must.StrNotContains(t, string(asset.NodePoolSpec), "Enterprise")

	dir := t.TempDir()
	for name, spec := range map[string][]byte{
		"pool.nomad.hcl":  asset.NodePoolSpec,
		"pool.nomad.json": asset.NodePoolSpecJSON,
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, name)
			must.NoError(t, os.WriteFile(file, spec, 0o600))

			ui := cli.NewMockUi()
			args := []string{"-address=" + url}
			if filepath.Ext(name) == ".json" {
				args = append(args, "-json")
			}
			code := (&NodePoolApplyCommand{Meta: Meta{Ui: ui}}).Run(append(args, file))
			must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))

			pool, _, err := client.NodePools().Info("example", nil)
			must.NoError(t, err)
			must.Eq(t, "Example node pool", pool.Description)
			must.Nil(t, pool.SchedulerConfiguration)
		})
	}
}
