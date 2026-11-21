// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
)

// spec parses src, the job file of a module, and returns its job block
// with an evaluation context in the way of Rig: the values of var, the
// release in rig, and runtime references given back as their text.
func spec(t *testing.T, src string) host.JobSpec {
	t.Helper()

	file, diags := hclsyntax.ParseConfig([]byte(src), "web.norca.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	content, diags := file.Body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "job", LabelNames: []string{"name"}}},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return host.JobSpec{
		Block: content.Blocks[0],
		EvalContext: &hcl.EvalContext{
			Variables: map[string]cty.Value{
				"var": cty.ObjectVal(map[string]cty.Value{"count": cty.NumberIntVal(2)}),
				"rig": cty.ObjectVal(map[string]cty.Value{"release": cty.StringVal("shop")}),
			},
			UndefinedVariable: func(tr hcl.Traversal) (cty.Value, hcl.Diagnostics) {
				r := tr.SourceRange()

				return cty.StringVal("${" + src[r.Start.Byte:r.End.Byte] + "}"), nil
			},
		},
	}
}

const webJob = `job "web" {
  name = rig.release

  group "web" {
    count = var.count

    task "web" {
      driver = "docker"

      config {
        image = "ghcr.io/example/web:1.4.2"
      }

      env {
        NODE = "${node.class}"
      }
    }
  }
}
`

func TestJobDecoder_DecodeJob(t *testing.T) {
	t.Parallel()

	job, diags := norcaapi.JobDecoder{}.DecodeJob(t.Context(), spec(t, webJob))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if job.ID != "web" {
		t.Errorf("ID = %q, want web", job.ID)
	}

	var decoded api.Job
	if err := json.Unmarshal(job.Spec, &decoded); err != nil {
		t.Fatalf("Spec is not the JSON of an API job: %v", err)
	}

	if *decoded.Name != "shop" {
		t.Errorf("name = %q, want the release shop", *decoded.Name)
	}

	group := decoded.TaskGroups[0]
	if *group.Count != 2 {
		t.Errorf("count = %d, want 2", *group.Count)
	}

	if env := group.Tasks[0].Env["NODE"]; env != "${node.class}" {
		t.Errorf("env NODE = %q, want the runtime reference passed through", env)
	}
}

func TestJobDecoder_DecodeJobErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		isCancelled bool
		spec        host.JobSpec
		expected    string
	}{
		{
			name:     "wrong type",
			spec:     spec(t, strings.Replace(webJob, "count = var.count", `count = "two"`, 1)),
			expected: "web.norca.hcl:5,",
		},
		{
			name:        "cancelled",
			isCancelled: true,
			spec:        spec(t, webJob),
			expected:    "The decoding was cancelled",
		},
		{
			name:     "no block",
			spec:     host.JobSpec{},
			expected: "No job block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.isCancelled {
				cancel()
			}

			_, diags := norcaapi.JobDecoder{}.DecodeJob(ctx, tt.spec)
			if !diags.HasErrors() {
				t.Fatal("DecodeJob() gave no error")
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
