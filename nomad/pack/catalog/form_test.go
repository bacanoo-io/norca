// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

// A pack whose install form and constraints are declared in HCL only.
var formPack = map[string]string{
	"metadata.hcl": `
app {
  url = "https://example.com"
}

pack {
  name    = "kv"
  version = "1.0.0"
}

ui {
  tab "general"  { label = "General" }
  tab "security" {
    label = "Security"
    icon  = "lock"
  }
}
`,
	"variables.hcl": `
variable "job_name" {
  type    = string
  default = "kv"
  ui {
    group = "Identity"
    order = 1
  }
  validation {
    pattern = "^[a-z][a-z0-9-]*$"
  }
}

variable "mode" {
  type    = string
  default = "single"
  ui {
    options       = ["single", "cluster"]
    option_labels = { cluster = "Clustered" }
  }
}

variable "replicas" {
  type    = number
  default = 3
  ui {
    tab     = "general"
    widget  = "slider"
    show_if = { field = "mode", equals = "cluster" }
  }
  validation {
    min = 1
    max = 7
  }
}

rule "cluster_replicas" {
  condition     = var.mode != "cluster" || var.replicas >= 3
  error_message = "A cluster needs at least 3 replicas."
}

variable "admin_password" {
  type    = string
  default = ""
  ui {
    tab   = "security"
    label = "Admin password"
  }
  secret {
    path = "nomad/jobs/kv"
    item = "password"
  }
}
`,
	"templates/kv.nomad.tpl": `job [[ var "job_name" . | quote ]] {
  group "kv" {
    count = [[ var "replicas" . ]]
    meta {
      password = [[ var "admin_password" . | quote ]]
    }
    task "kv" {
      driver = "docker"
      config {
        image = "kv:1"
      }
    }
  }
}
`,
}

func writeFormPack(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "kv")
	for name, content := range formPack {
		path := filepath.Join(dir, filepath.FromSlash(name))
		must.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return dir
}

func TestSynthesizeFormSchema_HCL(t *testing.T) {
	ci.Parallel(t)
	schema, err := SynthesizeFormSchema(writeFormPack(t))
	must.NoError(t, err)

	tabs := schema["x-ui-tabs"].([]any)
	must.Len(t, 2, tabs)
	must.Eq[any](t, "security", tabs[1].(map[string]any)["id"])
	must.Eq[any](t, "lock", tabs[1].(map[string]any)["icon"])

	props := schema["properties"].(map[string]any)
	job := props["job_name"].(map[string]any)
	must.Eq[any](t, "general", job["x-ui-tab"])
	must.Eq[any](t, "Identity", job["x-ui-group"])
	must.Eq[any](t, "^[a-z][a-z0-9-]*$", job["pattern"])

	mode := props["mode"].(map[string]any)
	must.Eq[any](t, "segmented", mode["x-ui-widget"])
	must.Eq[any](t, []any{"single", "cluster"}, mode["enum"])

	replicas := props["replicas"].(map[string]any)
	must.Eq[any](t, "general", replicas["x-ui-tab"], must.Sprint("the ui tab wins over the name"))
	must.Eq[any](t, "slider", replicas["x-ui-widget"])
	must.Eq[any](t, 7, replicas["maximum"])
	must.Eq[any](t, map[string]any{"field": "mode", "equals": "cluster"}, replicas["x-ui-show-if"])

	pass := props["admin_password"].(map[string]any)
	must.Eq[any](t, "security", pass["x-ui-tab"])
	must.Eq[any](t, "secret", pass["x-ui-widget"])
	must.Eq[any](t, "Admin password", pass["title"])
	must.Eq[any](t, "nomad/jobs/kv", pass["x-ui-secret-path"])
	must.Eq[any](t, "password", pass["x-ui-secret-item"])
}

func TestRenderPack_HCLValidation(t *testing.T) {
	ci.Parallel(t)
	dir := writeFormPack(t)
	render := func(values map[string]any) error {
		_, err := RenderPack(RenderInput{
			PackDir:      dir,
			RegistryName: "test",
			PackName:     "kv",
			Version:      "1.0.0",
			ReleaseName:  "kv",
			Values:       values,
		})
		return err
	}

	must.NoError(t, render(map[string]any{"mode": "cluster", "replicas": 5}))
	must.ErrorContains(t, render(map[string]any{"replicas": 9}), "maximum")
	must.ErrorContains(t, render(map[string]any{"mode": "both"}), "mode")
	must.ErrorContains(t, render(map[string]any{"job_name": "Bad Name"}), "job_name")
	must.ErrorContains(t, render(map[string]any{"mode": "cluster", "replicas": 2}), "A cluster needs at least 3 replicas.")

	out, err := RenderPack(RenderInput{PackDir: dir, RegistryName: "test", PackName: "kv", ReleaseName: "kv"})
	must.NoError(t, err)
	must.Len(t, 1, out.VariablesSchemas, must.Sprint("the servers validate again"))
}

func TestRenderPack_SecretVariable(t *testing.T) {
	ci.Parallel(t)
	out, err := RenderPack(RenderInput{
		PackDir:      writeFormPack(t),
		RegistryName: "test",
		PackName:     "kv",
		ReleaseName:  "kv",
		Values:       map[string]any{"admin_password": "s3cret-value", "replicas": 2},
	})
	must.NoError(t, err)
	must.Len(t, 1, out.Secrets)
	must.Eq(t, "nomad/jobs/kv", out.Secrets[0].Path)
	must.Eq(t, map[string]string{"password": "s3cret-value"}, out.Secrets[0].Items)
	_, inValues := out.Values["admin_password"]
	must.False(t, inValues, must.Sprint("not a release value"))
	b, err := json.Marshal(out)
	must.NoError(t, err)
	must.Eq(t, 1, strings.Count(string(b), "s3cret-value"), must.Sprint("only in the secrets, not in the jobs or the schema values"))
}
