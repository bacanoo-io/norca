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

func TestParse_ColocateSeparate(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "cache" {
  meta { app = "cache" }

  group "redis" {
    colocate {
      selector { app = "api" }
      scope    = "${node.unique.id}"
      weight   = 50
    }

    colocate {
      selector = { "app.kubernetes.io/name" = "api" }
      namespace = "other"
    }

    separate {
      selector { app = "cache" }
      scope    = "${attr.platform.aws.placement.availability-zone}"
      required = true
    }

    task "redis" {
      driver = "docker"
    }
  }
}
`
	job, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.NoError(t, err)

	tg := job.TaskGroups[0]
	must.Eq(t, []*api.AllocAffinity{
		{
			Selector: map[string]string{"app": "api"},
			Scope:    "${node.unique.id}",
			Weight:   pointer.Of(int8(50)),
		},
		{
			Selector:  map[string]string{"app.kubernetes.io/name": "api"},
			Namespace: "other",
		},
	}, tg.Colocate)
	must.Eq(t, []*api.AllocAffinity{
		{
			Selector: map[string]string{"app": "cache"},
			Scope:    "${attr.platform.aws.placement.availability-zone}",
			Required: true,
		},
	}, tg.Separate)

	// Canonicalization fills in the defaults.
	job.Canonicalize()
	must.Eq(t, api.AllocAffinityDefaultScope, tg.Colocate[1].Scope)
	must.Eq(t, int8(50), *tg.Colocate[1].Weight)
	must.Eq(t, int8(0), *tg.Separate[0].Weight)
}

func TestParse_ColocateSeparate_Invalid(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "cache" {
  group "redis" {
    separate {
      selector { app = "cache" }
      bogus = true
    }
    task "redis" {
      driver = "docker"
    }
  }
}
`
	_, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.ErrorContains(t, err, "bogus")

	hcl = `
job "cache" {
  group "redis" {
    separate {
      selector = { app = "cache" }
      selector { app = "cache" }
    }
    task "redis" {
      driver = "docker"
    }
  }
}
`
	_, err = ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.ErrorContains(t, err, "Duplicate selector block")
}
