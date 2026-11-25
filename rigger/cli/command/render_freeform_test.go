// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/jobspec2"
)

// freeFormJob has blocks where the job has no schema: the policy of a
// scaling block, with a check and its strategy, and the config of a task
// driver, with a nested block, a labelled block, a block given twice and an
// object written as an attribute.
const freeFormJob = `job "app" {
  group "web" {
    count = 1

    scaling {
      min = 1
      max = 5

      policy {
        evaluation_interval = "30s"
        cooldown            = "2m"

        check "cpu" {
          source = "nomad-apm"
          query  = "avg_cpu"

          strategy "target-value" {
            target = 70
          }
        }

        check "mem" {
          metric   = "nomad.memory"
          strategy = "target-value"
          target   = 80
        }
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = "nginx:1.28.0-alpine"
        ports = ["http"]

        logging {
          type = "json-file"

          config {
            max-size = "10m"
          }
        }

        mount {
          type   = "bind"
          source = "local/a"
          target = "/a"
        }

        mount {
          type   = "bind"
          source = "local/b"
          target = "/b"
        }

        labels = {
          team = "web"
        }
      }
    }
  }
}
`

// A job of a module decodes as Norca's own parser decodes the same file
// (norca job run): in the parts of a job without a schema a block is a value,
// as it is for jobspec2. Rigger refused them ("Unexpected "check" block;
// Blocks are not allowed here"), so a module could not write a scaling
// policy with checks or a driver config with nested blocks.
func TestRun_ModuleRender_FreeFormBlocks(t *testing.T) {
	t.Parallel()

	dir := writeTree(t, map[string]string{
		metadataFile:    "module {\n  name    = \"freeform\"\n  version = \"1.0.0\"\n}\n",
		"app.norca.hcl": freeFormJob,
	})

	out := riggerRun(t, exitOK, nil, cmdModule, cmdRender, dir, "-release", "web")

	var rendered struct {
		Resources []struct {
			Kind string  `json:"kind"`
			Spec api.Job `json:"spec"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}

	if len(rendered.Resources) != 1 {
		t.Fatalf("rendered %d resources, want the job:\n%s", len(rendered.Resources), out)
	}

	want, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{Path: "app.nomad.hcl", Body: []byte(freeFormJob)})
	if err != nil {
		t.Fatal(err)
	}

	got := rendered.Resources[0].Spec

	if g, w := jsonOf(t, got.TaskGroups[0].Scaling.Policy), jsonOf(t, want.TaskGroups[0].Scaling.Policy); g != w {
		t.Errorf("the scaling policy is\n%s\nwant, as norca job run reads it,\n%s", g, w)
	}

	if g, w := jsonOf(t, got.TaskGroups[0].Tasks[0].Config), jsonOf(t, want.TaskGroups[0].Tasks[0].Config); g != w {
		t.Errorf("the config of the driver is\n%s\nwant, as norca job run reads it,\n%s", g, w)
	}
}

// jsonOf is v as compact JSON with sorted keys.
func jsonOf(t *testing.T, v any) string {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}

	return compact.String()
}

// A block the decoder takes by its schema may still be written as an
// attribute with an object value (design 4.1): the policy of a scaling block
// as an object is the same policy as the block.
func TestRun_ModuleRender_BlockAsObjectInFreeFormBody(t *testing.T) {
	t.Parallel()

	const asObject = `job "app" {
  group "web" {
    scaling {
      min = 1
      max = 5

      policy = {
        evaluation_interval = "30s"
        cooldown            = "2m"
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = "nginx:1.28.0-alpine"
      }
    }
  }
}
`

	const asBlock = `job "app" {
  group "web" {
    scaling {
      min = 1
      max = 5

      policy {
        evaluation_interval = "30s"
        cooldown            = "2m"
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = "nginx:1.28.0-alpine"
      }
    }
  }
}
`

	policyOf := func(job string) string {
		dir := writeTree(t, map[string]string{
			metadataFile:    "module {\n  name    = \"policy\"\n  version = \"1.0.0\"\n}\n",
			"app.norca.hcl": job,
		})

		out := riggerRun(t, exitOK, nil, cmdModule, cmdRender, dir, "-release", "web")

		var rendered struct {
			Resources []struct {
				Spec api.Job `json:"spec"`
			} `json:"resources"`
		}
		if err := json.Unmarshal([]byte(out), &rendered); err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}

		return jsonOf(t, rendered.Resources[0].Spec.TaskGroups[0].Scaling.Policy)
	}

	if got, want := policyOf(asObject), policyOf(asBlock); got != want {
		t.Errorf("the policy written as an object is\n%s\nwant the policy of the block\n%s", got, want)
	}
}
