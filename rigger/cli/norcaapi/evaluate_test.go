// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

// install is the install of the tests.
var install = rig.Install{Release: "shop", Namespace: "apps", Region: "eu"}

// evaluate evaluates the module in fsys with the job decoder of Norca.
func evaluate(t *testing.T, fsys fstest.MapFS) (*rig.Result, hcl.Diagnostics) {
	t.Helper()

	eng, err := rig.New(rig.WithJobDecoder(norcaapi.JobDecoder{}))
	if err != nil {
		t.Fatal(err)
	}

	m, diags := eng.LoadModule(t.Context(), fsys)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return eng.Evaluate(t.Context(), m, install)
}

// firstJob decodes the job of the first resource.
func firstJob(t *testing.T, res *rig.Result) *api.Job {
	t.Helper()

	var job api.Job
	if err := json.Unmarshal(res.Resources[0].Spec, &job); err != nil {
		t.Fatal(err)
	}

	return &job
}

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

      template {
        destination = "local/app.env"
        data        = <<-EOT
          DIR=${runtime_env("NORCA_ALLOC_DIR")}
          ID=${NORCA_ALLOC_ID}
          PW=${secret_ref("app/db", "password")}
        EOT
      }
    }
  }
}
`)},
	}

	res, diags := evaluate(t, fsys)
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

	checkRuntime(t, &job)
}

// checkRuntime checks the job of TestEngine_Evaluate: install-time values
// resolved, runtime references written for where they land.
func checkRuntime(t *testing.T, job *api.Job) {
	t.Helper()

	if *job.ID != "shop-app-1" || *job.TaskGroups[0].Count != 2 {
		t.Errorf("id, count = %q, %d, want shop-app-1, 2", *job.ID, *job.TaskGroups[0].Count)
	}

	data := *job.TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl
	for _, want := range []string{`DIR={{ env "NORCA_ALLOC_DIR" }}`, `ID={{ env "NORCA_ALLOC_ID" }}`, `PW={{ with index (norcaSecret "app/db") "password" }}`} {
		if !strings.Contains(data, want) {
			t.Errorf("template data = %q, want it to contain %q", data, want)
		}
	}

	args, _ := job.TaskGroups[0].Tasks[0].Config["args"].([]any)
	if len(args) != 2 || args[0] != "${attr.kernel.name}" || args[1] != "${NORCA_ALLOC_ID}" {
		t.Errorf("args = %v, want the runtime references as written", args)
	}
}

// TestEngine_Evaluate_TemplateOnly refuses a secret outside a task template.
func TestEngine_Evaluate_TemplateOnly(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"app.norca.hcl": {Data: []byte(`job "app" {
  group "app" {
    task "app" {
      driver = "docker"

      env {
        PW = secret_ref("app/db", "password")
      }
    }
  }
}
`)},
	}

	_, diags := evaluate(t, fsys)
	if want := "secret_ref: can be used only in the data of a task template"; !strings.Contains(diags.Error(), want) {
		t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), want)
	}
}

// TestJobDecoder_Hook writes a hook into the meta of the job.
func TestJobDecoder_Hook(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"init.norca.hcl": {Data: []byte(`job "init" {
  type = "batch"

  hook {
    on      = ["post-install", "post-upgrade"]
    weight  = 10
    timeout = "5m"
    delete  = "on-success"
  }

  group "init" {
    task "init" {
      driver = "docker"
    }
  }
}
`)},
	}

	res, diags := evaluate(t, fsys)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	job := firstJob(t, res)

	expected := map[string]string{
		api.ReleaseMetaHook: "post-install,post-upgrade", api.ReleaseMetaHookWeight: "10",
		api.ReleaseMetaHookTimeout: "5m", api.ReleaseMetaHookDelete: "on-success",
	}
	for k, v := range expected {
		if job.Meta[k] != v {
			t.Errorf("meta %s = %q, want %q", k, job.Meta[k], v)
		}
	}
}
