// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
)

const formMetadata = `module {
  name    = "form"
  version = "1.0.0"
}

ui {
  tab "general" { label = "General" }
  tab "auth" {
    label = "Users"
    icon  = "key"
  }
}
`

const formVariables = `variable "count" {
  type    = number
  default = 2

  validation {
    min     = 1
    max     = 9
    integer = true
  }

  ui {
    tab    = "general"
    widget = "slider"
    label  = "Instances"
  }
}

variable "mode" {
  type    = string
  default = "single"

  validation {
    one_of = ["single", "cluster"]
  }

  ui {
    option_labels = { single = "One server", cluster = "A cluster" }
  }
}

variable "name" {
  type        = string
  description = "The name."

  validation {
    pattern    = "^[a-z]+$"
    max_length = 20
  }
}

variable "users" {
  type    = list(object({ name = string, admin = optional(bool, false) }))
  default = []

  validation {
    max_items = 5
    unique_by = "name"
  }
}

variable "labels" {
  type    = map(string)
  default = {}

  validation {
    key_pattern = "^[a-z]+$"
  }
}

variable "password_path" {
  type    = string
  default = "app/db"

  secret {
    path = "app/db"
    item = "password"
  }

  ui {
    tab     = "auth"
    show_if = "mode == \"cluster\""
  }
}

variable "token" {
  type      = string
  default   = "dev"
  sensitive = true

  ui {
    show_if = "length(users) > 0"
  }
}

variable "tls" {
  type    = bool
  default = false

  ui {
    show_if = "!var.mode"
  }
}
`

// FormSchema is the install form the web UI renders: a JSON Schema of the
// variables with the x-ui-* hints.
func TestModule_FormSchema(t *testing.T) {
	t.Parallel()

	e := engine(t)

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{
		metadataFile:  {Data: []byte(formMetadata)},
		variablesFile: {Data: []byte(formVariables)},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	// Through JSON, as the UI gets it.
	data, err := json.Marshal(m.FormSchema())
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Type       string                    `json:"type"`
		Required   []string                  `json:"required"`
		Tabs       []map[string]any          `json:"x-ui-tabs"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if got.Type != "object" || !reflect.DeepEqual(got.Required, []string{"name"}) {
		t.Errorf("type %q, required %v", got.Type, got.Required)
	}

	wantTabs := []map[string]any{
		{"id": "general", "label": "General", "order": float64(0)},
		{"id": "auth", "label": "Users", "icon": "key", "order": float64(1)},
	}
	if !reflect.DeepEqual(got.Tabs, wantTabs) {
		t.Errorf("tabs %v", got.Tabs)
	}

	p := got.Properties
	expect := func(name, key string, want any) {
		t.Helper()

		if !reflect.DeepEqual(p[name][key], want) {
			t.Errorf("%s.%s = %#v, want %#v", name, key, p[name][key], want)
		}
	}

	expect("count", "type", "integer")
	expect("count", "minimum", float64(1))
	expect("count", "maximum", float64(9))
	expect("count", "default", float64(2))
	expect("count", "title", "Instances")
	expect("count", "x-ui-widget", "slider")
	expect("count", "x-ui-tab", "general")
	expect("count", "x-ui-order", float64(10))

	expect("mode", "enum", []any{"single", "cluster"})
	expect("mode", "x-ui-enum-labels", map[string]any{"single": "One server", "cluster": "A cluster"})

	expect("name", "pattern", "^[a-z]+$")
	expect("name", "maxLength", float64(20))
	expect("name", "description", "The name.")

	expect("users", "type", "array")
	expect("users", "maxItems", float64(5))
	expect("users", "x-ui-unique-by", "name")
	expect("users", "items", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":  map[string]any{"type": "string"},
			"admin": map[string]any{"type": "boolean"},
		},
		"required": []any{"name"},
	})

	expect("labels", "type", "object")
	expect("labels", "propertyNames", map[string]any{"pattern": "^[a-z]+$"})

	expect("password_path", "x-ui-widget", "secret")
	expect("password_path", "x-ui-secret-path", "app/db")
	expect("password_path", "x-ui-secret-item", "password")
	expect("password_path", "x-ui-show-if", []any{"==", "mode", "cluster"})

	// A sensitive variable: no default in the form; a condition the form
	// can't evaluate is kept as written.
	expect("token", "default", nil)
	expect("token", "x-ui-secret", true)
	expect("token", "x-ui-show-if", nil)
	expect("token", "x-ui-show-if-hcl", "length(users) > 0")

	expect("tls", "x-ui-show-if", []any{"falsy", "mode"})
}

// Every example module has a form: each of its variables a property, and
// a JSON encoding.
func TestModule_FormSchemaOfExamples(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ root, dir string }{
		{"examples/module/web", "."},
		{"examples/registry", "modules/nats"},
		{"examples/registry", "modules/postgres"},
		{"examples/registry", "modules/redis"},
	} {
		e := engine(t)
		dir := tc.root + "/" + tc.dir

		m, diags := e.LoadModuleAt(t.Context(), os.DirFS(tc.root), tc.dir)
		if diags.HasErrors() {
			t.Fatalf("%s: %v", dir, diags)
		}

		schema := m.FormSchema()

		props, _ := schema["properties"].(map[string]any)
		if len(props) != len(m.Variables()) {
			t.Errorf("%s: %d properties for %d variables", dir, len(props), len(m.Variables()))
		}

		if _, err := json.Marshal(schema); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}
