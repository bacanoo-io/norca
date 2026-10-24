// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/jobspec"
	"github.com/shoenig/test/must"
)

const checkRolesJob = `
job "roles" {
  group "group" {
    service {
      name     = "api"
      port     = "http"
      provider = "nomad"

      check {
        role            = "startup"
        type            = "http"
        path            = "/healthz/started"
        interval        = "5s"
        timeout         = "2s"
        startup_timeout = "10m"
      }

      check {
        role     = "readiness"
        type     = "http"
        path     = "/healthz/ready"
        interval = "10s"
        timeout  = "2s"
      }

      check {
        role     = "liveness"
        type     = "http"
        path     = "/healthz/live"
        interval = "15s"
        timeout  = "3s"
        check_restart {
          limit = 3
        }
      }
    }
  }
}
`

func TestParse_CheckRoles(t *testing.T) {
	ci.Parallel(t)

	check := func(t *testing.T, job *api.Job) {
		checks := job.TaskGroups[0].Services[0].Checks
		must.Len(t, 3, checks)

		must.Eq(t, api.CheckRoleStartup, checks[0].Role)
		must.Eq(t, 10*time.Minute, checks[0].StartupTimeout)

		must.Eq(t, api.CheckRoleReadiness, checks[1].Role)
		must.Zero(t, checks[1].StartupTimeout)

		must.Eq(t, api.CheckRoleLiveness, checks[2].Role)
		must.Eq(t, 3, checks[2].CheckRestart.Limit)
	}

	t.Run("hcl2", func(t *testing.T) {
		job, err := ParseWithConfig(&ParseConfig{
			Path: "input.hcl",
			Body: []byte(checkRolesJob),
		})
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1", func(t *testing.T) {
		job, err := jobspec.Parse(strings.NewReader(checkRolesJob))
		must.NoError(t, err)
		check(t, job)
	})
}
