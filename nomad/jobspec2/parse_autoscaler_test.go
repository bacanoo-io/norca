// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestParse_ScalingEngine(t *testing.T) {
	ci.Parallel(t)

	hcl := `
job "example" {
  group "api" {
    count = 2

    scaling {
      min    = 2
      max    = 20
      engine = "builtin"

      policy {
        evaluation_interval = "30s"
      }
    }

    task "app" {
      driver = "docker"
    }
  }
}
`
	out, err := ParseWithConfig(&ParseConfig{
		Path:    "input.hcl",
		Body:    []byte(hcl),
		AllowFS: false,
	})
	must.NoError(t, err)
	must.NotNil(t, out.TaskGroups[0].Scaling)
	must.Eq(t, "builtin", out.TaskGroups[0].Scaling.Engine)
	must.Eq(t, "horizontal", out.TaskGroups[0].Scaling.Type)
}
