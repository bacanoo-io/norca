// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestObjectCommands_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &ObjectKindCommand{}
	var _ cli.Command = &ObjectKindApplyCommand{}
	var _ cli.Command = &ObjectKindListCommand{}
	var _ cli.Command = &ObjectKindInspectCommand{}
	var _ cli.Command = &ObjectKindDeleteCommand{}
	var _ cli.Command = &ObjectCommand{}
	var _ cli.Command = &ObjectApplyCommand{}
	var _ cli.Command = &ObjectListCommand{}
	var _ cli.Command = &ObjectGetCommand{}
	var _ cli.Command = &ObjectDeleteCommand{}
	var _ cli.Command = &ObjectEventsCommand{}
	var _ cli.Command = &ObjectScaleCommand{}
}

const testCLIObjectSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["version"],
      "properties": {
        "version":  {"type": "string"},
        "replicas": {"type": "integer", "default": 1},
        "storage":  {"type": "string", "default": "10GiB"}
      }
    }
  }
}`

const testCLIObjectKind = `
kind "PostgresCluster" {
  group  = "db.example.com"
  schema = file("pg.schema.json")

  rule {
    expression = "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)"
    message    = "Storage cannot be reduced."
  }

  columns {
    column "Version" { path = "spec.version" }
    column "Ready"   { path = "status.conditions[?type=='Ready'].status" }
  }

  scale {
    spec_path   = "spec.replicas"
    status_path = "status.replicas"
  }
}
`

func TestParseObjectKind(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "pg.schema.json"), []byte(testCLIObjectSchema), 0o600))
	path := filepath.Join(dir, "pg.hcl")

	kind, err := parseObjectKind(path, []byte(testCLIObjectKind), false)
	must.NoError(t, err)
	must.Eq(t, "db.example.com/PostgresCluster", kind.KindRef())
	must.True(t, kind.Namespaced)
	must.Eq(t, testCLIObjectSchema, kind.Schema)
	must.Len(t, 1, kind.Rules)
	must.Eq(t, []*api.ObjectKindColumn{
		{Name: "Version", Path: "spec.version"},
		{Name: "Ready", Path: "status.conditions[?type=='Ready'].status"},
	}, kind.Columns)
	must.Eq(t, "spec.replicas", kind.Scale.SpecPath)

	_, err = parseObjectKind(path, []byte(`kind "X" { group = "g" schema = "{}" unknown = 1 }`), false)
	must.Error(t, err)

	kind, err = parseObjectKind("kind.json", []byte(`{"Group": "g.example.com", "Kind": "X", "Schema": "{}"}`), false)
	must.NoError(t, err)
	must.False(t, kind.Namespaced)
}

func TestParseCustomObject(t *testing.T) {
	ci.Parallel(t)

	spec := `
kind       = "db.example.com/PostgresCluster"
name       = "billing"
namespace  = "prod"
labels     = { team = "finance" }
managed_by = "gitops"

spec {
  version  = "16"
  replicas = 3
  tags     = ["a", "b"]
  backup { schedule = "0 2 * * *" }
  user "alice" { admin = true }
  user "bob"   { admin = false }
  volume { size = "10GiB" }
  volume { size = "20GiB" }
}
`
	obj, err := parseCustomObject("billing.hcl", []byte(spec), false)
	must.NoError(t, err)
	must.Eq(t, "db.example.com", obj.Group)
	must.Eq(t, "PostgresCluster", obj.Kind)
	must.Eq(t, "billing", obj.Name)
	must.Eq(t, "prod", obj.Namespace)
	must.Eq(t, "gitops", obj.ManagedBy)
	must.Eq(t, map[string]string{"team": "finance"}, obj.Labels)
	must.Eq(t, map[string]any{
		"version":  "16",
		"replicas": float64(3),
		"tags":     []any{"a", "b"},
		"backup":   map[string]any{"schedule": "0 2 * * *"},
		"user": map[string]any{
			"alice": map[string]any{"admin": true},
			"bob":   map[string]any{"admin": false},
		},
		"volume": []any{map[string]any{"size": "10GiB"}, map[string]any{"size": "20GiB"}},
	}, obj.Spec)

	// The spec as an attribute.
	obj, err = parseCustomObject("x.hcl", []byte(`kind = "g.example.com/X"
name = "x"
spec = { a = { b = 1 } }`), false)
	must.NoError(t, err)
	must.Eq(t, map[string]any{"a": map[string]any{"b": float64(1)}}, obj.Spec)

	for _, bad := range []string{
		`name = "x"`,
		`kind = "nogroup"
name = "x"`,
		`kind = "g/X"
name = "x"
other = 1`,
		`kind = "g/X"
name = "x"
spec { a = 1 }
spec { b = 1 }`,
	} {
		_, err := parseCustomObject("bad.hcl", []byte(bad), false)
		must.Error(t, err, must.Sprint(bad))
	}

	// Finalizers and owners.
	obj, err = parseCustomObject("x.hcl", []byte(`kind = "g.example.com/X"
name       = "x"
finalizers = ["example.com/cleanup"]
owner {
  kind = "g.example.com/Parent"
  name = "p"
}
spec {}`), false)
	must.NoError(t, err)
	must.Eq(t, []string{"example.com/cleanup"}, obj.Finalizers)
	must.Eq(t, []*api.ObjectOwnerReference{{Kind: "g.example.com/Parent", Name: "p"}}, obj.Owners)

	obj, err = parseCustomObject("x.json", []byte(`{"Group": "g", "Kind": "X", "Name": "x", "Spec": {"a": 1}}`), false)
	must.NoError(t, err)
	must.Eq(t, map[string]any{"a": float64(1)}, obj.Spec)
}

func TestObjectCommands_Run(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "pg.schema.json"), []byte(testCLIObjectSchema), 0o600))
	kindPath := filepath.Join(dir, "pg.hcl")
	must.NoError(t, os.WriteFile(kindPath, []byte(testCLIObjectKind), 0o600))

	run := func(cmd cli.Command, ui *cli.MockUi, args ...string) int {
		return cmd.Run(append([]string{"-address", url}, args...))
	}

	// object-kind apply (dry run first)
	ui := cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, ui, "-dry-run", kindPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "dry run")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindApplyCommand{Meta: Meta{Ui: ui}}, ui, kindPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), `"db.example.com/PostgresCluster"`)

	// object-kind list / inspect
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindListCommand{Meta: Meta{Ui: ui}}, ui))
	must.StrContains(t, ui.OutputWriter.String(), "db.example.com/PostgresCluster")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindInspectCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster"))
	must.StrContains(t, ui.OutputWriter.String(), "Storage cannot be reduced.")
	must.StrContains(t, ui.OutputWriter.String(), `"required"`)

	// object apply
	objPath := filepath.Join(dir, "billing.hcl")
	must.NoError(t, os.WriteFile(objPath, []byte(`
kind   = "db.example.com/PostgresCluster"
name   = "billing"
labels = { team = "finance" }
spec {
  version = "16"
  storage = "100GiB"
}
`), 0o600))
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectApplyCommand{Meta: Meta{Ui: ui}}, ui, "-dry-run", objPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), `"replicas": 1`)
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectApplyCommand{Meta: Meta{Ui: ui}}, ui, objPath), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "generation 1")

	// A stale -cas is a conflict.
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectApplyCommand{Meta: Meta{Ui: ui}}, ui, "-cas", "1", objPath))
	must.StrContains(t, ui.ErrorWriter.String(), "read it again and retry")

	// The oldSelf rule rejects reducing the storage.
	smaller := filepath.Join(dir, "smaller.hcl")
	must.NoError(t, os.WriteFile(smaller, []byte(`
kind = "db.example.com/PostgresCluster"
name = "billing"
spec {
  version = "16"
  storage = "50GiB"
}
`), 0o600))
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectApplyCommand{Meta: Meta{Ui: ui}}, ui, smaller))
	must.StrContains(t, ui.ErrorWriter.String(), "Storage cannot be reduced.")

	// A controller writes the status.
	client, err := api.NewClient(&api.Config{Address: url})
	must.NoError(t, err)
	_, _, err = client.Objects().UpdateStatus("db.example.com", "PostgresCluster", "billing", map[string]any{
		"observedGeneration": 1,
		"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "reason": "Running"}},
	}, nil, nil)
	must.NoError(t, err)

	// object list shows the kind's columns.
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectListCommand{Meta: Meta{Ui: ui}}, ui, "-label", "team=finance", "db.example.com/PostgresCluster"), must.Sprint(ui.ErrorWriter.String()))
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "Version")
	must.StrContains(t, out, "Ready")
	must.StrContains(t, out, "billing")
	must.StrContains(t, out, "True")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectListCommand{Meta: Meta{Ui: ui}}, ui, "-label", "team=web", "db.example.com/PostgresCluster"))
	must.StrContains(t, ui.OutputWriter.String(), "No objects found")

	// object get
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectGetCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "billing"), must.Sprint(ui.ErrorWriter.String()))
	out = ui.OutputWriter.String()
	must.StrContains(t, out, "team=finance")
	must.StrContains(t, out, "Running")
	must.StrContains(t, out, `"storage": "100GiB"`)
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectGetCommand{Meta: Meta{Ui: ui}}, ui, "-json", "db.example.com/PostgresCluster", "billing"))
	must.StrContains(t, ui.OutputWriter.String(), `"Generation": 1`)

	// object scale shows and sets the count; the change is in the events.
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectScaleCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster/billing"), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "Replicas")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectScaleCommand{Meta: Meta{Ui: ui}}, ui, "-message", "more load", "db.example.com/PostgresCluster", "billing", "3"), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "3")
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectScaleCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster/billing", "-1"))
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectEventsCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "billing"), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "Scaled")
	must.StrContains(t, ui.OutputWriter.String(), "more load")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectGetCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "billing"))
	must.StrContains(t, ui.OutputWriter.String(), "Recent Events")

	// The kind can't be deleted while it has objects.
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectKindDeleteCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster"))

	// An object with a finalizer is only marked for deletion.
	_, _, err = client.Objects().Apply(&api.CustomObject{Group: "db.example.com", Kind: "PostgresCluster", Name: "fin",
		Spec: map[string]any{"version": "16"}, Finalizers: []string{"example.com/cleanup"}}, nil, nil)
	must.NoError(t, err)
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectDeleteCommand{Meta: Meta{Ui: ui}}, ui, "-propagation", "orphan", "db.example.com/PostgresCluster", "fin"), must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "marked for deletion")
	must.StrContains(t, ui.OutputWriter.String(), "example.com/cleanup")
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectGetCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "fin"))
	must.StrContains(t, ui.OutputWriter.String(), "Deleting")
	_, _, err = client.Objects().UpdateFinalizers("db.example.com", "PostgresCluster", "fin", nil, []string{"example.com/cleanup"}, nil, nil)
	must.NoError(t, err)

	// object delete
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectDeleteCommand{Meta: Meta{Ui: ui}}, ui, "-dry-run", "db.example.com/PostgresCluster", "billing"))
	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectDeleteCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "billing"), must.Sprint(ui.ErrorWriter.String()))
	ui = cli.NewMockUi()
	must.Eq(t, 1, run(&ObjectGetCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster", "billing"))

	ui = cli.NewMockUi()
	must.Eq(t, 0, run(&ObjectKindDeleteCommand{Meta: Meta{Ui: ui}}, ui, "db.example.com/PostgresCluster"), must.Sprint(ui.ErrorWriter.String()))
}

func TestParseObjectScaleArgs(t *testing.T) {
	ci.Parallel(t)

	g, k, n, count, err := parseObjectScaleArgs([]string{"db.example.com/PG/billing", "3"})
	must.NoError(t, err)
	must.Eq(t, []string{"db.example.com", "PG", "billing"}, []string{g, k, n})
	must.Eq(t, int64(3), *count)

	g, k, n, count, err = parseObjectScaleArgs([]string{"db.example.com/PG", "billing"})
	must.NoError(t, err)
	must.Eq(t, []string{"db.example.com", "PG", "billing"}, []string{g, k, n})
	must.Nil(t, count)

	for _, bad := range [][]string{{}, {"db.example.com/PG"}, {"db.example.com/PG/billing", "x"}, {"a/b/c", "1", "2"}} {
		_, _, _, _, err := parseObjectScaleArgs(bad)
		must.Error(t, err, must.Sprint(bad))
	}
}
