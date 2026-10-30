// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package variables

import (
	"testing"

	"github.com/hashicorp/nomad/pack/sdk/pack"
	"github.com/shoenig/test/must"
	"github.com/zclconf/go-cty/cty"
)

func TestSynthesizeUIFormSchema(t *testing.T) {
	t.Parallel()

	vCount := &Variable{Name: "instances", Description: "Number of instances"}
	vCount.SetType(cty.Number)
	vCount.SetDefault(cty.NumberIntVal(2))

	vPass := &Variable{Name: "superuser_password", Description: "Admin password"}
	vPass.SetType(cty.String)

	vDcs := &Variable{Name: "datacenters", Description: "Target datacenters"}
	vDcs.SetType(cty.List(cty.String))
	vDcs.SetDefault(cty.ListVal([]cty.Value{cty.StringVal("dc1")}))

	vars := map[pack.ID]map[ID]*Variable{
		"postgres": {
			"instances":          vCount,
			"superuser_password": vPass,
			"datacenters":        vDcs,
		},
	}

	rawOverlay := `{
		"properties": {
			"instances": {
				"minimum": 1,
				"maximum": 16,
				"x-ui-widget": "slider"
			},
			"edition": {
				"type": "string",
				"enum": ["standard", "postgis", "pgvector"],
				"default": "standard"
			}
		}
	}`

	schema := SynthesizeUIFormSchema("postgres", "PostgreSQL", vars, nil, map[string]string{"postgres": rawOverlay})
	must.Eq(t, "PostgreSQL", schema["title"])
	req, _ := schema["required"].([]string)
	must.SliceContains(t, req, "superuser_password")

	props := schema["properties"].(map[string]any)
	inst := props["instances"].(map[string]any)
	must.Eq[any](t, "slider", inst["x-ui-widget"])
	must.Eq[any](t, "resources", inst["x-ui-tab"])
	must.Eq[any](t, 2, inst["default"])

	pass := props["superuser_password"].(map[string]any)
	must.Eq[any](t, "secret", pass["x-ui-widget"])
	must.Eq[any](t, "secrets", pass["x-ui-tab"])

	dcs := props["datacenters"].(map[string]any)
	must.Eq[any](t, "datacenter-multiselect", dcs["x-ui-widget"])

	ed := props["edition"].(map[string]any)
	must.Eq[any](t, "segmented", ed["x-ui-widget"])
}

func TestSynthesizeUIFormSchema_VariableUI(t *testing.T) {
	t.Parallel()

	vPass := &Variable{Name: "redis_password"}
	vPass.SetType(cty.String)
	vPass.UI = map[string]any{
		"x-ui-tab":         "security",
		"x-ui-secret-path": "nomad/jobs/redis",
		"x-ui-secret-item": "password",
	}
	vMode := &Variable{Name: "mode"}
	vMode.SetType(cty.String)
	vMode.SetDefault(cty.StringVal("standalone"))
	vMode.UI = map[string]any{"enum": []any{"standalone", "replica"}, "x-ui-group": "Topology"}
	vCPU := &Variable{Name: "cpu"}
	vCPU.SetType(cty.Number)
	vCPU.SetDefault(cty.NumberIntVal(500))
	vCPU.UI = map[string]any{"x-ui-tab": "general"}

	vars := map[pack.ID]map[ID]*Variable{
		"redis": {"redis_password": vPass, "mode": vMode, "cpu": vCPU},
	}
	tabs := []any{
		map[string]any{"id": "security", "label": "Security", "order": 10},
		map[string]any{"id": "general", "label": "General", "order": 20},
	}
	schema := SynthesizeUIFormSchema("redis", "redis", vars, tabs, nil)
	props := schema["properties"].(map[string]any)

	pass := props["redis_password"].(map[string]any)
	must.Eq[any](t, "secret", pass["x-ui-widget"])
	must.Eq[any](t, "security", pass["x-ui-tab"], must.Sprint("the tab of the ui block wins"))
	must.Eq[any](t, "nomad/jobs/redis", pass["x-ui-secret-path"])

	mode := props["mode"].(map[string]any)
	must.Eq[any](t, "segmented", mode["x-ui-widget"], must.Sprint("options pick a choice widget"))
	must.Eq[any](t, "Topology", mode["x-ui-group"])

	cpu := props["cpu"].(map[string]any)
	must.Eq[any](t, "general", cpu["x-ui-tab"])
	_, hasGroup := cpu["x-ui-group"]
	must.False(t, hasGroup, must.Sprint("the inferred group of another tab is dropped"))

	must.Eq(t, tabs, schema["x-ui-tabs"].([]any))
}

func TestSynthesizeUIFormSchema_ItemOptions(t *testing.T) {
	t.Parallel()

	v := &Variable{Name: "entrypoints"}
	v.SetType(cty.List(cty.Object(map[string]cty.Type{
		"name":     cty.String,
		"port":     cty.Number,
		"protocol": cty.String,
	})))
	// What the decoder makes of ui { item_options = { protocol = [...] } }
	// with validation { item_min_length = 1 }.
	v.UI = map[string]any{"items": map[string]any{
		"minLength":  1,
		"properties": map[string]any{"protocol": map[string]any{"enum": []any{"http", "tcp"}}},
	}}
	vars := map[pack.ID]map[ID]*Variable{"traefik": {"entrypoints": v}}
	prop := SynthesizeUIFormSchema("traefik", "traefik", vars, nil, nil)["properties"].(map[string]any)["entrypoints"].(map[string]any)

	must.Eq[any](t, "object-list", prop["x-ui-widget"])
	items := prop["items"].(map[string]any)
	must.Eq[any](t, 1, items["minLength"])
	props := items["properties"].(map[string]any)
	protocol := props["protocol"].(map[string]any)
	must.Eq[any](t, []any{"http", "tcp"}, protocol["enum"])
	must.Eq[any](t, "string", protocol["type"], must.Sprint("the attribute keeps its type"))
	must.Eq[any](t, "Protocol", protocol["title"])
	port := props["port"].(map[string]any)
	must.Eq[any](t, "number", port["type"], must.Sprint("the other attributes stay"))
	must.MapLen(t, 3, props)
}
