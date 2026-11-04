// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/jobspec"
	"github.com/shoenig/test/must"
)

const serviceExposeJob = `
job "expose" {
  group "group" {
    service {
      name     = "api-public"
      port     = "https"
      provider = "nomad"

      expose {
        load_balancer {
          class    = "aws-nlb"
          port     = 443
          internal = true
        }
      }
    }

    service {
      name     = "plain"
      provider = "nomad"

      expose {}
    }
  }
}
`

func TestParse_ServiceExpose(t *testing.T) {
	ci.Parallel(t)

	check := func(t *testing.T, job *api.Job) {
		services := job.TaskGroups[0].Services
		must.NotNil(t, services[0].Expose)
		must.Eq(t, &api.ServiceLoadBalancer{Class: "aws-nlb", Port: 443, Internal: true}, services[0].Expose.LoadBalancer)
		must.NotNil(t, services[1].Expose)
		must.Nil(t, services[1].Expose.LoadBalancer)
	}

	t.Run("hcl2", func(t *testing.T) {
		job, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(serviceExposeJob)})
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1", func(t *testing.T) {
		job, err := jobspec.Parse(strings.NewReader(serviceExposeJob))
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("invalid key", func(t *testing.T) {
		bad := strings.Replace(serviceExposeJob, "internal = true", "intern = true", 1)
		_, err := jobspec.Parse(strings.NewReader(bad))
		must.ErrorContains(t, err, "intern")
		_, err = ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(bad)})
		must.Error(t, err)
	})
}
