// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/testutil"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestOperatorConsulKVMigrateCommand(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &OperatorConsulKVMigrateCommand{}

	s, client, addr := testServer(t, false, func(c *agent.Config) {
		c.Server.Bus = &agent.BusConfig{Enabled: pointer.Of(true), MaxStorage: "128MiB", MaxMemory: "32MiB"}
	})
	defer s.Shutdown()

	path := consulkv.ACLPath(consulkv.DefaultPath, "app/config")
	testutil.WaitForResult(func() (bool, error) {
		_, _, err := client.Variables().Create(&api.Variable{
			Namespace: "default", Path: path, Items: consulkv.EncodeVariableItems([]byte("v1"), 0),
		}, nil)
		return err == nil, err
	}, func(err error) { t.Fatal(err) })

	// The leader creates the bucket shortly after it became leader.
	var result *api.ConsulKVMigrateResult
	testutil.WaitForResult(func() (bool, error) {
		res, _, err := client.Operator().ConsulKVMigrate(&api.ConsulKVMigrateOptions{DryRun: true}, nil)
		if err != nil {
			return false, err
		}
		result = res
		if len(res.Copied) != 1 {
			return false, errors.New("the key is not listed")
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	must.Eq(t, []string{"app/config"}, result.Copied)

	ui := cli.NewMockUi()
	cmd := &OperatorConsulKVMigrateCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr, "-dry-run"}), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "Would copy")

	ui = cli.NewMockUi()
	cmd = &OperatorConsulKVMigrateCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr, "-json"}), must.Sprint(ui.ErrorWriter.String()))
	var decoded api.ConsulKVMigrateResult
	must.NoError(t, json.Unmarshal(ui.OutputWriter.Bytes(), &decoded))
	must.Eq(t, []string{"app/config"}, decoded.Copied)

	// The key is in the bucket now: a second run leaves it unchanged.
	ui = cli.NewMockUi()
	cmd = &OperatorConsulKVMigrateCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{"-address=" + addr, "-json"}))
	must.NoError(t, json.Unmarshal(ui.OutputWriter.Bytes(), &decoded))
	must.Eq(t, []string{"app/config"}, decoded.Unchanged)

	// Arguments are refused.
	ui = cli.NewMockUi()
	cmd = &OperatorConsulKVMigrateCommand{Meta: Meta{Ui: ui}}
	must.One(t, cmd.Run([]string{"-address=" + addr, "extra"}))
}
