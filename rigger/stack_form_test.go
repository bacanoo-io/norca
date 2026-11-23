// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// A published stack has an install form as a module has, built from its
// variables.hcl with the tabs of the ui block of its metadata.hcl, and its
// README.
func TestStack_FormSchemaAndReadme(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"stacks/site/metadata.hcl": {Data: []byte(`stack {
  name    = "site"
  version = "1.0.0"
}

ui {
  tab "database" { label = "Database" }
}
`)},
		"stacks/site/stack.hcl": {Data: []byte("rigger {\n  name = \"site\"\n}\n")},
		"stacks/site/variables.hcl": {Data: []byte(`variable "database_node" {
  type = string
  ui { tab = "database" }
}

variable "replicas" {
  type    = number
  default = 2
}
`)},
		"stacks/site/README.md": {Data: []byte("# site\n")},
	}

	e, err := rigger.New()
	if err != nil {
		t.Fatal(err)
	}

	s, diags := e.ReadStack(t.Context(), files, "stacks/site")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	schema := s.FormSchema()

	props, _ := schema["properties"].(map[string]any)
	node, _ := props["database_node"].(map[string]any)

	if node == nil || node["type"] != "string" || node["x-ui-tab"] != "database" {
		t.Errorf("database_node = %v", node)
	}

	if required, _ := schema["required"].([]string); len(required) != 1 || required[0] != "database_node" {
		t.Errorf("required = %v, want the variables without a default", schema["required"])
	}

	if tabs, _ := schema["x-ui-tabs"].([]map[string]any); len(tabs) != 1 || tabs[0]["label"] != "Database" {
		t.Errorf("x-ui-tabs = %v, want the tab of metadata.hcl", schema["x-ui-tabs"])
	}

	readme, err := s.Readme()
	if err != nil || !strings.Contains(string(readme), "# site") {
		t.Errorf("Readme() = %q, %v", readme, err)
	}
}
