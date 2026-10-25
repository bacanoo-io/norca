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

const serviceMTLSJob = `
job "mtls" {
  group "group" {
    service {
      name     = "api"
      port     = "http"
      provider = "nomad"

      mtls {
        mode = "sidecar"

        upstream "db" {
          service    = "postgres"
          local_port = 5432
        }

        upstream "cache" {
          service    = "redis"
          namespace  = "cache"
          region     = "eu"
          gateway    = "edge"
          local_port = 6379
        }
      }
    }

    service {
      name     = "admin"
      provider = "nomad"

      mtls {}
    }

    service {
      name     = "mtls-gateway"
      port     = "gw"
      provider = "nomad"

      mtls {
        mode = "gateway"
      }
    }
  }
}
`

func TestParse_ServiceMTLS(t *testing.T) {
	ci.Parallel(t)

	check := func(t *testing.T, job *api.Job) {
		m := job.TaskGroups[0].Services[0].MTLS
		must.NotNil(t, m)
		must.Eq(t, "sidecar", m.Mode)
		must.Len(t, 2, m.Upstreams)
		must.Eq(t, &api.ServiceMTLSUpstream{Name: "db", Service: "postgres", LocalPort: 5432}, m.Upstreams[0])
		must.Eq(t, &api.ServiceMTLSUpstream{
			Name: "cache", Service: "redis", Namespace: "cache", Region: "eu", Gateway: "edge", LocalPort: 6379}, m.Upstreams[1])

		empty := job.TaskGroups[0].Services[1].MTLS
		must.NotNil(t, empty)
		empty.Canonicalize()
		must.Eq(t, api.MTLSModeApp, empty.Mode)

		must.Eq(t, api.MTLSModeGateway, job.TaskGroups[0].Services[2].MTLS.Mode)
	}

	t.Run("hcl2", func(t *testing.T) {
		job, err := ParseWithConfig(&ParseConfig{
			Path: "input.hcl",
			Body: []byte(serviceMTLSJob),
		})
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1", func(t *testing.T) {
		job, err := jobspec.Parse(strings.NewReader(serviceMTLSJob))
		must.NoError(t, err)
		check(t, job)
	})

	t.Run("hcl1 invalid key", func(t *testing.T) {
		_, err := jobspec.Parse(strings.NewReader(strings.Replace(serviceMTLSJob, "local_port = 5432", "local_prt = 5432", 1)))
		must.ErrorContains(t, err, "local_prt")
	})
}
