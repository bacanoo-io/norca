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

// A job file of a module as an author writes it (design 4.4): install-time
// references (var, local, rig, a function of Rig, a dynamic block) next to
// runtime references the cluster resolves on the node.
const rigJobFile = `job "web" {
  name        = local.name
  datacenters = var.datacenters

  meta {
    release = rig.release
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

// passthrough is what Rig's evaluator would do for a runtime reference:
// give back its text, as the decoder of Norca does (types.config.go).
func passthrough(src []byte) func(hcl.Traversal) (cty.Value, hcl.Diagnostics) {
	return func(t hcl.Traversal) (cty.Value, hcl.Diagnostics) {
		r := t.SourceRange()
		return cty.StringVal("${" + string(src[r.Start.Byte:r.End.Byte]) + "}"), nil
	}
}

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

func TestRigSpike_DecodeJob(t *testing.T) {
	src := []byte(rigJobFile)
	file, diags := hclsyntax.ParseConfig(src, "web.norca.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	content, diags := file.Body.Content(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "job", LabelNames: []string{"name"}}}})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	funcs := Functions("", false)
	funcs["image_ref"] = imageRef
	ctx := &hcl.EvalContext{
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
			"local": cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("shop")}),
			"rig":   cty.ObjectVal(map[string]cty.Value{"release": cty.StringVal("shop")}),
		},
		UndefinedVariable: passthrough(src),
	}

	job, diags := DecodeJob(content.Blocks[0], ctx)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	check("job ID", *job.ID, "web")
	check("job name", *job.Name, "shop")
	check("datacenter", job.Datacenters[0], "eu-1")
	check("meta release", job.Meta["release"], "shop")
	check("constraint attribute", job.Constraints[0].LTarget, "${attr.kernel.name}")

	tg := job.TaskGroups[0]
	if *tg.Count != 3 {
		t.Errorf("count = %d, want 3", *tg.Count)
	}
	var labels []string
	for _, p := range tg.Networks[0].DynamicPorts {
		labels = append(labels, p.Label)
	}
	if len(labels) != 2 || labels[1] != "metrics" {
		t.Errorf("ports = %v, want http and metrics", labels)
	}

	task := tg.Tasks[0]
	check("image", task.Config["image"].(string), "ghcr.io/example/web:1.4.2@sha256:abc")
	check("env ALLOC", task.Env["ALLOC"], "${NOMAD_ALLOC_DIR}/data")
	check("env NODE", task.Env["NODE"], "${node.class}")
	check("env LEVEL", task.Env["LEVEL"], "debug")
}
