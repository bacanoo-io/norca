// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestApiServicesToStructs_CheckRoles(t *testing.T) {
	ci.Parallel(t)

	in := []*api.Service{{
		Name: "web",
		Checks: []api.ServiceCheck{
			{Name: "legacy"},
			{Name: "started", Role: api.CheckRoleStartup, StartupTimeout: 10 * time.Minute},
			{Name: "alive", Role: api.CheckRoleLiveness},
		},
	}}

	out := ApiServicesToStructs(in, false)
	must.Len(t, 3, out[0].Checks)

	must.Eq(t, "", out[0].Checks[0].Role)
	must.Zero(t, out[0].Checks[0].StartupTimeout)

	must.Eq(t, structs.CheckRoleStartup, out[0].Checks[1].Role)
	must.Eq(t, 10*time.Minute, out[0].Checks[1].StartupTimeout)

	must.Eq(t, structs.CheckRoleLiveness, out[0].Checks[2].Role)
}
