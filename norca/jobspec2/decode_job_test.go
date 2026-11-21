// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// riggerJobFile is a job file of a module as an author writes it (design 4.4):
// install-time references (var, local, rigger, a function of Rigger, a dynamic
// block) next to runtime references the cluster resolves on the node.
const riggerJobFile = `job "web" {
  name        = local.name
  datacenters = var.datacenters

  meta {
    release = rigger.release
  }

  constraint {
    attribute = "${attr.kernel.name}"
    value     = "linux"
  }

  group "web" {
    count = var.count

    network {
      port "http" {
        to = 8080
      }

      dynamic "port" {
        for_each = var.metrics ? ["metrics"] : []
        labels   = [port.value]
        content {
          to = 9090
        }
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = image_ref(var.image, var.digest)
      }

      env {
        ALLOC = "${NOMAD_ALLOC_DIR}/data"
        NODE  = "${node.class}"
        LEVEL = var.log_level
      }
    }
  }
}
`

// imageRef stands for the function image_ref of Rigger.
var imageRef = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "image", Type: cty.String}, {Name: "digest", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		if args[1].AsString() == "" {
			return args[0], nil
		}

		return cty.StringVal(args[0].AsString() + "@" + args[1].AsString()), nil
	},
})

func riggerContext(src []byte) *hcl.EvalContext {
	funcs := Functions("", false)
	funcs["image_ref"] = imageRef

	return &hcl.EvalContext{
		Functions: funcs,
		Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{
				"datacenters": cty.ListVal([]cty.Value{cty.StringVal("eu-1")}),
				"count":       cty.NumberIntVal(3),
				"metrics":     cty.True,
				"image":       cty.StringVal("ghcr.io/example/web:1.4.2"),
				"digest":      cty.StringVal("sha256:abc"),
				"log_level":   cty.StringVal("debug"),
			}),
			"local":  cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("shop")}),
			"rigger": cty.ObjectVal(map[string]cty.Value{"release": cty.StringVal("shop")}),
		},
		// What Rigger's evaluator does for a runtime reference: give back its
		// text, as ParseWithConfig does (types.config.go).
		UndefinedVariable: func(t hcl.Traversal) (cty.Value, hcl.Diagnostics) {
			r := t.SourceRange()

			return cty.StringVal("${" + string(src[r.Start.Byte:r.End.Byte]) + "}"), nil
		},
	}
}

func TestDecodeJob(t *testing.T) {
	t.Parallel()

	src := []byte(riggerJobFile)

	file, diags := hclsyntax.ParseConfig(src, "web.norca.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	content, diags := file.Body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "job", LabelNames: []string{"name"}}},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	job, diags := DecodeJob(content.Blocks[0], riggerContext(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	group := job.TaskGroups[0]
	task := group.Tasks[0]
	image, _ := task.Config["image"].(string)

	var ports []string
	for _, p := range group.Networks[0].DynamicPorts {
		ports = append(ports, p.Label)
	}

	tests := []struct {
		name     string
		got      any
		expected any
	}{
		{name: "job ID from the label", got: *job.ID, expected: "web"},
		{name: "name from local", got: *job.Name, expected: "shop"},
		{name: "datacenter from var", got: job.Datacenters[0], expected: "eu-1"},
		{name: "meta from rigger", got: job.Meta["release"], expected: "shop"},
		{name: "constraint passed through", got: job.Constraints[0].LTarget, expected: "${attr.kernel.name}"},
		{name: "count from var", got: *group.Count, expected: 3},
		{name: "ports of the dynamic block", got: len(ports), expected: 2},
		{name: "image from a function of Rigger", got: image, expected: "ghcr.io/example/web:1.4.2@sha256:abc"},
		{name: "task variable passed through", got: task.Env["ALLOC"], expected: "${NOMAD_ALLOC_DIR}/data"},
		{name: "node attribute passed through", got: task.Env["NODE"], expected: "${node.class}"},
		{name: "env from var", got: task.Env["LEVEL"], expected: "debug"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.expected {
				t.Errorf("got %v, want %v", tt.got, tt.expected)
			}
		})
	}
}
