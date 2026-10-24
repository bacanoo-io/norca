// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

func TestService_Canonicalize_CheckRoles(t *testing.T) {
	testutil.Parallel(t)

	job := &Job{Name: pointerOf("job")}
	tg := &TaskGroup{Name: pointerOf("group")}
	task := &Task{Name: "task"}

	s := &Service{
		CheckRestart: &CheckRestart{Limit: 2},
		Checks: []ServiceCheck{
			{Name: "legacy"},
			{Name: "started", Role: CheckRoleStartup, StartupTimeout: time.Minute},
			{Name: "ready", Role: CheckRoleReadiness},
			{Name: "alive", Role: CheckRoleLiveness},
		},
	}
	s.Canonicalize(task, tg, job)

	// the service check_restart applies to checks that may restart the task
	must.Eq(t, 2, s.Checks[0].CheckRestart.Limit)
	must.Eq(t, 2, s.Checks[3].CheckRestart.Limit)

	// startup and readiness checks do not inherit it
	must.Nil(t, s.Checks[1].CheckRestart)
	must.Nil(t, s.Checks[2].CheckRestart)

	must.Eq(t, time.Minute, s.Checks[1].StartupTimeout)
}
