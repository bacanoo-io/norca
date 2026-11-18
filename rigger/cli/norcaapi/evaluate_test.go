// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/norca/api"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

// TestEngine_Evaluate evaluates a module through the engine with the job
// decoder of Norca: install-time values resolved, runtime references kept,
// meta-arguments not seen by the decoder.
func TestEngine_Evaluate(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"metadata.hcl":  {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"variables.hcl": {Data: []byte("variable \"count\" {\n  default = 2\n}\n")},
		"locals.hcl":    {Data: []byte("locals {\n  job = \"${rig.release}-app\"\n}\n")},
		"app.norca.hcl": {Data: []byte(`job "app" {
  count       = 2
  id          = "${local.job}-${count.index}"
  datacenters = ["dc1"]

  group "app" {
    count = var.count

    task "app" {
      driver = "docker"

      config {
        image = "busybox"
        args  = ["${attr.kernel.name}", "${NORCA_ALLOC_ID}"]
      }
    }
  }
}
`)},
	}

	eng, err := rig.New(rig.WithJobDecoder(norcaapi.JobDecoder{}))
	if err != nil {
		t.Fatal(err)
	}

	m, diags := eng.LoadModule(t.Context(), fsys)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	res, diags := eng.Evaluate(t.Context(), m, rig.Install{Release: "shop", Namespace: "apps", Region: "eu"})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(res.Resources) != 2 || res.Resources[1].Name != "app[1]" {
		t.Fatalf("resources = %+v, want app[0] and app[1]", res.Resources)
	}

	var job api.Job
	if err := json.Unmarshal(res.Resources[1].Spec, &job); err != nil {
		t.Fatal(err)
	}

	if *job.ID != "shop-app-1" || *job.TaskGroups[0].Count != 2 {
		t.Errorf("id, count = %q, %d, want shop-app-1, 2", *job.ID, *job.TaskGroups[0].Count)
	}

	args, _ := job.TaskGroups[0].Tasks[0].Config["args"].([]any)
	if len(args) != 2 || args[0] != "${attr.kernel.name}" || args[1] != "${NORCA_ALLOC_ID}" {
		t.Errorf("args = %v, want the runtime references as written", args)
	}
}
