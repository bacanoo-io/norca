// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
)

func TestParse_Disruption(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "web" {
  group "api" {
    count = 6

    disruption {
      min_healthy = 4
    }

    task "api" {
      driver = "docker"
    }
  }

  group "cache" {
    disruption {
      max_unavailable = "25%"
    }

    task "redis" {
      driver = "docker"
    }
  }

  group "plain" {
    task "plain" {
      driver = "docker"
    }
  }
}
`
	job, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.NoError(t, err)

	must.Eq(t, &api.DisruptionBudget{MinHealthy: pointer.Of("4")}, job.TaskGroups[0].Disruption)
	must.Eq(t, &api.DisruptionBudget{MaxUnavailable: pointer.Of("25%")}, job.TaskGroups[1].Disruption)
	must.Nil(t, job.TaskGroups[2].Disruption)
}

func TestParse_Disruption_UnknownAttribute(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "web" {
  group "api" {
    disruption {
      max_parallel = 2
    }
    task "api" {
      driver = "docker"
    }
  }
}
`
	_, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.ErrorContains(t, err, "max_parallel")
}
