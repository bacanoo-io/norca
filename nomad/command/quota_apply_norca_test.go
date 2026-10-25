// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestQuotaApplyCommand_ParseForkFields(t *testing.T) {
	ci.Parallel(t)

	spec, err := parseQuotaSpec([]byte(`
name        = "team-a"
description = "Production quota of team A"

limit {
  region = "global"
  region_limit {
    cpu    = 40000
    memory = 81920
  }
  variables_limit = 50
  allocations     = 200
}

task_defaults {
  cpu    = 200
  memory = 256
}

task_max {
  cpu    = 8000
  cores  = 4
  memory = 16384
}

enforce = "submit"
`))
	must.NoError(t, err)
	must.Eq(t, "team-a", spec.Name)
	must.Eq(t, "submit", spec.Enforce)
	must.Len(t, 1, spec.Limits)
	must.Eq(t, 200, *spec.Limits[0].Allocations)
	must.Eq(t, 50, *spec.Limits[0].VariablesLimit)
	must.Eq(t, 40000, *spec.Limits[0].RegionLimit.CPU)
	must.Eq(t, 200, *spec.TaskDefaults.CPU)
	must.Eq(t, 256, *spec.TaskDefaults.MemoryMB)
	must.Eq(t, 4, *spec.TaskMax.Cores)
	must.Eq(t, 16384, *spec.TaskMax.MemoryMB)

	// Unknown keys are rejected
	_, err = parseQuotaSpec([]byte(`
name = "x"
task_defaults {
  disk = 10
}
`))
	must.ErrorContains(t, err, "invalid key")
}

func TestQuotaApplyCommand_Good(t *testing.T) {
	ci.Parallel(t)

	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &QuotaApplyCommand{Meta: Meta{Ui: ui}}

	// Apply the example written by quota init
	path := filepath.Join(t.TempDir(), "spec.hcl")
	must.NoError(t, os.WriteFile(path, []byte(defaultHclQuotaSpec), 0o600))

	code := cmd.Run([]string{"-address=" + url, path})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))

	spec, _, err := client.Quotas().Info("default-quota", nil)
	must.NoError(t, err)
	must.Eq(t, "schedule", spec.Enforce)
	must.Eq(t, 100, *spec.Limits[0].Allocations)
	must.Eq(t, 256, *spec.TaskDefaults.MemoryMB)

	// The JSON example is accepted as well
	path = filepath.Join(t.TempDir(), "spec.json")
	must.NoError(t, os.WriteFile(path, []byte(defaultJsonQuotaSpec), 0o600))
	code = cmd.Run([]string{"-address=" + url, "-json", path})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
}
