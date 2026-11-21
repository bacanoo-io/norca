// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// An object kind of a module is decoded by the parser of Norca: named by
// its name attribute, columns and webhooks in their order, rules from rule
// blocks.
func TestResourceDecoder_ObjectKind(t *testing.T) {
	t.Parallel()

	eng, err := rigger.New(rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}))
	if err != nil {
		t.Fatal(err)
	}

	m, diags := eng.LoadModule(t.Context(), fstest.MapFS{
		metadataFile: {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"record.kind.hcl": {Data: []byte(`kind "record" {
  name        = "Record"
  group       = "example.com"
  description = "A record"
  schema      = "{}"

  rule {
    expression = "has(self.spec)"
    message    = "a spec"
  }

  columns {
    column "Zone" { path = "spec.zone" }
    column "Name" { path = "spec.name" }
  }

  dynamic "webhook" {
    for_each = ["validate", "default"]
    labels   = [webhook.value]

    content {
      mode = webhook.value
      url  = "https://hooks.example.com/${webhook.value}"
    }
  }
}
`)},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	kind := decodedKind(t, eng, m)

	if kind.Kind != "Record" || kind.Group != "example.com" || !kind.Namespaced || len(kind.Rules) != 1 {
		t.Errorf("kind = %+v", kind)
	}

	checkOrder(t, kind)
}

// decodedKind evaluates m and decodes its one resource, an object kind.
func decodedKind(t *testing.T, eng *rigger.Engine, m *rigger.Module) *api.ObjectKind {
	t.Helper()

	res, diags := eng.Evaluate(t.Context(), m, install)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(res.Resources) != 1 || res.Resources[0].Kind != resource.KindObjectKind {
		t.Fatalf("resources = %+v", res.Resources)
	}

	var kind api.ObjectKind
	if err := json.Unmarshal(res.Resources[0].Spec, &kind); err != nil {
		t.Fatal(err)
	}

	return &kind
}

// checkOrder checks the columns and webhooks of the kind of the test.
func checkOrder(t *testing.T, kind *api.ObjectKind) {
	t.Helper()

	if len(kind.Columns) != 2 || kind.Columns[0].Name != "Zone" || kind.Columns[1].Name != "Name" {
		t.Errorf("columns = %+v", kind.Columns)
	}

	if len(kind.Webhooks) != 2 || kind.Webhooks[0].Name != "validate" || kind.Webhooks[1].URL != "https://hooks.example.com/default" {
		t.Errorf("webhooks = %+v", kind.Webhooks)
	}
}
