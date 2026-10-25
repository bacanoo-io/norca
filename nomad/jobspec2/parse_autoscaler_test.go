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

func TestParse_ScalingTypedPolicy(t *testing.T) {
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
        evaluation_interval     = "30s"
        cooldown                = "2m"
        on_check_error          = "ignore"
        scale_during_deployment = "up_only"

        check "cpu" {
          metric   = "nomad.cpu"
          strategy = "target-value"
          target   = 70
        }
        check "latency" {
          source   = "prometheus"
          query    = "histogram_quantile(0.95, sum(rate(http_req_seconds_bucket{job=\"api\"}[1m])) by (le))"
          strategy = "threshold"
          upper    = 0.3
          delta    = 1
        }
        check "legacy" {
          query = "avg_cpu"
          strategy "target-value" {
            target = 50
          }
        }
      }
    }

    task "app" {
      driver = "docker"
    }
  }
}
`
	out, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
	must.NoError(t, err)

	// The policy block stays an untyped document; the servers derive the
	// typed policy from it.
	p := out.TaskGroups[0].Scaling.Policy
	must.Eq[any](t, "30s", p["evaluation_interval"])
	must.Eq[any](t, "up_only", p["scale_during_deployment"])
	checks := p["check"].([]map[string]interface{})
	must.Len(t, 3, checks)
	cpu := checks[0]["cpu"].([]map[string]interface{})[0]
	must.Eq[any](t, "nomad.cpu", cpu["metric"])
	must.Eq[any](t, "target-value", cpu["strategy"])
}

func TestParse_ScalingTypedPolicy_Invalid(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name   string
		policy string
		err    string
	}{
		{
			name:   "bad interval",
			policy: `evaluation_interval = "often"`,
			err:    "policy.evaluation_interval must be a duration",
		},
		{
			name:   "interval not a string",
			policy: `cooldown = 30`,
			err:    "policy.cooldown must be a duration string",
		},
		{
			name:   "bad on_check_error",
			policy: `on_check_error = "maybe"`,
			err:    "policy.on_check_error must be one of",
		},
		{
			name:   "bad scale_during_deployment",
			policy: `scale_during_deployment = "always"`,
			err:    "policy.scale_during_deployment must be one of",
		},
		{
			name: "bad query window",
			policy: `check "cpu" {
  query        = "q"
  query_window = "later"
  strategy     = "threshold"
}`,
			err: "policy.check[cpu].query_window must be a duration",
		},
		{
			name: "two strategy blocks",
			policy: `check "cpu" {
  query = "q"
  strategy "threshold" {}
  strategy "target-value" {}
}`,
			err: "policy.check[cpu] must have exactly one strategy block",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hcl := `
job "example" {
  group "api" {
    scaling {
      max = 20
      policy {
` + tc.policy + `
      }
    }
    task "app" {
      driver = "docker"
    }
  }
}
`
			_, err := ParseWithConfig(&ParseConfig{Path: "input.hcl", Body: []byte(hcl)})
			must.ErrorContains(t, err, tc.err)
		})
	}
}
