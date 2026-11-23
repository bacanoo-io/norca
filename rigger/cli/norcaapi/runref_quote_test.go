// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// A runtime value that is the whole value of a string of an encoded
// configuration is in quotes wherever the configuration lands: in the data
// of a task template, where the template writes it, and in an attribute of
// the job, where the client does. The module is the one of the bug report
// of 2026-11-23 (findings L2 and L3), decoded by the job decoder of Norca.
func TestJobDecoder_QuotedRuntimeValues(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		metadataFile: {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		appFile: {Data: []byte(`job "app" {
  group "app" {
    task "app" {
      driver = "docker"

      env {
        CFG = jsonencode({ dir = runtime_env("NORCA_ALLOC_DIR") })
      }

      template {
        destination = "local/app.json"
        data        = jsonencode({ addr = service_address("db"), dir = runtime_env("NORCA_ALLOC_DIR") })
      }

      template {
        destination = "local/app.toml"
        data        = tomlencode({ addr = service_address("db") })
      }

      template {
        destination = "local/app.yaml"
        data        = yamlencode({ addr = service_address("db") })
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

	task := firstJob(t, res).TaskGroups[0].Tasks[0]

	const address = `"{{ with nomadService "db" }}{{ with index . 0 }}{{ .Address }}:{{ .Port }}{{ end }}{{ end }}"`

	templates := []struct {
		name     string
		expected string
	}{
		{name: "json", expected: `{"addr":` + address + `,"dir":"{{ env "NORCA_ALLOC_DIR" | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}"}`},
		{name: "toml", expected: "addr = " + address + "\n"},
		{name: "yaml", expected: "addr: " + address + "\n"},
	}

	if len(task.Templates) != len(templates) {
		t.Fatalf("the task has %d templates, want %d", len(task.Templates), len(templates))
	}

	for i, tt := range templates {
		if got := *task.Templates[i].EmbeddedTmpl; got != tt.expected {
			t.Errorf("the %s template is\n%s\nwant\n%s", tt.name, got, tt.expected)
		}
	}

	var cfg map[string]string
	if err := json.Unmarshal([]byte(task.Env["CFG"]), &cfg); err != nil {
		t.Fatalf("the env CFG %q is not JSON: %v", task.Env["CFG"], err)
	}

	if cfg["dir"] != "${NORCA_ALLOC_DIR}" || !strings.Contains(task.Env["CFG"], `"${NORCA_ALLOC_DIR}"`) {
		t.Errorf("the env CFG = %q, want dir to be the quoted ${NORCA_ALLOC_DIR}", task.Env["CFG"])
	}
}
