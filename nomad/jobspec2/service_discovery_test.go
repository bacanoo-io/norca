// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/jobspec"
	"github.com/shoenig/test/must"
)

const serviceDiscoveryJob = `
job "discovery" {
  group "group" {
    service {
      name     = "api"
      port     = "http"
      provider = "nomad"

      discovery {
        prefer            = "datacenter"
        min_local         = 2
        overflow_fraction = 0.3
        failover          = ["eu-gcp", "us-hetzner"]
        include_not_ready = true
      }

      check {
        type                     = "http"
        path                     = "/health"
        interval                 = "5s"
        timeout                  = "2s"
        success_before_passing   = 2
        failures_before_critical = 3
      }
    }
  }
}
`

func TestParse_ServiceDiscovery(t *testing.T) {
	ci.Parallel(t)

	check := func(t *testing.T, job *api.Job) {
		service := job.TaskGroups[0].Services[0]
		d := service.Discovery
		must.NotNil(t, d)
		must.Eq(t, "datacenter", d.Prefer)
		must.Eq(t, 2, d.MinLocal)
		must.Eq(t, 0.3, *d.OverflowFraction)
		must.Eq(t, []string{"eu-gcp", "us-hetzner"}, d.Failover)
		must.True(t, d.IncludeNotReady)
		must.Eq(t, 2, service.Checks[0].SuccessBeforePassing)
		must.Eq(t, 3, service.Checks[0].FailuresBeforeCritical)
	}

	t.Run("hcl2", func(t *testing.T) {
		job, err := ParseWithConfig(&ParseConfig{
			Path: "input.hcl",
			Body: []byte(serviceDiscoveryJob),
		})
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1", func(t *testing.T) {
		job, err := jobspec.Parse(strings.NewReader(serviceDiscoveryJob))
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1 invalid key", func(t *testing.T) {
		_, err := jobspec.Parse(strings.NewReader(strings.Replace(serviceDiscoveryJob, "min_local", "min_locals", 1)))
		must.ErrorContains(t, err, "min_locals")
	})
}
